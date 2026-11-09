package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// `spaces mcp`: a Model Context Protocol server on stdio (JSON-RPC 2.0, one message a line) for the Claude assistant.
// Three tools only read (the manifest allows them without asking): space_list, space_query, space_schema_get. Four
// write and go by the conversation's permission policy: space_create, space_schema_upsert, space_entity_upsert,
// space_entity_delete. It reads and writes the same spaces_state.json as the extension the hub runs, which reads
// the file again on its next render.

const mcpProtocol = "2025-06-18"

const mcpActor = "助手"

type mcpServer struct {
	store *Store
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// serve answers requests from in on out until in ends.
func (m *mcpServer) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" { // a notification: no answer
			continue
		}
		res, rerr := m.handle(msg.Method, msg.Params)
		resp := rpcResponse{JSONRPC: "2.0", ID: msg.ID, Result: res, Error: rerr}
		if rerr == nil && res == nil {
			resp.Result = struct{}{}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (m *mcpServer) handle(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{
			"protocolVersion": firstNonEmpty(p.ProtocolVersion, mcpProtocol),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "spaces", "version": about.Version},
			"instructions": "Persona 空间（Muse 风格的多空间本体工作台）。先用 space_list 看有哪些空间和对象类型，" +
				"space_schema_get 看某个空间的字段约束，space_query 查实体和关系；写入用 space_entity_upsert 等工具，" +
				"属性必须符合对象类型的字段（kind 决定类型，文本不会被转成数字）。",
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		out, err := m.call(p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}, nil
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}, "structuredContent": out}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}

var errUnknownTool = errors.New("unknown tool")

// readOnlyTools are the tools the manifest allows without asking.
var readOnlyTools = []string{"space_list", "space_query", "space_schema_get"}

func mcpTools() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	field := map[string]any{"type": "object", "properties": map[string]any{
		"key": str("snake_case"), "label": str("显示名"),
		"kind":     map[string]any{"type": "string", "enum": fieldKinds},
		"required": map[string]any{"type": "boolean"}, "unit": str("number 的单位"),
		"options":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "enum 的选项"},
		"target_type": str("relation 指向的类型"), "relation_type": str("relation 在关系图里的名字，如 belongs_to"),
	}, "required": []string{"key", "kind"}}
	ro := map[string]any{"readOnlyHint": true}
	return []map[string]any{
		{"name": "space_list", "description": "列出所有空间：名字、模板、是否当前、本体（对象类型和字段数）、每种类型的实体数和关系数。",
			"inputSchema": obj(map[string]any{"include_archived": map[string]any{"type": "boolean"}}), "annotations": ro},
		{"name": "space_query", "description": "按空间查实体及其关系边。type 可以是 feed（别名 dynamic/activity）、idea、goal、resource 或任何自定义对象类型，不填是除动态外的全部；" +
			"keyword 搜标题、标签和文本属性；status 按状态；filters 按属性键值精确匹配。",
			"inputSchema": obj(map[string]any{
				"space_id": str("不填是当前空间"), "type": str("对象类型 key"), "keyword": str("关键词"), "status": str("状态"),
				"filters":          map[string]any{"type": "object", "description": "属性 key → 值"},
				"include_archived": map[string]any{"type": "boolean"},
				"limit":            map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
			}), "annotations": ro},
		{"name": "space_schema_get", "description": "一个空间的完整本体：对象类型、字段约束（kind、必填、单位、选项、关系目标）、状态流转、关系类型和视图定义。",
			"inputSchema": obj(map[string]any{"space_id": str("不填是当前空间")}), "annotations": ro},
		{"name": "space_create", "description": "创建空间（模板 lifeos、deals、cloud、blank，或自定名字、图标、说明），也可以 switch / archive / unarchive 一个已有空间。",
			"inputSchema": obj(map[string]any{
				"action":   map[string]any{"type": "string", "enum": []string{"create", "switch", "archive", "unarchive"}},
				"space_id": str("switch / archive / unarchive 的空间；create 时是可选的新 id"),
				"template": map[string]any{"type": "string", "enum": []string{"lifeos", "deals", "cloud", "blank"}},
				"name":     str("空间名"), "icon": str("图标（folder、star、money、code …）"), "description": str("说明"),
				"seed": map[string]any{"type": "boolean", "description": "放入模板的示例数据（默认 true）"},
			})},
		{"name": "space_schema_upsert", "description": "在空间里定义或演进一个对象类型：字段、主字段、状态、图标、说明。内置类型（feed/idea/goal/resource）只能加字段。",
			"inputSchema": obj(map[string]any{
				"space_id": str("不填是当前空间"), "key": str("类型 key，snake_case"), "label": str("显示名"), "icon": str("图标"),
				"description": str("说明"), "primary_field": str("主字段 key"),
				"statuses": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"fields":   map[string]any{"type": "array", "items": field},
			}, "key")},
		{"name": "space_entity_upsert", "description": "新建（不给 id）或更新（给 id，属性合并，null 删除）一个实体：动态、点子、目标、资源或自定义类型。" +
			"relations 加关系边 [{type, target_id, direction: out|in}]；点子可带 incubate_to_goal: true 直接孵化为目标。",
			"inputSchema": obj(map[string]any{
				"space_id": str("不填是当前空间"), "id": str("要更新的实体"), "type": str("对象类型 key"), "title": str("标题"),
				"status": str("状态"), "tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"properties": map[string]any{"type": "object", "description": "字段 key → 值，按字段 kind 校验"},
				"relations": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"type": str("关系名"), "target_id": str("对方实体"), "direction": map[string]any{"type": "string", "enum": []string{"out", "in"}},
				}, "required": []string{"type", "target_id"}}},
				"incubate_to_goal": map[string]any{"type": "boolean"},
				"archived":         map[string]any{"type": "boolean"},
			})},
		{"name": "space_entity_delete", "description": "删除一个实体和它的关系边；archive: true 只归档。",
			"inputSchema": obj(map[string]any{"id": str("实体 id"), "archive": map[string]any{"type": "boolean"}}, "id")},
	}
}

func (m *mcpServer) call(name string, args json.RawMessage) (any, error) {
	dec := func(v any) error {
		d := json.NewDecoder(strings.NewReader(string(args)))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return invalidf("arguments: %v", err)
		}
		return nil
	}
	switch name {
	case "space_list":
		var p struct {
			IncludeArchived bool `json:"include_archived"`
		}
		if err := dec(&p); err != nil {
			return nil, err
		}
		var out any
		m.store.read(func(st *State) { out = listSpaces(st, p.IncludeArchived) })
		return out, nil
	case "space_query":
		var q Query
		if err := dec(&q); err != nil {
			return nil, err
		}
		var out any
		var err error
		m.store.read(func(st *State) { out, err = querySpace(st, q) })
		return out, err
	case "space_schema_get":
		var p struct {
			SpaceID string `json:"space_id"`
		}
		if err := dec(&p); err != nil {
			return nil, err
		}
		var out any
		var err error
		m.store.read(func(st *State) {
			var sp *Space
			if sp, err = st.space(p.SpaceID); err == nil {
				out = schemaOf(sp)
			}
		})
		return out, err
	case "space_create":
		var p struct {
			Action  string `json:"action"`
			SpaceID string `json:"space_id"`
			SpaceInput
		}
		if err := dec(&p); err != nil {
			return nil, err
		}
		var out any
		err := m.store.update(func(st *State, now time.Time) error {
			var sp *Space
			var err error
			switch firstNonEmpty(p.Action, "create") {
			case "create":
				in := p.SpaceInput
				in.ID = p.SpaceID
				sp, err = st.createSpace(in, mcpActor, now)
			case "switch":
				sp, err = st.switchSpace(p.SpaceID)
			case "archive":
				sp, err = st.archiveSpace(p.SpaceID, true, mcpActor, now)
			case "unarchive":
				sp, err = st.archiveSpace(p.SpaceID, false, mcpActor, now)
			default:
				err = invalidf("action is create, switch, archive or unarchive")
			}
			if err == nil {
				out = map[string]any{"space": spaceSummary(st, sp), "active": st.Active}
			}
			return err
		})
		return out, err
	case "space_schema_upsert":
		var p struct {
			SpaceID string `json:"space_id"`
			ObjectType
		}
		if err := dec(&p); err != nil {
			return nil, err
		}
		var out any
		err := m.store.update(func(st *State, now time.Time) error {
			sp, err := st.space(p.SpaceID)
			if err != nil {
				return err
			}
			t, created, err := st.upsertType(sp, p.ObjectType, mcpActor, now)
			if err == nil {
				out = map[string]any{"space_id": sp.ID, "created": created, "type": t}
			}
			return err
		})
		return out, err
	case "space_entity_upsert":
		var in EntityInput
		if err := dec(&in); err != nil {
			return nil, err
		}
		var out any
		err := m.store.update(func(st *State, now time.Time) error {
			e, created, err := st.upsertEntity(in, mcpActor, now)
			if err != nil {
				return err
			}
			sp, _, _ := st.findEntity(e.ID)
			res := map[string]any{"created": created, "entity": entityOut(sp, e)}
			if in.IncubateToGoal {
				if id, ok := e.Properties["incubated_target_id"].(string); ok {
					if g, ok := sp.entity(id); ok {
						res["goal"] = entityOut(sp, g)
					}
				}
			}
			out = res
			return nil
		})
		return out, err
	case "space_entity_delete":
		var p struct {
			ID      string `json:"id"`
			Archive bool   `json:"archive"`
		}
		if err := dec(&p); err != nil {
			return nil, err
		}
		var out any
		err := m.store.update(func(st *State, now time.Time) error {
			e, err := st.deleteEntity(p.ID, p.Archive, mcpActor, now)
			if err == nil {
				out = map[string]any{"id": e.ID, "archived": p.Archive, "deleted": !p.Archive, "title": e.Title}
			}
			return err
		})
		return out, err
	}
	return nil, fmt.Errorf("%w %q", errUnknownTool, name)
}

func spaceSummary(st *State, sp *Space) map[string]any {
	if sp == nil {
		return nil
	}
	counts := map[string]int{}
	for _, e := range sp.Entities {
		if !e.Archived {
			counts[e.Type]++
		}
	}
	types := []map[string]any{}
	for _, t := range sp.Types {
		types = append(types, map[string]any{"key": t.Key, "label": t.Label, "builtin": t.Builtin, "fields": len(t.Fields),
			"statuses": t.Statuses, "entities": counts[t.Key]})
	}
	return map[string]any{"id": sp.ID, "name": sp.Name, "icon": sp.Icon, "description": sp.Description, "template": sp.Template,
		"archived": sp.Archived, "active": st.Active == sp.ID, "types": types,
		"stats": map[string]any{"entities": len(sp.Entities) - sp.count(TypeFeed, false), "feed": sp.count(TypeFeed, false),
			"relations": len(sp.Relations), "by_type": counts}}
}

func listSpaces(st *State, archived bool) map[string]any {
	out := []map[string]any{}
	for _, sp := range st.Spaces {
		if sp.Archived && !archived {
			continue
		}
		out = append(out, spaceSummary(st, sp))
	}
	return map[string]any{"active": st.Active, "spaces": out}
}

type edgeOut struct {
	ID           string `json:"id"`
	RelationType string `json:"relation_type"`
	OtherID      string `json:"other_id"`
	OtherTitle   string `json:"other_title"`
	OtherType    string `json:"other_type"`
}

func entityOut(sp *Space, e *Entity) map[string]any {
	out, in := sp.edges(e.ID)
	edges := func(rels []Relation, other func(Relation) string) []edgeOut {
		list := []edgeOut{}
		for _, r := range rels {
			id := other(r)
			o := edgeOut{ID: r.ID, RelationType: r.RelationType, OtherID: id, OtherTitle: sp.title(id)}
			if x, ok := sp.entity(id); ok {
				o.OtherType = x.Type
			}
			list = append(list, o)
		}
		return list
	}
	return map[string]any{"id": e.ID, "space_id": e.SpaceID, "type": e.Type, "title": e.Title, "status": e.Status,
		"tags": e.Tags, "properties": e.Properties, "created_at": e.CreatedAt, "updated_at": e.UpdatedAt, "archived": e.Archived,
		"relations": map[string]any{
			"out": edges(out, func(r Relation) string { return r.ToID }),
			"in":  edges(in, func(r Relation) string { return r.FromID }),
		}}
}

func querySpace(st *State, q Query) (map[string]any, error) {
	sp, err := st.space(q.SpaceID)
	if err != nil {
		return nil, err
	}
	if t := canonicalType(q.Type); t != "" {
		if _, ok := sp.objectType(t); !ok {
			return nil, notFoundf("space %q has no type %q", sp.ID, q.Type)
		}
	}
	if q.Limit <= 0 {
		q.Limit = 30
	}
	q.Limit = min(q.Limit, 200)
	list, total := sp.query(q)
	ents := []map[string]any{}
	for _, e := range list {
		ents = append(ents, entityOut(sp, e))
	}
	return map[string]any{"space_id": sp.ID, "space": sp.Name, "type": canonicalType(q.Type), "total": total,
		"returned": len(ents), "entities": ents}, nil
}

// schemaOf is a space's ontology as space_schema_get answers it.
func schemaOf(sp *Space) map[string]any {
	used := sp.relationTypeList()
	rels := []map[string]string{}
	for _, rt := range used {
		rels = append(rels, map[string]string{"key": rt, "label": relationLabel(rt)})
	}
	views := []map[string]any{
		{"id": tabActivity, "title": "动态", "type": TypeFeed, "layout": "stream", "stats": []string{"总动态", "今日新增", "点子数", "目标与资源数"}},
		{"id": tabIdeas, "title": "点子", "type": TypeIdea, "layout": "status_groups", "groups": []string{IdeaInbox, IdeaExploring, IdeaIncubated},
			"actions": []string{"开始探索", "孵化为目标", "转化为资源", "归档"}},
		{"id": tabGoals, "title": "目标", "type": TypeGoal, "layout": "progress", "progress": "current_value / target_value unit"},
	}
	for _, t := range sp.Types {
		if t.Key == TypeFeed {
			continue
		}
		cols := []string{"title"}
		if len(t.Statuses) > 0 {
			cols = append(cols, "status")
		}
		for _, f := range t.Fields {
			if len(cols) < 8 {
				cols = append(cols, f.Key)
			}
		}
		views = append(views, map[string]any{"id": tabLibrary + ":" + t.Key, "title": "资源库 · " + t.Label, "type": t.Key,
			"layout": "table+cards", "columns": cols})
	}
	statusText := map[string]string{}
	for _, t := range sp.Types {
		for _, s := range t.Statuses {
			statusText[s] = statusLabel(s)
		}
	}
	return map[string]any{"space_id": sp.ID, "name": sp.Name, "types": sp.Types, "relation_types": rels,
		"field_kinds": fieldKinds, "status_labels": statusText, "views": views}
}
