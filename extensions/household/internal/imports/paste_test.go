package imports

import (
	"strings"
	"testing"
)

func TestParsePaste(t *testing.T) {
	text := strings.Join([]string{
		"牛奶 2 盒",
		"鸡蛋 12 个 2026-12-05",
		"",
		"- 苹果1.5斤",
		"2. 酱油 两瓶",
		"纸巾x3",
		"十个鸡蛋",
		"２盒酸奶",
		"番茄 3",
		"五花肉 半斤 ￥18.5",
		"维生素C 100片",
		"面包 12月3日到期",
		"洗洁精",
		"十三香",
		"!!!!",
		"12345",
		"2 盒",
		"今天去超市买了很多东西，记得放冰箱。",
		"酸奶 2026-02-30",
	}, "\n")
	lines, unread, notes := ParsePaste(text, "2026-11-26")
	type want struct {
		title, unit, expires string
		qty                  float64 // 0 = none
	}
	wants := []want{
		{"牛奶", "盒", "", 2},
		{"鸡蛋", "个", "2026-12-05", 12},
		{"苹果", "斤", "", 1.5},
		{"酱油", "瓶", "", 2},
		{"纸巾", "", "", 3},
		{"鸡蛋", "个", "", 10},
		{"酸奶", "盒", "", 2},
		{"番茄", "", "", 3},
		{"五花肉", "斤", "", 0.5},
		{"维生素C", "片", "", 100},
		{"面包", "", "2026-12-03", 0},
		{"洗洁精", "", "", 0},
		{"十三香", "", "", 0},
	}
	if len(lines) != len(wants) {
		t.Fatalf("%d lines read: %+v", len(lines), lines)
	}
	for i, w := range wants {
		l := lines[i]
		q := 0.0
		if l.Qty != nil {
			q = *l.Qty
		}
		if l.Title != w.title || l.Unit != w.unit || l.Expires != w.expires || q != w.qty {
			t.Fatalf("line %d: %+v (qty %v), want %+v", i+1, l, q, w)
		}
	}
	if p := lines[8].Price; p == nil || *p != 18.5 {
		t.Fatalf("price %v", p)
	}
	wantUnread := []string{"!!!!", "12345", "2 盒", "今天去超市买了很多东西，记得放冰箱。", "酸奶 2026-02-30"}
	if len(unread) != len(wantUnread) {
		t.Fatalf("unread %+v", unread)
	}
	for i, u := range unread {
		if u.Text != wantUnread[i] || u.Why == "" {
			t.Fatalf("unread %d: %+v, want %q with a reason", i, u, wantUnread[i])
		}
	}
	// Lines are numbered over the non-blank lines.
	if unread[0].Line != 14 {
		t.Fatalf("unread line number %d", unread[0].Line)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "按今年") {
		t.Fatalf("notes %v", notes)
	}
}

func TestHanNumbers(t *testing.T) {
	for s, want := range map[string]float64{"一": 1, "两": 2, "十": 10, "十二": 12, "二十五": 25, "一百二十": 120, "半": 0.5} {
		if got, ok := hanNumber(s); !ok || got != want {
			t.Fatalf("%s: %v %v", s, got, ok)
		}
	}
	if _, ok := hanNumber("三三"); ok {
		t.Fatal("三三 read as a number")
	}
}

func TestPasteBecomesTheSameDraftAsTheTool(t *testing.T) {
	fx := newFixture(t)
	lines, unread, _ := ParsePaste("牛奶 2 盒\n???", testToday)
	p := plan(t, fx, Input{Source: SrcPaste, Lines: lines, Unread: unread})
	if s := step(t, p, 1); s.Action != ActAddToItem || *s.After != 3 {
		t.Fatalf("step %+v", s)
	}
	if p.Totals.Unread != 1 || !strings.Contains(Summary(SrcPaste, p), "没读懂") {
		t.Fatalf("summary %q", Summary(SrcPaste, p))
	}
}

func TestPasteBeyondTheLimitIsOneUnreadLine(t *testing.T) {
	text := strings.Repeat("牛奶 1 盒\n", PasteMost+500)
	lines, unread, _ := ParsePaste(text, "2026-11-26")
	if len(lines) != PasteMost || len(unread) != 1 || !strings.Contains(unread[0].Text, "500 行") {
		t.Fatalf("%d lines, unread %+v", len(lines), unread)
	}
}
