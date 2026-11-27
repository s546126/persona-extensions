package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The page says what it is for and the first thing to try, in every locale and on old and new UI versions, and
// its title matches the manifest's page title.
func TestPageSaysWhatItIsFor(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "港湾的早晨"})
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		for _, version := range []int{1, 3} {
			view, err := a.render(context.Background(), uiReq("", locale, version, nil, pageState{}))
			if err != nil {
				t.Fatal(err)
			}
			checkView(t, view)
			intro := textsFor(locale).intro
			if intro == "" || len(view.Sections) == 0 || len(view.Sections[0].Items) == 0 || view.Sections[0].Items[0].Text != intro {
				t.Fatalf("%s v%d: page does not open with the intro:\n%s", locale, version, viewText(view))
			}
			for _, words := range []string{textsFor(locale).modeTranslation, textsFor(locale).modeVocabulary} {
				if !strings.Contains(intro, words) {
					t.Fatalf("%s intro does not name %q: %s", locale, words, intro)
				}
			}
		}
	}

	raw, err := os.ReadFile("persona-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Description string
		Contributes struct{ UI []struct{ ID, Title string } }
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Contributes.UI) != 1 || m.Contributes.UI[0].Title != textsFor("zh-Hans").title {
		t.Fatalf("manifest page title %+v, page title %q", m.Contributes.UI, textsFor("zh-Hans").title)
	}
	for _, jargon := range []string{"数据目录", "划词"} {
		if strings.Contains(m.Description, jargon) {
			t.Fatalf("description still says %q", jargon)
		}
	}
}

// A failed translation says what to do next, not only that it failed.
func TestTranslationFailureSaysWhatToDo(t *testing.T) {
	for id, text := range locales {
		if !strings.Contains(text.failedBare, "试试") && !strings.Contains(text.failedBare, "again") && !strings.Contains(text.failedBare, "もう一度") {
			t.Fatalf("%s failure text gives no next step: %q", id, text.failedBare)
		}
	}
}
