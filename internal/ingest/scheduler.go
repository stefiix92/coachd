package ingest

import (
	"context"
	"errors"
	"sync"
	"time"
)

// requestSyncTimeout bounds an agent-requested sync.
const requestSyncTimeout = 45 * time.Second

// RequestSync outcomes.
const (
	RequestSynced            = "synced"
	RequestRateLimited       = "rate_limited"
	RequestAlreadyRunning    = "already_running"
	RequestGarminRateLimited = "garmin_rate_limited"
	RequestAuthFailed        = "auth_failed"
	RequestFailed            = "failed"
)

// RequestResult answers an agent's sync request.
type RequestResult struct {
	Outcome       string
	LastSyncAt    *time.Time // latest successful sync of any data type
	NextAllowedAt time.Time
}

// Run performs one sync immediately, then one every PollInterval, and runs the
// FIT worker and, while any data type's history is incomplete, the backfill. It blocks until ctx ends and its goroutines have stopped.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.fitWorker(ctx)
	}()
	defer wg.Wait()

	s.launchBackfill(ctx, &wg)
	s.scheduledSync(ctx)
	t := time.NewTicker(s.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scheduledSync(ctx)
			s.launchBackfill(ctx, &wg)
		}
	}
}

// scheduledSync runs a sync, ignoring ErrSyncRunning (another path is syncing).
func (s *Service) scheduledSync(ctx context.Context) {
	if _, err := s.Sync(ctx); err != nil && !errors.Is(err, ErrSyncRunning) && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "scheduled sync failed", "error_class", errorClass(err), "error", err)
	}
}

// RequestSync runs an agent-requested sync, at most once per SyncMinInterval.
// A request refused because a sync is running or Garmin is paused does not
// use up the interval.
func (s *Service) RequestSync(ctx context.Context) RequestResult {
	now := s.now()
	s.reqMu.Lock()
	if next := s.lastRequest.Add(s.opts.SyncMinInterval); !s.lastRequest.IsZero() && now.Before(next) {
		s.reqMu.Unlock()
		return s.requestResult(ctx, RequestRateLimited, next)
	}
	if until, paused := s.pausedUntil(); paused {
		s.reqMu.Unlock()
		return s.requestResult(ctx, RequestGarminRateLimited, until)
	}
	// Reserve the slot so a concurrent request cannot start a second sync.
	prev := s.lastRequest
	s.lastRequest = now
	s.reqMu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, requestSyncTimeout)
	defer cancel()
	res, err := s.Sync(sctx)
	if errors.Is(err, ErrSyncRunning) {
		s.reqMu.Lock()
		if s.lastRequest.Equal(now) {
			s.lastRequest = prev
		}
		s.reqMu.Unlock()
		return s.requestResult(ctx, RequestAlreadyRunning, now)
	}
	outcome := requestOutcome(res)
	if err != nil {
		outcome = RequestFailed
	}
	next := now.Add(s.opts.SyncMinInterval)
	if until, paused := s.pausedUntil(); paused && until.After(next) {
		next = until
	}
	return s.requestResult(ctx, outcome, next)
}

// requestOutcome folds per-type outcomes with precedence
// garmin_rate_limited > auth_failed > failed > synced. An intervals.icu 429
// is failed: it is not Garmin's, and the request did not sync everything.
func requestOutcome(res SyncResult) string {
	has := func(o string) bool {
		for _, v := range res.Outcomes {
			if v == o {
				return true
			}
		}
		return false
	}
	switch {
	case has(OutcomeRateLimited):
		return RequestGarminRateLimited
	case has(OutcomeAuthFailed):
		return RequestAuthFailed
	case has(OutcomeError), has(OutcomeIntervalsRateLimited):
		return RequestFailed
	}
	return RequestSynced
}

func (s *Service) requestResult(ctx context.Context, outcome string, next time.Time) RequestResult {
	return RequestResult{Outcome: outcome, LastSyncAt: s.lastSyncAt(ctx), NextAllowedAt: next}
}

// lastSyncAt returns the latest LastSuccessAt across all data types, or nil.
func (s *Service) lastSyncAt(ctx context.Context) *time.Time {
	states, err := s.db.ListSyncStates(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "sync state read failed", "error_class", errorClass(err), "error", err)
		return nil
	}
	var last *time.Time
	for _, st := range states {
		if st.LastSuccessAt != nil && (last == nil || st.LastSuccessAt.After(*last)) {
			last = st.LastSuccessAt
		}
	}
	return last
}
