package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

func checkinAudit(at time.Time, d civil.Date) AuditEvent {
	return AuditEvent{At: at, Lane: "private", Actor: "agent", Client: "agent:private:t1", Event: "checkin_upserted", Target: d.String(), Outcome: "ok"}
}

func TestUpsertCheckinReplacesWhole(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	d := civil.Date{Year: 2026, Month: 11, Day: 20}
	at := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	first := Checkin{Date: d, Soreness: ip(2), Note: "a", Pain: []Pain{{Area: "knee", Side: "left", Level: 3}}, CreatedBy: "agent:private:t1", UpdatedAt: at}
	if err := db.UpsertCheckin(ctx, first, checkinAudit(at, d)); err != nil {
		t.Fatal(err)
	}
	second := Checkin{Date: d, Mood: ip(4), CreatedBy: "agent:private:t2", UpdatedAt: at.Add(time.Hour)}
	if err := db.UpsertCheckin(ctx, second, checkinAudit(at.Add(time.Hour), d)); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListCheckins(ctx, d, d)
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	c := got[0]
	if c.Mood == nil || *c.Mood != 4 || c.Soreness != nil || c.Note != "" || len(c.Pain) != 0 {
		t.Fatalf("not replaced whole: %+v", c)
	}
	if c.CreatedBy != "agent:private:t2" || !c.UpdatedAt.Equal(at.Add(time.Hour)) || c.Date != d {
		t.Fatalf("metadata: %+v", c)
	}
}

func TestUpsertCheckinAudits(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	d := civil.Date{Year: 2026, Month: 11, Day: 20}
	at := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	c := Checkin{Date: d, Soreness: ip(2), Fatigue: ip(3), Note: "a", Pain: []Pain{{Area: "knee", Side: "left", Level: 3}}, CreatedBy: "agent:private:t1", UpdatedAt: at}
	if err := db.UpsertCheckin(ctx, c, checkinAudit(at, d)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertCheckin(ctx, c, checkinAudit(at.Add(time.Minute), d)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT lane, actor, client, event, target, outcome FROM audit_log WHERE event='checkin_upserted'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var lane, actor, client, event, target, outcome string
		if err := rows.Scan(&lane, &actor, &client, &event, &target, &outcome); err != nil {
			t.Fatal(err)
		}
		n++
		if target != "2026-11-20" || client != "agent:private:t1" {
			t.Fatalf("row lacks date or writer: %s %s", target, client)
		}
		// Stored text must hold neither the note nor a score (the writer id
		// contains no digit 2 or 3 outside the date, so strip known parts).
		rest := strings.NewReplacer("2026-11-20", "", "agent:private:t1", "", "checkin_upserted", "", "private", "", "agent", "").Replace(lane + actor + client + event + target + outcome)
		for _, bad := range []string{"a", "2", "3", "knee"} {
			if strings.Contains(strings.ReplaceAll(rest, "ok", ""), bad) {
				t.Fatalf("audit row leaks %q: %q", bad, rest)
			}
		}
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
}

func TestListCheckinsRange(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	for _, day := range []int{20, 18, 15} {
		d := civil.Date{Year: 2026, Month: 11, Day: day}
		if err := db.UpsertCheckin(ctx, Checkin{Date: d, Mood: ip(3), CreatedBy: "x", UpdatedAt: at}, checkinAudit(at, d)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ListCheckins(ctx, civil.Date{Year: 2026, Month: 11, Day: 17}, civil.Date{Year: 2026, Month: 11, Day: 21})
	if err != nil || len(got) != 2 || got[0].Date.Day != 18 || got[1].Date.Day != 20 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
