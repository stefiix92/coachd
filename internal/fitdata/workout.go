package fitdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/fieldnum"
	"github.com/muktihari/fit/proto"
)

// maxSampleS caps how long one record counts for in time in target, so a
// pause between two records does not count.
const maxSampleS = 10

// Workout is the structured workout an activity recorded: the steps the watch
// followed and the steps as executed. It holds numbers only.
type Workout struct {
	NameUntrusted string        // wkt_name: Garmin or agent text, never trusted
	Steps         []WorkoutStep // planned, by message index
	Executed      []StepRun     // in order
	Fingerprint   string        // of Steps; see Fingerprint
}

// WorkoutStep is a workout_step message in FIT units. Absent values keep
// their FIT invalid sentinel (0xFF, 0xFFFFFFFF).
type WorkoutStep struct {
	Index         int
	Intensity     uint8 // typedef.Intensity
	DurationType  uint8 // typedef.WktStepDuration
	DurationValue uint32
	TargetType    uint8 // typedef.WktStepTarget
	TargetValue   uint32
	CustomLow     uint32
	CustomHigh    uint32
}

// StepRun is one executed step: consecutive laps with the same step index.
type StepRun struct {
	StepIndex, Iteration, LapCount int
	StartOffsetS, DurationS        float64 // offset as for laps; timer time
	DistanceKm                     float64
	AvgHRBpm, MaxHRBpm             *int
	AvgPaceSPerKm                  *float64
	AvgPowerW                      *int
	// TargetTime is set for a step whose target is a heart-rate range in bpm
	// or a speed range.
	TargetTime *TargetTime
}

// TargetTime splits a step's measured seconds by its target range. Below and
// above refer to the target's own numbers: heart rate in bpm, or pace in
// s/km (below is faster).
type TargetTime struct {
	MeasuredS, InS, BelowS, AboveS float64
}

func (c *collector) step(m *proto.Message) {
	s := WorkoutStep{
		Index:         len(c.steps),
		Intensity:     value(m, fieldnum.WorkoutStepIntensity).Uint8(),
		DurationType:  value(m, fieldnum.WorkoutStepDurationType).Uint8(),
		DurationValue: value(m, fieldnum.WorkoutStepDurationValue).Uint32(),
		TargetType:    value(m, fieldnum.WorkoutStepTargetType).Uint8(),
		TargetValue:   value(m, fieldnum.WorkoutStepTargetValue).Uint32(),
		CustomLow:     value(m, fieldnum.WorkoutStepCustomTargetValueLow).Uint32(),
		CustomHigh:    value(m, fieldnum.WorkoutStepCustomTargetValueHigh).Uint32(),
	}
	if i := value(m, fieldnum.WorkoutStepMessageIndex).Uint16(); i != 0xFFFF {
		s.Index = int(i)
	}
	c.steps = append(c.steps, s)
}

// workout returns the recorded workout, or nil when the file has no workout
// message or no lap with a valid step index. Lap offsets must be set.
func (c *collector) workout() *Workout {
	if !c.hasWorkout {
		return nil
	}
	steps := append([]WorkoutStep(nil), c.steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Index < steps[j].Index })
	byIndex := map[int]WorkoutStep{}
	for _, s := range steps {
		byIndex[s.Index] = s
	}

	var runs []StepRun
	iterations := map[int]int{}
	for i := 0; i < len(c.laps); {
		idx := c.laps[i].stepIndex
		j := i + 1
		for j < len(c.laps) && c.laps[j].stepIndex == idx {
			j++
		}
		if idx != 0xFFFF {
			iterations[int(idx)]++
			r := run(c.laps[i:j], c.points, byIndex[int(idx)])
			r.StepIndex, r.Iteration = int(idx), iterations[int(idx)]
			runs = append(runs, r)
		}
		i = j
	}
	if len(runs) == 0 {
		return nil
	}
	return &Workout{NameUntrusted: c.wktName, Steps: steps, Executed: runs, Fingerprint: Fingerprint(steps)}
}

// run sums laps into one executed step and counts its time in target from
// the records between its first lap's start and its last lap's end.
func run(laps []pendingLap, points []Point, step WorkoutStep) StepRun {
	r := StepRun{LapCount: len(laps), StartOffsetS: laps[0].StartOffsetS}
	var hrSum, hrTime, powSum, powTime float64
	for _, l := range laps {
		r.DurationS += l.DurationS
		r.DistanceKm += l.DistanceKm
		if l.AvgHRBpm != nil {
			hrSum += float64(*l.AvgHRBpm) * l.DurationS
			hrTime += l.DurationS
		}
		if l.MaxHRBpm != nil && (r.MaxHRBpm == nil || *l.MaxHRBpm > *r.MaxHRBpm) {
			r.MaxHRBpm = ptr(*l.MaxHRBpm)
		}
		if l.AvgPowerW != nil {
			powSum += float64(*l.AvgPowerW) * l.DurationS
			powTime += l.DurationS
		}
	}
	if hrTime > 0 {
		r.AvgHRBpm = ptr(int(math.Round(hrSum / hrTime)))
	}
	if powTime > 0 {
		r.AvgPowerW = ptr(int(math.Round(powSum / powTime)))
	}
	if r.DurationS > 0 {
		r.AvgPaceSPerKm = pace(r.DistanceKm * 1000 / r.DurationS)
	}

	last := laps[len(laps)-1]
	span := last.elapsedS
	if !valid(span) {
		span = last.DurationS
	}
	r.TargetTime = targetTime(points, step, r.StartOffsetS, last.StartOffsetS+span)
	return r
}

// targetTime counts the records in [from, to) against step's target, or
// returns nil when the target is not a heart-rate range in bpm or a speed
// range. Each record counts until the next one, at most maxSampleS and not
// past to.
func targetTime(points []Point, step WorkoutStep, from, to float64) *TargetTime {
	var measure func(p Point) (float64, bool)
	var low, high float64
	switch {
	case !step.CustomRange():
		return nil
	case typedef.WktStepTarget(step.TargetType) == typedef.WktStepTargetHeartRate:
		if step.CustomLow <= 100 || step.CustomHigh <= 100 {
			return nil // % of max heart rate
		}
		low, high = float64(step.CustomLow-100), float64(step.CustomHigh-100)
		measure = func(p Point) (float64, bool) {
			if p.HRBpm == nil {
				return 0, false
			}
			return float64(*p.HRBpm), true
		}
	case typedef.WktStepTarget(step.TargetType) == typedef.WktStepTargetSpeed:
		if step.CustomLow == 0 || step.CustomHigh == 0 {
			return nil
		}
		// Speed bounds in mm/s become pace bounds: the higher speed is the lower pace.
		low, high = 1e6/float64(step.CustomHigh), 1e6/float64(step.CustomLow)
		measure = func(p Point) (float64, bool) {
			if p.PaceSPerKm == nil {
				return 0, false
			}
			return *p.PaceSPerKm, true
		}
	default:
		return nil
	}

	tt := &TargetTime{}
	i := sort.Search(len(points), func(i int) bool { return points[i].OffsetS >= from })
	for ; i < len(points) && points[i].OffsetS < to; i++ {
		v, ok := measure(points[i])
		if !ok {
			continue
		}
		next := points[i].OffsetS + 1
		if i+1 < len(points) {
			next = points[i+1].OffsetS
		}
		w := math.Min(math.Min(next, to)-points[i].OffsetS, maxSampleS)
		tt.MeasuredS += w
		switch {
		case v < low:
			tt.BelowS += w
		case v > high:
			tt.AboveS += w
		default:
			tt.InS += w
		}
	}
	return tt
}

// CustomRange reports whether the step's target is given by custom bounds
// (target value 0 and both bounds valid), not by a zone.
func (s WorkoutStep) CustomRange() bool {
	return (s.TargetValue == 0 || s.TargetValue == math.MaxUint32) &&
		s.CustomLow != math.MaxUint32 && s.CustomHigh != math.MaxUint32
}

// Repeat reports whether the step repeats earlier steps: its duration value
// is the index of the first step repeated, and its target value is not a
// target (for repeat_until_steps_cmplt, it is the count).
func (s WorkoutStep) Repeat() bool {
	switch typedef.WktStepDuration(s.DurationType) {
	case typedef.WktStepDurationRepeatUntilStepsCmplt, typedef.WktStepDurationRepeatUntilTime,
		typedef.WktStepDurationRepeatUntilDistance, typedef.WktStepDurationRepeatUntilCalories,
		typedef.WktStepDurationRepeatUntilHrLessThan, typedef.WktStepDurationRepeatUntilHrGreaterThan,
		typedef.WktStepDurationRepeatUntilPowerLessThan, typedef.WktStepDurationRepeatUntilPowerGreaterThan,
		typedef.WktStepDurationRepeatUntilPowerLastLapLessThan, typedef.WktStepDurationRepeatUntilMaxPowerLastLapLessThan,
		typedef.WktStepDurationRepeatUntilTrainingPeaksTss:
		return true
	}
	return false
}

// Fingerprint is the hex SHA-256 of steps' structure, in index order: each
// step's index, duration type and value, and target. A repeat step's target
// value is its count; an open duration or target has no value; a custom range
// is its bounds, otherwise the target value. Names, notes and intensity are
// left out, because watches may truncate or rewrite them when they record a
// workout.
func Fingerprint(steps []WorkoutStep) string {
	h := sha256.New()
	for _, s := range steps {
		d := typedef.WktStepDuration(s.DurationType)
		dv := s.DurationValue
		if d == typedef.WktStepDurationOpen {
			dv = 0
		}
		if s.Repeat() {
			fmt.Fprintf(h, "%d:%d:%d:repeat:%d;", s.Index, d, dv, s.TargetValue)
			continue
		}
		t := typedef.WktStepTarget(s.TargetType)
		switch {
		case t == typedef.WktStepTargetOpen:
			fmt.Fprintf(h, "%d:%d:%d:%d;", s.Index, d, dv, t)
		case s.CustomRange():
			fmt.Fprintf(h, "%d:%d:%d:%d:%d-%d;", s.Index, d, dv, t, s.CustomLow, s.CustomHigh)
		default:
			fmt.Fprintf(h, "%d:%d:%d:%d:%d;", s.Index, d, dv, t, s.TargetValue)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
