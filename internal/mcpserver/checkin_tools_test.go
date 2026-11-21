package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/store"
)

func TestLogCheckinStoresAndReturns(t *testing.T) {
	e := newEnv(t, fullIdentity())
	got := e.call(t, "log_checkin", map[string]any{"soreness": 2, "mood": 4,
		"pain": []any{map[string]any{"area": "knee", "side": "left", "level": 3}}, "note": "  tight calf "}).object(t)
	wantKeys(t, "log_checkin", got, "checkin")
	c := obj(t, got["checkin"])
	pain, _ := c["pain"].([]any)
	if c["date"] != "2026-10-23" || c["soreness"] != 2.0 || c["mood"] != 4.0 || c["fatigue"] != nil || c["stress"] != nil ||
		c["motivation"] != nil || c["sleep_quality"] != nil || c["note_untrusted"] != "tight calf" || c["reported_via"] != "agent" ||
		c["updated_at"] != "2026-10-23T08:00:00+02:00" || len(pain) != 1 ||
		!mapsEqual(obj(t, pain[0]), map[string]any{"area": "knee", "side": "left", "level": 3.0}) {
		t.Fatalf("checkin %v", c)
	}
	stored, err := e.db.ListCheckins(context.Background(), mustDate(t, "2026-10-23"), mustDate(t, "2026-10-23"))
	if err != nil || len(stored) != 1 || stored[0].CreatedBy != "agent:private:claude" || stored[0].Note != "tight calf" {
		t.Fatalf("stored %+v %v", stored, err)
	}
	var found bool
	evs, err := e.db.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Event == "checkin_upserted" {
			found = true
			if ev.Target != "2026-10-23" || ev.Client != "claude" || ev.Outcome != "ok" {
				t.Errorf("audit %+v", ev)
			}
		}
	}
	if !found {
		t.Error("no checkin_upserted audit event")
	}
	if got := e.call(t, "log_checkin", map[string]any{"note": "x"}).object(t); obj(t, got["checkin"])["pain"] == nil {
		t.Error("pain is null, want []")
	}
}

func TestLogCheckinBounds(t *testing.T) {
	e := newEnv(t, fullIdentity())
	pain := func(area, side string, level int) []any {
		return []any{map[string]any{"area": area, "side": side, "level": level}}
	}
	var cases []map[string]any
	for _, f := range []string{"soreness", "fatigue", "stress", "mood", "motivation", "sleep_quality"} {
		for _, v := range []int{0, 6} {
			cases = append(cases, map[string]any{f: v})
		}
	}
	cases = append(cases,
		map[string]any{"pain": pain("knee", "left", -1)},
		map[string]any{"pain": pain("knee", "left", 11)},
		map[string]any{"pain": pain("elbow", "left", 3)},
		map[string]any{"pain": pain("knee", "up", 3)},
		map[string]any{"pain": []any{pain("knee", "left", 1)[0], pain("knee", "left", 1)[0], pain("knee", "left", 1)[0],
			pain("knee", "left", 1)[0], pain("knee", "left", 1)[0], pain("knee", "left", 1)[0]}},
		map[string]any{"note": strings.Repeat("é", 301)},
		map[string]any{"note": "a\x1bb"},
	)
	for _, args := range cases {
		if r := e.call(t, "log_checkin", args); !r.isError {
			t.Errorf("accepted %v: %s", args, r.text)
		}
	}
	// The ends are valid.
	for _, args := range []map[string]any{{"soreness": 1, "fatigue": 5}, {"pain": pain("lower_back", "none", 0)},
		{"pain": pain("other", "both", 10)}, {"note": strings.Repeat("é", 300)}} {
		if r := e.call(t, "log_checkin", args); r.isError {
			t.Errorf("refused %v: %s", args, r.text)
		}
	}
	stored, _ := e.db.ListCheckins(context.Background(), mustDate(t, "2026-10-23"), mustDate(t, "2026-10-23"))
	if len(stored) != 1 {
		t.Fatalf("stored %d rows", len(stored))
	}
}

func TestLogCheckinEmpty(t *testing.T) {
	e := newEnv(t, fullIdentity())
	for _, args := range []map[string]any{{}, {"pain": []any{}}, {"note": "  \n "}, {"date": "2026-10-22"}} {
		e.call(t, "log_checkin", args).wantError(t, "at least one")
	}
}

func TestLogCheckinDateWindow(t *testing.T) {
	// 23:30 UTC on the 21st is already the 22nd in Bratislava.
	now := time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC)
	e := newEnv(t, fullIdentity(), func(d *Deps) { d.Now = func() time.Time { return now } })
	for _, c := range []struct {
		date string
		ok   bool
	}{{"2026-11-22", true}, {"2026-11-16", true}, {"2026-11-15", false}, {"2026-11-23", false}, {"22.11.2026", false}} {
		r := e.call(t, "log_checkin", map[string]any{"date": c.date, "mood": 3})
		if r.isError == c.ok {
			t.Errorf("date %s: isError %v: %s", c.date, r.isError, r.text)
		}
	}
	if r := e.call(t, "log_checkin", map[string]any{"mood": 3}); r.isError || obj(t, r.object(t)["checkin"])["date"] != "2026-11-22" {
		t.Errorf("default date: %+v", r)
	}
}

func TestLogCheckinReplaces(t *testing.T) {
	e := newEnv(t, fullIdentity())
	e.call(t, "log_checkin", map[string]any{"soreness": 4, "fatigue": 4, "note": "first"}).object(t)
	e.call(t, "log_checkin", map[string]any{"mood": 5}).object(t)
	days := e.call(t, "get_daily_health", map[string]any{"from": "2026-10-23", "to": "2026-10-23"}).object(t)["days"].([]any)
	if len(days) != 1 {
		t.Fatalf("days %v", days)
	}
	c := obj(t, obj(t, days[0])["checkin"])
	if c["mood"] != 5.0 || c["soreness"] != nil || c["fatigue"] != nil || c["note_untrusted"] != "" {
		t.Fatalf("second check-in did not replace the first: %v", c)
	}
}

func TestLogCheckinNeedsWriteScope(t *testing.T) {
	e := newEnv(t, readIdentity())
	if r := e.call(t, "log_checkin", map[string]any{"mood": 3}); !r.isError {
		t.Fatalf("read-only token logged a check-in: %+v", r)
	}
	if got, err := e.db.ListCheckins(context.Background(), mustDate(t, "2026-10-01"), mustDate(t, "2026-11-01")); err != nil || len(got) != 0 {
		t.Fatalf("stored %+v %v", got, err)
	}
}

func TestDailyHealthShowsCheckin(t *testing.T) {
	e := newEnv(t, fullIdentity())
	ctx := context.Background()
	for _, h := range []store.DailyHealth{
		{Date: mustDate(t, "2026-10-20"), RestingHR: ip(48)},
		{Date: mustDate(t, "2026-10-21"), RestingHR: ip(50)},
	} {
		if err := e.db.UpsertDailyHealth(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"2026-10-19", "2026-10-21"} { // 19th: check-in only; 21st: both
		if r := e.call(t, "log_checkin", map[string]any{"date": d, "stress": 2}); r.isError {
			t.Fatal(r.text)
		}
	}
	days := e.call(t, "get_daily_health", map[string]any{"from": "2026-10-01", "to": "2026-10-23"}).object(t)["days"].([]any)
	if len(days) != 3 {
		t.Fatalf("days %v", days)
	}
	d0, d1, d2 := obj(t, days[0]), obj(t, days[1]), obj(t, days[2])
	if d0["date"] != "2026-10-19" || d1["date"] != "2026-10-20" || d2["date"] != "2026-10-21" {
		t.Fatalf("order %v %v %v", d0["date"], d1["date"], d2["date"])
	}
	if d0["resting_hr_bpm"] != nil || d0["sleep_duration_s"] != nil || d0["checkin"] == nil || obj(t, d0["checkin"])["stress"] != 2.0 {
		t.Errorf("check-in-only day %v", d0)
	}
	if d1["resting_hr_bpm"] != 48.0 {
		t.Errorf("health-only day %v", d1)
	}
	if v, ok := d1["checkin"]; !ok || v != nil {
		t.Errorf("health-only day checkin = %v (present %v), want null", v, ok)
	}
	if d2["resting_hr_bpm"] != 50.0 || d2["checkin"] == nil {
		t.Errorf("day with both %v", d2)
	}
}

func TestGetProfile(t *testing.T) {
	e := newEnv(t, readIdentity())
	ctx := context.Background()
	got := e.call(t, "get_profile", nil).object(t)
	if ss, ok := got["sport_settings"].([]any); !ok || len(ss) != 0 {
		t.Fatalf("empty store: %v, want []", got["sport_settings"])
	}
	upd := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC) // the 21st in Bratislava
	ftp, hr, spd := 250, 170, 3.5
	if err := e.db.ReplaceSportSettings(ctx, "intervals_icu", []store.SportSettings{
		{Source: "intervals_icu", SourceID: "1", Sports: []string{"Ride", "VirtualRide"}, FTPW: &ftp, ThresholdHRBpm: &hr,
			PowerZonesPct: []int{55, 75, 150}, HRZonesBpm: []int{120, 150, 190}, UpdatedAt: &upd},
		{Source: "intervals_icu", SourceID: "2", Sports: []string{"Run"}, ThresholdSpeedMps: &spd},
	}); err != nil {
		t.Fatal(err)
	}
	ss := e.call(t, "get_profile", nil).object(t)["sport_settings"].([]any)
	if len(ss) != 2 {
		t.Fatalf("entries %v", ss)
	}
	a, b := obj(t, ss[0]), obj(t, ss[1])
	if a["source"] != "intervals_icu" || a["ftp_w"] != 250.0 || a["threshold_hr_bpm"] != 170.0 || a["indoor_ftp_w"] != nil ||
		a["updated_on"] != "2026-10-21" || len(a["sports"].([]any)) != 2 || len(a["power_zone_upper_pct_ftp"].([]any)) != 3 ||
		len(a["hr_zone_upper_bpm"].([]any)) != 3 {
		t.Errorf("first entry %v", a)
	}
	if b["threshold_speed_mps"] != 3.5 || b["updated_on"] != nil || b["ftp_w"] != nil || len(b["power_zone_upper_pct_ftp"].([]any)) != 0 {
		t.Errorf("second entry %v", b)
	}
}

func TestProfileHasNoZoneNames(t *testing.T) {
	e := newEnv(t, readIdentity())
	ftp := 250
	if err := e.db.ReplaceSportSettings(context.Background(), "intervals_icu", []store.SportSettings{
		{Source: "intervals_icu", SourceID: "1", Sports: []string{"Ride"}, FTPW: &ftp, PowerZonesPct: []int{55, 75}}}); err != nil {
		t.Fatal(err)
	}
	r := e.call(t, "get_profile", nil)
	for _, bad := range []string{`"name"`, "Endurance", "zone_names"} {
		if strings.Contains(r.text, bad) {
			t.Errorf("result contains %q: %s", bad, r.text)
		}
	}
}
