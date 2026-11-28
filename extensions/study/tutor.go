package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nwverse/persona/sdk/go/wire"
	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

// Guided practice: an exercise is made from one card (and its neighbours in the same knowledge point) in one of four
// kinds, answered with up to three Socratic hints, judged with a diagnosis of the mistake, and a wrong answer or a
// weak knowledge point can be turned into reinforcement drafts that join FSRS once kept.

const (
	kindConcept  = "concept"  // explain or tell apart a concept
	kindCloze    = "cloze"    // fill in the blank
	kindChoice   = "choice"   // multiple choice
	kindScenario = "scenario" // apply the idea to a new situation
)

var exerciseKinds = []string{kindConcept, kindCloze, kindChoice, kindScenario}

var kindLabels = map[string]string{kindConcept: "概念辨析", kindCloze: "填空", kindChoice: "选择", kindScenario: "场景变式"}

func validKind(k string) bool { _, ok := kindLabels[k]; return ok }

const (
	verdictCorrect = "correct"
	verdictPartial = "partial"
	verdictWrong   = "wrong"
)

var verdictLabels = map[string]string{verdictCorrect: "答对了", verdictPartial: "答对一部分", verdictWrong: "没答对"}

// Mistake causes the diagnosis picks from.
var causeLabels = map[string]string{
	"concept":   "概念混淆",
	"recall":    "记忆遗漏",
	"misread":   "审题偏差",
	"reasoning": "推理错误",
	"transfer":  "迁移不足",
	"careless":  "粗心笔误",
	"none":      "",
}

const (
	maxHints      = 3
	maxQuestion   = 800
	maxOption     = 200
	maxAnswerText = 1000
	maxHint       = 300
	maxLearner    = 2000 // characters of the learner's answer that are kept and judged
)

// Exercise is one generated practice question. The answer stays with the extension; tools that show the exercise to
// the assistant before it is answered leave it out.
type Exercise struct {
	ID          string     `json:"id"`
	Card        string     `json:"card,omitempty"`
	Topic       string     `json:"topic"`
	Kind        string     `json:"kind"`
	Question    string     `json:"question"`
	Options     []string   `json:"options,omitempty"`
	Answer      string     `json:"answer"`
	Accepted    []string   `json:"accepted,omitempty"`
	Hints       []string   `json:"hints"`
	Explanation string     `json:"explanation,omitempty"`
	Plain       bool       `json:"plain,omitempty"`  // made without the model, straight from the card
	Served      int        `json:"served,omitempty"` // the most hints handed out so far
	Created     time.Time  `json:"created"`
	Result      *Judgement `json:"result,omitempty"`
}

// valid reports whether a stored exercise holds together, so a damaged file cannot make a tool index past its hints.
func (e Exercise) valid() bool {
	if !validKind(e.Kind) || e.Question == "" || e.Answer == "" || len(e.Hints) == 0 || len(e.Hints) > maxHints {
		return false
	}
	if e.Kind == kindChoice {
		return len(e.Options) >= 3 && len(e.Options) <= 5 && len(e.Answer) == 1 && int(e.Answer[0])-'A' >= 0 && int(e.Answer[0])-'A' < len(e.Options)
	}
	return true
}

func (e Exercise) clone() Exercise {
	e.Options = append([]string(nil), e.Options...)
	e.Accepted = append([]string(nil), e.Accepted...)
	e.Hints = append([]string(nil), e.Hints...)
	if e.Result != nil {
		r := *e.Result
		e.Result = &r
	}
	return e
}

// Judgement is how an answer was judged.
type Judgement struct {
	Verdict   string    `json:"verdict"`
	Score     int       `json:"score"`
	Cause     string    `json:"cause,omitempty"`
	Diagnosis string    `json:"diagnosis,omitempty"`
	Feedback  string    `json:"feedback,omitempty"`
	Answer    string    `json:"answer"` // the learner's answer, clipped
	Hints     int       `json:"hints,omitempty"`
	Rough     bool      `json:"rough,omitempty"` // judged without the model, by wording alone
	Rated     string    `json:"rated,omitempty"` // the FSRS rating it recorded, when the card was due
	At        time.Time `json:"at"`
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// optionLetter is A, B, C… for option i.
func optionLetter(i int) string { return string(rune('A' + i)) }

// ---- making an exercise ----

//go:embed prompts/exercise.md
var exercisePrompt string

//go:embed prompts/judge.md
var judgePrompt string

//go:embed prompts/reinforce.md
var reinforcePrompt string

var exerciseSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["question","answer","hints"],"properties":{` +
	`"question":{"type":"string","maxLength":800},` +
	`"options":{"type":"array","maxItems":5,"items":{"type":"string","maxLength":200}},` +
	`"answer":{"type":"string","maxLength":1000},` +
	`"accepted":{"type":"array","maxItems":5,"items":{"type":"string","maxLength":200}},` +
	`"hints":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"string","maxLength":300}},` +
	`"explanation":{"type":"string","maxLength":1000}}}`)

var judgeSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["verdict","score","cause","diagnosis","feedback"],"properties":{` +
	`"verdict":{"type":"string","enum":["correct","partial","wrong"]},` +
	`"score":{"type":"integer","minimum":0,"maximum":100},` +
	`"cause":{"type":"string","enum":["concept","recall","misread","reasoning","transfer","careless","none"]},` +
	`"diagnosis":{"type":"string","maxLength":300},` +
	`"feedback":{"type":"string","maxLength":800}}}`)

// fenceTags are the tags the prompts fence untrusted text in.
var fenceTags = []string{"material", "exercise", "answer", "learner_profile"}

// tagNoise says whether r can sit inside a tag without changing what a model reads: spaces, controls, and the
// characters that draw nothing (zero-width and format characters, combining marks, variation selectors, fillers).
func tagNoise(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0: // Hangul fillers
		return true
	}
	return unicode.IsSpace(r) || unicode.In(r, unicode.Z, unicode.C, unicode.M)
}

// opensTag says whether the text after a "<" spells one of the prompts' own tags, opening or closing, in any case
// and whatever spacing or invisible characters are put in between.
func opensTag(rest []rune) bool {
	const look = 96 // how far a disguised tag name may be spread
	var name []rune
	slash := false
	for i := 0; i < len(rest) && i < look && len(name) < 16; i++ {
		r := rest[i]
		switch {
		case tagNoise(r):
		case r == '/' && !slash && len(name) == 0:
			slash = true
		case r >= 0xFF01 && r <= 0xFF5E: // full-width forms read as the plain letter
			name = append(name, unicode.ToLower(r-0xFEE0))
		case r == '_' || unicode.IsLetter(r):
			name = append(name, unicode.ToLower(r))
		default:
			i = len(rest)
		}
		for _, tag := range fenceTags {
			if string(name) == tag {
				return true
			}
		}
	}
	return false
}

// fenced wraps untrusted text in a tag the prompts name. Tag-like text inside loses its "<", so it can neither close
// the fence early nor open a fake one.
func fenced(tag, s string) string {
	if strings.Contains(s, "<") {
		in := []rune(s)
		for i, r := range in {
			if r == '<' && opensTag(in[i+1:]) {
				in[i] = '‹'
			}
		}
		s = string(in)
	}
	return "<" + tag + ">\n" + s + "\n</" + tag + ">"
}

// askModel runs one JSON completion and decodes the answer, also when the model ignored the schema and wrote text.
func askModel(ctx context.Context, m modelAPI, system, user string, schema json.RawMessage, label string, out any) error {
	if m == nil {
		return errNoModel
	}
	res, err := m.ModelComplete(ctx, wire.ModelCompleteParams{
		System: system, Messages: []wire.ModelMessage{{Role: "user", Content: user}},
		MaxTokens: 2000, JSONSchema: schema, Label: label,
	})
	if err != nil {
		return err
	}
	raw := res.JSON
	if len(raw) == 0 {
		raw = json.RawMessage(extractJSON(res.Text))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("模型没给出能用的结果")
	}
	return nil
}

var errNoModel = errors.New("没有模型可用：在扩展权限里允许使用模型")

// exerciseMaterial is the card asked about and a few others from the same knowledge point, for context and for
// believable wrong options.
func exerciseMaterial(c *Card, neighbours []*Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Knowledge point: %s\n\nCard to practise:\nQ: %s\nA: %s\n", topicOf(c), c.Front, c.Back)
	if len(neighbours) > 0 {
		b.WriteString("\nOther cards of the same knowledge point:\n")
		for _, n := range neighbours {
			fmt.Fprintf(&b, "- Q: %s\n  A: %s\n", oneLine(n.Front), oneLine(n.Back))
		}
	}
	return clip(b.String(), maxMaterial)
}

type modelExercise struct {
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Answer      string   `json:"answer"`
	Accepted    []string `json:"accepted"`
	Hints       []string `json:"hints"`
	Explanation string   `json:"explanation"`
}

// makeExercise asks the model for an exercise of one kind about card c. When there is no model, or what it wrote does
// not hold together, the card itself becomes a recall exercise with hints cut from its answer.
func makeExercise(ctx context.Context, m modelAPI, c *Card, neighbours []*Card, kind string, now time.Time) Exercise {
	base := Exercise{ID: newID(), Card: c.ID, Topic: topicOf(c), Kind: kind, Created: now.UTC()}
	if m != nil {
		var out modelExercise
		user := "Kind: " + kind + "\n\n" + fenced("material", exerciseMaterial(c, neighbours))
		if err := askModel(ctx, m, exercisePrompt, user, exerciseSchema, "study.exercise", &out); err == nil {
			if e, ok := checkExercise(base, out); ok {
				return e
			}
		}
	}
	return plainExercise(base, c)
}

// checkExercise holds what the model wrote to the kind's shape; it reports false when it does not fit.
func checkExercise(e Exercise, m modelExercise) (Exercise, bool) {
	e.Question = clip(strings.TrimSpace(m.Question), maxQuestion)
	e.Answer = clip(strings.TrimSpace(m.Answer), maxAnswerText)
	e.Explanation = clip(strings.TrimSpace(m.Explanation), maxAnswerText)
	for _, h := range m.Hints {
		if h = clip(strings.TrimSpace(h), maxHint); h != "" && len(e.Hints) < maxHints {
			e.Hints = append(e.Hints, h)
		}
	}
	if e.Question == "" || e.Answer == "" || len(e.Hints) == 0 {
		return e, false
	}
	switch e.Kind {
	case kindChoice:
		for _, o := range m.Options {
			if o = clip(strings.TrimSpace(o), maxOption); o != "" {
				e.Options = append(e.Options, o)
			}
		}
		if len(e.Options) < 3 || len(e.Options) > 5 {
			return e, false
		}
		k := choiceIndex(e.Answer, e.Options)
		if k < 0 {
			return e, false
		}
		e.Answer = optionLetter(k)
	case kindCloze:
		if !strings.Contains(e.Question, "____") {
			return e, false
		}
		for _, a := range m.Accepted {
			if a = clip(strings.TrimSpace(a), maxOption); a != "" && len(e.Accepted) < 5 {
				e.Accepted = append(e.Accepted, a)
			}
		}
	}
	return e, true
}

// plainExercise asks the card's own question, with hints that give away more of the answer each time.
func plainExercise(e Exercise, c *Card) Exercise {
	e.Kind, e.Plain = kindConcept, true
	e.Question, e.Answer = c.Front, c.Back
	e.Hints = plainHints(e.Topic, c.Back)
	e.Options, e.Accepted, e.Explanation = nil, nil, ""
	return e
}

func plainHints(topic, answer string) []string {
	r := []rune(strings.Join(strings.Fields(answer), " "))
	n := len(r)
	first := string(r[:min(n, max(1, min(6, n/5)))])
	half := string(r[:min(n, max(1, n*2/5))])
	return []string{
		fmt.Sprintf("先别急着看答案：这道题考的是「%s」。它最核心的一点是什么？", clip(topic, 40)),
		fmt.Sprintf("答案大约 %d 个字，开头是「%s…」。", n, first),
		fmt.Sprintf("再多给一点：「%s…」", half),
	}
}

// choiceIndex reads a choice answer: an option's own text first (an option may itself start with a letter and a
// space), then a letter alone (A, b., (C)), then a letter with a separator ("B. …", "C、…") whose rest, if any, is that
// option or a space-separated letter followed by that option's text.
func choiceIndex(ans string, options []string) int {
	n := normalize(ans)
	for k, o := range options {
		if n != "" && normalize(o) == n {
			return k
		}
	}
	t := strings.Trim(strings.TrimSpace(ans), "()（）.。、:： ")
	rs := []rune(t)
	letter := func(r rune) int {
		if k := int(unicode.ToUpper(r) - 'A'); r < unicode.MaxASCII && k >= 0 && k < len(options) {
			return k
		}
		return -1
	}
	if len(rs) == 1 {
		return letter(rs[0])
	}
	if len(rs) > 2 {
		if k := letter(rs[0]); k >= 0 {
			rest := normalize(strings.TrimLeft(string(rs[2:]), ".、:：)） "))
			switch {
			case strings.ContainsRune(".、:：)）", rs[1]):
				return k
			case rs[1] == ' ' && rest == normalize(options[k]):
				return k
			}
		}
	}
	return -1
}

// ---- judging ----

type modelJudgement struct {
	Verdict   string `json:"verdict"`
	Score     int    `json:"score"`
	Cause     string `json:"cause"`
	Diagnosis string `json:"diagnosis"`
	Feedback  string `json:"feedback"`
}

// giveUp is an answer that says the learner does not know.
func giveUp(ans string) bool {
	switch normalize(strings.Trim(ans, "。.!！?？ ")) {
	case "", "不知道", "不会", "忘了", "跳过", "pass", "skip", "idk", "i don't know", "dont know", "don't know":
		return true
	}
	return false
}

// judge grades an answer. Choice and exact fill-ins are graded here; open answers and near misses go to the model,
// which also diagnoses the mistake. Without a model an open answer is judged roughly by how much of the reference
// answer's wording it has.
func judge(ctx context.Context, m modelAPI, e Exercise, answer string, hints int, now time.Time) Judgement {
	answer = clip(strings.TrimSpace(answer), maxLearner)
	j := Judgement{Answer: answer, Hints: min(max(hints, 0), maxHints), At: now.UTC()}
	if giveUp(answer) {
		j.Verdict, j.Cause, j.Diagnosis = verdictWrong, "recall", "这次没想起来。"
		return j
	}
	fixed := ""
	switch e.Kind {
	case kindChoice:
		k := choiceIndex(answer, e.Options)
		if k >= 0 && optionLetter(k) == e.Answer {
			j.Verdict, j.Score, j.Cause = verdictCorrect, 100, "none"
			return j
		}
		fixed = verdictWrong
	case kindCloze:
		got := normalize(answer)
		for _, a := range append([]string{e.Answer}, e.Accepted...) {
			if got != "" && got == normalize(a) {
				j.Verdict, j.Score, j.Cause = verdictCorrect, 100, "none"
				return j
			}
		}
	}
	if m != nil {
		var out modelJudgement
		if err := askModel(ctx, m, judgePrompt, judgeInput(e, answer), judgeSchema, "study.judge", &out); err == nil {
			if _, ok := verdictLabels[out.Verdict]; ok {
				j.Verdict = out.Verdict
				j.Score = min(max(out.Score, 0), 100)
				if fixed != "" {
					j.Verdict, j.Score = fixed, 0
				}
				if _, ok := causeLabels[out.Cause]; ok {
					j.Cause = out.Cause
				}
				switch {
				case j.Verdict == verdictCorrect:
					j.Cause = "none"
				case j.Cause == "" || j.Cause == "none":
					j.Cause = "concept"
				}
				j.Diagnosis = clip(strings.TrimSpace(out.Diagnosis), 300)
				j.Feedback = clip(strings.TrimSpace(out.Feedback), 800)
				return j
			}
		}
	}
	return roughJudge(e, j, fixed)
}

func judgeInput(e Exercise, answer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Kind: %s\nKnowledge point: %s\nQuestion:\n%s\n", e.Kind, e.Topic, e.Question)
	for k, o := range e.Options {
		fmt.Fprintf(&b, "%s. %s\n", optionLetter(k), o)
	}
	fmt.Fprintf(&b, "Reference answer:\n%s\n", e.Answer)
	if len(e.Accepted) > 0 {
		fmt.Fprintf(&b, "Also accepted: %s\n", strings.Join(e.Accepted, " | "))
	}
	if e.Explanation != "" {
		fmt.Fprintf(&b, "Explanation:\n%s\n", e.Explanation)
	}
	return fenced("exercise", b.String()) + "\n\n" + fenced("answer", answer)
}

// roughJudge is the judgement without a model: a choice is right or wrong; other answers by the share of the
// reference answer's character pairs they contain.
func roughJudge(e Exercise, j Judgement, fixed string) Judgement {
	j.Rough = true
	if fixed != "" {
		j.Verdict, j.Cause = fixed, "concept"
		if e.Kind == kindChoice {
			j.Diagnosis = "正确答案是 " + e.Answer + "。"
		}
		return j
	}
	r := overlap(e.Answer, j.Answer)
	j.Score = int(r * 100)
	switch {
	case r >= 0.6:
		j.Verdict, j.Cause = verdictCorrect, "none"
	case r >= 0.3:
		j.Verdict, j.Cause, j.Diagnosis = verdictPartial, "recall", "只答到了一部分要点。"
	default:
		j.Verdict, j.Cause, j.Diagnosis = verdictWrong, "recall", "和参考答案差得比较多。"
	}
	return j
}

// overlap is the share of the reference's adjacent character pairs (ignoring spaces, case and punctuation) that the
// answer also has.
func overlap(ref, ans string) float64 {
	pairs := func(s string) map[string]int {
		var rs []rune
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				rs = append(rs, r)
			}
		}
		out := map[string]int{}
		if len(rs) == 1 {
			out[string(rs)]++
		}
		for i := 0; i+1 < len(rs); i++ {
			out[string(rs[i:i+2])]++
		}
		return out
	}
	want, got := pairs(ref), pairs(ans)
	total, hit := 0, 0
	for p, n := range want {
		total += n
		hit += min(n, got[p])
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

// ratingFor maps a judged answer to an FSRS rating: wrong is Again; partial, or right only after two hints, is Hard;
// otherwise Good. Practice never gives Easy: the learner says that themselves in a review.
func ratingFor(j Judgement) fsrs.Rating {
	switch {
	case j.Verdict == verdictWrong:
		return fsrs.Again
	case j.Verdict == verdictPartial || j.Hints >= 2:
		return fsrs.Hard
	}
	return fsrs.Good
}

// ---- app side ----

// practice makes an exercise for a card (or the weakest card of a topic, or overall) and stores it. The model call
// is made without the lock.
func (a *app) practice(ctx context.Context, cardID, topic, kind string) (Exercise, error) {
	if kind == "" {
		kind = kindConcept
	}
	if !validKind(kind) {
		return Exercise{}, errors.New("题型只能是概念辨析、填空、选择或场景变式")
	}
	a.mu.Lock()
	now := a.now()
	var c *Card
	if cardID != "" {
		c = a.st.Cards[cardID]
		if c == nil {
			a.mu.Unlock()
			return Exercise{}, errors.New("没有这张卡片")
		}
	} else {
		c = weakestCard(a.sched, a.st, now, topic)
		if c == nil {
			a.mu.Unlock()
			if topic != "" {
				return Exercise{}, errors.New("这个知识点下还没有卡片")
			}
			return Exercise{}, errors.New("还没有卡片，先加一些再练习")
		}
	}
	card := *c
	var neighbours []*Card
	for _, o := range a.st.sortedCards() {
		if o.ID != card.ID && !o.Suspended && topicOf(o) == topicOf(&card) && len(neighbours) < 4 {
			cp := *o
			neighbours = append(neighbours, &cp)
		}
	}
	m := a.model()
	a.mu.Unlock()

	e := makeExercise(ctx, m, &card, neighbours, kind, now)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.update(func(s *state) error { s.putExercise(e); return nil }); err != nil {
		return Exercise{}, err
	}
	return e, nil
}

func (a *app) exercise(id string) (Exercise, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.st.Exercises[id]
	if e == nil {
		return Exercise{}, false
	}
	return e.clone(), true
}

// serveHint hands out hints up to level (1 to the exercise's count) and remembers the most handed out, so the
// grading knows how much help was given whatever the caller says. It answers how many are out now.
func (a *app) serveHint(id string, level int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.st.Exercises[id]
	if e == nil {
		return 0, errors.New("这道题已经过期了，换一道吧")
	}
	level = min(max(level, 1), len(e.Hints))
	if level > e.Served && e.Result == nil {
		if err := a.update(func(s *state) error { s.Exercises[id].Served = level; return nil }); err != nil {
			return 0, err
		}
	}
	return level, nil
}

// checkAnswer judges an answer to a stored exercise, logs the attempt and, when the card is due, records the review
// with FSRS. An exercise is judged once: answering it again gives the first judgement, so an answer written after
// seeing the reference answer does not count.
func (a *app) checkAnswer(ctx context.Context, id, answer string, hints int) (Exercise, error) {
	e, ok := a.exercise(id)
	if !ok {
		return Exercise{}, errors.New("这道题已经过期了，换一道吧")
	}
	if e.Result != nil {
		return e, nil
	}
	hints = max(hints, e.Served)
	m := a.model()
	j := judge(ctx, m, e, answer, hints, a.now())
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	err := a.update(func(s *state) error {
		x := s.Exercises[id]
		if x == nil {
			return errors.New("这道题已经过期了，换一道吧")
		}
		if x.Result != nil { // judged meanwhile by another call
			e = x.clone()
			return nil
		}
		if c := s.Cards[x.Card]; c != nil && !c.Suspended && c.FSRS.State != fsrs.New && !c.FSRS.Due.After(now) {
			r := ratingFor(j)
			if _, err := a.sched.grade(s, c.ID, r, now); err != nil {
				return err
			}
			j.Rated = ratingKey(r)
		}
		x.Result = &j
		s.logAttempt(Attempt{Exercise: x.ID, Card: x.Card, Topic: x.Topic, Kind: x.Kind, Verdict: j.Verdict, Cause: j.Cause, Hints: j.Hints,
			Rated: j.Rated != "", At: now.UTC()})
		e = x.clone()
		return nil
	})
	return e, err
}

// reinforce drafts cards that target a mistake (exercise given) or a weak knowledge point (topic given). They wait
// on the page like any other drafts and join FSRS once kept.
func (a *app) reinforce(ctx context.Context, exerciseID, topic string, n int) (int, string, error) {
	n = min(max(n, 1), 8)
	a.mu.Lock()
	now := a.now()
	var material, name, ref string
	var plain []cardText
	switch {
	case exerciseID != "":
		e := a.st.Exercises[exerciseID]
		if e == nil {
			a.mu.Unlock()
			return 0, "", errors.New("这道题已经过期了")
		}
		if e.Result == nil || e.Result.Verdict == verdictCorrect {
			a.mu.Unlock()
			return 0, "", errors.New("只有答错或只答对一部分的题才做强化卡")
		}
		name, ref = e.Topic, "exercise:"+e.ID
		material = reinforceFromExercise(*e)
		plain = []cardText{exerciseCard(*e)}
	case topic != "":
		var g *topicScore
		for _, t := range masteryByTopic(a.sched, a.st, now) {
			if t.Topic == topic {
				g = &t
				break
			}
		}
		if g == nil {
			a.mu.Unlock()
			return 0, "", errors.New("没有这个知识点")
		}
		name, ref = g.Topic, "topic:"+g.Topic
		material = reinforceFromTopic(*g, a.st)
		var missed []*Exercise
		for _, e := range a.st.Exercises {
			if e.Topic == topic && e.Result != nil && e.Result.Verdict != verdictCorrect {
				missed = append(missed, e)
			}
		}
		sort.Slice(missed, func(i, j int) bool { return missed[i].Created.After(missed[j].Created) })
		for _, e := range missed[:min(len(missed), n)] {
			plain = append(plain, exerciseCard(*e))
		}
	default:
		a.mu.Unlock()
		return 0, "", errors.New("说明要针对哪道题或哪个知识点")
	}
	m := a.model()
	a.mu.Unlock()

	var cards []cardText
	if m != nil {
		var out struct {
			Cards []cardText `json:"cards"`
		}
		system := strings.ReplaceAll(reinforcePrompt, "{{N}}", fmt.Sprint(n))
		if err := askModel(ctx, m, system, fenced("material", material), cardsSchema, "study.reinforce", &out); err == nil {
			for _, c := range out.Cards {
				if strings.TrimSpace(c.Front) != "" && strings.TrimSpace(c.Back) != "" && len(cards) < n {
					cards = append(cards, cardText{Front: c.Front, Back: c.Back})
				}
			}
		}
	}
	if len(cards) == 0 {
		cards = plain
	}
	if len(cards) == 0 {
		return 0, name, errors.New("这个知识点还没有答错的题，模型也不可用，先练几道再来")
	}
	for i := range cards {
		cards[i].Topic = name
	}
	k, err := a.addDrafts(cards, Source{Kind: srcTutor, Ref: clip(ref, 200), Title: clip("强化 · "+name, 200)})
	return k, name, err
}

// exerciseCard is a wrong answer turned into a card as it stands: the question (with options) and the right answer.
func exerciseCard(e Exercise) cardText {
	var front strings.Builder
	front.WriteString(e.Question)
	for k, o := range e.Options {
		front.WriteString("\n" + optionLetter(k) + ". " + o)
	}
	back := e.Answer
	if e.Kind == kindChoice && e.Answer != "" {
		if k := int(e.Answer[0]) - 'A'; k >= 0 && k < len(e.Options) {
			back = e.Answer + ". " + e.Options[k]
		}
	}
	if e.Explanation != "" {
		back += "\n" + e.Explanation
	}
	return cardText{Front: front.String(), Back: back}
}

func reinforceFromExercise(e Exercise) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Knowledge point: %s\nExercise (%s):\n%s\n", e.Topic, e.Kind, e.Question)
	for k, o := range e.Options {
		fmt.Fprintf(&b, "%s. %s\n", optionLetter(k), o)
	}
	fmt.Fprintf(&b, "Correct answer: %s\n", e.Answer)
	if e.Explanation != "" {
		fmt.Fprintf(&b, "Explanation: %s\n", e.Explanation)
	}
	if r := e.Result; r != nil {
		fmt.Fprintf(&b, "Learner's answer: %s\nVerdict: %s\nMistake: %s %s\n", r.Answer, r.Verdict, r.Cause, r.Diagnosis)
	}
	return clip(b.String(), maxMaterial)
}

func reinforceFromTopic(g topicScore, st state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Knowledge point: %s (mastery %d/100)\nWeakest cards:\n", g.Topic, g.Score)
	for i, cs := range g.Cards {
		if i == 8 {
			break
		}
		fmt.Fprintf(&b, "- Q: %s\n  A: %s\n  mastery %d, lapses %d\n", oneLine(cs.Card.Front), oneLine(cs.Card.Back), cs.Score, cs.Card.FSRS.Lapses)
	}
	b.WriteString("Recent mistakes:\n")
	n := 0
	for i := len(st.Attempts) - 1; i >= 0 && n < 8; i-- {
		at := st.Attempts[i]
		if at.Topic != g.Topic || at.Verdict == verdictCorrect {
			continue
		}
		n++
		line := "- " + at.Kind + " " + at.Verdict + " " + at.Cause
		if e := st.Exercises[at.Exercise]; e != nil && e.Result != nil {
			line += ": " + oneLine(e.Question) + " / learner: " + oneLine(e.Result.Answer) + " / " + e.Result.Diagnosis
		}
		b.WriteString(line + "\n")
	}
	return clip(b.String(), maxMaterial)
}
