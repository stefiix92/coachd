// Package telemetry wires logs, traces and metrics with an allowlist: only
// declared keys ever leave the process. Health values, free text and secrets
// must never be added to AllowedKeys.
package telemetry

import (
	"context"
	"encoding"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// AllowedKeys is the only set of attribute/label/log keys permitted in telemetry.
var AllowedKeys = []string{"lane", "tool", "outcome", "data_type", "endpoint", "status_class", "reason", "client",
	"activity_id", "calendar_date", "count", "duration_ms", "attempt", "retry_after_s", "error", "error_class",
	"component", "request_id", "version", "addr", "proposal_id", "status", "device_id", "format", "event",
	"http.request.method", "http.response.status_code", "http.route", "network.protocol.version", "server.address"}

const maxErrorLen = 500

func allowed(key string) bool { return slices.Contains(AllowedKeys, key) }

func capString(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	cut := maxErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// redactingHandler drops attributes whose leaf key is not allowlisted. Groups
// are structural: children are filtered, empty groups dropped. The record
// message is NOT filtered, so callers must keep messages constant strings.
type redactingHandler struct{ inner slog.Handler }

// NewRedactingHandler wraps inner so only AllowedKeys attributes pass through
// (including inside groups and WithAttrs) and the "error" value is capped at
// 500 characters. Values are resolved first so a LogValuer cannot smuggle data.
func NewRedactingHandler(inner slog.Handler) slog.Handler { return &redactingHandler{inner: inner} }

func filterAttr(a slog.Attr) (slog.Attr, bool) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		var kept []slog.Attr
		for _, c := range v.Group() {
			if f, ok := filterAttr(c); ok {
				kept = append(kept, f)
			}
		}
		if len(kept) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(kept...)}, true
	}
	if !allowed(a.Key) {
		return slog.Attr{}, false
	}
	if v.Kind() == slog.KindAny {
		// Structured values (maps, structs, slices) could smuggle undeclared
		// nested keys; only error/Stringer/TextMarshaler become strings.
		var str string
		switch x := v.Any().(type) {
		case error:
			str = x.Error()
		case fmt.Stringer:
			str = x.String()
		case encoding.TextMarshaler:
			b, err := x.MarshalText()
			if err != nil {
				return slog.Attr{}, false
			}
			str = string(b)
		default:
			return slog.Attr{}, false
		}
		return slog.String(a.Key, capString(str)), true
	}
	if a.Key == "error" {
		return slog.String("error", capString(v.String())), true
	}
	return slog.Attr{Key: a.Key, Value: v}, true
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if f, ok := filterAttr(a); ok {
			nr.AddAttrs(f)
		}
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var kept []slog.Attr
	for _, a := range attrs {
		if f, ok := filterAttr(a); ok {
			kept = append(kept, f)
		}
	}
	return &redactingHandler{inner: h.inner.WithAttrs(kept)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

type redactingExporter struct{ sdktrace.SpanExporter }

// NewRedactingExporter filters span, event and link attributes by AllowedKeys
// and caps the status description. Resource and scope are left untouched.
func NewRedactingExporter(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
	return &redactingExporter{inner}
}

func filterKVs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if allowed(string(kv.Key)) {
			out = append(out, kv)
		}
	}
	return out
}

func (e *redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		st := tracetest.SpanStubFromReadOnlySpan(s)
		st.Attributes = filterKVs(st.Attributes)
		for j := range st.Events {
			st.Events[j].Attributes = filterKVs(st.Events[j].Attributes)
		}
		for j := range st.Links {
			st.Links[j].Attributes = filterKVs(st.Links[j].Attributes)
		}
		st.Status.Description = capString(st.Status.Description)
		out[i] = st.Snapshot()
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

// ClientClass maps a user agent or MCP client name to a low-cardinality class.
func ClientClass(userAgentOrName string) string {
	s := strings.ToLower(userAgentOrName)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(s, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("claude"):
		return "claude"
	case has("openai", "chatgpt"):
		return "chatgpt"
	case has("cursor"):
		return "cursor"
	case has("vscode", "visual studio code"):
		return "vscode"
	case has("gemini"):
		return "gemini"
	case has("open-webui", "openwebui"):
		return "openwebui"
	}
	return "other"
}
