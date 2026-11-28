package main

// The hard evaluation (design l0-leverage §2, 0.2.0-beta.52): can the assistant tell how the user chooses, from what
// the extension keeps, where a model knowing nothing of the user cannot? Questions are drawn by this program from a
// copy of the state (the user's real choices first seen after a cutoff: a situation, what they chose and what they
// turned down), never written by hand or by a model playing anyone. Three kinds:
//
//   - K1, a personal trade-off: a default judgement or a way of working that holds globally;
//   - K2, a correction of the assistant: an interaction entry of type correction (what the assistant did was turned
//     down);
//   - K3, the same logic across projects: a judgement or a way of working scoped to a project, answered with nothing
//     of that project in memory, so only what was learnt elsewhere can carry it.
//
// A candidate a filter model (no memory, both option orders) gets right both times is dropped: what a stranger gets
// right says nothing of memory. The filter is the answering model itself, its answers kept apart so the no-memory arm
// is asked afresh: with a weaker filter (haiku before sonnet) the no-memory arm still got 89% and no arm could differ.
// The arms are then answered, each question in both orders:
//
//   - none: no memory;
//   - scatter: the curated entries in memory.entries' order up to beta.49 (standing, the old rank, newest first);
//   - overview: the overview's patterns built from evidence before the cutoff (overview-backtest.json);
//   - cogpipe: the old pipeline's injected text built before the cutoff (a file, given as it is);
//   - l0: the curated entries as the new outlet picks them (entry.Score, the rules/understanding quotas, principle first);
//   - resident: the new standing set, patterns and curated entries together.
//
// Each in two budgets: what the hub's standing room holds (1400 bytes) and a wide one. Every arm leaves out the
// questions' own entries, their classes and any entry or pattern resting on the same unit and day, and everything
// first seen on or after the cutoff; for K3, everything about the question's project too. The model never scores:
// the code counts the option the user really took.
//
// Nothing here goes to a hub. The questions, answers and readings stay in -out (the user's own data: keep it local).

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	exits "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

const (
	heKindTradeoff   = "K1"
	heKindCorrection = "K2"
	heKindCross      = "K3"
	hePerKind        = 20
	heBatch          = 15
	heTight          = 1400  // bytes: the hub's standing room for the extension
	heWide           = 18000 // bytes: about 6000 characters of Chinese, the forward check's budget
)

// heQuestion is one held-out choice.
type heQuestion struct {
	ID        string `json:"id"`   // the entry it was drawn from
	Kind      string `json:"kind"` // K1 K2 K3
	Facet     string `json:"facet"`
	Project   string `json:"project,omitempty"` // K3: the project left out of memory
	Situation string `json:"situation"`
	Choice    string `json:"choice"`
	Rejected  string `json:"rejected"`
	// Leak: the entries no arm may carry for it (its own, its class, those on the same unit and day).
	Leak []string `json:"leak"`
	// Units are the ref@day of its evidence, for leaving out patterns resting on them.
	Units []string `json:"units"`
	// Easy: a stranger got it right both ways; kept only for a kind with no hard question left (K2 so far), so its
	// side can still be read for "no worse", never for a win.
	Easy bool `json:"easy,omitempty"`
}

type heQuestions struct {
	Cutoff      string       `json:"cutoff"`
	End         string       `json:"end"`
	FilterModel string       `json:"filter_model"`
	Candidates  int          `json:"candidates"`
	Dropped     int          `json:"dropped_easy"`
	Questions   []heQuestion `json:"questions"`
}

func hardevalCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: hardeval build|run|collab-build|collab-run|standing [flags]")
	}
	switch args[0] {
	case "build":
		return hardevalBuild(args[1:])
	case "run":
		return hardevalRun(args[1:])
	case "collab-build":
		return collabBuild(args[1:])
	case "collab-run":
		return collabRun(args[1:])
	case "collab-why":
		return collabWhy(args[1:])
	case "standing":
		return hardevalStanding(args[1:])
	case "l1-rebuild":
		return hardevalL1Rebuild(args[1:])
	}
	return fmt.Errorf("unknown hardeval step %q", args[0])
}

func heHash(s string) string { x := sha1.Sum([]byte(s)); return fmt.Sprintf("%x", x) }

func heProject(e *entry.Entry) string {
	for _, s := range e.Scopes {
		if s.Kind == "project" && strings.TrimSpace(s.Name) != "" {
			return strings.TrimSpace(s.Name)
		}
	}
	return ""
}

func heUnits(e *entry.Entry) []string {
	var out []string
	for _, ev := range e.Evidence {
		if u := ev.Ref + "@" + ev.Day; !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// heCandidates are the real choices first seen in [cutoff, end], by kind, in hash order (no picking).
func heCandidates(st *store.State, cutoff, end string) map[string][]*entry.Entry {
	out := map[string][]*entry.Entry{}
	for _, e := range st.Entries {
		if !memsrc.Pool(st, e) || e.Inferred || e.Grain != entry.Action || e.First < cutoff || e.First > end ||
			len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		if strings.TrimSpace(e.Situation) == "" || strings.TrimSpace(e.Choice) == "" || strings.TrimSpace(e.Rejected) == "" {
			continue
		}
		kind := ""
		switch e.Facet {
		case entry.Interaction:
			if strings.EqualFold(strings.TrimSpace(e.Type), "correction") {
				kind = heKindCorrection
			}
		case entry.Judgement, entry.Workflow:
			kind = heKindTradeoff
			if heProject(e) != "" {
				kind = heKindCross
			}
		}
		if kind != "" {
			out[kind] = append(out[kind], e)
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return heHash(out[k][i].ID) < heHash(out[k][j].ID) })
	}
	return out
}

// heLeak is what no arm may carry for e: e, its class, and every entry with evidence on one of e's units.
func heLeak(st *store.State, e *entry.Entry, byUnit map[string][]string) []string {
	set := map[string]bool{e.ID: true}
	if e.Parent != "" {
		set[e.Parent] = true
	}
	for _, u := range heUnits(e) {
		for _, id := range byUnit[u] {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func heCLI(cmd, mdl, cache string) model.Model {
	return model.CLI{Command: cmd, Models: map[model.Tier]string{model.Small: mdl, model.Medium: mdl}, CacheDir: cache,
		Limit: model.Shared()}
}

type hePick struct {
	ID   string `json:"id"`
	Pick string `json:"pick"`
}

// heAsk puts questions (id, situation, A, B) to m with memory, in batches; the picks by id.
func heAsk(ctx context.Context, m model.Model, memory string, qs []heAsked) (map[string]string, error) {
	type pq struct {
		ID        string `json:"id"`
		Situation string `json:"situation"`
		A         string `json:"A"`
		B         string `json:"B"`
	}
	picks := map[string]string{}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for b := 0; b < len(qs); b += heBatch {
		batch := qs[b:min(len(qs), b+heBatch)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := struct {
				Memory    string `json:"memory"`
				Questions []pq   `json:"questions"`
			}{Memory: memory}
			for _, q := range batch {
				in.Questions = append(in.Questions, pq{ID: q.key, Situation: heClip(q.situation, 160), A: heClip(q.a, 120), B: heClip(q.b, 120)})
			}
			raw, _ := json.Marshal(in)
			resp, err := m.Complete(ctx, model.Request{Step: "hardeval", Tier: model.Small, System: overview.PredictPrompt,
				User: string(raw), MaxTokens: 1500, Schema: overview.PredictSchema})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil && !errors.Is(err, model.ErrRefused) {
					firstErr = err
				}
				return
			}
			var out struct {
				Answers []hePick `json:"answers"`
			}
			if json.Unmarshal([]byte(resp.Text), &out) != nil {
				return
			}
			for _, a := range out.Answers {
				picks[a.ID] = a.Pick
			}
		}()
	}
	wg.Wait()
	return picks, firstErr
}

// heAsked is one question in one option order: key, what is shown, and where the user's choice is.
type heAsked struct {
	key, situation, a, b, answer string
	q                            *heQuestion
}

// heOrders are q in both orders.
func heOrders(q *heQuestion) []heAsked {
	return []heAsked{
		{key: q.ID + "#1", situation: q.Situation, a: q.Choice, b: q.Rejected, answer: "A", q: q},
		{key: q.ID + "#2", situation: q.Situation, a: q.Rejected, b: q.Choice, answer: "B", q: q},
	}
}

func hardevalBuild(args []string) error {
	fs := flag.NewFlagSet("hardeval build", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	cutoff := fs.String("cutoff", "", "first held-out day, YYYY-MM-DD: questions are choices first seen on it or later")
	end := fs.String("end", "", "last held-out day")
	out := fs.String("out", "", "directory for questions.json and the answer cache (keep it local)")
	cmd := fs.String("model-cmd", "claude", "the model command line")
	filter := fs.String("filter-model", "claude-sonnet-5-5", "the model that drops what a stranger gets right (the answering one: a weaker filter leaves what the answerer gets right anyway)")
	perKind := fs.Int("per-kind", hePerKind, "questions per kind at most")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || *cutoff == "" || *end == "" || *out == "" {
		return errors.New("-state, -cutoff, -end and -out are required")
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	byUnit := map[string][]string{}
	for _, e := range st.Entries {
		for _, u := range heUnits(e) {
			byUnit[u] = append(byUnit[u], e.ID)
		}
	}
	cands := heCandidates(st, *cutoff, *end)
	m := heCLI(*cmd, *filter, filepath.Join(*out, "filter-cache")) // apart: the no-memory arm is asked afresh
	res := heQuestions{Cutoff: *cutoff, End: *end, FilterModel: *filter}
	for _, kind := range []string{heKindTradeoff, heKindCorrection, heKindCross} {
		pool := cands[kind]
		res.Candidates += len(pool)
		// Filter in rounds of 3×perKind candidates until perKind hard ones are found or the pool runs out.
		kept := 0
		var easy []heQuestion
		for start := 0; start < len(pool) && kept < *perKind; start += 3 * *perKind {
			var qs []heQuestion
			var asked []heAsked
			for _, e := range pool[start:min(len(pool), start+3**perKind)] {
				q := heQuestion{ID: e.ID, Kind: kind, Facet: string(e.Facet), Situation: e.Situation, Choice: e.Choice,
					Rejected: e.Rejected, Leak: heLeak(st, e, byUnit), Units: heUnits(e)}
				if kind == heKindCross {
					q.Project = heProject(e)
				}
				qs = append(qs, q)
			}
			for i := range qs {
				asked = append(asked, heOrders(&qs[i])...)
			}
			picks, err := heAsk(context.Background(), m, "", asked)
			if err != nil {
				return err
			}
			for i := range qs {
				right := 0
				for _, a := range heOrders(&qs[i]) {
					if picks[a.key] == a.answer {
						right++
					}
				}
				if right == 2 {
					res.Dropped++
					qs[i].Easy = true
					easy = append(easy, qs[i])
					continue
				}
				if kept < *perKind {
					res.Questions = append(res.Questions, qs[i])
					kept++
				}
			}
		}
		if kept == 0 {
			res.Questions = append(res.Questions, easy[:min(len(easy), *perKind)]...)
		}
	}
	b, _ := json.MarshalIndent(res, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "questions.json"), b, 0o600); err != nil {
		return err
	}
	fmt.Printf("hardeval build: %d candidates, %d dropped as easy, %d questions (%s)\n", res.Candidates, res.Dropped,
		len(res.Questions), heCount(res.Questions))
	return nil
}

func heCount(qs []heQuestion) string {
	n := map[string]int{}
	for _, q := range qs {
		n[q.Kind]++
	}
	return fmt.Sprintf("K1 %d, K2 %d, K3 %d", n[heKindTradeoff], n[heKindCorrection], n[heKindCross])
}

// heFilter is what one memory variant leaves out.
type heFilter struct {
	leak    map[string]bool
	units   map[string]bool
	project string
}

func (f heFilter) entryOut(st *store.State, e *entry.Entry, cutoff string) bool {
	if f.leak[e.ID] || (cutoff != "" && e.First >= cutoff) {
		return true
	}
	for _, u := range heUnits(e) {
		if f.units[u] {
			return true
		}
	}
	if f.project != "" {
		if heProject(e) == f.project || strings.Contains(e.Shown(), f.project) {
			return true
		}
	}
	return false
}

func (f heFilter) patternOut(st *store.State, p *overview.Pattern, cutoff string) bool {
	sawEntry := false
	for _, id := range p.EvidenceIDs {
		if f.leak[id] {
			return true
		}
		if e := st.Entries[id]; e != nil {
			sawEntry = true
			if f.entryOut(st, e, cutoff) {
				return true
			}
		}
	}
	if !sawEntry && cutoff != "" && p.LastEvidence >= cutoff {
		return true
	}
	return f.project != "" && strings.Contains(p.Text, f.project)
}

func heFit(lines []string, budget int) string {
	var b strings.Builder
	for _, l := range lines {
		if b.Len()+len(l)+1 > budget {
			continue
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// heMemories are the arms' memories for one filter, in one budget.
func heMemories(st *store.State, ov *overview.State, cog []string, f heFilter, cutoff string, budget int) map[string]string {
	mem := map[string]string{"none": ""}
	tpl := entry.TemplateQuotes(st.Entries)
	// scatter: memory.entries' order up to beta.49.
	var pool []*entry.Entry
	for _, e := range st.Entries {
		if memsrc.Pool(st, e) && len(e.Cats()) == 0 && e.OriginSpace == "" && !f.entryOut(st, e, cutoff) {
			pool = append(pool, e)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		if sa, sb := exits.Usable(st, a), exits.Usable(st, b); sa != sb {
			return sa
		}
		if ra, rb := memsrc.LegacyRank(a), memsrc.LegacyRank(b); ra != rb {
			return ra > rb
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		return a.ID < b.ID
	})
	var lines []string
	size := 0
	for _, e := range pool {
		if size > budget {
			break
		}
		l := "- " + heClip(e.Shown(), 200)
		lines = append(lines, l)
		size += len(l) + 1
	}
	mem["scatter"] = heFit(lines, budget)
	// overview: the backtest's used patterns.
	var ovCands []memsrc.Candidate
	absorbed := map[string]bool{}
	coveredByPat := map[string][]string{}
	var contractTexts []string
	l1LatestDay := ""
	lines = lines[:0]
	if ov != nil {
		for _, p := range ov.Sorted(func(p *overview.Pattern) bool {
			return p.Used() && len(p.Categories) == 0 && p.OriginSpace == "" && !f.patternOut(st, p, cutoff)
		}) {
			lines = append(lines, "- "+overview.Text(p))
			if k := entry.KindOfFacet(p.Facet); k != "" && !entry.TemplateBacked(st.Entries, p.EvidenceIDs, tpl) {
				if p.LastEvidence > l1LatestDay {
					l1LatestDay = p.LastEvidence
				}
				bucket := memsrc.BucketPrinciple
				maxBytes := memsrc.PrincipleTextMost
				if p.IsContract() {
					bucket = memsrc.BucketContract
					maxBytes = memsrc.ContractTextMost
				}
				body, suffix := overview.StandingParts(p)
				quotes := p.UniqueQuotes
				if quotes <= 0 {
					quotes = p.SourcesCount
				}
				prefix := memsrc.StandingPrefix(bucket, p.Hard, p.Polarity)
				txt := memsrc.FormatResidentLine(prefix+body, suffix, maxBytes)
				ovCands = append(ovCands, memsrc.Candidate{
					ID:        p.ID,
					Text:      txt,
					Kind:      k,
					Bucket:    bucket,
					Facet:     p.Facet,
					L1:        true,
					Hard:      p.Hard,
					Score:     overview.Score(p),
					Days:      quotes,
					Last:      p.LastEvidence,
					Polarity:  p.Polarity,
					Dimension: p.Dimension,
				})
				if len(p.CoveredIDs) > 0 {
					coveredByPat[p.ID] = p.CoveredIDs
				}
				if bucket == memsrc.BucketContract {
					for _, id := range p.EvidenceIDs {
						absorbed[id] = true
					}
					for _, id := range p.CoveredIDs {
						absorbed[id] = true
					}
					contractTexts = append(contractTexts, txt)
				}
			}
		}
	}
	mem["overview"] = heFit(lines, budget)
	// cogpipe: its injected text as it is, without lines naming the left-out project.
	lines = lines[:0]
	for _, l := range cog {
		if f.project != "" && strings.Contains(l, f.project) {
			continue
		}
		lines = append(lines, l)
	}
	mem["cogpipe"] = heFit(lines, budget)
	// l0 and resident: the new outlet's choice (mirroring memsrc.workStandingSet, v0.4.0 three-section + hot-slot).
	var l0Cands, hotCands []memsrc.Candidate
	cutoffTime, _ := time.Parse("2006-01-02", cutoff)
	for _, e := range pool {
		if !cutoffTime.IsZero() {
			hc, ok := memsrc.HotSlotCandidate(st, e, tpl, cutoffTime)
			if !ok && l1LatestDay != "" {
				hc, ok = memsrc.UnabsorbedRuleHotCandidate(st, e, tpl, l1LatestDay, cutoffTime)
			}
			if ok && (hc.Pinned || (!absorbed[e.ID] && !slices.ContainsFunc(contractTexts, func(t string) bool { return norm.Jaccard(t, hc.Text) >= 0.45 }))) {
				hotCands = append(hotCands, hc)
			}
		}
		if c, ok := memsrc.CuratedCandidate(st, e, tpl); ok {
			l0Cands = append(l0Cands, c)
		}
	}
	most := memsrc.ResidentMostFor(budget)
	join := func(cs []memsrc.Candidate) string {
		var ls []string
		for _, c := range cs {
			ls = append(ls, "- "+c.Text)
		}
		return heFit(ls, budget)
	}
	mem["l0"] = join(memsrc.ResidentWithin(l0Cands, budget, most))
	resCands := append(slices.Clone(l0Cands), ovCands...)
	resCands = append(resCands, hotCands...)
	_ = coveredByPat
	mem["resident"] = join(memsrc.ResidentWithin(resCands, budget, most))
	return mem
}

var heArms = []string{"none", "scatter", "overview", "cogpipe", "l0", "resident"}

type heResult struct {
	Model  string                                   `json:"model"`
	Cutoff string                                   `json:"cutoff"`
	N      map[string]int                           `json:"n"`
	Score  map[string]map[string]map[string]float64 `json:"score"` // budget → arm → kind (and "all", "understanding") → share right
	Chars  map[string]map[string]int                `json:"bytes"` // budget → arm → bytes of the global memory
	PerQ   map[string]map[string]map[string]int     `json:"per_q"` // budget → arm → question → right (0–2)
	Notes  []string                                 `json:"notes,omitempty"`
}

func hardevalRun(args []string) error {
	fs := flag.NewFlagSet("hardeval run", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	ovPath := fs.String("overview", "", "patterns built before the cutoff (overview-backtest.json)")
	cogPath := fs.String("cogpipe", "", "the old pipeline's injected text built before the cutoff (optional)")
	out := fs.String("out", "", "the directory hardeval build wrote")
	cmd := fs.String("model-cmd", "claude", "the model command line")
	mdl := fs.String("model", "claude-sonnet-5-5", "the model that answers")
	label := fs.String("label", "baseline", "name of this reading (results-<label>.json)")
	arms := fs.String("arms", strings.Join(heArms, ","), "arms to answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(*out, "questions.json"))
	if err != nil {
		return err
	}
	var qs heQuestions
	if err := json.Unmarshal(raw, &qs); err != nil {
		return err
	}
	var ov *overview.State
	if *ovPath != "" {
		b, err := os.ReadFile(*ovPath)
		if err != nil {
			return err
		}
		ov = &overview.State{}
		if err := json.Unmarshal(b, ov); err != nil {
			return err
		}
	}
	var cog []string
	if *cogPath != "" {
		b, err := os.ReadFile(*cogPath)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) != "" {
				cog = append(cog, l)
			}
		}
	}
	// The filters: one for K1 and K2 together (every question's leak), one per K3 project.
	all := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	for _, q := range qs.Questions {
		for _, id := range q.Leak {
			all.leak[id] = true
		}
		for _, u := range q.Units {
			all.units[u] = true
		}
	}
	groups := map[string][]*heQuestion{}
	for i := range qs.Questions {
		q := &qs.Questions[i]
		groups[q.Project] = append(groups[q.Project], q)
	}
	m := heCLI(*cmd, *mdl, filepath.Join(*out, "cache"))
	res := heResult{Model: *mdl, Cutoff: qs.Cutoff, N: map[string]int{}, Score: map[string]map[string]map[string]float64{},
		Chars: map[string]map[string]int{}, PerQ: map[string]map[string]map[string]int{}}
	for _, q := range qs.Questions {
		res.N[q.Kind]++
	}
	want := strings.Split(*arms, ",")
	samples := map[string]string{}
	for _, budget := range []int{heTight, heWide} {
		bk := fmt.Sprint(budget)
		res.Score[bk], res.Chars[bk], res.PerQ[bk] = map[string]map[string]float64{}, map[string]int{}, map[string]map[string]int{}
		for project, gq := range groups {
			f := all
			if project != "" {
				f = heFilter{leak: all.leak, units: all.units, project: project}
			}
			mems := heMemories(st, ov, cog, f, qs.Cutoff, budget)
			for _, arm := range want {
				if project == "" {
					res.Chars[bk][arm] = len(mems[arm])
					samples[bk+"/"+arm] = mems[arm]
				}
				if arm != "none" && strings.TrimSpace(mems[arm]) == "" {
					res.Notes = append(res.Notes, fmt.Sprintf("%s/%s: empty memory for group %q", bk, arm, project))
				}
				var asked []heAsked
				for _, q := range gq {
					asked = append(asked, heOrders(q)...)
				}
				picks, err := heAsk(context.Background(), m, mems[arm], asked)
				if err != nil {
					return fmt.Errorf("%s/%s: %w", bk, arm, err)
				}
				if res.PerQ[bk][arm] == nil {
					res.PerQ[bk][arm] = map[string]int{}
				}
				for _, a := range asked {
					if picks[a.key] == a.answer {
						res.PerQ[bk][arm][a.q.ID]++
					}
				}
			}
		}
		for _, arm := range want {
			sc := map[string]float64{}
			tot := map[string]int{}
			for _, q := range qs.Questions {
				r := float64(res.PerQ[bk][arm][q.ID]) / 2
				for _, k := range heBuckets(q.Kind) {
					sc[k] += r
					tot[k]++
				}
			}
			for k := range sc {
				sc[k] /= float64(tot[k])
			}
			res.Score[bk][arm] = sc
		}
	}
	b, _ := json.MarshalIndent(res, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "results-"+*label+".json"), b, 0o600); err != nil {
		return err
	}
	sb, _ := json.MarshalIndent(samples, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "memories-"+*label+".json"), sb, 0o600); err != nil {
		return err
	}
	fmt.Print(heTable(res, qs.Questions, want))
	return os.WriteFile(filepath.Join(*out, "results-"+*label+".md"), []byte(heTable(res, qs.Questions, want)), 0o600)
}

// heBuckets are the readings a question counts in: its kind, all, and understanding (K1, K3) or rules (K2).
func heBuckets(kind string) []string {
	side := "understanding"
	if kind == heKindCorrection {
		side = "rules"
	}
	return []string{kind, "all", side}
}

// heTable is the readings as a table, and each arm's wins, ties and losses against l0 question by question.
func heTable(res heResult, qs []heQuestion, arms []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hard evaluation, model %s, cutoff %s, n %v\n", res.Model, res.Cutoff, res.N)
	for _, budget := range []int{heTight, heWide} {
		bk := fmt.Sprint(budget)
		fmt.Fprintf(&b, "\n### budget %s bytes\n\n| arm | bytes | K1 | K2 | K3 | understanding | rules | all | vs l0 (win/tie/loss for l0) |\n|---|---|---|---|---|---|---|---|---|\n", bk)
		for _, arm := range arms {
			s := res.Score[bk][arm]
			w, t, l := 0, 0, 0
			for _, q := range qs {
				a, o := res.PerQ[bk]["l0"][q.ID], res.PerQ[bk][arm][q.ID]
				switch {
				case a > o:
					w++
				case a < o:
					l++
				default:
					t++
				}
			}
			fmt.Fprintf(&b, "| %s | %d | %.0f%% | %.0f%% | %.0f%% | %.0f%% | %.0f%% | %.0f%% | %d/%d/%d |\n", arm, res.Chars[bk][arm],
				100*s[heKindTradeoff], 100*s[heKindCorrection], 100*s[heKindCross], 100*s["understanding"], 100*s["rules"], 100*s["all"], w, t, l)
		}
	}
	for _, n := range res.Notes {
		fmt.Fprintln(&b, "- note:", n)
	}
	return b.String()
}

func heClip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}
