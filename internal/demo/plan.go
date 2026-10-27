package demo

import (
	"math"
	"time"

	"github.com/muktihari/fit/profile/typedef"

	"github.com/stefiix92/coachd/internal/civil"
)

// epoch numbers the days: day 0 is a Monday.
var epoch = civil.Date{Year: 2000, Month: time.January, Day: 3}

func dayNum(d civil.Date) int { return d.Sub(epoch) }

func mod(a, n int) int { return ((a % n) + n) % n }

func floorDiv(a, n int) int { return (a - mod(a, n)) / n }

// Activity IDs encode the date and the slot (the activity's index that day),
// far above the IDs Garmin has issued so far.
const (
	idBase      int64 = 90_000_000_000
	slotsPerDay       = 10
)

func activityID(d civil.Date, slot int) int64 {
	return idBase + int64(dayNum(d))*slotsPerDay + int64(slot)
}

func decodeID(id int64) (civil.Date, int, bool) {
	off := id - idBase
	if off < 0 {
		return civil.Date{}, 0, false
	}
	return epoch.AddDays(int(off / slotsPerDay)), int(off % slotsPerDay), true
}

// rng is splitmix64: a tiny generator whose output this package fixes, so the
// data never changes with a Go release.
type rng struct{ s uint64 }

// Streams keep each use of a day's randomness independent.
const (
	streamPlan uint64 = iota + 1
	streamSleep
	streamHRV
	streamDaily
	streamWeight
	streamSamples // + slot
)

func newRNG(day int, stream uint64) *rng {
	r := &rng{s: uint64(int64(day))*0x9E3779B97F4A7C15 ^ stream*0xD1B54A32D192ED03}
	r.next()
	return r
}

func (r *rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *rng) float() float64                 { return float64(r.next()>>11) / (1 << 53) }
func (r *rng) between(lo, hi float64) float64 { return lo + (hi-lo)*r.float() }
func (r *rng) chance(p float64) bool          { return r.float() < p }
func (r *rng) intn(lo, hi int) int            { return lo + int(r.next()%uint64(hi-lo+1)) }

// kind is a type of session.
type kind int

const (
	easyRun kind = iota
	recoveryRun
	intervals
	tempo
	longRun
	trailLongRun
	indoorRide
	strength
)

type lapMode int

const (
	lapPerKm lapMode = iota
	lapPerSegment
	lapPer10Min
	lapWhole
)

// kindInfo describes a kind. hills is the altitude swing in metres (0: no
// altitude, indoors); rpe is the range of the runner's rating (0-10).
type kindInfo struct {
	name, typeKey string
	sport         typedef.Sport
	subSport      typedef.SubSport
	run           bool
	laps          lapMode
	hills         float64
	rpe           [2]int
	loadPerMin    float64 // planned training load per minute, for the health model
}

var kinds = map[kind]kindInfo{
	easyRun:      {"Demo easy run", "running", typedef.SportRunning, typedef.SubSportGeneric, true, lapPerKm, 10, [2]int{3, 4}, 1.45},
	recoveryRun:  {"Demo recovery run", "running", typedef.SportRunning, typedef.SubSportGeneric, true, lapPerKm, 6, [2]int{2, 3}, 1.1},
	intervals:    {"Demo intervals", "running", typedef.SportRunning, typedef.SubSportTrack, true, lapPerSegment, 1.5, [2]int{7, 8}, 2.1},
	tempo:        {"Demo tempo run", "running", typedef.SportRunning, typedef.SubSportGeneric, true, lapPerSegment, 6, [2]int{6, 7}, 2.0},
	longRun:      {"Demo long run", "running", typedef.SportRunning, typedef.SubSportGeneric, true, lapPerKm, 16, [2]int{5, 7}, 1.8},
	trailLongRun: {"Demo trail long run", "trail_running", typedef.SportRunning, typedef.SubSportTrail, true, lapPerKm, 47, [2]int{6, 7}, 1.9},
	indoorRide:   {"Demo indoor ride", "indoor_cycling", typedef.SportCycling, typedef.SubSportIndoorCycling, false, lapPer10Min, 0, [2]int{4, 6}, 1.3},
	strength:     {"Demo strength", "strength_training", typedef.SportTraining, typedef.SubSportStrengthTraining, false, lapWhole, 0, [2]int{5, 6}, 0.7},
}

// segment is a stretch of steady effort: speed in m/s (0 when not moving),
// the heart rate it settles at, and power in W (rides).
type segment struct {
	secs             int
	speed, hr, power float64
}

// session is one planned activity.
type session struct {
	date  civil.Date
	slot  int
	kind  kind
	clock [3]int // local start: hour, minute, second
	segs  []segment
	feel  *uint8 // workout_feel 0-100, nil when not rated
	rpe   *uint8 // workout_rpe ×10, nil when not rated
}

func (s session) info() kindInfo { return kinds[s.kind] }

func (s session) id() int64 { return activityID(s.date, s.slot) }

func (s session) secs() int {
	n := 0
	for _, g := range s.segs {
		n += g.secs
	}
	return n
}

func (s session) start(tz *time.Location) time.Time {
	return time.Date(s.date.Year, s.date.Month, s.date.Day, s.clock[0], s.clock[1], s.clock[2], 0, tz)
}

// finished reports whether the session has ended by now.
func (s session) finished(tz *time.Location, now time.Time) bool {
	return !s.start(tz).Add(time.Duration(s.secs()) * time.Second).After(now)
}

// plannedLoad is a rough training load from the plan alone, for the health
// model, which must not build samples for every day it looks at.
func (s session) plannedLoad() float64 { return float64(s.secs()) / 60 * s.info().loadPerMin }

func dayLoad(d civil.Date) float64 {
	l := 0.0
	for _, s := range plan(d) {
		l += s.plannedLoad()
	}
	return l
}

func pace(secPerKm float64) float64 { return 1000 / secPerKm }

// plan is the day's sessions: a pure function of the date. The week runs
// strength on Monday, intervals on Tuesday, easy on Wednesday, tempo or easy
// on Thursday, an indoor ride every third Friday, easy on Saturday and the
// long run on Sunday, in four-week blocks whose last week is lighter. Some
// sessions are skipped, as in any runner's log.
func plan(d civil.Date) []session {
	n := dayNum(d)
	dow, wk := mod(n, 7), floorDiv(n, 7)
	phase, block := mod(wk, 4), mod(floorDiv(wk, 4), 3)
	light := phase == 3
	vol := []float64{1, 1.06, 1.12, 0.85}[phase]
	r := newRNG(n, streamPlan)

	var out []session
	add := func(k kind, p float64, hour, minute, spread int, segs func(r *rng) []segment) {
		if !r.chance(p) {
			return
		}
		sr := newRNG(n, streamSamples+uint64(len(out)))
		s := session{date: d, slot: len(out), kind: k, segs: segs(sr)}
		total := minute + sr.intn(0, spread)
		s.clock = [3]int{hour + total/60, total % 60, sr.intn(0, 59)}
		info := s.info()
		if sr.chance(0.65) {
			f := []uint8{25, 50, 50, 75, 75, 100}[sr.intn(0, 5)]
			e := uint8(sr.intn(info.rpe[0], info.rpe[1]) * 10)
			s.feel, s.rpe = &f, &e
		}
		out = append(out, s)
	}
	minutes := func(r *rng, lo, hi float64) int { return int(math.Round(r.between(lo, hi)*vol)) * 60 }
	easy := func(lo, hi float64) func(r *rng) []segment {
		return func(r *rng) []segment {
			return []segment{{secs: minutes(r, lo, hi), speed: pace(r.between(345, 365)), hr: r.between(139, 143)}}
		}
	}

	switch dow {
	case 0: // Monday
		add(strength, 0.8, 18, 20, 40, func(r *rng) []segment {
			var segs []segment
			for i, sets := 0, r.intn(14, 18); i < sets; i++ {
				segs = append(segs, segment{secs: 45, hr: r.between(122, 132)}, segment{secs: 105, hr: r.between(98, 106)})
			}
			return segs
		})
	case 1: // Tuesday
		if light {
			add(easyRun, 0.85, 6, 20, 40, easy(40, 50))
			break
		}
		add(intervals, 0.9, 6, 20, 40, func(r *rng) []segment {
			segs := []segment{{secs: 900, speed: pace(r.between(355, 365)), hr: 136}}
			for i, reps := 0, 5+phase; i < reps; i++ {
				segs = append(segs,
					segment{secs: 180, speed: pace(r.between(248, 258)), hr: r.between(169, 173)},
					segment{secs: 120, speed: pace(r.between(410, 430)), hr: 146})
			}
			return append(segs, segment{secs: 600, speed: pace(r.between(365, 375)), hr: 139})
		})
	case 2: // Wednesday
		add(easyRun, 0.85, 6, 20, 40, easy(45, 60))
	case 3: // Thursday
		if light || wk%2 != 0 {
			add(easyRun, 0.85, 6, 20, 40, easy(40, 55))
			break
		}
		add(tempo, 0.85, 6, 20, 40, func(r *rng) []segment {
			return []segment{
				{secs: 900, speed: pace(r.between(355, 365)), hr: 137},
				{secs: r.intn(20, 25) * 60, speed: pace(r.between(285, 295)), hr: r.between(160, 163)},
				{secs: 600, speed: pace(r.between(365, 375)), hr: 140},
			}
		})
	case 4: // Friday
		if mod(wk, 3) == 1 {
			add(indoorRide, 0.85, 19, 0, 30, func(r *rng) []segment {
				main := r.between(180, 200)
				return []segment{
					{secs: 600, speed: 4 + 140/40.0, hr: 118, power: 140},
					{secs: r.intn(35, 50) * 60, speed: 4 + main/40, hr: r.between(133, 138), power: main},
					{secs: 300, speed: 4 + 120/40.0, hr: 112, power: 120},
				}
			})
		}
	case 5: // Saturday
		if r.chance(0.5) {
			add(easyRun, 0.75, 8, 30, 60, easy(40, 55))
		} else {
			add(recoveryRun, 0.75, 8, 30, 60, func(r *rng) []segment {
				return []segment{{secs: r.intn(30, 35) * 60, speed: pace(r.between(375, 390)), hr: r.between(130, 134)}}
			})
		}
	case 6: // Sunday
		km := 16 + 2*float64(block) + 1.5*float64(phase)
		k, p := longRun, 0.0
		if light {
			km = 12
		}
		if block == 2 && !light {
			k, p = trailLongRun, 45
		}
		add(k, 0.95, 7, 45, 45, func(r *rng) []segment {
			secPerKm := r.between(352, 368) + p
			return []segment{{secs: int(math.Round(km * secPerKm)), speed: pace(secPerKm), hr: r.between(142, 145)}}
		})
	}
	return out
}

// sample is one second of an activity. dist is the distance covered before
// that second.
type sample struct {
	dist, speed, hr, alt, power float64
	cad                         uint8
}

// built is a session with its samples and laps ([from, to) sample ranges).
type built struct {
	session
	startAt time.Time
	samples []sample
	total   float64 // distance, m
	laps    [][2]int
}

// build generates the session's samples, one per second: speed with a slow
// wander, heart rate easing towards each segment's level with a slow drift
// on long efforts, altitude from a smooth profile along the distance.
func (s session) build(tz *time.Location) built {
	info := s.info()
	r := newRNG(dayNum(s.date), streamSamples+100+uint64(s.slot))
	b := built{session: s, startAt: s.start(tz), samples: make([]sample, 0, s.secs())}
	hr, dist, wander, hrWander := 92.0, 0.0, 0.0, 0.0
	if !info.run && info.laps == lapWhole {
		hr = 85
	}
	alt := func(d float64) float64 {
		return 180 + info.hills*(0.7*math.Sin(2*math.Pi*d/5000)+0.3*math.Sin(2*math.Pi*d/1700+1.3))
	}
	var bounds []int
	for _, g := range s.segs {
		bounds = append(bounds, len(b.samples))
		for k := 0; k < g.secs; k++ {
			i := len(b.samples)
			wander = 0.92*wander + 0.08*(r.float()*2-1)
			hrWander = 0.98*hrWander + 0.02*(r.float()*2-1)
			speed := g.speed * (1 + 0.3*wander)
			// Cardiac drift, a slow wander and the climb ahead (2.5 bpm per 1 % grade).
			target := g.hr + 0.05*float64(i)/60 + 25*hrWander
			sm := sample{dist: dist, speed: speed}
			if info.hills > 0 {
				sm.alt = alt(dist)
				target += 250 * (alt(dist+10) - sm.alt) / 10
			}
			tau := 25.0
			if target < hr {
				tau = 40
			}
			hr += (target - hr) / tau
			sm.hr = hr + (r.float()*2-1)*1.2
			switch {
			case info.run:
				sm.cad = uint8(math.Round(80 + 2.2*speed + (r.float()*2 - 1)))
			case g.power > 0:
				sm.power = g.power * (1 + 0.06*(r.float()*2-1))
				sm.cad = uint8(math.Round(r.between(86, 92)))
			}
			b.samples = append(b.samples, sm)
			dist += speed
		}
	}
	b.total = dist
	b.laps = laps(info.laps, b.samples, bounds)
	return b
}

func laps(mode lapMode, ss []sample, bounds []int) [][2]int {
	var starts []int
	switch mode {
	case lapPerKm:
		starts = []int{0}
		for i := 1; i < len(ss); i++ {
			if math.Floor(ss[i].dist/1000) > math.Floor(ss[i-1].dist/1000) {
				starts = append(starts, i)
			}
		}
	case lapPerSegment:
		starts = bounds
	case lapPer10Min:
		for i := 0; i < len(ss); i += 600 {
			starts = append(starts, i)
		}
	default:
		starts = []int{0}
	}
	out := make([][2]int, len(starts))
	for i, from := range starts {
		to := len(ss)
		if i+1 < len(starts) {
			to = starts[i+1]
		}
		out[i] = [2]int{from, to}
	}
	return out
}

// Assumed physiology of the demo runner, for training load and calories.
const (
	restHR, maxHR = 48.0, 188.0
	ftp           = 250.0
	weightKg      = 71.0
)

// summary is computed from samples [from, to): the activity's or a lap's.
type summary struct {
	secs                         int
	dist, avgSpeed, maxSpeed     float64
	gain, loss                   float64
	avgHR, maxHR                 float64
	avgCad, avgPower             float64
	load, aerobicTE, anaerobicTE float64
	calories                     float64
}

func summarize(b built, from, to int) summary {
	ss := b.samples[from:to]
	sm := summary{secs: len(ss)}
	if len(ss) == 0 {
		return sm
	}
	end := b.total
	if to < len(b.samples) {
		end = b.samples[to].dist
	}
	sm.dist = end - ss[0].dist
	sm.avgSpeed = sm.dist / float64(len(ss))
	var hrSum, cadSum, powSum, above float64
	cads := 0
	for i, s := range ss {
		hr := math.Round(s.hr)
		hrSum += hr
		sm.maxHR = max(sm.maxHR, hr)
		sm.maxSpeed = max(sm.maxSpeed, s.speed)
		if s.cad != 0 {
			cadSum += float64(s.cad)
			cads++
		}
		powSum += s.power
		if i > 0 {
			if d := s.alt - ss[i-1].alt; d > 0 {
				sm.gain += d
			} else {
				sm.loss -= d
			}
		}
		hrr := min(max((hr-restHR)/(maxHR-restHR), 0), 1)
		sm.load += hrr * 0.64 * math.Exp(1.92*hrr) / 60
		if hr >= 165 {
			above++
		}
		sm.calories += (0.6309*hr + 0.1988*weightKg + 0.2017*35 - 55.0969) / 4.184 / 60
	}
	n := float64(len(ss))
	sm.avgHR = hrSum / n
	if cads > 0 {
		sm.avgCad = cadSum / float64(cads)
	}
	sm.avgPower = powSum / n
	sm.aerobicTE = 5 * (1 - math.Exp(-sm.load/110))
	sm.anaerobicTE = min(4, 0.2+above/350)
	return sm
}

// vo2max drifts slowly over the season.
func vo2max(d civil.Date) float64 {
	return math.Round(52 + 1.2*math.Sin(2*math.Pi*float64(dayNum(d))/140))
}
