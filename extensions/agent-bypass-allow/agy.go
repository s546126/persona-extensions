package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Antigravity CLI (agy): Persona drives terminal-less agy conversations through its ACP companion
// (session/request_permission, session ids "agy-<uuid>", agent "agy"), and agy runs PreToolUse command hooks from
// <gemini-dir>/config/hooks.json before asking the client. Answering {"decision":"auto_approve"} lets an allowed call
// run without raising session/request_permission; printing nothing leaves agy's own check and Persona's card in place.

const agyAllowOutput = `{"decision":"auto_approve"}`

var agyAllowedTools = []string{
	"run_command",
	"read_file",
	"view_file",
	"find_by_name",
	"grep_search",
	"list_dir",
	"invoke_subagent",
}

var agyAssistantWriteTools = []string{
	"write_file",
	"write_to_file",
	"edit_file",
	"replace_file_content",
}

var agyHookMatcher = strings.Join(agyAllowedTools, "|")

var agyEventKeys = []string{"SessionStart", "PreToolUse", "PostToolUse", "PreInvocation", "PostInvocation", "Stop"}

func agyMatcherFor(assistantWrites bool) string {
	if !assistantWrites {
		return agyHookMatcher
	}
	return agyHookMatcher + "|" + strings.Join(agyAssistantWriteTools, "|")
}

func agyHooksPath(geminiDir string) string {
	p := filepath.Join(geminiDir, "config", "hooks.json")
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func agyHookCommand(bin, geminiDir, data string) string {
	return shellQuote(bin) + " hook --gemini-dir " + shellQuote(geminiDir) + " --data " + shellQuote(data) + " " + tagFlag
}

func installAgyHook(path, cmd string) (bool, error) {
	return installAgyHookWith(path, agyHookMatcher, cmd)
}

func installAgyHookWith(path, matcher, cmd string) (bool, error) {
	wantGroup, err := marshalPlain(map[string]any{
		"matcher": matcher,
		"hooks":   []any{map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout}},
	})
	if err != nil {
		return false, err
	}
	wantSpec, err := marshalPlain(map[string]any{"PreToolUse": []json.RawMessage{wantGroup}})
	if err != nil {
		return false, err
	}
	return editSettings(path, true, func(top *jsonObject) (bool, error) {
		changed := false
		installed := false
		var keep []jsonMember
		for _, m := range *top {
			var spec jsonObject
			if json.Unmarshal(m.Val, &spec) != nil {
				if m.Key == currentExtensionID || m.Key == legacyExtensionID {
					return false, fmt.Errorf("%w: hooks.%s is not an object", errSettingsFormat, m.Key)
				}
				keep = append(keep, m)
				continue
			}
			groups, hasPre, err := agyPreToolGroups(&spec)
			if err != nil {
				if m.Key == currentExtensionID || m.Key == legacyExtensionID {
					return false, err
				}
				keep = append(keep, m)
				continue
			}
			if m.Key == currentExtensionID && !installed {
				installed = true
				var rest []json.RawMessage
				if hasPre {
					rest, _, _ = splitOurs(groups)
				}
				spec.remove("enabled")
				if err := setAgyPreToolGroups(&spec, append(rest, wantGroup)); err != nil {
					return false, err
				}
				b, err := marshalPlain(spec)
				if err != nil {
					return false, err
				}
				if !sameJSON(m.Val, b) {
					changed = true
					keep = append(keep, jsonMember{Key: m.Key, Val: b})
				} else {
					keep = append(keep, m)
				}
				continue
			}
			if !hasPre {
				if (m.Key == currentExtensionID || m.Key == legacyExtensionID) && !agySpecHasEvents(&spec) {
					changed = true
					continue
				}
				keep = append(keep, m)
				continue
			}
			rest, ours, stripped := splitOurs(groups)
			if len(ours) == 0 && !stripped {
				keep = append(keep, m)
				continue
			}
			changed = true
			if err := setAgyPreToolGroups(&spec, rest); err != nil {
				return false, err
			}
			if (m.Key == currentExtensionID || m.Key == legacyExtensionID || len(spec) == 0) && !agySpecHasEvents(&spec) {
				continue
			}
			b, err := marshalPlain(spec)
			if err != nil {
				return false, err
			}
			keep = append(keep, jsonMember{Key: m.Key, Val: b})
		}
		if !installed {
			keep = append(keep, jsonMember{Key: currentExtensionID, Val: wantSpec})
			changed = true
		}
		if !changed {
			return false, nil
		}
		*top = keep
		return true, nil
	})
}

func uninstallAgyHook(path string) (bool, error) {
	return editSettings(path, false, func(top *jsonObject) (bool, error) {
		changed := false
		var keep []jsonMember
		for _, m := range *top {
			var spec jsonObject
			if json.Unmarshal(m.Val, &spec) != nil {
				keep = append(keep, m)
				continue
			}
			groups, hasPre, err := agyPreToolGroups(&spec)
			if err != nil || !hasPre {
				keep = append(keep, m)
				continue
			}
			rest, ours, stripped := splitOurs(groups)
			if len(ours) == 0 && !stripped {
				keep = append(keep, m)
				continue
			}
			changed = true
			if err := setAgyPreToolGroups(&spec, rest); err != nil {
				return false, err
			}
			if (m.Key == currentExtensionID || m.Key == legacyExtensionID || len(spec) == 0) && !agySpecHasEvents(&spec) {
				continue
			}
			b, err := marshalPlain(spec)
			if err != nil {
				return false, err
			}
			keep = append(keep, jsonMember{Key: m.Key, Val: b})
		}
		if !changed {
			return false, nil
		}
		*top = keep
		return true, nil
	})
}

func hasAgyHook(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var top jsonObject
	if json.Unmarshal(b, &top) != nil {
		return false
	}
	for _, m := range top {
		var spec jsonObject
		if json.Unmarshal(m.Val, &spec) != nil {
			continue
		}
		if raw, ok := spec.get("enabled"); ok {
			var enabled bool
			if json.Unmarshal(raw, &enabled) == nil && !enabled {
				continue
			}
		}
		groups, hasPre, err := agyPreToolGroups(&spec)
		if err != nil || !hasPre {
			continue
		}
		_, ours, stripped := splitOurs(groups)
		if len(ours) > 0 || stripped {
			return true
		}
	}
	return false
}

func agyPreToolGroups(spec *jsonObject) ([]json.RawMessage, bool, error) {
	raw, ok := spec.get("PreToolUse")
	if !ok {
		return nil, false, nil
	}
	var groups []json.RawMessage
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, true, fmt.Errorf("%w: PreToolUse is not a list", errSettingsFormat)
	}
	return groups, true, nil
}

func setAgyPreToolGroups(spec *jsonObject, groups []json.RawMessage) error {
	if len(groups) == 0 {
		spec.remove("PreToolUse")
		return nil
	}
	b, err := marshalPlain(groups)
	if err != nil {
		return err
	}
	spec.set("PreToolUse", b)
	return nil
}

func agySpecHasEvents(spec *jsonObject) bool {
	for _, k := range agyEventKeys {
		if _, ok := spec.get(k); ok {
			return true
		}
	}
	return false
}

type hookKind int

const (
	kindClaude hookKind = iota
	kindAgyPreTool
	kindAgyACP
)

func toAgySessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "agy-") {
		return id
	}
	return "agy-" + id
}

func isAgyNativeTool(name string) bool {
	switch name {
	case "run_command", "read_file", "view_file", "find_by_name", "grep_search", "list_dir",
		"invoke_subagent", "write_file", "write_to_file", "edit_file", "replace_file_content",
		"run_workflow", "define_subagent", "manage_subagents", "swarm", "search_web", "read_url_content", "ask_question":
		return true
	}
	return strings.HasPrefix(name, "mcp_") && !strings.HasPrefix(name, "mcp__")
}

func mapAgyToolName(name string) string {
	switch name {
	case "run_command":
		return "Bash"
	case "read_file", "view_file":
		return "Read"
	case "find_by_name":
		return "Glob"
	case "grep_search":
		return "Grep"
	case "list_dir":
		return "LS"
	case "invoke_subagent":
		return "Agent"
	case "write_file", "write_to_file":
		return "Write"
	case "edit_file", "replace_file_content":
		return "Edit"
	}
	return name
}

func mapAgyToolInput(rawName string, rawArgs json.RawMessage) toolInput {
	var in toolInput
	if len(bytes.TrimSpace(rawArgs)) == 0 {
		return in
	}
	_ = json.Unmarshal(rawArgs, &in)
	var m map[string]any
	if json.Unmarshal(rawArgs, &m) != nil {
		return in
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	switch rawName {
	case "run_command", "Bash":
		if in.Command == "" {
			in.Command = str("CommandLine", "command")
		}
		if in.Description == "" {
			in.Description = str("toolSummary", "description")
		}
	case "read_file", "view_file", "Read":
		if in.FilePath == "" {
			in.FilePath = str("Path", "AbsolutePath", "file_path")
		}
	case "write_file", "write_to_file", "Write":
		if in.FilePath == "" {
			in.FilePath = str("Path", "TargetFile", "file_path")
		}
	case "edit_file", "replace_file_content", "Edit":
		if in.FilePath == "" {
			in.FilePath = str("Path", "TargetFile", "file_path")
		}
	case "find_by_name", "Glob":
		if in.Pattern == "" {
			in.Pattern = str("Pattern", "pattern")
		}
		if in.Path == "" {
			in.Path = str("SearchDirectory", "Path", "path")
		}
	case "grep_search", "Grep":
		if in.Pattern == "" {
			in.Pattern = str("Query", "Pattern", "pattern")
		}
		if in.Path == "" {
			in.Path = str("SearchPath", "Path", "path")
		}
	case "list_dir", "LS":
		if in.Path == "" {
			in.Path = str("DirectoryPath", "Path", "path")
		}
	case "invoke_subagent", "Agent", "Task":
		if in.SubagentType == "" {
			in.SubagentType = str("TypeName", "subagent_type")
		}
		if in.Description == "" {
			in.Description = str("Role", "toolSummary", "description")
		}
	}
	return in
}

// extractAgyCommandCwd reads run_command's per-call Cwd/cwd parameter and resolves it relative to baseCwd.
// It returns invalid=true when Cwd is supplied with a non-string, conflicting, unresolvable, or
// dangling-symlink value so the caller fails closed instead of evaluating against the session workspace.
func extractAgyCommandCwd(rawName string, rawArgs json.RawMessage, baseCwd string) (cwd string, invalid bool) {
	if rawName != "run_command" && rawName != "Bash" {
		return "", false
	}
	if len(bytes.TrimSpace(rawArgs)) == 0 {
		return "", false
	}
	var m map[string]any
	if json.Unmarshal(rawArgs, &m) != nil {
		return "", false
	}
	var resolved string
	for _, k := range []string{"Cwd", "cwd"} {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", true
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if s == "~" || strings.HasPrefix(s, "~/") {
			home, err := os.UserHomeDir()
			if err != nil || home == "" || !filepath.IsAbs(home) {
				return "", true
			}
			// Preserve .. components so physical() resolves symlinks before collapsing ..
			s = home + strings.TrimPrefix(s, "~")
		} else if strings.HasPrefix(s, "~") {
			return "", true // ~user cannot be resolved safely
		}
		if !filepath.IsAbs(s) {
			if baseCwd == "" || !filepath.IsAbs(baseCwd) {
				return "", true
			}
			s = baseCwd + "/" + s
		}
		clean, ok := physical(s)
		if !ok || danglingLink(clean) {
			return "", true
		}
		if resolved != "" && resolved != clean {
			return "", true
		}
		resolved = clean
	}
	return resolved, false
}

func normalizeAgyMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "bypassPermissions", "always-proceed":
		return "bypassPermissions"
	case "plan":
		return "plan"
	case "auto":
		return "auto"
	case "accept-edits", "acceptEdits":
		return "acceptEdits"
	case "default":
		return "default"
	}
	return strings.TrimSpace(mode)
}

func resolveAgyDirs(a hookArgs) (geminiDir, agyDir string) {
	env := a.env()
	geminiDir = strings.TrimSpace(a.GeminiDir)
	agyDir = strings.TrimSpace(a.AgyDir)
	if agyDir == "" && geminiDir != "" {
		agyDir = filepath.Join(geminiDir, "antigravity-cli")
	}
	if agyDir == "" {
		agyDir = strings.TrimSpace(env("ANTIGRAVITY_CLI_CONFIG_DIR"))
	}
	if agyDir == "" {
		agyDir = strings.TrimSpace(env("ANTIGRAVITY_APP_DATA_DIR"))
	}
	if geminiDir == "" && agyDir != "" && filepath.Base(agyDir) == "antigravity-cli" {
		geminiDir = filepath.Dir(agyDir)
	}
	if geminiDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			geminiDir = filepath.Join(home, ".gemini")
		}
	}
	if agyDir == "" && geminiDir != "" {
		agyDir = filepath.Join(geminiDir, "antigravity-cli")
	}
	return geminiDir, agyDir
}

func readAgySettingsMode(geminiDir, agyDir string) string {
	var b []byte
	for _, dir := range []string{agyDir, geminiDir} {
		if dir == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
		if err == nil {
			b = data
			break
		}
	}
	if len(b) == 0 {
		return ""
	}
	var cfg struct {
		ToolPermission string `json:"toolPermission"`
		AgentMode      string `json:"agentMode"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	switch strings.TrimSpace(cfg.AgentMode) {
	case "plan":
		return "plan"
	case "auto":
		return "auto"
	case "accept-edits", "acceptEdits":
		return "acceptEdits"
	case "default":
		return "default"
	case "bypassPermissions":
		return "bypassPermissions"
	}
	if strings.TrimSpace(cfg.ToolPermission) == "always-proceed" {
		return "bypassPermissions"
	}
	return "default"
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

type acpPermissionParams struct {
	SessionID      string   `json:"sessionId"`
	Cwd            string   `json:"cwd"`
	WorkspacePaths []string `json:"workspacePaths"`
	TranscriptPath string   `json:"transcriptPath"`
	PermissionMode string   `json:"permissionMode"`
	Mode           string   `json:"mode"`
	AgentMode      string   `json:"agentMode"`
	ToolCall       struct {
		ToolCallID string          `json:"toolCallId"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		Title      string          `json:"title"`
		RawInput   json.RawMessage `json:"rawInput"`
		Input      json.RawMessage `json:"input"`
		Args       json.RawMessage `json:"args"`
	} `json:"toolCall"`
	Options []acpPermissionOption `json:"options"`
}

func pickAcpAllowOption(opts []acpPermissionOption) string {
	for _, want := range []string{"allow_once", "allow_always"} {
		for _, o := range opts {
			if o.Kind == want && strings.TrimSpace(o.OptionID) != "" {
				return o.OptionID
			}
		}
	}
	return ""
}

func parseHookRequest(a hookArgs, raw []byte) (hookInput, bool) {
	var probe struct {
		Method               string          `json:"method"`
		Params               json.RawMessage `json:"params"`
		HookEventNameCamel   string          `json:"hookEventName"`
		HookEventNameSnake   string          `json:"hook_event_name"`
		ConversationID       string          `json:"conversationId"`
		ParentConversationID string          `json:"parentConversationId"`
		SessionIDCamel       string          `json:"sessionId"`
		SessionIDSnake       string          `json:"session_id"`
		Agent                string          `json:"agent"`
		Runtime              string          `json:"runtime"`
		Kind                 string          `json:"kind"`
		Cwd                  string          `json:"cwd"`
		WorkspacePaths       []string        `json:"workspacePaths"`
		TranscriptPathCamel  string          `json:"transcriptPath"`
		PermissionModeCamel  string          `json:"permissionMode"`
		PermissionModeSnake  string          `json:"permission_mode"`
		Mode                 string          `json:"mode"`
		AgentMode            string          `json:"agentMode"`
		ToolInputRaw         json.RawMessage `json:"tool_input"`
		ToolCall             json.RawMessage `json:"toolCall"`
		Options              json.RawMessage `json:"options"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return hookInput{}, false
	}

	// 1. ACP session/request_permission (JSON-RPC envelope or direct params).
	if probe.Method == "session/request_permission" || (probe.HookEventNameCamel == "" && probe.HookEventNameSnake == "" && probe.SessionIDCamel != "" && len(probe.Options) > 0) {
		body := raw
		if probe.Method == "session/request_permission" {
			if len(probe.Params) == 0 {
				return hookInput{}, false
			}
			body = probe.Params
		}
		var p acpPermissionParams
		if json.Unmarshal(body, &p) != nil {
			return hookInput{}, false
		}
		optID := pickAcpAllowOption(p.Options)
		if optID == "" {
			return hookInput{}, false
		}
		rawName := strings.TrimSpace(p.ToolCall.Name)
		if rawName == "" && isAgyNativeTool(strings.TrimSpace(p.ToolCall.Title)) {
			rawName = strings.TrimSpace(p.ToolCall.Title)
		}
		args := p.ToolCall.RawInput
		if len(bytes.TrimSpace(args)) == 0 {
			args = p.ToolCall.Input
		}
		if len(bytes.TrimSpace(args)) == 0 {
			args = p.ToolCall.Args
		}
		geminiDir, agyDir := resolveAgyDirs(a)
		mode := firstNonEmpty(p.PermissionMode, p.Mode, p.AgentMode, probe.PermissionModeSnake)
		if mode != "" {
			mode = normalizeAgyMode(mode)
		} else {
			mode = readAgySettingsMode(geminiDir, agyDir)
		}
		cwd := p.Cwd
		if cwd == "" && len(p.WorkspacePaths) > 0 {
			cwd = p.WorkspacePaths[len(p.WorkspacePaths)-1]
		}
		cmdCwd, cmdCwdInvalid := extractAgyCommandCwd(rawName, args, cwd)
		return hookInput{
			HookEventName:  "PermissionRequest",
			SessionID:      toAgySessionID(p.SessionID),
			Cwd:            cwd,
			PermissionMode: mode,
			ToolName:       mapAgyToolName(rawName),
			TranscriptPath: p.TranscriptPath,
			ToolInput:      mapAgyToolInput(rawName, args),
			isAgy:          true,
			kind:           kindAgyACP,
			workspaces:     p.WorkspacePaths,
			cmdCwd:         cmdCwd,
			cmdCwdInvalid:  cmdCwdInvalid,
			geminiDir:      geminiDir,
			agyDir:         agyDir,
			acpOption:      optID,
		}, true
	}

	// 2. Antigravity PreToolUse hook payload.
	if probe.HookEventNameCamel != "" || (probe.HookEventNameSnake == "" && probe.ConversationID != "" && len(probe.ToolCall) > 0) {
		if probe.HookEventNameCamel != "" && probe.HookEventNameCamel != "PreToolUse" {
			return hookInput{}, false
		}
		var tc struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		if len(probe.ToolCall) == 0 || json.Unmarshal(probe.ToolCall, &tc) != nil {
			return hookInput{}, false
		}
		geminiDir, agyDir := resolveAgyDirs(a)
		mode := firstNonEmpty(probe.PermissionModeCamel, probe.PermissionModeSnake, probe.Mode, probe.AgentMode)
		if mode != "" {
			mode = normalizeAgyMode(mode)
		} else {
			mode = readAgySettingsMode(geminiDir, agyDir)
		}
		cwd := probe.Cwd
		if cwd == "" && len(probe.WorkspacePaths) > 0 {
			cwd = probe.WorkspacePaths[len(probe.WorkspacePaths)-1]
		}
		cmdCwd, cmdCwdInvalid := extractAgyCommandCwd(tc.Name, tc.Args, cwd)
		return hookInput{
			HookEventName:  "PermissionRequest",
			SessionID:      toAgySessionID(probe.ConversationID),
			Cwd:            cwd,
			PermissionMode: mode,
			ToolName:       mapAgyToolName(tc.Name),
			AgentID:        probe.ParentConversationID,
			TranscriptPath: probe.TranscriptPathCamel,
			ToolInput:      mapAgyToolInput(tc.Name, tc.Args),
			isAgy:          true,
			kind:           kindAgyPreTool,
			workspaces:     probe.WorkspacePaths,
			cmdCwd:         cmdCwd,
			cmdCwdInvalid:  cmdCwdInvalid,
			geminiDir:      geminiDir,
			agyDir:         agyDir,
		}, true
	}

	// 3. Claude Code PermissionRequest (or Persona companion-mapped agy request).
	var req hookInput
	if json.Unmarshal(raw, &req) != nil {
		return hookInput{}, false
	}
	isAgy := strings.EqualFold(probe.Agent, "agy") ||
		strings.EqualFold(probe.Runtime, "agy") ||
		strings.HasPrefix(req.SessionID, "agy-") ||
		isAgyNativeTool(req.ToolName)
	if !isAgy {
		return req, true
	}
	req.isAgy = true
	if req.TranscriptPath == "" {
		req.TranscriptPath = probe.TranscriptPathCamel
	}
	req.workspaces = probe.WorkspacePaths
	req.geminiDir, req.agyDir = resolveAgyDirs(a)
	rawTool := req.ToolName
	req.ToolName = mapAgyToolName(rawTool)
	if len(probe.ToolInputRaw) > 0 {
		req.ToolInput = mapAgyToolInput(rawTool, probe.ToolInputRaw)
		req.cmdCwd, req.cmdCwdInvalid = extractAgyCommandCwd(rawTool, probe.ToolInputRaw, req.Cwd)
	}
	if m := firstNonEmpty(req.PermissionMode, probe.PermissionModeCamel, probe.Mode, probe.AgentMode); m != "" {
		req.PermissionMode = normalizeAgyMode(m)
	}
	return req, true
}

func (req hookInput) allowReply() string {
	switch req.kind {
	case kindAgyPreTool:
		return agyAllowOutput
	case kindAgyACP:
		b, err := marshalPlain(map[string]any{
			"outcome": map[string]any{"outcome": "selected", "optionId": req.acpOption},
		})
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return allowOutput
	}
}

func (req hookInput) auditConfigDir(a hookArgs) string {
	if !req.isAgy {
		return a.ConfigDir
	}
	if a.GeminiDir != "" {
		return a.GeminiDir
	}
	if a.AgyDir != "" {
		return a.AgyDir
	}
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	return req.geminiDir
}

var agyAssistantDirName = regexp.MustCompile(`^assistant(-m-[0-9a-f]+)?$`)

var agyConversationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var agyRuntimePackageHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func realAgyPath(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	clean, ok := physical(p)
	if !ok || danglingLink(clean) {
		return "", false
	}
	return resolve(clean, true), true
}

func isAgyAssistantDir(dir, home string) bool {
	realDir, ok := realAgyPath(dir)
	if !ok {
		return false
	}
	realHome, ok := realAgyPath(home)
	if !ok {
		return false
	}
	parent := filepath.Dir(realDir)
	if filepath.Dir(parent) != realHome {
		return false
	}
	rootName := filepath.Base(parent)
	if rootName != ".persona" && rootName != ".jarvis" {
		return false
	}
	return agyAssistantDirName.MatchString(filepath.Base(realDir))
}

// isAgyPersonaSupervisionDir says whether dir is this conversation's Persona supervision root.
// Persona's ACP companion adds <agy dir>/persona-supervision/<conversation id> or
// <agy dir>/antigravity-cli/persona-supervision/<conversation id> as an additional workspace root;
// an arbitrary project or another session's directory remains an untrusted workspace.
func isAgyPersonaSupervisionDir(dir string, req hookInput) bool {
	id := strings.TrimPrefix(req.SessionID, "agy-")
	if !agyConversationID.MatchString(id) {
		return false
	}
	realDir, ok := realAgyPath(dir)
	if !ok {
		return false
	}
	realRoot, ok := realAgyPath(req.agyDir)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 2 {
		return parts[0] == "persona-supervision" && parts[1] == id
	}
	return len(parts) == 3 && parts[0] == "antigravity-cli" && parts[1] == "persona-supervision" && parts[2] == id
}

// isAgyPersonaRuntimePackageDir says whether dir is exactly one versioned Persona Antigravity runtime package.
// It is only an extra-workspace exemption: it neither proves an assistant session nor makes the package writable.
func isAgyPersonaRuntimePackageDir(dir, home string) bool {
	realDir, ok := realAgyPath(dir)
	if !ok {
		return false
	}
	realHome, ok := realAgyPath(home)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(realHome, realDir)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 4 &&
		(parts[0] == ".persona" || parts[0] == ".jarvis") &&
		parts[1] == "runtime-packages" &&
		parts[2] == "persona-antigravity-runtime" &&
		agyRuntimePackageHash.MatchString(parts[3])
}

func isAgyAssistantSession(req hookInput, getenv func(string) string) bool {
	if getenv("PERSONA_GUEST") != "" || getenv("JARVIS_GUEST") != "" {
		return false
	}
	hasEnv := getenv("PERSONA_ASSISTANT") == "1" || getenv("JARVIS_ASSISTANT") == "1"
	hasBrainTranscript := false
	if realTranscript, ok := realAgyPath(req.TranscriptPath); ok {
		if realAgyDir, ok := realAgyPath(req.agyDir); ok {
			brainRoot := filepath.Join(realAgyDir, "brain")
			hasBrainTranscript = realTranscript != brainRoot && within(realTranscript, brainRoot)
		}
	}
	if !hasEnv && !hasBrainTranscript {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	realHome, ok := realAgyPath(home)
	if !ok {
		return false
	}
	paths := req.workspaces
	if len(paths) == 0 && req.Cwd != "" {
		paths = []string{req.Cwd}
	}
	found := false
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return false
		}
		realP, ok := realAgyPath(p)
		if !ok {
			return false
		}
		if realP == realHome && len(paths) > 1 {
			continue // companion.mjs may start agy acp in $HOME while opening the session in ~/.persona/assistant
		}
		if !isAgyAssistantDir(p, home) {
			if isAgyPersonaSupervisionDir(p, req) || isAgyPersonaRuntimePackageDir(p, home) {
				continue
			}
			return false
		}
		found = true
	}
	return found
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
