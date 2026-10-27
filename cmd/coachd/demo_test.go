package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/config"
	"github.com/stefiix92/coachd/internal/demo"
	"github.com/stefiix92/coachd/internal/garminkey"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
)

func demoCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testCfg(t)
	cfg.Demo, cfg.GarminAPIKey, cfg.GarminKeyExpires = true, "", civil.Date{}
	return cfg
}

func startDemo(t *testing.T, cfg *config.Config, tel *telemetry.Telemetry) *running {
	t.Helper()
	return startServeGarmin(t, cfg, tel, demo.New(cfg.TZ, time.Now), garminkey.NoKey(demo.KeyExpires))
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %+v", name, err, res)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// eventually retries check until it reports true or 20 s pass.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !check(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A demo serve on an empty data directory marks it, ingests demo data, and
// an agent reads it over the private lane: activities with FIT detail, daily
// health and a far-future key expiry.
func TestServeDemoEndToEnd(t *testing.T) {
	cfg := demoCfg(t)
	if err := validateServeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	tel := testTel(t)
	tel.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	r := startDemo(t, cfg, tel)
	// As in the docs: the token is created once serve is up.
	tok := makeToken(t, cfg, "read")

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: "http://" + r.addrs.Private + "/mcp", DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: headerTransport{"Authorization": "Bearer " + tok}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	today := civil.Of(time.Now(), cfg.TZ)
	var acts struct {
		Activities []struct {
			ID   int64  `json:"id"`
			Name string `json:"name_untrusted"`
		} `json:"activities"`
	}
	eventually(t, "demo activities", func() bool {
		callTool(t, cs, "list_activities", map[string]any{"from": today.AddDays(-20).String(), "to": today.String()}, &acts)
		return len(acts.Activities) >= 5
	})
	for _, a := range acts.Activities {
		if !strings.HasPrefix(a.Name, "Demo ") {
			t.Errorf("activity %d named %q", a.ID, a.Name)
		}
	}
	var detail struct {
		Laps []struct {
			DistanceKm float64 `json:"distance_km"`
		} `json:"laps"`
		Series []json.RawMessage `json:"series"`
	}
	eventually(t, "FIT detail", func() bool {
		callTool(t, cs, "get_activity", map[string]any{"id": acts.Activities[0].ID, "detail": "series"}, &detail)
		return len(detail.Laps) > 0 && len(detail.Series) > 0
	})
	var health struct {
		Days []json.RawMessage `json:"days"`
	}
	callTool(t, cs, "get_daily_health", map[string]any{"from": today.AddDays(-10).String(), "to": today.AddDays(-1).String()}, &health)
	if len(health.Days) != 10 {
		t.Errorf("daily health days = %d, want 10", len(health.Days))
	}
	var status struct {
		GarminKey struct {
			ExpiresOn *string `json:"expires_on"`
		} `json:"garmin_key"`
	}
	callTool(t, cs, "get_status", map[string]any{}, &status)
	if status.GarminKey.ExpiresOn == nil || *status.GarminKey.ExpiresOn != demo.KeyExpires.String() {
		t.Errorf("garmin key expires %v", status.GarminKey.ExpiresOn)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}

	if marked, err := store.ReadDemo(context.Background(), filepath.Join(cfg.DataDir, "coachd.db")); err != nil || !marked {
		t.Fatalf("data directory not marked demo: %v %v", marked, err)
	}
	// One "demo mode" line, with allowlisted keys only, and no token anywhere.
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, tok) {
			t.Fatalf("token logged: %s", line)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); !strings.HasPrefix(msg, "demo mode") {
			continue
		}
		found++
		if rec["level"] != "WARN" {
			t.Errorf("demo mode logged at %v", rec["level"])
		}
		for k := range rec {
			if k != "time" && k != "level" && k != "msg" && !slices.Contains(telemetry.AllowedKeys, k) {
				t.Errorf("demo log key %q is not allowlisted", k)
			}
		}
	}
	if found != 1 {
		t.Errorf("%d demo mode log lines, want 1", found)
	}

	// The marked directory starts again in demo mode.
	startDemo(t, cfg, testTel(t)).stop(t)
}

// Demo mode never writes into a real data directory.
func TestServeDemoRefusesRealData(t *testing.T) {
	cfg := testCfg(t)
	makeToken(t, cfg, "read") // a real coachd.db, created outside demo mode
	cfg.Demo, cfg.GarminAPIKey, cfg.GarminKeyExpires = true, "", civil.Date{}
	err := runServe(context.Background(), cfg, demo.New(cfg.TZ, time.Now), garminkey.NoKey(demo.KeyExpires), testTel(t), nil)
	if err == nil || !strings.Contains(err.Error(), "COACHD_DEMO=true") || !strings.Contains(err.Error(), "not a demo one") {
		t.Fatalf("err = %v", err)
	}
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if marked, err := db.Demo(context.Background()); err != nil || marked {
		t.Fatalf("real database marked demo: %v %v", marked, err)
	}
	if sts, err := db.ListSyncStates(context.Background()); err != nil || len(sts) != 0 {
		t.Fatalf("a sync ran: %+v %v", sts, err)
	}
}

// A real coachd never starts on demo data.
func TestServeRefusesDemoData(t *testing.T) {
	cfg := testCfg(t)
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	err = runServe(context.Background(), cfg, emptyGarmin{}, testKeys(t, cfg), testTel(t), nil)
	if err == nil || !strings.Contains(err.Error(), "holds demo data") || !strings.Contains(err.Error(), "COACHD_DEMO=true") {
		t.Fatalf("err = %v", err)
	}
}

// An empty coachd.db file (no -wal) is a new database either mode may claim.
func TestServeDemoClaimsEmptyFile(t *testing.T) {
	cfg := demoCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "coachd.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startDemo(t, cfg, testTel(t)).stop(t)
}

func TestValidateServeConfigDemo(t *testing.T) {
	cfg := demoCfg(t)
	if err := validateServeConfig(cfg); err != nil {
		t.Fatalf("demo without a key: %v", err)
	}
	for name, set := range map[string]func(*config.Config){
		"key":     func(c *config.Config) { c.GarminAPIKey = "k" },
		"expires": func(c *config.Config) { c.GarminKeyExpires = civil.Date{Year: 2027, Month: 1, Day: 1} },
	} {
		c := demoCfg(t)
		set(c)
		if err := validateServeConfig(c); err == nil || !strings.Contains(err.Error(), "COACHD_DEMO=true") {
			t.Errorf("demo with a %s: err = %v", name, err)
		}
	}
	real := testCfg(t)
	real.GarminAPIKey = ""
	if err := validateServeConfig(real); err == nil || !strings.Contains(err.Error(), "COACHD_GARMIN_API_KEY_FILE") {
		t.Errorf("real without a key: err = %v", err)
	}
}
