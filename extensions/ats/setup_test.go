package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// setupApp is an app that is not set up, with the hub's computers as given.
func setupApp(t *testing.T, machines ...hubMachine) (*atsApp, *fakeHub) {
	t.Helper()
	a := newATSApp(newTestQueue(t, DefaultQueueConfig()))
	a.nowFunc = func() time.Time { return testStart }
	hub := newFakeHub()
	hub.machines = machines
	a.api = hub
	return a, hub
}

func renderHome(t *testing.T, a *atsApp) *wire.View {
	t.Helper()
	view, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: queueUI}})
	if err != nil {
		t.Fatal(err)
	}
	checked(t, view)
	return view
}

func act(t *testing.T, a *atsApp, action string, values map[string]string) *wire.ActionResult {
	t.Helper()
	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: queueUI, Action: action, Values: values}})
	if err != nil {
		t.Fatal(err)
	}
	if res.View != nil {
		checked(t, res.View)
	}
	return res
}

func hasKey(v *wire.View, key string) bool { return sectionOf(v, key) != "" }

// The page offers only computers ATS can work on now: online, confirmed, a Mac or Linux. A tap picks one, and the
// save step names it.
func TestSetupListsOnlineComputersAndPicksOne(t *testing.T) {
	a, _ := setupApp(t,
		hubMachine{ID: "m-studio", Name: "Studio", Online: true, OS: "linux"},
		hubMachine{ID: "m-laptop", Name: "Laptop", Online: true, OS: "darwin"},
		hubMachine{ID: "m-off", Name: "Off", Online: false, OS: "linux"},
		hubMachine{ID: "m-new", Name: "New", Online: true, OS: "linux", Trust: "pending"},
		hubMachine{ID: "m-win", Name: "Win", Online: true, OS: "windows"},
	)
	view := renderHome(t, a)
	for _, id := range []string{"m-studio", "m-laptop"} {
		b := findRow(t, view, "setup-machine-"+id)
		if b.Type != wire.ComponentButton || b.Action == nil || b.Action.ID != actSetupMachine+":"+id || !strings.HasPrefix(b.Title, "用这台") {
			t.Errorf("%s: %+v", id, b)
		}
	}
	for _, id := range []string{"m-off", "m-new", "m-win"} {
		if hasKey(view, "setup-machine-"+id) {
			t.Errorf("%s is offered", id)
		}
	}
	if hasKey(view, "setup-machine-note") {
		t.Error("with two computers the page says there is only one")
	}

	res := act(t, a, actSetupMachine+":m-laptop", nil)
	if res.View == nil {
		t.Fatal("picking a computer does not draw the page again")
	}
	if row := findRow(t, res.View, "setup-machine-m-laptop"); row.Type != wire.ComponentRow || row.Badge == nil || row.Badge.Text != "就用这台" {
		t.Errorf("the picked computer: %+v", row)
	}
	if b := findRow(t, res.View, "setup-machine-m-studio"); b.Type != wire.ComponentButton {
		t.Errorf("the other computer: %+v", b)
	}
	if next := findRow(t, res.View, "setup-next").Text; !strings.Contains(next, "打开「Laptop」") {
		t.Errorf("the save step does not name the computer: %q", next)
	}
}

func TestSetupPicksTheOnlyComputer(t *testing.T) {
	a, _ := setupApp(t, hubMachine{ID: "m-studio", Name: "Studio", Online: true, OS: "linux"})
	view := renderHome(t, a)
	if row := findRow(t, view, "setup-machine-m-studio"); row.Badge == nil || row.Badge.Text != "就用这台" {
		t.Errorf("the only computer is not picked: %+v", row)
	}
	if got := findRow(t, view, "setup-machine-note").Text; !strings.Contains(got, "只有这一台在线") {
		t.Errorf("the page does not say why: %q", got)
	}
	if next := findRow(t, view, "setup-next").Text; !strings.Contains(next, "打开「Studio」") {
		t.Errorf("the save step: %q", next)
	}
}

func TestSetupWithoutComputers(t *testing.T) {
	a, hub := setupApp(t, hubMachine{ID: "m-off", Name: "Off", OS: "linux"})
	if got := findRow(t, renderHome(t, a), "setup-machines").Text; !strings.Contains(got, "没有在线的") {
		t.Errorf("no computer online: %q", got)
	}
	a.api = &failingMachines{fakeHub: hub}
	if got := findRow(t, renderHome(t, a), "setup-machines").Text; !strings.Contains(got, "还没读到你的电脑") {
		t.Errorf("the hub did not answer: %q", got)
	}
}

type failingMachines struct{ *fakeHub }

func (f *failingMachines) Machines(context.Context) ([]hubMachine, error) {
	return nil, errors.New("down")
}

// The folder is taken from the field or a suggestion, checked, and shown back; the save step names it.
func TestSetupTakesAFolder(t *testing.T) {
	a, _ := setupApp(t, hubMachine{ID: "m-studio", Name: "Studio", Online: true, OS: "linux"})
	view := renderHome(t, a)
	for _, s := range repoSuggestions {
		b := findRow(t, view, "setup-repo-"+strings.TrimPrefix(s, "~/"))
		if b.Action == nil || b.Action.ID != actSetupRepo+":"+s {
			t.Errorf("suggestion %s: %+v", s, b)
		}
	}
	if b := findRow(t, view, "setup-repo-use"); b.Action == nil || !b.Action.Submit {
		t.Errorf("the field's button does not send the field: %+v", b)
	}
	if why := findRow(t, view, "setup-repo-why").Text; strings.Count(why, "。") != 1 {
		t.Errorf("why a folder: %q", why)
	}

	for _, bad := range []string{"", "  ", "code", "~code", "/a\n/b"} {
		res := act(t, a, actSetupRepo, map[string]string{"repo": bad})
		if res.Toast == "" || res.View == nil {
			t.Errorf("%q: %+v", bad, res)
		}
	}
	if a.draft.Repo != "" {
		t.Fatalf("a bad folder was kept: %q", a.draft.Repo)
	}
	res := act(t, a, actSetupRepo, map[string]string{"repo": " ~/work "})
	if res.Toast != "" || findRow(t, res.View, "setup-repo").Value != "~/work" {
		t.Errorf("the folder is not shown back: %+v", res)
	}
	if next := findRow(t, res.View, "setup-next").Text; !strings.Contains(next, "文件夹填「~/work」") {
		t.Errorf("the save step does not name the folder: %q", next)
	}
	res = act(t, a, actSetupRepo+":~/code", nil) // a suggestion, as an app without action values sends it
	if findRow(t, res.View, "setup-repo").Value != "~/code" || hasKey(res.View, "setup-repo-code") {
		t.Errorf("the suggestion: %+v", res.View)
	}
}

// Saving opens this extension's own settings, the one link a page may open there.
func TestSetupSaveOpensTheSettings(t *testing.T) {
	a, _ := setupApp(t)
	a.id = "ats"
	if res := act(t, a, actSetupSave, nil); res.OpenURL != "persona://settings/extensions/ats" || res.View != nil {
		t.Errorf("save: %+v", res)
	}
}

// Once the settings hold both, the page is the usual home page at once; one step saved is marked done.
func TestSetupDoneShowsTheHomePage(t *testing.T) {
	a, _ := setupApp(t, hubMachine{ID: testMachine, Name: "Studio", Online: true, OS: "linux"})
	a.cfg.Machines = []string{testMachine}
	view := renderHome(t, a)
	if row := findRow(t, view, "setup-machines"); row.Badge == nil || row.Badge.Text != "做好了" || !strings.Contains(row.Title, "Studio") {
		t.Errorf("the saved computer: %+v", row)
	}
	if next := findRow(t, view, "setup-next").Text; strings.Contains(next, "打开") || !strings.Contains(next, "填上文件夹") {
		t.Errorf("the save step with the computer saved: %q", next)
	}
	a.cfg.Repos = []string{"~/code"}
	view = renderHome(t, a)
	if got, want := strings.Join(sectionTitles(view), "|"), "|现在怎么样|正在做和排着的|最近做完的|数一数|干活的电脑"; got != want {
		t.Fatalf("sections when set up: %s, want %s", got, want)
	}
	for _, k := range []string{"setup-next", "setup-save", "setup-repo"} {
		if hasKey(view, k) {
			t.Errorf("set up: the page still has %s", k)
		}
	}
}

// A computer is left out only when its owner has not confirmed it or ATS has no program for its system; the
// hub's words are read without regard to case.
func TestSetupOffersEveryUsableComputer(t *testing.T) {
	for _, c := range []struct {
		m    hubMachine
		want bool
	}{
		{hubMachine{OS: "linux"}, true},
		{hubMachine{OS: "Darwin"}, true},
		{hubMachine{OS: "mac"}, true},
		{hubMachine{}, true},
		{hubMachine{OS: "linux", Trust: "trusted"}, true},
		{hubMachine{OS: "linux", Trust: "Pending"}, false},
		{hubMachine{OS: "windows"}, false},
	} {
		if got := usableMachine(c.m); got != c.want {
			t.Errorf("%+v: %v, want %v", c.m, got, c.want)
		}
	}
}

// Picking a computer sends the page's field along: a folder typed and not yet taken stays on the page.
func TestSetupKeepsATypedFolderWhenAComputerIsPicked(t *testing.T) {
	a, _ := setupApp(t,
		hubMachine{ID: "m-studio", Name: "Studio", Online: true, OS: "linux"},
		hubMachine{ID: "m-laptop", Name: "Laptop", Online: true, OS: "darwin"},
	)
	if b := findRow(t, renderHome(t, a), "setup-machine-m-laptop"); b.Action == nil || !b.Action.Submit {
		t.Fatalf("the computer's button does not send the field: %+v", b)
	}
	res := act(t, a, actSetupMachine+":m-laptop", map[string]string{"repo": "~/work"})
	if got := findRow(t, res.View, "setup-repo").Value; got != "~/work" {
		t.Errorf("the typed folder: %q", got)
	}
	res = act(t, a, actSetupMachine+":m-studio", map[string]string{"repo": ""})
	if got := findRow(t, res.View, "setup-repo").Value; got != "~/work" {
		t.Errorf("an empty field wiped the folder taken before: %q", got)
	}
	res = act(t, a, actSetupMachine+":m-laptop", map[string]string{"repo": "code"})
	if !strings.Contains(res.Toast, "文件夹没记下") || findRow(t, res.View, "setup-repo").Value != "~/work" {
		t.Errorf("a bad folder sent with a computer: %+v", res)
	}
}
