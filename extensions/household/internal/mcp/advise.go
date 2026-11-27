package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
)

const aboutAdvise = "今天的建议，只按家里的记录算，不改任何记录：先吃什么（快过期的，最快的在前）、能做什么菜（和 household_meal_suggest 前三道一样，" +
	"写明用得上哪些快过期的、还缺什么）、哪些家务到期、该买什么以及为什么（已过期、用完了、低于提醒线、按每天用量快用完，已经在采购清单上的会注明），最后是采购清单上其他还没买的东西（kind 为 shopping，不急）。" +
	"每一条有 rank（先后）、reason（原因）和记录编号（item_id、recipe_id、chore_id、buy_id），可以接着用 household_use、household_save（排一顿饭）、" +
	"household_restock、household_purchase、household_chore_done 去做。summary 和 household_status 的数字一致。同样的记录和日期，建议总是一样。" +
	"用户问「今天吃什么」「家里该买什么」「有什么快过期」时先调用它。"

type adviseOut struct {
	Today   model.Day   `json:"today"`
	Summary summaryOut  `json:"summary"`
	Tips    []rules.Tip `json:"tips"`
	Says    []string    `json:"says"`
	Next    string      `json:"next"`
}

// kindWords are the advice kinds in the user's words.
var kindWords = map[string]string{
	rules.AdviceEatFirst: "先吃", rules.AdviceCook: "可以做", rules.AdviceChore: "家务", rules.AdviceRestock: "该买",
	rules.AdviceShopping: "待买",
}

func runAdvise(e env, raw json.RawMessage) (any, error) {
	if err := noArgs(raw); err != nil {
		return nil, err
	}
	b := e.board()
	a := rules.Advise(b.h, b.today)
	sum := a.Summary
	out := adviseOut{
		Today: a.Today, Tips: a.Tips, Says: []string{},
		Summary: summaryOut{
			Expired: sum.Expired, Out: sum.Out, Low: sum.Low, Runout: sum.Runout, Expiring: sum.Expiring,
			OpenBuys: sum.OpenBuys, ChoresDue: sum.ChoresDue, Alerts: sum.Alerts, Tone: sum.Tone,
		},
	}
	for _, t := range a.Tips {
		out.Says = append(out.Says, fmt.Sprintf("%d. %s「%s」：%s", t.Rank, kindWords[t.Kind], t.Title, t.Reason))
	}
	if len(a.Tips) == 0 {
		out.Next = "今天没有要提醒的：没有快过期的、没有要买的（采购清单上也没有没买的）、没有到期的家务，也没有能推荐的菜（没有菜谱时可以请用户先加几道）。"
	} else {
		out.Next = strings.Join([]string{
			"按 says 的先后简短告诉用户，原因照 reason 说，不要自己加菜谱或数量。",
			"然后问用户要不要接着做：排一顿饭用 household_save（type meal，带 date、slot、recipe），",
			"把要买的加到采购清单用 household_restock，买到了用 household_purchase，用掉了用 household_use（问清用了多少），家务做完用 household_chore_done。这些都会改数据，要用户同意。",
		}, "")
	}
	return out, nil
}
