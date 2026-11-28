package api

import (
	"testing"
	"unicode/utf8"
)

func TestHardeningHostAllowedCaseInsensitiveClipAndUserinfo(t *testing.T) {
	if !hostAllowed("DL.WEWORK.QQ.COM", DefaultMediaHosts) {
		t.Fatal("hostAllowed rejected valid uppercase Tencent media host")
	}
	if !hostAllowed("dl.wework.qq.com.", DefaultMediaHosts) {
		t.Fatal("hostAllowed rejected valid FQDN trailing dot media host")
	}
	if hostAllowed("evilqq.com", DefaultMediaHosts) {
		t.Fatal("hostAllowed accepted suffix spoof without dot boundary")
	}

	if got := clip("abcdef", 4); utf8.RuneCountInString(got) > 4 {
		t.Fatalf("clip(\"abcdef\", 4) = %q (%d runes), want <= 4 runes", got, utf8.RuneCountInString(got))
	}
	if got := clip("abcdef", 0); got != "" {
		t.Fatalf("clip(\"abcdef\", 0) = %q, want empty", got)
	}
	if got := clip("abcdef", -3); got != "" {
		t.Fatalf("clip(\"abcdef\", -3) = %q, want empty", got)
	}

	if _, err := New(Options{BotID: "bot", Secret: "sec", URL: "wss://user:pass@openws.work.weixin.qq.com"}); err == nil {
		t.Fatal("New accepted WebSocket URL with embedded userinfo")
	}
}
