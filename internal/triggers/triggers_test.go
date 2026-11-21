package triggers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata"
	"github.com/stefiix92/coachd/internal/fitdata/fittest"
	"github.com/stefiix92/coachd/internal/fitworkout"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/workout"
)

var bratislava = func() *time.Location {
	l, err := time.LoadLocation("Europe/Bratislava")
	if err != nil {
		panic(err)
	}
	return l
}()

// recorder hears the events it wants: every event when want is nil, none
// when it is empty.
type recorder struct {
	want []string
	got  []Event
}

func (r *recorder) Wants(name string) bool { return r.want == nil || slices.Contains(r.want, name) }

func (r *recorder) Fire(_ context.Context, ev Event) { r.got = append(r.got, ev) }

type env struct {
	db  *store.DB
	ctx context.Context
	now time.Time
	rec *recorder
	svc *Service
}

// newEnv starts at the given local time in Europe/Bratislava.
func newEnv(t *testing.T, local string) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{db: db, ctx: context.Background(), rec: &recorder{}}
	e.at(local)
	e.svc = e.newService(e.rec)
	return e
}

func (e *env) newService(sinks ...Sink) *Service {
	return New(Options{DB: e.db, TZ: bratislava, Now: func() time.Time { return e.now }, Sinks: sinks,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func local(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, bratislava)
	if err != nil {
		panic(err)
	}
	return t
}

func (e *env) at(s string) { e.now = local(s) }

// synced records a successful activities sync started at the local time s.
func (e *env) synced(t *testing.T, s string) {
	t.Helper()
	at := local(s)
	st := store.SyncState{DataType: store.SyncTypeActivities, LastAttemptAt: &at, LastSuccessAt: &at, LastOutcome: "ok"}
	if err := e.db.PutSyncState(e.ctx, st); err != nil {
		t.Fatal(err)
	}
}

func (e *env) marks(t *testing.T) []store.EventMark {
	t.Helper()
	m, err := e.db.EventMarks(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) noMarks(t *testing.T) {
	t.Helper()
	if m := e.marks(t); len(m) != 0 {
		t.Fatalf("marks %v, want none", m)
	}
}

func (e *env) check(t *testing.T) {
	t.Helper()
	if err := e.svc.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func date(s string) civil.Date {
	d, err := civil.Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

func ip(v int) *int { return &v }

func (e *env) health(t *testing.T, day string, sleep *int) {
	t.Helper()
	if err := e.db.UpsertDailyHealth(e.ctx, store.DailyHealth{Date: date(day), SleepS: sleep}); err != nil {
		t.Fatal(err)
	}
}

func easyRun() workout.Workout {
	return workout.Workout{Name: "Easy", Sport: "running", Date: "2026-11-02", Steps: []workout.Step{
		{StepBase: workout.StepBase{Kind: "warmup", Duration: &workout.Duration{Type: "time", TimeS: ip(600)}}},
		{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "time", TimeS: ip(1500)},
			Target: &workout.Target{Type: "hr", LowBpm: ip(128), HighBpm: ip(145)}}},
		{StepBase: workout.StepBase{Kind: "cooldown", Duration: &workout.Duration{Type: "open"}}},
	}}
}

var created = time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)

func (e *env) proposal(t *testing.T, id, day string) {
	t.Helper()
	w := easyRun()
	canon, err := workout.CanonicalJSON(w)
	if err != nil {
		t.Fatal(err)
	}
	p := store.Proposal{ID: id, Date: date(day), Sport: store.SportRunning, Name: w.Name, ContentJSON: canon,
		ContentHash: workout.ContentHash(canon), Status: store.ProposalDelivered, StatusAt: created, CreatedAt: created,
		CreatedLane: "private", CreatedBy: "tok", CreatedClient: "test"}
	ev := store.AuditEvent{At: created, Lane: "private", Actor: "tok", Event: "proposal_delivered", Target: id, Outcome: "ok"}
	if err := e.db.InsertProposal(e.ctx, p, ev); err != nil {
		t.Fatal(err)
	}
}

var actStart = time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)

// activity stores an activity on day that recorded easyRun as a watch would.
func (e *env) activity(t *testing.T, id int64, day string) {
	t.Helper()
	e.pendingActivity(t, id, day)
	e.fitStored(t, id)
}

// pendingActivity stores an activity on day whose FIT file is not downloaded yet.
func (e *env) pendingActivity(t *testing.T, id int64, day string) {
	t.Helper()
	act := store.Activity{ID: id, StartLocal: actStart, StartUTC: actStart, Date: date(day), WeekStart: date(day).WeekStart(),
		Sport: store.SportRunning}
	if err := e.db.UpsertActivity(e.ctx, act, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

// fitStored stores activity id's FIT file and its detail: the workout it
// recorded is easyRun.
func (e *env) fitStored(t *testing.T, id int64) {
	t.Helper()
	wf, err := fitworkout.Encode(easyRun(), created)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fittest.Build(fittest.Options{Start: actStart, Records: 300, HR: 135, SpeedMS: 3,
		LapRecords: []int{100, 150, 50}, LapSteps: []uint16{0, 1, 2}, Workout: wf})
	if err != nil {
		t.Fatal(err)
	}
	a, err := fitdata.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetFIT(e.ctx, id, "fit/x.fit", store.FITStored); err != nil {
		t.Fatal(err)
	}
	if err := e.db.PutActivityDetail(e.ctx, id, store.NewActivityDetail(a)); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T, want int) {
	t.Helper()
	if len(e.rec.got) != want {
		t.Fatalf("events = %d %+v, want %d", len(e.rec.got), e.rec.got, want)
	}
}

// 2026-11-03 is a Tuesday, 2026-11-02 a Monday.

func TestHealthDayReadyOnce(t *testing.T) {
	e := newEnv(t, "2026-11-03 08:00")
	e.health(t, "2026-11-03", ip(25000))
	e.check(t)
	e.count(t, 1)
	if got := e.rec.got[0]; got != (Event{Name: HealthDayReady, Date: date("2026-11-03")}) {
		t.Fatalf("event = %+v", got)
	}
	e.check(t)
	e.count(t, 1)
}

func TestHealthDayNotReadyWithoutSleep(t *testing.T) {
	e := newEnv(t, "2026-11-03 08:00")
	e.health(t, "2026-11-03", nil)
	e.check(t)
	e.check(t)
	e.check(t)
	e.count(t, 0)
}

func TestWorkoutUnmatchedAfterSix(t *testing.T) {
	e := newEnv(t, "2026-11-03 05:59")
	e.proposal(t, "p1", "2026-11-02")
	e.check(t)
	e.count(t, 0)
	e.at("2026-11-03 06:00")
	e.synced(t, "2026-11-03 06:00")
	e.check(t)
	e.count(t, 1)
	if got := e.rec.got[0]; got != (Event{Name: WorkoutUnmatched, Date: date("2026-11-02"), ProposalID: "p1"}) {
		t.Fatalf("event = %+v", got)
	}
	e.check(t)
	e.count(t, 1)
}

func TestWorkoutMatchedSilent(t *testing.T) {
	e := newEnv(t, "2026-11-03 06:00")
	e.synced(t, "2026-11-03 06:00")
	e.proposal(t, "p1", "2026-11-02")
	e.activity(t, 100, "2026-11-02")
	e.check(t)
	e.count(t, 0)
}

func TestWeekClosedMonday(t *testing.T) {
	e := newEnv(t, "2026-11-01 23:59") // Sunday
	e.check(t)
	e.count(t, 0)
	e.at("2026-11-02 05:59")
	e.check(t)
	e.count(t, 0)
	e.at("2026-11-02 06:00")
	e.check(t)
	e.count(t, 1)
	if got := e.rec.got[0]; got != (Event{Name: WeekClosed, Date: date("2026-10-26")}) {
		t.Fatalf("event = %+v", got)
	}
	e.at("2026-11-03 09:00")
	e.check(t)
	e.count(t, 1)
}

func TestDowntimeSendsNoBurst(t *testing.T) {
	e := newEnv(t, "2026-11-03 09:00") // Tuesday
	e.synced(t, "2026-11-03 09:00")
	for i := 0; i < 60; i++ {
		day := date("2026-11-03").AddDays(-i).String()
		e.health(t, day, ip(25000))
		e.proposal(t, "p-"+day, day)
	}
	e.check(t)
	want := map[Event]bool{
		{Name: HealthDayReady, Date: date("2026-11-03")}:                               true,
		{Name: WorkoutUnmatched, Date: date("2026-11-02"), ProposalID: "p-2026-11-02"}: true,
	}
	for _, ev := range e.rec.got {
		if !want[ev] {
			t.Fatalf("unexpected event %+v", ev)
		}
	}
	e.count(t, 2)
}

func TestRestartRepeatsNothing(t *testing.T) {
	e := newEnv(t, "2026-11-02 08:00") // Monday: all three kinds are due
	e.synced(t, "2026-11-02 08:00")
	e.health(t, "2026-11-02", ip(25000))
	e.proposal(t, "p1", "2026-11-01")
	e.check(t)
	e.count(t, 3)
	r2 := &recorder{}
	s2 := e.newService(r2)
	if err := s2.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(r2.got) != 0 {
		t.Fatalf("second service fired %+v", r2.got)
	}
}

func TestLateActivityNoSecondEvent(t *testing.T) {
	e := newEnv(t, "2026-11-03 06:00")
	e.synced(t, "2026-11-03 06:00")
	e.proposal(t, "p1", "2026-11-02")
	e.check(t)
	e.count(t, 1)
	e.at("2026-11-03 07:30")
	e.activity(t, 100, "2026-11-02")
	e.check(t)
	e.count(t, 1)
}

// Until an activities sync has succeeded at or after 06:00 local today,
// yesterday's activity may still be on its way: nothing is sent or marked.
func TestWorkoutUnmatchedWaitsForMorningSync(t *testing.T) {
	e := newEnv(t, "2026-11-03 07:00")
	e.proposal(t, "p1", "2026-11-02")
	e.check(t) // no sync yet
	e.count(t, 0)
	e.noMarks(t)
	e.synced(t, "2026-11-02 23:00") // yesterday evening only
	e.check(t)
	e.count(t, 0)
	e.noMarks(t)
	e.synced(t, "2026-11-03 06:30")
	e.check(t)
	e.count(t, 1)
	if got := e.rec.got[0]; got != (Event{Name: WorkoutUnmatched, Date: date("2026-11-02"), ProposalID: "p1"}) {
		t.Fatalf("event = %+v", got)
	}
}

// An activity from yesterday whose FIT file is still pending holds the event
// back; once the FIT is stored the activity links, and nothing is sent.
func TestWorkoutUnmatchedWaitsForFIT(t *testing.T) {
	e := newEnv(t, "2026-11-03 07:00")
	e.synced(t, "2026-11-03 06:30")
	e.proposal(t, "p1", "2026-11-02")
	e.pendingActivity(t, 100, "2026-11-02")
	e.check(t)
	e.count(t, 0)
	e.noMarks(t)
	e.fitStored(t, 100)
	e.check(t)
	e.count(t, 0)
	e.noMarks(t)
}

// A pending FIT dated today holds the event back too; one from two days ago
// does not.
func TestWorkoutUnmatchedFITWindow(t *testing.T) {
	e := newEnv(t, "2026-11-03 07:00")
	e.synced(t, "2026-11-03 06:30")
	e.proposal(t, "p1", "2026-11-02")
	e.pendingActivity(t, 101, "2026-11-01")
	e.pendingActivity(t, 102, "2026-11-03")
	e.check(t)
	e.count(t, 0)
	if err := e.db.SetFIT(e.ctx, 102, "", store.FITUnavailable); err != nil {
		t.Fatal(err)
	}
	e.check(t)
	e.count(t, 1)
}

// safeRecorder is a recorder that Run's goroutine and the test share.
type safeRecorder struct {
	mu  sync.Mutex
	got []Event
}

func (r *safeRecorder) Wants(string) bool { return true }

func (r *safeRecorder) Fire(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
}

func (r *safeRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// run starts Run with interval and returns a stop that cancels it and fails
// the test unless Run returns promptly.
func (e *env) run(t *testing.T, r *safeRecorder, interval time.Duration) (stop func()) {
	t.Helper()
	svc := New(Options{DB: e.db, TZ: bratislava, Now: func() time.Time { return e.now }, Sinks: []Sink{r},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: interval})
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	return func() {
		t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Run checks at once, then on every interval, and returns when cancelled.
func TestRunChecksAtStartAndEveryInterval(t *testing.T) {
	e := newEnv(t, "2026-11-03 08:00")
	e.health(t, "2026-11-03", ip(25000))
	r := &safeRecorder{}
	stop := e.run(t, r, time.Hour) // no tick in time: only the check at start
	waitFor(t, "the check at start", func() bool { return r.len() == 1 })
	stop()

	r = &safeRecorder{}
	stop = e.run(t, r, 20*time.Millisecond)
	defer stop()
	time.Sleep(50 * time.Millisecond) // a few checks with nothing due
	if n := r.len(); n != 0 {
		t.Fatalf("events = %d before anything was due", n)
	}
	e.synced(t, "2026-11-03 07:00")
	e.proposal(t, "p1", "2026-11-02")
	waitFor(t, "a check on the interval", func() bool { return r.len() == 1 })
}

// allDue makes all three kinds due on Monday 2026-11-02 at local time s:
// today's sleep, a delivered workout from Sunday with no activity, the
// morning sync and the week that closed.
func (e *env) allDue(t *testing.T, s string) {
	t.Helper()
	e.at(s)
	e.synced(t, s)
	e.health(t, "2026-11-02", ip(25000))
	e.proposal(t, "p1", "2026-11-01")
}

// An event no sink wants is neither fired nor marked, and its check reads
// nothing: Check succeeds on a closed store.
func TestNoSinkWantsNothingMarked(t *testing.T) {
	e := newEnv(t, "2026-11-02 08:00")
	e.allDue(t, "2026-11-02 08:00")
	r := &recorder{want: []string{}}
	if err := e.newService(r).Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 0 {
		t.Fatalf("fired %+v, want nothing", r.got)
	}
	e.noMarks(t)
	if err := e.newService().Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.noMarks(t)
	e.db.Close()
	if err := e.newService(r).Check(e.ctx); err != nil {
		t.Fatalf("Check with no wanted event touched the store: %v", err)
	}
}

// A sink wanting only week.closed gets only that, and only it is marked; a
// receiver added later the same day still gets the other two.
func TestOnlyWantedEventMarked(t *testing.T) {
	e := newEnv(t, "2026-11-02 06:00")
	e.allDue(t, "2026-11-02 06:00")
	r := &recorder{want: []string{WeekClosed}}
	if err := e.newService(r).Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	if want := []Event{{Name: WeekClosed, Date: date("2026-10-26")}}; !slices.Equal(r.got, want) {
		t.Fatalf("fired %+v, want %+v", r.got, want)
	}
	if got, want := e.marks(t), []store.EventMark{{Event: WeekClosed, Key: "2026-10-26"}}; !slices.Equal(got, want) {
		t.Fatalf("marks %v, want %v", got, want)
	}

	e.at("2026-11-02 09:00")
	later := &recorder{}
	if err := e.newService(later).Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Name: HealthDayReady, Date: date("2026-11-02")},
		{Name: WorkoutUnmatched, Date: date("2026-11-01"), ProposalID: "p1"}}
	if !slices.Equal(later.got, want) {
		t.Fatalf("later receiver got %+v, want %+v", later.got, want)
	}
}

// Each sink gets only the events it wants.
func TestEventGoesOnlyToSinksThatWantIt(t *testing.T) {
	e := newEnv(t, "2026-11-02 08:00")
	e.allDue(t, "2026-11-02 08:00")
	week, day := &recorder{want: []string{WeekClosed}}, &recorder{want: []string{HealthDayReady, WeekClosed}}
	if err := e.newService(week, day).Check(e.ctx); err != nil {
		t.Fatal(err)
	}
	if want := []Event{{Name: WeekClosed, Date: date("2026-10-26")}}; !slices.Equal(week.got, want) {
		t.Fatalf("week sink got %+v, want %+v", week.got, want)
	}
	if want := []Event{{Name: HealthDayReady, Date: date("2026-11-02")}, {Name: WeekClosed, Date: date("2026-10-26")}}; !slices.Equal(day.got, want) {
		t.Fatalf("day sink got %+v, want %+v", day.got, want)
	}
}
