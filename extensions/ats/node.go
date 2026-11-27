package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The program on a computer (the manifest's node section). It does what an attempt needs on the
// computer where it runs, and nothing else:
//
//	ats.probe_launcher  check whether a configured launcher gateway accepts TCP connections
//	ats.prepare  check the repository out into a worktree of its own, at the commit the ref names now
//	ats.accept   run the task's acceptance command there (started here, reported with the event ats.accepted)
//	ats.release  remove the worktree again, unless it holds work that is not committed
//
// The agent itself is a Persona session the hub program starts in the worktree; this program never runs one.
// Repositories outside the allowlist in the settings are refused here too, whatever the hub program asked for.

const (
	outputTailBytes   = 4 << 10
	defaultAcceptTime = 10 * time.Minute
	maxAcceptTime     = 60 * time.Minute
	invalidRefMessage = "ref must be a branch, tag or commit"
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
var errInvalidRef = errors.New("invalid_ref: " + invalidRefMessage)

type nodeWorker struct {
	data                string
	launcherSettingsDir string
	notify              func(event string, params any)

	mu      sync.Mutex
	repos   []string
	running map[string]context.CancelFunc
}

type probeLauncherParams struct {
	Launcher string `json:"launcher"`
}

type probeLauncherResult struct {
	Reachable bool   `json:"reachable"`
	Endpoint  string `json:"endpoint,omitempty"`
	Error     string `json:"error,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

type prepareParams struct {
	TaskID   string `json:"task_id"`
	Attempt  int    `json:"attempt"`
	Repo     string `json:"repo"`
	Ref      string `json:"ref,omitempty"`
	Readonly bool   `json:"readonly"`
}

type prepareResult struct {
	Path    string `json:"path"`
	BaseSHA string `json:"base_sha"`
	Branch  string `json:"branch,omitempty"`
}

type acceptParams struct {
	TaskID         string `json:"task_id"`
	Attempt        int    `json:"attempt"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// acceptResult is what the acceptance command left: the evidence of the attempt.
type acceptResult struct {
	TaskID     string `json:"task_id"`
	Attempt    int    `json:"attempt"`
	Done       bool   `json:"done"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Error      string `json:"error,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Dirty      *bool  `json:"dirty,omitempty"`
}

type releaseParams struct {
	TaskID   string `json:"task_id"`
	Attempt  int    `json:"attempt"`
	Repo     string `json:"repo"`
	BaseSHA  string `json:"base_sha"`
	Readonly bool   `json:"readonly,omitempty"`
	// CommitMessage, when set, has uncommitted work committed to the attempt's branch with this message, so the
	// work is on the branch and the worktree can go.
	CommitMessage string `json:"commit_message,omitempty"`
}

type releaseResult struct {
	Removed bool   `json:"removed"`
	Kept    string `json:"kept,omitempty"`
	Branch  string `json:"branch,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Dirty   *bool  `json:"dirty,omitempty"`
}

func newNodeWorker(data string, repos []string, notify func(string, any)) *nodeWorker {
	return &nodeWorker{data: data, repos: repos, notify: notify, running: map[string]context.CancelFunc{}}
}

func (n *nodeWorker) findLauncherSettingsDir() string {
	if n.launcherSettingsDir != "" {
		return n.launcherSettingsDir
	}
	dir := filepath.Clean(n.data)
	for range 4 {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		settings := filepath.Join(dir, "launcher-settings")
		if info, err := os.Stat(settings); err == nil && info.IsDir() {
			return settings
		}
	}
	return ""
}

func (n *nodeWorker) probeLauncher(ctx context.Context, p probeLauncherParams) (probeLauncherResult, error) {
	skipped := probeLauncherResult{Reachable: true, Skipped: true}
	if strings.TrimSpace(p.Launcher) == "" {
		return skipped, nil
	}
	if !safeIDPattern.MatchString(p.Launcher) || strings.Contains(p.Launcher, "..") {
		return probeLauncherResult{}, errors.New("invalid launcher name")
	}
	dir := n.findLauncherSettingsDir()
	if dir == "" {
		return skipped, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, p.Launcher+"-env-settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		raw, err = os.ReadFile(filepath.Join(dir, p.Launcher+"-settings.json"))
	}
	if errors.Is(err, os.ErrNotExist) {
		return skipped, nil
	}
	if err != nil {
		return probeLauncherResult{}, fmt.Errorf("read launcher settings: %w", err)
	}
	var settings struct {
		Env struct {
			AnthropicBaseURL string `json:"ANTHROPIC_BASE_URL"`
			OpenAIBaseURL    string `json:"OPENAI_BASE_URL"`
		} `json:"env"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return probeLauncherResult{}, fmt.Errorf("decode launcher settings: %w", err)
	}
	baseURL := cmp.Or(strings.TrimSpace(settings.Env.AnthropicBaseURL), strings.TrimSpace(settings.Env.OpenAIBaseURL), strings.TrimSpace(settings.BaseURL))
	u, err := url.Parse(baseURL)
	if err != nil {
		return probeLauncherResult{}, errors.New("invalid launcher base URL")
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return skipped, nil
	}
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return skipped, nil
		}
	}
	addr := net.JoinHostPort(host, port)
	const timeout = 2 * time.Second
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(pctx, "tcp", addr)
	if err != nil {
		return probeLauncherResult{Endpoint: addr, Error: err.Error()}, nil
	}
	_ = conn.Close()
	return probeLauncherResult{Reachable: true, Endpoint: addr}, nil
}

func attemptKey(taskID string, attempt int) (string, error) {
	if !safeIDPattern.MatchString(taskID) || attempt <= 0 {
		return "", errors.New("task_id or attempt is not valid")
	}
	return fmt.Sprintf("%s-%d", taskID, attempt), nil
}

func (n *nodeWorker) worktree(key string) string { return filepath.Join(n.data, "worktrees", key) }
func (n *nodeWorker) resultFile(key string) string {
	return filepath.Join(n.data, "results", key+".json")
}

// expandHome turns ~ and ~/x into paths under this computer's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// realPath is the path with ~ expanded and symlinks followed, so an allowlisted directory cannot be left through
// a link.
func realPath(p string) (string, error) {
	p = expandHome(strings.TrimSpace(p))
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s is not an absolute path", p)
	}
	return filepath.EvalSymlinks(filepath.Clean(p))
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// allowedRepo resolves repo and checks it against the allowlist: the repository is an allowlisted directory or
// lies inside one. An empty allowlist allows nothing.
func (n *nodeWorker) allowedRepo(repo string) (string, error) {
	real, err := realPath(repo)
	if err != nil {
		return "", fmt.Errorf("repo: %w", err)
	}
	n.mu.Lock()
	roots := append([]string(nil), n.repos...)
	n.mu.Unlock()
	for _, r := range roots {
		root, err := realPath(r)
		if err == nil && within(root, real) {
			return real, nil
		}
	}
	return "", errors.New("repo_not_allowed: the repository is not under a directory in the extension's \"repos\" setting")
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	bin, err := findGit()
	if err != nil {
		return "", fmt.Errorf("find git: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cleanEnv(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			return s, fmt.Errorf("git %s: %w", args[0], err)
		}
		return s, fmt.Errorf("git %s: %w: %s", args[0], err, firstLinePreview(s, 300))
	}
	return s, nil
}

// findGit also checks the appended directories because exec resolves a command before applying Cmd.Env.
func findGit() (string, error) {
	bin, err := exec.LookPath("git")
	if !errors.Is(err, exec.ErrNotFound) {
		return bin, err
	}
	for _, dir := range additionalToolDirs(os.Getenv("PATH")) {
		if path, err := exec.LookPath(filepath.Join(dir, "git")); err == nil {
			return path, nil
		}
	}
	return "", err
}

func additionalToolDirs(path string) []string {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin"}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	existing := filepath.SplitList(path)
	var added []string
	for _, dir := range dirs {
		if slices.ContainsFunc(existing, func(entry string) bool { return filepath.Clean(entry) == dir }) {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			added = append(added, dir)
			existing = append(existing, dir)
		}
	}
	return added
}

// cleanEnv removes Persona-injected variables and completes the tool search path.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PERSONA_") || strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	path := os.Getenv("PATH")
	dirs := append(filepath.SplitList(path), additionalToolDirs(path)...)
	return append(env, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
}

func worktreeDirty(ctx context.Context, dir string) (bool, error) {
	status, err := git(ctx, dir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	return status != "", err
}

func validateRef(ref string) error {
	if ref == "" || len(ref) > 200 || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "+") ||
		strings.HasPrefix(ref, "^") || strings.ContainsRune(ref, ':') {
		return errInvalidRef
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errInvalidRef
		}
	}
	return nil
}

// resolveCommit finds ref in repo, fetching configured remotes if the commit is missing locally so
// multi-machine tasks sharing a Git remote can run seamlessly on another node.
func resolveCommit(ctx context.Context, repo, ref string) (string, error) {
	if err := validateRef(ref); err != nil {
		return "", err
	}
	if sha, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && sha != "" {
		return sha, nil
	}
	remotes, err := git(ctx, repo, "remote")
	if err != nil || strings.TrimSpace(remotes) == "" {
		return "", fmt.Errorf("ref_not_found: %s names no commit in the repository", ref)
	}
	_, _ = git(ctx, repo, "fetch", "--all", "--prune", "--quiet")
	if sha, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && sha != "" {
		return sha, nil
	}
	if ref != "HEAD" {
		// 禁用远端的引用映射，使定向 fetch 只记录 FETCH_HEAD。
		if _, ferr := git(ctx, repo, "fetch", "--no-tags", "--quiet", "--refmap=", "origin", "--", ref); ferr == nil {
			if sha, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && sha != "" {
				return sha, nil
			}
			if sha, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}"); err == nil && sha != "" {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("ref_not_found: %s names no commit in the repository", ref)
}

func (n *nodeWorker) prepare(ctx context.Context, p prepareParams) (prepareResult, error) {
	key, err := attemptKey(p.TaskID, p.Attempt)
	if err != nil {
		return prepareResult{}, err
	}
	repo, err := n.allowedRepo(p.Repo)
	if err != nil {
		return prepareResult{}, err
	}
	ref := cmpOr(p.Ref, "HEAD")
	if err := validateRef(ref); err != nil {
		return prepareResult{}, err
	}
	sha, err := resolveCommit(ctx, repo, ref)
	if err != nil {
		return prepareResult{}, err
	}
	path := n.worktree(key)
	res := prepareResult{Path: path, BaseSHA: sha}
	if !p.Readonly {
		res.Branch = "ats/" + key
	}
	if _, err := os.Stat(path); err == nil { // prepared before the hub program restarted
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return prepareResult{}, err
	}
	args := []string{"worktree", "add", "--detach", path, sha}
	if res.Branch != "" {
		args = []string{"worktree", "add", "-b", res.Branch, path, sha}
	}
	if _, err := git(ctx, repo, args...); err != nil {
		return prepareResult{}, err
	}
	return res, nil
}

// accept starts the acceptance command and returns; the result is written beside the worktrees and sent to the
// hub program as the event ats.accepted (acceptStatus answers it too, for a hub program that missed the event).
func (n *nodeWorker) accept(p acceptParams) error {
	key, err := attemptKey(p.TaskID, p.Attempt)
	if err != nil {
		return err
	}
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("command must not be empty")
	}
	dir := n.worktree(key)
	if _, err := os.Stat(dir); err != nil {
		return errors.New("worktree_missing: the attempt has no worktree on this computer")
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultAcceptTime
	}
	if timeout > maxAcceptTime {
		timeout = maxAcceptTime
	}
	n.mu.Lock()
	if _, busy := n.running[key]; busy {
		n.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	n.running[key] = cancel
	n.mu.Unlock()
	_ = os.Remove(n.resultFile(key))

	go func() {
		defer cancel()
		res := runAcceptance(ctx, dir, p.Command)
		res.TaskID, res.Attempt, res.Done = p.TaskID, p.Attempt, true
		gctx, gcancel := context.WithTimeout(context.Background(), 10*time.Second)
		if sha, err := git(gctx, dir, "rev-parse", "HEAD"); err == nil {
			res.HeadSHA = sha
		}
		if dirty, err := worktreeDirty(gctx, dir); err == nil {
			res.Dirty = boolPtr(dirty)
		} else {
			res.Error = cmpOr(res.Error, "check worktree: "+err.Error())
		}
		gcancel()
		if raw, err := json.Marshal(res); err == nil {
			_ = os.MkdirAll(filepath.Dir(n.resultFile(key)), 0o755)
			_ = os.WriteFile(n.resultFile(key), raw, 0o600)
		}
		n.mu.Lock()
		delete(n.running, key)
		n.mu.Unlock()
		if n.notify != nil {
			n.notify("ats.accepted", res)
		}
	}()
	return nil
}

// runAcceptance runs command with sh -c in dir, in a process group of its own so a timeout kills everything it
// started, and keeps the tail of what it printed.
func runAcceptance(ctx context.Context, dir, command string) acceptResult {
	start := time.Now()
	tail := &tailBuffer{max: outputTailBytes}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = tail, tail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	res := acceptResult{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		res.Error = err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		// Stop the group before killing it: otherwise killing sleep lets its parent run the next command before it is killed.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		if !res.TimedOut {
			res.Error = "canceled"
		}
	}
	res.DurationMS = time.Since(start).Milliseconds()
	res.OutputTail = tail.String()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil && res.Error == "" && !res.TimedOut && res.ExitCode <= 0 {
		res.Error = err.Error()
	}
	return res
}

func (n *nodeWorker) acceptStatus(taskID string, attempt int) (acceptResult, error) {
	key, err := attemptKey(taskID, attempt)
	if err != nil {
		return acceptResult{}, err
	}
	n.mu.Lock()
	_, busy := n.running[key]
	n.mu.Unlock()
	res := acceptResult{TaskID: taskID, Attempt: attempt}
	if busy {
		return res, nil
	}
	raw, err := os.ReadFile(n.resultFile(key))
	if err != nil {
		// Neither running nor finished: this program restarted under the command.
		res.Done, res.ExitCode, res.Error = true, -1, "lost: the acceptance command did not survive a restart of the computer's program"
		return res, nil
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return acceptResult{}, err
	}
	return res, nil
}

// release 停止验收命令并尝试移除工作树。无法自动提交的修改、只读任务中产生的提交都会保留工作树。
// 有新提交的分支会保留。
func (n *nodeWorker) release(ctx context.Context, p releaseParams) (releaseResult, error) {
	key, err := attemptKey(p.TaskID, p.Attempt)
	if err != nil {
		return releaseResult{}, err
	}
	n.mu.Lock()
	if cancel, busy := n.running[key]; busy {
		cancel()
	}
	n.mu.Unlock()
	_ = os.Remove(n.resultFile(key))
	path := n.worktree(key)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return releaseResult{Removed: true}, nil
	} else if err != nil {
		return releaseResult{}, fmt.Errorf("check worktree: %w", err)
	}
	res := releaseResult{}
	head, err := git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.HeadSHA = head
	if b, err := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		res.Branch = b
	}
	dirty, err := worktreeDirty(ctx, path)
	if err != nil {
		return res, err
	}
	res.Dirty = boolPtr(dirty)
	if p.Readonly && p.BaseSHA != "" && head != p.BaseSHA {
		res.Dirty, res.Kept = boolPtr(true), path
		return res, nil
	}
	if res.Dirty != nil && *res.Dirty {
		if res.Branch == "" || strings.TrimSpace(p.CommitMessage) == "" || commitAll(ctx, path, p.CommitMessage) != nil {
			res.Kept = path
			return res, nil
		}
		if sha, err := git(ctx, path, "rev-parse", "HEAD"); err == nil {
			res.HeadSHA = sha
		}
		if dirty, err := worktreeDirty(ctx, path); err != nil || dirty {
			res.Dirty, res.Kept = boolPtr(true), path
			return res, nil
		}
		res.Dirty = boolPtr(false)
	}
	repo, err := n.allowedRepo(p.Repo)
	if err != nil {
		return res, err
	}
	remove := []string{"worktree", "remove"}
	if !p.Readonly {
		remove = append(remove, "--force")
	}
	if _, err := git(ctx, repo, append(remove, path)...); err != nil {
		return res, err
	}
	res.Removed = true
	if res.Branch != "" {
		// A branch that never moved carries nothing: drop it.
		if p.BaseSHA != "" && p.BaseSHA == res.HeadSHA && strings.HasPrefix(res.Branch, "ats/") {
			if _, err := git(ctx, repo, "branch", "-D", res.Branch); err == nil {
				res.Branch = ""
			}
		}
	}
	return res, nil
}

// commitAll commits everything in the worktree that is not ignored, as the repository's configured identity or,
// when it has none, as ATS (git's guess from the account and host name is not used). Hooks and signing are
// skipped: nobody is there to answer them, and the commit only keeps the attempt's work.
func commitAll(ctx context.Context, dir, message string) error {
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	args := []string{"-c", "commit.gpgsign=false"}
	name, _ := git(ctx, dir, "config", "user.name")
	email, _ := git(ctx, dir, "config", "user.email")
	if name == "" || email == "" {
		args = append(args, "-c", "user.name=ATS", "-c", "user.email=ats@localhost")
	}
	args = append(args, "commit", "--quiet", "--no-verify", "-m", message)
	_, err := git(ctx, dir, args...)
	return err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(string(b.buf), "")
}

// nodeProgram is the program of the node role.
func nodeProgram(id, version string) ext.Program {
	var w *nodeWorker
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init: func(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
			var c ExtConfig
			if err := in.Config(&c); err != nil {
				return nil, err
			}
			data := strings.TrimSpace(in.Info().DataDir)
			if data == "" {
				dir, err := os.UserCacheDir()
				if err != nil {
					return nil, err
				}
				data = filepath.Join(dir, "persona-ats")
			}
			w = newNodeWorker(data, c.Repos, func(event string, params any) { _ = hub.Notify(event, params) })
			return nil, nil
		},
		Namespaces: map[string]ext.Handler{
			"ats": func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				if w == nil {
					return nil, errors.New("not initialized")
				}
				return w.rpc(ctx, method, params)
			},
		},
	}
}

// rpc answers the hub program's calls (ats.<method> over nodes.call).
func (n *nodeWorker) rpc(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "probe_launcher":
		var p probeLauncherParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return n.probeLauncher(ctx, p)
	case "prepare":
		p := prepareParams{Ref: "HEAD"}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Ref == "" {
			return nil, errInvalidRef
		}
		return n.prepare(ctx, p)
	case "accept":
		var p acceptParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := n.accept(p); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case "accept_status":
		var p acceptParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return n.acceptStatus(p.TaskID, p.Attempt)
	case "release":
		var p releaseParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return n.release(ctx, p)
	}
	return nil, ext.UnknownMethod("ats." + method)
}
