package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sftp -D sftp_server_command connects to a local sftp server: newer OpenSSH splits the value into a program and its
// arguments and execs it on this computer, like an ssh -o ProxyCommand value.
func TestSftpDirectServerCommand(t *testing.T) {
	// Probe the real client in a subtest so OpenSSH builds whose sftp -D does not split arguments (e.g. 8.4 on
	// Debian 11) skip only the fixture check while still running all guard policy cases below.
	t.Run("fixture", func(t *testing.T) {
		if _, err := exec.LookPath("sftp"); err != nil {
			t.Skip("sftp not in PATH")
		}
		for i, before := range [][]string{nil, {"-D", "true"}} {
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			out, _ := exec.CommandContext(ctx, "sftp", append(before, "-D", "rm -f "+sentinel)...).CombinedOutput()
			cancel()
			if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
				if i == 0 && strings.Contains(string(out), "No such file or directory") {
					t.Skipf("sftp -D does not split arguments on this OpenSSH build: %s", strings.TrimSpace(string(out)))
				}
				t.Fatalf("fixture: sftp %q -D did not run its value: %s", before, out)
			}
		}
	})
	for _, c := range []struct {
		command string
		blocked bool
	}{
		{`sftp -D 'rm -rf /x'`, true},
		{`sftp -D'rm -rf /x'`, true},
		{`sftp -qD 'rm -rf /x'`, true},
		{`sftp -qD'rm -rf /x'`, true},
		{`sftp -P 22 -D 'rm -rf /x'`, true},
		{`sftp -D 'sh -c "rm -rf /x"'`, true},
		{`sudo /usr/bin/sftp -D 'rm -rf /x'`, true},
		{`sftp -D true -D 'rm -rf /x'`, true},
		{`sftp -D 'rm -rf /x' -D true`, true},
		{`sftp -D /usr/libexec/sftp-server`, false},
		{`sftp -D '/usr/libexec/sftp-server -el debug3'`, false},
		{`sftp -b batch host`, false},
		{`sftp -o ConnectTimeout=5 host`, false},
	} {
		t.Run(c.command, func(t *testing.T) {
			g := newGuard("/home/me/project", false)
			if why := g.personOnly(c.command); (why != "") != c.blocked {
				t.Errorf("personOnly() = %q, want blocked %v", why, c.blocked)
			}
		})
	}
}
