package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	app := &hubApp{call: fakeNodes(nil)}
	exttest.Check(t, manifest, exttest.Serve(t, app.program(extensionID, version(t))), json.RawMessage(`{"machines":["m1"]}`))
}

func version(t *testing.T) string {
	var m struct{ Version string }
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	return m.Version
}

func TestManifestDefaultsMatchOptions(t *testing.T) {
	var m struct {
		Configuration struct {
			Properties map[string]struct {
				Default json.RawMessage `json:"default"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	var c config
	raw := map[string]json.RawMessage{}
	for k, p := range m.Configuration.Properties {
		if p.Default != nil {
			raw[k] = p.Default
		}
	}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(c.options())
	want, _ := json.Marshal(defaultOptions)
	if !bytes.Equal(got, want) {
		t.Errorf("the manifest's defaults %s differ from the hook's %s", got, want)
	}
}

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		text string
		non  bool
		lang string
	}{
		{"帮我修一下这个bug", true, "zh"},
		{"please 帮我看看 this function", true, "zh"},
		{"fix 配置.json loading in the parser", false, "zh"},
		{"rename the `用户` column to users", false, ""},
		{"look at this log:\n```\n错误：连接失败\n```\nwhy does it fail?", false, ""},
		{"このバグを直して", true, "ja"},
		{"이 버그를 고쳐줘", true, "ko"},
		{"Исправь эту ошибку", true, "ru"},
		{"Arregla este error, por favor", true, "es"},
		{"Add a \"Gracias\" button", false, ""},
		{"Fix the flaky test in the scheduler", false, ""},
		{"中", true, "zh"},
	} {
		if got := isNonEnglish(c.text); got != c.non {
			t.Errorf("isNonEnglish(%q) = %v, want %v (%+v)", c.text, got, c.non, analyze(c.text))
		}
		if got := detectLanguage(c.text); got != c.lang {
			t.Errorf("detectLanguage(%q) = %q, want %q", c.text, got, c.lang)
		}
	}
	if !hasStopword("这是 HOTFIX 快点", defaultOptions.Stopwords) || hasStopword("普通的请求", defaultOptions.Stopwords) ||
		hasStopword("看看 wasapi 的文档", defaultOptions.Stopwords) || !hasStopword("asap 修一下", defaultOptions.Stopwords) {
		t.Error("stopwords")
	}
	if !isGiveup("  GiveUp ", []string{"giveup"}) || isGiveup("giveup now", []string{"giveup"}) {
		t.Error("giveup words")
	}
}

func TestJudgeParsing(t *testing.T) {
	for _, c := range []struct {
		answer string
		want   verdict
	}{
		{`{"equivalent": true, "score": 92, "hint": "", "natural": true, "naturalHint": ""}`, verdict{Equivalent: true, Score: 92}},
		{"Sure!\n```json\n{\"equivalent\": \"false\", \"score\": \"40\", \"hint\": \"漏了 {范围}\"}\n```", verdict{Score: 40, Hint: "漏了 {范围}"}},
		{`{"equivalent": "yes", "score": 250}`, verdict{Score: 100}},
	} {
		v, err := judge(context.Background(), fixed(c.answer), "原文", "attempt")
		if err != nil {
			t.Fatal(err)
		}
		v.Natural = nil
		if v != c.want {
			t.Errorf("%q: %+v, want %+v", c.answer, v, c.want)
		}
	}
	if _, err := judge(context.Background(), fixed("no idea"), "a", "b"); err == nil {
		t.Error("an answer without JSON must fail")
	}
	if passed(verdict{Equivalent: false, Score: 69}, 70) || !passed(verdict{Score: 70}, 70) || !passed(verdict{Equivalent: true}, 70) {
		t.Error("passed")
	}
}

func fixed(answer string) llm {
	return func(context.Context, string) (string, error) { return answer, nil }
}

// fakeModel answers translations with "EN:<text>" and judgements with the given verdict; it records the prompts.
type fakeModel struct {
	verdict string
	fail    bool
	prompts []string
}

func (f *fakeModel) ask(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	if f.fail {
		return "", errors.New("model down")
	}
	if strings.HasPrefix(prompt, "Translate") {
		text := prompt[strings.Index(prompt, "<text>\n")+len("<text>\n") : strings.LastIndex(prompt, "\n</text>")]
		return "EN:" + text, nil
	}
	return f.verdict, nil
}

// computer is a node program's data directory with a fresh heartbeat and the given options.
func computer(t *testing.T, o options) string {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(o)
	if err := os.WriteFile(filepath.Join(dir, optionsFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, heartbeatFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type hookOut struct {
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
	SystemMessage string `json:"systemMessage"`
	Specific      struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func send(t *testing.T, data string, m *fakeModel, env map[string]string, session, prompt string) (hookOut, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": session, "prompt": prompt,
		"cwd": "/w/proj", "transcript_path": "/x.jsonl"})
	var out bytes.Buffer
	runHook(hookArgs{Data: data, ask: m.ask, getenv: func(k string) string { return env[k] }}, bytes.NewReader(b), &out, time.Now())
	var o hookOut
	if s := strings.TrimSpace(out.String()); s != "" {
		if err := json.Unmarshal([]byte(s), &o); err != nil {
			t.Fatalf("hook output %q: %v", s, err)
		}
	}
	return o, out.String()
}

func pairs(t *testing.T, data string) []entry {
	t.Helper()
	es, err := store{dir: data}.entries()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestBlockFlow(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{verdict: `{"equivalent": false, "score": 30, "hint": "说清楚是哪个测试"}`}
	const s = "s1"

	o, _ := send(t, data, m, nil, s, "帮我修一下那个不稳定的测试")
	if o.Decision != "block" || !strings.Contains(o.Reason, "giveup") || len(m.prompts) != 0 {
		t.Fatalf("a Chinese prompt is held back without a model call: %+v %v", o, m.prompts)
	}
	o, _ = send(t, data, m, nil, s, "还是中文写的")
	if o.Decision != "block" || !strings.Contains(o.Reason, "帮我修一下那个不稳定的测试") {
		t.Fatalf("a rewrite still in Chinese is held back with the original: %+v", o)
	}
	o, _ = send(t, data, m, nil, s, "fix the test")
	if o.Decision != "block" || !strings.Contains(o.Reason, "30") || !strings.Contains(o.Reason, "说清楚是哪个测试") {
		t.Fatalf("a rewrite that misses the meaning is held back with the hint: %+v", o)
	}
	m.verdict = `{"equivalent": true, "score": 88, "natural": false, "naturalHint": "Please fix the flaky test."}`
	o, _ = send(t, data, m, nil, s, "/fix the flaky test please")
	if o.Decision != "" || !strings.Contains(o.SystemMessage, "88") {
		t.Fatalf("a matching rewrite goes through, even one starting with /: %+v", o)
	}
	es := pairs(t, data)
	if len(es) != 1 || es[0].Mode != "rewrite" || es[0].English != "/fix the flaky test please" || es[0].Natural == nil || *es[0].Natural ||
		es[0].NaturalHint == "" || es[0].Score == nil || *es[0].Score != 88 || es[0].Language != "zh" {
		t.Fatalf("the pair: %+v", es)
	}
	if o, raw := send(t, data, m, nil, s, "next English prompt"); raw != "" || o.Decision != "" {
		t.Fatalf("the session is clear again: %q", raw)
	}
}

func TestGiveup(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{}
	send(t, data, m, nil, "s", "把日志级别调成 debug")
	o, _ := send(t, data, m, nil, "s", "giveup")
	if o.Decision != "" || !strings.Contains(o.SystemMessage, "EN:把日志级别调成 debug") ||
		!strings.Contains(o.Specific.Context, "把日志级别调成 debug") || o.Specific.Event != "UserPromptSubmit" {
		t.Fatalf("giveup: %+v", o)
	}
	if es := pairs(t, data); len(es) != 1 || es[0].Mode != "giveup" {
		t.Fatalf("%+v", es)
	}
	// with the model down, the original still reaches the model as context
	send(t, data, m, nil, "s", "把日志级别调成 info")
	m.fail = true
	o, _ = send(t, data, m, nil, "s", "放弃")
	if o.Decision != "" || !strings.Contains(o.Specific.Context, "把日志级别调成 info") {
		t.Fatalf("giveup without a model: %+v", o)
	}
}

func TestJudgeDownFailsOpen(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{}
	send(t, data, m, nil, "s", "重构这个函数")
	m.fail = true
	o, _ := send(t, data, m, nil, "s", "refactor this function")
	if o.Decision != "" || o.SystemMessage == "" {
		t.Fatalf("%+v", o)
	}
	if es := pairs(t, data); len(es) != 1 || es[0].Mode != "unverified" {
		t.Fatalf("%+v", es)
	}
}

func TestWarnAndLog(t *testing.T) {
	data := computer(t, options{Mode: "warn"})
	m := &fakeModel{}
	o, _ := send(t, data, m, nil, "s", "解释一下这段代码")
	if o.Decision != "" || !strings.Contains(o.SystemMessage, "EN:解释一下这段代码") || !strings.Contains(o.Specific.Context, "EN:解释一下这段代码") {
		t.Fatalf("warn: %+v", o)
	}
	m.fail = true
	if o, _ = send(t, data, m, nil, "s", "再解释一下"); o.Decision != "" || o.Specific.Context != "" {
		t.Fatalf("warn without a model lets it through: %+v", o)
	}

	data = computer(t, options{Mode: "log"})
	m = &fakeModel{}
	if _, raw := send(t, data, m, nil, "s", "解释一下这段代码"); raw != "" || len(m.prompts) != 0 {
		t.Fatalf("log mode says nothing and calls no model: %q %v", raw, m.prompts)
	}
	if es := pairs(t, data); len(es) != 1 || es[0].Mode != "log" || es[0].English != "" {
		t.Fatalf("%+v", es)
	}
}

func TestLeftAlone(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{}
	for name, c := range map[string]struct {
		env    map[string]string
		prompt string
	}{
		"english":       {nil, "Add a retry to the uploader"},
		"slash command": {nil, "/review 这个 PR"},
		"internal call": {map[string]string{internalEnv: "1"}, "翻译这句话"},
		"assistant":     {map[string]string{"PERSONA_ASSISTANT": "1"}, "帮我看看日程"},
		"old assistant": {map[string]string{"JARVIS_ASSISTANT": "1"}, "帮我看看日程"},
	} {
		if _, raw := send(t, data, m, c.env, "s-"+name, c.prompt); raw != "" {
			t.Errorf("%s: %q", name, raw)
		}
	}
	o, _ := send(t, data, m, nil, "s", "线上事故，马上回滚")
	if o.Decision != "" || o.SystemMessage == "" {
		t.Fatalf("an urgent word: %+v", o)
	}
	// a stale heartbeat: the node program is gone
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(data, heartbeatFile), old, old); err != nil {
		t.Fatal(err)
	}
	if _, raw := send(t, data, m, nil, "s2", "帮我修一下"); raw != "" {
		t.Fatalf("with a stale heartbeat the hook does nothing: %q", raw)
	}
}

func TestPendingExpires(t *testing.T) {
	st := store{dir: t.TempDir()}
	now := time.Now()
	if err := st.setPending("s", "原文", "zh", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p, err := st.getPending("s", now); err != nil || p != nil {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestQueueCompacts(t *testing.T) {
	st := store{dir: t.TempDir()}
	now := time.Now()
	for i := 0; i <= queueCompactAt; i++ {
		if err := st.add(entry{Original: "第" + strings.Repeat("一", i%5+1), Mode: "log"}, now); err != nil {
			t.Fatal(err)
		}
	}
	es, err := st.entries()
	if err != nil || len(es) != queueCap {
		t.Fatalf("%d %v", len(es), err)
	}
	// a damaged line costs only itself
	f, _ := os.OpenFile(filepath.Join(st.dir, queueFile), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("{not json\n")
	f.Close()
	if err := st.add(entry{Original: "最后", Mode: "log"}, now); err != nil {
		t.Fatal(err)
	}
	if es, _ := st.entries(); len(es) != queueCap+1 || es[len(es)-1].Original != "最后" {
		t.Fatalf("%d", len(es))
	}
}

func TestCommandLLM(t *testing.T) {
	ask := commandLLM([]string{"sh", "-c", `printf '%s|%s' "$EBD_INTERNAL" "$(cat)"`})
	out, err := ask(context.Background(), "提示\nline two")
	if err != nil || out != "1|提示\nline two" {
		t.Fatalf("the prompt goes on stdin, with the internal marker: %q %v", out, err)
	}
	if _, err := commandLLM([]string{"sh", "-c", "echo boom >&2; exit 3"})(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := commandLLM([]string{"sleep", "5"})(ctx, "x"); err == nil {
		t.Fatal("a command past its time must fail")
	}
}

func TestSettingsEdit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	orig := `{
    "model": "opus",
    "hooks": {
        "UserPromptSubmit": [
            {"hooks": [{"type": "command", "command": "other-tool notify && echo done"}]}
        ],
        "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
    }
}
`
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := hookCommand("/opt/ebd bin", "/data/it's")
	if changed, err := installHook(p, cmd); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := installHook(p, cmd); err != nil || changed {
		t.Fatalf("installing twice writes once: %v %v", changed, err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !hasHook(p) || !strings.Contains(s, "other-tool notify && echo done") || !strings.Contains(s, "say done") ||
		!strings.Contains(s, `"model": "opus"`) || !strings.Contains(s, "\n    \"model\"") {
		t.Fatalf("other hooks, keys and the indent stay:\n%s", s)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p + settingsBackupSuffix); string(b) != orig {
		t.Error("a copy of the file before the change")
	}
	if changed, err := uninstallHook(p); err != nil || !changed || hasHook(p) {
		t.Fatal(changed, err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "other-tool notify") || strings.Contains(string(b), tagFlag) {
		t.Fatalf("%s", b)
	}
	if err := os.WriteFile(p, []byte("[1,2]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHook(p, cmd); err == nil {
		t.Fatal("a file that is not an object is left alone")
	}
}

func TestNodeInstallsAndCleansUp(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, "claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &node{bin: "/opt/ebd", data: filepath.Join(home, "data")}
	if err := os.MkdirAll(n.data, 0o700); err != nil {
		t.Fatal(err)
	}
	n.beat()
	n.install([]string{claude, filepath.Join(home, "missing"), "relative/dir"})
	st, err := n.status("0.1.0", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dirs) != 3 || !st.Dirs[0].Installed || st.Dirs[1].Error == "" || st.Dirs[2].Error == "" {
		t.Fatalf("%+v", st.Dirs)
	}
	n.cleanup()
	if hasHook(filepath.Join(claude, "settings.json")) {
		t.Fatal("cleanup takes the entry out")
	}
	if _, err := os.Stat(filepath.Join(n.data, heartbeatFile)); !os.IsNotExist(err) {
		t.Fatal("cleanup removes the heartbeat")
	}
}

func fakeNodes(by map[string]any) nodeCaller {
	return func(_ context.Context, machine string, out any) error {
		v, ok := by[machine]
		if !ok {
			return errors.New("machine " + machine + " is offline")
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
}

func TestPage(t *testing.T) {
	score, unnatural := 88, false
	app := &hubApp{machines: []string{"m1", "m2"}, call: fakeNodes(map[string]any{
		"m1": status{Version: "0.1.0", Mode: "block", Dirs: []dirState{{Dir: "~/.claude", Settings: "/home/me/.claude/settings.json", Installed: true}},
			Total: 3, Due: 1, ByMode: map[string]int{"rewrite": 1, "log": 2},
			Recent: []entry{
				{At: "2026-11-05T10:00:00Z", Original: "帮我修一下那个测试", English: "fix that test for me", Mode: "rewrite", Score: &score, Natural: &unnatural, NaturalHint: "Please fix that test."},
				{At: "2026-11-05T09:00:00Z", Original: "看看日志", Mode: "log"},
			}},
	})}
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale, r.TZ = logUI, wire.SizeFull, 3, locale, "Asia/Shanghai"
		v, err := app.render(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		if len(v.Sections) != 3 || v.Sections[0].Title != "m1" || v.Sections[1].Items[0].Badge == nil {
			t.Fatalf("%s: %s", locale, raw)
		}
		if !strings.Contains(string(raw), "18:00") || !strings.Contains(string(raw), "fix that test for me") ||
			!strings.Contains(string(raw), "Please fix that test.") {
			t.Errorf("%s: the pair, in the user's time zone: %s", locale, raw)
		}
	}
	empty := &hubApp{call: fakeNodes(nil)}
	r := &ext.UIRequest{}
	r.Contribution, r.Size, r.UIVersion, r.Locale = logUI, wire.SizeFull, 3, "zh-Hans"
	v, err := empty.render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if err := exttest.CheckView(raw, false); err != nil || v.Sections[0].Items[0].Type != wire.ComponentEmpty {
		t.Fatalf("%v %s", err, raw)
	}
}

// In a session Persona's node started, a first prompt is not held back (the node would type it again), later ones
// are, and the model is asked to start its reply with the English, which Persona's apps would not show otherwise.
func TestPersonaSession(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{}
	env := map[string]string{"PERSONA_HOME": "/home/me/.persona"}
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	persona := func(session, prompt string) hookOut {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": session, "prompt": prompt,
			"transcript_path": transcript})
		var out bytes.Buffer
		runHook(hookArgs{Data: data, ask: m.ask, getenv: func(k string) string { return env[k] }}, bytes.NewReader(b), &out, time.Now())
		var o hookOut
		if out.Len() > 0 {
			if err := json.Unmarshal(out.Bytes(), &o); err != nil {
				t.Fatal(err)
			}
		}
		return o
	}
	// the first prompt: the transcript has none of the user's yet
	if err := os.WriteFile(transcript, []byte(`{"type":"queue-operation"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := persona("s", "把日志级别调成 debug")
	if o.Decision != "" || !strings.Contains(o.Specific.Context, "Begin your reply with one line, exactly: English: EN:把日志级别调成 debug") {
		t.Fatalf("a first prompt in a Persona session: %+v", o)
	}
	// a later one is held back as anywhere else
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o = persona("s", "继续"); o.Decision != "block" {
		t.Fatalf("a later prompt in a Persona session: %+v", o)
	}
	// outside Persona even a first prompt is held back
	if o, _ = send(t, data, m, nil, "s3", "继续"); o.Decision != "block" {
		t.Fatalf("outside Persona: %+v", o)
	}

	no := false
	data = computer(t, options{Mode: "warn", EchoInReply: &no})
	if o = persona("s4", "解释一下"); strings.Contains(o.Specific.Context, "Begin your reply") ||
		!strings.Contains(o.Specific.Context, "EN:解释一下") {
		t.Fatalf("echo switched off: %+v", o)
	}
	// outside Persona the terminal shows the system message: no echo
	data = computer(t, options{Mode: "warn"})
	if o, _ = send(t, data, m, map[string]string{}, "s", "解释一下"); strings.Contains(o.Specific.Context, "Begin your reply") {
		t.Fatalf("echo outside Persona: %+v", o)
	}
}
