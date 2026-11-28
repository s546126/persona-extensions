package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// unsetValue is the choice that stands for "no value". A choice is sent as "=" and the value; "=" alone is unset,
// so no option can collide with the unset marker.
const (
	choicePrefix = "="
	unsetLabel   = "未设置"
	pickLimit    = 100 // the newest entities offered by a relation's choice
	quickAddMost = 4   // the most optional fields shown in a quick-add fold
)

// fieldKinds maps the abstract control of a kind one to one to the field kind the apps draw. A computed kind has no
// control and is not in the table.
var fieldKinds = map[kernel.Input]string{
	kernel.InLine:   wire.FieldKindText,
	kernel.InNumber: wire.FieldKindNumber,
	kernel.InToggle: wire.FieldKindToggle,
	kernel.InChoice: wire.FieldKindChoice,
	kernel.InPick:   wire.FieldKindChoice,
}

// field is the form field of one stored property, named name, holding v. ok is false for a computed property, which
// has no input.
func (c *ctx) field(p kernel.Prop, name string, v any) (wire.Component, bool) {
	k, ok := kernel.KindOf(p.Kind)
	if !ok {
		return wire.Component{}, false
	}
	kind, ok := fieldKinds[k.Input]
	if !ok {
		return wire.Component{}, false
	}
	if p.Kind == kernel.KindDate && c.uiAtLeast(6) {
		kind = wire.FieldKindDate
	}
	f := wire.Component{
		Type: wire.ComponentField, Name: name, Label: p.Name, Kind: kind,
		Value: fieldValue(k, v),
	}
	switch {
	case kind == wire.FieldKindDate:
		// Native date picker: no text placeholder needed.
	case k.Input == kernel.InLine:
		f.Placeholder = kindPlaceholder(k)
	case k.Input == kernel.InChoice:
		f.Choices = optionChoices(k.Enum(&p))
	case k.Input == kernel.InPick:
		f.Choices = c.targetChoices(p, v)
	}
	return f, true
}

// multiRelationField renders a many-relation as a native multichoice on UI >= 6 when its targets fit the control's
// choice limit, and falls back to the single-add choice field otherwise.
func (c *ctx) multiRelationField(p kernel.Prop, name string, e *kernel.Entity) (wire.Component, bool) {
	if !c.uiAtLeast(6) || c.sp == nil {
		return c.addField(p, name, e)
	}
	k, ok := kernel.KindOf(p.Kind)
	if !ok || k.Refs == nil {
		return wire.Component{}, false
	}
	held := k.Refs(e.Values[p.Key])
	if len(held) > wire.MultiChoicesMost {
		return c.addField(p, name, e)
	}
	var set []*kernel.Entity
	have := map[string]bool{}
	for _, id := range held {
		if target := c.w.Entity(id); target != nil && target.Type == p.Target && !have[target.ID] {
			have[target.ID] = true
			set = append(set, target)
		}
	}
	var rest []*kernel.Entity
	for _, ent := range c.sp.Entities {
		if ent != nil && ent.Type == p.Target && !have[ent.ID] {
			rest = append(rest, ent)
		}
	}
	slices.SortStableFunc(rest, func(a, b *kernel.Entity) int { return b.Updated.Compare(a.Updated) })
	for _, ent := range rest {
		if len(set) >= wire.MultiChoicesMost {
			break
		}
		set = append(set, ent)
	}
	if len(set) == 0 {
		return c.addField(p, name, e)
	}
	choices := make([]wire.Choice, 0, len(set))
	for _, ent := range set {
		choices = append(choices, wire.Choice{Value: ent.ID, Title: cut(ent.Title, maxTitle)})
	}
	val := ""
	if len(held) > 0 {
		var selected []string
		for _, id := range held {
			if have[id] {
				selected = append(selected, id)
			}
		}
		if len(selected) > 0 {
			raw, _ := json.Marshal(selected)
			val = string(raw)
		}
	}
	return wire.Component{
		Type: wire.ComponentField, Name: name, Label: p.Name,
		Kind: wire.FieldKindMultichoice, Value: val, Choices: choices,
	}, true
}

// parseMultichoice decodes a multichoice JSON array of entity ids. ok is false when raw is not a JSON array (such as
// a single-choice "=id" value sent by an older client).
func parseMultichoice(raw string) (ids []string, isMulti bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
		return nil, true, errors.New("多选内容没有读懂，请重新选择")
	}
	return ids, true, nil
}

// quickAddFields selects up to quickAddMost high-value stored properties of t for quick capture, prioritizing card
// pills and lead first, then remaining stored properties in schema order. skipProp omits a pre-bound relation.
func (c *ctx) quickAddFields(t *kernel.Type, skipProp string) []wire.Component {
	card := t.CardOrDefault()
	var keys []string
	addKey := func(k string) {
		if k == "" || k == skipProp || slices.Contains(keys, k) {
			return
		}
		p, ok := propOf(t, k)
		if !ok || p.Many {
			return
		}
		kind, known := kernel.KindOf(p.Kind)
		if !known || kind.Input == kernel.InNone {
			return
		}
		keys = append(keys, k)
	}
	for _, k := range card.Pills {
		addKey(k)
	}
	addKey(card.Lead)
	for _, p := range t.Props {
		addKey(p.Key)
	}
	if len(keys) > quickAddMost {
		keys = keys[:quickAddMost]
	}
	var out []wire.Component
	for _, key := range keys {
		p, _ := propOf(t, key)
		if f, ok := c.field(p, "p_"+p.Key, nil); ok {
			out = append(out, f)
		}
	}
	return out
}

// fieldValue is the text a field starts with: a choice is sent as "=" and its value, a number as its plain decimal, a
// toggle as true or false, a relation as its first id.
func fieldValue(k *kernel.Kind, v any) string {
	switch k.Input {
	case kernel.InChoice:
		s, _ := v.(string)
		return choicePrefix + s
	case kernel.InPick:
		ids := k.Refs(v)
		if len(ids) == 0 {
			return choicePrefix
		}
		return choicePrefix + ids[0]
	case kernel.InNumber:
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return ""
	case kernel.InToggle:
		b, _ := v.(bool)
		return strconv.FormatBool(b)
	}
	s, _ := v.(string)
	return s
}

// optionChoices offers the unset choice and then the options, in order.
func optionChoices(options []string) []wire.Choice {
	out := []wire.Choice{{Value: choicePrefix, Title: unsetLabel}}
	for _, o := range options {
		out = append(out, wire.Choice{Value: choicePrefix + o, Title: o})
	}
	return out
}

// targetChoices offers the unset choice and the newest entities of the relation's target type. The value the field
// holds is always offered, even when it is older than the newest ones.
func (c *ctx) targetChoices(p kernel.Prop, v any) []wire.Choice {
	out := []wire.Choice{{Value: choicePrefix, Title: unsetLabel}}
	if c.sp == nil {
		return out
	}
	k, _ := kernel.KindOf(p.Kind)
	held := ""
	if ids := k.Refs(v); len(ids) > 0 {
		held = ids[0]
	}
	var set []*kernel.Entity
	for _, e := range c.sp.Entities {
		if e != nil && e.Type == p.Target {
			set = append(set, e)
		}
	}
	slices.SortStableFunc(set, func(a, b *kernel.Entity) int { return b.Updated.Compare(a.Updated) })
	if len(set) > pickLimit {
		set = set[:pickLimit]
	}
	if held != "" && !slices.ContainsFunc(set, func(e *kernel.Entity) bool { return e.ID == held }) {
		if e := c.w.Entity(held); e != nil {
			set = append([]*kernel.Entity{e}, set...)
		}
	}
	for _, e := range set {
		out = append(out, wire.Choice{Value: choicePrefix + e.ID, Title: cut(e.Title, maxTitle)})
	}
	return out
}

// submitted reads the text a form sent for a property into its canonical value through the kind's Parse. A choice or
// a relation is sent as "=" and the value; "" and "=" are unset. A computed property cannot be submitted.
func submitted(p kernel.Prop, raw string) (any, error) {
	k, ok := kernel.KindOf(p.Kind)
	if !ok {
		return nil, fmt.Errorf("「%s」的种类「%s」认不出来，这一项没法在这里填。请让助手看看", p.Name, p.Kind)
	}
	if k.Input == kernel.InNone {
		return nil, fmt.Errorf("「%s」是自动统计出来的，不用填", p.Name)
	}
	if k.Input == kernel.InChoice || k.Input == kernel.InPick {
		raw = strings.TrimPrefix(raw, choicePrefix)
	}
	if raw == "" {
		return nil, nil
	}
	return k.Parse(&p, raw)
}
