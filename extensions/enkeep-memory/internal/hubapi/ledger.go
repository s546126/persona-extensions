package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The hub's entry ledger (unified memory design v2 §5.1, appendix D interface I1, persona #13106): one append-only
// list of entries the user said, relayed or confirmed, and what agents and extensions published.
//
// The extension reads it with the RPC assistant.entries.list (grant assistant.entries.read): params {limit ≤200,
// after, by, trust, status, scope, tag, type}, answering {entries, after, has_more} in ascending publication time.
// The legacy names cursor and created_at are refused by the hub. A hub without the method, an extension without the
// grant (permission_denied with data {scope}), and an extension the hub cannot prove runs on the assistant's
// computer (it gets no personal entries: on a hub in a container, every by=user entry about the user is personal)
// all read as "nothing to verify by": the callers go on with the day's own records.
//
// The grant and the event are not declared in the manifest yet: the manifest schema this repository syncs from the
// SDK (sdk/go/wire/schema/manifest.schema.json) does not list them until persona #13106 is merged and synced, and a
// hub without the ledger refuses a manifest naming them (extension/perm.go, EventTopics). Until then the call answers
// permission_denied, the same "nothing" as above; the event handler is ready for when it is subscribed.

// LedgerMethod and LedgerScope are the RPC and the grant it needs.
const (
	LedgerMethod = "assistant.entries.list"
	LedgerScope  = "assistant.entries.read"
	// LedgerChanged is the event an owner's "wrong" mark sends ({id, record_id, status: "wrong"}).
	LedgerChanged = "assistant.entries.changed"
)

// LedgerSource is where an entry's quote was said.
type LedgerSource struct {
	Machine string `json:"machine,omitempty"`
	Session string `json:"session,omitempty"`
	MsgUUID string `json:"msg_uuid,omitempty"`
	CardID  string `json:"card_id,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// LedgerScopeOf is an entry's scope: all specified dimensions must match.
type LedgerScopeOf struct {
	Executor string   `json:"executor,omitempty"` // system | assistant | any
	Machine  []string `json:"machine,omitempty"`
	Project  []string `json:"project,omitempty"`
	Personal bool     `json:"personal,omitempty"`
}

// LedgerDecision is the structured choice on a decision ledger entry.
type LedgerDecision struct {
	Options    []string        `json:"options,omitempty"`
	Selected   []string        `json:"selected,omitempty"`
	Rejected   []string        `json:"rejected,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Reversible json.RawMessage `json:"reversible,omitempty"`
}

// LedgerEntry is one ledger entry as assistant.entries.list returns it.
type LedgerEntry struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // fact | preference | rule | decision
	Text  string `json:"text"`
	Quote string `json:"quote,omitempty"`
	// Source is where Quote was said; RelayedVia the relay sessions it came through ([] when said directly).
	Source     LedgerSource `json:"source"`
	RelayedVia []string     `json:"relayed_via"`
	At         time.Time    `json:"at"`
	ExpiresAt  *time.Time   `json:"expires_at"`
	// Supersedes is set on the newer entry ("" when none); the older one reads status superseded.
	Supersedes  string          `json:"supersedes"`
	Trust       string          `json:"trust"`  // user_verbatim | user_relayed | repo | agent | inferred
	Status      string          `json:"status"` // active | superseded | expired | wrong
	By          string          `json:"by"`     // user | agent | enkeep | import
	Tags        []string        `json:"tag"`    // reserved: seed, import, probe
	Scope       LedgerScopeOf   `json:"scope"`
	Decision    *LedgerDecision `json:"decision,omitempty"`
	ExtID       string          `json:"ext_id,omitempty"`
	OriginSpace string          `json:"origin_space,omitempty"`
	Category    []string        `json:"category,omitempty"`
	// replaced is set by ledgerClean when the list holds a newer entry superseding this one.
	replaced bool
}

// Superseded says whether the entry no longer holds because a newer one replaced it.
func (e LedgerEntry) Superseded() bool { return e.replaced || e.Status == "superseded" }

// Expired says whether the entry no longer holds at now.
func (e LedgerEntry) Expired(now time.Time) bool {
	return e.Status == "expired" || e.ExpiresAt != nil && !now.Before(*e.ExpiresAt)
}

// Probe says whether the entry is a test probe (never read as the user's).
func (e LedgerEntry) Probe() bool { return slices.Contains(e.Tags, "probe") }

// LedgerQuery selects ledger entries.
type LedgerQuery struct {
	By     string // user | agent | enkeep | import; "" all
	Status string // "" (the hub's default: everything but expired) | active | wrong | all …
	Tag    string
}

// LedgerReader reads the ledger. It is a separate interface, not part of Hub: a Hub that cannot read it (None,
// Offline, the tests' fakes) simply does not have the method.
type LedgerReader interface {
	Ledger(ctx context.Context, q LedgerQuery) ([]LedgerEntry, error)
}

// LedgerDenied is the hub's answer to an extension without the grant: the scope it needs.
type LedgerDenied struct{ Scope string }

func (d *LedgerDenied) Error() string { return "the ledger needs the grant " + d.Scope }

// ledgerPage is the page size (the hub's maximum); ledgerMost bounds one read.
const (
	ledgerPage = 200
	ledgerMost = 20000
)

// Ledger reads every page (up to ledgerMost) of assistant.entries.list, marks the entries a newer one supersedes, and
// leaves out probes, expired and wrong entries. Errors: *WaitingHub{"I1"} for a hub without the method,
// *LedgerDenied without the grant; no personal entry visible is an empty list, not an error.
func (h *HTTP) Ledger(ctx context.Context, q LedgerQuery) ([]LedgerEntry, error) {
	if h.Call == nil {
		return nil, &WaitingHub{Change: "I1"}
	}
	var all []LedgerEntry
	after := ""
	for len(all) < ledgerMost {
		params := map[string]any{"limit": ledgerPage}
		if q.By != "" {
			params["by"] = q.By
		}
		if q.Status != "" {
			params["status"] = q.Status
		}
		if q.Tag != "" {
			params["tag"] = q.Tag
		}
		if after != "" {
			params["after"] = after
		}
		var r struct {
			Entries []LedgerEntry `json:"entries"`
			After   string        `json:"after"`
			HasMore bool          `json:"has_more"`
		}
		if err := h.Call(ctx, LedgerMethod, params, &r); err != nil {
			return nil, ledgerErr(err)
		}
		all = append(all, r.Entries...)
		if !r.HasMore || r.After == "" || len(r.Entries) == 0 {
			break
		}
		after = r.After
	}
	return ledgerClean(all, time.Now()), nil
}

// ledgerErr maps assistant.entries.list's errors by their code.
func ledgerErr(err error) error {
	var coded *ext.Error
	code, scope := "", LedgerScope
	if errors.As(err, &coded) {
		code = coded.Code
		var d struct {
			Scope string `json:"scope"`
		}
		if len(coded.Data) > 0 && json.Unmarshal(coded.Data, &d) == nil && d.Scope != "" {
			scope = d.Scope
		}
	} else {
		for _, c := range []string{"unknown_method", "method_not_found", "permission_denied"} {
			if strings.Contains(err.Error(), c) {
				code = c
			}
		}
	}
	switch code {
	case "unknown_method", "method_not_found":
		return &WaitingHub{Change: "I1"}
	case "permission_denied":
		return &LedgerDenied{Scope: scope}
	}
	return err
}

// ledgerClean marks superseded entries and drops probes, expired and wrong entries.
func ledgerClean(all []LedgerEntry, now time.Time) []LedgerEntry {
	var out []LedgerEntry
	replaced := map[string]bool{}
	for _, e := range all {
		if e.Probe() || e.Expired(now) || e.Status == "wrong" {
			continue // and it replaces nothing
		}
		if e.Supersedes != "" {
			replaced[e.Supersedes] = true
		}
		out = append(out, e)
	}
	for i := range out {
		out[i].replaced = replaced[out[i].ID]
	}
	return out
}

var _ LedgerReader = (*HTTP)(nil)

// LedgerCleanForTest is ledgerClean at the current time, for other packages' tests.
func LedgerCleanForTest(all []LedgerEntry) []LedgerEntry { return ledgerClean(all, time.Now()) }
