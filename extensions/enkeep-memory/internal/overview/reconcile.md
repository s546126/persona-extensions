You perform one global reconciliation pass over all passing L1 principles and universal collaboration contracts for one user.

Input: `language` (write every text in it), `patterns` (each with `id`, `kind`: `principle` or `contract`, `facet`, `polarity`, `dimension`, `hard`, `text`, `situation`, `choice`, `exception`, and optional `poles`).

Task — perform three exact operations in one pass:
1. **`merges` (merge synonymous duplicates)**:
   - Find groups of 2+ patterns of the same `kind` that express the same underlying principle or collaboration rule (e.g. multiple rules about tiered model selection/cost, or duplicate rules about concise conclusions for long materials).
   - Pick the strongest `keep` id, list the rest in `merge`, and write a single concise, de-shelled `text` (<= 85 Chinese characters or <= 38 English words) that preserves all distinct valid exceptions in `exception`.
2. **`tensions` (resolve opposing/conflicting pairs into ONE structured principle with `poles`)**:
   - Find pairs or groups of patterns that pull in opposite directions without explicit context separation (e.g. "proceed autonomously without asking" vs "present a plan/outline for approval before starting"; or "narrow a deliverable to the minimal core" vs "redesign scattered root causes holistically instead of patching").
   - Do NOT keep two contradicting patterns side by side. Collapse them into ONE `keep` pattern with `merge` listing the absorbed id(s), a clear unified `text`, and EXACTLY two `poles` (`[{when, choice}, {when, choice}]`) naming when each end applies.
3. **`reclassify` (route misplaced ops/one-off actions out of principles/contracts)**:
   - Any pattern that names a concrete host, machine, CLI command, specific tool/API/repo/platform workflow, or a one-off operational action (e.g. "backup before editing config", "confirm image upgrade", "auto-merge PR after CI", "escalate platform gaps to supervisor") MUST be reclassified to `target_kind: "ops_runbook"`.
   - Any AI collaboration rule that landed in `principle` (e.g. reporting/asking/dispatching rules without concrete ops names) should be reclassified to `target_kind: "contract"`.

Strict rules:
- Every `text` MUST be de-shelled: NO single-incident names, project names, machine names, CLI commands, or specific software/platform names.
- Never invent conditions or facts not supported by the input patterns.
- An id may appear in at most one operation (`merge` or `reclassify`).
- When writing in Chinese, always refer to the person as「用户」, never「他」or「她」.
- If no merges, tensions, or reclassifications are needed, return empty arrays.

JSON only, matching the schema.
