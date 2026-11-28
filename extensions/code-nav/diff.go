package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Read-only Git diff inspection on the target computer: lists unstaged, staged, or default-branch changes and parses
// unified diffs into hunks with exact new-side line numbers so the UI can render diffs and jump straight into full
// source code at any modified hunk or line. Never runs any mutating git command, filters out secret paths and symlinks
// escaping the repository, and enforces strict size and line truncation limits.

const (
	diffModeUnstaged = "unstaged"
	diffModeStaged   = "staged"
	diffModeBranch   = "branch"

	maxDiffFiles          = 200
	maxDiffBytes          = 512 << 10 // max raw git diff bytes read for one file
	maxDiffLines          = 2000      // max unified diff lines kept per file
	maxDiffHunks          = 200
	maxDiffLineBytes      = 4000
	maxChangedLinesPerHnk = 12
)

type diffFileEntry struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added, modified, deleted, renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
	FirstLine int    `json:"first_line,omitempty"`
}

type diffStatusResult struct {
	IsGit     bool            `json:"is_git"`
	Branch    string          `json:"branch,omitempty"`
	BaseRef   string          `json:"base_ref,omitempty"`
	Mode      string          `json:"mode"`
	Files     []diffFileEntry `json:"files"`
	TotalAdd  int             `json:"total_add"`
	TotalDel  int             `json:"total_del"`
	Truncated bool            `json:"truncated,omitempty"`
	NoBase    bool            `json:"no_base,omitempty"`
}

type diffChangedLine struct {
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Kind    string `json:"kind"` // "+" or "-"
	Preview string `json:"preview"`
}

type diffHunkSummary struct {
	Index    int               `json:"index"`
	Header   string            `json:"header"`
	OldStart int               `json:"old_start"`
	NewStart int               `json:"new_start"`
	JumpLine int               `json:"jump_line"`
	JumpCol  int               `json:"jump_col,omitempty"`
	Added    int               `json:"added"`
	Deleted  int               `json:"deleted"`
	Preview  string            `json:"preview,omitempty"`
	Lines    []string          `json:"lines"`
	Changed  []diffChangedLine `json:"changed,omitempty"`
}

type diffFileResult struct {
	Path      string            `json:"path"`
	OldPath   string            `json:"old_path,omitempty"`
	AbsPath   string            `json:"abs_path,omitempty"`
	Status    string            `json:"status"`
	Language  string            `json:"language,omitempty"`
	Additions int               `json:"additions"`
	Deletions int               `json:"deletions"`
	Binary    bool              `json:"binary,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Hunks     []diffHunkSummary `json:"hunks,omitempty"`
}

func normalizeDiffMode(mode string) string {
	switch mode {
	case diffModeStaged, diffModeBranch:
		return mode
	default:
		return diffModeUnstaged
	}
}

func (n *node) gitEnv() ([]string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	env := make([]string, len(n.env), len(n.env)+2)
	copy(env, n.env)
	env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return env, n.home
}

func runGitLimited(ctx context.Context, dir string, env []string, maxBytes int, args ...string) ([]byte, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fullArgs := append([]string{"-c", "core.quotePath=off"}, args...)
	cmd := exec.CommandContext(cctx, "git", fullArgs...)
	cmd.Dir = dir
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	buf, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := len(buf) > maxBytes
	if truncated {
		buf = buf[:maxBytes]
		// Drain or kill the process cleanly so Wait finishes promptly.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if truncated {
		return buf, true, nil
	}
	if readErr != nil {
		return nil, false, readErr
	}
	if waitErr != nil {
		return buf, false, waitErr
	}
	return buf, false, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func gitCurrentBranch(ctx context.Context, dir string, env []string) string {
	if out, _, err := runGitLimited(ctx, dir, env, 4096, "symbolic-ref", "--short", "HEAD"); err == nil {
		if b := strings.TrimSpace(string(out)); b != "" {
			return b
		}
	}
	if out, _, err := runGitLimited(ctx, dir, env, 4096, "rev-parse", "--short", "HEAD"); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// gitDefaultBase finds the repository's default branch ref and merge-base with HEAD.
func gitDefaultBase(ctx context.Context, dir string, env []string) (baseRef, mergeBase string) {
	var candidates []string
	if out, _, err := runGitLimited(ctx, dir, env, 4096, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			candidates = append(candidates, ref)
		}
	}
	candidates = append(candidates, "origin/main", "origin/master", "main", "master")
	for _, ref := range candidates {
		if _, _, err := runGitLimited(ctx, dir, env, 1024, "rev-parse", "--verify", "--quiet", ref); err != nil {
			continue
		}
		if mb, _, err := runGitLimited(ctx, dir, env, 1024, "merge-base", ref, "HEAD"); err == nil {
			if sha := strings.TrimSpace(string(mb)); sha != "" {
				return ref, sha
			}
		}
	}
	return "", ""
}

// diffPathSafe checks that rel stays inside r.Path and is never a secret path or escaping symlink.
func (n *node) diffPathSafe(r repo, rel, home string) (abs string, exists bool, ok bool) {
	if rel == "" || !relOK(rel) {
		return "", false, false
	}
	joined := filepath.Clean(filepath.Join(r.Path, filepath.FromSlash(rel)))
	if !within(joined, r.Path) || secretPath(joined, home) {
		return "", false, false
	}
	if _, err := os.Lstat(joined); err != nil {
		// Deleted file (no longer on disk): lexical sandbox + secret check passed.
		return joined, false, true
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil || !within(real, r.Path) || secretPath(real, home) {
		return "", false, false
	}
	return real, true, true
}

// diffStatus lists changed files in r for mode ("unstaged", "staged", or "branch").
func (n *node) diffStatus(ctx context.Context, r repo, mode string) (diffStatusResult, error) {
	mode = normalizeDiffMode(mode)
	res := diffStatusResult{Mode: mode, Files: []diffFileEntry{}}
	if !isGitRepo(r.Path) {
		return res, nil
	}
	env, home := n.gitEnv()
	res.IsGit = true
	res.Branch = gitCurrentBranch(ctx, r.Path, env)

	var diffBaseArgs []string
	switch mode {
	case diffModeStaged:
		diffBaseArgs = []string{"diff", "--cached", "--no-ext-diff"}
	case diffModeBranch:
		baseRef, mb := gitDefaultBase(ctx, r.Path, env)
		if mb == "" {
			res.NoBase = true
			return res, nil
		}
		res.BaseRef = baseRef
		diffBaseArgs = []string{"diff", "--no-ext-diff", mb}
	default:
		diffBaseArgs = []string{"diff", "--no-ext-diff"}
	}

	// Collect name-status and numstat.
	statusMap := map[string]diffFileEntry{}
	nsArgs := append(slices.Clone(diffBaseArgs), "--name-status", "-z", "-M")
	if out, _, err := runGitLimited(ctx, r.Path, env, 2<<20, nsArgs...); err == nil {
		for _, e := range parseNameStatusZ(out) {
			if _, _, ok := n.diffPathSafe(r, e.Path, home); !ok {
				continue
			}
			if e.OldPath != "" {
				if _, _, ok := n.diffPathSafe(r, e.OldPath, home); !ok {
					e.OldPath = ""
				}
			}
			statusMap[e.Path] = e
		}
	}

	numArgs := append(slices.Clone(diffBaseArgs), "--numstat", "-z", "-M")
	if out, _, err := runGitLimited(ctx, r.Path, env, 2<<20, numArgs...); err == nil {
		for _, ns := range parseNumStatZ(out) {
			e, exists := statusMap[ns.path]
			if !exists {
				if _, _, ok := n.diffPathSafe(r, ns.path, home); !ok {
					continue
				}
				e = diffFileEntry{Path: ns.path, Status: wire.FileStatusModified}
			}
			e.Additions = ns.add
			e.Deletions = ns.del
			e.Binary = ns.binary
			statusMap[ns.path] = e
		}
	}

	// In unstaged mode, also include untracked non-ignored files from git status.
	if mode == diffModeUnstaged {
		if out, _, err := runGitLimited(ctx, r.Path, env, 2<<20, "status", "--porcelain=v1", "-z", "-uall"); err == nil {
			for _, rel := range parseUntrackedZ(out) {
				if _, already := statusMap[rel]; already {
					continue
				}
				real, exists, ok := n.diffPathSafe(r, rel, home)
				if !ok || !exists {
					continue
				}
				entry := diffFileEntry{
					Path:      rel,
					Status:    wire.FileStatusAdded,
					Untracked: true,
					FirstLine: 1,
				}
				if text, _, bin, err := readText(real); err == nil {
					if bin {
						entry.Binary = true
					} else if text != "" {
						lines := strings.Split(text, "\n")
						if len(lines) > 1 && lines[len(lines)-1] == "" {
							lines = lines[:len(lines)-1]
						}
						entry.Additions = len(lines)
					}
				}
				statusMap[rel] = entry
			}
		}
	}

	entries := make([]diffFileEntry, 0, len(statusMap))
	for _, e := range statusMap {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	if len(entries) > maxDiffFiles {
		res.Truncated = true
		entries = entries[:maxDiffFiles]
	}
	for _, e := range entries {
		res.TotalAdd += e.Additions
		res.TotalDel += e.Deletions
	}
	res.Files = entries
	return res, nil
}

// diffFile returns the parsed hunks and clickable modified lines for one file in r.
func (n *node) diffFile(ctx context.Context, r repo, mode, rel string) (diffFileResult, error) {
	mode = normalizeDiffMode(mode)
	if !isGitRepo(r.Path) {
		return diffFileResult{}, &ext.Error{Code: "invalid", Message: "not a git repository"}
	}
	env, home := n.gitEnv()
	abs, exists, ok := n.diffPathSafe(r, rel, home)
	if !ok {
		return diffFileResult{}, &ext.Error{Code: "forbidden", Message: "path is outside repository or holds secrets"}
	}

	res := diffFileResult{
		Path:   rel,
		Status: wire.FileStatusModified,
	}
	if exists {
		res.AbsPath = abs
	}
	if l, known := languageOf(rel); known {
		res.Language = l.id
	}

	// Check whether this file is untracked in unstaged mode.
	if mode == diffModeUnstaged && exists {
		if stOut, _, err := runGitLimited(ctx, r.Path, env, 64<<10, "status", "--porcelain=v1", "-z", "--", rel); err == nil {
			if slices.Contains(parseUntrackedZ(stOut), rel) {
				return n.untrackedFileDiff(res, abs)
			}
		}
	}

	var args []string
	switch mode {
	case diffModeStaged:
		args = []string{"diff", "--cached", "--no-ext-diff", "--no-color", "-U3", "-M"}
	case diffModeBranch:
		_, mb := gitDefaultBase(ctx, r.Path, env)
		if mb == "" {
			return res, nil
		}
		args = []string{"diff", "--no-ext-diff", "--no-color", "-U3", "-M", mb}
	default:
		args = []string{"diff", "--no-ext-diff", "--no-color", "-U3", "-M"}
	}

	// Query name-status first to preserve rename old path & status accurately.
	nsArgs := append(slices.Clone(args), "--name-status", "-z", "--", rel)
	if nsOut, _, err := runGitLimited(ctx, r.Path, env, 64<<10, nsArgs...); err == nil {
		for _, e := range parseNameStatusZ(nsOut) {
			if e.Path == rel {
				res.Status = e.Status
				if e.OldPath != "" {
					if _, _, safe := n.diffPathSafe(r, e.OldPath, home); safe {
						res.OldPath = e.OldPath
					}
				}
				break
			}
		}
	}

	patchArgs := append(args, "--", rel)
	// When a file was renamed, include OldPath so git diff can produce rename hunks cleanly.
	if res.OldPath != "" && res.OldPath != rel {
		patchArgs = append(patchArgs, res.OldPath)
	}
	patch, truncated, err := runGitLimited(ctx, r.Path, env, maxDiffBytes, patchArgs...)
	if err != nil {
		return diffFileResult{}, err
	}
	if !exists && res.Status == wire.FileStatusModified {
		res.Status = wire.FileStatusDeleted
	}
	parsed := parseUnifiedDiff(string(patch))
	res.Binary = parsed.binary
	res.Truncated = truncated || parsed.truncated
	res.Hunks = parsed.hunks
	res.Additions = parsed.additions
	res.Deletions = parsed.deletions
	if parsed.status != "" && res.Status == wire.FileStatusModified {
		res.Status = parsed.status
	}
	return res, nil
}

func (n *node) untrackedFileDiff(res diffFileResult, abs string) (diffFileResult, error) {
	res.Status = wire.FileStatusAdded
	text, cut, bin, err := readText(abs)
	if err != nil {
		return diffFileResult{}, err
	}
	if bin {
		res.Binary = true
		return res, nil
	}
	rawLines := strings.Split(text, "\n")
	if len(rawLines) > 1 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	truncated := cut
	if len(rawLines) > maxDiffLines {
		rawLines = rawLines[:maxDiffLines]
		truncated = true
	}
	h := diffHunkSummary{
		Index:    0,
		Header:   "@@ -0,0 +1," + strconv.Itoa(len(rawLines)) + " @@",
		OldStart: 0,
		NewStart: 1,
		JumpLine: 1,
		Added:    len(rawLines),
		Lines:    make([]string, 0, len(rawLines)),
	}
	for i, line := range rawLines {
		dl := "+" + cutRunes(line, maxDiffLineBytes)
		h.Lines = append(h.Lines, dl)
		trimmed := strings.TrimSpace(line)
		if h.Preview == "" && trimmed != "" {
			h.Preview = clip(trimmed, 140)
			h.JumpLine = i + 1
			h.JumpCol = firstNonSpaceCol(line)
		}
		if trimmed != "" && len(h.Changed) < maxChangedLinesPerHnk {
			h.Changed = append(h.Changed, diffChangedLine{
				Line:    i + 1,
				Col:     firstNonSpaceCol(line),
				Kind:    "+",
				Preview: clip(trimmed, 140),
			})
		}
	}
	res.Additions = len(rawLines)
	res.Truncated = truncated
	if len(h.Lines) > 0 {
		res.Hunks = []diffHunkSummary{h}
	}
	return res, nil
}

type parsedDiff struct {
	hunks     []diffHunkSummary
	additions int
	deletions int
	binary    bool
	truncated bool
	status    string
}

func parseUnifiedDiff(patch string) parsedDiff {
	var out parsedDiff
	if strings.TrimSpace(patch) == "" {
		return out
	}
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	totalLines := 0
	var cur *diffHunkSummary
	newLine := 0

	flushCur := func() {
		if cur == nil {
			return
		}
		if cur.JumpLine <= 0 {
			cur.JumpLine = max(1, cur.NewStart)
		}
		out.hunks = append(out.hunks, *cur)
		cur = nil
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if cur == nil {
			switch {
			case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
				out.binary = true
				return out
			case strings.HasPrefix(line, "new file mode "):
				out.status = wire.FileStatusAdded
				continue
			case strings.HasPrefix(line, "deleted file mode "):
				out.status = wire.FileStatusDeleted
				continue
			case strings.HasPrefix(line, "rename from "):
				out.status = wire.FileStatusRenamed
				continue
			}
		}
		if strings.HasPrefix(line, "diff --git ") {
			flushCur()
			continue
		}
		if strings.HasPrefix(line, "@@") {
			flushCur()
			if len(out.hunks) >= maxDiffHunks {
				out.truncated = true
				break
			}
			oldStart, newStart := parseHunkHeader(line)
			cur = &diffHunkSummary{
				Index:    len(out.hunks),
				Header:   clip(line, 2000),
				OldStart: oldStart,
				NewStart: newStart,
			}
			newLine = max(1, newStart)
			continue
		}
		if cur == nil {
			continue
		}
		if totalLines >= maxDiffLines {
			out.truncated = true
			break
		}
		if line == "" {
			line = " "
		}
		prefix := line[0]
		if prefix != ' ' && prefix != '+' && prefix != '-' && prefix != '\\' {
			// Skip stray header/footer lines outside hunks.
			continue
		}
		safeLine := cutRunes(line, maxDiffLineBytes)
		cur.Lines = append(cur.Lines, safeLine)
		totalLines++

		body := ""
		if len(line) > 1 {
			body = line[1:]
		}
		trimmed := strings.TrimSpace(body)
		switch prefix {
		case '+':
			cur.Added++
			out.additions++
			if cur.JumpLine == 0 && trimmed != "" {
				cur.JumpLine = newLine
				cur.JumpCol = firstNonSpaceCol(body)
				cur.Preview = clip(trimmed, 140)
			} else if cur.JumpLine == 0 {
				cur.JumpLine = newLine
				cur.JumpCol = 1
			}
			if trimmed != "" && len(cur.Changed) < maxChangedLinesPerHnk {
				cur.Changed = append(cur.Changed, diffChangedLine{
					Line:    newLine,
					Col:     firstNonSpaceCol(body),
					Kind:    "+",
					Preview: clip(trimmed, 140),
				})
			}
			newLine++
		case '-':
			cur.Deleted++
			out.deletions++
			if cur.Preview == "" && trimmed != "" {
				cur.Preview = "- " + clip(trimmed, 136)
			}
			if trimmed != "" && len(cur.Changed) < maxChangedLinesPerHnk && cur.Added == 0 {
				// Record anchor line on the new side where lines were deleted.
				anchor := max(1, newLine)
				if len(cur.Changed) == 0 || cur.Changed[len(cur.Changed)-1].Line != anchor {
					cur.Changed = append(cur.Changed, diffChangedLine{
						Line:    anchor,
						Col:     1,
						Kind:    "-",
						Preview: "- " + clip(trimmed, 136),
					})
				}
			}
		case ' ':
			newLine++
		}
	}
	flushCur()
	return out
}

func parseHunkHeader(line string) (oldStart, newStart int) {
	rest := strings.TrimPrefix(line, "@@")
	if i := strings.Index(rest, "@@"); i >= 0 {
		rest = rest[:i]
	}
	for _, f := range strings.Fields(rest) {
		if len(f) < 2 {
			continue
		}
		n, _, _ := strings.Cut(f[1:], ",")
		v, _ := strconv.Atoi(n)
		switch f[0] {
		case '-':
			oldStart = v
		case '+':
			newStart = v
		}
	}
	return oldStart, newStart
}

func firstNonSpaceCol(line string) int {
	col := 1
	for _, r := range line {
		if !unicode.IsSpace(r) {
			return col
		}
		col++
	}
	return 1
}

// parseNameStatusZ parses `git diff --name-status -z -M` output.
func parseNameStatusZ(raw []byte) []diffFileEntry {
	parts := bytes.Split(raw, []byte{0})
	var out []diffFileEntry
	for i := 0; i < len(parts); {
		code := string(parts[i])
		i++
		if code == "" {
			continue
		}
		switch code[0] {
		case 'R', 'C':
			if i+1 >= len(parts) {
				return out
			}
			oldP := string(parts[i])
			newP := string(parts[i+1])
			i += 2
			st := wire.FileStatusRenamed
			if code[0] == 'C' {
				st = wire.FileStatusAdded
			}
			out = append(out, diffFileEntry{Path: newP, OldPath: oldP, Status: st})
		default:
			if i >= len(parts) {
				return out
			}
			p := string(parts[i])
			i++
			st := wire.FileStatusModified
			switch code[0] {
			case 'A':
				st = wire.FileStatusAdded
			case 'D':
				st = wire.FileStatusDeleted
			}
			out = append(out, diffFileEntry{Path: p, Status: st})
		}
	}
	return out
}

type numStatEntry struct {
	path   string
	add    int
	del    int
	binary bool
}

// parseNumStatZ parses `git diff --numstat -z -M` output.
func parseNumStatZ(raw []byte) []numStatEntry {
	parts := bytes.Split(raw, []byte{0})
	var out []numStatEntry
	for i := 0; i < len(parts); {
		rec := string(parts[i])
		i++
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		addS, delS, pathPart := fields[0], fields[1], fields[2]
		var target string
		if pathPart == "" {
			// Rename record: "\t\t\0old\0new\0"
			if i+1 >= len(parts) {
				return out
			}
			target = string(parts[i+1])
			i += 2
		} else {
			target = pathPart
		}
		if target == "" {
			continue
		}
		e := numStatEntry{path: target}
		if addS == "-" || delS == "-" {
			e.binary = true
		} else {
			e.add, _ = strconv.Atoi(addS)
			e.del, _ = strconv.Atoi(delS)
		}
		out = append(out, e)
	}
	return out
}

// parseUntrackedZ extracts untracked file paths from `git status --porcelain=v1 -z -uall`.
func parseUntrackedZ(raw []byte) []string {
	parts := bytes.Split(raw, []byte{0})
	var out []string
	for i := 0; i < len(parts); i++ {
		rec := string(parts[i])
		if len(rec) < 4 {
			continue
		}
		xy := rec[:2]
		path := rec[3:]
		if xy == "??" && path != "" {
			out = append(out, path)
		}
		// Renamed/copied entries in porcelain v1 -z carry a second NUL-terminated path.
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			i++
		}
	}
	return out
}
