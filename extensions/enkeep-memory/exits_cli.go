package main

// Offline exit compile and look-up on a copy of the state (0.2.0-beta.37): what the per-turn block, the skills and the
// project blocks would hold, and what enkeep_recall answers, without a hub or a model. Lines already rewritten are
// reused; an entry without one goes out with its own text when it passes the check. Nothing is written to a hub.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	exits "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/graph"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/people"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

func readState(path string) (*store.State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &store.State{}
	return st, json.Unmarshal(raw, st)
}

// exitsCmd: enkeep-memory exits -state <state.json> -out <dir> [-projects <json: [{"id","name","aliases"}]>] [-lang zh]
func exitsCmd(args []string) error {
	fs := flag.NewFlagSet("exits", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	out := fs.String("out", "", "an empty directory for the exit files")
	projects := fs.String("projects", "", "the knowledge base's projects as JSON [{\"id\",\"name\",\"aliases\"}]; empty: the list kept in the state")
	lang := fs.String("lang", "zh-Hans", "language of the fixed wording")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || *out == "" {
		return fmt.Errorf("-state and -out are required")
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	var ps []entry.KBProject
	if *projects != "" {
		raw, err := os.ReadFile(*projects)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
	} else if st.Exits != nil {
		ps = st.Exits.Projects
	}
	st.Exits = nil // a fresh manifest in out: every file is written
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	rep, err := exits.Compile(context.Background(), st, exits.Options{Dir: *out, Lang: *lang, Loc: time.Local, Projects: ps})
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(rep.Files))
	for p := range rep.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := rep.Files[p]
		fmt.Printf("%s\t%s\t%d bytes\t%d entries\n", f.Exit, p, f.Bytes, f.Entries)
	}
	var secs []string
	for _, s := range exits.SectionOrder {
		secs = append(secs, fmt.Sprintf("%s %dB/%d lines", s, rep.Sections[s], rep.SecLines[s]))
	}
	fmt.Println("exit A sections: " + strings.Join(secs, "; "))
	skills, _ := filepath.Glob(filepath.Join(*out, exits.SkillsDir, "*"))
	fmt.Printf("skills: %d\n", len(skills))
	return nil
}

// recallCmd: enkeep-memory recall -state <state.json> -q <query> [-facets a,b] [-limit n]
func recallCmd(args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	q := fs.String("q", "", "the query")
	facets := fs.String("facets", "", "comma-separated facets")
	limit := fs.Int("limit", recall.DefaultLimit, "most lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	query := recall.Query{Text: *q, Limit: *limit}
	for _, f := range strings.Split(*facets, ",") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		x, ok := recall.FacetOf(f)
		if !ok {
			return fmt.Errorf("unknown facet %q", f)
		}
		query.Facets = append(query.Facets, x)
	}
	res := recall.Search(st, query)
	fmt.Printf("%d hit(s) of %d entries (%d entity skeleton(s))\n", len(res.Hits), res.Pool, len(res.Entities))
	for _, ec := range res.Entities {
		fmt.Print(recall.FormatEntity(ec))
	}
	for _, h := range res.Hits {
		fmt.Print(recall.Format(h))
	}
	return nil
}

// ReviewBundleSummary captures machine-checkable acceptance metrics for the 6 v0.4.0 review artifacts (§4.3).
type ReviewBundleSummary struct {
	StateDir                   string         `json:"state_dir"`
	OutDir                     string         `json:"out_dir"`
	Today                      string         `json:"today"`
	ResidentBytesLimit         int            `json:"resident_bytes_limit"`
	ResidentBytesActual        int            `json:"resident_bytes_actual"`
	ResidentLines              int            `json:"resident_lines"`
	ResidentHotLines           int            `json:"resident_hot_lines"`
	ResidentProfileLines       int            `json:"resident_profile_lines"`
	ResidentPrincipleLines     int            `json:"resident_principle_lines"`
	ResidentContractLines      int            `json:"resident_contract_lines"`
	ResidentOpsHits            int            `json:"resident_ops_hits"`
	ResidentS6Hits             int            `json:"resident_s6_hits"`
	ResidentOWCHits            int            `json:"resident_owc_hits"`
	ResidentSensitiveHits      int            `json:"resident_sensitive_hits"`
	ResidentDuplicates         int            `json:"resident_duplicates"`
	ResidentTruncatedLines     int            `json:"resident_truncated_lines"`
	ProfileCoreUsed            int            `json:"profile_core_used"`
	PrinciplesUsed             int            `json:"principles_used"`
	PrinciplesTotal            int            `json:"principles_total"`
	PrinciplesWithPoles        int            `json:"principles_with_poles"`
	ContractsUsed              int            `json:"contracts_used"`
	OpsRunbooksUsed            int            `json:"ops_runbooks_used"`
	ProfileSupervisorHits      int            `json:"profile_supervisor_hits"`
	ProfileEmoWorkHits         int            `json:"profile_emotion_work_hits"`
	ProfileHealthWorkHits      int            `json:"profile_health_work_hits"`
	ProfileSensitivePeopleHits int            `json:"profile_sensitive_people_hits"`
	ProfileWorldPeopleLines    int            `json:"profile_world_people_lines"`
	ProfileWorldMaxPerPerson   int            `json:"profile_world_max_per_person"`
	ProfileLowConfidenceHits   int            `json:"profile_low_confidence_hits"`
	ProfileSensitiveWorkHits   int            `json:"profile_sensitive_work_hits"`
	GraphNodesTotal            int            `json:"graph_nodes_total"`
	GraphNodesActive           int            `json:"graph_nodes_active"`
	GraphTasksArchived         int            `json:"graph_tasks_archived"`
	GraphTasksActive           int            `json:"graph_tasks_active"`
	GraphAboutArchived         int            `json:"graph_about_archived"`
	GraphAboutActive           int            `json:"graph_about_active"`
	GraphMerges                int            `json:"graph_merges"`
	GraphCollabMirrors         int            `json:"graph_collab_mirrors"`
	ContactsShown              int            `json:"contacts_shown"`
	SensitiveByCat             map[string]int `json:"sensitive_by_cat"`
	ColdstartLines             int            `json:"coldstart_lines"`
	ColdstartBytes             int            `json:"coldstart_bytes"`
	ColdstartProfileLines      int            `json:"coldstart_profile_lines"`
	ColdstartPrincipleLines    int            `json:"coldstart_principle_lines"`
	ColdstartContractLines     int            `json:"coldstart_contract_lines"`
	Files                      []string       `json:"files"`
}

// reviewBundleCmd: enkeep-memory review-bundle -state <dir> -out <dir> [-lang zh-Hans] [-resident-bytes 4096] [-today YYYY-MM-DD]
func reviewBundleCmd(args []string) error {
	fs := flag.NewFlagSet("review-bundle", flag.ContinueOnError)
	stateDir := fs.String("state", "", "state directory containing state.json, overview-state.json, profile-summary.json")
	outDir := fs.String("out", "", "output directory for the 6 v0.4.0 review artifacts")
	lang := fs.String("lang", "zh-Hans", "language for rendering")
	residentBytes := fs.Int("resident-bytes", 4096, "resident budget in bytes (800-16384: the hub's 4, 8 or 16 KB)")
	today := fs.String("today", "", "reference date YYYY-MM-DD (default: latest active day)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*stateDir) == "" || strings.TrimSpace(*outDir) == "" {
		return fmt.Errorf("-state and -out are required")
	}
	sum, err := GenerateReviewBundle(*stateDir, *outDir, *lang, *residentBytes, *today)
	if err != nil {
		return err
	}
	fmt.Printf("review-bundle written to %s (%d files)\n", *outDir, len(sum.Files))
	fmt.Printf("  resident: %d/%dB, %d lines (ops=%d s6=%d owc=%d sens=%d dup=%d)\n",
		sum.ResidentBytesActual, sum.ResidentBytesLimit, sum.ResidentLines,
		sum.ResidentOpsHits, sum.ResidentS6Hits, sum.ResidentOWCHits, sum.ResidentSensitiveHits, sum.ResidentDuplicates)
	fmt.Printf("  principles: used=%d/%d (with_poles=%d), contracts_used=%d\n",
		sum.PrinciplesUsed, sum.PrinciplesTotal, sum.PrinciplesWithPoles, sum.ContractsUsed)
	fmt.Printf("  graph: merges=%d active_nodes=%d/%d tasks_archived=%d about_archived=%d collab_mirrors=%d\n",
		sum.GraphMerges, sum.GraphNodesActive, sum.GraphNodesTotal, sum.GraphTasksArchived, sum.GraphAboutArchived, sum.GraphCollabMirrors)
	fmt.Printf("  coldstart: %d lines, %dB\n", sum.ColdstartLines, sum.ColdstartBytes)
	return nil
}

var (
	auditOpsResidentRe = regexp.MustCompile(`(?i)\b(sj-wsl|herdr|task_plan|cloudflare|email-relay)\b`)
	auditOWCShellRe    = regexp.MustCompile(`(?i)\bowc\b|在\s*linux\s*上验证成功`)
	auditProfileSupRe  = regexp.MustCompile(`担任.{0,10}(平台主管|网关主管)|Persona\s*平台主管|AsterGate\s*网关主管`)
)

// GenerateReviewBundle generates the 6 v0.4.0 human-review Markdown artifacts plus summary-v04.json into outDir.
func GenerateReviewBundle(stateDir, outDir, lang string, residentBytes int, today string) (*ReviewBundleSummary, error) {
	if residentBytes <= 0 {
		residentBytes = 4096
	}
	residentBytes = memsrc.ClampResidentBytes(residentBytes)
	if lang == "" {
		lang = "zh-Hans"
	}
	stPath := stateDir
	dir := stateDir
	if info, err := os.Stat(stateDir); err == nil && info.IsDir() {
		stPath = filepath.Join(stateDir, "state.json")
	} else {
		dir = filepath.Dir(stateDir)
	}
	st, err := readState(stPath)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if today == "" {
		if len(st.ActiveDays) > 0 {
			today = st.ActiveDays[len(st.ActiveDays)-1]
		} else {
			today = time.Now().Format("2006-01-02")
		}
	}
	ovHolder, err := overview.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open overview: %w", err)
	}
	ov := ovHolder.Snapshot()

	prof, err := profile.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("load profile: %w", err)
	}
	if prof == nil {
		refTime := time.Now()
		if t, perr := time.Parse("2006-01-02", today); perr == nil {
			refTime = t
		}
		prof = profile.BuildWith(st, overview.OverviewBackedProvider{Snapshot: func() *overview.State { return ov }}, refTime, lang)
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}

	sum := &ReviewBundleSummary{
		StateDir:           dir,
		OutDir:             outDir,
		Today:              today,
		ResidentBytesLimit: residentBytes,
		SensitiveByCat:     map[string]int{},
	}

	// 1. resident-v04.md
	residentMD, err := renderResidentV04(st, ov, prof, residentBytes, sum)
	if err != nil {
		return nil, fmt.Errorf("render resident-v04.md: %w", err)
	}
	// 2. principles-v04.md
	principlesMD := renderPrinciplesV04(ov, sum)
	// 3. profile-v04.md
	profileMD := renderProfileV04(st, ov, prof, sum)
	// 4. entities-v04.md
	entitiesMD := renderEntitiesV04(st, today, sum)
	// 5. sensitive-v04.md
	sensitiveMD := renderSensitiveV04(st, sum)
	// 6. coldstart-v04.md
	coldstartMD, err := renderColdstartV04(st, residentBytes, lang, today, sum)
	if err != nil {
		return nil, fmt.Errorf("render coldstart-v04.md: %w", err)
	}

	artifacts := []struct {
		name string
		body string
	}{
		{"resident-v04.md", residentMD},
		{"principles-v04.md", principlesMD},
		{"profile-v04.md", profileMD},
		{"entities-v04.md", entitiesMD},
		{"sensitive-v04.md", sensitiveMD},
		{"coldstart-v04.md", coldstartMD},
	}
	for _, a := range artifacts {
		p := filepath.Join(outDir, a.name)
		if err := writeFileAtomic0600(p, []byte(a.body)); err != nil {
			return nil, err
		}
		sum.Files = append(sum.Files, p)
	}
	sumPath := filepath.Join(outDir, "summary-v04.json")
	if err := writeMigrateJSON(sumPath, sum); err != nil {
		return nil, err
	}
	return sum, nil
}

func collectStandingEntries(st *store.State, ov *overview.State, prof *profile.Summary, residentBytes int) ([]memsrc.Entry, error) {
	extras := overview.Exits(ov)
	res, _, err := memsrc.PageWith(st, prof, extras, memsrc.Params{
		Contribution:  memorySourceID,
		Limit:         memsrc.MaxLimit,
		ResidentBytes: residentBytes,
	})
	if err != nil {
		return nil, err
	}
	var standing []memsrc.Entry
	for _, e := range res.Entries {
		if e.Standing {
			standing = append(standing, e)
		}
	}
	return standing, nil
}

func classifyStandingSection(e memsrc.Entry, ov *overview.State) string {
	if e.Rank >= 994 {
		return memsrc.BucketHot
	}
	if strings.HasPrefix(e.ID, "ov:profile:") {
		return memsrc.BucketProfile
	}
	if hexID, ok := strings.CutPrefix(e.ID, "ov:pattern:"); ok && ov != nil {
		p := ov.Patterns["ov:"+hexID]
		if p == nil {
			p = ov.Patterns[hexID]
		}
		if p != nil {
			switch p.KindOf() {
			case overview.KindProfileCore:
				return memsrc.BucketProfile
			case overview.KindPrinciple:
				return memsrc.BucketPrinciple
			case overview.KindContract:
				return memsrc.BucketContract
			}
		}
	}
	switch e.Facet {
	case string(entry.Persona):
		return memsrc.BucketProfile
	case string(entry.Interaction), string(entry.Tools):
		return memsrc.BucketContract
	default:
		return memsrc.BucketPrinciple
	}
}

func renderResidentV04(st *store.State, ov *overview.State, prof *profile.Summary, residentBytes int, sum *ReviewBundleSummary) (string, error) {
	standing, err := collectStandingEntries(st, ov, prof, residentBytes)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 新版 %s 常驻全文（resident-v04）\n\n", residentRoomLabel(residentBytes))
	b.WriteString("> 以下编号、条目 ID 与字节标注仅用于审阅，不占用实际注入的常驻字节；每条实际注入的就是正文本身（hub 另加行首「- 」与换行）。\n\n")

	totalBytes := 0
	seenText := map[string]bool{}
	grouped := map[string][]memsrc.Entry{}
	secBytes := map[string]int{}
	for _, e := range standing {
		sz := len(e.Text) + 3 // "- " + "\n"
		totalBytes += sz
		sec := classifyStandingSection(e, ov)
		grouped[sec] = append(grouped[sec], e)
		secBytes[sec] += sz

		if auditOpsResidentRe.MatchString(e.Text) {
			sum.ResidentOpsHits++
		}
		if auditOWCShellRe.MatchString(e.Text) {
			sum.ResidentOWCHits++
		}
		if len(e.Categories) > 0 {
			sum.ResidentSensitiveHits++
		}
		if raw := st.Entries[e.ID]; raw != nil && entry.IsAssistantNote(raw.Origin) {
			sum.ResidentS6Hits++
		}
		if strings.Contains(e.Text, "…") {
			sum.ResidentTruncatedLines++
		}
		key := strings.TrimSpace(strings.ToLower(e.Text))
		if seenText[key] {
			sum.ResidentDuplicates++
		}
		seenText[key] = true
	}
	sum.ResidentLines = len(standing)
	sum.ResidentBytesActual = totalBytes
	sum.ResidentHotLines = len(grouped[memsrc.BucketHot])
	sum.ResidentProfileLines = len(grouped[memsrc.BucketProfile])
	sum.ResidentPrincipleLines = len(grouped[memsrc.BucketPrinciple])
	sum.ResidentContractLines = len(grouped[memsrc.BucketContract])

	fmt.Fprintf(&b, "- 总条数：%d（热槽 %d | 画像与防误读 %d | 核心认知原则 %d | 通用协作契约 %d）\n",
		len(standing), sum.ResidentHotLines, sum.ResidentProfileLines, sum.ResidentPrincipleLines, sum.ResidentContractLines)
	fmt.Fprintf(&b, "- 总字节数：%d / %d B（超限：%t；热槽 %dB | 画像 %dB | 原则 %dB | 契约 %dB）\n",
		totalBytes, residentBytes, totalBytes > residentBytes,
		secBytes[memsrc.BucketHot], secBytes[memsrc.BucketProfile], secBytes[memsrc.BucketPrinciple], secBytes[memsrc.BucketContract])
	fmt.Fprintf(&b, "- 红线审计：ops_runbook=%d | S6直通=%d | OWC外壳=%d | 敏感条目=%d | 重复=%d | 截断=%d\n\n",
		sum.ResidentOpsHits, sum.ResidentS6Hits, sum.ResidentOWCHits, sum.ResidentSensitiveHits, sum.ResidentDuplicates, sum.ResidentTruncatedLines)

	sections := []struct {
		key   string
		title string
	}{
		{memsrc.BucketHot, "热槽（0–10%）"},
		{memsrc.BucketProfile, fmt.Sprintf("第 1 段 · 关于你与防误读边界（约 %.0f%%）", memsrc.ProfileShare*100)},
		{memsrc.BucketPrinciple, fmt.Sprintf("第 2 段 · 核心认知与决策原则（约 %.0f%%）", memsrc.PrincipleShare*100)},
		{memsrc.BucketContract, fmt.Sprintf("第 3 段 · 通用协作契约（约 %.0f%%）", memsrc.ContractShare*100)},
	}
	for _, s := range sections {
		items := grouped[s.key]
		fmt.Fprintf(&b, "## %s（%d 条 / %dB）\n\n", s.title, len(items), secBytes[s.key])
		if len(items) == 0 {
			b.WriteString("_（空）_\n\n")
			continue
		}
		for i, e := range items {
			fmt.Fprintf(&b, "%d. %s　〔%s · %s · %d 字节〕\n", i+1, e.Text, facetNameZH(e.Facet), e.ID, len(e.Text))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// residentRoomLabel names a resident room for a report title: "4 KB" for 4096, "1400 B" otherwise.
func residentRoomLabel(n int) string {
	if n%1024 == 0 {
		return fmt.Sprintf("%d KB", n/1024)
	}
	return fmt.Sprintf("%d B", n)
}

// facetNameZH is a facet's short Chinese name for review reports.
func facetNameZH(f string) string {
	switch entry.Facet(f) {
	case entry.Interaction:
		return "协作"
	case entry.Tools:
		return "工具"
	case entry.Judgement:
		return "判断"
	case entry.Workflow:
		return "做事"
	case entry.World:
		return "人和事"
	case entry.People:
		return "人物"
	case entry.Persona:
		return "画像"
	}
	return f
}

func renderPrinciplesV04(ov *overview.State, sum *ReviewBundleSummary) string {
	var b strings.Builder
	b.WriteString("# 全部 L1 核心原理清单（principles-v04）\n\n")

	var used, other []*overview.Pattern
	for _, p := range ov.Sorted(nil) {
		if p == nil {
			continue
		}
		switch p.KindOf() {
		case overview.KindProfileCore:
			if p.Used() {
				sum.ProfileCoreUsed++
			}
			continue
		case overview.KindContract:
			if p.Used() {
				sum.ContractsUsed++
			}
			continue
		case overview.KindOpsRunbook:
			if p.Used() {
				sum.OpsRunbooksUsed++
			}
			continue
		}
		sum.PrinciplesTotal++
		if p.Used() {
			used = append(used, p)
			if len(p.Poles) > 0 {
				sum.PrinciplesWithPoles++
			}
		} else {
			other = append(other, p)
		}
	}
	sum.PrinciplesUsed = len(used)

	fmt.Fprintf(&b, "- 活跃核心原理（A2 Principle）：%d 条（带张力 Poles：%d 条）\n", len(used), sum.PrinciplesWithPoles)
	fmt.Fprintf(&b, "- 其余原理（Candidate/Drifting/Retired）：%d 条\n", len(other))
	fmt.Fprintf(&b, "- 同批 L1 参考计数：画像核（A1）=%d 条 | 通用契约（B）=%d 条 | 运维手册（C，仅按需召回）=%d 条\n\n",
		sum.ProfileCoreUsed, sum.ContractsUsed, sum.OpsRunbooksUsed)

	b.WriteString("## 活跃核心原理\n\n")
	if len(used) == 0 {
		b.WriteString("_（无活跃原理）_\n\n")
	}
	for i, p := range used {
		fmt.Fprintf(&b, "### %d. `%s` [%s / %s / rooted=%t]\n", i+1, p.ID, p.Facet, p.Lifecycle, p.Rooted)
		fmt.Fprintf(&b, "- **表述**：%s\n", p.Text)
		if p.Dimension != "" {
			fmt.Fprintf(&b, "- **维度**：%s\n", p.Dimension)
		}
		if len(p.Poles) > 0 {
			b.WriteString("- **张力两端（Poles）**：\n")
			for _, pole := range p.Poles {
				fmt.Fprintf(&b, "  - 当「%s」→ %s\n", pole.When, pole.Choice)
			}
		}
		if p.Exception != "" {
			fmt.Fprintf(&b, "- **例外**：%s\n", p.Exception)
		}
		fmt.Fprintf(&b, "- **支撑**：scenes=%d, sources=%d, unique_quotes=%d, weeks=%d, counter_ratio=%.2f\n\n",
			p.ScenesCount, p.SourcesCount, p.UniqueQuotes, len(p.ActiveWeeks), p.CounterRatio)
	}

	if len(other) > 0 {
		b.WriteString("## 非活跃原理（供核查）\n\n")
		for _, p := range other {
			fmt.Fprintf(&b, "- `%s` [%s / %s / gate=%s] %s\n", p.ID, p.Facet, p.Lifecycle, p.Gate, p.Text)
		}
	}
	return b.String()
}

// isSensitiveProfileWorkLeak reports whether a point placed inside profile.Sensitive is actually an ops_runbook or a
// non-privacy work-methodology principle/contract that should never appear in profile:cat:*.
func isSensitiveProfileWorkLeak(st *store.State, ov *overview.State, pt profile.Point) bool {
	if strings.HasPrefix(pt.ID, "ov:runbook:") {
		return true
	}
	if ov != nil {
		pid := pt.ID
		if hexID, ok := strings.CutPrefix(pid, "ov:pattern:"); ok {
			pid = "ov:" + hexID
		} else if hexID, ok := strings.CutPrefix(pid, "ov:profile:"); ok {
			pid = "ov:" + hexID
		}
		if p := ov.Patterns[pid]; p != nil {
			if p.IsOpsRunbook() {
				return true
			}
			f := entry.Facet(p.Facet)
			if entry.IsWorkMethodFacet(f) && len(entry.PayloadCategories(f, p.Text, p.Situation, p.Choice, p.Exception)) == 0 {
				return true
			}
			return false
		}
	}
	if st != nil {
		if e := st.Entries[pt.ID]; e != nil {
			if entry.IsOpsRunbookEntry(e) {
				return true
			}
			if entry.IsWorkMethodFacet(e.Facet) {
				for _, c := range e.Categories {
					if c.By == entry.ByUser {
						return false
					}
				}
				return len(entry.PayloadCategories(e.Facet, e.PayloadTexts()...)) == 0
			}
			return false
		}
	}
	for _, pfx := range []string{"i-", "t-", "j-", "w-"} {
		if strings.HasPrefix(pt.ID, pfx) && len(entry.PayloadCategories(entry.Interaction, pt.Text)) == 0 {
			return true
		}
	}
	return false
}

func renderProfileV04(st *store.State, ov *overview.State, prof *profile.Summary, sum *ReviewBundleSummary) string {
	var b strings.Builder
	b.WriteString("# 新版用户侧写（profile-v04）\n\n")
	if prof == nil {
		b.WriteString("_（无侧写产物）_\n")
		return b.String()
	}

	checkSup := func(s string) {
		if auditProfileSupRe.MatchString(s) {
			sum.ProfileSupervisorHits++
		}
	}
	checkSup(prof.Overview)
	for _, r := range prof.Recent {
		checkSup(r)
	}
	for _, sec := range prof.Sections {
		checkSup(sec.Summary)
		for _, pt := range sec.Points {
			checkSup(pt.Text)
		}
	}
	for _, ssec := range prof.Sensitive {
		for _, pt := range ssec.Points {
			if isSensitiveProfileWorkLeak(st, ov, pt) {
				sum.ProfileSensitiveWorkHits++
				if ssec.Category == entry.CatEmotion {
					sum.ProfileEmoWorkHits++
				}
				if ssec.Category == entry.CatHealth {
					sum.ProfileHealthWorkHits++
				}
			}
		}
	}

	// Someone else's records in the sensitive sections, people lines in the world section (and the most of any one
	// person), and LOW_CONFIDENCE / one-off misreads anywhere in the profile.
	raw := func(id string) *entry.Entry {
		if st == nil {
			return nil
		}
		return st.Entries[id]
	}
	for _, ssec := range prof.Sensitive {
		for _, pt := range ssec.Points {
			if e := raw(pt.ID); e != nil && e.Facet == entry.People {
				sum.ProfileSensitivePeopleHits++
			}
		}
	}
	perPerson := map[string]int{}
	for _, sec := range prof.Sections {
		for _, pt := range sec.Points {
			e := raw(pt.ID)
			if e == nil {
				continue
			}
			if entry.IsUnreliableOneOff(e) {
				sum.ProfileLowConfidenceHits++
			}
			if sec.Facet != entry.World {
				continue
			}
			var names []string
			if e.Facet == entry.People {
				names = []string{e.Person}
			} else if strings.HasPrefix(e.Subject, "person:") {
				names = []string{e.Subject}
				if strings.HasPrefix(e.Target, "person:") {
					names = append(names, e.Target)
				}
			}
			if len(names) > 0 {
				sum.ProfileWorldPeopleLines++
			}
			for _, n := range names {
				perPerson[n]++
				sum.ProfileWorldMaxPerPerson = max(sum.ProfileWorldMaxPerPerson, perPerson[n])
			}
		}
	}
	for _, ssec := range prof.Sensitive {
		for _, pt := range ssec.Points {
			if e := raw(pt.ID); e != nil && entry.IsUnreliableOneOff(e) {
				sum.ProfileLowConfidenceHits++
			}
		}
	}

	fmt.Fprintf(&b, "- 模式：%s | 语言：%s | 日期：%s | 覆盖源条目：%d/%d\n",
		prof.Mode, prof.Lang, prof.Day, prof.CoveredCount, prof.SourceCount)
	fmt.Fprintf(&b, "- 红线审计：AI主管幻觉=%d | 敏感区混入工作规矩/手册=%d（emotion=%d, health=%d）| 敏感区混入他人记录=%d | 低置信/孤立误抽=%d\n",
		sum.ProfileSupervisorHits, sum.ProfileSensitiveWorkHits, sum.ProfileEmoWorkHits, sum.ProfileHealthWorkHits,
		sum.ProfileSensitivePeopleHits, sum.ProfileLowConfidenceHits)
	fmt.Fprintf(&b, "- 人和事分区：他人人物卡片与他人关系 %d 条（单人最多 %d 条）\n\n", sum.ProfileWorldPeopleLines, sum.ProfileWorldMaxPerPerson)

	if prof.Overview != "" {
		b.WriteString("## 总览\n\n")
		b.WriteString(prof.Overview + "\n\n")
	}
	if len(prof.Recent) > 0 {
		b.WriteString("## 近期关注\n\n")
		for _, r := range prof.Recent {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}
	b.WriteString("## 通用侧写分区\n\n")
	for _, sec := range prof.Sections {
		fmt.Fprintf(&b, "### %s（%d 条）\n\n", sec.Facet, len(sec.Points))
		if sec.Summary != "" {
			fmt.Fprintf(&b, "> %s\n\n", sec.Summary)
		}
		for _, pt := range sec.Points {
			fmt.Fprintf(&b, "- `[%s]` (%s) %s\n", pt.ID, pt.Last, pt.Text)
		}
		b.WriteString("\n")
	}
	b.WriteString("## 敏感侧写分区（严格隔离）\n\n")
	if len(prof.Sensitive) == 0 {
		b.WriteString("_（无敏感侧写条目）_\n")
	}
	for _, ssec := range prof.Sensitive {
		fmt.Fprintf(&b, "### profile:cat:%s（space=%s，%d 条）\n\n", ssec.Category, ssec.OriginSpace, len(ssec.Points))
		for _, pt := range ssec.Points {
			fmt.Fprintf(&b, "- `[%s]` (%s) %s\n", pt.ID, pt.Last, pt.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderEntitiesV04(st *store.State, today string, sum *ReviewBundleSummary) string {
	var b strings.Builder
	b.WriteString("# 合并后的人/事档案与图谱报告（entities-v04）\n\n")

	if st.Graph != nil {
		g := st.Graph
		sum.GraphNodesTotal = len(g.Nodes)
		sum.GraphMerges = len(g.Merges)
		for _, n := range g.Nodes {
			if n == nil {
				continue
			}
			if n.Type == "task" {
				if n.Status == "archived" {
					sum.GraphTasksArchived++
				} else if n.MergedInto == "" && n.Status != "not_entity" {
					sum.GraphTasksActive++
				}
			}
			if n.MergedInto == "" && n.Status != "archived" && n.Status != "not_entity" {
				sum.GraphNodesActive++
			}
		}
		collabSeen := map[string]bool{}
		for _, a := range g.Assertions {
			if a == nil {
				continue
			}
			p := graph.TypeName(a.P)
			if p == "about" {
				if a.Status == "archived" {
					sum.GraphAboutArchived++
				} else if a.Status != "retracted" && a.Status != "rejected" {
					sum.GraphAboutActive++
				}
			}
			if p == "collaborates_with" && a.Status != "archived" && a.Status != "retracted" && a.Status != "rejected" {
				s, o := g.Resolve(a.S), g.Resolve(a.O)
				if s != o {
					if collabSeen[o+"->"+s] {
						sum.GraphCollabMirrors++
					}
					collabSeen[s+"->"+o] = true
				}
			}
		}

		b.WriteString("## 图谱健康指标\n\n")
		fmt.Fprintf(&b, "- 节点总数：%d（活跃：%d）\n", sum.GraphNodesTotal, sum.GraphNodesActive)
		fmt.Fprintf(&b, "- 一次性 `task` 节点：已归档=%d，残余活跃=%d\n", sum.GraphTasksArchived, sum.GraphTasksActive)
		fmt.Fprintf(&b, "- 噪音 `about` 边：已归档=%d，残余活跃=%d\n", sum.GraphAboutArchived, sum.GraphAboutActive)
		fmt.Fprintf(&b, "- 双向 `collaborates_with` 镜像对：%d\n", sum.GraphCollabMirrors)
		fmt.Fprintf(&b, "- 实体归并记录（`graph.merges`）：%d\n\n", sum.GraphMerges)

		b.WriteString("## 实体归并明细（最多展示前 100 条）\n\n")
		if len(g.Merges) == 0 {
			b.WriteString("_（无归并记录）_\n\n")
		} else {
			limit := min(len(g.Merges), 100)
			for i := 0; i < limit; i++ {
				mr := g.Merges[i]
				fromName, intoName := mr.From, mr.Into
				if fn := g.Nodes[mr.From]; fn != nil && fn.Name != "" {
					fromName = fmt.Sprintf("%s (%s)", fn.Name, mr.From)
				}
				if tn := g.Nodes[mr.Into]; tn != nil && tn.Name != "" {
					intoName = fmt.Sprintf("%s (%s)", tn.Name, mr.Into)
				}
				fmt.Fprintf(&b, "%d. %s → %s (`%s`, %s)\n", i+1, fromName, intoName, mr.Proposal, mr.At)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("## 核心联系人档案抽检（前 20 位）\n\n")
	ranked := people.Ranked(st.Contacts)
	in := people.Input{
		Entries:     st.Entries,
		Contacts:    st.Contacts,
		Merges:      st.PersonMerges,
		GraphMerges: graph.PersonMergesFromGraph(st),
		Switches:    st.Switches,
		Rhythm:      st.Rhythm,
		Today:       today,
	}
	seenKey := map[string]bool{}
	shown := 0
	for _, ct := range ranked {
		if ct == nil || entry.IsGenericPersonName(ct.Name) {
			continue
		}
		card := people.BuildCard(in, ct.Person)
		if card == nil || seenKey[card.Person] {
			continue
		}
		seenKey[card.Person] = true
		shown++
		fmt.Fprintf(&b, "### %d. %s (`%s`, score=%d)\n", shown, card.Name, card.Person, ct.Score)
		lines := 0
		for _, sec := range people.Sections {
			for _, ln := range card.Parts[sec] {
				fmt.Fprintf(&b, "- [%s] %s\n", sec, ln.Text)
				lines++
				if lines >= 8 {
					break
				}
			}
			if lines >= 8 {
				break
			}
		}
		if lines == 0 {
			b.WriteString("- _（无持久化卡片条目）_\n")
		}
		b.WriteString("\n")
		if shown >= 20 {
			break
		}
	}
	sum.ContactsShown = shown
	return b.String()
}

func renderSensitiveV04(st *store.State, sum *ReviewBundleSummary) string {
	var b strings.Builder
	b.WriteString("# 敏感分类全量清单（sensitive-v04）\n\n")

	byCat := map[string][]*entry.Entry{}
	for _, e := range st.Entries {
		if e == nil || !e.Live() || e.Status != entry.Stable || !e.IsSensitive() {
			continue
		}
		for _, c := range e.Cats() {
			byCat[c] = append(byCat[c], e)
		}
	}

	total := 0
	for _, cat := range entry.CategoryNames {
		list := byCat[cat]
		sum.SensitiveByCat[cat] = len(list)
		total += len(list)
	}
	fmt.Fprintf(&b, "- 在用（Live + Stable）敏感条目总计：%d\n", total)
	fmt.Fprintf(&b, "- 常驻 4 KB 敏感条目泄漏数：%d\n\n", sum.ResidentSensitiveHits)

	for _, cat := range entry.CategoryNames {
		list := byCat[cat]
		sort.Slice(list, func(i, j int) bool {
			if list[i].Facet != list[j].Facet {
				return list[i].Facet < list[j].Facet
			}
			return list[i].ID < list[j].ID
		})
		fmt.Fprintf(&b, "## %s（%d 条）\n\n", cat, len(list))
		if len(list) == 0 {
			b.WriteString("_（无）_\n\n")
			continue
		}
		for _, e := range list {
			by := "legacy"
			for _, c := range e.Categories {
				if entry.CategoryOf(c.Name) == cat {
					by = c.By
					break
				}
			}
			fmt.Fprintf(&b, "- `[%s]` (facet=%s, by=%s, space=%s) %s\n", e.ID, e.Facet, by, e.OriginSpace, e.Shown())
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderColdstartV04(st *store.State, residentBytes int, lang, today string, sum *ReviewBundleSummary) (string, error) {
	refTime := time.Now()
	if t, err := time.Parse("2006-01-02", today); err == nil {
		refTime = t
	}
	// Deep-clone state and strip all S6 assistant notes (handwritten / assistant_note), passing ov=nil and prof=nil
	// to genuinely simulate a blank new user with 0 S6 notes + 0 L1 patterns relying purely on T0/T1 L0 behavior data.
	clone, err := st.Clone(refTime)
	if err != nil {
		return "", err
	}
	strippedS6 := 0
	for id, e := range clone.Entries {
		if e == nil {
			continue
		}
		if entry.IsAssistantNote(e.Origin) {
			delete(clone.Entries, id)
			strippedS6++
		}
	}

	standing, err := collectStandingEntries(clone, nil, nil, residentBytes)
	if err != nil {
		return "", err
	}

	totalBytes := 0
	coldGrouped := map[string][]memsrc.Entry{}
	for _, e := range standing {
		totalBytes += len(e.Text) + 3
		sec := classifyStandingSection(e, nil)
		coldGrouped[sec] = append(coldGrouped[sec], e)
	}
	sum.ColdstartLines = len(standing)
	sum.ColdstartBytes = totalBytes
	sum.ColdstartProfileLines = len(coldGrouped[memsrc.BucketProfile])
	sum.ColdstartPrincipleLines = len(coldGrouped[memsrc.BucketPrinciple])
	sum.ColdstartContractLines = len(coldGrouped[memsrc.BucketContract])

	var b strings.Builder
	b.WriteString("# 空白新用户冷启动仿真产物（0 S6 + 0 L1 纯行为冷启动仿真 · coldstart-v04）\n\n")
	fmt.Fprintf(&b, "- 仿真条件：0 S6 + 0 L1 纯行为冷启动仿真——剔除全部 S6 笔记（共移除 %d 条），置空全部 L1 Overview（`ov=nil`）与 `profile-summary`，仅凭 T0/T1 L0 行为数据通过三段回退编译常驻\n", strippedS6)
	fmt.Fprintf(&b, "- 产出条数：%d 条（画像 %d | 原则 %d | 契约 %d | 热槽 %d）| 总字节数：%d / %d B | 语言：%s\n\n",
		len(standing), sum.ColdstartProfileLines, sum.ColdstartPrincipleLines, sum.ColdstartContractLines,
		len(coldGrouped[memsrc.BucketHot]), totalBytes, residentBytes, lang)

	b.WriteString("## 冷启动常驻全文\n\n")
	if len(standing) == 0 {
		b.WriteString("_（空）_\n")
	}
	for i, e := range standing {
		sec := classifyStandingSection(e, nil)
		fmt.Fprintf(&b, "%d. `[%s]` (section=%s, facet=%s, rank=%d) %s\n", i+1, e.ID, sec, e.Facet, e.Rank, e.Text)
	}
	return b.String(), nil
}
