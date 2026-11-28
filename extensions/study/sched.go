package main

import (
	"errors"
	"fmt"
	"sort"
	"time"

	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

// scheduler wraps go-fsrs (FSRS-6) with the user's retention target.
type scheduler struct{ f *fsrs.FSRS }

func newScheduler(retention float64) scheduler {
	p := fsrs.DefaultParam()
	if retention >= 0.7 && retention <= 0.97 {
		p.RequestRetention = retention
	}
	p.EnableShortTerm = true
	p.EnableFuzz = true
	return scheduler{f: fsrs.NewFSRS(p)}
}

var ratings = []fsrs.Rating{fsrs.Again, fsrs.Hard, fsrs.Good, fsrs.Easy}

func parseRating(s string) (fsrs.Rating, error) {
	switch s {
	case "1", "again":
		return fsrs.Again, nil
	case "2", "hard":
		return fsrs.Hard, nil
	case "3", "good":
		return fsrs.Good, nil
	case "4", "easy":
		return fsrs.Easy, nil
	}
	return 0, errors.New("评分只能是 again、hard、good 或 easy")
}

func ratingLabel(r fsrs.Rating) string {
	switch r {
	case fsrs.Again:
		return "忘了"
	case fsrs.Hard:
		return "模糊"
	case fsrs.Good:
		return "记得"
	case fsrs.Easy:
		return "很熟"
	}
	return "?"
}

func ratingKey(r fsrs.Rating) string {
	switch r {
	case fsrs.Again:
		return "again"
	case fsrs.Hard:
		return "hard"
	case fsrs.Good:
		return "good"
	case fsrs.Easy:
		return "easy"
	}
	return ""
}

// preview is the next due time for each rating, for the four answer buttons.
func (s scheduler) preview(c *Card, now time.Time) map[fsrs.Rating]time.Time {
	out := map[fsrs.Rating]time.Time{}
	log, err := s.f.Repeat(c.FSRS, now.UTC())
	if err != nil {
		return out
	}
	for r, info := range log {
		out[r] = info.Card.Due
	}
	return out
}

// grade applies one review to the card and logs it.
func (s scheduler) grade(st *state, id string, r fsrs.Rating, now time.Time) (*Card, error) {
	c := st.Cards[id]
	if c == nil {
		return nil, errors.New("没有这张卡片")
	}
	wasNew := c.FSRS.State == fsrs.New
	info, err := s.f.Next(c.FSRS, now.UTC(), r)
	if err != nil {
		return nil, fmt.Errorf("没能排下一次复习：%w", err)
	}
	c.FSRS = info.Card
	st.logReview(Review{Card: id, Rating: r, At: now.UTC(), WasNew: wasNew, Due: c.FSRS.Due})
	return c, nil
}

// queue is what to review now: due cards that were seen before, most overdue first, then new cards up to the day's
// allowance (new cards already started today count against it).
func queue(st state, now time.Time, loc *time.Location, newPerDay int) []*Card {
	var due, fresh []*Card
	for _, c := range st.Cards {
		if c.Suspended {
			continue
		}
		if c.FSRS.State == fsrs.New {
			fresh = append(fresh, c)
		} else if !c.FSRS.Due.After(now) {
			due = append(due, c)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].FSRS.Due.Equal(due[j].FSRS.Due) {
			return due[i].FSRS.Due.Before(due[j].FSRS.Due)
		}
		return due[i].ID < due[j].ID
	})
	sort.Slice(fresh, func(i, j int) bool {
		if !fresh[i].Created.Equal(fresh[j].Created) {
			return fresh[i].Created.Before(fresh[j].Created)
		}
		return fresh[i].ID < fresh[j].ID
	})
	allow := max(0, newPerDay-newStartedOn(st, now, loc))
	if len(fresh) > allow {
		fresh = fresh[:allow]
	}
	return append(due, fresh...)
}

func newStartedOn(st state, now time.Time, loc *time.Location) int {
	today := now.In(loc).Format(time.DateOnly)
	n := 0
	for i := len(st.Reviews) - 1; i >= 0; i-- {
		r := st.Reviews[i]
		d := r.At.In(loc).Format(time.DateOnly)
		if d < today {
			break
		}
		if d == today && r.WasNew {
			n++
		}
	}
	return n
}

type stats struct {
	Total, Due, New, Learning, Mature, Suspended, Drafts int
	ReviewedToday, CorrectToday                          int
	Streak                                               int     // days in a row with at least one review, up to today or yesterday
	Retention                                            float64 // share of non-Again answers on reviews of cards seen before, last 30 days
	RetentionN                                           int
}

func computeStats(st state, now time.Time, loc *time.Location, newPerDay int) stats {
	out := stats{Total: len(st.Cards), Drafts: len(st.Drafts), Due: len(queue(st, now, loc, newPerDay))}
	for _, c := range st.Cards {
		switch {
		case c.Suspended:
			out.Suspended++
		case c.FSRS.State == fsrs.New:
			out.New++
		case c.FSRS.State == fsrs.Review && c.FSRS.ScheduledDays >= 21:
			out.Mature++
		default:
			out.Learning++
		}
	}
	today := now.In(loc).Format(time.DateOnly)
	since := now.AddDate(0, 0, -30)
	days := map[string]bool{}
	good := 0
	for _, r := range st.Reviews {
		d := r.At.In(loc).Format(time.DateOnly)
		days[d] = true
		if d == today {
			out.ReviewedToday++
			if r.Rating != fsrs.Again {
				out.CorrectToday++
			}
		}
		if !r.WasNew && r.At.After(since) {
			out.RetentionN++
			if r.Rating != fsrs.Again {
				good++
			}
		}
	}
	if out.RetentionN > 0 {
		out.Retention = float64(good) / float64(out.RetentionN)
	}
	day := now.In(loc)
	if !days[day.Format(time.DateOnly)] {
		day = day.AddDate(0, 0, -1)
	}
	for days[day.Format(time.DateOnly)] {
		out.Streak++
		day = day.AddDate(0, 0, -1)
	}
	return out
}

// humanGap says how long until t, in the short form the answer buttons show.
func humanGap(now, t time.Time) string {
	d := t.Sub(now)
	switch {
	case d < time.Minute:
		return "马上"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()+0.5))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d 天", int(d.Hours()/24+0.5))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%.1f 个月", d.Hours()/24/30)
	}
	return fmt.Sprintf("%.1f 年", d.Hours()/24/365)
}

type fsrsRating = fsrs.Rating
