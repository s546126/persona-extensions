package entry

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Lifecycle flags and deterministic decay windows (v0.4.0 §1.3).
const (
	FlagEphemeral  = "ephemeral"
	FlagOpsRunbook = "ops_runbook"

	EphemeralActiveDays  = 14 // ephemeral entries exit active recall after 14 days
	EphemeralDormantDays = 30 // ephemeral entries auto-transition to dormant after 30 days

	OpsRunbookDemoteDays  = 45 // ops_runbook entries lose ranking weight after 45 days
	OpsRunbookDormantDays = 60 // ops_runbook entries auto-transition to dormant after 60 days
)

// SkeletonPredicates are the structural relation predicates retained as the entity recall skeleton
// (design §2.2, l0-leverage §13): ownership, assignment, dependencies, team membership, collaboration, role,
// and reporting structure.
var SkeletonPredicates = map[string]bool{
	"assigned":          true,
	"collaborates_with": true,
	"depends_on":        true,
	"member_of":         true,
	"owns":              true,
	"reports_to":        true,
	"role":              true,
}

// IsSkeletonPredicate says whether p is one of the structural skeleton relation predicates.
func IsSkeletonPredicate(p string) bool {
	return SkeletonPredicates[strings.ToLower(strings.TrimSpace(p))]
}

var (
	ephemeralTypes = map[string]bool{
		"status":   true,
		"task":     true,
		"meeting":  true,
		"progress": true,
		"update":   true,
		"schedule": true,
		"event":    true,
		"incident": true,
	}

	ephemeralPredicates = map[string]bool{
		"waiting_on": true,
		"blocked_by": true,
		"about":      true,
		"committed":  true,
	}

	// Specific dates, clock times, one-off meetings/calls, PR/Issue numbers, and short-lived TODO/stuck cues.
	ephemeralTimeRe = regexp.MustCompile(`(?i)` +
		`\b20\d{2}[-/.]\d{1,2}[-/.]\d{1,2}\b|` +
		`(?:\b\d{1,2}[-/]\d{1,2}\s+\d{1,2}:\d{2}\b)|` +
		`(?:\b\d{1,2}:\d{2}\b)|` +
		`(?:\d{1,2}月\d{1,2}[日号])|` +
		`(?:(?:上午|下午|晚上|今晚|明早|明天|后天|昨天|今天|周[一二三四五六日天]|星期[一二三四五六日天])\s*\d{1,2}\s*[点时])`)

	ephemeralMeetingCallRe = regexp.MustCompile(`(?i)` +
		`(?:约\s*(?:了\s*)?(?:\d{1,2}[-/月]\d{1,2}[日号]?\s*)?(?:\d{1,2}:\d{2}\s*)?(?:call|语音|电话|会议|时间|一下|碰一下|对齐|聊聊|同步|1on1|1-on-1|sync))|` +
		`(?:(?:拉|开|定|改期|推迟|取消|约)(?:了|个|一个)?(?:会|会议|电话会|语音会|对齐会|评审会|sync\s*会))|` +
		`(?:calendar\s+invite|发(?:了)?会议邀请|定了\s*\d{1,2}[:点])`)

	ephemeralPRIssueRe = regexp.MustCompile(`(?i)` +
		`\b(?:PR|pull\s*request|MR|issue|ticket|bug)\s*#?\d+\b|` +
		`(?:^|[\s（(【\[])#\d{2,7}\b`)

	ephemeralProgressRe = regexp.MustCompile(`(?i)` +
		`(?:发(?:了|过|出)?\s*(?:TODO|todo|待办)(?:清单)?)|` +
		`(?:(?:TODO|todo|待办)\s*清单)|` +
		`(?:卡在|卡住|阻塞在|堵在|待看|待确认|待回复|待审批|待合入|待合并|等回复|等审批|等待回复|` +
		`正在修|正在查|排查中|处理中|跟进中|已提\s*PR|刚合入|刚合并|刚发版|刚提交|临时排查|单次排查)`)

	genericPersonNames = map[string]bool{
		"hr": true, "hrbp": true, "recruiter": true, "招聘": true, "面试官": true, "候选人": true,
		"对方": true, "某人": true, "同事": true, "某同事": true, "老板": true, "领导": true,
		"客户": true, "供应商": true, "客服": true, "未知": true, "someone": true, "unknown": true,
		"colleague": true, "contact": true, "person": true, "user": true, "用户": true,
		"群友": true, "管理员": true, "猎头": true, "某候选人": true, "该同事": true,
	}

	genericHRNoiseRe = regexp.MustCompile(`(?i)` +
		`(?:(?:联系|找|问|和|与|跟)\s*HR(?:\s*(?:沟通|跟进|确认|对齐|聊|联系|反馈|回复|协调))?)|` +
		`(?:HR\s*(?:沟通|跟进|回复|反馈|协调|推进|安排|发来|通知|表示))|` +
		`(?:(?:约|安排|推进|跟进|取消|改期)(?:了)?(?:初面|复面|终面|面试|招聘))|` +
		`(?:面试反馈|简历筛选|发简历|推简历|看简历|猎头推荐|招聘进度|入职流程|背景调查|发\s*offer|谈\s*offer|薪资沟通|内推流程)`)

	genericNamelessSubjectRe = regexp.MustCompile(`^(?:对方|该同事|某同事|该候选人|候选人|某人|猎头|面试官)\s*(?:回复|表示|说|发来|提到|确认|询问|沟通过|反馈|答应|希望|计划)`)
)

// IsEphemeral reports whether e is a short-lived status/progress/schedule/PR/TODO entry (D2 in v0.4.0 §1.3)
// that must exit active recall after 14 days and transition to dormant after 30 days.
func IsEphemeral(e *Entry) bool {
	if e == nil {
		return false
	}
	if slices.Contains(e.Flags, FlagEphemeral) {
		return true
	}
	if ephemeralTypes[strings.ToLower(strings.TrimSpace(e.Type))] {
		return true
	}
	if ephemeralPredicates[strings.ToLower(strings.TrimSpace(e.Predicate))] {
		return true
	}
	// Content heuristics apply to World and People entries (excluding structured Commitments which have Due/Done).
	if e.Facet != World && e.Facet != People {
		return false
	}
	if e.Commitment != nil || e.Part == PartCommitment {
		return false
	}
	text := strings.TrimSpace(e.Shown())
	if text == "" {
		text = strings.TrimSpace(e.Text)
	}
	if text == "" {
		return false
	}
	return ephemeralTimeRe.MatchString(text) ||
		ephemeralMeetingCallRe.MatchString(text) ||
		ephemeralPRIssueRe.MatchString(text) ||
		ephemeralProgressRe.MatchString(text)
}

// IsGenericPersonName reports whether a non-empty person display name is a generic role/pronoun placeholder
// ("HR", "对方", "同事", "某人", "someone", etc.) rather than a concrete named person.
func IsGenericPersonName(name string) bool {
	s := strings.TrimSpace(name)
	if s == "" {
		return false
	}
	for _, p := range []string{"person:", "people:", "person：", "people："} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			s = strings.TrimSpace(s[len(p):])
		}
	}
	return genericPersonNames[strings.ToLower(s)]
}

// IsGenericPeopleNoise reports whether a People entry is generic HR/recruiting chitchat or lacks a concrete
// named person (v0.4.0 §1.3 D1). Pinned entries, user-edited entries, and structured commitments are preserved.
func IsGenericPeopleNoise(e *Entry, contactName string) bool {
	if e == nil || e.Facet != People || e.Pinned || e.User != nil || e.Commitment != nil {
		return false
	}
	if contactName != "" && IsGenericPersonName(contactName) {
		return true
	}
	text := strings.TrimSpace(e.Shown())
	if text == "" {
		text = strings.TrimSpace(e.Text)
	}
	if text == "" {
		return false
	}
	if e.Person == "" && genericNamelessSubjectRe.MatchString(text) {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(e.Type))
	// Keep explicit durable role/relation lines on concrete named people (e.g. "serves as team HRBP").
	if contactName != "" && !IsGenericPersonName(contactName) && (t == "role" || t == "relation" || t == "responsibility") {
		return false
	}
	if genericHRNoiseRe.MatchString(text) {
		return true
	}
	if contactName == "" && genericNamelessSubjectRe.MatchString(text) {
		return true
	}
	return false
}

// IsOpsRunbookEntry reports whether e is an operational runbook entry subject to the 45d demote / 60d dormant
// lifecycle (v0.4.0 §1.3 C). Compatible with both facet==tools and Block 1's ClassOpsRunbook / ClassifyOpsRunbook.
func IsOpsRunbookEntry(e *Entry) bool {
	if e == nil {
		return false
	}
	if e.MemoryClass == ClassOpsRunbook || ClassifyOpsRunbook(e) {
		return true
	}
	if e.Facet == Tools {
		return true
	}
	if slices.Contains(e.Flags, FlagOpsRunbook) {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(e.Type))
	return t == "ops_runbook" || t == "runbook"
}

// AgeDays returns the number of calendar days from lastDay to today (both YYYY-MM-DD or RFC3339 prefix).
func AgeDays(lastDay, today string) (int, bool) {
	if len(lastDay) < 10 || len(today) < 10 {
		return 0, false
	}
	tl, err1 := time.Parse("2006-01-02", lastDay[:10])
	tt, err2 := time.Parse("2006-01-02", today[:10])
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return int(tt.Sub(tl).Hours() / 24), true
}

func entryLastDay(e *Entry) string {
	if e == nil {
		return ""
	}
	if e.Last != "" {
		return e.Last
	}
	if e.Updated != "" {
		return e.Updated
	}
	return e.First
}

// EphemeralExpiredRecall reports whether e is ephemeral and has exceeded the 14-day active recall window.
func EphemeralExpiredRecall(e *Entry, today string) bool {
	if e == nil || e.Pinned || !IsEphemeral(e) {
		return false
	}
	age, ok := AgeDays(entryLastDay(e), today)
	return ok && age >= EphemeralActiveDays
}

// ShouldDormantEphemeral reports whether e is ephemeral and has exceeded the 30-day auto-dormant window.
func ShouldDormantEphemeral(e *Entry, today string) bool {
	if e == nil || e.Pinned || !IsEphemeral(e) {
		return false
	}
	age, ok := AgeDays(entryLastDay(e), today)
	return ok && age >= EphemeralDormantDays
}

// OpsRunbookWeight returns the ranking multiplier for an ops_runbook entry as of today:
// 1.0 within 45 days (or if pinned / not an ops runbook), 0.5 between 45 and 59 days, and 0.25 at >= 60 days.
func OpsRunbookWeight(e *Entry, today string) float64 {
	if e == nil || e.Pinned || !IsOpsRunbookEntry(e) || today == "" {
		return 1.0
	}
	age, ok := AgeDays(entryLastDay(e), today)
	if !ok {
		return 1.0
	}
	switch {
	case age >= OpsRunbookDormantDays:
		return 0.25
	case age >= OpsRunbookDemoteDays:
		return 0.5
	default:
		return 1.0
	}
}

// ShouldDormantOpsRunbook reports whether an ops_runbook entry has had no new evidence for >= 60 days.
func ShouldDormantOpsRunbook(e *Entry, today string) bool {
	if e == nil || e.Pinned || !IsOpsRunbookEntry(e) {
		return false
	}
	age, ok := AgeDays(entryLastDay(e), today)
	return ok && age >= OpsRunbookDormantDays
}
