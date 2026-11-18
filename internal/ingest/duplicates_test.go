package ingest

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/store"
)

var dupBase = time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)

// dupAct is a stored activity starting offMin after dupBase and lasting durS.
func dupAct(id int64, src store.Source, sport store.Sport, offMin int, durS float64, power, hr bool) store.Activity {
	a := store.Activity{
		ID: id, Source: src, Sport: sport, StartUTC: dupBase.Add(time.Duration(offMin) * time.Minute),
		DurationS: durS, HasPower: power,
	}
	if hr {
		a.AvgHR = fp(140)
	}
	return a
}

func TestResolveDuplicates(t *testing.T) {
	const G, I = store.SourceGarmin, store.SourceIntervals
	const ride = store.SportCycling
	tests := []struct {
		name string
		acts []store.Activity
		want map[int64]int64 // loser -> winner; absent = shown
	}{
		{"garmin no power, intervals power: garmin hidden",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, false, true), dupAct(-2, I, ride, 0, 3600, true, true)},
			map[int64]int64{1: -2}},
		{"both power: intervals hidden",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(-2, I, ride, 0, 3600, true, true)},
			map[int64]int64{-2: 1}},
		{"intervals alone has hr: garmin hidden",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, false, false), dupAct(-2, I, ride, 0, 3600, false, true)},
			map[int64]int64{1: -2}},
		{"all equal: intervals hidden",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, false, true), dupAct(-2, I, ride, 0, 3600, false, true)},
			map[int64]int64{-2: 1}},
		{"different sport",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, false, true), dupAct(-2, I, store.SportRunning, 0, 3600, true, true)},
			nil},
		{"40 percent of the shorter: none",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(-2, I, ride, 36, 3600, true, true)},
			nil},
		{"exactly 50 percent of the shorter: pair",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(-2, I, ride, 30, 3600, true, true)},
			map[int64]int64{-2: 1}},
		{"back to back rides",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(-2, I, ride, 60, 3600, true, true)},
			nil},
		{"two intervals candidates: larger overlap pairs",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(-2, I, ride, 20, 3600, true, true), dupAct(-3, I, ride, 5, 3600, true, true)},
			map[int64]int64{-3: 1}},
		{"two garmin, one intervals: one pair",
			[]store.Activity{dupAct(1, G, ride, 0, 3600, true, true), dupAct(2, G, ride, 0, 3600, true, true), dupAct(-3, I, ride, 0, 3600, true, true)},
			map[int64]int64{-3: 1}},
		{"tie: lower start, then lower absolute id",
			[]store.Activity{dupAct(1, G, ride, 10, 1200, true, true), dupAct(-9, I, ride, 0, 3600, true, true), dupAct(-5, I, ride, 0, 3600, true, true)},
			map[int64]int64{-5: 1}},
		{"zero duration",
			[]store.Activity{dupAct(1, G, ride, 0, 0, true, true), dupAct(-2, I, ride, 0, 0, true, true)},
			nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check := func(acts []store.Activity) {
				t.Helper()
				got := resolveDuplicates(acts)
				for _, a := range acts {
					v, ok := got[a.ID]
					if !ok {
						t.Fatalf("no entry for %d", a.ID)
					}
					w, hidden := tc.want[a.ID]
					switch {
					case hidden && (v == nil || *v != w):
						t.Errorf("%d hidden by %v, want %d", a.ID, v, w)
					case !hidden && v != nil:
						t.Errorf("%d hidden by %d, want shown", a.ID, *v)
					}
				}
			}
			check(tc.acts)
			rng := rand.New(rand.NewSource(1))
			for range 10 {
				sh := append([]store.Activity(nil), tc.acts...)
				rng.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
				check(sh)
			}
		})
	}
}

// dupSetup has a Garmin ride (id 5001) and an intervals.icu ride (i777) of the
// same session on 2026-10-22; garminPower is the Garmin copy's average power.
func dupSetup(t *testing.T, garminPower *float64, rec *logRecorder) (*env, *fakeIntervals) {
	t.Helper()
	g := seededFake(t) // its run keeps a listed Garmin activity as reconcile's canary
	dto := activityDTO(5001, "cycling", "2026-10-22 18:00:00", "2026-10-22 16:00:00", 30000, false)
	dto.Duration = fp(3600)
	dto.AvgPower = garminPower
	g.activities = append(g.activities, item(t, dto))
	fi := newFakeIntervals()
	fi.setItems(intervalsItem(t, rideOn("i777", civil.Date{Year: 2026, Month: 10, Day: 22})))
	if rec == nil {
		return newIntervalsEnv(t, g, fi, nil), fi
	}
	return newIntervalsEnv(t, g, fi, rec), fi
}

func withoutGarmin(items []garmin.Item[garmin.ActivityDTO], id int64) []garmin.Item[garmin.ActivityDTO] {
	return slices.DeleteFunc(slices.Clone(items), func(it garmin.Item[garmin.ActivityDTO]) bool { return it.V.ActivityID == id })
}

func shownIDs(t *testing.T, e *env) []int64 {
	t.Helper()
	acts, _, err := e.db.ListActivities(context.Background(), store.ActivityQuery{
		From: civil.Date{Year: 2026, Month: 10, Day: 22}, To: civil.Date{Year: 2026, Month: 10, Day: 22}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range acts {
		if a.Sport != store.SportCycling {
			continue // the seeded run is no part of the pair
		}
		ids = append(ids, a.ID)
	}
	return ids
}

func TestSyncHidesDuplicate(t *testing.T) {
	e, _ := dupSetup(t, nil, nil)
	mustSync(t, e)
	if ids := shownIDs(t, e); len(ids) != 1 || ids[0] != -777 {
		t.Errorf("shown = %v, want only the intervals copy -777 (it has power)", ids)
	}
	var by sql.NullInt64
	if err := rawDB(t, e).QueryRow(`SELECT hidden_by FROM activities WHERE id=5001`).Scan(&by); err != nil || !by.Valid || by.Int64 != -777 {
		t.Errorf("garmin copy hidden_by = %v, %v, want -777", by, err)
	}
}

func TestSyncDuplicateFlipsAndClears(t *testing.T) {
	e, _ := dupSetup(t, nil, nil)
	mustSync(t, e)
	// Garmin copy gains power: tie on power, Garmin wins, intervals hidden.
	dto := activityDTO(5001, "cycling", "2026-10-22 18:00:00", "2026-10-22 16:00:00", 30000, false)
	dto.Duration, dto.AvgPower = fp(3600), fp(200)
	e.fake.activities = append(withoutGarmin(e.fake.activities, 5001), item(t, dto))
	e.clock.Set(testNow.Add(time.Hour))
	mustSync(t, e)
	if ids := shownIDs(t, e); len(ids) != 1 || ids[0] != 5001 {
		t.Errorf("after flip shown = %v, want [5001]", ids)
	}
	// The winner disappears at its source: the loser shows again.
	e.fake.activities = withoutGarmin(e.fake.activities, 5001)
	e.fake.deleted[5001] = true
	e.clock.Set(testNow.Add(2 * time.Hour))
	mustSync(t, e)
	if ids := shownIDs(t, e); len(ids) != 1 || ids[0] != -777 {
		t.Errorf("after delete shown = %v, want [-777]", ids)
	}
	a, _ := e.db.GetActivity(context.Background(), -777)
	if a.HiddenBy != nil {
		t.Errorf("intervals copy still hidden by %d", *a.HiddenBy)
	}
}

func TestSyncDuplicatesSkippedOnFailure(t *testing.T) {
	e, _ := dupSetup(t, nil, nil)
	e.fake.setErr(mSearch, errors.New("boom"))
	// Seed both copies directly, unmarked, then sync with Garmin failing.
	seedDup(t, e)
	res := mustSync(t, e)
	if res.Outcomes[TypeActivities] == OutcomeOK {
		t.Fatalf("garmin activities outcome ok, want failure")
	}
	for _, id := range []int64{5001, -777} {
		a, err := e.db.GetActivity(context.Background(), id)
		if err != nil || a.HiddenBy != nil {
			t.Errorf("%d = hidden %v, %v, want unmarked", id, a.HiddenBy, err)
		}
	}
}

func TestSyncDuplicatesIdempotent(t *testing.T) {
	rec := newLogRecorder()
	e, _ := dupSetup(t, nil, rec)
	mustSync(t, e)
	e.clock.Set(testNow.Add(time.Hour))
	mustSync(t, e)
	lines := rec.lines(t, "duplicates resolved")
	if len(lines) != 2 || lines[0].attrs["count"] != "1" || lines[1].attrs["count"] != "0" ||
		lines[0].attrs["data_type"] != TypeIntervals {
		t.Errorf("log lines = %+v, want counts 1 then 0", lines)
	}
}

// seedDup stores both copies of the session, unmarked.
func seedDup(t *testing.T, e *env) {
	t.Helper()
	dto := activityDTO(5001, "cycling", "2026-10-22 18:00:00", "2026-10-22 16:00:00", 30000, false)
	dto.Duration = fp(3600)
	ga, err := MapActivity(dto, e.svc.opts.TZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpsertActivity(context.Background(), ga, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	storeIntervals(t, e, civil.Date{Year: 2026, Month: 10, Day: 22}, "i777")
}
