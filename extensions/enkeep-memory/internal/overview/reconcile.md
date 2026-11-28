You perform one global reconciliation pass over all passing L1 principles and universal collaboration contracts for one user.

Input: `language` (write every text in it), `patterns` (each with `id`, `kind`: `principle` or `contract`, `facet`, `polarity`, `dimension`, `hard`, `text`, `situation`, `choice`, `exception`, and optional `poles`).

Task — perform three exact operations in one pass:
1. **`merges` (merge synonymous duplicates)**:
   - Find groups of 2+ patterns of the same `kind` that express the same underlying principle or collaboration rule (e.g. multiple rules about tiered model selection/cost, or duplicate rules about concise conclusions for long materials).
   - Pick the strongest `keep` id, list the rest in `merge`, and write a single concise, de-shelled `text` (<= 85 Chinese characters or <= 38 English words) that preserves all distinct valid exceptions in `exception`.
2. **`tensions` (resolve real two-pole tensions into ONE structured principle with `poles`)**:
   - Actively look across BOTH `principle` and `contract` patterns for real opposing tensions (e.g. "方案未定先对齐/出方案 vs 方向已定自主连续推进", "单次交付收窄核心 vs 反复偏离/散点根因整体设计", "proceed autonomously without asking vs present a plan/outline for approval before starting").
   - Two valid forms:
     - **Pair/group collapse**: when 2+ patterns (same kind or cross `principle`+`contract`) pull in opposite directions, keep the strongest `principle` as `keep`, list the absorbed id(s) in `merge`, write a unified `text`, and emit EXACTLY two `poles` (`[{when, choice}, {when, choice}]`).
     - **Single-principle upgrade**: when ONE existing `principle` already embeds a significant two-pole tension or one-sided `exception`, upgrade it directly by setting `keep` to its id, `merge: []`, a balanced unified `text`, and EXACTLY two `poles`.
3. **`reclassify` (route misplaced ops/one-off/narrow business rules out of principles/contracts)**:
   - Any pattern that names a concrete host, machine, CLI command, specific tool/API/repo/platform workflow, a one-off operational action (e.g. "backup before editing config", "confirm image upgrade", "auto-merge PR after CI", "escalate platform gaps to supervisor"), OR a narrow administrative/single-workflow business rule (e.g. "预算申报尚未核定时按当前估算先提报") MUST be reclassified to `target_kind: "ops_runbook"` so it does not occupy universal core principles.
   - Any AI collaboration rule that landed in `principle` (e.g. reporting/asking/dispatching rules without concrete ops names) should be reclassified to `target_kind: "contract"`. However, `workflow` patterns whose subject is the user's own working habit or method preference (e.g. "遇到深度分析时，用户交给多个子智能体做饱和分析再复核") belong in `principle`; do NOT reclassify them as assistant `contract`.

Strict rules:
- Every `text` MUST be de-shelled: NO single-incident names, project names, machine names, CLI commands, or specific software/platform names.
- Never invent conditions or facts not supported by the input patterns.
- An id may appear in at most one operation (`merge` or `reclassify`).
- When writing in Chinese, always refer to the person as「用户」, never「他」or「她」.
- If no merges, tensions, or reclassifications are needed, return empty arrays.

JSON only, matching the schema.
