package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/telemetry"
)

// ingestSpanKeys are the only attribute keys ingest spans may carry.
var ingestSpanKeys = []string{"data_type", "outcome", "count", "calendar_date", "error_class", "attempt"}

// traced makes e's Service record its spans in memory.
func traced(t *testing.T, e *env) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	e.svc.tracer = tp.Tracer("test")
	return sr
}

func spansNamed(sr *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

// assertSpanKeys checks every ended span's attributes against the ingest set,
// which must itself be allowlisted.
func assertSpanKeys(t *testing.T, sr *tracetest.SpanRecorder) {
	t.Helper()
	for _, k := range ingestSpanKeys {
		if !slices.Contains(telemetry.AllowedKeys, k) {
			t.Fatalf("ingest span key %q not in telemetry.AllowedKeys", k)
		}
	}
	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			if !slices.Contains(ingestSpanKeys, string(kv.Key)) {
				t.Errorf("span %s carries key %q", s.Name(), kv.Key)
			}
		}
		if len(s.Events()) != 0 || len(s.Status().Description) > 200 {
			t.Errorf("span %s: events %v, status %q", s.Name(), s.Events(), s.Status().Description)
		}
	}
}

func TestSyncSpans(t *testing.T) {
	f := seededFake(t)
	f.setErr(mHRV, &garmin.StatusError{Endpoint: "hrv", Status: 500})
	e := newEnv(t, f, nil)
	sr := traced(t, e)
	mustSync(t, e)

	roots := spansNamed(sr, "ingest.sync")
	if len(roots) != 1 {
		t.Fatalf("%d ingest.sync spans, want 1", len(roots))
	}
	root := roots[0]
	if root.Parent().IsValid() {
		t.Error("ingest.sync is not a root span")
	}
	if o, _ := attr(root, "outcome"); o != OutcomeError {
		t.Errorf("root outcome = %q, want %q", o, OutcomeError)
	}
	types := spansNamed(sr, "ingest.sync.type")
	if len(types) != len(dataTypes) {
		t.Fatalf("%d ingest.sync.type spans, want %d", len(types), len(dataTypes))
	}
	var seen []string
	for _, s := range types {
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("type span %v is not a child of ingest.sync", s.Attributes())
		}
		dt, _ := attr(s, "data_type")
		seen = append(seen, dt)
		o, _ := attr(s, "outcome")
		switch dt {
		case TypeHRV:
			if o != OutcomeError || s.Status().Code != codes.Error {
				t.Errorf("hrv span outcome %q status %v", o, s.Status())
			}
			if c, _ := attr(s, "error_class"); c != "garmin_status" {
				t.Errorf("hrv error_class = %q", c)
			}
		default:
			if o != OutcomeOK || s.Status().Code == codes.Error {
				t.Errorf("%s span outcome %q status %v", dt, o, s.Status())
			}
		}
	}
	slices.Sort(seen)
	want := slices.Clone(dataTypes)
	slices.Sort(want)
	if !slices.Equal(seen, want) {
		t.Errorf("type spans for %v, want %v", seen, want)
	}
	assertSpanKeys(t, sr)
}

func TestBackfillAndFITSpans(t *testing.T) {
	f := seededFake(t)
	e := newEnv(t, f, nil)
	sr := traced(t, e)
	mustSync(t, e) // stores idRun, idManual and idGone
	mustBackfill(t, e)
	e.svc.drainFIT(context.Background())

	chunks := spansNamed(sr, "ingest.backfill.chunk")
	if len(chunks) == 0 {
		t.Fatal("no ingest.backfill.chunk spans")
	}
	for _, s := range chunks {
		if _, ok := attr(s, "calendar_date"); !ok {
			t.Errorf("chunk span without calendar_date: %v", s.Attributes())
		}
		if a, _ := attr(s, "attempt"); a != "1" {
			t.Errorf("chunk attempt = %q, want 1", a)
		}
	}
	drains := spansNamed(sr, "ingest.fit.drain")
	if len(drains) != 1 {
		t.Fatalf("%d ingest.fit.drain spans, want 1", len(drains))
	}
	if c, _ := attr(drains[0], "count"); c != "2" { // idRun stored, idGone 404; idManual never needs one
		t.Errorf("drain count = %q, want 2", c)
	}
	assertSpanKeys(t, sr)
}

// ctxRecorder records, per log message, whether its context carried a span.
type ctxRecorder struct {
	mu     sync.Mutex
	traced map[string]bool
}

func (h *ctxRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *ctxRecorder) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, seen := h.traced[r.Message]; !seen {
		h.traced[r.Message] = true
	}
	h.traced[r.Message] = h.traced[r.Message] && trace.SpanContextFromContext(ctx).IsValid()
	return nil
}
func (h *ctxRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *ctxRecorder) WithGroup(string) slog.Handler      { return h }

func TestIngestLogsCarrySpanContext(t *testing.T) {
	f := seededFake(t)
	f.setErr(mDaily, errors.New("boom"))
	f.setErr(mWeight, &garmin.RateLimitedError{RetryAt: testNow.Add(time.Minute)})
	rec := &ctxRecorder{traced: map[string]bool{}}
	e := newEnv(t, f, rec)
	traced(t, e)
	mustSync(t, e)
	e.clock.Set(testNow.Add(2 * time.Minute))
	e.svc.drainFIT(context.Background())
	e.clock.Set(testNow.Add(time.Hour))
	f.setErr(mDaily, nil)
	f.setErr(mWeight, nil)
	mustBackfill(t, e)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, msg := range []string{"sync type finished", "garmin rate limited", "fit download finished", "backfill chunk stored"} {
		ok, seen := rec.traced[msg]
		if !seen {
			t.Errorf("no %q log", msg)
		} else if !ok {
			t.Errorf("%q logged without span context", msg)
		}
	}
}
