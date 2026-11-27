package sources

import (
	"fmt"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/convsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona/sdk/go/wire"
)

// recordSent tracks sent.read provenance returned on S8 kb.day responses (S13): availability (available /
// missing_scope / unavailable), deduplicated counts of sent-as-user messages matched to archived chats (by
// SentLedgerID so overlapping chats or retries never double-count), and a monotonic cursor of the highest ledger id
// observed.
func (s *Source) recordSent(chat, day string, r wire.KBDayResult) {
	if strings.HasPrefix(chat, convsrc.Prefix) {
		return
	}
	switch r.SentOrigin {
	case "available":
		s.note(entry.SrcSent, KindSent, nil)
	case "missing_scope":
		s.note(entry.SrcSent, KindSent, hubapi.ErrNoScope)
		return
	case "unavailable", "":
		s.mu.Lock()
		avail := s.statusOf(entry.SrcSent, KindSent).Available
		s.mu.Unlock()
		if !avail {
			s.note(entry.SrcSent, KindSent, &hubapi.WaitingHub{Change: "sent.read"})
		}
		return
	}

	var matched, bytes int
	var maxID int64
	s.mu.Lock()
	for _, m := range r.Messages {
		if m.SentBy == "" && m.SentLedgerID == 0 {
			continue
		}
		if m.SentLedgerID > 0 {
			if s.sentSeen[m.SentLedgerID] {
				continue
			}
			s.sentSeen[m.SentLedgerID] = true
			if m.SentLedgerID > maxID {
				maxID = m.SentLedgerID
			}
		} else {
			key := fmt.Sprintf("%s@%s#%d", chat, day, m.N)
			if s.sentNoID[key] {
				continue
			}
			s.sentNoID[key] = true
		}
		matched++
		bytes += len(m.Text)
	}
	if maxID > 0 {
		cur := fmt.Sprintf("%d|%s", maxID, day)
		if later(cur, s.pending[sentCursorKey]) {
			s.pending[sentCursorKey] = cur
		}
	}
	s.mu.Unlock()
	if matched > 0 {
		s.count(entry.SrcSent, KindSent, "chats/"+chat, day, matched, bytes)
	}
}

const sentCursorKey = entry.SrcSent + "/last_id"
