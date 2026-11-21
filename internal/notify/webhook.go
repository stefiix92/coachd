package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/outcome"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/triggers"
)

// EventActivityStored is the event of an activity coachd just stored.
const EventActivityStored = "activity.stored"

// Event is the JSON body a webhook receives. It carries IDs, a date and a
// sport only: no health values, names or other free text.
type Event struct {
	Event      string  `json:"event"`
	ActivityID int64   `json:"activity_id"`
	Date       string  `json:"date"`
	Sport      string  `json:"sport"`
	ProposalID *string `json:"proposal_id"` // the delivered workout it carried out, or null
}

// dayEvent is the body of a day, workout or week event: an event name, a
// date and, for workout.unmatched, the proposal ID. Nothing else.
type dayEvent struct {
	Event      string `json:"event"`
	Date       string `json:"date"`
	ProposalID string `json:"proposal_id,omitempty"`
}

// hookItem is one queued delivery: the marshalled body plus the keys its
// span and log lines carry.
type hookItem struct {
	body       []byte
	event      string
	activityID int64  // activity.stored only
	proposalID string // day events: the proposal, when present
}

// Webhook posts an Event to the runner's own automation (n8n, Home
// Assistant, an agent's trigger) when a recent activity is stored or a day,
// workout or week event fires, so a review can start without waiting for the
// next chat. Only the events it was configured with are sent. The URL and
// token are secrets and are never logged.
type Webhook struct {
	url, token string
	events     []string
	db         *store.DB
	client     *http.Client
	m          *telemetry.Metrics
	log        *slog.Logger
	tracer     trace.Tracer
	queue      chan hookItem
	sleep      func(ctx context.Context, d time.Duration) error
}

// NewWebhook posts to url with token as a bearer token (may be empty). Only
// the named events are sent. db links activities to proposals.
func NewWebhook(url, token string, events []string, db *store.DB, client *http.Client,
	m *telemetry.Metrics, log *slog.Logger, tracer trace.Tracer) *Webhook {
	if client == nil {
		client = http.DefaultClient
	}
	return &Webhook{url: url, token: token, events: events, db: db, client: client, m: m, log: log, tracer: tracer,
		queue: make(chan hookItem, queueSize), sleep: sleepCtx}
}

// ActivityStored enqueues the event of a: its link to a delivered proposal
// is looked up from the stored detail, and none when that fails. It never
// blocks; a full queue drops the event. It sends nothing unless activity.stored
// is among the configured events.
func (w *Webhook) ActivityStored(ctx context.Context, a store.Activity) {
	if !slices.Contains(w.events, EventActivityStored) {
		return
	}
	ev := Event{Event: EventActivityStored, ActivityID: a.ID, Date: a.Date.String(), Sport: string(a.Sport)}
	if d, err := w.db.GetActivityDetail(ctx, a.ID); err == nil && d.Workout != nil {
		ev.ProposalID, _ = outcome.Linker{DB: w.db}.ProposalFor(ctx,
			store.ActivityWorkout{ID: a.ID, Date: a.Date, Fingerprint: d.Workout.Fingerprint})
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	w.enqueue(ctx, hookItem{body: body, event: EventActivityStored, activityID: a.ID})
}

// Fire enqueues a day, workout or week event when it is among the configured
// events; it never blocks. A full queue drops the event.
func (w *Webhook) Fire(ctx context.Context, ev triggers.Event) {
	if !slices.Contains(w.events, ev.Name) {
		return
	}
	body, err := json.Marshal(dayEvent{Event: ev.Name, Date: ev.Date.String(), ProposalID: ev.ProposalID})
	if err != nil {
		return
	}
	w.enqueue(ctx, hookItem{body: body, event: ev.Name, proposalID: ev.ProposalID})
}

func (w *Webhook) enqueue(ctx context.Context, it hookItem) {
	select {
	case w.queue <- it:
	default:
		w.count(ctx, "dropped")
		w.log.Warn("webhook event dropped", it.logAttrs("outcome", "dropped")...)
	}
}

// logAttrs is the component, the item's ID keys and kv. Activity events log
// activity_id, as they always have; day events log event and proposal_id.
func (it hookItem) logAttrs(kv ...any) []any {
	attrs := []any{"component", "notify"}
	if it.activityID != 0 {
		attrs = append(attrs, "activity_id", it.activityID)
	} else {
		attrs = append(attrs, "event", it.event)
		if it.proposalID != "" {
			attrs = append(attrs, "proposal_id", it.proposalID)
		}
	}
	return append(attrs, kv...)
}

func (w *Webhook) count(ctx context.Context, outcome string) {
	if w.m != nil {
		w.m.WebhookSends.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// Run sends queued events until ctx is done.
func (w *Webhook) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-w.queue:
			w.deliver(ctx, it)
		}
	}
}

func (w *Webhook) deliver(ctx context.Context, it hookItem) {
	spanName := "event.webhook"
	if it.activityID != 0 {
		spanName = "activity.webhook"
	}
	ctx, span := w.tracer.Start(ctx, spanName)
	defer span.End()
	if it.activityID != 0 {
		span.SetAttributes(attribute.Int64("activity_id", it.activityID))
	} else {
		span.SetAttributes(attribute.String("event", it.event))
		if it.proposalID != "" {
			span.SetAttributes(attribute.String("proposal_id", it.proposalID))
		}
	}
	outcome, status, errClass := retry(ctx, w.sleep, func(ctx context.Context) (int, string) { return w.send(ctx, it.body) })
	if outcome == "" {
		return
	}
	w.count(ctx, outcome)
	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome == "sent" {
		return
	}
	span.SetStatus(codes.Error, outcome)
	if errClass == "" {
		errClass = "none"
	}
	w.log.Warn("webhook event failed", it.logAttrs("outcome", outcome,
		"status_class", statusClass(status), "error_class", errClass)...)
}

// send makes one attempt, as Ntfy.send does.
func (w *Webhook) send(ctx context.Context, body []byte) (int, string) {
	actx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Sprintf("%T", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "coachd")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, fmt.Sprintf("%T", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, ""
}
