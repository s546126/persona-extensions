package ui

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/spaces/internal/templates"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// jargon are the schema words the pages used to show. Each has an everyday word now (see words.go); none of them
// may come back in what a user reads.
var jargon = []string{
	"类型", "属性", "关联", "结构", "生命周期", "阶段", "流程", "视图", "汇总", "衍生", "标识", "字段", "记录", "本体",
	"主行", "进度分母", "经由", "反向名称", "源自", "指向", "主线",
}

// quoted are the names between 「 and 」: the user's own words, which the pages repeat as they are.
var quoted = regexp.MustCompile(`「[^」]*」|《[^》]*》`)

// fieldValue drops the value of an input from a dump line: a choice's value is never shown, only its title.
var fieldValueRe = regexp.MustCompile(`(\[输入 [^\]]*\][^\n]*?) 值：[^\n]*`)

// latin finds words in Latin letters.
var latin = regexp.MustCompile(`[A-Za-z]{2,}`)

// address finds the stored web addresses of the fixtures, which the table shows as they are.
var address = regexp.MustCompile(`(https://)?example\.com/[^\s|]*`)

// readable is what a user reads of a page state's dump: quoted names, stored addresses, input values and the type
// names a template row repeats from the template (the project template has a type named 记录) left out.
func readable(text string) string {
	text = address.ReplaceAllString(fieldValueRe.ReplaceAllString(text, "$1"), "")
	for _, tp := range templates.All() {
		if names := templateTypeNames(tp); names != "" {
			text = strings.ReplaceAll(text, "  "+names+"\n", "\n")
		}
	}
	return quoted.ReplaceAllString(text, "「」")
}

// keysIn are the type and property keys of every space, which the editor shows as their codes (代号), and the
// stored values of the fixtures that are addresses or codes.
func keysIn(b *kernel.Base) map[string]bool {
	out := map[string]bool{"https": true, "example": true, "com": true, "film": true, "author": true}
	for _, sp := range b.Spaces {
		for _, t := range sp.Types {
			out[t.Key] = true
			for _, p := range t.Props {
				out[p.Key] = true
			}
		}
	}
	return out
}

// TestPagesUseEverydayWords fails when a schema word or an English word comes back in a rendered page, the card or
// a toast of a refused change.
func TestPagesUseEverydayWords(t *testing.T) {
	allowed := keysIn(sampleBaseFor(t, "reading", "subscription"))
	for _, s := range pageStates(t) {
		text := readable(s.text)
		for _, w := range jargon {
			if strings.Contains(text, w) {
				t.Errorf("%s shows %q:\n%s", s.name, w, s.text)
			}
		}
		for _, w := range latin.FindAllString(text, -1) {
			if !allowed[w] {
				t.Errorf("%s shows the English word %q:\n%s", s.name, w, s.text)
			}
		}
	}
}

// sampleBaseFor is a base of the templates with the keys, with samples.
func sampleBaseFor(t *testing.T, keys ...string) *kernel.Base {
	t.Helper()
	var out *kernel.Base
	if err := storeWith(t, true, keys...).View(func(b *kernel.Base) error { out = b; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every page state the dump reaches states what it is for, its state or what to do next.
func TestFirstRunPagesSayWhatAndWhatNext(t *testing.T) {
	byName := map[string]pageState{}
	for _, s := range pageStates(t) {
		byName[s.name] = s
	}
	want := map[string][]string{
		"01-empty-page":                {"读书清单", "订阅", "第一步", "选一个模板", "助手"},
		"01-empty-card":                {"还没有空间", "选个模板"},
		"02-space-overview":            {"分类", "条目", "助手"},
		"04-blank-space":               {"这个空间还是空的", "新建分类"},
		"04-subscription-tab-empty":    {"还没有订阅", "新建订阅", "助手"},
		"02-tab-book-search-none":      {"没有找到", "换个词"},
		"03-editor-new-type":           {"新建分类", "代号", "例如"},
		"06-error-type-key-bad":        {"小写英文字母开头"},
		"06-error-stage-in-use":        {"先把这个状态里的条目挪到别的状态"},
		"06-error-prop-in-use":         {"删不掉", "先在那里去掉它"},
		"06-error-status-jump":         {"一次只能往前或往后走一步"},
		"06-error-add-empty-title":     {"标题是空的", "再点添加"},
		"06-error-type-name-empty":     {"请填一个名称"},
		"06-error-number-bad":          {"请改好再保存"},
		"02-entity-book":               {"它连到的", "连到它的", "由它新建"},
		"03-editor-book-new-rollup":    {"连到别的条目", "自动统计"},
		"03-editor-note-new-rollup":    {"统计哪些连着的条目"},
		"01-empty-template-form":       {"里面有这几个分类", "先放几条示例", "创建空间"},
		"02-tab-book-quick-add":        {"新的书叫什么", "添加"},
		"05-two-spaces-card":           {"2 个空间"},
		"01-empty-template-form-blank": {"自己从头搭"},
	}
	for name, words := range want {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("no page state %s", name)
		}
		for _, w := range words {
			if !strings.Contains(s.text, w) {
				t.Errorf("%s does not say %q:\n%s", name, w, s.text)
			}
		}
	}
}

// The card's empty state has a button, and it opens the nav page.
func TestEmptyCardOpensThePage(t *testing.T) {
	for _, s := range pageStates(t) {
		if s.name != "01-empty-card" {
			continue
		}
		empty := s.view.Sections[0].Items[0]
		if empty.Type != wire.ComponentEmpty || empty.Button == nil || empty.Button.Action.Push == nil ||
			empty.Button.Action.Push.UI != PageUI {
			t.Fatalf("empty card = %+v", empty)
		}
		return
	}
	t.Fatal("no empty card")
}

// The word tables cover every kind, built-in field and icon, so no kernel or English name falls through.
func TestWordTablesAreComplete(t *testing.T) {
	for _, k := range kernel.Kinds {
		if _, ok := kindWords[k.Name]; !ok {
			t.Errorf("kind %s has no everyday word", k.Name)
		}
	}
	for _, key := range builtinKeys {
		if builtinNames[key] == "" {
			t.Errorf("built-in field %s has no everyday name", key)
		}
	}
	for _, name := range wire.NavIcons {
		if iconTitles[name] == "" {
			t.Errorf("icon %s has no Chinese name", name)
		}
	}
	if len(builtinNames) != len(builtinKeys) {
		t.Errorf("builtinNames has %d names for %d fields", len(builtinNames), len(builtinKeys))
	}
}

// plainError rewrites the kernel's words but never the user's own names between 「 and 」, and keeps the hint
// of a hinted error after its cause.
func TestPlainErrorKeepsQuotedNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"类型「类型」没有属性「属性」", "分类「类型」没有填写项「属性」"},
		{"「x」只能关联一条记录", "「x」只能连到一条"},
		{"类型标识「Bad」不合法", "分类代号「Bad」不对。" + keyHint},
		{"「a」不能直接变为「c」；可以变为：b", "「a」不能直接变为「c」。一次只能往前或往后走一步，现在可以改成：b"},
		{"未闭合「阶段", "未闭合「阶段"},
	}
	for _, tc := range cases {
		if got := plainError(errors.New(tc.in)); got != tc.want {
			t.Errorf("plainError(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	base := errors.New("阶段「在读」还有记录")
	h := hinted(base, "先挪走")
	if got := toastOf(h); got != "状态「在读」还有条目。先挪走" || !errors.Is(h, base) {
		t.Errorf("hinted toast = %q", got)
	}
	if !slices.Contains(jargon, "类型") {
		t.Fatal("jargon list lost its first word")
	}
}
