package ingest

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/store"
)

// garminTimeLayout is the activity search time format.
const garminTimeLayout = "2006-01-02 15:04:05"

// gramsThreshold separates weigh-ins reported in grams (defensive) from
// kilograms (OpenAPI): no human weighs >= 1000 kg or < 1000 g.
const gramsThreshold = 1000

var sportByTypeKey = map[string]store.Sport{
	"running": store.SportRunning, "treadmill_running": store.SportRunning, "track_running": store.SportRunning,
	"indoor_running": store.SportRunning, "virtual_run": store.SportRunning,
	"trail_running": store.SportTrailRunning,
	"walking":       store.SportWalking, "casual_walking": store.SportWalking, "speed_walking": store.SportWalking,
	"hiking":  store.SportHiking,
	"cycling": store.SportCycling, "road_biking": store.SportCycling, "mountain_biking": store.SportCycling,
	"gravel_cycling": store.SportCycling, "indoor_cycling": store.SportCycling, "virtual_ride": store.SportCycling,
	"strength_training": store.SportStrength,
}

// NormalizeSport maps a Garmin activity typeKey to a coachd sport.
func NormalizeSport(typeKey string) store.Sport {
	if s, ok := sportByTypeKey[typeKey]; ok {
		return s
	}
	return store.SportOther
}

// MapActivity converts an activity search entry. Units stay metres and
// seconds. Date is the wall-clock date of startTimeLocal in tz. HasPower is
// avgPower above zero. A missing or invalid start time is an error.
func MapActivity(dto garmin.ActivityDTO, tz *time.Location) (store.Activity, error) {
	if dto.ActivityID <= 0 {
		return store.Activity{}, errors.New("ingest: activity: missing id")
	}
	local, err := time.ParseInLocation(garminTimeLayout, dto.StartTimeLocal, tz)
	if err != nil {
		return store.Activity{}, errors.New("ingest: activity: invalid startTimeLocal")
	}
	utc, err := time.Parse(garminTimeLayout, dto.StartTimeGMT)
	if err != nil {
		return store.Activity{}, errors.New("ingest: activity: invalid startTimeGMT")
	}
	date := civil.Of(local, tz)
	typeKey := ""
	if dto.ActivityType != nil {
		typeKey = dto.ActivityType.TypeKey
	}
	fit := store.FITPending
	if dto.ManualActivity {
		fit = store.FITUnavailable // manual entries have no FIT file
	}
	return store.Activity{
		ID:            dto.ActivityID,
		StartLocal:    local,
		StartUTC:      utc,
		Date:          date,
		WeekStart:     date.WeekStart(),
		TypeKey:       typeKey,
		Sport:         NormalizeSport(typeKey),
		NameUntrusted: dto.ActivityName,
		DistanceM:     deref(dto.Distance),
		DurationS:     deref(dto.Duration),
		MovingS:       deref(dto.MovingDuration),
		ElevGainM:     deref(dto.ElevationGain),
		ElevLossM:     deref(dto.ElevationLoss),
		AvgHR:         dto.AverageHR,
		MaxHR:         dto.MaxHR,
		Manual:        dto.ManualActivity,
		Garmin: store.ActivityGarmin{
			TrainingLoad: dto.ActivityTrainingLoad,
			AerobicTE:    dto.AerobicTrainingEffect,
			AnaerobicTE:  dto.AnaerobicTrainingEffect,
			VO2Max:       dto.VO2MaxValue,
			TSS:          dto.TrainingStressScore,
		},
		FITStatus: fit,
		Source:    store.SourceGarmin,
		HasPower:  deref(dto.AvgPower) > 0,
	}, nil
}

// MapSleep fills only the sleep fields. SpO2 is owned by daily v2.
func MapSleep(dto garmin.SleepDTO) (store.DailyHealth, error) {
	h, err := dailyOf(dto.CalendarDate)
	if err != nil {
		return h, err
	}
	if dto.SleepTimeSeconds != nil {
		v := int(*dto.SleepTimeSeconds)
		h.SleepS = &v
	}
	if s, ok := dto.SleepScores["overall"]; ok {
		h.Garmin.SleepScore = s.Value
	}
	return h, nil
}

// MapHRV fills only the HRV fields, rounding milliseconds to the nearest integer.
func MapHRV(dto garmin.HRVSummaryDTO) (store.DailyHealth, error) {
	h, err := dailyOf(dto.CalendarDate)
	if err != nil {
		return h, err
	}
	h.HRVOvernightMs = round(dto.LastNightAvg)
	h.HRVWeeklyAvgMs = round(dto.WeeklyAvg)
	if dto.Status != "" {
		s := dto.Status
		h.Garmin.HRVStatus = &s
	}
	if dto.Baseline != nil {
		h.Garmin.HRVBaselineLowMs = round(dto.Baseline.BalancedLow)
		h.Garmin.HRVBaselineHighMs = round(dto.Baseline.BalancedUpper)
	}
	return h, nil
}

// MapDailyV2 fills only the daily-summary fields: resting HR, SpO2, stress and Body Battery.
func MapDailyV2(dto garmin.DailySummaryV2DTO) (store.DailyHealth, error) {
	h, err := dailyOf(dto.CalendarDate)
	if err != nil {
		return h, err
	}
	if dto.HeartRate != nil {
		h.RestingHR = dto.HeartRate.RestingValue
	}
	if dto.PulseOx != nil {
		h.SpO2Avg = dto.PulseOx.AvgValue
	}
	if dto.Stress != nil {
		h.Garmin.StressAvg = dto.Stress.AvgLevel
	}
	if bb := dto.BodyBattery; bb != nil {
		h.Garmin.BodyBatteryMax, h.Garmin.BodyBatteryMin = bb.MaxValue, bb.MinValue
		h.Garmin.BodyBatteryCharged, h.Garmin.BodyBatteryDrained = bb.ChargedValue, bb.DrainedValue
	}
	return h, nil
}

// MapWeight fills only WeightKg. Values >= 1000 are grams, smaller values kilograms.
func MapWeight(dto garmin.WeightDTO) (store.DailyHealth, error) {
	date := dto.SummaryDate
	if date == "" && dto.LatestWeight != nil {
		date = dto.LatestWeight.CalendarDate
	}
	h, err := dailyOf(date)
	if err != nil {
		return h, err
	}
	if dto.LatestWeight != nil && dto.LatestWeight.Weight != nil {
		kg := *dto.LatestWeight.Weight
		if kg >= gramsThreshold {
			kg /= 1000
		}
		h.WeightKg = &kg
	}
	return h, nil
}

func dailyOf(calendarDate string) (store.DailyHealth, error) {
	d, err := civil.Parse(calendarDate)
	if err != nil {
		return store.DailyHealth{}, fmt.Errorf("ingest: invalid calendarDate: %w", err)
	}
	return store.DailyHealth{Date: d}, nil
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func round(p *float64) *int {
	if p == nil {
		return nil
	}
	v := int(math.Round(*p))
	return &v
}
