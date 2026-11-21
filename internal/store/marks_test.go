package store

import (
	"context"
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
