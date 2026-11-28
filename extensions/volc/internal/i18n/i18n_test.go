package i18n

import (
	"testing"
)

// Every language has every key the source (zh-Hans) has, and nothing else.
func TestCatalogsAgree(t *testing.T) {
	src := catalog(ZhHans)
	if len(src) < 20 {
		t.Fatalf("zh-Hans has %d texts", len(src))
	}
	for _, l := range []Lang{En, Ja} {
		c := catalog(l)
		for k := range src {
			if c[k] == "" {
				t.Errorf("%s lacks %s", l, k)
			}
		}
		for k := range c {
			if _, ok := src[k]; !ok {
				t.Errorf("%s has %s, zh-Hans does not", l, k)
			}
		}
	}
}

func TestResolve(t *testing.T) {
	for tag, want := range map[string]Lang{"": "", "zh-CN": ZhHans, "zh_Hant_TW": ZhHans, "en-US": En, "ja": Ja, "JA-jp": Ja, "fr": En, " ko-KR ": En} {
		if got := Resolve(tag); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", tag, got, want)
		}
	}
	if Lang("fr").Or(Default) != ZhHans || En.Or(Default) != En {
		t.Error("Or")
	}
}

func TestT(t *testing.T) {
	if got := T(ZhHans, "volc-check.ok"); got != "已连通" {
		t.Errorf("zh-Hans ok: %q", got)
	}
	if got := T("xx", "volc-check.ok"); got != "已连通" {
		t.Errorf("unknown language: %q", got)
	}
	if got := T(En, "volc-check.no_such_key"); got != "volc-check.no_such_key" {
		t.Errorf("missing key: %q", got)
	}
	if got := T(ZhHans, "volc-check.latency", Args{"ms": int64(42)}); got != "42 毫秒" {
		t.Errorf("latency: %q", got)
	}
	if got := T(ZhHans, "volc-check.failed_with", Args{"service": "识别", "why": "出错了。"}); got != "识别：出错了。" {
		t.Errorf("failed_with: %q", got)
	}
	// The value's full stop goes when the text's own punctuation follows; an unknown placeholder stays.
	if got := fill("（{why}），{other}{", Args{"why": "坏了。"}); got != "（坏了），{other}{" {
		t.Errorf("fill: %q", got)
	}
	if got := fill("{a}.{a}", Args{"a": "x..."}); got != "x....x..." {
		t.Errorf("ellipsis: %q", got)
	}
}
