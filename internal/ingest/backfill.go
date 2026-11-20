package ingest

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/summaries"
)

const (
	backfillRecentGap = 3 // days left to the incremental sync window
	// maxBackfillRateLimits bounds consecutive 429 retries of one chunk, so a
	// Retry-After in the past cannot turn into a retry storm.
	maxBackfillRateLimits = 5
)

// backfillChunkDays is the largest range requested at once, per data type
// (within Garmin's per-request caps).
var backfillChunkDays = map[string]int{
	TypeActivities: 90,
	TypeSleep:      28,
	TypeHRV:        28,
	TypeDaily:      28,
	TypeWeight:     90,
}

// Backfill walks each unfinished data type backwards from its saved cursor
// (or today−3) to today−BackfillDays in range-capped chunks, saving the cursor
// after every chunk, then recomputes summaries once every type is done. It
// returns nil at once if another Backfill is running. A 429 waits for the
// shared pause and retries the chunk. ctx ending, a 401 and repeated 429s end
// the run; any other error ends only that type, and the run continues with
// the next one. The cursor keeps progress for the next call; the returned
// error joins the failures.
func (s *Service) Backfill(ctx context.Context) error {
	if !s.backfillMu.TryLock() {
		return nil
	}
	defer s.backfillMu.Unlock()

	today := civil.Of(s.now(), s.opts.TZ)
	limit := today.AddDays(-s.opts.BackfillDays)
	var errs []error
	for _, dt := range dataTypes {
		err := s.backfillType(ctx, dt, today, limit)
		if err == nil {
			continue
		}
		errs = append(errs, err)
		if endsBackfill(ctx, err) {
			return errors.Join(errs...)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := summaries.Recompute(ctx, s.db, today, s.opts.BackfillDays); err != nil {
		s.log.ErrorContext(ctx, "summaries recompute failed", "data_type", "summaries", "outcome", OutcomeError,
			"error_class", errorClass(err), "error", err)
		return err
	}
	return nil
}

// endsBackfill reports whether err stops the whole run rather than one type:
// ctx ended, the key was rejected, or Garmin is rate limiting.
func endsBackfill(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, garmin.ErrUnauthorized) || outcomeOf(err) == OutcomeRateLimited
}

func (s *Service) backfillType(ctx context.Context, dt string, today, limit civil.Date) error {
	st, err := s.db.GetSyncState(ctx, dt)
	if err != nil || st.BackfillDone {
		return err
	}
	end := today.AddDays(-backfillRecentGap)
	if st.Cursor != "" {
		c, err := civil.Parse(st.Cursor)
		if err != nil {
			return err
		}
		end = c.AddDays(-1)
	}
	chunk := backfillChunkDays[dt]
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if end.Before(limit) {
			return s.saveBackfill(ctx, dt, "", true)
		}
		start := end.AddDays(-(chunk - 1))
		if start.Before(limit) {
			start = limit
		}
		last := start == limit
		if err := s.backfillStep(ctx, dt, start, end, last); err != nil || last {
			return err
		}
		end = start.AddDays(-1)
	}
}

// backfillStep fetches, stores and checkpoints the chunk [start, end] in an
// "ingest.backfill.chunk" span. last marks the type's history done. With the
// intervals.icu source on, an activities chunk then has its duplicate pairs
// resolved, before the checkpoint; Backfill's closing recompute covers the
// dates whose marks changed.
func (s *Service) backfillStep(ctx context.Context, dt string, start, end civil.Date, last bool) error {
	ctx, span := s.tracer.Start(ctx, "ingest.backfill.chunk", trace.WithAttributes(
		attribute.String("data_type", dt), attribute.String("calendar_date", start.String())))
	defer span.End()
	f, attempts, err := s.backfillChunk(ctx, dt, start, end)
	if err == nil {
		s.logSkips(ctx, dt, f)
		s.log.InfoContext(ctx, "backfill chunk stored", "data_type", dt, "calendar_date", start.String(), "count", f.stored)
		if dt == TypeActivities && f.stored > 0 {
			s.signalFIT()
		}
		if dt == TypeActivities && s.opts.Intervals != nil {
			s.resolveDuplicatesStep(ctx, start, end)
		}
		err = s.saveBackfill(ctx, dt, start.String(), last)
	} else {
		s.log.WarnContext(ctx, "backfill stopped", "data_type", dt, "calendar_date", start.String(),
			"outcome", outcomeOf(err), "error_class", errorClass(err), "error", err)
	}
	span.SetAttributes(attribute.String("outcome", outcomeOf(err)), attribute.Int("count", f.stored),
		attribute.Int("attempt", attempts))
	if err != nil {
		failSpan(span, err)
	}
	return err
}

// backfillChunk fetches and stores [start, end], waiting out Garmin pauses and
// retrying after a 429 up to maxBackfillRateLimits times in a row. attempts
// counts the fetches made.
func (s *Service) backfillChunk(ctx context.Context, dt string, start, end civil.Date) (f fetched, attempts int, err error) {
	for {
		if err := s.waitPause(ctx); err != nil {
			return fetched{}, attempts, err
		}
		if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
			return fetched{}, attempts, err
		}
		attempts++
		if dt == TypeActivities {
			f, err = s.fetchActivitiesPaced(ctx, start, end, s.opts.BackfillDelay)
		} else {
			f, err = s.fetchRange(ctx, dt, start, end)
		}
		// The fetchers have already recorded a 429's pause (notePause).
		if outcomeOf(err) != OutcomeRateLimited || attempts > maxBackfillRateLimits {
			return f, attempts, err
		}
	}
}

// waitPause blocks while Garmin calls are paused, or until ctx ends.
func (s *Service) waitPause(ctx context.Context) error {
	for {
		until, paused := s.pausedUntil()
		if !paused {
			return ctx.Err()
		}
		if err := sleepCtx(ctx, until.Sub(s.now())); err != nil {
			return err
		}
	}
}

// saveBackfill sets dt's Cursor (when non-empty) and BackfillDone, keeping the
// rest of its sync state.
func (s *Service) saveBackfill(ctx context.Context, dt, cursor string, done bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st, err := s.db.GetSyncState(ctx, dt)
	if err != nil {
		return err
	}
	st.DataType = dt
	if cursor != "" {
		st.Cursor = cursor
	}
	st.BackfillDone = done
	return s.db.PutSyncState(ctx, st)
}

// backfillPending reports whether any data type's history is incomplete.
func (s *Service) backfillPending(ctx context.Context) bool {
	sts, err := s.db.ListSyncStates(ctx)
	if err != nil {
		return true
	}
	done := map[string]bool{}
	for _, st := range sts {
		done[st.DataType] = st.BackfillDone
	}
	for _, dt := range dataTypes {
		if !done[dt] {
			return true
		}
	}
	return false
}

// launchBackfill starts Backfill in the background if history is incomplete.
// Backfill's own lock stops overlapping runs; failures are logged, not fatal.
func (s *Service) launchBackfill(ctx context.Context, wg *sync.WaitGroup) {
	if ctx.Err() != nil || !s.backfillPending(ctx) {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Backfill(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "backfill failed", "outcome", outcomeOf(err), "error_class", errorClass(err), "error", err)
		}
	}()
}
