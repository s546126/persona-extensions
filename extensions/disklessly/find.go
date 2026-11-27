package main

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// disklessly_sessions_find finds an old AI conversation from a description: the assistant turns
// the user's sentence into hints (keywords, a time window, the AI assistant, the project, the
// computer, where it is now) and this searches the local index with them. It reads only the
// index fields (title, first message, keywords, project, working folder, dates, tool), never a
// conversation file, and restores nothing: each candidate carries the disklessly_sessions_restore
// call that would, which stays a confirmed write.

const (
	findDefaultLimit = 10
	findMaxLimit     = 50
	findOnComputer   = "on_computer"
	findOnDrive      = "on_drive"
)

// findInput is what the assistant extracted from the user's description.
type findInput struct {
	Keywords []string `json:"keywords"`
	Match    string   `json:"match"`
	After    string   `json:"after"`
	Before   string   `json:"before"`
	Tool     string   `json:"tool"`
	Project  string   `json:"project"`
	Machine  string   `json:"machine"`
	Location string   `json:"location"`
	Limit    int      `json:"limit"`
}

// findCandidate is one conversation that fits the hints.
type findCandidate struct {
	Title       string      `json:"title"`
	When        string      `json:"when,omitempty"`
	Tool        string      `json:"tool"`
	Project     string      `json:"project,omitempty"`
	Folder      string      `json:"folder,omitempty"`
	MachineID   string      `json:"machine_id"`
	MachineName string      `json:"machine_name"`
	Location    string      `json:"location"`
	Size        string      `json:"size,omitempty"`
	FirstPrompt string      `json:"first_prompt,omitempty"`
	Why         []string    `json:"why"`
	Score       int         `json:"score"`
	ID          string      `json:"id"`
	CanRestore  bool        `json:"can_restore"`
	Note        string      `json:"note,omitempty"`
	RestoreCall *adviceCall `json:"restore_call,omitempty"`
	Resume      string      `json:"resume_after_restore,omitempty"`

	t time.Time
}

// findAnswer is the whole answer.
type findAnswer struct {
	ReadOnly   bool            `json:"read_only"`
	Hints      []string        `json:"hints"`
	Count      int             `json:"count"`
	Candidates []findCandidate `json:"candidates"`
	TooNarrow  []string        `json:"too_narrow,omitempty"`
	Suggest    string          `json:"suggest,omitempty"`
	HowToAct   string          `json:"how_to_act"`
}

// findQuery is a findInput checked and parsed.
type findQuery struct {
	words           []string
	all             bool
	after, before   time.Time // zero: open
	tool            string    // "claude", "codex" or another tool word, lower case
	project         []string  // lower-case words
	machine         *Machine
	location        string
	limit           int
	afterS, beforeS string
}

// parseFindDate reads 2026-11-02 (a day in loc) or an RFC 3339 time.
func parseFindDate(s string, loc *time.Location, endOfDay bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation(time.DateOnly, s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期 %q 看不懂，请用 2026-11-02 这样的格式", s)
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	return t, nil
}

// findTool normalizes an AI assistant's name: Claude Code → claude, Codex → codex.
func findTool(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "claude"):
		return "claude"
	case strings.Contains(s, "codex"):
		return "codex"
	}
	return s
}

func parseFind(st *State, in findInput, loc *time.Location) (findQuery, error) {
	q := findQuery{limit: cmp.Or(in.Limit, findDefaultLimit), afterS: in.After, beforeS: in.Before}
	if q.limit < 1 || q.limit > findMaxLimit {
		return q, fmt.Errorf("limit 要在 1 到 %d 之间", findMaxLimit)
	}
	for _, k := range in.Keywords {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !slices.Contains(q.words, k) {
			q.words = append(q.words, k)
		}
	}
	switch in.Match {
	case "", "any":
	case "all":
		q.all = true
	default:
		return q, errors.New(`match 只能是 "any" 或 "all"`)
	}
	var err error
	if q.after, err = parseFindDate(in.After, loc, false); err != nil {
		return q, err
	}
	if q.before, err = parseFindDate(in.Before, loc, true); err != nil {
		return q, err
	}
	if !q.after.IsZero() && !q.before.IsZero() && q.after.After(q.before) {
		return q, errors.New("after 比 before 还晚，请检查日期")
	}
	q.tool = findTool(in.Tool)
	q.project = strings.Fields(strings.ToLower(in.Project))
	if m := strings.TrimSpace(in.Machine); m != "" {
		if q.machine = st.machine(m); q.machine == nil {
			return q, fmt.Errorf("找不到电脑 %q（用 disklessly_machines_status 看有哪些）", m)
		}
	}
	switch in.Location {
	case "", "any":
	case findOnComputer, findOnDrive:
		q.location = in.Location
	default:
		return q, errors.New(`location 只能是 "on_computer"、"on_drive" 或 "any"`)
	}
	return q, nil
}

// findFields are the index fields a keyword is looked for in, with how much a hit there counts.
type findField struct {
	name   string
	weight int
	text   string
}

func findFieldsOf(e ColdEntry) []findField {
	return []findField{
		{"标题", 3, strings.ToLower(e.Title)},
		{"第一句话", 2, strings.ToLower(e.FirstPrompt)},
		{"关键词", 2, strings.ToLower(strings.Join(e.Keywords, " "))},
		{"项目", 1, strings.ToLower(e.Project)},
		{"工作文件夹", 1, strings.ToLower(e.Cwd)},
	}
}

// findFilters are the hard hints, each a name and a test, so a search with no result can say
// which of them left nothing.
type findFilter struct {
	name string
	ok   func(h ColdHit, t time.Time) bool
}

func (q findQuery) filters(d draw) []findFilter {
	var out []findFilter
	if !q.after.IsZero() || !q.before.IsZero() {
		name := "时间在 " + cmp.Or(q.afterS, "很早以前") + " 到 " + cmp.Or(q.beforeS, "现在") + " 之间"
		out = append(out, findFilter{name, func(_ ColdHit, t time.Time) bool {
			return !t.IsZero() && (q.after.IsZero() || !t.Before(q.after)) && (q.before.IsZero() || !t.After(q.before))
		}})
	}
	if q.tool != "" {
		out = append(out, findFilter{"用的是 " + cmp.Or(toolLabel(q.tool), q.tool), func(h ColdHit, _ time.Time) bool {
			tool := strings.ToLower(cmp.Or(h.Tool, toolFromPath(h.Path)))
			return tool == q.tool || strings.Contains(strings.ToLower(toolLabel(tool)), q.tool)
		}})
	}
	if len(q.project) > 0 {
		out = append(out, findFilter{"项目或文件夹里有「" + strings.Join(q.project, " ") + "」", func(h ColdHit, _ time.Time) bool {
			hay := strings.ToLower(h.Project + "\n" + h.Cwd)
			return allIn(hay, q.project)
		}})
	}
	if q.location != "" {
		name := "还在电脑上"
		if q.location == findOnDrive {
			name = "已经搬到网盘"
		}
		out = append(out, findFilter{name, func(h ColdHit, _ time.Time) bool {
			return (h.Location == LocLocal) == (q.location == findOnComputer)
		}})
	}
	return out
}

// keywordScore scores e against the keywords: how many match, their weights, and why.
func (q findQuery) keywordScore(e ColdEntry) (matched, weight int, why []string) {
	fields := findFieldsOf(e)
	for _, w := range q.words {
		best := findField{}
		for _, f := range fields {
			if f.weight > best.weight && strings.Contains(f.text, w) {
				best = f
			}
		}
		if best.weight > 0 {
			matched++
			weight += best.weight
			why = append(why, best.name+"里有「"+w+"」")
		}
	}
	return matched, weight, why
}

// findSessions searches the index with q, best first: more keywords matched, then heavier
// fields, then newer; ties by computer and id so the answer is the same for the same index.
func findSessions(st *State, in findInput, now time.Time, loc *time.Location) (*findAnswer, error) {
	q, err := parseFind(st, in, loc)
	if err != nil {
		return nil, err
	}
	d := draw{st: st, now: now, loc: loc}
	machineArg := ""
	if q.machine != nil {
		machineArg = q.machine.ID
	}
	hits := SearchCold(st, "", machineArg, 0, now)
	filters := q.filters(d)

	ans := &findAnswer{ReadOnly: true, Hints: q.hintNames(filters), Candidates: []findCandidate{},
		HowToAct: "这个工具只找、不拿回来。把前几个候选的 title、when 和 location 给用户看，问是哪一个；用户选定后照那个候选的 restore_call 原样调用 disklessly_sessions_restore" +
			"（会把对话从网盘拿回原来的位置，Persona 会请用户确认），再把它回答里的继续命令（resume）告诉用户。不要猜：没找到就照 too_narrow 和 suggest 请用户补充或放宽。"}
	var found []findCandidate
	pass := func(h ColdHit, t time.Time, skip int) bool {
		for i, f := range filters {
			if i != skip && !f.ok(h, t) {
				return false
			}
		}
		return true
	}
	for _, h := range hits {
		t := entryTime(h.ColdEntry)
		if !pass(h, t, -1) {
			continue
		}
		matched, weight, why := q.keywordScore(h.ColdEntry)
		if len(q.words) > 0 && (matched == 0 || (q.all && matched < len(q.words))) {
			continue
		}
		for _, f := range filters {
			why = append(why, f.name)
		}
		found = append(found, d.findCandidateOf(h, t, matched*10+weight, why))
	}
	slices.SortStableFunc(found, func(a, b findCandidate) int {
		return cmp.Or(-cmp.Compare(a.Score, b.Score), b.t.Compare(a.t), cmp.Compare(a.MachineID, b.MachineID), cmp.Compare(a.ID, b.ID))
	})
	ans.Count = len(found)
	if len(found) > 0 {
		ans.Candidates = found[:min(len(found), q.limit)]
	}
	if len(found) > 0 {
		if len(found) > q.limit {
			ans.Suggest = fmt.Sprintf("一共 %d 个符合，只列出最像的 %d 个；可以再加一个关键词或缩小时间。", len(found), q.limit)
		}
		return ans, nil
	}

	// Nothing: say which hints left nothing, by how many each one alone would let through.
	keywordsOK := func(e ColdEntry) bool {
		matched, _, _ := q.keywordScore(e)
		return len(q.words) == 0 || (matched > 0 && (!q.all || matched == len(q.words)))
	}
	for i, f := range filters {
		n := 0
		for _, h := range hits {
			if t := entryTime(h.ColdEntry); pass(h, t, i) && keywordsOK(h.ColdEntry) {
				n++
			}
		}
		if n > 0 {
			ans.TooNarrow = append(ans.TooNarrow, fmt.Sprintf("「%s」：去掉这一条能找到 %d 个", f.name, n))
		}
	}
	if len(ans.TooNarrow) == 0 && len(filters) > 1 {
		// No single hint is to blame: say what the keywords alone would find.
		n := 0
		for _, h := range hits {
			if keywordsOK(h.ColdEntry) {
				n++
			}
		}
		if n > 0 {
			names := make([]string, len(filters))
			for i, f := range filters {
				names[i] = f.name
			}
			ans.TooNarrow = append(ans.TooNarrow, fmt.Sprintf("「%s」这几条合起来太窄：都去掉能找到 %d 个", strings.Join(names, "」「"), n))
		}
	}
	if len(q.words) > 0 {
		n, nAny := 0, 0
		for _, h := range hits {
			t := entryTime(h.ColdEntry)
			if !pass(h, t, -1) {
				continue
			}
			n++
			if m, _, _ := q.keywordScore(h.ColdEntry); m > 0 {
				nAny++
			}
		}
		switch {
		case q.all && nAny > 0:
			ans.TooNarrow = append(ans.TooNarrow, fmt.Sprintf("关键词「%s」要求全部出现：改成任意一个（match=any）能找到 %d 个", strings.Join(q.words, "」「"), nAny))
		case n > 0:
			ans.TooNarrow = append(ans.TooNarrow, fmt.Sprintf("关键词「%s」：在标题、第一句话、关键词、项目和工作文件夹里都没有；不用关键词能找到 %d 个", strings.Join(q.words, "」「"), n))
		}
	}
	switch {
	case len(hits) == 0:
		ans.Suggest = "还没有记下任何 AI 对话（或这台电脑还没检查过）：先用 disklessly_sessions_status refresh=true 读一次，再找。"
	case len(ans.TooNarrow) > 0:
		ans.Suggest = "没找到。上面这些条件里有的太窄了：请用户确认或放宽（换个说法的关键词、把时间放宽一些、不限 AI 助手或项目），不要自己猜。记下的只有标题、第一句话和几个关键词，对话内容里的词可以用 disklessly_sessions_search deep=true 在电脑上搜全文。"
	default:
		ans.Suggest = "没找到，每个条件单独去掉也找不到：请用户换个说法描述，或用 disklessly_sessions_search deep=true 在电脑上搜全文（较慢）。"
	}
	return ans, nil
}

// hintNames words the hints, for the answer.
func (q findQuery) hintNames(filters []findFilter) []string {
	var out []string
	if len(q.words) > 0 {
		mode := "任意一个"
		if q.all {
			mode = "全部"
		}
		out = append(out, "关键词「"+strings.Join(q.words, "」「")+"」（"+mode+"）")
	}
	if q.machine != nil {
		out = append(out, "电脑「"+machineLabel(q.machine)+"」")
	}
	for _, f := range filters {
		out = append(out, f.name)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// findCandidateOf is hit h as a candidate: the page's title (never an id), and the restore call.
func (d draw) findCandidateOf(h ColdHit, t time.Time, score int, why []string) findCandidate {
	title, fromPrompt := d.coldTitle(h.ColdEntry)
	c := findCandidate{
		Title:       title,
		Tool:        cmp.Or(coldTool(h.ColdEntry), "AI 助手"),
		Project:     cmp.Or(coldProject(h.ColdEntry), noProjectText),
		MachineID:   h.MachineID,
		MachineName: h.MachineName,
		Location:    h.Location,
		Why:         why,
		Score:       score,
		ID:          h.ID,
		t:           t,
	}
	if why == nil {
		c.Why = []string{"最近的对话"}
	}
	if !t.IsZero() {
		c.When = t.In(d.loc).Format("2006-01-02 15:04")
	}
	if dir := strings.TrimSpace(lastPathElem(h.Cwd)); dir != "" && dir != coldProject(h.ColdEntry) {
		c.Folder = dir
	}
	if h.Bytes > 0 {
		c.Size = FormatBytes(h.Bytes)
	}
	if p := oneLine(h.FirstPrompt); p != "" && (!fromPrompt || p != title) {
		c.FirstPrompt = clipRunes(p, 120)
		if utf8.RuneCountInString(p) > 120 {
			c.FirstPrompt += "…"
		}
	}
	switch {
	case h.Location == LocLocal:
		c.Note = "它还在电脑上，不用拿回来。"
	case !h.Local:
		c.Note = "它是另一台设备上的对话，只能在原来那台设备上拿回来。"
	default:
		c.CanRestore = true
		c.RestoreCall = adviceCallp("disklessly_sessions_restore", map[string]any{"machine": h.MachineID, "id": h.ID})
		switch h.Location {
		case LocMissing:
			c.Note = "网盘里找不到它的存档，拿回来多半会失败。"
		case LocUnknown:
			c.Note = "位置「" + h.Location + "」只表示最近没能核对网盘，不代表丢了；拿回来时会再确认。"
		}
	}
	c.Resume = resumeHint(h.ColdEntry)
	return c
}
