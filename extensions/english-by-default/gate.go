package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// The gate: what one prompt leads to. It never fails the prompt: when the store or the model fails, the prompt
// goes through (fail open), because a tool for practice must not stand between the user and their work.

// options are the settings the hook works by; the node program writes them for it (optionsFile).
type options struct {
	Mode           string   `json:"mode"` // block, warn, log
	JudgeThreshold int      `json:"judge_threshold"`
	Stopwords      []string `json:"stopwords"`
	GiveupWords    []string `json:"giveup_words"`
	LLMCommand     []string `json:"llm_command"`
	LLMTimeoutSecs int      `json:"llm_timeout_seconds"`
	// EchoInReply: in a session Persona runs, ask the model to start its reply with the English, since Persona's
	// apps show neither the hook's message nor its context.
	EchoInReply *bool `json:"echo_in_reply,omitempty"`
}

var defaultOptions = options{
	Mode:           "warn",
	JudgeThreshold: 70,
	Stopwords:      []string{"紧急", "加急", "urgent", "asap", "立刻", "马上", "线上事故", "hotfix", "incident"},
	GiveupWords:    []string{"giveup", "give up", "放弃", "算了"},
	LLMCommand:     []string{"claude", "-p", "--model", "haiku", "--no-session-persistence"},
	LLMTimeoutSecs: 90,
	EchoInReply:    &yes,
}

var yes = true

// withDefaults fills what the settings left out.
func (o options) withDefaults() options {
	d := defaultOptions
	switch o.Mode {
	case "block", "warn", "log":
	default:
		o.Mode = d.Mode
	}
	if o.JudgeThreshold <= 0 || o.JudgeThreshold > 100 {
		o.JudgeThreshold = d.JudgeThreshold
	}
	if o.Stopwords == nil {
		o.Stopwords = d.Stopwords
	}
	if o.GiveupWords == nil {
		o.GiveupWords = d.GiveupWords
	}
	if len(o.LLMCommand) == 0 {
		o.LLMCommand = d.LLMCommand
	}
	if o.LLMTimeoutSecs <= 0 {
		o.LLMTimeoutSecs = d.LLMTimeoutSecs
	}
	if o.EchoInReply == nil {
		o.EchoInReply = d.EchoInReply
	}
	return o
}

// result is the gate's answer, which the hook writes in Claude Code's terms.
type result struct {
	Block             bool
	Reason            string // shown to the user when blocked
	AdditionalContext string // given to the model with the prompt
	SystemMessage     string // shown to the user
}

type gate struct {
	opts  options
	store store
	ask   llm
	now   time.Time
	// persona: the session is one that Persona's node started (hook.go). Persona's apps show a block's reason
	// (Claude Code writes it to the transcript as a notice) but not the hook's other messages (echo); and the node
	// types a session's first prompt again when it does not reach the transcript, so a first prompt (firstPrompt)
	// held back would be sent three times and the session reported as stuck: there block works as warn.
	persona, firstPrompt bool
}

// mode is the mode this prompt is handled by.
func (g *gate) mode() string {
	if g.persona && g.firstPrompt && g.opts.Mode == "block" {
		return "warn"
	}
	return g.opts.Mode
}

func (g *gate) echo() bool { return g.persona && g.opts.EchoInReply != nil && *g.opts.EchoInReply }

func (g *gate) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(g.opts.LLMTimeoutSecs)*time.Second)
}

func (g *gate) save(e entry) { _ = g.store.add(e, g.now) }

// decide answers one prompt of a session.
func (g *gate) decide(session, prompt string) result {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return result{}
	}
	// A session waiting for a rewrite comes first: a rewrite may well start with / (a path), and must not slip
	// past the check below and leave the blocked prompt behind.
	if p, _ := g.store.getPending(session, g.now); p != nil {
		return g.rewrite(session, text, *p)
	}
	if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "!") {
		return result{} // slash commands and shell escapes
	}
	if !isNonEnglish(text) {
		return result{}
	}
	lang := detectLanguage(text)
	m := messagesFor(lang)

	if hasStopword(text, g.opts.Stopwords) {
		g.save(entry{Original: text, Mode: "stopword", Language: lang, Session: session})
		return result{SystemMessage: m.stopword}
	}

	switch g.mode() {
	case "block":
		if err := g.store.setPending(session, text, lang, g.now); err != nil {
			return result{} // without the pending record the rewrite could not be matched: let it through
		}
		return result{Block: true, Reason: m.blockHeader + "\n" + m.rewrite + "\n" + m.giveupHint}
	case "log":
		// Silent, and no model call: the English is filled in later, when the pair is reviewed. The model
		// understands the original anyway.
		g.save(entry{Original: text, Mode: "log", Language: lang, Session: session})
		return result{}
	}

	// warn: the English goes to the user and, as context, to the model.
	ctx, cancel := g.context()
	defer cancel()
	english, err := translate(ctx, g.ask, text)
	if err != nil {
		g.save(entry{Original: text, Mode: "warn", Language: lang, Session: session})
		return result{SystemMessage: fill(m.warn, "english", m.translationFailed)}
	}
	g.save(entry{Original: text, English: english, Mode: "warn", Language: lang, Session: session})
	return result{AdditionalContext: warnContext(english, g.echo()), SystemMessage: fill(m.warn, "english", english)}
}

// rewrite answers a prompt of a session whose last prompt was blocked.
func (g *gate) rewrite(session, text string, p pending) result {
	m := messagesFor(p.Language)

	if isGiveup(text, g.opts.GiveupWords) {
		_ = g.store.clearPending(session)
		ctx, cancel := g.context()
		defer cancel()
		english, err := translate(ctx, g.ask, p.Original)
		g.save(entry{Original: p.Original, English: english, Mode: "giveup", Language: p.Language, Session: session})
		if err != nil {
			// The rewrite was "giveup": the original goes to the model in its place.
			return result{AdditionalContext: giveupContext(p.Original, ""), SystemMessage: m.giveupFailed}
		}
		return result{AdditionalContext: giveupContext(p.Original, english), SystemMessage: fill(m.giveupResult, "english", english)}
	}

	if isNonEnglish(text) {
		_ = g.store.setPending(session, p.Original, p.Language, g.now)
		return result{Block: true, Reason: fill(m.stillNonEnglish, "original", p.Original) + "\n" + m.giveupHint}
	}

	ctx, cancel := g.context()
	defer cancel()
	v, err := judge(ctx, g.ask, p.Original, text)
	if err != nil {
		_ = g.store.clearPending(session)
		g.save(entry{Original: p.Original, English: text, Mode: "unverified", Language: p.Language, Session: session})
		return result{SystemMessage: m.judgeDown}
	}
	score := v.Score
	if passed(v, g.opts.JudgeThreshold) {
		_ = g.store.clearPending(session)
		g.save(entry{Original: p.Original, English: text, Mode: "rewrite", Language: p.Language, Score: &score,
			Natural: v.Natural, NaturalHint: v.NaturalHint, Session: session})
		return result{SystemMessage: fill(m.matchOK, "score", strconv.Itoa(score))}
	}
	_ = g.store.setPending(session, p.Original, p.Language, g.now)
	hint := v.Hint
	if hint == "" {
		hint = m.hintFallback
	}
	return result{Block: true, Reason: fill(m.mismatch, "score", strconv.Itoa(score), "hint", hint)}
}

// warnContext gives the model the English of the user's prompt. The prompt stays the request: a translation can
// be wrong. With echo, the model is asked to show the English at the top of its reply, where a Persona app shows it.
func warnContext(english string, echo bool) string {
	s := "english-by-default: The user is practicing writing prompts in English. Their message in English, for " +
		"reference (their own words above remain the request):\n" + english
	if echo {
		s += "\nThe user reads this conversation in an app that does not show this note. Begin your reply with one " +
			"line, exactly: English: " + oneLine(english) + "\nThen answer the request as usual."
	}
	return s
}

// giveupContext tells the model what the user meant: their "giveup" replaced the request they wrote before.
func giveupContext(original, english string) string {
	s := "english-by-default: The user's real request is the message they wrote before this one, in another language:\n" + original
	if english != "" {
		s += "\nEnglish translation:\n" + english + "\nFirst show the user this English translation so they can learn it, then respond to the translated request."
	} else {
		s += "\nRespond to that request."
	}
	return s
}
