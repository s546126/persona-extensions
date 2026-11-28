package entry

import (
	"slices"
	"testing"
)

// Known legal_id false positives (a Visa card payment, "告状") are recognised; a real visa, H1B, passport or lawsuit
// never is, even next to a card payment.
func TestLegalIDFalsePositive(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"domestic_visa_card_payment", "user-b 的第一个号用 Google Play 付款，用的是国内 visa 和日本账号。", true},
		{"visa_card_suffix", "订阅用 visa 卡扣款", true},
		{"complaint_not_lawsuit", "用户在拒绝一个面试后遭到对方告状，感到烦躁。", true},
		{"real_visa_validity", "用户目前签证仍在有效期内，回国安排受 10 月 H1B 影响的判断牵制。", false},
		{"real_passport", "用户当前为 L-1B，visa 盖章今年到期，护照明年到期。", false},
		{"card_and_h1b_visa", "用 visa 卡付了 H1B visa application 的费用", false},
		{"lawsuit", "对方发来律师函，准备起诉", false},
		{"unrelated", "项目周会讨论排期", false},
	} {
		if got := LegalIDFalsePositive(tc.text); got != tc.want {
			t.Errorf("%s: LegalIDFalsePositive(%q) = %v, want %v", tc.name, tc.text, got, tc.want)
		}
	}
}

// "整理财务" is bookkeeping, not "理财" (personal wealth management); real personal finance still is finance.
func TestPayloadFinanceIgnoresBookkeepingCrossWord(t *testing.T) {
	for _, txt := range []string{
		"在整理财务账目时，助手按用户指定的口径、对象归类和合并方式计算，核对借贷方向与往来余额，不漏记不误判。",
		"整理财务收支或核对流水",
		"处理财务报表前先对齐口径",
	} {
		if cats := PayloadCategories(Interaction, txt); slices.Contains(cats, CatFinance) {
			t.Errorf("bookkeeping rule %q read as finance: %v", txt, cats)
		}
	}
	for _, txt := range []string{"用户在做个人理财规划", "用户每月定投指数基金", "用户把一部分工资拿去理财"} {
		if cats := PayloadCategories(Persona, txt); !slices.Contains(cats, CatFinance) {
			t.Errorf("personal finance %q not finance: %v", txt, cats)
		}
	}
}

// LOW_CONFIDENCE entries and single one-off persona sightings are unreliable; the user's own entries never are.
func TestIsUnreliableOneOff(t *testing.T) {
	low := &Entry{Facet: Persona, Count: 1, Flags: []string{FlagLowConfidence, "standalone"}, Leverage: &Leverage{Reach: ReachOneOff}}
	if !IsUnreliableOneOff(low) {
		t.Error("LOW_CONFIDENCE one-off persona entry should be unreliable")
	}
	lowWorld := &Entry{Facet: World, Count: 3, Flags: []string{FlagLowConfidence}}
	if !IsUnreliableOneOff(lowWorld) {
		t.Error("LOW_CONFIDENCE world entry should be unreliable")
	}
	oneOff := &Entry{Facet: Persona, Count: 1, Leverage: &Leverage{Reach: ReachOneOff}}
	if !IsUnreliableOneOff(oneOff) {
		t.Error("single one_off persona sighting should be unreliable")
	}
	seenTwice := &Entry{Facet: Persona, Count: 2, Leverage: &Leverage{Reach: ReachOneOff}}
	broad := &Entry{Facet: Persona, Count: 1, Leverage: &Leverage{Reach: ReachCrossProject}}
	owned := &Entry{Facet: Persona, Count: 1, Origin: SelfReport, Flags: []string{FlagLowConfidence}, Leverage: &Leverage{Reach: ReachOneOff}}
	for name, e := range map[string]*Entry{"seen_twice": seenTwice, "cross_project": broad, "self_report": owned, "nil": nil} {
		if IsUnreliableOneOff(e) {
			t.Errorf("%s should not be unreliable", name)
		}
	}
}
