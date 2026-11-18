package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/summaries"
)

// TestPerfServe measures startup and get_summaries latency over the private
// lane against two years of fake data. Run: COACHD_PERF=1 go test -run Perf -v ./cmd/coachd/
func TestPerfServe(t *testing.T) {
	if os.Getenv("COACHD_PERF") != "1" {
		t.Skip("set COACHD_PERF=1")
	}
	cfg := testCfg(t)
	ctx := context.Background()
	today := civil.Of(time.Now(), cfg.TZ)

	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	ip := func(v int) *int { return &v }
	for i := 0; i < 730; i++ {
		d := today.AddDays(-i)
		hrvStatus := "BALANCED"
		if err := db.UpsertDailyHealth(ctx, store.DailyHealth{Date: d, SleepS: ip(27000 + i%3000), HRVOvernightMs: ip(55 + i%15),
			HRVWeeklyAvgMs: ip(58), RestingHR: ip(48 + i%6), Garmin: store.DailyGarmin{SleepScore: ip(80), HRVStatus: &hrvStatus}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 400; i++ {
		d := today.AddDays(-(i * 730 / 400))
		start := time.Date(d.Year, d.Month, d.Day, 7, 0, 0, 0, cfg.TZ)
		if err := db.UpsertActivity(ctx, store.Activity{ID: int64(1000 + i), StartLocal: start, StartUTC: start.UTC(), Date: d,
			WeekStart: d.WeekStart(), TypeKey: "running", Sport: store.SportRunning, DistanceM: 8000 + float64(i%5000),
			DurationS: 2700, MovingS: 2650, ElevGainM: 60, FITStatus: store.FITUnavailable}, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := summaries.Recompute(ctx, db, today, 730); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 50 tokens: the limiter allows a burst of 30 per token, so rotate.
	const nTok, nCalls = 50, 1000
	toks := make([]string, nTok)
	for i := range toks {
		toks[i] = makeToken(t, cfg, "read")
	}

	started := time.Now()
	sctx, cancel := context.WithCancel(ctx)
	ready := make(chan Addrs, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServe(sctx, cfg, emptyGarmin{}, nil, testKeys(t, cfg), testTel(t), func(a Addrs) { ready <- a })
	}()
	var addrs Addrs
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	startup := time.Since(started)
	defer func() { cancel(); <-done }()

	sessions := make([]*mcp.ClientSession, nTok)
	for i, tok := range toks {
		c := mcp.NewClient(&mcp.Implementation{Name: "perf", Version: "1"}, nil)
		sessions[i], err = c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: "http://" + addrs.Private + "/mcp", DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: headerTransport{"Authorization": "Bearer " + tok}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sessions[i].Close()
	}

	args := map[string]any{"from": today.AddDays(-365).String(), "to": today.String()}
	measure := func() (p50, p95, max time.Duration) {
		lat := make([]time.Duration, 0, nCalls)
		for i := 0; i < nCalls; i++ {
			t0 := time.Now()
			res, err := sessions[i%nTok].CallTool(ctx, &mcp.CallToolParams{Name: "get_summaries", Arguments: args})
			d := time.Since(t0)
			if err != nil || res.IsError {
				t.Fatalf("call %d: %v %+v", i, err, res)
			}
			lat = append(lat, d)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		return lat[nCalls/2], lat[nCalls*95/100], lat[nCalls-1]
	}
	p50, p95, max := measure()
	fmt.Printf("PERF startup=%v get_summaries x%d p50=%v p95=%v max=%v\n", startup, nCalls, p50, p95, max)
	if startup >= time.Second {
		t.Errorf("startup %v >= 1s", startup)
	}
	if p95 >= 50*time.Millisecond {
		t.Errorf("p95 %v >= 50ms", p95)
	}

	// Same calls while a backfill-like writer upserts a transaction every 5 ms.
	wdb, err := store.Open(ctx, filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		for i := 0; wctx.Err() == nil; i++ {
			d := today.AddDays(-(i % 730))
			_ = wdb.UpsertDailyHealth(wctx, store.DailyHealth{Date: d, SleepS: ip(27000 + i%3000), RestingHR: ip(48 + i%6)})
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wp50, wp95, wmax := measure()
	wcancel()
	<-wdone
	wdb.Close()
	fmt.Printf("PERF with-writer get_summaries x%d p50=%v p95=%v max=%v\n", nCalls, wp50, wp95, wmax)
	if wp95 >= 50*time.Millisecond {
		t.Errorf("with-writer p95 %v >= 50ms", wp95)
	}
}
