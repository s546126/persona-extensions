package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tokenBudgets is what each measured text may cost, in bytes and estimated tokens (estTokens). The texts are
// measured by TestTokenCostAPI (what the assistant reads through the skill and the HTTP API) and
// TestTokenCostWorker (what the extension writes back to the to-do and hands the worker agent).
//
// A budget is a ratchet, not only a ceiling: checkBudget also fails when a text is more than budgetStale under
// its budget, so a change that makes a text smaller records its new size here, in the same diff.
//
// The skill and gate texts have about 2% headroom for small wording edits; composite and flow.list_evidence
// carry the same absolute headroom as skill.body, the only part of them that is hand-written. Every other budget
// is the measured value: the fixtures are deterministic, and the worker rig's commit SHAs vary in value only,
// not in length (checked over repeated runs).
var tokenBudgets = map[string]struct{ Bytes, Tokens int }{
	// Always in the assistant's context.
	"skill.desc": {205, 100},
	"gate.desc":  {125, 52},
	// Read once per skill load: SKILL.md without its frontmatter (the description is counted in skill.desc).
	"skill.body": {3996, 1662},

	// HTTP API answers.
	"list.todo":        {859, 238},
	"list.all":         {4781, 1373},
	"list.full":        {9354, 2516},
	"list.page2":       {1514, 437},
	"task.get":         {1617, 498},
	"task.full":        {3353, 1080},
	"evidence":         {1703, 473},
	"evidence.attempt": {3231, 855},
	"post.resp":        {385, 100},
	"err.repo":         {114, 29},
	"err.missing":      {44, 11},

	// Flows. composite: skill body + list + get + evidence (4 calls). flow.list_evidence: skill body + list +
	// evidence, the common "is it done" path of the skill's rule 2 (3 calls).
	"composite":          {8175, 2871},
	"flow.list_evidence": {6558, 2373},

	// Written back to the to-do: question, choice and reason, as its reader sees them.
	"wb.pass":       {630, 253},
	"wb.unverified": {613, 249},
	"wb.fail":       {660, 268},
	"wb.cancel":     {88, 41},

	// What ATS appends to the user's prompt for the worker agent.
	"prompt.first":  {834, 209},
	"prompt.retry2": {2643, 756},
	"prompt.retry3": {2643, 756},
}

// budgetStale is how far under its budget a text may fall before the budget has to be lowered.
const budgetStale = 0.05

// The budget keys each measuring test owns, by prefix. Every key in tokenBudgets belongs to exactly one group
// (TestBudgetKeysOwned), and each test measures every key of its group (checkBudgets), so a renamed or dropped
// row cannot leave a dead budget behind.
var (
	apiBudgetKeys    = []string{"skill.", "gate.", "list.", "task.", "evidence", "post.", "err.", "composite", "flow."}
	workerBudgetKeys = []string{"wb.", "prompt."}
)

func inBudgetGroup(group []string, key string) bool {
	for _, p := range group {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// checkBudgets checks every measured text against its budget (budgetProblem) and fails the test when a budget of
// group was not measured or a measured key is not in group. forbid is passed on to budgetProblem.
func checkBudgets(t *testing.T, group []string, measured map[string]string, forbid ...string) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(measured)) {
		if !inBudgetGroup(group, key) {
			t.Errorf("%s is measured by a test that does not own it", key)
		}
		if p := budgetProblem(key, measured[key], forbid...); p != "" {
			t.Error(p)
		}
	}
	for _, key := range unmeasuredBudgets(group, measured) {
		t.Errorf("the budget %s is not measured: a row was renamed or dropped", key)
	}
}

// unmeasuredBudgets lists the keys of group in tokenBudgets that measured has no text for.
func unmeasuredBudgets(group []string, measured map[string]string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(tokenBudgets)) {
		if _, ok := measured[key]; inBudgetGroup(group, key) && !ok {
			out = append(out, key)
		}
	}
	return out
}

// testTempRoot is the directory t.TempDir creates its per-test directories in: $GOTMPDIR when set, else
// os.TempDir. Every t.TempDir of every test, including the worker rig's worktrees, is under it.
func testTempRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(t.TempDir()))
}

// budgetProblem says what is wrong with text against the budget of key: it costs more than the budget, it costs
// more than budgetStale less (the budget is stale), or it contains a temporary directory ($GOTMPDIR, os.TempDir,
// or one of forbid, each also with its symlinks resolved). A temporary path differs between machines and runs,
// so a text that has one cannot be pinned.
func budgetProblem(key, text string, forbid ...string) string {
	b, ok := tokenBudgets[key]
	if !ok {
		return "no token budget for " + key
	}
	var paths []string
	for _, p := range append(forbid, os.Getenv("GOTMPDIR"), os.TempDir()) {
		if p == "" {
			continue
		}
		paths = append(paths, p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			paths = append(paths, real)
		}
	}
	for _, p := range paths {
		if p != "" && strings.Contains(text, p) {
			return fmt.Sprintf("%s contains the temporary path %s, so its size is not stable", key, p)
		}
	}
	bytes, toks := len(text), estTokens(text)
	switch {
	case bytes > b.Bytes || toks > b.Tokens:
		return fmt.Sprintf("over budget for %s: measured %d B / %d tok, budget %d B / %d tok", key, bytes, toks, b.Bytes, b.Tokens)
	case float64(bytes) < float64(b.Bytes)*(1-budgetStale) || float64(toks) < float64(b.Tokens)*(1-budgetStale):
		return fmt.Sprintf("budget stale for %s: measured %d B / %d tok, lower the budget (now %d B / %d tok)", key, bytes, toks, b.Bytes, b.Tokens)
	}
	return ""
}

// skillParts splits SKILL.md into its frontmatter description and the body after the frontmatter.
func skillParts(t *testing.T) (desc, body string) {
	t.Helper()
	raw, err := os.ReadFile("skills/ats-todo/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(string(raw), "---\n")
	if !ok {
		t.Fatal("SKILL.md does not start with frontmatter")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	for _, line := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(line, "description:"); ok {
			desc = strings.TrimSpace(v)
		}
	}
	if desc == "" {
		t.Fatal("SKILL.md has no description")
	}
	return desc, body
}

// The description decides when the skill is used, so a shorter one keeps the phrasings it answers to.
func TestSkillDescriptionKeepsTriggers(t *testing.T) {
	desc, _ := skillParts(t)
	for _, want := range []string{"做到哪了", "真做完了吗", "为什么卡住", "ATS 在跑什么", "交给 ATS", "待办", "验收"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the skill description lost %q: %s", want, desc)
		}
	}
}

// Every task is created with verdict unverified (queued, running, needs_input and failed tasks too), so the
// skill's "ran but not verified" answer must stay limited to succeeded tasks.
func TestSkillUnverifiedRuleIsForSucceededTasks(t *testing.T) {
	_, body := skillParts(t)
	var rule string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "2. ") {
			rule = line
		}
	}
	if rule == "" {
		t.Fatal("the skill has no rule 2")
	}
	const want = "`succeeded` 但 `verdict` 为 `unverified` 只能说「跑完了、没验收」"
	if !strings.Contains(rule, want) {
		t.Errorf("rule 2 does not limit the unverified answer to succeeded tasks (want %q): %s", want, rule)
	}
	if n := strings.Count(rule, "`unverified`"); n != 1 {
		t.Errorf("rule 2 mentions `unverified` %d times, want once (with its state condition): %s", n, rule)
	}
}

func TestBudgetProblem(t *testing.T) {
	const key = "err.missing"
	b := tokenBudgets[key]
	fits := strings.Repeat("x", b.Bytes)
	if p := budgetProblem(key, fits); p != "" {
		t.Errorf("a text of exactly the budget: %s", p)
	}
	if p := budgetProblem(key, fits+"x"); !strings.HasPrefix(p, "over budget for "+key) {
		t.Errorf("a text over the budget: %q", p)
	}
	if p := budgetProblem(key, fits[:b.Bytes/2]); !strings.HasPrefix(p, "budget stale for "+key) {
		t.Errorf("a text far under the budget: %q", p)
	}
	if p := budgetProblem("no.such.key", fits); !strings.HasPrefix(p, "no token budget") {
		t.Errorf("an unknown key: %q", p)
	}
	// Caught whether or not GOTMPDIR is set, and whether or not it is under os.TempDir.
	dir := t.TempDir()
	if p := budgetProblem(key, dir); !strings.Contains(p, "temporary path") {
		t.Errorf("a text with t.TempDir() in it: %q", p)
	}
	if p := budgetProblem(key, dir, testTempRoot(t)); !strings.Contains(p, "temporary path") {
		t.Errorf("a text with t.TempDir() in it, its root forbidden: %q", p)
	}
	if root := testTempRoot(t); !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Errorf("testTempRoot %s does not hold t.TempDir() %s", root, dir)
	}
	t.Setenv("GOTMPDIR", "/x/gotmp")
	if p := budgetProblem(key, fits[:b.Bytes-9]+"/x/gotmp/", "/x/app"); !strings.Contains(p, "temporary path /x/gotmp") {
		t.Errorf("a text with $GOTMPDIR in it: %q", p)
	}
	if p := budgetProblem(key, fits[:b.Bytes-6]+"/x/app", "/x/app"); !strings.Contains(p, "temporary path /x/app") {
		t.Errorf("a text with a forbidden path in it: %q", p)
	}
}

// Every budget belongs to exactly one measuring test, so none can go unmeasured.
func TestBudgetKeysOwned(t *testing.T) {
	for key := range tokenBudgets {
		if a, w := inBudgetGroup(apiBudgetKeys, key), inBudgetGroup(workerBudgetKeys, key); a == w {
			t.Errorf("the budget %s is owned by both measuring tests or by neither (api %v, worker %v)", key, a, w)
		}
	}
	measured := map[string]string{"wb.pass": "", "wb.unverified": "", "wb.fail": ""}
	if got := unmeasuredBudgets(workerBudgetKeys, measured); !slices.Contains(got, "wb.cancel") || !slices.Contains(got, "prompt.first") || slices.Contains(got, "wb.pass") || slices.Contains(got, "skill.body") {
		t.Errorf("unmeasured worker budgets: %v", got)
	}
}
