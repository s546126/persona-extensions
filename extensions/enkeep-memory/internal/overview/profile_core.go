package overview

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

//go:embed profile_core.md
var profileCorePrompt string

//go:embed profile_core.schema.json
var profileCoreSchema []byte

const (
	profileCoreCandMost  = 40
	profileCoreMaxItems  = 6
	profileCoreMaxTokens = 4000
)

type (
	profileCoreItemInput struct {
		ID        string   `json:"id"`
		Type      string   `json:"type,omitempty"`
		Text      string   `json:"text"`
		UserOwned bool     `json:"user_owned,omitempty"`
		Weeks     int      `json:"weeks"`
		Scenes    int      `json:"scenes"`
		Quotes    []string `json:"quotes,omitempty"`
	}
	profileCoreExistingInput struct {
		ID   string `json:"id"`
		Kind string `json:"kind,omitempty"`
		Text string `json:"text"`
	}
	profileCoreInput struct {
		Lang     string                     `json:"language"`
		Items    []profileCoreItemInput     `json:"items"`
		Existing []profileCoreExistingInput `json:"existing,omitempty"`
	}
	profileCoreProposal struct {
		Extends string   `json:"extends"`
		Kind    string   `json:"kind"` // identity | constraint | boundary
		Text    string   `json:"text"`
		Members []string `json:"members"`
		Reason  string   `json:"reason"`
	}
	profileCoreOutput struct {
		Items []profileCoreProposal `json:"items"`
	}
)

// profileCoreCandidates selects T0 (SelfReport / FromOnboarding / Pinned / user-edited) and T1 multi-week cross-scene
// stable Persona entries (v0.4.0 §1.3 A1). Strictly excludes S3/S4-only entries, S6 assistant notes, sensitive
// entries, restricted spaces, ephemeral entries, and ops runbooks.
func profileCoreCandidates(st *store.State, cutoff string) []candidate {
	var out []candidate
	for _, e := range st.Entries {
		if e == nil || !e.Live() || e.Facet != entry.Persona {
			continue
		}
		if e.Inferred || entry.IsAssistantNote(e.Origin) || isS3S4Only(e) || entry.IsEphemeral(e) || entry.IsOpsRunbookEntry(e) {
			continue
		}
		if e.OriginSpace != "" || len(e.Cats()) > 0 || len(entry.PayloadCategories(e.Facet, e.PayloadTexts()...)) > 0 {
			continue
		}
		if cutoff != "" && e.First >= cutoff {
			continue
		}
		if e.Grain == entry.Action {
			if p := st.Entries[e.Parent]; p != nil && p.Live() {
				continue
			}
		}
		owned := isUserOwned(e)
		if !owned && e.Status != entry.Stable {
			continue
		}
		if !usable(st, e) && !owned {
			continue
		}
		t := count(st, []string{e.ID}, cutoff)
		if !owned {
			if len(t.weeks) < MinWeeks || !t.crossScene() {
				continue
			}
		}
		out = append(out, candidate{e: e, t: t})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ao, bo := isUserOwned(a.e), isUserOwned(b.e)
		if ao != bo {
			return ao
		}
		if sa, sb := a.t.sceneCount(), b.t.sceneCount(); sa != sb {
			return sa > sb
		}
		if len(a.t.weeks) != len(b.t.weeks) {
			return len(a.t.weeks) > len(b.t.weeks)
		}
		if a.e.Count != b.e.Count {
			return a.e.Count > b.e.Count
		}
		return a.e.ID < b.e.ID
	})
	if len(out) > profileCoreCandMost {
		out = out[:profileCoreCandMost]
	}
	return out
}

func profileCoreSignature(cands []candidate) string {
	h := sha256.New()
	for _, c := range cands {
		fmt.Fprintf(h, "%s@%s\n", c.e.ID, c.e.Updated)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SyncProfileCore synthesizes 4–6 A1 profile_core items (user identity/stage/constraints + anti-misread boundaries)
// in a single Sonnet call from T0 and multi-week cross-scene T1 Persona facts (v0.4.0 §1.3 A1).
func (r *Runner) SyncProfileCore(ctx context.Context, st *store.State, s *State, run *Run, cutoff, now string) ([]string, error) {
	cands := profileCoreCandidates(st, cutoff)
	run.Counts["profile_core_candidates"] = len(cands)
	if len(cands) == 0 {
		return nil, nil
	}
	sig := profileCoreSignature(cands)
	existing := s.Sorted(func(p *Pattern) bool { return p.IsProfileCore() && p.Lifecycle != Retired })
	if cutoff == "" && s.ProfileCoreSig == sig && len(existing) > 0 {
		run.Counts["profile_core_skipped_unchanged"]++
		return nil, nil
	}

	in := profileCoreInput{Lang: r.lang()}
	local := map[string]*entry.Entry{}
	for i, c := range cands {
		id := fmt.Sprintf("i%d", i+1)
		local[id] = c.e
		in.Items = append(in.Items, profileCoreItemInput{
			ID:        id,
			Type:      c.e.Type,
			Text:      clip(c.e.Shown(), 180),
			UserOwned: isUserOwned(c.e),
			Weeks:     len(c.t.weeks),
			Scenes:    c.t.sceneCount(),
			Quotes:    itemQuotes(st, c.e, cutoff, 2),
		})
	}
	localP := map[string]*Pattern{}
	for i, p := range existing {
		pid := fmt.Sprintf("p%d", i+1)
		localP[pid] = p
		in.Existing = append(in.Existing, profileCoreExistingInput{
			ID:   pid,
			Kind: p.RuleType,
			Text: clip(p.Text, 180),
		})
	}

	var out profileCoreOutput
	err := r.call(ctx, run, "profile_core", model.Medium, profileCorePrompt, profileCoreSchema, in, profileCoreMaxTokens, &out)
	if errors.Is(err, model.ErrRefused) {
		run.Counts["profile_core_refused"]++
		run.Notes = append(run.Notes, "profile_core: the model declined")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var touched []string
	kept := 0
	for _, prop := range out.Items {
		if kept >= profileCoreMaxItems {
			break
		}
		text := strings.TrimSpace(prop.Text)
		if text == "" || hasProperNounOrOps(text) {
			run.Counts["profile_core_dropped"]++
			continue
		}
		var members []string
		var covered []string
		for _, mid := range prop.Members {
			if e := local[mid]; e != nil && e.Live() {
				members = union(members, e.ID)
				if isUserOwned(e) {
					covered = union(covered, e.ID)
				}
			}
		}
		if len(members) == 0 {
			run.Counts["profile_core_dropped"]++
			continue
		}
		var p *Pattern
		if ext := localP[prop.Extends]; ext != nil && ext.IsProfileCore() {
			p = ext
		}
		if p == nil {
			id := patternID(KindProfileCore, "", "", prop.Kind+"|"+text)
			p = s.Patterns[id]
			if p == nil {
				p = &Pattern{ID: id, Facet: string(entry.Persona), Kind: KindProfileCore, Lifecycle: Core, Created: now}
				s.Patterns[id] = p
				run.Counts["profile_core_new"]++
			}
		}
		p.Kind = KindProfileCore
		p.Facet = string(entry.Persona)
		p.RuleType = strings.TrimSpace(prop.Kind)
		p.Text = text
		p.EvidenceIDs = union(p.EvidenceIDs, members...)
		if len(covered) > 0 {
			p.CoveredIDs = union(p.CoveredIDs, covered...)
			sort.Strings(p.CoveredIDs)
		}
		p.Reason = strings.TrimSpace(prop.Reason)
		p.UpdatedAt = now
		recount(st, p, cutoff)
		if p.Gate == "" && len(p.Categories) == 0 && p.OriginSpace == "" {
			p.Lifecycle = Core
			p.Rooted = true
			kept++
		} else {
			p.Lifecycle = Candidate
		}
		touched = union(touched, p.ID)
		run.Counts["profile_core_synced"]++
	}
	if cutoff == "" {
		s.ProfileCoreSig = sig
	}
	return touched, nil
}
