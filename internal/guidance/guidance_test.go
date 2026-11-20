package guidance

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/workout"
)

func d(s string) civil.Date {
	v, err := civil.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestGoalFactsWeeksAndVolume(t *testing.T) {
	today := d("2026-10-23") // Friday; week starts 2026-10-19
	var weeks []store.WeekVolume
	for i, ws := range []string{"2026-09-21", "2026-09-28", "2026-10-05", "2026-10-12"} {
		weeks = append(weeks, store.WeekVolume{WeekStart: d(ws), Sport: store.SportRunning, DistanceM: float64(10000 * (i + 1)), ElevGainM: 100})
	}
	weeks = append(weeks,
		store.WeekVolume{WeekStart: d("2026-10-12"), Sport: store.SportTrailRunning, DistanceM: 10000, ElevGainM: 200},
		store.WeekVolume{WeekStart: d("2026-10-12"), Sport: store.SportCycling, DistanceM: 99000, ElevGainM: 300},
		store.WeekVolume{WeekStart: d("2026-10-19"), Sport: store.SportRunning, DistanceM: 50000, ElevGainM: 999}, // current week excluded
		store.WeekVolume{WeekStart: d("2026-09-14"), Sport: store.SportRunning, DistanceM: 50000, ElevGainM: 999}, // too old
	)
	tm := &store.DayMetrics{LongestRun30dM: 25000}
	f := ComputeGoalFacts(today, d("2027-01-01"), weeks, tm, store.Coverage{WeeksOfHistory: 9})
	if f.WeeksUntilGoal != 10 { // 70 days
		t.Fatalf("weeks=%d", f.WeeksUntilGoal)
	}
	if f.Last4WeeksAvgKm != (10+20+30+40+10)/4.0 {
		t.Fatalf("km=%v", f.Last4WeeksAvgKm)
	}
	if f.Last4WeeksAvgElevM != (400+200+300)/4.0 {
		t.Fatalf("elev=%v", f.Last4WeeksAvgElevM)
	}
	if f.LongestRun30dKm != 25 || f.WeeksOfHistory != 9 {
		t.Fatalf("%+v", f)
	}
	if g := ComputeGoalFacts(today, d("2026-10-24"), nil, nil, store.Coverage{}); g.WeeksUntilGoal != 1 || g.LongestRun30dKm != 0 {
		t.Fatalf("%+v", g)
	}
	if g := ComputeGoalFacts(today, d("2026-10-01"), nil, nil, store.Coverage{}); g.WeeksUntilGoal != 0 {
		t.Fatalf("past goal: %+v", g)
	}
}

func TestGoalFactsWeeksUntilGoalCeil(t *testing.T) {
	f := ComputeGoalFacts(d("2026-10-23"), d("2026-11-01"), nil, nil, store.Coverage{}) // 9 days
	if f.WeeksUntilGoal != 2 {
		t.Fatalf("%d", f.WeeksUntilGoal)
	}
}

func TestGoalFactsMissingWeeksAreZero(t *testing.T) {
	weeks := []store.WeekVolume{{WeekStart: d("2026-10-12"), Sport: store.SportRunning, DistanceM: 40000, ElevGainM: 400}}
	f := ComputeGoalFacts(d("2026-10-23"), d("2027-01-01"), weeks, nil, store.Coverage{})
	if f.Last4WeeksAvgKm != 10 || f.Last4WeeksAvgElevM != 100 {
		t.Fatalf("%+v", f)
	}
}

func run(distM, estS int) workout.Plan {
	return workout.Plan{Date: d("2026-10-25"), Sport: store.SportRunning, DistanceM: distM, EstimatedS: estS}
}

func has(ws []Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func fresh() (*time.Time, time.Time) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ls := now.Add(-time.Hour)
	return &ls, now
}

func TestSpikeDistance(t *testing.T) {
	ls, now := fresh()
	in := WarningInputs{Plan: run(32000, 0), LongestRun30dM: 25000, LongestRun30dS: 0, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	ws := Warnings(in)
	if len(ws) != 1 || ws[0].Code != "spike_distance" {
		t.Fatalf("%+v", ws)
	}
	if ws[0].Message != "planned 32.0 km is 128.0% of the longest run in the last 30 days (25.0 km)" {
		t.Fatal(ws[0].Message)
	}
	in.Plan = run(27500, 0) // exactly 1.1x: no fire
	if len(Warnings(in)) != 0 {
		t.Fatal("boundary fired")
	}
	in.Plan = run(0, 6000)
	in.LongestRun30dM, in.LongestRun30dS = 0, 4800
	ws = Warnings(in)
	if len(ws) != 1 || ws[0].Code != "spike_duration" || !strings.Contains(ws[0].Message, "100 min") || !strings.Contains(ws[0].Message, "80 min") || !strings.Contains(ws[0].Message, "125.0%") {
		t.Fatalf("%+v", ws)
	}
	in.Plan.EstimatedFromDefaultPace = true
	ws = Warnings(in)
	if len(ws) != 1 || ws[0].Message != "estimated about 100 min is 125.0% of the longest run duration in the last 30 days (80 min)" {
		t.Fatalf("%+v", ws)
	}
}

// TestSpikeNoRecentRun: a run plan with no run in the last 30 days still
// warns, with a factual message instead of a percentage.
func TestSpikeNoRecentRun(t *testing.T) {
	ls, now := fresh()
	in := WarningInputs{Plan: run(8000, 2880), LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	want := []Warning{
		{"spike_distance", "no running activity recorded in the last 30 days; planned 8.0 km"},
		{"spike_duration", "no running activity recorded in the last 30 days; planned about 48 min"},
	}
	if ws := Warnings(in); !slices.Equal(ws, want) {
		t.Fatalf("%+v", ws)
	}
	in.Plan.EstimatedFromDefaultPace = true
	want[1].Message = "no running activity recorded in the last 30 days; estimated about 48 min"
	if ws := Warnings(in); !slices.Equal(ws, want) {
		t.Fatalf("%+v", ws)
	}
	in.Plan.Sport = store.SportTrailRunning
	if ws := Warnings(in); !slices.Equal(ws, want) {
		t.Fatalf("trail: %+v", ws)
	}
	// Runs without distance (or without duration) are recorded, so the message says what is missing.
	in.Plan = run(5000, 1500)
	in.LongestRun30dS = 1800
	if ws := Warnings(in); !slices.Equal(ws, []Warning{{"spike_distance", "no running distance recorded in the last 30 days; planned 5.0 km"}}) {
		t.Fatalf("%+v", ws)
	}
	in.LongestRun30dM, in.LongestRun30dS = 5000, 0
	if ws := Warnings(in); !slices.Equal(ws, []Warning{{"spike_duration", "no running duration recorded in the last 30 days; planned about 25 min"}}) {
		t.Fatalf("%+v", ws)
	}
	// Nothing planned (open or heart-rate-bounded steps only), or not a run: no spike.
	in.LongestRun30dM = 0
	in.Plan = run(0, 0)
	if ws := Warnings(in); len(ws) != 0 {
		t.Fatalf("%+v", ws)
	}
	for _, sp := range []store.Sport{store.SportCycling, store.SportWalking, store.SportHiking, store.SportStrength, store.SportOther} {
		in.Plan = workout.Plan{Sport: sp, DistanceM: 8000, EstimatedS: 2880}
		if ws := Warnings(in); len(ws) != 0 {
			t.Fatalf("%s: %+v", sp, ws)
		}
	}
}

func TestSpikeOnlyForRuns(t *testing.T) {
	ls, now := fresh()
	for _, sp := range []store.Sport{store.SportCycling, store.SportWalking, store.SportStrength} {
		in := WarningInputs{Plan: workout.Plan{Sport: sp, DistanceM: 90000, EstimatedS: 30000}, LongestRun30dM: 1000, LongestRun30dS: 600, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
		if ws := Warnings(in); len(ws) != 0 {
			t.Fatalf("%s: %+v", sp, ws)
		}
	}
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportTrailRunning, DistanceM: 30000}, LongestRun30dM: 10000, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	if !has(Warnings(in), "spike_distance") {
		t.Fatal("trail should fire")
	}
}

func TestAboveMaxHR(t *testing.T) {
	ls, now := fresh()
	hi, mx := 192, 188
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportCycling, MaxTargetHR: &hi}, MaxHR90d: &mx, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	ws := Warnings(in)
	if len(ws) != 1 || ws[0].Code != "above_max_hr" || ws[0].Message != "planned target of 192 bpm is above the highest heart rate in the last 90 days (188 bpm)" {
		t.Fatalf("%+v", ws)
	}
	in.MaxHR90d = nil
	if has(Warnings(in), "above_max_hr") {
		t.Fatal("nil max")
	}
	eq := 188
	in.Plan.MaxTargetHR, in.MaxHR90d = &eq, &mx
	if has(Warnings(in), "above_max_hr") {
		t.Fatal("equal fired")
	}
}

func TestStaleAndThin(t *testing.T) {
	_, now := fresh()
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportCycling}, Now: now, WeeksOfHistory: 5}
	ws := Warnings(in)
	if len(ws) != 2 || ws[0].Code != "stale_data" || ws[1].Code != "thin_history" {
		t.Fatalf("%+v", ws)
	}
	old := now.Add(-25 * time.Hour)
	in.LastSyncAt = &old
	in.WeeksOfHistory = 6
	ws = Warnings(in)
	if len(ws) != 1 || ws[0].Code != "stale_data" || !strings.Contains(ws[0].Message, "(25 h)") {
		t.Fatalf("%+v", ws)
	}
	edge := now.Add(-24 * time.Hour)
	in.LastSyncAt = &edge
	if len(Warnings(in)) != 0 {
		t.Fatal("exactly 24h fired")
	}
}

func TestWarningsDeterministicOrder(t *testing.T) {
	_, now := fresh()
	hi, mx := 200, 180
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportRunning, DistanceM: 30000, EstimatedS: 9000, MaxTargetHR: &hi}, LongestRun30dM: 10000, LongestRun30dS: 3000, MaxHR90d: &mx, SameDay: 2, Now: now, WeeksOfHistory: 1,
		ZwiftUnsupported: "steps[0].duration: distance durations have no Zwift equivalent"}
	want := []string{"spike_distance", "spike_duration", "same_day", "above_max_hr", "zwift_not_exportable", "stale_data", "thin_history"}
	for i := 0; i < 3; i++ {
		ws := Warnings(in)
		if len(ws) != len(want) {
			t.Fatalf("%+v", ws)
		}
		for j, c := range want {
			if ws[j].Code != c {
				t.Fatalf("%d: %s != %s", j, ws[j].Code, c)
			}
		}
	}
}

func TestNoVerdictWords(t *testing.T) {
	_, now := fresh()
	hi, mx := 200, 180
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportRunning, DistanceM: 30000, EstimatedS: 9000, MaxTargetHR: &hi}, LongestRun30dM: 10000, LongestRun30dS: 3000, MaxHR90d: &mx, SameDay: 2, Now: now, WeeksOfHistory: 1,
		ZwiftUnsupported: "steps[1].steps[0].duration: open durations have no Zwift equivalent"}
	all := Warnings(in)
	in.Plan.EstimatedFromDefaultPace = true
	all = append(all, Warnings(in)...)
	in.LongestRun30dM, in.LongestRun30dS = 0, 0
	all = append(all, Warnings(in)...)
	in.LongestRun30dS = 3000
	all = append(all, Warnings(in)...)
	in.LongestRun30dM, in.LongestRun30dS = 10000, 0
	all = append(all, Warnings(in)...)
	for _, w := range all {
		words := strings.FieldsFunc(strings.ToLower(w.Message), func(r rune) bool { return r < 'a' || r > 'z' })
		for _, bad := range []string{"should", "ready", "too", "risky", "recommend", "safe", "dangerous", "must", "need"} {
			for _, word := range words {
				if word == bad {
					t.Errorf("%s: verdict word %q in %q", w.Code, bad, w.Message)
				}
			}
		}
	}
}

func TestSameDayMessageAndPlural(t *testing.T) {
	ls, now := fresh()
	in := WarningInputs{Plan: workout.Plan{Date: d("2026-10-25"), Sport: store.SportCycling}, SameDay: 1, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	ws := Warnings(in)
	if len(ws) != 1 || ws[0].Message != "1 other workout already approved or delivered for 2026-10-25" {
		t.Fatalf("%+v", ws)
	}
	in.SameDay = 2
	if m := Warnings(in)[0].Message; m != "2 other workouts already approved or delivered for 2026-10-25" {
		t.Fatal(m)
	}
	in.SameDay, in.WeeksOfHistory = 0, 1
	if m := Warnings(in)[0].Message; m != "activity history covers 1 week" {
		t.Fatal(m)
	}
	in.WeeksOfHistory = 0
	if m := Warnings(in)[0].Message; m != "activity history covers 0 weeks" {
		t.Fatal(m)
	}
}

func TestSpikeBoundaryExactAndFloor(t *testing.T) {
	ls, now := fresh()
	// 1.1*30000 is inexact in float; exactly 110% must not fire.
	in := WarningInputs{Plan: run(33000, 0), LongestRun30dM: 30000, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	if ws := Warnings(in); len(ws) != 0 {
		t.Fatalf("%+v", ws)
	}
	in.Plan = run(33001, 0)
	ws := Warnings(in)
	if len(ws) != 1 || !strings.Contains(ws[0].Message, "110.1% of") {
		t.Fatalf("%+v", ws)
	}
}

func TestStaleFlooredAndZeroMaxHR(t *testing.T) {
	_, now := fresh()
	old := now.Add(-24*time.Hour - 40*time.Minute)
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportCycling}, LastSyncAt: &old, Now: now, WeeksOfHistory: 10}
	if m := Warnings(in)[0].Message; m != "last successful Garmin sync was more than 24 h ago (24 h)" {
		t.Fatal(m)
	}
	hi, zero := 150, 0
	ls := now
	in = WarningInputs{Plan: workout.Plan{Sport: store.SportCycling, MaxTargetHR: &hi}, MaxHR90d: &zero, LastSyncAt: &ls, Now: now, WeeksOfHistory: 10}
	if len(Warnings(in)) != 0 {
		t.Fatal("zero max HR fired")
	}
}

func TestSpikeDurationBoundary(t *testing.T) {
	ls, now := fresh()
	in := WarningInputs{Plan: run(0, 5280), LongestRun30dS: 4800, LastSyncAt: ls, Now: now, WeeksOfHistory: 10}
	if ws := Warnings(in); len(ws) != 0 {
		t.Fatalf("exact 110%% fired: %+v", ws)
	}
	in.Plan = run(0, 5281)
	ws := Warnings(in)
	if len(ws) != 1 || ws[0].Code != "spike_duration" || !strings.Contains(ws[0].Message, "110.1% of") {
		t.Fatalf("%+v", ws)
	}
}

func TestZwiftNotExportable(t *testing.T) {
	ls, now := fresh()
	in := WarningInputs{Plan: workout.Plan{Sport: store.SportCycling}, LastSyncAt: ls, Now: now, WeeksOfHistory: 10,
		ZwiftUnsupported: "steps[2].duration: distance durations have no Zwift equivalent"}
	ws := Warnings(in)
	if len(ws) != 1 || ws[0].Code != "zwift_not_exportable" ||
		ws[0].Message != "not exportable to Zwift: steps[2].duration: distance durations have no Zwift equivalent" {
		t.Fatalf("%+v", ws)
	}
	in.ZwiftUnsupported = ""
	if ws := Warnings(in); len(ws) != 0 {
		t.Fatalf("exportable: %+v", ws)
	}
}
