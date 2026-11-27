package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageSize       = 12
	recentSection  = "recent"
	librarySection = "library"
	wishSection    = "wishlist"
)

type pageState struct {
	Query       string `json:"query,omitempty"`
	Sort        string `json:"sort,omitempty"`
	RecentPage  int    `json:"recent_page,omitempty"`
	LibraryPage int    `json:"library_page,omitempty"`
	WishPage    int    `json:"wishlist_page,omitempty"`
	AppID       uint32 `json:"appid,omitempty"`
	From        string `json:"from,omitempty"`
}

type snapshot struct {
	player   reading[player]
	recent   reading[gameList]
	library  reading[gameList]
	wishlist reading[wishList]
}

func (a *app) snapshot(ctx context.Context, storeLanguage string, refresh, includeLibrary bool) snapshot {
	s := snapshot{player: a.account(ctx, presenceTTL, refresh)}
	if s.player.Data == nil || !s.player.Data.Public {
		return s
	}
	var reads sync.WaitGroup
	reads.Go(func() { s.recent = a.recentGames(ctx, s.player, refresh) })
	reads.Go(func() { s.wishlist = a.wishlist(ctx, s.player, storeLanguage, refresh) })
	if includeLibrary {
		reads.Go(func() { s.library = a.ownedGames(ctx, s.player, refresh) })
	}
	reads.Wait()
	return s
}

func (a *app) viewText(r *ext.UIRequest) *text {
	if r.Locale == "" {
		return textFor(a.locale)
	}
	return textFor(r.Locale)
}

func (a *app) viewQuery(value string) string {
	if a.client.key != "" {
		value = strings.ReplaceAll(value, a.client.key, "")
	}
	return cleanText(value, maxQueryRunes)
}

func (a *app) readState(r *ext.UIRequest, t *text) (pageState, error) {
	var st pageState
	if len(r.State) > 2048 || r.DecodeState(&st) != nil ||
		st.RecentPage < 0 || st.RecentPage > maxGames/pageSize ||
		st.LibraryPage < 0 || st.LibraryPage > maxGames/pageSize ||
		st.WishPage < 0 || st.WishPage > maxWishlistItems/pageSize ||
		(st.AppID != 0 && st.From != recentSection && st.From != librarySection) {
		return pageState{}, invalid(t.invalidPage)
	}
	if st.Sort == "" {
		st.Sort = sortPlaytime
	}
	if st.Sort != sortPlaytime && st.Sort != sortName {
		return pageState{}, invalid(t.invalidPage)
	}
	if st.AppID == 0 {
		st.From = ""
	}
	st.Query = a.viewQuery(st.Query)
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	t := a.viewText(r)
	st, err := a.readState(r, t)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, t, st, false), nil
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	t := a.viewText(r)
	st, err := a.readState(r, t)
	if err != nil {
		return nil, err
	}
	refresh := false
	switch r.Action {
	case "refresh":
		refresh = true
		st.AppID, st.From = 0, ""
	case "search":
		st.Query, st.LibraryPage = a.viewQuery(r.Values["q"]), 0
	case "sort":
		order := r.Values["order"]
		if order != sortPlaytime && order != sortName {
			return nil, invalid(t.invalidAction)
		}
		if q, ok := r.Values["q"]; ok {
			st.Query = a.viewQuery(q)
		}
		st.Sort, st.LibraryPage = order, 0
	case "back":
		st.AppID, st.From = 0, ""
	case "recent-next":
		st.RecentPage++
	case "recent-previous":
		st.RecentPage--
	case "library-next":
		st.LibraryPage++
	case "library-previous":
		st.LibraryPage--
	case "wishlist-next":
		st.WishPage++
	case "wishlist-previous":
		st.WishPage--
	default:
		from, rawID, ok := strings.Cut(r.Action, "-game-")
		id, parseErr := strconv.ParseUint(rawID, 10, 32)
		if !ok || parseErr != nil || id == 0 || (from != recentSection && from != librarySection) {
			return nil, invalid(t.invalidAction)
		}
		st.AppID, st.From = uint32(id), from
	}
	view := a.page(ctx, t, st, refresh)
	if refresh && r.Hub != nil {
		_ = r.Hub.Invalidate(summaryID)
	}
	return &wire.ActionResult{View: view}, nil
}

func (a *app) page(ctx context.Context, t *text, st pageState, refresh bool) *wire.View {
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	if st.AppID != 0 {
		return a.gameDetail(ctx, t, st)
	}

	s := a.snapshot(ctx, t.storeLanguage, refresh, true)
	view := &wire.View{Title: "Steam"}
	profile := wire.Section{Title: t.overview}
	if s.player.Data == nil {
		profile.Items = []wire.Component{empty(t.message(s.player.Issue, false)), button(t.refresh, "refresh")}
		view.Sections = []wire.Section{profile}
		return view
	}
	p := s.player.Data
	name := p.Name
	if name == "" {
		name = t.status
	}
	profile.Items = []wire.Component{{Type: wire.ComponentRow, Title: name, Subtitle: t.presence(p)}}
	profile.Footer = t.message(s.player.Issue, s.player.Stale)
	if !p.Public {
		profile.Footer = joinText(profile.Footer, t.message(problemPrivate, false))
		profile.Items = append(profile.Items, button(t.refresh, "refresh"))
		view.Sections = []wire.Section{profile}
		return view
	}

	profile.Items = append(profile.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
		{Label: t.recentTime, Value: recentValue(t, s.recent)},
		{Label: t.owned, Value: countValue(s.library.Data)},
	}}, button(t.refresh, "refresh"))
	view.Search = &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}
	view.Filter = &wire.Filter{
		Name: "order", Title: t.sort, Value: st.Sort, Action: wire.Action{ID: "sort"},
		Choices: []wire.Choice{{Value: sortPlaytime, Title: t.byPlaytime}, {Value: sortName, Title: t.byName}},
	}
	view.Sections = []wire.Section{
		profile,
		gamesSection(t, recentSection, s.recent, &st),
		gamesSection(t, librarySection, s.library, &st),
		wishlistSection(t, s.wishlist, &st),
	}
	view.State = ext.EncodeState(st)
	view.RefreshAfter = int(presenceTTL.Seconds())
	return view
}

func (a *app) card(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	t := a.viewText(r)
	s := a.snapshot(ctx, t.storeLanguage, false, false)
	issue := s.player.Issue
	if issue == "" && s.player.Data != nil && !s.player.Data.Public {
		issue = problemPrivate
	}
	section := wire.Section{
		Footer: t.message(issue, s.player.Stale),
		Items: []wire.Component{
			{Type: wire.ComponentText, Text: t.presence(s.player.Data)},
			{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
				{Label: t.recentTime, Value: recentValue(t, s.recent), Detail: t.message(s.recent.Issue, s.recent.Stale)},
				{Label: t.discounted, Value: discountValue(t, s.wishlist), Detail: t.message(s.wishlist.Issue, s.wishlist.Stale)},
			}},
		},
	}
	return &wire.View{Title: t.overview, Sections: []wire.Section{section}, RefreshAfter: int(presenceTTL.Seconds())}, nil
}

func recentValue(t *text, r reading[gameList]) string {
	if r.Data == nil {
		return "—"
	}
	return t.playtime(r.Data.Playtime2Weeks)
}

func countValue(list *gameList) string {
	if list == nil {
		return "—"
	}
	return strconv.Itoa(list.Total)
}

func discountValue(t *text, r reading[wishList]) string {
	if r.Data == nil || (r.Data.Total > 0 && r.Data.Checked == 0) {
		return "—"
	}
	if r.Data.Checked < r.Data.Total {
		return fmt.Sprintf(t.atLeast, r.Data.Discounted)
	}
	return strconv.Itoa(r.Data.Discounted)
}

func gamesSection(t *text, kind string, r reading[gameList], st *pageState) wire.Section {
	title, emptyMessage := t.recent, t.noRecent
	index := &st.RecentPage
	if kind == librarySection {
		title, emptyMessage, index = t.library, t.noGames, &st.LibraryPage
		if st.Query != "" {
			emptyMessage = t.noMatches
		}
	}
	section := wire.Section{Title: title, Footer: t.message(r.Issue, r.Stale)}
	if r.Data == nil {
		section.Items, section.Footer = []wire.Component{empty(section.Footer)}, ""
		return section
	}
	games := r.Data.Games
	if kind == librarySection {
		games = libraryGames(games, st.Query, st.Sort)
	}
	if len(games) == 0 {
		section.Items = []wire.Component{empty(emptyMessage)}
	}
	start, end := window(len(games), index)
	for _, game := range games[start:end] {
		subtitle := t.totalTime + " · " + t.playtime(uint64(game.PlaytimeForever))
		if kind == recentSection {
			subtitle = t.recentTime + " · " + t.playtime(uint64(game.Playtime2Weeks)) + "  /  " + subtitle
		}
		id := appIDText(game.AppID)
		section.Items = append(section.Items, wire.Component{
			Type: wire.ComponentRow, Key: kind + "-" + id, Title: t.gameName(game.AppID, game.Name), Subtitle: subtitle,
			Action: &wire.Action{ID: kind + "-game-" + id},
		})
	}
	addPager(t, &section, kind, start, end, len(games))
	if r.Data.Truncated {
		section.Footer = joinText(section.Footer, fmt.Sprintf(t.capped, len(r.Data.Games), r.Data.Total))
	}
	return section
}

func wishlistSection(t *text, r reading[wishList], st *pageState) wire.Section {
	section := wire.Section{Title: t.wishlist, Footer: t.message(r.Issue, r.Stale)}
	if r.Data == nil {
		section.Items, section.Footer = []wire.Component{empty(section.Footer)}, ""
		return section
	}
	items := r.Data.Items
	if len(items) == 0 {
		section.Items = []wire.Component{empty(t.noWishes)}
	}
	start, end := window(len(items), &st.WishPage)
	for _, item := range items[start:end] {
		row := wire.Component{Type: wire.ComponentRow, Key: "wish-" + appIDText(item.AppID), Title: t.gameName(item.AppID, item.Name)}
		switch {
		case item.Price != nil:
			row.Value = item.Price.FinalFormatted
			if row.Value == "" {
				row.Value = fmt.Sprintf("%s %d.%02d", item.Price.Currency, item.Price.Final/100, item.Price.Final%100)
			}
			if discount := item.Price.DiscountPercent; discount > 0 {
				row.Badge = &wire.Badge{Text: fmt.Sprintf(t.discount, discount), Tone: wire.TonePositive}
			}
		case item.PriceChecked:
			row.Subtitle = t.noPrice
		default:
			row.Subtitle = t.priceUnchecked
		}
		section.Items = append(section.Items, row)
	}
	addPager(t, &section, wishSection, start, end, len(items))
	if r.Data.Total > len(items) {
		section.Footer = joinText(section.Footer, fmt.Sprintf(t.capped, len(items), r.Data.Total))
	}
	return section
}

func (a *app) gameDetail(ctx context.Context, t *text, st pageState) *wire.View {
	view := &wire.View{Title: t.details, State: ext.EncodeState(st)}
	section := wire.Section{Items: []wire.Component{button(t.back, "back")}}
	p := a.account(ctx, profileTTL, false)
	var games reading[gameList]
	if st.From == recentSection {
		games = a.recentGames(ctx, p, false)
	} else {
		games = a.ownedGames(ctx, p, false)
	}
	if games.Data == nil {
		section.Items = append(section.Items, empty(t.message(games.Issue, games.Stale)))
		view.Sections = []wire.Section{section}
		return view
	}
	i := slices.IndexFunc(games.Data.Games, func(g game) bool { return g.AppID == st.AppID })
	if i < 0 {
		section.Items = append(section.Items, empty(t.missingGame))
		view.Sections = []wire.Section{section}
		return view
	}
	game := games.Data.Games[i]
	view.Title = t.gameName(game.AppID, game.Name)
	section.Footer = t.message(games.Issue, games.Stale)
	stats := []wire.Stat{{Label: t.totalTime, Value: t.playtime(uint64(game.PlaytimeForever))}}
	if st.From == recentSection {
		stats = append(stats, wire.Stat{Label: t.recentTime, Value: t.playtime(uint64(game.Playtime2Weeks))})
	}
	section.Items = append(section.Items,
		wire.Component{Type: wire.ComponentStatGrid, Stats: stats},
		wire.Component{Type: wire.ComponentLink, Title: t.store, URL: storeLink(game.AppID)},
	)
	progress := a.achievements(ctx, p, game.AppID)
	achievement := wire.Section{Title: t.achievements}
	switch {
	case progress.Data == nil:
		achievement.Items = []wire.Component{empty(t.message(progress.Issue, false))}
	case progress.Data.Total == 0:
		achievement.Items = []wire.Component{empty(t.noAchievements)}
	default:
		achievement.Items = []wire.Component{{
			Type: wire.ComponentProgress, Label: t.achievements,
			Ratio:  pluginkit.Ratio(float64(progress.Data.Unlocked) / float64(progress.Data.Total)),
			Detail: fmt.Sprintf(t.unlocked, progress.Data.Unlocked, progress.Data.Total),
		}}
	}
	view.Sections = []wire.Section{section, achievement}
	return view
}

func window(total int, index *int) (start, end int) {
	*index = min(max(0, (total-1)/pageSize), max(0, *index))
	start = *index * pageSize
	return start, min(start+pageSize, total)
}

func addPager(t *text, section *wire.Section, kind string, start, end, total int) {
	if start > 0 {
		section.Items = append(section.Items, button(t.previous, kind+"-previous"))
	}
	if end < total {
		section.Items = append(section.Items, button(t.next, kind+"-next"))
	}
	if total > pageSize {
		section.Footer = joinText(section.Footer, fmt.Sprintf(t.showing, start+1, end, total))
	}
}

func button(title, action string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: action}}
}

func empty(message string) wire.Component {
	return wire.Component{Type: wire.ComponentEmpty, Title: message}
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " " + b
}
