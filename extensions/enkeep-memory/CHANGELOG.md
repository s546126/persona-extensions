# enkeep-memory changelog

## 0.4.1

- **Profile isolation.** Sensitive `profile:cat:*` sections hold only the user's own records; people-facet records stay
  on the person's card. The world section leads with the user's own facts, and people cards and world relation lines
  between other contacts are capped at one line per person and two in total. `LOW_CONFIDENCE` entries and
  single-sighting `one_off` persona entries no longer reach the profile (the user's own entries are exempt). A
  work-method pattern keeps a stored category only while payload-first rules still find it, so bookkeeping is no
  longer read as personal finance.
- **Compact 4/8/16 KB standing set.** Section shares are now 22/43/35 with elastic profile/principle growth (at most
  25%/47%), shorter `[画]`/`[原]`/`[硬]` tags, compact two-pole principles, and clustering of near-duplicate cold-start
  lines. The `resident_bytes` maximum is raised to 16384.
- **migrate-v04 Phase 1 and graph clean-ups.** Stable persona entries flagged `LOW_CONFIDENCE` and seen once are
  demoted to candidate; known `legal_id` false positives on non-work facets are cleared while real labels are kept.
  Person nodes whose name ends in a software/service suffix are merged into the matching project/system node or
  retyped (reversible via Unmerge).

## 0.4.0

- **Four-tier source authority, six memory classes, and payload-first privacy (`daily/19`).**
  - Introduced four-tier source authority (`T0`–`T3`), six `MemoryClass` categories, `OriginAssistantNote` (demoting
    assistant reflection notes from standing privilege), `IsTrueUserOwned` as the single source of truth for user
    ownership, and `ClassifyOpsRunbook` for separating operational runbooks from standing memory.
  - Switched sensitivity tagging to payload-first classification (`PayloadCategories`), added cron boilerplate
    pre-cleaning, removed legacy `rules_merge.*` and `Handwritten` standing privilege, and upgraded the daily
    extraction prompt to `daily/19`.
- **Entity governance, reversible graph resolution, and dual-lifecycle decay.**
  - Added entity governance with prefix stripping, undirected `collaborates_with` relations, and sunsetting of
    `about`/`same_circle`.
  - Added reversible Tier 1/Tier 2 graph resolution (`resolve.go`) requiring strong naming/alias evidence with bounded
    Tier 2 candidate pairs (`tier2MaxPairs=240`), plus dual-lifecycle decay for transient status (14/30d) and ops
    runbooks (45/60d).
- **L1 profile & contract synthesis, 4 KB three-section standing, and `state.json`-SSOT sensitive profile sections.**
  - Added L1 event-shell stripping, `KindProfileCore`/`SyncProfileCore`, pre-routed chunked `SyncContracts`
    (`<=35` candidates/batch, `contractMaxTokens=12000`) eliminating `max_tokens` JSON truncation, and structured
    `Poles` tension reconciliation (`Reconcile`).
  - Adopted a three-section + hot-slot 4 KB standing layout with clean blank-user cold-start fallback, and enforced
    `state.json` as the single source of truth for profile sensitive sections—excluding `ops_runbook` entries and
    tightening cross-word regexes with gender-neutral wording.
- **Production migration CLI and review-bundle exporter.**
  - Added `migrate-v04` for reversible in-place migration (Phase 0 atomic SHA-256 backup & byte-exact `-rollback`,
    Phase 1 deterministic clean, Phase 2 rate-limited Haiku resolution/audit/routing, Phase 3 L1 & profile rebuild)
    and `review-bundle` for exporting the 6-deliverable review package (PRs #1324, #1325, #1326, #1327, #1332, #1383,
    #1396).

## 0.3.0

- **Unified principle & contract aggregation architecture (L1).**
  - Refactored `internal/overview` into unified L1 cognitive principles (`judgement`/`workflow`/`persona`) and
    collaboration contracts (`interaction`/`tools` via `SyncContracts`), grounded in deduplicated first-person user
    quotes (`overview/3`), fixed cross-scene and quote-dedup evidence gates, counter-example search without `Choice`
    bias (capturing `Exception` and `Poles`), `Rooted` protection, and subject-ownership privacy tagging.
  - Added online per-contract violation recurrence counts and per-100-decision-cards recurrence rates to
    `enkeep_overview_status(from, to, query)` with no hardcoded rule names or new manifest permissions.
- **Verbatim pass-through for user-confirmed rules and three-bucket standing selection.**
  - User-owned rules (`handwritten`, `self_report`, user-edited) pass through verbatim with intact exceptions into
    standing (`4096 B`) and `memory.entries` Page 1 (`rank >= 820`, enriched occasion/tool subjects) rather than being
    rewritten by L1; L1 focuses on synthesizing new principles and contracts from scattered behavioral events and
    transferring cross-event `Hard`/`Polarity` signals.
  - Three-bucket standing outlet: Bucket 0 Hot Slot (`Pinned`, Fast-Path `fast_hot:<day>` TTL 7d, and post-L1
    unabsorbed high-certainty rules), Bucket 1 Contracts (80%, two-pass anti-monopoly polarity/group caps), and
    Bucket 2 Principles (20%, unified leverage score). Removed legacy `PatternShare=0.25`, `spillFloor=50`, and
    single-entry L0 90–99 hard-rule bidding.
- **Deterministic L0 pre-cleaning and high-purity 1-hop entity skeleton expansion.**
  - Added deterministic boilerplate/template stripping (`internal/relay`, `internal/curate/preclean.go`) and S3 agent
    facet scoping before L0 extraction.
  - Added high-purity 1-hop structural entity skeleton expansion (`internal/recall/expand.go`: whole-word ASCII
    matching `>= 3` chars, generic noun stoplist, shared-hub `depends_on` fan-in pruning), entity-anchored rule
    recall floor (`rank 750–891`), Chinese slug / T2 project block fixes, and enriched subjects on non-standing L1
    patterns for `decisions_query`.
- **Anti-overfit evaluation suite (`hardeval`).**
  - Added time-clean multi-checkpoint `hardeval l1-rebuild`, `-overview-dir`, `decisions-query-sim` real hub Page-1
    tag simulation, cross-pass semantic deduplication and entity-fact priority in two-layer recall, and multi-window
    non-overlapping holdout / entity-ablation extraction.

## 0.2.7

- **Read assistant structured ledger (`S12/ledger`) and sent-as-user provenance (`S13/sent`).** The manifest now requests
  `assistant.entries.read` and `sent.read` and subscribes to `assistant.entries.changed`.
  - Daily passes read active `assistant.entries.list` entries into `ledger/<day>` (and `ledger/<space>/<day>` when
    scoped to another assistant space) with entry-id deduplication, self-write exclusion (`by=enkeep`), per-unit content
    hash cursors, and structured decision details (`selected`, `rejected`, `reason`, `quote`) as high-evidence input.
  - With `sent.read`, archived `kb.day` messages sent by the assistant in the user's name (`SentBy != ""`) are
    relabelled as `voice.Ghost` so AI-drafted sends are not attributed to the user's own words, recalled sends are
    dropped, and `S13/sent` tracks deduplicated provenance counts and a monotonic `SentLedgerID` cursor.

## 0.2.6

- **Rules keep when they do not hold.** An entry's exception (read into its own field) was dropped at every exit, so
  a standing "do not stop to ask" went without "only when it is live, public or deletes data". Rules, ways of working
  and choices now carry it, `（例外：…）`, in the standing set, memory.entries and the exits; the standing line cuts its
  other words first, never the exception.
- **Rules saying one thing in other words are made one.** A rule merge puts the assistant's rules that compete for the
  standing set (settled or hard, across types and grains) to the model together, after each daily pass whose rules
  changed and in the weekly pass. Rules telling the assistant to do the same thing on the same kind of occasion become
  one: the strongest stays, with one sentence for the default and every condition under which it does not hold that
  any of them or the user's lines state. A rule the assistant's own memory or the user wrote is marked covered by it
  instead of merged away, and stands again if the covering rule goes. Every merge is an event and undoable.
- **One class takes at most a fifth of the standing room.** A facet and its type (as the type table names it) is a
  class; no class fills more than 20% of the room, whatever its scores.
- **Rules are found by their occasion.** A rule's subjects carry the words of its moment (派活 / dispatch, 汇报 /
  report, 提问 / ask, 开工 / task…) and its type, so a decisions_query tag of one occasion word brings them in; exit
  A's recall line says to query by occasion before dispatching, reporting or asking.
- **Evaluation.** `hardeval merge-rules` runs the rule merge on a copy of the state (`-before` an evaluation's cutoff).

## 0.2.5

- **The standing set ranks rules by leverage, and gives rules the larger share.** Hard rules scored exactly 90, and
  with over a hundred of them, which ones stood was decided by days of evidence and id: a complaint about slow replies
  stood while a cross-project rule seen on four days did not. Hard rules now score 90 to 99 by their own leverage.
  Times seen add at most 25 instead of 15, so a rule distilled from dozens of observations ranks above one seen seven
  times. Rules take 60% of the room and the understanding 40% (the reverse before): in the collaboration evaluation
  the rules side is what avoids mistakes. On the same snapshot at 4096 bytes, incidents avoided rise from 25% to 29%.
- **Collaboration evaluation diagnosis.** `hardeval collab-run` records per incident how often an arm was judged a
  wrong action or a needless ask; `hardeval collab-why` lists, for named incidents, the entries closest to what they
  needed, whether each stood or was in the scatter memory, and why not, plus (`-show`, `-diff`) the standing set of
  the day and the scatter lines that did not stand.

## 0.2.4

- **Non-blocking queued `enkeep_mark_wrong`, cutoff enforcement in `patternOut`, and schema-safe report rendering.**
  `applyMarks` now releases `marksMu` before waiting on `St.Lock()` and trims only the applied queue prefix (matching
  `applyReruns`), while `queueMarks` reads the lock-free `Live` snapshot so concurrent or pre-snapshot calls never
  deadlock. `heFilter.patternOut` now passes the evaluation `cutoff` to `entryOut` (with `LastEvidence` fallback when
  evidence entries are absent), preventing post-cutoff overview patterns from leaking into evaluation arms.
  `reportPage` now validates each chunk via `wire.CheckView` with plain-text fallback and a chunk cap.

## 0.2.3

- **Queue `enkeep_rerun` requests while a pass holds the store lock.** Instead of returning `busy` after the user
  approves an `enkeep_rerun` card while a daily or fast pass is running, `enkeep_rerun` validates the date range,
  persists the request to `reruns-queue.json` beside `state.json`, and returns `Queued` immediately. Pending requests
  are merged into `State.RerunQueue` / `State.ForceReruns` (leaving already-queued days intact) as soon as the lock
  frees, before `tick` starts the next day (and on startup if the process restarted), and are surfaced by
  `enkeep_rerun_status`.

## 0.2.2

- **Collaboration evaluation: recall like a search, not every subject.** The recall arms weigh each name in an
  incident by how few entries it points at (a repository's own name over the platform's), and find a world fact by an
  identifier named only in its text. The first version matched every subject, and common names filled the recall
  budget with unrelated rules. With it, the entity ablation goes the expected way on the baseline snapshot: without
  the world and people entries, entity incidents avoided fall from 10% to 0% and wrong actions rise from 44% to 49%.

## 0.2.1

- **Avoid untracked `MarkShown` writes when rendering the review tab.** Opening the review tab now records unseen
  proposals synchronously via a non-blocking lock when idle instead of spawning an untracked goroutine on every render,
  deduplicates and flushes pending marks when a curation run finishes (or via at most one tracked worker if another
  holder briefly owns the store lock), and skips state and overview writes in the read-only `enkeep-memory page`
  subcommand.

## 0.2.0

The first stable release. It ships what was prepared as 0.2.0-beta.53 (never published), listed below; from now on
releases are numbered without a pre-release suffix (0.2.1, …). The hub sees 0.2.0 as newer than every 0.2.0-beta.N,
so installs on a beta upgrade to it. The beta entries below keep their headings: those versions were published as
pre-releases.

The daily pass looks for the user's own thinking moves first, and the people-and-project skeleton is back in recall.

- **Daily prompt daily/18.** Before anything else it looks for how the user checks a claim (back to the first-hand
  source, a hunch marked as one and tested on data, an over-precise number taken as a weak sign), finds the holes (a
  premise that breaks the conclusion, the premise of a two-option question, what a limit really prevents) and keeps
  to the goal (stop detail that changes nothing, direction before strategy, the main contradiction first), also when
  the lines are not about the assistant. It writes the move, not the episode; how to use one machine or path is a
  tools observation at most. On the same raw input (19 day-batches, blind-judged, sentence only) the share of
  thinking moves goes from 21% (daily/16) to 57–61%, level with the old pipeline's 57%. The new version reads the
  rerun range again.
- **Rules hang on the person or project they are for.** daily/18 puts a rule that holds for one person, project,
  machine or chat in its scope and impact object; the impact object is now one of the subjects memory.entries
  answers, so mentioning that person or project brings the rule in.
- **Relation triples in three kinds.** The skeleton (assigned, depends_on, member_of, collaborates_with, with owns,
  role and reports_to) is in the pool again: ranked by its score in recall, fed to the overview and the evaluations,
  never standing. Only noise (about, same_circle) and passing edges (waiting_on, blocked_by, allocated, serves) not
  seen for 14 days are weak: answered last, out of the pool.
- **Cross-event patterns stand again.** A pattern scores at most 70, so in beta.52 the single entries above it (hard
  rules at 90) and the short lines filling what was left kept every pattern out of the standing set (none at 1400
  bytes, none on the rules side at 4096). Patterns are now offered a quarter of the room first, each side within its
  quota (live copy: 1 of 9 lines at 1400 bytes, 7 of 23 at 4096), and the room one side leaves takes no line scoring
  under 50 (it went to fragments such as a story character). `enkeep-memory hardeval standing` prints the set and why
  each pattern is in or out.
- **Collaboration evaluation.** Incidents are also tagged when avoiding them needs entity relations (mixing up people
  or projects, not knowing who is responsible, missing a dependency), reported in a column of their own; two recall
  arms answer with the standing set plus what the incident's names bring in, with and without the world and people
  entries (the ablation).

## 0.2.0-beta.52 (pre-release)

What stands in every turn is chosen by how far and how long an entry matters, not by how recent it is; the daily
prompt asks for the principle behind a choice. Design: the hub gives the extension's standing entries 1536 bytes and
packs them highest rank first, so the old "thousands standing, newest first" put short fragments in the turn.

- **Leverage (daily/17).** Each observation is labelled with its reach (one-off, a kind of task, a project, across
  projects, every turn), its horizon (days to lasting), the principle behind it (said, or inferred and marked so) and,
  for ③ and ⑥, the dimension of thinking (decision, thinking, reaction, attention, communication). The prompt asks
  not to keep a one-off detail without a principle, and to write an exception as the other pole's situation. Labels merge across
  sightings (the wider reach and longer horizon stay; a stated principle replaces an inferred one).
- **The standing set.** `memory.entries` marks only the set chosen by score as standing: the understanding of the user
  60% and the assistant's rules 40% of 1400 bytes, at most 12 lines, alike lines once, the two sides interleaved
  (docs/exits.md). Every other entry is recalled on demand and carries a line of the user's own words. Overview
  patterns compete for the same set on the entries' scale (at most 70, so long patterns do not fill it); the
  80-pattern standing cap is gone; the profile summary is no longer standing.
- **Two poles, not "unless".** The counter-example check may keep a contradiction as both ends, each in its situation
  (`两端：A时，X；B时，Y`), instead of an exception.
- **Rooted principles are not put to sleep.** A stable entry with evidence in four ISO weeks, or a lasting
  cross-project principle seen on two days, is not made dormant by 40 quiet days: the user is asked once instead.
- **Deep reading works again (S4).** The hub folds a conversation's tool work only when a page is read without
  `after_seq`, and the extension always pages with it, so every deep-reading signal was skipped as "no folded work
  near it". The extension now folds unfolded pages itself, as the hub would. Deep reading costs a model call per
  signal that finds work again.
- **Template lines are not the user's.** A line the user is said to have typed in agents' sessions (S3) that recurs
  word for word in three sessions or more is a dispatch prompt's or the platform's template, not the user's habit:
  an entry resting only on such lines never stands, nor a pattern resting mostly on them. The platform's task-brief
  boilerplate (the credential note, the supervisor's report tool and acceptance list, the result-file line) marks a
  whole text as Persona's. Nothing is deleted; a rerun reads the days again.
- **Corrections weigh, hard rules stand.** A correction of the assistant scores more for each day the user had to make
  it again; one made on two days within a week, or with a ban or anger in the user's words, is a hard rule: it stands
  at once, even as a candidate, and scores at least 90. Alike scores are ordered by days of evidence, then the
  latest, no longer by id.
- **Correction fast path.** Every `fast_minutes` (10; 0 off), when nothing else runs, today's assistant conversations
  are looked at past the last look; one where the user corrected the assistant is read at once with the daily prompt
  (one call, at most 24 a day) and the snapshot is published, so the correction stands in the next turns. The source
  cursors are not moved: the night's pass reads the whole day as always.
- **`resident_bytes`** (1400; 800–8192): the room the standing set is chosen for, so a wider standing room from the
  hub needs a setting, not a release; one more line per 120 bytes beyond 1400.
- **Hard and collaboration evaluations.** `enkeep-memory hardeval collab-build|collab-run` draws real collaboration
  incidents (the assistant's feedback memories, the user's corrections in its transcripts), answers each with every
  arm's memory as of the day before, and has another model judge blind whether the incident was avoided, with fewer
  questions, wrong actions and repeats. `enkeep-memory hardeval build|run` draws held-out questions from a copy of the state (real
  choices first seen after a cutoff: a trade-off, a correction of the assistant, the same judgement in another
  project), drops what a stranger model gets right both ways, and answers them with no memory, the scattered entries,
  the patterns, an external injection text and the new standing set, at the hub's budget and a wide one.

## 0.2.0-beta.51 (pre-release)

- **The profile synthesis is no longer cut off.** Its reply bound goes from 1,600 to 4,000 tokens: with a coherent
  description per facet the live replies ran to 2,500–3,200 and were cut at the bound, so the summary fell back to
  the patterns strung together.

## 0.2.0-beta.50 (pre-release)

- **Recent highlights again.** With the overview feeding the profile summary, a facet with a pattern dropped its
  scattered entries, the current-work ones the recent highlights come from among them, so the live summary had none.
  What the user is doing now is kept.
- **Profile page on a phone.** Under each facet's description, at most four sources, each a one-line title cut with
  an ellipsis (a long line was clipped at the screen's edge); a cross-event pattern is marked and opens the periodic
  overview (it opened an entry page that did not exist).

## 0.2.0-beta.49 (pre-release)

- **The weekly report waits for the running overview.** A pass is recorded as tried when it starts, so the report
  took a running week's overview for a failed one and went out without its section (2026-W47). It now waits until
  the pass ends; a failed pass still does not hold it.
- **The profile summary from the overview is synthesised.** After an overview pass the summary is written again with
  the daily pass's one synthesis call, not left as the patterns strung together until the next daily pass.
- **Recent highlights: only what the user is doing.** Only ⑥ entries of a doing or focus type (current work, focus,
  goal) make them; a ⑤ line (a repository's status, a decision on a rule, a colleague's commitment) no longer does,
  even in the user's own words.

## 0.2.0-beta.48 (pre-release)

The periodic cross-event overview (docs/periodic-overview.md): a layer above the daily curation that finds the same
way of deciding across projects, people, sources and weeks, checks it against counter-examples, and keeps it in its
own file. The daily curation and `state.json` are unchanged.

- **Weekly synthesis (L1).** After the week's curation pass, each of the six facets' classes and actions with
  evidence in the four weeks before are projected by time, project, people and source kind and synthesised into
  patterns (at most two calls per facet). A pattern is used only when code counts at least two scenes (projects or
  source kinds), three independent quoted unit·days and two ISO weeks, a capable stranger's default that differs, and
  a counter-example search (likely contradicting entries over sixty days, judged by a model) that does not overturn
  it: an exception makes it `scoped`, recent counter-examples at 25% or more make it `drifting` (not used).
- **Monthly consolidation (L2).** On the first Monday of a month: quiet but often-seen classes looked at again, alike
  patterns merged, the counter-search again for every pattern through the gate; drifting two months running →
  retired, project-bound and quiet for ninety days → `era_closed`. A principle is no longer dropped by the 40-day
  dormancy alone.
- **Light refresh (L3).** On every published snapshot, without a model: a pattern whose evidence the user marked
  wrong, deleted or forgot is counted again and leaves the exits at once; marking it right brings it back.
- **Exit.** Used patterns are answered by `memory.entries` beside the curated entries, connector and contribution
  ids unchanged: `ov:pattern:<id>` standing, rank 950–985; a pattern resting on any categorised evidence or a
  restricted space as `ov:cat:<category>:<id>`, never standing, rank 900, with every category and the space.
- **Weekly report.** The push is a summary of at most 300 characters (the hub refuses a body over 1000, which is why
  the 2026-W47 report was not sent); the full report, with the overview's section, is on the new start-tab page
  Periodic overview, with the monthly ones.
- **Forward check and spot check.** Before each weekly synthesis, the patterns kept so far predict the week's real
  choices (two options, scored by code) against the scattered entries in as many characters and against no memory.
  `enkeep_overview_run` also runs a backtest (patterns from before a week only) and a model's blind spot check kept
  for the user to go over.
- **Tools.** `enkeep_overview_status` (read) and `enkeep_overview_run` (write: weekly, monthly, backtest, audit, or
  sending a week's report again).
- **Models.** The overview's calls name their own chain (`model.Request.Models`): Claude only, a configured long or
  short model of another family is not used for it. Within the shared limit (four at once).
- **Profile summary.** `OverviewBackedProvider` (`internal/overview`) feeds the six-facet summary from the used
  patterns first; a facet with a general pattern no longer lists the scattered entries it was made from, a facet
  with none falls back to the settled entries. The synthesis writes each facet as a coherent description built from
  the patterns, not a few entries strung together (summary version 2, so every summary is written again).
- **Recent highlights are the user's own.** Only ⑥ entries and ⑤ lines in the user's own words with no one else's
  make them, and the model drops any item about what someone else did, arranged or is pushing.
- **One `memory.entries` entry per facet.** `profile:<facet>` carries the facet's description alone, with its
  subjects, and is recalled on its own (not standing: `profile:overview` is the standing one).
- **`page` command.** Also draws the profile summary, the periodic overview and the latest weekly report.

## 0.2.0-beta.47 (pre-release)

Fix View schema validation failures (HTTP 502) on subpages with large data sets:

- **Review tab (`tab=review`)**: paginate pending proposals (100 per page via `Sub` offset) instead of emitting every
  pending proposal at once, which exceeded the hub's 300-item limit.
- **Judgement and workflow facet tabs (`tab=facets`, `facet=judgement|workflow`)**: enforce a page-wide component budget
  across all sections and paginate type groups via `Sub` offset so facets with hundreds or thousands of types stay well
  under 300 items.
- **Run report detail (`tab=reports`, `open=<run>`)**: split reports over 18,000 runes into multiple `markdown`
  components (within `MarkdownComponent.text`'s 20,000-char limit), handle empty reports, and fall back to plain text
  chunks if a report contains disallowed markdown.
- **Preventive schema hardening**: cap person card rows (`cardPage`), entry counter-examples (`entryPage`), graph node
  assertions (`nodePage`), refusal table rows (`health`), legacy judgement rows/members, and long quotes/fields; give
  `open:card`, `open:u7` and `open:u8` rows distinct action IDs.

## 0.2.0-beta.46 (pre-release)

Six-facet user profile summary, mobile-friendly subpage, and `memory.entries` export (no `state.json` schema change; no external seeds required).

- **Six-facet profile summary (`internal/profile`).** Compiles settled entries across all six facets (`interaction`, `tools`, `judgement`, `workflow`, `world` + factual `people`, `persona` + recent highlights) into a readable "Who I am" overview persisted in `profile-summary.json`. Refreshes after each daily pass (single model synthesis call when the input fingerprint changes, with automatic fallback to deterministic aggregation) and initializes deterministically on startup when absent.
- **Pluggable input source (`InputProvider`).** Decouples summary generation from `store.State` via `InputProvider` (default `EntriesProvider`), so future periodic cross-event overview pattern entries can be plugged in without rewriting the subpage or `memory.entries` outlet.
- **Scene-graded privacy isolation.** General overview and six-facet summaries include only non-sensitive general entries (`categories: []`, excluding `people` evaluations/inferences and `life` entries in work-only mode). Sensitive categories (`health`, `finance`, `relationships`, `emotion`, `legal_id`) are isolated per category with full `categories` and `origin_space` metadata and `standing: false` so the hub's scene filter withholds them on lockscreen, outbound, dispatch and third-party scenes.
- **Mobile-friendly profile subpage (`page_profile.go`).** Adds a "Who I am (profile summary)" entry card on the Start tab and a vertical, mobile-readable subpage with overview, recent highlights, all six facet sections, blank-user onboarding guidance, sensitive-section notice, and an instant refresh button.
- **Native `memory.entries` export (`internal/memsrc`).** Exports `profile:overview`, `profile:<facet>`, and scene-gated `profile:cat:<category>` through the existing `memory_sources` / `memory.entries` protocol without changing connector or contribution IDs.

## 0.2.0-beta.45 (pre-release)

Recall denoising, priority ranking and dormancy tuning (outlet filtering only; no entries are deleted from state).

- **Filter low-value weak relation triples from `memory.entries`.** Bare world-facet relation triples with weak
  predicates (`depends_on`, `collaborates_with`, `about`, `member_of`, `same_circle`, `serves`, `assigned`, `allocated`,
  `waiting_on`, `blocked_by`) are excluded from `memory.entries` unless explicitly pinned, edited or confirmed right by
  the user. Substantive relations (`owns`, `decides`, `decided`, `reports_to`, `mentors`, `means`, `committed`) and all
  underlying state entries are preserved.
- **Re-rank `memory.entries` by usefulness.** `Rank` (0–1000, pinned 1000) now weights facet tier (prioritizing
  `interaction`, `judgement`, `workflow`, `tools` and `persona`, balanced with substantive `people` and `world`
  entries), generalization grain (`abstract` > `class` > `durable` > `action`), user authority (self-reported,
  user-edited or confirmed), evidence strength and status so the hub's 1,000-entry fetch keeps the highest-value
  entries first.
- **Tune dormancy thresholds.** One-off/transient entries now go dormant after 90 active days (aligned with
  `ruleDormantDays` and `roleSeedDays`), and established/durable entries (`class`, `abstract`, `durable`, self/person
  facts or multi-day entries) after 180 active days, preventing established memory from sleeping prematurely after 40
  active days.

## 0.2.0-beta.44 (pre-release)

The tools follow the hub's rules for a memory extension (Persona #13532, #13586: `extmgr/memory_sources.go`
`memoryWithheld`, `extension_assistant.go` `assistantMCPServers`). With beta.42 every `enkeep_*` tool was a read tool
of the connector, so declaring `memory_sources` withheld all of them from the assistant's conversations; this
version reads only through `memory.entries`, and the writes are writes.

- **No recall tool.** `enkeep_recall` is removed: the hub reads the entries with `memory.entries`, puts what each
  scene may use in its context, and `decisions_query` recalls more (`memory: [{id, text}]`, ids as
  `memory-extension:<id>`). The per-turn block's look-up line and the package skill now point to `decisions_query`.
- **Writes are actions** (capability `actions`; `actions.list`, `actions.describe`, `actions.do`): `enkeep_rerun`,
  `enkeep_resettle` (applies; no `dry_run`), `enkeep_seed_import` (imports; no `apply`) and `enkeep_mark_wrong`
  (takes `memory-extension:<id>` too). The hub runs them only through its gate: a card on the phone, described from
  the checked arguments and the pages' snapshot (never the store's lock), unless a `connector_gate` rule allows the
  write. A repeated `request_id` answers the first result.
- **Reads are read tools, for coding Agents** (`agent_tools`): `enkeep_rerun_status`, `enkeep_seed_status`, and two
  new previews split off the writes, `enkeep_resettle_preview` (what `dry_run` reported) and `enkeep_seed_preview`
  (what `apply: false` reported). The writes are in `agent_tools` too, as every tool was before. The hub does not offer
  the reads in the assistant's conversations, by design. The connector id and the contribution keys are unchanged.
- **`memory.entries` is tagged.** An untagged page puts none of its entries in any scene (the hub takes each for
  sensitive everywhere), and beta.42's pages were untagged while any entry read before the 0.2.0-beta.40 tagging rules
  was in the pool. Such an entry is now tagged on reading by the same rules on what it carries: its old model label,
  the money words of its text and quotes when it has no label, the identity-paper and legal words, a class's members'
  categories and space, the space's default categories. Its space is the main one unless it or a member says otherwise
  (no unit carried its space before beta.40). The state is not changed. `enkeep_rerun_status` says how many were
  tagged so.

## 0.2.0-beta.43 (pre-release)

Only a tool schema fix; nothing else changes.

- **`enkeep_seed_import` no longer lists an empty string for `expiring`.** Its enum was `["", "recent", "role"]`;
  Gemini rejects an empty enum value (`enum[0]: cannot be empty`) and then refuses every request of a session that
  has the tool, so a Gemini agent with Persona's MCP tools could not call its model at all. The enum is now
  `["recent", "role"]`: leave `expiring` out for no expiry. A caller that still sends `""` imports as before.
  A test checks that no tool here declares an empty string or nothing in an enum.

## 0.2.0-beta.42 (pre-release)

Only the memory source; nothing else changes. This version does not read the unified entries ledger, does not add
to people and things, and carries none of the version planned as beta.42 before (declined by the user on 2026-11-22).

- **The memory source (`memory_sources`, `memory.entries`; Persona #13532, protocol of PR #13547).** The hub can read
  the curated entries one by one, read-only, and grade them by space and scene itself. A page is ordered standing
  first, then by rank, then newest; it ends early with `has_more` and `after` to stay under 1 MB, and is answered from
  the last published snapshot (the call has 5 seconds; a run holds the state's lock for its whole pass).
  - Every entry the text outlets could carry is answered, not filtered by scene: live, not an evaluation or inference
    about a person, not life content when the user chose work only. `status` is `active` for what the exits use today
    (stable, shifting, pinned, written by the user), else `candidate`, `dormant` or `retired`. `standing` is what goes
    into the resident block (the exits' own gate, K22 included). `rank` is the exits' order (sightings × source kinds,
    pinned 1000). `subjects` are the card's person, the object and the scopes' names.
  - `tagged` is true only when every entry in the pool had its categories and space decided by the 0.2.0-beta.40
    rules (a new mark, `tagged`, set when an entry is read or imported, and when one is read again; a class when all
    its members are). Entries from before carry no such decision, so the page says false, leaves out `origin_space`
    and `categories` everywhere, and nothing is made up. `enkeep_rerun_status` says how many are not tagged.
  - The types are local (`internal/memsrc`) until the SDK copy carries `wire.MemoryEntriesParams`,
    `MemoryEntriesResult` and `MemoryEntry`.
  - The four text outlets (the instructions, the project files, the Inbox context, `enkeep_recall`) are unchanged.
  - `enkeep_rerun_status` reads the source as the hub would and says what came back and how long it took.

## 0.2.0-beta.41 (pre-release)

- **Consolidation judges the facets at once.** The daily pass's filing step asked the model one batch at a time
  (about 47 s each), the slowest serial stretch of a rerun day. With the daily pass's workers (4) the facets now have
  their calls out together; each facet's batches stay in order, and building a batch and applying its answer still
  take turns, so what is filed is the same. The process limiter still caps every model call at 4 at once and 20 a
  minute, with the scheduled daily and weekly passes ahead of the reruns.
- **A forced rerun checks the stored answers again.** `enkeep_rerun` with force reused the answers of units read
  before and skipped them, so a day queued after a switch changed showed nothing new. A forced day now runs the checks
  over the stored answers again, without a model call: the answer's old rejections go and what passes now is kept
  (a sighting kept before is found again, not counted twice).
- The daily run logs where its time went (read and triage, digest, consolidate, signals and exits), to find the time
  that is not a model call.

## 0.2.0-beta.40 (pre-release)

- **The ledger as the hub defines it.** The user's entries are read with the RPC `assistant.entries.list` (grant
  `assistant.entries.read`, persona #13106): pages by `after`, the time is `at`, the status says superseded, expired or
  wrong, probes carry the tag `probe`. The legacy route with `cursor` and `created_at` is gone. No personal entry
  visible (the hub shows them only to an extension proven to run on the assistant's computer) is a normal empty read;
  `enkeep_rerun_status` says what the last read found. An owner's "wrong" mark (`assistant.entries.changed`) makes the
  next read leave the entry out. The grant and the event are declared once the SDK's manifest schema (synced from
  persona #13106) lists them; until then the read answers "not granted", which is the same empty read.
- **Every space is collected; categories decide use by scene** (unified memory design v2 §3.2, §3.3b, §8). The
  health space's sessions, cards and to-dos are read like any other; nothing is dropped for being sensitive any more
  (K13 decided what was collected; it now decides nothing and is off the page). Entries carry their source space
  (`origin_space`) and categories (`categories`: health, finance, relationships, emotion, legal_id, each with who
  tagged it: the model's label, money words, identity and legal words, the space's default, or the user). The
  daily prompt is unchanged (daily/16).
  - Resident exits (the per-turn rules, the agents' rules, the profile and project files, the person pages) take an
    entry in a category only once the user turns on the new per-category switch K22.<category> (all off), or when
    the user wrote it about themself; a space's content goes there only as a way of working with the AI or as what
    the user is like. A class carries its members' categories.
  - `enkeep_recall` gives an entry in a category only when `categories` names every one of its categories, and
    shows them on the line; evaluations of and inferences about people never.
  - The weekly report and the commitments list leave entries in a category out.
  - The entry page shows the categories and the space, and the user can add or take off a category (only the user
    can take one off; one taken off is not added back by curation).
  - Seeds (E9): an item in a category is imported tagged, no longer left out; credentials and unconfirmed items
    still are.
  - Credentials are never kept, as before. The privacy words setting is renamed 不读名单: messages with them are
    still not read.

## 0.2.0-beta.39 (pre-release)

- **The tab bar fits a phone.** Eight tabs did not fit an iPhone's width; the app then draws them as a
  sideways-scrolling bar, which iOS shifts down by the navigation bar's height and draws under the first cards (half
  the labels hidden, "起步" twice). The bar now has five short tabs (起步 · 六面 · 人物 · 核对 · 待确认); the
  switches, the reports and the older pages are opened from a "更多" section at the end of the start page. The
  section under the bar is titled "各方面进度", no longer "起步" as the first tab is.
- **The start card says what its numbers mean.** Each facet's row counted every live entry not stable as "正在攒",
  dormant ones and the concrete actions under a class included, so ③ read 56 against 1411. It now shows the
  settled entries and those being watched (candidates, a class's actions not counted), and how many of those were
  seen more than once; the footer says once what "观察中" is and that the user has nothing to do. The line "离成条目最近
  的一条还差 1 次", on every row, is gone: it was right (almost every candidate was seen once, and two sightings on
  two days settle it) but said nothing.

## 0.2.0-beta.38 (pre-release)

- **Only days really read count.** A day the model never read (no model, or every unit screened out before any
  call) is no longer an active day, and statuses are settled again after every rerun step, not only when the
  queue empties: a long backfill cannot put entries to sleep on days still waiting.
- **Who said it, in three values.** Every line is the user's, the user's words relayed verbatim (verified), or
  someone else's (an agent, Persona, a probe, pasted material); what cannot be verified is never guessed. A task
  prompt or a relay Persona typed into a session stays Persona's, but the user's words it says it carries (a "用户原话"
  block, "用户说「…」") are now each a line of their own, kept as the user's when the day's own lines of the user (or,
  once the hub has it, its ledger of the user's entries) bear them out; before, K20 dropped them with the relay. The
  correction ledger reads the same way: a session Persona started, a relay or a probe no longer counts as the user's
  correction, and pasted material is taken out. In groups, words in a quote block or a referenced message are not the
  sender's (as in mail).
- **Extraction prompt daily/16.** Each of the six facets gets one line of what counts and a counter-example, at equal
  length. A unit over 12k tokens is read in parts that keep 1k tokens before and after each cut (the same observation
  seen in two parts is one). A short reply of the user's to the AI ("？？？", "又来了", "不是这个") goes to the
  correction judge whatever its words. Days already read are read with daily/16 only when rerun.
- **Marked wrong is a counter-example.** New evidence the consolidation matches with an entry marked wrong no longer
  adds up on it (and so cannot bring it back): it starts an entry of its own marked "once wrong", which earns its
  status from its own evidence.
- **One limit on model calls.** Every call of the process (daily, weekly, reruns, synthesis, exits, trials) waits for
  a turn: at most 4 at once and 20 a minute; the reruns wait behind the scheduled passes, and the weekly pass, once
  due, runs before them. The daily pass makes 4 calls at once; offline trials keep to at most 4 workers.
- **The cost line.** Calls and tokens are counted per day and per model, the reruns apart: each day's daily run notes
  the day's line and the totals, and `enkeep_rerun_status` shows today's and yesterday's lines, the totals, the call
  limit's state, and the reruns' progress (days done, days left, new usable entries resting on the days read). After
  every rerun step, every entry's status is settled again (what `enkeep_resettle` applies).
- **Seeds.** `enkeep_seed_import` imports the user's own profile and rules confirmed elsewhere, in a general format;
  it previews by default and writes with `apply`. Items in the four sensitive categories (by category, keywords and a
  model's reading), with a credential or not confirmed at the source are never imported; an item the curator already
  has marks that entry as confirmed; the rest become candidate entries of origin seed, which reach the exits only
  once the user's own records bear them out on an active day. A hard rule is not turned by counter-examples alone; a
  dating item (what the user does now, a role) goes dormant after 30 or 90 active days with nothing new.
  `enkeep_seed_status` says where the seeds stand. Seeds as a source are switch K21, on by default; a user with no
  seeds has nothing to do.
- **The hub's ledger, when it comes.** The extension reads `GET /api/v1/ledger?by=user` for the user's entries
  (quote, relayed_via, supersedes, expires_at, tag; probes and automation left out) to verify relayed words; a hub
  without it is skipped without a word.
- **The overview card counts entries.** The home card counted the old items (facts, practices and knowledge with
  status active or established), which the current curation no longer fills, so "做事方式" and "知识" read 0. It now
  shows the entries per facet that may go out to the assistant (live, stable, not sensitive: the exits' own gate) for
  the six facets of the plugin page, under the page's names (① 和 AI 的交互 … ⑥ 关于你), then 待确认 and the last
  daily pass in a second row (a stat grid holds six at most). People cards are not counted (they have their own tab).
- **The tools answer while a pass runs.** A daily pass holds the state's lock for its whole run, tens of minutes with
  a real model, and `enkeep_rerun_status`, `enkeep_recall` and `enkeep_mark_wrong` waited for it until the hub gave
  up. The status and `enkeep_recall` now read the pages' snapshot (the status says the snapshot's time and the pass in
  progress with its stage); `enkeep_recall`'s counts moved from the state to `recall.json` beside it (an upgrade moves
  them); `enkeep_mark_wrong` waits a few seconds, then queues its marks in `marks-queue.json` and applies them as soon
  as the pass lets go (or at the next start); `enkeep_rerun` and `enkeep_resettle` wait a few seconds, then say which
  pass is running.
- **A withdrawal is no longer held back by an addition.** Exit A is written at most once a day unless the change
  only takes entries out; a compile that withdrew an entry and also added one waited for the next day, so an entry
  marked wrong stayed in the assistant's block. Now the file is written at once whenever the one out there carries an
  entry that may no longer go out.

## 0.2.0-beta.37 (pre-release)

- **The long-term layer, said plainly.** The extension curates what holds over days; near-real-time working memory
  (what was decided an hour ago, which task runs) is the hub's to bring back after compaction. A ledger of the
  user's decisions, once the hub has one, will be read as a source the next day (interface in
  [docs/exits.md](docs/exits.md)); nothing is implemented for it yet.
- **No more ways-of-working skills.** The `enkeep-ways-of-working-<slug>` skills were loaded no time at all and agents
  never see them. ④ entries that hold across projects go into the per-turn block, those of a project into its
  project block, the rest stay in `enkeep_recall`; an upgrade removes the old directories. `enkeep-machines-and-tools`
  stays, a look-up handbook. The per-turn block is still at most 6 KB; its sections share it by proportion (① 22%,
  ② 8%, ③ 22%, ④ 18%, ⑤ 12%, ⑥ 18%), what one leaves going to the next. An action-grain judgement with its fork,
  choice and what was not chosen or why may go in. What holds only in a known project leaves the per-turn block.
  ⑤ adds a names index (K18) and a fixed line pointing at `enkeep_recall`.
- **`enkeep_recall`** (connector `enkeep`): look up curated entries about a person, a project, a system or a kind of
  task; candidates and dormant entries ranked lower; never a card's evaluation or inference. Only per-day counts are
  kept, shown by `enkeep_rerun_status`.
- **`enkeep_mark_wrong`**: the assistant or an agent marks entries wrong (by id, with a reason) without a card on the
  user's phone; recorded as the assistant's, undone by marking right on the page.
- **Who said it.** In a session Persona started every user-role line is Persona's (K20; before, only the first);
  texts with the hub's context block, the task-handoff boilerplate, a cross-session message or a marked relay are
  Persona's anywhere; short test prompts ("reply only OK, call no tool") are test prompts; pasted blocks are
  material. None of them teach anything about the user.
- **Agent project context reaches the agents.** Project files were named by an ASCII slug with a hash for any
  non-ASCII name, which no knowledge-base slug matched, so projects with Chinese names never got their block. Files
  are now named by the knowledge-base project's id (the slug the hub looks up), entries matched to projects by name,
  alias or id; a name the knowledge base does not have gets no file. ① entries of a project go to its block too, after
  a short general part (K19).

## 0.2.0-beta.34 (pre-release)

- **Sessions seen before are read on reruns.** A runtime lists only its recent sessions (enkeep: active in the last 7
  days, at most 80), so a rerun or replay of an older day did not find that day's agent sessions in the hub's list.
  Every session a pass found in the list is now kept (`seen_sessions` in the state: name, start, last active day) and
  a day up to its last active day reads it too; sessions read before this version are known from their cursors and the
  units a day admitted. The hub forwards a session's items to its runtime whether it lists it or not. Any runtime's
  sessions, not one's in particular. A session no pass ever listed is still not known.
- **The hub's session history is read** (nwverse/persona#11529: `GET /api/v1/sessions?include_inactive=1&since=…`):
  every pass also lists the sessions active from the day's start on, archived, finished and offline ones too, every
  page, and reads them with the live list's and the ones seen before, each once. Scheduled runs (`origin`
  `automation`, or no message of the user's) are left out; sessions that do not say (the node's own Claude Code and
  Codex sessions) are read, their speakers told apart as before. A source that did not list the window in full (not
  honored: unsupported, failed, node_unsupported, offline; or not complete) is recorded for the day and named by
  `enkeep_rerun_status` (day, machine, source, reason); its sessions are read as far as the live list and the seen
  ones have them, and a later pass of the day (a rerun) fills them in. A hub without session history (no `history` in
  its answer, or the query turned down) is read as before, and the status says so. A history row whose items the
  hub no longer lets the extension read (it keeps that for a day, in memory) is listed again and read once more.
  A session read from its start is not asked for on days before its first item.
- **A rerun of a day adds to what it admitted.** Before, it replaced the day's list of admitted units, so the units an
  earlier run read and the rerun no longer saw were dropped from it. Forgetting a source takes its units off every day.
- **A hold of the hub (`model_paused`) is a wait, not a failure.** The call waits until the hold lets go (the hub's
  `until`, at most 5 minutes a wait, 10 minutes a call, ending with the run) and asks the whole chain again, rather
  than count every candidate as unavailable. A call still held then puts its unit back: the day stays open and runs
  again when the hold lets go (daily, triage, consolidation, signals and exits alike), with no failure counted and no
  error logged; the exits stay as they were rather than go out with lines missing. `enkeep_rerun_status` counts holds
  apart from unanswered calls; the page says the day waits for the hold.

## 0.2.0-beta.33 (pre-release)

- **A unit the model could not read keeps its day open.** Before, a unit whose call failed with an error other than
  the model being unavailable was counted in `digest_errors` and passed over, and the day was finished and marked read
  with the current prompt, so a rerun skipped the unit for good. Now the day is not done, as when the model is away:
  what was read is kept, the pass waits with the same backoff, and the day runs again from where it stopped.
- **A chain with a candidate not offered and one not matching the schema** (`model_candidates_failed` with only
  `model_unavailable` and `invalid_output`) asks the purpose's model the old way, whose reply comes back even when it
  does not match, for the step's own checks of each item, as when every candidate did not match. Before, the call
  failed and the unit went unread. Seen live: Gemini not offered, Haiku adding a `summary_placeholder` key to a
  summary point. That key is the model's, not the schema's (every summary point is `{text, evidence}` with
  `additionalProperties: false`); the daily prompt will say so with its next version, and the item checks already
  keep a point's text and evidence and drop the rest.
- **`enkeep_rerun` takes `force: true`**: the days are read again even if they already carry the current prompt's
  marker (still past days only, at most 31 per call). Units read before come from the ledger, so only what was not
  read calls the model. `enkeep_rerun_status` lists the days waiting with force; a day leaves the list once its daily
  run completes.

## 0.2.0-beta.32 (pre-release)

- **Builds on an SDK copy from before candidate models.** The candidate parts of model.complete (`models`, `candidate`,
  `attempts`, `model_candidates_failed`'s data) are declared in the extension (`internal/model/candidates.go`) rather
  than taken from the SDK copy, so a repository that has not synced its SDK builds it unchanged. The calls on the wire
  are the same as 0.2.0-beta.31's.
- The size line's setting is titled 「长短分界」 (no implementation word in a setting's title).

## 0.2.0-beta.31 (pre-release)

- **The hub runs the chain.** A call hands its whole chain (`model_primary`, then `model_long` or `model_short` by
  size) to model.complete's `models` (persona #10398), and the hub tries the candidates in order. Each candidate may
  take `model_timeout` seconds (180; the schema's second try included), which replaces 0.2.0-beta.30's
  `model_primary_timeout`. What is counted and logged is what the hub says: the candidate that answered, the model the
  upstream named, and each failed candidate's code and reason from `attempts` / `model_candidates_failed`. When the
  user switched the lists off or replaced them on the purpose, the call is noted "candidates off" or "the user's own
  list". When no candidate matched the schema, the purpose's model is asked the old way and its reply goes to the
  step's own checks, as before. A hub without `models` still gets the old call.
- The SDK copy is synced from persona f7c715f076 (`ModelCompleteParams.Models`, `ModelAttempt`,
  `ModelCandidatesFailed`, `ModelNotOffered`).

## 0.2.0-beta.30 (pre-release)

- **Model routes: a primary model first, a fallback by length.** On a hub whose model.complete takes `models`, every
  text step (daily extraction and triage, weekly consolidation and synthesis, people cards, signals, exit rewriting)
  asks `model_primary` first (none by default: set it to the connection's fast model, a Flash-class one, say). When it fails (an error, a timeout after
  `model_primary_timeout`, 120 s; an empty answer; an answer that does not match the schema; a refusal; the model
  paused), the call goes to `model_long` (default `claude-sonnet-5-5`) if its estimated size is above
  `model_long_above` (16000 tokens), else to `model_short` (default `claude-haiku-5-5`). The estimate is the input
  (system prompt, user turn and schema: a quarter token per ASCII byte, one per other character) plus the room the
  step asks for its answer (`max_tokens`). On a hub without `models`, or when none of the route's models is on the
  connection, a call runs on the purpose's model as before; `model_route: purpose` turns the route off. The exits'
  format does not change. See [docs/model-routes.md](docs/model-routes.md).
- **Each call is counted and logged.** `enkeep_rerun_status` ends with the route and its counts: calls, fallback
  rate, unanswered calls, calls sent the old way, and per model the attempts, answers, failures by reason and average
  time. `model-routes.json` keeps the counts and the last 50 calls, `model-calls.jsonl` a line per call (models tried,
  why each failed, which answered; never what was sent or answered).

## 0.2.0-beta.29 (pre-release)

- **`enkeep_resettle`: settle every entry's status again, as a tool.** The assistant and Agents can call it on the
  `enkeep` connector. It settles every entry (dormant ones too) as of today on the real active days: an active day the
  run ledger proves was never read with the model (daily runs, none with a model call) and no entry rests on is not
  counted, since such days put entries to sleep early before 0.2.0-beta.21. `dry_run` (the default) changes nothing,
  not even the event log, and reports the five statuses before and after, the stable entries per facet and what
  would move; `dry_run: false` applies it. A second call moves nothing. It deletes nothing.
- **Done reruns leave the queue.** Once the queued days are all digested with the current prompt and filed, they are
  taken out of the `enkeep_rerun` queue; their markers stay, so asking for them again still queues nothing, and
  `enkeep_rerun_status` lists only `rerun_range`'s days and what still waits.

## 0.2.0-beta.28 (pre-release)

- **The rerun tools reach the assistant and Agents.** In 0.2.0-beta.27 the connector was `enkeepmemory` and its tools
  `enkeep_rerun` / `enkeep_rerun_status`; the hub keeps only a connector's tools whose names start with its id and
  "_", so it dropped both and no conversation saw them. The connector is now `enkeep`. A test runs the hub's own
  connector checks (`exttest.CheckConnectors`), which fail on such a name.
- The program declares its connector when it starts, as the manifest does; the hub no longer logs that it fell back
  to the manifest's.

## 0.2.0-beta.27 (pre-release)

- **Reruns are a tool, not only a setting.** The assistant and coding Agents can call `enkeep_rerun` (`from`, `to`:
  days to read again with the current daily prompt) and `enkeep_rerun_status` (what is done, what waits, what runs
  now, whether the passes wait for the model or the assistant's computer), through the extension's connector
  `enkeepmemory` (capability `tools`; `agent_tools: enkeep_*`). A call queues the days in the state and returns at
  once; they run as `rerun_range`'s do: one at a time in the background, completed days only, each at most once per
  prompt version, waiting while the model or the assistant's computer is away. At most 31 days per call. A rerun
  reads the user's own sources again inside the extension, sends nothing and deletes nothing, and what it finds goes
  through the same thresholds and review as any day's, so it is a tool rather than a gated write. `rerun_range` (the
  setting) works as before.

## 0.2.0-beta.26 (pre-release)

- **A day the assistant's computer is away for is tried again after a wait.** When the assistant's conversations
  cannot be read because its computer is not connected, the day's pass ends unfinished and waits like when the model
  is unavailable (`model_pause`, now with `what: assistant`), then tries the same day again; before, the day was only
  tried again at the plugin's next start, so a rerun range could pass over it. The page says the assistant's computer
  is not connected, not that the model is unavailable.

## 0.2.0-beta.25 (pre-release)

- **A unit an upstream turns down is split, not lost.** An OpenAI-style upstream answers some requests with HTTP 400
  "the request was rejected as invalid" for what they contain (seen with a GPT model on a group chat: the whole unit
  was lost for the day). That reply, and the hub's `model_refused` code, now count as a refusal: the unit is read
  again in smaller parts, and a part refused at the smallest size is recorded as skipped, as for any refusal.

## 0.2.0-beta.24 (pre-release)

- **People pages carry the user's own words and the inferred profile too.** The hub's privacy filter
  (nwverse/persona#6119) keeps a profile's private parts (`[用户原话]`, `[推断·仅供参考]`) out of conversations that
  write to other people: inbox drafting, the Lark agent and conversations an extension opens. With it, an accepted
  person card writes its evaluations and inferences to the knowledge base as well, not only facts and commitments
  (`ProfileViewsReleased`). `engines.persona` asks for a hub built with the filter (`>=202611122324`); a development
  build (0) is not checked. Not covered by the hub's filter, as #6119 says: a conversation reading `profiles.json`
  with its own file tools, an extension's own MCP tools, and paraphrases.

## 0.2.0-beta.22 (pre-release)

Ready for a GPT model on the knowledge-base purpose (tried with gpt-6-luna).

- **An observation without its sentence is not lost.** A model that fills a judgement's or a way's fields
  (situation, choice, rejected, why; a machine's pitfall and fix) and leaves `text` out gets the sentence made of
  those fields, nothing added. `text` is no longer required by the daily schema, so the hub does not refuse the
  whole answer for it; an observation whose fields do not say enough is still a schema rejection. The prompt
  (`daily/15`) says every observation has `text`, also when the fields are filled. On two replayed days gpt-6-luna
  left `text` out of 35 of 172 answers.
- **A reply that does not match the schema is read, not dropped.** When the hub returns `invalid_output` after its
  second try, the reply it carries is taken and each item is checked on its own, as for any answer; before, the
  whole unit was lost for the day.
- **Room for a reasoning model.** The smallest answer limits (triage 400, weekly 1500, dig and rewrite 2000 tokens)
  are 4000: a reasoning model counts its reasoning in `max_completion_tokens` and could be cut off before answering.

## 0.2.0-beta.21 (pre-release)

- **A day the model is unavailable to is not done.** When the hub cannot run the model (model.complete busy or
  unavailable) before every admitted unit of a day was read, the day's pass ends with `ErrModelDown` instead of
  finishing on triage alone: no finished run is recorded, the first-run replay stays on that day, and a rerun_range
  day is not marked rerun. What was read before is kept (the state is saved after every unit). The passes then wait
  (`model_pause` in the state) 10 minutes, doubling up to 2 hours, and try the same day again; the first pass that
  reaches the model clears the pause and the replay goes on from there. Before, a replay ran on through days with
  no model and counted them as read.
- The plugin page says so: "模型暂时不可用" for the first failures, "模型不可用，暂停中" after three in a row, with
  the hub's reason, since when, the next try and the day it resumes from.
- A hub with no model configured for the extension still triages and checks without one, as before.
- Offline replay: a gateway's 429 (no capacity) through `-pi` counts as the model unavailable, so a replay stops on
  that day instead of reading on without the model.

## 0.2.0-beta.20 (pre-release)

- **Agent cursors cover trailing folds.** A day ending in `Fold[From..To]` resumes after `To`, so a folded Hub or
  offline incremental read cannot refold new work into the previous day and skip it.

## 0.2.0-beta.19 (pre-release)

- **Reruns wait for completed days.** `rerun_range` no longer runs today or future dates. Premature or invalid
  daily/synth records and stale rerun markers no longer block the formal daily recovery or post-rerun synthesis;
  synthesis must follow the latest valid daily run.

## 0.2.0-beta.18 (pre-release)

- **Cancellation no longer drops correction issues.** After a successful gather, `signals.Run` checkpoints
  `signals.json` on every later return. A later state save no longer loses those issues to cancellation alone. The
  original cancellation and checkpoint errors remain visible; gather failures still leave partial cursors unsaved.

## 0.2.0-beta.17 (pre-release)

- **Mining failures are not successful digs.** A `signals_mine` error or malformed answer is recorded as `DecSkip`
  with a bounded, redacted reason. The segment's read bytes stay counted, a valid empty answer stays `DecDig`, and
  the existing terminal Dig ID and waiting-hub behavior are unchanged.

## 0.2.0-beta.16 (pre-release)

- **Truncated assistant memory is not read as complete.** Files whose bodies the hub cut or omitted are skipped
  without caching or advancing a cursor. Complete files in the same response are still read, and a later complete
  version becomes eligible.

## 0.2.0-beta.15 (pre-release)

- **A Persona-started session keeps its speakers straight.** Its first message is labelled Persona rather than the
  user. Only the actual first item of a session read from the start changes; later user messages and incremental
  pages keep the user's attribution.

## 0.2.0-beta.14 (pre-release)

- **Private recordings stay private on direct reads.** `Get("recordings", id)` checks the privacy metadata returned
  by that read even when no `Recent` catalog pass has marked the ID. A private detail returns not found; a public
  recording still needs only one underlying read.

## 0.2.0-beta.13 (pre-release)

- **Concurrent tool calls keep their own results.** Signal digging pairs `tool_use` and `tool_result` by the hub's
  Call ID, so one call's failure and result sequence cannot be attached to another concurrent call. Results without
  a Call ID keep the existing sequential fallback.

## 0.2.0-beta.12 (pre-release)

- **Classes merge as broadly as before.** 0.2.0-beta.11 asked the merge for narrow kinds, and far fewer classes
  merged (machines-and-tools classes of the same pitfall stayed apart and never became stable). The merge prompt is
  back to broad kinds; the program still refuses, in ①③④⑥, a merge whose sentence needs two or more alternatives.
- **The AI's lines supply no detail.** When a fact's detail is in another line of the conversation, only the people's
  lines are cited for it, never the assistant's or an agent's draft, which a fact never rests on.

## 0.2.0-beta.11 (pre-release)

- **The user as a person has a test of its own.** A ⑥ line is not asked at which moment the assistant would act
  differently (understanding has no such moment) but whether it is a specific fact about this user: background, role,
  what they work on or aim at, their circumstances, a specific view or habit. Lines true of almost anyone are kept
  out as before (`emptiness/3`; the acceptance tool judges ⑥ lines the same way). A line saying how the user
  handles a kind of task or chooses at a fork is no longer held back for stating "no behaviour of the assistant's":
  the assistant follows it when it does, helps with or plans that kind of task.
- **One plain statement is enough for a fact.** A ⑥ fact the user states about themself (role, responsibility,
  situation, current work, a goal, the background) and a ⑤′ fact or promise on the person's or the user's own line
  are stable at once; views, values, preferences, habits, evaluations and readings still need a second sighting.
  The per-turn instructions take such ⑥ facts as well as classes.
- **Details checked further.** One-digit numbers count as details too (a quote's Chinese numerals match: "两" carries
  "2"); common words of software work ("issue", "release", "URL"…) do not, since a claim uses them for a quote's own
  wording. A rejected item's log line names the detail no line has. CJK descriptions are not checked word by word:
  a clause check refused mostly correct facts in other words.
- **No "A or B or C" classes.** In the facets of situations and ways (①③④⑥), merging classes whose shared sentence
  needs two or more alternatives is refused: the classes stay apart instead of becoming one class that says nothing
  the assistant can act on. The merge prompt no longer asks for few, broad kinds.
- **Extraction (`daily/14`).** An item says no more than its quotes, a question, wish, complaint or guess is not read
  as a statement, and a short guide says which facet a line goes to when two seem to fit (a standing request to the
  AI is interaction, a conditional choice is a judgement, an evaluation of a thing is a world fact).
- Acceptance tool: with `-agents-dir`, the fact judge also sees the titles of the agent sessions a fact cites.

## 0.2.0-beta.10 (pre-release)

- **A fact states no more than its lines.** A name in Latin letters, a number of two or more digits or a time of day
  in a fact or a person card must be in a line it cites. When it is in another line of the same conversation, that
  line is cited too; when the conversation does not have it anywhere (the unit's own name and its members' names
  count, the projects it is filed under do not), the item is rejected (`DETAIL_NOT_IN_EVIDENCE`).
- **Exit lines answer the empty-talk question on their own.** After the rewrite, each line the assistant will read is
  asked, alone, at which moment the assistant would act differently without it; a line with no answer is kept off
  the exits (`emptiness/1`). The rewrite (`export/4`) now says what the assistant would otherwise do, and writes an
  interaction entry as an instruction, not as a judgement. The user's own words go out as before.
- **No member twice.** The per-turn instructions leave out an action entry whose class is already there, so the
  section's budget goes to different things.
- **Agents' reports count for machines and tools.** In an agent's session, the agent's own text and its folded work
  are what a machines-and-tools entry may rest on; they were read as an unknown person's and dropped.
- Acceptance tool: the fact judge sees the names of the cited chats and is not handed an empty context.
- R4.overturn counts adoptions overturned: a recommendation taken, then a correction of that fork type's judgements
  before the next one taken, each adoption once. It counted every correction of a judgement entry, and could exceed
  the adoptions it is read against.

## 0.2.0-beta.9 (pre-release)

- **Moved to the public extension repository.** The extension now lives in `nwverse/persona-extensions` and is
  published to the official market; its id stays `enkeep-memory`, so a hub that has 0.2.0-beta.8 from another market
  upgrades in place and keeps its data. Its behaviour is unchanged. The offline replay and acceptance tools no
  longer carry environment defaults: `-model` is required, `-provider` is empty (the `pi` command's own default) and
  `-tz` defaults to UTC.

## 0.2.0-beta.8 (pre-release)

The curator is rebuilt around six things it learns about the user, all equally important, and the people the user
deals with: how the user works with the AI, how the AI works with machines and tools, the user's default judgements,
how the user handles kinds of tasks, the user's world (facts, relations, the landscape) and the user as a person.
Needs a hub from build 202611121555 on (data-directory contributions, read-only memory, question history paging and
decision fields, folded work with error counts).

- **One entry structure for every facet**, at three grains (one action, a kind of situation, an overall tendency),
  with evidence that says which source and who said it, an impact that names which assistant action changes and when,
  counts by chances and active days, an open type table per facet, and an append-only event log that replays to the
  same entries. The user's edits, pins, deletions (tombstones) and right / wrong marks are never overwritten.
- **More sources, read lightly.** Besides the knowledge base and the Persona chats: other agents' sessions (text and
  folded work only; a tool segment is opened only when a friction signal points at it), ask_user cards with their
  recommendations and who answered, to-dos and their decisions, "about me", and the assistant's hand-written memory.
  Every source that is missing (no grant, the hub lacks it, the assistant's computer away) is shown as such and
  nothing fails. A first-run replay of up to 90 days builds the start from the user's own history.
- **Speaker attribution before anything is learned from chit-chat.** Each line is resolved to the user, one person
  of the conversation, or nobody; chit-chat is kept when the speaker is known and the user deals with them, and the
  intersection list decides whom people cards are made for. Per-source attribution checks can stop a source from
  teaching people and relations.
- **One extraction prompt** (English instructions, the user's language as a parameter) for every facet, with real
  impact, language, sensitive-category (off by default, per category) and secret checks.
- **Consolidation**: model-judged recall and merging into kinds of situation, counter-examples, adaptive thresholds
  that count chances and active days, and an abstract gate that does not relax with little data. The old memory layer
  is migrated into entries once.
- **People cards**: role and responsibilities, how they work with the user, the user's own words about them, labelled
  inferences with their basis, and commitments either way; a separate tip for drafting that never quotes an
  evaluation. Facts go to people pages now; the full card waits for the hub's sectioned person profiles.
- **Exits**: a per-turn instructions block (6 KB, a budget per facet, empty sections left out), skills for machines
  and tools and for ways of working, agent project context, inbox context and never-quote lines, each rewritten line
  by line with a basis hash and read back from the hub.
- **The correction ledger and friction digs**: corrections filed against entries or as new issues, recurrences
  counted from when an entry took effect, and pitfalls learned from the segments a signal pointed at.
- **The page**: a start card per facet, a sources card, three skippable first-run questions, the facets and the
  cards, switches K1–K17 (per category, per fork type, per source where they apply), a check page for the user, and a
  weekly report through notify.
- **Acceptance tools**: `review` and `accept` work out the readings, samples and smoke checks from a state directory.

## 0.2.0-beta.6 (pre-release)

- **Proposal undo survives interrupted writes.** Each knowledge-base proposal records its original before-image in a
  durable write-ahead record before `kb.update`. Startup replays interrupted writes in the background without
  blocking initialization, one failed record stays queued without blocking later records, and every automatic
  application checkpoints separately, so retries preserve the value undo must restore.

## 0.2.0-beta.5 (pre-release)

- **Forgetting a source also scrubs migration backups.** `Forget` now applies the same source predicate to every
  `state.json.bak-*` without normalizing the rollback snapshot: unknown fields survive, unrelated backups keep their
  exact bytes and mtime, and only changed backups are atomically rewritten.

## 0.2.0-beta.3 (pre-release)

- **Digest past days again with the current prompt.** New setting `rerun_range` (`YYYY-MM-DD..YYYY-MM-DD`, at most 31
  days): each day in it gets a daily pass with the current daily prompt, one day at a time, once per prompt version.
  What an earlier prompt produced stays as it was (an item the new pass does not produce again is not touched, a
  rejected one stays rejected); a claim it produces again folds into the item that has it. It starts without the
  hub's UI: `personad extension config enkeep-memory rerun_range=…`. The log says
  `daily <day> starts (prompt=daily/6 …)` for each.

## 0.2.0-beta.2 (pre-release)

- **Group noise stays out of the memory.** A group's job postings, shop or campaign activities, giveaways and one
  member's answer to another were listed as facts, practices and knowledge ("待确认", "偶现") when the user neither
  said them nor was named in them. The extraction already throws such items away when they rest only on other
  people's lines that do not name the user (`NOT_ABOUT_OWNER`); a regression test now pins the cases. The daily prompt
  (`daily/6`) says so too, so the model stops proposing them.
- **Items from before the check are not left waiting.** The one-off start-up clean keeps an item that rests only on
  other people's words as a candidate. `clean-sources -reject-others` (extension stopped, `-dry` first) marks such
  items rejected instead: hidden, kept in the state with their citations, and backed up first. It never runs by
  itself.

## 0.2.0-beta.1 (pre-release)

A pre-release: for hubs that choose to try it. It replaces 0.1.x on upgrade (a pre-release is newer than 0.1.1, older
than 0.2.0).

### What changed since 0.1.0

- **Only the user's own words are the user's.** A memory entry rests only on lines the user wrote, and needs one that
  says something of its own (a bare "好" or "对" to the assistant does not count). An item rests on the user's words or
  on a person addressing the user by name. The assistant's replies, its paraphrases ("你之前说过…"), agents' drafts
  under the user's name, text pasted from an assistant, and bots are context, never evidence. Every cited line records
  its role (user, assistant, agent_draft, bot, other) and where it is.
- **Persona conversations** are read through the hub's `assistant.conversations` / `assistant.messages` (permission
  `assistant.read`, new): only conversations the user opened (origin user or channel), only words, and the hub's
  typed-in messages (task notices, supervision, inbox notes, wake-ups) dropped. Agents' own sessions are never read:
  what Persona sends them is role=user with no mark.
- **Tiers by source.** Own conversations give every layer; code review gives the user's own judgements and practices
  only; groups the user takes part in give facts and decisions that concern the user; groups the user keeps quiet in
  give people, projects and their relations only; noise days (feeds, alerts, big crowds) are skipped. Settings:
  `tiers`, `engaged_days`, `noise_members`.
- **Items are tightened.** No one-off actions or passing states, no money notices; two independent sources before an
  item is active or proposed for pinning; pins only on a person or project, never on the source chat; an item a
  memory entry already has is folded into it.
- **Fixes.** Generic words for the user ("用户", "我", "owner") never count as the user's name (mention windows); a
  weekly run touches only its own week; coverage alarms only for days a daily run covered.
- **Settings renamed** so the install preview no longer takes them for secrets: `admit_all_below_tokens` →
  `admit_all_below`, `private_keywords` → `privacy_keywords`. Values saved under the old names still count.
- **Upgrading from 0.1.x** cleans the old state once, on start: `state.json` is backed up in the extension's data
  directory (the newest three kept), and what rested on agents, the assistant or bots goes; the counts are on the
  health page and in the log.

### Needs

- A hub built from nwverse/persona at or after #1292 (`engines.persona >=202611090012`). An older hub shows the
  extension as incompatible.

### Known issues

- **Messages Persona sent as the user are not told apart.** A chat reply, comment or mail Persona sent in the user's
  name through a connector is archived like one the user typed. Only those with a recognisable mark (an agent's
  trace, pasted assistant text, the user named in the third person) are excluded; waiting on nwverse/persona#2814.
- **Mute, fold and done** are read when the hub reports them (nwverse/persona#1261, open); until then a group the user
  has not written in for `engaged_days` stands in for one the user never opens.
- **Background groups keep no entities yet**: the graph keeps people and projects only through relations, and there is
  no relation for "this person is in this group".
- **Items are the weakest layer**: in the blind evaluation (docs/extensions/enkeep-memory-tiers-eval.md) about half of
  them were useful. None is active or proposed for pinning from a single source.
- **Agents' drafts in code review** are recognised by their marks and shape (long, structured, status templates); a
  short drafted comment can pass as the user's.
- **Memory is not delivered to the assistant yet**: injection levels are computed and shown in the extension; giving
  them to the assistant needs hub interfaces that do not exist yet.
