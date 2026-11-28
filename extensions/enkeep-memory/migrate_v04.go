package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	exits "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/graph"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

const (
	migrateV04FlagS3Supervisor = "reason:v04_s3_supervisor_attribution"
	migrateV04FlagCronPrompt   = "reason:v04_cron_prompt"
	migrateV04BatchSize        = 30

	migrateV04DefaultHaikuModel  = "claude-haiku-4-5-20251001"
	migrateV04DefaultSonnetModel = "claude-sonnet-5-5"

	migrateV04DefaultMaxHaiku  = 32
	migrateV04DefaultMaxSonnet = 15

	migrateV04MaxConcHard = 4
	migrateV04MaxRPMHard  = 20
)

var (
	// migrateV04TrackedFiles are the three plugin state files atomically backed up and restored in Phase 0.
	migrateV04TrackedFiles = []string{
		"state.json",
		overview.File,
		profile.FileName,
	}

	// s3SupervisorPersonaRe matches S3/S4-only persona entries where an AI agent/supervisor described its own
	// coordinator role and was misattributed to the user (0.4.0 §3.2 Phase 1 step 2).
	s3SupervisorPersonaRe = regexp.MustCompile(`(?i)` +
		`主管|统筹|总管|协调官|调度员|平台主管|网关主管|项目主管|任务主管|监督者|督导|` +
		`担任.{0,12}(主管|统筹|助手|代理|Agent|bot)|作为.{0,12}(主管|助手|代理|Agent)|` +
		`负责.{0,12}(统筹|派活|调度|巡检|验收|监督)|` +
		`\b(supervisor|coordinator|dispatcher|orchestrator|assistant\s+role|ai\s+agent)\b`)

	// fatalQuotaOrAuthWords identifies upstream quota/rate-limit/auth errors that must abort immediately without retry.
	fatalQuotaOrAuthWords = []string{
		"rate_limit", "rate limit", "too many requests", "429",
		"quota", "credit", "billing", "insufficient", "overloaded", "529",
		"unauthorized", "authentication", "invalid_api_key", "invalid api key",
		"not logged in", "login required", "401", "403", "forbidden", "token expired",
	}
)

// ErrMigrateQuotaOrAuth indicates a quota, rate-limit, or authentication failure that halts migration immediately.
var ErrMigrateQuotaOrAuth = errors.New("migrate-v04 aborted on quota/auth error")

// MigrateV04BackupManifest records exact file digests and absent files for byte-exact rollback.
type MigrateV04BackupManifest struct {
	Timestamp string            `json:"timestamp"`
	CreatedAt string            `json:"created_at"`
	Files     map[string]string `json:"files"`            // filename -> sha256 hex
	Sizes     map[string]int64  `json:"sizes,omitempty"`  // filename -> byte size
	Absent    []string          `json:"absent,omitempty"` // tracked files that did not exist at backup time
}

// MigrateV04StateStats captures before/after counts of state.json for auditing.
type MigrateV04StateStats struct {
	EntriesTotal     int            `json:"entries_total"`
	EntriesLive      int            `json:"entries_live"`
	EntriesStable    int            `json:"entries_stable"`
	EntriesCandidate int            `json:"entries_candidate"`
	EntriesDormant   int            `json:"entries_dormant"`
	EntriesRetired   int            `json:"entries_retired"`
	ByFacetStable    map[string]int `json:"by_facet_stable"`
	S6Handwritten    int            `json:"s6_handwritten"`
	S6AssistantNote  int            `json:"s6_assistant_note"`
	OpsRunbookCount  int            `json:"ops_runbook_count"`
	EphemeralCount   int            `json:"ephemeral_count"`
	SensitiveStable  int            `json:"sensitive_stable"`
	SensitiveByCat   map[string]int `json:"sensitive_by_cat"`
	GraphNodes       int            `json:"graph_nodes"`
	GraphNodesActive int            `json:"graph_nodes_active"`
	GraphTaskActive  int            `json:"graph_task_active"`
	GraphAssertions  int            `json:"graph_assertions"`
	GraphAboutActive int            `json:"graph_about_active"`
	GraphMerges      int            `json:"graph_merges"`
}

// MigrateV04Phase1Report summarizes deterministic Phase 1 cleaning (0 LLM).
type MigrateV04Phase1Report struct {
	S6Downgraded           int                        `json:"s6_downgraded"`
	S3PersonaRetired       int                        `json:"s3_persona_retired"`
	S3PersonaDemoted       int                        `json:"s3_persona_demoted"`
	CronWorkflowRetired    int                        `json:"cron_workflow_retired"`
	GenericPeopleDemoted   int                        `json:"generic_people_demoted"`
	OpsRunbookTagged       int                        `json:"ops_runbook_tagged"`
	LifecycleDecay         entry.LifecycleDecayReport `json:"lifecycle_decay"`
	Graph                  *graph.MergeReport         `json:"graph,omitempty"`
	SensitiveWorkCleared   int                        `json:"sensitive_work_cleared"`
	SensitiveClearedByCat  map[string]int             `json:"sensitive_cleared_by_cat,omitempty"`
	OriginSpaceWorkCleared int                        `json:"origin_space_work_cleared"`
	// LowConfidencePersonaDemoted counts stable persona entries flagged LOW_CONFIDENCE and seen once (not the user's
	// own) demoted to candidate, so a misread never stands in the stable pool or the profile.
	LowConfidencePersonaDemoted int `json:"low_confidence_persona_demoted"`
	// LegalIDFalsePositiveCleared counts non-work entries whose model-made legal_id label was a known false positive
	// (a Visa card payment, "告状") and was taken off.
	LegalIDFalsePositiveCleared int   `json:"legal_id_false_positive_cleared"`
	DurationMS                  int64 `json:"duration_ms"`
}

// MigrateV04Phase2Report summarizes lightweight Haiku disambiguation & audit.
type MigrateV04Phase2Report struct {
	Skipped                 bool               `json:"skipped,omitempty"`
	Tier2Graph              *graph.MergeReport `json:"tier2_graph,omitempty"`
	SensitiveChecked        int                `json:"sensitive_checked"`
	SensitiveFalsePositives int                `json:"sensitive_false_positives"`
	SensitiveBatches        int                `json:"sensitive_batches"`
	BoundaryRulesChecked    int                `json:"boundary_rules_checked"`
	BoundaryTaggedOps       int                `json:"boundary_tagged_ops"`
	BoundaryTaggedUniversal int                `json:"boundary_tagged_universal"`
	BoundaryBatches         int                `json:"boundary_batches"`
	HaikuCalls              int                `json:"haiku_calls"`
	DurationMS              int64              `json:"duration_ms"`
}

// MigrateV04Phase3Report summarizes L1 rebuild, profile refresh and exit compilation.
type MigrateV04Phase3Report struct {
	Skipped          bool   `json:"skipped,omitempty"`
	WeekPeriod       string `json:"week_period,omitempty"`
	VerdictsKept     int    `json:"verdicts_kept,omitempty"`
	PatternsTotal    int    `json:"patterns_total"`
	ProfileCoreUsed  int    `json:"profile_core_used"`
	PrinciplesUsed   int    `json:"principles_used"`
	ContractsUsed    int    `json:"contracts_used"`
	OpsRunbooksUsed  int    `json:"ops_runbooks_used"`
	ProfileMode      string `json:"profile_mode,omitempty"`
	ProfileSections  int    `json:"profile_sections,omitempty"`
	ExitFilesWritten int    `json:"exit_files_written,omitempty"`
	HaikuCalls       int    `json:"haiku_calls"`
	SonnetCalls      int    `json:"sonnet_calls"`
	DurationMS       int64  `json:"duration_ms"`
}

// MigrateV04Report is the full audit JSON emitted by migrate-v04.
type MigrateV04Report struct {
	Mode        string                 `json:"mode"` // dry | apply | rollback
	StateDir    string                 `json:"state_dir"`
	Today       string                 `json:"today"`
	BackupDir   string                 `json:"backup_dir,omitempty"`
	BackupTS    string                 `json:"backup_ts,omitempty"`
	Before      MigrateV04StateStats   `json:"before"`
	After       MigrateV04StateStats   `json:"after"`
	Phase1      MigrateV04Phase1Report `json:"phase1"`
	Phase2      MigrateV04Phase2Report `json:"phase2"`
	Phase3      MigrateV04Phase3Report `json:"phase3"`
	HaikuCalls  int                    `json:"haiku_calls"`
	SonnetCalls int                    `json:"sonnet_calls"`
	StartedAt   string                 `json:"started_at"`
	FinishedAt  string                 `json:"finished_at"`
}

// MigrateV04Options configures a migrate-v04 run.
type MigrateV04Options struct {
	StateDir    string
	DryRun      bool
	Apply       bool
	RollbackTS  string
	MaxPhase    int // 0..3; default 3
	Today       string
	Lang        string
	ReportPath  string
	SkipLLM     bool
	MaxConc     int
	MaxRPM      int
	MaxHaiku    int
	MaxSonnet   int
	HaikuModel  string
	SonnetModel string
	ModelCmd    string
	CacheDir    string
	ExitOutDir  string // optional: write exits.Compile output here in Phase 3
	Model       model.Model
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// migrateV04Cmd is the CLI entry point:
//
//	enkeep-memory migrate-v04 -state <dir> [-dry] [-apply] [-rollback <ts>]
func migrateV04Cmd(args []string) error {
	fs := flag.NewFlagSet("migrate-v04", flag.ContinueOnError)
	dir := fs.String("state", "", "state directory holding state.json (required)")
	dry := fs.Bool("dry", false, "dry run: compute cleaning and report without modifying state files")
	apply := fs.Bool("apply", false, "apply migration to state directory (creates Phase 0 backup first)")
	rollbackTS := fs.String("rollback", "", "rollback timestamp (e.g. 20261127T040000Z) to restore from <state>/backups/v04-<ts>/")
	maxPhase := fs.Int("phase", 3, "highest phase to run (0=backup only, 1=deterministic, 2=+Haiku, 3=+L1/profile/exits)")
	today := fs.String("today", "", "reference date YYYY-MM-DD (default: latest active day in state or local today)")
	lang := fs.String("lang", "zh-Hans", "language for L1/profile/exit rebuild")
	reportPath := fs.String("report", "", "optional path to write JSON migration report")
	skipLLM := fs.Bool("skip-llm", false, "skip model calls in Phase 2 and Phase 3 (deterministic only)")
	maxConc := fs.Int("max-conc", 4, "max concurrent model calls (capped at 4)")
	maxRPM := fs.Int("max-rpm", 15, "max model calls per minute (capped at 20)")
	maxHaiku := fs.Int("max-haiku", migrateV04DefaultMaxHaiku, "hard cap on Haiku calls (0 = unlimited)")
	maxSonnet := fs.Int("max-sonnet", migrateV04DefaultMaxSonnet, "hard cap on Sonnet calls (0 = unlimited)")
	modelCmd := fs.String("model-cmd", "claude", "local CLI command for offline model calls")
	haikuMdl := fs.String("haiku-model", migrateV04DefaultHaikuModel, "model name for Small/Haiku tier")
	sonnetMdl := fs.String("sonnet-model", migrateV04DefaultSonnetModel, "model name for Medium/Sonnet tier")
	cacheDir := fs.String("cache", "", "optional model response cache directory")
	exitOut := fs.String("exit-out", "", "optional output directory for Phase 3 exits.Compile")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dir) == "" {
		return fmt.Errorf("-state is required")
	}
	if *rollbackTS != "" {
		if *dry || *apply {
			return fmt.Errorf("-rollback cannot be combined with -dry or -apply")
		}
		if err := RollbackV04(*dir, *rollbackTS); err != nil {
			return err
		}
		fmt.Printf("migrate-v04 rollback %s: verified SHA256SUMS and restored state in %s\n", *rollbackTS, *dir)
		return nil
	}
	if !*dry && !*apply {
		*dry = true
	}
	if *dry && *apply {
		return fmt.Errorf("specify only one of -dry or -apply")
	}

	opts := MigrateV04Options{
		StateDir:    *dir,
		DryRun:      *dry,
		Apply:       *apply,
		MaxPhase:    *maxPhase,
		Today:       *today,
		Lang:        *lang,
		ReportPath:  *reportPath,
		SkipLLM:     *skipLLM,
		MaxConc:     *maxConc,
		MaxRPM:      *maxRPM,
		MaxHaiku:    *maxHaiku,
		MaxSonnet:   *maxSonnet,
		HaikuModel:  *haikuMdl,
		SonnetModel: *sonnetMdl,
		ModelCmd:    *modelCmd,
		CacheDir:    *cacheDir,
		ExitOutDir:  *exitOut,
		Logf:        func(f string, a ...any) { fmt.Fprintf(os.Stderr, "[migrate-v04] "+f+"\n", a...) },
	}
	rep, err := RunMigrateV04(context.Background(), opts)
	if rep != nil && opts.ReportPath != "" {
		_ = writeMigrateJSON(opts.ReportPath, rep)
	}
	if rep != nil {
		printMigrateV04Summary(rep)
	}
	return err
}

func printMigrateV04Summary(rep *MigrateV04Report) {
	fmt.Printf("migrate-v04 (%s) state=%s today=%s\n", rep.Mode, rep.StateDir, rep.Today)
	if rep.BackupDir != "" {
		fmt.Printf("  Phase 0 backup: %s (ts=%s)\n", rep.BackupDir, rep.BackupTS)
	}
	fmt.Printf("  Entries: total=%d live=%d->%d stable=%d->%d dormant=%d->%d retired=%d->%d\n",
		rep.Before.EntriesTotal, rep.Before.EntriesLive, rep.After.EntriesLive,
		rep.Before.EntriesStable, rep.After.EntriesStable,
		rep.Before.EntriesDormant, rep.After.EntriesDormant,
		rep.Before.EntriesRetired, rep.After.EntriesRetired)
	fmt.Printf("  Phase 1: s6_downgraded=%d s3_persona_retired=%d s3_persona_demoted=%d cron_retired=%d generic_people=%d ops_tagged=%d sens_work_cleared=%d low_conf_persona_demoted=%d legal_id_fp_cleared=%d\n",
		rep.Phase1.S6Downgraded, rep.Phase1.S3PersonaRetired, rep.Phase1.S3PersonaDemoted,
		rep.Phase1.CronWorkflowRetired, rep.Phase1.GenericPeopleDemoted, rep.Phase1.OpsRunbookTagged,
		rep.Phase1.SensitiveWorkCleared, rep.Phase1.LowConfidencePersonaDemoted, rep.Phase1.LegalIDFalsePositiveCleared)
	fmt.Printf("  Decay: ephemeral_tagged=%d demoted=%d dormant=%d ops_demoted=%d ops_dormant=%d\n",
		rep.Phase1.LifecycleDecay.EphemeralTagged, rep.Phase1.LifecycleDecay.EphemeralDemotedStable,
		rep.Phase1.LifecycleDecay.EphemeralDormant, rep.Phase1.LifecycleDecay.OpsDemoted,
		rep.Phase1.LifecycleDecay.OpsDormant)
	if rep.Phase1.Graph != nil {
		fmt.Printf("  Graph Tier 1: tasks_archived=%d about_archived=%d prefixed=%d merged=%d collab_dedup=%d (merges=%d->%d)\n",
			rep.Phase1.Graph.ArchivedTasks, rep.Phase1.Graph.ArchivedAbout, rep.Phase1.Graph.PrefixedCleaned,
			rep.Phase1.Graph.TotalMerged, rep.Phase1.Graph.DedupedCollabEdges,
			rep.Before.GraphMerges, rep.After.GraphMerges)
	}
	if !rep.Phase2.Skipped {
		t2m := 0
		if rep.Phase2.Tier2Graph != nil {
			t2m = rep.Phase2.Tier2Graph.Tier2Merged
		}
		fmt.Printf("  Phase 2 (Haiku=%d): tier2_merged=%d sens_checked=%d sens_fp=%d boundary_checked=%d ops=%d universal=%d\n",
			rep.Phase2.HaikuCalls, t2m, rep.Phase2.SensitiveChecked, rep.Phase2.SensitiveFalsePositives,
			rep.Phase2.BoundaryRulesChecked, rep.Phase2.BoundaryTaggedOps, rep.Phase2.BoundaryTaggedUniversal)
	}
	if !rep.Phase3.Skipped {
		fmt.Printf("  Phase 3 (Haiku=%d Sonnet=%d): patterns=%d profile_core=%d principles=%d contracts=%d ops_runbooks=%d profile=%s(%d secs) exits=%d\n",
			rep.Phase3.HaikuCalls, rep.Phase3.SonnetCalls, rep.Phase3.PatternsTotal,
			rep.Phase3.ProfileCoreUsed, rep.Phase3.PrinciplesUsed, rep.Phase3.ContractsUsed, rep.Phase3.OpsRunbooksUsed,
			rep.Phase3.ProfileMode, rep.Phase3.ProfileSections, rep.Phase3.ExitFilesWritten)
	}
	fmt.Printf("  Total model calls: haiku=%d sonnet=%d\n", rep.HaikuCalls, rep.SonnetCalls)
}

// RunMigrateV04 executes the configured phases of migrate-v04.
func RunMigrateV04(ctx context.Context, opts MigrateV04Options) (*MigrateV04Report, error) {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.MaxPhase < 0 || opts.MaxPhase > 3 {
		opts.MaxPhase = 3
	}
	if opts.Lang == "" {
		opts.Lang = "zh-Hans"
	}

	mode := "dry"
	if opts.Apply {
		mode = "apply"
	}
	rep := &MigrateV04Report{
		Mode:      mode,
		StateDir:  opts.StateDir,
		StartedAt: nowFn().UTC().Format(time.RFC3339),
	}

	// Open state without mutating disk until apply + save.
	stStore, err := store.Open(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	st := stStore.S
	if opts.DryRun {
		// Work on an isolated clone so nothing can accidentally write back.
		st, err = st.Clone(nowFn())
		if err != nil {
			return nil, fmt.Errorf("clone state for dry run: %w", err)
		}
	}

	today := strings.TrimSpace(opts.Today)
	if today == "" {
		if len(st.ActiveDays) > 0 {
			today = st.ActiveDays[len(st.ActiveDays)-1]
		} else {
			today = nowFn().Format("2006-01-02")
		}
	}
	rep.Today = today
	rep.Before = CollectMigrateV04Stats(st)

	// Phase 0: atomic backup (only on -apply; dry-run never touches state dir).
	if opts.Apply {
		ts, bdir, err := BackupV04(opts.StateDir, nowFn())
		if err != nil {
			return rep, fmt.Errorf("phase 0 backup: %w", err)
		}
		rep.BackupTS = ts
		rep.BackupDir = bdir
		logf("Phase 0 backup ready: %s", bdir)
	}
	if opts.MaxPhase == 0 {
		rep.After = CollectMigrateV04Stats(st)
		rep.FinishedAt = nowFn().UTC().Format(time.RFC3339)
		return rep, nil
	}

	// Phase 1: deterministic cleaning (0 LLM).
	p1Start := nowFn()
	rep.Phase1, err = RunMigrateV04Phase1(ctx, st, today)
	rep.Phase1.DurationMS = nowFn().Sub(p1Start).Milliseconds()
	if err != nil {
		return rep, fmt.Errorf("phase 1: %w", err)
	}
	logf("Phase 1 done in %dms: s6=%d s3_ret=%d s3_dem=%d cron=%d ops=%d sens_cleared=%d",
		rep.Phase1.DurationMS, rep.Phase1.S6Downgraded, rep.Phase1.S3PersonaRetired,
		rep.Phase1.S3PersonaDemoted, rep.Phase1.CronWorkflowRetired, rep.Phase1.OpsRunbookTagged,
		rep.Phase1.SensitiveWorkCleared)

	// Prepare guarded rate-limited model for Phase 2 and Phase 3.
	var guarded *migrateGuardedModel
	if !opts.SkipLLM && opts.MaxPhase >= 2 {
		base := opts.Model
		if base == nil {
			haiku := opts.HaikuModel
			if haiku == "" {
				haiku = migrateV04DefaultHaikuModel
			}
			sonnet := opts.SonnetModel
			if sonnet == "" {
				sonnet = migrateV04DefaultSonnetModel
			}
			base = &noRetryCLIModel{
				Command:  opts.ModelCmd,
				Models:   map[model.Tier]string{model.Small: haiku, model.Medium: sonnet},
				CacheDir: opts.CacheDir,
				Timeout:  5 * time.Minute,
			}
		}
		conc := clampInt(opts.MaxConc, 1, migrateV04MaxConcHard, 4)
		rpm := clampInt(opts.MaxRPM, 1, migrateV04MaxRPMHard, 15)
		guarded = newMigrateGuardedModel(base, &model.Limiter{Concurrent: conc, PerMinute: rpm}, opts.MaxHaiku, opts.MaxSonnet)
	}

	// Only flush state.json after Phase 1 when Phase 2 will not run (MaxPhase == 1 || SkipLLM).
	// When Phase 2 runs, defer stStore.Save() until Phase 2 succeeds so a mid-Phase-2 failure
	// never leaves a half-migrated state.json on disk.
	if opts.Apply && (opts.MaxPhase == 1 || opts.SkipLLM || guarded == nil) {
		if err := stStore.Save(); err != nil {
			return rep, fmt.Errorf("save after phase 1: %w", err)
		}
	}

	// Phase 2: lightweight Haiku disambiguation, sensitive audit, boundary rule split.
	if opts.MaxPhase >= 2 && guarded != nil {
		p2Start := nowFn()
		p2Rep, err := RunMigrateV04Phase2(ctx, st, guarded, today, logf)
		p2Rep.DurationMS = nowFn().Sub(p2Start).Milliseconds()
		rep.Phase2 = p2Rep
		if err != nil {
			rep.After = CollectMigrateV04Stats(st)
			rep.HaikuCalls, rep.SonnetCalls = guarded.Counts()
			rep.FinishedAt = nowFn().UTC().Format(time.RFC3339)
			return rep, fmt.Errorf("phase 2: %w", err)
		}
		if opts.Apply {
			if err := stStore.Save(); err != nil {
				return rep, fmt.Errorf("save after phase 2: %w", err)
			}
		}
	} else {
		rep.Phase2.Skipped = true
	}

	// Phase 3: L1 rebuild + profile refresh + exit compilation.
	if opts.MaxPhase >= 3 {
		if opts.DryRun {
			rep.Phase3.Skipped = true
		} else {
			p3Start := nowFn()
			var p3Model model.Model
			if guarded != nil {
				// Phase 3 uses stricter concurrency (<=2) and RPM (<=10) per §3.2.
				p3Conc := clampInt(opts.MaxConc, 1, 2, 2)
				p3RPM := clampInt(opts.MaxRPM, 1, 10, 10)
				p3Model = &model.Limited{
					Model: guarded,
					Limit: &model.Limiter{Concurrent: p3Conc, PerMinute: p3RPM},
				}
			}
			p3Rep, err := RunMigrateV04Phase3(ctx, opts.StateDir, st, p3Model, opts, today, nowFn, logf)
			p3Rep.DurationMS = nowFn().Sub(p3Start).Milliseconds()
			rep.Phase3 = p3Rep
			if err != nil {
				rep.After = CollectMigrateV04Stats(st)
				if guarded != nil {
					rep.HaikuCalls, rep.SonnetCalls = guarded.Counts()
				}
				rep.FinishedAt = nowFn().UTC().Format(time.RFC3339)
				return rep, fmt.Errorf("phase 3: %w", err)
			}
		}
	} else {
		rep.Phase3.Skipped = true
	}

	rep.After = CollectMigrateV04Stats(st)
	if guarded != nil {
		rep.HaikuCalls, rep.SonnetCalls = guarded.Counts()
	}
	rep.FinishedAt = nowFn().UTC().Format(time.RFC3339)
	return rep, nil
}

// ---------- Phase 0: Atomic Backup & Byte-Exact Rollback ----------

// BackupV04 atomically copies state.json, overview-state.json and profile-summary.json into
// <dir>/backups/v04-<ts>/ along with SHA256SUMS and MANIFEST.json.
func BackupV04(dir string, now time.Time) (ts string, backupDir string, err error) {
	ts = now.UTC().Format("20060102T150405Z")
	backupsRoot := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupsRoot, 0o700); err != nil {
		return "", "", err
	}
	backupDir = filepath.Join(backupsRoot, "v04-"+ts)
	if _, err := os.Stat(backupDir); err == nil {
		// Disambiguate within the same second.
		ts = now.UTC().Format("20060102T150405.000000000Z")
		backupDir = filepath.Join(backupsRoot, "v04-"+ts)
	}
	tmpDir := backupDir + ".tmp"
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	manifest := MigrateV04BackupManifest{
		Timestamp: ts,
		CreatedAt: now.UTC().Format(time.RFC3339),
		Files:     map[string]string{},
		Sizes:     map[string]int64{},
	}
	var sumBuf bytes.Buffer
	for _, name := range migrateV04TrackedFiles {
		src := filepath.Join(dir, name)
		info, statErr := os.Stat(src)
		if errors.Is(statErr, os.ErrNotExist) {
			manifest.Absent = append(manifest.Absent, name)
			continue
		}
		if statErr != nil {
			return "", "", statErr
		}
		if info.IsDir() {
			return "", "", fmt.Errorf("%s is a directory", src)
		}
		dst := filepath.Join(tmpDir, name)
		digest, size, copyErr := copyFileWithSHA256(src, dst)
		if copyErr != nil {
			return "", "", fmt.Errorf("backup %s: %w", name, copyErr)
		}
		manifest.Files[name] = digest
		manifest.Sizes[name] = size
		fmt.Fprintf(&sumBuf, "%s  %s\n", digest, name)
	}
	if _, ok := manifest.Files["state.json"]; !ok {
		return "", "", fmt.Errorf("state.json not found in %s", dir)
	}
	if err := writeFileAtomic0600(filepath.Join(tmpDir, "SHA256SUMS"), sumBuf.Bytes()); err != nil {
		return "", "", err
	}
	mBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := writeFileAtomic0600(filepath.Join(tmpDir, "MANIFEST.json"), mBytes); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmpDir, backupDir); err != nil {
		return "", "", err
	}
	cleanup = false
	return ts, backupDir, nil
}

// RollbackV04 verifies SHA256SUMS in <dir>/backups/v04-<ts>/ and atomically restores every tracked file.
// If any checksum mismatches or a backup file is missing, nothing in dir is modified.
func RollbackV04(dir, ts string) error {
	ts = strings.TrimSpace(ts)
	if ts == "" || strings.ContainsAny(ts, `/\`) || strings.Contains(ts, "..") {
		return fmt.Errorf("invalid rollback timestamp %q", ts)
	}
	bdir := filepath.Join(dir, "backups", "v04-"+ts)
	sumBytes, err := os.ReadFile(filepath.Join(bdir, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("read SHA256SUMS in %s: %w", bdir, err)
	}
	expected := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sumBytes)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return fmt.Errorf("malformed SHA256SUMS line %q", line)
		}
		name := filepath.Base(parts[1])
		expected[name] = strings.ToLower(parts[0])
	}
	if len(expected) == 0 {
		return fmt.Errorf("empty SHA256SUMS in %s", bdir)
	}

	// Verify all backup files BEFORE touching any live file.
	for name, want := range expected {
		got, err := fileSHA256(filepath.Join(bdir, name))
		if err != nil {
			return fmt.Errorf("verify backup %s: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("backup checksum mismatch for %s: got %s want %s", name, got, want)
		}
	}

	// Read optional manifest to know which files were absent at backup time.
	var absent []string
	if raw, err := os.ReadFile(filepath.Join(bdir, "MANIFEST.json")); err == nil {
		var m MigrateV04BackupManifest
		if json.Unmarshal(raw, &m) == nil {
			absent = m.Absent
		}
	}

	// Atomically restore each verified file and verify the restored target matches byte-for-byte.
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		src := filepath.Join(bdir, name)
		dst := filepath.Join(dir, name)
		digest, _, err := copyFileAtomic0600(src, dst)
		if err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		if digest != expected[name] {
			return fmt.Errorf("restored checksum mismatch for %s: got %s want %s", name, digest, expected[name])
		}
	}
	// Remove tracked files that did not exist prior to migration.
	for _, name := range absent {
		if !slices.Contains(migrateV04TrackedFiles, name) {
			continue
		}
		if _, ok := expected[name]; ok {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
	return nil
}

// ---------- Phase 1: Deterministic Cleaning (0 LLM, idempotent) ----------

// RunMigrateV04Phase1 performs all deterministic v0.4.0 cleaning passes on st in place.
// It makes zero model calls and is strictly idempotent.
func RunMigrateV04Phase1(ctx context.Context, st *store.State, today string) (MigrateV04Phase1Report, error) {
	rep := MigrateV04Phase1Report{
		SensitiveClearedByCat: map[string]int{},
	}
	if st == nil {
		return rep, nil
	}

	// Build contact name lookup for generic people noise check.
	contactName := map[string]string{}
	for k, c := range st.Contacts {
		if c != nil && strings.TrimSpace(c.Name) != "" {
			contactName[k] = c.Name
		}
	}

	// Deterministic traversal order.
	ids := make([]string, 0, len(st.Entries))
	for id := range st.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		e := st.Entries[id]
		if e == nil {
			continue
		}

		// Step 1: S6 handwritten -> assistant_note (strip UserOwned privilege, preserve ID and text).
		if e.Origin == entry.Handwritten {
			e.Origin = entry.OriginAssistantNote
			rep.S6Downgraded++
		}

		if !e.Live() {
			continue
		}

		trueUser := entry.IsTrueUserOwned(e)

		// Step 2: S3/S4-only persona cleanup.
		if e.Facet == entry.Persona && !trueUser && isS3S4OnlyEntry(e) {
			if isS3SupervisorPersona(e) {
				e.Status = entry.Retired
				e.StatusAt = today
				if !slices.Contains(e.Flags, migrateV04FlagS3Supervisor) {
					e.Flags = append(e.Flags, migrateV04FlagS3Supervisor)
				}
				rep.S3PersonaRetired++
				continue
			}
			if e.Status == entry.Stable {
				e.Status = entry.Candidate
				e.StatusAt = today
				rep.S3PersonaDemoted++
			}
		}

		// Step 2b: a stable persona entry the extraction flagged LOW_CONFIDENCE from a single sighting is a misread
		// until seen again (e.g. a vent about a rejected interview read as the user's PhD history): back to candidate.
		if e.Facet == entry.Persona && !trueUser && e.Status == entry.Stable && e.Count <= 1 &&
			slices.Contains(e.Flags, entry.FlagLowConfidence) {
			e.Status = entry.Candidate
			e.StatusAt = today
			rep.LowConfidencePersonaDemoted++
		}

		// Step 3: Cron / scheduled script workflow cleanup.
		if e.Facet == entry.Workflow && !trueUser && isCronWorkflowEntry(e) {
			e.Status = entry.Retired
			e.StatusAt = today
			if !slices.Contains(e.Flags, migrateV04FlagCronPrompt) {
				e.Flags = append(e.Flags, migrateV04FlagCronPrompt)
			}
			rep.CronWorkflowRetired++
			continue
		}

		// Step 4a: Generic nameless / HR recruiting noise in people facet.
		if e.Facet == entry.People && !trueUser && !e.Pinned {
			cname := contactName[e.Person]
			if entry.IsGenericPeopleNoise(e, cname) {
				changed := false
				if !slices.Contains(e.Flags, entry.FlagEphemeral) {
					e.Flags = append(e.Flags, entry.FlagEphemeral)
					changed = true
				}
				if e.MemoryClass != entry.ClassEphemeral {
					e.MemoryClass = entry.ClassEphemeral
					changed = true
				}
				if e.Status == entry.Stable || e.Status == entry.Candidate {
					e.Status = entry.Dormant
					e.StatusAt = today
					changed = true
				}
				if changed {
					rep.GenericPeopleDemoted++
				}
			}
		}

		// Step 6: Payload-First sensitivity re-evaluation on work-methodology facets and ops_runbook entries.
		if entry.IsWorkMethodFacet(e.Facet) || entry.IsOpsRunbookEntry(e) {
			clearedCats, spaceCleared := reevalWorkSensitivity(e)
			if len(clearedCats) > 0 {
				rep.SensitiveWorkCleared++
				for _, c := range clearedCats {
					rep.SensitiveClearedByCat[c]++
				}
			}
			if spaceCleared {
				rep.OriginSpaceWorkCleared++
			}
		}

		// Step 6b: known legal_id false positives on non-work facets. Step 6 re-checks the work facets only; an older
		// model also labelled a Visa card payment or "被告状" on persona/world/people entries as legal_id. A model label
		// is cleared only when the payload holds such a known false positive and nothing the strict detector accepts
		// (a real visa, passport, H1B, lawsuit keeps it); user labels are never touched.
		if !entry.IsWorkMethodFacet(e.Facet) && clearLegalIDFalsePositive(e) {
			rep.LegalIDFalsePositiveCleared++
		}

		// Step 7: Ops runbook deterministic pre-tagging (run before ApplyLifecycleDecay so old ops decay immediately).
		if e.MemoryClass != entry.ClassEphemeral && entry.ClassifyOpsRunbook(e) {
			changed := false
			if e.MemoryClass != entry.ClassOpsRunbook {
				e.MemoryClass = entry.ClassOpsRunbook
				changed = true
			}
			if !slices.Contains(e.Flags, entry.FlagOpsRunbook) {
				e.Flags = append(e.Flags, entry.FlagOpsRunbook)
				changed = true
			}
			if changed {
				rep.OpsRunbookTagged++
			}
		}
	}

	// Step 4b: Block 2 dual-lifecycle decay (ephemeral 14/30d, ops_runbook 45/60d).
	rep.LifecycleDecay = entry.ApplyLifecycleDecay(st.Entries, today)

	// Sync MemoryClass=ClassEphemeral for entries tagged FlagEphemeral by ApplyLifecycleDecay.
	for _, id := range ids {
		e := st.Entries[id]
		if e != nil && slices.Contains(e.Flags, entry.FlagEphemeral) && e.MemoryClass == "" {
			e.MemoryClass = entry.ClassEphemeral
		}
	}

	// Step 5: Block 2 graph deterministic cleaning and Tier 1 merge (0 LLM).
	gRep, err := graph.ResolveAndMergeAt(ctx, st, nil, today)
	if err != nil {
		return rep, fmt.Errorf("graph tier 1 resolve: %w", err)
	}
	rep.Graph = gRep

	return rep, nil
}

func isS3S4OnlyEntry(e *entry.Entry) bool {
	if e == nil {
		return false
	}
	if len(e.Evidence) > 0 {
		for _, ev := range e.Evidence {
			s := strings.TrimSpace(ev.Source)
			if s != entry.SrcAgent && s != entry.SrcTools {
				return false
			}
		}
		return true
	}
	if len(e.Kinds) > 0 {
		for _, k := range e.Kinds {
			s := strings.TrimSpace(k)
			if s != entry.SrcAgent && s != entry.SrcTools {
				return false
			}
		}
		return true
	}
	return false
}

func isS3SupervisorPersona(e *entry.Entry) bool {
	for _, txt := range e.PayloadTexts() {
		if strings.TrimSpace(txt) != "" && s3SupervisorPersonaRe.MatchString(txt) {
			return true
		}
	}
	return false
}

func isCronWorkflowEntry(e *entry.Entry) bool {
	for _, txt := range e.PayloadTexts() {
		if curate.IsCronOrScheduledPrompt(txt) {
			return true
		}
	}
	for _, ev := range e.Evidence {
		ref := strings.ToLower(strings.TrimSpace(ev.Ref))
		if strings.HasPrefix(ref, "cron/") || strings.HasPrefix(ref, "scheduled/") {
			return true
		}
		if curate.IsCronOrScheduledPrompt(ev.Quote) {
			return true
		}
	}
	return false
}

// clearLegalIDFalsePositive takes a model-made legal_id label (and a legacy Sensitive legal_id) off e when its payload
// is a known false positive (entry.LegalIDFalsePositive). ByUser labels are kept.
func clearLegalIDFalsePositive(e *entry.Entry) bool {
	modelTagged := entry.CategoryOf(e.Sensitive) == entry.CatLegalID
	for _, c := range e.Categories {
		if entry.CategoryOf(c.Name) != entry.CatLegalID {
			continue
		}
		if c.By == entry.ByUser {
			return false
		}
		modelTagged = true
	}
	if !modelTagged || !entry.LegalIDFalsePositive(e.PayloadTexts()...) {
		return false
	}
	e.Categories = slices.DeleteFunc(e.Categories, func(c entry.Category) bool {
		return entry.CategoryOf(c.Name) == entry.CatLegalID && c.By != entry.ByUser
	})
	if entry.CategoryOf(e.Sensitive) == entry.CatLegalID {
		e.Sensitive = ""
	}
	return true
}

// reevalWorkSensitivity re-evaluates non-user sensitive tags on work-methodology facets strictly via PayloadCategories
// (and clears all non-user sensitive tags on ops_runbook entries). User-authored tags (ByUser) are preserved untouched.
func reevalWorkSensitivity(e *entry.Entry) (cleared []string, spaceCleared bool) {
	beforeCats := e.Cats()
	if len(beforeCats) == 0 && e.OriginSpace == "" {
		return nil, false
	}
	var ownCats []string
	if !entry.IsOpsRunbookEntry(e) {
		ownCats = entry.PayloadCategories(e.Facet, e.PayloadTexts()...)
	}

	// Preserve ByUser tags; rebuild non-user tags strictly from ownCats.
	kept := make([]entry.Category, 0, len(e.Categories))
	hasCat := map[string]bool{}
	for _, c := range e.Categories {
		n := entry.CategoryOf(c.Name)
		if n == "" || slices.Contains(e.CatsRemoved, n) {
			continue
		}
		if c.By == entry.ByUser {
			kept = append(kept, entry.Category{Name: n, By: entry.ByUser})
			hasCat[n] = true
		}
	}
	for _, n := range ownCats {
		if !hasCat[n] && !slices.Contains(e.CatsRemoved, n) {
			kept = append(kept, entry.Category{Name: n, By: entry.ByModel})
			hasCat[n] = true
		}
	}
	e.Categories = kept
	if e.Sensitive != "" {
		e.Sensitive = ""
	}
	afterCats := e.Cats()
	for _, c := range beforeCats {
		if !slices.Contains(afterCats, c) {
			cleared = append(cleared, c)
		}
	}
	if len(afterCats) == 0 && e.OriginSpace != "" {
		e.OriginSpace = ""
		spaceCleared = true
	}
	return cleared, spaceCleared
}

// ---------- Phase 2: Lightweight Haiku Disambiguation & Audit ----------

const phase2SensitiveSystem = `You audit memory entries for Payload-First personal privacy sensitivity.
A category (emotion, health, finance, legal_id, relationships) is TRUE ONLY if the entry's OWN text directly discloses personal private facts:
- emotion: personal psychological/emotional vulnerability (depression, anxiety, insomnia, private inner distress). EXCLUDE work friction or impatience with AI ("user dislikes verbose answers", "repeated questions annoy user").
- health: personal medical metrics, symptoms, diagnoses, medication, checkups. EXCLUDE general data/workflow/reporting rules learned while discussing health data ("want recurring pipeline, not one-off export").
- finance: personal salary, assets, mortgage, investments, private bills. EXCLUDE AI model/token costs or engineering budgets.
- legal_id: personal ID numbers, visas, private contracts/lawsuits. EXCLUDE open-source licenses or API tokens.
- relationships: spouse, family, close friends, private evaluations of people. EXCLUDE public team org or work collaboration.

For each item, return verdict "keep" (genuine personal privacy payload) or "false_positive" (not personal privacy; safe to release to general pool) and confidence "high" or "low".`

var phase2SensitiveSchema = json.RawMessage(`{
  "type": "object",
  "required": ["items"],
  "properties": {
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "verdict", "confidence", "reason"],
        "properties": {
          "id": {"type": "string"},
          "verdict": {"type": "string", "enum": ["keep", "false_positive"]},
          "confidence": {"type": "string", "enum": ["high", "low"]},
          "reason": {"type": "string"}
        }
      }
    }
  }
}`)

const phase2BoundarySystem = `You classify high-scoring interaction rules into two mutually exclusive classes:
- "universal_contract": cross-tool, cross-project general AI collaboration norm that remains meaningful after stripping any concrete host, CLI tool, repo, or platform name (e.g. "verify raw evidence before asserting", "align on plan for architecture, execute directly for routine tasks", "explain technical terms in plain language").
- "ops_runbook": rule bound to a specific machine, tool, runtime, CLI command, repository, CI pipeline, API limit, or platform-specific workflow mechanism (e.g. host-specific tests, specific gateway/tool fallback, deployment topology).

Return one classification per item.`

var phase2BoundarySchema = json.RawMessage(`{
  "type": "object",
  "required": ["items"],
  "properties": {
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "memory_class", "reason"],
        "properties": {
          "id": {"type": "string"},
          "memory_class": {"type": "string", "enum": ["universal_contract", "ops_runbook"]},
          "reason": {"type": "string"}
        }
      }
    }
  }
}`)

// RunMigrateV04Phase2 runs lightweight Haiku tasks:
//  1. Tier 2 fuzzy graph disambiguation via graph.ResolveAndMergeAt
//  2. Payload-First audit of remaining non-user stable sensitive entries
//  3. Boundary rule classification (universal_contract vs ops_runbook) for unclassified high-score interaction rules
func RunMigrateV04Phase2(ctx context.Context, st *store.State, m model.Model, today string, logf func(string, ...any)) (MigrateV04Phase2Report, error) {
	var rep MigrateV04Phase2Report
	if st == nil || m == nil {
		rep.Skipped = true
		return rep, nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	counted := &tierCountingModel{inner: m}

	// 2.1 Tier 2 fuzzy entity merge.
	gRep, err := graph.ResolveAndMergeAt(ctx, st, counted, today)
	rep.Tier2Graph = gRep
	if err != nil {
		rep.HaikuCalls = counted.SmallCalls()
		return rep, fmt.Errorf("tier 2 graph merge: %w", err)
	}
	if gRep != nil {
		logf("Phase 2.1 Tier 2 graph: checked=%d batches=%d merged=%d", gRep.Tier2PairsChecked, gRep.Tier2Batches, gRep.Tier2Merged)
	}

	// 2.2 Residual stable sensitive entries audit (skip any entry with ByUser sensitivity).
	var sensCands []*entry.Entry
	for _, e := range st.Entries {
		if e == nil || !e.Live() || e.Status != entry.Stable || !e.IsSensitive() {
			continue
		}
		if hasUserTaggedCategory(e) {
			continue
		}
		sensCands = append(sensCands, e)
	}
	sort.Slice(sensCands, func(i, j int) bool {
		if entry.Score(sensCands[i]) != entry.Score(sensCands[j]) {
			return entry.Score(sensCands[i]) > entry.Score(sensCands[j])
		}
		return sensCands[i].ID < sensCands[j].ID
	})
	if len(sensCands) > 180 {
		sensCands = sensCands[:180]
	}
	rep.SensitiveChecked = len(sensCands)

	type sensItem struct {
		ID         string   `json:"id"`
		Facet      string   `json:"facet"`
		Categories []string `json:"categories"`
		Text       string   `json:"text"`
	}
	type sensResp struct {
		Items []struct {
			ID         string `json:"id"`
			Verdict    string `json:"verdict"`
			Confidence string `json:"confidence"`
			Reason     string `json:"reason"`
		} `json:"items"`
	}
	for i := 0; i < len(sensCands); i += migrateV04BatchSize {
		end := min(i+migrateV04BatchSize, len(sensCands))
		batch := sensCands[i:end]
		payload := make([]sensItem, 0, len(batch))
		byID := make(map[string]*entry.Entry, len(batch))
		for _, e := range batch {
			byID[e.ID] = e
			payload = append(payload, sensItem{
				ID:         e.ID,
				Facet:      string(e.Facet),
				Categories: e.Cats(),
				Text:       e.Shown(),
			})
		}
		body, _ := json.Marshal(map[string]any{"items": payload})
		resp, err := counted.Complete(ctx, model.Request{
			Step:      "migrate_v04_sens_audit",
			Tier:      model.Small,
			System:    phase2SensitiveSystem,
			User:      string(body),
			MaxTokens: 2000,
			Schema:    phase2SensitiveSchema,
		})
		rep.SensitiveBatches++
		if err != nil {
			rep.HaikuCalls = counted.SmallCalls()
			return rep, fmt.Errorf("sensitive audit batch %d: %w", rep.SensitiveBatches, err)
		}
		var out sensResp
		if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &out); err != nil {
			logf("Phase 2.2 batch %d unparseable JSON, skipping: %v", rep.SensitiveBatches, err)
			continue
		}
		for _, it := range out.Items {
			e := byID[it.ID]
			if e == nil || it.Verdict != "false_positive" || it.Confidence != "high" {
				continue
			}
			// Strip non-user sensitive categories.
			var kept []entry.Category
			for _, c := range e.Categories {
				if c.By == entry.ByUser {
					kept = append(kept, c)
				}
			}
			e.Categories = kept
			e.Sensitive = ""
			if len(e.Cats()) == 0 && entry.IsWorkMethodFacet(e.Facet) {
				e.OriginSpace = ""
			}
			rep.SensitiveFalsePositives++
		}
	}

	// 2.3 Boundary rule split for unclassified high-score stable interaction rules.
	var ruleCands []*entry.Entry
	for _, e := range st.Entries {
		if e == nil || !e.Live() || e.Status != entry.Stable || e.Facet != entry.Interaction {
			continue
		}
		if e.MemoryClass != "" || e.IsSensitive() || entry.Score(e) < 50 {
			continue
		}
		ruleCands = append(ruleCands, e)
	}
	sort.Slice(ruleCands, func(i, j int) bool {
		if entry.Score(ruleCands[i]) != entry.Score(ruleCands[j]) {
			return entry.Score(ruleCands[i]) > entry.Score(ruleCands[j])
		}
		return ruleCands[i].ID < ruleCands[j].ID
	})
	if len(ruleCands) > 120 {
		ruleCands = ruleCands[:120]
	}
	rep.BoundaryRulesChecked = len(ruleCands)

	type ruleItem struct {
		ID   string `json:"id"`
		Type string `json:"type,omitempty"`
		Text string `json:"text"`
	}
	type ruleResp struct {
		Items []struct {
			ID          string `json:"id"`
			MemoryClass string `json:"memory_class"`
			Reason      string `json:"reason"`
		} `json:"items"`
	}
	for i := 0; i < len(ruleCands); i += migrateV04BatchSize {
		end := min(i+migrateV04BatchSize, len(ruleCands))
		batch := ruleCands[i:end]
		payload := make([]ruleItem, 0, len(batch))
		byID := make(map[string]*entry.Entry, len(batch))
		for _, e := range batch {
			byID[e.ID] = e
			payload = append(payload, ruleItem{ID: e.ID, Type: e.Type, Text: e.Shown()})
		}
		body, _ := json.Marshal(map[string]any{"items": payload})
		resp, err := counted.Complete(ctx, model.Request{
			Step:      "migrate_v04_boundary_rules",
			Tier:      model.Small,
			System:    phase2BoundarySystem,
			User:      string(body),
			MaxTokens: 2000,
			Schema:    phase2BoundarySchema,
		})
		rep.BoundaryBatches++
		if err != nil {
			rep.HaikuCalls = counted.SmallCalls()
			return rep, fmt.Errorf("boundary rule batch %d: %w", rep.BoundaryBatches, err)
		}
		var out ruleResp
		if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &out); err != nil {
			logf("Phase 2.3 batch %d unparseable JSON, skipping: %v", rep.BoundaryBatches, err)
			continue
		}
		for _, it := range out.Items {
			e := byID[it.ID]
			if e == nil {
				continue
			}
			switch it.MemoryClass {
			case string(entry.ClassOpsRunbook):
				e.MemoryClass = entry.ClassOpsRunbook
				if !slices.Contains(e.Flags, entry.FlagOpsRunbook) {
					e.Flags = append(e.Flags, entry.FlagOpsRunbook)
				}
				rep.BoundaryTaggedOps++
			case string(entry.ClassUniversalContract):
				e.MemoryClass = entry.ClassUniversalContract
				rep.BoundaryTaggedUniversal++
			}
		}
	}

	rep.HaikuCalls = counted.SmallCalls()
	return rep, nil
}

func hasUserTaggedCategory(e *entry.Entry) bool {
	for _, c := range e.Categories {
		if c.By == entry.ByUser {
			return true
		}
	}
	return false
}

// ---------- Phase 3: L1 Rebuild, Profile Refresh & Exit Compilation ----------

// RunMigrateV04Phase3 rebuilds overview-state.json from the cleaned state (preserving any prior user verdicts),
// refreshes profile-summary.json, and optionally compiles exits.
// Block 3's SyncProfileCore and Reconcile hooks integrate directly into this pipeline once merged.
func RunMigrateV04Phase3(
	ctx context.Context,
	dir string,
	st *store.State,
	m model.Model,
	opts MigrateV04Options,
	today string,
	nowFn func() time.Time,
	logf func(string, ...any),
) (MigrateV04Phase3Report, error) {
	var rep MigrateV04Phase3Report
	if nowFn == nil {
		nowFn = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	refTime := nowFn()
	if t, err := time.Parse("2006-01-02", today); err == nil {
		refTime = t
	}
	yr, wk := refTime.ISOWeek()
	week := fmt.Sprintf("%04d-W%02d", yr, wk)
	rep.WeekPeriod = week

	counted := &tierCountingModel{inner: m}

	// Open overview state; reset old patterns for a clean v0.4.0 rebuild while preserving manual audit notes if any.
	ovHolder, err := overview.Open(dir)
	if err != nil {
		return rep, fmt.Errorf("open overview: %w", err)
	}
	if err := ovHolder.Update(func(s *overview.State) (bool, error) {
		s.Patterns = map[string]*overview.Pattern{}
		s.Weeks = map[string]*overview.Run{}
		s.Months = map[string]*overview.Run{}
		s.ContractsSig = ""
		s.ProfileCoreSig = ""
		s.Refreshed = ""
		s.Dead = nil
		return true, nil
	}); err != nil {
		return rep, fmt.Errorf("reset overview state: %w", err)
	}

	if m != nil {
		longM := opts.SonnetModel
		if longM == "" {
			longM = migrateV04DefaultSonnetModel
		}
		shortM := opts.HaikuModel
		if shortM == "" {
			shortM = migrateV04DefaultHaikuModel
		}
		runner := &overview.Runner{
			M:     counted,
			Long:  longM,
			Short: shortM,
			Lang:  opts.Lang,
			Now:   func() time.Time { return refTime },
			Logf:  logf,
		}
		var runErr error
		if err := ovHolder.Update(func(s *overview.State) (bool, error) {
			run, werr := runner.Weekly(ctx, st, s, week, "")
			if werr != nil {
				runErr = werr
				return false, nil
			}
			if run != nil {
				if s.Weeks == nil {
					s.Weeks = map[string]*overview.Run{}
				}
				s.Weeks[week] = run
			}
			overview.Refresh(st, s, refTime)
			return true, nil
		}); err != nil {
			rep.HaikuCalls, rep.SonnetCalls = counted.SmallCalls(), counted.MediumCalls()
			return rep, fmt.Errorf("overview weekly save: %w", err)
		}
		if runErr != nil {
			rep.HaikuCalls, rep.SonnetCalls = counted.SmallCalls(), counted.MediumCalls()
			return rep, fmt.Errorf("overview weekly: %w", runErr)
		}
	}

	ovSnap := ovHolder.Snapshot()
	rep.PatternsTotal = len(ovSnap.Patterns)
	for _, p := range ovSnap.Patterns {
		if p == nil || !p.Used() {
			continue
		}
		switch p.KindOf() {
		case overview.KindProfileCore:
			rep.ProfileCoreUsed++
		case overview.KindPrinciple:
			rep.PrinciplesUsed++
		case overview.KindContract:
			rep.ContractsUsed++
		case overview.KindOpsRunbook:
			rep.OpsRunbooksUsed++
		}
	}

	// Refresh profile-summary.json consuming the new L1 overview snapshot.
	profOpts := profile.Options{
		Provider:  overview.OverviewBackedProvider{Snapshot: ovHolder.Snapshot},
		Model:     counted,
		Tier:      model.Medium,
		Lang:      opts.Lang,
		Day:       today,
		Force:     true,
		SkipModel: m == nil,
		Now:       func() time.Time { return refTime },
		Logf:      logf,
	}
	sum, err := profile.Refresh(ctx, st, dir, profOpts)
	if err != nil {
		rep.HaikuCalls, rep.SonnetCalls = counted.SmallCalls(), counted.MediumCalls()
		return rep, fmt.Errorf("profile refresh: %w", err)
	}
	if sum != nil {
		rep.ProfileMode = sum.Mode
		rep.ProfileSections = len(sum.Sections)
	}

	// Optional exit compilation (e.g. during rehearsal or post-migration verification).
	if opts.ExitOutDir != "" {
		if err := os.MkdirAll(opts.ExitOutDir, 0o700); err != nil {
			return rep, err
		}
		var ps []entry.KBProject
		if st.Exits != nil {
			ps = st.Exits.Projects
		}
		exRep, err := exits.Compile(ctx, st, exits.Options{
			Dir:      opts.ExitOutDir,
			Lang:     opts.Lang,
			Loc:      time.Local,
			Projects: ps,
		})
		if err != nil {
			return rep, fmt.Errorf("compile exits: %w", err)
		}
		rep.ExitFilesWritten = len(exRep.Files)
	}

	rep.HaikuCalls = counted.SmallCalls()
	rep.SonnetCalls = counted.MediumCalls()
	return rep, nil
}

// ---------- Stats & Helpers ----------

// CollectMigrateV04Stats computes a snapshot of key entry, sensitivity and graph metrics.
func CollectMigrateV04Stats(st *store.State) MigrateV04StateStats {
	s := MigrateV04StateStats{
		ByFacetStable:  map[string]int{},
		SensitiveByCat: map[string]int{},
	}
	if st == nil {
		return s
	}
	s.EntriesTotal = len(st.Entries)
	for _, e := range st.Entries {
		if e == nil {
			continue
		}
		if e.Origin == entry.Handwritten {
			s.S6Handwritten++
		} else if e.Origin == entry.OriginAssistantNote {
			s.S6AssistantNote++
		}
		if e.Live() {
			s.EntriesLive++
		}
		switch e.Status {
		case entry.Stable:
			if e.Live() {
				s.EntriesStable++
				s.ByFacetStable[string(e.Facet)]++
				if e.IsSensitive() {
					s.SensitiveStable++
					for _, c := range e.Cats() {
						s.SensitiveByCat[c]++
					}
				}
			}
		case entry.Candidate:
			if e.Live() {
				s.EntriesCandidate++
			}
		case entry.Dormant:
			if e.Live() {
				s.EntriesDormant++
			}
		case entry.Retired:
			s.EntriesRetired++
		}
		if e.MemoryClass == entry.ClassOpsRunbook || slices.Contains(e.Flags, entry.FlagOpsRunbook) {
			s.OpsRunbookCount++
		}
		if e.MemoryClass == entry.ClassEphemeral || slices.Contains(e.Flags, entry.FlagEphemeral) {
			s.EphemeralCount++
		}
	}
	if st.Graph != nil {
		s.GraphNodes = len(st.Graph.Nodes)
		s.GraphAssertions = len(st.Graph.Assertions)
		s.GraphMerges = len(st.Graph.Merges)
		for _, n := range st.Graph.Nodes {
			if n == nil || n.MergedInto != "" || n.Status == "archived" || n.Status == "not_entity" {
				continue
			}
			s.GraphNodesActive++
			if n.Type == "task" {
				s.GraphTaskActive++
			}
		}
		for _, a := range st.Graph.Assertions {
			if a == nil || a.Status == "archived" || a.Status == "retracted" || a.Status == "rejected" {
				continue
			}
			if graph.TypeName(a.P) == "about" {
				s.GraphAboutActive++
			}
		}
	}
	return s
}

// ---------- Guarded Model Wrapper (Strict Rate Limit + Budget Cap + No-Retry on Quota/Auth) ----------

type migrateGuardedModel struct {
	inner     model.Model
	limit     *model.Limiter
	maxHaiku  int
	maxSonnet int

	mu          sync.Mutex
	haikuCalls  int
	sonnetCalls int
	aborted     error
}

func newMigrateGuardedModel(inner model.Model, lim *model.Limiter, maxHaiku, maxSonnet int) *migrateGuardedModel {
	if lim == nil {
		lim = &model.Limiter{Concurrent: migrateV04MaxConcHard, PerMinute: migrateV04MaxRPMHard}
	}
	if lim.Concurrent <= 0 || lim.Concurrent > migrateV04MaxConcHard {
		lim.Concurrent = migrateV04MaxConcHard
	}
	if lim.PerMinute <= 0 || lim.PerMinute > migrateV04MaxRPMHard {
		lim.PerMinute = migrateV04MaxRPMHard
	}
	return &migrateGuardedModel{
		inner:     inner,
		limit:     lim,
		maxHaiku:  maxHaiku,
		maxSonnet: maxSonnet,
	}
}

func (g *migrateGuardedModel) Counts() (haiku, sonnet int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.haikuCalls, g.sonnetCalls
}

func (g *migrateGuardedModel) Complete(ctx context.Context, r model.Request) (model.Response, error) {
	g.mu.Lock()
	if g.aborted != nil {
		err := g.aborted
		g.mu.Unlock()
		return model.Response{}, err
	}
	isMedium := r.Tier == model.Medium
	if isMedium {
		if g.maxSonnet > 0 && g.sonnetCalls >= g.maxSonnet {
			g.mu.Unlock()
			return model.Response{}, fmt.Errorf("%w: sonnet budget cap (%d) reached", ErrMigrateQuotaOrAuth, g.maxSonnet)
		}
		g.sonnetCalls++
	} else {
		if g.maxHaiku > 0 && g.haikuCalls >= g.maxHaiku {
			g.mu.Unlock()
			return model.Response{}, fmt.Errorf("%w: haiku budget cap (%d) reached", ErrMigrateQuotaOrAuth, g.maxHaiku)
		}
		g.haikuCalls++
	}
	g.mu.Unlock()

	release, err := g.limit.Acquire(ctx)
	if err != nil {
		return model.Response{}, err
	}
	defer release()

	resp, err := g.inner.Complete(ctx, r)
	if err != nil {
		if isQuotaOrAuthError(err) {
			wrapped := fmt.Errorf("%w: %v", ErrMigrateQuotaOrAuth, err)
			g.mu.Lock()
			if g.aborted == nil {
				g.aborted = wrapped
			}
			g.mu.Unlock()
			return model.Response{}, wrapped
		}
		return model.Response{}, err
	}
	return resp, nil
}

func isQuotaOrAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMigrateQuotaOrAuth) || errors.Is(err, model.ErrUnavailable) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, w := range fatalQuotaOrAuthWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// tierCountingModel counts small vs medium calls for a single phase.
type tierCountingModel struct {
	inner  model.Model
	mu     sync.Mutex
	small  int
	medium int
}

func (t *tierCountingModel) Complete(ctx context.Context, r model.Request) (model.Response, error) {
	if t.inner == nil {
		return model.Response{}, model.ErrUnavailable
	}
	t.mu.Lock()
	if r.Tier == model.Medium {
		t.medium++
	} else {
		t.small++
	}
	t.mu.Unlock()
	return t.inner.Complete(ctx, r)
}

func (t *tierCountingModel) SmallCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.small
}

func (t *tierCountingModel) MediumCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.medium
}

// noRetryCLIModel runs local `claude -p` once per request with zero automatic retry so quota/auth errors stop immediately.
type noRetryCLIModel struct {
	Command  string
	Models   map[model.Tier]string
	CacheDir string
	Timeout  time.Duration
}

func (c *noRetryCLIModel) Complete(ctx context.Context, r model.Request) (model.Response, error) {
	mdl := c.Models[r.Tier]
	if mdl == "" {
		mdl = c.Models[model.Small]
	}
	var cachePath string
	if c.CacheDir != "" {
		h := sha256.New()
		for _, p := range []string{mdl, r.System, r.User, string(r.Schema)} {
			fmt.Fprintf(h, "%d:%s\x00", len(p), p)
		}
		cachePath = filepath.Join(c.CacheDir, hex.EncodeToString(h.Sum(nil))+".json")
		if b, err := os.ReadFile(cachePath); err == nil {
			var hit struct {
				Text  string `json:"text"`
				Model string `json:"model"`
			}
			if json.Unmarshal(b, &hit) == nil && hit.Text != "" {
				return model.Response{Text: hit.Text, Model: hit.Model}, nil
			}
		}
	}
	cmd := c.Command
	if cmd == "" {
		cmd = "claude"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p"}
	if mdl != "" {
		args = append(args, "--model", mdl)
	}
	args = append(args, "--system-prompt", r.System, "--tools", "", "--strict-mcp-config",
		"--no-session-persistence", "--output-format", "json")
	if len(r.Schema) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, r.Schema); err != nil {
			return model.Response{}, fmt.Errorf("schema: %w", err)
		}
		args = append(args, "--json-schema", compact.String())
	}
	wd, err := os.MkdirTemp("", "enkeep-v04-model-")
	if err != nil {
		return model.Response{}, err
	}
	defer os.RemoveAll(wd)

	x := exec.CommandContext(cctx, cmd, args...)
	x.Dir = wd
	x.Stdin = strings.NewReader(r.User)
	var stdout, stderr bytes.Buffer
	x.Stdout, x.Stderr = &stdout, &stderr
	if err := x.Run(); err != nil {
		return model.Response{}, fmt.Errorf("%s (%s): %w: %s", cmd, r.Step, err, trimTail(stderr.String()+stdout.String(), 400))
	}
	var out struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return model.Response{}, fmt.Errorf("%s (%s): non-JSON output: %s", cmd, r.Step, trimTail(stdout.String(), 400))
	}
	if out.IsError {
		return model.Response{}, fmt.Errorf("%s (%s): %s %s", cmd, r.Step, out.Subtype, trimTail(out.Result, 400))
	}
	text := out.Result
	if len(out.StructuredOutput) > 0 && string(out.StructuredOutput) != "null" {
		text = string(out.StructuredOutput)
	}
	if strings.TrimSpace(text) == "" {
		return model.Response{}, fmt.Errorf("%s (%s): empty answer", cmd, r.Step)
	}
	resp := model.Response{
		Text:      text,
		Model:     mdl,
		InTokens:  out.Usage.InputTokens + out.Usage.CacheReadInputTokens + out.Usage.CacheCreationInputTokens,
		OutTokens: out.Usage.OutputTokens,
	}
	if cachePath != "" {
		if err := os.MkdirAll(c.CacheDir, 0o700); err == nil {
			b, _ := json.Marshal(struct {
				Text  string `json:"text"`
				Model string `json:"model"`
			}{Text: resp.Text, Model: resp.Model})
			_ = writeFileAtomic0600(cachePath, b)
		}
	}
	return resp, nil
}

func extractJSON(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

func trimTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}

func clampInt(v, lo, hi, def int) int {
	if v <= 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func copyFileWithSHA256(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		os.Remove(dst)
		return "", 0, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFileAtomic0600(src, dst string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".restore-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	digest, n, err := copyFileWithSHA256(src, tmpName)
	if err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	return digest, n, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileAtomic0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func writeMigrateJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(path, b)
}
