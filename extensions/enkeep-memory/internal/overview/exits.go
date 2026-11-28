package overview

import (
	"regexp"
	"slices"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
)

// The exit (unified design §2.1): the used L1 principles and collaboration contracts go to the hub through the
// extension's own memory source, memory.entries, beside the curated entries; the connector and the memory source keep
// their ids.
//
//   - a pattern with no category and no restricted space: "ov:pattern:<id>", rank 950–985 by its spread when recalled,
//     and a candidate for the three-bucket standing set with its Score and L1 metadata (memsrc decides whether it
//     stands, never the maker);
//   - one resting on any categorised evidence or said in a space: "ov:cat:<category>:<id>", never standing, rank 900,
//     with every category and the space, for the hub to grade by scene.
const (
	rankTop           = 985
	rankBottom        = 950
	rankRunbookTop    = 940
	rankRunbookBottom = 840
	rankCat           = 900
)

// genericRuleTypes name no specific occasion and do not trigger the per-group 20% cap in the contract bucket.
var genericRuleTypes = map[string]bool{"": true, "correction": true, "preference": true, "纠正": true}

// independentQuotes returns the pattern's distinct normalised user quote count, falling back to SourcesCount for
// older states written before UniqueQuotes was recorded.
func independentQuotes(p *Pattern) int {
	if p.UniqueQuotes > 0 {
		return p.UniqueQuotes
	}
	return p.SourcesCount
}

// Score ranks a used L1 pattern within its standing bucket by multi-dimensional leverage (unified design §2.1):
//   - Hard contract:     80 + min(20, viol*4) + min(24, scenes*6) + min(16, quotes*2) + owned(12+min(18,cov*3)) + check(6);
//     capped at 70 when confined to a single scene and not user-owned (keeps narrow incidents in entity recall).
//   - Regular contract:  lifecycle(core:30 / scoped:24) + min(24, scenes*6) + min(20, quotes*2) + min(16, weeks*4)
//   - owned(15+min(15,cov*3)); single-scene non-owned capped at 70.
//   - Principle:         lifecycle(core:30 / scoped:26) + min(25, scenes*5) + min(20, weeks*4) + min(15, quotes*2)
//   - hasPolesOrException(10) + owned(18) + decisionOrAttention(8) + judgementOrWorkflow(6)
func Score(p *Pattern) int {
	if p == nil || !p.Used() {
		return 0
	}
	quotes := independentQuotes(p)
	weeks := len(p.ActiveWeeks)
	cov := len(p.CoveredIDs)
	owned := cov > 0
	if p.IsProfileCore() {
		s := 110 + min(20, p.ScenesCount*5) + min(15, weeks*3) + min(10, quotes*2)
		if owned {
			s += 15
		}
		return s
	}
	if p.IsOpsRunbook() {
		s := 40 + min(20, p.ScenesCount*5) + min(20, quotes*3)
		if owned {
			s += 10
		}
		return s
	}
	if p.IsContract() {
		if p.Hard {
			s := 80 + min(20, p.ViolationCount*4) + min(24, p.ScenesCount*6) + min(16, quotes*2)
			if owned {
				s += 12 + min(18, cov*3)
			}
			if p.Polarity == PolarityCheck {
				s += 6
			}
			if p.ScenesCount <= 1 && !owned {
				s = min(s, 70)
			}
			return s
		}
		base := 24
		if p.Lifecycle == Core {
			base = 30
		}
		s := base + min(24, p.ScenesCount*6) + min(20, quotes*2) + min(16, weeks*4)
		if owned {
			s += 15 + min(15, cov*3)
		}
		if p.ScenesCount <= 1 && !owned {
			s = min(s, 70)
		}
		return s
	}
	base := 26
	if p.Lifecycle == Core {
		base = 30
	}
	bonus := 0
	if poleText(p.Poles) != "" || strings.TrimSpace(p.Exception) != "" {
		bonus += 10
	}
	if owned {
		bonus += 18
	}
	if p.Dimension == "decision" || p.Dimension == "attention" {
		bonus += 8
	}
	if p.Facet == string(entry.Judgement) || p.Facet == string(entry.Workflow) {
		bonus += 6
	}
	return base + min(25, p.ScenesCount*5) + min(20, weeks*4) + min(15, quotes*2) + bonus
}

// facetLabel names a facet in the entry's text, so a pattern read alone says which side of the user it is about.
var facetLabel = map[string]string{
	string(entry.Interaction): "和 AI 打交道",
	string(entry.Tools):       "AI 和工具",
	string(entry.Judgement):   "默认判断",
	string(entry.Workflow):    "做事方式",
	string(entry.World):       "人、项目和事实",
	string(entry.Persona):     "关于用户",
}

// suffixOf returns the boundary suffix ("（两端：…）" or "（例外：…）") for p, or "" if none applies or if the exception
// merely repeats p.Text.
func suffixOf(p *Pattern) string {
	if ends := poleText(p.Poles); ends != "" {
		return "（两端：" + ends + "）"
	}
	x := strings.TrimSpace(p.Exception)
	if x == "" {
		return ""
	}
	body := strings.TrimSpace(p.Text)
	if strings.Contains(body, x) || norm.Jaccard(body, x) >= 0.45 || norm.Overlap(body, x) >= 0.55 {
		return ""
	}
	if qb, qx := norm.Q(body), norm.Q(x); qx != "" && strings.Contains(qb, qx) {
		return ""
	}
	return "（例外：" + x + "）"
}

// Text is a pattern as one line the assistant can act on (for recall and UI). A pattern whose evidence goes both ways
// says both ends, each with when it holds (Poles), not "X unless W".
func Text(p *Pattern) string {
	return "跨事模式（" + facetLabel[p.Facet] + "）：" + strings.TrimSpace(p.Text) + suffixOf(p)
}

// polesRestate is how much of a pattern's statement its two ends already say (bigram overlap) for the standing line
// to carry the ends alone.
const polesRestate = 0.5

// StandingParts are a pattern's standing line in its compact form, body and suffix: both ends as "A时→X；B时→Y" with no
// "（两端：…）" wrapper; when the statement only sums up its two ends (polesRestate), the ends alone are the line, so
// the room goes to the content and not to saying it twice. A pattern without ends keeps its statement and exception.
func StandingParts(p *Pattern) (body, suffix string) {
	body = strings.TrimSpace(p.Text)
	ends := compactPoles(p.Poles)
	if ends == "" {
		return body, suffixOf(p)
	}
	if norm.Overlap(body, ends) >= polesRestate || norm.Contains(ends, body) {
		return ends + "。", ""
	}
	return strings.TrimRight(body, "。.") + "：" + ends + "。", ""
}

// compactPoles is both ends of a pattern as "A时→X；B时→Y"; "" unless there are two.
func compactPoles(ps []Pole) string {
	var parts []string
	for _, p := range ps {
		w, c := strings.TrimSpace(p.When), strings.TrimRight(strings.TrimSpace(p.Choice), "。.")
		if w == "" || c == "" {
			continue
		}
		parts = append(parts, strings.TrimSuffix(w, "时")+"时→"+c)
	}
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:2], "；")
}

// poleText is both ends of a pattern, "在 A 时 X；在 B 时 Y"; "" unless there are two.
func poleText(ps []Pole) string {
	var parts []string
	for _, p := range ps {
		w, c := strings.TrimSpace(p.When), strings.TrimSpace(p.Choice)
		if w == "" || c == "" {
			continue
		}
		parts = append(parts, strings.TrimSuffix(w, "时")+"时，"+c)
	}
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:2], "；")
}

// contractGroup returns "<facet>:<normalized rule_type>" for a contract's non-generic occasion/type, or "".
func contractGroup(p *Pattern) string {
	if !p.IsContract() {
		return ""
	}
	rt := strings.ToLower(strings.TrimSpace(p.RuleType))
	if genericRuleTypes[rt] {
		return ""
	}
	q := norm.Q(p.RuleType)
	if q == "" {
		return ""
	}
	return p.Facet + ":" + q
}

// patternIdent matches high-specificity ASCII tool, command, host, service, or repo names inside an L1 pattern text
// (e.g. task_plan, agent_start, sj-wsl, 1Password, Cloudflare, todo_complete) so on-demand recall and decisions_query
// can reach non-standing L1 contracts and principles by concrete tool/entity tags (unified design §2.2).
var patternIdent = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.-]{2,}`)

var patternIdentStop = map[string]bool{
	"the": true, "and": true, "for": true, "not": true, "all": true, "true": true, "false": true,
	"ttl": true, "api": true, "cli": true, "url": true, "pdf": true, "json": true, "md": true,
}

type patternOccasionRule struct {
	cues []string
	tags []string
}

var patternOccasions = []patternOccasionRule{
	{cues: []string{"派活", "派发", "派 Agent", "主管", "task_plan", "agent_start", "agent_send", "conversation_send"}, tags: []string{"派活", "dispatch"}},
	{cues: []string{"汇报", "交付", "结论", "摘要", "进展", "耗时", "时间戳"}, tags: []string{"汇报", "report"}},
	{cues: []string{"提问", "追问", "请示", "确认", "待定", "选项", "大白话", "授权卡", "卡片"}, tags: []string{"提问", "ask"}},
	{cues: []string{"开工", "执行", "plan mode", "大纲", "框架", "方案"}, tags: []string{"开工", "task"}},
	{cues: []string{"验收", "核实", "核对", "源头", "门禁", "测试", "编译"}, tags: []string{"验收", "门禁"}},
	{cues: []string{"排期", "估时间", "工期", "估计交付"}, tags: []string{"排期", "估时间"}},
	{cues: []string{"密钥", "凭据", "token", "1Password", "op CLI", "secret_env"}, tags: []string{"密钥", "凭据"}},
	{cues: []string{"配置", "升级", "回滚", "镜像", "备份"}, tags: []string{"配置", "升级"}},
	{cues: []string{"待办", "todo_complete", "STATE.md"}, tags: []string{"待办"}},
	{cues: []string{"飞书", "云文档", "多维表格"}, tags: []string{"飞书"}},
	{cues: []string{"评审", "质疑", "分歧", "独立判断"}, tags: []string{"评审", "review"}},
}

// EnrichSubjects builds up to memsrc.MaxSubjects recallable subjects for an L1 pattern by preserving its existing
// Subjects and deterministically adding high-specificity tool/entity identifiers and occasion words present in its
// text/exception. This keeps the ~60 non-standing L1 contracts and principles reachable via decisions_query and
// contextual recall without re-running L1 models (unified design §2.2).
func EnrichSubjects(p *Pattern) []string {
	if p == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= memsrc.MaxSubjects {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}
	for _, s := range p.Subjects {
		add(s)
	}
	src := strings.TrimSpace(p.Text) + " " + strings.TrimSpace(p.Exception)
	for _, m := range patternIdent.FindAllString(src, -1) {
		m = strings.Trim(m, ".-_")
		if len(m) < 3 || patternIdentStop[strings.ToLower(m)] {
			continue
		}
		hasLetter := false
		for i := 0; i < len(m); i++ {
			if (m[i] >= 'a' && m[i] <= 'z') || (m[i] >= 'A' && m[i] <= 'Z') {
				hasLetter = true
				break
			}
		}
		if !hasLetter {
			continue
		}
		add(m)
	}
	low := strings.ToLower(src)
	for _, r := range patternOccasions {
		for _, cue := range r.cues {
			if strings.Contains(low, strings.ToLower(cue)) {
				for _, tag := range r.tags {
					add(tag)
				}
				break
			}
		}
	}
	if p.Facet == string(entry.Tools) {
		add("工具")
		add("tool")
	}
	return out
}

// Exits are the patterns as memory.entries entries, each with its last evidence day, score, and L1 metadata for the
// three-bucket standing set.
func Exits(s *State) []memsrc.Extra {
	if s == nil {
		return nil
	}
	var out []memsrc.Extra
	for _, p := range s.Sorted(func(p *Pattern) bool { return p.Used() }) {
		hex := strings.TrimPrefix(p.ID, "ov:")
		x := memsrc.Entry{Text: clip(Text(p), memsrc.MaxText), Facet: p.Facet, Status: "active", Subjects: EnrichSubjects(p)}
		kind := ""
		l1Kind := ""
		if len(p.Categories) > 0 || p.OriginSpace != "" {
			cat := "space"
			if len(p.Categories) > 0 {
				cat = p.Categories[0]
			}
			x.ID = "ov:cat:" + cat + ":" + hex
			x.Categories = p.Categories[:min(len(p.Categories), memsrc.MaxCats)]
			x.OriginSpace = p.OriginSpace
			x.Rank = rankCat
		} else if p.IsOpsRunbook() || patternIsOpsRunbook(p) {
			// Class C ops_runbook: never enters global 4 KB standing (kind="" / l1Kind=""), stays high-rank (840-940)
			// in on-demand recall with enriched tool/host/command Subjects (v0.4.0 §1.7).
			x.ID = "ov:runbook:" + hex
			x.Rank = min(rankRunbookTop, rankRunbookBottom+p.ScenesCount*p.SourcesCount*4)
			kind = ""
			l1Kind = ""
		} else if p.IsProfileCore() {
			x.ID = "ov:profile:" + hex
			x.Rank = rankTop
			kind = entry.KindUnderstanding
			l1Kind = memsrc.BucketProfile
		} else {
			x.ID = "ov:pattern:" + hex
			x.Rank = min(rankTop, rankBottom+p.ScenesCount*p.SourcesCount/2)
			kind = entry.KindOfFacet(p.Facet)
			if p.IsContract() {
				l1Kind = memsrc.BucketContract
			} else {
				l1Kind = memsrc.BucketPrinciple
			}
		}
		body, suffix := StandingParts(p)
		out = append(out, memsrc.Extra{
			Entry:      x,
			Last:       p.LastEvidence,
			Score:      Score(p),
			Kind:       kind,
			Evidence:   slices.Clone(p.EvidenceIDs),
			L1Kind:     l1Kind,
			Hard:       p.Hard,
			Polarity:   p.Polarity,
			Dimension:  p.Dimension,
			Group:      contractGroup(p),
			Quotes:     independentQuotes(p),
			Body:       body,
			Suffix:     suffix,
			CoveredIDs: slices.Clone(p.CoveredIDs),
		})
	}
	return out
}
