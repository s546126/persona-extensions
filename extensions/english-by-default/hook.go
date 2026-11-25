package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The hook: what Claude Code runs on UserPromptSubmit (settings.go writes the entry). It reads the prompt on stdin
// and answers in Claude Code's terms: {"decision":"block","reason":…} to hold the prompt back, or additional
// context and a system message to let it through with them. It prints nothing, and so changes nothing, when:
//
//   - the node program is not running (its heartbeat file is stale): an extension switched off, stopped or
//     uninstalled without the chance to take the entry out does nothing;
//   - the prompt comes from the extension's own model call (internalEnv) or from a Persona assistant conversation
//     (Persona's node sets assistantEnv for those): the assistant is how the user talks to Persona, not practice;
//   - its input or settings cannot be read, or anything else fails.

const (
	heartbeatFile  = "heartbeat"
	optionsFile    = "options.json"
	heartbeatEvery = time.Minute
	heartbeatFresh = 5 * time.Minute
)

// assistantEnvs mark a Claude Code process that Persona's node started for its assistant (the current and the old
// name); Claude Code passes its environment to the hook.
var assistantEnvs = []string{"PERSONA_ASSISTANT", "JARVIS_ASSISTANT"}

type hookArgs struct {
	Data string // the node program's data directory: heartbeat, options and the store

	getenv func(string) string // nil is os.Getenv
	ask    llm                 // nil is the configured command
}

type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Prompt        string `json:"prompt"`
}

func runHook(a hookArgs, in io.Reader, out io.Writer, now time.Time) {
	getenv := a.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv(internalEnv) != "" {
		return
	}
	for _, k := range assistantEnvs {
		if getenv(k) == "1" {
			return
		}
	}
	if a.Data == "" {
		return
	}
	st, err := os.Stat(filepath.Join(a.Data, heartbeatFile))
	if err != nil || now.Sub(st.ModTime()) > heartbeatFresh {
		return
	}
	raw, err := os.ReadFile(filepath.Join(a.Data, optionsFile))
	if err != nil {
		return
	}
	var opts options
	if json.Unmarshal(raw, &opts) != nil {
		return
	}
	opts = opts.withDefaults()

	b, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return
	}
	var req hookInput
	if json.Unmarshal(b, &req) != nil {
		return
	}
	if req.HookEventName != "" && req.HookEventName != "UserPromptSubmit" {
		return
	}
	session := req.SessionID
	if session == "" {
		session = "default"
	}
	ask := a.ask
	if ask == nil {
		ask = commandLLM(opts.LLMCommand)
	}
	g := &gate{opts: opts, store: store{dir: a.Data}, ask: ask, now: now}
	writeResult(out, g.decide(session, req.Prompt))
}

// writeResult writes the gate's answer as Claude Code's hook output; nothing when there is nothing to say.
func writeResult(out io.Writer, r result) {
	var v map[string]any
	switch {
	case r.Block:
		v = map[string]any{"decision": "block", "reason": r.Reason}
	case r.AdditionalContext != "" || r.SystemMessage != "":
		v = map[string]any{}
		if r.AdditionalContext != "" {
			v["hookSpecificOutput"] = map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": r.AdditionalContext}
		}
		if r.SystemMessage != "" {
			v["systemMessage"] = r.SystemMessage
		}
	default:
		return
	}
	b, err := marshalPlain(v)
	if err != nil {
		return
	}
	_, _ = out.Write(append(b, '\n'))
}
