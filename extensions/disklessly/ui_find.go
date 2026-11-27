package main

import (
	"strings"
	"time"
)

// The AI 对话 page's search: every word must match (SearchCold), and a time word among them
// (今天, 上周, 上个月…) keeps the conversations of that time only. disklessly_sessions_search is
// not changed: it still takes the query as words.

// searchHint tells, under the results, what the search box takes.
const searchHint = "可以输几个词一起搜，也可以加「昨天」「上周」「上个月」这样的时间。"

// findAskText is a sentence to say to the Persona assistant, which finds a conversation from a
// description (disklessly_sessions_find); assistantFindHint says it in the page's advice block.
const (
	findAskText       = "帮我找回上周聊购物车结算的那段对话"
	assistantFindHint = "记不清词也能找：跟 Persona 助手说是聊什么的、大概什么时候，比如「" + findAskText + "」，它会找出最像的几个让你选。"
)

// dateWindow is the time a word of the search box names, in loc: [from, to).
func dateWindow(word string, now time.Time, loc *time.Location) (from, to time.Time, ok bool) {
	n := now.In(loc)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	year := time.Date(n.Year(), 1, 1, 0, 0, 0, 0, loc)
	switch word {
	case "今天":
		return day, day.AddDate(0, 0, 1), true
	case "昨天":
		return day.AddDate(0, 0, -1), day, true
	case "前天":
		return day.AddDate(0, 0, -2), day.AddDate(0, 0, -1), true
	case "这周", "本周", "这星期", "这个星期":
		return monday, monday.AddDate(0, 0, 7), true
	case "上周", "上星期", "上个星期":
		return monday.AddDate(0, 0, -7), monday, true
	case "这个月", "本月":
		return month, month.AddDate(0, 1, 0), true
	case "上个月", "上月":
		return month.AddDate(0, -1, 0), month, true
	case "今年":
		return year, year.AddDate(1, 0, 0), true
	case "去年":
		return year.AddDate(-1, 0, 0), year, true
	}
	return time.Time{}, time.Time{}, false
}

// pageSearch is the page's search: query's time words (all of them must hold) filter by date,
// the other words go to SearchCold. limit 0: all.
func (d draw) pageSearch(query, machine string, limit int) []ColdHit {
	type window struct{ from, to time.Time }
	var windows []window
	var rest []string
	for _, w := range strings.Fields(query) {
		if from, to, ok := dateWindow(w, d.now, d.loc); ok {
			windows = append(windows, window{from, to})
			continue
		}
		rest = append(rest, w)
	}
	if len(windows) == 0 {
		return SearchCold(d.st, query, machine, limit, d.now)
	}
	var out []ColdHit
	for _, h := range SearchCold(d.st, strings.Join(rest, " "), machine, 0, d.now) {
		t := entryTime(h.ColdEntry)
		ok := !t.IsZero()
		for _, win := range windows {
			ok = ok && !t.Before(win.from) && t.Before(win.to)
		}
		if ok {
			out = append(out, h)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
