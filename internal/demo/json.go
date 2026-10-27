package demo

import (
	"math"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
)

// The JSON below follows the Garmin payloads that internal/garmin decodes,
// with a few more fields Garmin also sends, so the stored raw JSON looks like
// a real sync. It carries no free text beyond the neutral activity name.

const garminTime = "2006-01-02 15:04:05"

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func ptr[T any](v T) *T { return &v }

type activityType struct {
	TypeKey string `json:"typeKey"`
}

type activityJSON struct {
	ActivityID      int64        `json:"activityId"`
	ActivityName    string       `json:"activityName"`
	StartTimeLocal  string       `json:"startTimeLocal"`
	StartTimeGMT    string       `json:"startTimeGMT"`
	ActivityType    activityType `json:"activityType"`
	Distance        float64      `json:"distance"`
	Duration        float64      `json:"duration"`
	ElapsedDuration float64      `json:"elapsedDuration"`
	MovingDuration  float64      `json:"movingDuration"`
	ElevationGain   *float64     `json:"elevationGain,omitempty"`
	ElevationLoss   *float64     `json:"elevationLoss,omitempty"`
	AverageSpeed    *float64     `json:"averageSpeed,omitempty"`
	MaxSpeed        *float64     `json:"maxSpeed,omitempty"`
	AverageHR       float64      `json:"averageHR"`
	MaxHR           float64      `json:"maxHR"`
	Calories        float64      `json:"calories"`
	RunCadence      *float64     `json:"averageRunningCadenceInStepsPerMinute,omitempty"`
	Steps           *int         `json:"steps,omitempty"`
	AvgPower        *float64     `json:"avgPower,omitempty"`
	NormPower       *float64     `json:"normPower,omitempty"`
	TrainingLoad    float64      `json:"activityTrainingLoad"`
	AerobicTE       float64      `json:"aerobicTrainingEffect"`
	AnaerobicTE     float64      `json:"anaerobicTrainingEffect"`
	VO2Max          *float64     `json:"vO2MaxValue,omitempty"`
	TSS             *float64     `json:"trainingStressScore,omitempty"`
	ManualActivity  bool         `json:"manualActivity"`
}

// normPower approximates normalized power for the demo's steady rides.
func normPower(avg float64) float64 { return avg * 1.04 }

// tss is the training stress score of a ride at normalized power np.
func tss(secs int, np float64) float64 { return float64(secs) * np * (np / ftp) / (ftp * 3600) * 100 }

func activityItem(b built) (garmin.Item[garmin.ActivityDTO], error) {
	info := b.info()
	sm := summarize(b, 0, len(b.samples))
	a := activityJSON{
		ActivityID:      b.id(),
		ActivityName:    info.name,
		StartTimeLocal:  b.startAt.Format(garminTime),
		StartTimeGMT:    b.startAt.UTC().Format(garminTime),
		ActivityType:    activityType{info.typeKey},
		Distance:        round2(sm.dist),
		Duration:        float64(sm.secs),
		ElapsedDuration: float64(sm.secs),
		MovingDuration:  float64(sm.secs),
		AverageHR:       math.Round(sm.avgHR),
		MaxHR:           sm.maxHR,
		Calories:        math.Round(sm.calories),
		TrainingLoad:    round1(sm.load),
		AerobicTE:       round1(sm.aerobicTE),
		AnaerobicTE:     round1(sm.anaerobicTE),
	}
	if sm.dist > 0 {
		a.AverageSpeed, a.MaxSpeed = ptr(round2(sm.avgSpeed)), ptr(round2(sm.maxSpeed))
	}
	if info.hills > 0 {
		a.ElevationGain, a.ElevationLoss = ptr(round1(sm.gain)), ptr(round1(sm.loss))
	}
	if info.run {
		spm := math.Round(sm.avgCad * 2)
		a.RunCadence, a.Steps = ptr(spm), ptr(int(spm*float64(sm.secs)/60))
		a.VO2Max = ptr(vo2max(b.date))
	}
	if sm.avgPower > 0 {
		np := normPower(sm.avgPower)
		a.AvgPower, a.NormPower, a.TSS = ptr(math.Round(sm.avgPower)), ptr(math.Round(np)), ptr(round1(tss(sm.secs, np)))
	}
	return item[garmin.ActivityDTO](a)
}

// The health model: each day's values follow the sleep of the night before
// it and the training of the day before, with day-to-day noise and a slow
// seasonal wave in HRV.

type sleepNight struct {
	secs  float64
	score int
}

// night is the sleep that ends on d's morning.
func night(d civil.Date) sleepNight {
	n := dayNum(d)
	r := newRNG(n, streamSleep)
	base := 26100.0 // 7 h 15
	if dow := mod(n, 7); dow == 5 || dow == 6 {
		base = 28200 // weekend mornings
	}
	if dayLoad(d.AddDays(-1)) > 150 {
		base += 900
	}
	secs := math.Round(base + r.between(-2400, 2100))
	score := int(math.Round(55 + (secs/3600-5.5)*12 + r.between(-6, 6)))
	return sleepNight{secs: secs, score: min(max(score, 38), 96)}
}

func qualifier(score int) string {
	switch {
	case score >= 90:
		return "EXCELLENT"
	case score >= 80:
		return "GOOD"
	case score >= 60:
		return "FAIR"
	}
	return "POOR"
}

type sleepScore struct {
	Value        int    `json:"value"`
	QualifierKey string `json:"qualifierKey"`
}

type sleepJSONT struct {
	CalendarDate      string                `json:"calendarDate"`
	SleepTimeSeconds  int64                 `json:"sleepTimeSeconds"`
	DeepSleepSeconds  int64                 `json:"deepSleepSeconds"`
	LightSleepSeconds int64                 `json:"lightSleepSeconds"`
	RemSleepSeconds   int64                 `json:"remSleepSeconds"`
	AwakeSleepSeconds int64                 `json:"awakeSleepSeconds"`
	SleepScores       map[string]sleepScore `json:"sleepScores"`
	AverageSpO2Value  float64               `json:"averageSpO2Value"`
}

func sleepJSON(d civil.Date) sleepJSONT {
	s := night(d)
	r := newRNG(dayNum(d), streamSleep+100)
	total := int64(s.secs)
	deep, rem := int64(s.secs*r.between(0.16, 0.21)), int64(s.secs*r.between(0.19, 0.24))
	return sleepJSONT{
		CalendarDate: d.String(), SleepTimeSeconds: total,
		DeepSleepSeconds: deep, RemSleepSeconds: rem, LightSleepSeconds: total - deep - rem,
		AwakeSleepSeconds: int64(r.between(600, 2400)),
		SleepScores:       map[string]sleepScore{"overall": {s.score, qualifier(s.score)}},
		AverageSpO2Value:  float64(r.intn(94, 97)),
	}
}

// hrvBase is the runner's HRV baseline on d, in ms.
func hrvBase(d civil.Date) float64 {
	return 62 + 3*math.Sin(2*math.Pi*float64(dayNum(d))/90)
}

// lastNight is the overnight HRV average ending on d's morning.
func lastNight(d civil.Date) float64 {
	r := newRNG(dayNum(d), streamHRV)
	v := hrvBase(d) - 0.03*dayLoad(d.AddDays(-1)) + r.between(-4.5, 4.5)
	if h := night(d).secs / 3600; h < 7 {
		v -= (7 - h) * 3
	}
	return math.Round(v)
}

type hrvBaseline struct {
	LowUpper      int `json:"lowUpper"`
	BalancedLow   int `json:"balancedLow"`
	BalancedUpper int `json:"balancedUpper"`
}

type hrvJSONT struct {
	CalendarDate      string      `json:"calendarDate"`
	WeeklyAvg         float64     `json:"weeklyAvg"`
	LastNightAvg      float64     `json:"lastNightAvg"`
	LastNight5MinHigh float64     `json:"lastNight5MinHigh"`
	Baseline          hrvBaseline `json:"baseline"`
	Status            string      `json:"status"`
}

func hrvJSON(d civil.Date) hrvJSONT {
	sum := 0.0
	for i := range 7 {
		sum += lastNight(d.AddDays(-i))
	}
	weekly := math.Round(sum / 7)
	base := hrvBase(d)
	b := hrvBaseline{LowUpper: int(math.Round(base - 11)), BalancedLow: int(math.Round(base - 7)), BalancedUpper: int(math.Round(base + 6))}
	status := "BALANCED"
	switch {
	case weekly < float64(b.LowUpper):
		status = "LOW"
	case weekly < float64(b.BalancedLow) || weekly > float64(b.BalancedUpper):
		status = "UNBALANCED"
	}
	last := lastNight(d)
	r := newRNG(dayNum(d), streamHRV+100)
	return hrvJSONT{CalendarDate: d.String(), WeeklyAvg: weekly, LastNightAvg: last,
		LastNight5MinHigh: last + float64(r.intn(12, 25)), Baseline: b, Status: status}
}

type dailyJSONT struct {
	CalendarDate string `json:"calendarDate"`
	HeartRate    struct {
		RestingValue int `json:"restingValue"`
	} `json:"heartRate"`
	Stress struct {
		AvgLevel int `json:"avgLevel"`
	} `json:"stress"`
	BodyBattery struct {
		MaxValue     int `json:"maxValue"`
		MinValue     int `json:"minValue"`
		ChargedValue int `json:"chargedValue"`
		DrainedValue int `json:"drainedValue"`
	} `json:"bodyBattery"`
	PulseOx struct {
		AvgValue int `json:"avgValue"`
	} `json:"pulseOx"`
}

func clampInt(v float64, lo, hi int) int { return min(max(int(math.Round(v)), lo), hi) }

func dailyJSON(d civil.Date) dailyJSONT {
	r := newRNG(dayNum(d), streamDaily)
	s := night(d)
	hours := s.secs / 3600
	var o dailyJSONT
	o.CalendarDate = d.String()
	o.HeartRate.RestingValue = clampInt(48+0.012*dayLoad(d.AddDays(-1))+(7-hours)*0.8+r.between(-1, 1), 44, 56)
	o.Stress.AvgLevel = clampInt(24+0.03*dayLoad(d)+r.between(-5, 5), 12, 55)
	bb := &o.BodyBattery
	bb.MaxValue = clampInt(40+float64(s.score)*0.6+r.between(-4, 4), 30, 100)
	bb.ChargedValue = clampInt(hours*9+r.between(-6, 6), 25, 85)
	bb.DrainedValue = clampInt(45+0.15*dayLoad(d)+r.between(-6, 6), 35, 95)
	bb.MinValue = clampInt(float64(bb.MaxValue-bb.DrainedValue), 5, 45)
	o.PulseOx.AvgValue = r.intn(95, 97)
	return o
}

type weightEntry struct {
	CalendarDate string  `json:"calendarDate"`
	Weight       float64 `json:"weight"`
}

type weightJSONT struct {
	SummaryDate        string      `json:"summaryDate"`
	NumOfWeightEntries int         `json:"numOfWeightEntries"`
	LatestWeight       weightEntry `json:"latestWeight"`
}

// weightJSON is a weigh-in on most Mondays and Thursdays, in kilograms.
func weightJSON(d civil.Date) (any, bool) {
	n := dayNum(d)
	r := newRNG(n, streamWeight)
	if dow := mod(n, 7); (dow != 0 && dow != 3) || !r.chance(0.85) {
		return nil, false
	}
	kg := round1(weightKg + 0.6*math.Sin(2*math.Pi*float64(n)/150) + r.between(-0.3, 0.3))
	return weightJSONT{SummaryDate: d.String(), NumOfWeightEntries: 1,
		LatestWeight: weightEntry{CalendarDate: d.String(), Weight: kg}}, true
}
