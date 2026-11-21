package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(NewRedactingHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})))
}

func assertAbsent(t *testing.T, out string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(out, n) {
			t.Errorf("output contains %q: %s", n, out)
		}
	}
}

func TestRedactingHandlerDropsUnknownKeys(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).Info("msg",
		"hrv_ms", 4242424, "sleep_s", 2520011, "token", "cdt_x", "name_untrusted", "secretname", "outcome", "ok")
	out := buf.String()
	if !strings.Contains(out, `"outcome":"ok"`) {
		t.Errorf("missing outcome: %s", out)
	}
	assertAbsent(t, out, "hrv_ms", "sleep_s", "token", "name_untrusted", "4242424", "2520011", "cdt_x", "secretname")
}

func TestRedactingHandlerCapsError(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).Info("msg", "error", strings.Repeat("a", 2000))
	out := buf.String()
	if strings.Contains(out, strings.Repeat("a", 501)) {
		t.Errorf("error not capped")
	}
	if !strings.Contains(out, strings.Repeat("a", 500)) {
		t.Errorf("error cap too aggressive")
	}
}

func TestCapStringRuneSafe(t *testing.T) {
	for _, in := range []string{"x" + strings.Repeat("é", 600), "xx" + strings.Repeat("€", 400)} {
		got := capString(in)
		if !utf8.ValidString(got) || len(got) > 500 || !strings.HasPrefix(in, got) || len(got) < 497 {
			t.Errorf("bad cap: len=%d valid=%v", len(got), utf8.ValidString(got))
		}
	}
}

type strg struct{}

func (strg) String() string { return "stringer-ok" }

func TestRedactingHandlerStructuredValues(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).Info("msg",
		"outcome", map[string]any{"hrv_ms": 4242424},
		"count", struct{ RestingHR int }{5151515},
		"reason", []int{3131313},
		"tool", strg{},
		"error", errors.New("boom"))
	out := buf.String()
	assertAbsent(t, out, "hrv_ms", "4242424", "RestingHR", "5151515", "3131313", `"outcome"`, `"count"`, `"reason"`)
	for _, want := range []string{`"tool":"stringer-ok"`, `"error":"boom"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
}

func TestRedactingHandlerGroupsAndWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf).With("hrv_ms", 7777771, "component", "sync").WithGroup("g")
	l.Info("msg",
		slog.Group("inner", slog.Int("resting_hr", 5151515), slog.String("outcome", "ok")),
		slog.Group("empty", slog.String("weight_kg", "81.5")),
		slog.String("sleep_s", "7777"))
	out := buf.String()
	assertAbsent(t, out, "hrv_ms", "7777771", "resting_hr", "5151515", "weight_kg", "81.5", "sleep_s", "7777", "empty")
	for _, want := range []string{`"component":"sync"`, `"outcome":"ok"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
}

type leaky struct{}

func (leaky) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("hrv_ms", 9999999), slog.String("outcome", "fine"))
}

type leakyError struct{}

func (leakyError) LogValue() slog.Value { return slog.StringValue(strings.Repeat("z", 900)) }

func TestRedactingHandlerResolvesLogValuer(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).Info("msg", "outcome", leaky{}, "error", leakyError{})
	out := buf.String()
	assertAbsent(t, out, "hrv_ms", "9999999", strings.Repeat("z", 501))
	if !strings.Contains(out, "fine") {
		t.Errorf("group children lost: %s", out)
	}
}

type fakeExporter struct{ got []sdktrace.ReadOnlySpan }

func (f *fakeExporter) ExportSpans(_ context.Context, s []sdktrace.ReadOnlySpan) error {
	f.got = append(f.got, s...)
	return nil
}
func (f *fakeExporter) Shutdown(context.Context) error { return nil }

func TestRedactingExporterFiltersAttributes(t *testing.T) {
	fe := &fakeExporter{}
	exp := NewRedactingExporter(fe)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	ctx, linked := tp.Tracer("t").Start(context.Background(), "linked")
	linked.End()
	_, sp := tp.Tracer("t").Start(ctx, "op",
		trace.WithAttributes(attribute.String("outcome", "ok"), attribute.Int("resting_hr", 44)),
		trace.WithLinks(trace.Link{SpanContext: linked.SpanContext(), Attributes: []attribute.KeyValue{
			attribute.String("hrv_ms", "88"), attribute.String("tool", "x")}}))
	sp.AddEvent("ev", trace.WithAttributes(attribute.String("sleep_s", "7"), attribute.String("count", "3")))
	sp.SetStatus(2, strings.Repeat("e", 900))
	sp.End()

	if len(fe.got) != 2 {
		t.Fatalf("got %d spans", len(fe.got))
	}
	s := fe.got[1]
	if len(s.Attributes()) != 1 || s.Attributes()[0].Key != "outcome" {
		t.Errorf("attrs: %v", s.Attributes())
	}
	if ev := s.Events(); len(ev) != 1 || len(ev[0].Attributes) != 1 || ev[0].Attributes[0].Key != "count" {
		t.Errorf("events: %v", ev)
	}
	if l := s.Links(); len(l) != 1 || len(l[0].Attributes) != 1 || l[0].Attributes[0].Key != "tool" {
		t.Errorf("links: %v", l)
	}
	if len(s.Status().Description) > 500 {
		t.Errorf("status desc not capped")
	}
}

func TestClientClass(t *testing.T) {
	cases := map[string]string{
		"Claude-User/1.0": "claude", "openai-mcp": "chatgpt", "ChatGPT-User": "chatgpt",
		"Cursor/0.4": "cursor", "vscode-mcp": "vscode", "Visual Studio Code": "vscode",
		"gemini-cli": "gemini", "open-webui": "openwebui", "OpenWebUI/1": "openwebui",
		"x": "other", "": "other",
	}
	for in, want := range cases {
		if got := ClientClass(in); got != want {
			t.Errorf("ClientClass(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAllowedKeysPinned fixes the allowlist: adding a key must be a deliberate
// change here too. Never add health values, free text or secrets.
func TestAllowedKeysPinned(t *testing.T) {
	want := []string{"lane", "tool", "outcome", "data_type", "endpoint", "status_class", "reason", "client",
		"activity_id", "calendar_date", "count", "duration_ms", "attempt", "retry_after_s", "error", "error_class",
		"component", "request_id", "version", "addr", "proposal_id", "status", "device_id", "format", "event",
		"http.request.method", "http.response.status_code", "http.route", "network.protocol.version", "server.address"}
	if !slices.Equal(AllowedKeys, want) {
		t.Fatalf("AllowedKeys changed:\n got %q\nwant %q", AllowedKeys, want)
	}
}
