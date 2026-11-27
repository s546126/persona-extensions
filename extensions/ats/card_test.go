package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// checkedCard holds the card to the SDK's rules for a compact view (an overview card) and gives its text, one
// line per component, for the assertions and the log.
func checkedCard(t *testing.T, name string, view *wire.View) string {
	t.Helper()
	if err := wire.CheckView(view); err != nil {
		t.Fatalf("%s: wire.CheckView failed: %v", name, err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if err := exttest.CheckView(raw, true); err != nil {
		t.Fatalf("%s: not a valid card: %v\n%s", name, err, raw)
	}
	var lines []string
	for _, s := range view.Sections {
		for _, c := range s.Items {
			for _, st := range c.Stats {
				lines = append(lines, st.Label+" "+st.Value)
			}
			if c.Text != "" {
				lines = append(lines, c.Text)
			}
		}
	}
	text := strings.Join(lines, "\n")
	t.Logf("card, %s:\n%s", name, text)
	return text
}

func cardText(t *testing.T, name string, q *Queue, cfg ExtConfig, now time.Time) string {
	t.Helper()
	return checkedCard(t, name, buildCardView(q, cfg, now, "UTC"))
}

func wantCard(t *testing.T, name, text string, want []string, bad ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("%s: the card does not say %q:\n%s", name, w, text)
		}
	}
	for _, b := range append(bad, "100%", "P0", "调度", "准入", "Agent") {
		if strings.Contains(text, b) {
			t.Errorf("%s: the card says %q:\n%s", name, b, text)
		}
	}
}

func TestCardBeforeSetupSaysWhatIsMissing(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	for _, c := range []struct {
		name    string
		cfg     ExtConfig
		missing string
	}{
		{"not set up", ExtConfig{}, "ATS 还没准备好：还没选干活的电脑，也还没告诉它能动哪些文件夹。"},
		{"no computer", ExtConfig{Repos: []string{"/srv/code"}}, "ATS 还没准备好：还没选干活的电脑。"},
		{"no folders", ExtConfig{Machines: []string{testMachine}}, "ATS 还没准备好：还没告诉它能动哪些文件夹。"},
	} {
		text := cardText(t, c.name, q, c.cfg, testStart)
		wantCard(t, c.name, text, []string{c.missing, "下一步：点开这里，在页面上选好电脑和文件夹。"}, "没结束", "等你处理", "排着")
		if hasLatin(strings.NewReplacer("ATS", "").Replace(text)) {
			t.Errorf("%s: the card has English: %q", c.name, text)
		}
	}
}

func TestCardBeforeSetupCountsLiveTasks(t *testing.T) {
	p := newPageRig(t)
	running := p.enqueue(EnqueueInput{Title: "整理发布说明", Repo: "/srv/code/app"})
	p.lease(running.ID, PhaseWorking, "")
	p.enqueue(EnqueueInput{Title: "补上单元测试", Repo: "/srv/code/app"})
	ended := p.enqueue(EnqueueInput{Title: "已取消的事", Repo: "/srv/code/app"})
	if _, err := p.q.CancelTask(ended.ID, testStart); err != nil {
		t.Fatal(err)
	}
	text := cardText(t, "not set up with live tasks", p.q, ExtConfig{Machines: []string{testMachine}}, testStart.Add(time.Minute))
	wantCard(t, "not set up with live tasks", text, []string{"ATS 还没准备好：还没告诉它能动哪些文件夹。", "点开这里",
		"之前交给它的事还有 2 件没结束"})
}

func TestCardWhenSetUp(t *testing.T) {
	online := []MachineSlot{{Name: testMachine, Title: "Studio", Online: true, MaxSlots: 2}}
	reset := testStart.Add(3 * time.Hour)
	now := testStart.Add(2 * time.Minute)

	t.Run("idle", func(t *testing.T) {
		p := newPageRig(t)
		if err := p.q.SetMachines(online); err != nil {
			t.Fatal(err)
		}
		text := cardText(t, "set up, idle", p.q, p.a.config(), now)
		wantCard(t, "idle", text, []string{"等你处理 0 件", "正在做 0 件", "排着 0 件", "没有要你处理的事。"}, "没做成", "额度")
		p.enqueue(EnqueueInput{Title: "补上单元测试", Repo: "/srv/code/app"})
		text = cardText(t, "set up, working", p.q, p.a.config(), now)
		wantCard(t, "working", text, []string{"排着 1 件", "没有要你处理的事，它在接着做。"}, "额度")
	})

	t.Run("waiting for you", func(t *testing.T) {
		p := newPageRig(t)
		none := 0
		if err := p.q.SetMachines(online); err != nil {
			t.Fatal(err)
		}
		first := p.enqueue(EnqueueInput{Title: "升级依赖", Repo: "/srv/code/app"})
		p.lease(first.ID, PhaseWorking, "Agent 在问你：用哪个分支？")
		p.enqueue(EnqueueInput{Title: "补上单元测试", Repo: "/srv/code/app"})
		failed := p.enqueue(EnqueueInput{Title: "修好构建", Repo: "/srv/code/app", MaxRetries: &none})
		p.complete(failed.ID, p.lease(failed.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "infra_error"})
		view := buildCardView(p.q, p.a.config(), now, "UTC")
		text := checkedCard(t, "waiting for you", view)
		wantCard(t, "waiting", text, []string{"等你处理 1 件", "排着 1 件", "没做成 1 件",
			"「升级依赖」等你处理：它在问你：用哪个分支？", "打开这件事对应的待办，在里面 ATS 的对话里回答它的问题或点同意。"}, "额度")
		if st := view.Sections[0].Items[0].Stats[0]; st.Tone != wire.ToneDanger {
			t.Errorf("the count of what waits for you is not marked: %+v", st)
		}
		// Low quota or no computer: what waits for the user still comes first.
		if err := p.q.SetMachines([]MachineSlot{{Name: testMachine, Online: false, MaxSlots: 2}}); err != nil {
			t.Fatal(err)
		}
		if text := cardText(t, "waiting, computer off", p.q, p.a.config(), now); !strings.Contains(text, "「升级依赖」等你处理") || strings.Contains(text, "没有电脑在线") {
			t.Errorf("with the computer off the card leads with the wrong thing:\n%s", text)
		}
	})

	t.Run("no computer online", func(t *testing.T) {
		p := newPageRig(t)
		if err := p.q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Studio", Online: false, MaxSlots: 2}}); err != nil {
			t.Fatal(err)
		}
		p.enqueue(EnqueueInput{Title: "补上单元测试", Repo: "/srv/code/app"})
		text := cardText(t, "none online", p.q, p.a.config(), now)
		wantCard(t, "none online", text, []string{"排着 1 件", "没有电脑在线"}, "额度")
		// Not connected to it yet at all.
		q := newTestQueue(t, DefaultQueueConfig())
		if err := q.SetMachines(nil); err != nil {
			t.Fatal(err)
		}
		wantCard(t, "not connected", cardText(t, "not connected yet", q, p.a.config(), now), []string{"还没连上你选的电脑"})
	})

	t.Run("quota low", func(t *testing.T) {
		p := newPageRig(t)
		if err := p.q.SetMachines(online); err != nil {
			t.Fatal(err)
		}
		if err := p.q.SetQuotaStatus(QuotaStatus{Provider: "test", RemainingPct: 20, ResetAt: &reset, CheckedAt: testStart}); err != nil {
			t.Fatal(err)
		}
		text := cardText(t, "quota low", p.q, p.a.config(), now)
		wantCard(t, "quota low", text, []string{"额度快用完了（还剩 20%）", "只做紧急和重要的事", "大约 13:00 额度恢复后再做"})
	})

	t.Run("quota exhausted", func(t *testing.T) {
		p := newPageRig(t)
		if err := p.q.SetMachines(online); err != nil {
			t.Fatal(err)
		}
		if err := p.q.SetQuotaStatus(QuotaStatus{Provider: "test", RemainingPct: 0, Exhausted: true, ResetAt: &reset, CheckedAt: testStart}); err != nil {
			t.Fatal(err)
		}
		text := cardText(t, "quota exhausted", p.q, p.a.config(), now)
		wantCard(t, "quota exhausted", text, []string{"额度用完了，大约 13:00 恢复"})
	})

	t.Run("no quota measured", func(t *testing.T) {
		p := newPageRig(t)
		if err := p.q.SetMachines(online); err != nil {
			t.Fatal(err)
		}
		for name, ga := range map[string]GenericGatewayAccounts{
			"a source, no accounts": {Sources: []GenericGatewaySource{{ID: "gw", Name: "Gateway", Type: "gateway"}}},
			"nothing":               {},
		} {
			if err := p.q.SetQuotaStatus(ParseGatewayAccounts(ga, testStart)); err != nil {
				t.Fatal(err)
			}
			text := cardText(t, "no quota measured ("+name+")", p.q, p.a.config(), now)
			wantCard(t, name, text, []string{"没有要你处理的事。"}, "额度", "%")
		}
	})
}

// The card opens the home page, and the program draws it under its own id.
func TestCardOpensTheHomePage(t *testing.T) {
	var m struct {
		Contributes struct {
			UI []struct{ ID, Placement, Opens string } `json:"ui"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	placements := map[string]string{}
	opens := ""
	for _, u := range m.Contributes.UI {
		placements[u.ID] = u.Placement
		if u.ID == cardUI {
			opens = u.Opens
		}
	}
	if placements[cardUI] != wire.PlacementOverviewCard {
		t.Fatalf("%s is placed at %q", cardUI, placements[cardUI])
	}
	if opens != queueUI || placements[opens] != wire.PlacementNavPage {
		t.Errorf("the card opens %q (placed at %q), want the home page %q", opens, placements[opens], queueUI)
	}

	a := newTestApp(t)
	if _, ok := a.program("ats", "test").UI[cardUI]; !ok {
		t.Fatal("the program does not draw the card")
	}
	view, err := a.render(context.Background(), &ext.UIRequest{Contribution: cardUI})
	if err != nil {
		t.Fatal(err)
	}
	if view.Title != cardTitle || len(view.Sections) != 1 || view.Sections[0].Items[0].Type != wire.ComponentStatGrid {
		t.Errorf("rendering %s did not draw the card: %+v", cardUI, view)
	}
}
