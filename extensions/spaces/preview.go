package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The browser preview (`spaces preview`): the same store and ontology as the extension, drawn as Muse (Glint)
// draws a space: the space list on the left, the tabs 主要聊天 / 动态 / 点子 / 目标 / 资源库 on top, KPI cards, schema
// badges and relation edges, and forms that change the data.

func (a *app) previewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.previewPage)
	mux.HandleFunc("POST /action", a.previewAction)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		var out any
		a.db().read(func(st *State) { out = listSpaces(st, true) })
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/schema", func(w http.ResponseWriter, r *http.Request) {
		var out any
		var err error
		a.db().read(func(st *State) {
			var sp *Space
			if sp, err = st.space(r.URL.Query().Get("space")); err == nil {
				out = schemaOf(sp)
			}
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, out)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// The page's model.

type pvSpace struct {
	ID, Name, Initial, Template string
	Active, Archived            bool
	Count                       int
}

type pvStat struct{ Label, Value, Tone string }

type pvEdge struct{ Dir, Label, Type, OtherID, OtherTitle, OtherType string }

type pvProp struct{ Label, Value, Kind string }

type pvEntity struct {
	ID, Type, TypeLabel, Title, Status, StatusLabel, Tone, Ago string
	Tags                                                       []string
	Props                                                      []pvProp
	Edges                                                      []pvEdge
	Archived                                                   bool
	Ratio                                                      int
	Detail                                                     string
	Actions                                                    []pvAction
}

type pvAction struct{ Op, Title, Value string }

type pvFeed struct{ Icon, Summary, Actor, Ago, Event, TargetID string }

type pvType struct {
	Key, Label, Icon, Description string
	Builtin, Selected             bool
	Count                         int
	Fields                        []FieldDef
	Statuses                      []string
}

type pvGroup struct {
	Status, Title string
	Items         []pvEntity
	Total         int
}

type pvForm struct {
	Name, Label, Kind, Value, Placeholder string
	Options                               []pvOption
}

type pvOption struct{ Value, Title string }

type pvModel struct {
	Spaces                  []pvSpace
	Space                   pvSpace
	Description             string
	Tab, Type, Q, Flash     string
	Tabs                    []pvTab
	Stats                   []pvStat
	Feed                    []pvFeed
	Groups                  []pvGroup
	Goals                   []pvEntity
	Types                   []pvType
	TypeSel                 *pvType
	Columns                 []string
	Rows                    [][]string
	Entities                []pvEntity
	Detail                  *pvEntity
	Form                    []pvForm
	Templates               []pvOption
	RelationTypes           []pvOption
	Targets                 []pvOption
	Generation              int64
	SpaceCount, EntityCount int
}

type pvTab struct {
	ID, Title, Badge string
	Active           bool
}

func (a *app) previewPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m := pvModel{Tab: firstNonEmpty(q.Get("tab"), tabActivity), Type: canonicalType(q.Get("type")), Q: q.Get("q"), Flash: q.Get("flash")}
	if m.Tab != "chat" && tabTitles[m.Tab] == "" {
		m.Tab = tabActivity
	}
	now := time.Now()
	a.db().read(func(st *State) {
		sp := st.activeSpace()
		if id := q.Get("space"); id != "" {
			if x, err := st.space(id); err == nil {
				sp = x
			}
		}
		if sp == nil {
			return
		}
		m.Generation = st.Generation
		for _, x := range st.Spaces {
			ps := pvSpace{ID: x.ID, Name: x.Name, Initial: string([]rune(x.Name)[:1]), Template: x.Template, Active: x.ID == sp.ID,
				Archived: x.Archived, Count: len(x.Entities) - x.count(TypeFeed, false)}
			m.Spaces = append(m.Spaces, ps)
			if ps.Active {
				m.Space = ps
			}
		}
		m.SpaceCount = len(st.Spaces)
		m.EntityCount = m.Space.Count
		m.Description = sp.Description
		for _, t := range append([]string{"chat"}, tabOrder...) {
			title := tabTitles[t]
			if t == "chat" {
				title = "主要聊天"
			}
			tab := pvTab{ID: t, Title: title, Active: t == m.Tab}
			switch t {
			case tabIdeas:
				tab.Badge = countBadge(sp.countStatus(TypeIdea, IdeaInbox))
			case tabGoals:
				tab.Badge = countBadge(sp.countStatus(TypeGoal, GoalActive))
			}
			m.Tabs = append(m.Tabs, tab)
		}
		for _, t := range templates() {
			m.Templates = append(m.Templates, pvOption{t.Key, t.Name})
		}
		for _, rt := range sp.relationTypeList() {
			m.RelationTypes = append(m.RelationTypes, pvOption{rt, relationLabel(rt) + " · " + rt})
		}
		for _, c := range entityChoices(sp, "", q.Get("id")) {
			m.Targets = append(m.Targets, pvOption{c.Value, c.Title})
		}
		if id := q.Get("id"); id != "" {
			if e, ok := sp.entity(id); ok {
				d := pvEntityOf(sp, e, now)
				m.Detail = &d
			}
		}
		switch m.Tab {
		case tabIdeas:
			m.Stats = []pvStat{{"待探索", strconv.Itoa(sp.countStatus(TypeIdea, IdeaInbox)), ""},
				{"孵化中", strconv.Itoa(sp.countStatus(TypeIdea, IdeaExploring)), "caution"},
				{"已转化", strconv.Itoa(sp.countStatus(TypeIdea, IdeaIncubated)), "positive"},
				{"已归档", strconv.Itoa(archivedCount(sp, TypeIdea)), ""}}
			for _, status := range []string{IdeaInbox, IdeaExploring, IdeaIncubated} {
				list, total := sp.query(Query{Type: TypeIdea, Status: status, IncludeArchived: true, Limit: 30})
				g := pvGroup{Status: status, Title: statusLabel(status), Total: total}
				for _, e := range list {
					g.Items = append(g.Items, pvEntityOf(sp, e, now))
				}
				m.Groups = append(m.Groups, g)
			}
		case tabGoals:
			goals, _ := sp.query(Query{Type: TypeGoal})
			sort.SliceStable(goals, func(i, j int) bool { return goalOrder(goals[i]) < goalOrder(goals[j]) })
			active, achieved, sum, edges := 0, 0, 0.0, 0
			for _, g := range goals {
				if g.Status == GoalActive {
					active++
				}
				if g.Status == GoalAchieved {
					achieved++
				}
				sum += goalRatio(g)
				out, in := sp.edges(g.ID)
				edges += len(out) + len(in)
				m.Goals = append(m.Goals, pvEntityOf(sp, g, now))
			}
			avg := 0
			if len(goals) > 0 {
				avg = int(math.Round(sum / float64(len(goals)) * 100))
			}
			m.Stats = []pvStat{{"进行中目标", strconv.Itoa(active), "caution"}, {"已达成", strconv.Itoa(achieved), "positive"},
				{"平均达成率", fmt.Sprintf("%d%%", avg), ""}, {"关联关系数", strconv.Itoa(edges), ""}}
		case tabLibrary:
			for _, t := range sp.Types {
				if t.Key == TypeFeed {
					continue
				}
				pt := pvType{Key: t.Key, Label: t.Label, Icon: t.Icon, Description: t.Description, Builtin: t.Builtin,
					Selected: t.Key == m.Type, Count: sp.count(t.Key, true), Fields: t.Fields, Statuses: t.Statuses}
				m.Types = append(m.Types, pt)
				if pt.Selected {
					sel := pt
					m.TypeSel = &sel
				}
			}
			if m.TypeSel == nil {
				m.Type = ""
			}
			list, total := sp.query(Query{Type: m.Type, Keyword: m.Q})
			t, _ := sp.objectType(m.Type)
			tbl := entityTable(sp, t, list)
			for _, c := range tbl.Columns {
				m.Columns = append(m.Columns, c.Title)
			}
			m.Rows = tbl.Rows
			for i, e := range list {
				if i == 40 {
					break
				}
				m.Entities = append(m.Entities, pvEntityOf(sp, e, now))
			}
			edges := 0
			for _, rel := range sp.Relations {
				a, _ := sp.entity(rel.FromID)
				b, _ := sp.entity(rel.ToID)
				if m.Type == "" || (a != nil && a.Type == m.Type) || (b != nil && b.Type == m.Type) {
					edges++
				}
			}
			m.Stats = []pvStat{{"实体总数", strconv.Itoa(total), ""}}
			if t != nil {
				for _, s := range t.Statuses {
					if len(m.Stats) < 4 && s != StatusArchived {
						n := 0
						for _, e := range list {
							if e.Status == s {
								n++
							}
						}
						m.Stats = append(m.Stats, pvStat{statusLabel(s), strconv.Itoa(n), statusTone(s)})
					}
				}
				m.Stats = append(m.Stats, pvStat{"关系边", strconv.Itoa(edges), ""}, pvStat{"字段数", strconv.Itoa(len(t.Fields)), ""})
				for _, c := range entityFields(sp, t, nil, "entity-add", "保存") {
					if c.Name == "" {
						continue
					}
					f := pvForm{Name: c.Name, Label: c.Label, Kind: c.Kind, Value: c.Value, Placeholder: c.Placeholder}
					for _, ch := range c.Choices {
						f.Options = append(f.Options, pvOption{ch.Value, ch.Title})
					}
					m.Form = append(m.Form, f)
				}
			} else {
				m.Stats = append(m.Stats, pvStat{"对象类型", strconv.Itoa(len(sp.Types)), ""}, pvStat{"关系边", strconv.Itoa(edges), ""})
			}
		default:
			feed := feedEntries(sp)
			today := 0
			y, mo, d := now.Date()
			for i, e := range feed {
				ey, em, ed := e.CreatedAt.In(time.Local).Date()
				if ey == y && em == mo && ed == d {
					today++
				}
				if i < 60 {
					ev, _ := e.Properties["event_type"].(string)
					actor, _ := e.Properties["actor"].(string)
					summary, _ := e.Properties["summary"].(string)
					target, _ := e.Properties["target_id"].(string)
					if _, ok := sp.entity(target); !ok {
						target = ""
					}
					m.Feed = append(m.Feed, pvFeed{Icon: glyph(eventIcons[ev]), Summary: firstNonEmpty(summary, e.Title), Actor: actor,
						Ago: ago(e.CreatedAt, now, time.Local), Event: eventTitles[ev], TargetID: target})
				}
			}
			m.Stats = []pvStat{{"总动态", strconv.Itoa(len(feed)), ""}, {"今日新增", strconv.Itoa(today), "positive"},
				{"点子数", strconv.Itoa(sp.count(TypeIdea, true)), ""},
				{"目标与资源数", strconv.Itoa(sp.count(TypeGoal, true) + sp.count(TypeResource, true)), ""}}
		}
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := previewTmpl.Execute(w, m); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

func glyph(icon string) string {
	switch icon {
	case "check":
		return "＋"
	case "doc":
		return "✎"
	case "branch":
		return "⇢"
	case "merge":
		return "✦"
	case "warning":
		return "✕"
	case "chat":
		return "💬"
	case "link":
		return "⛓"
	case "settings":
		return "⚙"
	case "folder":
		return "▣"
	}
	return "•"
}

func pvEntityOf(sp *Space, e *Entity, now time.Time) pvEntity {
	t, _ := sp.objectType(e.Type)
	p := pvEntity{ID: e.ID, Type: e.Type, TypeLabel: sp.typeLabel(e.Type), Title: e.Title, Status: e.Status,
		StatusLabel: statusLabel(e.Status), Tone: statusTone(e.Status), Ago: ago(e.UpdatedAt, now, time.Local), Tags: e.Tags,
		Archived: e.Archived}
	if t != nil {
		for _, f := range t.Fields {
			v, ok := e.Properties[f.Key]
			if !ok {
				continue
			}
			s := formatValue(f, v)
			if f.Kind == KindRelation {
				s = sp.title(s)
			}
			p.Props = append(p.Props, pvProp{Label: f.Label, Value: s, Kind: f.Kind})
		}
	}
	out, in := sp.edges(e.ID)
	for _, r := range out {
		o, _ := sp.entity(r.ToID)
		ed := pvEdge{Dir: "→", Label: relationLabel(r.RelationType), Type: r.RelationType, OtherID: r.ToID, OtherTitle: sp.title(r.ToID)}
		if o != nil {
			ed.OtherType = sp.typeLabel(o.Type)
		}
		p.Edges = append(p.Edges, ed)
	}
	for _, r := range in {
		o, _ := sp.entity(r.FromID)
		ed := pvEdge{Dir: "←", Label: relationLabel(r.RelationType), Type: r.RelationType, OtherID: r.FromID, OtherTitle: sp.title(r.FromID)}
		if o != nil {
			ed.OtherType = sp.typeLabel(o.Type)
		}
		p.Edges = append(p.Edges, ed)
	}
	if e.Type == TypeGoal {
		p.Ratio = int(math.Round(goalRatio(e) * 100))
		p.Detail = goalDetail(e)
		if e.Status != GoalAchieved {
			p.Actions = append(p.Actions, pvAction{"goal-step", "推进进度 +10%", e.ID}, pvAction{"goal-done", "标记达成", e.ID})
		}
	}
	if e.Type == TypeIdea {
		switch e.Status {
		case IdeaInbox:
			p.Actions = append(p.Actions, pvAction{"idea-explore", "开始探索", e.ID})
			fallthrough
		case IdeaExploring:
			p.Actions = append(p.Actions, pvAction{"idea-goal", "孵化为目标", e.ID}, pvAction{"idea-resource", "转化为资源", e.ID},
				pvAction{"archive", "归档", e.ID})
		}
	}
	return p
}

// previewAction runs a form's op and goes back to the page.
func (a *app) previewAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	op := f("op")
	back := url.Values{}
	for _, k := range []string{"tab", "type", "id"} {
		if v := f("back_" + k); v != "" {
			back.Set(k, v)
		}
	}
	flash := ""
	err := a.db().update(func(st *State, now time.Time) error {
		var err error
		switch op {
		case "switch":
			_, err = st.switchSpace(f("space"))
			back.Del("type")
			back.Del("id")
		case "create-space":
			_, err = st.createSpace(SpaceInput{Template: f("template"), Name: f("name"), Description: f("description")}, uiActor, now)
			flash = "创建了空间"
			back = url.Values{}
		case "archive-space":
			_, err = st.archiveSpace(f("space"), true, uiActor, now)
			back = url.Values{}
		case "post":
			if f("note") == "" {
				return invalidf("先写点什么")
			}
			_, _, err = st.upsertEntity(EntityInput{Type: TypeFeed, Title: truncate(f("note"), 80),
				Properties: map[string]any{"event_type": "note", "summary": f("note")}}, uiActor, now)
		case "idea-add":
			props := map[string]any{"summary": f("summary")}
			if p := f("priority"); p != "" && p != noneValue {
				props["priority"] = p
			}
			_, _, err = st.upsertEntity(EntityInput{Type: TypeIdea, Title: f("title"), Properties: props, Tags: []string{f("tags")}}, uiActor, now)
			flash = "记下了"
		case "idea-explore":
			_, err = st.setStatus(f("value"), IdeaExploring, uiActor, now)
		case "idea-goal":
			_, err = st.incubate(f("value"), TypeGoal, uiActor, now)
			flash = "孵化为目标"
		case "idea-resource":
			_, err = st.incubate(f("value"), TypeResource, uiActor, now)
			flash = "转化为资源"
		case "archive":
			_, err = st.deleteEntity(f("value"), true, uiActor, now)
		case "delete":
			_, err = st.deleteEntity(f("value"), false, uiActor, now)
			back.Del("id")
		case "goal-step":
			_, err = st.advanceGoal(f("value"), 0.1, nil, uiActor, now)
		case "goal-done":
			var e *Entity
			if e, err = st.setStatus(f("value"), GoalAchieved, uiActor, now); e != nil {
				if t, ok := number(e.Properties["target_value"]); ok {
					e.Properties["current_value"] = t
				}
			}
		case "goal-add":
			_, _, err = st.upsertEntity(EntityInput{Type: TypeGoal, Title: f("title"), Status: GoalActive, Properties: map[string]any{
				"target_value": f("target_value"), "current_value": f("current_value"), "unit": f("unit"), "deadline": f("deadline")}}, uiActor, now)
		case "relate":
			_, err = st.addRelation("", f("from"), f("rel_type"), f("rel_target"), uiActor, now)
		case "entity-add":
			sp := st.activeSpace()
			t, ok := sp.objectType(f("type"))
			if !ok {
				return invalidf("choose an object type first")
			}
			props := map[string]any{}
			for _, fd := range t.Fields {
				if v := f("f_" + fd.Key); v != "" && v != noneValue {
					props[fd.Key] = v
				}
			}
			_, _, err = st.upsertEntity(EntityInput{Type: t.Key, Title: f("title"), Status: f("status"), Tags: []string{f("tags")},
				Properties: props}, uiActor, now)
			flash = "新增了" + t.Label
		case "type-upsert":
			fields, perr := parseFieldSpec(f("fields"))
			if perr != nil {
				return perr
			}
			var statuses []string
			for _, s := range strings.Split(f("statuses"), ",") {
				statuses = append(statuses, strings.TrimSpace(s))
			}
			var t *ObjectType
			t, _, err = st.upsertType(st.activeSpace(), ObjectType{Key: f("key"), Label: f("label"), Icon: f("icon"),
				Description: f("description"), Statuses: statuses, Fields: fields}, uiActor, now)
			if t != nil {
				back.Set("tab", tabLibrary)
				back.Set("type", t.Key)
				flash = "保存了对象类型「" + t.Label + "」"
			}
		default:
			err = invalidf("unknown op %q", op)
		}
		return err
	})
	if err != nil {
		flash = userError(err)
	}
	if flash != "" {
		back.Set("flash", flash)
	}
	http.Redirect(w, r, "/?"+back.Encode(), http.StatusSeeOther)
}

var previewTmpl = template.Must(template.New("preview").Funcs(template.FuncMap{
	"pct": func(n int) string { return strconv.Itoa(n) + "%" },
}).Parse(previewHTML))

const previewHTML = `<!doctype html>
<html lang="zh-Hans"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Space.Name}} · 空间</title>
<style>
:root{--bg:#f6f6f4;--panel:#fff;--side:#efeeea;--ink:#1d1d1f;--mute:#6e6e73;--line:#e3e2de;--acc:#5b5bd6;--acc2:#ecebff;
--pos:#1f8a4c;--pos2:#e3f5ea;--cau:#a8650b;--cau2:#fdf1dc;--dan:#c4302b;--dan2:#fde7e6;--r:14px}
@media (prefers-color-scheme:dark){:root{--bg:#161618;--panel:#1f1f22;--side:#1a1a1d;--ink:#ececf0;--mute:#9a9aa3;--line:#2d2d32;
--acc:#8b8bff;--acc2:#2a2a46;--pos:#5fd38d;--pos2:#17301f;--cau:#f0b45a;--cau2:#33270f;--dan:#ff7b72;--dan2:#3a1a19}}
*{box-sizing:border-box}body{margin:0;font:14px/1.5 -apple-system,BlinkMacSystemFont,"PingFang SC","Noto Sans SC",sans-serif;background:var(--bg);color:var(--ink)}
a{color:inherit;text-decoration:none}.app{display:grid;grid-template-columns:248px 1fr;min-height:100vh}
aside{background:var(--side);border-right:1px solid var(--line);padding:18px 12px;display:flex;flex-direction:column;gap:4px}
.brand{font-weight:700;font-size:15px;padding:4px 10px 14px;display:flex;gap:8px;align-items:center}.brand i{width:22px;height:22px;border-radius:7px;background:var(--acc);display:inline-block}
.sl{font-size:12px;color:var(--mute);padding:8px 10px 4px}.sp{display:flex;align-items:center;gap:10px;padding:8px 10px;border-radius:10px;cursor:pointer;border:0;background:none;width:100%;color:inherit;font:inherit;text-align:left}
.sp:hover{background:var(--panel)}.sp.on{background:var(--panel);box-shadow:0 1px 2px rgba(0,0,0,.06);font-weight:600}
.av{width:26px;height:26px;border-radius:8px;background:var(--acc2);color:var(--acc);display:grid;place-items:center;font-weight:700;font-size:13px;flex:none}
.sp small{margin-left:auto;color:var(--mute);font-weight:400}.sp.arch{opacity:.5}
details.new{margin-top:6px}details.new summary{list-style:none;padding:8px 10px;color:var(--acc);cursor:pointer;border-radius:10px}details.new summary:hover{background:var(--panel)}
main{padding:0 32px 48px;min-width:0}header{position:sticky;top:0;background:var(--bg);padding:18px 0 0;z-index:2}
h1{margin:0 0 4px;font-size:22px}.desc{color:var(--mute);margin:0 0 12px;max-width:820px}
nav.tabs{display:flex;gap:4px;border-bottom:1px solid var(--line)}nav.tabs a{padding:10px 14px;border-bottom:2px solid transparent;color:var(--mute);font-weight:500}
nav.tabs a.on{color:var(--ink);border-color:var(--acc)}nav.tabs b{font-size:11px;background:var(--acc2);color:var(--acc);border-radius:99px;padding:1px 7px;margin-left:5px}
.flash{margin:14px 0 0;padding:10px 14px;border-radius:10px;background:var(--acc2);color:var(--acc)}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin:18px 0}
.kpi{background:var(--panel);border:1px solid var(--line);border-radius:var(--r);padding:14px 16px}.kpi span{color:var(--mute);font-size:12px}.kpi div{font-size:26px;font-weight:700;margin-top:2px}
.kpi.positive div{color:var(--pos)}.kpi.caution div{color:var(--cau)}.kpi.danger div{color:var(--dan)}
.grid{display:grid;grid-template-columns:minmax(0,1fr) 320px;gap:18px;align-items:start}@media(max-width:1100px){.grid{grid-template-columns:1fr}}
.card{background:var(--panel);border:1px solid var(--line);border-radius:var(--r);padding:16px;margin-bottom:14px}.card h3{margin:0 0 10px;font-size:14px}
.feed{list-style:none;margin:0;padding:0}.feed li{display:flex;gap:12px;padding:10px 0;border-bottom:1px solid var(--line)}.feed li:last-child{border:0}
.fi{width:30px;height:30px;border-radius:50%;background:var(--acc2);color:var(--acc);display:grid;place-items:center;flex:none;font-size:13px}
.meta{color:var(--mute);font-size:12px}.badge{display:inline-block;font-size:11px;padding:1px 8px;border-radius:99px;background:var(--line);color:var(--mute);margin-left:6px;vertical-align:1px}
.badge.positive{background:var(--pos2);color:var(--pos)}.badge.caution{background:var(--cau2);color:var(--cau)}.badge.danger{background:var(--dan2);color:var(--dan)}
.cols{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:14px}@media(max-width:1000px){.cols{grid-template-columns:1fr}}
.col h3{display:flex;justify-content:space-between}.ent{border:1px solid var(--line);border-radius:12px;padding:12px;margin-bottom:10px;background:var(--panel)}
.ent .t{font-weight:600}.props{display:flex;flex-wrap:wrap;gap:6px;margin:8px 0}.props span{font-size:12px;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:2px 8px}
.props span em{font-style:normal;color:var(--mute);margin-right:4px}.edges{display:flex;flex-wrap:wrap;gap:6px;margin-top:6px}
.edge{font-size:12px;border-radius:8px;padding:2px 8px;background:var(--acc2);color:var(--acc)}.tag{font-size:12px;color:var(--acc);margin-right:6px}
.acts{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}button,.btn{font:inherit;font-size:12.5px;border:1px solid var(--line);background:var(--panel);color:var(--ink);border-radius:9px;padding:5px 11px;cursor:pointer}
button.pri{background:var(--acc);border-color:var(--acc);color:#fff}button.dan{color:var(--dan)}button:hover{filter:brightness(.97)}
.bar{height:8px;border-radius:99px;background:var(--line);overflow:hidden;margin:8px 0 4px}.bar i{display:block;height:100%;background:var(--acc);border-radius:99px}.bar.done i{background:var(--pos)}
form.f{display:grid;gap:8px}input,select,textarea{font:inherit;width:100%;padding:7px 10px;border:1px solid var(--line);border-radius:9px;background:var(--bg);color:var(--ink)}
label{font-size:12px;color:var(--mute);display:grid;gap:3px}.types{display:flex;flex-wrap:wrap;gap:8px;margin:16px 0}
.ty{border:1px solid var(--line);background:var(--panel);border-radius:12px;padding:8px 12px;min-width:120px}.ty.on{border-color:var(--acc);box-shadow:0 0 0 2px var(--acc2)}
.ty b{display:block}.ty small{color:var(--mute)}table{width:100%;border-collapse:collapse;font-size:13px}th,td{padding:7px 10px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}
th{color:var(--mute);font-weight:500}.tw{overflow-x:auto}.schema{display:flex;flex-wrap:wrap;gap:6px}.schema span{font-size:12px;border:1px dashed var(--line);border-radius:8px;padding:2px 8px}
.schema span i{font-style:normal;color:var(--acc)}.chat{display:grid;place-items:center;min-height:50vh;color:var(--mute);text-align:center}
.chat code{background:var(--panel);border:1px solid var(--line);padding:2px 6px;border-radius:6px}.empty{color:var(--mute);padding:10px 0}
</style></head><body><div class="app">
<aside>
 <div class="brand"><i></i>Muse 空间</div>
 <div class="sl">空间 · {{.SpaceCount}}</div>
 {{range .Spaces}}<form method="post" action="/action"><input type="hidden" name="op" value="switch"><input type="hidden" name="space" value="{{.ID}}">
 <button class="sp{{if .Active}} on{{end}}{{if .Archived}} arch{{end}}" {{if .Archived}}disabled{{end}}><span class="av">{{.Initial}}</span>{{.Name}}<small>{{.Count}}</small></button></form>{{end}}
 <details class="new"><summary>＋ 创建空间</summary>
 <form class="f" method="post" action="/action" style="padding:6px 10px"><input type="hidden" name="op" value="create-space">
 <label>模板<select name="template">{{range .Templates}}<option value="{{.Value}}">{{.Title}}</option>{{end}}</select></label>
 <label>名字<input name="name" placeholder="不填用模板的名字"></label><label>说明<input name="description"></label>
 <button class="pri">创建</button></form></details>
 <div style="flex:1"></div>
 <div class="meta" style="padding:10px">generation {{.Generation}} · <a href="/api/state">state</a> · <a href="/api/schema?space={{.Space.ID}}">schema</a></div>
</aside>
<main>
<header>
 <h1>{{.Space.Name}}</h1><p class="desc">{{.Description}}</p>
 <nav class="tabs">{{range .Tabs}}<a class="{{if .Active}}on{{end}}" href="/?tab={{.ID}}">{{.Title}}{{if .Badge}}<b>{{.Badge}}</b>{{end}}</a>{{end}}</nav>
</header>
{{if .Flash}}<div class="flash">{{.Flash}}</div>{{end}}
{{if eq .Tab "chat"}}
 <div class="chat"><div><h2>主要聊天</h2><p>在 Persona 助手里对话：它通过 <code>mcp__spaces__space_query</code> 等工具读写这个空间。<br>只读工具直接运行，写入工具会先问你。</p></div></div>
{{else}}
<div class="kpis">{{range .Stats}}<div class="kpi {{.Tone}}"><span>{{.Label}}</span><div>{{.Value}}</div></div>{{end}}</div>
{{with .Detail}}<div class="card"><h3>{{.Title}} <span class="badge">{{.TypeLabel}}</span>{{if .StatusLabel}}<span class="badge {{.Tone}}">{{.StatusLabel}}</span>{{end}}</h3>
 <div class="props">{{range .Props}}<span><em>{{.Label}}</em>{{.Value}}</span>{{end}}</div>
 <div class="edges">{{range .Edges}}<a class="edge" href="/?tab=library&id={{.OtherID}}">{{.Dir}} {{.Label}} {{.Dir}} {{.OtherTitle}} · {{.OtherType}}</a>{{else}}<span class="meta">还没有关系</span>{{end}}</div>
 <form class="acts" method="post" action="/action"><input type="hidden" name="op" value="relate"><input type="hidden" name="from" value="{{.ID}}">
 <input type="hidden" name="back_tab" value="library"><input type="hidden" name="back_id" value="{{.ID}}">
 <select name="rel_type" style="width:auto">{{range $.RelationTypes}}<option value="{{.Value}}">{{.Title}}</option>{{end}}</select>
 <select name="rel_target" style="width:auto">{{range $.Targets}}<option value="{{.Value}}">{{.Title}}</option>{{end}}</select><button>添加关系</button></form>
 <form class="acts" method="post" action="/action"><input type="hidden" name="op" value="delete"><input type="hidden" name="value" value="{{.ID}}"><input type="hidden" name="back_tab" value="library"><button class="dan">删除</button></form>
</div>{{end}}
{{if eq .Tab "activity"}}<div class="grid"><div class="card"><h3>动态流</h3><ul class="feed">
 {{range .Feed}}<li><span class="fi">{{.Icon}}</span><div><div>{{if .TargetID}}<a href="/?tab=library&id={{.TargetID}}">{{.Summary}}</a>{{else}}{{.Summary}}{{end}}{{if .Event}}<span class="badge">{{.Event}}</span>{{end}}</div>
 <div class="meta">{{.Actor}} · {{.Ago}}</div></div></li>{{else}}<li class="empty">还没有动态</li>{{end}}</ul></div>
 <div><div class="card"><h3>发一条动态</h3><form class="f" method="post" action="/action"><input type="hidden" name="op" value="post">
 <textarea name="note" rows="3" placeholder="进展、想法、刚做完的事…"></textarea><button class="pri">发布</button></form></div></div></div>
{{else if eq .Tab "ideas"}}
 <div class="card"><h3>记个点子</h3><form class="f" method="post" action="/action" style="grid-template-columns:2fr 3fr 1fr 1fr auto;align-items:end">
 <input type="hidden" name="op" value="idea-add"><input type="hidden" name="back_tab" value="ideas">
 <label>点子<input name="title" required></label><label>说明<input name="summary"></label>
 <label>优先级<select name="priority"><option>中</option><option>高</option><option>低</option></select></label><label>标签<input name="tags"></label><button class="pri">记下</button></form></div>
 <div class="cols">{{range .Groups}}<div class="col"><h3>{{.Title}}<span class="meta">{{.Total}}</span></h3>
 {{range .Items}}{{template "ent" .}}{{else}}<div class="empty">暂无</div>{{end}}</div>{{end}}</div>
{{else if eq .Tab "goals"}}<div class="grid"><div>
 {{range .Goals}}<div class="ent"><div class="t">{{.Title}}<span class="badge {{.Tone}}">{{.StatusLabel}}</span></div>
 <div class="bar{{if eq .Status "achieved"}} done{{end}}"><i style="width:{{pct .Ratio}}"></i></div><div class="meta">{{.Detail}}</div>
 <div class="edges">{{range .Edges}}<span class="edge">{{.Dir}} {{.Label}} · {{.OtherTitle}}</span>{{end}}</div>
 <div class="acts">{{range .Actions}}<form method="post" action="/action"><input type="hidden" name="op" value="{{.Op}}"><input type="hidden" name="value" value="{{.Value}}"><input type="hidden" name="back_tab" value="goals"><button>{{.Title}}</button></form>{{end}}</div></div>
 {{else}}<div class="empty">还没有目标</div>{{end}}</div>
 <div class="card"><h3>新建目标</h3><form class="f" method="post" action="/action"><input type="hidden" name="op" value="goal-add"><input type="hidden" name="back_tab" value="goals">
 <label>目标<input name="title" required></label><label>目标值<input name="target_value" value="100"></label><label>当前值<input name="current_value" value="0"></label>
 <label>单位<input name="unit" value="%"></label><label>截止日期<input name="deadline" type="date"></label><button class="pri">创建目标</button></form></div></div>
{{else if eq .Tab "library"}}
 <div class="types"><a class="ty{{if not .TypeSel}} on{{end}}" href="/?tab=library"><b>全部</b><small>所有对象类型</small></a>
 {{range .Types}}<a class="ty{{if .Selected}} on{{end}}" href="/?tab=library&type={{.Key}}"><b>{{.Label}}</b><small>{{.Key}} · {{.Count}} 个{{if .Builtin}} · 内置{{end}}</small></a>{{end}}</div>
 {{with .TypeSel}}<div class="card"><h3>本体 · {{.Label}}</h3><p class="meta">{{.Description}}</p><div class="schema">
 {{range .Fields}}<span>{{.Label}} <i>{{.Key}}: {{.Kind}}{{if .Unit}} ({{.Unit}}){{end}}{{if .TargetType}} → {{.TargetType}}{{end}}{{if .Required}} *{{end}}</i></span>{{end}}
 {{range .Statuses}}<span class="badge">{{.}}</span>{{end}}</div></div>{{end}}
 <div class="card"><form method="get" action="/" style="display:flex;gap:8px;margin-bottom:10px"><input type="hidden" name="tab" value="library"><input type="hidden" name="type" value="{{.Type}}">
 <input name="q" value="{{.Q}}" placeholder="搜索标题、标签、属性"><button>搜索</button></form>
 <div class="tw"><table><tr>{{range .Columns}}<th>{{.}}</th>{{end}}</tr>{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</table></div></div>
 <div class="grid"><div>{{range .Entities}}{{template "ent" .}}{{else}}<div class="empty">没有符合的实体</div>{{end}}</div><div>
 {{if .Form}}<div class="card"><h3>新增{{.TypeSel.Label}}</h3><form class="f" method="post" action="/action"><input type="hidden" name="op" value="entity-add"><input type="hidden" name="type" value="{{.Type}}">
 <input type="hidden" name="back_tab" value="library"><input type="hidden" name="back_type" value="{{.Type}}">
 {{range .Form}}<label>{{.Label}}{{if .Options}}<select name="{{.Name}}">{{$v := .Value}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value $v}} selected{{end}}>{{.Title}}</option>{{end}}</select>{{else if eq .Kind "toggle"}}<select name="{{.Name}}"><option value="false">否</option><option value="true">是</option></select>{{else}}<input name="{{.Name}}" value="{{.Value}}" placeholder="{{.Placeholder}}">{{end}}</label>{{end}}
 <button class="pri">保存</button></form></div>{{end}}
 <div class="card"><h3>注册对象类型</h3><form class="f" method="post" action="/action"><input type="hidden" name="op" value="type-upsert">
 <label>类型 key<input name="key" placeholder="book"></label><label>名称<input name="label" placeholder="书"></label>
 <label>图标<input name="icon" value="list"></label><label>说明<input name="description"></label>
 <label>字段 <small>key:标签:kind[:补充]，分号隔开</small><textarea name="fields" rows="3">author:作者:text!; pages:页数:number:页; format:形式:enum:纸书|电子书</textarea></label>
 <label>状态<input name="statuses" value="想读, 在读, 读完"></label><button class="pri">保存对象类型</button></form></div></div></div>
{{end}}{{end}}
</main></div></body></html>
{{define "ent"}}<div class="ent"><a class="t" href="/?tab=library&id={{.ID}}">{{.Title}}</a>{{if .StatusLabel}}<span class="badge {{.Tone}}">{{.StatusLabel}}</span>{{end}}<span class="badge">{{.TypeLabel}}</span>
 <div class="props">{{range .Props}}<span><em>{{.Label}}</em>{{.Value}}</span>{{end}}</div>
 {{if .Tags}}<div>{{range .Tags}}<span class="tag">#{{.}}</span>{{end}}</div>{{end}}
 {{if .Edges}}<div class="edges">{{range .Edges}}<a class="edge" href="/?tab=library&id={{.OtherID}}">{{.Dir}} {{.Label}} · {{.OtherTitle}}</a>{{end}}</div>{{end}}
 {{if .Actions}}<div class="acts">{{range .Actions}}<form method="post" action="/action"><input type="hidden" name="op" value="{{.Op}}"><input type="hidden" name="value" value="{{.Value}}"><input type="hidden" name="back_tab" value="ideas"><button>{{.Title}}</button></form>{{end}}</div>{{end}}
 <div class="meta">{{.Ago}}</div></div>{{end}}`
