package main

import (
	"strings"
	"testing"
)

// A machine filter's label fits the web's select (at most 200 px, 162 px of it for 13 px text): the disk use comes
// first, so a long name loses its end and the number is never cut off, and the hub's own computer keeps its whole name.
func TestDiskChipLabelKeepsTheNumberAndFits(t *testing.T) {
	local := &Machine{ID: "local", Name: "local (example-host)", IsLocal: true}
	cases := []struct {
		ms   MachineStatus
		want string
	}{
		{MachineStatus{Machine: local, TotalBytes: 1 << 40, UsedPercent: 50.2}, "50% · 运行 Persona 的电脑"},
		{MachineStatus{Machine: local}, "运行 Persona 的电脑"},
		{MachineStatus{Machine: &Machine{ID: "d", Name: "build-box-2", Host: "192.0.2.13"}, TotalBytes: 1, UsedPercent: 41}, "41% · build-box-2"},
		{MachineStatus{Machine: &Machine{ID: "w", Name: "build-server-in-the-basement-rack-7"}, TotalBytes: 1, UsedPercent: 100}, "100% · build-server-in-t…"},
		{MachineStatus{Machine: &Machine{ID: "c", Name: "实验室里那台很旧的图形工作站"}, TotalBytes: 1, UsedPercent: 9}, "9% · 实验室里那台很旧的…"},
	}
	for _, c := range cases {
		got := diskChipLabel(c.ms)
		if got != c.want {
			t.Errorf("diskChipLabel(%s) = %q, want %q", c.ms.Machine.Name, got, c.want)
		}
		if w := chipWidth(got); w > chipUnits {
			t.Errorf("%q is %d units, over %d", got, w, chipUnits)
		}
	}
}

func TestChipFit(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 25, "short"},
		{"  padded  ", 25, "padded"},
		{"abcdefghij", 6, "abcde…"},
		{"一二三四五", 6, "一二…"}, // 2 + 2 + the ellipsis; a third wide rune would overflow
		{"ab一二三", 6, "ab一…"},
		{"ＡＢＣ", 4, "Ａ…"}, // full-width forms count 2
		{"exactly-25-units-long-xyz", 25, "exactly-25-units-long-xyz"},
	} {
		if got := chipFit(c.in, c.max); got != c.want {
			t.Errorf("chipFit(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if got := chipFit(c.in, c.max); chipWidth(got) > c.max {
			t.Errorf("chipFit(%q, %d) = %q is %d units", c.in, c.max, got, chipWidth(got))
		}
	}
	if strings.Count(chipFit(strings.Repeat("x", 100), chipUnits), "…") != 1 {
		t.Fatal("a cut label ends in one ellipsis")
	}
}
