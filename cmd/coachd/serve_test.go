package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/backup"
	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/config"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/garminkey"
	"github.com/stefiix92/coachd/internal/ingest"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
)

// emptyGarmin returns no data, so ingest stays idle.
type emptyGarmin struct{}

func (emptyGarmin) SearchActivities(context.Context, civil.Date, civil.Date, int, int) ([]garmin.Item[garmin.ActivityDTO], error) {
	return nil, nil
}
func (emptyGarmin) DownloadActivityFIT(context.Context, int64) ([]byte, error) {
	return nil, garmin.ErrNotFound
}
func (emptyGarmin) ActivityExists(context.Context, int64) (bool, error) { return true, nil }
func (emptyGarmin) SleepRange(context.Context, civil.Date, civil.Date) ([]garmin.Item[garmin.SleepDTO], error) {
	return nil, nil
}
func (emptyGarmin) HRVRange(context.Context, civil.Date, civil.Date) ([]garmin.Item[garmin.HRVSummaryDTO], error) {
	return nil, nil
}
func (emptyGarmin) DailySummariesV2(context.Context, civil.Date, civil.Date) ([]garmin.Item[garmin.DailySummaryV2DTO], error) {
	return nil, nil
}
func (emptyGarmin) WeightRange(context.Context, civil.Date, civil.Date) ([]garmin.Item[garmin.WeightDTO], error) {
	return nil, nil
}

func testTel(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	m, err := telemetry.NewMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	return &telemetry.Telemetry{Logger: slog.New(slog.DiscardHandler), Metrics: m,
		Tracer: tracenoop.NewTracerProvider().Tracer(""), Shutdown: func(context.Context) error { return nil }}
}

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	tz, err := time.LoadLocation("Europe/Bratislava")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		DataDir: t.TempDir(), TZ: tz,
		PublicAddr: "127.0.0.1:0", PrivateAddr: "127.0.0.1:0", OpsAddr: "127.0.0.1:0",
		GarminAPIKey: "k", GarminKeyExpires: civil.Of(time.Now(), tz).AddDays(60),
		PollInterval: time.Hour, SyncMinInterval: 10 * time.Minute, BackfillDays: 30,
	}
}

// makeToken creates a token in the store at cfg.DataDir before serve starts.
func makeToken(t *testing.T, cfg *config.Config, scope string) string {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, errb bytes.Buffer
	if code := runToken(context.Background(), []string{"create", "--name", "t", "--scope", scope}, db, time.Now, &out, &errb); code != 0 {
		t.Fatalf("token create: %d %s", code, errb.String())
	}
	return strings.TrimSpace(out.String())
}

type running struct {
	addrs  Addrs
	cancel context.CancelFunc
	done   chan error
	exited bool
}

func startServe(t *testing.T, cfg *config.Config) *running {
	t.Helper()
	return startServeWith(t, cfg, testTel(t))
}

// startServeWith is startServe with the caller's telemetry (to read logs).
func startServeWith(t *testing.T, cfg *config.Config, tel *telemetry.Telemetry) *running {
	t.Helper()
	return startServeGarmin(t, cfg, tel, emptyGarmin{}, testKeys(t, cfg))
}

// startServeGarmin is startServeWith with the caller's Garmin and key holder.
func startServeGarmin(t *testing.T, cfg *config.Config, tel *telemetry.Telemetry, g ingest.Garmin, keys *garminkey.Holder) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan Addrs, 1)
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() {
		r.done <- runServe(ctx, cfg, g, keys, tel, func(a Addrs) { ready <- a })
	}()
	select {
	case r.addrs = <-ready:
	case err := <-r.done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve not ready")
	}
	t.Cleanup(func() {
		cancel()
		if !r.exited {
			select {
			case <-r.done:
			case <-time.After(15 * time.Second):
			}
		}
	})
	return r
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.exited = true
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within 10 s")
		return nil
	}
}

type headerTransport map[string]string

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func getStatus(t *testing.T, endpoint string, hdr map[string]string) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: headerTransport(hdr)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("get_status: %+v", res)
	}
}

func post(t *testing.T, url string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestPrivateLaneRejectsWithoutToken(t *testing.T) {
	r := startServe(t, testCfg(t))
	if code := post(t, "http://"+r.addrs.Private+"/mcp", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code := post(t, "http://"+r.addrs.Private+"/mcp", map[string]string{"Authorization": "Bearer coachd_nope"}); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
}

func TestPrivateLaneAcceptsToken(t *testing.T) {
	cfg := testCfg(t)
	tok := makeToken(t, cfg, "read")
	r := startServe(t, cfg)
	getStatus(t, "http://"+r.addrs.Private+"/mcp", map[string]string{"Authorization": "Bearer " + tok})
	if code := post(t, "http://"+r.addrs.Private+"/other", map[string]string{"Authorization": "Bearer " + tok}); code != 404 {
		t.Fatalf("non-/mcp path: %d", code)
	}
}

func TestPublicListenerDisabledWithoutAccess(t *testing.T) {
	r := startServe(t, testCfg(t))
	if r.addrs.Public != "" {
		t.Fatalf("public bound: %q", r.addrs.Public)
	}
}

func TestOpsEndpoints(t *testing.T) {
	r := startServe(t, testCfg(t))
	base := "http://" + r.addrs.Ops
	for path, want := range map[string]int{"/livez": 200, "/readyz": 200, "/debug/pprof/": 200, "/mcp": 404} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d want %d", path, resp.StatusCode, want)
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	r := startServe(t, testCfg(t))
	start := time.Now()
	if err := r.stop(t); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if _, err := http.Get("http://" + r.addrs.Ops + "/livez"); err == nil {
		t.Fatal("ops still serving")
	}
}

// accessFixture is a fake Cloudflare Access JWKS plus a JWT minter.
func accessFixture(t *testing.T) (teamDomain string, mint func() string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cdn-cgi/access/certs", func(w http.ResponseWriter, r *http.Request) {
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": n, "e": e}}})
	})
	jwks := httptest.NewServer(mux)
	t.Cleanup(jwks.Close)
	mint = func() string {
		enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
		signing := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(map[string]any{
			"iss": jwks.URL, "aud": []string{"aud-1"}, "email": "me@example.com", "sub": "u1",
			"iat": time.Now().Add(-time.Hour).Unix(), "exp": time.Now().Add(time.Hour).Unix()})
		sum := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	return jwks.URL, mint
}

func accessCfg(t *testing.T) (*config.Config, func() string) {
	t.Helper()
	td, mint := accessFixture(t)
	cfg := testCfg(t)
	cfg.AccessTeamDomain, cfg.AccessAUD, cfg.AccessAllowedEmail = td, "aud-1", "me@example.com"
	cfg.PublicURL = "https://coachd.example.com"
	return cfg, mint
}

func TestPublicLaneWithAccess(t *testing.T) {
	cfg, mint := accessCfg(t)
	r := startServe(t, cfg)
	if r.addrs.Public == "" {
		t.Fatal("public not bound")
	}
	url := "http://" + r.addrs.Public + "/mcp"
	getStatus(t, url, map[string]string{"Cf-Access-Jwt-Assertion": mint()})
	if code := post(t, url, nil); code != 401 {
		t.Fatalf("missing header: %d", code)
	}
}

func TestServePublicLaneServesWeb(t *testing.T) {
	cfg, mint := accessCfg(t)
	tok := makeToken(t, cfg, "read")
	r := startServe(t, cfg)
	req, _ := http.NewRequest("GET", "http://"+r.addrs.Public+"/", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", mint())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("public /: %d %q csp=%q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Security-Policy"))
	}
	if resp, err = http.Get("http://" + r.addrs.Public + "/"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("public / without Access: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "nonce.key")); err != nil {
		t.Fatalf("nonce.key: %v", err)
	}
	if resp, err = http.Get("http://" + r.addrs.Private + "/"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("private /: %d", resp.StatusCode)
	}
	getStatus(t, "http://"+r.addrs.Public+"/mcp", map[string]string{"Cf-Access-Jwt-Assertion": mint()})
	getStatus(t, "http://"+r.addrs.Private+"/mcp", map[string]string{"Authorization": "Bearer " + tok})
}

// Managed OAuth agents share the runner's Access subject, so the page has
// its own rate limiter: agent traffic that exhausts /mcp's leaves / serving.
func TestWebLimiterSeparateFromMCP(t *testing.T) {
	cfg, mint := accessCfg(t)
	r := startServe(t, cfg)
	hdr := map[string]string{"Cf-Access-Jwt-Assertion": mint()}
	limited := false
	for i := 0; i < 200 && !limited; i++ {
		limited = post(t, "http://"+r.addrs.Public+"/mcp", hdr) == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("the /mcp limiter never answered 429")
	}
	req, _ := http.NewRequest("GET", "http://"+r.addrs.Public+"/", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", hdr["Cf-Access-Jwt-Assertion"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / after /mcp was limited: %d", resp.StatusCode)
	}
}

func TestValidateServeConfig(t *testing.T) {
	cfg := testCfg(t)
	cfg.GarminAPIKey = ""
	if err := validateServeConfig(cfg); err == nil || !strings.Contains(err.Error(), "COACHD_GARMIN_API_KEY_FILE") {
		t.Fatalf("key: %v", err)
	}
	cfg = testCfg(t)
	cfg.GarminKeyExpires = civil.Date{}
	if err := validateServeConfig(cfg); err == nil || !strings.Contains(err.Error(), "COACHD_GARMIN_KEY_EXPIRES") {
		t.Fatalf("expiry: %v", err)
	}
	if err := validateServeConfig(testCfg(t)); err != nil {
		t.Fatal(err)
	}
}

func TestLaneIgnoresClientHostInMetrics(t *testing.T) {
	for _, l := range []auth.Lane{auth.LanePublic, auth.LanePrivate} {
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		srv := httptest.NewServer(laneMux(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), nil, mp))
		for _, host := range []string{"evil-1.example", "evil-2.example"} {
			req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader("{}"))
			req.Host = host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		srv.Close()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "http.server.request.duration" {
					continue
				}
				h := m.Data.(metricdata.Histogram[float64])
				for _, dp := range h.DataPoints {
					seen++
					v, ok := dp.Attributes.Value("server.address")
					if want := "coachd-" + string(l); ok && v.AsString() != want {
						t.Errorf("%s: server.address %q want %q", l, v.AsString(), want)
					}
				}
			}
		}
		if seen == 0 {
			t.Fatalf("%s: no request duration points", l)
		}
		if seen != 1 {
			t.Errorf("%s: %d series for 2 hosts", l, seen)
		}
	}
}

func httpDo(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServeDeviceAPIWithoutAccess(t *testing.T) {
	cfg := testCfg(t)
	cfg.DeviceAPI = true
	r := startServe(t, cfg)
	if r.addrs.Public == "" {
		t.Fatal("public listener not bound")
	}
	pub, priv := "http://"+r.addrs.Public, "http://"+r.addrs.Private
	if code, body := httpDo(t, "POST", pub+"/device/v1/pairings", "", nil); code != 201 || !strings.Contains(body, "pairing_id") {
		t.Fatalf("pairing: %d %s", code, body)
	}
	for _, p := range []string{"/mcp", "/"} {
		if code, _ := httpDo(t, "GET", pub+p, "", nil); code != 404 {
			t.Errorf("public %s: %d, want 404", p, code)
		}
	}
	if code, _ := httpDo(t, "POST", priv+"/device/v1/pairings", "", nil); code != 404 {
		t.Errorf("private lane device path: %d, want 404", code)
	}
}

func TestServeDeviceAPIWithAccess(t *testing.T) {
	cfg, mint := accessCfg(t)
	cfg.DeviceAPI = true
	r := startServe(t, cfg)
	pub := "http://" + r.addrs.Public
	code, body := httpDo(t, "GET", pub+"/device/v1/workouts", "", nil)
	if code != 401 || !strings.Contains(body, "device not paired") {
		t.Fatalf("device path without Access JWT: %d %s", code, body)
	}
	if code, _ := httpDo(t, "GET", pub+"/", "", nil); code != 401 {
		t.Errorf("/ without Access: %d, want 401", code)
	}
	if code, _ := httpDo(t, "GET", pub+"/", "", map[string]string{"Cf-Access-Jwt-Assertion": mint()}); code != 200 {
		t.Errorf("/ with Access: %d, want 200", code)
	}
	getStatus(t, pub+"/mcp", map[string]string{"Cf-Access-Jwt-Assertion": mint()})
}

func TestServeDeviceAPIOff(t *testing.T) {
	r := startServe(t, testCfg(t))
	if r.addrs.Public != "" {
		t.Fatalf("public bound with device API off and no Access: %q", r.addrs.Public)
	}
	cfg, _ := accessCfg(t)
	r = startServe(t, cfg)
	if code, _ := httpDo(t, "POST", "http://"+r.addrs.Public+"/device/v1/pairings", "", nil); code != 401 {
		t.Errorf("device path with API off behind Access: %d, want 401 (Access)", code)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The pairing id is the claim secret and travels in the URL path: no log
// line or audit row may carry it, and url.path is not an allowed telemetry key.
func TestDeviceAPIPairingIDNotLeaked(t *testing.T) {
	for _, k := range telemetry.AllowedKeys {
		if strings.Contains(k, "path") || k == "http.target" || k == "url.full" {
			t.Fatalf("path-like key allowed in telemetry: %q", k)
		}
	}
	cfg := testCfg(t)
	cfg.DeviceAPI = true
	logs := &lockedBuf{}
	tel := testTel(t)
	tel.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan Addrs, 1)
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, emptyGarmin{}, testKeys(t, cfg), tel, func(a Addrs) { ready <- a }) }()
	var addrs Addrs
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve not ready")
	}
	_, body := httpDo(t, "POST", "http://"+addrs.Public+"/device/v1/pairings", "", nil)
	var pc struct {
		PairingID string `json:"pairing_id"`
	}
	if err := json.Unmarshal([]byte(body), &pc); err != nil || len(pc.PairingID) != 32 {
		t.Fatalf("pairing body %q: %v", body, err)
	}
	unknown := strings.Repeat("cd", 16)
	for _, id := range []string{pc.PairingID, unknown} {
		if code, _ := httpDo(t, "GET", "http://"+addrs.Public+"/device/v1/pairings/"+id, "", nil); code != 200 {
			t.Fatalf("poll: %d", code)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pc.PairingID, unknown} {
		if strings.Contains(logs.String(), id) {
			t.Errorf("pairing id %s in logs", id)
		}
		for _, e := range rows {
			if strings.Contains(fmt.Sprintf("%+v", e), id) {
				t.Errorf("pairing id %s in audit row %+v", id, e)
			}
		}
	}
	if strings.Contains(logs.String(), "/device/v1/pairings/") {
		t.Error("raw device path in logs")
	}
}

func TestServeRefusesLockedDataDir(t *testing.T) {
	cfg := testCfg(t)
	release, err := backup.LockData(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = runServe(context.Background(), cfg, emptyGarmin{}, testKeys(t, cfg), testTel(t), nil)
	if err == nil || !strings.Contains(err.Error(), "in use by another coachd process") {
		t.Fatalf("err %v", err)
	}
}

func TestServeWritesStartupBackup(t *testing.T) {
	cfg := testCfg(t)
	startServe(t, cfg)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, _ := filepath.Glob(filepath.Join(cfg.DataDir, "backups", "coachd-*.json")); len(m) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backup within 5 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBackupCheckSkipsWhenFresh(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := backup.Run(context.Background(), db, dir, "test", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	backupCheck(context.Background(), db, dir, "test", now, nil, slog.New(slog.DiscardHandler))
	if m, _ := filepath.Glob(filepath.Join(dir, "backups", "coachd-*.json")); len(m) != 1 {
		t.Fatalf("manifests %v", m)
	}
}

func TestBackupCheckRunsWhenNewestIsFutureDated(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := backup.Run(context.Background(), db, dir, "test", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	backupCheck(context.Background(), db, dir, "test", now, nil, slog.New(slog.DiscardHandler))
	if m, _ := filepath.Glob(filepath.Join(dir, "backups", "coachd-*.json")); len(m) != 2 {
		t.Fatalf("manifests %v, want a new backup beside the future-dated one", m)
	}
}

// backupCheckOnce runs backupCheck at now, with real metrics, and returns
// coachd.backup.runs by outcome.
func backupCheckOnce(t *testing.T, db *store.DB, dir string, now time.Time) map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	backupCheck(context.Background(), db, dir, "test", now, m, slog.New(slog.DiscardHandler))
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "coachd.backup.runs" {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("outcome")
				got[v.AsString()] += dp.Value
			}
		}
	}
	return got
}

func openBackupTestDB(t *testing.T) (string, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return dir, db
}

func TestBackupCheckCountsOutcomes(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	manifests := func(dir string) int {
		m, _ := filepath.Glob(filepath.Join(dir, "backups", "coachd-*.json"))
		return len(m)
	}

	t.Run("ok when the newest is stale", func(t *testing.T) {
		dir, db := openBackupTestDB(t)
		if _, err := backup.Run(context.Background(), db, dir, "test", now.Add(-backup.Interval-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := backupCheckOnce(t, db, dir, now); len(got) != 1 || got["ok"] != 1 {
			t.Fatalf("outcomes %v, want ok=1", got)
		}
		if n := manifests(dir); n != 2 {
			t.Fatalf("%d manifests, want the stale one and a new one", n)
		}
	})

	t.Run("busy while the backup lock is held", func(t *testing.T) {
		dir, db := openBackupTestDB(t)
		holdBackupLock(t, dir)
		if got := backupCheckOnce(t, db, dir, now); len(got) != 1 || got["busy"] != 1 {
			t.Fatalf("outcomes %v, want busy=1", got)
		}
		if n := manifests(dir); n != 0 {
			t.Fatalf("%d manifests, want none", n)
		}
	})

	t.Run("failed on an unreadable FIT file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode 0000 files")
		}
		dir, db := openBackupTestDB(t)
		fit := filepath.Join(dir, "fit", "2026", "1.fit")
		if err := os.MkdirAll(filepath.Dir(fit), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fit, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		if got := backupCheckOnce(t, db, dir, now); len(got) != 1 || got["failed"] != 1 {
			t.Fatalf("outcomes %v, want failed=1", got)
		}
		if n := manifests(dir); n != 0 {
			t.Fatalf("%d manifests, want none", n)
		}
	})
}

// builtinCfg is testCfg in builtin mode with a development public URL.
func builtinCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testCfg(t)
	cfg.PublicAuth, cfg.PublicURL = config.PublicAuthBuiltin, "http://localhost"
	return cfg
}

// noRedirect returns redirects to the caller instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func doReq(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func isJSON(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
}

func TestServeBuiltinMounts(t *testing.T) {
	cfg := builtinCfg(t)
	cfg.DeviceAPI = true
	tok := makeToken(t, cfg, "read")
	if !strings.HasPrefix(tok, "cdt_") {
		t.Fatalf("private token %q", tok)
	}
	r := startServe(t, cfg)
	if r.addrs.Public == "" {
		t.Fatal("public listener not bound")
	}
	pub := "http://" + r.addrs.Public
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	const challenge = `Bearer resource_metadata="http://localhost/.well-known/oauth-protected-resource/mcp", scope="coach:read coach:write"`

	resp, body := doReq(t, "GET", pub+"/.well-known/oauth-authorization-server", "", nil)
	if resp.StatusCode != 200 || !isJSON(resp) || !strings.Contains(body, `"issuer":"http://localhost"`) {
		t.Errorf("AS metadata: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	resp, body = doReq(t, "GET", pub+"/.well-known/oauth-protected-resource/mcp", "", nil)
	if resp.StatusCode != 200 || !isJSON(resp) || !strings.Contains(body, `"resource":"http://localhost/mcp"`) {
		t.Errorf("PRM: %d %s", resp.StatusCode, body)
	}

	resp, _ = doReq(t, "POST", pub+"/mcp", `{}`, jsonHdr)
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != challenge {
		t.Errorf("POST /mcp without token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	resp, _ = doReq(t, "GET", pub+"/", "", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2F" {
		t.Errorf("GET /: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Discovery probes and legacy endpoints get a JSON 404, never the login
	// page; DCR is off, so /oauth/register is one too.
	for _, p := range []string{"/.well-known/openid-configuration", "/.well-known/", "/authorize", "/token", "/register", "/oauth/register"} {
		resp, body = doReq(t, "GET", pub+p, "", nil)
		if resp.StatusCode != 404 || !isJSON(resp) || !strings.Contains(body, `"not_found"`) {
			t.Errorf("GET %s: %d %q %s", p, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	for _, p := range []string{"/oauth/token", "/oauth/revoke"} {
		resp, body = doReq(t, "POST", pub+p, "", nil)
		if resp.StatusCode != 400 || !isJSON(resp) || !strings.Contains(body, `"invalid_request"`) {
			t.Errorf("POST %s: %d %s", p, resp.StatusCode, body)
		}
	}

	// The public /mcp never accepts a private-lane token (M9).
	resp, _ = doReq(t, "POST", pub+"/mcp", `{}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + tok})
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != challenge+`, error="invalid_token"` {
		t.Errorf("POST /mcp with a cdt_ token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// The consent routes belong to the page: its CSP, not the OAuth JSON.
	resp, _ = doReq(t, "GET", pub+"/oauth/authorize", "", nil)
	if isJSON(resp) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("GET /oauth/authorize: %d %q csp=%q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Security-Policy"))
	}
	if resp, _ = doReq(t, "POST", pub+"/apps/1/revoke", "", nil); resp.StatusCode != 401 {
		t.Errorf("POST /apps/1/revoke without a session: %d", resp.StatusCode)
	}

	if resp, body = doReq(t, "POST", pub+"/device/v1/pairings", "", nil); resp.StatusCode != 201 || !strings.Contains(body, "pairing_id") {
		t.Errorf("device pairing: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "nonce.key")); err != nil {
		t.Errorf("nonce.key: %v", err)
	}

	// The private lane is unchanged and serves no OAuth endpoint.
	priv := "http://" + r.addrs.Private
	getStatus(t, priv+"/mcp", map[string]string{"Authorization": "Bearer " + tok})
	if code, _ := httpDo(t, "GET", priv+"/.well-known/oauth-authorization-server", "", nil); code != 404 {
		t.Errorf("private metadata: %d", code)
	}

	// A request without credentials is never audited; a bad token is
	// (sampled, well under the hourly cap).
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for _, e := range rows {
		if e.Event == "auth_failure" && e.Lane == "public" {
			outcomes[e.Outcome]++
		}
	}
	if outcomes["missing"] != 0 || outcomes["invalid"] != 1 {
		t.Errorf("public auth_failure rows by outcome %v, want invalid=1 and no missing", outcomes)
	}
}

// A plain-http public URL is for development only: serve says so at start.
func TestServeBuiltinDevWarning(t *testing.T) {
	const msg = "public URL is plain http: development only"
	for _, tc := range []struct {
		url  string
		warn bool
	}{{"http://localhost", true}, {"http://localhost:8080", true}, {"https://coachd.example.com", false}} {
		cfg := builtinCfg(t)
		cfg.PublicURL = tc.url
		logs := &lockedBuf{}
		tel := testTel(t)
		tel.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		r := startServeWith(t, cfg, tel)
		if err := r.stop(t); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			if rec["msg"] != msg {
				continue
			}
			found = true
			if rec["level"] != "WARN" {
				t.Errorf("%s: level %v", tc.url, rec["level"])
			}
			for k := range rec {
				if k != "time" && k != "level" && k != "msg" && !slices.Contains(telemetry.AllowedKeys, k) {
					t.Errorf("%s: log key %q is not allowlisted", tc.url, k)
				}
			}
		}
		if found != tc.warn {
			t.Errorf("%s: warning logged %v, want %v", tc.url, found, tc.warn)
		}
	}
}

// seedOAuth stores, at `at`, a passkey, a client and two grants: one whose
// code was redeemed (access token hash "access") and one whose code
// ("code-unused") was not. It returns those two hashes.
func seedOAuth(t *testing.T, cfg *config.Config, at time.Time) (access, unusedCode []byte) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	enr, err := db.CreateEnrollment(ctx, []byte("enrol"), at, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := db.EnrollPasskey(ctx, store.EnrollParams{EnrollmentID: enr, Nonce: "n-enrol", NonceExpires: at.Add(time.Hour),
		Passkey: store.Passkey{CredentialID: []byte("cred"), CredentialJSON: []byte(`{}`), Label: "phone"}, At: at, Lane: "public"})
	if err != nil {
		t.Fatal(err)
	}
	cid, err := db.PutOAuthClient(ctx, store.OAuthClient{ClientID: "https://app.example/client.json", Kind: "cimd", Name: "App",
		RedirectURIs: []string{"http://localhost/callback"}, Metadata: []byte(`{}`), FetchedAt: at, CacheUntil: at.Add(time.Hour), CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"code-used", "code-unused"} {
		if _, err := db.GrantConsent(ctx, store.ConsentGrant{Client: store.OAuthClient{ID: cid}, PasskeyID: pk, CredentialJSON: []byte(`{}`),
			Nonce: "n-" + code, NonceExpires: at.Add(10 * time.Minute), Scopes: []string{"coach:read"}, Resource: "http://localhost/mcp",
			CodeHash: []byte(code), RedirectURI: "http://localhost/callback", RedirectExplicit: true, CodeChallenge: strings.Repeat("c", 43),
			CodeExpires: at.Add(5 * time.Minute), At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RedeemCode(ctx, []byte("code-used"), at, func(store.OAuthCode) error { return nil }, store.NewTokens{
		AccessHash: []byte("access"), RefreshHash: []byte("refresh"),
		AccessExpires: at.Add(time.Hour), RefreshExpires: at.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return []byte("access"), []byte("code-unused")
}

// Outside builtin mode serve deletes every OAuth grant, code and token at
// start (spec §3, M13), so switching modes never revives a grant; builtin
// mode keeps them.
func TestServeAccessModeDeletesOAuthState(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(*testing.T) *config.Config
		kept bool
	}{
		{"access", func(t *testing.T) *config.Config { c, _ := accessCfg(t); return c }, false},
		{"public lane off", testCfg, false},
		{"builtin", builtinCfg, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)
			access, code := seedOAuth(t, cfg, time.Now())
			logs := &lockedBuf{}
			tel := testTel(t)
			tel.Logger = slog.New(slog.NewTextHandler(logs, nil))
			r := startServeWith(t, cfg, tel)
			if err := r.stop(t); err != nil {
				t.Fatal(err)
			}
			ctx, now := context.Background(), time.Now()
			db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "coachd.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, _, grantErr := db.AccessGrant(ctx, access, now)
			apps, err := db.ConnectedApps(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			_, codeErr := db.RedeemCode(ctx, code, now, func(store.OAuthCode) error { return nil }, store.NewTokens{
				AccessHash: []byte("access-2"), RefreshHash: []byte("refresh-2"), AccessExpires: now.Add(time.Hour), RefreshExpires: now.Add(time.Hour)})
			deleted := strings.Contains(logs.String(), `msg="oauth state deleted" component=serve count=6`)
			if tc.kept {
				if grantErr != nil || len(apps) != 1 || codeErr != nil || deleted {
					t.Fatalf("builtin: grant %v, apps %d, code %v, deleted logged %v", grantErr, len(apps), codeErr, deleted)
				}
				return
			}
			if !errors.Is(grantErr, store.ErrNotFound) || len(apps) != 0 || !errors.Is(codeErr, store.ErrNotFound) {
				t.Fatalf("grant %v, apps %d, code %v: OAuth state survived", grantErr, len(apps), codeErr)
			}
			if !deleted {
				t.Errorf("no deletion log with the count: %s", logs)
			}
		})
	}
}

// Access mode serves exactly what it did: Access answers every path first,
// the OAuth endpoints are not mounted (the page's HTML 404 behind Access),
// and the public /mcp accepts only the Access identity.
func TestServeAccessModeUnchanged(t *testing.T) {
	cfg, mint := accessCfg(t)
	tok := makeToken(t, cfg, "read")
	r := startServe(t, cfg)
	pub := "http://" + r.addrs.Public
	jwt := map[string]string{"Cf-Access-Jwt-Assertion": mint()}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration",
		"/oauth/authorize", "/authorize", "/apps"} {
		if resp, _ := doReq(t, "GET", pub+p, "", nil); resp.StatusCode != 401 {
			t.Errorf("GET %s without Access: %d, want 401", p, resp.StatusCode)
		}
		resp, body := doReq(t, "GET", pub+p, "", jwt)
		if resp.StatusCode != 404 || isJSON(resp) || !bytes.Contains([]byte(body), []byte("page not found")) {
			t.Errorf("GET %s with Access: %d %q, want the page's 404", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	for _, p := range []string{"/oauth/token", "/oauth/register", "/oauth/revoke"} {
		if resp, _ := doReq(t, "POST", pub+p, "", nil); resp.StatusCode != 401 {
			t.Errorf("POST %s without Access: %d, want 401", p, resp.StatusCode)
		}
	}
	resp, _ := doReq(t, "POST", pub+"/mcp", `{}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + tok})
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("POST /mcp with a cdt_ token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	getStatus(t, pub+"/mcp", jwt)
}

// housekeep also purges expired OAuth state.
func TestHousekeepingPurgesOAuth(t *testing.T) {
	cfg := testCfg(t)
	access, _ := seedOAuth(t, cfg, time.Now().Add(-40*24*time.Hour))
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := db.AccessGrant(context.Background(), access, time.Now()); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("before: %v, want ErrExpired", err)
	}
	housekeep(context.Background(), db, slog.New(slog.DiscardHandler))
	if _, _, err := db.AccessGrant(context.Background(), access, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after: %v, want ErrNotFound (purged)", err)
	}
}

// testKeys is the key holder serve builds for cfg.
func testKeys(t *testing.T, cfg *config.Config) *garminkey.Holder {
	t.Helper()
	h, err := garminkey.Load(cfg.DataDir, garminkey.Key{Value: cfg.GarminAPIKey, Expires: cfg.GarminKeyExpires})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// acceptAll is a Garmin exchanger that accepts any key.
type acceptAll struct{ used string }

func (a *acceptAll) UseKey(_ context.Context, key string, commit func() error) error {
	if err := commit(); err != nil {
		return err
	}
	a.used = key
	return nil
}

// A renewal on the page moves the expiry the MCP status and the gauge read,
// which hold keys.Expires.
func TestKeyRenewerFollowsRenewal(t *testing.T) {
	dir := t.TempDir()
	first, _ := civil.Parse("2026-11-22")
	keys, err := garminkey.Load(dir, garminkey.Key{Value: "old", Expires: first})
	if err != nil {
		t.Fatal(err)
	}
	expires := keys.Expires // as serve passes it to mcpserver and the gauges
	g := &acceptAll{}
	r := keyRenewer{keys: keys, g: g}
	next, _ := civil.Parse("2027-02-20")
	if err := r.Renew(context.Background(), "new", next); err != nil {
		t.Fatal(err)
	}
	if expires() != next || r.Expires() != next || keys.Value() != "new" || g.used != "new" {
		t.Fatalf("after renewal: expires %v, key %q, client %q", expires(), keys.Value(), g.used)
	}
}
