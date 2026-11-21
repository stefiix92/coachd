// Package triggers decides which day, workout and week events are due and
// fires each exactly once to its sinks. It holds no coaching logic: an event
// says a moment arrived, never what it means.
package triggers

import (
	"context"
	"log/slog"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/outcome"
	"github.com/stefiix92/coachd/internal/store"
)

// Event names fired by this package.
const (
	HealthDayReady   = "health.day_ready"
	WorkoutUnmatched = "workout.unmatched"
	WeekClosed       = "week.closed"
)

const (
	gateHour        = 6
	defaultInterval = 5 * time.Minute
)

// Event is one fired moment. ProposalID is set for WorkoutUnmatched only.
type Event struct {
	Name       string
	Date       civil.Date
	ProposalID string
}

// Sink hears each event once. It must not block.
type Sink interface {
	Fire(ctx context.Context, ev Event)
}

// Options configures a Service.
type Options struct {
	DB    *store.DB
	TZ    *time.Location
	Now   func() time.Time
	Sinks []Sink
	Log   *slog.Logger
	// Interval is the time between checks in Run; zero means 5 minutes.
	Interval time.Duration
}

// Service checks for due events.
type Service struct{ o Options }

// New returns a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	return &Service{o: o}
}

// fire records the event and, only on its first mark, hands it to the sinks.
func (s *Service) fire(ctx context.Context, ev Event, key string) error {
	first, err := s.o.DB.MarkEvent(ctx, ev.Name, key, s.o.Now())
	if err != nil || !first {
		return err
	}
	for _, k := range s.o.Sinks {
		k.Fire(ctx, ev)
	}
	return nil
}

// Check runs one pass over the due events.
func (s *Service) Check(ctx context.Context) error {
	now := s.o.Now()
	today := civil.Of(now, s.o.TZ)
	gated := now.In(s.o.TZ).Hour() >= gateHour

	hs, err := s.o.DB.ListDailyHealth(ctx, today, today)
	if err != nil {
		return err
	}
	for _, h := range hs {
		if h.SleepS != nil {
			if err := s.fire(ctx, Event{Name: HealthDayReady, Date: today}, today.String()); err != nil {
				return err
			}
		}
	}
	if !gated {
		return nil
	}

	if err := s.checkUnmatched(ctx, today); err != nil {
		return err
	}

	if today.WeekStart() == today {
		last := today.AddDays(-7)
		if err := s.fire(ctx, Event{Name: WeekClosed, Date: last}, last.String()); err != nil {
			return err
		}
	}
	return nil
}

// checkUnmatched fires WorkoutUnmatched for each proposal dated yesterday
// that is delivered and that no stored activity carried out. It does nothing
// until yesterday's activities can be taken as in (activitiesIn), so a later
// check tries again.
func (s *Service) checkUnmatched(ctx context.Context, today civil.Date) error {
	in, err := s.activitiesIn(ctx, today)
	if err != nil || !in {
		return err
	}
	yesterday := today.AddDays(-1)
	ps, err := s.o.DB.ListProposals(ctx, yesterday, yesterday, []store.ProposalStatus{store.ProposalDelivered})
	if err != nil || len(ps) == 0 {
		return err
	}
	linked, err := outcome.Linker{DB: s.o.DB}.ActivitiesFor(ctx, ps)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if len(linked[p.ID]) > 0 {
			continue
		}
		if err := s.fire(ctx, Event{Name: WorkoutUnmatched, Date: yesterday, ProposalID: p.ID}, p.ID); err != nil {
			return err
		}
	}
	return nil
}

// activitiesIn reports whether the activities that could carry out
// yesterday's workout are in: the activities sync succeeded at or after the
// gate hour today, and no activity dated yesterday or today still has its FIT
// file pending (linking needs the FIT's detail). It only orders events; it
// evaluates nothing.
func (s *Service) activitiesIn(ctx context.Context, today civil.Date) (bool, error) {
	st, err := s.o.DB.GetSyncState(ctx, store.SyncTypeActivities)
	if err != nil {
		return false, err
	}
	gate := time.Date(today.Year, today.Month, today.Day, gateHour, 0, 0, 0, s.o.TZ)
	if st.LastSuccessAt == nil || st.LastSuccessAt.Before(gate) {
		return false, nil
	}
	pending, err := s.o.DB.HasPendingFIT(ctx, today.AddDays(-1), today)
	if err != nil {
		return false, err
	}
	return !pending, nil
}

// Run checks now, then on every interval, until ctx ends. A failed check is
// logged and tried again on the next tick.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	for {
		if err := s.Check(ctx); err != nil && ctx.Err() == nil {
			s.o.Log.Error("triggers check failed", "component", "triggers", "error_class", "store")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
