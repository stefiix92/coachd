package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
)

func TestRunSyncsImmediatelyAndDrainsFIT(t *testing.T) {
	f := seededFake(t)
	fitDone := make(chan struct{})
	var once sync.Once
	f.setHook(func(m string) {
		if m == mFIT {
			once.Do(func() { close(fitDone) })
		}
	})
	e := newEnv(t, f, nil)
	// History complete: no backfill, whose chunks would also signal the FIT
	// worker and let the test cancel the initial sync mid-flight.
	for _, dt := range dataTypes {
		if err := e.db.PutSyncState(context.Background(), store.SyncState{DataType: dt, BackfillDone: true}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		e.svc.Run(ctx)
		close(stopped)
	}()
	<-fitDone // the initial sync signalled the FIT worker
	cancel()
	<-stopped
	if n := f.count(mSearch); n < 1 {
		t.Fatalf("search calls = %d, want the immediate sync", n)
	}
	if st := syncState(t, e, "activities"); st.LastOutcome != OutcomeOK {
		t.Fatalf("activities outcome = %q", st.LastOutcome)
	}
}

// keyRecorder records every attribute key logged, flattening groups.
type keyRecorder struct {
	mu   *sync.Mutex
	keys map[string]bool
}

func newKeyRecorder() *keyRecorder { return &keyRecorder{mu: &sync.Mutex{}, keys: map[string]bool{}} }

func (h *keyRecorder) add(a slog.Attr) {
	if a.Value.Kind() == slog.KindGroup {
		for _, c := range a.Value.Group() {
			h.add(c)
		}
		return
	}
	h.keys[a.Key] = true
}

func (h *keyRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *keyRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool { h.add(a); return true })
	return nil
}

func (h *keyRecorder) WithAttrs(as []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range as {
		h.add(a)
	}
	return h
}

func (h *keyRecorder) WithGroup(string) slog.Handler { return h }

func TestLogsUseOnlyAllowedKeys(t *testing.T) {
	f := seededFake(t)
	f.sleep = append(f.sleep, item(t, sleepDTO("bad-date", 1, 1)))
	f.activities = append(f.activities, item(t, activityDTO(9, "running", "", "", 1, false)))
	f.setErr(mHRV, garmin.ErrUnauthorized)
	f.setErr(mDaily, errors.New("boom"))
	f.setErr(mWeight, &garmin.RateLimitedError{RetryAt: testNow.Add(time.Minute)})
	f.fitErr[idRun] = errors.New("boom")
	rec := newKeyRecorder()
	e := newEnv(t, f, rec)
	mustSync(t, e)
	e.clock.Set(testNow.Add(2 * time.Minute))
	e.svc.drainFIT(context.Background())

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.keys) == 0 {
		t.Fatal("expected some logs")
	}
	for k := range rec.keys {
		if !slices.Contains(telemetry.AllowedKeys, k) {
			t.Errorf("log key %q is not in telemetry.AllowedKeys", k)
		}
	}
}

// The intervals.icu paths log and trace only allowlisted keys: an unmappable
// entry, the duplicate step, a FIT download 429, the deletion probe and an
// unauthorized listing.
func TestIntervalsTelemetryUsesOnlyAllowedKeys(t *testing.T) {
	fi := newFakeIntervals()
	bad := rideOn("i101", testToday.AddDays(-1))
	bad.StartDate = "yesterday"
	fi.setItems(intervalsItem(t, rideOn("i100", testToday.AddDays(-1))), intervalsItem(t, bad))
	fi.fileErr["i100"] = &intervals.RateLimitedError{RetryAt: testNow.Add(time.Minute)}
	rec := newKeyRecorder()
	e := newIntervalsEnv(t, seededFake(t), fi, rec)
	sr := traced(t, e)
	ctx := context.Background()

	mustSync(t, e)
	e.svc.drainFIT(ctx)
	e.clock.Set(testNow.Add(time.Hour))
	fi.setItems()
	fi.deleted["i100"] = true
	mustSync(t, e)
	if _, err := e.db.GetActivity(ctx, -100); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted ride: %v, want not found", err)
	}
	fi.setErr(miList, intervals.ErrUnauthorized)
	if res := mustSync(t, e); res.Outcomes[TypeIntervals] != OutcomeAuthFailed {
		t.Fatalf("intervals outcome = %q, want auth_failed", res.Outcomes[TypeIntervals])
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, k := range []string{"retry_after_s", "activity_id", "error_class", "count"} {
		if !rec.keys[k] {
			t.Errorf("no log carried %q: a path was not exercised", k)
		}
	}
	for k := range rec.keys {
		if !slices.Contains(telemetry.AllowedKeys, k) {
			t.Errorf("log key %q is not in telemetry.AllowedKeys", k)
		}
	}
	assertSpanKeys(t, sr)
}
