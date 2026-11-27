package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
)

// sectionLimit is how many rows an overview section shows; the rest are on their tab, and the footer says how many.
const sectionLimit = 20

// overviewSections is the 总览 tab: one line on what the extension is for, the stat grid with a caption on how its
// numbers relate, the imports waiting, today's advice, then what needs buying, what to use first and the chores due
// (each only when it has rows: the empty ones are folded into one line under the advice), the meal ideas for today
// (always: its empty line says what to do), and the ways to add things. With no records at all it offers the sample
// set and adding an item instead.
func (c *ctx) overviewSections() []wire.Section {
	if c.empty() {
		return c.welcomeSections()
	}
	sum := rules.Summarize(c.home, c.today)
	out := []wire.Section{
		{Items: []wire.Component{quiet(overviewIntro), statGrid(overviewStats(sum))}, Footer: statsCaption},
	}
	out = append(out, c.importSections()...)
	var shown []wire.Section
	var quietOnes []string
	for _, b := range []struct {
		sec  wire.Section
		none string
	}{
		{c.restockSection(), "库存里没有缺的"},
		{c.expiringSection(), "没有快过期的"},
		{c.choreSection(), "没有到期的家务"},
	} {
		if b.sec.Items[0].Type == wire.ComponentText { // orQuiet: the block has nothing but its empty line
			quietOnes = append(quietOnes, b.none)
			continue
		}
		shown = append(shown, b.sec)
	}
	out = append(out, c.adviceSection(foldedLine(quietOnes)))
	out = append(out, shown...)
	return append(out, c.mealSection(), addSection())
}

// foldedLine is the one quiet line that stands in for the overview blocks with nothing to show, "" for none.
func foldedLine(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "，") + "。"
}

// empty reports whether the household has no records at all.
func (c *ctx) empty() bool {
	h := c.home
	return len(h.Places)+len(h.Items)+len(h.Buys)+len(h.Chores)+len(h.Recipes)+len(h.Meals) == 0
}

// overviewIntro is the one line above the numbers of a household with records: what the extension is for. It is kept
// to one short line so the numbers stay at the top of a phone screen.
const overviewIntro = "记家里有什么、该买什么、家务和饭菜，到时提醒你。"

// statsCaption is the line under the numbers: 待买 counts the open entries of the shopping list, which the user puts
// there (by hand, with 一键补货 or through the assistant); 需要处理 counts what the rules find (alertsDetail).
const statsCaption = "待买是你记下要买的；需要处理是家庭管家自己发现的。"

// welcomeTitle is the empty state's title on the page and the card.
const welcomeTitle = "家里的东西还没开始记"

// welcomeSections is the overview of a household with no records: what the extension is for and the first steps, an
// empty state with 载入示例, the imports waiting, and the ways to add things (添加物品 among them).
func (c *ctx) welcomeSections() []wire.Section {
	out := []wire.Section{
		{Items: []wire.Component{{
			Type: wire.ComponentEmpty, Title: welcomeTitle,
			Detail: "家庭管家帮你记家里有什么、放在哪，东西快用完或快过期时提醒你买，还能管家务和每天吃什么。" +
				"先点「添加物品」记下第一件东西，或者载入一组示例看看各页是什么样；也可以直接跟助手说，例如「记一下冰箱里有两盒牛奶」。",
			Button: &wire.EmptyButton{Title: "载入示例", Action: wire.Action{
				ID: "load", Confirm: "载入一组虚构的示例记录？",
			}},
		}}},
	}
	// A household without records can still have a draft waiting: a pasted list or the assistant's first import.
	out = append(out, c.importSections()...)
	return append(out, addSection())
}

// overviewStats are the four totals of the overview: what needs handling, purchases open, chores due and items about
// to expire. Each of the first two says in its detail what it counts.
func overviewStats(s rules.Summary) []wire.Stat {
	return []wire.Stat{
		alertsStat(s),
		{Label: "待买", Value: strconv.Itoa(s.OpenBuys), Detail: "采购清单上没买的"},
		{Label: "家务到期", Value: strconv.Itoa(s.ChoresDue), Tone: cautionIf(s.ChoresDue)},
		{Label: "即将过期", Value: strconv.Itoa(s.Expiring), Tone: cautionIf(s.Expiring)},
	}
}

// alertsStat is 需要处理: the items that need buying (expired, used up, low or running out; rules.Kind.Buyable) plus
// the chores due, as rules.Summary.Alerts counts them, with the two parts in its detail. Items about to expire are not
// in it; they have their own number.
func alertsStat(s rules.Summary) wire.Stat {
	buy := s.Expired + s.Out + s.Low + s.Runout
	return wire.Stat{
		Label: "需要处理", Value: strconv.Itoa(s.Alerts),
		Detail: fmt.Sprintf("补货 %d · 家务 %d", buy, s.ChoresDue), Tone: s.Tone,
	}
}

// cautionIf is the caution tone when n is above zero, else no tone.
func cautionIf(n int) string {
	if n > 0 {
		return wire.ToneCaution
	}
	return ""
}

// footerOver says how many rows a section has beyond the ones it shows, "" when it shows them all.
func footerOver(total int) string {
	if total <= sectionLimit {
		return ""
	}
	return fmt.Sprintf("共 %d 项，只显示前 %d 项，其余在对应的页面", total, sectionLimit)
}

// restockSection is 需要补货: the one-tap restock, then a row per finding that calls for a purchase.
func (c *ctx) restockSection() wire.Section {
	var items []wire.Component
	if b, ok := c.restockButton(); ok {
		items = append(items, b)
	}
	total := 0
	for _, f := range rules.Findings(c.home, c.today) {
		if !f.Kind.Buyable() {
			continue
		}
		total++
		if total <= sectionLimit {
			items = append(items, c.findingRow(f))
		}
	}
	return wire.Section{Title: "需要补货", Footer: footerOver(total), Items: orQuiet(items, "没有需要补的货")}
}

// findingRow is a row for an item that needs buying: its estimate and place, and its finding as the badge.
func (c *ctx) findingRow(f rules.Finding) wire.Component {
	return wire.Component{
		Type: wire.ComponentRow, Title: cut(f.Item.Title, maxTitle),
		Subtitle: c.itemSubtitle(f.Item, f.Est),
		Badge:    &wire.Badge{Text: f.Kind.Label(), Tone: kindTone(f.Kind)},
		Action:   editAction(f.Item.ID, f.Item.Title),
	}
}

// expiringSection is 先用掉: the items that expire soon, to use first.
func (c *ctx) expiringSection() wire.Section {
	var rows []wire.Component
	total := 0
	for _, f := range rules.Findings(c.home, c.today) {
		if f.Kind != rules.Expiring {
			continue
		}
		total++
		if len(rows) < sectionLimit {
			rows = append(rows, c.expiringRow(f))
		}
	}
	return wire.Section{Title: "先用掉", Footer: footerOver(total), Items: orQuiet(rows, "没有即将过期的物品")}
}

// expiringRow is an item that expires soon: in how many days, and where it is.
func (c *ctx) expiringRow(f rules.Finding) wire.Component {
	when := "今天过期"
	if f.Days > 0 {
		when = fmt.Sprintf("%d 天后过期", f.Days)
	}
	return wire.Component{
		Type: wire.ComponentRow, Title: cut(f.Item.Title, maxTitle),
		Subtitle: join(when, c.placeName(f.Item.PlaceID)),
		Badge:    &wire.Badge{Text: f.Kind.Label(), Tone: wire.ToneCaution},
		Action:   editAction(f.Item.ID, f.Item.Title),
	}
}

// choreSection is 到期家务: the chores due, most overdue first.
func (c *ctx) choreSection() wire.Section {
	due := rules.ChoresDue(c.home, c.today)
	var rows []wire.Component
	for _, d := range slicesOf(due, sectionLimit) {
		rows = append(rows, c.choreRow(d))
	}
	return wire.Section{Title: "到期家务", Footer: footerOver(len(due)), Items: orQuiet(rows, "没有到期的家务")}
}

// choreRow is a chore due: its period, who does it and its next date, with 完成 as a swipe action that completes it.
func (c *ctx) choreRow(d rules.Due) wire.Component {
	badge := wire.Badge{Text: "今天", Tone: wire.ToneCaution}
	if d.Overdue > 0 {
		badge = wire.Badge{Text: fmt.Sprintf("逾期 %d 天", d.Overdue), Tone: wire.ToneDanger}
	}
	return wire.Component{
		Type: wire.ComponentRow, Title: cut(d.Chore.Title, maxTitle),
		Subtitle: choreLine(d.Chore),
		Badge:    &badge,
		Action:   editAction(d.Chore.ID, d.Chore.Title),
		Actions: []wire.RowAction{{
			Title: "完成", Swipe: true, Action: wire.Action{ID: "chore:" + d.Chore.ID},
		}},
	}
}

// mealSection is 今天吃什么: the three recipes that suit the ingredients at hand best, with 排到今晚 for each.
func (c *ctx) mealSection() wire.Section {
	var rows []wire.Component
	for _, idea := range rules.MealIdeas(c.home, c.today, 3) {
		rows = append(rows, ideaRow(idea))
	}
	return wire.Section{Title: "今天吃什么", Items: orQuiet(rows, "还没有可以做的菜：先到「餐食」里添加菜谱")}
}

// ideaRow is a recipe idea: what is missing or what to use first, and the badge says whether the food is all there.
func ideaRow(idea rules.Idea) wire.Component {
	badge := wire.Badge{Text: "食材齐了", Tone: wire.TonePositive}
	if len(idea.Missing) > 0 {
		badge = wire.Badge{Text: fmt.Sprintf("缺 %d 样", len(idea.Missing)), Tone: wire.ToneCaution}
	}
	return wire.Component{
		Type: wire.ComponentRow, Title: cut(idea.Recipe.Title, maxTitle),
		Subtitle: join(listOf("缺：", idea.Missing), listOf("先用：", idea.UseFirst)),
		Badge:    &badge,
		Action:   editAction(idea.Recipe.ID, idea.Recipe.Title),
		Actions: []wire.RowAction{{
			Title: "排到今晚", Swipe: true, Action: wire.Action{ID: "plan:" + idea.Recipe.ID},
		}},
	}
}

// listOf is prefix and the names joined with 、, "" for none.
func listOf(prefix string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	return prefix + strings.Join(names, "、")
}
