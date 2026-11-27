package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/relay"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona/sdk/go/wire"
)

// agentEntries lists the agent sessions with activity on the day (S3): not the assistant's own (its words are S1,
// through convsrc); every space's (0.2.0-beta.40), not one started after the day, nor before its first item.
//
// Three lists, together and each session once (by machine and session id). The hub's live list is what the runtimes
// list, which may be only their recent sessions (enkeep: active in the last 7 days, at most 80). The hub's session
// history (nwverse/persona#11529, 0.2.0-beta.34) has the sessions active from the day's start on, archived, finished
// and offline ones too; scheduled runs are left out. A hub without it, or a source that did not list the window in
// full, is recorded (State.SessionHistory, shown by enkeep_rerun_status), and those sessions are read as far as the
// other two lists have them. The sessions a pass found before are read up to the last day they were known to be
// active (State.SeenSessions): the hub forwards their items to the runtime whether it lists them or not.
//
// Every pass asks the history, the daily one too (decided as recommended): a session that started and was archived
// within the day is in no live list by the next morning, and one request a day costs little.
func (s *Source) agentEntries(ctx context.Context, since, end time.Time) []wire.KBEntry {
	ss, err := s.Hub.Sessions(ctx, since)
	s.note(entry.SrcAgent, KindAgents, err)
	if err != nil {
		s.logf("agent sessions not read: %v", err)
		return nil
	}
	hist := s.history(ctx, since)
	day := since.In(s.Loc).Format("2006-01-02")
	var out []wire.KBEntry
	s.mu.Lock()
	defer s.mu.Unlock()
	listed := map[string]bool{}
	add := func(id string, se hubapi.Session, updated time.Time) {
		if f := s.seen[id].First; f != "" && day < f {
			return // nothing of it before its first item
		}
		s.sessions[id] = se
		out = append(out, wire.KBEntry{Kind: KindAgents, ID: id, Name: sessionName(se, s.Machine), Updated: updated, Source: "persona"})
	}
	for _, list := range [][]hubapi.Session{ss, hist} {
		for _, se := range list {
			id := se.Machine + "/" + se.ID
			if listed[id] {
				continue
			}
			listed[id] = true
			if se.Assistant || (!se.Started.IsZero() && !se.Started.Before(end)) {
				continue
			}
			s.listed[id] = seenOf(se, s.Loc)
			add(id, se, se.Updated)
		}
	}
	for _, se := range hist {
		if id := se.Machine + "/" + se.ID; !slices.ContainsFunc(ss, func(x hubapi.Session) bool { return x.Machine+"/"+x.ID == id }) {
			s.fromHist[id] = true // its items are readable for a day after the history listed it (persona#11529)
		}
	}
	ids := make([]string, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		k := s.seen[id]
		if listed[id] || k.Last == "" || day > k.Last || (!k.Started.IsZero() && !k.Started.Before(end)) {
			continue
		}
		machine, session, _ := strings.Cut(id, "/")
		se := hubapi.Session{Machine: machine, ID: session, Title: k.Title, Agent: k.Agent, Project: k.Project, Started: k.Started,
			Updated: k.Updated, Ended: k.Ended, ByPersona: k.ByPersona}
		updated := k.Updated
		if updated.IsZero() {
			updated = since
		}
		add(id, se, updated)
	}
	return out
}

// history is the hub's session history from since to now (nil when the hub has none or it failed), with how fully
// it was listed kept for End.
func (s *Source) history(ctx context.Context, since time.Time) []hubapi.Session {
	hh, ok := s.Hub.(hubapi.HistoryHub)
	read := &store.HistoryRead{At: time.Now().UTC().Format(time.RFC3339)}
	var out []hubapi.Session
	switch {
	case !ok:
		read.Why = "this hub client has no session history"
	default:
		h, err := hh.History(ctx, since, time.Time{})
		switch {
		case err != nil:
			read.Why = "session history not read: " + err.Error()
			s.logf("agent session history not read: %v", err)
		case !h.Offered:
			read.Why = h.Why
		default:
			read.Offered = true
			for _, g := range h.Gaps {
				read.Gaps = append(read.Gaps, store.HistoryGap(g))
			}
			if !h.Complete && len(read.Gaps) == 0 { // the hub said incomplete without naming a source
				read.Gaps = append(read.Gaps, store.HistoryGap{Honored: true, Reason: "incomplete"})
			}
			out = h.Sessions
		}
	}
	s.mu.Lock()
	s.hist, s.histAt = read, since
	s.mu.Unlock()
	return out
}

// relist asks the history again, for a session it listed whose items the hub no longer lets the extension read (the
// hub keeps that for a day, in memory: a restart forgets it). It says whether it was asked.
func (s *Source) relist(ctx context.Context, id string) bool {
	s.mu.Lock()
	again, since := s.fromHist[id], s.histAt
	s.mu.Unlock()
	hh, ok := s.Hub.(hubapi.HistoryHub)
	if !again || !ok {
		return false
	}
	_, err := hh.History(ctx, since, time.Time{})
	return err == nil
}

// seenOf is what State.SeenSessions keeps of a listed session.
func seenOf(se hubapi.Session, loc *time.Location) store.SeenSession {
	last := ""
	if !se.Updated.IsZero() {
		last = se.Updated.In(loc).Format("2006-01-02")
	}
	return store.SeenSession{Title: se.Title, Agent: se.Agent, Project: se.Project, Started: se.Started, Updated: se.Updated,
		Ended: se.Ended, ByPersona: se.ByPersona, Last: last}
}

// cursorKey is the State.Cursors key of an agent session.
func cursorKey(id string) string { return entry.SrcAgent + "/" + id }

// folded is a session's folded page from the last seq read on: read once per pass (fold_work=1, after_seq), kept
// for the days after. A day before the one the cursor was read through (a rerun) reads the page from the start.
func (s *Source) folded(ctx context.Context, machine, session, day string) (*foldPage, error) {
	id := machine + "/" + session
	s.mu.Lock()
	page := s.folds[id]
	seq, through := cursorOf(s.cursors[cursorKey(id)])
	s.mu.Unlock()
	after := seq
	if through != "" && day <= through {
		after = 0 // read before: the items of that day are behind the cursor
	}
	if page != nil && page.base <= after {
		return page, nil
	}
	items, err := s.Hub.Items(ctx, machine, session, hubapi.ItemsQuery{Fold: true, After: after})
	if err != nil && s.relist(ctx, id) { // a history row the hub forgot it gave: list it again, read once more
		items, err = s.Hub.Items(ctx, machine, session, hubapi.ItemsQuery{Fold: true, After: after})
	}
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(items)
	s.addBytes(entry.SrcAgent, KindAgents, len(raw))
	for _, it := range items {
		if it.Kind == hubapi.KindFold && it.Fold != nil && !it.Fold.Known {
			s.partial(entry.SrcAgent, KindAgents, "H3", "折叠的工作里还没有报错数和工具名（等 hub 的 H3），不顺信号只能靠文字和卡片")
			break
		}
	}
	page = &foldPage{base: after, items: items}
	s.mu.Lock()
	s.folds[id] = page
	s.mu.Unlock()
	return page, nil
}

// agentDay reads an agent session's day: the user's words ("我"), the agent's (head and tail), what Persona sent it,
// the session's cards with their answers, one line per stretch of work, and how the session ended.
func (s *Source) agentDay(ctx context.Context, id, day string) ([]string, error) {
	machine, session, ok := strings.Cut(id, "/")
	if !ok {
		return nil, nil
	}
	start, end, err := s.bounds(day)
	if err != nil {
		return nil, err
	}
	page, err := s.folded(ctx, machine, session, day)
	if err != nil {
		s.note(entry.SrcAgent, KindAgents, err)
		return nil, nil // a session that cannot be read now is left out, not the day
	}
	if page.base == 0 { // read from the start: its first item dates the session (State.SeenSessions' First)
		first := ""
		for _, it := range page.items {
			if d := it.At.In(s.Loc).Format("2006-01-02"); !it.At.IsZero() && (first == "" || d < first) {
				first = d
			}
		}
		if first != "" {
			s.mu.Lock()
			s.first[id] = first
			s.mu.Unlock()
		}
	}
	type line struct {
		at   time.Time
		text string
	}
	s.mu.Lock()
	se := s.sessions[id]
	s.mu.Unlock()
	var ls []line
	last := -1
	for i, it := range page.items {
		if it.At.Before(start) || !it.At.Before(end) {
			continue
		}
		if it.Seq > last {
			last = it.Seq
		}
		if it.Fold != nil && it.Fold.To > last {
			last = it.Fold.To // Fold.Seq is only the range's start: the next page must resume after Fold.To.
		}
		byPersona := se.ByPersona && (page.base == 0 && i == 0 || entry.On(s.switches, entry.K20PersonaSession))
		if t := s.itemLine(it, byPersona); t != "" {
			ls = append(ls, line{it.At, t})
		}
		// The user's words a task prompt or a relay says it carries: each as a line of its own, kept only when the
		// day's own lines bear it out (curate's verifyRelays); otherwise dropped, never taken on the relay's word.
		for _, q := range relayQuotes(it, byPersona) {
			ls = append(ls, line{it.At, hhmm(it.At, s.Loc) + " " + SpeakerRelayCand + ": " + oneLine(q)})
		}
	}
	for _, c := range s.dayCards(ctx, day) {
		if c.Session == session && (c.Machine == "" || c.Machine == machine) {
			for _, l := range cardLines(c, s.Loc) {
				ls = append(ls, line{c.At, l})
			}
		}
	}
	if se.Ended != "" && !se.Updated.Before(start) && se.Updated.Before(end) {
		ls = append(ls, line{se.Updated, hhmm(se.Updated, s.Loc) + " " + SpeakerEnd + ": " + endText(se.Ended)})
	}
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].at.Before(ls[j].at) })
	var out []string
	for _, l := range ls {
		out = append(out, l.text)
	}
	if last >= 0 {
		s.mu.Lock()
		s.pending[cursorKey(id)] = fmt.Sprintf("%d|%s", last, day)
		s.mu.Unlock()
	}
	if len(out) > 0 { // the bytes were counted when the folded page was read
		s.count(entry.SrcAgent, KindAgents, KindAgents+"/"+id, day, len(out), 0)
	}
	return out, nil
}

// itemLine is one item as a day's line, or "" for what is not read daily (slash commands, notifications).
// byPersona: the session is one Persona started (a task, a relay) and the item is a user-role text Persona typed: its
// first item, or every user-role item while K20 is on (0.2.0-beta.37: a supervisor's or the assistant's later
// messages are not the user's words either). Otherwise a user-role text is still not the user's when it carries a
// task prompt's, a relay's or the hub's marks, or is a test prompt (internal/relay); pasted blocks are material.
func (s *Source) itemLine(it hubapi.Item, byPersona bool) string {
	at := hhmm(it.At, s.Loc)
	switch it.Kind {
	case hubapi.KindUser:
		if byPersona {
			clean := relay.StripBoilerplate(it.Text)
			if clean == "" {
				return ""
			}
			return at + " " + SpeakerPersona + ": " + oneLine(clip(clean, noticeCut))
		}
		switch relay.Classify(it.Text) {
		case relay.Persona:
			clean := relay.StripBoilerplate(it.Text)
			if clean == "" {
				return ""
			}
			return at + " " + SpeakerPersona + ": " + oneLine(clip(clean, noticeCut))
		case relay.Test:
			return at + " " + SpeakerTest + ": " + oneLine(clip(it.Text, noticeCut))
		}
		own, n := relay.SplitPaste(it.Text)
		if own == "" && n > 0 {
			return at + " " + SpeakerPaste + ": " + oneLine(clip(it.Text, noticeCut))
		}
		own = relay.StripBoilerplate(own)
		if own == "" {
			if n > 0 {
				return at + " " + SpeakerPaste + ": " + oneLine(clip(it.Text, noticeCut))
			}
			return ""
		}
		if n > 0 {
			own += " " + relay.PasteMark(n)
		}
		return at + " " + SpeakerUser + ": " + oneLine(own)
	case hubapi.KindAgent:
		return at + " " + SpeakerAgent + ": " + oneLine(headTail(it.Text, agentHead, agentTail))
	case hubapi.KindFold:
		if it.Fold == nil || it.Fold.Calls == 0 {
			return "" // thoughts only: nothing was done
		}
		return at + " " + SpeakerWork + ": " + foldText(it.Fold)
	case hubapi.KindNotice:
		switch {
		case it.Notice == "persona" || strings.HasPrefix(it.Notice, "injected:") || strings.HasPrefix(it.Notice, "source:"):
			clean := relay.StripBoilerplate(it.Text)
			if clean == "" {
				return ""
			}
			return at + " " + SpeakerPersona + ": " + oneLine(clip(clean, noticeCut))
		case it.Notice == "paste":
			return at + " " + SpeakerPaste + ": " + oneLine(clip(it.Text, noticeCut))
		case it.Notice == "peer" || strings.HasPrefix(it.Notice, "from:"):
			return at + " " + SpeakerOther + ": " + oneLine(clip(it.Text, noticeCut))
		}
	}
	return ""
}

// relayQuotes are the user's words a Persona-typed item claims to carry (relay.Quotes): a user-role text Persona
// typed (byPersona, or with a task prompt's or a relay's marks) and the hub's Persona notices. Before 0.2.0-beta.38
// such a text was Persona's as a whole (K20), and the user's real words relayed in it were lost with it.
func relayQuotes(it hubapi.Item, byPersona bool) []string {
	switch it.Kind {
	case hubapi.KindUser:
		if byPersona || relay.Classify(it.Text) == relay.Persona {
			return relay.Quotes(it.Text)
		}
	case hubapi.KindNotice:
		if it.Notice == "persona" || strings.HasPrefix(it.Notice, "injected:") || strings.HasPrefix(it.Notice, "source:") {
			return relay.Quotes(it.Text)
		}
	}
	return nil
}

// foldText is a stretch of work in one line: calls, tool names and errors (when the hub gives them, H3; otherwise
// said to be unknown, never "0"), the files changed, and the seq range a dig can open.
func foldText(f *hubapi.Fold) string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 次工具调用", f.Calls)
	if f.Known {
		if len(f.Tools) > 0 {
			fmt.Fprintf(&b, "（%s）", strings.Join(f.Tools, "、"))
		}
		fmt.Fprintf(&b, "，报错 %d 次", f.Errors)
	} else {
		b.WriteString("，报错数未知")
	}
	if len(f.Files) > 0 {
		files := f.Files
		more := ""
		if len(files) > 6 {
			more = fmt.Sprintf(" 等 %d 个", len(files))
			files = files[:6]
		}
		var short []string
		for _, p := range files {
			short = append(short, shortPath(p))
		}
		fmt.Fprintf(&b, "，改了 %s%s", strings.Join(short, "、"), more)
	}
	fmt.Fprintf(&b, " [seq %d-%d]", f.From, f.To)
	return b.String()
}

// shortPath keeps a path's last two parts: enough to say what was changed, without the home directory.
func shortPath(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

func endText(e string) string {
	switch e {
	case "interrupted":
		return "会话被打断"
	case "timeout":
		return "会话超时"
	case "normal":
		return "会话正常结束"
	}
	return e
}
