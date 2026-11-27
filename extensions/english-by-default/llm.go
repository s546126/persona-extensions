package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The model calls: translating a prompt and judging an English rewrite. They run the configured command on this
// computer (claude -p with haiku by default), with the prompt on standard input, so it never shows in the process
// list and has no length limit, and in the temporary directory, so no project's CLAUDE.md or transcript is touched.
// internalEnv is set for the command, so that its own prompt does not come back to this hook.

const internalEnv = "EBD_INTERNAL"

// llm runs one model call: prompt in, answer out.
type llm func(ctx context.Context, prompt string) (string, error)

// commandLLM is the configured command as an llm.
func commandLLM(argv []string) llm {
	return func(ctx context.Context, prompt string) (string, error) {
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return "", errors.New("no command configured")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(prompt)
		cmd.Dir = os.TempDir()
		cmd.Env = append(os.Environ(), internalEnv+"=1")
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("%s: %w", argv[0], ctx.Err())
			}
			return "", fmt.Errorf("%s: %v: %s", argv[0], err, oneLine(errOut.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
}

func translatePrompt(text string) string {
	return "Translate the following text into natural, concise English suitable as a prompt to an AI coding assistant. " +
		"Output ONLY the English translation, nothing else.\n\n" + text
}

// translate is the prompt in English.
func translate(ctx context.Context, ask llm, text string) (string, error) {
	out, err := ask(ctx, translatePrompt(text))
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errors.New("empty translation")
	}
	return out, nil
}

// verdict is the judge's answer on an English rewrite: whether it means the same as the original, and whether it
// reads as natural English.
type verdict struct {
	Equivalent  bool   `json:"equivalent"`
	Score       int    `json:"score"`
	Hint        string `json:"hint"`
	Natural     *bool  `json:"natural"`
	NaturalHint string `json:"naturalHint"`
}

func judgePrompt(original, attempt string) string {
	return "You are a language coach. Compare the MEANING of two texts.\n" +
		"A (original, may be any language):\n" + original + "\n\n" +
		"B (user's English rewrite):\n" + attempt + "\n\n" +
		"Judge whether B expresses the same intent and key details as A. Minor wording differences are fine; " +
		"missing key requirements, wrong scope, or changed intent are not.\n" +
		"Also judge whether B reads as natural, idiomatic English a native speaker would actually write " +
		"(as opposed to awkward, overly literal, or non-native-sounding phrasing).\n" +
		`Reply with ONLY a JSON object: {"equivalent": true|false, "score": 0-100, ` +
		`"hint": "if not equivalent, a short hint (in the language of A) about what is missing or wrong — do NOT give the full translation", ` +
		`"natural": true|false, "naturalHint": "if natural=false, a more natural/idiomatic way to phrase B, else empty string"}`
}

// judge asks whether attempt says what original says.
func judge(ctx context.Context, ask llm, original, attempt string) (verdict, error) {
	out, err := ask(ctx, judgePrompt(original, attempt))
	if err != nil {
		return verdict{}, err
	}
	raw, err := firstJSONObject(out)
	if err != nil {
		return verdict{}, err
	}
	// Models write numbers and booleans as strings now and then: read them leniently, and count only a real yes
	// (a string "false" must not pass for true).
	var loose struct {
		Equivalent  json.RawMessage `json:"equivalent"`
		Score       json.RawMessage `json:"score"`
		Hint        string          `json:"hint"`
		Natural     json.RawMessage `json:"natural"`
		NaturalHint string          `json:"naturalHint"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil {
		return verdict{}, fmt.Errorf("judge answer: %w", err)
	}
	v := verdict{Hint: strings.TrimSpace(loose.Hint), NaturalHint: strings.TrimSpace(loose.NaturalHint)}
	v.Equivalent = looseBool(loose.Equivalent) == 1
	v.Score = looseScore(loose.Score)
	switch looseBool(loose.Natural) {
	case 1:
		t := true
		v.Natural = &t
	case 0:
		f := false
		v.Natural = &f
	}
	return v, nil
}

// looseBool is 1 for true, 0 for false and -1 for anything else.
func looseBool(raw json.RawMessage) int {
	switch strings.Trim(strings.ToLower(strings.TrimSpace(string(raw))), `"`) {
	case "true":
		return 1
	case "false":
		return 0
	}
	return -1
}

func looseScore(raw json.RawMessage) int {
	var f float64
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if _, err := fmt.Sscan(s, &f); err != nil {
		return 0
	}
	return max(0, min(100, int(f+0.5)))
}

// firstJSONObject is the first balanced {…} in s, skipping braces inside strings.
func firstJSONObject(s string) ([]byte, error) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, errors.New("no JSON object in the answer")
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return []byte(s[start : i+1]), nil
			}
		}
	}
	return nil, errors.New("unbalanced JSON object in the answer")
}

// passed says whether a verdict lets the rewrite through.
func passed(v verdict, threshold int) bool {
	return v.Equivalent || v.Score >= threshold
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
