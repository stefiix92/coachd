package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/triggers"
)

type webhookRig struct {
	w      *Webhook
	db     *store.DB
	reader *sdkmetric.ManualReader
	logs   *bytes.Buffer
	spans  *tracetest.SpanRecorder
}

func newWebhookRig(t *testing.T, url, token string) *webhookRig {
	t.Helper()
	return newWebhookRigEvents(t, url, token, []string{EventActivityStored})
}

func newWebhookRigEvents(t *testing.T, url, token string, events []string) *webhookRig {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	spans := tracetest.NewSpanRecorder()
	w := NewWebhook(url, token, events, db, http.DefaultClient, m, slog.New(slog.NewJSONHandler(logs, nil)),
		sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("notify"))
	w.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	rig := &webhookRig{w: w, db: db, reader: r, logs: logs, spans: spans}
	t.Cleanup(func() {
		if url != "" && strings.Contains(logs.String(), url) || token != "" && strings.Contains(logs.String(), token) {
			t.Error("a log line holds the webhook URL or token")
		}
		for _, s := range spans.Ended() {
			for _, kv := range s.Attributes() {
				if !slices.Contains(telemetry.AllowedKeys, string(kv.Key)) {
					t.Errorf("span attribute %q is not allowlisted", kv.Key)
				}
			}
		}
	})
	return rig
}

func (r *webhookRig) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "coachd.webhook.sends" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func (r *webhookRig) run(t *testing.T, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !cond() {
		t.Fatal("condition not reached")
	}
}

var run26 = store.Activity{ID: 24745625144, Date: civil.Date{Year: 2026, Month: 10, Day: 26}, Sport: store.SportRunning}

func TestWebhookSendsEvent(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var auth, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, auth, ctype = append(bodies, string(b)), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		mu.Unlock()
	}))
	defer srv.Close()
	rig := newWebhookRig(t, srv.URL+"/hook/secret-path", "hook-token")
	rig.w.ActivityStored(context.Background(), run26)
	// Wait for the counted outcome, not the server's receipt: the handler
	// records the body before the response reaches coachd, and stopping Run
	// in between interrupts the send, which is not counted.
	rig.run(t, func() bool { return rig.outcomes(t)["sent"] == 1 })
	if got := rig.outcomes(t); len(got) != 1 {
		t.Fatalf("outcomes %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != `{"event":"activity.stored","activity_id":24745625144,"date":"2026-10-26","sport":"running","proposal_id":null}` {
		t.Fatalf("bodies %q", bodies)
	}
	if auth != "Bearer hook-token" || ctype != "application/json" {
		t.Fatalf("headers %q %q", auth, ctype)
	}
}

// The event names the delivered proposal the activity carried out.
func TestWebhookEventLinksProposal(t *testing.T) {
	var got Event
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		n.Add(1)
	}))
	defer srv.Close()
	rig := newWebhookRig(t, srv.URL, "")
	ctx := context.Background()
	// A delivered proposal whose FIT steps the activity recorded.
	canon := []byte(`{"name":"Easy","sport":"running","date":"2026-10-26","steps":[{"kind":"active","duration":{"type":"time","time_s":1800}}]}`)
	p := store.Proposal{ID: strings.Repeat("ab", 16), Date: run26.Date, Sport: store.SportRunning, Name: "Easy", ContentJSON: canon,
		ContentHash: "h", Status: store.ProposalDelivered, StatusAt: time.Now(), CreatedAt: time.Now(), CreatedLane: "private"}
	if err := rig.db.InsertProposal(ctx, p, store.AuditEvent{At: time.Now(), Event: "proposal_delivered", Target: p.ID, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	steps := []fitdata.WorkoutStep{{Index: 0, Intensity: 0, DurationType: 0, DurationValue: 1800000, TargetType: 2,
		TargetValue: 0xFFFFFFFF, CustomLow: 0xFFFFFFFF, CustomHigh: 0xFFFFFFFF}}
	a := run26
	a.StartLocal, a.StartUTC, a.WeekStart, a.FITStatus = time.Now(), time.Now(), a.Date.WeekStart(), store.FITStored
	if err := rig.db.UpsertActivity(ctx, a, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := rig.db.PutActivityDetail(ctx, a.ID, store.ActivityDetail{Workout: &fitdata.Workout{Steps: steps,
		Fingerprint: fitdata.Fingerprint(steps)}}); err != nil {
		t.Fatal(err)
	}
	rig.w.ActivityStored(ctx, a)
	rig.run(t, func() bool { return n.Load() == 1 })
	if got.ProposalID == nil || *got.ProposalID != p.ID {
		t.Fatalf("event %+v, want proposal %s", got, p.ID)
	}
}

func TestWebhookRetriesAndRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		codes    []int
		attempts int32
		outcome  string
	}{
		"5xx then ok":  {[]int{502, 200}, 2, "sent"},
		"always 5xx":   {[]int{500, 500, 500}, 3, "error"},
		"4xx at once":  {[]int{404}, 1, "rejected"},
		"429 is retry": {[]int{429, 204}, 2, "sent"},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := calls.Add(1) - 1
				w.WriteHeader(tc.codes[min(int(i), len(tc.codes)-1)])
			}))
			defer srv.Close()
			rig := newWebhookRig(t, srv.URL, "")
			rig.w.ActivityStored(context.Background(), run26)
			rig.run(t, func() bool { return rig.outcomes(t)[tc.outcome] == 1 })
			if calls.Load() != tc.attempts {
				t.Fatalf("attempts %d, want %d", calls.Load(), tc.attempts)
			}
		})
	}
}

func TestWebhookQueueFullDrops(t *testing.T) {
	rig := newWebhookRig(t, "http://127.0.0.1:1/hook", "")
	for i := 0; i < queueSize+1; i++ {
		rig.w.ActivityStored(context.Background(), run26) // never blocks: no worker runs
	}
	if got := rig.outcomes(t); got["dropped"] != 1 {
		t.Fatalf("outcomes %v", got)
	}
}

// receiver collects the bodies an httptest server gets.
type receiver struct {
	mu     sync.Mutex
	bodies []string
}

func (r *receiver) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
}

func (r *receiver) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.bodies)
}

var allEvents = []string{"activity.stored", "health.day_ready", "workout.unmatched", "week.closed"}

func TestWebhookDayEventBodies(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer srv.Close()
	rig := newWebhookRigEvents(t, srv.URL, "", allEvents)
	ctx := context.Background()
	rig.w.Fire(ctx, triggers.Event{Name: triggers.HealthDayReady, Date: civil.Date{Year: 2026, Month: 11, Day: 21}})
	rig.w.Fire(ctx, triggers.Event{Name: triggers.WorkoutUnmatched, Date: civil.Date{Year: 2026, Month: 11, Day: 20}, ProposalID: "p_1"})
	rig.w.Fire(ctx, triggers.Event{Name: triggers.WeekClosed, Date: civil.Date{Year: 2026, Month: 11, Day: 16}})
	rig.run(t, func() bool { return rig.outcomes(t)["sent"] == 3 })
	want := []string{
		`{"event":"health.day_ready","date":"2026-11-21"}`,
		`{"event":"workout.unmatched","date":"2026-11-20","proposal_id":"p_1"}`,
		`{"event":"week.closed","date":"2026-11-16"}`,
	}
	if got := rc.got(); !slices.Equal(got, want) {
		t.Fatalf("bodies %q, want %q", got, want)
	}
}

func TestWebhookActivityBodyUnchanged(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer srv.Close()
	rig := newWebhookRigEvents(t, srv.URL, "", allEvents)
	rig.w.ActivityStored(context.Background(), run26)
	rig.run(t, func() bool { return rig.outcomes(t)["sent"] == 1 })
	want := `{"event":"activity.stored","activity_id":24745625144,"date":"2026-10-26","sport":"running","proposal_id":null}`
	if got := rc.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("bodies %q, want %q", got, want)
	}
}

func TestWebhookFiltersEvents(t *testing.T) {
	// Only activity.stored configured: a week event sends nothing.
	rig := newWebhookRigEvents(t, "http://127.0.0.1:1/hook", "", []string{"activity.stored"})
	rig.w.Fire(context.Background(), triggers.Event{Name: triggers.WeekClosed, Date: run26.Date})
	if n := len(rig.w.queue); n != 0 {
		t.Fatalf("queued %d, want 0 for an unconfigured day event", n)
	}
	// Only week.closed configured: an activity sends nothing.
	rig = newWebhookRigEvents(t, "http://127.0.0.1:1/hook", "", []string{"week.closed"})
	rig.w.ActivityStored(context.Background(), run26)
	if n := len(rig.w.queue); n != 0 {
		t.Fatalf("queued %d, want 0 for an unconfigured activity", n)
	}
	rig.w.Fire(context.Background(), triggers.Event{Name: triggers.WeekClosed, Date: run26.Date})
	if n := len(rig.w.queue); n != 1 {
		t.Fatalf("queued %d, want 1 for the configured event", n)
	}
}

func TestWebhookDaySpanKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	rig := newWebhookRigEvents(t, srv.URL, "", allEvents)
	rig.w.Fire(context.Background(), triggers.Event{Name: triggers.WorkoutUnmatched, Date: run26.Date, ProposalID: "p_1"})
	rig.run(t, func() bool { return rig.outcomes(t)["sent"] == 1 })
	var found bool
	for _, s := range rig.spans.Ended() {
		if s.Name() != "event.webhook" {
			continue
		}
		found = true
		got := map[string]string{}
		for _, kv := range s.Attributes() {
			got[string(kv.Key)] = kv.Value.String()
		}
		want := map[string]string{"event": "workout.unmatched", "outcome": "sent", "proposal_id": "p_1"}
		if len(got) != len(want) {
			t.Fatalf("attributes %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("attributes %v, want %v", got, want)
			}
		}
	}
	if !found {
		t.Fatal("no event.webhook span")
	}
}

// Fire sends day events only: the activity event has its own body.
func TestWebhookFireIgnoresActivityEvent(t *testing.T) {
	rig := newWebhookRigEvents(t, "http://127.0.0.1:1/hook", "", allEvents)
	rig.w.Fire(context.Background(), triggers.Event{Name: EventActivityStored, Date: run26.Date})
	if n := len(rig.w.queue); n != 0 {
		t.Fatalf("queued %d, want 0", n)
	}
}
