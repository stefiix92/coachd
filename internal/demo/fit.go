package demo

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
)

// encodeFIT writes b as an activity FIT file: file_id, timer events, one
// record per second, laps, one session (with the runner's rating when the
// session has one) and the activity message. The summary values come from
// the same samples as the activity search entry, so the two agree.
func encodeFIT(b built, tz *time.Location) ([]byte, error) {
	info := b.info()
	start := b.startAt.UTC()
	at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Second) }
	end := at(len(b.samples))
	moves := b.total > 0

	f := filedef.NewActivity()
	f.FileId.SetType(typedef.FileActivity).SetManufacturer(typedef.ManufacturerDevelopment).SetProduct(1).
		SetSerialNumber(uint32(b.id() % 1_000_000_000)).SetTimeCreated(start)
	f.Events = append(f.Events,
		mesgdef.NewEvent(nil).SetTimestamp(start).SetEvent(typedef.EventTimer).SetEventType(typedef.EventTypeStart),
		mesgdef.NewEvent(nil).SetTimestamp(end).SetEvent(typedef.EventTimer).SetEventType(typedef.EventTypeStopAll))

	f.Records = make([]*mesgdef.Record, 0, len(b.samples))
	for i, s := range b.samples {
		rec := mesgdef.NewRecord(nil).SetTimestamp(at(i)).SetHeartRate(uint8(math.Round(s.hr)))
		if moves {
			rec.SetDistanceScaled(s.dist).SetEnhancedSpeedScaled(s.speed)
		}
		if info.hills > 0 {
			rec.SetEnhancedAltitudeScaled(s.alt)
		}
		if s.cad != 0 {
			rec.SetCadence(s.cad)
		}
		if s.power > 0 {
			rec.SetPower(uint16(math.Round(s.power)))
		}
		f.Records = append(f.Records, rec)
	}

	trigger := map[lapMode]typedef.LapTrigger{lapPerKm: typedef.LapTriggerDistance, lapPerSegment: typedef.LapTriggerManual,
		lapPer10Min: typedef.LapTriggerTime, lapWhole: typedef.LapTriggerSessionEnd}[info.laps]
	for i, l := range b.laps {
		sm := summarize(b, l[0], l[1])
		lap := mesgdef.NewLap(nil).SetMessageIndex(typedef.MessageIndex(i)).
			SetTimestamp(at(l[1])).SetStartTime(at(l[0])).SetEvent(typedef.EventLap).SetEventType(typedef.EventTypeStop).
			SetSport(info.sport).SetSubSport(info.subSport).SetLapTrigger(trigger).
			SetTotalTimerTimeScaled(float64(sm.secs)).SetTotalElapsedTimeScaled(float64(sm.secs)).
			SetAvgHeartRate(uint8(math.Round(sm.avgHR))).SetMaxHeartRate(uint8(sm.maxHR)).
			SetTotalCalories(uint16(math.Round(sm.calories)))
		if i == len(b.laps)-1 && info.laps != lapWhole {
			lap.SetLapTrigger(typedef.LapTriggerSessionEnd)
		}
		if moves {
			lap.SetTotalDistanceScaled(sm.dist).SetEnhancedAvgSpeedScaled(sm.avgSpeed).SetEnhancedMaxSpeedScaled(sm.maxSpeed)
		}
		if info.hills > 0 {
			lap.SetTotalAscent(uint16(math.Round(sm.gain))).SetTotalDescent(uint16(math.Round(sm.loss)))
		}
		if sm.avgCad > 0 {
			lap.SetAvgCadence(uint8(math.Round(sm.avgCad)))
		}
		if sm.avgPower > 0 {
			lap.SetAvgPower(uint16(math.Round(sm.avgPower)))
		}
		f.Laps = append(f.Laps, lap)
	}

	sm := summarize(b, 0, len(b.samples))
	sess := mesgdef.NewSession(nil).SetTimestamp(end).SetStartTime(start).
		SetEvent(typedef.EventSession).SetEventType(typedef.EventTypeStop).SetTrigger(typedef.SessionTriggerActivityEnd).
		SetSport(info.sport).SetSubSport(info.subSport).
		SetTotalTimerTimeScaled(float64(sm.secs)).SetTotalElapsedTimeScaled(float64(sm.secs)).
		SetAvgHeartRate(uint8(math.Round(sm.avgHR))).SetMaxHeartRate(uint8(sm.maxHR)).
		SetTotalCalories(uint16(math.Round(sm.calories))).
		SetFirstLapIndex(0).SetNumLaps(uint16(len(b.laps))).
		SetTotalTrainingEffectScaled(round1(sm.aerobicTE)).SetTotalAnaerobicTrainingEffectScaled(round1(sm.anaerobicTE))
	if moves {
		sess.SetTotalDistanceScaled(sm.dist).SetEnhancedAvgSpeedScaled(sm.avgSpeed).SetEnhancedMaxSpeedScaled(sm.maxSpeed)
	}
	if info.hills > 0 {
		sess.SetTotalAscent(uint16(math.Round(sm.gain))).SetTotalDescent(uint16(math.Round(sm.loss)))
	}
	if sm.avgCad > 0 {
		sess.SetAvgCadence(uint8(math.Round(sm.avgCad)))
	}
	if sm.avgPower > 0 {
		np := normPower(sm.avgPower)
		sess.SetAvgPower(uint16(math.Round(sm.avgPower))).SetNormalizedPower(uint16(math.Round(np))).
			SetThresholdPower(uint16(ftp)).SetIntensityFactorScaled(np / ftp).SetTrainingStressScoreScaled(tss(sm.secs, np))
	}
	if b.feel != nil {
		sess.SetWorkoutFeel(*b.feel)
	}
	if b.rpe != nil {
		sess.SetWorkoutRpe(*b.rpe)
	}
	f.Sessions = append(f.Sessions, sess)

	// local_timestamp is the end in local wall-clock time, counted as if UTC.
	l := end.In(tz)
	local := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
	f.Activity = mesgdef.NewActivity(nil).SetTimestamp(end).SetLocalTimestamp(local).
		SetTotalTimerTimeScaled(float64(sm.secs)).SetNumSessions(1).SetType(typedef.ActivityManual).
		SetEvent(typedef.EventActivity).SetEventType(typedef.EventTypeStop)

	fit := f.ToFIT(nil)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		return nil, fmt.Errorf("demo: encode fit: %w", err)
	}
	return buf.Bytes(), nil
}
