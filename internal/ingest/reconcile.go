package ingest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
)

const (
	// maxReconcileProbes bounds the missing-activity probes of one sync. The
	// canary probe is not counted.
	maxReconcileProbes = 10
	// existsMemo is how long an activity Garmin confirmed to exist is not
	// probed again.
	existsMemo = 24 * time.Hour
	// outcomeSkipped: a confirmed 404 was found but no listed activity could
	// verify the probe, so nothing was deleted.
	outcomeSkipped = "skipped"
)

var (
	// errProbeUnreliable ends a reconcile run whose canary, a listed activity,
	// was not confirmed to exist: the probe's 404s cannot be trusted.
	errProbeUnreliable = errors.New("ingest: reconcile: activity probe unreliable")
	// errNoCanary ends a reconcile run that found a 404 while no stored
	// activity in the window was listed to verify the probe with.
	errNoCanary = errors.New("ingest: reconcile: no listed activity to verify the probe")
)

// reconcile mirrors activities deleted in Garmin. It runs only after a
// complete incremental activities fetch of [from, to], in an "ingest.reconcile"
// span, and logs one summary line. It never changes the activities sync outcome.
func (s *Service) reconcile(ctx context.Context, from, to civil.Date, listed map[int64]bool) {
	ctx, span := s.tracer.Start(ctx, "ingest.reconcile", trace.WithAttributes(attribute.String("data_type", TypeActivities)))
	defer span.End()
	deleted, err := s.reconcileActivities(ctx, from, to, listed)
	attrs := []any{"data_type", TypeActivities, "count", deleted}
	switch {
	case err == nil:
		span.SetAttributes(attribute.String("outcome", OutcomeOK), attribute.Int("count", deleted))
		s.log.InfoContext(ctx, "reconcile finished", append(attrs, "outcome", OutcomeOK)...)
	case errors.Is(err, errNoCanary):
		span.SetAttributes(attribute.String("outcome", outcomeSkipped), attribute.Int("count", deleted))
		s.log.InfoContext(ctx, "reconcile finished", append(attrs, "outcome", outcomeSkipped, "reason", "no_listed_activity")...)
	default:
		outcome := outcomeOf(err)
		span.SetAttributes(attribute.String("outcome", outcome), attribute.Int("count", deleted))
		failSpan(span, err)
		s.log.WarnContext(ctx, "reconcile finished",
			append(attrs, "outcome", outcome, "error_class", errorClass(err), "error", err)...)
	}
}

// reconcileActivities looks at activities stored with a date in
// [max(from, to−119), to] (inside the post-sync summaries recompute) that
// Garmin did not list, oldest first. It probes up to maxReconcileProbes of
// them, skipping any Garmin confirmed to exist in the last 24 h; each probe is
// paced by BackfillDelay after checking the pause. Deletion is a soft delete:
//   - An existing activity is kept and remembered.
//   - Any probe error keeps the activity and ends the run (a 429 also notes
//     the pause), leaving the rest for the next sync.
//   - A confirmed not-found (false, nil) marks the activity deleted (hidden
//     from every read; its row, raw JSON, detail and FIT file are kept, and
//     Garmin listing it again restores it) and writes an "activity_deleted"
//     audit row, but only once a canary, the newest listed activity, has been
//     confirmed to exist in this run. A canary that is not confirmed ends the
//     run with errProbeUnreliable, and no listed activity ends it with
//     errNoCanary, both before any deletion.
//
// It returns how many activities it marked deleted.
func (s *Service) reconcileActivities(ctx context.Context, from, to civil.Date, listed map[int64]bool) (int, error) {
	if floor := to.AddDays(-(summaryDays - 1)); from.Before(floor) {
		from = floor
	}
	stored, err := s.db.ActivityIDsInRange(ctx, store.SourceGarmin, from, to)
	if err != nil {
		return 0, err
	}
	now := s.now()
	s.pruneExists(now)
	var canary int64 // newest listed activity in the window: stored is oldest first
	for _, id := range stored {
		if listed[id] {
			canary = id
		}
	}
	verified := false
	deleted, probes := 0, 0
	for _, id := range stored {
		if listed[id] || s.knownToExist(id, now) {
			continue
		}
		if probes == maxReconcileProbes {
			break
		}
		probes++
		exists, err := s.probe(ctx, id)
		if err != nil {
			return deleted, err
		}
		if exists {
			s.noteExists(id, now)
			continue
		}
		if !verified {
			if err := s.verifyProbe(ctx, canary); err != nil {
				return deleted, err
			}
			verified = true
		}
		_, ok, err := s.softDeleteActivity(ctx, TypeActivities, id)
		if err != nil {
			return deleted, err
		}
		if ok {
			deleted++
		}
	}
	return deleted, nil
}

// softDeleteActivity soft-deletes live activity id, confirmed gone at its
// source: it marks it deleted, logs "reconcile deleted activity" under data
// type dt and writes an "activity_deleted" audit row (a failed audit write is
// logged, not returned). ok is false, with a nil error, when the activity is
// no longer live (already gone or soft-deleted); date is its date when ok.
func (s *Service) softDeleteActivity(ctx context.Context, dt string, id int64) (date civil.Date, ok bool, err error) {
	a, err := s.db.GetActivity(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return civil.Date{}, false, nil
	}
	if err != nil {
		return civil.Date{}, false, err
	}
	at := s.now()
	err = s.db.MarkActivityDeleted(ctx, id, at)
	if errors.Is(err, store.ErrNotFound) {
		return civil.Date{}, false, nil
	}
	if err != nil {
		return civil.Date{}, false, err
	}
	s.log.InfoContext(ctx, "reconcile deleted activity", "data_type", dt, "activity_id", id,
		"calendar_date", a.Date.String(), "outcome", "deleted")
	if err := s.db.AppendAudit(ctx, store.AuditEvent{At: at, Lane: "ingest", Actor: "ingest",
		Event: "activity_deleted", Target: strconv.FormatInt(id, 10), Outcome: OutcomeOK}); err != nil {
		s.log.WarnContext(ctx, "audit write failed", "data_type", dt, "activity_id", id,
			"error_class", errorClass(err), "error", err)
	}
	return a.Date, true, nil
}

// probe waits BackfillDelay, checks the pause, then asks Garmin whether id
// exists. A 429 notes the pause.
func (s *Service) probe(ctx context.Context, id int64) (bool, error) {
	if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
		return false, err
	}
	if err := s.checkPause(); err != nil {
		return false, err
	}
	exists, err := s.g.ActivityExists(ctx, id)
	if err != nil {
		s.notePause(ctx, err)
		return false, err
	}
	return exists, nil
}

// verifyProbe confirms that the probe still reports an activity Garmin just
// listed (canary, 0 when none) as existing, so its 404s can be trusted (e.g.
// not an endpoint that now answers 404 to everything).
func (s *Service) verifyProbe(ctx context.Context, canary int64) error {
	if canary == 0 {
		return errNoCanary
	}
	exists, err := s.probe(ctx, canary)
	if err != nil {
		// %v, not %w: the run's outcome is an unreliable probe, not e.g. a rate limit.
		return fmt.Errorf("%w: canary: %v", errProbeUnreliable, err)
	}
	if !exists {
		return fmt.Errorf("%w: canary reported deleted", errProbeUnreliable)
	}
	return nil
}

// knownToExist reports whether Garmin confirmed id exists less than
// existsMemo before now.
func (s *Service) knownToExist(id int64, now time.Time) bool {
	s.existsMu.Lock()
	defer s.existsMu.Unlock()
	at, ok := s.existsAt[id]
	return ok && now.Sub(at) < existsMemo
}

// noteExists remembers that Garmin confirmed id exists at now.
func (s *Service) noteExists(id int64, now time.Time) {
	s.existsMu.Lock()
	defer s.existsMu.Unlock()
	s.existsAt[id] = now
}

// pruneExists forgets confirmations at least existsMemo old.
func (s *Service) pruneExists(now time.Time) {
	s.existsMu.Lock()
	defer s.existsMu.Unlock()
	for id, at := range s.existsAt {
		if now.Sub(at) >= existsMemo {
			delete(s.existsAt, id)
		}
	}
}
