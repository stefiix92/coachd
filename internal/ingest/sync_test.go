package ingest

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
)

var allTypes = []string{"activities", "sleep", "hrv", "daily", "weight"}

type counts struct{ activities, health, raw int }

func rowCounts(t *testing.T, e *env) counts {
	t.Helper()
	ctx := context.Background()
	acts, _, err := e.db.ListActivities(ctx, store.ActivityQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	health, err := e.db.ListDailyHealth(ctx, civil.Date{Year: 2000, Month: 1, Day: 1}, civil.Date{Year: 2100, Month: 1, Day: 1})
	if err != nil {
		t.Fatal(err)
	}
	return counts{activities: len(acts), health: len(health), raw: rawCount(t, e, "")}
}

// rawCount counts raw_payloads rows, optionally for one source.
func rawCount(t *testing.T, e *env, source string) int {
	t.Helper()
	sdb, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "coachd.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var n int
	q, args := `SELECT count(*) FROM raw_payloads`, []any{}
	if source != "" {
		q, args = q+` WHERE source = ?`, []any{source}
	}
	if err := sdb.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func syncState(t *testing.T, e *env, dt string) store.SyncState {
	t.Helper()
	st, err := e.db.GetSyncState(context.Background(), dt)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustSync(t *testing.T, e *env) SyncResult {
	t.Helper()
	res, err := e.svc.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSyncStoresActivitiesAndHealth(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	ctx := context.Background()
	res := mustSync(t, e)
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeOK {
			t.Errorf("outcome[%s] = %q, want ok", dt, res.Outcomes[dt])
		}
		st := syncState(t, e, dt)
		if st.DataType != dt || st.LastOutcome != OutcomeOK || st.LastAttemptAt == nil || !st.LastAttemptAt.Equal(testNow) ||
			st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(testNow) || st.FreshestDataAt == nil {
			t.Errorf("state[%s] = %+v", dt, st)
		}
	}

	// Windows with no state: activities today−14, daily types today−13 .. today.
	// Activities take two requests: the page and the empty page that ends paging.
	if r := e.fake.ranges[mSearch]; len(r) != 2 || r[0] != (dateRange{testToday.AddDays(-14), testToday}) || r[1] != r[0] {
		t.Errorf("activity ranges = %v", r)
	}
	for _, m := range []string{mSleep, mHRV, mDaily, mWeight} {
		if r := e.fake.ranges[m]; len(r) != 1 || r[0] != (dateRange{testToday.AddDays(-13), testToday}) {
			t.Errorf("%s ranges = %v", m, r)
		}
	}

	run, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if run.Sport != store.SportRunning || run.DistanceM != 10000 || run.Date != (civil.Date{Year: 2026, Month: 10, Day: 22}) ||
		run.FITStatus != store.FITPending {
		t.Errorf("run = %+v", run)
	}
	manual, err := e.db.GetActivity(ctx, idManual)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Sport != store.SportStrength || !manual.Manual || manual.FITStatus != store.FITUnavailable {
		t.Errorf("manual = %+v", manual)
	}
	if got := rowCounts(t, e); got != (counts{activities: 3, health: 4, raw: 4 * 4}) {
		t.Errorf("counts = %+v", got)
	}
	for _, src := range []string{"sleep", "hrv", "daily", "weight"} {
		if n := rawCount(t, e, src); n != 4 {
			t.Errorf("raw[%s] = %d, want 4", src, n)
		}
	}

	h, err := e.db.ListDailyHealth(ctx, testToday, testToday)
	if err != nil || len(h) != 1 {
		t.Fatalf("health today: %v %v", h, err)
	}
	d := h[0]
	if d.SleepS == nil || *d.SleepS != 27180 || d.HRVOvernightMs == nil || *d.HRVOvernightMs != 53 ||
		d.RestingHR == nil || *d.RestingHR != 51 || d.SpO2Avg == nil || *d.SpO2Avg != 96 ||
		d.WeightKg == nil || *d.WeightKg != 79.3 || d.Garmin.SleepScore == nil || *d.Garmin.SleepScore != 83 {
		t.Errorf("health today = %+v", d)
	}

	wantAct := time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC)
	if st := syncState(t, e, "activities"); !st.FreshestDataAt.Equal(wantAct) {
		t.Errorf("activities freshest = %v, want %v", st.FreshestDataAt, wantAct)
	}
	// Daily freshest: today at local midnight (CEST, UTC+2) as UTC.
	wantDay := time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC)
	if st := syncState(t, e, "sleep"); !st.FreshestDataAt.Equal(wantDay) {
		t.Errorf("sleep freshest = %v, want %v", st.FreshestDataAt, wantDay)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	mustSync(t, e)
	first := rowCounts(t, e)
	if first.activities == 0 || first.health == 0 || first.raw == 0 {
		t.Fatalf("first sync stored nothing: %+v", first)
	}
	e.clock.Set(testNow.Add(time.Hour))
	mustSync(t, e)
	if second := rowCounts(t, e); second != first {
		t.Fatalf("counts after second sync = %+v, want %+v", second, first)
	}
}

func TestSyncWindows(t *testing.T) {
	for _, tc := range []struct {
		name               string
		lastSuccess        *time.Time
		actFrom, dailyFrom civil.Date
		catchUp            bool
	}{
		// No state: 14-day windows (today−14 .. today, today−13 .. today).
		{"no state", nil, testToday.AddDays(-14), testToday.AddDays(-13), false},
		// Recent success: the minimum windows still apply, so late uploads are refetched.
		{"success 2 days ago", ptime(testNow.AddDate(0, 0, -2)), testToday.AddDays(-14), testToday.AddDays(-13), false},
		// Long gap: activities from last−2, daily from last−3.
		{"success 20 days ago", ptime(testNow.AddDate(0, 0, -20)), testToday.AddDays(-22), testToday.AddDays(-23), false},
		// Very long gap: daily clamped to today−27 (28-day range cap); activities unclamped.
		{"success 60 days ago", ptime(testNow.AddDate(0, 0, -60)), testToday.AddDays(-62), testToday.AddDays(-27), true},
		// Clock moved backwards: minimum windows.
		{"success in the future", ptime(testNow.AddDate(0, 0, 5)), testToday.AddDays(-14), testToday.AddDays(-13), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, seededFake(t), nil)
			if tc.lastSuccess != nil {
				for _, dt := range allTypes {
					if err := e.db.PutSyncState(context.Background(), store.SyncState{
						DataType: dt, LastAttemptAt: tc.lastSuccess, LastSuccessAt: tc.lastSuccess, LastOutcome: OutcomeOK,
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			mustSync(t, e)
			if r := e.fake.ranges[mSearch]; len(r) != 2 || r[0] != (dateRange{tc.actFrom, testToday}) || r[1] != r[0] {
				t.Errorf("activity ranges = %v, want 2 x {%v %v}", r, tc.actFrom, testToday)
			}
			for _, m := range []string{mSleep, mHRV, mDaily, mWeight} {
				r := e.fake.ranges[m]
				if tc.catchUp {
					// Catch-up chunks come first; the normal window is last.
					if len(r) < 2 || r[len(r)-1] != (dateRange{tc.dailyFrom, testToday}) {
						t.Errorf("%s ranges = %v, want catch-up then {%v %v}", m, r, tc.dailyFrom, testToday)
					}
				} else if len(r) != 1 || r[0] != (dateRange{tc.dailyFrom, testToday}) {
					t.Errorf("%s ranges = %v, want [{%v %v}]", m, r, tc.dailyFrom, testToday)
				}
			}
		})
	}
}

func ptime(t time.Time) *time.Time { return &t }

func TestSyncPaginatesActivities(t *testing.T) {
	f := newFake()
	for i := 0; i < 250; i++ {
		f.activities = append(f.activities, item(t, activityDTO(int64(5000+i), "running",
			"2026-10-20 07:00:00", "2026-10-20 05:00:00", 1000, false)))
	}
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	if res.Outcomes["activities"] != OutcomeOK || res.Items["activities"] != 250 {
		t.Fatalf("outcome = %q items = %d", res.Outcomes["activities"], res.Items["activities"])
	}
	// Pages are capped at 99 whatever the limit: paging ends on an empty page.
	if n := f.count(mSearch); n != 4 {
		t.Fatalf("search calls = %d, want 4 (99+99+52+0)", n)
	}
	if got := rowCounts(t, e).activities; got != 250 {
		t.Fatalf("activities = %d, want 250 (none skipped between pages)", got)
	}
}

func TestSyncSkipsDailyItemWithBadDate(t *testing.T) {
	f := seededFake(t)
	f.sleep = append(f.sleep, item(t, sleepDTO("not-a-date", 1, 1)))
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	if res.Outcomes["sleep"] != OutcomeOK || res.Items["sleep"] != 4 {
		t.Fatalf("sleep outcome = %q items = %d", res.Outcomes["sleep"], res.Items["sleep"])
	}
	if n := rawCount(t, e, "sleep"); n != 4 {
		t.Fatalf("raw sleep = %d, want 4", n)
	}
}

func TestSyncManualActivityNoFITRetryLoop(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	e.svc.drainFIT(ctx)
	e.clock.Set(testNow.Add(time.Hour))
	mustSync(t, e) // re-upserting must not reset FIT state
	e.svc.drainFIT(ctx)

	if n := e.fake.fitCount(idManual); n != 0 {
		t.Errorf("manual activity FIT requested %d times, want 0", n)
	}
	if n := e.fake.fitCount(idGone); n != 1 {
		t.Errorf("404 activity FIT requested %d times, want 1", n)
	}
	if n := e.fake.fitCount(idRun); n != 1 {
		t.Errorf("stored activity FIT requested %d times, want 1", n)
	}
	for id, want := range map[int64]store.FITStatus{idRun: store.FITStored, idManual: store.FITUnavailable, idGone: store.FITUnavailable} {
		a, err := e.db.GetActivity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.FITStatus != want {
			t.Errorf("activity %d fit = %q, want %q", id, a.FITStatus, want)
		}
	}
}

func TestSyncAuthFailedNoRetryStorm(t *testing.T) {
	f := seededFake(t)
	for _, m := range []string{mSearch, mSleep, mHRV, mDaily, mWeight} {
		f.setErr(m, garmin.ErrUnauthorized)
	}
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeAuthFailed {
			t.Errorf("outcome[%s] = %q, want auth_failed", dt, res.Outcomes[dt])
		}
		st := syncState(t, e, dt)
		if st.LastOutcome != OutcomeAuthFailed || st.LastSuccessAt != nil || st.LastAttemptAt == nil {
			t.Errorf("state[%s] = %+v", dt, st)
		}
	}
	for _, m := range []string{mSearch, mSleep, mHRV, mDaily, mWeight} {
		if n := f.count(m); n != 1 {
			t.Errorf("%s calls = %d, want 1", m, n)
		}
	}
	if m, err := e.db.ListDayMetrics(context.Background(), testToday, testToday); err != nil || len(m) != 0 {
		t.Errorf("no type ok: summaries must not be recomputed, got %v %v", m, err)
	}
}

func TestSyncTypesAreIndependent(t *testing.T) {
	f := seededFake(t)
	f.setErr(mSleep, garmin.ErrUnauthorized)
	f.setErr(mHRV, &garmin.StatusError{Endpoint: "hrv", Status: 500})
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	want := map[string]string{"activities": OutcomeOK, "sleep": OutcomeAuthFailed, "hrv": OutcomeError, "daily": OutcomeOK, "weight": OutcomeOK}
	for dt, o := range want {
		if res.Outcomes[dt] != o {
			t.Errorf("outcome[%s] = %q, want %q", dt, res.Outcomes[dt], o)
		}
	}
	if st := syncState(t, e, "hrv"); st.LastOutcome != OutcomeError || st.LastSuccessAt != nil {
		t.Errorf("hrv state = %+v", st)
	}
}

func TestSyncRateLimitedPauses(t *testing.T) {
	f := seededFake(t)
	retryAt := testNow.Add(time.Hour)
	f.setErr(mSearch, &garmin.RateLimitedError{RetryAt: retryAt})
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeRateLimited {
			t.Errorf("outcome[%s] = %q, want garmin_rate_limited", dt, res.Outcomes[dt])
		}
		if st := syncState(t, e, dt); st.LastOutcome != OutcomeRateLimited {
			t.Errorf("state[%s] outcome = %q", dt, st.LastOutcome)
		}
	}
	if n := f.totalCalls(); n != 1 {
		t.Fatalf("calls after rate limit = %d, want 1 (remaining types skipped)", n)
	}

	f.resetCalls()
	f.setErr(mSearch, nil)
	e.clock.Set(retryAt.Add(-time.Second))
	res = mustSync(t, e)
	e.svc.drainFIT(context.Background())
	if n := f.totalCalls(); n != 0 {
		t.Fatalf("calls while paused = %d, want 0", n)
	}
	if res.Outcomes["activities"] != OutcomeRateLimited {
		t.Fatalf("paused outcome = %q", res.Outcomes["activities"])
	}
	if r := e.svc.RequestSync(context.Background()); r.Outcome != "garmin_rate_limited" || !r.NextAllowedAt.Equal(retryAt) {
		t.Fatalf("RequestSync while paused = %+v, want garmin_rate_limited until %v", r, retryAt)
	}
	if n := f.totalCalls(); n != 0 {
		t.Fatalf("RequestSync while paused made %d calls", n)
	}

	e.clock.Set(retryAt)
	res = mustSync(t, e)
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeOK {
			t.Errorf("after pause outcome[%s] = %q, want ok", dt, res.Outcomes[dt])
		}
	}
}

func TestSyncRateLimitedInPastStillSkipsRemainingTypes(t *testing.T) {
	f := seededFake(t)
	f.setErr(mSleep, &garmin.RateLimitedError{RetryAt: testNow.Add(-time.Minute)})
	e := newEnv(t, f, nil)
	res := mustSync(t, e)
	if res.Outcomes["activities"] != OutcomeOK || res.Outcomes["sleep"] != OutcomeRateLimited ||
		res.Outcomes["hrv"] != OutcomeRateLimited || res.Outcomes["weight"] != OutcomeRateLimited {
		t.Fatalf("outcomes = %v", res.Outcomes)
	}
	if n := f.count(mHRV) + f.count(mDaily) + f.count(mWeight); n != 0 {
		t.Fatalf("types after the 429 made %d calls, want 0", n)
	}
}

func TestSyncIntervalsOff(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	res := mustSync(t, e)
	if o, ok := res.Outcomes[TypeIntervals]; ok {
		t.Errorf("outcome[%s] = %q, want no entry", TypeIntervals, o)
	}
	if len(res.Outcomes) != len(allTypes) {
		t.Errorf("outcomes = %v, want the Garmin types only", res.Outcomes)
	}
	sts, err := e.db.ListSyncStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sts {
		if st.DataType == TypeIntervals {
			t.Errorf("sync_state row %+v, want none", st)
		}
	}
}

func TestSyncGarmin429DoesNotSkipIntervals(t *testing.T) {
	g := seededFake(t)
	retryAt := testNow.Add(time.Hour)
	g.setErr(mSearch, &garmin.RateLimitedError{RetryAt: retryAt})
	fi := newFakeIntervals()
	fi.setItems(intervalsItem(t, rideOn("i55751783", testToday.AddDays(-1))))
	e := newIntervalsEnv(t, g, fi, nil)
	res := mustSync(t, e)
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeRateLimited {
			t.Errorf("outcome[%s] = %q, want garmin_rate_limited", dt, res.Outcomes[dt])
		}
	}
	if res.Outcomes[TypeIntervals] != OutcomeOK || res.Items[TypeIntervals] != 1 || fi.count(miList) != 1 {
		t.Fatalf("intervals outcome = %q items = %d list calls = %d, want ok 1 1",
			res.Outcomes[TypeIntervals], res.Items[TypeIntervals], fi.count(miList))
	}
	if _, err := e.db.GetActivity(context.Background(), -55751783); err != nil {
		t.Fatalf("intervals activity: %v", err)
	}

	// Still inside Garmin's pause: Garmin makes no call, intervals.icu syncs.
	g.resetCalls()
	e.clock.Set(retryAt.Add(-time.Minute))
	res = mustSync(t, e)
	if n := g.totalCalls(); n != 0 {
		t.Errorf("Garmin calls while paused = %d, want 0", n)
	}
	if res.Outcomes[TypeIntervals] != OutcomeOK || fi.count(miList) != 2 {
		t.Errorf("intervals outcome = %q list calls = %d, want ok 2", res.Outcomes[TypeIntervals], fi.count(miList))
	}
}

func TestSyncIntervals429DoesNotTouchGarmin(t *testing.T) {
	fi := newFakeIntervals()
	retryAt := testNow.Add(time.Hour)
	fi.setErr(miList, &intervals.RateLimitedError{RetryAt: retryAt})
	rec := newLogRecorder()
	e := newIntervalsEnv(t, seededFake(t), fi, rec)
	res := mustSync(t, e)
	if res.Outcomes[TypeIntervals] != OutcomeIntervalsRateLimited {
		t.Fatalf("intervals outcome = %q, want rate_limited", res.Outcomes[TypeIntervals])
	}
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeOK {
			t.Errorf("outcome[%s] = %q, want ok", dt, res.Outcomes[dt])
		}
	}
	if st := syncState(t, e, TypeIntervals); st.LastOutcome != OutcomeIntervalsRateLimited || st.LastSuccessAt != nil {
		t.Errorf("intervals state = %+v", st)
	}
	if _, paused := e.svc.pausedUntil(); paused {
		t.Error("Garmin paused by an intervals.icu 429")
	}
	if until, paused := e.svc.intervalsPausedUntil(); !paused || !until.Equal(retryAt) {
		t.Errorf("intervals paused until %v (%v), want %v", until, paused, retryAt)
	}
	if n := len(rec.lines(t, "garmin rate limited")); n != 0 {
		t.Errorf("garmin rate limited logged %d times, want 0", n)
	}

	// Inside the pause: no intervals.icu call, Garmin types still sync.
	fi.setErr(miList, nil)
	e.clock.Set(retryAt.Add(-time.Minute))
	res = mustSync(t, e)
	if res.Outcomes[TypeIntervals] != OutcomeIntervalsRateLimited || fi.count(miList) != 1 {
		t.Errorf("paused: intervals outcome = %q list calls = %d, want rate_limited 1", res.Outcomes[TypeIntervals], fi.count(miList))
	}
	if res.Outcomes[TypeSleep] != OutcomeOK || res.Outcomes[TypeHRV] != OutcomeOK {
		t.Errorf("paused: sleep/hrv = %q %q, want ok", res.Outcomes[TypeSleep], res.Outcomes[TypeHRV])
	}

	e.clock.Set(retryAt)
	res = mustSync(t, e)
	if res.Outcomes[TypeIntervals] != OutcomeOK || fi.count(miList) != 2 {
		t.Errorf("after pause: intervals outcome = %q list calls = %d, want ok 2", res.Outcomes[TypeIntervals], fi.count(miList))
	}
}

func TestSyncIntervalsUnauthorizedLeavesGarminOK(t *testing.T) {
	fi := newFakeIntervals()
	fi.setErr(miList, intervals.ErrUnauthorized)
	e := newIntervalsEnv(t, seededFake(t), fi, nil)
	storeIntervals(t, e, testToday.AddDays(-2), "i101") // unlisted, but the listing failed
	res := mustSync(t, e)
	if res.Outcomes[TypeIntervals] != OutcomeAuthFailed {
		t.Fatalf("intervals outcome = %q, want auth_failed", res.Outcomes[TypeIntervals])
	}
	for _, dt := range allTypes {
		if res.Outcomes[dt] != OutcomeOK {
			t.Errorf("outcome[%s] = %q, want ok", dt, res.Outcomes[dt])
		}
	}
	if st := syncState(t, e, TypeIntervals); st.LastOutcome != OutcomeAuthFailed || st.LastSuccessAt != nil || st.LastAttemptAt == nil {
		t.Errorf("intervals state = %+v", st)
	}
	if p := fi.probed(); len(p) != 0 {
		t.Errorf("probes after a failed listing = %v, want none", p)
	}
	if _, err := e.db.GetActivity(context.Background(), -101); err != nil {
		t.Errorf("stored activity: %v, want kept", err)
	}
}

// blockFirst makes the first call to method signal entered and wait for release.
func blockFirst(f *fakeGarmin, method string) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.setHook(func(m string) {
		if m != method {
			return
		}
		once.Do(func() {
			close(entered)
			<-release
		})
	})
	return entered, release
}

func TestConcurrentSyncAlreadyRunning(t *testing.T) {
	f := seededFake(t)
	entered, release := blockFirst(f, mSearch)
	e := newEnv(t, f, nil)

	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Sync(context.Background())
		done <- err
	}()
	<-entered
	if _, err := e.svc.Sync(context.Background()); !errors.Is(err, ErrSyncRunning) {
		t.Errorf("concurrent Sync err = %v, want ErrSyncRunning", err)
	}
	if r := e.svc.RequestSync(context.Background()); r.Outcome != "already_running" {
		t.Errorf("concurrent RequestSync = %q, want already_running", r.Outcome)
	}
	if n := f.count(mSearch); n != 1 {
		t.Errorf("search calls = %d, want 1", n)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if r := e.svc.RequestSync(context.Background()); r.Outcome != "synced" {
		t.Fatalf("RequestSync after sync finished = %q, want synced (already_running must not consume the slot)", r.Outcome)
	}
}

func TestRequestSyncAlreadyRunningWhenSyncMuHeld(t *testing.T) {
	f := seededFake(t)
	e := newEnv(t, f, nil)
	e.svc.syncMu.Lock()
	r := e.svc.RequestSync(context.Background())
	e.svc.syncMu.Unlock()
	if r.Outcome != "already_running" || f.totalCalls() != 0 {
		t.Fatalf("RequestSync = %+v calls = %d, want already_running with no calls", r, f.totalCalls())
	}
	if r := e.svc.RequestSync(context.Background()); r.Outcome != "synced" {
		t.Fatalf("next RequestSync = %q, want synced", r.Outcome)
	}
}

func TestRequestSyncRateLimited(t *testing.T) {
	f := seededFake(t)
	e := newEnv(t, f, nil)
	ctx := context.Background()
	first := e.svc.RequestSync(ctx)
	if first.Outcome != "synced" || !first.NextAllowedAt.Equal(testNow.Add(10*time.Minute)) {
		t.Fatalf("first = %+v", first)
	}
	if first.LastSyncAt == nil || !first.LastSyncAt.Equal(testNow) {
		t.Fatalf("first LastSyncAt = %v, want %v", first.LastSyncAt, testNow)
	}
	calls := f.totalCalls()

	e.clock.Set(testNow.Add(5 * time.Minute))
	second := e.svc.RequestSync(ctx)
	if second.Outcome != "rate_limited" || !second.NextAllowedAt.Equal(testNow.Add(10*time.Minute)) {
		t.Fatalf("second = %+v, want rate_limited until first+10m", second)
	}
	if second.LastSyncAt == nil || !second.LastSyncAt.Equal(testNow) {
		t.Fatalf("second LastSyncAt = %v", second.LastSyncAt)
	}
	if n := f.totalCalls(); n != calls {
		t.Fatalf("rate-limited request made %d Garmin calls", n-calls)
	}

	e.clock.Set(testNow.Add(10 * time.Minute))
	if third := e.svc.RequestSync(ctx); third.Outcome != "synced" {
		t.Fatalf("third (at first+10m) = %+v, want synced", third)
	}
}

func TestRequestSyncOutcomePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs map[string]error
		want string
	}{
		{"all ok", nil, "synced"},
		{"error", map[string]error{mHRV: errors.New("boom")}, "failed"},
		{"auth beats error", map[string]error{mHRV: errors.New("boom"), mDaily: garmin.ErrUnauthorized}, "auth_failed"},
		{"rate limit beats auth", map[string]error{mSleep: garmin.ErrUnauthorized, mWeight: &garmin.RateLimitedError{RetryAt: testNow.Add(time.Minute)}}, "garmin_rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seededFake(t)
			for m, err := range tc.errs {
				f.setErr(m, err)
			}
			e := newEnv(t, f, nil)
			if r := e.svc.RequestSync(context.Background()); r.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", r.Outcome, tc.want)
			}
		})
	}
}

func TestSyncRecomputesSummaries(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	mustSync(t, e)
	m, err := e.db.ListDayMetrics(context.Background(), testToday, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("ListDayMetrics(today) is empty after a successful sync")
	}
}

// The post-sync recompute must reach back to the oldest date a sync stored,
// not only summaryDays: a long outage stores data older than today−120.
func TestSyncRecomputeCoversDeepCatchUp(t *testing.T) {
	old := testToday.AddDays(-150)
	metricsOn := func(t *testing.T, e *env, d civil.Date) []store.DayMetrics {
		t.Helper()
		ms, err := e.db.ListDayMetrics(context.Background(), d, d)
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}
	t.Run("daily catch-up", func(t *testing.T) {
		f := seededFake(t)
		f.sleep = append(f.sleep, item(t, sleepDTO(old.String(), 27000, 80)))
		e := newEnv(t, f, nil)
		setLastSuccess(t, e, 200)
		mustSync(t, e)
		if h, err := e.db.ListDailyHealth(context.Background(), old, old); err != nil || len(h) != 1 {
			t.Fatalf("daily health at today-150 = %v %v, want stored by catch-up", h, err)
		}
		if ms := metricsOn(t, e, old); len(ms) != 1 {
			t.Fatalf("day metrics at today-150 = %v, want one row (recompute must cover the catch-up)", ms)
		}
	})
	t.Run("activities window", func(t *testing.T) {
		f := seededFake(t)
		f.activities = append(f.activities, item(t, activityDTO(5150, "running",
			old.String()+" 07:00:00", old.String()+" 05:00:00", 12000, false)))
		e := newEnv(t, f, nil)
		last := testNow.AddDate(0, 0, -200) // only activities had the outage
		if err := e.db.PutSyncState(context.Background(), store.SyncState{
			DataType: TypeActivities, LastAttemptAt: &last, LastSuccessAt: &last, LastOutcome: OutcomeOK,
		}); err != nil {
			t.Fatal(err)
		}
		mustSync(t, e)
		ms := metricsOn(t, e, old)
		if len(ms) != 1 || ms[0].LongestRun7dM != 12000 {
			t.Fatalf("day metrics at today-150 = %+v, want longest run 7d 12000", ms)
		}
	})
	t.Run("normal sync keeps summaryDays", func(t *testing.T) {
		e := newEnv(t, seededFake(t), nil)
		mustSync(t, e)
		if ms := metricsOn(t, e, testToday.AddDays(-summaryDays)); len(ms) != 1 {
			t.Fatalf("day metrics at today-%d = %v, want one row", summaryDays, ms)
		}
		if ms := metricsOn(t, e, testToday.AddDays(-(summaryDays + 1))); len(ms) != 0 {
			t.Fatalf("day metrics at today-%d = %v, want none (recompute covers summaryDays)", summaryDays+1, ms)
		}
	})
}

// levelRecorder records the level and message of every log record.
type levelRecorder struct {
	mu   *sync.Mutex
	recs *[]string
}

func newLevelRecorder() *levelRecorder { return &levelRecorder{mu: &sync.Mutex{}, recs: &[]string{}} }

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, r.Level.String()+" "+r.Message)
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

func (h *levelRecorder) count(rec string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range *h.recs {
		if r == rec {
			n++
		}
	}
	return n
}

func TestEmptySleepDaysDoNotWarn(t *testing.T) {
	const warn = "WARN skipped unmappable items"
	emptySleep := garmin.SleepDTO{} // calendarDate null, sleepTimeSeconds null

	t.Run("sync", func(t *testing.T) {
		f := newFake()
		f.sleep = append(f.sleep, item(t, emptySleep), item(t, emptySleep))
		rec := newLevelRecorder()
		e := newEnv(t, f, rec)
		if _, err := e.svc.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := rec.count(warn); n != 0 {
			t.Fatalf("empty sleep days logged %d WARN", n)
		}
	})
	t.Run("backfill", func(t *testing.T) {
		f := newFake()
		f.sleep = append(f.sleep, item(t, emptySleep))
		rec := newLevelRecorder()
		e := newEnv(t, f, rec)
		mustBackfill(t, e)
		if n := rec.count(warn); n != 0 {
			t.Fatalf("empty sleep days logged %d WARN", n)
		}
	})
	t.Run("data with bad date still warns", func(t *testing.T) {
		f := newFake()
		f.sleep = append(f.sleep, item(t, sleepDTO("bad-date", 27000, 80)), item(t, emptySleep))
		rec := newLevelRecorder()
		e := newEnv(t, f, rec)
		if _, err := e.svc.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := rec.count(warn); n != 1 {
			t.Fatalf("WARN count = %d, want 1", n)
		}
	})
}

// setLastSuccess puts every type's LastSuccessAt at the given number of days ago.
func setLastSuccess(t *testing.T, e *env, daysAgo int) {
	t.Helper()
	ts := testNow.AddDate(0, 0, -daysAgo)
	for _, dt := range allTypes {
		if err := e.db.PutSyncState(context.Background(), store.SyncState{
			DataType: dt, LastAttemptAt: &ts, LastSuccessAt: &ts, LastOutcome: OutcomeOK,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func checkChunks(t *testing.T, rs []dateRange, first, lastChunkEnd civil.Date) {
	t.Helper()
	if len(rs) < 2 || rs[0].From != first {
		t.Fatalf("ranges = %v, want to start at %v", rs, first)
	}
	if rs[len(rs)-1] != (dateRange{testToday.AddDays(-27), testToday}) {
		t.Fatalf("last range = %v, want the normal window", rs[len(rs)-1])
	}
	if rs[len(rs)-2].To != lastChunkEnd {
		t.Fatalf("last chunk ends %v, want %v", rs[len(rs)-2].To, lastChunkEnd)
	}
	for i, r := range rs {
		if r.To.Sub(r.From) > 27 {
			t.Errorf("range %d = %v longer than 28 days", i, r)
		}
		if i > 0 && r.From != rs[i-1].To.AddDays(1) {
			t.Errorf("range %d = %v does not follow %v", i, r, rs[i-1])
		}
	}
}

func TestDailyCatchUpAfterLongOutage(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	setLastSuccess(t, e, 60)
	mustSync(t, e)
	for _, m := range []string{mSleep, mHRV, mDaily, mWeight} {
		checkChunks(t, e.fake.rangesOf(m), testToday.AddDays(-63), testToday.AddDays(-28))
	}
	if n := len(e.fake.rangesOf(mSearch)); n != 2 {
		t.Errorf("activity requests = %d, want the usual 2 (no catch-up)", n)
	}
}

func TestDailyCatchUpCappedAt365(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	setLastSuccess(t, e, 500)
	mustSync(t, e)
	rs := e.fake.rangesOf(mSleep)
	checkChunks(t, rs, testToday.AddDays(-365), testToday.AddDays(-28))
}

func TestDailyCatchUpFailedChunkKeepsGap(t *testing.T) {
	e := newEnv(t, seededFake(t), nil)
	setLastSuccess(t, e, 60)
	e.fake.setErrOnce(mSleep, errors.New("boom"))
	res := mustSync(t, e)
	if res.Outcomes[TypeSleep] != OutcomeError {
		t.Fatalf("sleep outcome = %q, want error", res.Outcomes[TypeSleep])
	}
	if st := syncState(t, e, TypeSleep); st.LastOutcome != OutcomeError ||
		st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(testNow.AddDate(0, 0, -60)) {
		t.Fatalf("sleep state = %+v, LastSuccessAt must not advance", st)
	}
	if res.Outcomes[TypeHRV] != OutcomeOK {
		t.Errorf("hrv outcome = %q, want ok", res.Outcomes[TypeHRV])
	}
	first := e.fake.rangesOf(mSleep)
	// Retry: the whole gap is fetched again.
	e.fake.resetCalls()
	res = mustSync(t, e)
	if res.Outcomes[TypeSleep] != OutcomeOK {
		t.Fatalf("retry outcome = %q", res.Outcomes[TypeSleep])
	}
	rs := e.fake.rangesOf(mSleep)
	checkChunks(t, rs, testToday.AddDays(-63), testToday.AddDays(-28))
	if len(first) == 0 {
		t.Fatal("no first-sync sleep ranges")
	}
}

func TestDailyCatchUpBoundaries(t *testing.T) {
	// last−3 == today−28: one 1-day chunk, then the normal window.
	e := newEnv(t, seededFake(t), nil)
	setLastSuccess(t, e, 25)
	mustSync(t, e)
	want := []dateRange{{testToday.AddDays(-28), testToday.AddDays(-28)}, {testToday.AddDays(-27), testToday}}
	for _, m := range []string{mSleep, mHRV, mDaily, mWeight} {
		if r := e.fake.rangesOf(m); !equalRanges(r, want) {
			t.Errorf("%s ranges = %v, want %v", m, r, want)
		}
	}
	// last−3 == today−27: no catch-up.
	e = newEnv(t, seededFake(t), nil)
	setLastSuccess(t, e, 24)
	mustSync(t, e)
	for _, m := range []string{mSleep, mHRV, mDaily, mWeight} {
		if r := e.fake.rangesOf(m); !equalRanges(r, want[1:]) {
			t.Errorf("%s ranges = %v, want %v", m, r, want[1:])
		}
	}
}

func equalRanges(a, b []dateRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A failed chunk must not mask a more severe error from the normal window.
func TestDailyCatchUpErrorPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		normal error
		want   string
	}{
		{"rate limited", &garmin.RateLimitedError{RetryAt: testNow.Add(time.Hour)}, OutcomeRateLimited},
		{"unauthorized", garmin.ErrUnauthorized, OutcomeAuthFailed},
		{"plain", errors.New("boom2"), OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seededFake(t)
			e := newEnv(t, f, nil)
			setLastSuccess(t, e, 60)
			calls := 0
			// Chunk 1 fails plain; chunk loop stops; the normal window (call 2) gets tc.normal.
			f.setErrOnce(mSleep, errors.New("boom"))
			f.setHook(func(m string) {
				if m == mSleep {
					calls++
					if calls == 1 {
						f.setErrOnce(mSleep, tc.normal)
					}
				}
			})
			res := mustSync(t, e)
			if res.Outcomes[TypeSleep] != tc.want {
				t.Fatalf("sleep outcome = %q, want %q", res.Outcomes[TypeSleep], tc.want)
			}
			if _, paused := e.svc.pausedUntil(); paused != (tc.want == OutcomeRateLimited) {
				t.Errorf("paused = %v", paused)
			}
		})
	}
}

// A 429 or auth failure on a chunk skips the normal window; a plain error does not.
func TestDailyCatchUpChunkErrorNormalWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantCalls  int
		wantOutcom string
	}{
		{"rate limited", &garmin.RateLimitedError{RetryAt: testNow.Add(time.Hour)}, 1, OutcomeRateLimited},
		{"unauthorized", garmin.ErrUnauthorized, 1, OutcomeAuthFailed},
		{"plain", errors.New("boom"), 2, OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seededFake(t)
			e := newEnv(t, f, nil)
			setLastSuccess(t, e, 60)
			f.setErrOnce(mSleep, tc.err)
			res := mustSync(t, e)
			if n := f.count(mSleep); n != tc.wantCalls {
				t.Errorf("sleep requests = %d, want %d", n, tc.wantCalls)
			}
			if res.Outcomes[TypeSleep] != tc.wantOutcom {
				t.Errorf("outcome = %q, want %q", res.Outcomes[TypeSleep], tc.wantOutcom)
			}
		})
	}
}

// BackfillDelay separates consecutive requests, including the last chunk and
// the normal window, but is not waited before the first chunk.
func TestDailyCatchUpPacing(t *testing.T) {
	const delay = 250 * time.Millisecond
	f := seededFake(t)
	e := newEnv(t, f, nil)
	e.svc.opts.BackfillDelay = delay
	setLastSuccess(t, e, 60) // 2 chunks + normal window per daily type
	var mu sync.Mutex
	var at []time.Time
	f.setHook(func(m string) {
		if m == mSleep {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		}
	})
	start := time.Now()
	mustSync(t, e)
	if len(at) != 3 {
		t.Fatalf("sleep requests = %d, want 3", len(at))
	}
	if d := at[0].Sub(start); d >= delay {
		t.Errorf("first chunk waited %v, want no pacing delay", d)
	}
	for i := 1; i < 3; i++ {
		if d := at[i].Sub(at[i-1]); d < delay {
			t.Errorf("gap before request %d = %v, want >= %v", i, d, delay)
		}
	}
}
