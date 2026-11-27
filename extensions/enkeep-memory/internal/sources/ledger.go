package sources

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona/sdk/go/wire"
)

// ledgerEntries reads the hub's structured assistant ledger (S12, assistant.entries.read) for day, groups active
// entries by origin space ("<day>" for the main space, "<space>/<day>" for another space so privacy tagging follows
// the space), deduplicates by entry id, and skips groups whose content hash matches the committed cursor.
func (s *Source) ledgerEntries(ctx context.Context, day string, since, end time.Time) []wire.KBEntry {
	lr, ok := s.Hub.(hubapi.LedgerReader)
	if !ok || lr == nil {
		s.note(entry.SrcLedger, KindLedger, hubapi.ErrNotConnected)
		return nil
	}
	start, boundEnd, err := s.bounds(day)
	if err != nil {
		return nil
	}
	if !end.IsZero() {
		boundEnd = end
	}
	es, err := lr.Ledger(ctx, hubapi.LedgerQuery{})
	s.note(entry.SrcLedger, KindLedger, err)
	if err != nil {
		s.logf("assistant ledger not read: %v", err)
		return nil
	}
	raw, _ := json.Marshal(es)
	s.addBytes(entry.SrcLedger, KindLedger, len(raw))
	if len(es) == 0 {
		s.empty(entry.SrcLedger, KindLedger)
		return nil
	}
	seenID := map[string]bool{}
	bySpace := map[string][]hubapi.LedgerEntry{}
	for _, e := range es {
		if e.ID == "" || seenID[e.ID] || e.Superseded() || e.By == "enkeep" {
			continue
		}
		if e.At.IsZero() || e.At.Before(start) || !e.At.Before(boundEnd) {
			continue
		}
		seenID[e.ID] = true
		space := strings.TrimSpace(e.OriginSpace)
		bySpace[space] = append(bySpace[space], e)
	}
	if len(bySpace) == 0 {
		return nil
	}
	spaces := make([]string, 0, len(bySpace))
	for sp := range bySpace {
		spaces = append(spaces, sp)
	}
	sort.Strings(spaces)

	var out []wire.KBEntry
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range spaces {
		group := bySpace[sp]
		sort.SliceStable(group, func(i, j int) bool {
			if !group[i].At.Equal(group[j].At) {
				return group[i].At.Before(group[j].At)
			}
			return group[i].ID < group[j].ID
		})
		id := ledgerUnitID(sp, day)
		h := ledgerHash(group)
		if s.cursors[ledgerCursorKey(id)] == h {
			continue
		}
		s.ledgers[id] = group
		s.pending[ledgerCursorKey(id)] = h
		updated := group[len(group)-1].At
		if updated.IsZero() {
			updated = since
		}
		out = append(out, wire.KBEntry{
			Kind:    KindLedger,
			ID:      id,
			Name:    ledgerUnitName(sp, day),
			Updated: updated,
			Source:  "persona",
		})
	}
	return out
}

func ledgerUnitID(space, day string) string {
	if space == "" {
		return day
	}
	return space + "/" + day
}

func ledgerUnitSpace(id string) string {
	sp, _, ok := strings.Cut(id, "/")
	if !ok {
		return ""
	}
	return sp
}

func ledgerUnitName(space, day string) string {
	if space == "" {
		return "台账 · " + day
	}
	return "台账 · " + space + " · " + day
}

func ledgerCursorKey(id string) string { return entry.SrcLedger + "/" + id }

func ledgerHash(es []hubapi.LedgerEntry) string {
	raw, _ := json.Marshal(es)
	return hubapi.Hash(string(raw))
}

// ledgerDay renders a day's ledger entries as high-evidence lines ("HH:MM speaker: text"). Entries stated or relayed
// by the user carry SpeakerUser so downstream speaker attribution treats them as the user's own high-evidence input;
// agent/import entries carry SpeakerLedger.
func (s *Source) ledgerDay(id, day string) []string {
	s.mu.Lock()
	es := s.ledgers[id]
	s.mu.Unlock()
	if len(es) == 0 {
		return nil
	}
	var out []string
	bytes := 0
	for _, e := range es {
		line := formatLedgerLine(e, s.Loc)
		if line == "" {
			continue
		}
		out = append(out, line)
		bytes += len(line)
	}
	if len(out) > 0 {
		s.count(entry.SrcLedger, KindLedger, KindLedger+"/"+id, day, len(out), bytes)
	}
	return out
}

func formatLedgerLine(e hubapi.LedgerEntry, loc *time.Location) string {
	text := oneLine(clip(strings.TrimSpace(e.Text), 600))
	quote := oneLine(clip(strings.TrimSpace(e.Quote), 400))
	if text == "" && quote == "" {
		return ""
	}
	speaker := SpeakerLedger
	if e.By == "user" || e.Trust == "user_verbatim" || e.Trust == "user_relayed" {
		speaker = SpeakerUser
	}
	var b strings.Builder
	if label := ledgerTypeLabel(e.Type); label != "" {
		b.WriteString("[" + label + "] ")
	}
	if text != "" {
		b.WriteString(text)
	} else {
		b.WriteString(quote)
	}
	if d := e.Decision; d != nil {
		var parts []string
		if len(d.Selected) > 0 {
			parts = append(parts, "选定："+oneLine(clip(strings.Join(d.Selected, " / "), 200)))
		}
		if len(d.Rejected) > 0 {
			parts = append(parts, "否决："+oneLine(clip(strings.Join(d.Rejected, " / "), 200)))
		}
		if r := oneLine(clip(strings.TrimSpace(d.Reason), 240)); r != "" {
			parts = append(parts, "理由："+r)
		}
		if len(parts) > 0 {
			b.WriteString("；" + strings.Join(parts, "；"))
		}
	}
	if quote != "" && text != "" && quote != text {
		b.WriteString("；原话：「" + quote + "」")
	}
	if len(e.Scope.Project) > 0 {
		b.WriteString("（项目 " + oneLine(strings.Join(e.Scope.Project, ", ")) + "）")
	}
	return hhmm(e.At, loc) + " " + speaker + ": " + b.String()
}

func ledgerTypeLabel(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "rule":
		return "规则"
	case "decision":
		return "决定"
	case "preference":
		return "偏好"
	case "fact":
		return "事实"
	}
	return strings.TrimSpace(t)
}
