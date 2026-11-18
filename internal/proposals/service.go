// Package proposals is the lifecycle service for agent-proposed workouts:
// validate, warn, store, cancel, list and expire. It has no path to
// approved, delivered or withdrawn: those transitions belong to the runner's
// passkey-protected page, never to an agent-reachable service.
package proposals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/guidance"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/workout"
	"github.com/stefiix92/coachd/internal/zwiftworkout"
)

const maxRationaleRunes = 2000

// ErrNotPending means the proposal exists but is no longer pending. It wraps
// store.ErrConflict.
var ErrNotPending = fmt.Errorf("proposal is not pending: %w", store.ErrConflict)

// NotPendingError is returned by Cancel; it matches ErrNotPending and store.ErrConflict.
type NotPendingError struct {
	ID     string
	Status store.ProposalStatus
}

func (e *NotPendingError) Error() string {
	return fmt.Sprintf("proposal %s is %s; only pending proposals can be cancelled", e.ID, e.Status)
}

func (e *NotPendingError) Is(target error) bool {
	return target == ErrNotPending || target == store.ErrConflict
}

// Service holds the proposal lifecycle operations.
type Service struct {
	db       *store.DB
	tz       *time.Location
	now      func() time.Time
	limits   workout.Limits
	log      *slog.Logger
	metrics  *telemetry.Metrics
	notifier Notifier
}

// Notifier is told about each new pending proposal. It must not block.
type Notifier interface {
	ProposalPending(id string, date civil.Date, link trace.SpanContext)
}

// Deps are the Service's collaborators. Metrics and Notifier may be nil.
type Deps struct {
	DB       *store.DB
	TZ       *time.Location
	Now      func() time.Time
	Limits   workout.Limits
	Log      *slog.Logger
	Metrics  *telemetry.Metrics
	Notifier Notifier
}

func New(d Deps) *Service {
	return &Service{db: d.DB, tz: d.TZ, now: d.Now, limits: d.Limits, log: d.Log, metrics: d.Metrics, notifier: d.Notifier}
}

// Actor identifies who performs an operation (from auth.Identity).
type Actor struct{ Lane, Subject, Client string }

// ProposeResult is the outcome of Propose. Verdict is "pending" or "invalid".
type ProposeResult struct {
	ID       string
	Verdict  string
	Reason   string
	Warnings []guidance.Warning
}

func invalid(format string, a ...any) ProposeResult {
	return ProposeResult{Verdict: "invalid", Reason: fmt.Sprintf(format, a...)}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("proposals: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func checkReplaces(newID, oldID string, st store.ProposalStatus) error {
	if oldID == newID {
		return errors.New("replaces: a proposal cannot replace itself")
	}
	if st == store.ProposalPending || slices.Contains(store.DesiredStatuses, st) {
		return nil
	}
	return fmt.Errorf("replaces: proposal is %s; only pending, approved, delivering, delivery_failed or delivered proposals can be replaced", st)
}

// Propose validates w, attaches warnings and stores a pending proposal. An
// invalid workout, rationale or replaces reference stores nothing and returns
// Verdict "invalid" with a reason; the error is for infrastructure failures.
func (s *Service) Propose(ctx context.Context, w workout.Workout, rationale string, replaces *string, a Actor) (ProposeResult, error) {
	now := s.now()
	today := civil.Of(now, s.tz)
	plan, err := workout.Validate(w, today, s.limits)
	if err != nil {
		var inv *workout.Invalid
		if errors.As(err, &inv) {
			return invalid("%s", inv.Reason), nil
		}
		return ProposeResult{}, err
	}
	if err := workout.CheckText("rationale", rationale, maxRationaleRunes); err != nil {
		var inv *workout.Invalid
		if errors.As(err, &inv) {
			return invalid("%s", inv.Reason), nil
		}
		return ProposeResult{}, err
	}
	id, err := newID()
	if err != nil {
		return ProposeResult{}, err
	}
	if replaces != nil {
		old, err := s.db.GetProposal(ctx, *replaces)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("replaces: no proposal with that id"), nil
		}
		if err != nil {
			return ProposeResult{}, err
		}
		if err := checkReplaces(id, old.ID, old.Status); err != nil {
			return invalid("%s", err.Error()), nil
		}
	}
	canon, err := workout.CanonicalJSON(w)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("proposals: canonical json: %w", err)
	}
	in, err := s.warningInputs(ctx, plan, replaces, now, today)
	if err != nil {
		return ProposeResult{}, err
	}
	if plan.Sport == store.SportCycling {
		in.ZwiftUnsupported = zwiftworkout.Unsupported(w)
	}
	warnings := guidance.Warnings(in)
	if warnings == nil {
		warnings = []guidance.Warning{}
	}
	wj, err := json.Marshal(warnings)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("proposals: warnings json: %w", err)
	}
	p := store.Proposal{
		ID: id, Date: plan.Date, Sport: plan.Sport, Name: w.Name, ContentJSON: canon, ContentHash: workout.ContentHash(canon),
		Rationale: rationale, Replaces: replaces, WarningsJSON: wj, Status: store.ProposalPending, StatusAt: now, CreatedAt: now,
		CreatedLane: a.Lane, CreatedBy: a.Subject, CreatedClient: a.Client,
	}
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		p.TraceID, p.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	ev := store.AuditEvent{At: now, Lane: a.Lane, Actor: a.Subject, Client: a.Client, Event: "proposal_pending", Target: id, Outcome: "ok"}
	if err := s.db.InsertProposal(ctx, p, ev); err != nil {
		return ProposeResult{}, err
	}
	if s.metrics != nil {
		for _, w := range warnings {
			s.metrics.ProposalWarnings.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", w.Code)))
		}
	}
	if s.notifier != nil {
		s.notifier.ProposalPending(id, plan.Date, sc)
	}
	return ProposeResult{ID: id, Verdict: "pending", Warnings: warnings}, nil
}

func (s *Service) warningInputs(ctx context.Context, plan workout.Plan, replaces *string, now time.Time, today civil.Date) (guidance.WarningInputs, error) {
	in := guidance.WarningInputs{Plan: plan, Now: now}
	runs, err := s.db.Runs(ctx, today.AddDays(-29), today)
	if err != nil {
		return in, err
	}
	for _, r := range runs {
		in.LongestRun30dM = math.Max(in.LongestRun30dM, r.DistanceM)
		in.LongestRun30dS = math.Max(in.LongestRun30dS, r.MovingS)
	}
	hr, err := s.db.MaxHR(ctx, today.AddDays(-89), today)
	if err != nil {
		return in, err
	}
	if hr != nil {
		v := int(math.Floor(*hr))
		in.MaxHR90d = &v
	}
	approved, err := s.db.ListProposals(ctx, plan.Date, plan.Date, store.DesiredStatuses)
	if err != nil {
		return in, err
	}
	for _, p := range approved {
		if replaces == nil || p.ID != *replaces {
			in.SameDay++
		}
	}
	states, err := s.db.ListSyncStates(ctx)
	if err != nil {
		return in, err
	}
	for _, st := range states { // Garmin's only: intervals.icu must not hide a Garmin outage
		if st.FromGarmin() && st.LastSuccessAt != nil && (in.LastSyncAt == nil || st.LastSuccessAt.After(*in.LastSyncAt)) {
			t := *st.LastSuccessAt
			in.LastSyncAt = &t
		}
	}
	cov, err := s.db.Coverage(ctx, today)
	if err != nil {
		return in, err
	}
	in.WeeksOfHistory = cov.WeeksOfHistory
	return in, nil
}

// Cancel moves a pending proposal to cancelled. A proposal in any other
// status yields an error wrapping ErrNotPending (and store.ErrConflict).
func (s *Service) Cancel(ctx context.Context, id string, a Actor) (store.Proposal, error) {
	p, err := s.db.TransitionProposal(ctx, id, store.Transition{
		From: []store.ProposalStatus{store.ProposalPending}, To: store.ProposalCancelled,
		At: s.now(), Lane: a.Lane, Actor: a.Subject, Client: a.Client,
	})
	if errors.Is(err, store.ErrConflict) {
		if cur, gerr := s.db.GetProposal(ctx, id); gerr == nil {
			return store.Proposal{}, &NotPendingError{ID: id, Status: cur.Status}
		}
		return store.Proposal{}, ErrNotPending
	}
	return p, err
}

func (s *Service) List(ctx context.Context, from, to civil.Date, statuses []store.ProposalStatus) ([]store.Proposal, error) {
	return s.db.ListProposals(ctx, from, to, statuses)
}

// staleDelivery is how long a fetched, unacknowledged delivery waits before it
// is marked failed.
const staleDelivery = 15 * time.Minute

// ExpireDue expires pending, approved, delivering and delivery_failed proposals
// dated before today in the runner's timezone and returns how many moved. It
// also fails stale deliveries and counts them separately.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.now()
	n, err := s.db.ExpireProposals(ctx, civil.Of(now, s.tz), now)
	if err != nil {
		return n, err
	}
	failed, err := s.db.FailStaleDeliveries(ctx, now.Add(-staleDelivery), now)
	if err != nil {
		return n, err
	}
	if failed > 0 {
		if s.metrics != nil {
			s.metrics.Deliveries.Add(ctx, int64(failed), metric.WithAttributes(attribute.String("outcome", "failed")))
		}
		s.log.Warn("deliveries failed", "component", "proposals", "count", failed, "outcome", "failed")
	}
	return n, nil
}

// expireTick runs ExpireDue once and logs the outcome. ExpireDue reports
// expired proposals only once their transition is committed, so they are
// logged even when the later stale-delivery step fails (for example on a
// context cancelled at shutdown).
func (s *Service) expireTick(ctx context.Context) {
	n, err := s.ExpireDue(ctx)
	if n > 0 {
		s.log.Info("proposals expired", "component", "proposals", "count", n, "outcome", "ok")
	}
	if err != nil && ctx.Err() == nil {
		s.log.Warn("proposal expiry failed", "component", "proposals", "outcome", "error", "error_class", fmt.Sprintf("%T", err))
	}
}

// RunExpiry runs ExpireDue at start and every `every` until ctx is done. It
// first adds 0 to Deliveries{outcome=failed} so the series exists before the
// first failure (an increase() alert cannot see a series that starts at 1).
func (s *Service) RunExpiry(ctx context.Context, every time.Duration) {
	if s.metrics != nil {
		s.metrics.Deliveries.Add(ctx, 0, metric.WithAttributes(attribute.String("outcome", "failed")))
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.expireTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
