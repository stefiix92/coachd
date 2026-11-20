package ingest

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
)

// resolveDuplicates returns, for every activity in acts, the ID of the copy
// that hides it, or nil. A pair is one Garmin and one intervals.icu activity
// of the same sport whose shared time is at least half of the shorter one;
// the richer copy (power, else heart rate, else Garmin) is shown. Pairing is
// one-to-one and deterministic for any input order. Pure: no store, no clock.
func resolveDuplicates(acts []store.Activity) map[int64]*int64 {
	out := make(map[int64]*int64, len(acts))
	var garmins, others []store.Activity
	for _, a := range acts {
		out[a.ID] = nil
		if a.DurationS <= 0 {
			continue
		}
		if a.Source == store.SourceIntervals {
			others = append(others, a)
		} else {
			garmins = append(garmins, a)
		}
	}
	slices.SortFunc(garmins, func(a, b store.Activity) int {
		return cmp.Or(a.StartUTC.Compare(b.StartUTC), cmp.Compare(a.ID, b.ID))
	})
	taken := make(map[int64]bool, len(others))
	for _, g := range garmins {
		best, bestOverlap := -1, 0.0
		for i, o := range others {
			if taken[o.ID] || o.Sport != g.Sport {
				continue
			}
			ov := overlapSeconds(g, o)
			if ov*2 < min(g.DurationS, o.DurationS) {
				continue
			}
			if best < 0 || ov > bestOverlap || (ov == bestOverlap && intervalsBefore(o, others[best])) {
				best, bestOverlap = i, ov
			}
		}
		if best < 0 {
			continue
		}
		o := others[best]
		taken[o.ID] = true
		winner, loser := g, o
		if intervalsRicher(o, g) {
			winner, loser = o, g
		}
		id := winner.ID
		out[loser.ID] = &id
	}
	return out
}

// overlapSeconds is the time both activities cover.
func overlapSeconds(a, b store.Activity) float64 {
	start := a.StartUTC
	if b.StartUTC.After(start) {
		start = b.StartUTC
	}
	end := a.StartUTC.Add(secondsToDuration(a.DurationS))
	if e := b.StartUTC.Add(secondsToDuration(b.DurationS)); e.Before(end) {
		end = e
	}
	return max(0, end.Sub(start).Seconds())
}

func secondsToDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// intervalsBefore orders tied candidates: earlier start, then lower absolute ID.
func intervalsBefore(a, b store.Activity) bool {
	if c := a.StartUTC.Compare(b.StartUTC); c != 0 {
		return c < 0
	}
	return a.ID > b.ID // IDs are negative: the larger one has the lower absolute value
}

// intervalsRicher reports whether the intervals.icu copy o beats the Garmin
// copy g: power over none, else heart rate over none, else Garmin.
func intervalsRicher(o, g store.Activity) bool {
	if o.HasPower != g.HasPower {
		return o.HasPower
	}
	return o.AvgHR != nil && g.AvgHR == nil
}

// applyDuplicates resolves the duplicate pairs dated in [from, to] and writes
// the marks that differ from the stored ones. It returns how many it changed.
// A mark whose hider is dated outside [from, to] (the two copies of a session
// can carry different local dates) is kept while the hider is not
// soft-deleted. One runs at a time, so a sync and a backfill never write
// marks from views of the store taken before each other's writes.
func (s *Service) applyDuplicates(ctx context.Context, from, to civil.Date) (int, error) {
	s.dupMu.Lock()
	defer s.dupMu.Unlock()
	acts, err := s.db.ActivitiesForDuplicates(ctx, from, to)
	if err != nil {
		return 0, err
	}
	marks := resolveDuplicates(acts)
	loaded := make(map[int64]bool, len(acts))
	for _, a := range acts {
		loaded[a.ID] = true
	}
	changed := 0
	for _, a := range acts {
		want := marks[a.ID]
		if (want == nil) == (a.HiddenBy == nil) && (want == nil || *want == *a.HiddenBy) {
			continue
		}
		if want == nil && !loaded[*a.HiddenBy] {
			kept, err := s.db.ActivityNotDeleted(ctx, *a.HiddenBy)
			if err != nil {
				return changed, err
			}
			if kept {
				continue
			}
		}
		if err := s.db.SetHiddenBy(ctx, a.ID, want); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// resolveDuplicatesStep runs applyDuplicates and logs it; a store error is
// logged and changes no outcome.
func (s *Service) resolveDuplicatesStep(ctx context.Context, from, to civil.Date) {
	n, err := s.applyDuplicates(ctx, from, to)
	if err != nil {
		s.log.ErrorContext(ctx, "duplicates resolve failed", "data_type", TypeIntervals, "outcome", OutcomeError,
			"count", n, "error_class", errorClass(err), "error", err)
		return
	}
	s.log.InfoContext(ctx, "duplicates resolved", "data_type", TypeIntervals, "count", n)
}
