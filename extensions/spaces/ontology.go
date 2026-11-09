package main

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The ontology: every space holds its own object types (the schema), the entities of those types (an EAV record:
// typed properties checked against the type's fields) and the relations between entities. Four types are built in
// and present in every space: feed (动态), idea (点子), goal (目标) and resource (资源库).
//
// Two design rules hold here: a type's fields belong to its space (so two spaces' "status" or "price" never overwrite
// each other), and a value is converted by the field's declared kind, never by sniffing the string (so a text "007"
// stays "007").

// Field kinds.
const (
	KindText     = "text"
	KindNumber   = "number"
	KindBoolean  = "boolean"
	KindDate     = "date"
	KindEnum     = "enum"
	KindURL      = "url"
	KindRelation = "relation"
)

var fieldKinds = []string{KindText, KindNumber, KindBoolean, KindDate, KindEnum, KindURL, KindRelation}

// The built-in types.
const (
	TypeFeed     = "feed"
	TypeIdea     = "idea"
	TypeGoal     = "goal"
	TypeResource = "resource"
)

// Idea and goal statuses (the workflows of the 点子 and 目标 tabs).
const (
	IdeaInbox      = "inbox"
	IdeaExploring  = "exploring"
	IdeaIncubated  = "incubated"
	StatusArchived = "archived"
	GoalPlanned    = "planned"
	GoalActive     = "active"
	GoalAchieved   = "achieved"
)

// Relation types the views know a label for; any other snake_case word is allowed too.
var relationLabels = map[string]string{
	"spawns":     "孵化出",
	"belongs_to": "属于",
	"located_in": "存放在",
	"supports":   "支撑",
	"relates_to": "关联",
	"uses":       "使用",
	"blocks":     "阻塞",
}

// relationTypes are the known relation types, in a stable order.
var relationTypes = []string{"spawns", "belongs_to", "located_in", "supports", "relates_to", "uses", "blocks"}

func (sp *Space) relationTypeList() []string {
	used := slices.Clone(relationTypes)
	for _, r := range sp.Relations {
		if !slices.Contains(used, r.RelationType) {
			used = append(used, r.RelationType)
		}
	}
	for _, t := range sp.Types {
		for _, f := range t.Fields {
			if f.Kind == KindRelation {
				rt := firstNonEmpty(f.RelationType, f.Key)
				if !slices.Contains(used, rt) {
					used = append(used, rt)
				}
			}
		}
	}
	sort.Strings(used[len(relationTypes):])
	return used
}

var statusLabels = map[string]string{
	IdeaInbox: "待探索", IdeaExploring: "孵化中", IdeaIncubated: "已转化", StatusArchived: "已归档",
	GoalPlanned: "规划中", GoalActive: "进行中", GoalAchieved: "已达成",
	"saved": "已收藏", "using": "在用",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

func relationLabel(t string) string {
	if l, ok := relationLabels[t]; ok {
		return l
	}
	return t
}

// FieldDef is one property of an object type.
type FieldDef struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Options  []string `json:"options,omitempty"`
	// TargetType: a relation field's entity type ("" any type).
	TargetType string `json:"target_type,omitempty"`
	// RelationType: the edge a relation field keeps in the graph (belongs_to, located_in …; the field's key when empty).
	RelationType string `json:"relation_type,omitempty"`
}

// ObjectType is a type of entity in one space: its fields, its primary field and its status workflow.
type ObjectType struct {
	Key          string     `json:"key"`
	Label        string     `json:"label"`
	Icon         string     `json:"icon,omitempty"`
	Description  string     `json:"description,omitempty"`
	PrimaryField string     `json:"primary_field,omitempty"`
	Statuses     []string   `json:"statuses,omitempty"`
	Fields       []FieldDef `json:"fields"`
	Builtin      bool       `json:"builtin,omitempty"`
}

func (t *ObjectType) field(key string) (FieldDef, bool) {
	for _, f := range t.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return FieldDef{}, false
}

// Entity is one record of a space.
type Entity struct {
	ID         string         `json:"id"`
	SpaceID    string         `json:"space_id"`
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     string         `json:"status,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Archived   bool           `json:"archived,omitempty"`
}

// Relation is a typed edge between two entities of the same space.
type Relation struct {
	ID           string    `json:"id"`
	SpaceID      string    `json:"space_id"`
	FromID       string    `json:"from_id"`
	RelationType string    `json:"relation_type"`
	ToID         string    `json:"to_id"`
	CreatedAt    time.Time `json:"created_at"`
	// ByField: the edge mirrors a relation field of FromID (it follows the field when the field changes).
	ByField bool `json:"by_field,omitempty"`
}

// Space is one isolated workspace: its schema, entities and relations.
type Space struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Icon        string       `json:"icon,omitempty"`
	Description string       `json:"description,omitempty"`
	Template    string       `json:"template,omitempty"`
	Archived    bool         `json:"archived,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Types       []ObjectType `json:"types"`
	Entities    []*Entity    `json:"entities"`
	Relations   []Relation   `json:"relations"`
}

// State is everything the extension keeps: spaces_state.json.
type State struct {
	Version    int      `json:"version"`
	Generation int64    `json:"generation"`
	Seq        int64    `json:"seq"`
	Active     string   `json:"active"`
	Spaces     []*Space `json:"spaces"`
}

const stateVersion = 1

// feedLimit: the activity stream keeps the newest this many entries of a space.
const feedLimit = 500

var (
	keyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// errNotFound and errInvalid tell the MCP server and the UI how to word a failure.
var (
	errNotFound = errors.New("not found")
	errInvalid  = errors.New("invalid")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalid, fmt.Sprintf(format, args...))
}

func notFoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errNotFound, fmt.Sprintf(format, args...))
}

// builtinTypes are the four first-class types of every space, fresh copies each call.
func builtinTypes() []ObjectType {
	return []ObjectType{
		{Key: TypeFeed, Label: "动态", Icon: wire.NavIconBell, Builtin: true, PrimaryField: "summary",
			Description: "空间里发生的每一件事：新建、更新、状态流转、孵化、删除，以及手动发的动态。",
			Fields: []FieldDef{
				{Key: "event_type", Label: "事件", Kind: KindText},
				{Key: "summary", Label: "摘要", Kind: KindText},
				{Key: "actor", Label: "来自", Kind: KindText},
				{Key: "target_id", Label: "对象 ID", Kind: KindText},
				{Key: "target_type", Label: "对象类型", Kind: KindText},
			}},
		{Key: TypeIdea, Label: "点子", Icon: wire.NavIconStar, Builtin: true, PrimaryField: "summary",
			Description: "灵感收件箱：待探索 → 孵化中 → 已转化为目标或资源 → 归档。",
			Statuses:    []string{IdeaInbox, IdeaExploring, IdeaIncubated, StatusArchived},
			Fields: []FieldDef{
				{Key: "category", Label: "分类", Kind: KindText},
				{Key: "priority", Label: "优先级", Kind: KindEnum, Options: []string{"高", "中", "低"}},
				{Key: "summary", Label: "说明", Kind: KindText},
				{Key: "incubated_target_id", Label: "转化为", Kind: KindRelation, RelationType: "spawns"},
			}},
		{Key: TypeGoal, Label: "目标", Icon: wire.NavIconGauge, Builtin: true, PrimaryField: "milestone_summary",
			Description: "可度量的目标：规划中 → 进行中 → 已达成 → 归档，进度 = 当前值 / 目标值。",
			Statuses:    []string{GoalPlanned, GoalActive, GoalAchieved, StatusArchived},
			Fields: []FieldDef{
				{Key: "target_value", Label: "目标值", Kind: KindNumber},
				{Key: "current_value", Label: "当前值", Kind: KindNumber},
				{Key: "unit", Label: "单位", Kind: KindText},
				{Key: "deadline", Label: "截止日期", Kind: KindDate},
				{Key: "priority", Label: "优先级", Kind: KindEnum, Options: []string{"高", "中", "低"}},
				{Key: "milestone_summary", Label: "里程碑", Kind: KindText},
			}},
		{Key: TypeResource, Label: "资源", Icon: wire.NavIconFolder, Builtin: true, PrimaryField: "summary",
			Description: "资源库的通用条目：链接、文档、工具、清单。",
			Statuses:    []string{"saved", "using", StatusArchived},
			Fields: []FieldDef{
				{Key: "category", Label: "分类", Kind: KindText},
				{Key: "url", Label: "链接", Kind: KindURL},
				{Key: "summary", Label: "说明", Kind: KindText},
				{Key: "rating", Label: "评分", Kind: KindNumber, Unit: "分"},
			}},
	}
}

func isBuiltin(key string) bool {
	switch key {
	case TypeFeed, TypeIdea, TypeGoal, TypeResource:
		return true
	}
	return false
}

// canonicalType maps the aliases a query may use to a type key ("" all types).
func canonicalType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "dynamic", "activity", "activities", "动态":
		return TypeFeed
	case "ideas", "点子":
		return TypeIdea
	case "goals", "目标":
		return TypeGoal
	case "resources", "library", "资源", "资源库":
		return TypeResource
	case "all", "*", "全部":
		return ""
	}
	return t
}

// ---- Spaces ----

func (st *State) nextID(prefix string) string {
	st.Seq++
	return fmt.Sprintf("%s%d", prefix, st.Seq)
}

func (st *State) space(id string) (*Space, error) {
	if id == "" {
		id = st.Active
	}
	for _, sp := range st.Spaces {
		if sp.ID == id {
			return sp, nil
		}
	}
	return nil, notFoundf("no space %q", id)
}

// activeSpace is the space the views show: the active one, else the first not archived, else any.
func (st *State) activeSpace() *Space {
	if sp, err := st.space(st.Active); err == nil && !sp.Archived {
		return sp
	}
	for _, sp := range st.Spaces {
		if !sp.Archived {
			return sp
		}
	}
	if len(st.Spaces) > 0 {
		return st.Spaces[0]
	}
	return nil
}

func (st *State) liveSpaces() []*Space {
	var out []*Space
	for _, sp := range st.Spaces {
		if !sp.Archived {
			out = append(out, sp)
		}
	}
	return out
}

// SpaceInput creates a space.
type SpaceInput struct {
	ID          string `json:"id,omitempty"`
	Template    string `json:"template,omitempty"`
	Name        string `json:"name,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	Seed        *bool  `json:"seed,omitempty"`
}

func validIcon(icon string) bool { return slices.Contains(wire.NavIcons, icon) }

// createSpace makes a space from a template (blank when none), switches to it and logs it.
func (st *State) createSpace(in SpaceInput, actor string, now time.Time) (*Space, error) {
	tplKey := strings.TrimSpace(in.Template)
	if tplKey == "" {
		tplKey = "blank"
	}
	tpl, ok := templateByKey(tplKey)
	if !ok {
		return nil, invalidf("unknown template %q (lifeos, deals, cloud, blank)", tplKey)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = tpl.Name
	}
	if len([]rune(name)) > 40 {
		return nil, invalidf("a space name has at most 40 characters")
	}
	icon := strings.TrimSpace(in.Icon)
	if icon == "" {
		icon = tpl.Icon
	}
	if !validIcon(icon) {
		return nil, invalidf("icon %q is not one of %s", icon, strings.Join(wire.NavIcons, ", "))
	}
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = tpl.Description
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = tplKey
		if tplKey == "blank" {
			id = "space"
		}
		base := id
		for n := 2; ; n++ {
			if _, err := st.space(id); err != nil {
				break
			}
			id = fmt.Sprintf("%s-%d", base, n)
		}
	} else {
		if !keyRe.MatchString(strings.ReplaceAll(id, "-", "_")) {
			return nil, invalidf("space id %q: lowercase letters, digits, - and _", id)
		}
		if _, err := st.space(id); err == nil {
			return nil, invalidf("space %q exists", id)
		}
	}
	sp := &Space{ID: id, Name: name, Icon: icon, Description: desc, Template: tplKey, CreatedAt: now, UpdatedAt: now,
		Types: builtinTypes(), Entities: []*Entity{}, Relations: []Relation{}}
	for _, t := range tpl.Types {
		sp.Types = append(sp.Types, t)
	}
	st.Spaces = append(st.Spaces, sp)
	st.Active = sp.ID
	if in.Seed == nil || *in.Seed {
		if err := tpl.seed(st, sp, now); err != nil {
			return nil, err
		}
	}
	st.log(sp, "space_created", fmt.Sprintf("创建了空间「%s」（%s 模板）", sp.Name, tpl.Name), actor, "", "", now)
	return sp, nil
}

func (st *State) switchSpace(id string) (*Space, error) {
	sp, err := st.space(id)
	if err != nil {
		return nil, err
	}
	if sp.Archived {
		return nil, invalidf("space %q is archived; unarchive it first", id)
	}
	st.Active = sp.ID
	return sp, nil
}

func (st *State) archiveSpace(id string, archived bool, actor string, now time.Time) (*Space, error) {
	sp, err := st.space(id)
	if err != nil {
		return nil, err
	}
	if archived && len(st.liveSpaces()) == 1 && !sp.Archived {
		return nil, invalidf("the last open space cannot be archived")
	}
	sp.Archived = archived
	sp.UpdatedAt = now
	word := "归档"
	if !archived {
		word = "恢复"
	}
	st.log(sp, "space_archived", fmt.Sprintf("%s了空间「%s」", word, sp.Name), actor, "", "", now)
	if archived && st.Active == sp.ID {
		if next := st.activeSpace(); next != nil {
			st.Active = next.ID
		}
	}
	return sp, nil
}

// ---- Schema ----

func (sp *Space) objectType(key string) (*ObjectType, bool) {
	for i := range sp.Types {
		if sp.Types[i].Key == key {
			return &sp.Types[i], true
		}
	}
	return nil, false
}

// customTypes are the space's types other than the four built in.
func (sp *Space) customTypes() []ObjectType {
	var out []ObjectType
	for _, t := range sp.Types {
		if !t.Builtin {
			out = append(out, t)
		}
	}
	return out
}

// normalizeType checks a type definition and fills its defaults.
func normalizeType(t ObjectType) (ObjectType, error) {
	t.Key = strings.TrimSpace(t.Key)
	if !keyRe.MatchString(t.Key) {
		return t, invalidf("type key %q: a lowercase letter, then letters, digits or _ (32 at most)", t.Key)
	}
	switch t.Key {
	case "all", "dynamic", "activity", "library", "space", "entity", "relation":
		return t, invalidf("type key %q is reserved", t.Key)
	}
	t.Label = strings.TrimSpace(t.Label)
	if t.Label == "" {
		t.Label = t.Key
	}
	if len([]rune(t.Label)) > 40 {
		return t, invalidf("type %s: a label has at most 40 characters", t.Key)
	}
	if t.Icon == "" {
		t.Icon = wire.NavIconList
	}
	if !validIcon(t.Icon) {
		return t, invalidf("type %s: icon %q is not one of %s", t.Key, t.Icon, strings.Join(wire.NavIcons, ", "))
	}
	if len(t.Fields) > 40 {
		return t, invalidf("type %s: at most 40 fields", t.Key)
	}
	seen := map[string]bool{}
	for i := range t.Fields {
		f := &t.Fields[i]
		f.Key = strings.TrimSpace(f.Key)
		if !keyRe.MatchString(f.Key) {
			return t, invalidf("type %s: field key %q: a lowercase letter, then letters, digits or _", t.Key, f.Key)
		}
		switch f.Key {
		case "title", "status", "tags", "id", "type":
			return t, invalidf("type %s: field key %q is an entity column, not a property", t.Key, f.Key)
		}
		if seen[f.Key] {
			return t, invalidf("type %s: field %q twice", t.Key, f.Key)
		}
		seen[f.Key] = true
		f.Label = strings.TrimSpace(f.Label)
		if f.Label == "" {
			f.Label = f.Key
		}
		f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
		if f.Kind == "" {
			f.Kind = KindText
		}
		if !slices.Contains(fieldKinds, f.Kind) {
			return t, invalidf("type %s: field %s: kind %q is not one of %s", t.Key, f.Key, f.Kind, strings.Join(fieldKinds, ", "))
		}
		if f.Kind == KindEnum && len(f.Options) == 0 {
			return t, invalidf("type %s: enum field %s needs options", t.Key, f.Key)
		}
		if f.Kind != KindEnum {
			f.Options = nil
		}
		if f.Kind != KindRelation {
			f.TargetType, f.RelationType = "", ""
		} else if f.RelationType != "" && !keyRe.MatchString(f.RelationType) {
			return t, invalidf("type %s: field %s: relation_type %q is not snake_case", t.Key, f.Key, f.RelationType)
		}
	}
	if t.PrimaryField != "" && !seen[t.PrimaryField] {
		return t, invalidf("type %s: primary_field %q is not one of its fields", t.Key, t.PrimaryField)
	}
	var statuses []string
	for _, s := range t.Statuses {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(statuses, s) {
			statuses = append(statuses, s)
		}
	}
	t.Statuses = statuses
	return t, nil
}

// upsertType defines a type in sp, or evolves one: a built-in type keeps its own fields and statuses and can only
// gain fields.
func (st *State) upsertType(sp *Space, in ObjectType, actor string, now time.Time) (*ObjectType, bool, error) {
	in.Builtin = false
	t, err := normalizeType(in)
	if err != nil {
		return nil, false, err
	}
	old, exists := sp.objectType(t.Key)
	if exists && old.Builtin {
		merged := *old
		for _, f := range t.Fields {
			if _, ok := merged.field(f.Key); !ok {
				merged.Fields = append(merged.Fields, f)
			}
		}
		if in.Description != "" {
			merged.Description = t.Description
		}
		if in.Label != "" {
			merged.Label = t.Label
		}
		t = merged
	}
	for _, f := range t.Fields {
		if f.Kind == KindRelation && f.TargetType != "" && f.TargetType != t.Key {
			if _, ok := sp.objectType(f.TargetType); !ok {
				return nil, false, invalidf("type %s: field %s: target_type %q is not a type of this space", t.Key, f.Key, f.TargetType)
			}
		}
	}
	if exists {
		*old = t
	} else {
		sp.Types = append(sp.Types, t)
	}
	sp.UpdatedAt = now
	verb := "注册了对象类型"
	if exists {
		verb = "更新了对象类型"
	}
	st.log(sp, "schema_upserted", fmt.Sprintf("%s「%s」（%d 个字段）", verb, t.Label, len(t.Fields)), actor, "", "", now)
	got, _ := sp.objectType(t.Key)
	return got, !exists, nil
}

// ---- Values ----

// normalizeValue converts v to f's kind: numbers become float64, booleans bool, dates YYYY-MM-DD, everything
// else a string. A value is never re-typed by its look: a text field keeps "007".
func normalizeValue(sp *Space, f FieldDef, v any) (any, error) {
	if s, ok := v.(string); ok {
		v = strings.TrimSpace(s)
	}
	switch f.Kind {
	case KindNumber:
		switch x := v.(type) {
		case float64:
			return checkNumber(f, x)
		case float32:
			return checkNumber(f, float64(x))
		case int:
			return checkNumber(f, float64(x))
		case int64:
			return checkNumber(f, float64(x))
		case string:
			n, err := strconv.ParseFloat(strings.ReplaceAll(x, ",", ""), 64)
			if err != nil {
				return nil, invalidf("%s (%s) is a number, not %q", f.Label, f.Key, x)
			}
			return checkNumber(f, n)
		}
		return nil, invalidf("%s (%s) is a number", f.Label, f.Key)
	case KindBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(x) {
			case "true", "yes", "1", "on", "是":
				return true, nil
			case "false", "no", "0", "off", "否":
				return false, nil
			}
		}
		return nil, invalidf("%s (%s) is true or false", f.Label, f.Key)
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		s = strconv.FormatBool(x)
	case int:
		s = strconv.Itoa(x)
	default:
		return nil, invalidf("%s (%s) is %s, not %T", f.Label, f.Key, f.Kind, v)
	}
	if len([]rune(s)) > 2000 {
		return nil, invalidf("%s (%s): at most 2000 characters", f.Label, f.Key)
	}
	switch f.Kind {
	case KindDate:
		if dateRe.MatchString(s) {
			if _, err := time.Parse(time.DateOnly, s); err == nil {
				return s, nil
			}
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Format(time.DateOnly), nil
		}
		return nil, invalidf("%s (%s) is a date YYYY-MM-DD, not %q", f.Label, f.Key, s)
	case KindEnum:
		if !slices.Contains(f.Options, s) {
			return nil, invalidf("%s (%s) is one of %s, not %q", f.Label, f.Key, strings.Join(f.Options, " / "), s)
		}
	case KindURL:
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, invalidf("%s (%s) is an http(s) link, not %q", f.Label, f.Key, s)
		}
	case KindRelation:
		target, ok := sp.entity(s)
		if !ok {
			return nil, invalidf("%s (%s): no entity %q in this space", f.Label, f.Key, s)
		}
		if f.TargetType != "" && target.Type != f.TargetType {
			return nil, invalidf("%s (%s): %q is a %s, not a %s", f.Label, f.Key, s, target.Type, f.TargetType)
		}
	}
	return s, nil
}

func checkNumber(f FieldDef, n float64) (any, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, invalidf("%s (%s) is a finite number", f.Label, f.Key)
	}
	return n, nil
}

// formatValue writes a property for a view.
func formatValue(f FieldDef, v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if math.Abs(x) >= 1e6 || (x != math.Trunc(x) && len(s) > 8) {
			s = strconv.FormatFloat(x, 'f', 2, 64)
		}
		if f.Unit != "" {
			s += " " + f.Unit
		}
		return s
	case bool:
		if x {
			return "是"
		}
		return "否"
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		n, err := strconv.ParseFloat(x, 64)
		return n, err == nil
	}
	return 0, false
}

// ---- Entities ----

func (sp *Space) entity(id string) (*Entity, bool) {
	for _, e := range sp.Entities {
		if e.ID == id {
			return e, true
		}
	}
	return nil, false
}

// findEntity looks id up in every space.
func (st *State) findEntity(id string) (*Space, *Entity, bool) {
	for _, sp := range st.Spaces {
		if e, ok := sp.entity(id); ok {
			return sp, e, true
		}
	}
	return nil, nil, false
}

// RelationInput is an edge to add with an entity: from the entity to target (direction out), or from target to it
// (in).
type RelationInput struct {
	Type      string `json:"type"`
	TargetID  string `json:"target_id"`
	Direction string `json:"direction,omitempty"`
}

// EntityInput creates an entity (no id) or updates one (properties merged, a null property removed).
type EntityInput struct {
	SpaceID        string          `json:"space_id,omitempty"`
	ID             string          `json:"id,omitempty"`
	Type           string          `json:"type,omitempty"`
	Title          string          `json:"title,omitempty"`
	Status         string          `json:"status,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Properties     map[string]any  `json:"properties,omitempty"`
	Relations      []RelationInput `json:"relations,omitempty"`
	IncubateToGoal bool            `json:"incubate_to_goal,omitempty"`
	Archived       *bool           `json:"archived,omitempty"`
}

// upsertEntity creates or updates an entity and logs it. With IncubateToGoal an idea is incubated too.
func (st *State) upsertEntity(in EntityInput, actor string, now time.Time) (*Entity, bool, error) {
	var sp *Space
	var e *Entity
	created := in.ID == ""
	if created {
		var err error
		if sp, err = st.space(in.SpaceID); err != nil {
			return nil, false, err
		}
	} else {
		var ok bool
		if sp, e, ok = st.findEntity(in.ID); !ok {
			return nil, false, notFoundf("no entity %q", in.ID)
		}
		if in.SpaceID != "" && in.SpaceID != sp.ID {
			return nil, false, invalidf("entity %q is in space %q, not %q", in.ID, sp.ID, in.SpaceID)
		}
	}
	typeKey := canonicalType(in.Type)
	if created {
		if typeKey == "" {
			return nil, false, invalidf("type is needed to create an entity")
		}
	} else if typeKey != "" && typeKey != e.Type {
		return nil, false, invalidf("entity %q is a %s; its type does not change", e.ID, e.Type)
	} else {
		typeKey = e.Type
	}
	t, ok := sp.objectType(typeKey)
	if !ok {
		return nil, false, notFoundf("space %q has no type %q (space_schema_get lists them)", sp.ID, typeKey)
	}
	next := Entity{Properties: map[string]any{}}
	if !created {
		next = *e
		next.Properties = make(map[string]any, len(e.Properties))
		for k, v := range e.Properties {
			next.Properties[k] = v
		}
	}
	if title := strings.TrimSpace(in.Title); title != "" {
		if len([]rune(title)) > 200 {
			return nil, false, invalidf("a title has at most 200 characters")
		}
		next.Title = title
	}
	if in.Tags != nil {
		next.Tags = cleanTags(in.Tags)
	}
	keys := make([]string, 0, len(in.Properties))
	for k := range in.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in.Properties[k]
		f, ok := t.field(k)
		if !ok {
			names := make([]string, 0, len(t.Fields))
			for _, f := range t.Fields {
				names = append(names, f.Key)
			}
			return nil, false, invalidf("%s has no field %q (fields: %s)", t.Label, k, strings.Join(names, ", "))
		}
		if s, isStr := v.(string); v == nil || (isStr && strings.TrimSpace(s) == "") {
			delete(next.Properties, k)
			continue
		}
		nv, err := normalizeValue(sp, f, v)
		if err != nil {
			return nil, false, err
		}
		next.Properties[k] = nv
	}
	if next.Title == "" {
		if s, ok := next.Properties[t.PrimaryField].(string); ok && s != "" {
			next.Title = truncate(s, 60)
		} else {
			return nil, false, invalidf("a title is needed")
		}
	}
	for _, f := range t.Fields {
		if _, has := next.Properties[f.Key]; f.Required && !has {
			return nil, false, invalidf("%s needs %s (%s)", t.Label, f.Label, f.Key)
		}
	}
	oldStatus := next.Status
	if s := strings.TrimSpace(in.Status); s != "" {
		if len(t.Statuses) > 0 && !slices.Contains(t.Statuses, s) {
			return nil, false, invalidf("%s's status is one of %s, not %q", t.Label, strings.Join(t.Statuses, " / "), s)
		}
		next.Status = s
	} else if next.Status == "" && len(t.Statuses) > 0 {
		next.Status = t.Statuses[0]
	}
	if in.Archived != nil {
		next.Archived = *in.Archived
	}
	if next.Status == StatusArchived {
		next.Archived = true
	}
	if t.Key == TypeGoal {
		autoAchieve(&next)
	}
	if t.Key == TypeFeed && next.Properties["actor"] == nil {
		next.Properties["actor"] = actor
	}
	for _, r := range in.Relations {
		if err := checkRelation(sp, r); err != nil {
			return nil, false, err
		}
	}
	next.UpdatedAt = now
	if created {
		next.ID = st.nextID("e")
		next.SpaceID = sp.ID
		next.Type = t.Key
		next.CreatedAt = now
		e = &next
		sp.Entities = append(sp.Entities, e)
	} else {
		*e = next
	}
	sp.UpdatedAt = now
	// A relation field keeps its edge in the graph.
	for _, f := range t.Fields {
		if f.Kind != KindRelation {
			continue
		}
		rt := f.RelationType
		if rt == "" {
			rt = f.Key
		}
		sp.dropRelations(func(r Relation) bool { return r.FromID == e.ID && r.RelationType == rt && r.ByField })
		if target, ok := e.Properties[f.Key].(string); ok && target != "" {
			st.relate(sp, e.ID, rt, target, now, true)
		}
	}
	for _, r := range in.Relations {
		from, to := e.ID, r.TargetID
		if r.Direction == "in" {
			from, to = to, from
		}
		st.relate(sp, from, strings.TrimSpace(r.Type), to, now, false)
	}
	if t.Key != TypeFeed {
		switch {
		case created:
			st.log(sp, "created", fmt.Sprintf("新建%s「%s」", t.Label, e.Title), actor, e.ID, e.Type, now)
		case oldStatus != e.Status:
			st.log(sp, "status_changed", fmt.Sprintf("%s「%s」：%s → %s", t.Label, e.Title, statusLabel(oldStatus), statusLabel(e.Status)), actor, e.ID, e.Type, now)
		default:
			st.log(sp, "updated", fmt.Sprintf("更新了%s「%s」", t.Label, e.Title), actor, e.ID, e.Type, now)
		}
	} else {
		st.trimFeed(sp)
	}
	if in.IncubateToGoal {
		if e.Type != TypeIdea {
			return nil, false, invalidf("incubate_to_goal is for ideas, %q is a %s", e.ID, e.Type)
		}
		if _, err := st.incubate(e.ID, TypeGoal, actor, now); err != nil {
			return nil, false, err
		}
	}
	return e, created, nil
}

// autoAchieve marks a goal achieved when its current value reaches the target.
func autoAchieve(e *Entity) {
	target, ok1 := number(e.Properties["target_value"])
	cur, ok2 := number(e.Properties["current_value"])
	if ok1 && ok2 && target > 0 && cur >= target && (e.Status == GoalPlanned || e.Status == GoalActive) {
		e.Status = GoalAchieved
	}
}

func cleanTags(in []string) []string {
	out := []string{}
	for _, t := range in {
		for _, part := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == '，' || r == '#' }) {
			part = strings.TrimSpace(part)
			if part != "" && len([]rune(part)) <= 30 && !slices.Contains(out, part) && len(out) < 20 {
				out = append(out, part)
			}
		}
	}
	return out
}

func checkRelation(sp *Space, r RelationInput) error {
	if !keyRe.MatchString(strings.TrimSpace(r.Type)) {
		return invalidf("relation type %q is snake_case (spawns, belongs_to, located_in, supports …)", r.Type)
	}
	if _, ok := sp.entity(r.TargetID); !ok {
		return invalidf("relation target %q is not an entity of space %q", r.TargetID, sp.ID)
	}
	if r.Direction != "" && r.Direction != "out" && r.Direction != "in" {
		return invalidf("relation direction is out or in, not %q", r.Direction)
	}
	return nil
}

// relate adds an edge unless the same one is there.
func (st *State) relate(sp *Space, from, typ, to string, now time.Time, fromField bool) {
	for _, r := range sp.Relations {
		if r.FromID == from && r.ToID == to && r.RelationType == typ {
			return
		}
	}
	sp.Relations = append(sp.Relations, Relation{ID: st.nextID("r"), SpaceID: sp.ID, FromID: from, RelationType: typ,
		ToID: to, CreatedAt: now, ByField: fromField})
}

func (sp *Space) dropRelations(drop func(Relation) bool) int {
	kept := sp.Relations[:0]
	n := 0
	for _, r := range sp.Relations {
		if drop(r) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	sp.Relations = kept
	return n
}

// addRelation adds an edge between two entities of a space and logs it.
func (st *State) addRelation(spaceID, from, typ, to, actor string, now time.Time) (Relation, error) {
	sp, a, ok := st.findEntity(from)
	if !ok {
		return Relation{}, notFoundf("no entity %q", from)
	}
	if spaceID != "" && sp.ID != spaceID {
		return Relation{}, invalidf("entity %q is not in space %q", from, spaceID)
	}
	if err := checkRelation(sp, RelationInput{Type: typ, TargetID: to}); err != nil {
		return Relation{}, err
	}
	if from == to {
		return Relation{}, invalidf("an entity does not relate to itself")
	}
	st.relate(sp, from, typ, to, now, false)
	b, _ := sp.entity(to)
	st.log(sp, "related", fmt.Sprintf("「%s」%s「%s」", a.Title, relationLabel(typ), b.Title), actor, a.ID, a.Type, now)
	for _, r := range sp.Relations {
		if r.FromID == from && r.ToID == to && r.RelationType == typ {
			return r, nil
		}
	}
	return Relation{}, errNotFound
}

// deleteEntity removes an entity and its edges (archive: keeps it, archived).
func (st *State) deleteEntity(id string, archive bool, actor string, now time.Time) (*Entity, error) {
	sp, e, ok := st.findEntity(id)
	if !ok {
		return nil, notFoundf("no entity %q", id)
	}
	t, _ := sp.objectType(e.Type)
	label := e.Type
	if t != nil {
		label = t.Label
	}
	if archive {
		e.Archived = true
		if t != nil && slices.Contains(t.Statuses, StatusArchived) {
			e.Status = StatusArchived
		}
		e.UpdatedAt = now
		st.log(sp, "archived", fmt.Sprintf("归档了%s「%s」", label, e.Title), actor, e.ID, e.Type, now)
		return e, nil
	}
	sp.Entities = slices.DeleteFunc(sp.Entities, func(x *Entity) bool { return x.ID == id })
	sp.dropRelations(func(r Relation) bool { return r.FromID == id || r.ToID == id })
	for _, other := range sp.Entities { // relation fields pointing at it
		if ot, ok := sp.objectType(other.Type); ok {
			for _, f := range ot.Fields {
				if f.Kind == KindRelation && other.Properties[f.Key] == id {
					delete(other.Properties, f.Key)
				}
			}
		}
	}
	sp.UpdatedAt = now
	if e.Type != TypeFeed {
		st.log(sp, "deleted", fmt.Sprintf("删除了%s「%s」", label, e.Title), actor, "", e.Type, now)
	}
	return e, nil
}

// incubate turns an idea into a goal (or a resource): the new entity, a spawns edge from the idea, the idea
// marked incubated, and the activity logged.
func (st *State) incubate(ideaID, into, actor string, now time.Time) (*Entity, error) {
	sp, idea, ok := st.findEntity(ideaID)
	if !ok {
		return nil, notFoundf("no entity %q", ideaID)
	}
	if idea.Type != TypeIdea {
		return nil, invalidf("%q is a %s, not an idea", ideaID, idea.Type)
	}
	if into != TypeGoal && into != TypeResource {
		return nil, invalidf("an idea incubates into a goal or a resource, not %q", into)
	}
	if idea.Status == IdeaIncubated {
		if tid, _ := idea.Properties["incubated_target_id"].(string); tid != "" {
			if existing, ok := sp.entity(tid); ok {
				return existing, nil
			}
		}
		return nil, invalidf("idea %q is already incubated", ideaID)
	}
	props := map[string]any{}
	summary, _ := idea.Properties["summary"].(string)
	status := GoalPlanned
	if into == TypeGoal {
		props["target_value"], props["current_value"], props["unit"] = 100.0, 0.0, "%"
		if p, ok := idea.Properties["priority"].(string); ok {
			props["priority"] = p
		}
		props["milestone_summary"] = firstNonEmpty(summary, "由点子孵化，先拆出第一个里程碑")
	} else {
		status = "saved"
		props["summary"] = firstNonEmpty(summary, "由点子转化")
		if c, ok := idea.Properties["category"].(string); ok {
			props["category"] = c
		}
	}
	tt, _ := sp.objectType(into)
	child := &Entity{ID: st.nextID("e"), SpaceID: sp.ID, Type: into, Title: idea.Title, Status: status,
		Tags: slices.Clone(idea.Tags), Properties: props, CreatedAt: now, UpdatedAt: now}
	sp.Entities = append(sp.Entities, child)
	idea.Status = IdeaIncubated
	idea.Properties["incubated_target_id"] = child.ID
	idea.UpdatedAt = now
	st.relate(sp, idea.ID, "spawns", child.ID, now, true)
	sp.UpdatedAt = now
	st.log(sp, "incubated", fmt.Sprintf("点子「%s」孵化为%s", idea.Title, tt.Label), actor, child.ID, child.Type, now)
	return child, nil
}

// setStatus moves an entity along its type's workflow.
func (st *State) setStatus(id, status, actor string, now time.Time) (*Entity, error) {
	e, _, err := st.upsertEntity(EntityInput{ID: id, Status: status}, actor, now)
	return e, err
}

// advanceGoal adds step (a fraction of the target) to a goal's current value; set, when not nil, sets it instead.
func (st *State) advanceGoal(id string, step float64, set *float64, actor string, now time.Time) (*Entity, error) {
	_, e, ok := st.findEntity(id)
	if !ok {
		return nil, notFoundf("no entity %q", id)
	}
	if e.Type != TypeGoal {
		return nil, invalidf("%q is a %s, not a goal", id, e.Type)
	}
	target, _ := number(e.Properties["target_value"])
	cur, _ := number(e.Properties["current_value"])
	if set != nil {
		cur = *set
	} else {
		if target <= 0 {
			target = 100
			e.Properties["target_value"] = target
		}
		cur = math.Min(target, cur+target*step)
	}
	cur = math.Round(cur*100) / 100
	status := ""
	if e.Status == GoalPlanned {
		status = GoalActive
	}
	return st.upsertEntityStatus(e, cur, status, actor, now)
}

func (st *State) upsertEntityStatus(e *Entity, cur float64, status, actor string, now time.Time) (*Entity, error) {
	out, _, err := st.upsertEntity(EntityInput{ID: e.ID, Status: status, Properties: map[string]any{"current_value": cur}}, actor, now)
	return out, err
}

// ---- Feed ----

// log records an activity (a feed entity) in sp.
func (st *State) log(sp *Space, event, summary, actor, targetID, targetType string, now time.Time) {
	if actor == "" {
		actor = "你"
	}
	props := map[string]any{"event_type": event, "summary": summary, "actor": actor}
	if targetID != "" {
		props["target_id"] = targetID
	}
	if targetType != "" {
		props["target_type"] = targetType
	}
	sp.Entities = append(sp.Entities, &Entity{ID: st.nextID("e"), SpaceID: sp.ID, Type: TypeFeed, Title: truncate(summary, 80),
		Properties: props, CreatedAt: now, UpdatedAt: now})
	st.trimFeed(sp)
}

func (st *State) trimFeed(sp *Space) {
	n := 0
	for _, e := range sp.Entities {
		if e.Type == TypeFeed {
			n++
		}
	}
	if n <= feedLimit {
		return
	}
	drop := n - feedLimit
	sp.Entities = slices.DeleteFunc(sp.Entities, func(e *Entity) bool {
		if drop > 0 && e.Type == TypeFeed {
			drop--
			return true
		}
		return false
	})
}

// ---- Queries ----

// Query selects entities of a space.
type Query struct {
	SpaceID         string         `json:"space_id,omitempty"`
	Type            string         `json:"type,omitempty"`
	Keyword         string         `json:"keyword,omitempty"`
	Status          string         `json:"status,omitempty"`
	Filters         map[string]any `json:"filters,omitempty"`
	IncludeArchived bool           `json:"include_archived,omitempty"`
	Limit           int            `json:"limit,omitempty"`
}

// query answers q newest first, and how many matched in all.
func (sp *Space) query(q Query) ([]*Entity, int) {
	typ := canonicalType(q.Type)
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	var out []*Entity
	for _, e := range sp.Entities {
		if typ != "" && e.Type != typ {
			continue
		}
		if typ == "" && e.Type == TypeFeed && q.Type != "all" && q.Type != "*" {
			continue // all types: the feed only when asked by name
		}
		if e.Archived && !q.IncludeArchived && q.Status != StatusArchived {
			continue
		}
		if q.Status != "" && e.Status != q.Status {
			continue
		}
		if kw != "" && !matches(e, kw) {
			continue
		}
		ok := true
		for k, want := range q.Filters {
			got, has := e.Properties[k]
			if !has || !sameValue(got, want) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return idNum(out[i].ID) > idNum(out[j].ID)
	})
	total := len(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, total
}

func idNum(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimLeft(id, "abcdefghijklmnopqrstuvwxyz-"), 10, 64)
	return n
}

func matches(e *Entity, kw string) bool {
	if strings.Contains(strings.ToLower(e.Title), kw) {
		return true
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), kw) {
			return true
		}
	}
	for _, v := range e.Properties {
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), kw) {
			return true
		}
	}
	return false
}

func sameValue(got, want any) bool {
	if gn, ok := got.(float64); ok {
		if wn, ok := number(want); ok {
			return gn == wn
		}
	}
	if gb, ok := got.(bool); ok {
		switch w := want.(type) {
		case bool:
			return gb == w
		case string:
			return strconv.FormatBool(gb) == strings.ToLower(w)
		}
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// edges are an entity's relations: out from it, in to it.
func (sp *Space) edges(id string) (out, in []Relation) {
	for _, r := range sp.Relations {
		if r.FromID == id {
			out = append(out, r)
		}
		if r.ToID == id {
			in = append(in, r)
		}
	}
	return out, in
}

func (sp *Space) count(typ string, live bool) int {
	n := 0
	for _, e := range sp.Entities {
		if e.Type == typ && (!live || !e.Archived) {
			n++
		}
	}
	return n
}

func (sp *Space) title(id string) string {
	if e, ok := sp.entity(id); ok {
		return e.Title
	}
	return id
}

// typeLabel is a type's label in sp.
func (sp *Space) typeLabel(key string) string {
	if t, ok := sp.objectType(key); ok {
		return t.Label
	}
	return key
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// goalRatio is a goal's progress from 0 to 1.
func goalRatio(e *Entity) float64 {
	target, ok1 := number(e.Properties["target_value"])
	cur, ok2 := number(e.Properties["current_value"])
	if e.Status == GoalAchieved {
		return 1
	}
	if !ok1 || !ok2 || target <= 0 {
		return 0
	}
	return math.Max(0, math.Min(1, cur/target))
}
