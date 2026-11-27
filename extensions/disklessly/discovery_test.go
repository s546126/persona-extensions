package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

func discoveryHome(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh/config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestDiscoveryFiltersGitAndWildcardHosts(t *testing.T) {
	home := discoveryHome(t, `Host * *.internal !ignored
 User dev
Host code.example.org *.example.org
 User git
Host github.com gitlab.com gitlab.corp.example bitbucket.org
Host git-alias
 HostName github.com
Host worker
 HostName 192.0.2.10
`)
	st := &State{Machines: []*Machine{
		{ID: "code.example.org", Host: "code.example.org", Source: "ssh-config", Enabled: true},
		{ID: "git-alias", Host: "git-alias", Source: "ssh-config", Enabled: true},
	}}
	if n := st.DiscoverMachines(home, t0); n != 1 {
		t.Fatalf("added=%d want 1", n)
	}
	if len(st.Machines) != 2 || st.machine("worker") == nil {
		t.Fatalf("machines=%+v", st.Machines)
	}
	for _, h := range []string{"code.example.org", "github.com", "gitlab.com", "gitlab.corp.example", "bitbucket.org", "git-alias", "*", "*.example.org"} {
		if st.machine(h) != nil {
			t.Errorf("unexpected host %q", h)
		}
	}
}

func TestDiscoveryDeduplicatesConnectionAndKeepsFirstAlias(t *testing.T) {
	home := discoveryHome(t, `Host nl40 l40
 HostName 192.0.2.10
 User dev
Host repeat
 HostName 192.0.2.10
 User dev
 Port 22
Host other-port
 HostName 192.0.2.10
 User dev
 Port 2222
Host other-user
 HostName 192.0.2.10
 User root
Host mac serial
`)
	if err := os.MkdirAll(filepath.Join(home, ".persona"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".persona/config.json"), []byte(`{"machines":[{"id":"persona-copy","host":"192.0.2.10","user":"dev","port":22}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	st := &State{}
	if n := st.DiscoverMachines(home, t0); n != 4 {
		t.Fatalf("added=%d want 4", n)
	}
	for _, h := range []string{"nl40", "mac", "other-port", "other-user"} {
		if st.machine(h) == nil {
			t.Errorf("missing %q", h)
		}
	}
	for _, h := range []string{"l40", "repeat", "serial", "persona-copy"} {
		if st.machine(h) != nil {
			t.Errorf("duplicate %q", h)
		}
	}
	// Old state can list the second alias first; retain the first SSH alias and its scan.
	st.Machines = append([]*Machine{{ID: "l40", Host: "l40", Source: "ssh-config", Enabled: true, Reachable: true}}, st.Machines...)
	st.Scans["l40"] = &Scan{MachineID: "l40", Mount: MountInfo{TotalBytes: 100 * gib}}
	st.DiscoverMachines(home, t0)
	if st.machine("l40") != nil || st.Scans["nl40"] == nil || st.Scans["nl40"].MachineID != "nl40" {
		t.Fatal("old duplicates were not merged into the first alias")
	}
}

func TestDiscoveryMergesLocalInterfaceIP(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	address := "127.0.0.1"
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err == nil && !ip.IsLoopback() && ip.To4() != nil {
			address = ip.String()
			break
		}
	}
	home := discoveryHome(t, "Host self-alias\n HostName "+address+"\n")
	st := &State{Machines: []*Machine{{ID: "self-alias", Host: "self-alias", Source: "ssh-config", Enabled: true}}, Scans: map[string]*Scan{"self-alias": {MachineID: "self-alias", Mount: MountInfo{TotalBytes: 100 * gib}}}}
	st.DiscoverMachines(home, t0)
	if len(st.Machines) != 1 || !st.Machines[0].IsLocal || st.Scans["local"] == nil {
		t.Fatalf("self IP %s not merged: %+v", address, st.Machines)
	}
}

func mockDiscoverySSH(t *testing.T) {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
case " $* " in
 *" no-shell "*) cat >/dev/null; echo 'Welcome to a restricted service'; exit 0;;
 *" error-shell "*) cat >/dev/null; echo 'Shell access is not supported'; exit 1;;
 *" no-stdout "*) cat >/dev/null; echo disklessly-shell-probe >&2; exit 0;;
 *" offline "*) exit 255;;
 *) exec /bin/sh -s;;
esac
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISKLESSLY_SSH", executable)
}

func TestDiscoveryRequiresEchoOnStdout(t *testing.T) {
	mockDiscoverySSH(t)
	home := discoveryHome(t, "Host shell\nHost no-shell\nHost error-shell\nHost no-stdout\nHost offline\n")
	s := newStore(t.TempDir())
	s.homeDir = home
	if err := s.update(func(st *State, _ time.Time) error {
		if len(st.Machines) != 2 || st.machine("shell") == nil {
			t.Fatalf("expected local and shell, got %+v", st.Machines)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The check is cached, but persisted candidates are rechecked by a fresh store.
	raw, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "no-shell") {
		t.Fatal("restricted endpoint persisted")
	}
}

func TestDeletedMachineStaysDeletedAcrossMCPRefreshAndRestart(t *testing.T) {
	mockDiscoverySSH(t)
	home := discoveryHome(t, "Host primary alias\n HostName 192.0.2.10\n User dev\n")
	dir := t.TempDir()
	s := newStore(dir)
	s.homeDir = home
	srv := &mcpServer{store: s}
	if _, err := srv.call(context.Background(), "disklessly_machine_delete", json.RawMessage(`{"machine":"primary"}`)); err != nil {
		t.Fatal(err)
	}
	// Change the first alias and restart: only a connection tombstone can suppress this import.
	if err := os.WriteFile(filepath.Join(home, ".ssh/config"), []byte("Host replacement alias\n HostName 192.0.2.10\n User dev\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := newStore(dir)
	restarted.homeDir = home
	srv = &mcpServer{store: restarted}
	for range 2 {
		out, err := srv.call(context.Background(), "disklessly_machines_status", json.RawMessage(`{"refresh":true}`))
		if err != nil {
			t.Fatal(err)
		}
		if out.(map[string]any)["machine_count"] != 1 {
			t.Fatalf("deleted connection reimported: %+v", out)
		}
	}
	raw, err := os.ReadFile(restarted.path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "deleted_machines") {
		t.Fatal("no persisted tombstone")
	}
	if _, err := srv.call(context.Background(), "disklessly_machine_upsert", json.RawMessage(`{"host":"replacement"}`)); err != nil {
		t.Fatal(err)
	}
	out, err := srv.call(context.Background(), "disklessly_machines_status", json.RawMessage(`{"refresh":true}`))
	if err != nil || out.(map[string]any)["machine_count"] != 2 {
		t.Fatalf("explicit re-add did not survive refresh: %v %v", out, err)
	}
}

func TestDiscoveryKeepsLoopbackSSHTunnels(t *testing.T) {
	home := discoveryHome(t, "Host tunnel\n HostName localhost\n Port 2232\n")
	st := &State{}
	st.DiscoverMachines(home, t0)
	if m := st.machine("tunnel"); m == nil || m.IsLocal || m.Port != 2232 {
		t.Fatalf("SSH tunnel was merged into local: %+v", m)
	}
	cmd, err := shellCommand(context.Background(), &Machine{Host: "localhost", Port: 2232}, "echo test")
	if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), "-p 2232") {
		t.Fatalf("tunnel command: %v %v", cmd, err)
	}
}

func TestDiscoveryMigratesDisabledAliases(t *testing.T) {
	home := discoveryHome(t, "Host first second\n HostName 192.0.2.20\n")
	st := &State{Machines: []*Machine{
		{ID: "second", Host: "second", Source: "ssh-config", Enabled: false},
		{ID: "first", Host: "first", Source: "ssh-config", Enabled: false},
	}}
	st.DiscoverMachines(home, t0)
	if len(st.Machines) != 1 || len(st.DeletedMachines) == 0 {
		t.Fatalf("disabled aliases revived: %+v", st.Machines)
	}
}

func TestDiscoveryDisabledDuplicatePreservesEnabledConnection(t *testing.T) {
	home := discoveryHome(t, "Host first second\n HostName 192.0.2.20\n")
	st := &State{Machines: []*Machine{
		{ID: "second", Host: "second", Source: "ssh-config", Enabled: false},
		{ID: "first", Host: "first", Source: "ssh-config", Enabled: true},
	}}
	st.DiscoverMachines(home, t0)
	if len(st.Machines) != 2 || st.machine("first") == nil || st.machine("second") != nil {
		t.Fatalf("removing a duplicate also removed the enabled connection: %+v", st.Machines)
	}
}

func TestManualMachineDeletionSuppressesAutomaticImport(t *testing.T) {
	home := discoveryHome(t, "Host worker\n HostName 192.0.2.10\n")
	st := &State{}
	_, err := st.upsertMachine(MachineInput{Host: "worker", Hostname: "192.0.2.10", Source: "manual"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.deleteMachine("worker"); err != nil {
		t.Fatal(err)
	}
	if n := st.DiscoverMachines(home, t0); n != 0 {
		t.Fatal("manual deletion reimported")
	}
	if _, err := st.upsertMachine(MachineInput{Host: "worker", Hostname: "192.0.2.10", Source: "manual"}, t0); err != nil {
		t.Fatal(err)
	}
	st.DiscoverMachines(home, t0)
	if st.machine("worker") == nil {
		t.Fatal("explicit re-add should clear tombstone")
	}
}

func TestUnscannedStatusAndFleetUI(t *testing.T) {
	st, m := localState(t)
	ms := st.machineStatus(m)
	if ms.Level != HealthUnscanned || ms.LastScannedAt != nil {
		t.Fatalf("unscanned status=%+v", ms)
	}
	if b := healthBadge(ms); b.Text != "还没检查" || b.Tone != wire.ToneNeutral {
		t.Fatalf("badge=%+v", b)
	}
	sections := (draw{st: st, now: t0}).fleetTab(page{}, st.statuses(), m)
	raw, err := json.Marshal(sections)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "都还宽裕") || !strings.Contains(string(raw), "1 台还没检查") {
		t.Fatalf("misleading fleet: %s", raw)
	}
	m.Reachable = false
	if got := st.machineStatus(m).Level; got != HealthOffline {
		t.Fatalf("failed probe status=%s", got)
	}
	m.Reachable = true
	st.Scans[m.ID] = &Scan{Mount: MountInfo{TotalBytes: 100 * gib, UsedBytes: 50 * gib, AvailableBytes: 50 * gib}, ScannedAt: t0}
	if got := st.machineStatus(m).Level; got != HealthOK {
		t.Fatalf("scanned healthy machine=%s", got)
	}
}
