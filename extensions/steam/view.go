package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageSize           = 12
	recentSection      = "recent"
	librarySection     = "library"
	wishSection        = "wishlist"
	gameNewsAction     = "game-news"
	newsPageLimit      = 5
	backlogStatusField = "backlog_status"
	recapAction        = "playtime-recap"
	recapPeriodAction  = "recap-period"
)

type pageState struct {
	Query          string      `json:"query,omitempty"`
	Sort           string      `json:"sort,omitempty"`
	RecentPage     int         `json:"recent_page,omitempty"`
	LibraryPage    int         `json:"library_page,omitempty"`
	WishPage       int         `json:"wishlist_page,omitempty"`
	AppID          uint32      `json:"appid,omitempty"`
	From           string      `json:"from,omitempty"`
	News           bool        `json:"news,omitempty"`
	BacklogAccount string      `json:"backlog_account,omitempty"`
	Recap          recapPeriod `json:"recap,omitempty"`
}

type snapshot struct {
	player   reading[player]
	recent   reading[gameList]
	library  reading[gameList]
	wishlist reading[wishList]
}

func (d *steamData) snapshot(ctx context.Context, storeLanguage string, refresh, includeLibrary bool) snapshot {
	s := snapshot{player: d.account(ctx, presenceTTL, refresh)}
	if _, issue := d.readID(s.player); issue != "" {
		return s
	}
	var reads sync.WaitGroup
	reads.Go(func() { s.recent = d.recentGames(ctx, s.player, refresh) })
	reads.Go(func() { s.wishlist = d.wishlist(ctx, s.player, storeLanguage, refresh) })
	if includeLibrary {
		reads.Go(func() { s.library = d.ownedGames(ctx, s.player, refresh) })
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
	if a.qr != nil {
		value = a.qr.redact(value)
	}
	return cleanText(value, maxQueryRunes)
}

func (a *app) readState(r *ext.UIRequest, t *text) (pageState, error) {
	var st pageState
	if len(r.State) > 2048 || r.DecodeState(&st) != nil ||
		st.RecentPage < 0 || st.RecentPage > maxGames/pageSize ||
		st.LibraryPage < 0 || st.LibraryPage > maxGames/pageSize ||
		st.WishPage < 0 || st.WishPage > maxWishlistItems/pageSize ||
		(st.Recap != "" && (!slices.Contains(recapPeriods[:], st.Recap) || st.News || st.AppID != 0)) ||
		(st.AppID != 0 && (st.News || (st.From != recentSection && st.From != librarySection))) {
		return pageState{}, invalid(t.invalidPage)
	}
	if st.Sort == "" {
		st.Sort = sortPlaytime
	}
	if st.Sort != sortPlaytime && st.Sort != sortName {
		return pageState{}, invalid(t.invalidPage)
	}
	if st.AppID == 0 {
		st.From, st.BacklogAccount = "", ""
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
		st.AppID, st.From, st.News = 0, "", false
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
		st.AppID, st.From, st.News, st.Recap = 0, "", false, ""
	case gameNewsAction:
		st.AppID, st.From, st.News, st.Recap = 0, "", true, ""
	case recapAction:
		st.AppID, st.From, st.News, st.Recap = 0, "", false, recapMonth
	case recapPeriodAction:
		period := recapPeriod(r.Values["period"])
		if st.Recap == "" || !slices.Contains(recapPeriods[:], period) {
			return nil, invalid(t.invalidAction)
		}
		st.Recap = period
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
	case "backlog-save", "backlog-clear":
		return a.backlogAction(ctx, r, t, st)
	default:
		from, rawID, ok := strings.Cut(r.Action, "-game-")
		id, parseErr := strconv.ParseUint(rawID, 10, 32)
		if !ok || parseErr != nil || id == 0 || (from != recentSection && from != librarySection) {
			return nil, invalid(t.invalidAction)
		}
		st.AppID, st.From, st.News, st.Recap = uint32(id), from, false, ""
	}
	view := a.page(ctx, t, st, refresh)
	if refresh && r.Hub != nil {
		_ = r.Hub.Invalidate(summaryID)
	}
	return &wire.ActionResult{View: view}, nil
}

func (a *app) page(ctx context.Context, t *text, st pageState, refresh bool) *wire.View {
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	return a.accountPage(ctx, d, t, st, refresh)
}

func (a *app) accountPage(ctx context.Context, d *steamData, t *text, st pageState, refresh bool) *wire.View {
	if d.config.AuthMode == modeQR && d.setup != "" {
		return unavailablePage(t, d.setup)
	}
	var view *wire.View
	st.BacklogAccount = ""
	switch {
	case st.Recap != "":
		view = a.recapPage(ctx, d, t, st, refresh)
	case st.News:
		view = a.gameNewsPage(ctx, d, t, st)
	case st.AppID != 0:
		view = a.gameDetail(ctx, d, t, st)
	case d.config.FriendsRadarEnabled && d.setup == "":
		var friends wire.Section
		var reads sync.WaitGroup
		reads.Go(func() { friends = d.friendsSection(ctx, t) })
		view = a.overview(ctx, d, t, st, refresh)
		reads.Wait()
		view.Sections = append(view.Sections, friends)
	default:
		view = a.overview(ctx, d, t, st, refresh)
	}
	if issue := d.sessionIssue(); issue != "" {
		return unavailablePage(t, issue)
	}
	return view
}

func unavailablePage(t *text, issue problem) *wire.View {
	return &wire.View{Title: "Steam", Sections: []wire.Section{{Title: t.overview,
		Items: []wire.Component{empty(t.message(issue, false)), button(t.refresh, "refresh")}}}}
}

func (a *app) overview(ctx context.Context, d *steamData, t *text, st pageState, refresh bool) *wire.View {
	s := d.snapshot(ctx, t.storeLanguage, refresh, true)
	view := &wire.View{Title: "Steam"}
	profile := wire.Section{Title: t.overview}
	if p := s.player.Data; p == nil {
		if d.session == nil {
			return unavailablePage(t, s.player.Issue)
		}
		profile.Items = []wire.Component{empty(t.message(s.player.Issue, false))}
	} else {
		name := p.Name
		if name == "" {
			name = t.status
		}
		profile.Items = []wire.Component{{Type: wire.ComponentRow, Title: name, Subtitle: t.presence(p)}}
		profile.Footer = t.message(s.player.Issue, s.player.Stale)
		if d.session == nil && !p.Public {
			profile.Footer = joinText(profile.Footer, t.message(problemPrivate, false))
			profile.Items = append(profile.Items, button(t.refresh, "refresh"))
			view.Sections = []wire.Section{profile}
			return view
		}
	}

	profile.Items = append(profile.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
		{Label: t.recentTime, Value: recentValue(t, s.recent)},
		{Label: t.owned, Value: countValue(s.library.Data)},
	}}, button(t.refresh, "refresh"), button(t.recentNews, gameNewsAction), button(t.recap.title, recapAction))
	view.Search = &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}
	view.Filter = &wire.Filter{
		Name: "order", Title: t.sort, Value: st.Sort, Action: wire.Action{ID: "sort"},
		Choices: []wire.Choice{{Value: sortPlaytime, Title: t.byPlaytime}, {Value: sortName, Title: t.byName}},
	}
	var entries map[uint32]BacklogEntry
	var backlogIssue problem
	if s.library.Data != nil {
		entries, backlogIssue = a.backlogEntries(d, s.player)
	}
	view.Sections = []wire.Section{
		profile,
		gamesSection(t, recentSection, s.recent, &st, entries),
		gamesSection(t, librarySection, s.library, &st, entries),
		wishlistSection(t, s.wishlist, &st),
	}
	if s.library.Data != nil {
		view.Sections = append(view.Sections, backlogSection(t, s.library.Data, s.recent, entries, backlogIssue))
	}
	view.State = ext.EncodeState(st)
	view.RefreshAfter = int(presenceTTL.Seconds())
	return view
}

func (a *app) gameNewsPage(ctx context.Context, d *steamData, t *text, st pageState) *wire.View {
	options := gameNewsOptions{limit: newsPageLimit, gamesLimit: defaultNewsGames, officialOnly: true}
	news := a.accountGameNews(ctx, d, t, options)
	section := wire.Section{Items: []wire.Component{button(t.back, "back")}}
	for _, item := range news.Updates {
		date := time.Unix(item.Date, 0).UTC().Format(time.DateOnly)
		section.Items = append(section.Items, wire.Component{
			Type: wire.ComponentLink, Key: item.GID, URL: item.URL,
			Title: item.Title + " · " + item.GameName + " · " + date,
		})
	}
	if news.Available && len(news.Updates) == 0 {
		section.Items = append(section.Items, empty(t.noRecentNews))
	}
	seen := make(map[string]bool)
	for _, warning := range news.Warnings {
		if !seen[warning.Message] {
			section.Footer = joinText(section.Footer, warning.Message)
			seen[warning.Message] = true
		}
	}
	return &wire.View{Title: t.recentNews, State: ext.EncodeState(st), Sections: []wire.Section{section}}
}

func (a *app) recapPage(ctx context.Context, d *steamData, t *text, st pageState, refresh bool) *wire.View {
	r := a.readPlaytimeRecap(ctx, d, t, st.Recap, defaultRecapTop, refresh)
	choices := make([]wire.Choice, 0, len(recapPeriods))
	for _, period := range recapPeriods {
		choices = append(choices, wire.Choice{Value: string(period), Title: t.recap.periods[period]})
	}
	view := &wire.View{Title: t.recap.title, State: ext.EncodeState(st), Filter: &wire.Filter{
		Name: "period", Title: t.recap.period, Choices: choices, Value: string(st.Recap), Action: wire.Action{ID: recapPeriodAction},
	}}
	summary := wire.Section{Items: []wire.Component{button(t.back, "back"), button(t.refresh, "refresh")}, Footer: r.Hint}
	var stats []wire.Stat
	if library := r.Library; library != nil {
		if r.Truncated {
			summary.Footer = joinText(summary.Footer, t.message(problemPartialLibrary, false))
		}
		stats = append(stats,
			wire.Stat{Label: t.owned, Value: strconv.FormatUint(uint64(library.OwnedGames), 10)},
			wire.Stat{Label: t.totalTime, Value: t.playtime(library.TotalMinutes)},
			wire.Stat{Label: t.backlog.bucketTitle(neverPlayed), Value: strconv.FormatUint(uint64(library.UnplayedGames), 10)})
	}
	if r.Recent != nil {
		stats = append(stats, wire.Stat{Label: t.recentTime, Value: t.playtime(r.Recent.Playtime)})
	}
	if len(stats) > 0 {
		summary.Items = append(summary.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: stats})
	}
	for _, warning := range r.Warnings {
		summary.Footer = joinText(summary.Footer, warning.Message)
	}
	view.Sections = []wire.Section{summary}
	if delta := r.Delta; delta != nil {
		window := fmt.Sprintf(t.recap.window, delta.From.UTC().Format(time.DateOnly), delta.To.UTC().Format(time.DateOnly))
		section := wire.Section{Title: t.recap.topGames, Footer: window, Items: []wire.Component{{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
			{Label: t.recap.addedTime, Value: t.playtime(delta.PlaytimeMinutes)},
			{Label: t.recap.newGames, Value: strconv.Itoa(delta.NewGames)},
		}}}}
		addRecapGames(&section, t, delta.TopGames)
		view.Sections = append(view.Sections, section)
		if p := delta.Progress; p != nil {
			stats := make([]wire.Stat, 0, len(recapBuckets)+1)
			for _, bucket := range recapBuckets {
				label := t.backlog.statuses[BacklogStatus(bucket)]
				if bucket == recapBacklog {
					label = t.recap.backlog
				}
				stats = append(stats, wire.Stat{Label: label, Value: fmt.Sprintf("%+d", p.CountChanges[bucket]),
					Detail: fmt.Sprintf(t.recap.movedInto, p.MovedInto[bucket])})
			}
			if p.Rate != nil {
				stats = append(stats, wire.Stat{Label: t.recap.consumption, Value: fmt.Sprintf("%.0f%%", *p.Rate*100),
					Detail: fmt.Sprintf("%d / %d", p.Consumed, p.AtStart)})
			}
			view.Sections = append(view.Sections, wire.Section{Title: t.recap.progress,
				Items: []wire.Component{{Type: wire.ComponentStatGrid, Stats: stats}}})
		}
	} else if r.Recent != nil {
		section := wire.Section{Title: t.recent}
		addRecapGames(&section, t, r.Recent.TopGames)
		if len(section.Items) == 0 {
			section.Items = []wire.Component{empty(t.noRecent)}
		}
		view.Sections = append(view.Sections, section)
	}
	if r.Annotations != nil {
		stats := make([]wire.Stat, 0, len(backlogStatuses))
		for _, status := range backlogStatuses {
			stats = append(stats, wire.Stat{Label: t.backlog.statuses[status], Value: strconv.Itoa(r.Annotations[status])})
		}
		view.Sections = append(view.Sections, wire.Section{Title: t.recap.annotations,
			Items: []wire.Component{{Type: wire.ComponentStatGrid, Stats: stats}}})
	}
	return view
}

func addRecapGames(section *wire.Section, t *text, games []recapGame) {
	for _, game := range games {
		section.Items = append(section.Items, wire.Component{
			Type: wire.ComponentRow, Key: appIDText(game.AppID), Title: game.Name, Value: t.playtime(uint64(game.Minutes)),
		})
	}
}

func (a *app) card(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	t := a.viewText(r)
	s := d.snapshot(ctx, t.storeLanguage, false, false)
	if issue := d.sessionIssue(); issue != "" {
		s = snapshot{player: reading[player]{Issue: issue}}
	}
	issue := s.player.Issue
	if issue == "" && d.session == nil && s.player.Data != nil && !s.player.Data.Public {
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

func gamesSection(t *text, kind string, r reading[gameList], st *pageState, entries map[uint32]BacklogEntry) wire.Section {
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
		row := wire.Component{
			Type: wire.ComponentRow, Key: kind + "-" + id, Title: t.gameName(game.AppID, game.Name), Subtitle: subtitle,
			Action: &wire.Action{ID: kind + "-game-" + id},
		}
		if label := t.backlog.statuses[entries[game.AppID].Status]; label != "" {
			row.Badge = &wire.Badge{Text: label}
		}
		section.Items = append(section.Items, row)
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

func (a *app) gameDetail(ctx context.Context, d *steamData, t *text, st pageState) *wire.View {
	view := &wire.View{Title: t.details, State: ext.EncodeState(st)}
	section := wire.Section{Items: []wire.Component{button(t.back, "back")}}
	var p reading[player]
	if d.session == nil {
		p = d.account(ctx, profileTTL, false)
	}
	var games reading[gameList]
	if st.From == recentSection {
		games = d.recentGames(ctx, p, false)
	} else {
		games = d.ownedGames(ctx, p, false)
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
	progress := d.achievements(ctx, p, game.AppID, t.storeLanguage)
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
	steamID, _ := d.readID(p)
	st.BacklogAccount = steamID
	view.State = ext.EncodeState(st)
	view.Sections = []wire.Section{section, achievement, a.backlogDetail(t, steamID, game.AppID)}
	return view
}

func backlogSection(t *text, owned *gameList, recent reading[gameList], entries map[uint32]BacklogEntry, issue problem) wire.Section {
	section := wire.Section{Title: t.backlog.title}
	if issue != "" {
		section.Items = []wire.Component{empty(t.message(issue, false))}
		return section
	}
	counts := backlogCounts(backlogGames(owned, recent, entries))
	for _, buckets := range [][]string{backlogBuckets[:4], backlogBuckets[4:]} {
		stats := make([]wire.Stat, 0, len(buckets))
		for _, bucket := range buckets {
			stats = append(stats, wire.Stat{Label: t.backlog.bucketTitle(bucket), Value: strconv.Itoa(counts[bucket])})
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: stats})
	}
	section.Footer = t.backlog.overlap
	if owned.Truncated {
		section.Footer = joinText(section.Footer, fmt.Sprintf(t.capped, len(owned.Games), owned.Total))
	}
	return section
}

func (a *app) backlogDetail(t *text, steamID string, appID uint32) wire.Section {
	section := wire.Section{Title: t.backlog.annotation}
	entry, marked, err := a.backlog.Get(steamID, appID)
	if err != nil {
		section.Items = []wire.Component{empty(t.message(problemBacklogStore, false))}
		return section
	}
	status := entry.Status
	if !marked {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: t.backlog.unmarked})
	}
	if t.backlog.statuses[status] == "" {
		status = BacklogUnplayed
	}
	if note := cleanText(entry.Note, maxBacklogNoteRunes); note != "" {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Title: t.backlog.note, Subtitle: note})
	}
	if tags := cleanText(strings.Join(entry.Tags, " · "), maxBacklogTags*(maxBacklogTagRunes+3)); tags != "" {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Title: t.backlog.tags, Subtitle: tags})
	}
	choices := make([]wire.Choice, 0, len(backlogStatuses))
	for _, value := range backlogStatuses {
		choices = append(choices, wire.Choice{Value: string(value), Title: t.backlog.statuses[value]})
	}
	section.Items = append(section.Items, wire.Component{
		Type: wire.ComponentField, Kind: "choice", Label: t.backlog.status, Name: backlogStatusField,
		Value: string(status), Choices: choices,
	}, wire.Component{
		Type: wire.ComponentButton, Title: t.backlog.save, Action: &wire.Action{ID: "backlog-save", Submit: true},
	})
	if marked {
		section.Items = append(section.Items, wire.Component{
			Type: wire.ComponentButton, Title: t.backlog.clear,
			Action: &wire.Action{ID: "backlog-clear", Confirm: t.backlog.confirmClear},
		})
	}
	return section
}

func (a *app) backlogAction(ctx context.Context, r *ext.UIRequest, t *text, st pageState) (*wire.ActionResult, error) {
	if !validSteamID(st.BacklogAccount) {
		return nil, invalid(t.invalidAction)
	}
	args := backlogMarkArgs{AppID: st.AppID, Clear: r.Action == "backlog-clear"}
	if !args.Clear {
		status := BacklogStatus(r.Values[backlogStatusField])
		args.Status = &status
	}
	if args.validate() != nil {
		return nil, invalid(t.invalidAction)
	}
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	id, issue := d.backlogMarkID(ctx)
	if issue == "" {
		if id != st.BacklogAccount {
			return nil, invalid(t.backlog.accountChanged)
		}
		_, issue = a.markBacklog(ctx, d, id, args)
	}
	if issue != "" {
		return nil, &ext.Error{Code: string(issue), Message: t.message(issue, false)}
	}
	toast := t.backlog.saved
	if args.Clear {
		toast = t.backlog.cleared
	}
	return &wire.ActionResult{View: a.accountPage(ctx, d, t, st, false), Toast: toast}, nil
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
