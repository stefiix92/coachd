// Package notify tells the runner about new pending proposals, app
// connections (builtin auth mode), day, workout and week nudges and agent
// messages through ntfy, and posts events to a webhook. A proposal notice
// carries only a date and a link: no workout name, metrics or health data. A
// connection notice is a fixed text and a link to /apps: no client name. A
// nudge or message push is a fixed text and a link to the page: no health
// value and no agent text. The
// topic URL and token are secrets and are never logged.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/triggers"
)

const (
	queueSize      = 32
	attemptTimeout = 10 * time.Second
)

// backoff is the wait before attempts 2 and 3 (attempts at 0 s, 5 s and 30 s).
var backoff = []time.Duration{5 * time.Second, 25 * time.Second}

// noticeKind tells a proposal notice from the two connection notices.
type noticeKind int

const (
	kindProposal noticeKind = iota
	kindAppConnected
	kindConnectionRevoked
	kindDayReady
	kindWorkoutUnmatched
	kindWeekClosed
	kindAgentMessage
)

type notice struct {
	kind  noticeKind
	id    string // proposal notices only
	event string // nudges only: the event name (never set for the message push)
	date  civil.Date
	link  trace.SpanContext
}

// Connection notice texts (spec §9, M14). They never name the client.
const (
	textAppConnected      = "A new app was connected to coachd"
	textConnectionRevoked = "coachd revoked an app connection after a token was reused"
)

// Nudge and message push texts (proactive spec §5, §6). They carry no health
// value, name or agent text.
const (
	textDayReady         = "Last night's sleep data is in"
	textWorkoutUnmatched = "Yesterday's workout has no matching activity"
	textWeekClosed       = "Last week's numbers are complete"
	textAgentMessage     = "Your agent left a message"
)

// messageInterval is the least time between two agent-message pushes.
const messageInterval = 15 * time.Minute

var _ triggers.Sink = (*Ntfy)(nil)

// Ntfy sends one notification per pending proposal, connection event, nudge
// or (rate-limited) agent message from a single worker.
type Ntfy struct {
	topicURL, token, publicURL string
	client                     *http.Client
	m                          *telemetry.Metrics
	log                        *slog.Logger
	tracer                     trace.Tracer
	queue                      chan notice
	sleep                      func(ctx context.Context, d time.Duration) error
	now                        func() time.Time
	mu                         sync.Mutex
	lastMessage                time.Time // the last message push enqueued
}

// NewNtfy sends to topicURL (the full topic URL). token may be empty.
func NewNtfy(topicURL, token, publicURL string, client *http.Client,
	m *telemetry.Metrics, log *slog.Logger, tracer trace.Tracer) *Ntfy {
	if client == nil {
		client = http.DefaultClient
	}
	return &Ntfy{
		topicURL: topicURL, token: token, publicURL: strings.TrimRight(publicURL, "/"),
		client: client, m: m, log: log, tracer: tracer,
		queue: make(chan notice, queueSize),
		sleep: sleepCtx, now: time.Now,
	}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (n *Ntfy) count(ctx context.Context, outcome string) {
	if n.m != nil {
		n.m.NotifySends.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// ProposalPending enqueues a notice; it never blocks. A full queue drops it.
func (n *Ntfy) ProposalPending(id string, date civil.Date, link trace.SpanContext) {
	n.enqueue(notice{id: id, date: date, link: link})
}

// AppConnected enqueues "a new app was connected"; it never blocks (the
// token endpoint calls it inline). A full queue drops it.
func (n *Ntfy) AppConnected() { n.enqueue(notice{kind: kindAppConnected}) }

// ConnectionRevoked enqueues "an app connection was revoked after a token
// was reused"; it never blocks. A full queue drops it.
func (n *Ntfy) ConnectionRevoked() { n.enqueue(notice{kind: kindConnectionRevoked}) }

// Fire enqueues a nudge for a day, workout or week event; it never blocks.
// Other events send nothing.
func (n *Ntfy) Fire(_ context.Context, ev triggers.Event) {
	var kind noticeKind
	switch ev.Name {
	case triggers.HealthDayReady:
		kind = kindDayReady
	case triggers.WorkoutUnmatched:
		kind = kindWorkoutUnmatched
	case triggers.WeekClosed:
		kind = kindWeekClosed
	default:
		return
	}
	n.enqueue(notice{kind: kind, event: ev.Name, id: ev.ProposalID})
}

// AgentMessage enqueues "your agent left a message", at most once per 15
// minutes counted from the last push enqueued; a message inside the window is
// not pushed. A push dropped by a full queue does not start the window. It
// never blocks.
func (n *Ntfy) AgentMessage() {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if !n.lastMessage.IsZero() && now.Sub(n.lastMessage) < messageInterval {
		return
	}
	if n.enqueue(notice{kind: kindAgentMessage}) {
		n.lastMessage = now
	}
}

// enqueue queues nt without blocking and reports whether it was queued; a
// full queue drops and counts it.
func (n *Ntfy) enqueue(nt notice) bool {
	select {
	case n.queue <- nt:
		return true
	default:
		n.count(context.Background(), "dropped")
		n.log.Warn("notification dropped", n.logAttrs(nt, "outcome", "dropped")...)
		return false
	}
}

// logAttrs is the component, the proposal id (proposal notices), the event
// and proposal id (nudges) and kv.
func (n *Ntfy) logAttrs(nt notice, kv ...any) []any {
	attrs := []any{"component", "notify"}
	if nt.kind == kindProposal {
		attrs = append(attrs, "proposal_id", nt.id)
	}
	if nt.kind == kindDayReady || nt.kind == kindWorkoutUnmatched || nt.kind == kindWeekClosed {
		attrs = append(attrs, "event", nt.event)
		if nt.id != "" {
			attrs = append(attrs, "proposal_id", nt.id)
		}
	}
	return append(attrs, kv...)
}

// Run sends queued notices until ctx is done.
func (n *Ntfy) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case nt := <-n.queue:
			n.deliver(ctx, nt)
		}
	}
}

func (n *Ntfy) deliver(ctx context.Context, nt notice) {
	var opts []trace.SpanStartOption
	if nt.link.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: nt.link}))
	}
	name := "proposal.notify"
	switch nt.kind {
	case kindAppConnected, kindConnectionRevoked:
		name = "connection.notify"
	case kindDayReady, kindWorkoutUnmatched, kindWeekClosed, kindAgentMessage:
		name = "event.notify"
	}
	ctx, span := n.tracer.Start(ctx, name, opts...)
	defer span.End()
	if nt.kind == kindProposal {
		span.SetAttributes(attribute.String("proposal_id", nt.id))
	}
	if nt.kind == kindDayReady || nt.kind == kindWorkoutUnmatched || nt.kind == kindWeekClosed {
		span.SetAttributes(attribute.String("event", nt.event))
		if nt.id != "" {
			span.SetAttributes(attribute.String("proposal_id", nt.id))
		}
	}

	outcome, status, errClass := retry(ctx, n.sleep, func(ctx context.Context) (int, string) { return n.send(ctx, nt) })
	if outcome == "" {
		return
	}
	n.finish(ctx, span, outcome)
	if outcome != "sent" {
		n.fail(nt, outcome, status, errClass)
	}
}

// retry makes up to 1+len(backoff) attempts of send, which returns the HTTP
// status (0 on a network error) and an error class. A 2xx is "sent"; a 4xx
// other than 429 is "rejected" at once; anything else is retried, then
// "error". The outcome is empty when ctx ended first.
func retry(ctx context.Context, sleep func(context.Context, time.Duration) error,
	send func(context.Context) (int, string)) (outcome string, status int, errClass string) {
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			if sleep(ctx, backoff[attempt-1]) != nil {
				return "", status, errClass
			}
		}
		status, errClass = send(ctx)
		if ctx.Err() != nil {
			return "", status, errClass
		}
		switch {
		case status >= 200 && status < 300:
			return "sent", status, errClass
		case status >= 400 && status < 500 && status != http.StatusTooManyRequests:
			return "rejected", status, errClass
		}
	}
	return "error", status, errClass
}

// finish counts the outcome and records it on the span, marking the span as
// an error unless the notice was sent.
func (n *Ntfy) finish(ctx context.Context, span trace.Span, outcome string) {
	n.count(ctx, outcome)
	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome != "sent" {
		span.SetStatus(codes.Error, outcome)
	}
}

func (n *Ntfy) fail(nt notice, outcome string, status int, errClass string) {
	if errClass == "" {
		errClass = "none"
	}
	n.log.Warn("notification failed", n.logAttrs(nt, "outcome", outcome,
		"status_class", statusClass(status), "error_class", errClass)...)
}

// message is the notice's body and ntfy Actions header.
func (n *Ntfy) message(nt notice) (body, actions string) {
	switch nt.kind {
	case kindAppConnected:
		return textAppConnected, fmt.Sprintf("view, Open, %s/apps, clear=true", n.publicURL)
	case kindConnectionRevoked:
		return textConnectionRevoked, fmt.Sprintf("view, Open, %s/apps, clear=true", n.publicURL)
	case kindDayReady:
		return textDayReady, n.homeAction()
	case kindWorkoutUnmatched:
		return textWorkoutUnmatched, n.homeAction()
	case kindWeekClosed:
		return textWeekClosed, n.homeAction()
	case kindAgentMessage:
		return textAgentMessage, n.homeAction()
	}
	return fmt.Sprintf("Workout for %s ready to review", time.Date(nt.date.Year, nt.date.Month, nt.date.Day, 0, 0, 0, 0, time.UTC).Format("Mon 2 Jan")),
		fmt.Sprintf("view, Review, %s/p/%s, clear=true", n.publicURL, nt.id)
}

// homeAction is the Actions header linking to the page.
func (n *Ntfy) homeAction() string {
	return fmt.Sprintf("view, Open, %s/, clear=true", n.publicURL)
}

// send makes one attempt. It returns the HTTP status (0 on a network error)
// and the error class (the error's Go type, never its text: that can hold the URL).
func (n *Ntfy) send(ctx context.Context, nt notice) (int, string) {
	actx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	body, actions := n.message(nt)
	req, err := http.NewRequestWithContext(actx, http.MethodPost, n.topicURL, strings.NewReader(body))
	if err != nil {
		return 0, fmt.Sprintf("%T", err)
	}
	req.Header.Set("Title", "coachd")
	req.Header.Set("Actions", actions)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, fmt.Sprintf("%T", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, ""
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	}
	return "error"
}
