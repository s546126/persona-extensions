package entry

import (
	"regexp"
	"slices"
	"strings"
)

// Categories (unified memory design v2 §3.2/§3.3b, domain spaces §3.6, user 11-22 05:44: "敏感空间也可以收集……
// 但是信息使用的时候确实要按照场景去做分级"): everything is collected and tagged; what an exit may carry is decided by
// the categories and the scene, not by leaving content out. A credential is not a category: it is never kept.
//
// An entry with no category is general. The enumeration is fixed in code (the scene rules depend on it).
const (
	CatHealth        = "health"
	CatFinance       = "finance"
	CatRelationships = "relationships" // family, partner, friends, colleagues, and evaluations of a person
	CatEmotion       = "emotion"
	CatLegalID       = "legal_id" // identity papers and numbers, visas, contracts, lawsuits
)

// CategoryNames lists the categories in display order.
var CategoryNames = []string{CatHealth, CatFinance, CatRelationships, CatEmotion, CatLegalID}

// Who tagged a category.
const (
	ByModel        = "model"         // the extraction's label, or the plugin's keyword rules
	BySpaceDefault = "space_default" // the source space's default categories
	ByUser         = "user"
)

// Category is one category of an entry and who put it there.
type Category struct {
	Name string `json:"name"`
	By   string `json:"by"`
}

// CategoryOf maps a label (the daily prompt's sensitive field, the K13 names, a seed's category) to a category, ""
// when it is none. K13's "family" is part of relationships.
func CategoryOf(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	switch l {
	case SensFamily:
		return CatRelationships
	case CatHealth, CatFinance, CatRelationships, CatEmotion, CatLegalID:
		return l
	}
	return ""
}

// Cats are the entry's categories: Categories, and the legacy single Sensitive label (entries before 0.2.0-beta.40).
func (e *Entry) Cats() []string {
	var out []string
	for _, c := range e.Categories {
		if n := CategoryOf(c.Name); n != "" && !slices.Contains(out, n) && !slices.Contains(e.CatsRemoved, n) {
			out = append(out, n)
		}
	}
	if n := CategoryOf(e.Sensitive); n != "" && !slices.Contains(out, n) && !slices.Contains(e.CatsRemoved, n) {
		out = append(out, n)
	}
	return out
}

// IsSensitive says whether the entry carries any category (not general).
func (e *Entry) IsSensitive() bool { return len(e.Cats()) > 0 }

// AddCategory adds a category (anyone may add); one the user removed is not added back but by the user.
func (e *Entry) AddCategory(name, by string) bool {
	n := CategoryOf(name)
	if n == "" || slices.Contains(e.Cats(), n) || by != ByUser && slices.Contains(e.CatsRemoved, n) {
		return false
	}
	e.CatsRemoved = slices.DeleteFunc(e.CatsRemoved, func(r string) bool { return r == n })
	e.Categories = append(e.Categories, Category{Name: n, By: by})
	return true
}

// RemoveCategory takes a category off; only the user may (agents and the plugin only add).
func (e *Entry) RemoveCategory(name, by string) bool {
	n := CategoryOf(name)
	if n == "" || by != ByUser || !slices.Contains(e.Cats(), n) {
		return false
	}
	e.Categories = slices.DeleteFunc(e.Categories, func(c Category) bool { return CategoryOf(c.Name) == n })
	if CategoryOf(e.Sensitive) == n {
		e.Sensitive = ""
	}
	e.CatsRemoved = append(e.CatsRemoved, n)
	return true
}

// IsWorkMethodFacet reports whether f is one of the four work-methodology facets (interaction, tools, workflow,
// judgement) that do not inherit conversation or space sensitivity unless their own payload discloses personal
// privacy (0.4.0 §1.6).
func IsWorkMethodFacet(f Facet) bool {
	switch f {
	case Interaction, Tools, Workflow, Judgement:
		return true
	}
	return false
}

var (
	// Payload-First category detectors (0.4.0 §1.6). Each matches only genuine personal privacy disclosures in the
	// entry's own payload, excluding work-interaction frustration words, data/export workflows, model/token costs,
	// open-source licenses/API tokens, and public org structure.
	payloadHealthRe = regexp.MustCompile(`(?i)` +
		`体检|病历|医学诊断|临床诊断|医生诊断|医院确诊|确诊为|确诊病例|病理诊断|就诊|复诊|门诊|看病|住院|手术|吃药|服药|用药|处方|剂量|药量|药物副作用|吃药副作用|服药副作用|不良反应|过敏|哮喘|` +
		`高血压|糖尿病|甲状腺|肿瘤|结节|发烧|咳嗽|头痛|胃痛|胸闷|心悸|血压|血糖|血脂|心率|静息心率|血氧|` +
		`HRV|体脂率|BMI|体温|睡眠障碍|失眠|睡不着|打鼾|呼吸暂停|怀孕|备孕|产检|流产|疫苗|看医生|挂号|理疗|康复训练|` +
		`慢病|慢性病|痛风|颈椎病|腰椎|近视手术|牙科|拔牙|根管|脂肪肝|` +
		`\b(diagnos(is|ed)|symptom|prescription|medication|dosage|blood pressure|blood sugar|cholesterol|` +
		`heart rate|hrv|spo2|lab result|medical record|hospitaliz\w*|surgery|pregnan\w*|illness|chronic disease|` +
		`insomnia|allergy|clinic|doctor'?s? visit|follow-up visit)\b`)

	payloadEmotionRe = regexp.MustCompile(`(?i)` +
		`抑郁|焦虑症|重度焦虑|持续焦虑|严重焦虑|惊恐发作|恐慌|心理咨询|心理治疗|精神内耗|情绪低谷|` +
		`情绪崩溃|崩溃大哭|想哭|绝望|无助|孤独感|空虚|自卑|自我怀疑|自我否定|讨厌自己|压力太大|喘不过气|` +
		`情绪耗竭|倦怠感|心累|难过|伤心|委屈|痛苦|失落|内疚|愧疚|恐惧|害怕失去|创伤|PTSD|` +
		`\b(depress(ion|ed|ing)|anxiety|anxious|panic attack|therapy|therapist|psycholog(ist|ical)|` +
		`mental health|emotional(ly)? (exhausted|drained|vulnerable)|burnout|lonely|loneliness|grief|trauma|` +
		`ptsd|suicid\w*|self[- ]doubt|feel(ing)? (hopeless|overwhelmed|worthless|sad))\b`)

	// Strict personal finance payload (salary, assets, mortgage, investments, personal bank/tax/cards).
	payloadFinanceStrictRe = regexp.MustCompile(`(?i)` +
		`工资条?|薪资|薪水|年薪|月薪|底薪|年终奖|绩效奖金|个人所得税|个税|退税|报税|房贷|车贷|按揭|首付|` +
		`买房|购房|还贷|贷款|负债|存款|储蓄|身家|净资产|家庭资产|个人资产|理财|股票持仓|基金持仓|基金定投|股票定投|定投计划|每月定投|每周定投|炒股|` +
		`信用卡账单?|还款日|个人账单|银行卡|工资入账|资金入账|款项入账|汇款入账|银行入账|货款入账|到账|转账|汇款|扣款|股权激励|期权归属|股票期权|` +
		`\b(salary|annual income|paycheck|payslip|pay ?stub|bonus|mortgage|down payment|net worth|` +
		`personal (savings|assets|debt|finance|tax)|bank (account|balance|statement)|` +
		`credit card (bill|debt|statement)|wire transfer|direct deposit|minimum payment|401k|ira|rsu|vest(ing|ed)|tax return)\b`)

	// Ambiguous balance/quota/bill words that count as personal finance only when NOT in an engineering/AI-cost context.
	payloadFinanceSoftRe = regexp.MustCompile(`(?i)账户余额|可用额度|信用卡|账单|\b(account )?balance\b|available balance|statement (is )?ready`)
	payloadEngCostCtxRe  = regexp.MustCompile(`(?i)模型|算力|token|api|claude|gpt|openai|anthropic|gemini|quota|云服务|aws|gcp|azure|调用成本|工程成本|项目预算|配额`)

	payloadLegalIDRe = regexp.MustCompile(`(?i)` +
		`身份证号?|护照号?|签证|驾驶证|驾照|户口本|户口簿|迁户口|落户|户籍证明|户籍信息|居住证|绿卡|社保号|医保卡号|税号|美国工卡|EAD工卡|实体工卡|工卡审批|工卡办理|\bEAD\b|居留许可|` +
		`私人合同|购房合同|租房合同|劳动仲裁|诉讼|起诉|成为被告|列为被告|起诉被告|原告方|被告方|民事诉讼|刑事诉讼|官司|律师函|仲裁|判决书|离婚协议|遗嘱|` +
		`\b(us|chinese|my|renew|renewal|expired?|number|photo)\s+(?:the\s+|my\s+)?passport\b|\bpassport\s+(number|renewal|expir\w*|stamp|photo|application)\b|` +
		`\b(h1b|h-1b|f1|f-1|b1/b2|o1|l1|eb[123]|work|student|tourist|schengen|us)\s+visa\b|\bvisa\s+(status|stamp|interview|renewal|expir\w*|approv\w*|application|extension)\b|` +
		`\b(green card|driver'?s? licen[cs]e|social security( number)?|ssn|national id|id card|` +
		`tax id|lawsuit|litigation|sued|arbitration|court order|subpoena|legal dispute|prenup)\b`)

	payloadRelationshipsRe = regexp.MustCompile(`(?i)` +
		`配偶|伴侣|老婆|老公|妻子|丈夫|女友|男友|女朋友|男朋友|谈对象|处对象|找对象|未婚妻|未婚夫|` +
		`前任(男友|女友|对象|伴侣|配偶)|前妻|前夫|前女友|前男友|恋爱|谈恋爱|相亲|约会对象|初次约会|浪漫约会|结婚|婚姻|离婚|分手|冷战|家庭矛盾|婆媳|父母|爸爸|妈妈|` +
		`父亲|母亲|岳父|岳母|公公|婆婆|儿子|女儿|孩子|宝宝|家人|亲戚|闺蜜|死党|` +
		`\b(spouse|partner|wife|husband|girlfriend|boyfriend|fianc[eé]e?|ex-(wife|husband|girlfriend|boyfriend)|` +
		`marriage|divorce|breakup|dating|romance|parents|mother|father|daughter|son|kids|children|family conflict|in-laws)\b`)
)

// crossWordLicaiRe matches "理财" when its "理" ends a preceding verb ("整理财务", "处理财务", "管理财务"…): bookkeeping
// and accounting chores, not the user's personal wealth management. RE2 has no look-behind, so the joint is broken
// before the finance detectors run.
var crossWordLicaiRe = regexp.MustCompile(`(整|处|管|清|梳|料|护|办|代|经|助|协|修|治|审|受|调|合|道|心|物|地|原|伦|推|真|条|事)理财`)

// legalIDFalsePositiveRe are the known legal_id false positives older models labelled on non-work entries: the card
// brand "visa" in a payment context (a domestic Visa card, paying through Google Play) and "告状" (complaining to
// someone's boss), which is not a lawsuit.
var legalIDFalsePositiveRe = regexp.MustCompile(`(?i)` +
	`(国内|国外|境外|外币|双币|信用卡|借记卡|储蓄卡|银行卡|万事达|mastercard|master card|google\s*play|apple\s*pay|app\s*store|付款|支付|刷卡|绑卡|绑定)[^。；;\n]{0,12}\bvisa\b|` +
	`\bvisa\b[^。；;\n]{0,12}(卡|付款|支付|扣款|账户|账号|\bcard\b|\bpay(ment)?\b|\bdebit\b|\bcredit\b)|` +
	`被?告状|打小报告`)

// LegalIDFalsePositive says whether texts carry one of the known legal_id false positives (legalIDFalsePositiveRe)
// and nothing the strict legal_id detector accepts (a real visa, passport, H1B, lawsuit…): such a model-made legal_id
// label is safe to clear.
func LegalIDFalsePositive(texts ...string) bool {
	hit := false
	for _, raw := range texts {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if payloadLegalIDRe.MatchString(t) {
			return false
		}
		if legalIDFalsePositiveRe.MatchString(t) {
			hit = true
		}
	}
	return hit
}

// PayloadCategories classifies sensitive categories strictly from the entry's own payload texts (0.4.0 §1.6
// Payload-First), never from conversation topic or user correction tone. Results are deduplicated in CategoryNames
// order.
func PayloadCategories(facet Facet, texts ...string) []string {
	matched := map[string]bool{}
	for _, raw := range texts {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		ft := crossWordLicaiRe.ReplaceAllString(t, "${1}理 财")
		if payloadHealthRe.MatchString(t) {
			matched[CatHealth] = true
		}
		if payloadFinanceStrictRe.MatchString(ft) ||
			(payloadFinanceSoftRe.MatchString(ft) && !(IsWorkMethodFacet(facet) && payloadEngCostCtxRe.MatchString(ft))) {
			matched[CatFinance] = true
		}
		if payloadRelationshipsRe.MatchString(t) {
			matched[CatRelationships] = true
		}
		if payloadEmotionRe.MatchString(t) {
			matched[CatEmotion] = true
		}
		if payloadLegalIDRe.MatchString(t) {
			matched[CatLegalID] = true
		}
	}
	if len(matched) == 0 {
		return nil
	}
	out := make([]string, 0, len(matched))
	for _, name := range CategoryNames {
		if matched[name] {
			out = append(out, name)
		}
	}
	return out
}

// MergeCategories folds user-authored categories and Payload-First supported categories from `from` into e.
// Unconditional union contagion from one sighting/member is removed (0.4.0 §1.6): work-methodology facets never
// inherit model/space sensitivity or OriginSpace from another sighting unless e's own payload discloses it.
func (e *Entry) MergeCategories(from *Entry) {
	if from == nil {
		return
	}
	// Always preserve explicit user-tagged categories (T0).
	for _, c := range from.Categories {
		if c.By == ByUser {
			e.AddCategory(c.Name, ByUser)
		}
	}
	if IsOpsRunbookEntry(e) {
		return
	}
	ownCats := PayloadCategories(e.Facet, e.PayloadTexts()...)
	for _, n := range ownCats {
		e.AddCategory(n, ByModel)
	}
	if IsWorkMethodFacet(e.Facet) {
		return
	}
	// Non-work facets (world, people, persona) retain payload-supported or space-default sensitivity from from.
	fromPayload := PayloadCategories(from.Facet, from.PayloadTexts()...)
	for _, c := range from.Categories {
		if c.By == ByUser {
			continue
		}
		n := CategoryOf(c.Name)
		if n == "" {
			continue
		}
		if c.By == BySpaceDefault || slices.Contains(fromPayload, n) || slices.Contains(ownCats, n) {
			e.AddCategory(n, c.By)
		}
	}
	if n := CategoryOf(from.Sensitive); n != "" && (slices.Contains(fromPayload, n) || slices.Contains(ownCats, n)) {
		e.AddCategory(n, ByModel)
	}
	if e.OriginSpace == "" {
		e.OriginSpace = from.OriginSpace
	}
}

// SpaceDefaults are the default categories of a space, by slug. The hub does not tell an extension a space's
// default categories yet; the health space's is fixed by its slug (domain spaces §3.6.4 keeps the slug).
func SpaceDefaults(space string) []string {
	if strings.TrimSpace(space) == "health" {
		return []string{CatHealth}
	}
	return nil
}

// TagSpace sets the entry's source space, when it has none, and adds the space's default categories.
// Work-methodology facets (interaction, tools, workflow, judgement) do not inherit space sensitivity or space
// confinement unless their own payload directly discloses personal privacy (0.4.0 §1.6).
func (e *Entry) TagSpace(space string) {
	space = strings.TrimSpace(space)
	if space == "" || IsOpsRunbookEntry(e) {
		return
	}
	if IsWorkMethodFacet(e.Facet) {
		ownCats := PayloadCategories(e.Facet, e.PayloadTexts()...)
		if len(ownCats) == 0 {
			return
		}
		for _, c := range ownCats {
			e.AddCategory(c, ByModel)
		}
		if e.OriginSpace == "" {
			e.OriginSpace = space
		}
		return
	}
	if e.OriginSpace == "" {
		e.OriginSpace = space
	}
	for _, c := range SpaceDefaults(space) {
		e.AddCategory(c, BySpaceDefault)
	}
}

// Resident says whether the entry may reach the resident exits (the assistant's instruction block, the agents'
// rules, the profile and project files): the strict side of scene 2 (unified memory design v2 §3.3b, §8; 0.4.0 §1.6).
// A general entry may; one with categories only when the user widened every one of them (K22.<category> on), or when
// the user wrote it about themself (about me). Restricted-space facts (world/people) stay confined to their space,
// while de-shelled general work-methodology rules (interaction/tools/workflow/judgement with no sensitive payload) are
// not blocked by OriginSpace.
func (e *Entry) Resident(sw map[string]SwitchSetting) bool {
	if e.OriginSpace != "" && !IsWorkMethodFacet(e.Facet) && e.Facet != Persona {
		return false
	}
	if e.Origin == SelfReport {
		return true
	}
	for _, c := range e.Cats() {
		if !On(sw, K22Resident+"."+c) {
			return false
		}
	}
	return true
}

// Named says whether every category of the entry is among names (a look-up asks for a category by name before it is
// given content in it).
func (e *Entry) Named(names []string) bool {
	for _, c := range e.Cats() {
		if !slices.ContainsFunc(names, func(n string) bool { return CategoryOf(n) == c }) {
			return false
		}
	}
	return true
}
