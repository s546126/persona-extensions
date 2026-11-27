package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageUI       = "search-as-code"
	cardUI       = "search-as-code-card"
	maxShownHits = 25
)

const (
	modeHome       = ""
	modeHit        = "hit"
	modeRecipeEdit = "recipe-edit"
)

type page struct {
	Tab          string `json:"t,omitempty"`
	Scope        string `json:"s,omitempty"`
	Query        string `json:"q,omitempty"`
	RecipeID     string `json:"r,omitempty"`
	HitID        string `json:"h,omitempty"`
	EditRecipeID string `json:"er,omitempty"`
	Mode         string `json:"m,omitempty"`
}

type draw struct {
	st    *State
	now   time.Time
	loc   *time.Location
	fleet *fleet // nil: no other computers
	ctx   context.Context
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var p page
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: "invalid page state"}
	}
	var v *wire.View
	err := a.db().read(func(st *State) {
		v = a.drawer(ctx, st, r).view(p)
	})
	return v, err
}

func (a *app) drawer(ctx context.Context, st *State, r *ext.UIRequest) draw {
	loc, err := time.LoadLocation(r.TZ)
	if err != nil {
		loc = time.UTC
	}
	a.mu.Lock()
	f := a.fleet
	a.mu.Unlock()
	if !f.hasRemote() {
		f = nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return draw{st: st, now: a.now(), loc: loc, fleet: f, ctx: ctx}
}

func (d draw) activeExecution(p page) *ExecutionResult {
	scope := cmp.Or(p.Scope, d.st.DefaultScope, "conversation")
	if !isValidScope(scope) {
		scope = "conversation"
	}
	if d.st.LastRun != nil && p.Query == d.st.LastRun.Program.Query &&
		(p.Scope == "" || p.Scope == d.st.LastRun.Program.Scope) &&
		(p.RecipeID == "" || p.RecipeID == d.st.LastRun.Program.RecipeID) {
		return d.st.LastRun
	}
	in := CompileInput{
		Query: p.Query,
		Scope: scope,
	}
	if p.RecipeID != "" {
		in.Recipe = d.st.recipe(p.RecipeID)
	}
	prog, err := CompileProgram(in)
	if err != nil {
		prog, _ = CompileProgram(CompileInput{Scope: scope})
	}
	return ExecuteProgram(d.st, prog, d.now)
}

func (d draw) view(p page) *wire.View {
	switch p.Mode {
	case modeHit:
		return d.hitDetailView(p)
	case modeRecipeEdit:
		return d.recipeFormView(p)
	default:
		return d.homeView(p)
	}
}

func (d draw) homeView(p page) *wire.View {
	scope := cmp.Or(p.Scope, d.st.DefaultScope, "conversation")
	if !isValidScope(scope) {
		scope = "conversation"
	}
	p.Scope = scope

	tab := cmp.Or(p.Tab, "results")
	if !slices.Contains([]string{"results", "code", "recipes", "sources"}, tab) {
		tab = "results"
	}
	p.Tab = tab

	res := d.activeExecution(p)
	recipes := d.st.allRecipes()

	v := &wire.View{
		Title: pageTitle,
		State: ext.EncodeState(p),
	}

	scopeChoices := make([]wire.Choice, len(scopes))
	for i, sc := range scopes {
		scopeChoices[i] = wire.Choice{Value: sc, Title: scopeLabels[sc]}
	}
	v.Filter = &wire.Filter{
		Name:    "scope",
		Title:   "搜哪些内容",
		Choices: scopeChoices,
		Value:   scope,
		Action:  wire.Action{ID: "scope"},
	}

	v.Search = &wire.Search{
		Placeholder: "想找什么？例如：上次 429 报错是怎么修好的",
		Value:       p.Query,
		Action:      wire.Action{ID: "search"},
	}

	v.Tabs = &wire.Tabs{
		Name:  "tab",
		Value: tab,
		Items: []wire.Tab{
			{Value: "results", Title: "结果", Badge: countBadge(res.TotalMatches)},
			{Value: "recipes", Title: "常用搜索", Badge: countBadge(len(recipes))},
			{Value: "sources", Title: "搜哪些地方"},
			{Value: "code", Title: "高级：怎么搜的"},
		},
		Action: wire.Action{ID: "tab"},
	}

	switch tab {
	case "code":
		v.Sections = d.codeAndTraceSections(p, res)
	case "recipes":
		v.Sections = d.recipesSections(p, recipes)
	case "sources":
		v.Sections = d.sourcesSections(p)
	default:
		v.Sections = d.resultsSections(p, res, recipes)
	}
	v.Sections = append(d.restoreSection(), v.Sections...)
	return v
}

func (d draw) resultsSections(p page, res *ExecutionResult, recipes []*Recipe) []wire.Section {
	var secs []wire.Section
	fresh := p.Query == "" && p.RecipeID == ""

	// 1. What this is for and how to start, until the user searched for something, or while there is nothing to
	// search yet.
	ready := readinessOf(res, d.fleet != nil)
	if fresh || ready != readyToShow {
		secs = append(secs, introSection())
	}
	// Nothing read at all: say what is missing and what to do instead of a row of zeros.
	if ready != readyToShow {
		return append(secs, d.setupSections(res, ready)...)
	}

	// 2. Where this result came from: how much was searched, on how many computers.
	stats := []wire.Stat{
		{Label: "找到", Value: fmt.Sprintf("%d 段", res.TotalMatches), Tone: toneIf(res.TotalMatches > 0, wire.TonePositive)},
		{Label: "搜了", Value: fmt.Sprintf("%d 段聊天", res.ScannedSessions) + coldNote(res.ScannedArchived, res.ScannedUnconfirmed, res.ArchivedOmitted)},
	}
	if len(res.Machines) > 0 {
		stats = append(stats, machinesStat(res.Machines))
	}
	stats = append(stats, wire.Stat{Label: "用时", Value: durationLabel(res.TotalDurationMs)})
	var recipe *Recipe
	if p.RecipeID != "" {
		recipe = d.st.recipe(p.RecipeID)
	}
	secs = append(secs, wire.Section{
		Title:  resultTitle(res, recipe),
		Footer: "想看每一步是怎么搜的，打开「高级：怎么搜的」。",
		Items:  []wire.Component{{Type: wire.ComponentStatGrid, Key: "res-stats", Stats: stats}},
	})

	// 3. Saved searches to try with one tap, while no words are typed.
	if p.Query == "" {
		recSec := wire.Section{
			Title:  "常用搜索（点一下就搜）",
			Footer: "也可以在「常用搜索」里把自己常搜的存起来。",
		}
		for _, r := range recipes[:min(len(recipes), 4)] {
			badgeText := scopeLabels[r.Scope]
			if p.RecipeID == r.ID {
				badgeText = "正在看"
			}
			recSec.Items = append(recSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      "quick-rec-" + r.ID,
				Icon:     cmp.Or(r.Icon, "search"),
				Title:    r.Name,
				Subtitle: r.Description,
				Badge:    &wire.Badge{Text: badgeText, Tone: toneIf(p.RecipeID == r.ID, wire.TonePositive)},
				Action:   &wire.Action{ID: "run-recipe", Values: map[string]string{"id": r.ID}},
			})
		}
		secs = append(secs, recSec)
	}

	// 4. Nothing matched.
	if len(res.Hits) == 0 {
		empty := wire.Component{
			Type:   wire.ComponentEmpty,
			Icon:   "search",
			Title:  "没找到相关的聊天",
			Detail: "换几个词试试，或者把上面的「搜哪些内容」换成「全部」。一直什么都搜不到的话，到「搜哪些地方」看看有没有找到聊天记录。",
			Button: &wire.EmptyButton{Title: "清空重搜", Action: wire.Action{ID: "reset"}},
		}
		secs = append(secs, wire.Section{Title: "找到的聊天", Items: []wire.Component{empty}})
		return secs
	}

	// 5. How the hits spread over projects (or computers, models).
	if len(res.Groups) > 0 {
		field := res.Groups[0].Field
		grpStats := make([]wire.Stat, 0, min(len(res.Groups), wire.StatsMost))
		for _, g := range res.Groups[:min(len(res.Groups), wire.StatsMost)] {
			val := fmt.Sprintf("%d 段", g.Count)
			tone := wire.ToneNeutral
			if g.ErrorCount > 0 {
				val += fmt.Sprintf("（%d 段有报错）", g.ErrorCount)
				tone = wire.ToneCaution
			}
			grpStats = append(grpStats, wire.Stat{
				Label: clipRunes(groupKeyLabel(field, g.Key), 20),
				Value: val,
				Tone:  tone,
			})
		}
		secs = append(secs, wire.Section{
			Title: "按" + groupFieldLabel(field) + "分",
			Items: []wire.Component{{Type: wire.ComponentStatGrid, Key: "grp-stats", Stats: grpStats}},
		})
	}

	// 6. The hits, most relevant first.
	hitSec := wire.Section{
		Title: fmt.Sprintf("找到的聊天 · %d 段", len(res.Hits)),
	}
	shownHits := res.Hits[:min(len(res.Hits), maxShownHits)]
	for i, h := range shownHits {
		icon := "chat"
		if h.SourceKind == "file" {
			icon = "file"
		} else if h.ErrorCount > 0 {
			icon = "warning"
		}
		metaParts := []string{cmp.Or(h.Project, "未归入项目"), harnessLabel(h.Harness)}
		if len(res.Machines) > 0 || h.Machine != "" {
			metaParts = append([]string{"电脑 " + machineTitle(machineLabel(h.Machine, h.MachineName))}, metaParts...)
		}
		if h.Model != "" {
			metaParts = append(metaParts, h.Model)
		}
		if h.Branch != "" {
			metaParts = append(metaParts, "分支 "+h.Branch)
		}
		if h.TurnCount > 0 {
			metaParts = append(metaParts, fmt.Sprintf("%d 条消息", h.TurnCount))
		}
		archived := h.Location == locationArchived
		cold := archived || h.Location == locationUnconfirmed
		if cold && h.ErrorCount > 0 {
			metaParts = append(metaParts, fmt.Sprintf("%d 处报错", h.ErrorCount))
		}
		var badge *wire.Badge
		if h.ErrorCount > 0 {
			badge = &wire.Badge{
				Text: fmt.Sprintf("%d 处报错", h.ErrorCount),
				Tone: wire.ToneDanger,
			}
		}
		if cold {
			icon = "cloud"
			badge = &wire.Badge{Text: locationLabel(h.Location), Tone: wire.ToneCaution}
		}
		if h.Cached && !cold {
			badge = &wire.Badge{Text: "离线副本", Tone: wire.ToneCaution}
		}
		restoring := archived && d.st.restoreRunning(h.EntryID)
		if restoring {
			badge = &wire.Badge{Text: "恢复中", Tone: wire.ToneCaution}
		}
		hitPage := p
		hitPage.Mode = modeHit
		hitPage.HitID = h.ID
		row := wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("hit-row-%d", i),
			Icon:     icon,
			Title:    clipRunes(h.Title, 120),
			Subtitle: strings.Join(metaParts, " · "),
			Value:    d.formatTime(h.UpdatedAt),
			Badge:    badge,
			Action:   pushPage(hitPage, h.Title),
		}
		if archived && h.EntryID != "" && !restoring {
			row.Actions = []wire.RowAction{{Title: "恢复", Icon: "refresh", Swipe: true, Action: *restoreAction(h.Machine, h.EntryID, h.OriginalPath, h.Harness, h.SessionID)}}
		}
		hitSec.Items = append(hitSec.Items, row)
		for j, sn := range h.Snippets[:min(len(h.Snippets), 2)] {
			hitSec.Items = append(hitSec.Items, wire.Component{
				Type:  wire.ComponentText,
				Key:   fmt.Sprintf("hit-snip-%d-%d", i, j),
				Style: wire.TextStyleSecondary,
				Text:  snippetPrefix(sn) + sn.Text,
			})
		}
		if len(h.Correlated) > 0 {
			var corrNames []string
			for _, cr := range h.Correlated[:min(len(h.Correlated), 2)] {
				corrNames = append(corrNames, fmt.Sprintf("%s（%s）", clipRunes(cr.Title, 28), cr.Reason))
			}
			hitSec.Items = append(hitSec.Items, wire.Component{
				Type:  wire.ComponentText,
				Key:   fmt.Sprintf("hit-corr-%d", i),
				Style: wire.TextStyleCaption,
				Text:  "相关的其他聊天：" + strings.Join(corrNames, "；"),
			})
		}
	}
	hitSec.Footer = "点一段聊天看前后经过。"
	if len(res.Hits) > maxShownHits {
		hitSec.Footer = fmt.Sprintf("只显示最相关的 %d 段（共 %d 段）。换几个更具体的词可以缩小范围。", maxShownHits, len(res.Hits))
	}
	secs = append(secs, hitSec)
	return secs
}

// noSessionsDetail says where chats are read from and what to do when none were found.
const noSessionsDetail = "这里会自动读取这台电脑上 Claude Code（~/.claude/projects）和 Codex（~/.codex/sessions）的聊天记录。先用它们聊几次再回来；记录放在别的文件夹的话，到「搜哪些地方」把文件夹填上；在别的电脑上的话，到「" + settingsPlace + "」的「" + machinesSetting + "」里选上那台电脑。"

// introSection says what the page is for, gives one example and the ways to start a search.
func introSection() wire.Section {
	return wire.Section{
		Title: "这是做什么的",
		Items: []wire.Component{
			{
				Type: wire.ComponentText,
				Key:  "intro-what",
				Text: "在你以前和 Claude Code、Codex 的聊天记录里找东西。比如想知道「上次那个 429 报错是怎么修好的？」，在上面的搜索框输入「429」，就能找到当时的聊天，还会标出是哪个项目、哪台电脑上的。",
			},
			{
				Type:  wire.ComponentText,
				Key:   "intro-how",
				Style: wire.TextStyleSecondary,
				Text:  "怎么开始：在上面的搜索框里输入几个词；或者点下面的常用搜索；也可以直接对 Persona 助手说「帮我在以前的聊天记录里找找……」，它读之前会先问你。",
			},
		},
	}
}

// snippetPrefix says who said a snippet ("你：", "文件 第 3 行：").
func snippetPrefix(sn EvidenceSnippet) string {
	if sn.Role == "File" && sn.Line > 0 {
		return fmt.Sprintf("文件 第 %d 行：", sn.Line)
	}
	return roleLabel(sn.Role) + "："
}

// durationLabel is a run's duration in plain words.
func durationLabel(ms int64) string {
	if ms < 1000 {
		return "不到 1 秒"
	}
	return fmt.Sprintf("%.1f 秒", float64(ms)/1000)
}

func (d draw) codeAndTraceSections(p page, res *ExecutionResult) []wire.Section {
	var secs []wire.Section

	// 1. Step Execution Trace Table
	cols := []wire.TableColumn{
		{Title: "第几步"},
		{Title: "步骤"},
		{Title: "进 → 留下", Align: "end"},
		{Title: "用时", Align: "end"},
		{Title: "做了什么"},
	}
	rows := make([][]string, 0, len(res.StepTraces))
	for _, tr := range res.StepTraces {
		durStr := fmt.Sprintf("%d µs", tr.DurationUs)
		if tr.DurationMs >= 1 {
			durStr = fmt.Sprintf("%d ms", tr.DurationMs)
		}
		rows = append(rows, []string{
			strconv.Itoa(tr.Index),
			clipRunes(tr.Code, 80),
			fmt.Sprintf("%d → %d", tr.InputCount, tr.OutputCount),
			durStr,
			clipRunes(tr.Note, 80),
		})
	}
	secs = append(secs, wire.Section{
		Title:  "这次是怎么搜的",
		Footer: "给想自己调整搜索的人看。每次搜索都会变成上面这样几步，按顺序执行：表里记着每一步进来多少条、留下多少条、花了多久。",
		Items: []wire.Component{
			{
				Type:     wire.ComponentCode,
				Key:      "trace-code-full",
				Path:     "search_program.sac",
				Language: "elixir",
				Text:     res.Program.CodeListing,
			},
			{
				Type:    wire.ComponentTable,
				Key:     "trace-table",
				Columns: cols,
				Rows:    rows,
			},
		},
	})

	if len(res.Machines) > 0 {
		rows := make([][]string, 0, len(res.Machines))
		for _, m := range res.Machines {
			rows = append(rows, []string{machineTitle(m.Name), machineStatusLabel(m.Status), strconv.Itoa(m.Sessions), strconv.Itoa(m.Hits), durationLabel(m.DurationMs), m.Note})
		}
		secs = append(secs, wire.Section{
			Title:  "每台电脑的情况",
			Footer: "在线的电脑在它自己那里搜，只把找到的片段传回来；不在线的电脑，搜的是之前同步过来的精简副本。",
			Items: []wire.Component{{
				Type:    wire.ComponentTable,
				Key:     "trace-machines",
				Columns: []wire.TableColumn{{Title: "电脑"}, {Title: "状态"}, {Title: "搜了几段", Align: "end"}, {Title: "找到", Align: "end"}, {Title: "用时", Align: "end"}, {Title: "说明"}},
				Rows:    rows,
			}},
		})
	}

	// 2. Interactive Search-as-Code Pipeline Runner & Recipe Saver
	secs = append(secs, wire.Section{
		Title:  "自己写搜索步骤",
		Footer: "一行一步，用 |> 连起来。能用的步骤：scan_sessions（搜聊天）、scan_files（搜代码文件夹）、where（按项目、模型、时间筛选）、exclude_tools（去掉命令和工具输出）、match_regex（按规则匹配）、correlate（找提到同一文件的其他聊天）、dedup（去重）、group_by（分组）、rank（排序）、limit（最多几条）。加 machine=\"电脑\" 只搜那台电脑。",
		Items: []wire.Component{
			{
				Type:        wire.ComponentField,
				Name:        "custom_code",
				Label:       "搜索步骤",
				Kind:        wire.FieldKindText,
				Value:       res.Program.CodeListing,
				Placeholder: `scan_sessions(query="timeout", scope="conversation") |> exclude_tools() |> correlate(by="file") |> group_by(field="project") |> rank(by="relevance_freshness") |> limit(n=20)`,
			},
			{
				Type:   wire.ComponentButton,
				Title:  "按这些步骤搜",
				Action: &wire.Action{ID: "run-code", Submit: true},
			},
			{
				Type:   wire.ComponentButton,
				Title:  "存为常用搜索",
				Action: pushPage(page{Scope: p.Scope, Query: res.Program.CodeListing, Mode: modeRecipeEdit}, "存为常用搜索"),
			},
		},
	})

	// 3. Recent Runs History
	if len(d.st.History) > 0 {
		histSec := wire.Section{Title: "最近搜过的", Footer: "点一条再搜一次。"}
		for i, h := range d.st.History[:min(len(d.st.History), 8)] {
			histSec.Items = append(histSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("hist-%d", i),
				Icon:     "clock",
				Title:    clipRunes(h.Title, 80),
				Subtitle: fmt.Sprintf("%s · 找到 %d 段 · %s", scopeLabels[h.Scope], h.TotalMatches, durationLabel(h.TotalDurationMs)),
				Value:    d.formatTime(h.RanAt),
				Action:   &wire.Action{ID: "rerun-history", Values: map[string]string{"i": strconv.Itoa(i)}},
			})
		}
		secs = append(secs, histSec)
	}

	return secs
}

func (d draw) recipesSections(p page, recipes []*Recipe) []wire.Section {
	listSec := wire.Section{
		Title:  "常用搜索",
		Footer: "点一下就按它搜。也可以新建自己常用的搜索，或者改一改已有的。",
	}
	for _, r := range recipes {
		tag := "我建的"
		if r.Builtin {
			tag = "自带"
		}
		listSec.Items = append(listSec.Items,
			wire.Component{
				Type:     wire.ComponentRow,
				Key:      "rec-row-" + r.ID,
				Icon:     cmp.Or(r.Icon, "search"),
				Title:    r.Name,
				Subtitle: fmt.Sprintf("%s · 搜：%s", r.Description, scopeLabels[r.Scope]),
				Badge:    &wire.Badge{Text: tag, Tone: toneIf(r.Builtin, wire.TonePositive)},
				Action:   &wire.Action{ID: "run-recipe", Values: map[string]string{"id": r.ID}},
				Actions: []wire.RowAction{{
					Title: "修改",
					Icon:  "edit",
					Action: *pushPage(page{
						Tab:          "recipes",
						Scope:        p.Scope,
						EditRecipeID: r.ID,
						Mode:         modeRecipeEdit,
					}, "修改常用搜索"),
				}},
			},
		)
	}
	listSec.Items = append(listSec.Items, wire.Component{
		Type:   wire.ComponentButton,
		Key:    "btn-new-recipe",
		Title:  "＋ 新建常用搜索",
		Action: pushPage(page{Tab: "recipes", Scope: p.Scope, Mode: modeRecipeEdit}, "新建常用搜索"),
	})
	return []wire.Section{listSec}
}

func (d draw) sourcesSections(p page) []wire.Section {
	_, stats := loadSessions(d.st)

	statItems := []wire.Stat{
		{Label: "找到的聊天", Value: fmt.Sprintf("%d 段", stats.TotalSessions), Tone: toneIf(stats.TotalSessions > 0, wire.TonePositive)},
		{Label: "消息", Value: fmt.Sprintf("%d 条", stats.TotalTurns)},
		{Label: "有报错的", Value: fmt.Sprintf("%d 段", stats.ErrorSessions), Tone: toneIf(stats.ErrorSessions > 0, wire.ToneCaution)},
		{Label: "聊天记录文件夹", Value: fmt.Sprintf("%d 个", len(stats.SessionDirs))},
		{Label: "已移到云端", Value: fmt.Sprintf("%d 段", stats.ArchivedSessions)},
	}
	if stats.UnconfirmedSessions > 0 {
		statItems = append(statItems, wire.Stat{Label: "可能没传完", Value: fmt.Sprintf("%d 段", stats.UnconfirmedSessions), Tone: wire.ToneCaution})
	}
	if stats.ArchivedOmitted > 0 {
		statItems = append(statItems, wire.Stat{Label: "太多没搜", Value: fmt.Sprintf("%d 段", stats.ArchivedOmitted), Tone: wire.ToneCaution})
	}

	overview := wire.Section{
		Title:  "这台电脑上的聊天记录",
		Footer: "每次搜索都会重新读这些文件夹，不用手动刷新。",
		Items: []wire.Component{
			{Type: wire.ComponentStatGrid, Key: "src-stats", Stats: statItems},
		},
	}
	if stats.TotalSessions == 0 {
		overview.Items = append(overview.Items, wire.Component{
			Type:   wire.ComponentEmpty,
			Key:    "src-empty",
			Icon:   "folder",
			Title:  "这台电脑上还没找到聊天记录",
			Detail: noSessionsDetail,
		})
	}
	for i, dir := range stats.SessionDirs {
		overview.Items = append(overview.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("sdir-%d", i),
			Icon:     "folder",
			Title:    shortenHome(dir),
			Subtitle: "Claude Code / Codex 的聊天记录文件夹",
			Badge:    &wire.Badge{Text: "聊天记录"},
		})
	}
	for i, dir := range stats.ColdTextDirs {
		overview.Items = append(overview.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("cdir-%d", i),
			Icon:     "cloud",
			Title:    shortenHome(dir),
			Subtitle: "聊天移到云端后，disklessly 在本机留下的文字索引",
			Badge:    &wire.Badge{Text: "云端聊天"},
		})
	}
	for i, dir := range stats.RepoDirs {
		overview.Items = append(overview.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("rdir-%d", i),
			Icon:     "code",
			Title:    shortenHome(dir),
			Subtitle: "代码文件夹：搜索时在词后面加 file:* 就会一起搜",
			Badge:    &wire.Badge{Text: "代码"},
		})
	}

	// How the chats spread over tools, models and projects.
	var distRows [][]string
	for h, cnt := range stats.ByHarness {
		distRows = append(distRows, []string{"工具", harnessLabel(h), strconv.Itoa(cnt)})
	}
	for m, cnt := range stats.ByModel {
		distRows = append(distRows, []string{"模型", m, strconv.Itoa(cnt)})
	}
	for proj, cnt := range stats.ByProject {
		distRows = append(distRows, []string{"项目", proj, strconv.Itoa(cnt)})
	}
	slices.SortFunc(distRows, func(a, b []string) int {
		if c := cmp.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return cmp.Compare(a[1], b[1])
	})

	secs := []wire.Section{overview}
	if d.fleet != nil {
		secs = append(secs, d.machinesSection())
	}
	if len(distRows) > 0 {
		secs = append(secs, wire.Section{
			Title: "按工具、模型和项目分",
			Items: []wire.Component{{
				Type:    wire.ComponentTable,
				Key:     "src-dist-table",
				Columns: []wire.TableColumn{{Title: "按什么分"}, {Title: "名称"}, {Title: "聊天", Align: "end"}},
				Rows:    distRows[:min(len(distRows), 20)],
			}},
		})
	}

	cfgSec := wire.Section{
		Title:  "再加几个文件夹（一般不用填）",
		Footer: "默认已经会搜 ~/.claude/projects、~/.codex/sessions，以及 disklessly 留在本机的云端聊天索引。只有聊天记录放在别处，或者想连代码一起搜时才需要填。多个文件夹用逗号隔开。",
		Items: []wire.Component{
			{
				Type:        wire.ComponentField,
				Name:        "session_dirs",
				Label:       "其他聊天记录文件夹",
				Kind:        wire.FieldKindText,
				Value:       shortenHomeList(d.st.SessionDirs),
				Placeholder: "例如 ~/backup/claude-projects",
			},
			{
				Type:        wire.ComponentField,
				Name:        "repo_dirs",
				Label:       "代码文件夹（搜索时加 file:* 一起搜）",
				Kind:        wire.FieldKindText,
				Value:       shortenHomeList(d.st.RepoDirs),
				Placeholder: "例如 ~/code/my-app",
			},
			{
				Type:        wire.ComponentField,
				Name:        "cold_text_dirs",
				Label:       "云端聊天的索引文件夹（disklessly 的 sessions/text）",
				Kind:        wire.FieldKindText,
				Value:       shortenHomeList(d.st.ColdTextDirs),
				Placeholder: "~/.local/share/disklessly/sessions/text",
			},
			{
				Type:   wire.ComponentButton,
				Title:  "保存文件夹",
				Action: &wire.Action{ID: "save-sources", Submit: true},
			},
		},
	}
	secs = append(secs, cfgSec)
	return secs
}

func (d draw) hitDetailView(p page) *wire.View {
	res := d.activeExecution(p)
	var hit *SearchHit
	for _, h := range res.Hits {
		if h.ID == p.HitID {
			hit = h
			break
		}
	}
	var sess *SessionRecord
	if machine, localID := splitHitID(p.HitID); machine != "" {
		// Another computer's session: its synced index has the turns (none until the first sync).
		if d.fleet != nil {
			sess = d.fleet.cachedSession(machine, strings.TrimPrefix(localID, "session:"))
		}
	} else {
		sessions, _ := loadSessions(d.st)
		for _, s := range sessions {
			if "session:"+s.SessionID == p.HitID || s.SessionID == strings.TrimPrefix(p.HitID, "session:") {
				sess = s
				break
			}
		}
	}
	if hit == nil && sess == nil {
		return &wire.View{
			Title: "聊天详情",
			State: ext.EncodeState(p),
			Sections: []wire.Section{{Items: []wire.Component{{
				Type:   wire.ComponentEmpty,
				Icon:   "search",
				Title:  "找不到这段聊天了",
				Detail: "它可能已经被移走、删除，或者那台电脑还没同步过来。返回上一页重新搜一次。",
			}}}},
		}
	}

	title := ""
	project := ""
	harness := ""
	model := ""
	branch := ""
	cwd := ""
	machineID, machineName := splitHitID(p.HitID)
	machineName = ""
	updatedAt := d.now
	var files []string
	var correlated []CorrelatedRef
	var snippets []EvidenceSnippet

	if hit != nil {
		title = hit.Title
		project = hit.Project
		harness = hit.Harness
		model = hit.Model
		branch = hit.Branch
		cwd = hit.CWD
		updatedAt = hit.UpdatedAt
		files = hit.Files
		correlated = hit.Correlated
		snippets = hit.Snippets
		machineID, machineName = hit.Machine, hit.MachineName
	}
	if sess != nil {
		if title == "" {
			title = sess.Title
		}
		project = cmp.Or(project, sess.Project)
		harness = cmp.Or(harness, sess.Harness)
		model = cmp.Or(model, sess.Model)
		branch = cmp.Or(branch, sess.Branch)
		cwd = cmp.Or(cwd, sess.CWD)
		updatedAt = sess.UpdatedAt
		if len(files) == 0 {
			files = sess.Files
		}
	}

	head := wire.Component{
		Type:     wire.ComponentHeader,
		Title:    clipRunes(title, 160),
		Subtitle: fmt.Sprintf("%s · %s · 更新于 %s", cmp.Or(project, "未归入项目"), harnessLabel(harness), d.formatTime(updatedAt)),
		Facts: []wire.Fact{
			{Label: "项目", Value: cmp.Or(project, "未归入项目")},
			{Label: "工具", Value: harnessLabel(cmp.Or(harness, "claude-code"))},
			{Label: "模型", Value: cmp.Or(model, "未知")},
		},
	}
	if branch != "" {
		head.Facts = append(head.Facts, wire.Fact{Label: "分支", Value: branch})
	}
	if machineID != "" || d.fleet != nil {
		head.Facts = append(head.Facts, wire.Fact{Label: "电脑", Value: machineTitle(machineLabel(machineID, cmp.Or(machineName, sessMachineName(sess))))})
	}
	if hit != nil && hit.Cached {
		head.Badges = append(head.Badges, wire.Badge{Text: "离线副本", Tone: wire.ToneCaution})
	}
	if sess != nil {
		head.Facts = append(head.Facts,
			wire.Fact{Label: "消息", Value: fmt.Sprintf("%d 条", sess.TurnCount)},
			wire.Fact{Label: "运行命令和工具", Value: fmt.Sprintf("%d 次", sess.ToolCalls)},
		)
		if sess.ErrorCount > 0 {
			head.Badges = []wire.Badge{{Text: fmt.Sprintf("%d 处报错", sess.ErrorCount), Tone: wire.ToneDanger}}
		}
		if sess.Archived {
			head.Badges = append(head.Badges, wire.Badge{Text: locationLabel(sessionLocation(sess)), Tone: wire.ToneCaution})
			head.Facts = append(head.Facts, wire.Fact{Label: "原来的位置", Value: cmp.Or(sess.FilePath, "未知")})
			if !sess.ArchivedAt.IsZero() {
				head.Facts = append(head.Facts, wire.Fact{Label: "移到云端的时间", Value: d.formatTime(sess.ArchivedAt)})
			}
		}
	}

	secs := []wire.Section{{Items: []wire.Component{head}}}
	switch {
	case sess != nil && sess.Archived && sess.Unconfirmed:
		secs = append(secs, wire.Section{
			Title: "这段聊天可能没传完",
			Items: []wire.Component{{
				Type: wire.ComponentText,
				Key:  "unconfirmed",
				Text: "本机有 disklessly 留下的文字索引，但上传没有完成，云端可能没有这段聊天的副本，所以这里不提供恢复。想确认的话，在终端运行 disklessly sessions status。",
			}},
		})
	case sess != nil && sess.Archived:
		sec := wire.Section{
			Title:  "这段聊天已移到云端",
			Footer: "原文件已被 disklessly 移到云端，这里搜的是那台电脑留下的文字（只有你和助手说的话）。点「恢复」会在它所在的电脑上把原文件放回原来的位置，之后可以接着聊。",
		}
		if d.st.restoreRunning(sess.EntryID) {
			sec.Items = []wire.Component{{Type: wire.ComponentText, Key: "restoring", Text: "正在恢复，好了以后在首页显示结果。"}}
		} else {
			sec.Items = []wire.Component{{
				Type:   wire.ComponentButton,
				Key:    "restore-session",
				Title:  "恢复",
				Action: restoreAction(machineID, sess.EntryID, sess.FilePath, sess.Harness, sess.SessionID),
			}}
		}
		secs = append(secs, sec)
	}

	if len(snippets) > 0 {
		snipSec := wire.Section{Title: "和搜索词有关的片段"}
		for i, sn := range snippets {
			snipSec.Items = append(snipSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("det-snip-%d", i),
				Icon:     roleIcon(sn.Role),
				Title:    fmt.Sprintf("%s · 第 %d 行", roleLabel(sn.Role), max(sn.Line, 1)),
				Subtitle: sn.Text,
			})
		}
		secs = append(secs, snipSec)
	}

	if sess != nil && len(sess.Turns) > 0 {
		turnSec := wire.Section{
			Title:  "聊天经过",
			Footer: "已略去中间的命令和工具输出，只留下你的提问、助手的回答和报错。",
		}
		shown := 0
		for i, tr := range sess.Turns {
			if tr.IsTool && !tr.IsError {
				continue
			}
			sn := toSnippet(tr)
			turnSec.Items = append(turnSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("turn-%d", i),
				Icon:     roleIcon(sn.Role),
				Title:    fmt.Sprintf("%s（第 %d 行）", roleLabel(sn.Role), tr.Line),
				Subtitle: clipRunes(tr.Text, 300),
				Badge:    badgeIfError(tr.IsError, tr.ErrorKind),
			})
			if shown++; shown >= 20 {
				break
			}
		}
		if len(turnSec.Items) > 0 {
			secs = append(secs, turnSec)
		}
	}

	if len(files) > 0 {
		fileSec := wire.Section{
			Title:  "聊到的文件",
			Footer: "点一个文件，找出所有聊到或改过它的聊天。",
		}
		for i, fp := range files[:min(len(files), 10)] {
			fileSec.Items = append(fileSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("file-%d", i),
				Icon:     "file",
				Title:    fp,
				Subtitle: "找出所有聊到它的聊天",
				Action:   &wire.Action{ID: "pivot-query", Values: map[string]string{"q": filepath.Base(fp)}},
			})
		}
		secs = append(secs, fileSec)
	}

	if len(correlated) > 0 {
		corrSec := wire.Section{Title: "相关的其他聊天"}
		for i, cr := range correlated {
			corrPage := p
			corrPage.HitID = cr.ID
			corrSec.Items = append(corrSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("corr-%d", i),
				Icon:     "branch",
				Title:    clipRunes(cr.Title, 100),
				Subtitle: cr.Reason,
				Action:   pushPage(corrPage, cr.Title),
			})
		}
		secs = append(secs, corrSec)
	}

	return &wire.View{
		Title:    clipRunes(title, 80),
		State:    ext.EncodeState(p),
		Sections: secs,
	}
}

func (d draw) recipeFormView(p page) *wire.View {
	var existing *Recipe
	if p.EditRecipeID != "" {
		existing = d.st.recipe(p.EditRecipeID)
	}
	idVal := ""
	nameVal := ""
	descVal := ""
	scopeVal := cmp.Or(p.Scope, "conversation")
	codeVal := strings.TrimSpace(p.Query)
	if codeVal == "" || !looksLikePipelineCode(codeVal) {
		codeVal = "scan_sessions(query=\"\", scope=\"conversation\")\n|> exclude_tools()\n|> correlate(by=\"file\")\n|> group_by(field=\"project\")\n|> rank(by=\"relevance_freshness\")\n|> limit(n=25)"
	}
	title := "新建常用搜索"
	if existing != nil {
		idVal = existing.ID
		nameVal = existing.Name
		descVal = existing.Description
		scopeVal = existing.Scope
		codeVal = existing.Code
		title = "修改常用搜索 · " + existing.Name
	}

	scopeChoices := make([]wire.Choice, len(scopes))
	for i, sc := range scopes {
		scopeChoices[i] = wire.Choice{Value: sc, Title: scopeLabels[sc]}
	}

	items := []wire.Component{
		{
			Type:        wire.ComponentField,
			Name:        "id",
			Label:       "代号（用来区分，小写字母开头，可用字母、数字和 -）",
			Kind:        wire.FieldKindText,
			Value:       idVal,
			Placeholder: "例如 quota-debugging",
		},
		{
			Type:        wire.ComponentField,
			Name:        "name",
			Label:       "名称 *",
			Kind:        wire.FieldKindText,
			Value:       nameVal,
			Placeholder: "例如 网关报错排查",
		},
		{
			Type:        wire.ComponentField,
			Name:        "description",
			Label:       "说明",
			Kind:        wire.FieldKindText,
			Value:       descVal,
			Placeholder: "一句话说明用来找什么",
		},
		{
			Type:    wire.ComponentField,
			Name:    "recipe_scope",
			Label:   "搜哪些内容",
			Kind:    wire.FieldKindChoice,
			Choices: scopeChoices,
			Value:   scopeVal,
		},
		{
			Type:        wire.ComponentField,
			Name:        "code",
			Label:       "搜索步骤 *（写法见「高级：怎么搜的」）",
			Kind:        wire.FieldKindText,
			Value:       codeVal,
			Placeholder: `scan_sessions(query="", scope="conversation") |> exclude_tools() |> limit(n=25)`,
		},
		{
			Type:   wire.ComponentButton,
			Title:  "保存",
			Action: &wire.Action{ID: "save-recipe", Submit: true},
		},
	}

	if existing != nil && !existing.Builtin {
		items = append(items, wire.Component{
			Type:  wire.ComponentButton,
			Title: "删除这个常用搜索",
			Role:  wire.ButtonRoleDestructive,
			Action: &wire.Action{
				ID:      "delete-recipe",
				Values:  map[string]string{"id": existing.ID},
				Confirm: "确定删除常用搜索「" + existing.Name + "」？删除后不能找回。",
			},
		})
	}

	return &wire.View{
		Title:    title,
		State:    ext.EncodeState(p),
		Sections: []wire.Section{{Title: "常用搜索", Footer: "存好以后，在「结果」和「常用搜索」里点一下就能按它搜。", Items: items}},
	}
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	var p page
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: "invalid page state"}
	}
	res := &wire.ActionResult{}

	switch r.Action {
	case "restore-session":
		return a.runRestore(ctx, r, p)
	case "sync-machines":
		a.mu.Lock()
		f := a.fleet
		a.mu.Unlock()
		if f.hasRemote() {
			f.syncAll(ctx)
			res.Toast = "已同步在线电脑的聊天副本"
		}
		res.View, _ = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p), TZ: r.TZ}})
		return res, nil
	}

	// A search runs after the state is let go (it may wait on other computers), then its result is recorded.
	var pending *SearchProgram
	var scan *State
	run := func(st *State, prog SearchProgram) {
		pending, scan = &prog, st.scanState()
	}
	err := a.db().update(func(st *State, now time.Time) error {
		switch r.Action {
		case "scope":
			sc := strings.TrimSpace(r.Values["scope"])
			if !isValidScope(sc) {
				return fmt.Errorf("没有「%s」这个范围", sc)
			}
			p.Scope = sc
			p.RecipeID = ""
			prog, err := CompileProgram(CompileInput{Query: p.Query, Scope: p.Scope})
			if err != nil {
				return err
			}
			run(st, prog)

		case "clear-restores":
			st.Restores = slices.DeleteFunc(st.Restores, func(n RestoreNotice) bool { return !n.Running })

		case "tab":
			p.Tab = strings.TrimSpace(r.Values["tab"])
			if q, ok := r.Values["q"]; ok {
				p.Query = strings.TrimSpace(q)
			}

		case "search":
			p.Query = strings.TrimSpace(r.Values["q"])
			p.RecipeID = ""
			p.Tab = "results"
			prog, err := CompileProgram(CompileInput{Query: p.Query, Scope: cmp.Or(p.Scope, st.DefaultScope)})
			if err != nil {
				return err
			}
			p.Scope = prog.Scope
			run(st, prog)

		case "reset":
			p.Query = ""
			p.RecipeID = ""
			p.Scope = cmp.Or(st.DefaultScope, "conversation")
			p.Tab = "results"
			prog, _ := CompileProgram(CompileInput{Scope: p.Scope})
			run(st, prog)

		case "run-recipe":
			id := strings.TrimSpace(r.Values["id"])
			rec := st.recipe(id)
			if rec == nil {
				return fmt.Errorf("找不到常用搜索「%s」", id)
			}
			p.RecipeID = rec.ID
			p.Scope = rec.Scope
			p.Tab = "results"
			prog, err := CompileProgram(CompileInput{Recipe: rec, Query: p.Query, Scope: rec.Scope})
			if err != nil {
				return err
			}
			run(st, prog)
			res.Toast = "已按「" + rec.Name + "」搜索"

		case "run-code":
			code := strings.TrimSpace(r.Values["custom_code"])
			prog, err := CompileProgram(CompileInput{Code: code, Scope: cmp.Or(p.Scope, st.DefaultScope)})
			if err != nil {
				return err
			}
			p.Query = prog.CodeListing
			p.Scope = prog.Scope
			p.RecipeID = ""
			p.Tab = "results"
			run(st, prog)
			res.Toast = "搜完了"

		case "rerun-history":
			idx, err := strconv.Atoi(r.Values["i"])
			if err != nil || idx < 0 || idx >= len(st.History) {
				return fmt.Errorf("这条搜索记录已经不在了")
			}
			h := st.History[idx]
			prog, err := CompileProgram(CompileInput{Code: h.Code, Scope: h.Scope})
			if err != nil {
				return err
			}
			p.Query = h.Query
			p.Scope = h.Scope
			p.RecipeID = h.RecipeID
			p.Tab = "results"
			run(st, prog)
			res.Toast = "已重新搜一次"

		case "pivot-query":
			q := strings.TrimSpace(r.Values["q"])
			p.Mode = modeHome
			p.HitID = ""
			p.Query = q
			p.RecipeID = ""
			p.Tab = "results"
			prog, err := CompileProgram(CompileInput{Query: q, Scope: cmp.Or(p.Scope, st.DefaultScope)})
			if err != nil {
				return err
			}
			run(st, prog)
			res.Pop = true
			res.Toast = "已搜索「" + q + "」"

		case "save-recipe":
			rec, err := st.upsertRecipe(RecipeInput{
				ID:          cmp.Or(strings.TrimSpace(r.Values["id"]), p.EditRecipeID),
				Name:        r.Values["name"],
				Description: r.Values["description"],
				Scope:       r.Values["recipe_scope"],
				Code:        r.Values["code"],
			}, now)
			if err != nil {
				return err
			}
			p.Mode = modeHome
			p.EditRecipeID = ""
			p.Tab = "recipes"
			res.Pop = true
			res.Toast = "已保存常用搜索「" + rec.Name + "」"

		case "delete-recipe":
			id := cmp.Or(strings.TrimSpace(r.Values["id"]), p.EditRecipeID)
			if err := st.deleteRecipe(id); err != nil {
				return err
			}
			p.Mode = modeHome
			p.EditRecipeID = ""
			p.Tab = "recipes"
			res.Pop = true
			res.Toast = "已删除常用搜索"

		case "save-sources":
			st.SessionDirs = splitDirs(r.Values["session_dirs"])
			st.RepoDirs = splitDirs(r.Values["repo_dirs"])
			st.ColdTextDirs = splitDirs(r.Values["cold_text_dirs"])
			res.Toast = "已保存文件夹，下次搜索就会用上"

		case "open":
			// Handled by Push on client

		default:
			return fmt.Errorf("unknown action %q", r.Action)
		}
		return nil
	})
	if err != nil {
		return nil, &ext.Error{Code: "invalid", Message: actionFailure(r.Action, err)}
	}
	if pending != nil {
		execRes := a.execute(ctx, scan, *pending, a.now())
		_ = a.db().update(func(st *State, now time.Time) error {
			st.recordRun(execRes, now)
			return nil
		})
		if r.Action == "run-code" {
			res.Toast = fmt.Sprintf("搜完了，找到 %d 段", execRes.TotalMatches)
		}
	}
	if res.Pop {
		return res, nil
	}
	res.View, err = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p), TZ: r.TZ}})
	return res, err
}

// card is the Overview card: what this is for, the first saved searches and one line about the last run that was
// stored. Drawing it never runs a pipeline: the hub draws the card every time Overview opens, and a scan of every
// session there (what activeExecution does when the last run had a query) took seconds and held the store's read
// lock meanwhile.
func (a *app) card(_ context.Context, _ *ext.UIRequest) (*wire.View, error) {
	var items []wire.Component
	a.mu.Lock()
	f := a.fleet
	a.mu.Unlock()
	err := a.db().read(func(st *State) {
		res := st.LastRun
		// Nothing to search yet, or the last search read nothing: say why instead of the quick searches.
		if line, intro := cardSetupText(res, f.hasRemote(), chatFoldersEmpty(st), f.selectedCount()); line != "" {
			if intro {
				items = append(items, cardIntro())
			}
			items = append(items, wire.Component{Type: wire.ComponentText, Key: "card-setup", Text: line})
			return
		}
		if res == nil || res.ScannedSessions == 0 {
			items = append(items, cardIntro())
		}
		for i, rec := range st.allRecipes()[:min(len(st.allRecipes()), 3)] {
			items = append(items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("card-rec-%d", i),
				Icon:     cmp.Or(rec.Icon, "search"),
				Title:    rec.Name,
				Subtitle: clipRunes(rec.Description, 44),
			})
		}
		if res != nil && res.ScannedSessions > 0 {
			what := "上次搜索"
			if q := strings.TrimSpace(res.Program.Query); q != "" && !looksLikePipelineCode(q) {
				what = "上次搜「" + clipRunes(q, 20) + "」"
			}
			items = append(items, wire.Component{
				Type:  wire.ComponentText,
				Key:   "card-summary",
				Style: wire.TextStyleCaption,
				Text:  fmt.Sprintf("%s：搜了%s %d 段聊天%s，找到 %d 段", what, machinesNote(res.Machines), res.ScannedSessions, archivedNote(res.ScannedArchived), res.TotalMatches),
			})
		}
	})
	return &wire.View{Sections: []wire.Section{{Items: items}}}, err
}

// cardIntro is the card's line about what this is for.
func cardIntro() wire.Component {
	return wire.Component{
		Type: wire.ComponentText,
		Key:  "card-intro",
		Text: "在以前和 Claude Code、Codex 的聊天记录里找东西。点开输入几个词就能搜，也可以直接问 Persona 助手。",
	}
}

// restoreAction restores an archived session on the computer that holds it (machine: "" for the hub's).
func restoreAction(machine, entryID, path, harness, sessionID string) *wire.Action {
	values := map[string]string{"entry": entryID}
	confirm := restoreConfirm(path, harness, sessionID)
	if machine != "" {
		values["machine"] = machine
		confirm = "在会话所在的电脑上" + confirm
	}
	return &wire.Action{ID: "restore-session", Values: values, Confirm: confirm}
}

// runRestore starts restoring an archived session through the disklessly CLI in the background and returns at
// once: a restore can take minutes, longer than the hub waits for a page action. The outcome is stored in the
// state (which refreshes the page), and a successful restore re-runs the last search so the hit shows as local.
func (a *app) runRestore(ctx context.Context, r *ext.UIRequest, p page) (*wire.ActionResult, error) {
	entryID := strings.TrimSpace(r.Values["entry"])
	machine := strings.TrimSpace(r.Values["machine"])
	if machine != "" {
		return a.runRemoteRestore(ctx, r, p, machine, entryID)
	}
	res := &wire.ActionResult{}
	var started bool
	var dirs []string
	err := a.db().update(func(st *State, now time.Time) error {
		if st.restoreRunning(entryID) {
			res.Toast = errRestoreBusy.Error()
			return nil
		}
		rec := findArchivedSession(st, entryID)
		if rec == nil {
			res.Toast = "这台电脑上找不到这段云端聊天：可能已经恢复过，或者它在别的电脑上。重新搜一次再看。"
			return nil
		}
		st.setRestore(RestoreNotice{EntryID: entryID, Title: rec.Title, Path: rec.FilePath, Running: true, StartedAt: now.UTC()})
		dirs, started = slices.Clone(st.ColdTextDirs), true
		res.Toast = "正在从云端恢复到 " + rec.FilePath + "，好了以后在本页显示结果"
		return nil
	})
	if err != nil {
		return nil, err
	}
	if started {
		db := a.db()
		a.restores.Add(1)
		go func() {
			defer a.restores.Done()
			out, rerr := restoreArchived(context.Background(), &State{ColdTextDirs: dirs}, entryID, db.dir)
			_ = db.update(func(st *State, now time.Time) error {
				n := RestoreNotice{EntryID: entryID, StartedAt: now.UTC(), At: now.UTC()}
				for _, o := range st.Restores {
					if o.EntryID == entryID {
						n.Title, n.Path, n.StartedAt = o.Title, o.Path, o.StartedAt
					}
				}
				if rerr != nil {
					n.Message = rerr.Error()
				} else {
					n.OK, n.Path, n.Resume = true, cmp.Or(out.Path, n.Path), out.Resume
					if st.LastRun != nil {
						st.recordRun(ExecuteProgram(st, st.LastRun.Program, now), now)
					}
				}
				st.setRestore(n)
				return nil
			})
		}()
	}
	res.View, err = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p), TZ: r.TZ}})
	return res, err
}

// runRemoteRestore restores an archived session of another computer there, in the background like runRestore.
func (a *app) runRemoteRestore(ctx context.Context, r *ext.UIRequest, p page, machine, entryID string) (*wire.ActionResult, error) {
	a.mu.Lock()
	f := a.fleet
	a.mu.Unlock()
	res := &wire.ActionResult{}
	if f == nil || !validEntryID.MatchString(entryID) {
		res.Toast = remoteRestoreRefused
		return res, nil
	}
	// The action carries the computer from when the page was drawn: it must still be a picked one.
	if _, ok := f.pickedRemote(ctx, machine); !ok {
		res.Toast = remoteRestoreRefused
		return res, nil
	}
	title, path := entryID, ""
	if c := f.loadCache(machine); c != nil {
		for _, s := range c.Sessions {
			if s.EntryID == entryID {
				title, path = s.Title, s.FilePath
			}
		}
	}
	var started bool
	err := a.db().update(func(st *State, now time.Time) error {
		if st.restoreRunning(entryID) {
			res.Toast = errRestoreBusy.Error()
			return nil
		}
		st.setRestore(RestoreNotice{EntryID: entryID, Machine: machine, Title: title, Path: path, Running: true, StartedAt: now.UTC()})
		started = true
		res.Toast = "正在那台电脑上从云端恢复，好了以后在本页显示结果"
		return nil
	})
	if err != nil {
		return nil, err
	}
	if started {
		db := a.db()
		a.restores.Add(1)
		go func() {
			defer a.restores.Done()
			out, rerr := f.restore(context.Background(), machine, entryID)
			_ = db.update(func(st *State, now time.Time) error {
				n := RestoreNotice{EntryID: entryID, Machine: machine, Title: title, Path: path, StartedAt: now.UTC(), At: now.UTC()}
				for _, o := range st.Restores {
					if o.EntryID == entryID {
						n.StartedAt = o.StartedAt
					}
				}
				if rerr != nil {
					n.Message = rerr.Error()
				} else {
					n.OK, n.Path, n.Resume = true, cmp.Or(out.Path, n.Path), out.Resume
				}
				st.setRestore(n)
				return nil
			})
		}()
	}
	res.View, err = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p), TZ: r.TZ}})
	return res, err
}

// remoteRestoreRefused: the computer of a restore is not among the picked ones (any more).
const remoteRestoreRefused = "没法在那台电脑上恢复：它不在要搜的电脑里了。到「" + settingsPlace + "」的「" + machinesSetting + "」里重新选上它，再试一次。"

// machinesSection lists the computers a search reaches, with their sync state; a row searches that computer.
func (d draw) machinesSection() wire.Section {
	sec := wire.Section{
		Title:  "要搜的电脑",
		Footer: "在「" + settingsPlace + "」的「" + machinesSetting + "」里选。在线的电脑当场在它那里搜，只把找到的片段传回来；不在线的，搜之前同步过来的精简副本（每 15 分钟同步一次，只有你和助手说的话）。点一台电脑就只搜它；右边是在搜索框里只搜它的写法。",
	}
	for i, m := range d.fleet.statuses(d.ctx) {
		row := wire.Component{
			Type:   wire.ComponentRow,
			Key:    fmt.Sprintf("machine-%d", i),
			Icon:   "link",
			Title:  machineTitle(m.Name),
			Action: &wire.Action{ID: "pivot-query", Values: map[string]string{"q": "machine:" + m.Key}},
		}
		switch {
		case m.Local:
			row.Subtitle = "运行 Persona 的这台电脑 · 直接读它的聊天记录"
			row.Badge = &wire.Badge{Text: "本机", Tone: wire.TonePositive}
		case m.Online:
			row.Subtitle = "在线 · 当场搜"
			row.Badge = &wire.Badge{Text: "在线", Tone: wire.TonePositive}
		default:
			row.Subtitle = "不在线"
			row.Badge = &wire.Badge{Text: "不在线", Tone: wire.ToneCaution}
		}
		if !m.Local {
			if m.SyncedAt.IsZero() {
				row.Subtitle += " · 还没同步过，不在线时搜不到它"
			} else {
				row.Subtitle += fmt.Sprintf(" · 已同步 %d 段聊天（%s）", m.CachedSessions, d.formatTime(m.SyncedAt))
			}
		}
		row.Value = "machine:" + m.Key
		sec.Items = append(sec.Items, row)
	}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Key: "sync-machines", Title: "现在同步在线的电脑", Action: &wire.Action{ID: "sync-machines"}})
	return sec
}

func machinesStat(runs []MachineRun) wire.Stat {
	online, cached, missing := 0, 0, 0
	for _, m := range runs {
		switch m.Status {
		case machineOnline, machineLocal:
			online++
		case machineCached:
			cached++
		default:
			missing++
		}
	}
	v := fmt.Sprintf("%d 台", len(runs))
	if cached > 0 || missing > 0 {
		v += fmt.Sprintf("（%d 台不在线）", cached+missing)
	}
	return wire.Stat{Label: "搜了几台电脑", Value: v, Tone: toneIf(cached+missing > 0, wire.ToneCaution)}
}

func machineStatusLabel(status string) string {
	switch status {
	case machineLocal:
		return "本机"
	case machineOnline:
		return "在线"
	case machineCached:
		return "不在线 · 搜的是同步过的副本"
	default:
		return "不在线 · 没有副本，没搜到"
	}
}

// machinesNote is " N 台电脑上的" for a search of several computers, else "本机".
func machinesNote(runs []MachineRun) string {
	if len(runs) == 0 {
		return "本机"
	}
	return fmt.Sprintf(" %d 台电脑上的", len(runs))
}

func sessMachineName(s *SessionRecord) string {
	if s == nil {
		return ""
	}
	return s.MachineName
}

// restoreSection shows the page's recent restores.
func (d draw) restoreSection() []wire.Section {
	if len(d.st.Restores) == 0 {
		return nil
	}
	sec := wire.Section{Title: "会话恢复", Footer: "从云端取回的聊天放回原来的位置后，就能在 Claude Code 或 Codex 里接着聊。"}
	for i, n := range d.st.Restores {
		row := wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("restore-%d", i),
			Title:    clipRunes(cmp.Or(n.Title, n.EntryID), 120),
			Subtitle: n.Path,
		}
		switch {
		case n.Running:
			row.Icon, row.Badge = "refresh", &wire.Badge{Text: "恢复中", Tone: wire.ToneCaution}
			row.Value = d.formatTime(n.StartedAt)
		case n.OK:
			row.Icon, row.Badge = "check", &wire.Badge{Text: "已恢复", Tone: wire.TonePositive}
			row.Value = d.formatTime(n.At)
			if n.Resume != "" {
				row.Subtitle = n.Path + " · 接着聊：运行 " + n.Resume
			}
		default:
			row.Icon, row.Badge = "warning", &wire.Badge{Text: "恢复失败", Tone: wire.ToneDanger}
			row.Value = d.formatTime(n.At)
			row.Subtitle = restoreFailure(n.Message)
		}
		sec.Items = append(sec.Items, row)
	}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Key: "clear-restores", Title: "清除记录", Action: &wire.Action{ID: "clear-restores"}})
	return []wire.Section{sec}
}

func pushPage(p page, title string) *wire.Action {
	return &wire.Action{
		ID:   "open",
		Push: &wire.Push{State: ext.EncodeState(p), Title: clipRunes(title, 60)},
	}
}

func archivedNote(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("（含 %d 段已移到云端的）", n)
}

func countBadge(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func toneIf(cond bool, t string) string {
	if cond {
		return t
	}
	return wire.ToneNeutral
}

func roleIcon(role string) string {
	switch role {
	case "Error":
		return "warning"
	case "User":
		return "person"
	case "File":
		return "file"
	case "Tool":
		return "code"
	default:
		return "chat"
	}
}

func badgeIfError(isErr bool, kind string) *wire.Badge {
	if !isErr {
		return nil
	}
	return &wire.Badge{Text: errorKindLabel(kind), Tone: wire.ToneDanger}
}

func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home) {
		rel := strings.TrimPrefix(p, home)
		if rel == "" {
			return "~"
		}
		if strings.HasPrefix(rel, "/") {
			return "~" + rel
		}
	}
	if idx := strings.Index(p, "/.claude/projects"); idx >= 0 {
		return "~" + p[idx:]
	}
	if idx := strings.Index(p, "/.codex/sessions"); idx >= 0 {
		return "~" + p[idx:]
	}
	if idx := strings.Index(p, "/workspace/"); idx >= 0 {
		return "~" + p[idx:]
	}
	return p
}

func shortenHomeList(dirs []string) string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = shortenHome(d)
	}
	return strings.Join(out, ", ")
}

func (d draw) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	local := t.In(d.loc)
	nowLocal := d.now.In(d.loc)
	if local.Year() == nowLocal.Year() && local.YearDay() == nowLocal.YearDay() {
		return local.Format("15:04")
	}
	if local.Year() == nowLocal.Year() {
		return local.Format("01-02 15:04")
	}
	return local.Format("2006-01-02")
}
