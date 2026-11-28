package main

// The collaboration evaluation (design l0-leverage §11, 0.2.0-beta.52): with the memory, does the assistant avoid a
// collaboration incident that really happened (the user corrected it, swore at it, or had to say a thing again), and
// does it ask less, do less wrong, make the user repeat less? Understanding and the assistant's rules are judged
// together, by the outcome.
//
// Incidents are drawn by this program, never written by hand:
//
//   - the assistant's feedback memories (a directory of Markdown files with `type: feedback`, each written right after
//     an incident, with its day and the user's words), dated in [cutoff, end];
//   - the assistant's session transcripts (Claude Code JSONL): a user line in [cutoff, end] that reads as a
//     correction (a fixed list of cue words), with the request before it and what the assistant did in between.
//
// A model turns each candidate into a situation (what was asked and how things stood before the assistant's move,
// without the right move in it), the mistake and what the user wanted, or drops it as no incident. An incident a
// model without memory already handles is dropped (that model is the answering one; its answers are kept apart so the
// no-memory arm is asked afresh). Each arm's memory (hardeval's arms: none, scatter, overview, cogpipe, l0, resident,
// built from what was seen before the cutoff) then answers every situation, and another model judges each answer
// blind to the arm against the incident. Nothing here goes to a hub; the data stays in -out.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// collabCue is what a user's correction reads like (the candidates; a model decides).
var collabCue = regexp.MustCompile(`不是|别再|不要再|又|说过|说了|重复|第[二三四五六]遍|为什么还|怎么还|他妈|错了|搞错|我说的是|早就|不用问|别问|别来问|直接做|直接往下|直接干|废话|脱裤子|搞混|弄混|搞错人|认错|张冠李戴|不是他|不是她|谁负责|他负责|她负责|负责人|哪个项目|不是这个项目|不是那个|哪个仓库|搞错仓库|错仓库|派错|找错人|属于哪个|哪台机器|哪个主管|归谁`)

// collabEntityCue targets incidents involving entity ownership, roles, projects, repositories, machines or dependencies
// (unified design §3.2: entity ablation set expansion).
var collabEntityCue = regexp.MustCompile(`搞混|弄混|搞错人|认错|张冠李戴|不是他|不是她|谁负责|他负责|她负责|负责人|哪个项目|不是这个项目|不是那个|哪个仓库|搞错仓库|错仓库|哪个库|依赖|归属|派错|找错人|找错|谁的|属于哪个|哪个服务|哪台机器|主机名|搞错机器|哪个主管|错主管|谁管|归谁`)

var collabDay = regexp.MustCompile(`\b(1[0-2]|0?[1-9])-([0-3][0-9])\b`)

const (
	collabBatch            = 6
	collabMost             = 60 // incidents kept at most
	collabCandMost         = 160
	collabEntityCandMost   = 320
	collabEntityDFMost     = 100 // max World/People DF for a specific entity name in entity-ablation indexing
	collabCtxMost          = 1800
	collabStepModel        = "hardeval-collab"
	decisionsQueryMaxBytes = 3072 // hub decisions_query truncation cap (entries_sources.go)
	armDecisionsQuerySim   = "decisions-query-sim"
)

// collabWindow is an inclusive [Start, End] date range (YYYY-MM-DD).
type collabWindow struct {
	Start string
	End   string
}

func (w collabWindow) String() string { return w.Start + ".." + w.End }

func (w collabWindow) contains(day string) bool { return day >= w.Start && day <= w.End }

func parseCollabWindows(spec, cutoff, end string) ([]collabWindow, error) {
	if strings.TrimSpace(spec) == "" {
		if cutoff == "" || end == "" {
			return nil, errors.New("-windows or both -cutoff and -end are required")
		}
		if cutoff > end {
			return nil, fmt.Errorf("invalid window %s..%s", cutoff, end)
		}
		return []collabWindow{{Start: cutoff, End: end}}, nil
	}
	var ws []collabWindow
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bounds := strings.Split(part, "..")
		if len(bounds) != 2 || len(bounds[0]) != 10 || len(bounds[1]) != 10 || bounds[0] > bounds[1] {
			return nil, fmt.Errorf("invalid window %q (want YYYY-MM-DD..YYYY-MM-DD)", part)
		}
		ws = append(ws, collabWindow{Start: bounds[0], End: bounds[1]})
	}
	if len(ws) == 0 {
		return nil, errors.New("empty -windows")
	}
	return ws, nil
}

func windowFor(day string, ws []collabWindow) (string, bool) {
	for _, w := range ws {
		if w.contains(day) {
			return w.String(), true
		}
	}
	return "", false
}

type collabCand struct {
	ID        string   `json:"id"`
	Source    string   `json:"source"` // memory | session | state:<S1|S2|S3|S6|S8>
	Day       string   `json:"day"`
	Window    string   `json:"window,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Text      string   `json:"text"`
	Leak      []string `json:"leak,omitempty"`
	Units     []string `json:"units,omitempty"`
}

// collabIncident is one incident as the model wrote it from a candidate.
type collabIncident struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Day       string `json:"day"`
	Window    string `json:"window,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Kind      string `json:"kind"` // corrected | scolded | repeated
	Situation string `json:"situation"`
	Mistake   string `json:"mistake"`
	Want      string `json:"want"`
	// Entity: avoiding it needed concrete World/People entity facts or relations (who someone is/owns/contacts,
	// or project/repo/service/machine/channel ownership, topology, dependencies, location or entity-bound rules).
	Entity bool `json:"entity,omitempty"`
	// PriorMemory marks incidents for which state.json already held a matching pre-incident rule/principle
	// (First < Day, outside Leak/Units), separating solvable recurrences from first-time novel mistakes.
	PriorMemory bool `json:"prior_memory,omitempty"`
	// Leak and Units carry source entry/class IDs and ref@day units (for state.json-sourced incidents) so collab-run
	// excludes them from every arm's memory in addition to the day cutoff.
	Leak  []string `json:"leak,omitempty"`
	Units []string `json:"units,omitempty"`
	// Recall is what the arm's recall brought in for this incident (collabRecall or hub decisions_query), shown to the answer only.
	Recall      string `json:"-"`
	RecallLabel string `json:"-"`
}

type collabSet struct {
	Cutoff        string           `json:"cutoff"`
	End           string           `json:"end"`
	Windows       []string         `json:"windows,omitempty"`
	EntityOnly    bool             `json:"entity_only,omitempty"`
	Candidates    int              `json:"candidates"`
	Excluded      int              `json:"excluded,omitempty"`
	NoIncident    int              `json:"no_incident"`
	Duplicates    int              `json:"duplicates"`
	NoPriorMemory int              `json:"no_prior_memory,omitempty"`
	Avoided       int              `json:"avoided_without_memory"`
	Incidents     []collabIncident `json:"incidents"`
}

const collabExtractPrompt = `你在从用户和 AI 助手的真实记录里整理「协作事故」：用户纠正过助手、骂过助手、不得不把一件事重复交代，或者在跨渠道（飞书、微信、知识库、Agent 会话等）记录中对涉及具体人与项目的错误默认做法作出明确纠偏。每条材料是一份助手事后写的反馈记忆、一段会话（用户的请求、助手的动作、用户随后的话），或一条带用户原话的结构化渠道记录（情境、用户实际决定/要求、被否决做法、用户原话）。

对每条材料判断它是不是一次协作事故（incident）。是的话写出：
- kind：corrected（纠正）、scolded（骂了）、repeated（重复交代）之一；
- situation：事故发生前的情境，以助手的视角写：用户要什么、事情进行到哪一步、助手手上有什么。只写事实，不能透露正确做法，也不能写出助手后来做错了什么或用户的反应。必须严格落在 mistake 发生之前的决策岔口，且主题与 mistake 直接对应。80–200 字。人名、项目名、系统名照材料原样写（评测只在本机，不进仓库），不写邮箱、账号、密钥。
- mistake：助手当时怎么做错了（或若按被否决做法会怎么错），一句话。
- want：用户真正要的是什么样的做法，一句话，写成能推广到同类情境的做法。
- entity：要避开这次事故，是否必须知道具体实体（人、项目、仓库、服务、机器或频道）的事实或关系（对应 World/People 记忆：某人是谁/职权分工/联络对象，或某个项目/仓库/机器/服务的归属、拓扑、依赖、位置与实体绑定约定）。是为 true；只靠与具体实体无关的通用做事习惯就能避开的为 false。

不是协作事故的（只是普通追问、闲聊、用户改主意、技术报错、没有明确错误默认做法的普通纪要，或者材料只包含事后追问/答完卡片后的后续轮次、前后轮次主题错位而无法还原 mistake 发生前的决策岔口）incident 为 false，其余字段留空。输出 JSON。`

const collabExtractSchema = `{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","incident","kind","situation","mistake","want","entity"],"properties":{"id":{"type":"string"},"incident":{"type":"boolean"},"entity":{"type":"boolean"},"kind":{"type":"string","enum":["","corrected","scolded","repeated"]},"situation":{"type":"string"},"mistake":{"type":"string"},"want":{"type":"string"}}}}}}`

const collabAnswerPrompt = `你是这位用户的主 AI 助手：能和用户对话、能派 Agent 干活、能执行命令和发授权卡。下面给你对这位用户的记忆（可能为空）和若干个互相独立的情境。对每个情境，写出你此刻实际会怎么做：如果要回复用户，先写那句回复；再写你要执行的动作。具体、简短，每个不超过 150 字。只依据记忆和情境本身。输出 JSON。`

const collabAnswerSchema = `{"type":"object","additionalProperties":false,"required":["answers"],"properties":{"answers":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","response"],"properties":{"id":{"type":"string"},"response":{"type":"string"}}}}}}`

const collabJudgePrompt = `你在评判 AI 助手在一个真实协作情境里的应对。每条给你：情境、当时真实发生的事故（助手怎么做错了）、用户真正要的做法、以及一份待评的应对（你不知道它来自哪个版本）。判断：
- avoided：这份应对是否避开了这次事故，并且做法符合用户要的（true/false）；
- needless_ask：是否去问了用户本不需要问的事、或请用户确认他已经同意过的事；
- wrong_action：是否做了用户不要的事；
- repeat：照这份应对，用户是否还得把这件事再说一遍。
只按应对文字判断，不因为写得长或客气加分。输出 JSON。`

const collabJudgeSchema = `{"type":"object","additionalProperties":false,"required":["verdicts"],"properties":{"verdicts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","avoided","needless_ask","wrong_action","repeat"],"properties":{"id":{"type":"string"},"avoided":{"type":"boolean"},"needless_ask":{"type":"boolean"},"wrong_action":{"type":"boolean"},"repeat":{"type":"boolean"}}}}}}`

// collabContextBlock strips Persona/Jarvis context wrappers prepended to real user messages in managed sessions
// (e.g. supervisor and agent transcripts under ~/.claude/projects/), without touching pure system tags.
var collabContextBlock = regexp.MustCompile(`(?s)<(?:persona-context|jarvis-context)\b[^>]*>.*?</(?:persona-context|jarvis-context)>`)

// collabMemoryCands are the feedback memories dated in [cutoff, end].
func collabMemoryCands(dir, cutoff, end string) ([]collabCand, error) {
	return collabMemoryCandsWindows(dir, []collabWindow{{Start: cutoff, End: end}})
}

func collabMemoryCandsWindows(dir string, ws []collabWindow) ([]collabCand, error) {
	return collabMemoryCandsInDir(dir, ws, true)
}

func collabMemoryCandsInDir(dir string, ws []collabWindow, legacyID bool) ([]collabCand, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	year := ws[0].Start[:4]
	parent := filepath.Base(filepath.Dir(filepath.Clean(dir)))
	var out []collabCand
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		s := string(raw)
		if !strings.Contains(s, "type: feedback") {
			continue
		}
		body := s
		if parts := strings.SplitN(s, "\n---", 2); strings.HasPrefix(s, "---") && len(parts) == 2 {
			body = parts[1]
		}
		m := collabDay.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		mo, _ := strconv.Atoi(m[1])
		dd, _ := strconv.Atoi(m[2])
		day := fmt.Sprintf("%s-%02d-%02d", year, mo, dd)
		win, ok := windowFor(day, ws)
		if !ok {
			continue
		}
		key := filepath.Base(f)
		if !legacyID {
			key = filepath.Join(parent, key)
		}
		out = append(out, collabCand{ID: "m-" + heHash(key)[:10], Source: "memory", Day: day, Window: win, Text: heClip(body, 2400)})
	}
	return out, nil
}

// discoverCollabDirs resolves session directories and memory directories from explicit comma-separated flags and an
// optional sessionsRoot (e.g. ~/.claude/projects). When includeSubstrs is non-empty, only subdirectories under
// sessionsRoot whose basename contains at least one substring (case-insensitive) are included.
func discoverCollabDirs(memSpec, sessionsSpec, sessionsRoot, includeSpec string) (sessDirs []string, memDirs []string, explicitMem map[string]bool, err error) {
	explicitMem = map[string]bool{}
	seenSess := map[string]bool{}
	seenMem := map[string]bool{}
	addSess := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		c := filepath.Clean(d)
		if !seenSess[c] {
			seenSess[c] = true
			sessDirs = append(sessDirs, c)
		}
	}
	addMem := func(d string, explicit bool) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		c := filepath.Clean(d)
		if explicit {
			explicitMem[c] = true
		}
		if !seenMem[c] {
			seenMem[c] = true
			memDirs = append(memDirs, c)
		}
	}
	for _, d := range strings.Split(memSpec, ",") {
		addMem(d, true)
	}
	for _, d := range strings.Split(sessionsSpec, ",") {
		addSess(d)
	}
	if root := strings.TrimSpace(sessionsRoot); root != "" {
		var filters []string
		for _, p := range strings.Split(includeSpec, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				filters = append(filters, p)
			}
		}
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			name := ent.Name()
			if len(filters) > 0 {
				low := strings.ToLower(name)
				matched := false
				for _, f := range filters {
					if strings.Contains(low, f) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			sub := filepath.Join(root, name)
			addSess(sub)
			if info, serr := os.Stat(filepath.Join(sub, "memory")); serr == nil && info.IsDir() {
				addMem(filepath.Join(sub, "memory"), false)
			}
		}
	}
	return sessDirs, memDirs, explicitMem, nil
}

// collabLine is one line of a transcript: a user's own text, or the assistant's text and calls.
// legacyIdx keeps the pre-context-stripping line index so existing dev-set incident IDs remain stable; ctxLine > 0
// marks a user line recovered after stripping a <persona-context> / <jarvis-context> wrapper.
type collabLine struct {
	user      bool
	day       string
	text      string
	legacyIdx int
	ctxLine   int
}

func cleanCollabUserText(raw string) (text string, fromContext bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if !strings.HasPrefix(trimmed, "<") {
		return raw, false
	}
	stripped := strings.TrimSpace(collabContextBlock.ReplaceAllString(trimmed, ""))
	if stripped == "" || strings.HasPrefix(stripped, "<") {
		return "", false
	}
	return stripped, true
}

func collabRead(path string) ([]collabLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []collabLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	legacyIdx := 0
	rawLine := 0
	for sc.Scan() {
		rawLine++
		var j struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			IsMeta    bool   `json:"isMeta"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &j) != nil || j.IsMeta || (j.Type != "user" && j.Type != "assistant") {
			continue
		}
		day := ""
		if len(j.Timestamp) >= 10 {
			day = j.Timestamp[:10] // UTC; close enough for a cutoff a week back
		}
		var text string
		if json.Unmarshal(j.Message.Content, &text) == nil {
			if j.Type == "user" {
				if cleaned, fromCtx := cleanCollabUserText(text); cleaned != "" {
					if fromCtx {
						out = append(out, collabLine{user: true, day: day, text: cleaned, legacyIdx: -1, ctxLine: rawLine})
					} else {
						out = append(out, collabLine{user: true, day: day, text: cleaned, legacyIdx: legacyIdx})
						legacyIdx++
					}
				}
			}
			continue
		}
		var parts []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(j.Message.Content, &parts) != nil {
			continue
		}
		var b strings.Builder
		var legacyB strings.Builder
		usedCtx := false
		for _, p := range parts {
			switch {
			case p.Type == "text" && j.Type == "user":
				if cleaned, fromCtx := cleanCollabUserText(p.Text); cleaned != "" {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(cleaned)
					if fromCtx {
						usedCtx = true
					} else {
						legacyB.WriteString(p.Text)
					}
				}
			case p.Type == "text" && j.Type == "assistant":
				b.WriteString(p.Text + "\n")
				legacyB.WriteString(p.Text + "\n")
			case p.Type == "tool_use":
				s := "[" + p.Name + " " + heClip(string(p.Input), 160) + "]\n"
				b.WriteString(s)
				legacyB.WriteString(s)
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			if j.Type == "user" && usedCtx && strings.TrimSpace(legacyB.String()) == "" {
				out = append(out, collabLine{user: true, day: day, text: t, legacyIdx: -1, ctxLine: rawLine})
			} else {
				out = append(out, collabLine{user: j.Type == "user", day: day, text: t, legacyIdx: legacyIdx})
				legacyIdx++
			}
		}
	}
	return out, sc.Err()
}

// collabSessionCands are the user lines in [cutoff, end] that read as a correction, with their context.
func collabSessionCands(dirs []string, cutoff, end string) ([]collabCand, error) {
	return collabSessionCandsWindows(dirs, []collabWindow{{Start: cutoff, End: end}}, collabCue)
}

func collabSessionCandsWindows(dirs []string, ws []collabWindow, cue *regexp.Regexp) ([]collabCand, error) {
	if cue == nil {
		cue = collabCue
	}
	var out []collabCand
	seenFiles := map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			cf := filepath.Clean(f)
			if seenFiles[cf] {
				continue
			}
			seenFiles[cf] = true
			lines, err := collabRead(f)
			if err != nil {
				return nil, err
			}
			sid := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			for i, l := range lines {
				if !l.user || len(l.text) > 1500 || !cue.MatchString(l.text) {
					continue
				}
				win, ok := windowFor(l.day, ws)
				if !ok {
					continue
				}
				// The request before: the last user line before the assistant's move; the move: what is between.
				req, move := -1, ""
				var mv []string
				for k := i - 1; k >= 0; k-- {
					if lines[k].user {
						req = k
						break
					}
					mv = append([]string{lines[k].text}, mv...)
				}
				if req < 0 || len(mv) == 0 {
					continue
				}
				move = strings.Join(mv, "\n")
				text := "用户的请求：" + heClip(lines[req].text, 600) + "\n助手的动作：" + heClip(move, collabCtxMost) +
					"\n用户随后说：" + heClip(l.text, 600)
				idKey := f + fmt.Sprint(l.legacyIdx)
				if l.ctxLine > 0 {
					idKey = fmt.Sprintf("%s:ctx:%d", f, l.ctxLine)
				}
				out = append(out, collabCand{ID: "s-" + heHash(idKey)[:10], Source: "session", Day: l.day, Window: win, SessionID: sid, Text: text})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return heHash(out[i].ID) < heHash(out[j].ID) })
	return out, nil
}

// collabPriorEntities indexes non-sensitive, non-inferred World/People entity names with their earliest First day and
// document frequency, so entity-ablation extraction only selects incidents involving concrete entities that already
// existed in memory strictly before the incident day (unified design §3.2).
type collabPriorEntities struct {
	first map[string]string
	df    map[string]int
	names []string // pre-filtered names with df <= collabEntityDFMost
}

func buildCollabPriorEntities(st *store.State) *collabPriorEntities {
	if st == nil {
		return nil
	}
	p := &collabPriorEntities{first: map[string]string{}, df: map[string]int{}}
	rec := func(raw, day string) {
		n := strings.TrimSpace(raw)
		if day == "" || collabGeneric[n] || strings.EqualFold(n, "persona") {
			return
		}
		rs := []rune(n)
		if len(rs) < 2 || (len(n) == len(rs) && len(rs) < 4) {
			return
		}
		p.df[n]++
		if cur, ok := p.first[n]; !ok || day < cur {
			p.first[n] = day
		}
	}
	for _, e := range st.Entries {
		if e == nil || e.Inferred || len(e.Cats()) > 0 || e.OriginSpace != "" || e.First == "" {
			continue
		}
		if e.Facet != entry.World && e.Facet != entry.People {
			continue
		}
		if e.Person != "" {
			if c := st.Contacts[e.Person]; c != nil && c.Manual != "removed" {
				rec(c.Name, e.First)
			}
		}
		rec(e.Object, e.First)
		for _, sc := range e.Scopes {
			if sc.Kind != entry.ScopeGlobal {
				rec(sc.Name, e.First)
			}
		}
	}
	for n, df := range p.df {
		if df > 0 && df <= collabEntityDFMost {
			p.names = append(p.names, n)
		}
	}
	sort.Strings(p.names)
	return p
}

func (p *collabPriorEntities) hasPrior(name, day string) bool {
	if p == nil || day == "" {
		return false
	}
	n := strings.TrimSpace(name)
	d := p.first[n]
	return d != "" && d < day && p.df[n] <= collabEntityDFMost
}

func (p *collabPriorEntities) mentionsPrior(text, day string) bool {
	if p == nil || day == "" || text == "" {
		return false
	}
	for _, n := range p.names {
		if p.first[n] < day && strings.Contains(text, n) {
			return true
		}
	}
	return false
}

func (p *collabPriorEntities) entryHasPrior(st *store.State, e *entry.Entry) ([]string, bool) {
	if p == nil || e == nil || e.First == "" {
		return nil, false
	}
	var out []string
	add := func(raw string) {
		n := strings.TrimSpace(raw)
		if n != "" && p.hasPrior(n, e.First) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if e.Person != "" && st != nil {
		if c := st.Contacts[e.Person]; c != nil && c.Manual != "removed" {
			add(c.Name)
		}
	}
	add(e.Object)
	add(e.Impact.Object)
	for _, sc := range e.Scopes {
		if sc.Kind != entry.ScopeGlobal {
			add(sc.Name)
		}
	}
	return out, len(out) > 0
}

// collabSessionCandsEntity targets real correction turns (collabCue) whose context involves a concrete entity: either
// an explicit entity-confusion cue (collabEntityCue) or a World/People entity name that already existed before the day.
// Intersecting collabCue with entity relevance avoids the high non-incident noise of matching collabEntityCue alone.
func collabSessionCandsEntity(dirs []string, ws []collabWindow, prior *collabPriorEntities) ([]collabCand, error) {
	var out []collabCand
	seenFiles := map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			cf := filepath.Clean(f)
			if seenFiles[cf] {
				continue
			}
			seenFiles[cf] = true
			lines, err := collabRead(f)
			if err != nil {
				return nil, err
			}
			sid := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			for i, l := range lines {
				if !l.user || len(l.text) > 1500 || !collabCue.MatchString(l.text) {
					continue
				}
				win, ok := windowFor(l.day, ws)
				if !ok {
					continue
				}
				req := -1
				var mv []string
				for k := i - 1; k >= 0; k-- {
					if lines[k].user {
						req = k
						break
					}
					mv = append([]string{lines[k].text}, mv...)
				}
				if req < 0 || len(mv) == 0 {
					continue
				}
				entCtx := heClip(lines[req].text, 600) + "\n" + l.text
				if !collabEntityCue.MatchString(entCtx) && !prior.mentionsPrior(entCtx, l.day) {
					continue
				}
				move := strings.Join(mv, "\n")
				text := "用户的请求：" + heClip(lines[req].text, 600) + "\n助手的动作：" + heClip(move, collabCtxMost) +
					"\n用户随后说：" + heClip(l.text, 600)
				idKey := f + fmt.Sprint(l.legacyIdx)
				if l.ctxLine > 0 {
					idKey = fmt.Sprintf("%s:ctx:%d", f, l.ctxLine)
				}
				out = append(out, collabCand{ID: "s-" + heHash(idKey)[:10], Source: "session", Day: l.day, Window: win, SessionID: sid, Text: text})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return heHash(out[i].ID) < heHash(out[j].ID) })
	return out, nil
}

// collabStateCands draws real, user-quoted, entity-bound Action records from state.json across configured sources
// (e.g. S1/S2/S3/S6 interaction corrections and S8 Feishu/WeChat/KB/GitHub decisions). Every candidate must reference
// at least one World/People entity that already existed strictly before its First day, and carries Leak/Units so
// collab-run excludes the source entry, its parent class and same-unit siblings from every arm.
func collabStateCands(st *store.State, prior *collabPriorEntities, ws []collabWindow, allowedSources map[string]bool) []collabCand {
	if st == nil || prior == nil || len(allowedSources) == 0 {
		return nil
	}
	byUnit := map[string][]string{}
	for _, e := range st.Entries {
		if e == nil {
			continue
		}
		for _, u := range heUnits(e) {
			byUnit[u] = append(byUnit[u], e.ID)
		}
	}
	var out []collabCand
	for _, e := range st.Entries {
		if e == nil || e.Inferred || e.Grain != entry.Action || len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		win, ok := windowFor(e.First, ws)
		if !ok {
			continue
		}
		sit, ch, rej := strings.TrimSpace(e.Situation), strings.TrimSpace(e.Choice), strings.TrimSpace(e.Rejected)
		if sit == "" || len([]rune(ch)) < 4 || len([]rune(rej)) < 4 {
			continue
		}
		var src, quote string
		for _, ev := range e.Evidence {
			if ev.Reported || !allowedSources[ev.Source] {
				continue
			}
			if q := strings.TrimSpace(ev.Quote); q != "" {
				src, quote = ev.Source, q
				break
			}
		}
		if src == "" {
			continue
		}
		eligible := false
		switch {
		case e.Facet == entry.Interaction:
			eligible = true
		case src == entry.SrcChat && (e.Facet == entry.Judgement || e.Facet == entry.Workflow || e.Facet == entry.Tools):
			eligible = true
		}
		if !eligible {
			continue
		}
		ents, ok := prior.entryHasPrior(st, e)
		if !ok {
			continue
		}
		text := fmt.Sprintf("来源：%s（日期：%s，涉及实体：%s）\n情境：%s\n用户实际决定/要求：%s\n被否决/错误做法：%s\n用户原话：%s",
			src, e.First, strings.Join(ents, "、"), heClip(sit, 500), heClip(ch, 400), heClip(rej, 400), heClip(quote, 600))
		out = append(out, collabCand{
			ID:     "st-" + heHash(e.ID)[:10],
			Source: "state:" + src,
			Day:    e.First,
			Window: win,
			Text:   text,
			Leak:   heLeak(st, e, byUnit),
			Units:  heUnits(e),
		})
	}
	sort.Slice(out, func(i, j int) bool { return heHash(out[i].ID) < heHash(out[j].ID) })
	return out
}

// collabBalanceBySource balances candidates round-robin across (source bucket, window) groups up to limit so no single
// channel or window crowds out the others.
func collabBalanceBySource(cands []collabCand, limit int) []collabCand {
	if len(cands) <= limit {
		return cands
	}
	byKey := map[string][]collabCand{}
	var keys []string
	for _, c := range cands {
		k := c.Source + "|" + c.Window
		if _, exists := byKey[k]; !exists {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], c)
	}
	sort.Strings(keys)
	var out []collabCand
	for len(out) < limit {
		took := false
		for _, k := range keys {
			if xs := byKey[k]; len(xs) > 0 && len(out) < limit {
				out, byKey[k], took = append(out, xs[0]), xs[1:], true
			}
		}
		if !took {
			break
		}
	}
	return out
}

// collabDayFilter builds the anti-leak filter for all incidents on a given day, merging each incident's Leak and Units.
func collabDayFilter(incs []collabIncident) heFilter {
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	for _, inc := range incs {
		for _, id := range inc.Leak {
			f.leak[id] = true
		}
		for _, u := range inc.Units {
			f.units[u] = true
		}
	}
	return f
}

func collabSourceRank(src string) int {
	switch {
	case src == "memory":
		return 0
	case src == "session":
		return 1
	default:
		return 2
	}
}

// collabBalanceCands takes at most cap candidates round-robin across windows so one larger window does not starve another.
func collabBalanceCands(cands []collabCand, ws []collabWindow, limit int) []collabCand {
	if len(cands) <= limit || len(ws) <= 1 {
		return cands[:min(len(cands), limit)]
	}
	byWin := map[string][]collabCand{}
	for _, c := range cands {
		byWin[c.Window] = append(byWin[c.Window], c)
	}
	var out []collabCand
	for len(out) < limit {
		took := false
		for _, w := range ws {
			k := w.String()
			if xs := byWin[k]; len(xs) > 0 && len(out) < limit {
				out, byWin[k], took = append(out, xs[0]), xs[1:], true
			}
		}
		if !took {
			break
		}
	}
	return out
}

// collabExclusion holds incidents from other sets (e.g. dev set) that must never enter a held-out set.
type collabExclusion struct {
	ids  map[string]bool
	incs []collabIncident
}

func loadCollabExclusions(spec string) (collabExclusion, error) {
	ex := collabExclusion{ids: map[string]bool{}}
	if strings.TrimSpace(spec) == "" {
		return ex, nil
	}
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return ex, err
		}
		var s collabSet
		if err := json.Unmarshal(raw, &s); err != nil {
			return ex, fmt.Errorf("exclude %s: %w", p, err)
		}
		for _, inc := range s.Incidents {
			ex.ids[inc.ID] = true
			ex.incs = append(ex.incs, inc)
		}
	}
	return ex, nil
}

func (ex collabExclusion) matches(inc collabIncident) bool {
	if ex.ids[inc.ID] {
		return true
	}
	for _, k := range ex.incs {
		if k.Day == inc.Day && norm.Jaccard(k.Want, inc.Want) >= 0.35 {
			return true
		}
	}
	return false
}

// collabCall runs one batched request and decodes its JSON into v.
func collabCall(ctx context.Context, m model.Model, system, schema string, in any, v any) error {
	raw, _ := json.Marshal(in)
	resp, err := m.Complete(ctx, model.Request{Step: collabStepModel, Tier: model.Small, System: system, User: string(raw),
		MaxTokens: 6000, Schema: json.RawMessage(schema)})
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp.Text), v)
}

// collabBatches runs fn over xs in batches, at most the shared limiter's calls at once; the first error.
func collabBatches[T any](xs []T, fn func([]T) error) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for b := 0; b < len(xs); b += collabBatch {
		batch := xs[b:min(len(xs), b+collabBatch)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(batch); err != nil && !errors.Is(err, model.ErrRefused) {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

type collabVerdict struct {
	ID          string `json:"id"`
	Avoided     bool   `json:"avoided"`
	NeedlessAsk bool   `json:"needless_ask"`
	WrongAction bool   `json:"wrong_action"`
	Repeat      bool   `json:"repeat"`
}

// collabAnswer answers every incident with memory, then judges the answers; the verdicts by incident id.
// attempt > 0 asks again (a repeat): it is in the input, so the answer is not the cached one.
func collabAnswer(ctx context.Context, answer, judge model.Model, memory string, incs []collabIncident, attempt int) (map[string]collabVerdict, map[string]string, error) {
	type q struct {
		ID        string `json:"id"`
		Situation string `json:"situation"`
	}
	resp := map[string]string{}
	var mu sync.Mutex
	err := collabBatches(incs, func(b []collabIncident) error {
		in := struct {
			Attempt    int    `json:"attempt,omitempty"`
			Memory     string `json:"memory"`
			Situations []q    `json:"situations"`
		}{Attempt: attempt, Memory: memory}
		for _, x := range b {
			s := x.Situation
			if x.Recall != "" {
				lbl := x.RecallLabel
				if lbl == "" {
					lbl = "按需召回（情境里提到的人、项目带出的记忆）："
				}
				s += "\n\n" + lbl + "\n" + x.Recall
			}
			in.Situations = append(in.Situations, q{x.ID, s})
		}
		var out struct {
			Answers []struct{ ID, Response string } `json:"answers"`
		}
		if err := collabCall(ctx, answer, collabAnswerPrompt, collabAnswerSchema, in, &out); err != nil {
			return err
		}
		mu.Lock()
		for _, a := range out.Answers {
			resp[a.ID] = a.Response
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	type jq struct {
		ID        string `json:"id"`
		Situation string `json:"situation"`
		Mistake   string `json:"mistake"`
		Want      string `json:"want"`
		Response  string `json:"response"`
	}
	verdicts := map[string]collabVerdict{}
	err = collabBatches(incs, func(b []collabIncident) error {
		var in struct {
			Attempt int  `json:"attempt,omitempty"`
			Items   []jq `json:"items"`
		}
		in.Attempt = attempt
		for _, x := range b {
			in.Items = append(in.Items, jq{x.ID, x.Situation, x.Mistake, x.Want, resp[x.ID]})
		}
		var out struct {
			Verdicts []collabVerdict `json:"verdicts"`
		}
		if err := collabCall(ctx, judge, collabJudgePrompt, collabJudgeSchema, in, &out); err != nil {
			return err
		}
		mu.Lock()
		for _, v := range out.Verdicts {
			verdicts[v.ID] = v
		}
		mu.Unlock()
		return nil
	})
	return verdicts, resp, err
}

func collabBuild(args []string) error {
	fs := flag.NewFlagSet("hardeval collab-build", flag.ContinueOnError)
	memDir := fs.String("memory-dir", "", "comma-separated feedback memory directories (read only)")
	sessions := fs.String("sessions", "", "comma-separated directories of session transcripts (*.jsonl, read only)")
	sessionsRoot := fs.String("sessions-root", "", "root directory whose subdirectories hold session transcripts (*.jsonl) and optional memory/ dirs (read only)")
	sessionsInclude := fs.String("sessions-include", "", "comma-separated case-insensitive substrings filtering subdirectory names under -sessions-root (empty: all)")
	statePath := fs.String("state", "", "optional read-only state.json for prior World/People entity indexing and cross-channel candidate extraction")
	stateSources := fs.String("state-sources", "", "comma-separated Evidence.Source codes from -state (e.g. S1,S2,S3,S6,S8; defaults to S1,S2,S3,S6,S8 when -state and -entity-only are set)")
	cutoff := fs.String("cutoff", "", "first held-out day, YYYY-MM-DD (used when -windows is empty)")
	end := fs.String("end", "", "last held-out day (used when -windows is empty)")
	windowsSpec := fs.String("windows", "", "comma-separated date windows YYYY-MM-DD..YYYY-MM-DD (overrides -cutoff/-end)")
	excludeSpec := fs.String("exclude", "", "comma-separated collab.json files whose incident IDs and same-day wants must be excluded")
	entityOnly := fs.Bool("entity-only", false, "target entity-related incidents (collabCue intersected with entity relevance) and keep only entity=true")
	requirePrior := fs.Bool("require-prior-memory", false, "keep only incidents that match a pre-incident rule/principle in -state (First < day, outside leak/units)")
	most := fs.Int("most", collabMost, "incidents kept at most")
	candMost := fs.Int("cand-most", 0, "session/state candidates scanned at most (0: 160, or 320 with -entity-only)")
	out := fs.String("out", "", "directory for collab.json and the caches (keep it local)")
	cmd := fs.String("model-cmd", "claude", "the model command line")
	mdl := fs.String("model", "claude-sonnet-5-5", "the model that writes the incidents and answers without memory (the answering one)")
	jdg := fs.String("judge-model", "claude-opus-5-5", "the model that judges")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := parseCollabWindows(*windowsSpec, *cutoff, *end)
	if err != nil {
		return err
	}
	if *out == "" || (*memDir == "" && *sessions == "" && *sessionsRoot == "" && *statePath == "") {
		return errors.New("-out and at least one of -memory-dir, -sessions, -sessions-root or -state are required")
	}
	sessDirs, memDirs, explicitMem, err := discoverCollabDirs(*memDir, *sessions, *sessionsRoot, *sessionsInclude)
	if err != nil {
		return err
	}
	var st *store.State
	var prior *collabPriorEntities
	if strings.TrimSpace(*statePath) != "" {
		st, err = readState(*statePath)
		if err != nil {
			return err
		}
		prior = buildCollabPriorEntities(st)
	}
	allowedStateSrc := map[string]bool{}
	srcSpec := strings.TrimSpace(*stateSources)
	if srcSpec == "" && st != nil && *entityOnly {
		srcSpec = "S1,S2,S3,S6,S8"
	}
	for _, s := range strings.Split(srcSpec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			allowedStateSrc[s] = true
		}
	}
	ex, err := loadCollabExclusions(*excludeSpec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	scanLimit := *candMost
	if scanLimit <= 0 {
		scanLimit = collabCandMost
		if *entityOnly {
			scanLimit = collabEntityCandMost
		}
	}
	var cands []collabCand
	seenCandID := map[string]bool{}
	excluded := 0
	for _, md := range memDirs {
		c, err := collabMemoryCandsInDir(md, ws, explicitMem[md])
		if err != nil {
			return err
		}
		for _, x := range c {
			if ex.ids[x.ID] {
				excluded++
				continue
			}
			if seenCandID[x.ID] {
				continue
			}
			seenCandID[x.ID] = true
			cands = append(cands, x)
		}
	}
	var dynamicCands []collabCand
	if len(sessDirs) > 0 {
		var c []collabCand
		if *entityOnly {
			c, err = collabSessionCandsEntity(sessDirs, ws, prior)
		} else {
			c, err = collabSessionCandsWindows(sessDirs, ws, collabCue)
		}
		if err != nil {
			return err
		}
		for _, x := range c {
			if ex.ids[x.ID] {
				excluded++
				continue
			}
			if seenCandID[x.ID] {
				continue
			}
			seenCandID[x.ID] = true
			dynamicCands = append(dynamicCands, x)
		}
	}
	if st != nil && len(allowedStateSrc) > 0 {
		for _, x := range collabStateCands(st, prior, ws, allowedStateSrc) {
			if ex.ids[x.ID] {
				excluded++
				continue
			}
			if seenCandID[x.ID] {
				continue
			}
			seenCandID[x.ID] = true
			dynamicCands = append(dynamicCands, x)
		}
	}
	if len(allowedStateSrc) > 0 {
		cands = append(cands, collabBalanceBySource(dynamicCands, scanLimit)...)
	} else {
		cands = append(cands, collabBalanceCands(dynamicCands, ws, scanLimit)...)
	}
	ctx := context.Background()
	ext := heCLI(*cmd, *mdl, filepath.Join(*out, "extract-cache"))
	winLabels := make([]string, len(ws))
	for i, w := range ws {
		winLabels[i] = w.String()
	}
	set := collabSet{
		Cutoff:     ws[0].Start,
		End:        ws[len(ws)-1].End,
		Windows:    winLabels,
		EntityOnly: *entityOnly,
		Candidates: len(cands),
		Excluded:   excluded,
	}
	var mu sync.Mutex
	byID := map[string]collabCand{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	var incs []collabIncident
	err = collabBatches(cands, func(b []collabCand) error {
		type item struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		var in struct {
			Items []item `json:"items"`
		}
		for _, c := range b {
			in.Items = append(in.Items, item{c.ID, c.Text})
		}
		var o struct {
			Items []struct {
				ID, Kind, Situation, Mistake, Want string
				Incident, Entity                   bool
			} `json:"items"`
		}
		if err := collabCall(ctx, ext, collabExtractPrompt, collabExtractSchema, in, &o); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, x := range o.Items {
			c, ok := byID[x.ID]
			if !ok {
				continue
			}
			if !x.Incident || strings.TrimSpace(x.Situation) == "" || strings.TrimSpace(x.Want) == "" {
				set.NoIncident++
				continue
			}
			if *entityOnly && !x.Entity {
				set.NoIncident++
				continue
			}
			incs = append(incs, collabIncident{ID: c.ID, Source: c.Source, Day: c.Day, Window: c.Window, SessionID: c.SessionID,
				Kind: x.Kind, Situation: x.Situation, Mistake: x.Mistake, Want: x.Want, Entity: x.Entity,
				Leak: c.Leak, Units: c.Units})
		}
		return nil
	})
	if err != nil {
		return err
	}
	// One incident told twice (e.g. a memory written after a session or state record): memory > session > state.
	sort.Slice(incs, func(i, j int) bool {
		if ri, rj := collabSourceRank(incs[i].Source), collabSourceRank(incs[j].Source); ri != rj {
			return ri < rj
		}
		return heHash(incs[i].ID) < heHash(incs[j].ID)
	})
	var kept []collabIncident
	for _, x := range incs {
		if ex.matches(x) {
			set.Excluded++
			continue
		}
		dup := false
		for _, k := range kept {
			if k.Day == x.Day && norm.Jaccard(k.Want, x.Want) >= 0.35 {
				dup = true
				break
			}
		}
		if dup {
			set.Duplicates++
			continue
		}
		if st != nil {
			x.PriorMemory = hasPriorRuleMemory(st, x)
			if *requirePrior && !x.PriorMemory {
				set.NoPriorMemory++
				continue
			}
		}
		kept = append(kept, x)
	}
	// Drop what the answering model gets right without memory (its answers kept apart from the run's).
	ans := heCLI(*cmd, *mdl, filepath.Join(*out, "filter-cache"))
	judge := heCLI(*cmd, *jdg, filepath.Join(*out, "filter-cache"))
	v, _, err := collabAnswer(ctx, ans, judge, "", kept, 0)
	if err != nil {
		return err
	}
	for _, x := range kept {
		if v[x.ID].Avoided {
			set.Avoided++
			continue
		}
		if len(set.Incidents) < *most {
			set.Incidents = append(set.Incidents, x)
		}
	}
	b, _ := json.MarshalIndent(set, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "collab.json"), b, 0o600); err != nil {
		return err
	}
	n := map[string]int{}
	byWin := map[string]int{}
	entN := 0
	for _, x := range set.Incidents {
		if strings.HasPrefix(x.Source, "state:") {
			n["state"]++
		} else {
			n[x.Source]++
		}
		if x.Window != "" {
			byWin[x.Window]++
		}
		if x.Entity {
			entN++
		}
	}
	fmt.Printf("hardeval collab-build: %d candidates, %d excluded, %d not an incident, %d duplicates, %d no prior memory, %d avoided without memory, %d incidents (memory %d, session %d, state %d, entity %d, windows %v)\n",
		set.Candidates, set.Excluded, set.NoIncident, set.Duplicates, set.NoPriorMemory, set.Avoided, len(set.Incidents), n["memory"], n["session"], n["state"], entN, byWin)
	return nil
}

type collabResult struct {
	Model   string                                   `json:"model"`
	Judge   string                                   `json:"judge"`
	Cutoff  string                                   `json:"cutoff"` // "per incident": each incident's memory is what was there the day before
	N       int                                      `json:"n"`
	Repeats int                                      `json:"repeats"`
	Score   map[string]map[string]map[string]float64 `json:"score"` // budget → arm → avoided | needless_ask | wrong_action | repeat
	Bytes   map[string]map[string]int                `json:"bytes"` // budget → arm → mean bytes of the memory
	// PerInc: budget → arm → incident → share of the repeats it was avoided in.
	PerInc map[string]map[string]map[string]float64 `json:"per_incident"`
	// PerWrong and PerAsk: budget → arm → incident → share of the repeats judged a wrong action / a needless ask.
	PerWrong map[string]map[string]map[string]float64 `json:"per_incident_wrong,omitempty"`
	PerAsk   map[string]map[string]map[string]float64 `json:"per_incident_ask,omitempty"`
	// Entity are the incidents that need entity relations (collabIncident.Entity), reported apart.
	Entity []string `json:"entity,omitempty"`
	// PriorMemory are the incidents backed by a matching pre-incident rule/principle in state.json (solvable recurrences).
	PriorMemory []string `json:"prior_memory,omitempty"`
	// Segments reports per-window metrics when incidents carry Window labels (window → budget → arm → metric).
	Segments map[string]map[string]map[string]map[string]float64 `json:"segments,omitempty"`
	SegmentN map[string]int                                      `json:"segment_n,omitempty"`
}

const collabDecisionsQueryPrompt = `你是这位用户的主 AI 助手。线上规则提示：涉及具体的人、项目、系统、机器、工具命令或账号，或者要在岔口上替用户选、派活、汇报、验收或问用户之前，先调 decisions_query(tag) 从记忆池查局部规矩、负责人和例外（tag 按空格拆词，所有词以子串同时匹配才命中，结果截断在 3072 字节；用单个实体/工具名如 task_plan、sj-wsl、1Password、项目名、人名，或单个场合词如「派活」「汇报」「提问」「开工」「验收」「排期」「配置」「门禁」最容易命中）。

下面给你当前常驻记忆和若干互相独立的情境。对每个情境，判断是否需要调用 decisions_query 查常驻之外的局部规矩、工具规范或实体关系：
- 纯粹已被常驻覆盖且不涉具体实体/工具细节时返回空数组 []；
- 需要查则返回 1 到 3 个简短 tag（每个尽量 1 个词，不要写长句）。
输出 JSON。`

const collabDecisionsQuerySchema = `{"type":"object","additionalProperties":false,"required":["queries"],"properties":{"queries":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","tags"],"properties":{"id":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}}}}`

// collabQueryPlan asks the answering model which decisions_query tags (0-3) it wants to query for each incident.
func collabQueryPlan(ctx context.Context, answer model.Model, memory string, incs []collabIncident, attempt int) (map[string][]string, error) {
	type q struct {
		ID        string `json:"id"`
		Situation string `json:"situation"`
	}
	planned := map[string][]string{}
	var mu sync.Mutex
	err := collabBatches(incs, func(b []collabIncident) error {
		in := struct {
			Attempt    int    `json:"attempt,omitempty"`
			Memory     string `json:"memory"`
			Situations []q    `json:"situations"`
		}{Attempt: attempt, Memory: memory}
		for _, x := range b {
			in.Situations = append(in.Situations, q{x.ID, x.Situation})
		}
		var out struct {
			Queries []struct {
				ID   string   `json:"id"`
				Tags []string `json:"tags"`
			} `json:"queries"`
		}
		if err := collabCall(ctx, answer, collabDecisionsQueryPrompt, collabDecisionsQuerySchema, in, &out); err != nil {
			return err
		}
		mu.Lock()
		for _, item := range out.Queries {
			if len(item.Tags) > 3 {
				item.Tags = item.Tags[:3]
			}
			planned[item.ID] = item.Tags
		}
		mu.Unlock()
		return nil
	})
	return planned, err
}

// collabHubPage1 builds the exact first page of memory.entries (up to memsrc.MaxLimit = 500, standing first then rank
// desc) visible before day with no sensitive or restricted-space entries, using real overview.Exits(ov) so non-standing
// L1 contracts/principles carry enriched subjects and exact online ranks, matching the hub's decisions_query scope.
func collabHubPage1(st *store.State, ov *overview.State, f heFilter, day string, budget int) []memsrc.Entry {
	daySt := &store.State{
		Contacts: st.Contacts,
		Graph:    sliceGraphBefore(st.Graph, day),
		Entries:  make(map[string]*entry.Entry, len(st.Entries)),
	}
	for id, e := range st.Entries {
		if len(e.Cats()) > 0 || e.OriginSpace != "" || f.entryOut(st, e, day) {
			continue
		}
		daySt.Entries[id] = e
	}
	var extras []memsrc.Extra
	if ov != nil {
		cleanOv := &overview.State{Patterns: map[string]*overview.Pattern{}}
		for id, p := range ov.Patterns {
			if p != nil && p.Used() && len(p.Categories) == 0 && p.OriginSpace == "" && !f.patternOut(st, p, day) {
				cleanOv.Patterns[id] = p
			}
		}
		extras = overview.Exits(cleanOv)
	}
	res, _, err := memsrc.PageWith(daySt, nil, extras, memsrc.Params{
		Limit:         memsrc.MaxLimit,
		ResidentBytes: budget,
	})
	if err != nil {
		return nil
	}
	return res.Entries
}

// hubDecisionsQuery matches tags against page-1 entries using the hub's decisions_query rule (entries_sources.go):
// whitespace-split terms, all-terms case-insensitive substring AND across text, subjects, facet and categories,
// preserving page-1 order (standing + rank) and truncating at maxBytes (3072 B).
func hubDecisionsQuery(page []memsrc.Entry, tags []string, maxBytes int) string {
	if len(tags) == 0 || maxBytes <= 0 {
		return ""
	}
	matched := map[string]bool{}
	var lines []string
	for _, rawTag := range tags {
		terms := strings.Fields(strings.ToLower(strings.TrimSpace(rawTag)))
		if len(terms) == 0 {
			continue
		}
		for _, e := range page {
			if matched[e.ID] {
				continue
			}
			if hubEntryMatchesTerms(e, terms) {
				matched[e.ID] = true
				lines = append(lines, fmt.Sprintf("- [%s] %s", e.ID, e.Text))
			}
		}
	}
	return heFit(lines, maxBytes)
}

func hubEntryMatchesTerms(e memsrc.Entry, terms []string) bool {
	haystack := make([]string, 0, 2+len(e.Subjects)+len(e.Categories))
	haystack = append(haystack, strings.ToLower(e.Text), strings.ToLower(e.Facet))
	for _, s := range e.Subjects {
		haystack = append(haystack, strings.ToLower(s))
	}
	for _, c := range e.Categories {
		haystack = append(haystack, strings.ToLower(c))
	}
	for _, term := range terms {
		found := false
		for _, h := range haystack {
			if strings.Contains(h, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func collabRun(args []string) error {
	fs := flag.NewFlagSet("hardeval collab-run", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	ovPath := fs.String("overview", "", "patterns built before the cutoff (overview-backtest.json)")
	ovDir := fs.String("overview-dir", "", "directory of time-clean L1 checkpoints <YYYY-MM-DD>.json (each incident uses the latest <= its day)")
	cogDir := fs.String("cogpipe-dir", "", "the old pipeline's injected texts, one <YYYY-MM-DD>.md a build: each incident gets the last one before its day")
	out := fs.String("out", "", "the directory collab-build wrote")
	cmd := fs.String("model-cmd", "claude", "the model command line")
	mdl := fs.String("model", "claude-sonnet-5-5", "the model that answers")
	jdg := fs.String("judge-model", "claude-opus-5-5", "the model that judges")
	label := fs.String("label", "baseline", "name of this reading (collab-<label>.json)")
	arms := fs.String("arms", strings.Join(heArms, ","), "arms to answer (supports decisions-query-sim, recall, recall-noent)")
	budgets := fs.String("budgets", "1400,4096", "memory budgets in bytes")
	repeats := fs.Int("repeats", 3, "times each arm answers (and is judged), averaged")
	recallBytes := fs.Int("recall", 2048, "bytes of recall per incident for the recall arms (recall: the standing set and what the incident's names bring in; recall-noent: the same without the world and people entries)")
	if err := fs.Parse(args); err != nil {
		return err
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
	ovs, err := collabOverviews(*ovDir)
	if err != nil {
		return err
	}
	cogs, err := collabCogpipe(*cogDir)
	if err != nil {
		return err
	}
	// Incidents by day: each day's memory is what was first seen before it.
	byDay := map[string][]collabIncident{}
	for _, x := range set.Incidents {
		byDay[x.Day] = append(byDay[x.Day], x)
	}
	var days []string
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	ans := heCLI(*cmd, *mdl, filepath.Join(*out, "cache"))
	judge := heCLI(*cmd, *jdg, filepath.Join(*out, "cache"))
	res := collabResult{Model: *mdl, Judge: *jdg, Cutoff: "per incident", N: len(set.Incidents), Repeats: *repeats,
		Score: map[string]map[string]map[string]float64{}, Bytes: map[string]map[string]int{}, PerInc: map[string]map[string]map[string]float64{},
		PerWrong: map[string]map[string]map[string]float64{}, PerAsk: map[string]map[string]map[string]float64{}}
	winByID := map[string]string{}
	segN := map[string]int{}
	for _, inc := range set.Incidents {
		if inc.Entity {
			res.Entity = append(res.Entity, inc.ID)
		}
		if inc.PriorMemory || hasPriorRuleMemory(st, inc) {
			res.PriorMemory = append(res.PriorMemory, inc.ID)
		}
		if inc.Window != "" {
			winByID[inc.ID] = inc.Window
			segN[inc.Window]++
		}
	}
	if len(segN) > 0 {
		res.SegmentN = segN
		res.Segments = map[string]map[string]map[string]map[string]float64{}
		for w := range segN {
			res.Segments[w] = map[string]map[string]map[string]float64{}
		}
	}
	names := collabNames(st)
	samples := map[string]string{}
	responses := map[string]map[string]string{}
	armList := strings.Split(*arms, ",")
	for _, bs := range strings.Split(*budgets, ",") {
		budget := 0
		fmt.Sscan(bs, &budget)
		bk := fmt.Sprint(budget)
		res.Score[bk], res.Bytes[bk], res.PerInc[bk] = map[string]map[string]float64{}, map[string]int{}, map[string]map[string]float64{}
		res.PerWrong[bk], res.PerAsk[bk] = map[string]map[string]float64{}, map[string]map[string]float64{}
		for w := range segN {
			res.Segments[w][bk] = map[string]map[string]float64{}
		}
		for _, arm := range armList {
			sc := map[string]float64{}
			segSc := map[string]map[string]float64{}
			for w := range segN {
				segSc[w] = map[string]float64{}
			}
			per, wrong, ask := map[string]float64{}, map[string]float64{}, map[string]float64{}
			bytes := 0
			for _, d := range days {
				daySt := st
				dayOv := ov
				dayNames := names
				if len(ovs) > 0 {
					daySt = sliceStateBefore(st, d)
					dayOv = collabOverviewFor(ovs, d)
					dayNames = collabNames(daySt)
				}
				dayF := collabDayFilter(byDay[d])
				mems := heMemories(daySt, dayOv, collabCogFor(cogs, d), dayF, d, budget)
				mem, incs := mems[arm], byDay[d]
				var hubPage []memsrc.Entry
				switch arm {
				case "recall", "recall-noent":
					mem = mems["resident"]
					incs = slices.Clone(incs)
					for i := range incs {
						incs[i].Recall = dayNames.recallTwoLayer(daySt, dayOv, dayF, d, incs[i].Situation, mem, *recallBytes, arm == "recall")
						bytes += len(incs[i].Recall)
						samples[bk+"/"+arm+"/"+incs[i].ID] = incs[i].Recall
					}
				case armDecisionsQuerySim:
					mem = mems["resident"]
					hubPage = collabHubPage1(daySt, dayOv, dayF, d, budget)
				}
				bytes += len(mem) * len(incs)
				samples[bk+"/"+arm+"/"+d] = mem
				for r := 0; r < *repeats; r++ {
					runIncs := incs
					if arm == armDecisionsQuerySim {
						tagsByID, qerr := collabQueryPlan(context.Background(), ans, mem, incs, r)
						if qerr != nil {
							return fmt.Errorf("%s/%s/%s query-plan: %w", bk, arm, d, qerr)
						}
						runIncs = slices.Clone(incs)
						for i := range runIncs {
							tags := tagsByID[runIncs[i].ID]
							rec := hubDecisionsQuery(hubPage, tags, decisionsQueryMaxBytes)
							runIncs[i].Recall = rec
							runIncs[i].RecallLabel = "decisions_query 返回结果（tag=" + strings.Join(tags, ", ") + "）："
							if r == 0 {
								bytes += len(rec)
								samples[bk+"/"+arm+"/"+runIncs[i].ID] = "tags=" + strings.Join(tags, ", ") + "\n" + rec
							}
						}
					}
					v, rs, err := collabAnswer(context.Background(), ans, judge, mem, runIncs, r)
					if err != nil {
						return fmt.Errorf("%s/%s/%s: %w", bk, arm, d, err)
					}
					if r == 0 {
						if responses[bk+"/"+arm] == nil {
							responses[bk+"/"+arm] = map[string]string{}
						}
						for k, x := range rs {
							responses[bk+"/"+arm][k] = x
						}
					}
					for _, x := range byDay[d] {
						vv := v[x.ID]
						sc["avoided"] += b2f(vv.Avoided)
						sc["needless_ask"] += b2f(vv.NeedlessAsk)
						sc["wrong_action"] += b2f(vv.WrongAction)
						sc["repeat"] += b2f(vv.Repeat)
						if w := winByID[x.ID]; w != "" {
							segSc[w]["avoided"] += b2f(vv.Avoided)
							segSc[w]["needless_ask"] += b2f(vv.NeedlessAsk)
							segSc[w]["wrong_action"] += b2f(vv.WrongAction)
							segSc[w]["repeat"] += b2f(vv.Repeat)
						}
						per[x.ID] += b2f(vv.Avoided) / float64(*repeats)
						wrong[x.ID] += b2f(vv.WrongAction) / float64(*repeats)
						ask[x.ID] += b2f(vv.NeedlessAsk) / float64(*repeats)
					}
				}
			}
			n := float64(max(1, len(set.Incidents)**repeats))
			for k := range sc {
				sc[k] /= n
			}
			res.Score[bk][arm], res.PerInc[bk][arm] = sc, per
			res.PerWrong[bk][arm], res.PerAsk[bk][arm] = wrong, ask
			res.Bytes[bk][arm] = bytes / max(1, len(set.Incidents))
			for w, cnt := range segN {
				wn := float64(max(1, cnt**repeats))
				for k := range segSc[w] {
					segSc[w][k] /= wn
				}
				res.Segments[w][bk][arm] = segSc[w]
			}
		}
	}
	for name, v := range map[string]any{"collab-" + *label + ".json": res, "collab-memories-" + *label + ".json": samples,
		"collab-responses-" + *label + ".json": responses} {
		b, _ := json.MarshalIndent(v, "", " ")
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o600); err != nil {
			return err
		}
	}
	md := collabTable(res, armList)
	if err := os.WriteFile(filepath.Join(*out, "collab-"+*label+".md"), []byte(md), 0o600); err != nil {
		return err
	}
	fmt.Print(md)
	return nil
}

// collabCogpipe reads a directory of injected texts named by their build day; nil without one.
func collabCogpipe(dir string) (map[string][]string, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var ls []string
		for _, l := range strings.Split(string(b), "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "<!--") { // its build notes are not memory
				ls = append(ls, l)
			}
		}
		out[strings.TrimSuffix(filepath.Base(f), ".md")] = ls
	}
	return out, nil
}

// collabCogFor is the last build before day.
func collabCogFor(cogs map[string][]string, day string) []string {
	best := ""
	for d := range cogs {
		if d < day && d > best {
			best = d
		}
	}
	return cogs[best]
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// collabTable: per budget, each arm's share avoided and of needless asks, wrong actions and repeats (over incidents
// and repeats), and per incident against the new standing set (resident): win/tie/loss for it by the share avoided.
func collabTable(r collabResult, arms []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "collaboration evaluation, answer %s, judge %s, memory per incident (the day before), %d incidents × %d repeats\n",
		r.Model, r.Judge, r.N, r.Repeats)
	var bks []string
	for bk := range r.Score {
		bks = append(bks, bk)
	}
	sort.Slice(bks, func(i, j int) bool { return len(bks[i]) < len(bks[j]) || len(bks[i]) == len(bks[j]) && bks[i] < bks[j] })
	for _, bk := range bks {
		fmt.Fprintf(&b, "\n### budget %s bytes\n\n| arm | bytes | avoided | needless asks | wrong actions | user repeats | vs resident (win/tie/loss for resident) | entity incidents avoided (%d) |\n|---|---|---|---|---|---|---|---|\n", bk, len(r.Entity))
		for _, arm := range arms {
			s := r.Score[bk][arm]
			w, t, l := 0, 0, 0
			for id, v := range r.PerInc[bk][arm] {
				rv := r.PerInc[bk]["resident"][id]
				switch {
				case rv == v:
					t++
				case rv > v:
					w++
				default:
					l++
				}
			}
			ent := "-"
			if len(r.Entity) > 0 {
				sum := 0.0
				for _, id := range r.Entity {
					sum += r.PerInc[bk][arm][id]
				}
				ent = fmt.Sprintf("%.0f%%", 100*sum/float64(len(r.Entity)))
			}
			fmt.Fprintf(&b, "| %s | %d | %.0f%% | %.0f%% | %.0f%% | %.0f%% | %d/%d/%d | %s |\n", arm, r.Bytes[bk][arm], 100*s["avoided"],
				100*s["needless_ask"], 100*s["wrong_action"], 100*s["repeat"], w, t, l, ent)
		}
		if len(r.PriorMemory) > 0 && len(r.PriorMemory) < r.N {
			fmt.Fprintf(&b, "\n#### prior-memory recurrences (%d/%d incidents)\n\n| arm | avoided | needless asks | wrong actions | vs resident (win/tie/loss for resident) |\n|---|---|---|---|---|\n", len(r.PriorMemory), r.N)
			for _, arm := range arms {
				avSum, askSum, wrongSum := 0.0, 0.0, 0.0
				w, t, l := 0, 0, 0
				for _, id := range r.PriorMemory {
					v := r.PerInc[bk][arm][id]
					avSum += v
					askSum += r.PerAsk[bk][arm][id]
					wrongSum += r.PerWrong[bk][arm][id]
					rv := r.PerInc[bk]["resident"][id]
					switch {
					case rv == v:
						t++
					case rv > v:
						w++
					default:
						l++
					}
				}
				pn := float64(len(r.PriorMemory))
				fmt.Fprintf(&b, "| %s | %.0f%% | %.0f%% | %.0f%% | %d/%d/%d |\n", arm,
					100*avSum/pn, 100*askSum/pn, 100*wrongSum/pn, w, t, l)
			}
		}
		if len(r.Segments) > 0 {
			var wins []string
			for w := range r.Segments {
				wins = append(wins, w)
			}
			sort.Strings(wins)
			for _, w := range wins {
				fmt.Fprintf(&b, "\n#### segment %s (%d incidents)\n\n| arm | avoided | needless asks | wrong actions | user repeats |\n|---|---|---|---|---|\n", w, r.SegmentN[w])
				for _, arm := range arms {
					s := r.Segments[w][bk][arm]
					fmt.Fprintf(&b, "| %s | %.0f%% | %.0f%% | %.0f%% | %.0f%% |\n", arm, 100*s["avoided"],
						100*s["needless_ask"], 100*s["wrong_action"], 100*s["repeat"])
				}
			}
		}
	}
	return b.String()
}

// hasPriorRuleMemory reports whether st already contained a non-sensitive, pool-eligible rule or principle entry
// strictly before inc.Day (excluding inc.Leak and inc.Units) whose wording or principle overlaps inc.Want.
// This is computed purely from pre-incident memory (never from arm scores) to separate solvable recurrences from
// first-time novel mistakes (unified design §3.2).
func hasPriorRuleMemory(st *store.State, inc collabIncident) bool {
	if st == nil || strings.TrimSpace(inc.Want) == "" || inc.Day == "" {
		return false
	}
	f := collabDayFilter([]collabIncident{inc})
	for _, e := range st.Entries {
		if e == nil || !memsrc.Pool(st, e) || e.Inferred || len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		if entry.KindOf(e) == "" || f.entryOut(st, e, inc.Day) {
			continue
		}
		if norm.Jaccard(e.Shown(), inc.Want) >= 0.14 {
			return true
		}
		if p := e.Principle(); p != "" && norm.Jaccard(p, inc.Want) >= 0.16 {
			return true
		}
	}
	return false
}

// heInputs reads the overview's backtest patterns and the external injected text, either optional.
func heInputs(ovPath, cogPath string) (*overview.State, []string, error) {
	var ov *overview.State
	if ovPath != "" {
		b, err := os.ReadFile(ovPath)
		if err != nil {
			return nil, nil, err
		}
		ov = &overview.State{}
		if err := json.Unmarshal(b, ov); err != nil {
			return nil, nil, err
		}
	}
	var cog []string
	if cogPath != "" {
		b, err := os.ReadFile(cogPath)
		if err != nil {
			return nil, nil, err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) != "" {
				cog = append(cog, l)
			}
		}
	}
	return ov, cog, nil
}

// collabNameIndex maps the names entries can be recalled by (memsrc.Subjects: a card's person, the object, the
// person or project a rule is for, the scopes) to the entries, for the recall arms.
type collabNameIndex map[string][]*entry.Entry

func collabNames(st *store.State) collabNameIndex {
	idx := collabNameIndex{}
	for _, e := range st.Entries {
		if !memsrc.Pageable(st, e) || len(e.Cats()) > 0 || e.OriginSpace != "" || e.Inferred {
			continue
		}
		for _, n := range memsrc.Subjects(st, e) {
			if r := []rune(n); len(r) < 2 || (len(n) == len(r) && len(r) < 3) || collabGeneric[n] {
				continue
			}
			idx[n] = append(idx[n], e)
		}
	}
	// A name behind very many entries ("Persona", a word for the user) names nothing in particular: the hub's recall
	// would not bring all of them in by it either.
	for n, es := range idx {
		if len(es) > collabNameMost {
			delete(idx, n)
		}
	}
	return idx
}

// collabGeneric are words for the user and the assistant, never a subject to recall by.
var collabGeneric = map[string]bool{"用户": true, "助手": true, "我": true, "the user": true, "user": true, "assistant": true, "AI": true}

// collabIdent is an identifier-like name in a situation (a repository, a service, a tool): long enough to name one.
var collabIdent = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_.]*-[A-Za-z0-9_.-]{3,}|[A-Za-z][A-Za-z0-9_]{7,}`)

// collabNameMost is the most entries one name may bring in before it counts as too common to name anything.
const collabNameMost = 150

// collabQueryIdent matches ASCII tool/command/host/service tokens (>=3 chars) in an incident situation.
var collabQueryIdent = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.-]{2,}`)

func collabBigrams(s string) map[string]bool {
	r := []rune(norm.Q(s))
	out := make(map[string]bool, max(0, len(r)-1))
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])] = true
	}
	return out
}

func collabExtractIdents(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range collabQueryIdent.FindAllString(s, -1) {
		m = strings.ToLower(strings.Trim(m, ".-_"))
		if len(m) < 3 || seen[m] {
			continue
		}
		hasLetter := false
		for i := 0; i < len(m); i++ {
			if m[i] >= 'a' && m[i] <= 'z' {
				hasLetter = true
				break
			}
		}
		if !hasLetter {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// recallNonStandingL1 retrieves the most situation-relevant non-standing verbatim UserOwned rules/principles and
// pure-behavior L1 contracts/principles (strictly pre-cutoff and leak-filtered), keeping both handwritten rules and
// the ~60 L1 rules outside 4096 B standing reachable on demand (unified design §2.1 & §2.2).
func recallNonStandingL1(st *store.State, ov *overview.State, f heFilter, day, situation, standing string, budget int) []string {
	if budget <= 0 || strings.TrimSpace(situation) == "" {
		return nil
	}
	sitLow := strings.ToLower(situation)
	sitIdents := collabExtractIdents(situation)
	sitBi := collabBigrams(situation)
	type scoredRule struct {
		id    string
		line  string
		body  string
		score float64
		owned bool
		sc    int
	}
	scoreText := func(head, exc string, subjs []string) float64 {
		txtLow := strings.ToLower(head + " " + exc)
		s := 0.0
		for _, idt := range sitIdents {
			if strings.Contains(txtLow, idt) {
				s += 5.0
				continue
			}
			for _, sb := range subjs {
				if strings.Contains(strings.ToLower(sb), idt) {
					s += 4.0
					break
				}
			}
		}
		for _, sb := range subjs {
			sb = strings.TrimSpace(sb)
			if len([]rune(sb)) < 2 || collabGeneric[sb] {
				continue
			}
			if strings.Contains(sitLow, strings.ToLower(sb)) {
				s += 2.5
			}
		}
		ovl := 0
		for bi := range collabBigrams(head) {
			if sitBi[bi] {
				ovl++
			}
		}
		s += 0.35 * float64(ovl)
		return s
	}

	var cands []scoredRule
	if st != nil {
		for _, e := range st.Entries {
			if e == nil || f.entryOut(st, e, day) || !entry.IsTrueUserOwned(e) || !e.Live() || !memsrc.Pool(st, e) {
				continue
			}
			head := strings.TrimSpace(recall.Head(e))
			if head == "" || strings.Contains(standing, heClip(head, 24)) {
				continue
			}
			s := scoreText(head, e.Exception, memsrc.Subjects(st, e))
			if s >= 5.0 {
				txt := memsrc.ResidentText(e)
				cands = append(cands, scoredRule{
					id:    e.ID,
					line:  "- " + heClip(txt, 240),
					body:  txt,
					score: s,
					owned: true,
					sc:    entry.Score(e),
				})
			}
		}
	}
	if ov != nil {
		for _, p := range ov.Patterns {
			if p == nil || !p.Used() || len(p.Categories) > 0 || p.OriginSpace != "" || f.patternOut(st, p, day) {
				continue
			}
			head := strings.TrimSpace(p.Text)
			if head == "" || strings.Contains(standing, heClip(head, 24)) {
				continue
			}
			s := scoreText(head, p.Exception, overview.EnrichSubjects(p))
			if s >= 5.0 {
				cands = append(cands, scoredRule{
					id:    p.ID,
					line:  "- " + heClip(overview.Text(p), 240),
					body:  head,
					score: s,
					sc:    overview.Score(p),
				})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].owned != cands[j].owned {
			return cands[i].owned
		}
		if cands[i].sc != cands[j].sc {
			return cands[i].sc > cands[j].sc
		}
		return cands[i].id < cands[j].id
	})
	var lines []string
	var keptBodies []string
	used := 0
	for _, c := range cands {
		if len(lines) >= 4 {
			break
		}
		if slices.ContainsFunc(keptBodies, func(b string) bool { return norm.Jaccard(b, c.body) >= 0.45 }) {
			continue
		}
		if used+len(c.line)+1 > budget {
			continue
		}
		lines = append(lines, c.line)
		keptBodies = append(keptBodies, c.body)
		used += len(c.line) + 1
	}
	return lines
}

// collabEntityNoiseStop filters generic common nouns and short ASCII tokens out of entity-slot recall queries,
// and collabEphemeralFact filters one-off PR/ticket status lines out of the persistent entity supplement slot.
var collabEntityNoiseStop = map[string]bool{
	"文档": true, "系统": true, "模型": true, "任务": true, "测试": true, "配置": true, "工具": true,
	"方案": true, "项目": true, "用户": true, "助手": true, "实验": true, "报告": true, "笔记": true,
	"推送": true, "代码": true, "文件": true, "目录": true, "接口": true, "服务": true, "机器": true,
	"launch": true, "push": true, "status": true, "config": true, "agent": true, "task": true,
	"test": true, "doc": true, "docs": true, "file": true, "code": true, "repo": true,
	"prompt": true, "session": true, "review": true, "model": true, "user": true, "system": true,
}

var collabEphemeralFact = regexp.MustCompile(`(?i)\bPR\s*#\d+|#\d{3,}|答应.*再看|临时计划|打算把.*挪到`)

func collabIsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// collabContainsEntity matches n inside situation using whole-word boundaries for pure-ASCII names
// (preventing "Launch" from matching "launcher" or "ST" from matching "astergate").
func collabContainsEntity(situation, name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	nq := norm.Q(n)
	if nq == "" || collabEntityNoiseStop[nq] || collabGeneric[n] {
		return false
	}
	if collabIsASCII(n) {
		if len(n) < 3 {
			return false
		}
		lowSit := strings.ToLower(situation)
		lowN := strings.ToLower(n)
		isWord := func(b byte) bool {
			return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
		}
		for off := 0; off < len(lowSit); {
			i := strings.Index(lowSit[off:], lowN)
			if i < 0 {
				return false
			}
			pos := off + i
			leftOK := pos == 0 || !isWord(lowSit[pos-1])
			end := pos + len(lowN)
			rightOK := end == len(lowSit) || !isWord(lowSit[end])
			if leftOK && rightOK {
				return true
			}
			off = pos + 1
		}
		return false
	}
	return len([]rune(n)) >= 2 && strings.Contains(situation, n)
}

// collabStripRecallPrefix removes bullet and semantic tier prefixes so cross-pass Jaccard deduplication catches
// near-identical rules emitted by standing, UserOwned, L1, local recall, and 1-hop entity expansion.
func collabStripRecallPrefix(ln string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "-"))
	s = memsrc.StripStandingTag(s)
	if strings.HasPrefix(s, "跨事模式（") {
		if i := strings.Index(s, "）："); i >= 0 {
			s = s[i+len("）："):]
		}
	}
	return strings.TrimSpace(s)
}

// recallEntityFacts retrieves persistent high-IDF World/People facts directly naming entities in situation
// (strictly World/People, whole-word ASCII matched, excluding ephemeral PR/ticket noise).
func (idx collabNameIndex) recallEntityFacts(st *store.State, f heFilter, day, situation, standing string, budget int) []string {
	if st == nil || budget <= 0 {
		return nil
	}
	hits := map[string]float64{}
	total := float64(max(1, len(st.Entries)))
	idf := func(df int) float64 { return math.Log(total / float64(1+df)) }
	var cands []*entry.Entry
	seen := map[string]bool{}
	add := func(e *entry.Entry, w float64) {
		if e == nil || (e.Facet != entry.World && e.Facet != entry.People) {
			return
		}
		if !seen[e.ID] {
			seen[e.ID] = true
			cands = append(cands, e)
		}
		hits[e.ID] += w
	}
	for n, es := range idx {
		if !collabContainsEntity(situation, n) {
			continue
		}
		w := idf(len(es))
		for _, e := range es {
			if (e.Facet != entry.World && e.Facet != entry.People) || f.entryOut(st, e, day) {
				continue
			}
			add(e, w)
		}
	}
	names := map[string]bool{}
	for n := range idx {
		if collabContainsEntity(situation, n) && len([]rune(n)) >= 4 {
			names[n] = true
		}
	}
	for _, n := range collabIdent.FindAllString(situation, -1) {
		if collabContainsEntity(situation, n) {
			names[n] = true
		}
	}
	for n := range names {
		var found []*entry.Entry
		for _, e := range st.Entries {
			if (e.Facet == entry.World || e.Facet == entry.People) && strings.Contains(e.Shown(), n) {
				found = append(found, e)
			}
		}
		w := idf(len(found))
		for _, e := range found {
			if !memsrc.Pageable(st, e) || len(e.Cats()) > 0 || e.OriginSpace != "" || e.Inferred || f.entryOut(st, e, day) {
				continue
			}
			add(e, w)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if hits[a.ID] != hits[b.ID] {
			return hits[a.ID] > hits[b.ID]
		}
		if ra, rb := memsrc.Rank(a), memsrc.Rank(b); ra != rb {
			return ra > rb
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		return a.ID < b.ID
	})
	var out []string
	used := 0
	for _, e := range cands {
		if len(out) >= 16 {
			break
		}
		t := heClip(e.Shown(), 200)
		if t == "" || collabEphemeralFact.MatchString(t) || strings.Contains(standing, t) {
			continue
		}
		ln := "- " + t
		if used+len(ln)+1 > budget {
			continue
		}
		out = append(out, ln)
		used += len(ln) + 1
	}
	return out
}

// recallTwoLayer implements the unified design §2.2 two-layer contextual recall within budget bytes:
//   - Unified cross-pass semantic deduplication (norm.Jaccard >= 0.45) initialized from standing so no rule is
//     repeated across standing, verbatim UserOwned/L1 recall, local rule recall, and 1-hop entity expansion;
//   - Main rule slot (~70%, <= 1400 B for recall; 100% for recall-noent): high-precision non-standing UserOwned/L1
//     rules (score >= 5.0, <= 650 B) + high-IDF non-entity local rules;
//   - Controlled entity supplement slot (remaining ~30%, >= 648 B when entities=true): concise 1-hop [实体骨架]
//     edges (<= 2/entity, <= 220 B) + persistent high-IDF World/People entity facts FIRST (never evicted by
//     duplicate general rules) + deduplicated 1-hop neighbor rules in any remaining room;
//   - Zero-waste backfill: when an incident has few/no entity facts, unused entity-slot bytes flow back to remaining
//     non-entity local rules so non-entity scenes never lose budget relative to recall-noent.
func (idx collabNameIndex) recallTwoLayer(st *store.State, ov *overview.State, f heFilter, day, situation, standing string, budget int, entities bool) string {
	if budget <= 0 {
		return ""
	}
	var out []string
	seen := map[string]bool{}
	var keptBodies []string
	for _, sl := range strings.Split(standing, "\n") {
		if b := collabStripRecallPrefix(sl); b != "" {
			keptBodies = append(keptBodies, b)
		}
	}
	used := 0
	addLine := func(ln string, capBytes int, dedupSemantic bool) bool {
		ln = strings.TrimSpace(ln)
		if ln == "" || seen[ln] {
			return false
		}
		if used+len(ln)+1 > capBytes || used+len(ln)+1 > budget {
			return false
		}
		body := collabStripRecallPrefix(ln)
		if dedupSemantic && body != "" {
			if slices.ContainsFunc(keptBodies, func(prev string) bool { return norm.Jaccard(prev, body) >= 0.45 }) {
				return false
			}
		}
		seen[ln] = true
		if dedupSemantic && body != "" {
			keptBodies = append(keptBodies, body)
		}
		out = append(out, ln)
		used += len(ln) + 1
		return true
	}

	// Pass 1: Main rule slot (identical priority in recall and recall-noent so entity facts never evict core rules).
	mainCap := min(1400, budget*70/100)
	if !entities {
		mainCap = budget
	}
	l1Budget := min(650, mainCap*45/100)
	for _, ln := range recallNonStandingL1(st, ov, f, day, situation, standing, l1Budget) {
		addLine(ln, mainCap, true)
	}
	var nonEntLines []string
	if budget-used > 40 {
		for _, ln := range strings.Split(idx.recall(st, f, day, situation, standing, budget, false), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				nonEntLines = append(nonEntLines, ln)
			}
		}
		for _, ln := range nonEntLines {
			addLine(ln, mainCap, true)
		}
	}
	if !entities {
		return strings.Join(out, "\n")
	}

	// Pass 2: Controlled entity supplement slot (remaining budget - used, >= ~648 B):
	// 2a: Concise 1-hop [实体骨架] edges (<= 2 per top-2 entity, <= 220 B total);
	// 2b: Persistent high-IDF World/People entity facts FIRST (never crowded out by ec.Rules);
	// 2c: Deduplicated 1-hop neighbor rules (ec.Rules) in leftover room.
	var hopRules []string
	if st != nil && st.Graph != nil && budget-used > 40 {
		cleanSt := &store.State{
			Version:    st.Version,
			Onboarding: st.Onboarding,
			Contacts:   st.Contacts,
			Graph:      sliceGraphBefore(st.Graph, day),
			Entries:    make(map[string]*entry.Entry, len(st.Entries)),
		}
		for id, e := range st.Entries {
			if e == nil || len(e.Cats()) > 0 || e.OriginSpace != "" || e.Inferred || f.entryOut(st, e, day) {
				continue
			}
			cleanSt.Entries[id] = e
		}
		type entQ struct {
			name string
			df   int
		}
		var qs []entQ
		seenQ := map[string]bool{}
		for n, es := range idx {
			if !seenQ[n] && collabContainsEntity(situation, n) {
				seenQ[n] = true
				qs = append(qs, entQ{name: n, df: len(es)})
			}
		}
		for _, idt := range collabIdent.FindAllString(situation, -1) {
			if !seenQ[idt] && collabContainsEntity(situation, idt) {
				seenQ[idt] = true
				qs = append(qs, entQ{name: idt, df: 1})
			}
		}
		sort.SliceStable(qs, func(i, j int) bool {
			if qs[i].df != qs[j].df {
				return qs[i].df < qs[j].df
			}
			return len(qs[i].name) > len(qs[j].name)
		})
		if len(qs) > 2 {
			qs = qs[:2]
		}
		skelCap := min(budget, used+220)
		for _, q := range qs {
			sr := recall.Search(cleanSt, recall.Query{Text: q.name, Limit: 4})
			for _, ec := range sr.Entities {
				skelAdded := 0
				for _, edge := range ec.Skeleton {
					if skelAdded >= 2 {
						break
					}
					if addLine(fmt.Sprintf("- [实体骨架] %s --%s--> %s", edge.From, edge.Predicate, edge.To), skelCap, false) {
						skelAdded++
					}
				}
				for i, rule := range ec.Rules {
					if i >= 2 {
						break
					}
					t := heClip(rule.Text, 200)
					if t == "" || collabEphemeralFact.MatchString(t) {
						continue
					}
					hopRules = append(hopRules, "- "+t)
				}
			}
		}
	}

	// 2b: Persistent high-IDF World/People entity facts before 1-hop rules so ownership/topology facts are never evicted.
	if rem := budget - used; rem > 40 {
		for _, ln := range idx.recallEntityFacts(st, f, day, situation, standing, rem*2) {
			addLine(ln, budget, true)
		}
	}

	// 2c: Deduplicated 1-hop neighbor rules in any remaining entity-slot room.
	if budget-used > 40 {
		for _, ln := range hopRules {
			addLine(ln, budget, true)
		}
	}

	// Pass 3: Zero-waste backfill — if few/no entity facts matched, return unused bytes to remaining non-entity rules.
	if budget-used > 40 {
		for _, ln := range nonEntLines {
			addLine(ln, budget, true)
		}
	}
	return strings.Join(out, "\n")
}

// recall is what mentioning the incident's names brings in, as the hub's recall would: the entries whose subjects the
// situation names, first seen before the day, not already in the standing memory, highest rank first, in budget
// bytes. entities false leaves out the world and people entries (the ablation).
func (idx collabNameIndex) recall(st *store.State, f heFilter, day, situation, standing string, budget int, entities bool) string {
	// hits: how specifically the situation's names point at an entry, each name weighing by how few entries it is
	// behind (idf), so "Persona" brings in less than a repository's own name.
	hits := map[string]float64{}
	total := float64(max(1, len(st.Entries)))
	idf := func(df int) float64 { return math.Log(total / float64(1+df)) }
	var cands []*entry.Entry
	seen := map[string]bool{}
	add := func(e *entry.Entry, w float64) {
		if !seen[e.ID] {
			seen[e.ID] = true
			cands = append(cands, e)
		}
		hits[e.ID] += w
	}
	for n, es := range idx {
		if !strings.Contains(situation, n) {
			continue
		}
		entOK := collabContainsEntity(situation, n)
		w := idf(len(es))
		for _, e := range es {
			isEnt := e.Facet == entry.World || e.Facet == entry.People
			if f.entryOut(st, e, day) || (isEnt && (!entities || !entOK)) {
				continue
			}
			add(e, w)
		}
	}
	// A fact that names the thing in its text without carrying it as a subject (an older world line) is found by the
	// name too, as a search would.
	if entities {
		names := map[string]bool{}
		for n := range idx {
			if collabContainsEntity(situation, n) && len([]rune(n)) >= 4 {
				names[n] = true
			}
		}
		for _, n := range collabIdent.FindAllString(situation, -1) { // a repository, a service, a command named in it
			if collabContainsEntity(situation, n) {
				names[n] = true
			}
		}
		for n := range names {
			var found []*entry.Entry
			for _, e := range st.Entries {
				if (e.Facet == entry.World || e.Facet == entry.People) && strings.Contains(e.Shown(), n) {
					found = append(found, e)
				}
			}
			w := idf(len(found))
			for _, e := range found {
				if !memsrc.Pageable(st, e) || len(e.Cats()) > 0 || e.OriginSpace != "" || e.Inferred || f.entryOut(st, e, day) {
					continue
				}
				add(e, w)
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if hits[a.ID] != hits[b.ID] {
			return hits[a.ID] > hits[b.ID]
		}
		if ra, rb := memsrc.Rank(a), memsrc.Rank(b); ra != rb {
			return ra > rb
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		return a.ID < b.ID
	})
	var lines []string
	for _, e := range cands {
		if len(lines) > 64 {
			break
		}
		t := heClip(e.Shown(), 200)
		if strings.Contains(standing, t) {
			continue
		}
		lines = append(lines, "- "+t)
	}
	return heFit(lines, budget)
}
