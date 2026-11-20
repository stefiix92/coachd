// Package guidance computes plain facts about a goal and deterministic,
// number-bearing workout warnings. It is pure: no clock, no I/O. Warnings
// never block and never advise.
package guidance

import (
	"fmt"
	"math"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/workout"
)

// GoalFacts are unrounded facts returned when a goal is set.
type GoalFacts struct {
	WeeksUntilGoal                      int
	Last4WeeksAvgKm, Last4WeeksAvgElevM float64
	LongestRun30dKm                     float64
	WeeksOfHistory                      int
}

// ComputeGoalFacts averages the 4 full Monday-start weeks before today's week
// (missing weeks count as 0). Distance covers running and trail_running;
// elevation covers all sports.
func ComputeGoalFacts(today, goalDate civil.Date, weeks []store.WeekVolume, todayMetrics *store.DayMetrics, cov store.Coverage) GoalFacts {
	f := GoalFacts{WeeksOfHistory: cov.WeeksOfHistory}
	if days := goalDate.Sub(today); days > 0 {
		f.WeeksUntilGoal = (days + 6) / 7
	}
	cur := today.WeekStart()
	from := cur.AddDays(-28)
	var distM, elevM float64
	for _, w := range weeks {
		if w.WeekStart.Before(from) || !w.WeekStart.Before(cur) {
			continue
		}
		if w.Sport == store.SportRunning || w.Sport == store.SportTrailRunning {
			distM += w.DistanceM
		}
		elevM += w.ElevGainM
	}
	f.Last4WeeksAvgKm = distM / 1000 / 4
	f.Last4WeeksAvgElevM = elevM / 4
	if todayMetrics != nil {
		f.LongestRun30dKm = todayMetrics.LongestRun30dM / 1000
	}
	return f
}

// Warning is a deterministic, non-blocking fact about a proposed workout.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WarningInputs carries everything Warnings needs.
type WarningInputs struct {
	Plan                           workout.Plan
	LongestRun30dM, LongestRun30dS float64
	MaxHR90d                       *int
	SameDay                        int        // count of other workouts on Plan.Date in store.DesiredStatuses
	LastSyncAt                     *time.Time // latest successful sync of Garmin's data types
	Now                            time.Time
	WeeksOfHistory                 int
	// ZwiftUnsupported is why a cycling workout cannot be a Zwift workout
	// (zwiftworkout.Unsupported); empty when it can, or for other sports.
	ZwiftUnsupported string
}

// pct1 is a/b as a percentage rounded UP to one decimal, so a spike that only
// fires above 110% never prints as 110.0. The epsilon absorbs float noise on
// values that are exact at one decimal.
func pct1(a, b float64) string { return fmt.Sprintf("%.1f", math.Ceil(a*1000/b-1e-9)/10) }

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// noRun words a zero 30-day baseline for measure. other is the other
// baseline: when it is above 0 runs exist, they just lack this measure.
func noRun(measure string, other float64) string {
	if other > 0 {
		return "no running " + measure + " recorded in the last 30 days"
	}
	return "no running activity recorded in the last 30 days"
}

// Warnings returns warnings ordered: spike_distance, spike_duration, same_day,
// above_max_hr, zwift_not_exportable, stale_data, thin_history.
func Warnings(in WarningInputs) []Warning {
	var ws []Warning
	p := in.Plan
	isRun := p.Sport == store.SportRunning || p.Sport == store.SportTrailRunning
	if isRun && p.DistanceM > 0 {
		km := float64(p.DistanceM) / 1000
		switch {
		case in.LongestRun30dM <= 0:
			ws = append(ws, Warning{"spike_distance", fmt.Sprintf("%s; planned %.1f km", noRun("distance", in.LongestRun30dS), km)})
		case float64(p.DistanceM)*10 > 11*in.LongestRun30dM:
			ws = append(ws, Warning{"spike_distance", fmt.Sprintf(
				"planned %.1f km is %s%% of the longest run in the last 30 days (%.1f km)",
				km, pct1(float64(p.DistanceM), in.LongestRun30dM), in.LongestRun30dM/1000)})
		}
	}
	if isRun && p.EstimatedS > 0 {
		// A duration from the default pace is an estimate, and says so.
		mins := int(math.Round(float64(p.EstimatedS) / 60))
		planned, about := fmt.Sprintf("planned %d min", mins), fmt.Sprintf("planned about %d min", mins)
		if p.EstimatedFromDefaultPace {
			planned = fmt.Sprintf("estimated about %d min", mins)
			about = planned
		}
		switch {
		case in.LongestRun30dS <= 0:
			ws = append(ws, Warning{"spike_duration", fmt.Sprintf("%s; %s", noRun("duration", in.LongestRun30dM), about)})
		case float64(p.EstimatedS)*10 > 11*in.LongestRun30dS:
			ws = append(ws, Warning{"spike_duration", fmt.Sprintf(
				"%s is %s%% of the longest run duration in the last 30 days (%d min)",
				planned, pct1(float64(p.EstimatedS), in.LongestRun30dS), int(math.Round(in.LongestRun30dS/60)))})
		}
	}
	if in.SameDay > 0 {
		ws = append(ws, Warning{"same_day", fmt.Sprintf(
			"%s already approved or delivered for %s", plural(in.SameDay, "other workout"), p.Date)})
	}
	if p.MaxTargetHR != nil && in.MaxHR90d != nil && *in.MaxHR90d > 0 && *p.MaxTargetHR > *in.MaxHR90d {
		ws = append(ws, Warning{"above_max_hr", fmt.Sprintf(
			"planned target of %d bpm is above the highest heart rate in the last 90 days (%d bpm)", *p.MaxTargetHR, *in.MaxHR90d)})
	}
	if in.ZwiftUnsupported != "" {
		ws = append(ws, Warning{"zwift_not_exportable", "not exportable to Zwift: " + in.ZwiftUnsupported})
	}
	if in.LastSyncAt == nil {
		ws = append(ws, Warning{"stale_data", "no successful Garmin sync is recorded"})
	} else if age := in.Now.Sub(*in.LastSyncAt); age > 24*time.Hour {
		ws = append(ws, Warning{"stale_data", fmt.Sprintf(
			"last successful Garmin sync was more than 24 h ago (%d h)", int(math.Floor(age.Hours())))})
	}
	if in.WeeksOfHistory < 6 {
		ws = append(ws, Warning{"thin_history", fmt.Sprintf(
			"activity history covers %s", plural(in.WeeksOfHistory, "week"))})
	}
	return ws
}
