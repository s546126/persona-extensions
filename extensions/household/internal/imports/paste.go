package imports

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
)

// The paste parser reads one thing per line: a name, then a quantity with or without a unit, then a date, in that order
// or with the quantity first (「2 盒牛奶」). It is deliberately simple and never guesses: a line it cannot read is kept
// as it was, with the reason, and shown that way.

// PasteMost is the most lines the paste field takes.
const PasteMost = MaxLines

// units are the units the parser knows, longest first so that 千克 wins over 克.
var units = []string{
	"公斤", "千克", "毫升", "kg", "ml",
	"个", "盒", "瓶", "袋", "包", "罐", "桶", "支", "条", "块", "片", "根", "颗", "斤", "两", "克", "升", "g", "l",
	"卷", "板", "双", "只", "打", "箱", "份", "听", "提", "枚", "粒", "张", "套", "件", "节", "棵", "把", "杯", "头", "串",
}

var (
	bulletPattern = regexp.MustCompile(`^\s*(?:[-*•·]|\d{1,2}[.、)）]|[（(]\d{1,2}[)）])\s*`)
	isoDate       = regexp.MustCompile(`(\d{4})\s*[-/.年]\s*(\d{1,2})\s*[-/.月]\s*(\d{1,2})\s*[日号]?`)
	monthDay      = regexp.MustCompile(`(\d{1,2})\s*月\s*(\d{1,2})\s*[日号]`)
	dateWords     = regexp.MustCompile(`(保质期至|保质期到|过期日|有效期至|有效期到|到期|过期|保质期)[:：]?\s*$`)
	pricePattern  = regexp.MustCompile(`[¥￥]\s*\d+(\.\d+)?|\d+(\.\d+)?\s*元`)
)

const (
	latinNum = `\d+(?:\.\d+)?`
	hanNum   = `[零一二两三四五六七八九十百半]+`
)

// ParsePaste reads the pasted text into lines, numbered from 1 over the lines that are not blank. Lines it cannot read
// come back as Unread. A date on a line is its expiry date; without a year it is in the year of today, and the note says
// so.
func ParsePaste(text string, today model.Day) ([]Line, []Unread, []string) {
	var lines []Line
	var unread []Unread
	var notes []string
	n, over := 0, 0
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		n++
		if n > PasteMost {
			over++
			continue
		}
		l, note, why := parseLine(raw, today)
		if why != "" {
			unread = append(unread, Unread{Line: n, Text: cut(strings.TrimSpace(raw), 60), Why: why})
			continue
		}
		if note != "" {
			notes = append(notes, fmt.Sprintf("第 %d 行：%s", n, note))
		}
		lines = append(lines, l)
	}
	if over > 0 {
		unread = append(unread, Unread{Line: PasteMost + 1, Text: fmt.Sprintf("第 %d 行以后的 %d 行", PasteMost+1, over),
			Why: fmt.Sprintf("一次最多 %d 行，后面的没有读，请分几次粘贴", PasteMost)})
	}
	return lines, unread, notes
}

// parseLine reads one line; why is set when it cannot.
func parseLine(raw string, today model.Day) (Line, string, string) {
	s := halfWidth(strings.TrimSpace(raw))
	s = bulletPattern.ReplaceAllString(s, "")
	var l Line
	note := ""
	// A date, anywhere on the line.
	if m := isoDate.FindStringSubmatchIndex(s); m != nil {
		d, ok := dayOf(s[m[2]:m[3]], s[m[4]:m[5]], s[m[6]:m[7]])
		if !ok {
			return l, "", "日期「" + strings.TrimSpace(s[m[0]:m[1]]) + "」不对"
		}
		l.Expires = string(d)
		s = s[:m[0]] + " " + s[m[1]:]
	} else if m := monthDay.FindStringSubmatchIndex(s); m != nil {
		d, ok := dayOf(string(today)[:4], s[m[2]:m[3]], s[m[4]:m[5]])
		if !ok {
			return l, "", "日期「" + strings.TrimSpace(s[m[0]:m[1]]) + "」不对"
		}
		l.Expires = string(d)
		note = "日期没写年份，按今年 " + string(d) + " 记"
		s = s[:m[0]] + " " + s[m[1]:]
	}
	s = strings.TrimSpace(dateWords.ReplaceAllString(strings.TrimSpace(s), ""))
	if p := pricePattern.FindString(s); p != "" {
		v := strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "¥￥元"))
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			l.Price = &f
		}
		s = strings.TrimSpace(strings.Replace(s, p, " ", 1))
	}
	title, qty, unit, onlyQty := splitQty(s)
	if onlyQty {
		return Line{}, "", "只有数量，没有名称"
	}
	if qty != nil {
		l.Qty, l.Unit = qty, unit
	}
	title = strings.TrimSpace(strings.Trim(title, " ,，:：;；-"))
	if why := badTitle(title); why != "" {
		return Line{}, "", why
	}
	l.Title = title
	return l, note, ""
}

// splitQty takes a quantity (and its unit) off the end of the text, or off its start before a name. A number in
// Chinese needs a unit (「两盒」); a bare number needs a space or a times sign before it (「牛奶 2」「牛奶x2」). onlyQty
// reports a text that is a quantity and nothing else.
func splitQty(s string) (title string, qty *float64, unit string, onlyQty bool) {
	if m := tailQty.FindStringSubmatch(s); m != nil {
		title, sep, n, u := m[1], m[2], m[3], m[4]
		latin := n[0] >= '0' && n[0] <= '9'
		if u != "" || latin && (sep != "" || title == "") {
			if q, good := number(n); good {
				if strings.TrimSpace(title) == "" {
					return "", nil, "", true
				}
				return title, &q, normUnit(u), false
			}
		}
	}
	if m := headQty.FindStringSubmatch(s); m != nil {
		if q, good := number(m[1]); good {
			return m[3], &q, normUnit(m[2]), false
		}
	}
	return s, nil, "", false
}

var (
	unitAlt = strings.Join(units, "|")
	tailQty = regexp.MustCompile(`(?i)^(.*?)(\s*[x×*]\s*|\s+)?(` + latinNum + `|` + hanNum + `)\s*(` + unitAlt + `)?\s*$`)
	headQty = regexp.MustCompile(`(?i)^(` + latinNum + `|` + hanNum + `)\s*(` + unitAlt + `)\s*(.+)$`)
)

// badTitle says why a name cannot be a thing's name: empty, only digits and signs, too long, or a sentence.
func badTitle(t string) string {
	if t == "" {
		return "只有数量或日期，没有名称"
	}
	if utf8.RuneCountInString(t) > 30 {
		return "太长了，不像一样东西的名称"
	}
	letters := 0
	for _, r := range t {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.IsLetter(r):
			letters++
		case strings.ContainsRune("，。！？!?；;", r):
			return "像是一句话，不像一样东西的名称"
		}
	}
	if letters == 0 {
		return "看不出是什么东西"
	}
	return ""
}

// number reads a quantity in Latin digits or in Chinese (两, 十二, 半); false when it is not one or is not above zero.
func number(s string) (float64, bool) {
	if unicode.IsDigit(rune(s[0])) {
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil && f > 0 && f < 1e6
	}
	f, ok := hanNumber(s)
	return f, ok && f > 0
}

// hanNumber reads a Chinese number below a thousand: 三, 十, 十二, 二十五, 一百二十, 半.
func hanNumber(s string) (float64, bool) {
	if s == "半" {
		return 0.5, true
	}
	digits := map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	total, cur := 0, -1
	for _, r := range s {
		switch r {
		case '百':
			if cur < 0 {
				cur = 1
			}
			total += cur * 100
			cur = -1
		case '十':
			if cur < 0 {
				cur = 1
			}
			total += cur * 10
			cur = -1
		default:
			d, ok := digits[r]
			if !ok || cur >= 0 && d != 0 && cur != 0 {
				return 0, false
			}
			cur = d
		}
	}
	if cur > 0 {
		total += cur
	}
	return float64(total), true
}

// dayOf is the date of the year, month and day written out, false when there is no such day.
func dayOf(y, m, d string) (model.Day, bool) {
	yi, err1 := strconv.Atoi(y)
	mi, err2 := strconv.Atoi(m)
	di, err3 := strconv.Atoi(d)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", false
	}
	return model.ParseDay(fmt.Sprintf("%04d-%02d-%02d", yi, mi, di))
}
