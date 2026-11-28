package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/spaces/internal/templates"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

func TestHardeningOversizedStateAndNegativeNPassCheckView(t *testing.T) {
	st := crowded(t, 30, false)
	app := newApp(st)
	hugeQ := strings.Repeat("搜", 3000)
	hugeF := strings.Repeat("f", 20000)

	t.Run("oversized_Q_and_F", func(t *testing.T) {
		v, err := app.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 4,
			State:     ext.EncodeState(state{T: "item", Q: hugeQ, F: hugeF}),
		}})
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("Page with oversized Q/F failed wire.CheckView: %v", err)
		}
	})

	t.Run("negative_N", func(t *testing.T) {
		v, err := app.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 4,
			State:     ext.EncodeState(state{T: "item", N: -5}),
		}})
		if err != nil {
			t.Fatalf("Page with negative N failed: %v", err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("Page with negative N failed wire.CheckView: %v", err)
		}
		var got state
		if err := (&ext.UIRequest{UIParams: wire.UIParams{State: v.State}}).DecodeState(&got); err != nil {
			t.Fatalf("DecodeState: %v", err)
		}
		if got.N < 0 {
			t.Fatalf("expected negative N to be clamped to >= 0, got %d", got.N)
		}
	})

	t.Run("fold_huge_arg", func(t *testing.T) {
		res, err := app.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 4,
			Action:    "fold:" + hugeF,
		}})
		if err != nil {
			t.Fatalf("Act fold: %v", err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("Act fold with huge arg failed wire.CheckView: %v", err)
		}
	})
}

func TestHardeningLoweredDenseViewsPassCheckView(t *testing.T) {
	t.Run("entityPage_v1", func(t *testing.T) {
		st := store.Open("")
		var mainID, spID string
		err := st.Update(func(b *kernel.Base, _ time.Time) error {
			sp, err := templates.Apply(b, "blank", "密集空间", false, testNow)
			if err != nil {
				return err
			}
			spID = sp.ID
			props := []kernel.Prop{
				{Key: "links", Name: "关联", Kind: kernel.KindRelation, Target: "node", Many: true, Back: "来源"},
			}
			for i := range 23 {
				props = append(props, kernel.Prop{Key: fmt.Sprintf("u%d", i), Name: fmt.Sprintf("链接%d", i), Kind: kernel.KindURL})
			}
			flow := []kernel.Stage{
				{Name: "阶段1", Phase: kernel.Todo},
				{Name: "阶段2", Phase: kernel.Doing},
				{Name: "阶段3", Phase: kernel.Doing},
				{Name: "阶段4", Phase: kernel.Doing},
				{Name: "阶段5", Phase: kernel.Doing},
				{Name: "阶段6", Phase: kernel.Done},
				{Name: "放弃1", Phase: kernel.Dropped},
				{Name: "放弃2", Phase: kernel.Dropped},
			}
			if err := sp.PutType(kernel.Type{Key: "node", Name: "节点", Flow: flow, Props: props}, testNow); err != nil {
				return err
			}
			// Create 105 target nodes first.
			var ids []string
			for i := range 105 {
				title := fmt.Sprintf("目标 %03d", i)
				e, err := sp.PutEntity(b, kernel.EntityInput{Type: "node", Title: &title}, testNow)
				if err != nil {
					return err
				}
				ids = append(ids, e.ID)
			}
			vals := map[string]any{"links": ids[:100]}
			for i := range 23 {
				// >60 runes https URL produces BOTH a link component AND a body text component in entityHead!
				vals[fmt.Sprintf("u%d", i)] = fmt.Sprintf("https://example.com/very/long/path/segment/%s/%d", strings.Repeat("abcde", 12), i)
			}
			title := "中心节点"
			mainEnt, err := sp.PutEntity(b, kernel.EntityInput{Type: "node", Title: &title, Values: vals}, testNow)
			if err != nil {
				return err
			}
			mainID = mainEnt.ID
			// Add 105 backlinks pointing at mainID.
			for i := range 105 {
				bt := fmt.Sprintf("入边 %03d", i)
				if _, err := sp.PutEntity(b, kernel.EntityInput{
					Type:   "node",
					Title:  &bt,
					Values: map[string]any{"links": []string{mainID}},
				}, testNow); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		app := newApp(st)
		for _, ver := range []int{4, 3, 2, 1} {
			v, err := app.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
				UIVersion: ver,
				State:     ext.EncodeState(state{S: spID, E: mainID, F: "edit"}),
			}})
			if err != nil {
				t.Fatalf("v%d Page: %v", ver, err)
			}
			if err := wire.CheckView(v); err != nil {
				t.Fatalf("entityPage at UI v%d failed wire.CheckView: %v", ver, err)
			}
		}
	})

	t.Run("progress_and_feed_v1", func(t *testing.T) {
		st := store.Open("")
		var spID string
		err := st.Update(func(b *kernel.Base, _ time.Time) error {
			// Create 25 spaces so filterRows expands into 25 items on UI < 3.
			for i := range 25 {
				sp, err := templates.Apply(b, "blank", fmt.Sprintf("空间 %02d", i), false, testNow)
				if err != nil {
					return err
				}
				if i == 0 {
					spID = sp.ID
					// 7 types so tabRows expands into 8 items on UI < 4.
					for k := range 6 {
						if err := sp.PutType(kernel.Type{Key: fmt.Sprintf("t%d", k), Name: fmt.Sprintf("类%d", k)}, testNow); err != nil {
							return err
						}
					}
					ty := kernel.Type{
						Key:  "book",
						Name: "书",
						Flow: []kernel.Stage{{Name: "在读", Phase: kernel.Doing}, {Name: "读完", Phase: kernel.Done}},
						Props: []kernel.Prop{
							{Key: "cur", Name: "已读", Kind: kernel.KindNumber},
							{Key: "tot", Name: "总页", Kind: kernel.KindNumber},
							{Key: "note", Name: "笔记", Kind: kernel.KindText},
						},
						Card: kernel.Card{
							Lead:     "note",
							Progress: &kernel.Ratio{Of: "cur", Over: "tot"},
						},
						Views: []kernel.View{
							{Name: "进度", Kind: "progress"},
							{Name: "动态", Kind: "feed"},
							{Name: "列表", Kind: "list"},
							{Name: "看板", Kind: "board"},
							{Name: "表格", Kind: "table"},
						},
					}
					if err := sp.PutType(ty, testNow); err != nil {
						return err
					}
					longNote := strings.Repeat("摘录内容很长", 20)
					for n := range 150 {
						title := fmt.Sprintf("书籍 %03d", n)
						if _, err := sp.PutEntity(b, kernel.EntityInput{
							Type:  "book",
							Title: &title,
							Values: map[string]any{
								"cur":  float64(50),
								"tot":  float64(100),
								"note": longNote,
							},
						}, testNow); err != nil {
							return err
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		app := newApp(st)
		for _, viewIdx := range []int{0, 1} {
			for _, ver := range []int{4, 3, 2, 1} {
				v, err := app.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
					UIVersion: ver,
					State:     ext.EncodeState(state{S: spID, T: "book", V: viewIdx, N: 10}),
				}})
				if err != nil {
					t.Fatalf("view %d v%d Page: %v", viewIdx, ver, err)
				}
				if err := wire.CheckView(v); err != nil {
					t.Fatalf("view %d at UI v%d failed wire.CheckView: %v", viewIdx, ver, err)
				}
			}
		}
	})
}
