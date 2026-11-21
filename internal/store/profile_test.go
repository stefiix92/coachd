package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func ss(id string, ftp int) SportSettings {
	return SportSettings{Source: "intervals_icu", SourceID: id, Sports: []string{"Ride"}, FTPW: &ftp}
}

func TestReplaceSportSettingsAtomic(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.ReplaceSportSettings(ctx, "intervals_icu", []SportSettings{ss("1", 200), ss("2", 210)}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSportSettings(ctx, "other", []SportSettings{ss("9", 300)}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSportSettings(ctx, "intervals_icu", []SportSettings{ss("3", 220)}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListSportSettings(ctx)
	if err != nil || len(got) != 2 || got[0].Source != "intervals_icu" || got[0].SourceID != "3" || got[1].Source != "other" {
		t.Fatalf("after replace: %+v, %v", got, err)
	}
	if err := db.ReplaceSportSettings(ctx, "intervals_icu", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ListSportSettings(ctx)
	if len(got) != 1 || got[0].Source != "other" {
		t.Fatalf("after nil replace: %+v", got)
	}
}

func TestSportSettingsRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	ftp, ift, thr, mx := 250, 240, 165, 188
	spd := 3.45
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	full := SportSettings{Source: "intervals_icu", SourceID: "2", Sports: []string{"Ride", "VirtualRide"},
		FTPW: &ftp, IndoorFTPW: &ift, ThresholdHRBpm: &thr, MaxHRBpm: &mx, ThresholdSpeedMps: &spd,
		PowerZonesPct: []int{55, 75, 90}, HRZonesBpm: []int{142, 158}, UpdatedAt: &at}
	empty := SportSettings{Source: "intervals_icu", SourceID: "1"}
	if err := db.ReplaceSportSettings(ctx, "intervals_icu", []SportSettings{full, empty}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListSportSettings(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(got[0].Sports) != 0 || got[0].FTPW != nil || got[0].ThresholdSpeedMps != nil || len(got[0].PowerZonesPct) != 0 || len(got[0].HRZonesBpm) != 0 || got[0].UpdatedAt != nil {
		t.Fatalf("all-null entry: %+v", got[0])
	}
	if !reflect.DeepEqual(got[1], full) {
		t.Fatalf("full entry: %+v want %+v", got[1], full)
	}
}
