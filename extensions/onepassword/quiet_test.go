package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// lapsingCLI is op on a computer whose authorization by the 1Password app can lapse (10 idle minutes, 12 hours, the
// app locking): while it has, op whoami answers "not signed in" at once, and any other command would show the user a
// prompt, which nobody answers (counted in prompts).
type lapsingCLI struct {
	*fakeCLI
	mu         sync.Mutex
	authorized bool
	prompts    []string
	whoamis    int
}

func (l *lapsingCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	l.mu.Lock()
	authorized := l.authorized
	key := strings.Join(args, " ")
	if args[0] == "whoami" {
		l.whoamis++
	}
	if !authorized && key != "--version" && args[0] != "whoami" {
		l.prompts = append(l.prompts, key)
	}
	l.mu.Unlock()
	switch {
	case authorized || key == "--version":
		return l.fakeCLI.run(ctx, args...)
	case args[0] == "whoami":
		return nil, opError("[ERROR] 2026/11/27 19:42:20 account is not signed in")
	}
	return nil, opError("[ERROR] 2026/11/27 19:42:20 authorization prompt dismissed, please try again")
}

func (l *lapsingCLI) set(authorized bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.authorized = authorized
}

func (l *lapsingCLI) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.prompts...)
}

// Authorized once, never prompted again (nwverse/persona#17636): after the user authorized op once, an everyday ask
// with an earlier answer to fall back on never runs an op that could prompt. The vault tools answer what was kept
// at once (no prompt, no wait), describe never prompts, and a resolve Persona marks quiet (it keeps the value) answers
// not_signed_in at once. Only an ask with nothing to fall back on (a first resolve) may prompt.
func TestAuthorizedOnceNeverPromptsAgain(t *testing.T) {
	cli := &lapsingCLI{fakeCLI: newFakeCLI(), authorized: true}
	a, _ := onComputerRun(t, cli.run)
	clock := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	const ref = "op://Dev/GitHub token/credential"

	// The first uses, the user authorizes op once.
	if text := mustTool(t, a, "onepassword_vaults"); !strings.Contains(text, "Dev") {
		t.Fatalf("vaults: %s", text)
	}
	out, err := secretsCall(t, a, "describe", map[string]string{"reference": ref})
	if err != nil || out.(map[string]string)["title"] != "GitHub token › credential" {
		t.Fatalf("describe: %v %v", out, err)
	}
	if out, err := secretsCall(t, a, "resolve", map[string]any{"reference": ref, "purpose": map[string]string{"kind": "launcher", "id": "agy-gateway"}}); err != nil || out.(map[string]string)["value"] != secretValues[1] {
		t.Fatalf("resolve: %v", err)
	}

	// The authorization lapsed, long after anything was kept (metaTTL is long over): nothing prompts.
	cli.set(false)
	cli.mu.Lock()
	whoamiBefore := cli.whoamis
	cli.mu.Unlock()
	clock = clock.Add(2 * time.Hour)
	for range 3 {
		start := time.Now()
		text := mustTool(t, a, "onepassword_vaults")
		if !strings.Contains(text, "Dev") {
			t.Fatalf("the vaults kept do not answer: %s", text)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the vault tool waited %v", d)
		}
	}
	out, err = secretsCall(t, a, "describe", map[string]string{"reference": ref})
	if err != nil || out.(map[string]string)["title"] != "GitHub token › credential" {
		t.Fatalf("describe from what is kept: %v %v", out, err)
	}
	// Words for an item never looked up: not_signed_in (Persona shows the reference), no prompt.
	if _, err := secretsCall(t, a, "describe", map[string]string{"reference": "op://Shared/Other item/password"}); codeOf(err) != "not_signed_in" {
		t.Fatalf("describe of something not kept: %v", err)
	}
	// Persona keeps the value and asks quietly: not_signed_in at once, op read never runs.
	quiet := map[string]any{"reference": ref, "purpose": map[string]string{"kind": "launcher", "id": "agy-gateway"}, "quiet": true}
	if _, err := secretsCall(t, a, "resolve", quiet); codeOf(err) != "not_signed_in" {
		t.Fatalf("a quiet resolve: %v", err)
	}
	if p := cli.asked(); len(p) != 0 {
		t.Fatalf("op prompted the user for an everyday ask: %v", p)
	}
	cli.mu.Lock()
	whoami := cli.whoamis - whoamiBefore
	cli.mu.Unlock()
	if whoami < 5 { // three vault tools, two describes, one resolve: each checked first
		t.Fatalf("quiet asks checked the authorization %d times", whoami)
	}

	// A first resolve (nothing kept anywhere) may have the user authorize op: that is the one prompt.
	if _, err := secretsCall(t, a, "resolve", map[string]any{"reference": ref, "purpose": map[string]string{"kind": "launcher", "id": "new"}}); codeOf(err) != "not_signed_in" {
		t.Fatalf("a first resolve, nobody approving: %v", err)
	}
	if p := cli.asked(); len(p) != 1 || !strings.HasPrefix(p[0], "read ") {
		t.Fatalf("a first resolve may prompt once: %v", p)
	}

	// Authorized again: a quiet ask reads as usual.
	cli.set(true)
	if out, err := secretsCall(t, a, "resolve", quiet); err != nil || out.(map[string]string)["value"] != secretValues[1] {
		t.Fatalf("a quiet resolve while authorized: %v", err)
	}
}

// The program on the computer, asked quietly: op whoami first, and nothing else while it says not signed in.
func TestQuietNodeAsksWhoamiFirst(t *testing.T) {
	cli := &lapsingCLI{fakeCLI: newFakeCLI()}
	n := &nodeApp{run: cli.run}
	for _, m := range []string{"vaults", "read"} {
		raw, _ := json.Marshal(map[string]any{"quiet": true, "reference": "op://Dev/GitHub token/credential"})
		if _, err := n.handle(context.Background(), m, raw); opCode(err) != codeLocked {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if p := cli.asked(); len(p) != 0 {
		t.Fatalf("prompted: %v", p)
	}
	cli.set(true)
	raw, _ := json.Marshal(map[string]any{"quiet": true})
	if _, err := n.handle(context.Background(), "vaults", raw); err != nil {
		t.Fatalf("authorized: %v", err)
	}
	if ran := cli.ran(); len(ran) != 2 || ran[0] != "whoami --format json" || ran[1] != "vault list --format json" {
		t.Fatalf("ran: %v", ran)
	}
}
