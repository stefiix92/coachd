package triggers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
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

type recorder struct{ got []Event }

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

func (e *env) newService(r *recorder) *Service {
	return New(Options{DB: e.db, TZ: bratislava, Now: func() time.Time { return e.now }, Sinks: []Sink{r},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func (e *env) at(local string) {
	t, err := time.ParseInLocation("2006-01-02 15:04", local, bratislava)
	if err != nil {
		panic(err)
	}
	e.now = t
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

// activity stores an activity on day that recorded easyRun as a watch would.
func (e *env) activity(t *testing.T, id int64, day string) {
	t.Helper()
	wf, err := fitworkout.Encode(easyRun(), created)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	b, err := fittest.Build(fittest.Options{Start: start, Records: 300, HR: 135, SpeedMS: 3,
		LapRecords: []int{100, 150, 50}, LapSteps: []uint16{0, 1, 2}, Workout: wf})
	if err != nil {
		t.Fatal(err)
	}
	a, err := fitdata.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	act := store.Activity{ID: id, StartLocal: start, StartUTC: start, Date: date(day), WeekStart: date(day).WeekStart(),
		Sport: store.SportRunning, FITStatus: store.FITStored, FITPath: "fit/x.fit"}
	if err := e.db.UpsertActivity(e.ctx, act, []byte(`{}`)); err != nil {
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
	e.proposal(t, "p1", "2026-11-02")
	e.check(t)
	e.count(t, 1)
	e.at("2026-11-03 07:30")
	e.activity(t, 100, "2026-11-02")
	e.check(t)
	e.count(t, 1)
}
