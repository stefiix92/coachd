package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/workout"
)

// reviewBody opens /review as the browser would, runs the authenticator on
// its approve button and returns the body app.js posts to /review/approve.
func (ts *testServer) reviewBody(t *testing.T) []byte {
	t.Helper()
	rec := ts.do(t, "GET", "/review", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /review: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	c, o, u := ceremonyAttr.FindStringSubmatch(body), optionsAttr.FindStringSubmatch(body), urlAttr.FindStringSubmatch(body)
	if c == nil || o == nil || u == nil {
		t.Fatalf("no approve button on /review: %s", body)
	}
	if got := html.UnescapeString(u[1]); got != "/review/approve" {
		t.Fatalf("data-url = %q", got)
	}
	cred, err := ts.authn.Get([]byte(html.UnescapeString(o[1])))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"ceremony": html.UnescapeString(c[1]), "credential": json.RawMessage(cred)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (ts *testServer) reviewPage(t *testing.T) string {
	t.Helper()
	rec := ts.do(t, "GET", "/review", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /review: status %d, body %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestReviewApproveAll(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	old := ts.addProposal(t, "Old long run", ts.today().AddDays(3), store.ProposalDelivered, "")
	a := ts.propose(t, intervals(ts.today().AddDays(1)), proposeOpts{traced: true})
	b := ts.addProposal(t, "Easy", ts.today().AddDays(2), store.ProposalPending, `[{"code":"same_day","message":"two workouts that day"}]`)
	tenMin := 600
	c := ts.propose(t, workout.Workout{Name: "New long run", Sport: "running", Date: ts.today().AddDays(3).String(),
		Steps: []workout.Step{{StepBase: workout.StepBase{Kind: "active", Duration: &workout.Duration{Type: "time", TimeS: &tenMin}}}}},
		proposeOpts{replaces: old.ID})
	ts.clock.Add(time.Minute)

	page := ts.reviewPage(t)
	for _, want := range []string{"&lt;b&gt;Intervals&lt;/b&gt;", "Easy", "New long run", "two workouts that day",
		"Replaces", "Old long run", "Approve all 3 with passkey", `href="/p/` + a.ID + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	if strings.Contains(page, "<b>Intervals</b>") {
		t.Fatal("agent text not escaped")
	}

	rec := ts.do(t, "POST", "/review/approve", ts.reviewBody(t), nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true,"count":3}` {
		t.Fatalf("approve all: status %d, body %s", rec.Code, rec.Body)
	}
	for id, want := range map[string]store.ProposalStatus{a.ID: store.ProposalApproved, b.ID: store.ProposalApproved,
		c.ID: store.ProposalApproved, old.ID: store.ProposalWithdrawn} {
		if got := ts.status(t, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	rows := ts.audit(t, "proposal_approved")
	if len(rows) != 3 {
		t.Fatalf("proposal_approved rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Lane != "public" || r.Actor != "runner" || r.Client != "passkey:1" || r.Outcome != "ok" || !r.At.Equal(ts.clock.Now()) {
			t.Errorf("audit row %+v", r)
		}
	}
	if w := ts.audit(t, "proposal_withdrawn"); len(w) != 1 || w[0].Target != old.ID {
		t.Fatalf("proposal_withdrawn rows = %+v", w)
	}
	if !signedIn(t, ts.do(t, "GET", "/review", nil, sessionCookie(t, rec))) {
		t.Fatal("approve all did not start a session")
	}
	if n, _ := ts.approvalLatency(t); n != 3 {
		t.Fatalf("approval latency count = %d, want 3", n)
	}
	spans := ts.spansNamed("proposal.approve_all")
	if len(spans) != 1 {
		t.Fatalf("proposal.approve_all spans = %d", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["count"] != "3" || attrs["outcome"] != "approved" || attrs["proposal_id"] != "" {
		t.Fatalf("span attributes = %v", attrs)
	}
	if l := spans[0].Links(); len(l) != 1 || l[0].SpanContext.TraceID().String() != testTraceID {
		t.Fatalf("span links = %+v", l)
	}
	if !strings.Contains(ts.reviewPage(t), "No workout is waiting that can be approved.") {
		t.Fatal("review page after approval should be empty")
	}
}

// The set leaves out what a single approval would refuse, and what another
// waiting workout replaces.
func TestReviewSetRules(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	past := ts.addProposal(t, "Yesterday", ts.today().AddDays(-1), store.ProposalPending, "")
	tampered := ts.addProposal(t, "Tampered", ts.today().AddDays(1), store.ProposalPending, "")
	ts.rawExec(t, `UPDATE proposals SET content_json = ? WHERE id = ?`, `{"name":"x","sport":"running","date":"`+ts.today().AddDays(1).String()+`","steps":[]}`, tampered.ID)
	first := ts.addProposal(t, "First draft", ts.today().AddDays(2), store.ProposalPending, "")
	second := ts.propose(t, intervals(ts.today().AddDays(2)), proposeOpts{replaces: first.ID})
	today := ts.addProposal(t, "Today", ts.today(), store.ProposalPending, "")

	page := ts.reviewPage(t)
	for _, want := range []string{"Approve all 2 with passkey", "Not included", whyDatePassed, whyTampered, whyReplaced} {
		if !strings.Contains(page, html.EscapeString(want)) {
			t.Errorf("review page lacks %q", want)
		}
	}
	rec := ts.do(t, "POST", "/review/approve", ts.reviewBody(t), nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true,"count":2}` {
		t.Fatalf("approve all: status %d, body %s", rec.Code, rec.Body)
	}
	for id, want := range map[string]store.ProposalStatus{today.ID: store.ProposalApproved, second.ID: store.ProposalApproved,
		first.ID: store.ProposalSuperseded, past.ID: store.ProposalPending, tampered.ID: store.ProposalPending} {
		if got := ts.status(t, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

func TestReviewCap(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	for i := 0; i < maxApproveAll+1; i++ {
		ts.addProposal(t, "Run", ts.today().AddDays(1+i%14), store.ProposalPending, "")
	}
	page := ts.reviewPage(t)
	if !strings.Contains(page, "Approve all 20 with passkey") || !strings.Contains(page, whyOverCap) {
		t.Fatalf("review page over the cap: %s", page)
	}
}

// Any change between the page and the approval refuses the whole set.
func TestReviewApproveAllConflicts(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(ts *testServer, p store.Proposal)
		want   string
	}{
		"cancelled by the agent": {func(ts *testServer, p store.Proposal) {
			ts.rawExec(t, `UPDATE proposals SET status = 'cancelled' WHERE id = ?`, p.ID)
		}, "this proposal is cancelled; reload the page"},
		"content changed": {func(ts *testServer, p store.Proposal) {
			ts.rawExec(t, `UPDATE proposals SET content_json = '{}' WHERE id = ?`, p.ID)
		}, msgContentChanged},
		"hash column changed": {func(ts *testServer, p store.Proposal) {
			ts.rawExec(t, `UPDATE proposals SET content_hash = 'other' WHERE id = ?`, p.ID)
		}, msgContentChanged},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.enroll(t)
			a := ts.addProposal(t, "Easy", ts.today().AddDays(1), store.ProposalPending, "")
			b := ts.addProposal(t, "Tempo", ts.today().AddDays(2), store.ProposalPending, "")
			body := ts.reviewBody(t)
			tc.change(ts, b)
			rec := ts.do(t, "POST", "/review/approve", body, nil)
			if rec.Code != http.StatusConflict || jsonError(t, rec) != longDay(b.Date)+": "+tc.want {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if got := ts.status(t, a.ID); got != store.ProposalPending {
				t.Fatalf("a = %s, want pending: nothing may be approved", got)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Fatal("a refused approval set a cookie")
			}
			if n := len(ts.audit(t, "proposal_approved")); n != 0 {
				t.Fatalf("proposal_approved rows = %d", n)
			}
		})
	}
}

func TestReviewApproveAllReplay(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	ts.addProposal(t, "Easy", ts.today().AddDays(1), store.ProposalPending, "")
	body := ts.reviewBody(t)
	if rec := ts.do(t, "POST", "/review/approve", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("first: status %d, body %s", rec.Code, rec.Body)
	}
	rec := ts.do(t, "POST", "/review/approve", body, nil)
	if rec.Code != http.StatusConflict || jsonError(t, rec) != msgApproveUsed {
		t.Fatalf("replay: status %d, body %s", rec.Code, rec.Body)
	}
	if f := ts.audit(t, "passkey_failure"); len(f) != 1 || f[0].Outcome != "used" || f[0].Target != "/review/approve" {
		t.Fatalf("passkey_failure rows = %+v", f)
	}
}

// A single approval's ceremony does not approve a set, nor the reverse.
func TestReviewCeremoniesDoNotMix(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	p := ts.addProposal(t, "Easy", ts.today().AddDays(1), store.ProposalPending, "")
	if rec := ts.do(t, "POST", "/review/approve", ts.approveBody(t, p.ID), nil); rec.Code != http.StatusConflict ||
		jsonError(t, rec) != msgApproveExpired {
		t.Fatalf("single ceremony on /review/approve: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := ts.do(t, "POST", "/p/"+p.ID+"/approve", ts.reviewBody(t), nil); rec.Code != http.StatusConflict ||
		jsonError(t, rec) != msgApproveExpired {
		t.Fatalf("set ceremony on /p/{id}/approve: status %d, body %s", rec.Code, rec.Body)
	}
	if got := ts.status(t, p.ID); got != store.ProposalPending {
		t.Fatalf("status = %s", got)
	}
}

func TestReviewMalformed(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	for _, body := range []any{map[string]any{}, map[string]any{"ceremony": "x"}, map[string]any{"credential": map[string]any{}}} {
		if rec := ts.do(t, "POST", "/review/approve", body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status %d", body, rec.Code)
		}
	}
}

func TestReviewWithoutPasskey(t *testing.T) {
	ts := newTestServer(t)
	ts.addProposal(t, "Easy", ts.today().AddDays(1), store.ProposalPending, "")
	page := ts.reviewPage(t)
	if strings.Contains(page, `data-action="approve"`) || !strings.Contains(page, "No passkey enrolled") {
		t.Fatalf("review page without a passkey: %s", page)
	}
}

func TestReviewLinks(t *testing.T) {
	ts := newTestServer(t)
	ts.enroll(t)
	a := ts.addProposal(t, "Easy", ts.today().AddDays(1), store.ProposalPending, "")
	home := func() string {
		rec := ts.do(t, "GET", "/", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /: %d", rec.Code)
		}
		return rec.Body.String()
	}
	if strings.Contains(home(), `href="/review"`) || strings.Contains(ts.page(t, a.ID, nil), `href="/review"`) {
		t.Fatal("one waiting workout must not offer Review all")
	}
	ts.addProposal(t, "Tempo", ts.today().AddDays(2), store.ProposalPending, "")
	if !strings.Contains(home(), `href="/review"`) {
		t.Fatal("home lacks Review all")
	}
	if page := ts.page(t, a.ID, nil); !strings.Contains(page, "1 more workout waiting") || !strings.Contains(page, `href="/review"`) {
		t.Fatalf("workout page lacks the waiting line: %s", page)
	}
	if _, err := ts.db.TransitionProposal(context.Background(), a.ID, store.Transition{From: []store.ProposalStatus{store.ProposalPending},
		To: store.ProposalDeclined, At: ts.clock.Now(), Lane: "public", Actor: "runner"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ts.page(t, a.ID, nil), "more workout waiting") {
		t.Fatal("a declined workout's page must not show the waiting line")
	}
}
