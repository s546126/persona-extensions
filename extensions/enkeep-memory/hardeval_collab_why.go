package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	expt "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
)

// collabWhy prints, for each named incident, the entries closest to what it needed (by shared character pairs with
// its situation, mistake and want), whether each was in the standing set and the scatter memory of the day before,
// and why not: a diagnosis for the incidents the standing set lost, read only.
func collabWhy(args []string) error {
	fs := flag.NewFlagSet("hardeval collab-why", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	ovPath := fs.String("overview", "", "patterns built before the cutoff (overview-backtest.json)")
	out := fs.String("out", "", "the directory collab-build wrote")
	ids := fs.String("ids", "", "incident ids, comma separated")
	budget := fs.Int("budget", 4096, "memory budget in bytes")
	top := fs.Int("top", 8, "closest entries shown per incident")
	diff := fs.Bool("diff", false, "also print the scatter lines of each incident's day that did not stand, with why")
	show := fs.Bool("show", false, "also print the standing set of each incident's day, with scores")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || *out == "" || *ids == "" {
		return errors.New("-state, -out and -ids are required")
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(*out, "collab.json"))
	if err != nil {
		return err
	}
	var set collabSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return err
	}
	ov, _, err := heInputs(*ovPath, "")
	if err != nil {
		return err
	}
	tpl := entry.TemplateQuotes(st.Entries)
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	for _, id := range strings.Split(*ids, ",") {
		i := slices.IndexFunc(set.Incidents, func(x collabIncident) bool { return x.ID == id })
		if i < 0 {
			fmt.Printf("# %s: no such incident\n", id)
			continue
		}
		inc := set.Incidents[i]
		mems := heMemories(st, ov, nil, f, inc.Day, *budget)
		want := whyPairs(inc.Situation + inc.Mistake + inc.Want)
		type hit struct {
			e     *entry.Entry
			score float64
		}
		var hits []hit
		for _, e := range st.Entries {
			if !e.Live() || f.entryOut(st, e, inc.Day) {
				continue
			}
			p := whyPairs(e.Shown())
			if len(p) == 0 {
				continue
			}
			n := 0
			for k := range p {
				if want[k] {
					n++
				}
			}
			hits = append(hits, hit{e, float64(n) / float64(len(p)+8)})
		}
		sort.Slice(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
		if *show {
			var cands []memsrc.Candidate
			for _, e := range st.Entries {
				if memsrc.Pool(st, e) && len(e.Cats()) == 0 && e.OriginSpace == "" && !f.entryOut(st, e, inc.Day) {
					if c, ok := memsrc.CuratedCandidate(st, e, tpl); ok {
						cands = append(cands, c)
					}
				}
			}
			n := map[int]int{}
			for _, c := range cands {
				n[c.Score/10*10]++
			}
			fmt.Printf("## %s candidates by score band: %v\n", inc.Day, n)
			for _, c := range memsrc.ResidentWithin(cands, *budget, memsrc.ResidentMostFor(*budget)) {
				fmt.Printf("  [%s %d %s] %s\n", c.Kind, c.Score, c.ID, heClip(c.Text, 90))
			}
		}
		if *diff {
			fmt.Printf("## %s scatter lines not standing:\n", inc.Day)
			for _, e := range st.Entries {
				l := heClip(e.Shown(), 200)
				if !strings.Contains(mems["scatter"], "- "+l+"\n") {
					continue
				}
				c, ok := memsrc.CuratedCandidate(st, e, tpl)
				if ok && strings.Contains(mems["resident"], "- "+c.Text+"\n") {
					continue
				}
				lv := e.Leverage
				reach, hor := "", ""
				if lv != nil {
					reach, hor = lv.Reach, lv.Horizon
				}
				fmt.Printf("  %s %s/%s grain=%s kind=%q score=%d curated=%v reach=%q horizon=%q count=%d hard=%v origin=%s | %s\n", e.ID, e.Facet,
					e.Status, e.Grain, entry.KindOf(e), entry.Score(e), ok, reach, hor, e.Count, entry.Hard(e), e.Origin, heClip(e.Shown(), 60))
			}
		}
		fmt.Printf("# %s %s\nmistake: %s\nwant: %s\n", inc.ID, inc.Day, inc.Mistake, inc.Want)
		for _, h := range hits[:min(*top, len(hits))] {
			e := h.e
			c, ok := memsrc.CuratedCandidate(st, e, tpl)
			text := memsrc.ResidentText(e)
			inRes := ok && strings.Contains(mems["resident"], "- "+c.Text+"\n")
			inSc := strings.Contains(mems["scatter"], heClip(e.Shown(), 40))
			why := "in"
			switch {
			case inRes:
			case entry.KindOf(e) == "":
				why = "no kind (" + string(e.Facet) + "/" + e.Type + ")"
			case !expt.Usable(st, e):
				why = "not settled (" + string(e.Status) + ")"
			case !memsrc.Pool(st, e):
				why = "not in the pool (" + string(e.Status) + ")"
			case !ok:
				why = "template or tagged"
			default:
				why = fmt.Sprintf("ranked out (score %d)", c.Score)
			}
			fmt.Printf("  %.2f res=%v sc=%v %s/%s %s score=%d first=%s days=%d | %s | %s\n", h.score, inRes, inSc, e.Facet, e.Status,
				e.ID, entry.Score(e), e.First, len(e.ActiveDays), why, heClip(text, 110))
		}
	}
	return nil
}

// whyPairs are the character pairs of s, without spaces and punctuation.
func whyPairs(s string) map[string]bool {
	var rs []rune
	for _, r := range s {
		if r > 0x2fff || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			rs = append(rs, r)
		}
	}
	out := map[string]bool{}
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = true
	}
	return out
}
