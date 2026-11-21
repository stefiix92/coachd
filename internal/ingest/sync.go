// Package ingest pulls Garmin data, and activities from intervals.icu when it
// is configured, into the store: incremental syncs on a schedule and on agent
// request, plus a worker downloading original FIT files.
// It never evaluates training; it only maps, stores and summarises numbers.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/summaries"
	"github.com/stefiix92/coachd/internal/telemetry"
)

// Garmin is the subset of the Garmin client ingest uses; *garmin.Client satisfies it.
type Garmin interface {
	SearchActivities(ctx context.Context, from, to civil.Date, start, limit int) ([]garmin.Item[garmin.ActivityDTO], error)
	// ActivityExists reports false with a nil error only when Garmin confirms
	// the activity is gone (404); any doubt is an error.
	ActivityExists(ctx context.Context, id int64) (bool, error)
	DownloadActivityFIT(ctx context.Context, id int64) ([]byte, error)
	SleepRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.SleepDTO], error)
	HRVRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.HRVSummaryDTO], error)
	DailySummariesV2(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.DailySummaryV2DTO], error)
	WeightRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.WeightDTO], error)
}

var _ Garmin = (*garmin.Client)(nil)

// Intervals is the subset of the intervals.icu client ingest uses; *intervals.Client satisfies it.
type Intervals interface {
	ListActivities(ctx context.Context, from, to civil.Date) ([]intervals.Item, error)
	DownloadFile(ctx context.Context, id string) ([]byte, error)
	DownloadGeneratedFIT(ctx context.Context, id string) ([]byte, error)
	// ActivityExists reports false with a nil error only on a 404.
	ActivityExists(ctx context.Context, id string) (bool, error)
}

var _ Intervals = (*intervals.Client)(nil)

// Options configures the Service. PollInterval and SyncMinInterval must be
// positive and BackfillDelay non-negative (config.Load enforces this).
type Options struct {
	TZ                                           *time.Location
	DataDir                                      string
	BackfillDays                                 int
	BackfillDelay, SyncMinInterval, PollInterval time.Duration
	Now                                          func() time.Time // defaults to time.Now
	// Events hears about recent activities once their FIT file is handled;
	// nil hears nothing.
	Events    Events
	Intervals Intervals // nil: the intervals.icu source is off
}

// Events is told about an activity once its FIT file is stored (with its
// detail) or confirmed unavailable, when the activity started within
// recentActivity and is not a hidden copy: a backfill of old activities tells
// it nothing. It must not block.
type Events interface {
	ActivityStored(ctx context.Context, a store.Activity)
}

// recentActivity is how old an activity may be for Events to hear of it.
const recentActivity = 48 * time.Hour

// Data types, in sync order. They are also the sync_state keys and the
// raw_payloads sources for daily types.
const (
	TypeActivities = store.SyncTypeActivities
	// TypeIntervals is the activities of intervals.icu, synced only when
	// Options.Intervals is set. It has no backfill and no catch-up: its first
	// window reaches back BackfillDays.
	TypeIntervals = store.SyncTypeIntervals
	TypeSleep     = "sleep"
	TypeHRV       = "hrv"
	TypeDaily     = "daily"
	TypeWeight    = "weight"
)

// dataTypes are the Garmin data types in sync order; Backfill walks only
// these. syncTypes adds TypeIntervals.
var dataTypes = []string{TypeActivities, TypeSleep, TypeHRV, TypeDaily, TypeWeight}

// syncTypes is the sync order: dataTypes, with TypeIntervals after
// TypeActivities when the intervals.icu source is on.
func (s *Service) syncTypes() []string {
	if s.opts.Intervals == nil {
		return dataTypes
	}
	return slices.Insert(slices.Clone(dataTypes), 1, TypeIntervals)
}

// Per-type sync outcomes, stored as SyncState.LastOutcome.
const (
	OutcomeOK          = "ok"
	OutcomeRateLimited = "garmin_rate_limited"
	// OutcomeIntervalsRateLimited is TypeIntervals paused after an intervals.icu 429.
	OutcomeIntervalsRateLimited = "rate_limited"
	OutcomeAuthFailed           = "auth_failed"
	OutcomeError                = "error"
)

const (
	activityPage     = 100 // requested; Garmin may return fewer per page
	maxActivityPages = 100 // guards against an API ignoring `start`
	// Incremental windows (see windowStart). Minimum windows refetch late
	// uploads; overlaps reach back past the last success after a gap.
	activityMinDays     = 14
	activityOverlapDays = 2
	dailyMinDays        = 13
	dailyOverlapDays    = 3
	dailyMaxDays        = 27  // one request: 28 days is the smallest daily range cap
	catchUpMaxDays      = 365 // furthest back an outage catch-up reaches
	summaryDays         = 120
	maxPause            = 24 * time.Hour // caps a pathological Retry-After
)

// ErrSyncRunning is returned by Sync when another sync holds the lock.
var ErrSyncRunning = errors.New("sync already running")

// errPaused marks a Garmin call skipped because of an earlier 429.
var errPaused = errors.New("ingest: garmin paused after rate limit")

// Service runs syncs and the FIT worker.
type Service struct {
	db     *store.DB
	g      Garmin
	tel    *telemetry.Telemetry
	log    *slog.Logger
	tracer trace.Tracer
	opts   Options

	syncMu     sync.Mutex // one sync at a time; TryLock only
	backfillMu sync.Mutex // one backfill at a time; TryLock only

	pauseMu             sync.Mutex
	pauseUntil          time.Time // no Garmin calls before this instant
	intervalsPauseUntil time.Time // no intervals.icu calls before this instant

	stateMu sync.Mutex // serialises sync_state read-modify-write
	dupMu   sync.Mutex // serialises applyDuplicates: Sync and Backfill both run it

	reqMu       sync.Mutex
	lastRequest time.Time // last RequestSync that ran a sync

	existsMu sync.Mutex
	existsAt map[int64]time.Time // reconcile: activity id -> when Garmin confirmed it exists

	fillMu     sync.Mutex
	fillFailed map[int64]bool // activity ids whose local detail fill failed in this process

	fitSignal chan struct{} // cap 1; wakes the FIT worker
}

// SyncResult reports each data type's outcome and how many items were stored.
type SyncResult struct {
	Outcomes map[string]string
	Items    map[string]int
}

// New builds a Service. It does not start anything; see Run.
func New(db *store.DB, g Garmin, t *telemetry.Telemetry, o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	tr := t.Tracer
	if tr == nil {
		tr = noop.NewTracerProvider().Tracer("")
	}
	return &Service{
		db: db, g: g, tel: t, opts: o, tracer: tr,
		log:        t.Logger.With("component", "ingest"),
		fitSignal:  make(chan struct{}, 1),
		existsAt:   map[int64]time.Time{},
		fillFailed: map[int64]bool{},
	}
}

func (s *Service) now() time.Time { return s.opts.Now() }

// Sync fetches every data type for its incremental window and stores it. Types
// are independent, except that a Garmin 429 skips the remaining Garmin types
// (not TypeIntervals, which pauses only itself on an intervals.icu 429). It
// returns ErrSyncRunning if a sync is already in progress, or ctx's error if
// ctx ends before all types ran. Each run is one "ingest.sync" span with an
// "ingest.sync.type" child per data type. If any type succeeded, summaries are
// recomputed for summaryDays, or back to the oldest date stored by this sync.
func (s *Service) Sync(ctx context.Context) (SyncResult, error) {
	if !s.syncMu.TryLock() {
		return SyncResult{}, ErrSyncRunning
	}
	defer s.syncMu.Unlock()
	defer s.signalFIT()
	ctx, span := s.tracer.Start(ctx, "ingest.sync")
	defer span.End()

	today := civil.Of(s.now(), s.opts.TZ)
	res := SyncResult{Outcomes: map[string]string{}, Items: map[string]int{}}
	rateLimited, anyOK, stored := false, false, 0 // rateLimited: Garmin only
	var all fetched                               // only its earliest date is used
	dupFrom, dupOK := s.duplicatesWindow(ctx, today)
	for _, dt := range s.syncTypes() {
		if err := ctx.Err(); err != nil {
			failSpan(span, err)
			return res, err
		}
		outcome, f := s.syncType(ctx, dt, today, rateLimited && dt != TypeIntervals)
		rateLimited = rateLimited || outcome == OutcomeRateLimited
		anyOK = anyOK || outcome == OutcomeOK
		res.Outcomes[dt], res.Items[dt] = outcome, f.stored
		stored += f.stored
		all.storedOn(f.earliest)
	}
	span.SetAttributes(attribute.String("outcome", worstOutcome(res.Outcomes)), attribute.Int("count", stored))
	if dupOK && res.Outcomes[TypeActivities] == OutcomeOK && res.Outcomes[TypeIntervals] == OutcomeOK {
		s.resolveDuplicatesStep(ctx, dupFrom, today) // both ok: anyOK is set, so the recompute below covers it
	}
	if anyOK {
		if err := summaries.Recompute(ctx, s.db, today, recomputeDays(today, all.earliest)); err != nil {
			s.log.ErrorContext(ctx, "summaries recompute failed", "data_type", "summaries", "outcome", OutcomeError,
				"error_class", errorClass(err), "error", err)
		}
	}
	return res, nil
}

// duplicatesWindow is the start of the range the duplicate step covers, the
// earlier of the activities and intervals windows, taken before the sync moves
// them. ok is false when the intervals source is off or a window is unknown.
func (s *Service) duplicatesWindow(ctx context.Context, today civil.Date) (from civil.Date, ok bool) {
	if s.opts.Intervals == nil {
		return civil.Date{}, false
	}
	a, err := s.windowStart(ctx, TypeActivities, today)
	if err != nil {
		return civil.Date{}, false
	}
	i, err := s.windowStart(ctx, TypeIntervals, today)
	if err != nil {
		return civil.Date{}, false
	}
	if i.Before(a) {
		a = i
	}
	return a, true
}

// recomputeDays is how many days the post-sync recompute covers: summaryDays,
// or back to earliest (the oldest date this sync stored, zero when none) when
// that is older, e.g. after a long-outage catch-up.
func recomputeDays(today, earliest civil.Date) int {
	if earliest == (civil.Date{}) {
		return summaryDays
	}
	return max(summaryDays, today.Sub(earliest)+1)
}

// syncType fetches and stores one data type's window (or skips it when Garmin
// is paused) in an "ingest.sync.type" span, and records its outcome. It
// returns the outcome and what was fetched.
func (s *Service) syncType(ctx context.Context, dt string, today civil.Date, paused bool) (string, fetched) {
	ctx, span := s.tracer.Start(ctx, "ingest.sync.type", trace.WithAttributes(attribute.String("data_type", dt)))
	defer span.End()
	attempt := s.now()
	start := time.Now()
	var f fetched
	var err error
	if paused {
		err = errPaused
	} else {
		from, ferr := s.windowStart(ctx, dt, today)
		if ferr != nil {
			err = ferr
		} else {
			span.SetAttributes(attribute.String("calendar_date", from.String()))
			var cerr error
			var chunks int
			f, chunks, cerr = s.catchUp(ctx, dt, today)
			if cerr == nil || outcomeOf(cerr) == OutcomeError {
				// A failed chunk keeps the type from succeeding, but must not
				// starve the recent window; rate limits and auth stop the type.
				var nf fetched
				if chunks > 0 {
					err = sleepCtx(ctx, s.opts.BackfillDelay) // pace after the last chunk
				}
				if err == nil {
					nf, err = s.fetchRange(ctx, dt, from, today)
				}
				f = f.merge(nf)
			}
			err = worseErr(cerr, err)
			if err == nil && f.complete {
				switch dt {
				case TypeActivities:
					s.reconcile(ctx, from, today, f.ids)
				case TypeIntervals:
					f.storedOn(s.reconcileIntervals(ctx, from, today, f.ids))
				}
			}
		}
	}
	outcome := outcomeOf(err)
	span.SetAttributes(attribute.String("outcome", outcome), attribute.Int("count", f.stored))
	if err != nil {
		failSpan(span, err)
	}
	s.finishType(ctx, dt, attempt, time.Since(start), outcome, f, err)
	return outcome, f
}

// catchUp fetches the part of a daily type's outage gap that the normal window
// (clamped to today−27) cannot reach: [max(last−3, today−365), today−28] in
// chunks of at most 28 days, oldest first, each in an "ingest.sync.catchup"
// span. It stops at the first failed chunk and returns its error; chunks
// stored before it stay stored. Activities (of either source) and short
// outages need nothing.
func (s *Service) catchUp(ctx context.Context, dt string, today civil.Date) (f fetched, chunks int, _ error) {
	if dt == TypeActivities || dt == TypeIntervals {
		return f, 0, nil
	}
	st, err := s.db.GetSyncState(ctx, dt)
	if err != nil || st.LastSuccessAt == nil {
		return f, 0, err
	}
	from := civil.Of(*st.LastSuccessAt, s.opts.TZ).AddDays(-dailyOverlapDays)
	if floor := today.AddDays(-catchUpMaxDays); from.Before(floor) {
		from = floor
	}
	end := today.AddDays(-(dailyMaxDays + 1))
	for from.Sub(end) <= 0 {
		to := from.AddDays(dailyMaxDays)
		if to.Sub(end) > 0 {
			to = end
		}
		if chunks > 0 {
			if err := sleepCtx(ctx, s.opts.BackfillDelay); err != nil {
				return f, chunks, err
			}
		}
		chunks++
		cf, err := s.catchUpChunk(ctx, dt, from, to)
		f = f.merge(cf)
		if err != nil {
			return f, chunks, err
		}
		from = to.AddDays(1)
	}
	return f, chunks, nil
}

// worseErr returns the more severe of a and b by outcome (rate limited >
// auth failed > other error), preferring a on a tie; nil loses to any error.
func worseErr(a, b error) error {
	rank := func(e error) int {
		switch outcomeOf(e) {
		case OutcomeRateLimited, OutcomeIntervalsRateLimited:
			return 3
		case OutcomeAuthFailed:
			return 2
		}
		if e != nil {
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func (s *Service) catchUpChunk(ctx context.Context, dt string, from, to civil.Date) (fetched, error) {
	ctx, span := s.tracer.Start(ctx, "ingest.sync.catchup", trace.WithAttributes(
		attribute.String("data_type", dt), attribute.String("calendar_date", from.String())))
	defer span.End()
	f, err := s.fetchRange(ctx, dt, from, to)
	span.SetAttributes(attribute.String("outcome", outcomeOf(err)), attribute.Int("count", f.stored))
	if err != nil {
		failSpan(span, err)
	}
	return f, err
}

// merge adds o's counts to f, keeps the later freshest time and the earlier
// earliest date, and takes o's activity listing (catch-up chunks have none).
func (f fetched) merge(o fetched) fetched {
	f.stored += o.stored
	f.skipped += o.skipped
	f.empty += o.empty
	f.complete, f.ids = o.complete, o.ids
	if o.freshest != nil {
		f.seen(*o.freshest)
	}
	f.storedOn(o.earliest)
	return f
}

// worstOutcome folds per-type outcomes with precedence
// garmin_rate_limited > rate_limited > auth_failed > error > ok.
func worstOutcome(outcomes map[string]string) string {
	worst := OutcomeOK
	rank := map[string]int{OutcomeOK: 0, OutcomeError: 1, OutcomeAuthFailed: 2, OutcomeIntervalsRateLimited: 3, OutcomeRateLimited: 4}
	for _, o := range outcomes {
		if rank[o] > rank[worst] {
			worst = o
		}
	}
	return worst
}

// maxSpanStatus caps a span status description. Errors from internal/garmin
// carry only the endpoint and status, so the text holds no secrets.
const maxSpanStatus = 200

// failSpan marks span as failed with err's class and capped text.
func failSpan(span trace.Span, err error) {
	span.SetAttributes(attribute.String("error_class", errorClass(err)))
	desc := err.Error()
	if len(desc) > maxSpanStatus {
		desc = strings.ToValidUTF8(desc[:maxSpanStatus], "")
	}
	span.SetStatus(codes.Error, desc)
}

// windowStart returns the first date of dt's incremental window ending today:
//   - activities: min(today−14, lastSuccess−2), or today−14 with no success;
//   - intervals activities: as activities, but min(today−14,
//     today−BackfillDays) with no success (its first window is its backfill);
//   - daily types: max(today−27, min(today−13, lastSuccess−3)), or today−13.
//
// lastSuccess is the local date of the type's LastSuccessAt.
func (s *Service) windowStart(ctx context.Context, dt string, today civil.Date) (civil.Date, error) {
	activities := dt == TypeActivities || dt == TypeIntervals
	minDays, overlap := dailyMinDays, dailyOverlapDays
	if activities {
		minDays, overlap = activityMinDays, activityOverlapDays
	}
	from := today.AddDays(-minDays)
	st, err := s.db.GetSyncState(ctx, dt)
	if err != nil {
		return civil.Date{}, err
	}
	switch {
	case st.LastSuccessAt != nil:
		if gap := civil.Of(*st.LastSuccessAt, s.opts.TZ).AddDays(-overlap); gap.Before(from) {
			from = gap
		}
	case dt == TypeIntervals:
		if first := today.AddDays(-s.opts.BackfillDays); first.Before(from) {
			from = first
		}
	}
	if !activities {
		if floor := today.AddDays(-dailyMaxDays); from.Before(floor) {
			from = floor
		}
	}
	return from, nil
}

// finishType records metrics, the log line and the sync state for one type.
func (s *Service) finishType(ctx context.Context, dt string, attempt time.Time, took time.Duration, outcome string, f fetched, err error) {
	m := s.tel.Metrics
	m.SyncRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("data_type", dt), attribute.String("outcome", outcome)))
	m.SyncDuration.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String("data_type", dt)))
	s.logSkips(ctx, dt, f)
	attrs := []any{"data_type", dt, "outcome", outcome, "count", f.stored, "duration_ms", took.Milliseconds()}
	if err != nil {
		s.log.WarnContext(ctx, "sync type finished", append(attrs, "error_class", errorClass(err), "error", err)...)
	} else {
		s.log.InfoContext(ctx, "sync type finished", attrs...)
	}
	// Record state even if ctx was cancelled mid-fetch (e.g. request timeout).
	if serr := s.recordState(context.WithoutCancel(ctx), dt, attempt, outcome, f.freshest); serr != nil {
		s.log.ErrorContext(ctx, "sync state write failed", "data_type", dt, "error_class", errorClass(serr), "error", serr)
	}
}

// logSkips warns about items that carried data but could not be mapped, and
// logs expected empty days at debug level.
func (s *Service) logSkips(ctx context.Context, dt string, f fetched) {
	if f.skipped > 0 {
		s.log.WarnContext(ctx, "skipped unmappable items", "data_type", dt, "count", f.skipped)
	}
	if f.empty > 0 {
		s.log.DebugContext(ctx, "skipped empty items", "data_type", dt, "count", f.empty)
	}
}

// sleepEmpty reports a day with no date and no data (Garmin returns one sleep
// item per day, null when nothing was recorded).
func sleepEmpty(d garmin.SleepDTO) bool {
	return d.CalendarDate == "" && d.SleepTimeSeconds == nil && len(d.SleepScores) == 0 && d.AverageSpO2Value == nil
}

// recordState updates dt's sync state, preserving Cursor and BackfillDone and
// never moving FreshestDataAt backwards.
func (s *Service) recordState(ctx context.Context, dt string, attempt time.Time, outcome string, freshest *time.Time) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st, err := s.db.GetSyncState(ctx, dt)
	if err != nil {
		return err
	}
	st.DataType = dt
	st.LastAttemptAt = &attempt
	st.LastOutcome = outcome
	if outcome == OutcomeOK {
		st.LastSuccessAt = &attempt
	}
	if freshest != nil && (st.FreshestDataAt == nil || freshest.After(*st.FreshestDataAt)) {
		st.FreshestDataAt = freshest
	}
	return s.db.PutSyncState(ctx, st)
}

// fetched summarises one fetchRange call.
type fetched struct {
	stored, skipped int
	empty           int        // dateless items with no data: expected "no data" days
	freshest        *time.Time // latest activity start, or latest item date at local midnight
	// earliest is the oldest date stored (activity or item date), or soft-deleted
	// by the intervals.icu deletion probe; zero when none.
	earliest civil.Date
	// Activities only: complete is true when paging ended on an empty page or
	// on a page adding no new ID (not on an error or the page guard); ids holds
	// every activity ID Garmin listed. For TypeIntervals, complete is true once
	// every list call of the window succeeded, and ids holds the coachd ID of
	// every entry of the form i<digits> they returned, imported or not.
	complete bool
	ids      map[int64]bool
}

func (f *fetched) seen(t time.Time) {
	if f.freshest == nil || t.After(*f.freshest) {
		f.freshest = &t
	}
}

// storedOn notes that data dated d was stored; a zero d is ignored.
func (f *fetched) storedOn(d civil.Date) {
	if d == (civil.Date{}) {
		return
	}
	if f.earliest == (civil.Date{}) || d.Before(f.earliest) {
		f.earliest = d
	}
}

// fetchRange fetches dt for [from, to] and stores every item. Items stored
// before an error stay stored and are reflected in the result.
func (s *Service) fetchRange(ctx context.Context, dt string, from, to civil.Date) (fetched, error) {
	switch dt {
	case TypeActivities:
		return s.fetchActivities(ctx, from, to)
	case TypeIntervals:
		return s.fetchIntervals(ctx, from, to)
	case TypeSleep:
		return fetchDaily(ctx, s, dt, from, to, s.g.SleepRange, MapSleep, sleepEmpty)
	case TypeHRV:
		return fetchDaily(ctx, s, dt, from, to, s.g.HRVRange, MapHRV, nil)
	case TypeDaily:
		return fetchDaily(ctx, s, dt, from, to, s.g.DailySummariesV2, MapDailyV2, nil)
	case TypeWeight:
		return fetchDaily(ctx, s, dt, from, to, s.g.WeightRange, MapWeight, nil)
	}
	return fetched{}, fmt.Errorf("ingest: unknown data type %q", dt)
}

func (s *Service) fetchActivities(ctx context.Context, from, to civil.Date) (fetched, error) {
	return s.fetchActivitiesPaced(ctx, from, to, 0)
}

// fetchActivitiesPaced is fetchActivities waiting delay between page requests.
// Garmin's page-size cap is undocumented, so a short page does not mean the
// last one: paging advances by the items received and ends on an empty page,
// or on a page that adds no activity ID not already seen in this fetch (an API
// ignoring `start`). Only those two endings mark the fetch complete. An ID
// listed twice is stored once.
func (s *Service) fetchActivitiesPaced(ctx context.Context, from, to civil.Date, delay time.Duration) (fetched, error) {
	f := fetched{ids: map[int64]bool{}}
	offset := 0
	for page := 0; ; page++ {
		if page == maxActivityPages {
			return f, errors.New("ingest: activities: too many pages")
		}
		if page > 0 {
			if err := sleepCtx(ctx, delay); err != nil {
				return f, err
			}
		}
		if err := s.checkPause(); err != nil {
			return f, err
		}
		items, err := s.g.SearchActivities(ctx, from, to, offset, activityPage)
		if err != nil {
			s.notePause(ctx, err)
			return f, err
		}
		if len(items) == 0 {
			f.complete = true
			return f, nil
		}
		offset += len(items)
		fresh := 0
		for _, it := range items {
			if id := it.V.ActivityID; id > 0 {
				if f.ids[id] {
					continue
				}
				f.ids[id] = true
				fresh++
			}
			a, err := MapActivity(it.V, s.opts.TZ)
			if err != nil {
				f.skipped++
				continue
			}
			if err := s.db.UpsertActivity(ctx, a, it.Raw); err != nil {
				return f, err
			}
			f.stored++
			f.seen(a.StartUTC)
			f.storedOn(a.Date)
		}
		if fresh == 0 {
			f.complete = true
			return f, nil
		}
	}
}

func fetchDaily[T any](ctx context.Context, s *Service, dt string, from, to civil.Date,
	get func(context.Context, civil.Date, civil.Date) ([]garmin.Item[T], error),
	mapFn func(T) (store.DailyHealth, error), isEmpty func(T) bool) (fetched, error) {
	var f fetched
	if err := s.checkPause(); err != nil {
		return f, err
	}
	items, err := get(ctx, from, to)
	if err != nil {
		s.notePause(ctx, err)
		return f, err
	}
	for _, it := range items {
		h, err := mapFn(it.V)
		if err != nil {
			if isEmpty != nil && isEmpty(it.V) {
				f.empty++
			} else {
				f.skipped++
			}
			continue
		}
		if err := s.db.SaveRaw(ctx, dt, h.Date.String(), it.Raw); err != nil {
			return f, err
		}
		if err := s.db.UpsertDailyHealth(ctx, h); err != nil {
			return f, err
		}
		f.stored++
		f.seen(time.Date(h.Date.Year, h.Date.Month, h.Date.Day, 0, 0, 0, 0, s.opts.TZ).UTC())
		f.storedOn(h.Date)
	}
	return f, nil
}

// checkPause returns errPaused while Garmin calls are paused after a 429.
func (s *Service) checkPause() error {
	if _, paused := s.pausedUntil(); paused {
		return errPaused
	}
	return nil
}

// pausedUntil reports whether Garmin calls are paused, and until when.
func (s *Service) pausedUntil() (time.Time, bool) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.pauseUntil, s.now().Before(s.pauseUntil)
}

// notePause pauses Garmin calls if err is a 429 (keeping the later RetryAt,
// capped at maxPause from now) and reports whether it was.
func (s *Service) notePause(ctx context.Context, err error) bool {
	var rl *garmin.RateLimitedError
	if !errors.As(err, &rl) {
		return false
	}
	until := rl.RetryAt
	if limit := s.now().Add(maxPause); until.After(limit) {
		until = limit
	}
	s.pauseMu.Lock()
	if until.After(s.pauseUntil) {
		s.pauseUntil = until
	}
	s.pauseMu.Unlock()
	s.tel.Metrics.GarminRateLimited.Add(ctx, 1)
	s.log.WarnContext(ctx, "garmin rate limited", "retry_after_s", int64(until.Sub(s.now()).Seconds()))
	return true
}

func outcomeOf(err error) string {
	var rl *garmin.RateLimitedError
	var irl *intervals.RateLimitedError
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, errPaused), errors.As(err, &rl):
		return OutcomeRateLimited
	case errors.Is(err, errIntervalsPaused), errors.As(err, &irl):
		return OutcomeIntervalsRateLimited
	case errors.Is(err, garmin.ErrUnauthorized), errors.Is(err, intervals.ErrUnauthorized):
		return OutcomeAuthFailed
	}
	return OutcomeError
}

// errorClass is a low-cardinality error label for logs.
func errorClass(err error) string {
	var rl *garmin.RateLimitedError
	var irl *intervals.RateLimitedError
	var se *garmin.StatusError
	var ise *intervals.StatusError
	switch {
	case errors.Is(err, errProbeUnreliable):
		return "probe_unreliable"
	case errors.Is(err, errPaused), errors.As(err, &rl), errors.Is(err, errIntervalsPaused), errors.As(err, &irl):
		return "rate_limited"
	case errors.Is(err, garmin.ErrUnauthorized), errors.Is(err, intervals.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, garmin.ErrNotFound), errors.Is(err, intervals.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.As(err, &se):
		return "garmin_status"
	case errors.As(err, &ise):
		return "intervals_status"
	}
	return "other"
}
