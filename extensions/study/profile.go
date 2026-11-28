package main

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The learner profile: three optional settings that only change how something is explained (depth, examples and
// analogies, tone), never what the material says. With all three empty nothing is added to any prompt.
const (
	levelBeginner  = "beginner"
	levelPractical = "practical"
	levelDeep      = "deep"

	styleBalanced = "balanced"
	styleAnalogy  = "analogy"
	styleRigorous = "rigorous"

	maxInterests = 200
)

var learnerLevels = map[string]string{
	levelBeginner:  "beginner: new to the subject; start from intuition and define every term",
	levelPractical: "practical: wants to apply it; favour worked examples and when-to-use guidance",
	levelDeep:      "deep: comfortable with the basics; include mechanisms, edge cases and trade-offs",
}

var explainStyles = map[string]string{
	styleBalanced: "balanced: plain explanation with an example where it helps",
	styleAnalogy:  "analogy: lead with an everyday comparison, then state the exact idea",
	styleRigorous: "rigorous: precise definitions and conditions first, few metaphors",
}

// profileTag matches an opening or closing tag of any block the prompts fence data in, in any letter case, closed or
// not. The profile text is the user's own setting, yet it is still data inside a prompt: it must not be able to end
// its block or open another one.
var profileTag = regexp.MustCompile(`(?i)<\s*/?\s*(learner_profile|material|exercise|answer)\b[^<>]*>?`)

// cleanInterests keeps the interests setting to one short line with no fence tag and no control character.
func cleanInterests(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	for range 8 { // a tag may be put together from the pieces around a removed one
		next := profileTag.ReplaceAllString(s, " ")
		if next == s {
			break
		}
		s = next
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029':
			return -1
		case r == '<':
			return '‹' // whatever is left of a tag after eight rounds cannot open one either
		case r == '>':
			return '›'
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxInterests {
		s = strings.TrimSpace(string(r[:maxInterests]))
	}
	return s
}

// formatLearnerProfile is the <learner_profile> block the prompts get before their material, or "" when the user
// set none of the three, so a request then is byte for byte what 0.2.0 sent.
func formatLearnerProfile(c config) string {
	level, style, interests := learnerLevels[c.LearnerLevel], explainStyles[c.ExplainStyle], cleanInterests(c.LearnerInterests)
	if level == "" && style == "" && interests == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<learner_profile>\n")
	if level != "" {
		b.WriteString("Level: " + level + "\n")
	}
	if interests != "" {
		b.WriteString("Interests: " + interests + "\n")
	}
	if style != "" {
		b.WriteString("Style: " + style + "\n")
	}
	b.WriteString("</learner_profile>")
	return b.String()
}

// profiled puts the learner profile in front of the first user message of every model call made through it.
type profiled struct {
	m       modelAPI
	profile string
}

func (p profiled) ModelComplete(ctx context.Context, req wire.ModelCompleteParams) (wire.ModelCompleteResult, error) {
	msgs := append([]wire.ModelMessage(nil), req.Messages...)
	for i := range msgs {
		if msgs[i].Role == "user" {
			msgs[i].Content = p.profile + "\n\n" + msgs[i].Content
			break
		}
	}
	req.Messages = msgs
	return p.m.ModelComplete(ctx, req)
}

// withLearner wraps a model so its prompts carry the profile; without a model or a profile it changes nothing.
func withLearner(m modelAPI, profile string) modelAPI {
	if m == nil || profile == "" {
		return m
	}
	return profiled{m: m, profile: profile}
}

// model is the hub's model with the learner profile attached, or nil when the extension has no hub. The
// configuration is fixed once the program has started, so reading it needs no lock.
func (a *app) model() modelAPI {
	if a.hub == nil {
		return nil
	}
	return withLearner(a.hub, formatLearnerProfile(a.cfg))
}
