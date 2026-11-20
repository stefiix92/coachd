// Package fittest builds small synthetic activity FIT files for tests.
package fittest

import (
	"bytes"
	"fmt"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"github.com/muktihari/fit/proto"
)

// Options describes the synthetic activity to encode.
type Options struct {
	Start   time.Time // session/lap/record start (UTC)
	Laps    int       // records split evenly across laps, unless LapRecords is set
	Records int       // 1 Hz records
	HR      uint8     // heart rate on every record (0xFF = invalid sentinel)
	SpeedMS float64   // enhanced speed on every record, m/s

	// Optional; zero values keep the even, constant activity above.
	LapRecords []int                        // records per lap; overrides Laps and must sum to Records
	HRAt       func(i int) uint8            // heart rate of record i, in place of HR
	SpeedAt    func(i int) float64          // speed of record i in m/s, in place of SpeedMS
	LapSteps   []uint16                     // wkt_step_index per lap (0xFFFF = invalid); nil leaves it unset
	Workout    []byte                       // a FIT workout file whose workout and workout_step messages the activity records
	Gaps       map[int]time.Duration        // extra time before record i (a pause)
	LapPower   func(lap int) (watts uint16) // avg_power per lap; nil leaves it unset
	PowerAt    func(i int) (watts uint16)   // power of record i; nil leaves it unset
	Feel, RPE  *uint8                       // the session's workout_feel (0-100) and workout_rpe (×10); nil leaves them unset
	Modify     func(steps []proto.Message)  // edits the copied workout_step messages, as a watch might
}

// Build encodes a valid activity FIT file.
func Build(o Options) ([]byte, error) {
	counts := o.LapRecords
	if counts == nil {
		if o.Laps < 1 || o.Records < o.Laps {
			return nil, fmt.Errorf("fittest: need Laps >= 1 and Records >= Laps")
		}
		per := o.Records / o.Laps
		for l := 0; l < o.Laps; l++ {
			n := per
			if l == o.Laps-1 {
				n = o.Records - per*(o.Laps-1)
			}
			counts = append(counts, n)
		}
	}
	sum := 0
	for _, n := range counts {
		sum += n
	}
	if sum != o.Records || len(counts) == 0 {
		return nil, fmt.Errorf("fittest: LapRecords must sum to Records")
	}
	if o.LapSteps != nil && len(o.LapSteps) != len(counts) {
		return nil, fmt.Errorf("fittest: need one LapSteps entry per lap")
	}

	f := filedef.NewActivity()
	f.FileId.SetType(typedef.FileActivity).SetTimeCreated(o.Start)

	times := make([]time.Time, o.Records+1)
	t, dist := o.Start, 0.0
	for i := 0; i <= o.Records; i++ {
		t = t.Add(o.Gaps[i])
		times[i] = t
		t = t.Add(time.Second)
	}
	for i := 0; i < o.Records; i++ {
		hr, speed := o.HR, o.SpeedMS
		if o.HRAt != nil {
			hr = o.HRAt(i)
		}
		if o.SpeedAt != nil {
			speed = o.SpeedAt(i)
		}
		rec := mesgdef.NewRecord(nil).
			SetTimestamp(times[i]).
			SetDistanceScaled(dist).
			SetHeartRate(hr).
			SetEnhancedSpeedScaled(speed).
			SetEnhancedAltitudeScaled(200)
		if o.PowerAt != nil {
			rec.SetPower(o.PowerAt(i))
		}
		f.Records = append(f.Records, rec)
		dist += speed
	}

	first := 0
	for l, n := range counts {
		start, end := times[first], times[first+n]
		lap := mesgdef.NewLap(nil).
			SetTimestamp(end).
			SetStartTime(start).
			SetTotalTimerTimeScaled(float64(n)).
			SetTotalElapsedTimeScaled(end.Sub(start).Seconds()).
			SetTotalDistanceScaled(float64(n) * o.SpeedMS).
			SetAvgHeartRate(o.HR).
			SetMaxHeartRate(o.HR).
			SetEnhancedAvgSpeedScaled(o.SpeedMS).
			SetTotalAscent(5)
		if o.LapSteps != nil {
			lap.SetWktStepIndex(typedef.MessageIndex(o.LapSteps[l]))
		}
		if o.LapPower != nil {
			lap.SetAvgPower(o.LapPower(l))
		}
		f.Laps = append(f.Laps, lap)
		first += n
	}

	end := times[o.Records]
	sess := mesgdef.NewSession(nil).SetTimestamp(end).SetStartTime(o.Start)
	if o.Feel != nil {
		sess.SetWorkoutFeel(*o.Feel)
	}
	if o.RPE != nil {
		sess.SetWorkoutRpe(*o.RPE)
	}
	f.Sessions = append(f.Sessions, sess)
	f.Activity = mesgdef.NewActivity(nil).SetTimestamp(end).SetNumSessions(1)

	fit := f.ToFIT(nil)
	if o.Workout != nil {
		msgs, err := workoutMessages(o.Workout)
		if err != nil {
			return nil, err
		}
		if o.Modify != nil {
			o.Modify(msgs[1:])
		}
		fit.Messages = append(fit.Messages, msgs...) // a watch writes them at the end
	}
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		return nil, fmt.Errorf("fittest: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// workoutMessages returns the workout message, then the workout_step
// messages, of a FIT workout file.
func workoutMessages(b []byte) ([]proto.Message, error) {
	fit, err := decoder.New(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, fmt.Errorf("fittest: decode workout: %w", err)
	}
	var wkt []proto.Message
	var steps []proto.Message
	for _, m := range fit.Messages {
		switch m.Num {
		case mesgnum.Workout:
			wkt = append(wkt, m)
		case mesgnum.WorkoutStep:
			steps = append(steps, m)
		}
	}
	if len(wkt) != 1 {
		return nil, fmt.Errorf("fittest: workout file has %d workout messages", len(wkt))
	}
	return append(wkt, steps...), nil
}

// Step is one workout step for WorkoutFile, in FIT units.
type Step struct {
	Intensity     typedef.Intensity
	Duration      typedef.WktStepDuration
	DurationValue uint32
	Target        typedef.WktStepTarget
	TargetValue   uint32
	Low, High     uint32 // custom target bounds; 0 leaves them unset
}

// WorkoutFile encodes a FIT workout file with name and steps.
func WorkoutFile(name string, steps []Step) ([]byte, error) {
	msgs := []proto.Message{
		mesgdef.NewFileId(nil).SetType(typedef.FileWorkout).SetManufacturer(typedef.ManufacturerDevelopment).
			SetTimeCreated(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)).ToMesg(nil),
		mesgdef.NewWorkout(nil).SetSport(typedef.SportRunning).SetNumValidSteps(uint16(len(steps))).
			SetWktName(name).ToMesg(nil),
	}
	for i, s := range steps {
		st := mesgdef.NewWorkoutStep(nil).SetMessageIndex(typedef.MessageIndex(i)).SetIntensity(s.Intensity).
			SetDurationType(s.Duration).SetDurationValue(s.DurationValue).
			SetTargetType(s.Target).SetTargetValue(s.TargetValue)
		if s.Low != 0 || s.High != 0 {
			st.SetCustomTargetValueLow(s.Low).SetCustomTargetValueHigh(s.High)
		}
		msgs = append(msgs, st.ToMesg(nil))
	}
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&proto.FIT{Messages: msgs}); err != nil {
		return nil, fmt.Errorf("fittest: encode workout: %w", err)
	}
	return buf.Bytes(), nil
}
