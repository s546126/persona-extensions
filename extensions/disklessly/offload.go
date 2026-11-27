package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The disk_offload point (Persona docs/server/machine-usage-map.md, "Extension point disk_offload"): Persona's
// usage map shows what this extension already moved off a computer next to what is still on it, and offers "move
// to the cloud" for files, folders and apps. The hub calls offload.inventory / run / job / cancel with the
// Persona machine; this program maps it to its own Machine record and does the work with the disklessly CLI on
// that computer, over the same local shell or SSH as the rest of the extension. Long work runs as a cold job
// (startJob: one job per machine, coldParallel at once); its progress is kept here, by job id, for the hub to poll.

const (
	offloadNamespace = "offload"
	// offloadInventoryTTL is how long an inventory is answered from memory; refresh bypasses it.
	offloadInventoryTTL = 30 * time.Second
	// offloadJobKeep is how long a finished job can still be polled (the contract asks for at least 10 minutes).
	offloadJobKeep = 30 * time.Minute
	// offloadMaxItems is the most inventory rows returned; the rest is summed in more.
	offloadMaxItems = 500
	// offloadMaxRun is the most paths or item ids one run takes.
	offloadMaxRun = 50
	// offloadProbeTimeout bounds the inventory and check scripts (the hub gives the inventory 20 seconds).
	offloadProbeTimeout = 18 * time.Second
	// offloadItemTimeout bounds one item's upload or download.
	offloadItemTimeout = 2 * time.Hour
)

// Inventory states.
const (
	invReady        = "ready"
	invNeedsInstall = "needs_install"
	invNeedsSetup   = "needs_setup"
	invUnavailable  = "unavailable"
)

// Job states and phases.
const (
	ojRunning  = "running"
	ojDone     = "done"
	ojFailed   = "failed"
	ojCanceled = "canceled"

	phaseInstall  = "install"
	phaseCheck    = "check"
	phaseUpload   = "upload"
	phaseVerify   = "verify"
	phaseCleanup  = "cleanup"
	phaseDownload = "download"
)

// offloadHostname is the hub computer's host name (a variable for tests).
var offloadHostname = os.Hostname

type personaMachine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Online *bool  `json:"online"`
}

type offloadParams struct {
	Contribution string         `json:"contribution"`
	Locale       string         `json:"locale"`
	Machine      personaMachine `json:"machine"`
	Refresh      bool           `json:"refresh"`
	Action       string         `json:"action"`
	Paths        []string       `json:"paths"`
	Items        []string       `json:"items"`
	DryRun       bool           `json:"dry_run"`
	Install      bool           `json:"install"`
	RequestID    string         `json:"request_id"`
	Job          string         `json:"job"`
}

type offloadTool struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
	Source    string `json:"source,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// restoreRef says how an inventory row comes back.
type restoreRef struct {
	how string // "fetch", "unpack", "data", "session"
	arg string // placeholder path, bundle id, or session entry id
}

type offloadItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Bytes       uint64 `json:"bytes"`
	StoredBytes uint64 `json:"stored_bytes,omitempty"`
	OriginPath  string `json:"origin_path,omitempty"`
	StubPath    string `json:"stub_path,omitempty"`
	LocalBytes  uint64 `json:"local_bytes,omitempty"`
	OffloadedAt int64  `json:"offloaded_at,omitempty"`
	UsedAt      int64  `json:"used_at,omitempty"`
	Group       string `json:"group,omitempty"`
	Restorable  bool   `json:"restorable"`

	restore restoreRef
}

type offloadMore struct {
	Count int    `json:"count"`
	Bytes uint64 `json:"bytes"`
}

type offloadInventoryResult struct {
	State       string        `json:"state"`
	Detail      string        `json:"detail,omitempty"`
	Tool        *offloadTool  `json:"tool,omitempty"`
	At          int64         `json:"at"`
	TotalBytes  uint64        `json:"total_bytes"`
	StoredBytes uint64        `json:"stored_bytes,omitempty"`
	Partial     bool          `json:"partial,omitempty"`
	Items       []offloadItem `json:"items"`
	More        *offloadMore  `json:"more,omitempty"`
}

// offloadState is the point's memory: recent inventories and the jobs started through it.
type offloadState struct {
	mu   sync.Mutex
	inv  map[string]invCacheEntry // by machine id + "|" + locale
	all  map[string]map[string]offloadItem
	jobs map[string]*offloadJob
}

type invCacheEntry struct {
	at  time.Time
	res offloadInventoryResult
}

// offload is the handler of the "offload" namespace.
func (a *app) offload(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p offloadParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &ext.Error{Code: "invalid_params", Message: err.Error()}
		}
	}
	var res any
	var err error
	switch method {
	case "inventory":
		res, err = a.offloadInventory(ctx, p)
	case "run":
		res, err = a.offloadRun(ctx, p)
	case "job":
		res, err = a.offloadJobStatus(p)
	case "cancel":
		return a.offloadCancel(p)
	default:
		return nil, ext.UnknownMethod(offloadNamespace + "." + method)
	}
	if err != nil {
		return nil, err
	}
	return fitOffloadSchema(res), nil
}

// Free-text bounds of Persona's offload schema (offload.schema.json). Persona holds every answer to it and refuses
// the whole answer when one string is too long, so a long session title or a CLI's error would empty the cloud side
// or leave a job's progress unreadable.
var (
	offloadTextMost = map[string]int{"detail": 300, "error": 300, "reason": 300, "name": 200, "group": 200}
	offloadToolMost = map[string]int{"name": 80, "title": 80, "version": 40, "source": 300, "detail": 300}
)

// fitOffloadSchema is v as JSON with its free-text fields clipped to offloadTextMost (the answer's, its items') and
// its tool's to offloadToolMost. Ids are never clipped: they are what a restore names.
func fitOffloadSchema(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return v
	}
	clipFields(m, offloadTextMost)
	if tool, ok := m["tool"].(map[string]any); ok {
		clipFields(tool, offloadToolMost)
	}
	if items, ok := m["items"].([]any); ok {
		for _, x := range items {
			if it, ok := x.(map[string]any); ok {
				clipFields(it, offloadTextMost)
			}
		}
	}
	return m
}

// clipFields shortens m's string fields named in most to that many characters, ending a clipped one with "…".
func clipFields(m map[string]any, most map[string]int) {
	for k, n := range most {
		s, ok := m[k].(string)
		if !ok {
			continue
		}
		if r := []rune(s); len(r) > n {
			m[k] = strings.TrimRight(string(r[:n-1]), " ") + "…"
		}
	}
}

// offloadMachine finds the extension's machine for a Persona machine: by id, by name, and the hub's own computer by
// its host name (Persona names a computer after its host name; this extension calls it "local").
func offloadMachine(st *State, ref personaMachine) *Machine {
	for _, c := range []string{ref.ID, sanitizeID(ref.ID), ref.Name} {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if m := st.machine(c); m != nil {
			return m
		}
	}
	short := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(s, ".local")
		if i := strings.IndexByte(s, '.'); i > 0 {
			s = s[:i]
		}
		return s
	}
	want := map[string]bool{}
	for _, s := range []string{ref.ID, ref.Name} {
		if s = short(s); s != "" {
			want[s] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	for _, m := range st.Machines {
		if m.Hostname != "" && want[short(m.Hostname)] {
			return m
		}
	}
	if hn, err := offloadHostname(); err == nil && want[short(hn)] {
		for _, m := range st.Machines {
			if m.IsLocal {
				return m
			}
		}
	}
	return nil
}

func personaLabel(ref personaMachine) string { return cmp.Or(strings.TrimSpace(ref.Name), ref.ID) }

// target is what a call needs to know about its machine, read under the store lock.
type offloadTarget struct {
	m     *Machine
	cat   *ColdCatalog
	label string
}

func (a *app) offloadTarget(ref personaMachine) (*offloadTarget, error) {
	var t *offloadTarget
	err := a.db().read(func(st *State) {
		m := offloadMachine(st, ref)
		if m == nil {
			return
		}
		mc := *m
		t = &offloadTarget{m: &mc, label: cmp.Or(strings.TrimSpace(ref.Name), machineLabel(m))}
		if c := st.cold(m.ID); c != nil {
			t.cat = cloneCold(c)
		}
	})
	return t, err
}

// unavailableReason is why a machine cannot be asked at all, or "".
func unavailableReason(loc string, ref personaMachine, t *offloadTarget) string {
	switch {
	case t == nil:
		return otr(loc, "unknown_machine", personaLabel(ref))
	case strings.EqualFold(ref.OS, "windows"):
		return otr(loc, "windows")
	case ref.Online != nil && !*ref.Online:
		return otr(loc, "offline", t.label)
	}
	return ""
}

// ---- Probing the tool ----

// toolProbe is what the inventory script found about the CLI and the machine.
type toolProbe struct {
	noCLI     bool
	offloadOK bool
	cli       ColdCLI
	platform  string
	brew      string
	home      string
}

const probeHeadScript = `export LC_ALL=C
echo "@@SECTION:INV_UNAME"
uname -sm 2>/dev/null || true
echo "@@SECTION:INV_HOME"
printf '%s\n' "$HOME"
` + findBrewScript + `
echo "@@SECTION:INV_BREW"
printf '%s\n' "$BREW"
` + resolveCLIScript + `
if [ -z "$DKL" ]; then echo "@@SECTION:INV_NOCLI"; echo "@@SECTION:END"; exit 0; fi
` + cliInfoScript + `echo "@@SECTION:INV_PROBE"
"$DKL" --json offload 2>&1 | tr '\n' ' '; echo
`

// probeScript only asks about the tool.
func probeScript() string { return probeHeadScript + "echo \"@@SECTION:END\"\n" }

// stateDirScript sets $SD to disklessly's state folder (DISKLESSLY_STATE_DIR, else the platform default, else the
// folder of an install made before the rename).
const stateDirScript = `SD="${DISKLESSLY_STATE_DIR:-}"
if [ -z "$SD" ]; then
  if [ "$(uname -s)" = Darwin ]; then
    SD="$HOME/Library/Application Support/Disklessly"
    if [ ! -e "$SD" ] && [ -e "$HOME/Library/Application Support/256isenough" ]; then SD="$HOME/Library/Application Support/256isenough"; fi
  else
    case "$XDG_DATA_HOME" in /*) SD="$XDG_DATA_HOME/disklessly" ;; *) SD="$HOME/.local/share/disklessly" ;; esac
  fi
fi
`

// inventoryScript asks the CLI for packed apps and app data offloads, and reads its registries of offloaded files
// and folders (with each placeholder still on disk) and linked bundles. Image manifests are read from a repo that
// is a folder on the machine, for each app's size and original path.
func inventoryScript() string {
	return probeHeadScript + `echo "@@SECTION:INV_STATUS"
ST=$("$DKL" --json status 2>/dev/null | tr '\n' ' ')
printf '%s\n' "$ST"
echo "@@SECTION:INV_LIST"
"$DKL" --json list 2>/dev/null | tr '\n' ' '; echo
if [ "$(uname -s)" = Darwin ]; then
  echo "@@SECTION:INV_DATA"
  "$DKL" --json data status 2>/dev/null | tr '\n' ' '; echo
fi
` + stateDirScript + `echo "@@SECTION:INV_FILES"
cat "$SD/files/offloaded.json" 2>/dev/null | tr '\n' ' '; echo
echo "@@SECTION:INV_LINKED"
cat "$SD/linked-bundles.json" 2>/dev/null | tr '\n' ' '; echo
echo "@@SECTION:INV_PH"
grep -o '"placeholderPath" *: *"[^"]*"' "$SD/files/offloaded.json" 2>/dev/null | sed 's/^"placeholderPath" *: *"//; s/"$//; s#\\/#/#g' | head -n 5000 | while IFS= read -r p; do
  [ -f "$p" ] || continue
  printf '%s\t%s\n' "$p" "$(head -c 16384 "$p" | tr '\n\t' '  ')"
done
REPO=$(printf '%s' "$ST" | sed -n 's/.*"repoPath" *: *"\([^"]*\)".*/\1/p' | sed 's#\\/#/#g')
echo "@@SECTION:INV_MANIFESTS"
if [ -n "$REPO" ] && [ -d "$REPO/manifests" ]; then
  for f in "$REPO"/manifests/*.json; do
    [ -f "$f" ] || continue
    printf '%s\n' "$(head -c 262144 "$f" | tr '\n\t' '  ')"
  done | head -n 2000
fi
echo "@@SECTION:END"
`
}

func parseProbe(sections map[string][]string) toolProbe {
	var p toolProbe
	p.platform = parsePlatformString(firstNonEmpty(sections["INV_UNAME"]))
	p.home = strings.TrimSpace(firstNonEmpty(sections["INV_HOME"]))
	for _, l := range sections["INV_BREW"] {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "/") {
			p.brew = l
			break
		}
	}
	if _, ok := sections["INV_NOCLI"]; ok {
		p.noCLI = true
		return p
	}
	p.cli = parseCLIInfo(sections["COLD_CLI"])
	// `disklessly offload` without a path answers its own usage line; a CLI from before files offload does not
	// know the command.
	probe := strings.ToLower(strings.Join(sections["INV_PROBE"], " "))
	p.offloadOK = !strings.Contains(probe, "unknown command")
	return p
}

// probeMachine runs the probe script.
func probeMachine(ctx context.Context, runner Runner, m *Machine) (toolProbe, error) {
	out, err := runner(ctx, m, probeScript(), offloadProbeTimeout)
	if err != nil {
		return toolProbe{}, err
	}
	return parseProbe(splitSections(out)), nil
}

// installRoute is how the CLI would be installed on a machine: Homebrew there, else a copy from another machine.
type installRoute struct {
	brew  bool
	offer installOffer
}

func (r installRoute) ok() bool { return r.brew || r.offer.src != nil }

func (a *app) installRouteFor(m *Machine, pr toolProbe) installRoute {
	r := installRoute{brew: pr.brew != ""}
	if r.brew {
		return r
	}
	_ = a.db().read(func(st *State) {
		target := *m
		target.Platform = cmp.Or(pr.platform, target.Platform)
		r.offer = installSourceFor(st, &target)
	})
	return r
}

// toolFor describes the CLI on a machine for the caller, with one sentence on how it would be installed.
func toolFor(loc, label string, pr toolProbe, r installRoute) offloadTool {
	t := offloadTool{Name: "disklessly", Title: otr(loc, "tool_title"), Version: shortVersion(pr.cli.Version), Installed: !pr.noCLI && pr.offloadOK}
	switch {
	case r.brew:
		t.Source = otr(loc, "source_brew", label)
	case r.offer.src != nil:
		t.Source = otr(loc, "source_copy", cmp.Or(shortVersion(r.offer.cli.Version), "?"), machineLabel(r.offer.src), path.Base(r.offer.cli.Home), label)
	default:
		t.Source = otr(loc, "source_none", label)
	}
	if !pr.noCLI && !pr.offloadOK {
		t.Detail = otr(loc, "cli_old", cmp.Or(t.Version, "?"), label)
	}
	return t
}

// ---- Inventory ----

func (a *app) offloadInventory(ctx context.Context, p offloadParams) (any, error) {
	loc := offloadLocale(p.Locale)
	now := a.now()
	res := offloadInventoryResult{At: now.Unix(), Items: []offloadItem{}}
	t, err := a.offloadTarget(p.Machine)
	if err != nil {
		return nil, err
	}
	if why := unavailableReason(loc, p.Machine, t); why != "" {
		res.State, res.Detail = invUnavailable, why
		return res, nil
	}
	key := t.m.ID + "|" + loc
	if !p.Refresh {
		a.offloadMu().Lock()
		e, ok := a.offloadSt().inv[key]
		a.offloadMu().Unlock()
		if ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < offloadInventoryTTL {
			return e.res, nil
		}
	}
	res, all := a.gatherInventory(ctx, t, loc, now)
	if res.State != invUnavailable {
		s := a.offloadSt()
		a.offloadMu().Lock()
		if s.inv == nil {
			s.inv = map[string]invCacheEntry{}
			s.all = map[string]map[string]offloadItem{}
		}
		s.inv[key] = invCacheEntry{at: now, res: res}
		s.all[t.m.ID] = all
		a.offloadMu().Unlock()
	}
	return res, nil
}

func (a *app) offloadSt() *offloadState { return &a.offloadData }
func (a *app) offloadMu() *sync.Mutex   { return &a.offloadData.mu }
func (a *app) forgetInventory(id string) {
	a.offloadMu().Lock()
	a.dropInventoryLocked(id)
	a.offloadMu().Unlock()
}

func (a *app) dropInventoryLocked(id string) {
	for k := range a.offloadData.inv {
		if strings.HasPrefix(k, id+"|") {
			delete(a.offloadData.inv, k)
		}
	}
	delete(a.offloadData.all, id)
}

// gatherInventory asks machine t what is offloaded. all holds every row (not only the first 500) by id, with how
// to restore it.
func (a *app) gatherInventory(ctx context.Context, t *offloadTarget, loc string, now time.Time) (offloadInventoryResult, map[string]offloadItem) {
	res := offloadInventoryResult{At: now.Unix(), Items: []offloadItem{}}
	out, err := a.run()(ctx, t.m, inventoryScript(), offloadProbeTimeout)
	sections := splitSections(out)
	if err != nil {
		if _, started := sections["INV_PROBE"]; !started {
			res.State, res.Detail = invUnavailable, otr(loc, "unreachable", t.label, firstLine([]string{err.Error()}))
			return res, nil
		}
		// The CLI answered part of it: use what came back.
		res.Partial = true
	}
	pr := parseProbe(sections)
	if pr.noCLI || !pr.offloadOK {
		tool := toolFor(loc, t.label, pr, a.installRouteFor(t.m, pr))
		res.State, res.Tool = invNeedsInstall, &tool
		res.Detail = cmp.Or(tool.Detail, otr(loc, "no_cli", t.label))
		return res, nil
	}
	tool := toolFor(loc, t.label, pr, installRoute{})
	tool.Source = ""
	res.Tool = &tool
	items, partial := parseInventory(sections, pr, t.cat, now)
	res.Partial = res.Partial || partial
	if ce := cliFailure(sections["INV_LIST"]); ce != nil && noBackendError(ce) && len(items) == 0 {
		res.State, res.Detail = invNeedsSetup, otr(loc, "no_backend", t.label)
		return res, nil
	}
	if t.cat != nil && t.cat.stateCode() == ColdNoBackend && len(items) == 0 {
		res.State, res.Detail = invNeedsSetup, otr(loc, "no_backend", t.label)
		return res, nil
	}
	res.State = invReady
	if res.Partial {
		res.Detail = otr(loc, "partial")
	}
	all := make(map[string]offloadItem, len(items))
	for _, it := range items {
		all[it.ID] = it
		res.TotalBytes += it.Bytes
		res.StoredBytes += it.StoredBytes
	}
	slices.SortStableFunc(items, func(x, y offloadItem) int {
		if x.Bytes != y.Bytes {
			return cmp.Compare(y.Bytes, x.Bytes)
		}
		return strings.Compare(x.ID, y.ID)
	})
	if len(items) > offloadMaxItems {
		more := &offloadMore{Count: len(items) - offloadMaxItems}
		for _, it := range items[offloadMaxItems:] {
			more.Bytes += it.Bytes
		}
		res.More = more
		items = items[:offloadMaxItems]
	}
	if items != nil {
		res.Items = items // nil would be "items": null, which Persona's offload schema refuses
	}
	return res, all
}

// What the CLI and its registries say; field names are the CLI's.
type invManifest struct {
	ImageID   string `json:"imageId"`
	BundleID  string `json:"bundleId"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	CreatedAt string `json:"createdAt"`
	Image     struct {
		Size       uint64 `json:"size"`
		Encryption *struct {
			PlainSize uint64 `json:"plainSize"`
		} `json:"encryption"`
	} `json:"image"`
	App struct {
		OriginalPath string `json:"originalPath"`
	} `json:"app"`
}

type invStatus struct {
	RepoPath string `json:"repoPath"`
	Apps     map[string]struct {
		ImageID  string          `json:"imageId"`
		State    string          `json:"state"`
		LastUsed json.RawMessage `json:"lastUsed"`
	} `json:"apps"`
	Cache map[string]struct {
		LastUsed json.RawMessage `json:"lastUsed"`
		Bytes    uint64          `json:"bytes"`
	} `json:"cache"`
	Restored map[string]json.RawMessage `json:"restored"`
}

type invListRow struct {
	ImageID  string `json:"imageId"`
	BundleID string `json:"bundleId"`
	Name     string `json:"name"`
}

type invData struct {
	Apps []struct {
		App        string   `json:"app"`
		BundleID   string   `json:"bundleId"`
		State      string   `json:"state"`
		Roots      []string `json:"roots"`
		Bytes      uint64   `json:"bytes"`
		LocalBytes uint64   `json:"localBytes"`
	} `json:"apps"`
}

type invFileRecord struct {
	ID              string `json:"id"`
	PlaceholderPath string `json:"placeholderPath"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Size            uint64 `json:"size"`
	CreatedAt       string `json:"createdAt"`
}

type invPlaceholder struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Size         uint64 `json:"size"`
	StoredSize   uint64 `json:"storedSize"`
	OriginalPath string `json:"originalPath"`
	CreatedAt    string `json:"createdAt"`
	ModifiedAt   string `json:"modifiedAt"`
}

type invLinked struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	AppPath string   `json:"appPath"`
	Paths   []string `json:"paths"`
}

// jsonSection decodes the JSON a section holds (any shape) into v.
func jsonSection(lines []string, v any) bool {
	raw := strings.TrimSpace(strings.Join(lines, " "))
	if raw == "" || (raw[0] != '{' && raw[0] != '[') {
		return false
	}
	return json.Unmarshal([]byte(raw), v) == nil
}

// unixOf reads a CLI time (RFC 3339 text or seconds since 1970); 0 when there is none.
func unixOf(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		// Foundation's default Date encoding counts from 2001.
		if f < 1e9 {
			f += 978307200
		}
		if f < 0 {
			return 0
		}
		return int64(f)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return unixText(str)
	}
	return 0
}

func unixText(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		if u := t.Unix(); u > 0 {
			return u
		}
	}
	return 0
}

// parseInventory turns the inventory script's sections and the cold index into rows.
func parseInventory(sections map[string][]string, pr toolProbe, cat *ColdCatalog, now time.Time) (items []offloadItem, partial bool) {
	var linked []invLinked
	jsonSection(sections["INV_LINKED"], &linked)
	linkedOf := map[string]string{}
	for _, b := range linked {
		name := cmp.Or(b.Name, strings.TrimSuffix(path.Base(b.AppPath), ".app"))
		linkedOf[b.AppPath] = name
		for _, p := range b.Paths {
			linkedOf[p] = name
		}
	}

	// Apps this computer offloaded: the state's apps not brought back, with their manifest.
	var status invStatus
	statusOK := jsonSection(sections["INV_STATUS"], &status)
	var list []invListRow
	jsonSection(sections["INV_LIST"], &list)
	manifests := map[string]invManifest{}
	for _, l := range sections["INV_MANIFESTS"] {
		var mf invManifest
		if json.Unmarshal([]byte(strings.TrimSpace(l)), &mf) == nil && mf.ImageID != "" {
			manifests[mf.ImageID] = mf
		}
	}
	listed := map[string]invListRow{}
	for _, r := range list {
		listed[r.ImageID] = r
	}
	if statusOK {
		ids := make([]string, 0, len(status.Apps))
		for id := range status.Apps {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, bundle := range ids {
			st := status.Apps[bundle]
			if st.ImageID == "" || st.State == "restored" {
				continue
			}
			if _, back := status.Restored[bundle]; back {
				continue
			}
			mf, haveManifest := manifests[st.ImageID]
			row, haveRow := listed[st.ImageID]
			if !haveManifest && !haveRow {
				continue
			}
			it := offloadItem{
				ID: "app:" + bundle, Kind: "app", Name: cmp.Or(mf.Name, row.Name, bundle), Restorable: true,
				restore: restoreRef{how: "unpack", arg: bundle},
			}
			if haveManifest {
				it.StoredBytes = mf.Image.Size
				it.Bytes = mf.Image.Size
				if mf.Image.Encryption != nil && mf.Image.Encryption.PlainSize > 0 {
					it.Bytes = mf.Image.Encryption.PlainSize
				}
				it.OriginPath = mf.App.OriginalPath
				it.StubPath = mf.App.OriginalPath
				it.OffloadedAt = unixText(mf.CreatedAt)
			} else {
				// The repo is not a folder here (a network backend): the size is unknown.
				partial = true
			}
			if c, ok := status.Cache[st.ImageID]; ok {
				it.LocalBytes = c.Bytes
				it.UsedAt = unixOf(c.LastUsed)
			}
			if u := unixOf(st.LastUsed); u > 0 {
				it.UsedAt = u
			}
			if g, ok := linkedOf[it.OriginPath]; ok && it.OriginPath != "" {
				it.Group = g
			}
			items = append(items, it)
		}
	}

	// App data offloads (Application Support moved while the app is closed).
	var data invData
	if jsonSection(sections["INV_DATA"], &data) {
		for _, d := range data.Apps {
			if d.State != "offloaded" || d.BundleID == "" {
				continue
			}
			it := offloadItem{
				ID: "data:" + d.BundleID, Kind: "app_data", Name: cmp.Or(d.App, d.BundleID), Bytes: d.Bytes,
				LocalBytes: d.LocalBytes, Group: cmp.Or(d.App, d.BundleID), Restorable: true,
				restore: restoreRef{how: "data", arg: d.BundleID},
			}
			if len(d.Roots) == 1 {
				root := d.Roots[0]
				if !strings.HasPrefix(root, "/") && pr.home != "" {
					root = pr.home + "/Library/Application Support/" + root
				}
				it.OriginPath = root
			}
			items = append(items, it)
		}
	}

	// Files and folders whose placeholder is still on disk; those of a linked bundle are bundle rows.
	placeholders := map[string]invPlaceholder{}
	for _, l := range sections["INV_PH"] {
		p, body, ok := strings.Cut(l, "\t")
		if !ok || p == "" {
			continue
		}
		var ph invPlaceholder
		_ = json.Unmarshal([]byte(strings.TrimSpace(body)), &ph)
		placeholders[p] = ph
	}
	var records []invFileRecord
	jsonSection(sections["INV_FILES"], &records)
	seen := map[string]bool{}
	for _, r := range records {
		ph, live := placeholders[r.PlaceholderPath]
		if !live || seen[r.PlaceholderPath] {
			continue
		}
		seen[r.PlaceholderPath] = true
		origin := cmp.Or(ph.OriginalPath, strings.TrimSuffix(r.PlaceholderPath, ".disklessly"))
		kind := "file"
		if cmp.Or(r.Kind, ph.Kind) == "folder" {
			kind = "dir"
		}
		it := offloadItem{
			ID: "file:" + cmp.Or(r.ID, r.PlaceholderPath), Kind: kind, Name: cmp.Or(r.Name, ph.Name, path.Base(origin)),
			Bytes: cmp.Or(r.Size, ph.Size), StoredBytes: ph.StoredSize, OriginPath: origin, StubPath: r.PlaceholderPath,
			OffloadedAt: unixText(cmp.Or(r.CreatedAt, ph.CreatedAt)), UsedAt: unixText(ph.ModifiedAt), Restorable: true,
			restore: restoreRef{how: "fetch", arg: r.PlaceholderPath},
		}
		if g, ok := linkedOf[origin]; ok {
			it.Kind, it.Group = "bundle", g
		}
		items = append(items, it)
	}

	// Archived sessions, from the cold index this extension keeps (refreshed by its own jobs).
	if cat != nil {
		if cat.Capped {
			partial = true
		}
		for _, e := range cat.Entries {
			if !e.Local || (e.State != "archived" && e.State != "remote") {
				continue
			}
			items = append(items, offloadItem{
				ID: "session:" + e.ID, Kind: "session", Name: cmp.Or(e.Title, e.SessionID, e.ID), Bytes: e.Bytes,
				OriginPath: e.Path, OffloadedAt: unixText(e.ArchivedAt), UsedAt: unixText(e.Mtime),
				Group: toolLabel(e.Tool), Restorable: true, restore: restoreRef{how: "session", arg: e.ID},
			})
		}
	}
	return items, partial
}

// ---- Checking paths ----

// pathCheck is the outcome for one path of a run.
type pathCheck struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Bytes  uint64 `json:"bytes,omitempty"`
	kind   string
}

// checkScript stats each path (index i on line @@CHK i ...) without walking it, and reports disklessly's own
// folders (@@OWN), so the policy can be checked here.
func checkScript(paths []string) string {
	var b strings.Builder
	b.WriteString(`export LC_ALL=C
printf '@@HOME\t%s\n' "$HOME"
` + stateDirScript + `printf '@@OWN\t%s\n' "$SD"
if [ "$(uname -s)" = Darwin ]; then printf '@@OWN\t%s\n' "$HOME/Library/Caches/Disklessly"; else
  case "$XDG_CACHE_HOME" in /*) printf '@@OWN\t%s\n' "$XDG_CACHE_HOME/disklessly" ;; *) printf '@@OWN\t%s\n' "$HOME/.cache/disklessly" ;; esac
fi
if [ -n "$DISKLESSLY_CACHE_DIR" ]; then printf '@@OWN\t%s\n' "$DISKLESSLY_CACHE_DIR"; fi
if [ -n "$DISKLESSLY_REPO_PATH" ]; then printf '@@OWN\t%s\n' "$DISKLESSLY_REPO_PATH"; fi
chk() {
  if [ -L "$2" ]; then printf '@@CHK\t%s\tsymlink\n' "$1"; return; fi
  if [ ! -e "$2" ]; then printf '@@CHK\t%s\tnot_found\n' "$1"; return; fi
  if [ -d "$2" ]; then k=dir; elif [ -f "$2" ]; then k=file; else printf '@@CHK\t%s\tnot_regular\n' "$1"; return; fi
  s=-; if [ -e "$2/Contents/Resources/stub.json" ]; then s=stub; fi
  t=-; if [ -e "$2.disklessly" ]; then t=taken; fi
  printf '@@CHK\t%s\tok\t%s\t%s\t%s\n' "$1" "$k" "$s" "$t"
}
`)
	for i, p := range paths {
		fmt.Fprintf(&b, "chk %d %s\n", i, shellQuote(p))
	}
	return b.String()
}

// sizeScript sums what each path holds on disk, in KiB (@@SIZE i kb).
func sizeScript(paths []string) string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	for i, p := range paths {
		fmt.Fprintf(&b, "printf '@@SIZE\\t%d\\t%%s\\n' \"$(du -sk %s 2>/dev/null | awk 'NR==1{print $1}')\"\n", i, shellQuote(p))
	}
	return b.String()
}

// offloadPolicy is disklessly's FileOffloadPolicy.refusal (DisklesslyCore/FileOffload.swift) for a file or folder,
// plus keys and credentials, which Persona refuses as well: the code of the refusal, or "".
func offloadPolicy(p, home string, own []string) string {
	if strings.HasSuffix(p, ".disklessly") {
		return "placeholder"
	}
	if home == "" || !strings.HasPrefix(p, home+"/") {
		if p == home {
			return "home_root"
		}
		return "outside_home"
	}
	rest := strings.Split(strings.TrimPrefix(p, home+"/"), "/")
	top := rest[0]
	switch top {
	case "Library", "Applications", ".Trash", "Public":
		return "app_managed"
	case ".ssh", ".gnupg", ".aws", ".kube", ".password-store", ".docker":
		return "credentials"
	}
	if len(rest) == 1 && slices.Contains([]string{"Desktop", "Documents", "Downloads", "Movies", "Music", "Pictures"}, top) {
		return "standard_folder"
	}
	for _, c := range rest[:len(rest)-1] {
		if strings.EqualFold(path.Ext(c), ".app") {
			return "inside_app"
		}
	}
	switch strings.ToLower(strings.TrimPrefix(path.Ext(rest[len(rest)-1]), ".")) {
	case "photoslibrary", "musiclibrary", "tvlibrary", "imovielibrary", "photolibrary", "aplibrary", "migratedphotolibrary",
		"vmwarevm", "pvm", "utm", "vbox", "vdi", "vmdk", "qcow2", "hdd", "hds", "sparsebundle", "sparseimage",
		"icloud", "download", "crdownload", "part", "partial":
		return "app_managed"
	}
	if slices.Contains([]string{"Docker.raw", "Docker.qcow2", "data.img", "disk.img", ".DS_Store"}, rest[len(rest)-1]) {
		return "app_managed"
	}
	for _, o := range own {
		if o != "" && (p == o || strings.HasPrefix(p, o+"/") || strings.HasPrefix(o, p+"/")) {
			return "own_data"
		}
	}
	return ""
}

// appOffloadPolicy enforces the same sensitive-location redlines as offloadPolicy for macOS .app bundles while still
// allowing top-level apps under /Applications and $HOME/Applications.
func appOffloadPolicy(p, home string, own []string) string {
	inApps := strings.HasPrefix(p, "/Applications/")
	inHome := home != "" && strings.HasPrefix(p, home+"/")
	if !inApps && !inHome {
		return "outside_home"
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for _, c := range parts[:len(parts)-1] {
		if strings.EqualFold(path.Ext(c), ".app") {
			return "inside_app"
		}
	}
	if inHome {
		rest := strings.Split(strings.TrimPrefix(p, home+"/"), "/")
		switch rest[0] {
		case "Library", ".Trash", "Public":
			return "app_managed"
		case ".ssh", ".gnupg", ".aws", ".kube", ".password-store", ".docker":
			return "credentials"
		}
	}
	for _, o := range own {
		if o != "" && (p == o || strings.HasPrefix(p, o+"/") || strings.HasPrefix(o, p+"/")) {
			return "own_data"
		}
	}
	return ""
}

// validPath reports an absolute, clean path a shell line can carry.
func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "/" && !strings.ContainsAny(p, "\x00\r\n")
}

// checkOffloadPaths says, for each path, whether disklessly would take it: the path's shape here, its kind and
// state on the machine, and disklessly's own policy.
func checkOffloadPaths(ctx context.Context, runner Runner, m *Machine, platform string, kinds []string, paths []string, loc string) ([]pathCheck, error) {
	out := make([]pathCheck, len(paths))
	var ask []string
	idx := []int{}
	for i, p := range paths {
		out[i].Path = p
		if !validPath(p) {
			out[i].Code = "bad_path"
			continue
		}
		ask = append(ask, p)
		idx = append(idx, i)
	}
	if len(ask) > 0 {
		raw, err := runner(ctx, m, checkScript(ask), offloadProbeTimeout)
		if err != nil {
			return nil, err
		}
		home := ""
		var own []string
		for _, line := range strings.Split(raw, "\n") {
			f := strings.Split(strings.TrimRight(line, "\r"), "\t")
			switch f[0] {
			case "@@HOME":
				if len(f) > 1 {
					home = strings.TrimRight(f[1], "/")
				}
			case "@@OWN":
				if len(f) > 1 && strings.HasPrefix(f[1], "/") {
					own = append(own, strings.TrimRight(f[1], "/"))
				}
			case "@@CHK":
				if len(f) < 3 {
					continue
				}
				j, err := strconv.Atoi(f[1])
				if err != nil || j < 0 || j >= len(idx) {
					continue
				}
				c := &out[idx[j]]
				if f[2] != "ok" || len(f) < 6 {
					c.Code = f[2]
					continue
				}
				c.kind = f[3]
				stub, taken := f[4] == "stub", f[5] == "taken"
				isApp := c.kind == "dir" && strings.EqualFold(path.Ext(c.Path), ".app")
				switch {
				case isApp:
					c.kind = "app"
					switch {
					case !strings.HasPrefix(platform, "darwin"):
						c.Code = "app_platform"
					case stub:
						c.Code = "already_offloaded"
					default:
						c.Code = appOffloadPolicy(c.Path, home, own)
					}
				case taken:
					c.Code = "placeholder_taken"
				default:
					c.Code = offloadPolicy(c.Path, home, own)
				}
				if c.Code == "" && !slices.Contains(kinds, c.kind) {
					c.Code = "kind"
				}
			}
		}
		for _, i := range idx {
			if out[i].kind == "" && out[i].Code == "" {
				out[i].Code = "not_found"
			}
		}
	}
	for i := range out {
		out[i].OK = out[i].Code == ""
		if !out[i].OK {
			out[i].Reason = refusalText(loc, out[i].Code, "")
		}
	}
	return out, nil
}

// ---- Running ----

// offloadKinds are what this provider offloads (persona-extension.json contributes.disk_offload).
var offloadKinds = []string{"file", "dir", "app"}

func (a *app) offloadRun(ctx context.Context, p offloadParams) (any, error) {
	loc := offloadLocale(p.Locale)
	if p.Action != "offload" && p.Action != "restore" {
		return nil, &ext.Error{Code: "invalid_params", Message: otr(loc, "bad_action")}
	}
	n := len(p.Paths)
	if p.Action == "restore" {
		n = len(p.Items)
	}
	if n == 0 || n > offloadMaxRun {
		return nil, &ext.Error{Code: "invalid_params", Message: fmt.Sprintf("1 to %d %s", offloadMaxRun, map[bool]string{true: "paths", false: "items"}[p.Action == "offload"])}
	}
	t, err := a.offloadTarget(p.Machine)
	if err != nil {
		return nil, err
	}
	if why := unavailableReason(loc, p.Machine, t); why != "" {
		return nil, &ext.Error{Code: "unavailable", Message: why}
	}
	runner := a.run()
	pr, err := probeMachine(ctx, runner, t.m)
	if err != nil {
		return nil, &ext.Error{Code: "unavailable", Message: otr(loc, "unreachable", t.label, firstLine([]string{err.Error()}))}
	}
	route := a.installRouteFor(t.m, pr)
	tool := toolFor(loc, t.label, pr, route)
	needInstall := !tool.Installed
	if p.Action == "offload" {
		return a.runOffload(ctx, p, loc, t, pr, route, tool, needInstall)
	}
	return a.runRestore(ctx, p, loc, t, route, tool, needInstall)
}

// refusedItem is one refused path or item of offload.run's answer. Persona holds the answer to its offload schema
// (OffloadRunItem has no room for other fields), so the machine code stays here and only the sentence is sent.
type refusedItem struct {
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Code   string `json:"-"`
	Reason string `json:"reason"`
}

// runItem is one path of a dry run's answer as Persona's offload schema has it (OffloadRunItem): the verdict, the
// sentence when refused and the size when known, without pathCheck's machine code.
type runItem struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Bytes  uint64 `json:"bytes,omitempty"`
}

// runItems is checks as a dry run answers them.
func runItems(checks []pathCheck) []runItem {
	out := make([]runItem, len(checks))
	for i, c := range checks {
		out[i] = runItem{Path: c.Path, OK: c.OK, Reason: c.Reason, Bytes: c.Bytes}
	}
	return out
}

func (a *app) runOffload(ctx context.Context, p offloadParams, loc string, t *offloadTarget, pr toolProbe, route installRoute, tool offloadTool, needInstall bool) (any, error) {
	platform := pr.platform
	if platform == "" && strings.EqualFold(p.Machine.OS, "darwin") {
		platform = "darwin"
	}
	checks, err := checkOffloadPaths(ctx, a.run(), t.m, platform, offloadKinds, p.Paths, loc)
	if err != nil {
		return nil, &ext.Error{Code: "unavailable", Message: otr(loc, "unreachable", t.label, firstLine([]string{err.Error()}))}
	}
	if p.DryRun {
		return map[string]any{"status": "ok", "items": runItems(checks), "tool": tool}, nil
	}
	if needInstall && !p.Install {
		return map[string]any{"status": "needs_install", "tool": tool}, nil
	}
	var refused []refusedItem
	var work []offloadJobItem
	for _, c := range checks {
		if !c.OK {
			refused = append(refused, refusedItem{Path: c.Path, Code: c.Code, Reason: c.Reason})
			continue
		}
		work = append(work, offloadJobItem{Path: c.Path, Status: "pending", kind: c.kind, name: path.Base(c.Path)})
	}
	if needInstall && !route.ok() {
		for _, w := range work {
			refused = append(refused, refusedItem{Path: w.Path, Code: "no_install_route", Reason: otr(loc, "no_install_route", t.label)})
		}
		work = nil
	}
	if len(work) == 0 {
		return map[string]any{"status": "refused", "items": refused}, nil
	}
	for _, r := range refused {
		work = append(work, offloadJobItem{Path: r.Path, Status: "skipped", Error: r.Reason})
	}
	return a.startOffloadJob(p, loc, t, route, needInstall, "offload", work)
}

func (a *app) runRestore(ctx context.Context, p offloadParams, loc string, t *offloadTarget, route installRoute, tool offloadTool, needInstall bool) (any, error) {
	all := a.knownItems(t.m.ID)
	if all == nil || slices.ContainsFunc(p.Items, func(id string) bool { _, ok := all[id]; return !ok }) {
		_, fresh := a.gatherInventory(ctx, t, loc, a.now())
		if fresh != nil {
			all = fresh
			a.offloadMu().Lock()
			if a.offloadData.all == nil {
				a.offloadData.all = map[string]map[string]offloadItem{}
			}
			a.offloadData.all[t.m.ID] = fresh
			a.offloadMu().Unlock()
		}
	}
	var refused []refusedItem
	var work []offloadJobItem
	var checks []map[string]any
	for _, id := range p.Items {
		it, ok := all[id]
		code := ""
		switch {
		case !ok:
			code = "unknown_item"
		case !it.Restorable:
			code = "not_restorable"
		}
		c := map[string]any{"id": id, "ok": code == ""}
		if ok {
			c["path"], c["bytes"] = it.OriginPath, it.Bytes
		}
		if code != "" {
			c["reason"] = refusalText(loc, code, "")
			refused = append(refused, refusedItem{ID: id, Code: code, Reason: refusalText(loc, code, "")})
		} else {
			work = append(work, offloadJobItem{ID: id, Path: it.OriginPath, Status: "pending", Bytes: it.Bytes, kind: it.Kind, name: it.Name, restore: it.restore})
		}
		checks = append(checks, c)
	}
	if p.DryRun {
		return map[string]any{"status": "ok", "items": checks, "tool": tool}, nil
	}
	if needInstall && !p.Install {
		return map[string]any{"status": "needs_install", "tool": tool}, nil
	}
	if needInstall && !route.ok() {
		for _, w := range work {
			refused = append(refused, refusedItem{ID: w.ID, Code: "no_install_route", Reason: otr(loc, "no_install_route", t.label)})
		}
		work = nil
	}
	if len(work) == 0 {
		return map[string]any{"status": "refused", "items": refused}, nil
	}
	for _, r := range refused {
		work = append(work, offloadJobItem{ID: r.ID, Status: "skipped", Error: r.Reason})
	}
	return a.startOffloadJob(p, loc, t, route, needInstall, "restore", work)
}

func (a *app) knownItems(id string) map[string]offloadItem {
	a.offloadMu().Lock()
	defer a.offloadMu().Unlock()
	return a.offloadData.all[id]
}

// ---- Jobs ----

type offloadJobItem struct {
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Bytes  uint64 `json:"bytes,omitempty"`

	kind    string
	name    string
	restore restoreRef
}

type offloadJob struct {
	id        string
	persona   string // the Persona machine id it was started for
	machine   string // this extension's machine id
	action    string
	loc       string
	created   time.Time
	finished  time.Time
	state     string
	phase     string
	phases    []string
	doneBytes uint64
	total     uint64
	freed     *uint64
	err       string
	items     []offloadJobItem
	cancel    bool
}

func newJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "off-" + hex.EncodeToString(b[:])
}

// jobDo runs f on job j under the point's lock.
func (a *app) jobDo(j *offloadJob, f func(j *offloadJob)) {
	a.offloadMu().Lock()
	defer a.offloadMu().Unlock()
	f(j)
}

func (a *app) setPhase(j *offloadJob, phase string) {
	a.jobDo(j, func(j *offloadJob) {
		if j.phase != phase {
			j.phase = phase
			j.phases = append(j.phases, phase)
		}
	})
}

// pruneJobsLocked drops jobs finished longer than offloadJobKeep ago.
func (a *app) pruneJobsLocked(now time.Time) {
	for id, j := range a.offloadData.jobs {
		if !j.finished.IsZero() && now.Sub(j.finished) > offloadJobKeep {
			delete(a.offloadData.jobs, id)
		}
	}
}

func (a *app) startOffloadJob(p offloadParams, loc string, t *offloadTarget, route installRoute, needInstall bool, action string, items []offloadJobItem) (any, error) {
	now := a.now()
	j := &offloadJob{id: newJobID(), persona: p.Machine.ID, machine: t.m.ID, action: action, loc: loc, created: now, state: ojRunning, items: items}
	for _, it := range items {
		if it.Status == "pending" {
			j.total += it.Bytes
		}
	}
	runner, streamer := a.run(), a.stream()
	label := map[string]string{"offload": "正在把选中的内容搬到网盘", "restore": "正在从网盘拿回选中的内容"}[action]
	kind := map[string]string{"offload": jobFileOffload, "restore": jobFileRestore}[action]
	a.offloadMu().Lock()
	if a.offloadData.jobs == nil {
		a.offloadData.jobs = map[string]*offloadJob{}
	}
	a.pruneJobsLocked(now)
	a.offloadData.jobs[j.id] = j
	a.offloadMu().Unlock()

	started, _, err := a.startJob(t.m.ID, kind, label, func(ctx context.Context, sc *scratch, now time.Time) (string, error) {
		return a.runOffloadJob(ctx, runner, streamer, sc, j, route, needInstall, now)
	})
	if err != nil || !started {
		a.offloadMu().Lock()
		delete(a.offloadData.jobs, j.id)
		a.offloadMu().Unlock()
		if err != nil {
			return nil, err
		}
		var refused []refusedItem
		for _, it := range items {
			refused = append(refused, refusedItem{Path: it.Path, ID: it.ID, Code: "busy", Reason: otr(loc, "busy", t.label)})
		}
		return map[string]any{"status": "refused", "items": refused}, nil
	}
	return map[string]any{"status": "started", "job": j.id}, nil
}

// availScript prints the free bytes of the volume holding the home folder (the data volume on macOS), as the
// cleaner measures it.
const availScript = `export LC_ALL=C
if [ -d "/System/Volumes/Data" ]; then
  A=$(df -kP /System/Volumes/Data 2>/dev/null | awk 'NR==2 { print $4 * 1024 }')
else
  A=$(df -kP "$HOME" 2>/dev/null | awk 'NR==2 { print $4 * 1024 }')
fi
echo "@@AVAIL ${A:-}"
`

func availBytes(ctx context.Context, runner Runner, m *Machine) (uint64, bool) {
	out, err := runner(ctx, m, availScript, 30*time.Second)
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "@@AVAIL "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// itemScript runs one CLI command for an item and reports its exit code, its JSON answer and whether the result
// is on disk (@@ITEM_VERIFIED: the placeholder or stub is in place, or the original is back).
func itemScript(args []string, verify string) string {
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = shellQuote(s)
	}
	return resolveCLIScript + `
if [ -z "$DKL" ]; then echo "@@ITEM_RC ` + strconv.Itoa(cliExitMissing) + `"; exit 0; fi
OUT=$("$DKL" --json ` + strings.Join(q, " ") + ` 2>/dev/null)
RC=$?
echo "@@ITEM_RC $RC"
printf '@@ITEM_JSON %s\n' "$(printf '%s' "$OUT" | tr '\n' ' ')"
if ` + verify + `; then echo "@@ITEM_VERIFIED"; fi
`
}

type itemOutcome struct {
	rc       int
	verified bool
	ok       bool
	msg      string
	bytes    uint64
}

func parseItem(out string) itemOutcome {
	o := itemOutcome{rc: -1}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "@@ITEM_RC "):
			o.rc, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "@@ITEM_RC ")))
		case strings.HasPrefix(line, "@@ITEM_JSON "):
			var resp struct {
				OK        bool      `json:"ok"`
				SizeBytes uint64    `json:"sizeBytes"`
				Error     *cliError `json:"error"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "@@ITEM_JSON ")), &resp) == nil {
				o.ok, o.bytes = resp.OK, resp.SizeBytes
				if resp.Error != nil {
					o.msg = cmp.Or(resp.Error.Message, resp.Error.Name)
				}
			}
		case line == "@@ITEM_VERIFIED":
			o.verified = true
		}
	}
	return o
}

// offloadItemScript is the command for offloading one checked path.
func offloadItemScript(it offloadJobItem) string {
	p := shellQuote(it.Path)
	if it.kind == "app" {
		return itemScript([]string{"pack", it.Path}, `[ -e `+p+`/Contents/Resources/stub.json ] || [ ! -e `+p+` ]`)
	}
	return itemScript([]string{"offload", it.Path}, `[ -e `+shellQuote(it.Path+".disklessly")+` ] && [ ! -e `+p+` ]`)
}

// restoreItemScript is the command for bringing one inventory row back.
func restoreItemScript(it offloadJobItem) string {
	switch it.restore.how {
	case "fetch":
		return itemScript([]string{"fetch", it.restore.arg}, `[ -e `+shellQuote(cmp.Or(it.Path, strings.TrimSuffix(it.restore.arg, ".disklessly")))+` ]`)
	case "unpack":
		verify := "true"
		if it.Path != "" {
			verify = `[ -e ` + shellQuote(it.Path) + ` ] && [ ! -e ` + shellQuote(it.Path) + `/Contents/Resources/stub.json ]`
		}
		return itemScript([]string{"unpack", it.restore.arg}, verify)
	case "data":
		return itemScript([]string{"data", "disable", it.restore.arg}, "true")
	}
	return ""
}

func (a *app) runOffloadJob(ctx context.Context, runner Runner, streamer StreamRunner, sc *scratch, j *offloadJob, route installRoute, needInstall bool, now time.Time) (string, error) {
	loc := j.loc
	name := machineLabel(sc.m)
	finish := func(state, msg string) {
		a.jobDo(j, func(j *offloadJob) {
			j.state, j.err, j.finished = state, msg, a.now()
			for i := range j.items {
				if j.items[i].Status == "pending" || j.items[i].Status == "running" {
					j.items[i].Status = "skipped"
				}
			}
		})
		a.forgetInventory(j.machine)
	}
	if needInstall {
		a.setPhase(j, phaseInstall)
		var err error
		if route.brew {
			_, err = brewInstall(ctx, runner, sc, now)
		} else {
			_, err = copyInstallWork(ctx, runner, streamer, route.offer.src, route.offer.cli, sc, now)
		}
		if err != nil {
			msg, _ := humanizeErr(err)
			finish(ojFailed, otr(loc, "install_failed", msg))
			return "", err
		}
		pr, err := probeMachine(ctx, runner, sc.m)
		if err != nil || pr.noCLI || !pr.offloadOK {
			finish(ojFailed, otr(loc, "still_old", name))
			return "", errors.New(otr(locZH, "still_old", name))
		}
	}
	a.setPhase(j, phaseCheck)
	if j.action == "offload" {
		var paths []string
		var idx []int
		for i, it := range j.items {
			if it.Status == "pending" {
				paths, idx = append(paths, it.Path), append(idx, i)
			}
		}
		if out, err := runner(ctx, sc.m, sizeScript(paths), offloadProbeTimeout*5); err == nil {
			for _, line := range strings.Split(out, "\n") {
				f := strings.Split(strings.TrimSpace(line), "\t")
				if len(f) != 3 || f[0] != "@@SIZE" {
					continue
				}
				k, err1 := strconv.Atoi(f[1])
				kb, err2 := strconv.ParseUint(f[2], 10, 64)
				if err1 == nil && err2 == nil && k >= 0 && k < len(idx) {
					a.jobDo(j, func(j *offloadJob) {
						j.items[idx[k]].Bytes = kb * 1024
						j.total += kb * 1024
					})
				}
			}
		}
	}
	before, haveBefore := availBytes(ctx, runner, sc.m)

	var done, failed int
	var lastErr string
	canceled := false
	for i := range j.items {
		var it offloadJobItem
		stop := false
		a.jobDo(j, func(j *offloadJob) {
			if j.cancel {
				stop = true
				return
			}
			it = j.items[i]
			if it.Status == "pending" {
				j.items[i].Status = "running"
			}
		})
		if stop {
			canceled = true
			break
		}
		if it.Status != "pending" {
			continue
		}
		var err error
		bytes := it.Bytes
		if j.action == "offload" {
			a.setPhase(j, phaseUpload)
			out, rerr := runner(ctx, sc.m, offloadItemScript(it), offloadItemTimeout)
			a.setPhase(j, phaseVerify)
			err = itemError(loc, name, rerr, parseItem(out), &bytes)
		} else {
			a.setPhase(j, phaseDownload)
			if it.restore.how == "session" {
				_, _, err = RestoreCold(ctx, runner, sc.st, sc.m, it.restore.arg, now)
			} else {
				out, rerr := runner(ctx, sc.m, restoreItemScript(it), offloadItemTimeout)
				err = itemError(loc, name, rerr, parseItem(out), nil)
			}
		}
		a.jobDo(j, func(j *offloadJob) {
			if err != nil {
				msg, _ := humanizeErr(err)
				j.items[i].Status, j.items[i].Error = "failed", msg
				failed++
				lastErr = msg
				return
			}
			j.items[i].Status = "done"
			if bytes > j.items[i].Bytes {
				j.total += bytes - j.items[i].Bytes
				j.items[i].Bytes = bytes
			}
			j.doneBytes += j.items[i].Bytes
			done++
		})
	}

	a.setPhase(j, phaseCleanup)
	if after, ok := availBytes(ctx, runner, sc.m); ok && haveBefore {
		a.jobDo(j, func(j *offloadJob) {
			var f uint64
			if after > before {
				f = after - before
			}
			j.freed = &f
		})
	}
	if j.action == "restore" && slices.ContainsFunc(j.items, func(it offloadJobItem) bool { return it.restore.how == "session" && it.Status == "done" }) {
		_, _ = RefreshColdIndex(ctx, runner, sc.st, sc.m, now)
	}
	verb := map[string]string{"offload": "搬到网盘", "restore": "拿回来"}[j.action]
	switch {
	case canceled:
		finish(ojCanceled, otr(loc, "canceled"))
		return fmt.Sprintf("已取消；%d 项已%s", done, verb), nil
	case done == 0 && failed > 0:
		finish(ojFailed, otr(loc, "all_failed", lastErr))
		return "", errors.New(lastErr)
	}
	finish(ojDone, "")
	note := fmt.Sprintf("%d 项已%s", done, verb)
	if failed > 0 {
		note += fmt.Sprintf("，%d 项失败：%s", failed, lastErr)
	}
	return note, nil
}

// itemError is why one item failed, or nil. bytes, when set, gets the size the CLI reported.
func itemError(loc, name string, rerr error, o itemOutcome, bytes *uint64) error {
	switch {
	case rerr != nil:
		return rerr
	case o.rc == cliExitMissing:
		return errors.New(otr(loc, "no_cli", name))
	case o.rc != 0 || !o.ok:
		return rawError(cmp.Or(o.msg, fmt.Sprintf("disklessly exit %d", o.rc)), o.msg)
	case !o.verified:
		return errors.New(cmp.Or(o.msg, "disklessly reported success, but the result is not on disk"))
	}
	if bytes != nil && o.bytes > 0 {
		*bytes = o.bytes
	}
	return nil
}

func (a *app) lookupJob(p offloadParams) (*offloadJob, error) {
	a.offloadMu().Lock()
	a.pruneJobsLocked(a.now())
	j := a.offloadData.jobs[p.Job]
	a.offloadMu().Unlock()
	if j == nil || (p.Machine.ID != "" && j.persona != "" && p.Machine.ID != j.persona) {
		return nil, &ext.Error{Code: "not_found", Message: "no such job " + strconv.Quote(p.Job)}
	}
	return j, nil
}

func (a *app) offloadJobStatus(p offloadParams) (any, error) {
	j, err := a.lookupJob(p)
	if err != nil {
		return nil, err
	}
	a.offloadMu().Lock()
	defer a.offloadMu().Unlock()
	res := map[string]any{
		"state": j.state, "done_bytes": j.doneBytes, "total_bytes": j.total, "items": slices.Clone(j.items),
	}
	if j.phase != "" {
		res["phase"] = j.phase
	}
	if j.freed != nil {
		res["freed_bytes"] = *j.freed
	}
	if j.err != "" {
		res["error"] = j.err
	}
	return res, nil
}

// offloadCancel stops a job between items: what is running finishes, what has not started is skipped.
func (a *app) offloadCancel(p offloadParams) (any, error) {
	j, err := a.lookupJob(p)
	if err != nil {
		return nil, err
	}
	a.jobDo(j, func(j *offloadJob) {
		if j.state == ojRunning {
			j.cancel = true
		}
	})
	return struct{}{}, nil
}

// filesOffload is disklessly_files_offload: the point's check and offload of paths on one machine, run in the
// call (the assistant asks the user first; the tool is not in the manifest's allow list). It never installs.
func (m *mcpServer) filesOffload(ctx context.Context, machine string, paths []string, dryRun bool) (any, error) {
	if len(paths) == 0 || len(paths) > offloadMaxRun {
		return nil, fmt.Errorf("paths 需要 1 到 %d 个绝对路径", offloadMaxRun)
	}
	var mach *Machine
	if err := m.store.read(func(st *State) {
		if mm := st.machine(machine); mm != nil && strings.TrimSpace(machine) != "" {
			c := *mm
			mach = &c
		}
	}); err != nil {
		return nil, err
	}
	if mach == nil {
		return nil, fmt.Errorf("找不到机器 %q", machine)
	}
	runner := m.run()
	pr, err := probeMachine(ctx, runner, mach)
	if err != nil {
		return nil, err
	}
	checks, err := checkOffloadPaths(ctx, runner, mach, pr.platform, offloadKinds, paths, locZH)
	if err != nil {
		return nil, err
	}
	installed := !pr.noCLI && pr.offloadOK
	if dryRun {
		return map[string]any{"machine": mach.ID, "dry_run": true, "cli_installed": installed, "cli_version": shortVersion(pr.cli.Version), "items": checks}, nil
	}
	if !installed {
		return nil, fmt.Errorf("「%s」上没有能上云文件的 disklessly CLI；先经用户确认用 disklessly_cli_install 安装或升级", machineLabel(mach))
	}
	name := machineLabel(mach)
	before, haveBefore := availBytes(ctx, runner, mach)
	type result struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Bytes  uint64 `json:"bytes,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	var results []result
	for _, c := range checks {
		if !c.OK {
			results = append(results, result{Path: c.Path, Status: "refused", Error: c.Reason})
			continue
		}
		var bytes uint64
		out, rerr := runner(ctx, mach, offloadItemScript(offloadJobItem{Path: c.Path, kind: c.kind}), offloadItemTimeout)
		if err := itemError(locZH, name, rerr, parseItem(out), &bytes); err != nil {
			msg, _ := humanizeErr(err)
			results = append(results, result{Path: c.Path, Status: "failed", Error: msg})
			continue
		}
		results = append(results, result{Path: c.Path, Status: "done", Bytes: bytes})
	}
	res := map[string]any{"machine": mach.ID, "items": results}
	if after, ok := availBytes(ctx, runner, mach); ok && haveBefore {
		var freed uint64
		if after > before {
			freed = after - before
		}
		res["freed_bytes"], res["freed"] = freed, FormatBytes(freed)
	}
	return res, nil
}
