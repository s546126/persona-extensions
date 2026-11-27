package imports

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// halfWidth turns full-width ASCII forms (Ａ, １, （) into their half-width letters and the ideographic space into a
// space. Everything else is kept.
func halfWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '　':
			b.WriteRune(' ')
		case r >= '！' && r <= '～':
			b.WriteRune(r - 0xfee0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanTitle is a title as it is stored: half-width, trimmed, inner runs of spaces made one.
func cleanTitle(s string) string {
	return strings.Join(strings.Fields(halfWidth(s)), " ")
}

// titleKey is the form two titles are compared in for an exact match: half-width, lower case, without spaces and
// the middle dots that receipts use as separators.
func titleKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(halfWidth(s)) {
		if unicode.IsSpace(r) || r == '·' || r == '•' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sizePattern matches the pack sizes and counts that receipts and shops write after a name: 250ml, 1.5L, 500g, ×12,
// *6, 12枚装, 3袋装.
var sizePattern = regexp.MustCompile(`(?i)(\d+(\.\d+)?\s*(ml|l|g|kg|毫升|升|克|千克|公斤|斤|cm|米)|[x×*]\s*\d+|\d+\s*[个只枚片包袋盒瓶罐卷抽支条块根粒颗]?装)`)

// bracketPattern matches a part in brackets: （进口）, [特价], 【会员】.
var bracketPattern = regexp.MustCompile(`[(（\[【<《][^)）\]】>》]*[)）\]】>》]`)

// softWords are words that describe a product rather than name it; they are left out when two names are compared
// loosely, so that 纯牛奶 and 鲜牛奶 both come near 牛奶.
var softWords = []string{"新鲜", "有机", "进口", "特价", "家庭装", "大包装", "精选", "优质", "纯", "鲜", "散装", "整箱", "原味"}

// coreKey is the loose form of a title: the exact key without sizes, brackets and describing words. It may be empty.
func coreKey(s string) string {
	k := strings.ToLower(halfWidth(s))
	k = bracketPattern.ReplaceAllString(k, "")
	k = sizePattern.ReplaceAllString(k, "")
	k = titleKey(k)
	for _, w := range softWords {
		k = strings.ReplaceAll(k, w, "")
	}
	return strings.Trim(k, "-_/、,.")
}

// closeness tells how near a new title is to a recorded one when they are not the same: 0 when they are not near at
// all, else a rank where a smaller number is nearer. Two titles are near when their loose forms are the same, or one
// contains the other and the shorter has at least two characters (so 盐 does not come near 盐焗鸡).
func closeness(a, b string) int {
	ca, cb := coreKey(a), coreKey(b)
	if ca == "" || cb == "" {
		return 0
	}
	if ca == cb {
		return 1
	}
	short, long := ca, cb
	if utf8.RuneCountInString(short) > utf8.RuneCountInString(long) {
		short, long = long, short
	}
	if utf8.RuneCountInString(short) < 2 || !strings.Contains(long, short) {
		return 0
	}
	return 2 + utf8.RuneCountInString(long) - utf8.RuneCountInString(short)
}

// unitAliases maps the ways a unit is written to the one the records use.
var unitAliases = map[string]string{
	"kg": "千克", "公斤": "千克", "g": "克", "ml": "毫升", "l": "升",
	"pcs": "个", "pc": "个", "只装": "只", "个装": "个", "瓶装": "瓶", "盒装": "盒", "袋装": "袋", "包装": "包",
}

// normUnit is a unit as the records write it: half-width, trimmed, lower case for Latin letters, aliases resolved.
func normUnit(u string) string {
	u = strings.ToLower(strings.TrimSpace(halfWidth(u)))
	if a, ok := unitAliases[u]; ok {
		return a
	}
	return u
}

// weights and volumes are the units that convert into each other, in grams and millilitres.
var (
	weights = map[string]float64{"克": 1, "千克": 1000, "斤": 500, "两": 50}
	volumes = map[string]float64{"毫升": 1, "升": 1000}
)

// convert turns qty of unit from into the unit to. It reports false when the two units do not convert into each
// other. The same unit converts to itself.
func convert(qty float64, from, to string) (float64, bool) {
	if from == to {
		return qty, true
	}
	for _, table := range []map[string]float64{weights, volumes} {
		f, ok1 := table[from]
		t, ok2 := table[to]
		if ok1 && ok2 {
			return round3(qty * f / t), true
		}
	}
	return 0, false
}

// round3 rounds to three decimals, so that conversions do not leave 0.30000000000000004 in the records.
func round3(f float64) float64 {
	const k = 1000
	if f < 0 {
		return -float64(int64(-f*k+0.5)) / k
	}
	return float64(int64(f*k+0.5)) / k
}
