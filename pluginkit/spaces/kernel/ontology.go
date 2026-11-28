package kernel

import (
	"cmp"
	"slices"
	"time"
)

// SchemaEdge describes one relation property connecting FromType to ToType within a space.
type SchemaEdge struct {
	FromType string `json:"from_type"`
	FromName string `json:"from_name"`
	PropKey  string `json:"prop_key"`
	PropName string `json:"prop_name"`
	ToType   string `json:"to_type"`
	ToName   string `json:"to_name"`
	Many     bool   `json:"many,omitempty"`
	Back     string `json:"back,omitempty"`
}

// DueItem is an entity with a date property that is upcoming or recently overdue relative to a day.
// Days is negative when overdue, 0 when due today, and positive when due in the future.
type DueItem struct {
	Entity *Entity
	Type   *Type
	Prop   Prop
	Date   string
	Days   int
}

// SpaceSummary aggregates counts across all entities and types of one space.
type SpaceSummary struct {
	Types       int      `json:"types"`
	Total       int      `json:"total"`
	Todo        int      `json:"todo"`
	Doing       int      `json:"doing"`
	Done        int      `json:"done"`
	Dropped     int      `json:"dropped"`
	Relations   int      `json:"relations"`
	PercentDone *float64 `json:"percent_done,omitempty"`
}

// overdueWindow is how many days back an unfinished entity with a lifecycle is still surfaced as overdue.
const overdueWindow = 30

// SchemaEdges returns every relation property declared across the types of the space, in schema order.
func (w *World) SchemaEdges() []SchemaEdge {
	if w == nil || w.s == nil {
		return nil
	}
	var out []SchemaEdge
	for _, t := range w.s.Types {
		if t == nil {
			continue
		}
		for _, p := range t.Props {
			if p.Kind != KindRelation {
				continue
			}
			toName := p.Target
			if dst := w.types[p.Target]; dst != nil {
				toName = dst.Name
			}
			out = append(out, SchemaEdge{
				FromType: t.Key,
				FromName: t.Name,
				PropKey:  p.Key,
				PropName: p.Name,
				ToType:   p.Target,
				ToName:   toName,
				Many:     p.Many,
				Back:     p.Back,
			})
		}
	}
	return out
}

// BacklinkProps returns every relation edge in the space whose target is targetType.
func (w *World) BacklinkProps(targetType string) []SchemaEdge {
	if targetType == "" {
		return nil
	}
	var out []SchemaEdge
	for _, edge := range w.SchemaEdges() {
		if edge.ToType == targetType {
			out = append(out, edge)
		}
	}
	return out
}

// Summary computes space-wide type, entity, lifecycle phase and relation counts in one pass.
func (w *World) Summary() SpaceSummary {
	if w == nil || w.s == nil {
		return SpaceSummary{}
	}
	sum := SpaceSummary{Types: len(w.s.Types)}
	for _, e := range w.s.Entities {
		if e == nil {
			continue
		}
		sum.Total++
		sum.Relations += len(w.out[e.ID])
		t := w.typeOf(e)
		if t == nil || len(t.Flow) == 0 {
			continue
		}
		st, ok := t.Stage(e.Status)
		if !ok {
			continue
		}
		switch st.Phase {
		case Todo:
			sum.Todo++
		case Doing:
			sum.Doing++
		case Done:
			sum.Done++
		case Dropped:
			sum.Dropped++
		}
	}
	if den := sum.Todo + sum.Doing + sum.Done; den > 0 {
		pct := float64(sum.Done) / float64(den)
		sum.PercentDone = &pct
	}
	return sum
}

// Upcoming finds entities with a date property falling today through horizonDays ahead, plus open entities with a
// lifecycle whose date passed within the last overdueWindow days. Entities in phase done or dropped are excluded.
// Each entity appears at most once, under its earliest qualifying date. Results are ordered earliest date first.
func (w *World) Upcoming(today string, horizonDays int) []DueItem {
	if w == nil || w.s == nil || horizonDays < 0 {
		return nil
	}
	base, err := time.Parse(time.DateOnly, today)
	if err != nil {
		return nil
	}
	var out []DueItem
	for _, e := range w.s.Entities {
		if e == nil {
			continue
		}
		t := w.typeOf(e)
		if t == nil {
			continue
		}
		hasFlow := len(t.Flow) > 0
		if hasFlow {
			st, ok := t.Stage(e.Status)
			if !ok || st.Phase == Done || st.Phase == Dropped {
				continue
			}
		}
		var best *DueItem
		for i := range t.Props {
			p := t.Props[i]
			if p.Kind != KindDate {
				continue
			}
			raw, ok := e.Values[p.Key].(string)
			if !ok || raw == "" {
				continue
			}
			d, err := time.Parse(time.DateOnly, raw)
			if err != nil {
				continue
			}
			days := int(d.Sub(base) / (24 * time.Hour))
			if days > horizonDays {
				continue
			}
			if days < 0 && (!hasFlow || days < -overdueWindow) {
				continue
			}
			cand := DueItem{Entity: e, Type: t, Prop: p, Date: raw, Days: days}
			if best == nil || cand.Days < best.Days {
				best = &cand
			}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	slices.SortStableFunc(out, func(a, b DueItem) int {
		if c := cmp.Compare(a.Days, b.Days); c != 0 {
			return c
		}
		if c := b.Entity.Updated.Compare(a.Entity.Updated); c != 0 {
			return c
		}
		return cmp.Compare(a.Entity.ID, b.Entity.ID)
	})
	return out
}
