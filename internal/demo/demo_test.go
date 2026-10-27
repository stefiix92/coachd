package demo_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/demo"
	"github.com/stefiix92/coachd/internal/fitdata"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/garminkey"
	"github.com/stefiix92/coachd/internal/ingest"
	"github.com/stefiix92/coachd/internal/store"
)

var (
	_ ingest.Garmin = (*demo.Garmin)(nil)
	// A Tuesday, 21:30 in Bratislava.
	tz, _ = time.LoadLocation("Europe/Bratislava")
	now   = time.Date(2026, 10, 27, 21, 30, 0, 0, tz)
	today = civil.Of(now, tz)
	ctx   = context.Background()
)

func at(t time.Time) *demo.Garmin { return demo.New(tz, func() time.Time { return t }) }

// all lists every activity in [from, to] by paging as ingest does.
func all(t *testing.T, g *demo.Garmin, from, to civil.Date, page int) []garmin.Item[garmin.ActivityDTO] {
	t.Helper()
	var out []garmin.Item[garmin.ActivityDTO]
	for start := 0; ; {
		items, err := g.SearchActivities(ctx, from, to, start, page)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			return out
		}
		out = append(out, items...)
		start += len(items)
	}
}

func TestNoGarminKey(t *testing.T) {
	if _, ok := any(at(now)).(garminkey.Exchanger); ok {
		t.Fatal("the demo Garmin must not renew keys: the page would offer renewal")
	}
	if !today.Before(demo.KeyExpires) || demo.KeyExpires.Sub(today) < 3650 {
		t.Fatalf("KeyExpires %v is not far ahead", demo.KeyExpires)
	}
}

// The same date gives the same data, whenever it is asked for.
func TestDeterministic(t *testing.T) {
	from, to := today.AddDays(-60), today.AddDays(-1)
	a, b := at(now), at(now.Add(30*24*time.Hour))
	acts := func(g *demo.Garmin) []garmin.Item[garmin.ActivityDTO] { return all(t, g, from, to, 100) }
	// The later instance lists less of the window (history slides), so
	// compare the common, older days only through the earlier listing.
	got, again := acts(a), acts(at(now))
	if len(got) == 0 || len(got) != len(again) {
		t.Fatalf("listings differ: %d vs %d", len(got), len(again))
	}
	later := map[int64]string{}
	for _, it := range acts(b) {
		later[it.V.ActivityID] = string(it.Raw)
	}
	for i, it := range got {
		if string(it.Raw) != string(again[i].Raw) {
			t.Fatalf("activity %d differs between calls", it.V.ActivityID)
		}
		if raw, ok := later[it.V.ActivityID]; ok && raw != string(it.Raw) {
			t.Fatalf("activity %d differs a month later", it.V.ActivityID)
		}
		f1, err1 := a.DownloadActivityFIT(ctx, it.V.ActivityID)
		f2, err2 := b.DownloadActivityFIT(ctx, it.V.ActivityID)
		if err1 != nil || err2 != nil || !bytes.Equal(f1, f2) {
			t.Fatalf("FIT %d differs: %v %v", it.V.ActivityID, err1, err2)
		}
	}
	s1, _ := a.HRVRange(ctx, from, to)
	s2, _ := b.HRVRange(ctx, from, to)
	if len(s1) != 60 || len(s2) == 0 {
		t.Fatalf("hrv days: %d, %d", len(s1), len(s2))
	}
	for _, x := range s2 {
		for _, y := range s1 {
			if x.V.CalendarDate == y.V.CalendarDate && string(x.Raw) != string(y.Raw) {
				t.Fatalf("hrv %s differs a month later", x.V.CalendarDate)
			}
		}
	}
}

func TestRanges(t *testing.T) {
	g := at(now)
	first := today.AddDays(-(demo.HistoryDays - 1))
	// Asking far back and past today returns the history window only.
	acts := all(t, g, today.AddDays(-1000), today.AddDays(30), 100)
	if len(acts) == 0 {
		t.Fatal("no activities")
	}
	prev := today.AddDays(1)
	seen := map[int64]bool{}
	for _, it := range acts {
		a, err := ingest.MapActivity(it.V, tz)
		if err != nil {
			t.Fatal(err)
		}
		if a.Date.Before(first) || today.Before(a.Date) {
			t.Fatalf("activity dated %v outside [%v, %v]", a.Date, first, today)
		}
		if prev.Before(a.Date) {
			t.Fatalf("not newest first: %v after %v", a.Date, prev)
		}
		if a.StartUTC.Add(time.Duration(a.DurationS) * time.Second).After(now) {
			t.Fatalf("activity %d has not ended yet", a.ID)
		}
		if seen[a.ID] {
			t.Fatalf("activity %d listed twice", a.ID)
		}
		seen[a.ID], prev = true, a.Date
		if !strings.HasPrefix(a.NameUntrusted, "Demo ") {
			t.Fatalf("name %q", a.NameUntrusted)
		}
	}
	// A narrow range returns only its days.
	one := today.AddDays(-9)
	for _, it := range all(t, g, one, one, 100) {
		if a, _ := ingest.MapActivity(it.V, tz); a.Date != one {
			t.Fatalf("activity dated %v for %v", a.Date, one)
		}
	}
	// Paging in small pages gives the same list.
	if small := all(t, g, today.AddDays(-1000), today, 7); len(small) != len(acts) {
		t.Fatalf("paged %d, whole %d", len(small), len(acts))
	}
	sleep, err := g.SleepRange(ctx, today.AddDays(-1000), today.AddDays(5))
	if err != nil || len(sleep) != demo.HistoryDays {
		t.Fatalf("sleep days = %d, %v", len(sleep), err)
	}
	if sleep[0].V.CalendarDate != first.String() || sleep[len(sleep)-1].V.CalendarDate != today.String() {
		t.Fatalf("sleep from %s to %s", sleep[0].V.CalendarDate, sleep[len(sleep)-1].V.CalendarDate)
	}
}

// Today's activities appear once they have ended; today's daily data from
// 07:00.
func TestAppearsDuringTheDay(t *testing.T) {
	early := time.Date(today.Year, today.Month, today.Day, 5, 0, 0, 0, tz)
	if acts := all(t, at(early), today, today, 100); len(acts) != 0 {
		t.Fatalf("%d activities before any could have ended", len(acts))
	}
	for name, get := range map[string]func(*demo.Garmin) (int, error){
		"sleep": func(g *demo.Garmin) (int, error) { s, err := g.SleepRange(ctx, today, today); return len(s), err },
		"hrv":   func(g *demo.Garmin) (int, error) { s, err := g.HRVRange(ctx, today, today); return len(s), err },
		"daily": func(g *demo.Garmin) (int, error) { s, err := g.DailySummariesV2(ctx, today, today); return len(s), err },
	} {
		if n, err := get(at(early)); err != nil || n != 0 {
			t.Errorf("%s at 05:00: %d items, %v", name, n, err)
		}
		if n, err := get(at(now)); err != nil || n != 1 {
			t.Errorf("%s at 21:30: %d items, %v", name, n, err)
		}
	}
}

// Every payload maps through ingest, and the runner looks like one: four or
// five runs a week, a weekly long run, strength and an occasional ride.
func TestPlausibleAndMappable(t *testing.T) {
	g := at(now)
	from := today.AddDays(-(demo.HistoryDays - 1))
	acts := all(t, g, from, today, 100)
	weeks := float64(demo.HistoryDays) / 7
	runs, rides, strength := 0, 0, 0
	longest := map[civil.Date]float64{}
	for _, it := range acts {
		a, _ := ingest.MapActivity(it.V, tz)
		switch a.Sport {
		case store.SportRunning, store.SportTrailRunning:
			runs++
			if a.DistanceM < 3000 || a.DistanceM > 30000 || a.AvgHR == nil || *a.AvgHR < 120 || *a.AvgHR > 175 {
				t.Errorf("implausible run %+v", a)
			}
			if a.DistanceM > longest[a.WeekStart] {
				longest[a.WeekStart] = a.DistanceM
			}
			if p := a.DurationS / (a.DistanceM / 1000); p < 240 || p > 450 {
				t.Errorf("run pace %.0f s/km", p)
			}
		case store.SportCycling:
			rides++
			if a.TypeKey != "indoor_cycling" {
				t.Errorf("ride type %q", a.TypeKey)
			}
		case store.SportStrength:
			strength++
		default:
			t.Errorf("unexpected sport %q", a.Sport)
		}
		if a.Garmin.TrainingLoad == nil || a.Garmin.AerobicTE == nil {
			t.Errorf("activity %d lacks load or training effect", a.ID)
		}
	}
	if per := float64(runs) / weeks; per < 3.5 || per > 5.5 {
		t.Errorf("%.1f runs a week", per)
	}
	if rides == 0 || strength == 0 || float64(rides) > weeks/2 {
		t.Errorf("rides %d, strength %d", rides, strength)
	}
	long := 0
	for _, km := range longest {
		if km >= 12000 {
			long++
		}
	}
	if float64(long) < 0.8*weeks {
		t.Errorf("long runs in %d of %.0f weeks", long, weeks)
	}

	sleep, _ := g.SleepRange(ctx, from, today)
	hrv, _ := g.HRVRange(ctx, from, today)
	daily, _ := g.DailySummariesV2(ctx, from, today)
	weight, _ := g.WeightRange(ctx, from, today)
	if len(sleep) != demo.HistoryDays || len(hrv) != demo.HistoryDays || len(daily) != demo.HistoryDays {
		t.Fatalf("days: sleep %d hrv %d daily %d", len(sleep), len(hrv), len(daily))
	}
	if len(weight) < demo.HistoryDays/7 || len(weight) > demo.HistoryDays*2/7 {
		t.Errorf("%d weigh-ins", len(weight))
	}
	statuses := map[string]int{}
	for i := range sleep {
		s, err := ingest.MapSleep(sleep[i].V)
		if err != nil || s.SleepS == nil || *s.SleepS < 5*3600 || *s.SleepS > 10*3600 || s.Garmin.SleepScore == nil {
			t.Fatalf("sleep %s: %+v %v", sleep[i].V.CalendarDate, s, err)
		}
		h, err := ingest.MapHRV(hrv[i].V)
		if err != nil || h.HRVOvernightMs == nil || h.HRVWeeklyAvgMs == nil || h.Garmin.HRVStatus == nil ||
			h.Garmin.HRVBaselineLowMs == nil || h.Garmin.HRVBaselineHighMs == nil || *h.HRVOvernightMs < 35 || *h.HRVOvernightMs > 90 {
			t.Fatalf("hrv %s: %+v %v", hrv[i].V.CalendarDate, h, err)
		}
		statuses[*h.Garmin.HRVStatus]++
		d, err := ingest.MapDailyV2(daily[i].V)
		if err != nil || d.RestingHR == nil || *d.RestingHR < 40 || *d.RestingHR > 60 || d.SpO2Avg == nil ||
			d.Garmin.StressAvg == nil || d.Garmin.BodyBatteryMax == nil || d.Garmin.BodyBatteryMin == nil ||
			*d.Garmin.BodyBatteryMin > *d.Garmin.BodyBatteryMax {
			t.Fatalf("daily %s: %+v %v", daily[i].V.CalendarDate, d, err)
		}
	}
	if statuses["BALANCED"] < demo.HistoryDays/2 {
		t.Errorf("HRV statuses %v", statuses)
	}
	for _, it := range weight {
		w, err := ingest.MapWeight(it.V)
		if err != nil || w.WeightKg == nil || *w.WeightKg < 65 || *w.WeightKg > 78 {
			t.Fatalf("weight %+v %v", w, err)
		}
	}
}

// FIT files decode with coachd's own decoder and agree with the summary.
func TestFITDecodes(t *testing.T) {
	g := at(now)
	acts := all(t, g, today.AddDays(-27), today, 100)
	kinds, rated := map[string]bool{}, 0
	for _, it := range acts {
		a, _ := ingest.MapActivity(it.V, tz)
		b, err := g.DownloadActivityFIT(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		fa, err := fitdata.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s %d: %v", a.NameUntrusted, a.ID, err)
		}
		kinds[a.NameUntrusted] = true
		if len(fa.Points) != int(a.DurationS) || len(fa.Laps) == 0 {
			t.Fatalf("%s: %d points for %v s, %d laps", a.NameUntrusted, len(fa.Points), a.DurationS, len(fa.Laps))
		}
		dist, secs := 0.0, 0.0
		for _, l := range fa.Laps {
			dist += l.DistanceKm
			secs += l.DurationS
		}
		if math.Abs(dist*1000-a.DistanceM) > 1 || secs != a.DurationS {
			t.Fatalf("%s: laps %.3f km %v s, summary %.0f m %v s", a.NameUntrusted, dist, secs, a.DistanceM, a.DurationS)
		}
		last := fa.Points[len(fa.Points)-1]
		if last.HRBpm == nil {
			t.Fatalf("%s: no heart rate", a.NameUntrusted)
		}
		switch a.Sport {
		case store.SportRunning, store.SportTrailRunning:
			if last.PaceSPerKm == nil || last.DistanceKm == nil || last.AltitudeM == nil {
				t.Fatalf("%s: point %+v", a.NameUntrusted, last)
			}
		case store.SportStrength:
			if last.PaceSPerKm != nil || last.AltitudeM != nil {
				t.Fatalf("strength has pace or altitude: %+v", last)
			}
		}
		if fa.Perceived.FeelPct != nil {
			rated++
			if fa.Perceived.RPE == nil || *fa.Perceived.RPE < 1 || *fa.Perceived.RPE > 10 {
				t.Fatalf("rpe %v", fa.Perceived.RPE)
			}
		}
	}
	if rated == 0 || rated == len(acts) {
		t.Errorf("%d of %d activities rated; want some", rated, len(acts))
	}
	for _, k := range []string{"Demo easy run", "Demo intervals", "Demo strength"} {
		if !kinds[k] {
			t.Errorf("no %q in four weeks: %v", k, kinds)
		}
	}
}

func TestUnknownActivities(t *testing.T) {
	g := at(now)
	acts := all(t, g, today.AddDays(-6), today, 100)
	if len(acts) == 0 {
		t.Fatal("no activities this week")
	}
	id := acts[0].V.ActivityID
	if ok, err := g.ActivityExists(ctx, id); !ok || err != nil {
		t.Fatalf("listed %d: exists %v %v", id, ok, err)
	}
	for _, missing := range []int64{1, 20_000_000_000, id + 9} {
		if ok, err := g.ActivityExists(ctx, missing); ok || err != nil {
			t.Errorf("%d: exists %v %v", missing, ok, err)
		}
		if _, err := g.DownloadActivityFIT(ctx, missing); !errors.Is(err, garmin.ErrNotFound) {
			t.Errorf("%d: download err %v", missing, err)
		}
	}
	// Tomorrow's sessions do not exist yet.
	tomorrow := at(now.Add(48 * time.Hour))
	for _, it := range all(t, tomorrow, today.AddDays(1), today.AddDays(2), 100) {
		if ok, _ := g.ActivityExists(ctx, it.V.ActivityID); ok {
			t.Fatalf("future activity %d exists", it.V.ActivityID)
		}
	}
}
