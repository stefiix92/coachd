package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/stefiix92/coachd/internal/fitdata"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
)

const (
	fitBatch = 10
	// maxFITFailureStreak stops a drain after this many consecutive failed
	// downloads, so an outage (e.g. Garmin 5xx) does not use up every pending
	// activity's limited attempts.
	maxFITFailureStreak = 2
	// detailFillBatch bounds how many stored FITs without a detail row one
	// drain decodes, so a large pre-existing archive fills over several drains.
	detailFillBatch = 20
)

// signalFIT wakes the FIT worker without blocking.
func (s *Service) signalFIT() {
	select {
	case s.fitSignal <- struct{}{}:
	default:
	}
}

// fitWorker drains pending FIT downloads each time it is signalled. It never
// holds syncMu, so downloads do not delay syncs.
func (s *Service) fitWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.fitSignal:
			s.drainFIT(ctx)
		}
	}
}

// drainFIT downloads FIT files for activities that need one, each from its
// own source and at most once per drain, in one "ingest.fit.drain" span. It
// stops when nothing new is left, when every source is stopped (paused by a
// 429, key rejected, unreachable, maxFITFailureStreak consecutive failures)
// or when ctx ends. Then, however downloads ended, it fills missing detail
// from local files. The span's outcome says why the first source stopped.
func (s *Service) drainFIT(ctx context.Context) {
	ctx, span := s.tracer.Start(ctx, "ingest.fit.drain")
	defer span.End()
	tried, err := s.drainFITBatches(ctx)
	s.fillMissingDetail(ctx)
	outcome := outcomeOf(err)
	if errors.Is(err, errFITFailureStreak) {
		outcome = "stopped"
	}
	span.SetAttributes(attribute.String("outcome", outcome), attribute.Int("count", tried))
	// A pause (429) or the failure-streak stop is expected back-pressure, not a span error.
	if err != nil && outcome != OutcomeRateLimited && outcome != OutcomeIntervalsRateLimited && outcome != "stopped" {
		failSpan(span, err)
	}
}

// storeDetail decodes a FIT file's laps, series and workout, downsamples the
// series and stores them. Failures are logged, returned and leave the FIT stored:
// fillMissingDetail retries it once per process.
func (s *Service) storeDetail(ctx context.Context, id int64, r io.Reader) error {
	a, err := fitdata.Decode(r)
	if err == nil {
		err = s.db.PutActivityDetail(ctx, id, store.NewActivityDetail(a))
	}
	if err != nil {
		s.log.WarnContext(ctx, "fit detail failed", "data_type", "fit", "activity_id", id, "error_class", errorClass(err))
	}
	return err
}

// fillMissingDetail computes detail for up to detailFillBatch stored FITs that
// have none (stored before detail existed, or a decode that failed). It reads
// local files only and makes no Garmin call, so it neither paces nor stops on
// a Garmin pause; only ctx stops it. An activity whose fill fails (file
// missing or undecodable) is skipped for the life of the process.
func (s *Service) fillMissingDetail(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.fillMu.Lock()
	skip := len(s.fillFailed)
	s.fillMu.Unlock()
	// Ask for enough rows to get past those that already failed.
	batch, err := s.db.StoredFITsWithoutDetail(ctx, detailFillBatch+skip)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "fit detail query failed", "data_type", "fit", "error_class", errorClass(err))
		}
		return
	}
	done := 0
	for _, a := range batch {
		if done == detailFillBatch || ctx.Err() != nil {
			return
		}
		if s.fillFailedBefore(a.ID) {
			continue
		}
		done++
		if err := s.fillDetail(ctx, a); err != nil && ctx.Err() == nil {
			s.noteFillFailed(a.ID)
		}
	}
}

// fillDetail stores the detail decoded from a's stored FIT file.
func (s *Service) fillDetail(ctx context.Context, a store.Activity) error {
	f, err := os.Open(filepath.Join(s.opts.DataDir, filepath.FromSlash(a.FITPath)))
	if err != nil {
		s.log.WarnContext(ctx, "fit detail failed", "data_type", "fit", "activity_id", a.ID, "error_class", errorClass(err))
		return err
	}
	defer f.Close()
	return s.storeDetail(ctx, a.ID, f)
}

func (s *Service) fillFailedBefore(id int64) bool {
	s.fillMu.Lock()
	defer s.fillMu.Unlock()
	return s.fillFailed[id]
}

func (s *Service) noteFillFailed(id int64) {
	s.fillMu.Lock()
	defer s.fillMu.Unlock()
	s.fillFailed[id] = true
}

// fitSources are the sources the FIT worker downloads from, in one queue.
var fitSources = []store.Source{store.SourceGarmin, store.SourceIntervals}

// drainFITBatches does drainFIT's work. A source that is paused, rejects its
// key, is unreachable or fails maxFITFailureStreak downloads in a row is
// stopped for the rest of the drain: its activities are skipped, untouched,
// while the other source keeps draining. intervals.icu activities are skipped
// the same way while that source is off, without counting as a stop. It
// returns how many downloads it attempted and why the first source stopped
// (nil when none did), ending early once every source is stopped.
func (s *Service) drainFITBatches(ctx context.Context) (int, error) {
	tried := map[int64]bool{}
	skipped := map[int64]bool{} // activities of a stopped source
	stopped := map[store.Source]bool{}
	streak := map[store.Source]int{}
	var first error
	halt := func(src store.Source, err error) {
		stopped[src] = true
		if first == nil && !errors.Is(err, errIntervalsOff) {
			first = err
		}
	}
	// open reports whether src may still be called, halting it if it is
	// paused or off.
	open := func(src store.Source) bool {
		if !stopped[src] {
			if err := s.checkFITSource(src); err != nil {
				halt(src, err)
			}
		}
		return !stopped[src]
	}
	for {
		if !slices.ContainsFunc(fitSources, open) {
			return len(tried), first
		}
		// Ask for enough rows to get past those already tried or skipped in this drain.
		batch, err := s.db.ActivitiesNeedingFIT(ctx, fitBatch+len(tried)+len(skipped))
		if err != nil {
			if ctx.Err() == nil {
				s.log.ErrorContext(ctx, "fit query failed", "data_type", "fit", "error_class", errorClass(err), "error", err)
			}
			return len(tried), err
		}
		progressed := false
		for _, a := range batch {
			if tried[a.ID] || skipped[a.ID] {
				continue
			}
			progressed = true
			if !open(a.Source) {
				skipped[a.ID] = true
				continue
			}
			if len(tried) > 0 {
				if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
					return len(tried), err
				}
			}
			tried[a.ID] = true
			status, stop := s.fetchFIT(ctx, a)
			if stop != nil {
				if ctx.Err() != nil {
					return len(tried), stop
				}
				halt(a.Source, stop)
				continue
			}
			switch status {
			case store.FITStored:
				streak[a.Source] = 0
			case store.FITFailed:
				if streak[a.Source]++; streak[a.Source] >= maxFITFailureStreak {
					s.log.WarnContext(ctx, "fit drain stopped after consecutive failures", "data_type", "fit", "count", streak[a.Source])
					halt(a.Source, errFITFailureStreak)
				}
			}
		}
		if !progressed {
			return len(tried), first
		}
	}
}

var (
	// errFITFailureStreak stops a source after maxFITFailureStreak failed downloads.
	errFITFailureStreak = errors.New("ingest: fit: consecutive download failures")
	// errIntervalsOff marks an intervals.icu activity, left from an earlier
	// configuration, while the intervals.icu source is off.
	errIntervalsOff = errors.New("ingest: fit: intervals.icu source is off")
)

// checkFITSource returns why src may not be called now: its pause, or
// errIntervalsOff for intervals.icu when it is not configured.
func (s *Service) checkFITSource(src store.Source) error {
	if src != store.SourceIntervals {
		return s.checkPause()
	}
	if s.opts.Intervals == nil {
		return errIntervalsOff
	}
	return s.checkIntervalsPause()
}

// fetchFIT downloads one activity's FIT file from its source, stores it and
// records its status, which it returns. A non-nil stop means draining that
// source (or, once ctx ends, everything) should stop; status is then empty if
// the activity's state was left untouched. If the activity row vanished
// during the download, the file just written is removed and the status is
// empty (not a failure) unless the download itself failed.
func (s *Service) fetchFIT(ctx context.Context, a store.Activity) (status store.FITStatus, stop error) {
	if err := s.checkFITSource(a.Source); err != nil {
		return "", err
	}
	var body []byte
	var err error
	if a.Source == store.SourceIntervals {
		body, status, err = s.downloadIntervalsFIT(ctx, a.SourceID)
	} else {
		body, status, err = s.downloadGarminFIT(ctx, a.ID)
	}
	if status == "" {
		// 429, rejected key, transport failure or cancellation: not the
		// activity's fault, so leave its state untouched and stop draining
		// its source.
		s.logFIT(ctx, a.ID, outcomeOf(err), err)
		return "", err
	}
	rel := ""
	if status == store.FITStored {
		if rel, err = s.writeFIT(a, body); err != nil {
			status = store.FITFailed
		}
	}
	serr := s.db.SetFIT(ctx, a.ID, rel, status)
	if errors.Is(serr, store.ErrNotFound) {
		s.removeOrphanFIT(ctx, a.ID, rel)
		s.log.WarnContext(ctx, "fit download finished", "data_type", "fit", "activity_id", a.ID,
			"outcome", "skipped", "reason", "activity_missing")
		if status != store.FITFailed {
			status = ""
		}
		return status, ctx.Err()
	}
	if serr != nil {
		s.logFIT(ctx, a.ID, OutcomeError, serr)
		return store.FITFailed, ctx.Err()
	}
	if status == store.FITStored {
		s.storeDetail(ctx, a.ID, bytes.NewReader(body))
	}
	s.logFIT(ctx, a.ID, string(status), err)
	// A hidden copy is skipped like a soft-deleted activity: Events hears of
	// the copy that is shown.
	if s.opts.Events != nil && (status == store.FITStored || status == store.FITUnavailable) &&
		a.HiddenBy == nil && s.now().Sub(a.StartUTC) <= recentActivity {
		s.opts.Events.ActivityStored(ctx, a)
	}
	return status, nil
}

// downloadGarminFIT downloads a Garmin activity's FIT file. A 404 is
// unavailable (e.g. manual or deleted: never retried); an empty body, a 5xx
// and other errors are failed. An empty status means draining Garmin should
// stop: a 429 (which pauses Garmin calls), a rejected key, a transport
// failure or ctx ending, as err says.
func (s *Service) downloadGarminFIT(ctx context.Context, id int64) ([]byte, store.FITStatus, error) {
	body, err := s.g.DownloadActivityFIT(ctx, id)
	var se *garmin.StatusError
	switch {
	case err == nil && len(body) > 0:
		return body, store.FITStored, nil
	case err == nil:
		return nil, store.FITFailed, errors.New("ingest: fit: empty body")
	case errors.Is(err, garmin.ErrNotFound):
		return nil, store.FITUnavailable, err
	}
	transport := errors.As(err, &se) && se.Status == 0
	if s.notePause(ctx, err) || errors.Is(err, garmin.ErrUnauthorized) || transport || ctx.Err() != nil {
		return nil, "", err
	}
	return nil, store.FITFailed, err
}

// downloadIntervalsFIT downloads an intervals.icu activity's original file,
// or, when that is no usable FIT (a 404, or a body that fails the FIT
// integrity check: a GPX, a TCX, an empty or damaged file), the FIT file
// intervals.icu generates from the activity's streams. When neither is a
// usable FIT the activity is unavailable and never retried. A 5xx, a damaged
// gzip or an oversized file is failed. An empty status means draining
// intervals.icu should stop: a 429 (which pauses intervals.icu calls), a
// rejected key, a transport failure or ctx ending, as err says.
func (s *Service) downloadIntervalsFIT(ctx context.Context, sourceID string) ([]byte, store.FITStatus, error) {
	body, status, err := s.intervalsFIT(ctx, s.opts.Intervals.DownloadFile, sourceID)
	if status == store.FITUnavailable {
		body, status, err = s.intervalsFIT(ctx, s.opts.Intervals.DownloadGeneratedFIT, sourceID)
	}
	return body, status, err
}

// intervalsFIT runs one download and classifies it as downloadIntervalsFIT says.
func (s *Service) intervalsFIT(ctx context.Context, dl func(context.Context, string) ([]byte, error), sourceID string) ([]byte, store.FITStatus, error) {
	body, err := dl(ctx, sourceID)
	switch {
	case err == nil:
		if err := fitdata.CheckIntegrity(body); err != nil {
			return nil, store.FITUnavailable, err
		}
		return body, store.FITStored, nil
	case errors.Is(err, intervals.ErrNotFound):
		return nil, store.FITUnavailable, err
	}
	err = s.intervalsErr(ctx, err)
	var rl *intervals.RateLimitedError
	var se *intervals.StatusError
	transport := errors.As(err, &se) && se.Status == 0
	if errors.As(err, &rl) || errors.Is(err, intervals.ErrUnauthorized) || transport || ctx.Err() != nil {
		return nil, "", err
	}
	return nil, store.FITFailed, err
}

// removeOrphanFIT removes a FIT file writeFIT just wrote (rel, relative to
// DataDir; empty when none) for an activity whose row no longer exists.
func (s *Service) removeOrphanFIT(ctx context.Context, id int64, rel string) {
	if rel == "" {
		return
	}
	if err := os.Remove(filepath.Join(s.opts.DataDir, filepath.FromSlash(rel))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.WarnContext(ctx, "fit file not removed", "data_type", "fit", "activity_id", id, "error_class", errorClass(err))
	}
}

func (s *Service) logFIT(ctx context.Context, id int64, outcome string, err error) {
	attrs := []any{"data_type", "fit", "activity_id", id, "outcome", outcome}
	if err != nil {
		s.log.WarnContext(ctx, "fit download finished", append(attrs, "error_class", errorClass(err), "error", err)...)
		return
	}
	s.log.InfoContext(ctx, "fit download finished", attrs...)
}

// writeFIT atomically writes body to DataDir/fit/<yyyy>/<name> (dir 0700,
// file 0600), named by fitFileName, and returns the path relative to DataDir.
func (s *Service) writeFIT(a store.Activity, body []byte) (string, error) {
	name, err := fitFileName(a)
	if err != nil {
		return "", err
	}
	rel := path.Join("fit", fmt.Sprintf("%04d", a.Date.Year), name)
	final := filepath.Join(s.opts.DataDir, filepath.FromSlash(rel))
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ingest: fit dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".fit-*.tmp") // created 0600
	if err != nil {
		return "", fmt.Errorf("ingest: fit temp: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("ingest: fit chmod: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return "", fmt.Errorf("ingest: fit write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("ingest: fit sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("ingest: fit close: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", fmt.Errorf("ingest: fit rename: %w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil { // best effort: persist the rename
		d.Sync()
		d.Close()
	}
	return rel, nil
}

// fitFileName names a's FIT file: <id>.fit for Garmin, <source_id>.fit for
// intervals.icu (i55751783.fit, so no name starts with a dash). The source ID
// becomes part of a path, so it must be the i<digits> form of a's ID.
func fitFileName(a store.Activity) (string, error) {
	if a.Source != store.SourceIntervals {
		return strconv.FormatInt(a.ID, 10) + ".fit", nil
	}
	if id, ok := IntervalsID(a.SourceID); !ok || id != a.ID {
		return "", errors.New("ingest: fit: invalid intervals.icu activity id")
	}
	return a.SourceID + ".fit", nil
}

// sleepCtx waits d (no wait when d <= 0) or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
