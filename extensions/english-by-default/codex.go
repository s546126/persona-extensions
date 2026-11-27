package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Codex CLI support. Codex (checked with codex-cli 0.171.0-alpha.13.2) reads the same hook layout as Claude Code,
// from <CODEX_HOME>/hooks.json: {"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":…,"timeout":…}]}]}},
// gives the hook the same input on stdin (hook_event_name, session_id, prompt, transcript_path, cwd, plus model,
// permission_mode and turn_id) and takes the same answer on stdout ({"decision":"block","reason":…} or
// hookSpecificOutput.additionalContext and systemMessage; its output schema allows no other field). So the entry,
// the edit of the file (settings.go) and the hook's answer (hook.go) are Claude Code's; what differs is:
//
//   - Codex runs a hook only after the user has reviewed it (/hooks in the Codex TUI). Until then it skips the hook
//     without a word, and a changed entry (a new version of this extension runs from a new path) needs reviewing
//     again. Codex keeps the review in its config.toml, under [hooks.state."<hooks.json>:user_prompt_submit:<group>:
//     <handler>"] with a trusted_hash. This extension never writes that: trusting its own hook would skip the review
//     Codex asks of the user. It only reads whether its slot has a review, for the page.
//   - The review is keyed by the group's place in the list, so the entry is always added at the end (a group of
//     another tool's after it would move when it is taken out, and want reviewing again).
//   - The transcript is Codex's rollout: a user turn is a response_item message with role user that is not the
//     <environment_context> (or another <…> block) Codex writes itself.

const (
	agentClaude = "claude"
	agentCodex  = "codex"

	defaultCodexDir = "~/.codex"
	codexEvent      = "user_prompt_submit"
)

// codexHooksPath is dir's hooks.json, through a symlink.
func codexHooksPath(dir string) string {
	p := filepath.Join(dir, "hooks.json")
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// codexReviewed says whether Codex has a review on record for our entry in the hooks file at hooksPath: the
// config.toml next to it has a trusted_hash under our slot's [hooks.state."…"] table. Codex checks the hash against
// the entry itself, so a record from before a change may be stale; the page says so.
func codexReviewed(dir, hooksPath string) bool {
	gi := ourGroupIndex(hooksPath)
	if gi < 0 {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return false
	}
	paths := []string{hooksPath, filepath.Join(dir, "hooks.json")}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		paths = append(paths, filepath.Join(real, "hooks.json"))
	}
	for _, p := range paths {
		if tomlTableHas(b, fmt.Sprintf("hooks.state.%q", fmt.Sprintf("%s:%s:%d:0", p, codexEvent, gi)), "trusted_hash") {
			return true
		}
	}
	return false
}

// ourGroupIndex is the place of our group in the hooks file's UserPromptSubmit list; -1 when it is not there.
func ourGroupIndex(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	var top jsonObject
	if json.Unmarshal(b, &top) != nil {
		return -1
	}
	_, groups, err := eventGroups(&top)
	if err != nil {
		return -1
	}
	for i, g := range groups {
		if _, ours, _ := splitOurs([]json.RawMessage{g}); len(ours) == 1 {
			return i
		}
	}
	return -1
}

// tomlTableHas says whether the TOML document b has a table [header] with a key named key in it. It is no TOML
// parser: it reads header lines and key = value lines, which is all Codex writes there.
func tomlTableHas(b []byte, header, key string) bool {
	in := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = line == "["+header+"]"
			continue
		}
		if in {
			if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
				return true
			}
		}
	}
	return false
}

// hasCodexUserTurn is hasUserTurn for a Codex rollout.
func hasCodexUserTurn(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 4<<20))
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Type == "event_msg" && e.Payload.Type == "user_message" {
			return true
		}
		if e.Type != "response_item" || e.Payload.Type != "message" || e.Payload.Role != "user" {
			continue
		}
		for _, c := range e.Payload.Content {
			if t := strings.TrimSpace(c.Text); c.Type == "input_text" && t != "" && !strings.HasPrefix(t, "<") {
				return true
			}
		}
	}
	return false
}
