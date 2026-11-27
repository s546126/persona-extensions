// Package entry holds every persisted type of the v4 curator: the one entry structure all six facets share, the
// people the user deals with, the correction ledger and friction digs, the sources' status, the switches, the
// first-run questions and the state of the exits. It holds types and small pure helpers only; the logic that fills
// them lives in curate, sources, speaker, signals, people and export, which all import this package (never the
// other way round), so the store can keep everything without import cycles.
//
// See CONTRACT.md next to this file for who writes which field and the file formats of the exits.
package entry

import (
	"regexp"
	"strings"
)

// Facet is one of the six things the curator is for, plus the people the user deals with (⑤′). They are equal: no
// facet may be dropped, narrowed or filled with generic text to make room for another.
type Facet string

const (
	Interaction Facet = "interaction" // ① how the user and the AI work together
	Tools       Facet = "tools"       // ② how the AI works with tools, machines and runtimes
	Judgement   Facet = "judgement"   // ③ the user's default judgements
	Workflow    Facet = "workflow"    // ④ how the user handles things
	World       Facet = "world"       // ⑤ entities and the physical world: facts, relations, the landscape
	People      Facet = "people"      // ⑤′ people the user deals with: one card per person entry
	Persona     Facet = "persona"     // ⑥ the user as a person
)

// Facets in display order.
var Facets = []Facet{Interaction, Tools, Judgement, Workflow, World, People, Persona}

// Valid says whether f is a known facet.
func (f Facet) Valid() bool {
	for _, x := range Facets {
		if x == f {
			return true
		}
	}
	return false
}

// Grain is how general an entry is: one concrete action, a class of situations, or an abstract tendency.
type Grain string

const (
	Action   Grain = "action"   // one concrete thing done or said, with its own quotes
	Class    Grain = "class"    // a kind of situation and what the user does in it, resting on actions
	Abstract Grain = "abstract" // an overall tendency, resting on ≥3 classes and passing the abstract gate (5.3)
)

// Origin is where an entry comes from: behaviour (the default), what the user wrote about themself ("about me", the
// first-run questions), or the assistant's own memory notes (T2 clue). Only SelfReport and FromOnboarding start stable.
type Origin string

const (
	Behavior       Origin = "behavior"
	SelfReport     Origin = "self_report" // the "about me" notes (T0)
	FromOnboarding Origin = "onboarding"  // an answer to the first-run questions (T0)
	// OriginAssistantNote is an assistant-authored memory file note (S6, T2 clue): never UserOwned and never
	// passed straight into the resident 4 KB block without user corroboration.
	OriginAssistantNote Origin = "assistant_note"
	// Handwritten is the legacy pre-0.4.0 S6 origin value kept for reading/migrating older state.json files.
	// It carries no UserOwned privilege; migrate-v04 rewrites it to OriginAssistantNote.
	Handwritten Origin = "handwritten"
	// Seed is the user's own profile or rules confirmed elsewhere (another memory, a notes file), imported
	// (enkeep_seed_import, unified memory design v2 §11.2). Unlike the others it starts a candidate: it reaches the
	// exits only once the user's own records bear it out (an action filed under it), and never while K21 is off.
	Seed Origin = "seed"
)

// IsAssistantNote reports whether o is an S6 assistant-authored note (current or legacy pre-0.4.0 value).
func IsAssistantNote(o Origin) bool {
	return o == OriginAssistantNote || o == Handwritten
}

// MemoryClass classifies an entry's architectural role, lifecycle and exit routing (0.4.0 §1.3).
type MemoryClass string

const (
	ClassCoreProfile       MemoryClass = "core_profile"       // A1: user identity, stage & anti-misread boundaries
	ClassPrinciple         MemoryClass = "principle"          // A2: cross-scene cognitive & decision principles
	ClassUniversalContract MemoryClass = "universal_contract" // B: cross-tool universal AI collaboration rules
	ClassOpsRunbook        MemoryClass = "ops_runbook"        // C: host/tool/command/API/repo/platform runbooks (never global 4 KB)
	ClassEntityDossier     MemoryClass = "entity_dossier"     // D1: persistent named entity facts & structural relations
	ClassEphemeral         MemoryClass = "ephemeral"          // D2: short-lived status/meetings/PR progress (14-30d TTL)
)

// SeedInfo is where a seed entry came from, in the import's general format (internal/seed).
type SeedInfo struct {
	Source    string `json:"source"`              // enkeep, a file, another memory
	ID        string `json:"id"`                  // the item's id at the source: a later import updates it
	Kind      string `json:"kind"`                // rule | fact | guardrail | preference
	Hard      bool   `json:"hard,omitempty"`      // a hard rule the user confirmed: never turned by counter-examples alone
	Category  string `json:"category,omitempty"`  // the source's category
	Confirmed string `json:"confirmed,omitempty"` // when the user confirmed it at the source (YYYY-MM-DD)
	Effective string `json:"effective,omitempty"` // from when it holds (YYYY-MM-DD)
	// Expiring: "recent" (what the user is doing now) or "role" (responsibilities) date; "" never by itself.
	Expiring string `json:"expiring,omitempty"`
	Imported string `json:"imported"` // when it was imported (RFC 3339)
	Text     string `json:"text"`     // the text as the source has it, to tell a change at the next import
}

// Status of an entry. Thresholds adapt to the user's amount of data (4.4): see curate's thresholds.
type Status string

const (
	Candidate Status = "candidate"
	Stable    Status = "stable"
	Shifting  Status = "shifting" // recent counter-examples ≥ 25%: shown, not exported
	Dormant   Status = "dormant"  // quiet for 90 active days (180 for established entries); a confirmed entry is asked about once, not dropped
	Retired   Status = "retired"  // superseded, invalidated (valid_to set) or merged into another
)

// Domain: what an observation is about, judged from its topic (not its source).
const (
	Work = "work"
	Life = "life"
)

// The K13 names, before 0.2.0-beta.40 (category.go maps them: family is part of relationships). Content in them is
// collected and tagged now; K13.<category> on lets that category into the resident exits (the user's widening).
const (
	SensEmotion = "emotion"
	SensFinance = "finance"
	SensHealth  = "health"
	SensFamily  = "family"
)

// Sensitives lists the categories in display order.
var Sensitives = []string{SensEmotion, SensFinance, SensHealth, SensFamily}

// Parts of a person's card (⑤′), kept and shown apart.
const (
	PartFact       = "fact"       // role, what they are responsible for, relation to the user, projects
	PartUserEval   = "user_eval"  // what the user said about them, in the user's words
	PartInferred   = "inferred"   // a profile or judgement the model drew: always labelled, with evidence
	PartCommitment = "commitment" // a promise either way, with a due date when known
)

// Scope kinds: where an entry holds.
const (
	ScopeGlobal  = "global"
	ScopeRuntime = "runtime" // an assistant or agent runtime
	ScopeProject = "project"
	ScopeObject  = "object" // a machine, a tool, a service
)

// Scope is one place an entry holds. Entries alike in content but differing in scope merge, taking the union.
type Scope struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// Source codes (design §3). Evidence carries one; the sources card lists them.
const (
	SrcAssistant = "S1"  // the user's chats with the assistant (assistant.read)
	SrcAsk       = "S2"  // ask_user questions and answers (agents.read interactions)
	SrcAgent     = "S3"  // other agents' sessions: text and folded work (agents.read items, fold_work=1)
	SrcTools     = "S4"  // a dug segment of tool calls and results, from S3 or the assistant's own session
	SrcTodo      = "S5"  // to-dos and their decisions (todos.read)
	SrcMemory    = "S6"  // the assistant's hand-written memory (H2)
	SrcAbout     = "S7"  // "about me" (assistant.ask, NotesEffective)
	SrcChat      = "S8"  // connected message sources: chats, mail, code, docs, meetings, events (kb.read)
	SrcKB        = "S9"  // the knowledge base's people, projects, relations and kb.owner
	SrcRecording = "S11" // recordings, minutes and day reviews (kb.read recordings)
	SrcLedger    = "S12" // assistant structured ledger entries (assistant.entries.read)
	SrcSent      = "S13" // sent-as-user provenance on archived chats (sent.read)
)

// Who a line is by, after speaker resolution (3.2).
const (
	WhoSelf = "self" // the user
	// "person:<kb id>" for someone the knowledge base has; "" when the line could not be resolved.
)

// Evidence anchors an entry to one line. Quote is the original words, never translated.
type Evidence struct {
	Source  string `json:"source"`            // S1…S11
	Ref     string `json:"ref"`               // "<kind>/<id>" of the unit (a chat, an agent session, a to-do…)
	Day     string `json:"day"`               // YYYY-MM-DD, local
	N       int    `json:"n,omitempty"`       // line number in the unit's day
	Seq     int    `json:"seq,omitempty"`     // agent session item sequence, when it came from items
	Speaker string `json:"speaker,omitempty"` // the display name as written
	Who     string `json:"who,omitempty"`     // WhoSelf, "person:<id>" or "" (unresolved)
	Role    string `json:"role,omitempty"`    // voice.Role*: user, assistant, agent_draft, bot, other
	Quote   string `json:"quote"`
	// Reported: the line talks about the entry's subject (someone else speaking of a person), not by them.
	Reported bool `json:"reported,omitempty"`
}

// Impact is the real check that replaced "≥8 characters" (5.0): which assistant action changes, at which moment,
// for which object. An entry that cannot name one is not kept.
type Impact struct {
	Moment string `json:"moment"`           // see Moments
	Action string `json:"action"`           // what the assistant does differently
	Object string `json:"object,omitempty"` // the person, project, machine, tool or kind of task
}

// Moments an impact may name: each maps to an exit (§6).
var Moments = []string{
	"every_turn",   // exit A
	"ask_user",     // exit A, F: when asking the user
	"report",       // exit A: when reporting back
	"dispatch",     // exit D: when sending an agent
	"task_start",   // exit B: when a kind of task starts
	"use_object",   // exit B: when touching that machine or tool
	"reply_person", // exit E: when triaging or drafting a reply to someone
	"kb_lookup",    // exit C: when the knowledge base is asked about it
}

// Commitment is a promise either way (⑤ and ⑤′).
type Commitment struct {
	From string `json:"from"`          // WhoSelf or "person:<id>"
	To   string `json:"to"`            // WhoSelf or "person:<id>"
	What string `json:"what"`          // what was promised
	Due  string `json:"due,omitempty"` // YYYY-MM-DD
	Done string `json:"done,omitempty"`
}

// UserEdit is the user's own version of an entry's text or its exit text: never overwritten by the curator; when
// the basis changes the page only says so.
type UserEdit struct {
	Text string `json:"text"`
	At   string `json:"at"`
}

// Verdict is one mark on the check page: right, wrong or unsure. Wrong withdraws the entry from every exit at once
// and keeps it as a counter-example.
type Verdict struct {
	Mark string `json:"mark"` // right | wrong | unsure
	At   string `json:"at"`
	By   string `json:"by"` // user | model
	Note string `json:"note,omitempty"`
}

// ExportText is an entry rewritten for an exit, with the hash of the body it was written from (6.4).
type ExportText struct {
	Text  string `json:"text"`
	Basis string `json:"basis"` // sha1 of the entry's Text and structured fields when it was written
	At    string `json:"at"`
	User  string `json:"user,omitempty"` // the user's own exit wording: kept even when Basis changes
	// Withheld: at the rewrite the model could name no moment the assistant would act differently for this line
	// (the empty-talk question, 5.0): it stays on the page and out of the exits until its basis changes.
	Withheld bool `json:"withheld,omitempty"`
}

// Entry is one thing learned, in any facet and grain. Facet-specific fields are empty where they do not apply.
type Entry struct {
	ID    string `json:"id"`
	Facet Facet  `json:"facet"`
	Grain Grain  `json:"grain"`
	// Type is the open type of the facet: an interaction kind (①), object kind (②: machine, tool, runtime, model,
	// gateway), fork type (③), task type (④), relation or fact kind (⑤), profile key (⑥). For ⑤′ it is the
	// dimension of the card line (role, responsibility, collaboration, responsiveness, style…). Types come from the
	// user's own data (State.Types), never from a fixed list.
	Type string `json:"type,omitempty"`
	// Text is the body in the target language. Quotes in Evidence stay as said.
	Text string `json:"text"`

	// ③④① situation, choice and reason; ② object, approach and pitfall.
	Situation string `json:"situation,omitempty"` // the fork, the kind of situation or occasion
	Choice    string `json:"choice,omitempty"`    // what the user chose or does
	Rejected  string `json:"rejected,omitempty"`  // what was not chosen
	Why       string `json:"why,omitempty"`       // the user's reason (original or labelled inferred)
	Exception string `json:"exception,omitempty"` // when the user does otherwise
	Object    string `json:"object,omitempty"`    // ②: the machine, tool or runtime; ④⑤: the project
	Pitfall   string `json:"pitfall,omitempty"`   // ②: what fails
	Fix       string `json:"fix,omitempty"`       // ②: how it was got around
	// Default is what a capable person who does not know the user would do: required to lift to Abstract (5.3).
	Default    string   `json:"default,omitempty"`
	Situations []string `json:"situations,omitempty"` // Abstract: the ≥2 situation types it governs

	// ⑤′ people.
	Person     string      `json:"person,omitempty"` // "person:<kb id>" the card belongs to
	Part       string      `json:"part,omitempty"`   // PartFact, PartUserEval, PartInferred, PartCommitment
	Commitment *Commitment `json:"commitment,omitempty"`
	// ⑤ world: the relation, when the entry is one (subject and object are "person:<id>" or "project:<id>").
	Subject   string `json:"subject,omitempty"`
	Predicate string `json:"predicate,omitempty"`
	Target    string `json:"target,omitempty"`

	Inferred     bool   `json:"inferred,omitempty"`      // the model's reading, not stated
	ReportedOnly bool   `json:"reported_only,omitempty"` // rests only on what others said about the subject
	SingleSource bool   `json:"single_source,omitempty"` // ⑤: one source so far (4.4); exits D and E carry the mark
	Domain       string `json:"domain,omitempty"`        // Work | Life
	Sensitive    string `json:"sensitive,omitempty"`     // before 0.2.0-beta.40: one of Sensitives; read by Cats
	// Categories (category.go): what the entry is about among health, finance, relationships, emotion and legal_id,
	// and who tagged each; none: general. CatsRemoved are the ones the user took off (never added back but by them).
	Categories  []Category `json:"categories,omitempty"`
	CatsRemoved []string   `json:"cats_removed,omitempty"`
	// Tagged: its categories and source space were decided by the plugin's tagging (0.2.0-beta.40 rules: the model's
	// label, the money and legal words, the space's defaults) when it was read or imported. An entry from before
	// carries only the old model label, if any; the memory source answers tagged false while one is in the pool.
	Tagged bool `json:"tagged,omitempty"`
	// OriginSpace is the slug of the space the entry's first evidence was said in ("": the main space).
	OriginSpace string     `json:"origin_space,omitempty"`
	Scopes      []Scope    `json:"scopes,omitempty"`
	Impact      Impact     `json:"impact"`
	Evidence    []Evidence `json:"evidence"`

	// Members are lower-grain entries: a class's actions, an abstract entry's classes. Parent is the class an
	// action was filed under. Counters are actions that went against it.
	Members  []string `json:"members,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Counters []string `json:"counters,omitempty"`

	// Counting (5.0): one per entry, session and day. Chances are the times the kind of situation came up
	// (for ③④: the class's fork seen, whatever was chosen). ActiveDays are distinct days with any source data on
	// which it was seen. Kinds are the distinct source codes.
	First      string   `json:"first"`
	Last       string   `json:"last"`
	Count      int      `json:"count"`
	Chances    int      `json:"chances,omitempty"`
	Sessions   int      `json:"sessions,omitempty"`
	ActiveDays []string `json:"active_days,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`

	Status   Status `json:"status"`
	StatusAt string `json:"status_at,omitempty"`
	// Effective is when the entry first went into an exit: recurrences of a correction count from here (5.1).
	Effective string `json:"effective,omitempty"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"` // set when invalidated; the entry is kept, not deleted

	Origin        Origin      `json:"origin"`
	OriginVersion string      `json:"origin_version,omitempty"` // prompt@ruleset, or the memory file's hash
	MemoryClass   MemoryClass `json:"memory_class,omitempty"`   // 0.4.0 §1.3 architectural class
	Seed          *SeedInfo   `json:"seed,omitempty"`           // Origin Seed: where it came from

	User       *UserEdit   `json:"user,omitempty"`
	Pinned     bool        `json:"pinned,omitempty"`
	Deleted    bool        `json:"deleted,omitempty"` // a tombstone: the same content extracted again is dropped
	Verdicts   []Verdict   `json:"verdicts,omitempty"`
	Export     *ExportText `json:"export,omitempty"`
	MergedInto string      `json:"merged_into,omitempty"`

	Flags   []string `json:"flags,omitempty"`
	Updated string   `json:"updated"`

	// Leverage is how far and how long the entry reaches, and the principle behind it, as the daily pass read them
	// (daily/17, leverage.go). Absent on entries read before; the score falls back to the other fields then.
	Leverage *Leverage `json:"leverage,omitempty"`
}

// Shown is the text the user sees: their own version when they wrote one.
func (e *Entry) Shown() string {
	if e.User != nil && e.User.Text != "" {
		return e.User.Text
	}
	return e.Text
}

// Live says whether an entry may be used at all: not deleted, retired or merged, not marked wrong last.
func (e *Entry) Live() bool {
	if e.Deleted || e.Status == Retired || e.MergedInto != "" || e.ValidTo != "" {
		return false
	}
	if n := len(e.Verdicts); n > 0 && e.Verdicts[n-1].Mark == "wrong" {
		return false
	}
	return true
}

// IsTrueUserOwned is the single source of truth for T0 user-sovereign entries (0.4.0 §1.2):
// SelfReport ("about me"), FromOnboarding, Pinned, or a non-empty user edit.
// S6 assistant notes (OriginAssistantNote and legacy Handwritten) are never user-owned.
func IsTrueUserOwned(e *Entry) bool {
	if e == nil {
		return false
	}
	if e.Pinned || e.Origin == SelfReport || e.Origin == FromOnboarding {
		return true
	}
	return e.User != nil && strings.TrimSpace(e.User.Text) != ""
}

// PayloadTexts returns the entry's own structured text fields (never raw Evidence quotes) for Payload-First
// sensitivity checks and deterministic ops-runbook classification.
func (e *Entry) PayloadTexts() []string {
	if e == nil {
		return nil
	}
	return []string{
		e.Shown(), e.Text, e.Situation, e.Choice, e.Rejected,
		e.Why, e.Exception, e.Object, e.Pitfall, e.Fix,
		e.Impact.Action, e.Impact.Object,
	}
}

var (
	// opsRunbookCommandRe detects concrete CLI tools, shell invocations, file/script paths, env vars and git/CI ops.
	opsRunbookCommandRe = regexp.MustCompile(`(?i)` +
		`\b(git|gh|ssh|scp|rsync|docker(\s+compose)?|kubectl|helm|npm|pnpm|yarn|cargo|go\s+(test|vet|build|run|mod|fmt)|` +
		`python3?|pytest|curl|wget|brew|apt(-get)?|systemctl|launchctl|tmux|xcodebuild|xcrun|simctl|adb|gradle|` +
		`make|cmake|jq|sed|awk|lsof|pkill|crontab|sqlite3|psql|gofmt|xcodegen)\b` +
		`|~/[a-z0-9._-]+|/(tmp|etc|usr|var|opt|home|users)/[a-z0-9._/-]+` +
		`|\b[a-z0-9_-]+\.(sh|py|go|json|yaml|yml|toml|env|plist|service|sql|xcstrings|xcodeproj)\b` +
		`|\b[A-Z][A-Z0-9_]{2,}_(KEY|TOKEN|URL|HOST|PORT|DIR|PATH|ENV|HOME|CONFIG|SECRET)\b` +
		`|\borigin/(main|master)\b|\b(worktree|squash[- ]?merge|cherry-pick|dockerfile|makefile|workflow\.yml)\b`)

	// opsRunbookInfraRe detects concrete hostnames, infra services, agent engines, platform tools and API mechanics.
	opsRunbookInfraRe = regexp.MustCompile(`(?i)` +
		`\b[a-z0-9]+-(wsl|vps|nas|mac|linux|server|node|host|box|vm|prod|staging|ci)\b` +
		`|localhost:\d+|127\.0\.0\.1|\b\d{1,3}(?:\.\d{1,3}){3}:\d+\b` +
		`|\b(wsl2?|vps|nas|ec2|s3|rds|k8s|kubernetes|nginx|caddy|systemd|tailscale|wireguard|cloudflare|` +
		`postgres(ql)?|redis|mysql|mongodb|mac\s*mini|ubuntu|debian|centos|alpine|jump[- ]?host|bastion)\b` +
		`|\b(herdr|orca|tern|paseo|codex|claude\s*code|opencode|cursor|aider|copilot|` +
		`1password|bitwarden|keychain|email-relay|personad|persona-link|mcp|webhook|oauth|jwt|grpc|websocket|` +
		`http\s*[45]\d\d|rate\s*limit|api\s*(key|token|limit|quota|endpoint)|rest\s*api|graphql|cron(tab)?|launchd)\b` +
		`|\b(task_plan|agent_start|decisions_query|memory_sources|memory\.entries|kb\.update|kb\.read|` +
		`ask_user|todo_create|enkeep[-_]memory|persona-extensions|testflight|deriveddata|app\s*store\s*connect)\b`)

	// opsRunbookZHRe detects Chinese environment/deployment/CI/quota/platform-mechanism workflow rules.
	opsRunbookZHRe = regexp.MustCompile(
		`部署到|放\s*VPS|跑门禁|CI\s*门禁|门禁脚本|定时脚本|定时任务|端口转发|跳板机|容器镜像|镜像升级|` +
			`轮换密钥|API\s*限流|额度.{0,8}分流|改配置前.{0,6}备份|工作树|合入\s*main|合并\s*PR|推分支|拉取仓库|环境变量`)
)

// ClassifyOpsRunbook deterministically reports whether e is a Class C environment/tool ops runbook (0.4.0 §1.3):
// any tools-facet entry, any entry already tagged ClassOpsRunbook, or any entry whose own text/object names concrete
// hosts, tools, CLI commands, APIs, repositories or platform-specific workflow mechanics.
func ClassifyOpsRunbook(e *Entry) bool {
	if e == nil {
		return false
	}
	if e.MemoryClass == ClassOpsRunbook || e.Facet == Tools {
		return true
	}
	if e.Facet == World || e.Facet == People {
		return false
	}
	for _, s := range e.Scopes {
		if s.Kind == ScopeObject && strings.TrimSpace(s.Name) != "" {
			return true
		}
	}
	for _, txt := range e.PayloadTexts() {
		if strings.TrimSpace(txt) == "" {
			continue
		}
		if opsRunbookCommandRe.MatchString(txt) || opsRunbookInfraRe.MatchString(txt) || opsRunbookZHRe.MatchString(txt) {
			return true
		}
	}
	return false
}

// Tombstone keeps what the user deleted so the same content is not made again.
type Tombstone struct {
	Facet Facet  `json:"facet"`
	Key   string `json:"key"` // norm.Q of the text (and person, for ⑤′)
	Text  string `json:"text"`
	At    string `json:"at"`
}

// Event kinds of the event log: replaying the log over an empty state gives the same entries (P3's acceptance).
const (
	EvObserve = "observe" // a new action entry from a unit (Entry is the whole entry, JSON in Data)
	EvFile    = "file"    // an action filed under a class (Other)
	EvCounter = "counter" // an action went against a class (Other)
	EvCreate  = "create"  // a class or abstract entry made (Data)
	EvMerge   = "merge"   // Entry merged into Other
	EvStatus  = "status"  // From → To
	EvUser    = "user"    // edit, pin, unpin, delete, restore, verdict (Detail says which; Data the payload)
	EvExport  = "export"  // went into an exit (Detail: the exit)
	EvInvalid = "invalid" // ValidTo set by newer contrary evidence (Other: the newer entry)
)

// Event is one change to an entry.
type Event struct {
	At     string `json:"at"`
	Run    string `json:"run,omitempty"`
	Kind   string `json:"kind"`
	Entry  string `json:"entry"`
	Other  string `json:"other,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
	Data   []byte `json:"data,omitempty"`
}

// TypeDef is one type in a facet's open type table (5.2, 5.4).
type TypeDef struct {
	Name    string `json:"name"`
	Origin  string `json:"origin"` // seed (the first-run starter pack), model (seen ≥2 times), user
	Seen    int    `json:"seen"`
	Renamed string `json:"renamed,omitempty"` // the user renamed or merged it into this one
}
