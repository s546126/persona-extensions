package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The detection corpus (testdata/detect.jsonl): prompts labeled by whether the gate should treat them as written in
// another language. want true: the user wrote it in another language (short replies and mixed technical Chinese
// included); want false: English, even when it quotes, names or asks to translate text in another language.

type corpusCase struct {
	Cat  string `json:"cat"`
	Want bool   `json:"want"`
	Text string `json:"text"`
}

func loadCorpus(t *testing.T, name string) []corpusCase {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cs []corpusCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c corpusCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return cs
}

func TestDetectCorpus(t *testing.T) {
	for _, c := range []struct {
		file            string
		maxWrong, maxFP int
	}{
		{"testdata/detect.jsonl", maxWrong, maxFalsePositives},
		{"testdata/holdout.jsonl", maxWrong, maxFalsePositives},
	} {
		t.Run(c.file, func(t *testing.T) { checkCorpus(t, c.file, c.maxWrong, c.maxFP) })
	}
}

func checkCorpus(t *testing.T, file string, maxWrong, maxFalsePositives int) {
	cs := loadCorpus(t, file)
	type tally struct{ ok, n int }
	byCat := map[string]*tally{}
	var ok, fp, fn int
	for _, c := range cs {
		got := isNonEnglish(c.Text)
		tl := byCat[c.Cat]
		if tl == nil {
			tl = &tally{}
			byCat[c.Cat] = tl
		}
		tl.n++
		switch {
		case got == c.Want:
			ok++
			tl.ok++
		case got:
			fp++
			t.Logf("false positive [%s]: %q", c.Cat, c.Text)
		default:
			fn++
			t.Logf("missed [%s]: %q", c.Cat, c.Text)
		}
	}
	cats := make([]string, 0, len(byCat))
	for k := range byCat {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	for _, k := range cats {
		t.Logf("%-10s %d/%d", k, byCat[k].ok, byCat[k].n)
	}
	t.Logf("accuracy %d/%d (%.1f%%), false positives %d, missed %d", ok, len(cs), 100*float64(ok)/float64(len(cs)), fp, fn)
	if fp > maxFalsePositives || len(cs)-ok > maxWrong {
		t.Errorf("detection regressed: %d wrong (at most %d), %d false positives (at most %d)", len(cs)-ok, maxWrong, fp, maxFalsePositives)
	}
}

// What the corpus may still get wrong. Lower these as the detector improves; never raise them to make a change pass.
const (
	maxWrong          = 0
	maxFalsePositives = 0
)
