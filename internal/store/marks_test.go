package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestMarkEventOnce(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
	first, err := db.MarkEvent(ctx, "week.closed", "2026-11-16", at)
	if err != nil || !first {
		t.Fatalf("first = %v, %v", first, err)
	}
	first, err = db.MarkEvent(ctx, "week.closed", "2026-11-16", at.Add(time.Hour))
	if err != nil || first {
		t.Fatalf("second = %v, %v", first, err)
	}
	first, _ = db.MarkEvent(ctx, "week.closed", "2026-11-23", at)
	if !first {
		t.Fatal("another key must be first")
	}
	first, _ = db.MarkEvent(ctx, "health.day_ready", "2026-11-16", at)
	if !first {
		t.Fatal("another event must be first")
	}
}

func TestEventMarks(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if got, err := db.EventMarks(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	at := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
	db.MarkEvent(ctx, "week.closed", "2026-11-16", at)
	db.MarkEvent(ctx, "health.day_ready", "2026-11-21", at)
	db.MarkEvent(ctx, "week.closed", "2026-11-09", at)
	got, err := db.EventMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []EventMark{{"health.day_ready", "2026-11-21"}, {"week.closed", "2026-11-09"}, {"week.closed", "2026-11-16"}}
	if !slices.Equal(got, want) {
		t.Fatalf("marks %v, want %v", got, want)
	}
}
