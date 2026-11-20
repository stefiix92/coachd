package proposals

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	_ "modernc.org/sqlite"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/guidance"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/workout"
)

var bratislava = func() *time.Location {
	l, err := time.LoadLocation("Europe/Bratislava")
	if err != nil {
		panic(err)
	}
	return l
}()

type fixture struct {
	svc  *Service
	db   *store.DB
	path string
	now  *time.Time
	logs *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coachd.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, bratislava)
	f := &fixture{db: db, path: path, now: &now, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.svc = New(Deps{DB: db, TZ: bratislava, Now: func() time.Time { return *f.now }, Limits: workout.DefaultLimits, Log: log})
	return f
}

var actor = Actor{Lane: "private", Subject: "agent-token", Client: "claude"}

func wk(date string) workout.Workout {
	ts := 1800
	return workout.Workout{Name: "Easy", Sport: "running", Date: date,
		Steps: []workout.Step{{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "time", TimeS: &ts}}}}}
}

func auditEvents(t *testing.T, f *fixture, target string) []string {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	rows, err := sdb.Query(`SELECT event FROM audit_log WHERE target=? ORDER BY id`, target)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

type auditRow struct{ Event, Lane, Actor, Client string }

func auditRows(t *testing.T, f *fixture, target string) []auditRow {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	rows, err := sdb.Query(`SELECT event, lane, actor, client FROM audit_log WHERE target=? ORDER BY id`, target)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Event, &r.Lane, &r.Actor, &r.Client); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func auditTotal(t *testing.T, f *fixture) int {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var n int
	if err := sdb.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedAct(t *testing.T, f *fixture, id int64, date string, sport store.Sport, distM, movingS float64, maxHR *float64) {
	t.Helper()
	dt, err := civil.Parse(date)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(dt.Year, dt.Month, dt.Day, 7, 0, 0, 0, bratislava)
	a := store.Activity{ID: id, StartLocal: start, StartUTC: start.UTC(), Date: dt, WeekStart: dt.WeekStart(), TypeKey: string(sport),
		Sport: sport, DistanceM: distM, DurationS: movingS, MovingS: movingS, MaxHR: maxHR}
	if err := f.db.UpsertActivity(context.Background(), a, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func mustPropose(t *testing.T, f *fixture, date string, replaces *string) ProposeResult {
	t.Helper()
	r, err := f.svc.Propose(context.Background(), wk(date), "why", replaces, actor)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "pending" {
		t.Fatalf("verdict = %+v", r)
	}
	return r
}

func TestProposeStoresPendingWithWarnings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := mustPropose(t, f, "2026-10-25", nil)
	if len(r.ID) != 32 || len(r.Warnings) == 0 {
		t.Fatalf("result = %+v", r)
	}
	codes := map[string]bool{}
	for _, w := range r.Warnings {
		codes[w.Code] = true
	}
	if !codes["stale_data"] || !codes["thin_history"] {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
	p, err := f.db.GetProposal(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := workout.CanonicalJSON(wk("2026-10-25"))
	var ws []map[string]string
	if p.Status != store.ProposalPending || string(p.ContentJSON) != string(canon) || p.ContentHash != workout.ContentHash(canon) ||
		p.Rationale != "why" || p.Replaces != nil || p.CreatedLane != "private" || p.CreatedBy != "agent-token" || p.CreatedClient != "claude" ||
		p.Date.String() != "2026-10-25" || p.Sport != store.SportRunning || p.Name != "Easy" ||
		json.Unmarshal(p.WarningsJSON, &ws) != nil || len(ws) != len(r.Warnings) {
		t.Fatalf("stored = %+v", p)
	}
	if ev := auditEvents(t, f, r.ID); len(ev) != 1 || ev[0] != "proposal_pending" {
		t.Fatalf("audit = %v", ev)
	}
}

func TestProposeWarningInputs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Same-day: an approved proposal on the date is counted, a pending one is not.
	a := mustPropose(t, f, "2026-10-25", nil)
	if _, err := f.db.TransitionProposal(ctx, a.ID, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
		To: store.ProposalApproved, At: *f.now, Lane: "public", Actor: "runner"}); err != nil {
		t.Fatal(err)
	}
	mustPropose(t, f, "2026-10-25", nil)
	r := mustPropose(t, f, "2026-10-25", nil)
	n := 0
	for _, w := range r.Warnings {
		if w.Code == "same_day" {
			n++
			if !strings.Contains(w.Message, "1 other workout") {
				t.Fatalf("msg = %q", w.Message)
			}
		}
	}
	if n != 1 {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
}

func TestProposeInvalidStoresNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := wk("2026-10-25")
	bad.Sport = "swimming"
	r, err := f.svc.Propose(ctx, bad, "why", nil, actor)
	if err != nil || r.Verdict != "invalid" || r.ID != "" || !strings.Contains(r.Reason, "sport") {
		t.Fatalf("r = %+v, %v", r, err)
	}
	for name, rat := range map[string]string{"long": strings.Repeat("x", 2001), "control": "a\x00b"} {
		r, err := f.svc.Propose(ctx, wk("2026-10-25"), rat, nil, actor)
		if err != nil || r.Verdict != "invalid" || !strings.Contains(r.Reason, "rationale") {
			t.Fatalf("%s: r = %+v, %v", name, r, err)
		}
	}
	if r, err := f.svc.Propose(ctx, wk("2026-10-25"), strings.Repeat("é", 1998)+"\n\t", nil, actor); err != nil || r.Verdict != "pending" {
		t.Fatalf("2000 runes + nl/tab: %+v, %v", r, err)
	}
	if n := auditTotal(t, f); n != 1 { // only the one valid proposal
		t.Fatalf("audit rows = %d", n)
	}
	ps, err := f.svc.List(ctx, civil.Date{Year: 2000, Month: 1, Day: 1}, civil.Date{Year: 2100, Month: 1, Day: 1}, nil)
	if err != nil || len(ps) != 1 {
		t.Fatalf("stored = %d, %v", len(ps), err)
	}
}

func TestProposeReplacesValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	missing := "nope"
	r, err := f.svc.Propose(ctx, wk("2026-10-25"), "why", &missing, actor)
	if err != nil || r.Verdict != "invalid" || !strings.Contains(r.Reason, "replaces") {
		t.Fatalf("missing: %+v, %v", r, err)
	}
	c := mustPropose(t, f, "2026-10-25", nil)
	if _, err := f.svc.Cancel(ctx, c.ID, actor); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.Propose(ctx, wk("2026-10-25"), "why", &c.ID, actor)
	if err != nil || r.Verdict != "invalid" || !strings.Contains(r.Reason, "cancelled") {
		t.Fatalf("cancelled: %+v, %v", r, err)
	}
	p := mustPropose(t, f, "2026-10-25", nil)
	ok := mustPropose(t, f, "2026-10-25", &p.ID)
	got, err := f.db.GetProposal(ctx, ok.ID)
	if err != nil || got.Replaces == nil || *got.Replaces != p.ID || got.Status != store.ProposalPending {
		t.Fatalf("replaces: %+v, %v", got, err)
	}
	old, _ := f.db.GetProposal(ctx, p.ID)
	if old.Status != store.ProposalPending {
		t.Fatalf("replaced status changed: %s", old.Status)
	}
	// self id: can never match a random new id, but the check is explicit.
	if err := checkReplaces("x", "x", store.ProposalPending); err == nil {
		t.Fatal("self replace accepted")
	}
}

func TestCancelOnlyPending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := mustPropose(t, f, "2026-10-25", nil)
	p, err := f.svc.Cancel(ctx, r.ID, Actor{Lane: "public", Subject: "me@x", Client: "web"})
	if err != nil || p.Status != store.ProposalCancelled {
		t.Fatalf("cancel: %+v, %v", p, err)
	}
	_, err = f.svc.Cancel(ctx, r.ID, actor)
	if !errors.Is(err, ErrNotPending) || !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("again: %v", err)
	}
	if _, err := f.svc.Cancel(ctx, "missing", actor); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	a := mustPropose(t, f, "2026-10-25", nil)
	if _, err := f.db.TransitionProposal(ctx, a.ID, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
		To: store.ProposalApproved, At: *f.now, Lane: "public", Actor: "runner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Cancel(ctx, a.ID, actor); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approved cancel: %v", err)
	}
	got, _ := f.db.GetProposal(ctx, a.ID)
	if got.Status != store.ProposalApproved {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := mustPropose(t, f, "2026-10-26", nil)
	b := mustPropose(t, f, "2026-10-24", nil)
	if _, err := f.svc.Cancel(ctx, b.ID, actor); err != nil {
		t.Fatal(err)
	}
	pend, err := f.svc.List(ctx, civil.Date{Year: 2026, Month: 10, Day: 24}, civil.Date{Year: 2026, Month: 10, Day: 26},
		[]store.ProposalStatus{store.ProposalPending})
	if err != nil || len(pend) != 1 || pend[0].ID != a.ID {
		t.Fatalf("pending = %+v, %v", pend, err)
	}
	all, err := f.svc.List(ctx, civil.Date{Year: 2026, Month: 10, Day: 24}, civil.Date{Year: 2026, Month: 10, Day: 26},
		[]store.ProposalStatus{store.ProposalCancelled})
	if err != nil || len(all) != 1 || all[0].ID != b.ID {
		t.Fatalf("list = %+v, %v", all, err)
	}
}

func TestExpireAtLocalMidnightAcrossDST(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := mustPropose(t, f, "2026-10-25", nil) // DST ends this night in Europe/Bratislava
	*f.now = time.Date(2026, 10, 25, 23, 59, 0, 0, bratislava)
	if n, err := f.svc.ExpireDue(ctx); err != nil || n != 0 {
		t.Fatalf("23:59: n=%d, %v", n, err)
	}
	if p, _ := f.db.GetProposal(ctx, r.ID); p.Status != store.ProposalPending {
		t.Fatalf("status = %s", p.Status)
	}
	*f.now = time.Date(2026, 10, 26, 0, 0, 0, 0, bratislava)
	if n, err := f.svc.ExpireDue(ctx); err != nil || n != 1 {
		t.Fatalf("00:00: n=%d, %v", n, err)
	}
	if p, _ := f.db.GetProposal(ctx, r.ID); p.Status != store.ProposalExpired {
		t.Fatalf("status = %s", p.Status)
	}
	if n, _ := f.svc.ExpireDue(ctx); n != 0 {
		t.Fatalf("second run n=%d", n)
	}
}

func TestProposalIDsRandom128bit(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" || seen[id] {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
}

func TestAuditRowsPerTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := mustPropose(t, f, "2026-10-24", nil)
	b := mustPropose(t, f, "2026-10-26", nil)
	canceller := Actor{Lane: "public", Subject: "me@example.com", Client: "web"}
	if _, err := f.svc.Cancel(ctx, b.ID, canceller); err != nil {
		t.Fatal(err)
	}
	*f.now = time.Date(2026, 10, 25, 9, 0, 0, 0, bratislava)
	if _, err := f.svc.ExpireDue(ctx); err != nil {
		t.Fatal(err)
	}
	wantA := []auditRow{{"proposal_pending", "private", "agent-token", "claude"}, {"proposal_expired", "system", "system", ""}}
	if got := auditRows(t, f, a.ID); fmt.Sprint(got) != fmt.Sprint(wantA) {
		t.Fatalf("a audit = %v", got)
	}
	wantB := []auditRow{{"proposal_pending", "private", "agent-token", "claude"}, {"proposal_cancelled", "public", "me@example.com", "web"}}
	if got := auditRows(t, f, b.ID); fmt.Sprint(got) != fmt.Sprint(wantB) {
		t.Fatalf("b audit = %v", got)
	}
}

// stale_data reads Garmin's data types only: a working intervals.icu sync
// does not hide a Garmin outage.
func TestStaleDataIgnoresIntervals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for typ, ago := range map[string]time.Duration{"activities": 30 * time.Hour, "intervals_activities": time.Hour} {
		at := f.now.Add(-ago)
		if err := f.db.PutSyncState(ctx, store.SyncState{DataType: typ, LastSuccessAt: &at, LastOutcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	r := mustPropose(t, f, "2026-10-25", nil)
	for _, w := range r.Warnings {
		if w.Code == "stale_data" {
			return
		}
	}
	t.Fatalf("warnings = %+v, want stale_data (Garmin last synced 30 h ago)", r.Warnings)
}

func TestWarningInputsExact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hr := func(v float64) *float64 { return &v }
	seedAct(t, f, 1, "2026-09-24", store.SportRunning, 10000, 3000, nil) // today-29: included
	seedAct(t, f, 2, "2026-10-10", store.SportRunning, 8000, 3000, nil)
	seedAct(t, f, 7, "2026-10-23", store.SportRunning, 5000, 3600, nil)       // today: sole source of the longest duration
	seedAct(t, f, 3, "2026-09-23", store.SportTrailRunning, 30000, 9000, nil) // today-30: excluded
	seedAct(t, f, 4, "2026-07-26", store.SportStrength, 0, 1800, hr(195.9))   // today-89: included, floored
	seedAct(t, f, 5, "2026-07-25", store.SportStrength, 0, 1800, hr(205))     // today-90: excluded
	seedAct(t, f, 6, "2026-08-01", store.SportStrength, 0, 1800, nil)
	for typ, ago := range map[string]time.Duration{"activities": 2 * time.Hour, "sleep": 30 * time.Hour} {
		at := f.now.Add(-ago)
		if err := f.db.PutSyncState(ctx, store.SyncState{DataType: typ, LastSuccessAt: &at, LastOutcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	dist, hrHigh, hrLow := 12000, 196, 150
	w := wk("2026-10-25")
	w.Steps = []workout.Step{{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "distance", DistanceM: &dist},
		Target: &workout.Target{Type: "hr", LowBpm: &hrLow, HighBpm: &hrHigh}}}}
	r, err := f.svc.Propose(ctx, w, "why", nil, actor)
	if err != nil || r.Verdict != "pending" {
		t.Fatalf("r = %+v, %v", r, err)
	}
	want := []guidance.Warning{
		{Code: "spike_distance", Message: "planned 12.0 km is 120.0% of the longest run in the last 30 days (10.0 km)"},
		{Code: "spike_duration", Message: "estimated about 72 min is 120.0% of the longest run duration in the last 30 days (60 min)"},
		{Code: "above_max_hr", Message: "planned target of 196 bpm is above the highest heart rate in the last 90 days (195 bpm)"},
	}
	if fmt.Sprint(r.Warnings) != fmt.Sprint(want) {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
	p, _ := f.db.GetProposal(ctx, r.ID)
	var stored []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(p.WarningsJSON, &stored); err != nil || len(stored) != 3 || stored[0].Code != "spike_distance" || stored[2].Message != want[2].Message {
		t.Fatalf("stored = %s, %v", p.WarningsJSON, err)
	}
	if !strings.Contains(string(p.WarningsJSON), `{"code":"spike_distance","message":"planned 12.0 km is 120.0%`) {
		t.Fatalf("json keys: %s", p.WarningsJSON)
	}

	// A plan within bounds on well-covered data yields no warnings, stored as [].
	r2 := mustPropose(t, f, "2026-10-26", nil)
	if len(r2.Warnings) != 0 {
		t.Fatalf("warnings = %+v", r2.Warnings)
	}
	p2, _ := f.db.GetProposal(ctx, r2.ID)
	if string(p2.WarningsJSON) != "[]" {
		t.Fatalf("warnings_json = %s", p2.WarningsJSON)
	}
}

func TestSpikeWarningWithoutDayMetrics(t *testing.T) {
	f := newFixture(t)
	*f.now = time.Date(2026, 10, 24, 0, 30, 0, 0, bratislava) // just after midnight, no metrics row yet
	seedAct(t, f, 1, "2026-10-20", store.SportRunning, 10000, 3000, nil)
	dist := 20000
	w := wk("2026-10-25")
	w.Steps = []workout.Step{{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "distance", DistanceM: &dist}}}}
	r, err := f.svc.Propose(context.Background(), w, "why", nil, actor)
	if err != nil || r.Verdict != "pending" {
		t.Fatalf("r = %+v, %v", r, err)
	}
	found := false
	for _, x := range r.Warnings {
		found = found || x.Code == "spike_distance"
	}
	if !found {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
}

// TestSpikeWarningNoRecentRun: with no run dated in the last 30 days, a run
// plan still gets spike warnings, worded as facts.
func TestSpikeWarningNoRecentRun(t *testing.T) {
	f := newFixture(t)
	seedAct(t, f, 1, "2026-09-23", store.SportRunning, 30000, 9000, nil) // today-30: outside the window
	seedAct(t, f, 2, "2026-10-20", store.SportCycling, 40000, 5000, nil)
	spikes := func(r ProposeResult) []guidance.Warning {
		var out []guidance.Warning
		for _, w := range r.Warnings {
			if strings.HasPrefix(w.Code, "spike_") {
				out = append(out, w)
			}
		}
		return out
	}
	r := mustPropose(t, f, "2026-10-25", nil) // 30 min, time only
	want := []guidance.Warning{{Code: "spike_duration", Message: "no running activity recorded in the last 30 days; planned about 30 min"}}
	if got := spikes(r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("time plan: %+v", r.Warnings)
	}
	dist := 10000
	w := wk("2026-10-25")
	w.Steps = []workout.Step{{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "distance", DistanceM: &dist}}}}
	r, err := f.svc.Propose(context.Background(), w, "why", nil, actor)
	if err != nil || r.Verdict != "pending" {
		t.Fatalf("r = %+v, %v", r, err)
	}
	want = []guidance.Warning{
		{Code: "spike_distance", Message: "no running activity recorded in the last 30 days; planned 10.0 km"},
		{Code: "spike_duration", Message: "no running activity recorded in the last 30 days; estimated about 60 min"},
	}
	if got := spikes(r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("distance plan: %+v", r.Warnings)
	}
}

func TestSameDayExcludesReplaced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	approve := func(id string) {
		if _, err := f.db.TransitionProposal(ctx, id, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
			To: store.ProposalApproved, At: *f.now, Lane: "public", Actor: "runner"}); err != nil {
			t.Fatal(err)
		}
	}
	a := mustPropose(t, f, "2026-10-25", nil)
	approve(a.ID)
	has := func(r ProposeResult) bool {
		for _, w := range r.Warnings {
			if w.Code == "same_day" {
				return true
			}
		}
		return false
	}
	if r := mustPropose(t, f, "2026-10-25", &a.ID); has(r) {
		t.Fatalf("replaced proposal counted: %+v", r.Warnings)
	}
	if r := mustPropose(t, f, "2026-10-25", nil); !has(r) {
		t.Fatalf("expected same_day: %+v", r.Warnings)
	}
}

func TestSameDayCountsDeliveryStates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	move := func(id string, to store.ProposalStatus) {
		if _, err := f.db.TransitionProposal(ctx, id, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
			To: to, At: *f.now, Lane: "public", Actor: "runner"}); err != nil {
			t.Fatal(err)
		}
	}
	going := mustPropose(t, f, "2026-10-25", nil)
	move(going.ID, store.ProposalDelivering)
	failed := mustPropose(t, f, "2026-10-25", nil)
	move(failed.ID, store.ProposalDeliveryFailed)
	sameDay := func(r ProposeResult) string {
		for _, w := range r.Warnings {
			if w.Code == "same_day" {
				return w.Message
			}
		}
		return ""
	}
	if msg := sameDay(mustPropose(t, f, "2026-10-25", nil)); !strings.Contains(msg, "2 other workouts") {
		t.Fatalf("same_day = %q, want 2 other workouts", msg)
	}
	// The replaced proposal is still excluded.
	if msg := sameDay(mustPropose(t, f, "2026-10-25", &going.ID)); !strings.Contains(msg, "1 other workout ") {
		t.Fatalf("same_day replacing a delivering proposal = %q, want 1 other workout", msg)
	}
}

func TestReplacesTargetStatuses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		st store.ProposalStatus
		ok bool
	}{
		{store.ProposalApproved, true}, {store.ProposalDelivering, true}, {store.ProposalDeliveryFailed, true},
		{store.ProposalDelivered, true},
		{store.ProposalExpired, false}, {store.ProposalDeclined, false}, {store.ProposalWithdrawn, false},
		{store.ProposalCancelled, false},
	} {
		old := mustPropose(t, f, "2026-10-25", nil)
		if tc.st != store.ProposalCancelled {
			// walk through store transitions to reach the status
			if _, err := f.db.TransitionProposal(ctx, old.ID, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
				To: tc.st, At: *f.now, Lane: "public", Actor: "runner"}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := f.svc.Cancel(ctx, old.ID, actor); err != nil {
			t.Fatal(err)
		}
		before := auditTotal(t, f)
		r, err := f.svc.Propose(ctx, wk("2026-10-25"), "why", &old.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		if tc.ok != (r.Verdict == "pending") {
			t.Fatalf("%s: %+v", tc.st, r)
		}
		if !tc.ok {
			if !strings.Contains(r.Reason, string(tc.st)) || auditTotal(t, f) != before {
				t.Fatalf("%s: reason %q, audit %d->%d", tc.st, r.Reason, before, auditTotal(t, f))
			}
		}
	}
}

func TestNotPendingError(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := mustPropose(t, f, "2026-10-25", nil)
	if _, err := f.svc.Cancel(ctx, r.ID, actor); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Cancel(ctx, r.ID, actor)
	var np *NotPendingError
	if !errors.As(err, &np) || np.ID != r.ID || np.Status != store.ProposalCancelled ||
		!errors.Is(err, ErrNotPending) || !errors.Is(err, store.ErrConflict) ||
		err.Error() != "proposal "+r.ID+" is cancelled; only pending proposals can be cancelled" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunExpiryRunsAtStartAndLogsAllowlisted(t *testing.T) {
	f := newFixture(t)
	r := mustPropose(t, f, "2026-10-25", nil)
	*f.now = time.Date(2026, 10, 27, 9, 0, 0, 0, bratislava)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); f.svc.RunExpiry(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := f.db.GetProposal(context.Background(), r.ID); p.Status == store.ProposalExpired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunExpiry did not return")
	}
	wg.Wait()
	if p, _ := f.db.GetProposal(context.Background(), r.ID); p.Status != store.ProposalExpired {
		t.Fatalf("status = %s", p.Status)
	}
	var rec map[string]any
	line := strings.TrimSpace(f.logs.String())
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log %q: %v", line, err)
	}
	for k := range rec {
		switch k {
		case "time", "level", "msg", "component", "count", "outcome":
		default:
			t.Fatalf("non-allowlisted log key %q", k)
		}
	}
	if rec["count"] != float64(1) {
		t.Fatalf("log = %v", rec)
	}
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []string
	dates []civil.Date
	seen  []bool
	db    *store.DB
	links []trace.SpanContext
}

func (n *fakeNotifier) ProposalPending(id string, d civil.Date, link trace.SpanContext) {
	_, err := n.db.GetProposal(context.Background(), id)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, id)
	n.dates = append(n.dates, d)
	n.seen = append(n.seen, err == nil)
	n.links = append(n.links, link)
}

func newDepsSvc(t *testing.T, f *fixture, d Deps) *Service {
	t.Helper()
	d.DB, d.TZ, d.Now, d.Limits = f.db, bratislava, func() time.Time { return *f.now }, workout.DefaultLimits
	d.Log = slog.New(slog.NewJSONHandler(f.logs, nil))
	return New(d)
}

func TestProposeDoesNotWaitForNotify(t *testing.T) {
	f := newFixture(t)
	n := &fakeNotifier{db: f.db}
	svc := newDepsSvc(t, f, Deps{Notifier: n})
	res, err := svc.Propose(context.Background(), wk("2026-10-28"), "why", nil, actor)
	if err != nil || res.Verdict != "pending" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(n.calls) != 1 || n.calls[0] != res.ID || n.dates[0] != (civil.Date{Year: 2026, Month: 10, Day: 28}) || !n.seen[0] {
		t.Fatalf("calls %+v", n)
	}
	// invalid proposals do not notify
	if _, err := svc.Propose(context.Background(), wk("2020-01-01"), "why", nil, actor); err != nil || len(n.calls) != 1 {
		t.Fatalf("invalid notified: %v %d", err, len(n.calls))
	}
	// nil notifier works
	svc = newDepsSvc(t, f, Deps{})
	if res, err := svc.Propose(context.Background(), wk("2026-10-28"), "why", nil, actor); err != nil || res.Verdict != "pending" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestProposeStoresTraceContext(t *testing.T) {
	f := newFixture(t)
	n := &fakeNotifier{db: f.db}
	svc := newDepsSvc(t, f, Deps{Notifier: n})
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	ctx, sp := tp.Tracer("t").Start(context.Background(), "op")
	defer sp.End()
	res, err := svc.Propose(ctx, wk("2026-10-28"), "why", nil, actor)
	if err != nil || res.Verdict != "pending" {
		t.Fatalf("%+v %v", res, err)
	}
	p, err := f.db.GetProposal(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc := sp.SpanContext()
	if p.TraceID != sc.TraceID().String() || p.SpanID != sc.SpanID().String() {
		t.Fatalf("stored %q %q want %s %s", p.TraceID, p.SpanID, sc.TraceID(), sc.SpanID())
	}
	if n.links[0].TraceID() != sc.TraceID() {
		t.Fatalf("link %v", n.links[0])
	}
	// no span: empty
	res, _ = svc.Propose(context.Background(), wk("2026-10-28"), "why", nil, actor)
	p, _ = f.db.GetProposal(context.Background(), res.ID)
	if p.TraceID != "" || p.SpanID != "" {
		t.Fatalf("expected empty, got %q %q", p.TraceID, p.SpanID)
	}
}

func TestProposeCountsWarnings(t *testing.T) {
	f := newFixture(t)
	r := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	svc := newDepsSvc(t, f, Deps{Metrics: m})
	res, err := svc.Propose(context.Background(), wk("2026-10-28"), "why", nil, actor)
	if err != nil || len(res.Warnings) == 0 {
		t.Fatalf("need warnings: %+v %v", res, err)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "coachd.proposals.warnings" {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("reason"))
				got[v.AsString()] += dp.Value
			}
		}
	}
	want := map[string]int64{}
	for _, w := range res.Warnings {
		want[w.Code]++
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestExpireDueFailsStale(t *testing.T) {
	f := newFixture(t)
	r := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	svc := newDepsSvc(t, f, Deps{Metrics: m})
	ctx := context.Background()
	res := mustPropose(t, f, "2026-10-25", nil)
	pr, err := f.db.GetProposal(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApproveProposal(ctx, store.Approval{ID: res.ID, ContentHash: pr.ContentHash, Today: civil.Of(*f.now, bratislava),
		Nonce: "n1", NonceExpires: f.now.Add(time.Hour), At: *f.now, Lane: "public", Client: "passkey:1"}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO devices (label, token_hash, created_at) VALUES ('w', x'01', '2026-10-23T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordFetch(ctx, res.ID, 1, f.now.Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ExpireDue(ctx); err != nil || n != 0 {
		t.Fatalf("expired n=%d %v", n, err)
	}
	if p, _ := f.db.GetProposal(ctx, res.ID); p.Status != store.ProposalDeliveryFailed {
		t.Fatalf("status = %s", p.Status)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	var failed int64
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "coachd.proposals.deliveries" {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value(attribute.Key("outcome")); v.AsString() == "failed" {
					failed += dp.Value
				}
			}
		}
	}
	if failed != 1 {
		t.Fatalf("failed counter = %d", failed)
	}
	if !strings.Contains(f.logs.String(), "deliveries failed") {
		t.Fatalf("log missing: %s", f.logs.String())
	}
}

// The failed-delivery series exists at 0 from the start, so the alert's
// increase() sees the first failure.
func TestRunExpiryZeroesFailedDeliveries(t *testing.T) {
	f := newFixture(t)
	r := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	svc := newDepsSvc(t, f, Deps{Metrics: m})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // one pass, then return
	svc.RunExpiry(ctx, time.Hour)
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var points []int64
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "coachd.proposals.deliveries" {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value(attribute.Key("outcome")); v.AsString() == "failed" {
					points = append(points, dp.Value)
				}
			}
		}
	}
	if len(points) != 1 || points[0] != 0 {
		t.Fatalf("failed series = %v, want one point at 0", points)
	}
}

// Only a cycling workout coachd cannot write as a Zwift file gets the
// warning; the reason comes from zwiftworkout.
func TestProposeZwiftWarning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	at := f.now.Add(-time.Hour)
	if err := f.db.PutSyncState(ctx, store.SyncState{DataType: "activities", LastSuccessAt: &at, LastOutcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	ten, km := 600, 5000
	timeStep := workout.Step{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "time", TimeS: &ten}}}
	distStep := workout.Step{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "distance", DistanceM: &km}}}
	codes := func(sport string, steps ...workout.Step) []guidance.Warning {
		t.Helper()
		w := wk("2026-10-25")
		w.Sport, w.Steps = sport, steps
		r, err := f.svc.Propose(ctx, w, "why", nil, actor)
		if err != nil || r.Verdict != "pending" {
			t.Fatalf("r = %+v, %v", r, err)
		}
		var out []guidance.Warning
		for _, x := range r.Warnings {
			if x.Code == "zwift_not_exportable" {
				out = append(out, x)
			}
		}
		return out
	}
	got := codes("cycling", timeStep, distStep)
	if len(got) != 1 || got[0].Message != "not exportable to Zwift: steps[1].duration: distance durations have no Zwift equivalent" {
		t.Fatalf("cycling with a distance step: %+v", got)
	}
	if got := codes("cycling", timeStep); len(got) != 0 {
		t.Fatalf("all-time cycling: %+v", got)
	}
	if got := codes("running", timeStep, distStep); len(got) != 0 {
		t.Fatalf("running: %+v", got)
	}
}

// An expiry is committed before stale deliveries are failed, so a failure in
// that second step (here a missing table; at shutdown a cancelled context)
// must not hide the expiry from the log.
func TestExpireTickLogsCommittedExpiryWhenLaterStepFails(t *testing.T) {
	f := newFixture(t)
	r := mustPropose(t, f, "2026-10-25", nil)
	*f.now = time.Date(2026, 10, 27, 9, 0, 0, 0, bratislava)
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE deliveries`); err != nil {
		t.Fatal(err)
	}

	f.svc.expireTick(context.Background())

	if p, _ := f.db.GetProposal(context.Background(), r.ID); p.Status != store.ProposalExpired {
		t.Fatalf("status = %s", p.Status)
	}
	var expired, failed bool
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log %q: %v", line, err)
		}
		switch rec["msg"] {
		case "proposals expired":
			expired = rec["count"] == float64(1)
		case "proposal expiry failed":
			failed = true
		}
	}
	if !expired || !failed {
		t.Fatalf("expired logged %v, failure logged %v; logs:\n%s", expired, failed, f.logs)
	}
}
