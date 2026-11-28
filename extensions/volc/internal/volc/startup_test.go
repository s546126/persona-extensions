package volc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/volc/internal/i18n"
	"github.com/nwverse/persona-extensions/extensions/volc/internal/volctest"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The stand-in names the program's variable as the program does.
func TestStandInEnv(t *testing.T) {
	if volctest.EnvService != EnvService {
		t.Fatalf("volctest.EnvService %q, the program reads %q", volctest.EnvService, EnvService)
	}
}

// run is one start of the program on the saved settings form and key, with its data directory dir, as the hub
// starts it after a save: initialize, then the startup test (if any) is waited for.
func run(t *testing.T, dir string, form Form, key string) *Plugin {
	t.Helper()
	pl := testPlugin(t, Form{}, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pl.start(ctx, wire.InitializeParams{Config: mustJSON(form), Secrets: map[string]string{SecretKey: key}}, dir)
	select {
	case <-pl.selfTested:
	case <-time.After(15 * time.Second):
		t.Fatal("the startup test did not end")
	}
	return pl
}

// calls is how often the service was asked: recognition sessions it accepted and synthesis requests.
func calls(fv *volctest.Service) int { return fv.Count() + len(fv.Syntheses()) }

// setupOf is what GET /<point>/volc/info says in setup ("" when it says nothing).
func setupOf(t *testing.T, pl *Plugin, point string) string {
	t.Helper()
	srv := httptest.NewServer(pl.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/" + point + "/volc/info")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var v struct {
		Protocol int    `json:"protocol"`
		Setup    string `json:"setup"`
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil || res.StatusCode != 200 || v.Protocol != 1 {
		t.Fatalf("%s info: %d %+v %v", point, res.StatusCode, v, err)
	}
	return v.Setup
}

func refuseKey(s *volctest.Script) {
	s.RefuseASR, s.RefuseBody = 401, `{"code":45000010,"message":"invalid api key"}`
	s.TTSStatus, s.TTSBody = 401, `{"code":45000010,"message":"invalid api key"}`
}

// The hub does not check the first key (the extension is not running to be asked): a wrong one is found by the
// startup test, said by both points' info as setup "key_rejected" and on the page, and the program keeps running.
// The same settings started again are not tested again; another key saved (with or without a settings check) is
// ready again.
func TestStartupTestRejectsAWrongFirstKey(t *testing.T) {
	fast(t)
	fv := fake(t)
	dir := t.TempDir()
	fv.Set(refuseKey)

	pl := run(t, dir, Form{}, "wrong-key-123456")
	if calls(fv) == 0 || pl.tests.Load() != 1 {
		t.Fatalf("no startup test: %d calls, %d tests", calls(fv), pl.tests.Load())
	}
	for _, point := range []string{"transcription", "speech"} {
		if got := setupOf(t, pl, point); got != "key_rejected" {
			t.Errorf("%s info after a refused key: setup %q", point, got)
		}
	}
	v, err := pl.renderStatus(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: StatusPage, Locale: "zh-Hans"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := v.Sections[0].Items
	why := i18n.T(i18n.ZhHans, "volc-check.why_forbidden")
	if rows[0].Badge == nil || rows[0].Badge.Tone != "danger" || rows[0].Subtitle != why || rows[1].Subtitle != why {
		t.Errorf("the page does not say the key was refused: %+v", rows)
	}
	if strings.Contains(string(mustRead(t, dir)), "wrong-key") {
		t.Error("status.json holds the key")
	}

	// Restarted with the same settings (a crash, the hub restarting): not tested again, still refused.
	before := calls(fv)
	again := run(t, dir, Form{}, "wrong-key-123456")
	if calls(fv) != before || again.tests.Load() != 0 {
		t.Errorf("the same fingerprint was tested again: %d calls", calls(fv)-before)
	}
	if got := setupOf(t, again, "transcription"); got != "key_rejected" {
		t.Errorf("restarted: setup %q", got)
	}
	// Another voice is another fingerprint: tested once more.
	other := run(t, dir, Form{Voice: Voices[1]}, "wrong-key-123456")
	if calls(fv) == before || other.tests.Load() != 1 {
		t.Error("new settings were not tested")
	}

	// A good key saved through settings.check of the running program: the check's outcome is kept for the new
	// settings, so the run that has them is ready without a test of its own.
	fv.Set(func(s *volctest.Script) { s.RefuseASR, s.TTSStatus = 0, 0 })
	params, _ := json.Marshal(wire.SettingsCheckParams{Config: mustJSON(Form{}), Secrets: map[string]string{SecretKey: "good-key"}})
	out, err := again.settings(context.Background(), "check", params)
	if err != nil || !out.(wire.SettingsCheckResult).Ok {
		t.Fatalf("settings.check of the good key: %+v %v", out, err)
	}
	before = calls(fv)
	good := run(t, dir, Form{}, "good-key")
	if calls(fv) != before || good.tests.Load() != 0 {
		t.Errorf("the checked key was tested again at start: %d calls", calls(fv)-before)
	}
	for _, point := range []string{"transcription", "speech"} {
		if got := setupOf(t, good, point); got != "" {
			t.Errorf("%s info with the good key: setup %q", point, got)
		}
	}

	// A good key that reached the program without a check: the startup test finds it good.
	fresh := run(t, t.TempDir(), Form{}, "another-good-key")
	if fresh.tests.Load() != 1 || setupOf(t, fresh, "transcription") != "" || setupOf(t, fresh, "speech") != "" {
		t.Errorf("an unchecked good key: %d tests", fresh.tests.Load())
	}
	asr, tts := fresh.board.get()
	if asr == nil || !asr.OK || asr.Source != sourceStartup || tts == nil || !tts.OK {
		t.Errorf("outcomes %+v %+v", asr, tts)
	}
}

// Only a refused key is key_rejected: a model not turned on, or a service out of reach, is said on the page and in
// the log, and is kept like any outcome, so the same settings are not tried again at the next start.
func TestStartupTestOtherFailures(t *testing.T) {
	fast(t)
	fv := fake(t)
	dir := t.TempDir()
	fv.Set(func(s *volctest.Script) {
		s.RefuseRequest, s.RefuseMessage = 45000030, "requested resource not granted"
	})
	pl := run(t, dir, Form{}, "k")
	asr, _ := pl.board.get()
	if asr == nil || asr.OK || asr.Kind != failNotEnabled {
		t.Fatalf("outcome %+v", asr)
	}
	if got := setupOf(t, pl, "transcription"); got != "" {
		t.Errorf("a model not turned on is not a refused key: setup %q", got)
	}
	before := calls(fv)
	run(t, dir, Form{}, "k")
	if calls(fv) != before {
		t.Error("tested again at the next start")
	}
}

// Nothing to test: no key (setup "key"), and with reading answers off a recognition outcome is enough. A start
// tests at most once in testInterval with the page's Test Connection.
func TestStartupTestBounds(t *testing.T) {
	fast(t)
	fv := fake(t)
	none := run(t, t.TempDir(), Form{}, "")
	if calls(fv) != 0 || none.tests.Load() != 0 || setupOf(t, none, "transcription") != "key" || setupOf(t, none, "speech") != "key" {
		t.Errorf("without a key: %d calls", calls(fv))
	}

	off := false
	dir := t.TempDir()
	quiet := run(t, dir, Form{ReadAnswers: &off}, "k")
	if quiet.tests.Load() != 1 || len(fv.Syntheses()) != 0 {
		t.Errorf("reading off: %d tests, %d syntheses", quiet.tests.Load(), len(fv.Syntheses()))
	}
	before := calls(fv)
	if run(t, dir, Form{ReadAnswers: &off}, "k").tests.Load() != 0 || calls(fv) != before {
		t.Error("reading off, restarted: tested again")
	}

	// The page's Test Connection right after the startup test is within testInterval: not tried again.
	pl := run(t, t.TempDir(), Form{}, "k2")
	before = calls(fv)
	res, err := pl.statusAction(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: StatusPage, Action: "test"}})
	if err != nil || res.View == nil || calls(fv) != before {
		t.Errorf("Test Connection right after the startup test: %d calls, %v", calls(fv)-before, err)
	}
}

// A program stopped during its startup test keeps no outcome: the next start tests the settings.
func TestStartupTestStoppedKeepsNothing(t *testing.T) {
	fast(t)
	fv := fake(t)
	fv.Set(func(s *volctest.Script) { s.DropAll = true })
	old := checkMost
	checkMost = time.Minute
	t.Cleanup(func() { checkMost = old })
	dir := t.TempDir()
	pl := testPlugin(t, Form{}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pl.start(ctx, wire.InitializeParams{Config: mustJSON(Form{}), Secrets: map[string]string{SecretKey: "k"}}, dir)
	<-pl.selfTested
	if asr, tts := pl.board.get(); asr != nil || tts != nil || pl.tests.Load() != 0 {
		t.Errorf("a stopped test kept %+v %+v", asr, tts)
	}
}

// checkAndSave is settings.check of the running pl on form and key (the save the hub makes after it), which must pass.
func checkAndSave(t *testing.T, pl *Plugin, form Form, key string) {
	t.Helper()
	params, _ := json.Marshal(wire.SettingsCheckParams{Config: mustJSON(form), Secrets: map[string]string{SecretKey: key}})
	out, err := pl.settings(context.Background(), "check", params)
	if err != nil || !out.(wire.SettingsCheckResult).Ok {
		t.Fatalf("settings.check %+v: %+v %v", form, out, err)
	}
}

// Each point answers for its own service: a key only synthesis refuses leaves transcription ready, also once reading
// answers is turned off, after Test Connection and with another voice; a key only recognition refuses leaves
// speech ready.
func TestKeyRejectedIsPerService(t *testing.T) {
	fast(t)
	fv := fake(t)
	old := testInterval
	testInterval = 0 // Test Connection right after a start is not held back by the interval here
	t.Cleanup(func() { testInterval = old })
	dir := t.TempDir()

	// Only synthesis refuses the key.
	fv.Set(func(s *volctest.Script) {
		s.TTSStatus, s.TTSBody = 401, `{"code":45000010,"message":"invalid api key"}`
	})
	pl := run(t, dir, Form{}, "k")
	if pl.tests.Load() != 1 {
		t.Fatalf("startup tests %d", pl.tests.Load())
	}
	if got := setupOf(t, pl, "transcription"); got != "" {
		t.Errorf("synthesis refused: transcription setup %q, want none", got)
	}
	if got := setupOf(t, pl, "speech"); got != "key_rejected" {
		t.Errorf("synthesis refused: speech setup %q", got)
	}

	// Reading answers turned off: the check passes (nothing to try), and the run with it is not tested again.
	off := false
	checkAndSave(t, pl, Form{ReadAnswers: &off}, "k")
	before := calls(fv)
	quiet := run(t, dir, Form{ReadAnswers: &off}, "k")
	if calls(fv) != before || quiet.tests.Load() != 0 {
		t.Errorf("reading off: tested again (%d calls)", calls(fv)-before)
	}
	if got := setupOf(t, quiet, "transcription"); got != "" {
		t.Errorf("reading off: transcription setup %q", got)
	}
	// Test Connection tries recognition only, which works.
	res, err := quiet.statusAction(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: StatusPage, Action: "test"}})
	if err != nil || res.View == nil || quiet.tests.Load() != 1 || fv.Count() == 0 {
		t.Fatalf("Test Connection: %v, %d tests", err, quiet.tests.Load())
	}
	if got := setupOf(t, quiet, "transcription"); got != "" {
		t.Errorf("after Test Connection: transcription setup %q", got)
	}
	// Another voice with reading off: a new fingerprint, still not held back.
	checkAndSave(t, quiet, Form{ReadAnswers: &off, Voice: Voices[2]}, "k")
	voiced := run(t, dir, Form{ReadAnswers: &off, Voice: Voices[2]}, "k")
	if got := setupOf(t, voiced, "transcription"); got != "" {
		t.Errorf("reading off, another voice: transcription setup %q", got)
	}

	// Only recognition refuses the key; synthesis works.
	fv.Set(func(s *volctest.Script) {
		s.TTSStatus, s.TTSBody = 0, ""
		s.RefuseASR, s.RefuseBody = 401, `{"code":45000010,"message":"invalid api key"}`
	})
	asrOnly := run(t, t.TempDir(), Form{}, "k")
	if got := setupOf(t, asrOnly, "transcription"); got != "key_rejected" {
		t.Errorf("recognition refused: transcription setup %q", got)
	}
	if got := setupOf(t, asrOnly, "speech"); got != "" {
		t.Errorf("recognition refused: speech setup %q, want none", got)
	}
}
