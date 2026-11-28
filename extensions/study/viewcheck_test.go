package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

// Every page the extension draws, in every state it can be in, must pass wire.CheckView (what the apps accept),
// survive a JSON round trip through wire.CheckViewJSON, and keep the rules the schema cannot say on its own.

var allModes = []string{modeHome, modeReview, modeDrafts, modeTopics, modeNotion, modeObsidian, modeNLM, modeCards, modeCard, modeWrite, modeLogseq, modeWeak, modeTutor, modeGuide, modeMap}

// checkPage holds one view to everything an app checks, and to this extension's own promises.
func checkPage(t *testing.T, label string, v *wire.View) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s: no view", label)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("%s: CheckView: %v", label, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if err := wire.CheckViewJSON(raw); err != nil {
		t.Fatalf("%s: CheckViewJSON: %v", label, err)
	}
	keys := map[string]bool{}
	for i, s := range v.Sections {
		for j, c := range s.Items {
			if c.Key != "" {
				if keys[c.Key] {
					t.Fatalf("%s: section %d item %d: key %q twice", label, i, j, c.Key)
				}
				keys[c.Key] = true
			}
			if c.Type == wire.ComponentSegmented {
				if len(c.Choices) < 2 || len(c.Choices) > 5 {
					t.Fatalf("%s: segmented with %d choices", label, len(c.Choices))
				}
				for _, ch := range c.Choices {
					if n := utf8.RuneCountInString(ch.Title); n == 0 || n > 12 {
						t.Fatalf("%s: segmented choice %q", label, ch.Title)
					}
				}
			}
		}
	}
	if len(v.State) > wire.StateMost {
		t.Fatalf("%s: state of %d bytes", label, len(v.State))
	}
}

func viewOf(t *testing.T, a *app, st pageState) *wire.View {
	t.Helper()
	v, err := a.view(t.Context(), st, "")
	if err != nil {
		t.Fatalf("%+v: %v", st, err)
	}
	checkPage(t, fmt.Sprintf("%+v", st), v)
	return v
}

func actOn(t *testing.T, a *app, action string, st pageState, values map[string]string) (*wire.ActionResult, error) {
	t.Helper()
	res, err := a.action(t.Context(), request(action, st, values))
	if err == nil {
		checkPage(t, action, res.View)
	}
	return res, err
}

func stateOf(t *testing.T, v *wire.View) pageState {
	t.Helper()
	var st pageState
	if err := (&ext.UIRequest{UIParams: wire.UIParams{State: v.State}}).DecodeState(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestViewsEmpty(t *testing.T) {
	// No cards, no computer, no Notion, no model: every page still draws, mostly as an empty state.
	a := testApp(t, &fakeHub{}, defaults())
	for _, m := range allModes {
		viewOf(t, a, pageState{Mode: m})
	}
	viewOf(t, a, pageState{Mode: modeCard, Card: "missing"})
	viewOf(t, a, pageState{Mode: modeTutor, Exercise: "expired", Hints: 3})
	viewOf(t, a, pageState{Mode: modeWeak, Topic: "nothing", Status: statusWeak})
	viewOf(t, a, pageState{Mode: modeReview, Done: 3})
	if _, err := actOn(t, a, "tutor_start", pageState{Mode: modeTutor}, nil); err == nil {
		t.Fatal("practice without cards")
	}
	// No hub at all.
	b, err := openApp("", defaults(), "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range allModes {
		v, err := b.view(t.Context(), pageState{Mode: m}, "")
		if err != nil {
			t.Fatal(m, err)
		}
		checkPage(t, "no hub "+m, v)
	}
	for _, fam := range []string{wire.WidgetFamilySmall, wire.WidgetFamilyAccessoryInline, wire.WidgetFamilyAccessoryCircular, wire.WidgetFamilyAccessoryRectangular} {
		w, err := b.renderWidget(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: widgetID, Placement: wire.PlacementWidget, Size: fam, UIVersion: 4}})
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, "widget "+fam, w)
	}
}

func TestViewsLongAndOddFields(t *testing.T) {
	long := strings.Repeat("长", 5000)
	h := &fakeHub{
		topics: []wire.KBEntry{{ID: "t1", Name: "", OneLine: long}, {ID: "t2", Name: long}},
		node:   &nodeWorker{cfg: nodeConfig{Vault: t.TempDir(), Logseq: t.TempDir()}, look: func() string { return "" }},
	}
	a := testApp(t, h, config{Machines: []string{"mac"}})
	src := Source{Kind: "unknown-kind", Ref: long, Title: strings.Repeat("题", 300)}
	a.addDirect([]cardText{
		// An Obsidian embed and an http link are outside the Markdown subset: the answer falls back to text.
		{Front: strings.Repeat("问", maxFront+50), Back: strings.Repeat("答", maxBack-40) + " ![[diagram.png]] [x](http://example.com)", Topic: strings.Repeat("知", 200)},
		{Front: "Markdown answer", Back: "**bold** and [a link](https://example.com)"},
		{Front: "Reference link", Back: "[a][1]\n\n[1]: https://example.com"},
	}, src)
	a.addDrafts([]cardText{{Front: long, Back: long, Topic: long}}, src)
	for _, c := range a.st.Cards {
		if utf8.RuneCountInString(c.Topic) > maxTopic || utf8.RuneCountInString(c.Front) > maxFront {
			t.Fatalf("not clipped: %d %d", utf8.RuneCountInString(c.Topic), utf8.RuneCountInString(c.Front))
		}
	}
	for _, m := range allModes {
		viewOf(t, a, pageState{Mode: m})
	}
	for _, c := range a.st.sortedCards() {
		viewOf(t, a, pageState{Mode: modeCard, Card: c.ID})
		viewOf(t, a, pageState{Mode: modeReview, Card: c.ID, Shown: true})
		viewOf(t, a, pageState{Mode: modeWeak, Topic: topicOf(c)})
	}
	if got := answerBlock("![img](https://example.com/x.png)"); got.Type != wire.ComponentText {
		t.Fatal("an image answer stayed Markdown")
	}
	if got := answerBlock("**fine**"); got.Type != wire.ComponentMarkdown {
		t.Fatal("plain Markdown became text")
	}
	if got := answerBlock(""); got.Text == "" {
		t.Fatal("an empty answer")
	}
	// A failing source with a very long error message draws as an error page.
	h.kbErr = errors.New(long)
	viewOf(t, a, pageState{Mode: modeTopics})
	// A long notebook title and query in the state.
	viewOf(t, a, pageState{Mode: modeNLM, Notebook: "nb", NBTitle: strings.Repeat("本", 300), Query: strings.Repeat("q", 200)})
	// Empty strings never reach a required field.
	for _, c := range []wire.Component{text("", ""), button("", "x", nil), row("", "", "x", nil), empty("", "")} {
		checkPage(t, "helper "+c.Type, &wire.View{Sections: []wire.Section{{Items: []wire.Component{c}}}})
	}
}

func TestViewsLargeCollections(t *testing.T) {
	var notebooks []string
	for i := 0; i < 400; i++ {
		notebooks = append(notebooks, fmt.Sprintf(`{"id":"nb_%d","title":"Notebook %d"}`, i, i))
	}
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch cmd := strings.Join(args, " "); {
		case strings.HasPrefix(cmd, "--quiet auth check"):
			return []byte(`{}`), nil
		case cmd == "--quiet list --json":
			return []byte(`{"notebooks":[` + strings.Join(notebooks, ",") + `]}`), nil
		}
		return nil, errors.New("unexpected")
	}
	h := &fakeHub{node: &nodeWorker{cfg: nodeConfig{}, look: func() string { return "/bin/notebooklm" }, run: run}}
	for i := 0; i < 400; i++ {
		h.topics = append(h.topics, wire.KBEntry{ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("Topic %d", i)})
	}
	a := testApp(t, h, config{Machines: []string{"mac"}, NewPerDay: 200})
	var cards []cardText
	for i := 0; i < 450; i++ {
		cards = append(cards, cardText{Front: fmt.Sprintf("Question %d", i), Back: "Answer", Topic: fmt.Sprintf("Topic %d", i%150)})
	}
	if n, err := a.addDirect(cards, Source{Kind: srcManual}); err != nil || n != 450 {
		t.Fatal(n, err)
	}
	var drafts []cardText
	for i := 0; i < maxDrafts+50; i++ {
		drafts = append(drafts, cardText{Front: fmt.Sprintf("Draft %d", i), Back: "B"})
	}
	if n, _ := a.addDrafts(drafts, Source{Kind: srcManual}); n != maxDrafts {
		t.Fatal(n)
	}
	// Some reviews spread the topics over every status.
	for i, c := range a.st.sortedCards() {
		if i%3 == 0 {
			continue
		}
		r := fsrs.Good
		if i%3 == 1 {
			r = fsrs.Again
		}
		if _, err := a.grade(c.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range allModes {
		viewOf(t, a, pageState{Mode: m})
	}
	for _, s := range []string{"", statusWeak, statusShaky, statusSolid, statusNew} {
		viewOf(t, a, pageState{Mode: modeWeak, Status: s})
		viewOf(t, a, pageState{Mode: modeWeak, Status: s, Topic: "Topic 7"})
	}
	// Filtering through the page's own actions.
	st := pageState{Mode: modeWeak}
	for _, f := range []string{"all", statusWeak, statusShaky, statusSolid, statusNew} {
		res, err := actOn(t, a, "weak_filter", st, map[string]string{"status": f})
		if err != nil {
			t.Fatal(f, err)
		}
		if got := stateOf(t, res.View); (f == "all" && got.Status != "") || (f != "all" && got.Status != f) {
			t.Fatalf("%s: %+v", f, got)
		}
	}
	if _, err := actOn(t, a, "weak_filter", st, map[string]string{"status": "bogus"}); err == nil {
		t.Fatal("a bad filter was accepted")
	}
	res, err := actOn(t, a, "weak_topic", st, map[string]string{"topic": "Topic 3"})
	if err != nil || stateOf(t, res.View).Topic != "Topic 3" || res.View.Title != "Topic 3" {
		t.Fatal(err)
	}
	// One big notebook list.
	viewOf(t, a, pageState{Mode: modeNLM})
	// The home page and the widget with this much.
	for _, fam := range []string{wire.WidgetFamilySmall, wire.WidgetFamilyAccessoryInline, wire.WidgetFamilyAccessoryCircular, wire.WidgetFamilyAccessoryRectangular} {
		w, err := a.renderWidget(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: widgetID, Placement: wire.PlacementWidget, Size: fam, UIVersion: 4}})
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, "widget "+fam, w)
	}
}

func TestViewsTutorStates(t *testing.T) {
	exercises := map[string]string{
		kindChoice:   choiceExercise,
		kindCloze:    `{"question":"The ____ makes ATP.","answer":"mitochondrion","accepted":["mitochondria"],"hints":["Think energy."],"explanation":"Respiration."}`,
		kindConcept:  `{"question":"How is a mitochondrion different from a chloroplast?","answer":"One releases energy, the other captures it.","hints":["a","b","c"]}`,
		kindScenario: `{"question":"A cell's mitochondria stop working. What happens?","answer":"ATP runs out.","hints":["a","b"],"explanation":"No respiration."}`,
	}
	kind := kindChoice
	verdict := `{"verdict":"partial","score":60,"cause":"transfer","diagnosis":"` + strings.Repeat("诊", 400) + `","feedback":"` + strings.Repeat("馈", 900) + `"}`
	h := &fakeHub{model: func(p wire.ModelCompleteParams) (wire.ModelCompleteResult, error) {
		switch p.Label {
		case "study.exercise":
			return wire.ModelCompleteResult{JSON: json.RawMessage(exercises[kind])}, nil
		case "study.judge":
			return wire.ModelCompleteResult{JSON: json.RawMessage(verdict)}, nil
		}
		return wire.ModelCompleteResult{JSON: json.RawMessage(`{"cards":[{"front":"Reinforce","back":"Card"}]}`)}, nil
	}}
	a := testApp(t, h, defaults())
	a.addDirect([]cardText{{Front: "Powerhouse of the cell?", Back: "Mitochondrion", Topic: "Cells"}}, Source{Kind: srcManual})
	id := a.st.sortedCards()[0].ID
	if _, err := a.grade(id, fsrs.Easy); err != nil {
		t.Fatal(err)
	}
	setNow(a, t0.Add(90*24*time.Hour))

	// From the home page into the tutor, through each kind.
	res, err := actOn(t, a, "go", pageState{}, map[string]string{"mode": modeTutor, "topic": "Cells"})
	if err != nil {
		t.Fatal(err)
	}
	st := stateOf(t, res.View)
	if st.Mode != modeTutor || st.Topic != "Cells" || st.Kind != kindConcept {
		t.Fatalf("%+v", st)
	}
	for _, k := range exerciseKinds {
		kind = k
		res, err = actOn(t, a, "tutor_kind", st, map[string]string{"kind": k})
		if err != nil {
			t.Fatal(err)
		}
		st = stateOf(t, res.View)
		res, err = actOn(t, a, "tutor_start", st, nil)
		if err != nil {
			t.Fatal(err)
		}
		st = stateOf(t, res.View)
		e, ok := a.exercise(st.Exercise)
		if !ok || e.Kind != k || e.Plain {
			t.Fatalf("%s: %+v", k, e)
		}
		// Ask for every hint, and one more than there are.
		for i := 0; i <= len(e.Hints); i++ {
			res, err = actOn(t, a, "tutor_hint", st, nil)
			if err != nil {
				t.Fatal(err)
			}
			st = stateOf(t, res.View)
		}
		if st.Hints != len(e.Hints) {
			t.Fatal(st.Hints, len(e.Hints))
		}
		answer := "B"
		if k != kindChoice {
			answer = "my answer"
		}
		if _, err := actOn(t, a, "tutor_answer", st, map[string]string{"answer": "  "}); err == nil {
			t.Fatal("an empty answer was submitted")
		}
		res, err = actOn(t, a, "tutor_answer", st, map[string]string{"answer": answer})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fmt.Sprint(res.View.Sections), "参考答案") {
			t.Fatal("no result page")
		}
	}
	// The last answer was partial: reinforcement from the result page goes to the drafts.
	res, err = actOn(t, a, "reinforce_exercise", st, nil)
	if err != nil || stateOf(t, res.View).Mode != modeDrafts || len(a.st.Drafts) == 0 {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "go_topic", st, map[string]string{"topic": "Cells"}); err != nil {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "reinforce_topic", pageState{Mode: modeWeak, Topic: "Cells"}, nil); err != nil {
		t.Fatal(err)
	}
	// A card page offers practice for that card and changes its topic.
	cs := pageState{Mode: modeCard, Card: id}
	viewOf(t, a, cs)
	res, err = actOn(t, a, "go", cs, map[string]string{"mode": modeTutor, "card": id})
	if err != nil || stateOf(t, res.View).Card != id {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "set_topic", cs, map[string]string{"topic": "  Organelles \n"}); err != nil || a.st.Cards[id].Topic != "Organelles" {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "set_topic", pageState{Mode: modeCard, Card: "gone"}, map[string]string{"topic": "x"}); err == nil {
		t.Fatal("changed a missing card")
	}
	// Without a model: plain exercises and rough judgement still draw.
	h.mu.Lock()
	h.model = nil
	h.mu.Unlock()
	res, err = actOn(t, a, "tutor_start", pageState{Mode: modeTutor, Kind: kindChoice}, nil)
	if err != nil || res.Toast == "" {
		t.Fatal(err)
	}
	st = stateOf(t, res.View)
	if _, err := actOn(t, a, "tutor_answer", st, map[string]string{"answer": "不知道"}); err != nil {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "tutor_kind", st, map[string]string{"kind": "essay"}); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

func TestPageStateBounds(t *testing.T) {
	a := testApp(t, &fakeHub{}, defaults())
	for _, st := range []pageState{
		{Status: "bogus"}, {Kind: "essay"}, {Hints: -1}, {Hints: maxHints + 1}, {Done: -1},
		{Topic: strings.Repeat("知", maxTopic+1)}, {Exercise: strings.Repeat("x", 65)}, {Card: strings.Repeat("c", 65)},
		{Mode: modeGuide, Guide: strings.Repeat("g", 65)}, {Mode: modeGuide, Guide: "g", Section: -1}, {Mode: modeGuide, Guide: "g", Section: maxGuideSection},
		{Mode: modeGuide, Guide: "g", GuideChecked: true, GuideAns: strings.Repeat("答", maxCheckText+1)},
	} {
		if _, err := a.render(t.Context(), request("", st, nil)); err == nil {
			t.Fatalf("%+v was accepted", st)
		}
	}
	// Just inside the bounds is accepted, and draws even though the lesson is not there.
	for _, st := range []pageState{
		{Mode: modeGuide, Guide: strings.Repeat("g", 64), Section: maxGuideSection - 1, GuideChecked: true, GuideAns: strings.Repeat("答", maxCheckText)},
		{Mode: modeMap, Topic: strings.Repeat("知", maxTopic), Status: statusNew},
	} {
		v, err := a.render(t.Context(), request("", st, nil))
		if err != nil {
			t.Fatalf("%+v was refused: %v", st, err)
		}
		checkPage(t, "bounds", v)
	}
	if _, err := a.action(t.Context(), request("go", pageState{}, map[string]string{"mode": "nowhere"})); err == nil {
		t.Fatal("an unknown page")
	}
	if _, err := a.action(t.Context(), request("nothing", pageState{}, nil)); err == nil {
		t.Fatal("an unknown action")
	}
}

func TestViewsConcurrent(t *testing.T) {
	// Pages render while tools, reviews and practice change the state; run with -race.
	h := &fakeHub{model: modelBy(map[string]string{"study.exercise": choiceExercise, "study.guide": guideJSON})}
	a := testApp(t, h, defaults())
	var cards []cardText
	for i := 0; i < 30; i++ {
		cards = append(cards, cardText{Front: fmt.Sprintf("Q%d", i), Back: "A", Topic: fmt.Sprintf("T%d", i%4)})
	}
	a.addDirect(cards, Source{Kind: srcManual})
	ids := []string{}
	for _, c := range a.st.sortedCards() {
		ids = append(ids, c.ID)
	}
	lesson, err := a.guide(context.Background(), "T0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				for _, st := range []pageState{{Mode: modeHome}, {Mode: modeWeak}, {Mode: modeCards}, {Mode: modeTutor}, {Mode: modeReview},
					{Mode: modeGuide}, {Mode: modeGuide, Guide: lesson.ID, Section: i % 4}, {Mode: modeGuide, Guide: lesson.ID, Section: 2, GuideChecked: true, GuideAns: "ATP"},
					{Mode: modeMap}, {Mode: modeMap, Status: statusNew}, {Mode: modeMap, Topic: fmt.Sprintf("T%d", g)}} {
					v, err := a.view(context.Background(), st, "")
					if err == nil {
						err = wire.CheckView(v)
					}
					if err != nil {
						errs <- err
						return
					}
				}
				if _, err := a.grade(ids[(g*10+i)%len(ids)], fsrs.Good); err != nil {
					errs <- err
					return
				}
				if _, err := a.practice(context.Background(), "", fmt.Sprintf("T%d", g), kindChoice); err != nil {
					errs <- err
					return
				}
				if _, err := a.callTool(context.Background(), "study_weak_points", json.RawMessage(`{}`)); err != nil {
					errs <- err
					return
				}
				if _, err := a.callTool(context.Background(), "study_map", json.RawMessage(fmt.Sprintf(`{"topic":"T%d"}`, (g+i)%4))); err != nil {
					errs <- err
					return
				}
				if _, err := a.callTool(context.Background(), "study_map", json.RawMessage(`{"status":"weak"}`)); err != nil {
					errs <- err
					return
				}
				if _, err := a.callTool(context.Background(), "study_guide", json.RawMessage(fmt.Sprintf(`{"topic":"T%d"}`, g))); err != nil {
					errs <- err
					return
				}
				if _, err := a.guide(context.Background(), "", fmt.Sprintf("text %d %d", g, i), ""); err != nil {
					errs <- err
					return
				}
				if _, err := a.guideDrafts(lesson.ID); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(a.st.Exercises) > maxExercises || len(a.st.Guides) > maxGuides {
		t.Fatal(len(a.st.Exercises), len(a.st.Guides))
	}
}

func TestViewsStateRoundTrip(t *testing.T) {
	// What an action writes into the page state, the next render must read back: long Chinese text is clipped by
	// characters and checked by characters.
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch cmd := strings.Join(args, " "); {
		case strings.HasPrefix(cmd, "--quiet auth check"):
			return []byte(`{}`), nil
		case strings.HasPrefix(cmd, "--quiet artifact list -n nb_1 --type flashcard"):
			return []byte(`{"artifacts":[{"id":"art_f","title":"` + strings.Repeat("卡", 300) + `","status":"completed"}]}`), nil
		case strings.HasPrefix(cmd, "--quiet artifact list -n nb_1 --type quiz"):
			return []byte(`{"artifacts":[{"id":"art_q","title":"Quiz","status":"completed"}]}`), nil
		case cmd == "--quiet list --json":
			return []byte(`{"notebooks":[{"id":"nb_1","title":"` + strings.Repeat("本", 300) + `"}]}`), nil
		}
		return nil, errors.New("unexpected")
	}
	vault, graph := writeVault(t), writeGraph(t)
	h := &fakeHub{node: &nodeWorker{cfg: nodeConfig{Vault: vault, Logseq: graph}, look: func() string { return "/bin/notebooklm" }, run: run}}
	a := testApp(t, h, config{Machines: []string{"mac"}})
	long := strings.Repeat("搜", 500)
	for _, c := range []struct{ mode, action string }{{modeObsidian, "vault_search"}, {modeLogseq, "logseq_search"}, {modeNotion, "notion_search"}} {
		res, err := actOn(t, a, c.action, pageState{Mode: c.mode}, map[string]string{"q": long})
		if err != nil {
			t.Fatal(c.action, err)
		}
		if _, err := a.render(t.Context(), request("", stateOf(t, res.View), nil)); err != nil {
			t.Fatalf("%s: the state it wrote is refused: %v", c.action, err)
		}
	}
	// Notebooks and their sets, with titles at the limit, and back.
	res, err := actOn(t, a, "go", pageState{}, map[string]string{"mode": modeNLM})
	if err != nil {
		t.Fatal(err)
	}
	res, err = actOn(t, a, "nlm_open", stateOf(t, res.View), map[string]string{"id": "nb_1", "title": strings.Repeat("本", 300)})
	if err != nil {
		t.Fatal(err)
	}
	st := stateOf(t, res.View)
	if _, err := a.render(t.Context(), request("", st, nil)); err != nil || st.Notebook != "nb_1" {
		t.Fatal(err)
	}
	if _, err := actOn(t, a, "nlm_back", st, nil); err != nil {
		t.Fatal(err)
	}
	// Lists of real notes and pages.
	viewOf(t, a, pageState{Mode: modeObsidian})
	if v := viewOf(t, a, pageState{Mode: modeLogseq}); !strings.Contains(fmt.Sprint(v.Sections), "lang/rust") {
		t.Fatal("the Logseq page list")
	}
	if _, err := actOn(t, a, "import_logseq", pageState{Mode: modeLogseq}, nil); err != nil {
		t.Fatal(err)
	}
	// A long quick-question answer is clipped by characters, and a section moved past either end stays in range.
	gh := &fakeHub{model: modelBy(map[string]string{"study.guide": guideJSON})}
	b := testApp(t, gh, defaults())
	res, err = actOn(t, b, "guide_make", pageState{Mode: modeGuide}, map[string]string{"text": strings.Repeat("材", maxGuideText), "title": strings.Repeat("题", 500)})
	if err != nil {
		t.Fatal(err)
	}
	st = stateOf(t, res.View)
	for _, step := range []struct {
		action string
		values map[string]string
	}{{"guide_check", map[string]string{"answer": strings.Repeat("答 ", 3000)}}, {"guide_next", nil}, {"guide_next", nil}, {"guide_next", nil},
		{"guide_reveal", nil}, {"guide_prev", nil}, {"guide_prev", nil}, {"guide_prev", nil}, {"guide_check", map[string]string{"answer": "线粒体"}}} {
		res, err := actOn(t, b, step.action, st, step.values)
		if err != nil {
			t.Fatal(step.action, err)
		}
		st = stateOf(t, res.View)
		if _, err := b.render(t.Context(), request("", st, nil)); err != nil {
			t.Fatalf("%s: the state it wrote is refused: %v", step.action, err)
		}
	}
	if st.Section != 0 || !st.GuideChecked || st.GuideAns != "线粒体" {
		t.Fatalf("%+v", st)
	}
	for _, f := range []string{"all", statusWeak, statusNew} {
		res, err := actOn(t, b, "weak_filter", pageState{Mode: modeMap}, map[string]string{"status": f})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.render(t.Context(), request("", stateOf(t, res.View), nil)); err != nil {
			t.Fatal(err)
		}
	}
	// Forged ids are refused rather than clipped into another id.
	if _, err := a.action(t.Context(), request("open_card", pageState{}, map[string]string{"id": strings.Repeat("c", 65)})); err == nil {
		t.Fatal("a forged card id")
	}
}

func TestViewsErrorPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	fail := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New(strings.Repeat("错", 3000))
	}
	h := &fakeHub{node: &nodeWorker{cfg: nodeConfig{Vault: "/nonexistent/vault", Logseq: "relative"}, look: func() string { return "/bin/notebooklm" }, run: fail}}
	a := testApp(t, h, config{Machines: []string{"mac"}})
	a.notion = newNotion(srv.URL, "synthetic-secret")
	for _, m := range []string{modeNotion, modeObsidian, modeLogseq, modeNLM} {
		v := viewOf(t, a, pageState{Mode: m})
		if !strings.Contains(fmt.Sprint(v.Sections), "empty") {
			t.Fatalf("%s: %+v", m, v.Sections)
		}
	}
	// The notebook is not signed in: the page says how to sign in.
	viewOf(t, a, pageState{Mode: modeNLM, Notebook: "nb_1", NBTitle: "x"})
}

func TestViewsTutorResults(t *testing.T) {
	long := strings.Repeat("题", maxQuestion)
	options := `["` + strings.Repeat("甲", maxOption) + `","` + strings.Repeat("乙", maxOption) + `","丙","丁"]`
	hints := `["` + strings.Repeat("提", maxHint) + `","b","c"]`
	verdict := `{"verdict":"correct","score":100,"cause":"none","diagnosis":"Spot on.","feedback":"` + strings.Repeat("好", 800) + `"}`
	h := &fakeHub{}
	h.model = func(p wire.ModelCompleteParams) (wire.ModelCompleteResult, error) {
		switch p.Label {
		case "study.exercise":
			if strings.Contains(p.Messages[0].Content, "Kind: choice") {
				return wire.ModelCompleteResult{JSON: json.RawMessage(`{"question":"` + long + `","options":` + options + `,"answer":"A","hints":` + hints + `,"explanation":"` + strings.Repeat("解", 1000) + `"}`)}, nil
			}
			return wire.ModelCompleteResult{JSON: json.RawMessage(`{"question":"` + long + `","answer":"` + strings.Repeat("答", maxAnswerText) + `","hints":` + hints + `}`)}, nil
		case "study.judge":
			return wire.ModelCompleteResult{JSON: json.RawMessage(verdict)}, nil
		}
		return wire.ModelCompleteResult{}, errors.New("no")
	}
	a := testApp(t, h, defaults())
	a.addDirect([]cardText{{Front: "Q", Back: "A", Topic: strings.Repeat("知", maxTopic)}}, Source{Kind: srcNotion, Title: "Page", URL: "https://www.notion.so/page-1"})
	id := a.st.sortedCards()[0].ID
	if v := viewOf(t, a, pageState{Mode: modeCard, Card: id}); !strings.Contains(fmt.Sprint(v.Sections), "https://www.notion.so/page-1") {
		t.Fatal("no link to the source")
	}
	for _, k := range []string{kindChoice, kindConcept} {
		e, err := a.practice(t.Context(), id, "", k)
		if err != nil || e.Plain {
			t.Fatal(k, err)
		}
		st := pageState{Mode: modeTutor, Exercise: e.ID, Kind: k, Hints: 3}
		v := viewOf(t, a, st)
		if k == kindChoice && !strings.Contains(fmt.Sprint(v.Sections), strings.Repeat("甲", maxOption)) {
			t.Fatal("a choice option was cut")
		}
		answer := "A"
		if k == kindChoice {
			answer = "B" // wrong, whatever the model says
		}
		if _, err := a.checkAnswer(t.Context(), e.ID, answer, 3); err != nil {
			t.Fatal(err)
		}
		v = viewOf(t, a, st)
		hasReinforce := false
		for _, act := range wire.Actions(v) {
			hasReinforce = hasReinforce || act.ID == "reinforce_exercise"
		}
		if (k == kindChoice) != hasReinforce {
			t.Fatalf("%s: reinforce button %v", k, hasReinforce)
		}
	}
	for _, fam := range []string{wire.WidgetFamilyMedium, wire.WidgetFamilyLarge} {
		w, err := a.renderWidget(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: widgetID, Placement: wire.PlacementWidget, Size: fam, UIVersion: 4}})
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, "widget "+fam, w)
	}
}

// rows are the list rows of a view.
func rows(v *wire.View) []wire.Component {
	var out []wire.Component
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == wire.ComponentRow {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestViewsGuideStates(t *testing.T) {
	h := &fakeHub{model: modelBy(map[string]string{"study.guide": guideJSON})}
	a := testApp(t, h, defaults())
	// The start page with nothing to make a lesson from.
	if v := viewOf(t, a, pageState{Mode: modeGuide}); len(rows(v)) != 0 || !strings.Contains(pageText(v), "guide_make") {
		t.Fatal(pageText(v))
	}
	// With more knowledge points and lessons than the start page lists.
	var cards []cardText
	for i := 0; i < guideTopicRows+5; i++ {
		cards = append(cards, cardText{Front: fmt.Sprintf("Q%d", i), Back: "A", Topic: fmt.Sprintf("Topic %d", i)})
	}
	cards = append(cards, cardText{Front: "Powerhouse of the cell?", Back: "Mitochondrion", Topic: "Cells"})
	a.addDirect(cards, Source{Kind: srcManual})
	var lesson Guide
	for i := 0; i < guideRecentRows+3; i++ {
		setNow(a, t0.Add(time.Duration(i)*time.Minute))
		g, err := a.guide(t.Context(), "", fmt.Sprintf("material %d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		lesson = g
	}
	v := viewOf(t, a, pageState{Mode: modeGuide})
	if n := len(rows(v)); n != guideTopicRows+guideRecentRows {
		t.Fatal(n)
	}
	if !strings.Contains(pageText(v), fmt.Sprintf("共 %d 个", guideTopicRows+6)) {
		t.Fatal("no note of the knowledge points not listed")
	}
	// First, middle and last section, each with its quick question open, answered right, wrong, or revealed.
	for sec := 0; sec < len(lesson.Sections); sec++ {
		for _, st := range []pageState{
			{Mode: modeGuide, Guide: lesson.ID, Section: sec},
			{Mode: modeGuide, Guide: lesson.ID, Section: sec, GuideChecked: true, GuideAns: "线粒体"},
			{Mode: modeGuide, Guide: lesson.ID, Section: sec, GuideChecked: true, GuideAns: "ATP"},
			{Mode: modeGuide, Guide: lesson.ID, Section: sec, GuideChecked: true, GuideAns: "完全不对的回答"},
			{Mode: modeGuide, Guide: lesson.ID, Section: sec, GuideChecked: true},
			{Mode: modeGuide, Guide: lesson.ID, Section: sec, GuideChecked: true, GuideAns: strings.Repeat("答", maxCheckText)},
		} {
			v := viewOf(t, a, st)
			if !strings.Contains(pageText(v), fmt.Sprintf("第 %d / 3 节", sec+1)) {
				t.Fatalf("%+v: %s", st, pageText(v))
			}
		}
	}
	right := pageText(viewOf(t, a, pageState{Mode: modeGuide, Guide: lesson.ID, Section: 0, GuideChecked: true, GuideAns: "线粒体"}))
	wrong := pageText(viewOf(t, a, pageState{Mode: modeGuide, Guide: lesson.ID, Section: 0, GuideChecked: true, GuideAns: "叶绿体"}))
	revealed := pageText(viewOf(t, a, pageState{Mode: modeGuide, Guide: lesson.ID, Section: 0, GuideChecked: true}))
	if !strings.Contains(right, "答对了") || !strings.Contains(wrong, "不太一样") || strings.Contains(revealed, "答对了") || strings.Contains(revealed, "不太一样") ||
		!strings.Contains(revealed, "参考答案：线粒体") {
		t.Fatal(right, wrong, revealed)
	}
	// Out of range: past the end shows the last section, below zero the first.
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: lesson.ID, Section: maxGuideSection - 1}); !strings.Contains(pageText(v), "第 3 / 3 节") {
		t.Fatal(pageText(v))
	}
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: lesson.ID, Section: -5}); !strings.Contains(pageText(v), "第 1 / 3 节") {
		t.Fatal(pageText(v))
	}
	// A lesson that is gone.
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: "missing", Section: 3, GuideChecked: true, GuideAns: "x"}); !strings.Contains(pageText(v), "已经不在了") {
		t.Fatal(pageText(v))
	}
	// A topic lesson's last section offers the tutor; the plain one made without a model, too, but no cards.
	topical, err := a.guide(t.Context(), "Cells", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: topical.ID, Section: 2}); !strings.Contains(pageText(v), "guide_drafts") || !strings.Contains(pageText(v), "mode:tutor") {
		t.Fatal(pageText(v))
	}
	h.mu.Lock()
	h.model = nil
	h.mu.Unlock()
	plain, err := a.guide(t.Context(), "Topic 3", "", "")
	if err != nil || !plain.Plain {
		t.Fatal(err)
	}
	for _, st := range []pageState{
		{Mode: modeGuide, Guide: plain.ID},
		{Mode: modeGuide, Guide: plain.ID, GuideChecked: true, GuideAns: "A"},
		{Mode: modeGuide, Guide: plain.ID, GuideChecked: true},
	} {
		v := viewOf(t, a, st)
		if p := pageText(v); !strings.Contains(p, "模型暂时不可用") || strings.Contains(p, "guide_drafts") || !strings.Contains(p, "mode:tutor") {
			t.Fatal(p)
		}
	}
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: plain.ID, GuideChecked: true, GuideAns: "A"}); !strings.Contains(pageText(v), "答对了") {
		t.Fatal(pageText(v))
	}
	// The start page marks the plain lesson.
	if v := viewOf(t, a, pageState{Mode: modeGuide}); !strings.Contains(pageText(v), "按卡片做的") {
		t.Fatal(pageText(v))
	}
	// A lesson whose topic has no cards left offers no tutor.
	for _, c := range a.st.sortedCards() {
		if c.Topic == "Cells" {
			if err := a.update(func(s *state) error { s.Cards[c.ID].Suspended = true; return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if v := viewOf(t, a, pageState{Mode: modeGuide, Guide: topical.ID, Section: 2}); strings.Contains(pageText(v), "mode:tutor") {
		t.Fatal(pageText(v))
	}
}

func TestViewsGuideLongFields(t *testing.T) {
	long := strings.Repeat("长", 5000)
	var secs []string
	for i := 0; i < 8; i++ {
		points := strings.TrimSuffix(strings.Repeat(mustJSON(long)+",", 7), ",")
		secs = append(secs, fmt.Sprintf(`{"title":%s,"intuition":%s,"analogy":%s,"key_points":[%s],"micro_check":{"question":%s,"answer":%s,"accepted":[%s,%s]}}`,
			mustJSON(long), mustJSON(long), mustJSON(long), points, mustJSON(long), mustJSON(long), mustJSON(long), mustJSON(long)))
	}
	h := &fakeHub{model: modelBy(map[string]string{"study.guide": `{"title":` + mustJSON(long) + `,"sections":[` + strings.Join(secs, ",") + `]}`})}
	a := testApp(t, h, defaults())
	a.addDirect([]cardText{{Front: long, Back: long, Topic: long}}, Source{Kind: srcManual, Title: long})
	topic := a.st.sortedCards()[0].Topic
	for _, g := range []func() (Guide, error){
		func() (Guide, error) { return a.guide(t.Context(), topic, "", "") },
		func() (Guide, error) { return a.guide(t.Context(), "", long, long) },
	} {
		g, err := g()
		if err != nil {
			t.Fatal(err)
		}
		for sec := 0; sec < maxGuideSection; sec++ {
			viewOf(t, a, pageState{Mode: modeGuide, Guide: g.ID, Section: sec})
			viewOf(t, a, pageState{Mode: modeGuide, Guide: g.ID, Section: sec, GuideChecked: true, GuideAns: strings.Repeat("答", maxCheckText)})
			viewOf(t, a, pageState{Mode: modeGuide, Guide: g.ID, Section: sec, GuideChecked: true})
		}
	}
	viewOf(t, a, pageState{Mode: modeGuide})
	// A plain lesson out of 5000-character cards.
	h.mu.Lock()
	h.model = nil
	h.mu.Unlock()
	setNow(a, t0.Add(48*time.Hour))
	g, err := a.guide(t.Context(), topic, "", "")
	if err != nil || !g.Plain {
		t.Fatal(err)
	}
	viewOf(t, a, pageState{Mode: modeGuide, Guide: g.ID})
	viewOf(t, a, pageState{Mode: modeGuide, Guide: g.ID, GuideChecked: true, GuideAns: strings.Repeat("长", maxCheckText)})
	viewOf(t, a, pageState{Mode: modeGuide})
	viewOf(t, a, pageState{Mode: modeMap})
	viewOf(t, a, pageState{Mode: modeMap, Topic: topic})
}

func TestViewsMapStates(t *testing.T) {
	// Empty, with and without a topic or a filter.
	e := testApp(t, &fakeHub{}, defaults())
	for _, st := range []pageState{{Mode: modeMap}, {Mode: modeMap, Status: statusWeak}, {Mode: modeMap, Topic: "Cells"}} {
		if v := viewOf(t, e, st); !strings.Contains(pageText(v), "地图还是空的") {
			t.Fatal(pageText(v))
		}
	}
	a := mapApp(t)
	all := masteryByTopic(a.sched, a.st, a.now())
	// The overview with every filter: a level with no knowledge point says so.
	for _, s := range []string{"", statusWeak, statusShaky, statusSolid, statusNew} {
		v := viewOf(t, a, pageState{Mode: modeMap, Status: s})
		want := len(all)
		if s != "" {
			want = len(filterTopics(all, s, ""))
		}
		if n := len(rows(v)); n != want {
			t.Fatalf("%q: %d rows, want %d", s, n, want)
		}
		if (want == 0) != strings.Contains(pageText(v), "没有这类知识点") {
			t.Fatalf("%q: %s", s, pageText(v))
		}
	}
	// One knowledge point with every filter, and the actions that lead in and out.
	for _, g := range all {
		levels := cardLevels(g)
		for _, s := range []string{"", statusWeak, statusShaky, statusSolid, statusNew} {
			v := viewOf(t, a, pageState{Mode: modeMap, Topic: g.Topic, Status: s})
			want := len(g.Cards)
			if s != "" {
				want = levels[s]
			}
			if n := len(rows(v)); n != min(want, mapCardRows) || v.Title != g.Topic {
				t.Fatalf("%s %q: %d rows, want %d", g.Topic, s, n, want)
			}
			if p := pageText(v); !strings.Contains(p, "guide_open") || !strings.Contains(p, "mode:tutor") || (want == 0) != strings.Contains(p, "没有这类卡片") {
				t.Fatal(p)
			}
		}
	}
	// An unknown topic shows the whole map.
	if v := viewOf(t, a, pageState{Mode: modeMap, Topic: "Nowhere"}); v.Title != "知识地图" || len(rows(v)) != len(all) {
		t.Fatal(pageText(v))
	}
	res, err := actOn(t, a, "go", pageState{}, map[string]string{"mode": modeMap, "topic": "  Cells "})
	if err != nil || stateOf(t, res.View).Topic != "Cells" || res.View.Title != "Cells" {
		t.Fatal(err)
	}
	res, err = actOn(t, a, "weak_filter", stateOf(t, res.View), map[string]string{"status": statusNew})
	if err != nil || stateOf(t, res.View).Status != statusNew || stateOf(t, res.View).Mode != modeMap {
		t.Fatal(err)
	}
	res, err = actOn(t, a, "weak_topic", stateOf(t, res.View), map[string]string{"topic": ""})
	if err != nil || stateOf(t, res.View).Topic != "" || stateOf(t, res.View).Status != "" || res.View.Title != "知识地图" {
		t.Fatal(err)
	}
	res, err = actOn(t, a, "guide_open", stateOf(t, res.View), map[string]string{"topic": "Cells"})
	if err != nil || stateOf(t, res.View).Mode != modeGuide {
		t.Fatal(err)
	}
	// The home page leads to both new pages.
	if p := pageText(viewOf(t, a, pageState{})); !strings.Contains(p, "mode:map") || !strings.Contains(p, "mode:guide") {
		t.Fatal(p)
	}
}

func TestViewsMapLarge(t *testing.T) {
	// 450 knowledge points, one of them with 450 cards, names of 5000 characters.
	a := testApp(t, &fakeHub{}, config{NewPerDay: 200})
	var cards []cardText
	for i := 0; i < 450; i++ {
		cards = append(cards, cardText{Front: fmt.Sprintf("%d %s", i, strings.Repeat("问", 5000)), Back: strings.Repeat("答", 5000), Topic: fmt.Sprintf("%d %s", i, strings.Repeat("知", 5000))})
		cards = append(cards, cardText{Front: fmt.Sprintf("Big %d", i), Back: "A", Topic: "Big"})
	}
	if n, err := a.addDirect(cards, Source{Kind: srcManual, Title: strings.Repeat("源", 5000)}); err != nil || n != 900 {
		t.Fatal(n, err)
	}
	// One write for all the reviews: a file of 900 long cards written per review would only make the test slow.
	a.mu.Lock()
	err := a.update(func(s *state) error {
		for i, c := range s.sortedCards() {
			if i%5 == 0 {
				continue
			}
			if _, err := a.sched.grade(s, c.ID, []fsrs.Rating{fsrs.Again, fsrs.Good, fsrs.Easy, fsrs.Hard}[i%4], a.now()); err != nil {
				return err
			}
		}
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	setNow(a, t0.Add(10*24*time.Hour))
	all := masteryByTopic(a.sched, a.st, a.now())
	if len(all) != 451 {
		t.Fatal(len(all))
	}
	for _, s := range []string{"", statusWeak, statusShaky, statusSolid, statusNew} {
		v := viewOf(t, a, pageState{Mode: modeMap, Status: s})
		want := len(all)
		if s != "" {
			want = len(filterTopics(all, s, ""))
		}
		n := len(rows(v))
		if n > maxListRows || n != min(want, maxListRows) {
			t.Fatalf("%q: %d rows of %d", s, n, want)
		}
		if hidden := want - n; (hidden > 0) != strings.Contains(pageText(v), fmt.Sprintf("还有 %d 个没列出", hidden)) {
			t.Fatalf("%q: no note of %d not listed", s, hidden)
		}
		for _, sec := range v.Sections {
			if len(sec.Items) > wire.ItemsMost {
				t.Fatal(len(sec.Items))
			}
		}
	}
	if v := viewOf(t, a, pageState{Mode: modeMap}); !strings.Contains(pageText(v), "没列出") {
		t.Fatal("no not-listed caption")
	}
	for _, s := range []string{"", statusWeak, statusShaky, statusSolid, statusNew} {
		v := viewOf(t, a, pageState{Mode: modeMap, Topic: "Big", Status: s})
		if n := len(rows(v)); n > mapCardRows {
			t.Fatal(n)
		}
	}
	if !strings.Contains(pageText(viewOf(t, a, pageState{Mode: modeMap, Topic: "Big"})), fmt.Sprintf("还有 %d 张没列出", 450-mapCardRows)) {
		t.Fatal("no note of the cards not listed")
	}
	for _, g := range all[:5] {
		viewOf(t, a, pageState{Mode: modeMap, Topic: g.Topic})
	}
	viewOf(t, a, pageState{Mode: modeGuide})
	if out, err := a.callTool(t.Context(), "study_map", json.RawMessage(`{"limit":20}`)); err != nil || strings.Count(out, "\n- \"") != 20 {
		t.Fatal(err, len(out))
	}
	// Concurrent renders of the large map while grading.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	ids := a.st.sortedCards()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				for _, st := range []pageState{{Mode: modeMap}, {Mode: modeMap, Topic: "Big"}, {Mode: modeMap, Status: statusWeak}, {Mode: modeGuide}} {
					v, err := a.view(context.Background(), st, "")
					if err == nil {
						err = wire.CheckView(v)
					}
					if err != nil {
						errs <- err
						return
					}
				}
				if _, err := a.grade(ids[g*10+i].ID, fsrs.Good); err != nil {
					errs <- err
					return
				}
				if _, err := a.callTool(context.Background(), "study_map", json.RawMessage(`{"topic":"Big","status":"new"}`)); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
