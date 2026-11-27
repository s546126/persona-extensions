package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Runner executes a shell script on a machine (either local /bin/sh or remote via ssh).
// Tests can swap Runner on app/mcpServer to deterministically verify scan and clean without touching real disks.
type Runner func(ctx context.Context, m *Machine, script string, timeout time.Duration) (string, error)

// DefaultRunner executes scripts locally via /bin/sh when m.IsLocal (or a self-host alias),
// and via OpenSSH with BatchMode=yes for remote machines.
func DefaultRunner(ctx context.Context, m *Machine, script string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := shellCommand(cctx, m, "sh -s", "-s")
	if err != nil {
		return "", err
	}
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// When the time is up only the shell is killed: a du or find it started still holds stdout, and Run would wait
	// for it to finish (minutes on a large cache). WaitDelay closes the pipes and returns what was read by then.
	cmd.WaitDelay = runnerWaitDelay
	if err := cmd.Run(); err != nil {
		return stdout.String(), runError(m, err, stderr.String())
	}
	return stdout.String(), nil
}

// runnerWaitDelay is how long a run waits, after its time is up and the shell was killed, for the programs the shell
// started to let go of its output; tests shorten it.
var runnerWaitDelay = 5 * time.Second

// probeTimeout bounds one scan's probe script; tests shorten it.
var probeTimeout = 90 * time.Second

// runError words a failed run like the scan does: an SSH failure names the host.
func runError(m *Machine, err error, stderr string) error {
	reason := lastNonEmptyLine(stderr)
	if reason == "" {
		reason = err.Error()
	}
	if m != nil && !m.IsLocal {
		return fmt.Errorf("SSH 连接 %s 失败：%s", m.Host, reason)
	}
	return fmt.Errorf("本地探测命令执行失败：%s", reason)
}

// shellCommand runs /bin/sh with localArgs on this machine, or remote (the command line the
// remote login shell runs) over OpenSSH with BatchMode=yes.
func shellCommand(ctx context.Context, m *Machine, remote string, localArgs ...string) (*exec.Cmd, error) {
	if m == nil || m.IsLocal || isLocalConnection(m.Host, m.Port) {
		return exec.CommandContext(ctx, "/bin/sh", localArgs...), nil
	}
	if !IsConcreteHost(m.Host) {
		return nil, fmt.Errorf("非法 SSH 主机名：%q", m.Host)
	}
	args := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=12",
		"-o", "ServerAliveInterval=15",
	}
	if m.Port > 0 && m.Port != 22 {
		args = append(args, "-p", strconv.Itoa(m.Port))
	}
	if m.IdentityFile != "" {
		args = append(args, "-i", m.IdentityFile)
	}
	target := m.Host
	if m.User != "" && !strings.Contains(target, "@") {
		target = m.User + "@" + target
	}
	args = append(args, "--", target, remote)
	return exec.CommandContext(ctx, resolveSSHBinary(), args...), nil
}

func resolveSSHBinary() string {
	if env := strings.TrimSpace(os.Getenv("DISKLESSLY_SSH")); env != "" {
		return env
	}
	for _, p := range []string{"/usr/bin/ssh", "/bin/ssh", "/usr/local/bin/ssh", "/opt/homebrew/bin/ssh"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ssh"); err == nil {
		return p
	}
	return "ssh"
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ProbeScript is the single-trip, read-only POSIX shell probe that inspects:
// - OS & architecture (uname -sm), hostname, $HOME, installed disklessly CLI version
// - Root / data mount capacity (df -B1 / or df -kP /)
// - Reclaimable caches across all 6 categories:
//  1. go-cache (Go build cache, Go module cache)
//  2. node-python-cache (npm, pnpm, yarn, pip, uv)
//  3. docker-unused (Docker buildx cache, dangling images; never running containers)
//  4. rust-xcode-build (Cargo registry/git, Xcode DerivedData, SwiftPM, Clang, Gradle)
//  5. tmp-logs (systemd journal, rotated /var/log, build/install leftovers in /tmp of any age and anything there older than 7d)
//  6. cold-agent-sessions (Claude Code & Codex transcripts tiered into hot <=7d vs cold >7d)
const ProbeScript = `export LC_ALL=C
export LANG=C
echo "@@SECTION:UNAME"
uname -sm 2>/dev/null || true
echo "@@SECTION:HOSTNAME"
hostname 2>/dev/null || uname -n 2>/dev/null || echo "machine"
echo "@@SECTION:HOME"
printf '%s\n' "$HOME"
echo "@@SECTION:BIN_VERSION"
` + resolveCLIScript + `
if [ -n "$DKL" ]; then
  "$DKL" version 2>/dev/null | head -n 1 || true
fi
echo "@@SECTION:DF"
if [ -d "/System/Volumes/Data" ]; then
  df -kP /System/Volumes/Data 2>/dev/null || df -kP / 2>/dev/null || true
else
  df -B1 / 2>/dev/null || df -kP / 2>/dev/null || true
fi
echo "@@SECTION:CACHES"
probe_dir() {
  id="$1"; cat="$2"; title="$3"; target="$4"; safety="$5"; reason="$6"
  if [ -e "$target" ]; then
    kb=$(du -xsk "$target" 2>/dev/null | awk '{print $1}' | head -n 1)
    if [ -n "$kb" ] && [ "$kb" -gt 0 ] 2>/dev/null; then
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$cat" "$title" "$target" "$kb" "$safety" "$reason"
    fi
  fi
}
GOCACHE_DIR=$(go env GOCACHE 2>/dev/null || printf '%s' "$HOME/.cache/go-build")
GOMOD_DIR=$(go env GOMODCACHE 2>/dev/null || printf '%s' "$HOME/go/pkg/mod")
[ -z "$GOCACHE_DIR" ] && GOCACHE_DIR="$HOME/.cache/go-build"
[ -z "$GOMOD_DIR" ] && GOMOD_DIR="$HOME/go/pkg/mod"
probe_dir "cache-gobuild" "go-cache" "Go 编译缓存" "$GOCACHE_DIR" "safe" "Go 编译时留下的缓存，删了下次编译会自动再生成"
probe_dir "cache-gomod" "go-cache" "Go 下载的依赖包" "$GOMOD_DIR" "safe" "Go 项目下载过的依赖包，删了下次用到时会重新下载"

probe_dir "cache-npm" "node-python-cache" "npm 下载的包" "$HOME/.npm" "safe" "npm 下载过的包，删了下次安装时会重新下载"
probe_dir "cache-pnpm" "node-python-cache" "pnpm 存的包" "$HOME/.local/share/pnpm/store" "safe" "pnpm 给所有项目共用的一份包，删了下次安装时会重新下载"
probe_dir "cache-pnpm-cache" "node-python-cache" "pnpm 的下载缓存" "$HOME/.cache/pnpm" "safe" "pnpm 临时存的包信息和一次性运行的工具，删了会自动重新下载"
probe_dir "cache-yarn" "node-python-cache" "Yarn 下载的包" "$HOME/.cache/yarn" "safe" "Yarn 下载过的包，删了下次安装时会重新下载"
probe_dir "cache-pip" "node-python-cache" "pip 下载的 Python 包" "$HOME/.cache/pip" "safe" "pip 下载过的 Python 安装包，删了下次安装时会重新下载"
probe_dir "cache-uv" "node-python-cache" "uv 下载的 Python 包" "$HOME/.cache/uv" "safe" "uv 下载过的 Python 包，删了下次用到时会重新下载"

probe_dir "cache-cargo-registry" "rust-xcode-build" "Rust 下载的依赖包（Cargo）" "$HOME/.cargo/registry" "safe" "Rust 项目下载过的依赖包，删了下次编译时会重新下载"
probe_dir "cache-cargo-git" "rust-xcode-build" "Rust 从代码仓库拉的依赖（Cargo）" "$HOME/.cargo/git" "safe" "Rust 项目直接从代码仓库拉下来的依赖，删了下次编译时会重新拉"
probe_dir "cache-xcode-derived" "rust-xcode-build" "Xcode 编译缓存" "$HOME/Library/Developer/Xcode/DerivedData" "safe" "Xcode 编译时留下的文件，删了下次编译会重新生成，第一次会慢一些"
probe_dir "cache-swiftpm" "rust-xcode-build" "Swift 下载的依赖包" "$HOME/.cache/org.swift.swiftpm" "safe" "Swift 项目下载过的依赖包，删了下次编译时会重新下载"
probe_dir "cache-clang" "rust-xcode-build" "Clang 编译缓存" "$HOME/.cache/clang" "safe" "C 和 C++ 编译时留下的缓存，删了下次编译会自动再生成"
probe_dir "cache-gradle" "rust-xcode-build" "Gradle 编译和下载缓存" "$HOME/.gradle/caches" "safe" "安卓和 Java 项目编译时留下的缓存和下载的包，删了下次编译时会重新生成"
probe_dir "cache-playwright" "rust-xcode-build" "Playwright 自动测试用的浏览器" "$HOME/.cache/ms-playwright" "review" "自动测试用的几个浏览器，删了下次跑测试前要重新下载，比较大、比较慢"

echo "@@SECTION:TMP_LOGS"
if command -v journalctl >/dev/null 2>&1; then
  journalctl --disk-usage 2>/dev/null || sudo -n journalctl --disk-usage 2>/dev/null || true
fi
if [ -d /var/log ]; then
  rot_kb=$(find /var/log -maxdepth 2 -type f \( -name '*.gz' -o -name '*.1' -o -name '*.old' \) -exec du -k {} + 2>/dev/null | awk '{s+=$1} END {print s+0}')
  if [ -n "$rot_kb" ] && [ "$rot_kb" -gt 0 ] 2>/dev/null; then
    printf 'logs-rotated\ttmp-logs\t系统换下来的旧日志\t/var/log\t%s\tsafe\t/var/log 里换下来的旧日志文件（.gz 压缩的和 .1、.old 结尾的），正在写的日志不碰\n' "$rot_kb"
  fi
fi
if [ -d /tmp ]; then
  tmp_kb=$(find /tmp -mindepth 1 -maxdepth 1 \( -name 'go-build*' -o -name 'playwright*' -o -name 'npm-*' -o -name 'pnpm-*' -o -name 'cargo-install*' -o -name 'disklessly-*' -o -mtime +7 \) -exec du -xsk {} + 2>/dev/null | awk '{s+=$1} END {print s+0}')
  if [ -n "$tmp_kb" ] && [ "$tmp_kb" -gt 0 ] 2>/dev/null; then
    printf 'tmp-stale\ttmp-logs\t临时文件夹里的旧文件和编译剩下的文件\t/tmp\t%s\tsafe\t临时文件夹 /tmp 里编译和安装剩下的文件（go-build、playwright、npm、pnpm、cargo-install 等开头的，最近的也删）和放了 7 天以上的文件\n' "$tmp_kb"
  fi
fi

echo "@@SECTION:DOCKER"
if command -v docker >/dev/null 2>&1; then
  dangling_kb=$(docker images -f "dangling=true" --format '{{.Size}}' 2>/dev/null | awk '
    function to_kb(s,   v) {
      v = s + 0
      if (index(s, "GB") || index(s, "GiB")) return int(v * 1024 * 1024)
      if (index(s, "MB") || index(s, "MiB")) return int(v * 1024)
      if (index(s, "kB") || index(s, "KB") || index(s, "KiB")) return int(v)
      return 0
    }
    { sum += to_kb($0) }
    END { print sum + 0 }
  ')
  if [ -n "$dangling_kb" ] && [ "$dangling_kb" -gt 0 ] 2>/dev/null; then
    printf 'docker-dangling\tdocker-unused\tDocker 没名字的旧镜像\tdocker://dangling-images\t%s\tsafe\t重新构建后被换下来、已经没有名字的旧镜像，没有任何容器（不管在不在运行）用到它们\n' "$dangling_kb"
  fi
  bcache_line=$(docker system df --format '{{.Type}}|{{.Reclaimable}}' 2>/dev/null | awk -F'|' '$1=="Build Cache"{print $2}' | head -n 1)
  if [ -n "$bcache_line" ]; then
    printf 'docker-buildx\t%s\n' "$bcache_line"
  fi
fi

echo "@@SECTION:SESSIONS"
probe_sessions() {
  tool="$1"; label="$2"; dir="$3"
  if [ -d "$dir" ]; then
    hot_c=$(find "$dir" -type f \( -name '*.jsonl' -o -name '*.json' \) -mtime -7 2>/dev/null | wc -l | tr -d ' ')
    hot_kb=$(find "$dir" -type f \( -name '*.jsonl' -o -name '*.json' \) -mtime -7 -exec du -k {} + 2>/dev/null | awk '{s+=$1} END {print s+0}')
    cold_c=$(find "$dir" -type f \( -name '*.jsonl' -o -name '*.json' \) -mtime +6 2>/dev/null | wc -l | tr -d ' ')
    cold_kb=$(find "$dir" -type f \( -name '*.jsonl' -o -name '*.json' \) -mtime +6 -exec du -k {} + 2>/dev/null | awk '{s+=$1} END {print s+0}')
    real=$(cd "$dir" 2>/dev/null && pwd -P)
    printf 'tool\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$tool" "$label" "$dir" "${hot_c:-0}" "${hot_kb:-0}" "${cold_c:-0}" "${cold_kb:-0}" "${real:-$dir}"
  fi
}
probe_sessions "claude" "Claude Code" "$HOME/.claude/projects"
probe_sessions "codex" "Codex" "$HOME/.codex/sessions"
arch_dir="$HOME/.disklessly/session-archives"
if [ -d "$arch_dir" ]; then
  ac=$(find "$arch_dir" -type f -name '*.tar.gz' 2>/dev/null | wc -l | tr -d ' ')
  akb=$(du -xsk "$arch_dir" 2>/dev/null | awk '{print $1}' | head -n 1)
  printf 'archive\t%s\t%s\t%s\n' "$arch_dir" "${ac:-0}" "${akb:-0}"
fi
` + resolveCLIScript + `
if [ -n "$DKL" ]; then
  echo "@@SECTION:SESSIONS_CLI_PLAN"
  "$DKL" --json sessions ` + cliDaysFlag + ` 2>/dev/null | tr '\n' ' '; echo
  echo "@@SECTION:SESSIONS_CLI_STATUS"
  "$DKL" --json sessions status 2>/dev/null | tr '\n' ' '; echo
  echo "@@SECTION:SESSIONS_FILES"
  for d in "$HOME/.claude/projects" "$HOME/.codex/sessions"; do
    if [ -d "$d" ]; then
      r=$(cd "$d" 2>/dev/null && pwd -P) || r="$d"
      find "$r/" -type f \( -name '*.jsonl' -o -name '*.json' \) -mtime +5 -exec du -k {} + 2>/dev/null
    fi
  done
fi
echo "@@SECTION:END"
`

// ParseProbeOutput parses the output of ProbeScript into a Scan report.
func ParseProbeOutput(m *Machine, output string, now time.Time) *Scan {
	sections := map[string][]string{}
	cur := ""
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(line, "@@SECTION:") {
			cur = strings.TrimSpace(strings.TrimPrefix(line, "@@SECTION:"))
			sections[cur] = nil
		} else if cur != "" {
			sections[cur] = append(sections[cur], line)
		}
	}

	unameLine := firstNonEmpty(sections["UNAME"])
	platform := parsePlatformString(unameLine)
	hostname := cmp.Or(firstNonEmpty(sections["HOSTNAME"]), m.Host)
	remoteHome := firstNonEmpty(sections["HOME"])
	binVer := firstNonEmpty(sections["BIN_VERSION"])
	scanMode := "builtin-sh"
	if binVer != "" {
		scanMode = "disklessly-cli"
	}

	mount := parseDFSection(sections["DF"], platform)
	var items []ScanItem

	for _, line := range sections["CACHES"] {
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
		if err != nil || kb == 0 {
			continue
		}
		items = append(items, ScanItem{
			ID:       parts[0],
			Category: parts[1],
			Title:    parts[2],
			Path:     parts[3],
			Bytes:    kb * 1024,
			Safety:   parts[5],
			Action:   "clean",
			Reason:   parts[6],
		})
	}

	for _, line := range sections["TMP_LOGS"] {
		if strings.Contains(line, "\t") {
			parts := strings.Split(line, "\t")
			if len(parts) >= 7 {
				kb, err := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
				if err == nil && kb > 0 {
					items = append(items, ScanItem{
						ID:       parts[0],
						Category: parts[1],
						Title:    parts[2],
						Path:     parts[3],
						Bytes:    kb * 1024,
						Safety:   parts[5],
						Action:   "clean",
						Reason:   parts[6],
					})
				}
			}
			continue
		}
		if b := parseJournalBytes(line); b > 0 {
			items = append(items, ScanItem{
				ID:       "journalctl-vacuum",
				Category: CatTmpLogs,
				Title:    journalTitle,
				Path:     "/var/log/journal",
				Bytes:    b,
				Safety:   SafetySafe,
				Action:   "clean",
				Reason:   journalReason,
			})
		}
	}

	for _, line := range sections["DOCKER"] {
		parts := strings.Split(line, "\t")
		if len(parts) >= 7 && parts[0] == "docker-dangling" {
			kb, err := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
			if err == nil && kb > 0 {
				items = append(items, ScanItem{
					ID:       parts[0],
					Category: parts[1],
					Title:    parts[2],
					Path:     parts[3],
					Bytes:    kb * 1024,
					Safety:   parts[5],
					Action:   "clean",
					Reason:   parts[6],
				})
			}
		} else if len(parts) == 2 && parts[0] == "docker-buildx" {
			if b := parseHumanSize(parts[1]); b > 0 {
				items = append(items, ScanItem{
					ID:       "docker-buildx",
					Category: CatDockerUnused,
					Title:    buildCacheTitle,
					Path:     "docker://build-cache",
					Bytes:    b,
					Safety:   SafetySafe,
					Action:   "clean",
					Reason:   buildCacheReason,
				})
			}
		}
	}

	sess := SessionSummary{
		ArchiveDir: cmp.Or(remoteHome, "~") + "/.disklessly/session-archives",
	}
	for _, line := range sections["SESSIONS"] {
		parts := strings.Split(line, "\t")
		if len(parts) >= 8 && parts[0] == "tool" {
			hotC, _ := strconv.Atoi(parts[4])
			hotKB, _ := strconv.ParseUint(parts[5], 10, 64)
			coldC, _ := strconv.Atoi(parts[6])
			coldKB, _ := strconv.ParseUint(parts[7], 10, 64)
			ts := AgentToolSessions{
				Tool:      parts[1],
				Label:     parts[2],
				Directory: parts[3],
				HotCount:  hotC,
				HotBytes:  hotKB * 1024,
				ColdCount: coldC,
				ColdBytes: coldKB * 1024,
			}
			if len(parts) >= 9 {
				ts.RealDirectory = strings.TrimSpace(parts[8])
			}
			sess.HotCount += ts.HotCount
			sess.HotBytes += ts.HotBytes
			sess.ColdCount += ts.ColdCount
			sess.ColdBytes += ts.ColdBytes
			sess.Tools = append(sess.Tools, ts)
		} else if len(parts) >= 4 && parts[0] == "archive" {
			sess.ArchiveDir = parts[1]
			ac, _ := strconv.Atoi(parts[2])
			akb, _ := strconv.ParseUint(parts[3], 10, 64)
			sess.ArchivedCount = ac
			sess.ArchivedBytes = akb * 1024
		}
	}

	items = append(items, applySessionCLI(&sess, sections)...)

	// Sort items by size descending, then path ascending.
	slices.SortFunc(items, func(a, b ScanItem) int {
		if a.Bytes != b.Bytes {
			if a.Bytes > b.Bytes {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Path, b.Path)
	})

	var reclaimable, safeBytes, reviewBytes uint64
	for _, it := range items {
		if it.InUse {
			continue
		}
		reclaimable += it.Bytes
		if it.Category == CatColdAgentSession {
			// Uploaded off the machine, not a rebuildable cache: never part of the safe total.
			continue
		}
		if it.Safety == SafetySafe {
			safeBytes += it.Bytes
		} else {
			reviewBytes += it.Bytes
		}
	}

	return &Scan{
		MachineID:        m.ID,
		Host:             m.Host,
		Hostname:         hostname,
		RemoteHome:       remoteHome,
		Platform:         platform,
		BinaryVersion:    binVer,
		ScanMode:         scanMode,
		Mount:            mount,
		Items:            items,
		Sessions:         sess,
		ReclaimableBytes: reclaimable,
		SafeBytes:        safeBytes,
		ReviewBytes:      reviewBytes,
		ScannedAt:        now,
	}
}

func firstNonEmpty(lines []string) string {
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

func parsePlatformString(uname string) string {
	fields := strings.Fields(strings.ToLower(uname))
	if len(fields) < 2 {
		return strings.TrimSpace(uname)
	}
	sys, arch := fields[0], fields[1]
	switch {
	case strings.Contains(sys, "linux"):
		sys = "linux"
	case strings.Contains(sys, "darwin"):
		sys = "darwin"
	}
	switch arch {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	}
	return sys + "-" + arch
}

func parseDFSection(lines []string, platform string) MountInfo {
	fsType := "ext4"
	if strings.HasPrefix(platform, "darwin") {
		fsType = "apfs"
	}
	var header string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "filesystem") {
			header = strings.ToLower(line)
			continue
		}
		cols := strings.Fields(line)
		if len(cols) < 6 {
			continue
		}
		total, err1 := strconv.ParseUint(cols[1], 10, 64)
		used, err2 := strconv.ParseUint(cols[2], 10, 64)
		avail, err3 := strconv.ParseUint(cols[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		// df -kP reports 1024-byte blocks; df -B1 reports 1B-blocks.
		isBytes := strings.Contains(header, "1b-blocks") || total > 10*1024*1024*1024
		mult := uint64(1024)
		if isBytes {
			mult = 1
		}
		return MountInfo{
			Device:         cols[0],
			MountPoint:     strings.Join(cols[5:], " "),
			FSType:         fsType,
			TotalBytes:     total * mult,
			UsedBytes:      used * mult,
			AvailableBytes: avail * mult,
		}
	}
	return MountInfo{MountPoint: "/", FSType: fsType}
}

func parseJournalBytes(line string) uint64 {
	_, after, ok := strings.Cut(line, "take up ")
	if !ok {
		return 0
	}
	tok, _, _ := strings.Cut(strings.TrimSpace(after), " ")
	return parseHumanSize(tok)
}

func parseHumanSize(raw string) uint64 {
	s := strings.TrimSpace(raw)
	if idx := strings.IndexAny(s, " ("); idx > 0 {
		s = s[:idx]
	}
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "0B" || s == "0" {
		return 0
	}
	type unit struct {
		suffix string
		mult   float64
	}
	units := []unit{
		{"TIB", float64(1024 * gib)},
		{"TB", float64(1024 * gib)},
		{"T", float64(1024 * gib)},
		{"GIB", float64(gib)},
		{"GB", float64(gib)},
		{"G", float64(gib)},
		{"MIB", float64(mib)},
		{"MB", float64(mib)},
		{"M", float64(mib)},
		{"KIB", 1024},
		{"KB", 1024},
		{"K", 1024},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			numStr := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil || val <= 0 {
				return 0
			}
			return uint64(val * u.mult)
		}
	}
	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// ScanMachine probes m using runner and updates st.Scans[m.ID] and m's metadata.
func ScanMachine(ctx context.Context, runner Runner, st *State, m *Machine, now time.Time) (*Scan, error) {
	if runner == nil {
		runner = DefaultRunner
	}
	out, err := runner(ctx, m, ProbeScript, probeTimeout)
	scan := ParseProbeOutput(m, out, now)
	if err != nil {
		// The probe reads the root volume first and sizes caches after it. On a computer with large caches the sizing
		// can outlast the probe's time: the watermark it already read is kept, and the caches are marked partial. A
		// probe that stopped before its df line was whole has no watermark (a cut line parses to no volume) and fails.
		if scan.Mount.TotalBytes == 0 {
			m.Reachable = false
			m.LastError = err.Error()
			m.UpdatedAt = now
			return nil, err
		}
		scan.Partial = true
		scan.PartialReason = err.Error()
	}
	m.Reachable = true
	m.LastError = ""
	if scan.Platform != "" {
		m.Platform = scan.Platform
	}
	if scan.BinaryVersion != "" {
		m.BinaryVersion = scan.BinaryVersion
	}
	m.ScanMode = scan.ScanMode
	m.UpdatedAt = now
	if st.Scans == nil {
		st.Scans = map[string]*Scan{}
	}
	st.Scans[m.ID] = scan
	if !scan.Partial && (scan.BinaryVersion != "" || scan.Sessions.CLIReady) {
		// Keep the local cold index current without pulling the index on every scan; a failed
		// refresh only marks the cloud state unknown.
		refreshColdIfStale(ctx, runner, st, m, now, coldScanRefreshAge)
	}
	return scan, nil
}

// sizeKB looks up a transcript's size in the SESSIONS_FILES map, which lists files under the
// resolved tool directory. p may be in either the configured (dir) or the resolved (real) form.
func sizeKB(kb map[string]uint64, p, dir, real string) uint64 {
	if v, ok := kb[p]; ok {
		return v
	}
	for _, pair := range [][2]string{{dir, real}, {real, dir}} {
		from, to := path.Clean(pair[0]), path.Clean(pair[1])
		if pair[0] == "" || pair[1] == "" || from == to || !underDir(p, from) {
			continue
		}
		if v, ok := kb[to+strings.TrimPrefix(path.Clean(p), from)]; ok {
			return v
		}
	}
	return 0
}

// applySessionCLI replaces the find-based cold session estimate with the disklessly CLI's own
// cold plan (which skips hot, open and per-project longer-threshold sessions), attaches the
// cloud backup state, and returns the archivable cold session items. Without a CLI that reports
// both its plan and a reachable backend, no session item is offered: the extension never
// archives or deletes transcripts itself.
func applySessionCLI(sess *SessionSummary, sections map[string][]string) []ScanItem {
	var plan sessionReport
	planOK := parseCLIJSONLine(sections["SESSIONS_CLI_PLAN"], &plan)
	var status CloudStatus
	if parseCLIJSONLine(sections["SESSIONS_CLI_STATUS"], &status) {
		sess.Cloud = &status
	}
	sess.CLIReady = planOK && sess.Cloud != nil
	switch {
	case !sess.CLIReady:
		sess.Note = "这台电脑上没有能搬对话的配套小工具（disklessly），旧对话原样留着；到「AI 对话」页装好或升级它，再检查一次。"
		return nil
	case !sess.Cloud.Backend.Reachable:
		sess.Note = "网盘连不上，旧对话先不搬；确认网盘在线后再检查一次。"
	}

	kb := map[string]uint64{}
	for _, line := range sections["SESSIONS_FILES"] {
		size, p, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if v, err := strconv.ParseUint(strings.TrimSpace(size), 10, 64); err == nil {
			kb[p] = v
		}
	}
	sess.ColdCount, sess.ColdBytes = 0, 0
	var items []ScanItem
	for i := range sess.Tools {
		ts := &sess.Tools[i]
		ts.ColdCount, ts.ColdBytes = 0, 0
		for _, p := range plan.Cold {
			if underAny(p, ts.Directory, ts.RealDirectory) {
				ts.ColdCount++
				ts.ColdBytes += sizeKB(kb, p, ts.Directory, ts.RealDirectory) * 1024
			}
		}
		sess.ColdCount += ts.ColdCount
		sess.ColdBytes += ts.ColdBytes
		if ts.ColdCount == 0 || !sess.Cloud.Backend.Reachable {
			continue
		}
		items = append(items, ScanItem{
			ID:         "sessions-cold-" + ts.Tool,
			Category:   CatColdAgentSession,
			Title:      coldItemTitle(ts.Label, ts.ColdCount),
			Path:       ts.Directory,
			Bytes:      ts.ColdBytes,
			Safety:     SafetySafe,
			Action:     "archive",
			Reason:     coldItemReason,
			SessionCnt: ts.ColdCount,
			RealPath:   ts.RealDirectory,
		})
	}
	return items
}

// The titles and reasons of the items the parser makes itself (the probe script prints the others).
const (
	journalTitle     = "系统日志（只删 7 天以前的）"
	journalReason    = "这里显示的是全部系统日志的大小，只删其中 7 天以前的部分，最近 7 天的留着"
	buildCacheTitle  = "Docker 构建缓存（没在用的）"
	buildCacheReason = "正在进行的构建没用到的缓存，正在运行的服务和容器绝不碰"
	coldItemReason   = "先放进网盘、读回核对无误才删电脑上的那份；最近 7 天用过、正打开着或还有程序在用的对话留着"
)

func coldItemTitle(label string, count int) string {
	return fmt.Sprintf("%s 一周没动的对话（%d 个）", label, count)
}

// itemText is the current wording of one kind of scan item.
type itemText struct{ title, reason string }

// itemWords is the wording this version gives each scan item, by item id: the probe script's
// probe_dir and printf lines, and the items the parser makes itself. A scan stored by an older
// version keeps that version's words (such as 「Playwright 浏览器缓存 (ms-playwright)」); the
// pages show these instead, so one kind of item has one name and one description everywhere.
var itemWords = func() map[string]itemText {
	out := map[string]itemText{
		"journalctl-vacuum": {journalTitle, journalReason},
		"docker-buildx":     {buildCacheTitle, buildCacheReason},
	}
	probe := regexp.MustCompile(`(?m)^probe_dir "([^"]+)" "[^"]+" "([^"]+)" "[^"]+" "[^"]+" "([^"]+)"$`)
	for _, m := range probe.FindAllStringSubmatch(ProbeScript, -1) {
		out[m[1]] = itemText{m[2], m[3]}
	}
	printed := regexp.MustCompile(`(?m)^\s*printf '([a-z-]+)\\t[a-z-]+\\t([^\\]+)\\t[^\\]+\\t%s\\t[a-z]+\\t([^\\]+)\\n'`)
	for _, m := range printed.FindAllStringSubmatch(ProbeScript, -1) {
		out[m[1]] = itemText{m[2], m[3]}
	}
	return out
}()

// coldToolLabels names the AI assistants of the probe script's session directories.
var coldToolLabels = map[string]string{"claude": "Claude Code", "codex": "Codex"}

// itemTitle is the name the pages give scan item it: this version's wording of its kind, else its own.
func itemTitle(it ScanItem) string {
	if w, ok := itemWords[it.ID]; ok {
		return w.title
	}
	if tool, ok := strings.CutPrefix(it.ID, "sessions-cold-"); ok && it.Category == CatColdAgentSession && it.SessionCnt > 0 {
		if label, ok := coldToolLabels[tool]; ok {
			return coldItemTitle(label, it.SessionCnt)
		}
	}
	return strings.TrimSpace(it.Title)
}

// itemReason is the description the pages give scan item it, as itemTitle does for its name.
func itemReason(it ScanItem) string {
	if w, ok := itemWords[it.ID]; ok {
		return w.reason
	}
	if it.Category == CatColdAgentSession && strings.HasPrefix(it.ID, "sessions-cold-") {
		return coldItemReason
	}
	return strings.TrimSpace(it.Reason)
}
