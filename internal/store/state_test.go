package store

import (
	"context"
	"testing"
	"time"
)

func TestSyncStateRoundTrip(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	z, err := db.GetSyncState(ctx, "sleep")
	if err != nil || z.LastAttemptAt != nil || z.DataType != "" && z.DataType != "sleep" {
		t.Fatalf("absent: %+v %v", z, err)
	}
	now := time.Date(2026, 10, 23, 6, 0, 0, 123, time.UTC)
	s := SyncState{DataType: "sleep", LastAttemptAt: &now, LastSuccessAt: &now, LastOutcome: "ok", Cursor: "2026-10-22", BackfillDone: true}
	if err := db.PutSyncState(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.LastOutcome = "rate_limited"
	s.BackfillDone = false
	db.PutSyncState(ctx, s)
	db.PutSyncState(ctx, SyncState{DataType: "hrv"})
	got, _ := db.GetSyncState(ctx, "sleep")
	if got.LastOutcome != "rate_limited" || got.BackfillDone || got.Cursor != "2026-10-22" || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(now) || got.FreshestDataAt != nil {
		t.Fatalf("%+v", got)
	}
	all, err := db.ListSyncStates(ctx)
	if err != nil || len(all) != 2 || all[0].DataType != "hrv" {
		t.Fatalf("%+v %v", all, err)
	}
}

// DeleteSyncState removes one data type's row and leaves the others; an
// absent row is no error.
func TestDeleteSyncState(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	for _, dt := range []string{"activities", "intervals_activities"} {
		if err := db.PutSyncState(ctx, SyncState{DataType: dt, LastSuccessAt: &now, LastOutcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.DeleteSyncState(ctx, "intervals_activities"); err != nil {
			t.Fatal(err)
		}
	}
	all, err := db.ListSyncStates(ctx)
	if err != nil || len(all) != 1 || all[0].DataType != "activities" || all[0].LastSuccessAt == nil {
		t.Fatalf("after delete: %+v %v, want only activities", all, err)
	}
}
