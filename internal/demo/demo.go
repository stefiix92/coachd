// Package demo is a synthetic Garmin for trying coachd without a Garmin
// Personal API key (COACHD_DEMO=true, docs/design/demo-mode.md).
//
// Garmin implements ingest.Garmin with data that is a pure function of the
// calendar date and the runner's timezone: a date always gives the same
// activities, IDs, health values and FIT files, so ingest's idempotent
// upserts behave as they do with Garmin. Data reaches back HistoryDays from
// today and appears as the day goes by: an activity once it has ended, a
// day's sleep, HRV, daily summary and weight from 07:00. It makes no network
// call. It does not implement garminkey.Exchanger: there is no key to renew.
//
// The runner it describes is plausible, not real: four or five runs a week
// with a weekly long run, a strength session and an occasional indoor ride,
// in four-week blocks whose last week is lighter. Names are neutral and say
// "Demo". Nothing here evaluates training.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/garmin"
)

// HistoryDays is how far back from today the demo lists data.
const HistoryDays = 182

// KeyExpires stands in for the Garmin key's expiry, which demo mode does not
// have: far enough ahead that no expiry warning or alert fires.
var KeyExpires = civil.Date{Year: 2099, Month: time.December, Day: 31}

// morning is the local hour from which a day's sleep, HRV, daily summary and
// weight are available.
const morning = 7

// Garmin is the synthetic Garmin. It is safe for concurrent use.
type Garmin struct {
	tz  *time.Location
	now func() time.Time
}

// New returns a synthetic Garmin for a runner in tz. now defaults to time.Now.
func New(tz *time.Location, now func() time.Time) *Garmin {
	if now == nil {
		now = time.Now
	}
	return &Garmin{tz: tz, now: now}
}

func (g *Garmin) today() civil.Date { return civil.Of(g.now(), g.tz) }

// window clamps [from, to] to the listed history, HistoryDays up to today.
func (g *Garmin) window(from, to civil.Date) (civil.Date, civil.Date) {
	today := g.today()
	if first := today.AddDays(-(HistoryDays - 1)); from.Before(first) {
		from = first
	}
	if today.Before(to) {
		to = today
	}
	return from, to
}

// dayReady reports whether d's daily data (sleep, HRV, summary, weight) is
// available at now.
func (g *Garmin) dayReady(d civil.Date, now time.Time) bool {
	today := civil.Of(now, g.tz)
	return d.Before(today) || (d == today && now.In(g.tz).Hour() >= morning)
}

// SearchActivities lists the finished activities dated in [from, to], newest
// first, from start, at most limit of them.
func (g *Garmin) SearchActivities(ctx context.Context, from, to civil.Date, start, limit int) ([]garmin.Item[garmin.ActivityDTO], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	from, to = g.window(from, to)
	now := g.now()
	var listed []session
	for d := to; !d.Before(from); d = d.AddDays(-1) {
		ss := plan(d)
		for i := len(ss) - 1; i >= 0; i-- {
			if ss[i].finished(g.tz, now) {
				listed = append(listed, ss[i])
			}
		}
	}
	items := []garmin.Item[garmin.ActivityDTO]{}
	if start < 0 || limit <= 0 || start >= len(listed) {
		return items, nil
	}
	for _, s := range listed[start:min(len(listed), start+limit)] {
		it, err := activityItem(s.build(g.tz))
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// lookup finds the finished activity with id.
func (g *Garmin) lookup(id int64) (session, bool) {
	d, slot, ok := decodeID(id)
	if !ok || g.today().Before(d) {
		return session{}, false
	}
	for _, s := range plan(d) {
		if s.slot == slot {
			return s, s.finished(g.tz, g.now())
		}
	}
	return session{}, false
}

// ActivityExists reports whether id is a finished demo activity. Any other
// id is gone, as a Garmin 404 would say.
func (g *Garmin) ActivityExists(ctx context.Context, id int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, ok := g.lookup(id)
	return ok, nil
}

// DownloadActivityFIT returns the activity's FIT file, built from the same
// samples as its summary; garmin.ErrNotFound for any other id.
func (g *Garmin) DownloadActivityFIT(ctx context.Context, id int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, ok := g.lookup(id)
	if !ok {
		return nil, garmin.ErrNotFound
	}
	return encodeFIT(s.build(g.tz), g.tz)
}

// SleepRange returns the sleep summaries of [from, to], oldest first.
func (g *Garmin) SleepRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.SleepDTO], error) {
	return daily[garmin.SleepDTO](ctx, g, from, to, func(d civil.Date) (any, bool) { return sleepJSON(d), true })
}

// HRVRange returns the HRV summaries of [from, to], oldest first.
func (g *Garmin) HRVRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.HRVSummaryDTO], error) {
	return daily[garmin.HRVSummaryDTO](ctx, g, from, to, func(d civil.Date) (any, bool) { return hrvJSON(d), true })
}

// DailySummariesV2 returns the daily summaries of [from, to], oldest first.
func (g *Garmin) DailySummariesV2(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.DailySummaryV2DTO], error) {
	return daily[garmin.DailySummaryV2DTO](ctx, g, from, to, func(d civil.Date) (any, bool) { return dailyJSON(d), true })
}

// WeightRange returns the days in [from, to] with a weigh-in, oldest first.
func (g *Garmin) WeightRange(ctx context.Context, from, to civil.Date) ([]garmin.Item[garmin.WeightDTO], error) {
	return daily[garmin.WeightDTO](ctx, g, from, to, weightJSON)
}

// daily builds the items of the available days in [from, to] from gen, which
// returns a day's JSON object or false for a day without one.
func daily[T any](ctx context.Context, g *Garmin, from, to civil.Date, gen func(civil.Date) (any, bool)) ([]garmin.Item[T], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	from, to = g.window(from, to)
	now := g.now()
	items := []garmin.Item[T]{}
	for d := from; !to.Before(d); d = d.AddDays(1) {
		if !g.dayReady(d, now) {
			continue
		}
		v, ok := gen(d)
		if !ok {
			continue
		}
		it, err := item[T](v)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// item encodes v as Garmin would send it and decodes the DTO from that JSON,
// as the Garmin client does, so the raw payload and the DTO always agree.
func item[T any](v any) (garmin.Item[T], error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return garmin.Item[T]{}, fmt.Errorf("demo: encode: %w", err)
	}
	var dto T
	if err := json.Unmarshal(raw, &dto); err != nil {
		return garmin.Item[T]{}, fmt.Errorf("demo: decode: %w", err)
	}
	return garmin.Item[T]{V: dto, Raw: raw}, nil
}
