package curate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/triage"
)

// The correction fast path (0.2.0-beta.52, design l0-leverage §11 change 3, after the collaboration gap analysis):
// a correction the user makes in a conversation with the assistant waited for the next night's pass and then stood
// as a candidate, so the same mistake came back within a day or two. Every few minutes, when nothing else runs, the
// day's assistant conversations (S1) are looked at past where the last look stopped; a conversation where a line of
// the user's reads as a correction is read at once with the daily prompt (one call), and the snapshot is published,
// so the standing set takes the correction in this turn's next page. The source cursors are not moved and the day
// is not marked read: the night's pass reads the whole day as always, and a sighting counts once per session and day.

// fastCue is what a user's correction of the assistant reads like (a cue only: the daily prompt decides).
var fastCue = regexp.MustCompile(`不是说|说过|说了.{0,6}(遍|次)|又(来|犯|是|这样)|别再|不要再|为什么还|怎么还|怎么又|第[二三四五六]遍|以后(都|一律|别|不要)|一律|直接(做|干|往下)|别(问|来问)我|不用问|搞错|错了|你他妈|脱裤子|(?i)\b(stop|don't|do not) (ask|do that)\b|i (already )?told you|again\?`)

const (
	// FastEvery is how often the fast path looks, when nothing else runs.
	FastEvery = 10 * time.Minute
	// fastMost is how many conversations it reads in a day at most (its cost: one daily call each).
	fastMost = 24
)

// RunFast looks at today's assistant conversations past where it last stopped and reads at once those where the
// user corrected the assistant. It returns how many it read.
func (c *Curator) RunFast(ctx context.Context) (int, error) {
	if c.Model == nil {
		return 0, nil
	}
	c.St.Lock()
	defer c.St.Unlock()
	day := c.now().In(c.Loc).Format("2006-01-02")
	if c.St.S.FastSeen == nil {
		c.St.S.FastSeen = map[string]int{}
	}
	for k := range c.St.S.FastSeen {
		if !strings.HasSuffix(k, "@"+day) {
			delete(c.St.S.FastSeen, k) // a past day is the daily pass's
		}
	}
	reads := c.fastReads(day)
	if reads >= fastMost {
		return 0, nil
	}
	c.collectBegin(day) // the sources read today; nothing is committed (no collectEnd)
	units, err := c.unitsWhere(ctx, day, func(kind, id string) bool { return kind == "chats" && strings.HasPrefix(id, "persona-") })
	if err != nil {
		return 0, err
	}
	owner := c.ownerNames(ctx)
	known := c.knownEntities(ctx)
	b := c.builder(day, owner)
	metered := &model.Metered{Model: c.Model, Usage: c.St, Day: day}
	run := &store.Run{ID: fmt.Sprintf("fast-%s-%d", day, c.now().Unix()), Kind: "fast", Period: day,
		Started: c.now().UTC().Format(time.RFC3339), Counts: map[string]int{}}
	n := 0
	for i := range units {
		u := &units[i]
		if unitCode(u) != entry.SrcAssistant {
			continue
		}
		key := u.Ref + "@" + day
		last := c.St.S.FastSeen[key]
		top, hit := last, false
		for _, l := range u.Lines {
			if l.N <= last {
				continue
			}
			top = max(top, l.N)
			if u.Who[l.N] == entry.WhoSelf && fastCue.MatchString(l.Body) {
				hit = true
			}
		}
		c.St.S.FastSeen[key] = top
		if !hit || reads+n >= fastMost {
			continue
		}
		d := triage.Decision{Chat: u.Ref, Name: u.Meta.Name, Kind: u.Meta.Kind, Rule: "fast", Action: triage.Include,
			Tier: triage.TierOwn, Shown: map[int]bool{}}
		for _, l := range u.Lines {
			d.Shown[l.N] = true
		}
		prevUpdated := make(map[string]string, len(c.St.S.Entries))
		for id, e := range c.St.S.Entries {
			prevUpdated[id] = e.Updated
		}
		for _, p := range dailyParts(u, d) {
			if err := c.batch("daily", u, p.d, p.part, 0, run, func(d triage.Decision, part string) error {
				return c.digest(ctx, metered, b, u, d, part, owner, known, run)
			}); err != nil {
				c.logf("fast path: %s: %v", u.Meta.Name, err)
				break
			}
		}
		// Mark newly observed or updated correction rules from this fast-path pass as temporary hot-slot contracts
		// (unified design §2.1: TTL 7 days, stands in Bucket 0 until L1 SyncContracts absorbs them).
		for id, e := range c.St.S.Entries {
			if prevUpdated[id] == e.Updated || !e.Live() || entry.KindOf(e) != entry.KindRules {
				continue
			}
			if strings.EqualFold(e.Type, "correction") || strings.TrimSpace(e.Rejected) != "" || entry.Hard(e) {
				entry.SetFastHot(e, day)
				run.Counts["fast_hot_marked"]++
			}
		}
		n++
	}
	c.St.S.FastSeen["@reads@"+day] = reads + n
	if n > 0 {
		run.Counts["fast_units"] = n
		run.Counts["model_calls"] = metered.Calls
		run.Finished = c.now().UTC().Format(time.RFC3339)
		c.St.S.Runs = append(c.St.S.Runs, run)
		c.logf("fast path %s: read %d conversation(s) with a correction, %d call(s)", day, n, metered.Calls)
	}
	if err := c.St.Save(); err != nil {
		return n, err
	}
	if n > 0 {
		c.Publish()
	}
	return n, nil
}

// fastReads is how many conversations the fast path read today.
func (c *Curator) fastReads(day string) int { return c.St.S.FastSeen["@reads@"+day] }
