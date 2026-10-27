package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSendMessage(t *testing.T) {
	e := newEnv(t, fullIdentity())
	canary := "canary-7f3a easy today, HRV 41"
	got := e.call(t, "send_message", map[string]any{"text": canary}).object(t)
	wantKeys(t, "send_message", got, "message")
	msg := obj(t, got["message"])
	want := map[string]any{"id": 1.0, "text": canary, "created_at": "2026-10-23T08:00:00+02:00"}
	if !mapsEqual(msg, want) {
		t.Fatalf("message\n got %v\nwant %v", msg, want)
	}
	stored, err := e.db.RecentMessages(context.Background(), testNow.Add(-time.Hour), 10)
	if err != nil || len(stored) != 1 || stored[0].Text != canary || stored[0].CreatedBy != "agent:private:claude" {
		t.Fatalf("stored %+v %v", stored, err)
	}
	if strings.Contains(e.logs.String(), "canary-7f3a") {
		t.Errorf("logs carry message text: %s", e.logs.String())
	}
	for _, sp := range e.spans.Ended() {
		for _, kv := range sp.Attributes() {
			if strings.Contains(kv.Value.String(), "canary-7f3a") {
				t.Errorf("span %s attribute %s carries message text", sp.Name(), kv.Key)
			}
		}
	}

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"text": ""}, "text: required"},
		{map[string]any{"text": strings.Repeat("é", 301)}, "text: 301 characters exceeds the 300 limit"},
		{map[string]any{"text": "a\x1bb"}, "text: contains a control character (U+001B); only newline and tab are allowed"},
	} {
		if r := e.call(t, "send_message", c.args); !r.isError || r.text != c.want {
			t.Errorf("send_message %.40v: got %+v, want tool error %q", c.args, r, c.want)
		}
	}
}

func TestSendMessageNeedsWriteScope(t *testing.T) {
	e := newEnv(t, readIdentity())
	if r := e.call(t, "send_message", map[string]any{"text": "hi"}); !r.isError {
		t.Fatalf("read-only token sent a message: %+v", r)
	}
	if got, err := e.db.RecentMessages(context.Background(), time.Time{}, 10); err != nil || len(got) != 0 {
		t.Fatalf("stored %+v %v", got, err)
	}
}
