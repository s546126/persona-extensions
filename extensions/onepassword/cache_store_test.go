package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// This is the Hub's already-sealed connector slot as seen by the extension, never a file or an op session.
type memoryMetadata struct {
	mu               sync.Mutex
	data             []byte
	loadErr, saveErr error
	loadFailures     int
	ops              []string
	started, release chan struct{}
}

func (s *memoryMetadata) Load(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadFailures > 0 {
		s.loadFailures--
		return nil, errors.New("temporary Hub storage failure")
	}
	return append([]byte(nil), s.data...), s.loadErr
}
func (s *memoryMetadata) Save(ctx context.Context, b []byte) error {
	s.mu.Lock()
	started, release := s.started, s.release
	s.started = nil
	s.mu.Unlock()
	if started != nil {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.data = append([]byte(nil), b...)
	s.ops = append(s.ops, "save")
	return nil
}
func (s *memoryMetadata) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	s.ops = append(s.ops, "delete")
	return nil
}
func (s *memoryMetadata) bytes() []byte { b, _ := s.Load(context.Background()); return b }

func restoreMetadata(t *testing.T, a *app, s metadataStore, source string) {
	t.Helper()
	a.startMeta(context.Background(), s, source)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.waitMeta(ctx); err != nil {
		t.Fatal(err)
	}
}

// One granted authorization; >15 minutes later the Hub/app is recreated and the CLI authorization has lapsed.
// The vault list and descriptions must still answer, repeatedly, without a second prompt or a refreshed timestamp.
func TestMetadataSurvivesHubRecreationAfterAuthorizationExpires(t *testing.T) {
	cli := &lapsingCLI{fakeCLI: newFakeCLI()}
	var grantMu sync.Mutex
	grants := 0
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		grantMu.Lock()
		if grants == 0 && args[0] != "whoami" && args[0] != "--version" {
			grants++
			cli.set(true)
		}
		grantMu.Unlock()
		return cli.run(ctx, args...)
	}
	a, _ := onComputerRun(t, run)
	// This fixture exercises cache/auth RPCs. The SDK parser's cold WASM startup is tested separately and must not
	// consume secretTimeout before the fake node sees a describe request.
	a.validate = func(context.Context, string) error { return nil }
	at := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return at }
	slot := &memoryMetadata{}
	source := metadataSource("m1", "")
	restoreMetadata(t, a, slot, source)
	if text := mustTool(t, a, "onepassword_vaults"); !strings.Contains(text, "Dev") {
		t.Fatal(text)
	}
	const ref = "op://Dev/GitHub token/credential"
	if got := describe(t, a, ref)["title"]; got != "GitHub token › credential" {
		t.Fatal(got)
	}
	before := slot.bytes()
	assertNoCLIValues(t, "stored metadata", string(before))
	assertNoSecrets(t, "stored metadata", string(before))
	first, err := decodeMetadata(before, source, at)
	if err != nil || first.Vaults == nil || !first.Vaults.At.Equal(at) {
		t.Fatalf("first record: %v", err)
	}
	cli.set(false)
	at = at.Add(16 * time.Minute)
	// New Hub-side app and new resident node app. Only the Hub's sealed slot is shared.
	b, _ := onComputerRun(t, run)
	b.validate = a.validate
	b.now = a.now
	restoreMetadata(t, b, slot, source)
	for range 3 {
		if text := mustTool(t, b, "onepassword_vaults"); !strings.Contains(text, "Dev") {
			t.Fatal(text)
		}
		if got := describe(t, b, ref)["title"]; got != "GitHub token › credential" {
			t.Fatal(got)
		}
	}
	if grants != 1 || len(cli.asked()) != 0 {
		t.Fatalf("authorizations=%d, unexpected prompts=%v", grants, cli.asked())
	}
	if string(slot.bytes()) != string(before) {
		t.Fatal("a quiet refusal renewed the acquired time or changed the stored answer")
	}
}

// Scrubbed field names still match after reconstruction: each app owns a different initial seed; only the seed in
// the sealed record can find the earlier keyed name without persisting any readable form of its secret value.
func TestRestoredMetadataKeepsItsPrivateNameIndex(t *testing.T) {
	a, f, _ := clocked(t)
	a.validate = func(context.Context, string) error { return nil }
	slot := &memoryMetadata{}
	source := metadataSource("", testToken)
	restoreMetadata(t, a, slot, source)
	ref := "op://Dev/GitHub token/sec2/PIN " + secretValues[5]
	want := describe(t, a, ref)
	before := slot.bytes()
	assertNoSecrets(t, "stored metadata", string(before))
	b := newApp(fakeConnect(f))
	b.validate = a.validate
	b.token = testToken
	b.now = a.now
	initial := b.metaSeed
	restoreMetadata(t, b, slot, source)
	if initial == b.metaSeed {
		t.Fatal("the second app did not restore the first app's seed")
	}
	f.mu.Lock()
	f.failAll = &ext.Error{Code: codeLocked, Message: "authorization expired"}
	f.mu.Unlock()
	b.now = func() time.Time { return a.now().Add(time.Hour) }
	if got := describe(t, b, ref); got["title"] != want["title"] || got["detail"] != want["detail"] {
		t.Fatalf("description after restart: %v, want %v", got, want)
	}
}

func TestMetadataRejectsCorruptionAndChangedSources(t *testing.T) {
	a, _, _ := clocked(t)
	slot := &memoryMetadata{}
	source := metadataSource("", testToken)
	restoreMetadata(t, a, slot, source)
	if _, err := a.listVaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	valid := slot.bytes()
	for _, tc := range []struct {
		name, source string
		alter        func([]byte) []byte
	}{
		{"corrupt", source, func([]byte) []byte { return []byte("not json") }},
		{"token changed", metadataSource("", "different synthetic token"), nil},
		{"computer selected", metadataSource("m2", testToken), nil},
		{"unsupported", source, func(b []byte) []byte { return []byte(strings.Replace(string(b), `"version":1`, `"version":99`, 1)) }},
		{"changed token", metadataSource("", testToken+"changed"), nil},
		{"changed computer", metadataSource("m2", ""), nil},
		{"changed connection mode", metadataSource("m1", ""), nil},
		{"invalid time", source, func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), "2026-11-12T09:00:00Z", "2099-01-01T00:00:00Z"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), valid...)
			if tc.alter != nil {
				b = tc.alter(b)
			}
			s := &memoryMetadata{data: b}
			fresh := newApp(nil)
			fresh.now = a.now
			restoreMetadata(t, fresh, s, tc.source)
			fresh.metaMu.Lock()
			kept := fresh.meta.vaults != nil
			fresh.metaMu.Unlock()
			if kept || len(s.bytes()) != 0 {
				t.Fatal("corrupt or other-source metadata survived")
			}
		})
	}
	if metadataSource("m1", testToken) != metadataSource("m1", "unused changed token") || metadataSource("m1", "") == metadataSource("m2", "") {
		t.Fatal("computer source identity")
	}
}

func TestMetadataStorageFailuresNeverAuthorizeOrPersistErrors(t *testing.T) {
	a, f, _ := clocked(t)
	source := metadataSource("", testToken)
	slot := &memoryMetadata{loadErr: errors.New("private storage detail")}
	a.startMeta(context.Background(), slot, source)
	if _, err := a.listVaults(context.Background()); !errors.Is(err, errMetadataStore) {
		t.Fatalf("load failure: %v", err)
	}
	if f.count() != 0 || len(slot.bytes()) != 0 {
		t.Fatal("an unread record caused a provider request or was overwritten")
	}
	slot.mu.Lock()
	slot.loadErr = nil
	slot.mu.Unlock()
	slot.saveErr = errors.New("private storage detail")
	restoreMetadata(t, a, slot, source)
	if _, err := a.listVaults(context.Background()); !errors.Is(err, errMetadataStore) {
		t.Fatalf("save failure: %v", err)
	}
	slot.saveErr = nil
	if _, err := a.listVaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || len(slot.bytes()) == 0 {
		t.Fatal("a failed save was not retried from the already fetched metadata")
	}
	if err := a.forgetMeta(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failAll = errors.New("synthetic provider failure")
	f.mu.Unlock()
	if _, err := a.listVaults(context.Background()); err == nil {
		t.Fatal("provider failure accepted")
	}
	if len(slot.bytes()) != 0 {
		t.Fatal("a provider error was persisted")
	}
}

func TestMetadataLoadRetryRestoresTheUnreadAnswerWithoutCallingTheProvider(t *testing.T) {
	a, f, _ := clocked(t)
	source := metadataSource("", testToken)
	first := &memoryMetadata{}
	restoreMetadata(t, a, first, source)
	if _, err := a.listVaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	slot := &memoryMetadata{data: first.bytes(), loadFailures: 1}
	b := newApp(fakeConnect(f))
	b.token = testToken
	b.now = a.now
	b.startMeta(context.Background(), slot, source)
	if _, err := b.listVaults(context.Background()); !errors.Is(err, errMetadataStore) {
		t.Fatalf("first failed load: %v", err)
	}
	if vs, err := b.listVaults(context.Background()); err != nil || len(vs) != 3 {
		t.Fatalf("restored after retry: %v %v", vs, err)
	}
	if f.count() != 1 || string(slot.bytes()) != string(first.bytes()) {
		t.Fatal("storage recovery called the provider or changed the unread record")
	}
}

func TestMetadataClearFollowsAnInflightSaveAndFencesOldReads(t *testing.T) {
	a, _, _ := clocked(t)
	slot := &memoryMetadata{started: make(chan struct{}), release: make(chan struct{})}
	started := slot.started
	restoreMetadata(t, a, slot, metadataSource("", testToken))
	done := make(chan error, 1)
	go func() { _, err := a.listVaults(context.Background()); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("save did not start")
	}
	cleared := make(chan error, 1)
	go func() { cleared <- a.forgetMeta() }()
	close(slot.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	if len(slot.bytes()) != 0 {
		t.Fatal("the earlier save replaced the clear")
	}
	_, err := cachedRead(context.Background(), a, false, (*metaCache).getVaults,
		func(c *metaCache, k kept[[]vaultInfo]) { c.vaults = &k },
		func() ([]vaultInfo, error) {
			if err := a.forgetMeta(); err != nil {
				return nil, err
			}
			return []vaultInfo{{ID: "old", Title: "old answer"}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	a.metaMu.Lock()
	kept := a.meta.vaults != nil
	a.metaMu.Unlock()
	if kept || len(slot.bytes()) != 0 {
		t.Fatal("an old callback refilled cleared memory or sealed storage")
	}
}

func TestMetadataRecordHasByteAndEntryBounds(t *testing.T) {
	at := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	r := metadataRecord{Format: metadataFormat, Version: metadataVersion, Source: "source", Seed: make([]byte, 32),
		Vaults: &metadataEntry[[]vaultInfo]{Value: []vaultInfo{{ID: "v", Title: "Vault"}}, At: at},
		Items:  map[string]metadataEntry[[]itemInfo]{"v": {At: at, Value: []itemInfo{{ID: "i", VaultID: "v", Title: strings.Repeat("x", maxMetadataBytes)}}}}}
	b, err := boundedMetadata(r)
	if err != nil || len(b) > maxMetadataBytes {
		t.Fatalf("bounded record: %d %v", len(b), err)
	}
	got, err := decodeMetadata(b, "source", at)
	if err != nil || got.Vaults == nil || len(got.Items) != 0 {
		t.Fatalf("eviction: %v %+v", err, got)
	}
	for i := range maxMetaCacheEntries + 1 {
		r.Items[strconv.Itoa(i)] = metadataEntry[[]itemInfo]{At: at}
	}
	b, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeMetadata(b, "source", at); err == nil {
		t.Fatal("oversized entry map restored")
	}
}

// The actual SDK adapter uses only the connector's own sealed slot, with the manifest's existing contribution;
// no storage/files permission or new core endpoint is needed. The test Hub validates every RPC against its schema.
func TestMetadataUsesTheExistingSealedConnectorProtocol(t *testing.T) {
	a, _ := connected(t)
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program(a)), func(p *wire.InitializeParams) { p.Secrets = map[string]string{tokenSecret: testToken} })
	var out wire.ConnectorCallResult
	if err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: connectorID, Name: "onepassword_vaults", Args: json.RawMessage(`{}`)}, &out); err != nil {
		t.Fatal(err)
	}
	b := h.Secret(connectorID)
	if len(b) == 0 || len(h.Secret("unrelated-connector")) != 0 {
		t.Fatal("metadata was not saved only to its own connector")
	}
	if _, err := decodeMetadata(b, metadataSource("", testToken), time.Now()); err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "connector secret RPC", string(b))
	if err := a.forgetMeta(); err != nil {
		t.Fatal(err)
	}
	if len(h.Secret(connectorID)) != 0 {
		t.Fatal("clear did not delete the sealed record")
	}
}
