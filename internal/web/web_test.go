package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/passkey/passkeytest"
	"github.com/stefiix92/coachd/internal/store"
)

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func checkPageHeaders(t *testing.T, name string, h http.Header, cache string) {
	t.Helper()
	for k, want := range map[string]string{
		"Content-Security-Policy": wantCSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           cache,
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", name, k, got, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	for path, status := range map[string]int{"/": 200, "/login": 200, "/enroll": 200, "/nope": 404} {
		rec := ts.do(t, "GET", path, nil, nil)
		if rec.Code != status {
			t.Errorf("GET %s: status %d, want %d", path, rec.Code, status)
		}
		checkPageHeaders(t, "GET "+path, rec.Header(), "no-store")
	}
	// JSON and error responses carry them too.
	rec := ts.do(t, "POST", "/login/begin", "{}", nil)
	checkPageHeaders(t, "POST /login/begin", rec.Header(), "no-store")
	rec = ts.do(t, "POST", "/logout", nil, nil)
	checkPageHeaders(t, "POST /logout without session", rec.Header(), "no-store")
}

func TestRejectsNonPublicIdentity(t *testing.T) {
	ts := newTestServer(t)
	ctxs := map[string]context.Context{
		"no identity": context.Background(),
		"private lane": auth.WithIdentity(context.Background(), auth.Identity{Lane: auth.LanePrivate, Subject: "tok",
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite}}),
	}
	for name, ctx := range ctxs {
		for _, mk := range []func() *http.Request{
			func() *http.Request { return httptest.NewRequest("GET", testOrigin+"/", nil) },
			func() *http.Request { return httptest.NewRequest("GET", testOrigin+"/static/app.js", nil) },
			func() *http.Request {
				r := httptest.NewRequest("POST", testOrigin+"/login/begin", strings.NewReader("{}"))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		} {
			req := mk()
			rec := httptest.NewRecorder()
			ts.h.ServeHTTP(rec, req.WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s, %s %s: status %d, want 403", name, req.Method, req.URL.Path, rec.Code)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("%s: CSP = %q", name, got)
			}
		}
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	ts := newTestServer(t)
	cookie := ts.enroll(t)
	ctx := context.Background()

	req := httptest.NewRequest("POST", testOrigin+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	if rec := ts.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout: status %d, want 403", rec.Code)
	}
	if _, err := ts.db.SessionByHash(ctx, hashToken(cookie.Value), ts.clock.Now()); err != nil {
		t.Fatalf("cross-site logout ended the session: %v", err)
	}
	// An old browser's foreign Origin is refused as well.
	req = httptest.NewRequest("POST", testOrigin+"/login/begin", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example")
	if rec := ts.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status %d, want 403", rec.Code)
	}

	req = httptest.NewRequest("POST", testOrigin+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(cookie)
	if rec := ts.serve(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin logout: status %d, want 303", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	ts := newTestServer(t)
	// Small enough for every handler to act on, were it not for its size.
	big := `{"ceremony":"` + strings.Repeat("a", 65*1024) + `","token":"x","credential":{"id":"x"}}`
	for _, path := range []string{"/login/begin", "/login/finish", "/enroll/begin", "/enroll/finish", "/logout"} {
		rec := ts.do(t, "POST", path, big, nil)
		if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with 65 KiB: status %d, want 413 or 400", path, rec.Code)
		}
	}
	// Without a Content-Length the reader cap still applies.
	req := httptest.NewRequest("POST", testOrigin+"/login/finish", io.MultiReader(strings.NewReader(big)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	if rec := ts.serve(req); rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("chunked 65 KiB: status %d, want 413 or 400", rec.Code)
	}
}

// signedIn reports whether the page shows the signed-in state.
func signedIn(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	body := rec.Body.String()
	in := strings.Contains(body, `action="/logout"`)
	out := strings.Contains(body, `href="/login"`)
	if in == out {
		t.Fatalf("page shows signed-in=%v and signed-out=%v", in, out)
	}
	return in
}

func TestEnrollLoginLogout(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	token := ts.newEnrollment(t)
	rec := ts.enrollWith(t, token, "  phone  ")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("enroll finish: status %d, body %s", rec.Code, rec.Body)
	}
	raw := strings.Join(rec.Header().Values("Set-Cookie"), "\n")
	for _, attr := range []string{"__Host-coachd_session=", "Path=/", "Secure", "HttpOnly", "SameSite=Lax", "Max-Age=43200"} {
		if !strings.Contains(raw, attr) {
			t.Errorf("Set-Cookie %q lacks %q", raw, attr)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Errorf("__Host- cookie with a Domain: %q", raw)
	}
	cookie := sessionCookie(t, rec)
	if v, err := base64.RawURLEncoding.DecodeString(cookie.Value); err != nil || len(v) != 32 {
		t.Fatalf("cookie value is not 32 bytes base64url: %d bytes, %v", len(v), err)
	}
	pks, err := ts.db.ActivePasskeys(ctx)
	if err != nil || len(pks) != 1 || pks[0].Label != "phone" {
		t.Fatalf("passkeys = %+v, %v", pks, err)
	}
	pid := pks[0].ID

	if !signedIn(t, ts.do(t, "GET", "/", nil, cookie)) {
		t.Fatal("home after enrolment shows signed out")
	}
	if signedIn(t, ts.do(t, "GET", "/", nil, nil)) {
		t.Fatal("home without a cookie shows signed in")
	}

	rec = ts.do(t, "POST", "/logout", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("logout: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	cleared := sessionCookie(t, rec)
	if cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout cookie = %+v, want cleared", cleared)
	}
	if _, err := ts.db.SessionByHash(ctx, hashToken(cookie.Value), ts.clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after logout: %v", err)
	}
	if signedIn(t, ts.do(t, "GET", "/", nil, cookie)) {
		t.Fatal("home with the logged-out cookie shows signed in")
	}

	cookie2 := ts.login(t)
	if cookie2.Value == cookie.Value {
		t.Fatal("login reused the old session token")
	}
	if !signedIn(t, ts.do(t, "GET", "/", nil, cookie2)) {
		t.Fatal("home after login shows signed out")
	}

	enrolled, logins := ts.audit(t, "passkey_enrolled"), ts.audit(t, "passkey_login")
	if len(enrolled) != 1 || enrolled[0].Target != strconv.FormatInt(pid, 10) || enrolled[0].Lane != "public" || enrolled[0].Actor != "runner" ||
		enrolled[0].Client != "passkey:"+strconv.FormatInt(pid, 10) {
		t.Fatalf("passkey_enrolled rows = %+v", enrolled)
	}
	want := store.AuditEvent{Lane: "public", Actor: "runner", Client: "passkey:" + strconv.FormatInt(pid, 10),
		Event: "passkey_login", Target: strconv.FormatInt(pid, 10), Outcome: "ok"}
	if len(logins) != 1 {
		t.Fatalf("passkey_login rows = %+v", logins)
	}
	got := logins[0]
	got.At = time.Time{}
	if got != want {
		t.Fatalf("passkey_login = %+v, want %+v", got, want)
	}
	if f := ts.audit(t, "passkey_failure"); len(f) != 0 {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
	pk, err := ts.db.PasskeyByCredentialID(ctx, pks[0].CredentialID)
	if err != nil || pk.LastUsedAt == nil {
		t.Fatalf("login did not record use: %+v, %v", pk, err)
	}
	// Nothing secret reaches the logs.
	logs := ts.logs.String()
	for _, secret := range []string{token, cookie.Value, cookie2.Value, "phone"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain a secret or label: %s", logs)
		}
	}
}

func TestEnrollTokenSingleUse(t *testing.T) {
	ts := newTestServer(t)
	token := ts.newEnrollment(t)
	if rec := ts.enrollWith(t, token, "phone"); rec.Code != http.StatusOK {
		t.Fatalf("first enrolment: status %d, body %s", rec.Code, rec.Body)
	}
	rec := ts.do(t, "POST", "/enroll/begin", map[string]any{"token": token}, nil)
	if rec.Code != http.StatusNotFound || jsonError(t, rec) != "enrolment link invalid or expired" {
		t.Fatalf("second begin: status %d, body %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{"unknown token": `{"token":"nope"}`, "empty token": `{"token":""}`} {
		rec := ts.do(t, "POST", "/enroll/begin", body, nil)
		if rec.Code != http.StatusNotFound || jsonError(t, rec) != "enrolment link invalid or expired" {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	// An expired enrolment is refused too.
	late := ts.newEnrollment(t)
	ts.clock.Add(10 * time.Minute)
	if rec := ts.do(t, "POST", "/enroll/begin", map[string]any{"token": late}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expired enrolment: status %d", rec.Code)
	}
	// R-F3: a token miss needs no passkey, so an Access-only client could
	// repeat it at will. It is counted and logged, never audited.
	if f := ts.audit(t, "passkey_failure"); len(f) != 0 {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
	if got := ts.authFailures(t); got["public/enroll_token"] != 4 || got["public/passkey"] != 0 {
		t.Fatalf("auth failures = %v", got)
	}
	logs := ts.logs.String()
	if n := strings.Count(logs, `"level":"WARN","msg":"enrolment token invalid or expired"`); n != 4 ||
		!strings.Contains(logs, `"reason":"enroll_token"`) {
		t.Fatalf("%d warn lines: %s", n, logs)
	}
	for _, secret := range []string{token, late} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain a token: %s", logs)
		}
	}
}

// R-P1: finishing needs the token of the enrolment the ceremony is bound to.
func TestEnrollFinishNeedsMatchingToken(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	tokA, tokB := ts.newEnrollment(t), ts.newEnrollment(t)
	b := decodeBegin(t, ts.do(t, "POST", "/enroll/begin", map[string]any{"token": tokA}, nil))
	cred, err := ts.authn.Create(b.Options)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"other enrolment": tokB, "no token": "", "unknown": "nope"} {
		rec := ts.do(t, "POST", "/enroll/finish", map[string]any{"ceremony": b.Ceremony, "token": tok, "label": "x", "credential": json.RawMessage(cred)}, nil)
		if rec.Code != http.StatusNotFound || jsonError(t, rec) != "enrolment link invalid or expired" {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: set a cookie", name)
		}
	}
	if pks, err := ts.db.ActivePasskeys(ctx); err != nil || len(pks) != 0 {
		t.Fatalf("passkeys after refused finishes = %+v, %v", pks, err)
	}
	rec := ts.do(t, "POST", "/enroll/finish", map[string]any{"ceremony": b.Ceremony, "token": tokA, "label": "x", "credential": json.RawMessage(cred)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("matching token: status %d, body %s", rec.Code, rec.Body)
	}
	// Only the ceremony bound to another enrolment is a failed check; the
	// two token misses are counted, not audited (R-F3).
	if f := ts.audit(t, "passkey_failure"); len(f) != 1 || f[0].Outcome != "invalid" || f[0].Target != "/enroll/finish" {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
	if got := ts.authFailures(t); got["public/enroll_token"] != 2 || got["public/passkey"] != 1 {
		t.Fatalf("auth failures = %v", got)
	}
}

func TestEnrollLabel(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for name, label := range map[string]string{
		"too long":          strings.Repeat("é", 41),
		"control character": "pho\x00ne",
		"tab":               "pho\tne",
		"newline":           "pho\nne",
	} {
		rec := ts.enrollWith(t, ts.newEnrollment(t), label)
		if rec.Code != http.StatusBadRequest || !strings.HasPrefix(jsonError(t, rec), "label: ") {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	for _, label := range []string{"pho\tne", "pho\nne"} {
		rec := ts.enrollWith(t, ts.newEnrollment(t), label)
		if got := jsonError(t, rec); got != "label: must be a single line (no tab or newline)" {
			t.Errorf("label %q: error %q", label, got)
		}
	}
	// Exactly 40 runes is fine; blank becomes "passkey <date in the timezone>".
	ts.clock.Set(time.Date(2026, 10, 23, 22, 30, 0, 0, time.UTC)) // 00:30 on 24 Oct in Bratislava
	if rec := ts.enrollWith(t, ts.newEnrollment(t), "   "); rec.Code != http.StatusOK {
		t.Fatalf("blank label: status %d, body %s", rec.Code, rec.Body)
	}
	ts.authn = passkeytest.New(testOrigin) // the first one is now excluded
	if rec := ts.enrollWith(t, ts.newEnrollment(t), strings.Repeat("é", 40)); rec.Code != http.StatusOK {
		t.Fatalf("40-rune label: status %d, body %s", rec.Code, rec.Body)
	}
	pks, err := ts.db.ActivePasskeys(ctx)
	if err != nil || len(pks) != 2 || pks[0].Label != "passkey 2026-10-24" || pks[1].Label != strings.Repeat("é", 40) {
		t.Fatalf("passkeys = %+v, %v", pks, err)
	}
}

func TestLoginBeginWithoutPasskey(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/login/begin", "{}", nil)
	if rec.Code != http.StatusConflict || jsonError(t, rec) != "no passkey enrolled" {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestLoginReplay(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	body := ts.loginFinishBody(t)
	if rec := ts.do(t, "POST", "/login/finish", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("first: status %d, body %s", rec.Code, rec.Body)
	}
	rec := ts.do(t, "POST", "/login/finish", body, nil)
	if rec.Code != http.StatusConflict || jsonError(t, rec) != "already used; try again" {
		t.Fatalf("replay: status %d, body %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("replay set a cookie")
	}
	if n := len(ts.audit(t, "passkey_login")); n != 1 {
		t.Fatalf("passkey_login rows = %d", n)
	}
	if f := ts.audit(t, "passkey_failure"); len(f) != 1 || f[0].Outcome != "used" {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
}

func TestLoginWithoutUVFails(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	ts.authn.SetUserVerified(false)
	body := ts.loginFinishBody(t)
	rec := ts.do(t, "POST", "/login/finish", body, nil)
	if rec.Code != http.StatusUnauthorized || jsonError(t, rec) != "passkey check failed" {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("failed login set a cookie")
	}
	f := ts.audit(t, "passkey_failure")
	if len(f) != 1 || f[0].Outcome != "invalid" || f[0].Target != "/login/finish" || f[0].Lane != "public" || f[0].Actor != "runner" {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
	if got := ts.authFailures(t)["public/passkey"]; got != 1 {
		t.Fatalf("auth failures public/passkey = %d", got)
	}
	if n := len(ts.audit(t, "passkey_login")); n != 0 {
		t.Fatalf("passkey_login rows = %d", n)
	}
	if logs := ts.logs.String(); strings.Contains(logs, string(body[:40])) {
		t.Fatalf("logs contain the request body: %s", logs)
	}
}

func TestLoginFinishErrors(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	for name, tc := range map[string]struct {
		body   string
		status int
		msg    string
	}{
		"not JSON":      {"{", 400, "malformed request"},
		"no credential": {`{"ceremony":"x"}`, 400, "malformed request"},
		"forged":        {`{"ceremony":"x.y","credential":{"id":"x"}}`, 401, "passkey check failed"},
	} {
		rec := ts.do(t, "POST", "/login/finish", tc.body, nil)
		if rec.Code != tc.status || jsonError(t, rec) != tc.msg {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	// A ceremony past its 5 minutes is refused.
	body := ts.loginFinishBody(t)
	ts.clock.Add(5 * time.Minute)
	if rec := ts.do(t, "POST", "/login/finish", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired ceremony: status %d", rec.Code)
	}
	// Newest first: the expired ceremony, then the forged one, which is not
	// classed as a page left open (R-T6d).
	if f := ts.audit(t, "passkey_failure"); len(f) != 2 || f[0].Outcome != "expired" || f[1].Outcome != "invalid" {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
}

// An outage inside the ceremony is a 500, not a failed check: no
// passkey_failure row and no auth-failure count (R-T6d).
func TestPasskeyStoreFailureIs500(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	body := ts.loginFinishBody(t)
	ts.rawExec(t, `ALTER TABLE used_nonces RENAME TO used_nonces_gone`)
	rec := ts.do(t, "POST", "/login/finish", body, nil)
	if rec.Code != http.StatusInternalServerError || jsonError(t, rec) != "internal error" {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("failed login set a cookie")
	}
	if f := ts.audit(t, "passkey_failure"); len(f) != 0 {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
	if got := ts.authFailures(t)["public/passkey"]; got != 0 {
		t.Fatalf("auth failures public/passkey = %d", got)
	}
}

func TestRequireSession(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/logout", nil, nil)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("form post: status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if b := rec.Body.String(); !strings.Contains(b, "sign in with your passkey first") || !strings.Contains(b, `href="/login"`) {
		t.Fatalf("form post body: %s", b)
	}
	rec = ts.do(t, "POST", "/logout", "{}", nil)
	if rec.Code != http.StatusUnauthorized || jsonError(t, rec) != "sign in with your passkey first" {
		t.Fatalf("JSON post: status %d, body %s", rec.Code, rec.Body)
	}
	// A cookie that is not a session is no session.
	rec = ts.do(t, "POST", "/logout", nil, &http.Cookie{Name: "__Host-coachd_session", Value: "bogus"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus cookie: status %d", rec.Code)
	}
	// An expired session is no session.
	cookie := ts.enroll(t)
	ts.clock.Add(12 * time.Hour)
	if rec := ts.do(t, "POST", "/logout", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: status %d", rec.Code)
	}
}

// agentSpan is how the pages show agent-written text.
func agentSpan(escaped string) string {
	return `<span class="agent"><span class="agent-label">written by the agent</span> <span class="agent-text">` + escaped + `</span></span>`
}

func TestHomeEscapesAgentText(t *testing.T) {
	ts := newTestServer(t)
	today := ts.today()
	p := ts.addProposal(t, "<script>x</script>", today.AddDays(1), store.ProposalPending, `[{"code":"c","message":"m"}]`)
	ts.addProposal(t, `Easy "run" & <b>strides</b>`, today.AddDays(2), store.ProposalApproved, "")
	if _, err := ts.db.SetGoal(context.Background(), store.Goal{Name: "<img src=x onerror=alert(1)>", Date: today.AddDays(60),
		Sport: store.SportRunning, Target: "<i>sub 1:45</i>", CreatedBy: "agent", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	body := ts.do(t, "GET", "/", nil, nil).Body.String()
	for _, raw := range []string{"<script>x", "<b>strides", "<img src=x", "<i>sub"} {
		if strings.Contains(body, raw) {
			t.Errorf("home contains unescaped %q", raw)
		}
	}
	for _, want := range []string{
		agentSpan("&lt;script&gt;x&lt;/script&gt;"),
		agentSpan("Easy &#34;run&#34; &amp; &lt;b&gt;strides&lt;/b&gt;"),
		agentSpan("&lt;img src=x onerror=alert(1)&gt;"),
		agentSpan("&lt;i&gt;sub 1:45&lt;/i&gt;"),
		`href="/p/` + p.ID + `"`,
		"1 warning",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
}

func TestHomeLists(t *testing.T) {
	ts := newTestServer(t)
	today := ts.today()
	pend := ts.addProposal(t, "pending past", today.AddDays(-1), store.ProposalPending, `[{"code":"a","message":"a"},{"code":"b","message":"b"}]`)
	appr := ts.addProposal(t, "approved soon", today.AddDays(3), store.ProposalApproved, "")
	deliv := ts.addProposal(t, "delivered edge", today.AddDays(14), store.ProposalDelivered, "")
	far := ts.addProposal(t, "approved far", today.AddDays(15), store.ProposalApproved, "")
	old := ts.addProposal(t, "delivered yesterday", today.AddDays(-1), store.ProposalDelivered, "")
	decl := ts.addProposal(t, "declined", today.AddDays(2), store.ProposalDeclined, "")
	body := ts.do(t, "GET", "/", nil, nil).Body.String()
	for _, p := range []store.Proposal{pend, appr, deliv} {
		if !strings.Contains(body, `href="/p/`+p.ID+`"`) {
			t.Errorf("home lacks %q", p.Name)
		}
	}
	for _, p := range []store.Proposal{far, old, decl} {
		if strings.Contains(body, p.ID) {
			t.Errorf("home lists %q", p.Name)
		}
	}
	for _, want := range []string{"2 warnings", "Thu 22 Oct", "Mon 26 Oct", "delivered", "No active goal"} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	for _, link := range []string{`href="/notes"`, `href="/passkeys"`, `href="/audit"`} {
		if !strings.Contains(body, link) {
			t.Errorf("home lacks link %s", link)
		}
	}
}

func TestHomeListsDeliveryStates(t *testing.T) {
	ts := newTestServer(t)
	today := ts.today()
	going := ts.addProposal(t, "on its way", today.AddDays(1), store.ProposalDelivering, "")
	failed := ts.addProposal(t, "not arrived", today.AddDays(2), store.ProposalDeliveryFailed, "")
	body := ts.do(t, "GET", "/", nil, nil).Body.String()
	for _, c := range []struct {
		p      store.Proposal
		status string
	}{{going, "delivering"}, {failed, "delivery_failed"}} {
		want := `href="/p/` + c.p.ID + `"`
		i := strings.Index(body, want)
		if i < 0 {
			t.Errorf("home lacks %q", c.p.Name)
			continue
		}
		line := body[i:]
		if j := strings.Index(line, "</li>"); j >= 0 {
			line = line[:j]
		}
		if !strings.Contains(line, c.status) {
			t.Errorf("home row of %q lacks status %q: %s", c.p.Name, c.status, line)
		}
	}
}

func TestHomeNoPasskeyBanner(t *testing.T) {
	ts := newTestServer(t)
	banner := "No passkey enrolled. On the host, run: <code>docker compose exec coachd /coachd enroll</code>"
	for _, path := range []string{"/", "/login"} {
		if body := ts.do(t, "GET", path, nil, nil).Body.String(); !strings.Contains(body, banner) {
			t.Errorf("GET %s without passkeys lacks the banner: %s", path, body)
		}
	}
	if strings.Contains(ts.do(t, "GET", "/login", nil, nil).Body.String(), `data-action="login"`) {
		t.Error("login page offers sign-in without a passkey")
	}
	ts.enroll(t)
	for _, path := range []string{"/", "/login"} {
		if body := ts.do(t, "GET", path, nil, nil).Body.String(); strings.Contains(body, "No passkey enrolled") {
			t.Errorf("GET %s with a passkey shows the banner", path)
		}
	}
	if !strings.Contains(ts.do(t, "GET", "/login", nil, nil).Body.String(), `data-action="login"`) {
		t.Error("login page lacks the sign-in button")
	}
}

func TestStaticAssets(t *testing.T) {
	ts := newTestServer(t)
	for path, ctype := range map[string]string{"/static/app.js": "text/javascript", "/static/app.css": "text/css",
		"/static/favicon.svg": "image/svg+xml", "/static/favicon.png": "image/png", "/static/icon.png": "image/png"} {
		rec := ts.do(t, "GET", path, nil, nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("GET %s: status %d, Content-Type %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s: empty", path)
		}
		checkPageHeaders(t, "GET "+path, rec.Header(), "public, max-age=3600")
	}
	for _, path := range []string{"/static/", "/static/nope.js", "/static/sub/app.js"} {
		if rec := ts.do(t, "GET", path, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
	if rec := ts.do(t, "GET", "/static/../templates/home.html", nil, nil); rec.Code == http.StatusOK {
		t.Errorf("path traversal served: %s", rec.Body)
	}
	js := ts.do(t, "GET", "/static/app.js", nil, nil).Body.String()
	// Access's login redirect drops the #token fragment: the page asks for
	// the link again.
	for _, want := range []string{`history.replaceState(null, "", "/enroll")`, `[data-action="login"]`, `[data-action="approve"]`,
		"getTransports", "getClientExtensionResults", "open the enrolment link again"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if !strings.Contains(ts.do(t, "GET", "/", nil, nil).Body.String(), `role="alert"`) {
		t.Error(`pages lack the role="alert" element app.js reports errors in`)
	}
}

// The CSP forbids inline script, style and event handlers; the templates
// must not need them.
func TestNoInlineScriptOrStyle(t *testing.T) {
	inline := regexp.MustCompile(`(?i)<style|\sstyle=|\son[a-z]+=|<script(\s[^>]*)?>[^<]|javascript:`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		if m := inline.Find(b); m != nil {
			t.Errorf("%s: inline script or style %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionTokenHashing(t *testing.T) {
	ts := newTestServer(t)
	cookie := ts.enroll(t)
	sum := sha256.Sum256([]byte(cookie.Value))
	if _, err := ts.db.SessionByHash(context.Background(), sum[:], ts.clock.Now()); err != nil {
		t.Fatalf("session is not stored by SHA-256 of the cookie value: %v", err)
	}
	if _, err := ts.db.SessionByHash(context.Background(), []byte(cookie.Value), ts.clock.Now()); err == nil {
		t.Fatal("session stored by its plain token")
	}
}

func TestCountdown(t *testing.T) {
	today := civil.Date{Year: 2026, Month: time.October, Day: 24}
	for n, want := range map[int]string{0: "today", 1: "tomorrow", -1: "yesterday", 62: "in 62 days", -3: "3 days ago"} {
		if got := countdown(today, today.AddDays(n)); got != want {
			t.Errorf("countdown(%+d days) = %q, want %q", n, got, want)
		}
	}
}

func TestNavMarksCurrentPage(t *testing.T) {
	ts := newTestServer(t)
	c := ts.enroll(t)
	body := ts.do(t, "GET", "/notes", nil, c).Body.String()
	if !strings.Contains(body, `<a href="/notes" aria-current="page">`) || strings.Count(body, `aria-current="page"`) != 1 {
		t.Errorf("notes page must mark its own tab, and only it: %s", body)
	}
}

func TestHomeShowsMessages(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	add := func(text string, at time.Time) {
		t.Helper()
		if _, err := ts.db.InsertMessage(ctx, store.Message{Text: text, CreatedBy: "agent:private:claude", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	add("msg-old", t0.AddDate(0, 0, -8))
	for i := 1; i <= 6; i++ {
		add(fmt.Sprintf("msg-%d", i), t0.Add(-time.Duration(7-i)*time.Hour))
	}
	body := ts.do(t, "GET", "/", nil, nil).Body.String()
	if !strings.Contains(body, "Messages") || !strings.Contains(body, "Written by your agent") {
		t.Errorf("home lacks the Messages section")
	}
	prev := -1
	for i := 6; i >= 2; i-- {
		at := strings.Index(body, fmt.Sprintf("msg-%d<", i))
		if at < 0 || at < prev {
			t.Fatalf("msg-%d missing or out of order (at %d after %d)", i, at, prev)
		}
		prev = at
	}
	for _, gone := range []string{"msg-1<", "msg-old"} {
		if strings.Contains(body, gone) {
			t.Errorf("home shows %q", gone)
		}
	}
	// t0 is 09:00 UTC; msg-6 is one hour earlier: 08:00 UTC = 10:00 local.
	if !strings.Contains(body, "2026-10-23 10:00:00") {
		t.Errorf("home lacks the newest message time in the runner's timezone")
	}
}

func TestHomeEscapesMessage(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.db.InsertMessage(context.Background(), store.Message{
		Text: `<script>alert(1)</script> <a href="javascript:x">y</a>`, CreatedBy: "agent:private:claude", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	body := ts.do(t, "GET", "/", nil, nil).Body.String()
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("home lacks the escaped text")
	}
	for _, raw := range []string{"<script>alert", `href="javascript:`} {
		if strings.Contains(body, raw) {
			t.Errorf("home contains unescaped %q", raw)
		}
	}
}

func TestHomeNoMessagesNoSection(t *testing.T) {
	ts := newTestServer(t)
	if body := ts.do(t, "GET", "/", nil, nil).Body.String(); strings.Contains(body, "Messages") {
		t.Errorf("home has a Messages heading without messages")
	}
}
