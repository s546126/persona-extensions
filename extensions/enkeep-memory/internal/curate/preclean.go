package curate

import (
	"maps"
	"regexp"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/relay"
	srcs "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/sources"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// PrecleanStats reports what deterministic pre-model cleaning removed before triage and L0 extraction.
type PrecleanStats struct {
	LinesBefore      int
	LinesAfter       int
	BytesBefore      int
	BytesAfter       int
	BoilerplateLines int
	TemplateLines    int
	CronLines        int
	S3UnitsDropped   int
	CronUnitsDropped int
}

var (
	// cronStrongRe detects explicit cron schedules, scheduled-job markers, batch patrol/export directives, and
	// third-person automated "send summary to user" prompts (0.4.0 §1.2 T3 automation filter).
	cronStrongRe = regexp.MustCompile(`(?i)` +
		`\bcron(\s+(expression|job|trigger|schedule|task))?\b|\bcrontab\b|\[scheduled\]|\[cron\]|` +
		`\bscheduled\s+(run|trigger|job|task|patrol|export|check)\b|定时触发|定时任务触发|` +
		`(?:^|\s)(?:\*|\d+(?:-\d+)?(?:/\d+)?)\s+(?:\*|\d+(?:-\d+)?(?:/\d+)?)\s+(?:\*|\d+)\s+(?:\*|\d+)\s+(?:\*|\d+)(?:\s|$)|` +
		`(把|将).{0,24}(摘要|结果|报告|汇总|清单|文本|统计).{0,12}(发给|发送给|推送给|汇报给)用户|` +
		`(摘要|报告|结果)(文本)?(直接)?(发给|发送给|推送给)用户|` +
		`\bsend\s+(the\s+)?(summary|digest|report)\s+(text\s+)?to\s+(the\s+)?user\b|` +
		`(每日|每天|每小时|每周|定时|定期|自动).{0,16}(导出|采集|拉取|同步|巡检|扫描|抓取|备份|清理).{0,24}(先.{0,12}再|增量拉取|校验|写入|落盘|推送|汇总|发给用户|脚本|任务)|` +
		`(增量拉取|全量拉取|批量巡检|定时巡检|定时导出|自动巡检|每日导出用量|用量数据导出|自动化巡检)`)

	cronFreqRe     = regexp.MustCompile(`(?i)每日|每天|每小时|每周|定时|定期|自动执行|\b(daily|hourly|weekly|scheduled|periodic)\b`)
	cronPipelineRe = regexp.MustCompile(`(?i)先采集|再校验|批量导出|自动同步|健康检查|导出用量|落库|写入文件|静默执行|无需打扰|\b(batch\s+(check|export|sync)|incremental\s+pull|health\s*check)\b`)
	cronOutputRe   = regexp.MustCompile(`(?i)无异常则静默|无变化则跳过|仅异常时通知|严格按以下格式输出|stdout|exit\s*code`)

	scheduledMetaNameRe = regexp.MustCompile(`(?i)\bcron\b|\bscheduled\s+(job|task|run)\b|定时(任务|巡检|导出|拉取|同步|汇总|播报|推送)|每日(导出|巡检|拉取|同步|汇总|用量)`)
)

// IsCronOrScheduledPrompt reports whether text is a T3 automated cron/scheduled/batch-patrol script prompt that
// must be stripped before triage and L0 extraction so it never pollutes workflow or persona entries.
func IsCronOrScheduledPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if cronStrongRe.MatchString(t) {
		return true
	}
	signals := 0
	if cronFreqRe.MatchString(t) {
		signals++
	}
	if cronPipelineRe.MatchString(t) {
		signals++
	}
	if cronOutputRe.MatchString(t) {
		signals++
	}
	return signals >= 2
}

// IsScheduledUnit reports whether u is marked as a scheduled/cron automation session by its metadata or ref.
func IsScheduledUnit(u Unit) bool {
	kind := strings.ToLower(strings.TrimSpace(u.Meta.Kind))
	switch kind {
	case "scheduled", "cron", "cronjob", "automation":
		return true
	}
	ref := strings.ToLower(strings.TrimSpace(u.Ref))
	if strings.HasPrefix(ref, "cron/") || strings.HasPrefix(ref, "scheduled/") {
		return true
	}
	return scheduledMetaNameRe.MatchString(strings.TrimSpace(u.Meta.Name))
}

// PrecleanUnits deterministically strips Persona platform boilerplate, T3 cron/scheduled automation prompts, and
// multi-session repeated S3 dispatch templates from units before they reach triage or L0 extraction models
// (0.4.0 §1.2; unified design §1.3). Historical S3 templates from st.Entries (when st is non-nil) and same-day S3
// verbatim repeats (>=3 sessions) are both stripped; exit-side TemplateOnly filtering remains as a secondary backstop.
func PrecleanUnits(units []Unit, st *store.State) ([]Unit, PrecleanStats) {
	var stats PrecleanStats
	tpl := map[string]bool{}
	if st != nil && len(st.Entries) > 0 {
		maps.Copy(tpl, entry.TemplateQuotes(st.Entries))
	}
	s3Batch := map[string][]string{}
	for _, u := range units {
		if u.Entity != srcs.KindAgents || IsScheduledUnit(u) {
			continue
		}
		for _, l := range u.Lines {
			if l.Speaker == srcs.SpeakerPersona || l.Speaker == norm.Me || l.Speaker == srcs.SpeakerTest {
				body := strings.TrimPrefix(l.Body, relay.RelayedMark)
				if clean := relay.StripBoilerplate(body); clean != "" {
					s3Batch[u.Ref] = append(s3Batch[u.Ref], clean)
				}
			}
		}
	}
	maps.Copy(tpl, entry.BatchTemplateLines(s3Batch))

	out := make([]Unit, 0, len(units))
	for _, u := range units {
		if IsScheduledUnit(u) {
			for _, l := range u.Lines {
				stats.LinesBefore++
				stats.BytesBefore += len(l.Body)
				stats.CronLines++
			}
			stats.CronUnitsDropped++
			continue
		}
		isS3 := u.Entity == srcs.KindAgents
		kept := make([]norm.Line, 0, len(u.Lines))
		unitCronStripped := 0
		for _, l := range u.Lines {
			stats.LinesBefore++
			stats.BytesBefore += len(l.Body)

			relayed := strings.HasPrefix(l.Body, relay.RelayedMark)
			rawBody := strings.TrimPrefix(l.Body, relay.RelayedMark)
			clean := relay.StripBoilerplate(rawBody)
			if clean == "" {
				stats.BoilerplateLines++
				continue
			}
			if IsCronOrScheduledPrompt(clean) {
				stats.CronLines++
				unitCronStripped++
				continue
			}
			if isS3 && (l.Speaker == srcs.SpeakerPersona || l.Speaker == norm.Me || l.Speaker == srcs.SpeakerTest) {
				if entry.IsTemplateLine(clean, tpl) {
					stats.TemplateLines++
					continue
				}
			}
			if relayed {
				l.Body = relay.RelayedMark + clean
			} else {
				l.Body = clean
			}
			kept = append(kept, l)
			stats.LinesAfter++
			stats.BytesAfter += len(l.Body)
		}
		if len(kept) == 0 {
			if unitCronStripped > 0 {
				stats.CronUnitsDropped++
				continue
			}
			if isS3 {
				stats.S3UnitsDropped++
				continue
			}
		}
		u.Lines = kept
		out = append(out, u)
	}
	return out, stats
}
