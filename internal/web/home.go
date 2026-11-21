package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/store"
)

// upcomingDays is how far ahead the home page lists approved workouts
// (approved or on their way to the watch: store.DesiredStatuses).
const upcomingDays = 14

// Every date a proposal can have.
var (
	firstDate = civil.Date{Year: 1, Month: time.January, Day: 1}
	lastDate  = civil.Date{Year: 9999, Month: time.December, Day: 31}
)

// homePage is the data of home.html. Name, goal name, target and notes are
// agent-written: the template shows them escaped and labelled.
type homePage struct {
	NoPasskey bool
	Pending   []proposalRow
	Upcoming  []proposalRow
	Goal      *goalView
	Key       *keyView // nil without key renewal
	Messages  []messageRow
}

// messageRow is one agent message; Text is agent-written and untrusted.
type messageRow struct{ At, Text string }

// messagesShown and messagesWindow bound the home page's Messages section.
const (
	messagesShown  = 5
	messagesWindow = 7 * 24 * time.Hour
)

type proposalRow struct {
	ID, Day, Name, Status string
	Date                  dateParts
	Warnings              int
}

type goalView struct {
	Name, Day, Sport, Distance, Elevation, Target, Notes string
	Countdown                                            string
}

// dateParts splits a date for the pages' calendar badges: "Fri", "23", "Oct".
type dateParts struct{ Weekday, Day, Month string }

func noon(d civil.Date) time.Time { return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, time.UTC) }

// day formats a date as "Fri 23 Oct".
func day(d civil.Date) string { return noon(d).Format("Mon 2 Jan") }

func partsOf(d civil.Date) dateParts {
	t := noon(d)
	return dateParts{Weekday: t.Format("Mon"), Day: t.Format("2"), Month: t.Format("Jan")}
}

// countdown describes how far date is from today: "today", "tomorrow",
// "in 12 days", "yesterday" or "3 days ago".
func countdown(today, date civil.Date) string {
	switch n := date.Sub(today); {
	case n == 0:
		return "today"
	case n == 1:
		return "tomorrow"
	case n == -1:
		return "yesterday"
	case n > 1:
		return fmt.Sprintf("in %d days", n)
	default:
		return fmt.Sprintf("%d days ago", -n)
	}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, err := func() (homePage, error) {
		var p homePage
		has, err := s.hasPasskey(ctx)
		if err != nil {
			return p, err
		}
		p.NoPasskey = !has
		p.Key = s.keyView()
		pending, err := s.d.DB.ListProposals(ctx, firstDate, lastDate, []store.ProposalStatus{store.ProposalPending})
		if err != nil {
			return p, err
		}
		if p.Pending, err = proposalRows(pending); err != nil {
			return p, err
		}
		today := civil.Of(s.d.Now(), s.d.TZ)
		upcoming, err := s.d.DB.ListProposals(ctx, today, today.AddDays(upcomingDays), store.DesiredStatuses)
		if err != nil {
			return p, err
		}
		if p.Upcoming, err = proposalRows(upcoming); err != nil {
			return p, err
		}
		msgs, err := s.d.DB.RecentMessages(ctx, s.d.Now().Add(-messagesWindow), messagesShown)
		if err != nil {
			return p, err
		}
		for _, m := range msgs {
			p.Messages = append(p.Messages, messageRow{At: m.CreatedAt.In(s.d.TZ).Format(auditLayout), Text: m.Text})
		}
		g, err := s.d.DB.ActiveGoal(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return p, nil
		}
		if err != nil {
			return p, err
		}
		p.Goal = newGoalView(g)
		p.Goal.Countdown = countdown(today, g.Date)
		return p, nil
	}()
	if err != nil {
		s.internalError(w, r, err, false)
		return
	}
	s.render(w, r, http.StatusOK, "home", page)
}

func proposalRows(ps []store.Proposal) ([]proposalRow, error) {
	out := make([]proposalRow, 0, len(ps))
	for _, p := range ps {
		var warnings []json.RawMessage
		if len(p.WarningsJSON) > 0 {
			if err := json.Unmarshal(p.WarningsJSON, &warnings); err != nil {
				return nil, fmt.Errorf("web: proposal %s warnings: %w", p.ID, err)
			}
		}
		out = append(out, proposalRow{ID: p.ID, Day: day(p.Date), Date: partsOf(p.Date), Name: p.Name, Status: string(p.Status),
			Warnings: len(warnings)})
	}
	return out, nil
}

func newGoalView(g store.Goal) *goalView {
	v := &goalView{Name: g.Name, Day: day(g.Date) + " " + strconv.Itoa(g.Date.Year),
		Sport: strings.ReplaceAll(string(g.Sport), "_", " "), Target: g.Target, Notes: g.Notes}
	if g.DistanceKm != nil {
		v.Distance = strconv.FormatFloat(*g.DistanceKm, 'f', -1, 64) + "\u00a0km" // no-break space: the unit stays with its number
	}
	if g.ElevationGainM != nil {
		v.Elevation = strconv.FormatFloat(*g.ElevationGainM, 'f', 0, 64) + "\u00a0m climb"
	}
	return v
}
