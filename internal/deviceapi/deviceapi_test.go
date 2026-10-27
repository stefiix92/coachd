package deviceapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // the tests use Europe/Bratislava

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitworkout"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/workout"
)

// t0 is 11:00 on Friday 23 Oct 2026 in Europe/Bratislava.
var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// The trace context stored on every test proposal, as propose_workout stores it.
const (
	testTraceID = "0102030405060708090a0b0c0d0e0f10"
	testSpanID  = "0102030405060708"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type testServer struct {
	srv    *Server
	h      http.Handler
	db     *store.DB
	dbPath string
	tz     *time.Location
	clock  *fakeClock
	reader *sdkmetric.ManualReader
	logs   *lockedBuffer
	spans  *tracetest.SpanRecorder
}

// newTestServer returns the device API over a fresh store, with a fake clock
// at t0. On cleanup it checks that every log line and span attribute carries
// only allowlisted keys.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coachd.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tz, err := time.LoadLocation("Europe/Bratislava")
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: t0}
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	// The record time is dropped: its digits could match a pairing code.
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("deviceapi")
	srv := New(Deps{DB: db, TZ: tz, Now: clk.Now, Log: log, Metrics: m, Tracer: tracer})
	ts := &testServer{srv: srv, h: srv.Handler(), db: db, dbPath: dbPath, tz: tz, clock: clk, reader: reader, logs: logs, spans: spans}
	t.Cleanup(func() {
		checkLogKeys(t, logs.String())
		checkSpanKeys(t, spans)
	})
	return ts
}

// checkSpanKeys fails the test when a span or link attribute key is outside
// telemetry.AllowedKeys.
func checkSpanKeys(t *testing.T, sr *tracetest.SpanRecorder) {
	t.Helper()
	for _, s := range sr.Ended() {
		kvs := slices.Clone(s.Attributes())
		for _, l := range s.Links() {
			kvs = append(kvs, l.Attributes...)
		}
		for _, e := range s.Events() {
			kvs = append(kvs, e.Attributes...)
		}
		for _, kv := range kvs {
			if !slices.Contains(telemetry.AllowedKeys, string(kv.Key)) {
				t.Errorf("span %s: attribute key %q is not allowlisted", s.Name(), kv.Key)
			}
		}
	}
}

// checkLogKeys fails the test when a log line carries a key outside
// telemetry.AllowedKeys (besides slog's own level and msg).
func checkLogKeys(t *testing.T, logs string) {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("log line is not JSON: %v", err)
			continue
		}
		for k := range m {
			if k != "time" && k != "level" && k != "msg" && !slices.Contains(telemetry.AllowedKeys, k) {
				t.Errorf("log key %q is not allowlisted (line %s)", k, line)
			}
		}
	}
}

// rawExec runs SQL on the test database over a second connection, to tamper
// with rows underneath the server.
func (ts *testServer) rawExec(t *testing.T, query string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+ts.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (ts *testServer) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	return rec
}

// do sends a request. A non-empty token is sent as a bearer token; a
// non-empty body is sent as application/json.
func (ts *testServer) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "https://coachd.example.com"+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return ts.serve(req)
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return v
}

// jsonError checks the response is JSON and returns its "error" field.
func jsonError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, body %s", ct, rec.Body)
	}
	return decodeBody[struct {
		Error string `json:"error"`
	}](t, rec).Error
}

type createdBody struct {
	PairingID  string `json:"pairing_id"`
	Code       string `json:"code"`
	ExpiresInS int    `json:"expires_in_s"`
}

type pollBody struct {
	Status string `json:"status"`
	Token  string `json:"token"`
}

func (ts *testServer) createPairing(t *testing.T, body string) createdBody {
	t.Helper()
	rec := ts.do(t, "POST", "/device/v1/pairings", "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pairing: status %d, body %s", rec.Code, rec.Body)
	}
	return decodeBody[createdBody](t, rec)
}

func (ts *testServer) poll(t *testing.T, id string) pollBody {
	t.Helper()
	rec := ts.do(t, "GET", "/device/v1/pairings/"+id, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: status %d, body %s", rec.Code, rec.Body)
	}
	return decodeBody[pollBody](t, rec)
}

func (ts *testServer) confirm(t *testing.T, code string) store.Pairing {
	t.Helper()
	p, err := ts.db.ConfirmPairing(context.Background(), code, "fenix", ts.clock.Now(), "cli", "cli", "cli")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return p
}

// pair runs the whole pairing flow and returns the device token and device.
func (ts *testServer) pair(t *testing.T) (string, store.Device) {
	t.Helper()
	pc := ts.createPairing(t, `{"model":"006-B4313-00"}`)
	ts.confirm(t, pc.Code)
	st := ts.poll(t, pc.PairingID)
	if st.Status != "paired" {
		t.Fatalf("poll after confirm: %+v", st)
	}
	h := sha256.Sum256([]byte(st.Token))
	d, err := ts.db.DeviceByTokenHash(context.Background(), h[:])
	if err != nil {
		t.Fatal(err)
	}
	return st.Token, d
}

// addProposal stores a valid one-step running workout as a proposal in status,
// with a stored trace context.
func (ts *testServer) addProposal(t *testing.T, name string, date civil.Date, status store.ProposalStatus) store.Proposal {
	t.Helper()
	tenMin := 600
	w := workout.Workout{Name: name, Sport: "running", Date: date.String(), Steps: []workout.Step{
		{StepBase: workout.StepBase{Kind: "warmup", Duration: &workout.Duration{Type: "time", TimeS: &tenMin}}}}}
	return ts.addWorkout(t, w, "base building", status)
}

// addWorkout stores w as a proposal in status with rationale, and a stored
// trace context.
func (ts *testServer) addWorkout(t *testing.T, w workout.Workout, rationale string, status store.ProposalStatus) store.Proposal {
	t.Helper()
	date, err := civil.Parse(w.Date)
	if err != nil {
		t.Fatal(err)
	}
	canon, err := workout.CanonicalJSON(w)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	now := ts.clock.Now()
	p := store.Proposal{ID: hex.EncodeToString(id), Date: date, Sport: store.Sport(w.Sport), Name: w.Name,
		ContentJSON: canon, ContentHash: workout.ContentHash(canon), Rationale: rationale,
		WarningsJSON: []byte("[]"), Status: status, StatusAt: now, CreatedAt: now,
		CreatedLane: "private", CreatedBy: "agent", CreatedClient: "claude", TraceID: testTraceID, SpanID: testSpanID}
	if err := ts.db.InsertProposal(context.Background(), p, store.AuditEvent{At: now, Lane: "private", Actor: "agent",
		Client: "claude", Event: "proposal_pending", Target: p.ID, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (ts *testServer) status(t *testing.T, id string) store.ProposalStatus {
	t.Helper()
	p, err := ts.db.GetProposal(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Status
}

func (ts *testServer) withdraw(t *testing.T, id string) {
	t.Helper()
	if _, err := ts.db.TransitionProposal(context.Background(), id, store.Transition{
		From: store.DesiredStatuses, To: store.ProposalWithdrawn, At: ts.clock.Now(),
		Lane: "public", Actor: "runner", Client: "passkey:1"}); err != nil {
		t.Fatal(err)
	}
}

func (ts *testServer) today() civil.Date { return civil.Of(ts.clock.Now(), ts.tz) }

// auditCount counts the audit rows for event and target.
func (ts *testServer) auditCount(t *testing.T, event, target string) int {
	t.Helper()
	all, err := ts.db.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range all {
		if e.Event == event && (target == "" || e.Target == target) {
			n++
		}
	}
	return n
}

// counter returns the named Int64 counter's values by attribute key.
func (ts *testServer) counter(t *testing.T, name string, keys ...string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := ts.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				var parts []string
				for _, k := range keys {
					v, _ := dp.Attributes.Value(attribute.Key(k))
					parts = append(parts, v.AsString())
				}
				out[strings.Join(parts, "/")] += dp.Value
			}
		}
	}
	return out
}

func (ts *testServer) spansNamed(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range ts.spans.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// linksStoredTrace reports whether s links to the test proposals' trace context.
func linksStoredTrace(s sdktrace.ReadOnlySpan) bool {
	for _, l := range s.Links() {
		if l.SpanContext.TraceID().String() == testTraceID && l.SpanContext.SpanID().String() == testSpanID {
			return true
		}
	}
	return false
}

// decodeFIT decodes a FIT file with muktihari/fit and returns its workout message.
func decodeFIT(t *testing.T, b []byte) *mesgdef.Workout {
	t.Helper()
	fit, err := decoder.New(bytes.NewReader(b)).Decode()
	if err != nil {
		t.Fatalf("decode FIT: %v", err)
	}
	for i := range fit.Messages {
		if fit.Messages[i].Num == mesgnum.Workout {
			return mesgdef.NewWorkout(&fit.Messages[i])
		}
	}
	t.Fatal("no workout message")
	return nil
}

var (
	hex32  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	digit6 = regexp.MustCompile(`^[0-9]{6}$`)
	token  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

func TestPairingFlow(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/device/v1/pairings", "", `{"model":"006-B4313-00"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	pc := decodeBody[createdBody](t, rec)
	if !hex32.MatchString(pc.PairingID) || !digit6.MatchString(pc.Code) || pc.ExpiresInS != 600 {
		t.Fatalf("create: %s", rec.Body)
	}
	if st := ts.poll(t, pc.PairingID); st != (pollBody{Status: "pending"}) {
		t.Fatalf("poll before confirm: %+v", st)
	}
	if body := strings.TrimSpace(ts.do(t, "GET", "/device/v1/pairings/"+pc.PairingID, "", "").Body.String()); body != `{"status":"pending"}` {
		t.Fatalf("pending body %s", body)
	}
	ts.confirm(t, pc.Code)
	st := ts.poll(t, pc.PairingID)
	if st.Status != "paired" || !token.MatchString(st.Token) {
		t.Fatalf("poll after confirm: %+v", st)
	}
	if again := ts.poll(t, pc.PairingID); again != (pollBody{Status: "expired"}) {
		t.Fatalf("poll after claim: %+v", again)
	}
	if rec := ts.do(t, "GET", "/device/v1/workouts", st.Token, ""); rec.Code != http.StatusOK {
		t.Fatalf("workouts with the new token: status %d, body %s", rec.Code, rec.Body)
	}
	if ts.auditCount(t, "device_paired", "") != 1 {
		t.Error("device_paired audit row missing")
	}
	devs, err := ts.db.ListDevices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Model != "006-B4313-00" || devs[0].LastSeenAt == nil || !devs[0].LastSeenAt.Equal(t0) {
		t.Errorf("device after the first request: %+v %v", devs, err)
	}

	// Unknown, malformed and expired pairing ids all read as expired. A
	// fresh server: pairing polls allow a burst of 30.
	ts = newTestServer(t)
	unknown := strings.Repeat("ab", 16)
	for _, id := range []string{unknown, "ABCDEF0123456789ABCDEF0123456789", "short", strings.Repeat("a", 33), "x%27%20OR%201%3D1"} {
		if st := ts.poll(t, id); st != (pollBody{Status: "expired"}) {
			t.Errorf("poll %q: %+v", id, st)
		}
	}
	late := ts.createPairing(t, "")
	ts.clock.Add(601 * time.Second)
	if st := ts.poll(t, late.PairingID); st != (pollBody{Status: "expired"}) {
		t.Errorf("poll after expiry: %+v", st)
	}
}

// liftCreateLimit gives pairing creates a large burst, for tests that create
// more pairings at once than the global rate limit allows.
func (ts *testServer) liftCreateLimit() {
	ts.srv.pairCreate = auth.NewLimiter(pairCreatePerMin, 1000)
	ts.h = ts.srv.Handler()
}

func TestPairingCap(t *testing.T) {
	ts := newTestServer(t)
	ts.liftCreateLimit()
	codes := map[string]bool{}
	for i := 0; i < 5; i++ {
		codes[ts.createPairing(t, "").Code] = true
	}
	if len(codes) != 5 {
		t.Errorf("codes not unique among pending pairings: %v", codes)
	}
	rec := ts.do(t, "POST", "/device/v1/pairings", "", "")
	if rec.Code != http.StatusTooManyRequests || jsonError(t, rec) != "too many pending pairings" {
		t.Fatalf("sixth: status %d, body %s", rec.Code, rec.Body)
	}
	// Expired pairings no longer count.
	ts.clock.Add(601 * time.Second)
	ts.createPairing(t, "")
}

func TestPairingModelSanitized(t *testing.T) {
	ts := newTestServer(t)
	ts.liftCreateLimit()
	pc := ts.createPairing(t, `{"model":"fēnix<script>"}`)
	if got := ts.confirm(t, pc.Code).Model; got != "fnixscript" {
		t.Errorf("model = %q, want fnixscript", got)
	}
	long := strings.Repeat("abcdefghij", 4) // 40 characters
	pc = ts.createPairing(t, `{"model":"`+long+`","ignored":true}`)
	if got := ts.confirm(t, pc.Code).Model; got != long[:32] {
		t.Errorf("model = %q, want %q", got, long[:32])
	}
	pc = ts.createPairing(t, `{"model":"Fenix 7S_Pro-1.0"}`)
	if got := ts.confirm(t, pc.Code).Model; got != "Fenix 7S_Pro-1.0" {
		t.Errorf("model = %q", got)
	}
	pc = ts.createPairing(t, "")
	if got := ts.confirm(t, pc.Code).Model; got != "" {
		t.Errorf("model without a body = %q", got)
	}

	for name, body := range map[string]string{"not JSON": `{"model":`, "array": `[]`, "model a number": `{"model":7}`,
		"trailing data": `{} {}`} {
		rec := ts.do(t, "POST", "/device/v1/pairings", "", body)
		if rec.Code != http.StatusBadRequest || jsonError(t, rec) != "malformed request" {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest("POST", "https://coachd.example.com/device/v1/pairings", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	if rec := ts.serve(req); rec.Code != http.StatusBadRequest || jsonError(t, rec) != "malformed request" {
		t.Errorf("text/plain body: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestPairingLogsNoSecrets(t *testing.T) {
	ts := newTestServer(t)
	pc := ts.createPairing(t, `{"model":"006-B4313-00"}`)
	ts.poll(t, pc.PairingID)
	ts.confirm(t, pc.Code)
	st := ts.poll(t, pc.PairingID)
	ts.poll(t, pc.PairingID)
	p := ts.addProposal(t, "Easy run", ts.today(), store.ProposalApproved)
	ts.do(t, "GET", "/device/v1/workouts", st.Token, "")
	ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/fit", st.Token, "")
	ts.do(t, "POST", "/device/v1/workouts/"+p.ID+"/ack", st.Token, "{}")
	ts.do(t, "GET", "/device/v1/workouts", st.Token+"x", "")

	var dump strings.Builder
	dump.WriteString(ts.logs.String())
	for _, s := range ts.spans.Ended() {
		dump.WriteString(s.Name() + " " + s.Status().Description)
		kvs := slices.Clone(s.Attributes())
		for _, l := range s.Links() {
			kvs = append(kvs, l.Attributes...)
		}
		for _, e := range s.Events() {
			dump.WriteString(" " + e.Name)
			kvs = append(kvs, e.Attributes...)
		}
		for _, kv := range kvs {
			dump.WriteString(" " + string(kv.Key) + "=" + kv.Value.String())
		}
	}
	out := dump.String()
	if !strings.Contains(out, "device_id") {
		t.Errorf("expected device_id in the telemetry: %s", out)
	}
	for name, secret := range map[string]string{"pairing id": pc.PairingID, "pairing id prefix": pc.PairingID[:8],
		"code": pc.Code, "token": st.Token, "token prefix": st.Token[:12]} {
		if strings.Contains(out, secret) {
			t.Errorf("telemetry contains the %s", name)
		}
	}
}

func TestWorkoutsRequiresToken(t *testing.T) {
	ts := newTestServer(t)
	unknown := strings.Repeat("A", 43)
	for name, header := range map[string]string{
		"missing":         "",
		"basic":           "Basic " + unknown,
		"no token":        "Bearer",
		"empty token":     "Bearer ",
		"short":           "Bearer abc",
		"bad alphabet":    "Bearer " + strings.Repeat("+", 43),
		"unknown":         "Bearer " + unknown,
		"unknown, scheme": "bearer " + unknown,
	} {
		req := httptest.NewRequest("GET", "https://coachd.example.com/device/v1/workouts", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := ts.serve(req)
		if rec.Code != http.StatusUnauthorized || jsonError(t, rec) != "device not paired" {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q", name, got)
		}
	}
	if got := ts.counter(t, "coachd.auth.failures", "lane", "reason"); got["device/missing"] != 1 || got["device/invalid"] != 7 {
		t.Errorf("auth failures = %v", got)
	}
	tok, _ := ts.pair(t)
	req := httptest.NewRequest("GET", "https://coachd.example.com/device/v1/workouts", nil)
	req.Header.Set("Authorization", "bearer "+tok)
	if rec := ts.serve(req); rec.Code != http.StatusOK {
		t.Errorf("lower-case scheme: status %d", rec.Code)
	}
}

func TestRevokedDeviceRejected(t *testing.T) {
	ts := newTestServer(t)
	tok, dev := ts.pair(t)
	p := ts.addProposal(t, "Easy run", ts.today(), store.ProposalApproved)
	if rec := ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/fit", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("fit before revoke: status %d", rec.Code)
	}
	if err := ts.db.RevokeDevice(context.Background(), dev.ID, ts.clock.Now(), "cli", "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/device/v1/workouts", ""},
		{"GET", "/device/v1/workouts/" + p.ID + "/fit", ""},
		{"POST", "/device/v1/workouts/" + p.ID + "/ack", "{}"},
	} {
		rec := ts.do(t, r.method, r.path, tok, r.body)
		if rec.Code != http.StatusUnauthorized || jsonError(t, rec) != "device not paired" {
			t.Errorf("%s %s: status %d, body %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if s := ts.status(t, p.ID); s != store.ProposalDelivering {
		t.Errorf("status after revoked ack = %s", s)
	}
}

func TestDesiredSet(t *testing.T) {
	ts := newTestServer(t)
	// 00:30 on Saturday 24 Oct in Bratislava, still Friday in UTC.
	ts.clock.Set(time.Date(2026, 10, 23, 22, 30, 0, 0, time.UTC))
	tok, _ := ts.pair(t)
	today := ts.today()
	if today.String() != "2026-10-24" {
		t.Fatalf("today = %s", today)
	}
	rec := ts.do(t, "GET", "/device/v1/workouts", tok, "")
	if body := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || body != `{"today":"2026-10-24","workouts":[]}` {
		t.Fatalf("empty set: status %d, body %s", rec.Code, body)
	}

	long := strings.Repeat("é", 20) // 40 bytes
	ts.addProposal(t, "Yesterday", today.AddDays(-1), store.ProposalApproved)
	a := ts.addProposal(t, long, today, store.ProposalApproved)
	ts.clock.Add(time.Second)
	b := ts.addProposal(t, "Tempo", today.AddDays(3), store.ProposalDeliveryFailed)
	ts.clock.Add(time.Second)
	c := ts.addProposal(t, "Long run", today.AddDays(3), store.ProposalDelivered) // later created_at
	d := ts.addProposal(t, "Hills", today.AddDays(14), store.ProposalDelivering)
	ts.addProposal(t, "Too far", today.AddDays(15), store.ProposalApproved)
	ts.addProposal(t, "Pending", today, store.ProposalPending)
	ts.addProposal(t, "Declined", today, store.ProposalDeclined)
	ts.addProposal(t, "Withdrawn", today, store.ProposalWithdrawn)

	rec = ts.do(t, "GET", "/device/v1/workouts", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	trunc := strings.Repeat("é", 16)
	want := `{"today":"2026-10-24","workouts":[` +
		`{"id":"` + a.ID + `","date":"2026-10-24","name":"` + trunc + `"},` +
		`{"id":"` + b.ID + `","date":"2026-10-27","name":"Tempo"},` +
		`{"id":"` + c.ID + `","date":"2026-10-27","name":"Long run"},` +
		`{"id":"` + d.ID + `","date":"2026-11-07","name":"Hills"}]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("desired set\n got %s\nwant %s", got, want)
	}
	set := decodeBody[struct {
		Workouts []struct{ ID, Name string }
	}](t, rec)
	if set.Workouts[0].Name != fitworkout.WorkoutName(long) || len(set.Workouts[0].Name) != fitworkout.MaxNameBytes {
		t.Errorf("name %q is not the FIT name", set.Workouts[0].Name)
	}

	fit := ts.do(t, "GET", "/device/v1/workouts/"+a.ID+"/fit", tok, "")
	if fit.Code != http.StatusOK {
		t.Fatalf("fit: status %d, body %s", fit.Code, fit.Body)
	}
	if wk := decodeFIT(t, fit.Body.Bytes()); wk.WktName != set.Workouts[0].Name {
		t.Errorf("FIT wkt_name %q, desired name %q", wk.WktName, set.Workouts[0].Name)
	}
}

func TestDesiredExcludesWithdrawn(t *testing.T) {
	ts := newTestServer(t)
	tok, _ := ts.pair(t)
	today := ts.today()
	keep := ts.addProposal(t, "Easy run", today.AddDays(1), store.ProposalApproved)
	gone := ts.addProposal(t, "Intervals", today.AddDays(2), store.ProposalApproved)
	old := ts.addProposal(t, "Old", today.AddDays(2), store.ProposalWithdrawn)
	pending := ts.addProposal(t, "Pending", today.AddDays(2), store.ProposalPending)
	if rec := ts.do(t, "GET", "/device/v1/workouts/"+gone.ID+"/fit", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("fit before withdraw: status %d", rec.Code)
	}
	ts.withdraw(t, gone.ID) // withdrawn after it reached the watch

	set := decodeBody[struct {
		Workouts []struct{ ID string }
	}](t, ts.do(t, "GET", "/device/v1/workouts", tok, ""))
	if len(set.Workouts) != 1 || set.Workouts[0].ID != keep.ID {
		t.Fatalf("desired set = %+v", set.Workouts)
	}
	for name, id := range map[string]string{"withdrawn after fetch": gone.ID, "withdrawn": old.ID, "pending": pending.ID,
		"unknown": strings.Repeat("0", 32)} {
		rec := ts.do(t, "GET", "/device/v1/workouts/"+id+"/fit", tok, "")
		if rec.Code != http.StatusNotFound || jsonError(t, rec) != "not found" {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	if s := ts.status(t, pending.ID); s != store.ProposalPending {
		t.Errorf("pending moved to %s", s)
	}
}

func TestFetchAndAck(t *testing.T) {
	ts := newTestServer(t)
	tok, dev := ts.pair(t)
	devID := strconv.FormatInt(dev.ID, 10)
	p := ts.addProposal(t, "Easy run", ts.today().AddDays(1), store.ProposalApproved)
	q := ts.addProposal(t, "Tempo", ts.today().AddDays(2), store.ProposalApproved)

	rec := ts.do(t, "POST", "/device/v1/workouts/"+p.ID+"/ack", tok, "{}")
	if rec.Code != http.StatusNotFound || jsonError(t, rec) != "not found" {
		t.Fatalf("ack before fetch: status %d, body %s", rec.Code, rec.Body)
	}
	if s := ts.status(t, p.ID); s != store.ProposalApproved {
		t.Fatalf("status after early ack = %s", s)
	}

	rec = ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/fit", tok, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/vnd.ant.fit" {
		t.Fatalf("fit: status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if wk := decodeFIT(t, rec.Body.Bytes()); wk.WktName != "Easy run" {
		t.Errorf("wkt_name = %q", wk.WktName)
	}
	w, err := fitworkout.Encode(mustWorkout(t, p), p.CreatedAt)
	if err != nil || !bytes.Equal(w, rec.Body.Bytes()) {
		t.Errorf("body is not Encode(w, CreatedAt): %v", err)
	}
	if s := ts.status(t, p.ID); s != store.ProposalDelivering {
		t.Fatalf("status after fetch = %s", s)
	}

	ts.clock.Add(time.Minute)
	rec = ts.do(t, "POST", "/device/v1/workouts/"+p.ID+"/ack", tok, "{}")
	if body := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || body != `{"status":"delivered"}` {
		t.Fatalf("ack: status %d, body %s", rec.Code, body)
	}
	if s := ts.status(t, p.ID); s != store.ProposalDelivered {
		t.Fatalf("status after ack = %s", s)
	}
	ts.clock.Add(time.Minute)
	rec = ts.do(t, "POST", "/device/v1/workouts/"+p.ID+"/ack", tok, "{}")
	if body := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || body != `{"status":"delivered"}` {
		t.Fatalf("second ack: status %d, body %s", rec.Code, body)
	}
	if n := ts.auditCount(t, "proposal_delivered", p.ID); n != 1 {
		t.Errorf("proposal_delivered rows = %d", n)
	}

	if rec := ts.do(t, "GET", "/device/v1/workouts/"+q.ID+"/fit", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("fit q: status %d", rec.Code)
	}
	ts.withdraw(t, q.ID)
	rec = ts.do(t, "POST", "/device/v1/workouts/"+q.ID+"/ack", tok, "{}")
	if rec.Code != http.StatusConflict || jsonError(t, rec) != "workout is withdrawn" {
		t.Fatalf("ack withdrawn: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := ts.do(t, "POST", "/device/v1/workouts/not-an-id/ack", tok, "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("ack of a malformed id: status %d", rec.Code)
	}

	if got := ts.counter(t, "coachd.proposals.deliveries", "outcome"); got["watch_fetch"] != 2 || got["watch_ack"] != 1 || len(got) != 2 {
		t.Errorf("deliveries = %v", got)
	}

	// Content changed after approval: the approved bytes are gone, so nothing is served.
	x := ts.addProposal(t, "Easy run", ts.today().AddDays(3), store.ProposalApproved)
	ts.rawExec(t, `UPDATE proposals SET content_json=replace(content_json, 'Easy run', 'Hard run') WHERE id=?`, x.ID)
	rec = ts.do(t, "GET", "/device/v1/workouts/"+x.ID+"/fit", tok, "")
	if rec.Code != http.StatusInternalServerError || jsonError(t, rec) != "internal error" {
		t.Fatalf("tampered fit: status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if s := ts.status(t, x.ID); s != store.ProposalApproved {
		t.Errorf("tampered proposal moved to %s", s)
	}
	if !strings.Contains(ts.logs.String(), `"error_class":"integrity"`) {
		t.Errorf("integrity failure not logged: %s", ts.logs.String())
	}

	fits := ts.spansNamed("device.fit")
	if len(fits) != 3 || spanAttr(fits[2], "outcome") != "integrity" {
		t.Fatalf("device.fit spans = %d", len(fits))
	}
	for _, s := range fits[:2] {
		if spanAttr(s, "device_id") != devID || spanAttr(s, "outcome") != "fetched" || !linksStoredTrace(s) {
			t.Errorf("device.fit span: %v links %v", s.Attributes(), s.Links())
		}
	}
	if spanAttr(fits[0], "proposal_id") != p.ID || spanAttr(fits[1], "proposal_id") != q.ID {
		t.Errorf("device.fit proposal ids: %v %v", fits[0].Attributes(), fits[1].Attributes())
	}
	var acks []string
	for _, s := range ts.spansNamed("device.ack") {
		if spanAttr(s, "device_id") != devID || !linksStoredTrace(s) {
			t.Errorf("device.ack span: %v links %v", s.Attributes(), s.Links())
		}
		acks = append(acks, spanAttr(s, "proposal_id")[:8]+":"+spanAttr(s, "outcome"))
	}
	want := []string{p.ID[:8] + ":not_found", p.ID[:8] + ":delivered", p.ID[:8] + ":already_delivered", q.ID[:8] + ":conflict"}
	if !slices.Equal(acks, want) {
		t.Errorf("device.ack spans = %v, want %v", acks, want)
	}
}

func mustWorkout(t *testing.T, p store.Proposal) workout.Workout {
	t.Helper()
	var w workout.Workout
	if err := json.Unmarshal(p.ContentJSON, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestDeviceRateLimit(t *testing.T) {
	ts := newTestServer(t)
	tok, _ := ts.pair(t)
	for i := 1; i <= 70; i++ {
		if rec := ts.do(t, "GET", "/device/v1/workouts", tok, ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	rec := ts.do(t, "GET", "/device/v1/workouts", tok, "")
	if rec.Code != http.StatusTooManyRequests || jsonError(t, rec) != "too many requests" || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("71st: status %d, Retry-After %q, body %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	// Another device has its own bucket.
	other, _ := ts.pair(t)
	if rec := ts.do(t, "GET", "/device/v1/workouts", other, ""); rec.Code != http.StatusOK {
		t.Errorf("other device: status %d", rec.Code)
	}

	// Pairing create has a global bucket of burst 5 (10/min).
	ps := newTestServer(t)
	for i := 1; i <= 5; i++ {
		if rec := ps.do(t, "POST", "/device/v1/pairings", "", ""); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: status %d, body %s", i, rec.Code, rec.Body)
		}
	}
	rec = ps.do(t, "POST", "/device/v1/pairings", "", "")
	if rec.Code != http.StatusTooManyRequests || jsonError(t, rec) != "too many requests" || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("6th create: status %d, body %s", rec.Code, rec.Body)
	}
	if n := ps.auditCount(t, "device_pairing_started", ""); n != 5 {
		t.Errorf("device_pairing_started rows = %d, want 5 (a refused create reached the store)", n)
	}
	// Polling has its own global bucket of burst 30 (60/min): two watches
	// polling every 3 s fit.
	for i := 1; i <= 30; i++ {
		if rec := ps.do(t, "GET", "/device/v1/pairings/"+strings.Repeat("ab", 16), "", ""); rec.Code != http.StatusOK {
			t.Fatalf("poll %d: status %d", i, rec.Code)
		}
	}
	rec = ps.do(t, "GET", "/device/v1/pairings/"+strings.Repeat("ab", 16), "", "")
	if rec.Code != http.StatusTooManyRequests || jsonError(t, rec) != "too many requests" || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("31st poll: status %d, body %s", rec.Code, rec.Body)
	}
	// Exhausted polls leave creates alone.
	pp := newTestServer(t)
	for i := 1; i <= 31; i++ {
		pp.do(t, "GET", "/device/v1/pairings/"+strings.Repeat("ab", 16), "", "")
	}
	if rec := pp.do(t, "POST", "/device/v1/pairings", "", ""); rec.Code != http.StatusCreated {
		t.Fatalf("create after exhausted polls: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestBodyCap(t *testing.T) {
	ts := newTestServer(t)
	big := `{"model":"` + strings.Repeat("a", 5000) + `"}`
	for _, chunked := range []bool{false, true} {
		req := httptest.NewRequest("POST", "https://coachd.example.com/device/v1/pairings", strings.NewReader(big))
		req.Header.Set("Content-Type", "application/json")
		if chunked {
			req.ContentLength = -1
		}
		rec := ts.serve(req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("chunked=%v: status %d, body %s", chunked, rec.Code, rec.Body)
		}
	}
	if n := ts.auditCount(t, "device_pairing_started", ""); n != 0 {
		t.Errorf("an oversized create stored a pairing: %d rows", n)
	}
	// Just under the cap is accepted.
	ok := `{"model":"x","pad":"` + strings.Repeat("a", 4096-len(`{"model":"x","pad":""}`)) + `"}`
	if len(ok) != 4096 {
		t.Fatalf("len %d", len(ok))
	}
	ts.createPairing(t, ok)

	tok, _ := ts.pair(t)
	p := ts.addProposal(t, "Easy run", ts.today(), store.ProposalApproved)
	ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/fit", tok, "")
	req := httptest.NewRequest("POST", "https://coachd.example.com/device/v1/workouts/"+p.ID+"/ack", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.ContentLength = -1
	if rec := ts.serve(req); rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized ack: status %d", rec.Code)
	}
	if s := ts.status(t, p.ID); s != store.ProposalDelivering {
		t.Errorf("oversized ack moved the status to %s", s)
	}
}

func TestDeviceHeaders(t *testing.T) {
	ts := newTestServer(t)
	tok, _ := ts.pair(t)
	p := ts.addProposal(t, "Easy run", ts.today(), store.ProposalApproved)
	w := ts.addProposal(t, "Old", ts.today(), store.ProposalApproved)

	type exchange struct {
		name string
		rec  *httptest.ResponseRecorder
	}
	head := httptest.NewRequest("HEAD", "https://coachd.example.com/device/v1/workouts", nil)
	head.Header.Set("Authorization", "Bearer "+tok)
	dotted := httptest.NewRequest("GET", "https://coachd.example.com/device/v1/./workouts", nil)
	dotted.Header.Set("Authorization", "Bearer "+tok)
	plain := httptest.NewRequest("POST", "https://coachd.example.com/device/v1/pairings", strings.NewReader("x"))
	plain.Header.Set("Content-Type", "text/plain")
	ts.do(t, "GET", "/device/v1/workouts/"+w.ID+"/fit", tok, "")
	ts.withdraw(t, w.ID)

	cases := []exchange{
		{"create", ts.do(t, "POST", "/device/v1/pairings", "", "")},
		{"poll", ts.do(t, "GET", "/device/v1/pairings/"+strings.Repeat("ab", 16), "", "")},
		{"malformed create", ts.do(t, "POST", "/device/v1/pairings", "", "{")},
		{"wrong content type", ts.serve(plain)},
		{"no token", ts.do(t, "GET", "/device/v1/workouts", "", "")},
		{"workouts", ts.do(t, "GET", "/device/v1/workouts", tok, "")},
		{"fit not in set", ts.do(t, "GET", "/device/v1/workouts/"+strings.Repeat("0", 32)+"/fit", tok, "")},
		{"ack before fetch", ts.do(t, "POST", "/device/v1/workouts/"+p.ID+"/ack", tok, "{}")},
		{"ack conflict", ts.do(t, "POST", "/device/v1/workouts/"+w.ID+"/ack", tok, "{}")},
		{"unknown path", ts.do(t, "GET", "/device/v1/nope", tok, "")},
		{"outside the API", ts.do(t, "GET", "/mcp", "", "")},
		{"wrong method", ts.do(t, "DELETE", "/device/v1/workouts", tok, "")},
		{"GET on ack", ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/ack", tok, "")},
		{"HEAD", ts.serve(head)},
		{"dot segment", ts.serve(dotted)},
	}
	wantStatus := map[string]int{"create": 201, "poll": 200, "malformed create": 400, "wrong content type": 400,
		"no token": 401, "workouts": 200, "fit not in set": 404, "ack before fetch": 404, "ack conflict": 409,
		"unknown path": 404, "outside the API": 404, "wrong method": 404, "GET on ack": 404, "HEAD": 404, "dot segment": 404}
	for _, x := range cases {
		h := x.rec.Header()
		if x.rec.Code != wantStatus[x.name] {
			t.Errorf("%s: status %d, want %d (body %s)", x.name, x.rec.Code, wantStatus[x.name], x.rec.Body)
		}
		if !strings.HasPrefix(h.Get("Content-Type"), "application/json") || h.Get("Cache-Control") != "no-store" ||
			h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", x.name, h)
		}
		var v map[string]any
		if err := json.Unmarshal(x.rec.Body.Bytes(), &v); err != nil {
			t.Errorf("%s: body %q is not a JSON object", x.name, x.rec.Body)
		}
		if x.rec.Code == http.StatusNotFound && strings.TrimSpace(x.rec.Body.String()) != `{"error":"not found"}` {
			t.Errorf("%s: body %s", x.name, x.rec.Body)
		}
	}

	fit := ts.do(t, "GET", "/device/v1/workouts/"+p.ID+"/fit", tok, "")
	h := fit.Header()
	if fit.Code != http.StatusOK || h.Get("Content-Type") != "application/vnd.ant.fit" || h.Get("Cache-Control") != "no-store" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Disposition") != "" ||
		h.Get("Content-Length") != strconv.Itoa(fit.Body.Len()) {
		t.Errorf("fit: status %d, headers %v", fit.Code, h)
	}
}
