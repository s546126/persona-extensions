package memsrc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// All texts are placeholders.

// sized is a candidate whose text is n bytes (rounded down to whole characters) of the idx-th run of CJK characters,
// so no two candidates are alike.
func sized(idx int, id, bucket string, l1 bool, score, n int) Candidate {
	var b strings.Builder
	base := rune(0x4e00 + idx*40)
	for i := 0; b.Len()+3 <= n; i++ {
		b.WriteRune(base + rune(i%40))
	}
	kind := entry.KindUnderstanding
	if bucket == BucketContract {
		kind = entry.KindRules
	}
	return Candidate{ID: id, Kind: kind, Bucket: bucket, L1: l1, Score: score, Text: b.String()}
}

// resident_bytes is bounded to the hub's 4/8/16 KB rooms.
func TestClampResidentBytes(t *testing.T) {
	for in, want := range map[int]int{0: ResidentBytes, -5: ResidentBytes, 100: MinResidentBytes, 1400: 1400, 4096: 4096,
		8192: 8192, 16384: 16384, 20000: MaxResidentBytes} {
		if got := ClampResidentBytes(in); got != want {
			t.Errorf("ClampResidentBytes(%d) = %d, want %d", in, got, want)
		}
	}
}

// In 4 KB the six profile-core lines and at least nine of eleven core principles fit beside the contracts (the
// rehearsal's sizes: profile 110-190 B, principles 110-320 B, contracts 80-170 B); 8 and 16 KB hold more, never over.
func TestResidentSharesFitProfileCoreAndPrinciples(t *testing.T) {
	var cs []Candidate
	for i, n := range []int{136, 169, 114, 193, 135, 156} {
		cs = append(cs, sized(i, fmt.Sprintf("prof%d", i), BucketProfile, true, 200-i, n-12+len(ProfileTag)))
	}
	for i, n := range []int{230, 200, 320, 111, 108, 140, 150, 130, 125, 120, 135} {
		cs = append(cs, sized(10+i, fmt.Sprintf("princ%d", i), BucketPrinciple, true, 150-i, n))
	}
	for i := 0; i < 34; i++ {
		c := sized(30+i, fmt.Sprintf("cont%d", i), BucketContract, true, 140-i, 90+(i*13)%80)
		c.Polarity = []string{"check", "act", "communicate", "route"}[i%4]
		c.Hard = i < 5
		cs = append(cs, c)
	}
	for _, room := range []int{4096, 8192, 16384} {
		got := ResidentWithin(cs, room, ResidentMostFor(room))
		n := map[string]int{}
		bytes := map[string]int{}
		total, hard := 0, 0
		for _, c := range got {
			n[c.Bucket]++
			sz := len(c.Text) + residentOverhead
			bytes[c.Bucket] += sz
			total += sz
			if strings.HasPrefix(c.Text, HardTag) {
				hard++
			}
		}
		if total > room {
			t.Errorf("room %d: %d bytes", room, total)
		}
		if n[BucketProfile] != 6 {
			t.Errorf("room %d: profile lines = %d, want all 6 (%v)", room, n[BucketProfile], n)
		}
		if n[BucketPrinciple] < 9 {
			t.Errorf("room %d: principle lines = %d, want >= 9 (%v)", room, n[BucketPrinciple], n)
		}
		if n[BucketContract] == 0 || hard == 0 || hard > HardContractLimit {
			t.Errorf("room %d: contracts = %d, hard tags = %d", room, n[BucketContract], hard)
		}
		if room == 4096 && bytes[BucketContract] < int(float64(room)*0.28) {
			t.Errorf("4 KB: contracts squeezed to %d B", bytes[BucketContract])
		}
		if room == 16384 && n[BucketPrinciple] != 11 {
			t.Errorf("16 KB: principle lines = %d, want all 11", n[BucketPrinciple])
		}
	}
}

// The section tags are the short forms; the old long forms are still stripped.
func TestStandingTagsShort(t *testing.T) {
	if StandingPrefix(BucketProfile, false, "") != "[画]" || StandingPrefix(BucketPrinciple, false, "") != "[原]" ||
		StandingPrefix(BucketContract, true, "check") != "" || HardTag != "[硬]" {
		t.Error("standing tags")
	}
	for _, tag := range []string{ProfileTag, PrincipleTag, HardTag} {
		if len(tag) != 5 {
			t.Errorf("tag %q is %d bytes, want 5", tag, len(tag))
		}
	}
	for _, s := range []string{"[原]先小样本验证", "【原则】先小样本验证", "【画像】先小样本验证", "[硬]先小样本验证"} {
		if StripStandingTag(s) != "先小样本验证" {
			t.Errorf("StripStandingTag(%q) = %q", s, StripStandingTag(s))
		}
	}
}

// Cold start: three wordings of "逐条对照验收标准给证据" stand as one line; different rules that merely share a few words
// both stand.
func TestColdStartDedupNearDuplicateContracts(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{}}
	add := func(id, text string, count int) {
		st.Entries[id] = &entry.Entry{ID: id, Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: text, Count: count, Last: "2026-11-25", Tagged: true,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAsk, Ref: "a/" + id, Day: "2026-11-25", Who: entry.WhoSelf, Quote: "q " + id}}}
	}
	add("i-aaaaaaaa01", "用户要求交付时先给一句话结论，再逐条列证据，并按验收标准说明证据（PR 链接、CI 结果、截图、线上验证命令和输出）；做不到的要说清原因和需要谁决定。", 5)
	add("i-aaaaaaaa02", "用户要求未满足的验收项照实写，并说明原因、补齐条件、补齐时是否需要人管；做不到的要说清为什么、要谁决定，逐条给证据。", 4)
	add("i-aaaaaaaa03", "用户要求完成后逐条对照验收标准说明证据，做不到的要说清楚原因和需要谁来决定。", 3)
	add("i-aaaaaaaa04", "用户要求写长文前先给大纲或骨架并对齐，不接受从素材直接跳到草稿。", 3)
	add("i-aaaaaaaa05", "用户在验收中要求没验证到的项必须写明，不能含糊带过。", 3)
	add("i-aaaaaaaa06", "用户反感排版里的 AI 味，要求用自然段落与清晰脉络，不滥用章节标题。", 3)

	res, _, err := PageWith(st, nil, nil, Params{Contribution: "memory", Limit: 50, ResidentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	stood := map[string]bool{}
	for _, e := range res.Entries {
		if e.Standing {
			stood[e.ID] = true
		}
	}
	dups := 0
	for _, id := range []string{"i-aaaaaaaa01", "i-aaaaaaaa02", "i-aaaaaaaa03"} {
		if stood[id] {
			dups++
		}
	}
	if dups != 1 {
		t.Errorf("acceptance-evidence wordings standing = %d, want 1 (%v)", dups, stood)
	}
	for _, id := range []string{"i-aaaaaaaa04", "i-aaaaaaaa05", "i-aaaaaaaa06"} {
		if !stood[id] {
			t.Errorf("distinct rule %s was deduplicated away (%v)", id, stood)
		}
	}
	if coldAlike("用户要求助手先给结论", "用户要求助手先跑测试") {
		t.Error("two short different rules read as one")
	}
}
