package web

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/store"
)

// builtinServer is a consent server in builtin mode: the session gate in
// front of the page, /apps mounted, and no identity on any request. edit,
// when set, changes the dependencies first.
func builtinServer(t *testing.T, edit ...func(*Deps)) *testServer {
	t.Helper()
	ts := consentServer(t)
	d := ts.srv.d
	d.Builtin = true
	for _, e := range edit {
		e(&d)
	}
	srv, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	ts.srv, ts.h, ts.builtin = srv, srv.Handler(), true
	return ts
}

// samplePath turns a route pattern into a request path that it matches.
func samplePath(pattern string) string {
	p := pattern
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		p = rest
	}
	switch p {
	case "/":
		return "/no-such-page"
	case "/static/":
		return "/static/app.js"
	}
	return strings.NewReplacer("{$}", "", "{id}", "1").Replace(p)
}

// allAudit returns every audit row, newest first.
func (ts *testServer) allAudit(t *testing.T) []string {
	t.Helper()
	rows, err := ts.db.ListAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range rows {
		out = append(out, e.Event)
	}
	return out
}

const msgGate = "sign in with your passkey first"

// TestGateRouteWalk sends a request without a cookie to every route in the
// route table, with every method the route answers: each is exempt, or
// redirected to the login page (GET, HEAD), or refused with 401.
func TestGateRouteWalk(t *testing.T) {
	ts := builtinServer(t)
	routes := ts.srv.routeTable()
	var patterns, open []string
	for _, rt := range routes {
		patterns = append(patterns, rt.pattern)
		if rt.open {
			open = append(open, rt.pattern)
		}
	}
	for _, want := range []string{"GET /oauth/authorize", "POST /oauth/authorize/finish", "GET /apps", "POST /apps/{id}/revoke", "/"} {
		if !slices.Contains(patterns, want) {
			t.Fatalf("route table lacks %q: %q", want, patterns)
		}
	}
	wantOpen := []string{"GET /login", "POST /login/begin", "POST /login/finish", "GET /enroll", "POST /enroll/begin",
		"POST /enroll/finish", "GET /static/", "GET /oauth/authorize"}
	slices.Sort(open)
	slices.Sort(wantOpen)
	if !slices.Equal(open, wantOpen) {
		t.Fatalf("exempt routes %q, want %q", open, wantOpen)
	}
	for _, rt := range routes {
		var methods []string
		switch method, _, ok := strings.Cut(rt.pattern, " "); {
		case !ok:
			methods = []string{"GET", "HEAD", "POST", "PUT", "DELETE"}
		case method == "GET":
			methods = []string{"GET", "HEAD"}
		default:
			methods = []string{method}
		}
		path := samplePath(rt.pattern)
		for _, m := range methods {
			var body io.Reader
			if m != "GET" && m != "HEAD" {
				body = strings.NewReader("{}")
			}
			req := httptest.NewRequest(m, testOrigin+path, body)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := ts.serve(req)
			name := rt.pattern + " as " + m + " " + path
			if v := rec.Header().Get("WWW-Authenticate"); v != "" {
				t.Errorf("%s: WWW-Authenticate %q", name, v)
			}
			if rt.open {
				if rec.Code == http.StatusSeeOther || rec.Code == http.StatusUnauthorized {
					t.Errorf("%s (exempt): status %d", name, rec.Code)
				}
				continue
			}
			if m == "GET" || m == "HEAD" {
				want := "/login?next=" + url.QueryEscape(path)
				if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
					t.Errorf("%s: status %d, Location %q, want 303 to %q", name, rec.Code, rec.Header().Get("Location"), want)
				}
				continue
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", name, rec.Code)
				continue
			}
			if got := jsonError(t, rec); got != msgGate {
				t.Errorf("%s: error %q", name, got)
			}
		}
	}
	if rows := ts.allAudit(t); len(rows) != 0 {
		t.Errorf("the walk wrote audit rows: %q", rows)
	}
}

// TestGateAccessModeUnchanged: without Builtin the page is still behind
// publicOnly, so a request without a public-lane identity gets 403, exempt
// routes and /static included.
func TestGateAccessModeUnchanged(t *testing.T) {
	ts := consentServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/login"}, {"POST", "/login/begin"}, {"POST", "/login/finish"}, {"GET", "/enroll"},
		{"GET", "/static/app.js"}, {"GET", authQuery(cClaudeAI, cClaudeAIRed)}, {"GET", "/apps"}, {"GET", "/no-such-page"},
	} {
		for _, id := range []*auth.Identity{nil, {Lane: auth.LanePrivate, Subject: "tok"}} {
			req := httptest.NewRequest(c.method, testOrigin+c.path, nil)
			if id != nil {
				req = req.WithContext(auth.WithIdentity(req.Context(), *id))
			}
			rec := httptest.NewRecorder()
			ts.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s (identity %v): status %d, want 403", c.method, c.path, id, rec.Code)
			}
		}
	}
}

// TestPageRejectsBearer: the page never accepts a bearer token; with one and
// no cookie a request is treated as having no credentials at all.
func TestPageRejectsBearer(t *testing.T) {
	ts := builtinServer(t)
	cookie := ts.enroll(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/audit", http.StatusSeeOther},
		{"GET", "/apps", http.StatusSeeOther},
		{"HEAD", "/", http.StatusSeeOther},
		{"POST", "/notes", http.StatusUnauthorized},
		{"POST", "/logout", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(c.method, testOrigin+c.path, nil)
		req.Header.Set("Authorization", "Bearer cda_x")
		rec := ts.serve(req)
		if rec.Code != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, rec.Code, c.want)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s %s: WWW-Authenticate set", c.method, c.path)
		}
	}
	// The same request with the session cookie passes the gate.
	req := httptest.NewRequest("GET", testOrigin+"/audit", nil)
	req.Header.Set("Authorization", "Bearer cda_x")
	req.AddCookie(cookie)
	if rec := ts.serve(req); rec.Code != http.StatusOK {
		t.Fatalf("with the cookie: status %d", rec.Code)
	}
}

// TestGateSessionPasses: with a session every page works in builtin mode,
// writes included, and audit rows name the passkey.
func TestGateSessionPasses(t *testing.T) {
	ts := builtinServer(t)
	cookie := ts.enroll(t)
	for _, p := range []string{"/", "/notes", "/passkeys", "/devices", "/audit", "/apps"} {
		if rec := ts.do(t, "GET", p, nil, cookie); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", p, rec.Code)
		}
	}
	rec := ts.do(t, "POST", "/notes", url.Values{"category": {"other"}, "text": {"hello"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /notes: status %d, body %s", rec.Code, rec.Body)
	}
	rows := ts.audit(t, "note_created")
	if len(rows) != 1 || rows[0].Actor != "runner" || rows[0].Lane != "public" || !strings.HasPrefix(rows[0].Client, "passkey:") {
		t.Fatalf("note_created rows %+v", rows)
	}
}

func TestSafeNext(t *testing.T) {
	long := "/" + strings.Repeat("a", 8<<10) // 8 KiB + 1 bytes
	for _, c := range []struct{ in, want string }{
		{"/p/abc", "/p/abc"},
		{"/oauth/authorize?client_id=x&state=y", "/oauth/authorize?client_id=x&state=y"},
		{"/", "/"},
		{"/notes#top", "/notes"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"/%09/evil", "/"},
		{"/\t/evil", "/"},
		{"/%2F/evil.example", "/"},
		{"/%5Cevil.example", "/"},
		{"https:evil", "/"},
		{"https://evil/", "/"},
		{"javascript:alert(1)", "/"},
		{"", "/"},
		{"p/abc", "/"},
		{"?x=1", "/"},
		{"/a b", "/"},
		{long, "/"},
		{long[:8<<10], long[:8<<10]},
	} {
		if got := safeNext(c.in); got != c.want {
			name := c.in
			if len(name) > 40 {
				name = name[:40] + "..."
			}
			t.Errorf("safeNext(%q) = %q, want %q", name, got, c.want)
		}
	}
}

func FuzzSafeNext(f *testing.F) {
	for _, s := range []string{"/p/abc", "/oauth/authorize?client_id=x&state=y", "//evil.example", "/\\evil.example",
		"/%09/evil", "/\t/evil", "https:evil", "https://evil/", "javascript:alert(1)", "", "/%2F/evil", "/%5Cevil",
		"/a?b=%00", "/a#frag", "?x", "/./", "/..//evil", "/%2f%2fevil", "/été?x=ü", "/a?b c", "/%zz", "\x7f/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := safeNext(raw)
		if !strings.HasPrefix(got, "/") || strings.HasPrefix(got, "//") || strings.HasPrefix(got, "/\\") {
			t.Fatalf("safeNext(%q) = %q", raw, got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] <= 0x20 {
				t.Fatalf("safeNext(%q) = %q has byte %#x", raw, got, got[i])
			}
		}
		if len(got) > maxNext {
			t.Fatalf("safeNext(%q) is %d bytes", raw, len(got))
		}
		u, err := url.Parse(got)
		if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
			t.Fatalf("safeNext(%q) = %q leaves the origin: %+v %v", raw, got, u, err)
		}
		if again := safeNext(got); again != got {
			t.Fatalf("safeNext is not idempotent: %q -> %q -> %q", raw, got, again)
		}
	})
}

// dataNext returns the login button's data-next attribute, unescaped.
func dataNext(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `data-next="`)
	if !ok {
		t.Fatalf("no data-next in %s", body)
	}
	v, _, _ := strings.Cut(rest, `"`)
	return html.UnescapeString(v)
}

// TestNextKeepsLongAuthorizeURL: a DCR client's authorize URL of over
// 3 KiB, with a 1 KiB state, survives the trip through the login page.
func TestNextKeepsLongAuthorizeURL(t *testing.T) {
	ts := builtinServer(t)
	ts.enroll(t) // the login page shows its button only with a passkey
	redirect := "https://dcr.example/cb/" + strings.Repeat("a", 480)
	reg, err := json.Marshal(map[string]any{"redirect_uris": []string{redirect, redirect + "b", redirect + "c"}, "client_name": "Long"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", cSelf+"/oauth/register", strings.NewReader(string(reg)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	ts.srv.d.OAuth.Handler().ServeHTTP(rr, req)
	var r struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &r); err != nil || r.ClientID == "" {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}
	q := url.Values{"client_id": {r.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {cChallenge}, "code_challenge_method": {"S256"}, "state": {strings.Repeat("s", 1<<10)},
		"scope": {"coach:read coach:write"}}
	path := "/oauth/authorize?" + q.Encode()
	if len(path) < 3<<10 {
		t.Fatalf("authorize URL is only %d bytes", len(path))
	}
	rec := ts.do(t, "GET", path, nil, nil)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("authorize: status %d, Location %q", rec.Code, loc)
	}
	rec = ts.do(t, "GET", loc, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page: status %d", rec.Code)
	}
	if got := dataNext(t, rec.Body.String()); got != path {
		t.Fatalf("data-next differs (%d bytes, want %d)", len(got), len(path))
	}
}

// TestLoginPageNext: the login page carries the validated next, and "/"
// when next is absent or rejected; a gated page round-trips through it.
func TestLoginPageNext(t *testing.T) {
	ts := builtinServer(t)
	ts.enroll(t)
	for _, c := range []struct{ query, want string }{
		{"", "/"},
		{"?next=" + url.QueryEscape("//evil.example"), "/"},
		{"?next=" + url.QueryEscape("/\\evil.example"), "/"},
		{"?next=" + url.QueryEscape("https://evil.example/"), "/"},
		{"?next=" + url.QueryEscape("/p/abc?x=1&y=<b>"), "/p/abc?x=1&y=<b>"},
	} {
		rec := ts.do(t, "GET", "/login"+c.query, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.query, rec.Code)
		}
		if got := dataNext(t, rec.Body.String()); got != c.want {
			t.Errorf("%s: data-next %q, want %q", c.query, got, c.want)
		}
		if strings.Contains(rec.Body.String(), "<b>") {
			t.Errorf("%s: next not escaped", c.query)
		}
	}
	rec := ts.do(t, "GET", "/p/abc?x=1", nil, nil)
	rec = ts.do(t, "GET", rec.Header().Get("Location"), nil, nil)
	if got := dataNext(t, rec.Body.String()); got != "/p/abc?x=1" {
		t.Fatalf("round trip: data-next %q", got)
	}
}

const badAssertion = `{"ceremony":"forged","credential":{}}`

// TestFailureBucketDoesNotBlockValidLogin: bad login finishes drain the
// login failure bucket (burst 10; the fake clock never refills it), the rest
// answer 429 and are counted but not audited, and a valid login still works.
func TestFailureBucketDoesNotBlockValidLogin(t *testing.T) {
	for _, builtin := range []bool{true, false} {
		t.Run(map[bool]string{true: "builtin", false: "access"}[builtin], func(t *testing.T) {
			ts := newTestServer(t)
			if builtin {
				ts = builtinServer(t)
			}
			ts.enroll(t)
			n401, n429 := 0, 0
			for i := range 40 {
				rec := ts.do(t, "POST", "/login/finish", badAssertion, nil)
				switch rec.Code {
				case http.StatusUnauthorized:
					if n429 > 0 {
						t.Fatalf("finish %d: 401 after a 429", i+1)
					}
					n401++
				case http.StatusTooManyRequests:
					n429++
					if got := rec.Header().Get("Retry-After"); got != "60" {
						t.Fatalf("Retry-After = %q", got)
					}
					if jsonError(t, rec) == "" {
						t.Fatal("429 without an error message")
					}
				default:
					t.Fatalf("finish %d: status %d, body %s", i+1, rec.Code, rec.Body)
				}
			}
			if n401 != 10 || n429 != 30 {
				t.Fatalf("%d answered 401, %d answered 429; want 10 and 30", n401, n429)
			}
			if rows := ts.audit(t, "passkey_failure"); len(rows) != 10 {
				t.Fatalf("%d passkey_failure rows, want 10: a 429 is not audited", len(rows))
			}
			if got := ts.authFailures(t)["public/passkey"]; got != 40 {
				t.Fatalf("auth failures counted %d, want 40", got)
			}
			rec := ts.do(t, "POST", "/login/finish", ts.loginFinishBody(t), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("valid login: status %d, body %s", rec.Code, rec.Body)
			}
			sessionCookie(t, rec)
			// The bucket refills over time: 2 s buys one more answered failure.
			ts.clock.Add(2 * time.Second)
			if rec := ts.do(t, "POST", "/login/finish", badAssertion, nil); rec.Code != http.StatusUnauthorized {
				t.Fatalf("after a refill: status %d", rec.Code)
			}
		})
	}
}

// TestEnrollBucketSeparate: the login, enrolment and consent buckets are
// separate, and a full enrolment bucket still lets a valid enrolment in.
func TestEnrollBucketSeparate(t *testing.T) {
	ts := builtinServer(t)
	for range 10 {
		ts.do(t, "POST", "/login/finish", badAssertion, nil)
	}
	if rec := ts.do(t, "POST", "/login/finish", badAssertion, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login bucket not empty: status %d", rec.Code)
	}
	tok := ts.newEnrollment(t)
	badEnroll := map[string]any{"ceremony": "forged", "token": tok, "label": "x", "credential": map[string]any{}}
	if rec := ts.do(t, "POST", "/enroll/finish", badEnroll, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("enrol finish with the login bucket empty: status %d, body %s", rec.Code, rec.Body)
	}
	for range 9 {
		ts.do(t, "POST", "/enroll/finish", badEnroll, nil)
	}
	if rec := ts.do(t, "POST", "/enroll/finish", badEnroll, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("enrol bucket not empty: status %d", rec.Code)
	}
	if rec := ts.enrollWith(t, ts.newEnrollment(t), "phone"); rec.Code != http.StatusOK {
		t.Fatalf("valid enrolment with the bucket empty: status %d, body %s", rec.Code, rec.Body)
	}
	if rows := ts.audit(t, "passkey_failure"); len(rows) != 20 {
		t.Fatalf("%d passkey_failure rows, want 20 (10 login, 10 enrol)", len(rows))
	}
}

// oauthRequests returns coachd.oauth.requests for endpoint and outcome.
func (ts *testServer) oauthRequests(t *testing.T, endpoint, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := ts.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "coachd.oauth.requests" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				e, _ := dp.Attributes.Value(attribute.Key("endpoint"))
				o, _ := dp.Attributes.Value(attribute.Key("outcome"))
				if e.AsString() == endpoint && o.AsString() == outcome {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// TestConsentBucket: bad consent finishes drain the consent bucket; the
// 429s count as consent rate_limited, and a valid approval still works.
func TestConsentBucket(t *testing.T) {
	ts := builtinServer(t)
	cookie := ts.enroll(t)
	for i := range 10 {
		if rec := ts.do(t, "POST", "/oauth/authorize/finish", badAssertion, cookie); rec.Code != http.StatusUnauthorized {
			t.Fatalf("finish %d: status %d", i+1, rec.Code)
		}
	}
	for range 3 {
		rec := ts.do(t, "POST", "/oauth/authorize/finish", badAssertion, cookie)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
			t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if n := ts.oauthRequests(t, "consent", "rate_limited"); n != 3 {
		t.Fatalf("consent rate_limited = %d, want 3", n)
	}
	if n := ts.oauthRequests(t, "consent", "invalid_request"); n != 10 {
		t.Fatalf("consent invalid_request = %d, want 10", n)
	}
	if rows := ts.audit(t, "passkey_failure"); len(rows) != 10 {
		t.Fatalf("%d passkey_failure rows, want 10", len(rows))
	}
	body := ts.consentFinishBody(t, cookie, ts.consentToken(t, cookie))
	if rec := ts.do(t, "POST", "/oauth/authorize/finish", body, cookie); rec.Code != http.StatusOK {
		t.Fatalf("valid approval with the bucket empty: status %d, body %s", rec.Code, rec.Body)
	}
}

// TestRecordFailureSampled: with a sampler of 2 an hour, 5 bad logins write
// 2 passkey_failure rows; the first failure of the next hour first writes an
// audit_suppressed row with the count. The metric counts every failure.
func TestRecordFailureSampled(t *testing.T) {
	ts := builtinServer(t, func(d *Deps) { d.Sampler = auth.NewAuditSampler(2, d.Now) })
	for i := range 5 {
		if rec := ts.do(t, "POST", "/login/finish", badAssertion, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("finish %d: status %d", i+1, rec.Code)
		}
	}
	if rows := ts.audit(t, "passkey_failure"); len(rows) != 2 {
		t.Fatalf("%d passkey_failure rows, want 2", len(rows))
	}
	if rows := ts.audit(t, "audit_suppressed"); len(rows) != 0 {
		t.Fatalf("audit_suppressed before the hour ended: %+v", rows)
	}
	ts.clock.Add(time.Hour)
	if rec := ts.do(t, "POST", "/login/finish", badAssertion, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	sup := ts.audit(t, "audit_suppressed")
	if len(sup) != 1 || sup[0].Outcome != "3" || sup[0].Target != "pre_auth_failures" || sup[0].Actor != "runner" || sup[0].Lane != "public" {
		t.Fatalf("audit_suppressed rows %+v", sup)
	}
	if rows := ts.audit(t, "passkey_failure"); len(rows) != 3 {
		t.Fatalf("%d passkey_failure rows, want 3", len(rows))
	}
	if all := ts.allAudit(t); len(all) < 2 || all[0] != "passkey_failure" || all[1] != "audit_suppressed" {
		t.Fatalf("audit_suppressed must precede the row that reports it: %q", all)
	}
	if got := ts.authFailures(t)["public/passkey"]; got != 6 {
		t.Fatalf("auth failures counted %d, want 6", got)
	}
}

// TestSamplerOnlyPreAuthFinishes: spec §6 samples only failed login,
// enrolment and consent finishes. With the sampler exhausted by bad logins,
// a failed approval (a signed-in action) is still audited.
func TestSamplerOnlyPreAuthFinishes(t *testing.T) {
	ts := builtinServer(t, func(d *Deps) { d.Sampler = auth.NewAuditSampler(1, d.Now) })
	cookie := ts.enroll(t)
	for range 3 {
		ts.do(t, "POST", "/login/finish", badAssertion, nil)
	}
	if rows := ts.audit(t, "passkey_failure"); len(rows) != 1 {
		t.Fatalf("%d passkey_failure rows after 3 bad logins with a sampler of 1, want 1", len(rows))
	}
	p := ts.addProposal(t, "Easy run", ts.today().AddDays(1), store.ProposalPending, "")
	for i := range 2 {
		rec := ts.do(t, "POST", "/p/"+p.ID+"/approve", badAssertion, cookie)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusConflict {
			t.Fatalf("approve %d: status %d, body %s", i+1, rec.Code, rec.Body)
		}
	}
	var approve int
	for _, e := range ts.audit(t, "passkey_failure") {
		if e.Target == "/p/{id}/approve" {
			approve++
		}
	}
	if approve != 2 {
		t.Fatalf("%d approve passkey_failure rows, want 2: approvals are never sampled", approve)
	}
}

// TestGateOddRequests pins builtin-mode requests without a cookie whose
// method or path is not a route's own: each is gated (303 to the login page
// for GET and HEAD, 401 otherwise), or answered only by the mux's own
// redirect to the clean path, never by a gated handler. Nothing is audited.
func TestGateOddRequests(t *testing.T) {
	ts := builtinServer(t)
	const gate, mux = "gate", "mux"
	for _, c := range []struct {
		method, path, by, location string
	}{
		{"OPTIONS", "/login", gate, ""},
		{"PUT", "/login", gate, ""},
		{"GET", "/login/", gate, "/login?next=%2Flogin%2F"},
		{"HEAD", "/login/", gate, "/login?next=%2Flogin%2F"},
		{"GET", "/static", mux, "/static/"},
		{"HEAD", "/static", mux, "/static/"},
		{"GET", "/audit/../login", mux, "/login"},
		{"HEAD", "/audit/../login", mux, "/login"},
		{"GET", "/static/../audit", gate, "/login?next=%2Fstatic%2F..%2Faudit"},
		{"HEAD", "/static/../audit", gate, "/login?next=%2Fstatic%2F..%2Faudit"},
		{"POST", "/static/../audit", gate, ""},
		{"POST", "/audit/../login/begin", mux, "/login/begin"},
		{"GET", "//login", mux, "/login"},
		{"GET", "/static/../p/abc", gate, "/login?next=%2Fstatic%2F..%2Fp%2Fabc"},
	} {
		name := c.method + " " + c.path
		rec := ts.serve(httptest.NewRequest(c.method, testOrigin+c.path, nil))
		loc := rec.Header().Get("Location")
		switch {
		case c.by == mux:
			if rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently || loc != c.location {
				t.Errorf("%s: status %d, Location %q, want the mux's redirect to %q", name, rec.Code, loc, c.location)
			}
		case c.method == "GET" || c.method == "HEAD":
			if rec.Code != http.StatusSeeOther || loc != c.location {
				t.Errorf("%s: status %d, Location %q, want 303 to %q", name, rec.Code, loc, c.location)
			}
		default:
			// No JSON content type: the HTML sign-in page with a link.
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", name, rec.Code)
				continue
			}
			if b := rec.Body.String(); !strings.Contains(b, msgGate) || !strings.Contains(b, `href="/login"`) {
				t.Errorf("%s: not the sign-in page:\n%s", name, b)
			}
		}
		if (c.by == gate) && c.method != "GET" && c.method != "HEAD" {
			req := httptest.NewRequest(c.method, testOrigin+c.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			jrec := ts.serve(req)
			if jrec.Code != http.StatusUnauthorized || jsonError(t, jrec) != msgGate {
				t.Errorf("%s (JSON): status %d, body %s", name, jrec.Code, jrec.Body)
			}
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: WWW-Authenticate set", name)
		}
	}
	if rows := ts.allAudit(t); len(rows) != 0 {
		t.Errorf("odd requests wrote audit rows: %q", rows)
	}
}

// TestStrangerFloodLogsAreBounded: in builtin mode a flood of bad
// /login/finish and /enroll/begin requests adds only a small constant number
// of log lines, while the auth-failure metric still counts every request.
func TestStrangerFloodLogsAreBounded(t *testing.T) {
	ts := builtinServer(t)
	ts.enroll(t)
	before := strings.Count(ts.logs.String(), "\n")
	for range 1000 {
		ts.do(t, "POST", "/login/finish", badAssertion, nil)
	}
	loginLines := strings.Count(ts.logs.String(), "\n") - before
	if loginLines > 15 {
		t.Errorf("1000 bad logins wrote %d log lines", loginLines)
	}
	before = strings.Count(ts.logs.String(), "\n")
	for range 1000 {
		ts.do(t, "POST", "/enroll/begin", map[string]any{"token": "nope"}, nil)
	}
	enrollLines := strings.Count(ts.logs.String(), "\n") - before
	if enrollLines > 15 {
		t.Errorf("1000 bad enrolments wrote %d log lines", enrollLines)
	}
	if got := ts.authFailures(t)["public/enroll_token"]; got != 1000 {
		t.Errorf("enroll_token failures counted %d, want 1000", got)
	}
	if got := ts.authFailures(t)["public/passkey"]; got != 1000 {
		t.Errorf("passkey failures counted %d, want 1000", got)
	}
}
