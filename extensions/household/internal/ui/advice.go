package ui

import (
	"fmt"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
)

// adviceNames is how many names a line of 今天的建议 lists before it says how many more there are.
const adviceNames = 3

// adviceSection is 今天的建议: at most one line per kind of advice (rules.Advise, the same as the household_advise
// tool), each with its reason, opening the tab where the user acts. Its counts are the summary's, the ones the
// numbers above use: 先吃 counts 即将过期, 该买 the 补货 part of 需要处理, 家务 the 家务到期. The open purchases no
// 该买 line names come last, so the block says 今天没有要特别注意的 only when 待买 is 0 too. folded is the line that
// stands in for the blocks below that have nothing to show (foldedLine), the block's footer.
func (c *ctx) adviceSection(folded string) wire.Section {
	a := rules.Advise(c.home, c.today)
	by := map[string][]rules.Tip{}
	for _, t := range a.Tips {
		by[t.Kind] = append(by[t.Kind], t)
	}
	s := a.Summary
	var rows []wire.Component
	if eat := by[rules.AdviceEatFirst]; len(eat) > 0 {
		rows = append(rows, adviceRow(fmt.Sprintf("先吃 %d 样：%s", s.Expiring, names(eat)),
			eat[0].Title+" "+eat[0].Reason, stockTab))
	}
	if cook := by[rules.AdviceCook]; len(cook) > 0 {
		rows = append(rows, adviceRow("可以做："+cook[0].Title, cook[0].Reason, mealTab))
	}
	if buy := by[rules.AdviceRestock]; len(buy) > 0 {
		n := s.Expired + s.Out + s.Low + s.Runout
		sub := buy[0].Title + " " + buy[0].Reason
		on := 0
		for _, t := range buy {
			if t.OnList {
				on++
			}
		}
		if on > 0 && !buy[0].OnList {
			sub += fmt.Sprintf("。其中 %d 样已经在采购清单上", on)
		}
		rows = append(rows, adviceRow(fmt.Sprintf("该买 %d 样：%s", n, names(buy)), sub, buyTab))
	}
	if chores := by[rules.AdviceChore]; len(chores) > 0 {
		rows = append(rows, adviceRow(fmt.Sprintf("家务 %d 件：%s", s.ChoresDue, names(chores)),
			chores[0].Title+" "+chores[0].Reason, choreTab))
	}
	if list := by[rules.AdviceShopping]; len(list) > 0 {
		title := fmt.Sprintf("采购清单上还有 %d 样没买：%s", len(list), shoppingNames(list))
		if len(list) < s.OpenBuys {
			title = fmt.Sprintf("采购清单上另外还有 %d 样没买：%s", len(list), shoppingNames(list))
		}
		rows = append(rows, adviceRow(title, "是你记下要买的，不急；买到了在「采购」里点「已买」", buyTab))
	}
	if len(rows) == 0 {
		return wire.Section{Title: "今天的建议", Items: []wire.Component{quiet(nothingToday)}}
	}
	return wire.Section{Title: "今天的建议", Items: rows, Footer: folded}
}

// nothingToday is 今天的建议 when there is no advice at all: nothing to eat first, cook, buy or do, and nothing on the
// shopping list.
const nothingToday = "今天没有要特别注意的"

// shoppingNames lists the first entries of the shopping list, then 等.
func shoppingNames(ts []rules.Tip) string {
	var out []string
	for i, t := range ts {
		if i == adviceNames {
			return strings.Join(out, "、") + "等"
		}
		out = append(out, t.Title)
	}
	return strings.Join(out, "、")
}

// adviceRow is one line of 今天的建议, opening a tab.
func adviceRow(title, reason, tab string) wire.Component {
	return wire.Component{Type: wire.ComponentRow, Title: cut(title, maxTitle), Subtitle: cut(reason, 300),
		Action: &wire.Action{ID: "tab:" + tab}}
}

// names lists the first titles of the tips, then how many more.
func names(ts []rules.Tip) string {
	var out []string
	for i, t := range ts {
		if i == adviceNames {
			return strings.Join(out, "、") + fmt.Sprintf(" 等 %d 样", len(ts))
		}
		out = append(out, t.Title)
	}
	return strings.Join(out, "、")
}
