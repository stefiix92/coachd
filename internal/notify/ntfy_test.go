package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/telemetry"
)

type rig struct {
	n      *Ntfy
	reader *sdkmetric.ManualReader
	logs   *bytes.Buffer
	spans  *tracetest.SpanRecorder
}

func newRig(t *testing.T, url, token string) *rig {
	t.Helper()
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	m, err := telemetry.NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	spans := tracetest.NewSpanRecorder()
	n := NewNtfy(url, token, "https://coachd.example", http.DefaultClient, m,
		slog.New(slog.NewJSONHandler(logs, nil)), sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("notify"))
	n.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return &rig{n: n, reader: r, logs: logs, spans: spans}
}

func (r *rig) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "coachd.notify.sends" {
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

var d20261028 = civil.Date{Year: 2026, Month: 10, Day: 28}

func runUntil(t *testing.T, n *Ntfy, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestNtfyMessage(t *testing.T) {
	for _, token := range []string{"tok", ""} {
		var mu sync.Mutex
		var got http.Header
		var method string
		var body []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			got, method = req.Header.Clone(), req.Method
			body, _ = io.ReadAll(req.Body)
		}))
		r := newRig(t, srv.URL, token)
		r.n.ProposalPending("abc123", d20261028, trace.SpanContext{})
		runUntil(t, r.n, func() bool { mu.Lock(); defer mu.Unlock(); return got != nil })
		srv.Close()
		mu.Lock()
		if method != http.MethodPost || string(body) != "Workout for Wed 28 Oct ready to review" {
			t.Errorf("method %q body %q", method, body)
		}
		if got.Get("Title") != "coachd" {
			t.Errorf("Title %q", got.Get("Title"))
		}
		if want := "view, Review, https://coachd.example/p/abc123, clear=true"; got.Get("Actions") != want {
			t.Errorf("Actions %q", got.Get("Actions"))
		}
		if token != "" && got.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization %q", got.Get("Authorization"))
		}
		if token == "" {
			if _, ok := got["Authorization"]; ok {
				t.Errorf("Authorization sent with empty token")
			}
		}
		mu.Unlock()
	}
}

func TestNtfyRetriesThenGivesUp(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	r := newRig(t, srv.URL, "")
	var sleeps []time.Duration
	r.n.sleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	r.n.ProposalPending("id1", d20261028, trace.SpanContext{})
	runUntil(t, r.n, func() bool { return r.outcomes(t)["error"] == 1 })
	if n.Load() != 3 {
		t.Fatalf("requests %d", n.Load())
	}
	if got := r.outcomes(t); got["error"] != 1 || len(got) != 1 {
		t.Fatalf("outcomes %v", got)
	}
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second || sleeps[1] != 25*time.Second {
		t.Fatalf("sleeps %v", sleeps)
	}
	if !strings.Contains(r.logs.String(), `"status_class":"5xx"`) || !strings.Contains(r.logs.String(), `"proposal_id":"id1"`) {
		t.Fatalf("log %s", r.logs)
	}
}

func TestNtfyRejectedNoRetry(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		w.WriteHeader(403)
	}))
	defer srv.Close()
	r := newRig(t, srv.URL, "")
	r.n.ProposalPending("id1", d20261028, trace.SpanContext{})
	runUntil(t, r.n, func() bool { return r.outcomes(t)["rejected"] == 1 })
	if n.Load() != 1 {
		t.Fatalf("requests %d", n.Load())
	}
}

func TestNtfyDropsWhenFull(t *testing.T) {
	r := newRig(t, "http://127.0.0.1:1", "")
	for i := 0; i < 33; i++ {
		r.n.ProposalPending("id", d20261028, trace.SpanContext{})
	}
	if got := r.outcomes(t); got["dropped"] != 1 {
		t.Fatalf("outcomes %v", got)
	}
}

func TestNtfyRunStopsOnCancel(t *testing.T) {
	r := newRig(t, "http://127.0.0.1:1", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.n.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestNtfyLogsNoSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	topic := srv.URL + "/secret-topic-xyz"
	r := newRig(t, topic, "supersecrettoken")
	r.n.ProposalPending("id1", d20261028, trace.SpanContext{})
	runUntil(t, r.n, func() bool { return r.outcomes(t)["error"] == 1 })
	// also a network failure
	r2 := newRig(t, "http://127.0.0.1:1/secret-topic-xyz", "supersecrettoken")
	r2.n.ProposalPending("id2", d20261028, trace.SpanContext{})
	runUntil(t, r2.n, func() bool { return r2.outcomes(t)["error"] == 1 })
	for _, l := range []string{r.logs.String(), r2.logs.String()} {
		if l == "" {
			t.Fatal("no logs captured")
		}
		for _, bad := range []string{"supersecrettoken", "secret-topic-xyz", "127.0.0.1"} {
			if strings.Contains(l, bad) {
				t.Fatalf("log leaks %q: %s", bad, l)
			}
		}
	}
}

func TestProposalPendingDoesNotWait(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	r := newRig(t, srv.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.n.Run(ctx)
	start := time.Now()
	r.n.ProposalPending("id1", d20261028, trace.SpanContext{})
	if time.Since(start) > 10*time.Millisecond {
		t.Fatalf("ProposalPending took %v", time.Since(start))
	}
}

// The proposal.notify span records the send's outcome; a failure marks the
// span as an error.
func TestNtfySpanOutcome(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
		failed  bool
	}{
		{http.StatusOK, "sent", false},
		{http.StatusForbidden, "rejected", true},
		{http.StatusInternalServerError, "error", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(tc.status) }))
		r := newRig(t, srv.URL, "")
		r.n.ProposalPending("id1", d20261028, trace.SpanContext{})
		runUntil(t, r.n, func() bool { return r.outcomes(t)[tc.outcome] == 1 })
		srv.Close()
		spans := r.spans.Ended()
		if len(spans) != 1 || spans[0].Name() != "proposal.notify" {
			t.Fatalf("%s: spans = %v", tc.outcome, spans)
		}
		attrs := map[string]string{}
		for _, kv := range spans[0].Attributes() {
			if !slices.Contains(telemetry.AllowedKeys, string(kv.Key)) {
				t.Errorf("%s: attribute %q is not allowlisted", tc.outcome, kv.Key)
			}
			attrs[string(kv.Key)] = kv.Value.String()
		}
		if attrs["outcome"] != tc.outcome || attrs["proposal_id"] != "id1" {
			t.Errorf("%s: attributes = %v", tc.outcome, attrs)
		}
		st := spans[0].Status()
		switch {
		case tc.failed && (st.Code != codes.Error || st.Description != tc.outcome):
			t.Errorf("%s: status = %+v, want Error %q", tc.outcome, st, tc.outcome)
		case !tc.failed && st.Code == codes.Error:
			t.Errorf("%s: status = %+v, want no error", tc.outcome, st)
		}
	}
}

// The connection notices (spec §9, M14) carry fixed texts, no client name,
// and link to the Connected apps page.
func TestNtfyAppNotices(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(*Ntfy)
		body string
	}{
		{"connected", (*Ntfy).AppConnected, "A new app was connected to coachd"},
		{"revoked", (*Ntfy).ConnectionRevoked, "coachd revoked an app connection after a token was reused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got http.Header
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				got = req.Header.Clone()
				body, _ = io.ReadAll(req.Body)
			}))
			defer srv.Close()
			r := newRig(t, srv.URL, "tok")
			tc.send(r.n)
			runUntil(t, r.n, func() bool { return r.outcomes(t)["sent"] == 1 })
			mu.Lock()
			defer mu.Unlock()
			if got == nil {
				t.Fatal("no notice sent")
			}
			if string(body) != tc.body {
				t.Errorf("body %q, want %q", body, tc.body)
			}
			if got.Get("Title") != "coachd" || got.Get("Authorization") != "Bearer tok" {
				t.Errorf("Title %q Authorization %q", got.Get("Title"), got.Get("Authorization"))
			}
			if want := "view, Open, https://coachd.example/apps, clear=true"; got.Get("Actions") != want {
				t.Errorf("Actions %q, want %q", got.Get("Actions"), want)
			}
			// The only free text is the fixed body: no header carries more.
			for k, v := range got {
				switch k {
				case "Title", "Actions", "Authorization", "Content-Type", "Content-Length", "User-Agent", "Accept-Encoding":
				default:
					t.Errorf("unexpected header %s: %q", k, v)
				}
			}
			spans := r.spans.Ended()
			if len(spans) != 1 {
				t.Fatalf("spans %v", spans)
			}
			for _, kv := range spans[0].Attributes() {
				if !slices.Contains(telemetry.AllowedKeys, string(kv.Key)) || kv.Key == "proposal_id" {
					t.Errorf("span attribute %q", kv.Key)
				}
			}
		})
	}
}

// The token endpoint calls the notices inline: a full queue drops and
// counts instead of blocking, as ProposalPending does.
func TestNtfyAppNoticesDoNotBlock(t *testing.T) {
	r := newRig(t, "http://127.0.0.1:1", "")
	start := time.Now()
	for i := 0; i < queueSize; i++ {
		r.n.AppConnected()
	}
	r.n.ConnectionRevoked()
	r.n.AppConnected()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("enqueue took %v", d)
	}
	if got := r.outcomes(t); got["dropped"] != 2 {
		t.Fatalf("outcomes %v, want dropped=2", got)
	}
	if strings.Contains(r.logs.String(), "proposal_id") {
		t.Errorf("dropped app notice logged a proposal id: %s", r.logs)
	}
}
