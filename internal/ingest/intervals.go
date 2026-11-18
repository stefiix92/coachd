package ingest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
)

// intervalsLocalLayout is the format of start_date_local, which has no zone.
const intervalsLocalLayout = "2006-01-02T15:04:05"

// maxIntervalsProbes bounds the deletion probes of one intervals.icu sync.
const maxIntervalsProbes = 10

// intervalsListDays is the most days one intervals.icu list call covers, so
// a first window of BackfillDays stays well inside the client's body cap and
// timeout.
const intervalsListDays = 90

var (
	// errNotImported marks a list entry that the import rules exclude: an ID
	// not of the form i<digits> (a Strava stub), a STRAVA or GARMIN_CONNECT
	// source, or no start time or no positive elapsed time.
	errNotImported = errors.New("ingest: intervals activity: not imported")
	// errIntervalsPaused marks an intervals.icu call skipped because of an
	// earlier intervals.icu 429.
	errIntervalsPaused = errors.New("ingest: intervals.icu paused after rate limit")
)

var sportByIntervalsType = map[string]store.Sport{
	"Ride": store.SportCycling, "VirtualRide": store.SportCycling, "GravelRide": store.SportCycling,
	"MountainBikeRide": store.SportCycling, "EBikeRide": store.SportCycling,
	"Run": store.SportRunning, "VirtualRun": store.SportRunning,
	"TrailRun":       store.SportTrailRunning,
	"Walk":           store.SportWalking,
	"Hike":           store.SportHiking,
	"WeightTraining": store.SportStrength,
}

// IntervalsID maps "i55751783" to -55751783; ok is false for any other form.
// The digits must not start with 0, so the mapping is one to one.
func IntervalsID(sourceID string) (id int64, ok bool) {
	digits, found := strings.CutPrefix(sourceID, "i")
	if !found || digits == "" || digits[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return -n, true
}

// intervalsSourceID is the intervals.icu ID of the activity stored as id.
func intervalsSourceID(id int64) string {
	return "i" + strconv.FormatInt(-id, 10)
}

// MapIntervalsActivity converts a list entry. Units stay metres and seconds;
// DurationS is the elapsed time. Date is the wall-clock date of
// start_date_local in tz. Garmin's derived values stay empty. It returns
// errNotImported for an entry the import rules exclude, and another error
// when a start time is there but cannot be read.
func MapIntervalsActivity(a intervals.Activity, tz *time.Location) (store.Activity, error) {
	id, ok := IntervalsID(a.ID)
	if !ok || a.Source == "STRAVA" || a.Source == "GARMIN_CONNECT" ||
		a.StartDate == "" || a.StartDateLocal == "" || deref(a.ElapsedTime) <= 0 {
		return store.Activity{}, errNotImported
	}
	local, err := time.ParseInLocation(intervalsLocalLayout, a.StartDateLocal, tz)
	if err != nil {
		return store.Activity{}, errors.New("ingest: intervals activity: invalid start_date_local")
	}
	utc, err := time.Parse(time.RFC3339, a.StartDate)
	if err != nil {
		return store.Activity{}, errors.New("ingest: intervals activity: invalid start_date")
	}
	sport, ok := sportByIntervalsType[a.Type]
	if !ok {
		sport = store.SportOther
	}
	fit := store.FITUnavailable
	if a.FileType == "fit" {
		fit = store.FITPending
	}
	date := civil.Of(local, tz)
	return store.Activity{
		ID:            id,
		StartLocal:    local,
		StartUTC:      utc.UTC(),
		Date:          date,
		WeekStart:     date.WeekStart(),
		TypeKey:       a.Type,
		Sport:         sport,
		NameUntrusted: a.Name,
		DistanceM:     deref(a.Distance),
		DurationS:     deref(a.ElapsedTime),
		MovingS:       deref(a.MovingTime),
		ElevGainM:     deref(a.TotalElevationGain),
		AvgHR:         a.AverageHeartrate,
		MaxHR:         a.MaxHeartrate,
		Manual:        a.Source == "MANUAL",
		FITStatus:     fit,
		Source:        store.SourceIntervals,
		SourceID:      a.ID,
		HasPower:      deref(a.AverageWatts) > 0,
	}, nil
}

// fetchIntervals lists intervals.icu activities for [from, to] in calls of at
// most intervalsListDays, oldest first and BackfillDelay apart, and stores
// every entry the import rules admit. Excluded entries count as neither
// stored nor skipped. ids holds every listed i<digits> ID, imported or not,
// for the deletion probe. The first failed call ends the listing and returns
// its error; what earlier calls stored stays stored, and the listing is
// complete only when every call succeeded.
func (s *Service) fetchIntervals(ctx context.Context, from, to civil.Date) (fetched, error) {
	f := fetched{ids: map[int64]bool{}}
	for start := from; !to.Before(start); start = start.AddDays(intervalsListDays) {
		if start != from {
			if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
				return f, err
			}
		}
		end := start.AddDays(intervalsListDays - 1)
		if to.Before(end) {
			end = to
		}
		if err := s.listIntervals(ctx, &f, start, end); err != nil {
			return f, err
		}
	}
	f.complete = true
	return f, nil
}

// listIntervals makes one list call for [from, to] and stores its entries
// into f; an ID already in f.ids is stored once. The call asks for to + 1 day
// as newest: whether intervals.icu reads newest as the start of that day is
// unconfirmed, and the extra day costs only an entry listed twice.
func (s *Service) listIntervals(ctx context.Context, f *fetched, from, to civil.Date) error {
	if err := s.checkIntervalsPause(); err != nil {
		return err
	}
	items, err := s.opts.Intervals.ListActivities(ctx, from, to.AddDays(1))
	if err != nil {
		return s.intervalsErr(ctx, err)
	}
	for _, it := range items {
		if id, ok := IntervalsID(it.V.ID); ok {
			if f.ids[id] {
				continue // listed twice: stored once
			}
			f.ids[id] = true
		}
		a, err := MapIntervalsActivity(it.V, s.opts.TZ)
		if errors.Is(err, errNotImported) {
			continue
		}
		if err != nil {
			f.skipped++
			continue
		}
		if err := s.db.UpsertActivity(ctx, a, it.Raw); err != nil {
			return err
		}
		f.stored++
		f.seen(a.StartUTC)
		f.storedOn(a.Date)
	}
	return nil
}

// intervalsErr returns ctx's error if ctx has ended (the client reports a
// cancelled call as a transport error), else err, pausing intervals.icu calls
// if it is a 429.
func (s *Service) intervalsErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	s.noteIntervalsPause(ctx, err)
	return err
}

// reconcileIntervals mirrors intervals.icu activities deleted at the source.
// It runs only after a successful listing of [from, to], in an
// "ingest.reconcile" span, and logs one summary line; it never changes the
// type's outcome. It returns the oldest date it soft-deleted, zero when none.
func (s *Service) reconcileIntervals(ctx context.Context, from, to civil.Date, listed map[int64]bool) civil.Date {
	ctx, span := s.tracer.Start(ctx, "ingest.reconcile", trace.WithAttributes(attribute.String("data_type", TypeIntervals)))
	defer span.End()
	deleted, earliest, err := s.reconcileIntervalsActivities(ctx, from, to, listed)
	outcome := outcomeOf(err)
	span.SetAttributes(attribute.String("outcome", outcome), attribute.Int("count", deleted))
	attrs := []any{"data_type", TypeIntervals, "count", deleted, "outcome", outcome}
	if err != nil {
		failSpan(span, err)
		s.log.WarnContext(ctx, "reconcile finished", append(attrs, "error_class", errorClass(err), "error", err)...)
	} else {
		s.log.InfoContext(ctx, "reconcile finished", attrs...)
	}
	return earliest
}

// reconcileIntervalsActivities probes, oldest first, up to maxIntervalsProbes
// live intervals.icu activities dated in [from, to] whose ID the listing did
// not name, each paced by BackfillDelay after checking the pause. A confirmed
// not-found (false, nil) soft-deletes the activity (listing it again restores
// it) and writes an "activity_deleted" audit row. Any probe error keeps the
// activity and ends the run (a 429 also notes the pause). No canary is needed:
// the listing just succeeded with the same key. It returns how many it
// deleted and the oldest date among them.
func (s *Service) reconcileIntervalsActivities(ctx context.Context, from, to civil.Date, listed map[int64]bool) (int, civil.Date, error) {
	var earliest civil.Date
	stored, err := s.db.ActivityIDsInRange(ctx, store.SourceIntervals, from, to)
	if err != nil {
		return 0, earliest, err
	}
	deleted, probes := 0, 0
	for _, id := range stored {
		if listed[id] {
			continue
		}
		if probes == maxIntervalsProbes {
			break
		}
		probes++
		exists, err := s.probeIntervals(ctx, intervalsSourceID(id))
		if err != nil {
			return deleted, earliest, err
		}
		if exists {
			continue
		}
		date, ok, err := s.softDeleteActivity(ctx, TypeIntervals, id)
		if err != nil {
			return deleted, earliest, err
		}
		if !ok {
			continue
		}
		deleted++
		if earliest == (civil.Date{}) || date.Before(earliest) {
			earliest = date
		}
	}
	return deleted, earliest, nil
}

// probeIntervals waits BackfillDelay, checks the pause, then asks
// intervals.icu whether sourceID exists. A 429 notes the pause.
func (s *Service) probeIntervals(ctx context.Context, sourceID string) (bool, error) {
	if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
		return false, err
	}
	if err := s.checkIntervalsPause(); err != nil {
		return false, err
	}
	exists, err := s.opts.Intervals.ActivityExists(ctx, sourceID)
	if err != nil {
		return false, s.intervalsErr(ctx, err)
	}
	return exists, nil
}

// checkIntervalsPause returns errIntervalsPaused while intervals.icu calls
// are paused after a 429.
func (s *Service) checkIntervalsPause() error {
	if _, paused := s.intervalsPausedUntil(); paused {
		return errIntervalsPaused
	}
	return nil
}

// intervalsPausedUntil reports whether intervals.icu calls are paused, and
// until when. Garmin's pause is separate (pausedUntil).
func (s *Service) intervalsPausedUntil() (time.Time, bool) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.intervalsPauseUntil, s.now().Before(s.intervalsPauseUntil)
}

// noteIntervalsPause pauses intervals.icu calls if err is a 429 (keeping the
// later RetryAt, capped at maxPause from now) and reports whether it was. It
// leaves Garmin's pause and rate-limit counter alone.
func (s *Service) noteIntervalsPause(ctx context.Context, err error) bool {
	var rl *intervals.RateLimitedError
	if !errors.As(err, &rl) {
		return false
	}
	until := rl.RetryAt
	if limit := s.now().Add(maxPause); until.After(limit) {
		until = limit
	}
	s.pauseMu.Lock()
	if until.After(s.intervalsPauseUntil) {
		s.intervalsPauseUntil = until
	}
	s.pauseMu.Unlock()
	s.log.WarnContext(ctx, "intervals.icu rate limited", "data_type", TypeIntervals,
		"retry_after_s", int64(until.Sub(s.now()).Seconds()))
	return true
}
