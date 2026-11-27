// Package overview is the periodic cross-event overview (docs/periodic-overview.md): the layer above the daily
// curation that asks, across days, projects, people and sources, which scattered entries are the same way of
// deciding, where that way holds and where it does not, and which ways are drifting or belonged to a phase that ended.
//
// It keeps its own file, overview-state.json beside state.json, and never changes the curated state: it reads a
// published snapshot (curate.Live) and writes only its patterns, runs and readings. Four layers:
//
//   - L1, weekly cross-event synthesis (Weekly): the week's classes and actions, projected by time, project, people
//     and source, synthesised into patterns facet by facet; each pattern searched for counter-examples and put
//     through a code gate (two scenes, three independent quoted pieces of evidence, two weeks) before it is used;
//   - L2, monthly consolidation and drift (Monthly): every pattern's lifecycle settled again from its evidence over
//     ninety days, overlapping patterns merged, quiet but high-count classes looked at again;
//   - L3, light refresh (Refresh): deterministic, no model, on every published snapshot: a pattern whose evidence
//     the user marked wrong or deleted is recounted and leaves the exits at once when it no longer passes;
//   - the weekly report's short summary (Summary), the full text staying on the plugin's page.
//
// The patterns reach the assistant through the hub's own memory source (Exits, memory.entries), graded by scene
// there: a pattern resting on any categorised or restricted-space evidence carries those categories and is never
// standing.
package overview

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
)

// File is the overview's state, in the extension's data directory.
const File = "overview-state.json"

// ReportDir holds the full weekly and monthly reports (weekly/<ISO week>.md, monthly/<YYYY-MM>.md).
const ReportDir = "overview-reports"

// Lifecycles of a pattern.
const (
	Candidate = "candidate"   // seen, not through the gate yet: shown on the page, not used
	Core      = "core_stable" // through the gate, no counter-example worth a boundary: used
	Scoped    = "scoped"      // through the gate, with an exception the counter-search drew: used, with it
	Drifting  = "drifting"    // recent counter-examples ≥ 25%: shown, not used, asked about in the report
	EraClosed = "era_closed"  // project-bound and quiet for 90 days: kept, not used
	Retired   = "retired"     // overturned, merged, or its evidence gone: kept for the record
)

// Perspectives: where a pattern's evidence differs most, the reason it is cross-event at all.
const (
	PerspTemporal    = "temporal"
	PerspProject     = "project"
	PerspPeople      = "people"
	PerspCrossSource = "cross_source"
)

// Kinds of L1 patterns (v0.4.0 §1.3): profile core (A1), cognitive principles (A2), universal collaboration
// contracts (B), and environment/tool ops runbooks (C).
const (
	KindProfileCore = "profile_core"
	KindPrinciple   = "principle"
	KindContract    = "contract"
	KindOpsRunbook  = "ops_runbook"
)

// Contract polarities (unified design §1.3 step C): action direction of a collaboration contract so downstream
// standing selection can balance autonomous execution against verification and communication.
const (
	PolarityAct         = "act"
	PolarityCheck       = "check"
	PolarityCommunicate = "communicate"
	PolarityRoute       = "route"
)

// Polarities lists the allowed contract polarity values.
var Polarities = []string{PolarityAct, PolarityCheck, PolarityCommunicate, PolarityRoute}

// Facets are the six the overview covers. People's cards (⑤′) feed World with their facts and commitments only;
// their evaluations and inferences never come in (privacy line, design 2.8).
var (
	Facets          = []entry.Facet{entry.Interaction, entry.Tools, entry.Judgement, entry.Workflow, entry.World, entry.Persona}
	ContractFacets  = []entry.Facet{entry.Interaction, entry.Tools}
	PrincipleFacets = []entry.Facet{entry.Judgement, entry.Workflow, entry.World, entry.Persona}
)

// Pattern is one cross-event L1 principle or collaboration contract.
type Pattern struct {
	ID          string `json:"id"` // ov:<10 hex>
	Facet       string `json:"facet"`
	Kind        string `json:"kind,omitempty"` // principle | contract (empty inferred from Facet)
	Perspective string `json:"perspective"`
	Lifecycle   string `json:"lifecycle"`
	Text        string `json:"text"`
	Situation   string `json:"situation,omitempty"`
	Choice      string `json:"choice,omitempty"`
	Rejected    string `json:"rejected,omitempty"`
	Exception   string `json:"exception,omitempty"`
	// Poles are both ends when the user goes both ways, each with when it holds (0.2.0-beta.52); Text says them in
	// place of the exception.
	Poles           []Pole   `json:"poles,omitempty"`
	DefaultContrast string   `json:"default_contrast,omitempty"`
	HoldsForAnyone  bool     `json:"holds_for_anyone,omitempty"`
	Dimension       string   `json:"dimension,omitempty"`       // principle cognitive dimension (decision/thinking/reaction/attention/communication)
	Polarity        string   `json:"polarity,omitempty"`        // contract polarity (act/check/communicate/route)
	RuleType        string   `json:"rule_type,omitempty"`       // contract occasion/type (e.g. correction, dispatch, report)
	Hard            bool     `json:"hard,omitempty"`            // contract: hard rule (corrected >=2 days within 7d or ban/anger)
	ViolationCount  int      `json:"violation_count,omitempty"` // contract: distinct days the user had to correct it
	CoveredIDs      []string `json:"covered_ids,omitempty"`     // contract: user-edited/handwritten entries covered (never deleted)
	Rooted          bool     `json:"rooted,omitempty"`          // core >=4 ISO weeks or hard contract: never auto-closed by silence
	EvidenceIDs     []string `json:"evidence_ids"`              // the curated entries it rests on (classes, actions)
	CounterIDs      []string `json:"counter_ids,omitempty"`     // entries the counter-search judged against it
	ScenesCount     int      `json:"scenes_count"`              // distinct projects and source kinds among the evidence
	SourcesCount    int      `json:"sources_count"`             // distinct quoted units·days among the evidence
	UniqueQuotes    int      `json:"unique_quotes,omitempty"`   // distinct normalised user quotes among the evidence
	ActiveWeeks     []string `json:"active_weeks,omitempty"`
	CounterRatio    float64  `json:"counter_ratio,omitempty"`
	Gate            string   `json:"gate,omitempty"`   // the first gate it fails, "" when through
	Reason          string   `json:"reason,omitempty"` // why the lifecycle (the counter-search's, the month's)
	Categories      []string `json:"categories,omitempty"`
	OriginSpace     string   `json:"origin_space,omitempty"`
	Subjects        []string `json:"subjects,omitempty"`
	LastEvidence    string   `json:"last_evidence,omitempty"` // YYYY-MM-DD
	DriftMonths     int      `json:"drift_months,omitempty"`  // consecutive monthly passes it stayed drifting
	Withdrawn       string   `json:"withdrawn,omitempty"`     // the lifecycle L3 took it out of when its evidence was withdrawn
	Created         string   `json:"created"`
	UpdatedAt       string   `json:"updated_at"`
}

// KindOf returns the pattern's L1 kind (profile_core, principle, contract, or ops_runbook), inferring from Facet for
// states written before Kind.
func (p *Pattern) KindOf() string {
	switch p.Kind {
	case KindProfileCore, KindPrinciple, KindContract, KindOpsRunbook:
		return p.Kind
	}
	if p.Facet == string(entry.Tools) {
		return KindOpsRunbook
	}
	if p.Facet == string(entry.Interaction) {
		return KindContract
	}
	return KindPrinciple
}

// IsProfileCore reports whether p is an A1 user profile & anti-misread boundary entry.
func (p *Pattern) IsProfileCore() bool { return p.KindOf() == KindProfileCore }

// IsContract reports whether p is a B universal collaboration contract.
func (p *Pattern) IsContract() bool { return p.KindOf() == KindContract }

// IsPrinciple reports whether p is an A2 cognitive principle.
func (p *Pattern) IsPrinciple() bool { return p.KindOf() == KindPrinciple }

// IsOpsRunbook reports whether p is a C environment/tool operational runbook (never global 4 KB standing).
func (p *Pattern) IsOpsRunbook() bool { return p.KindOf() == KindOpsRunbook }

// Pole is one end of a pattern the user goes both ways on: in which situation, and what they do then.
type Pole struct {
	When   string `json:"when"`
	Choice string `json:"choice"`
}

// Used says whether the pattern goes to the exits.
func (p *Pattern) Used() bool { return p.Lifecycle == Core || p.Lifecycle == Scoped }

// Run is one weekly or monthly pass.
type Run struct {
	Period  string         `json:"period"`
	At      string         `json:"at"`
	Cutoff  string         `json:"cutoff,omitempty"` // evidence before this day only (a backtest)
	Calls   int            `json:"calls"`
	Counts  map[string]int `json:"counts,omitempty"`
	Notes   []string       `json:"notes,omitempty"`
	Error   string         `json:"error,omitempty"`
	Changed []string       `json:"changed,omitempty"` // patterns made or moved by it
}

// Holdout is one forward check: the patterns frozen before a week, asked to predict the user's choices in it, against
// the scattered entries in the same space and against no memory.
type Holdout struct {
	Week        string             `json:"week"`
	At          string             `json:"at"`
	Backtest    bool               `json:"backtest,omitempty"` // patterns built with a cutoff at the week's start
	N           int                `json:"n"`
	Candidates  int                `json:"candidates,omitempty"`   // choice candidates scanned before dropping easy ones
	DroppedEasy int                `json:"dropped_easy,omitempty"` // candidates a no-memory model got right in both orders
	Hits        map[string]int     `json:"hits"`                   // arm → right answers
	Rate        map[string]float64 `json:"rate"`
	Chars       map[string]int     `json:"chars"` // arm → characters of memory shown
	Patterns    int                `json:"patterns"`
	Excluded    int                `json:"excluded,omitempty"` // patterns left out because they rest on a test item
	Calls       int                `json:"calls"`
	Notes       []string           `json:"notes,omitempty"`
}

// Audit is a blind spot check of patterns by a model, kept for the user to go over.
type Audit struct {
	At      string      `json:"at"`
	Items   []AuditItem `json:"items"`
	Calls   int         `json:"calls"`
	Summary map[string]int
}

// AuditItem is one checked pattern.
type AuditItem struct {
	ID      string `json:"id"`
	Facet   string `json:"facet"`
	Text    string `json:"text"`
	Verdict string `json:"verdict"` // right | mostly_right | one_off | generic | unsupported
	Reason  string `json:"reason,omitempty"`
}

// State is the overview's file.
type State struct {
	Version  int                 `json:"version"`
	Patterns map[string]*Pattern `json:"patterns"`
	Weeks    map[string]*Run     `json:"weeks,omitempty"`
	Months   map[string]*Run     `json:"months,omitempty"`
	Holdouts []Holdout           `json:"holdouts,omitempty"`
	Audits   []Audit             `json:"audits,omitempty"`
	// Refreshed is the last light refresh that changed anything, and Dead the evidence ids it found gone (so a
	// snapshot with nothing new is not counted again).
	Refreshed      string   `json:"refreshed,omitempty"`
	Dead           []string `json:"dead,omitempty"`
	ContractsSig   string   `json:"contracts_sig,omitempty"`    // signature of the last aggregated contract rule set
	ProfileCoreSig string   `json:"profile_core_sig,omitempty"` // signature of the last profile-core input set
}

func newState() *State {
	return &State{Version: 1, Patterns: map[string]*Pattern{}, Weeks: map[string]*Run{}, Months: map[string]*Run{}}
}

func (s *State) clone() *State {
	b, _ := json.Marshal(s)
	n := newState()
	_ = json.Unmarshal(b, n)
	if n.Patterns == nil {
		n.Patterns = map[string]*Pattern{}
	}
	if n.Weeks == nil {
		n.Weeks = map[string]*Run{}
	}
	if n.Months == nil {
		n.Months = map[string]*Run{}
	}
	return n
}

// Sorted is the patterns in a stable order: facet order, used first, then evidence, then id.
func (s *State) Sorted(keep func(*Pattern) bool) []*Pattern {
	var out []*Pattern
	for _, p := range s.Patterns {
		if keep == nil || keep(p) {
			out = append(out, p)
		}
	}
	order := map[string]int{}
	for i, f := range Facets {
		order[string(f)] = i
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if order[a.Facet] != order[b.Facet] {
			return order[a.Facet] < order[b.Facet]
		}
		if a.Used() != b.Used() {
			return a.Used()
		}
		if wa, wb := a.ScenesCount*a.SourcesCount, b.ScenesCount*b.SourcesCount; wa != wb {
			return wa > wb
		}
		return a.ID < b.ID
	})
	return out
}

// Holder keeps the overview's state and its file. Snapshot never waits for a run: a change is made on a copy, saved,
// then swapped in.
type Holder struct {
	mu   sync.Mutex // one change at a time
	dir  string
	snap struct {
		sync.RWMutex
		s *State
	}
}

// Open reads dir's overview state; a missing file is an empty state.
func Open(dir string) (*Holder, error) {
	h := &Holder{dir: dir}
	s := newState()
	raw, err := os.ReadFile(filepath.Join(dir, File))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, s); err != nil {
			return nil, err
		}
		s = s.clone()
	}
	h.snap.s = s
	return h, nil
}

// Dir is where the file and the reports are.
func (h *Holder) Dir() string { return h.dir }

// Snapshot is the current state; never change it.
func (h *Holder) Snapshot() *State {
	if h == nil {
		return newState()
	}
	h.snap.RLock()
	defer h.snap.RUnlock()
	return h.snap.s
}

// Update changes a copy of the state with f and, when f says it changed something, saves and publishes it.
func (h *Holder) Update(f func(s *State) (bool, error)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.Snapshot().clone()
	changed, err := f(n)
	if err != nil || !changed {
		return err
	}
	if err := writeAtomic(filepath.Join(h.dir, File), n); err != nil {
		return err
	}
	h.snap.Lock()
	h.snap.s = n
	h.snap.Unlock()
	return nil
}

func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// SaveReport writes a report's full text: kind weekly or monthly, period its week or month.
func (h *Holder) SaveReport(kind, period, text string) error {
	return SaveReportIn(h.dir, kind, period, text)
}

// SaveReportIn writes a report's full text into dir's reports, without a holder (the weekly report when the overview
// is off).
func SaveReportIn(dir, kind, period, text string) error {
	if !validPeriod(period) || (kind != "weekly" && kind != "monthly") {
		return errors.New("report: bad kind or period")
	}
	return writeFile(filepath.Join(dir, ReportDir, kind, period+".md"), []byte(text))
}

// Report reads a report's full text; "" when there is none.
func (h *Holder) Report(kind, period string) string { return ReportIn(h.dir, kind, period) }

// ReportIn reads a report's full text from dir's reports.
func ReportIn(dir, kind, period string) string {
	if !validPeriod(period) || (kind != "weekly" && kind != "monthly") {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, ReportDir, kind, period+".md"))
	if err != nil {
		return ""
	}
	return string(b)
}

// Reports lists the periods with a report of the kind, newest first.
func (h *Holder) Reports(kind string) []string { return ReportsIn(h.dir, kind) }

// ReportsIn lists dir's reports of the kind, newest first.
func ReportsIn(dir, kind string) []string {
	ents, _ := os.ReadDir(filepath.Join(dir, ReportDir, kind))
	var out []string
	for _, e := range ents {
		n := e.Name()
		if filepath.Ext(n) == ".md" {
			out = append(out, n[:len(n)-3])
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// validPeriod: "2026-W47" or "2026-11", nothing a path could escape with.
func validPeriod(p string) bool {
	if len(p) < 7 || len(p) > 8 {
		return false
	}
	for _, c := range p {
		if !(c >= '0' && c <= '9' || c == '-' || c == 'W') {
			return false
		}
	}
	return true
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Clone is a deep copy, for a pass to work on without holding the holder.
func (s *State) Clone() *State { return s.clone() }

// SaveScratch writes a scratch state (a backtest's patterns) beside the overview's own file, for review.
func SaveScratch(dir, name string, s *State) error {
	if filepath.Base(name) != name || filepath.Ext(name) != ".json" || name == File {
		return errors.New("scratch name must be a plain .json file name other than the overview's")
	}
	return writeAtomic(filepath.Join(dir, name), s)
}

// Prompt is the version of the overview's prompts, recorded in the run log.
const Prompt = "overview/4"
