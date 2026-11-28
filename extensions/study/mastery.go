package main

import (
	"math"
	"sort"
	"time"

	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

// Mastery statuses, weakest first. They are also the values of the weak-points filter.
const (
	statusWeak  = "weak"  // below 50: needs targeted work
	statusShaky = "shaky" // 50 to 74: recalled, but not reliably
	statusSolid = "solid" // 75 and above
	statusNew   = "new"   // not reviewed or practised yet
)

var statusLabels = map[string]string{statusWeak: "薄弱", statusShaky: "不稳", statusSolid: "掌握", statusNew: "未学"}

func validStatus(s string) bool { _, ok := statusLabels[s]; return ok }

func statusOf(score int, seen bool) string {
	switch {
	case !seen:
		return statusNew
	case score < 50:
		return statusWeak
	case score < 75:
		return statusShaky
	}
	return statusSolid
}

// recentWindow is how far back reviews and practice count toward recent accuracy.
const recentWindow = 30 * 24 * time.Hour

// cardScore is one card's mastery.
type cardScore struct {
	Card     *Card
	Score    int
	Status   string
	Recent   float64 // share of recent answers recalled or correct
	RecentN  int
	Mistakes int // recent wrong practice answers
}

type tally struct{ good, n, wrong int }

// recentTallies counts recent reviews and attempts per card id.
func recentTallies(st state, now time.Time) map[string]*tally {
	since := now.Add(-recentWindow)
	out := map[string]*tally{}
	get := func(id string) *tally {
		t := out[id]
		if t == nil {
			t = &tally{}
			out[id] = t
		}
		return t
	}
	for _, r := range st.Reviews {
		if r.At.Before(since) {
			continue
		}
		t := get(r.Card)
		t.n++
		if r.Rating != fsrs.Again {
			t.good++
		}
	}
	for _, at := range st.Attempts {
		if at.Card == "" || at.At.Before(since) {
			continue
		}
		t := get(at.Card)
		if at.Verdict == verdictWrong {
			t.wrong++ // a practice mistake, also when the attempt was recorded as the review
		}
		if at.Rated {
			continue // its answer already counts as that review
		}
		t.n++
		if at.Verdict == verdictCorrect {
			t.good++
		}
	}
	return out
}

// scoreCard scores one card from 0 to 100 from what FSRS knows of it and how the learner did lately:
//
//	45% retrievability now (FSRS forgetting curve)
//	20% stability, saturating at about a month
//	15% ease, from FSRS difficulty (1 easy … 10 hard)
//	20% recent accuracy over reviews and practice answers in the last 30 days
//
// minus 6 points a lapse, at most 30. A card never reviewed nor practised is new and scores 0; one only practised
// scores 70 times its recent accuracy.
func scoreCard(s scheduler, c *Card, t *tally, now time.Time) cardScore {
	out := cardScore{Card: c}
	if t != nil && t.n > 0 {
		out.Recent, out.RecentN, out.Mistakes = float64(t.good)/float64(t.n), t.n, t.wrong
	}
	seen := c.FSRS.State != fsrs.New || out.RecentN > 0
	if !seen {
		out.Status = statusNew
		return out
	}
	if c.FSRS.State == fsrs.New {
		// Practised but never reviewed: FSRS knows nothing yet, so the score is recent accuracy alone, capped at 70
		// (shaky) until spaced reviews show it lasts.
		out.Score = int(math.Round(70 * out.Recent))
		out.Status = statusOf(out.Score, true)
		return out
	}
	var r float64
	if c.FSRS.State != fsrs.New {
		if v, err := s.f.Retrievability(c.FSRS, now.UTC()); err == nil && !math.IsNaN(v) {
			r = v
		}
	}
	stab := 1 - math.Exp(-math.Max(0, c.FSRS.Stability)/30)
	ease := 0.5
	if d := c.FSRS.Difficulty; d >= 1 && d <= 10 {
		ease = 1 - (d-1)/9
	}
	recent := r // with no recent answers, recent accuracy follows retrievability
	if out.RecentN > 0 {
		recent = out.Recent
	}
	v := 100*(0.45*r+0.20*stab+0.15*ease+0.20*recent) - math.Min(30, 6*float64(c.FSRS.Lapses))
	out.Score = int(math.Round(math.Max(0, math.Min(100, v))))
	out.Status = statusOf(out.Score, true)
	return out
}

// topicScore sums up one knowledge point.
type topicScore struct {
	Topic    string
	Score    int // the average of its seen cards
	Status   string
	Cards    []cardScore // weakest first
	Seen     int
	Weak     int
	Mistakes int
	Recent   float64
	RecentN  int
}

// masteryByTopic scores every card that is not paused and groups them by knowledge point, weakest topic first.
func masteryByTopic(s scheduler, st state, now time.Time) []topicScore {
	tallies := recentTallies(st, now)
	groups := map[string]*topicScore{}
	for _, c := range st.Cards {
		if c.Suspended {
			continue
		}
		cs := scoreCard(s, c, tallies[c.ID], now)
		name := topicOf(c)
		g := groups[name]
		if g == nil {
			g = &topicScore{Topic: name}
			groups[name] = g
		}
		g.Cards = append(g.Cards, cs)
	}
	out := make([]topicScore, 0, len(groups))
	for _, g := range groups {
		sum, good := 0, 0.0
		for _, cs := range g.Cards {
			if cs.Status == statusNew {
				continue
			}
			g.Seen++
			sum += cs.Score
			if cs.Status == statusWeak {
				g.Weak++
			}
			g.Mistakes += cs.Mistakes
			g.RecentN += cs.RecentN
			good += cs.Recent * float64(cs.RecentN)
		}
		if g.Seen > 0 {
			g.Score = int(math.Round(float64(sum) / float64(g.Seen)))
		}
		if g.RecentN > 0 {
			g.Recent = good / float64(g.RecentN)
		}
		g.Status = statusOf(g.Score, g.Seen > 0)
		sort.Slice(g.Cards, func(i, j int) bool { return lessCard(g.Cards[i], g.Cards[j]) })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Status == statusNew) != (b.Status == statusNew) {
			return b.Status == statusNew // topics with nothing seen go last
		}
		if a.Score != b.Score {
			return a.Score < b.Score
		}
		return a.Topic < b.Topic
	})
	return out
}

// lessCard orders cards weakest first; new cards come after every seen one.
func lessCard(a, b cardScore) bool {
	if (a.Status == statusNew) != (b.Status == statusNew) {
		return b.Status == statusNew
	}
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.Card.ID < b.Card.ID
}

// weakestCard is the card to practise in a topic (or across all topics when topic is empty): the lowest score among
// seen cards, else the oldest new one.
func weakestCard(s scheduler, st state, now time.Time, topic string) *Card {
	for _, g := range masteryByTopic(s, st, now) {
		if topic != "" && g.Topic != topic {
			continue
		}
		if len(g.Cards) > 0 {
			return g.Cards[0].Card
		}
	}
	return nil
}

// filterTopics keeps the topics of one status ("" keeps all) and, when name is given, only that topic.
func filterTopics(all []topicScore, status, name string) []topicScore {
	out := all[:0:0]
	for _, g := range all {
		if name != "" && g.Topic != name {
			continue
		}
		if status != "" && g.Status != status {
			continue
		}
		out = append(out, g)
	}
	return out
}
