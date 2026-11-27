package rules

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
)

// Advice kinds, in the order Advise ranks them.
const (
	AdviceEatFirst = "eat_first" // an item that expires soon: use it first
	AdviceCook     = "cook"      // a recipe that suits what is at hand (the meal suggestions)
	AdviceChore    = "chore"     // a chore due
	AdviceRestock  = "restock"   // an item to buy: expired, used up, low or running out
	AdviceShopping = "shopping"  // an open purchase on the shopping list that no restock tip names yet
)

// AdviceMeals is how many recipes the advice names: the same three the overview's 今天吃什么 shows.
const AdviceMeals = 3

// Tip is one piece of advice: what, why in words, and the ids of the records it is about, so that the next step can be
// taken with the existing tools.
type Tip struct {
	Kind   string `json:"kind"`
	Rank   int    `json:"rank"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
	// The records: the item (eat first, restock, a shopping entry linked to one), the recipe (cook), the chore, the
	// open purchase (of a restock item, or the shopping entry itself).
	ItemID   string `json:"item_id,omitempty"`
	RecipeID string `json:"recipe_id,omitempty"`
	ChoreID  string `json:"chore_id,omitempty"`
	BuyID    string `json:"buy_id,omitempty"`
	// For cook: the ingredients at hand that expire soon, and those missing.
	UseFirst []string `json:"use_first,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	// OnList reports a restock item that already has a 待买 purchase.
	OnList bool `json:"on_list,omitempty"`
	// Days: until expiry (eat first), past expiry (restock expired), stock left at the daily rate (restock running out),
	// overdue (chore).
	Days int `json:"days"`
}

// Advice is today's advice, ranked: what to eat first, what to cook, the chores due, what to buy, then the rest of the
// shopping list. Summary is the same
// count as the overview's and household_status's, so the advice never contradicts them.
type Advice struct {
	Today   model.Day `json:"today"`
	Tips    []Tip     `json:"tips"`
	Summary Summary   `json:"-"`
}

// Advise builds today's advice from the records alone. It is pure: the same records and day give the same advice.
//
//   - eat first: the items about to expire (the 即将过期 findings), soonest first;
//   - cook: the first AdviceMeals meal ideas (MealIdeas, the same as the overview and household_meal_suggest);
//   - chores: the chores due, most overdue first (ChoresDue);
//   - restock: the findings that call for a purchase (expired, used up, low, running out), in the order of Findings;
//     those not on the shopping list yet come first;
//   - shopping: last and least urgent, the open purchases (待买) whose item has no restock tip, in the order of the
//     list; with the restock tips marked on_list they cover every open purchase, so the advice is empty only when
//     the 待买 number is 0 too.
func Advise(h *model.Home, today model.Day) Advice {
	a := Advice{Today: today, Tips: []Tip{}, Summary: Summarize(h, today)}
	findings := Findings(h, today)
	var eat []Finding
	for _, f := range findings {
		if f.Kind == Expiring {
			eat = append(eat, f)
		}
	}
	slices.SortStableFunc(eat, func(x, y Finding) int { return cmp.Compare(x.Days, y.Days) })
	for _, f := range eat {
		a.Tips = append(a.Tips, Tip{Kind: AdviceEatFirst, Title: f.Item.Title, ItemID: f.Item.ID, Days: f.Days,
			Reason: expiryWords(f.Days) + "，先用掉"})
	}
	for _, idea := range MealIdeas(h, today, AdviceMeals) {
		a.Tips = append(a.Tips, Tip{Kind: AdviceCook, Title: idea.Recipe.Title, RecipeID: idea.Recipe.ID,
			UseFirst: idea.UseFirst, Missing: idea.Missing, Reason: ideaWords(idea)})
	}
	for _, d := range ChoresDue(h, today) {
		why := "今天该做"
		if d.Overdue > 0 {
			why = fmt.Sprintf("已经逾期 %d 天", d.Overdue)
		}
		a.Tips = append(a.Tips, Tip{Kind: AdviceChore, Title: d.Chore.Title, ChoreID: d.Chore.ID, Days: d.Overdue, Reason: why})
	}
	open := map[string]string{}
	for _, b := range h.Buys {
		if b.Status == model.BuyTodo && b.ItemID != "" {
			if _, ok := open[b.ItemID]; !ok {
				open[b.ItemID] = b.ID
			}
		}
	}
	var buy []Finding
	for _, f := range findings {
		if f.Kind.Buyable() {
			buy = append(buy, f)
		}
	}
	slices.SortStableFunc(buy, func(x, y Finding) int { return cmp.Compare(boolInt(x.OpenBuy), boolInt(y.OpenBuy)) })
	for _, f := range buy {
		t := Tip{Kind: AdviceRestock, Title: f.Item.Title, ItemID: f.Item.ID, OnList: f.OpenBuy, BuyID: open[f.Item.ID],
			Reason: restockWords(f)}
		if f.Kind == Expired || f.Kind == Runout {
			t.Days = f.Days
		}
		if f.OpenBuy {
			t.Reason += "；已经在采购清单上"
		}
		a.Tips = append(a.Tips, t)
	}
	named := map[string]bool{}
	for _, f := range buy {
		named[f.Item.ID] = true
	}
	for _, b := range h.Buys {
		if b.Status != model.BuyTodo || b.ItemID != "" && named[b.ItemID] {
			continue
		}
		a.Tips = append(a.Tips, Tip{Kind: AdviceShopping, Title: b.Title, ItemID: b.ItemID, BuyID: b.ID, OnList: true,
			Reason: shoppingWords(b)})
	}
	for i := range a.Tips {
		a.Tips[i].Rank = i + 1
	}
	return a
}

// expiryWords is how soon an item expires.
func expiryWords(days int) string {
	if days <= 0 {
		return "今天过期"
	}
	return fmt.Sprintf("%d 天后过期", days)
}

// ideaWords says why a recipe suits today.
func ideaWords(idea Idea) string {
	var parts []string
	if len(idea.UseFirst) > 0 {
		parts = append(parts, "用得上快过期的"+strings.Join(idea.UseFirst, "、"))
	}
	if len(idea.Missing) == 0 {
		parts = append(parts, "食材齐了")
	} else {
		parts = append(parts, "还缺"+strings.Join(idea.Missing, "、"))
	}
	return strings.Join(parts, "，")
}

// restockWords says why an item is to be bought, with the numbers the records give.
func restockWords(f Finding) string {
	it := f.Item
	switch f.Kind {
	case Expired:
		return fmt.Sprintf("已经过期 %d 天，先看看还能不能用", f.Days)
	case Out:
		return "已经用完了"
	case Low:
		return fmt.Sprintf("约剩 %s%s，到了低库存线 %s%s", fnum(f.Est), it.Unit, fnum(it.Min), it.Unit)
	case Runout:
		return fmt.Sprintf("约剩 %s%s，按每天 %s%s 算，%d 天左右用完", fnum(f.Est), it.Unit, fnum(it.Daily), it.Unit, f.Days)
	}
	return f.Kind.Label()
}

// shoppingWords says why an open purchase is advice: how much, if recorded, and that it is still to buy.
func shoppingWords(b model.Buy) string {
	why := "在采购清单上，还没买"
	if b.Amount > 0 {
		why = fmt.Sprintf("要买 %s%s，", fnum(b.Amount), b.Unit) + why
	}
	return why
}

// fnum is a number without trailing zeros, rounded to two decimals.
func fnum(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}
