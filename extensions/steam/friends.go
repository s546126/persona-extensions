package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	friendsRadarTool  = "steam_friends_radar"
	coopOverlapTool   = "steam_coop_overlap"
	friendListPath    = "/ISteamUser/GetFriendList/v1/"
	friendTTL         = 90 * time.Second
	friendBatchSize   = 100
	maxFriends        = 2_000
	pageFriendsShown  = 5
	maxOverlapFriends = 5
	maxCoopScan       = 3 * storeBatchSize

	problemFriendsDisabled  problem = "friends_radar_disabled"
	problemFriendList       problem = "friends_list_unavailable"
	problemFriendListQR     problem = "qr_friends_list_unavailable"
	problemFriendState      problem = "friends_presence_unavailable"
	problemFriendStateQR    problem = "qr_friends_presence_unavailable"
	problemFriendListCap    problem = "friends_list_truncated"
	problemFriendUnverified problem = "friend_not_verified"
	problemFriendName       problem = "friend_name_not_found"
	problemFriendNameDup    problem = "friend_name_ambiguous"
	problemFriendLibrary    problem = "friend_game_details_unavailable"
	problemFriendLibraryQR  problem = "qr_game_details_unavailable"
	problemFriendLibraryCap problem = "shared_library_truncated"
	problemCoopStore        problem = "store_categories_unavailable"
)

var friendsTools = []wire.ConnectorTool{
	{
		Name:        friendsRadarTool,
		Description: "Read friends' visible presence and current games. The in_game_now groups cover the returned page. Use next_offset to read more.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `,"online_only":{"type":"boolean","default":true},"in_game_only":{"type":"boolean","default":false}}}`),
	},
	{
		Name:        coopOverlapTool,
		Description: "Find games shared by the current account and all selected friends whose libraries Steam shares. Provide one friend selector and continue with next_offset. Check friend visibility and coop_support in the result.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `,"friend_steam_id":{"type":"string","pattern":"^[0-9]{17}$"},"friend_steam_ids":{"type":"array","description":"One to five distinct friend SteamID64 strings.","items":{"type":"string","pattern":"^[0-9]{17}$"}},"friend_query":{"type":"string","description":"An exact friend nickname, ignoring case."},"coop_only":{"type":"boolean","default":true,"description":"Keep multiplayer, co-op and games with unknown Store categories."}}}`),
	},
}

type friendsRadarArgs struct {
	listArgs
	OnlineOnly *bool `json:"online_only"`
	InGameOnly bool  `json:"in_game_only"`
}

type friendRoster struct {
	ids       []string
	truncated bool
}

type friendActivity struct {
	roster  *friendRoster
	players []player
	issue   problem
}

func (d *steamData) readFriendRoster(ctx context.Context) (string, reading[friendRoster]) {
	if d.setup != "" {
		return "", reading[friendRoster]{Issue: d.setup}
	}
	id := d.config.SteamID
	if !validSteamID(id) {
		r := d.resolveVanity(ctx, false)
		// A stale vanity mapping could belong to a different account.
		if r.Issue != "" {
			return "", reading[friendRoster]{Issue: r.Issue}
		}
		id = *r.Data
	}
	return id, d.friendList.read(ctx, id, friendTTL, false, func(ctx context.Context) (*friendRoster, problem) {
		client, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		return client.friendsList(ctx, id)
	})
}

func (c steamClient) friendsList(ctx context.Context, id string) (*friendRoster, problem) {
	var reply struct {
		List *struct {
			Friends []struct {
				SteamID      string `json:"steamid"`
				Relationship string `json:"relationship"`
			} `json:"friends"`
		} `json:"friendslist"`
	}
	unavailable := problemFriendList
	if c.access != "" {
		unavailable = problemFriendListQR
	}
	params := url.Values{"steamid": {id}, "relationship": {"friend"}}
	switch issue := c.get(ctx, apiBase+friendListPath, params, true, &reply); issue {
	case "":
	case problemBadCall, problemDenied, problemMissing:
		return nil, unavailable
	default:
		return nil, issue
	}
	if reply.List == nil || reply.List.Friends == nil {
		return nil, unavailable
	}
	roster := &friendRoster{ids: make([]string, 0, min(len(reply.List.Friends), maxFriends))}
	seen := make(map[string]bool, cap(roster.ids))
	for _, f := range reply.List.Friends {
		if f.SteamID == id || !validSteamID(f.SteamID) || (f.Relationship != "" && f.Relationship != "friend") || seen[f.SteamID] {
			continue
		}
		if len(roster.ids) == maxFriends {
			roster.truncated = true
			break
		}
		seen[f.SteamID] = true
		roster.ids = append(roster.ids, f.SteamID)
	}
	slices.Sort(roster.ids)
	return roster, ""
}

func (d *steamData) readFriendPresence(ctx context.Context, id string, roster *friendRoster) reading[[]player] {
	// A changed roster must never reuse presence from a removed friend.
	digest := sha256.Sum256([]byte(strings.Join(roster.ids, ",")))
	return d.friendState.read(ctx, fmt.Sprintf("%s/%x", id, digest), friendTTL, false, func(ctx context.Context) (*[]player, problem) {
		if len(roster.ids) == 0 {
			empty := []player{}
			return &empty, ""
		}
		client, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		return client.friendsPresence(ctx, roster.ids)
	})
}

func (c steamClient) friendsPresence(ctx context.Context, ids []string) (*[]player, problem) {
	players := make([]player, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	var issue problem
	for batch := range slices.Chunk(ids, friendBatchSize) {
		var batchPlayers []player
		if c.access != "" {
			batchPlayers, issue = c.friendLinks(ctx, batch)
		} else {
			batchPlayers, issue = c.friendSummaries(ctx, batch)
		}
		if issue != "" {
			if len(players) == 0 {
				return nil, issue
			}
			break
		}
		for _, p := range batchPlayers {
			if seen[p.SteamID] {
				continue
			}
			if p.State == "invisible" {
				p.State, p.PlayApp, p.Playing = "offline", 0, ""
			}
			seen[p.SteamID] = true
			players = append(players, p)
		}
	}
	slices.SortFunc(players, func(a, b player) int {
		if order := cmp.Compare(friendRank(a), friendRank(b)); order != 0 {
			return order
		}
		if byName := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); byName != 0 {
			return byName
		}
		return strings.Compare(a.SteamID, b.SteamID)
	})
	return &players, issue
}

func (c steamClient) friendSummaries(ctx context.Context, ids []string) ([]player, problem) {
	var reply struct {
		Response *struct {
			Players []struct {
				SteamID    string `json:"steamid"`
				Name       string `json:"personaname"`
				Visibility int    `json:"communityvisibilitystate"`
				State      *int   `json:"personastate"`
				GameID     string `json:"gameid"`
				Game       string `json:"gameextrainfo"`
			} `json:"players"`
		} `json:"response"`
	}
	params := url.Values{"steamids": {strings.Join(ids, ",")}}
	switch issue := c.get(ctx, apiBase+playerPath, params, true, &reply); issue {
	case "":
	case problemBadCall, problemDenied, problemMissing:
		return nil, problemFriendState
	default:
		return nil, issue
	}
	if reply.Response == nil || reply.Response.Players == nil {
		return nil, problemResponse
	}
	players := make([]player, 0, len(ids))
	for _, item := range reply.Response.Players {
		if !slices.Contains(ids, item.SteamID) {
			continue
		}
		p := player{SteamID: item.SteamID, Name: c.text(item.Name), Public: item.Visibility == 3, State: "unknown"}
		if p.Public {
			c.setPresence(&p, item.State, item.GameID, item.Game)
		}
		players = append(players, p)
	}
	return players, ""
}

func (c steamClient) friendLinks(ctx context.Context, ids []string) ([]player, problem) {
	var reply struct {
		Response *struct {
			Accounts []struct {
				Public *struct {
					SteamID    json.Number `json:"steamid"`
					Name       string      `json:"persona_name"`
					Visibility int         `json:"visibility_state"`
				} `json:"public_data"`
				Private *struct {
					State       *int        `json:"persona_state"`
					GameID      json.Number `json:"game_id"`
					Game        string      `json:"game_extra_info"`
					GamePrivate bool        `json:"game_is_private"`
				} `json:"private_data"`
			} `json:"accounts"`
		} `json:"response"`
	}
	params := make(url.Values, len(ids))
	for i, id := range ids {
		params.Set(fmt.Sprintf("steamids[%d]", i), id)
	}
	switch issue := c.get(ctx, apiBase+playerLinkPath, params, true, &reply); issue {
	case "":
	case problemBadCall, problemDenied, problemMissing:
		return nil, problemFriendStateQR
	default:
		return nil, issue
	}
	if reply.Response == nil {
		return nil, problemResponse
	}
	players := make([]player, 0, len(ids))
	for _, item := range reply.Response.Accounts {
		if item.Public == nil || !slices.Contains(ids, item.Public.SteamID.String()) {
			continue
		}
		p := player{SteamID: item.Public.SteamID.String(), Name: c.text(item.Public.Name), Public: item.Public.Visibility == 3, State: "unknown"}
		if details := item.Private; details != nil {
			if details.GamePrivate {
				c.setPresence(&p, details.State, "", "")
			} else {
				c.setPresence(&p, details.State, details.GameID.String(), details.Game)
			}
		}
		players = append(players, p)
	}
	return players, ""
}

func isPlaying(p player) bool { return p.PlayApp != 0 || p.Playing != "" }

func friendRank(p player) int {
	if isPlaying(p) {
		return 0
	}
	switch p.State {
	case "online", "looking_to_play", "looking_to_trade":
		return 1
	case "away", "busy", "snooze":
		return 2
	case "offline":
		return 3
	default:
		return 4
	}
}

func (d *steamData) readFriendActivity(ctx context.Context) friendActivity {
	id, roster := d.readFriendRoster(ctx)
	if roster.Data == nil {
		return friendActivity{issue: roster.Issue}
	}
	presence := d.readFriendPresence(ctx, id, roster.Data)
	activity := friendActivity{roster: roster.Data, issue: presence.Issue}
	if presence.Data != nil {
		activity.players = *presence.Data
	}
	return activity
}

func (d *steamData) friendsRadar(ctx context.Context, out map[string]any, t *text, args friendsRadarArgs) {
	a := d.readFriendActivity(ctx)
	describeReading(out, "friends_radar", reading[friendActivity]{Issue: a.issue}, t)
	if a.roster != nil {
		out["friend_count"] = len(a.roster.ids)
		if a.roster.truncated {
			describeReading(out, "friends_list", reading[friendRoster]{Issue: problemFriendListCap}, t)
		}
	}
	if a.players == nil {
		return
	}
	activeOnly := args.OnlineOnly == nil || *args.OnlineOnly
	matches := make([]player, 0, len(a.players))
	playing, known := 0, 0
	for _, p := range a.players {
		rank := friendRank(p)
		if rank == 0 {
			playing++
		}
		if rank < 4 {
			known++
		}
		if (args.InGameOnly && rank != 0) || (activeOnly && rank >= 3) {
			continue
		}
		matches = append(matches, p)
	}
	out["available"], out["in_game_count"] = true, playing
	out["presence_unknown_count"] = len(a.roster.ids) - known
	page := setPage(out, "friends", matches, args.listArgs)
	out["in_game_now"] = groupCurrentGames(page)
}

type currentFriendGame struct {
	AppID   uint32   `json:"appid,omitempty"`
	Game    string   `json:"game,omitempty"`
	Count   int      `json:"count"`
	Friends []string `json:"friends"`
}

func groupCurrentGames(page []player) []currentFriendGame {
	type gameKey struct {
		appID uint32
		name  string
	}
	groups := make([]currentFriendGame, 0)
	indices := make(map[gameKey]int)
	for _, p := range page {
		if !isPlaying(p) {
			break // In-game friends are first in the sorted page.
		}
		key := gameKey{appID: p.PlayApp}
		if key.appID == 0 {
			key.name = strings.ToLower(p.Playing)
		}
		i, found := indices[key]
		if !found {
			i = len(groups)
			indices[key] = i
			groups = append(groups, currentFriendGame{AppID: p.PlayApp})
		}
		group := &groups[i]
		if group.Game == "" {
			group.Game = p.Playing
		}
		name := p.Name
		if name == "" {
			name = p.SteamID
		}
		group.Friends = append(group.Friends, name)
		group.Count++
	}
	return groups
}

func (d *steamData) friendsSection(ctx context.Context, t *text) wire.Section {
	a := d.readFriendActivity(ctx)
	if a.players == nil {
		return wire.Section{Title: t.friendsPlaying, Items: []wire.Component{empty(t.message(a.issue, false))}}
	}
	section := wire.Section{Title: t.friendsPlaying, Footer: t.message(a.issue, false)}
	if a.roster != nil && a.roster.truncated {
		section.Footer = joinText(section.Footer, t.message(problemFriendListCap, false))
	}
	for i, p := range a.players {
		if !isPlaying(p) {
			break
		}
		if i == pageFriendsShown {
			section.Footer = joinText(section.Footer, t.moreFriendsPlaying)
			break
		}
		name := p.Name
		if name == "" {
			name = t.unknownFriend
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Title: name, Subtitle: t.gameName(p.PlayApp, p.Playing)})
	}
	if len(section.Items) == 0 {
		section.Items = []wire.Component{empty(t.noFriendsPlaying)}
	}
	return section
}

type coopOverlapArgs struct {
	listArgs
	FriendID    *string  `json:"friend_steam_id"`
	FriendIDs   []string `json:"friend_steam_ids"`
	FriendQuery *string  `json:"friend_query"`
	CoopOnly    *bool    `json:"coop_only"`
}

func (a *coopOverlapArgs) normalizeSelection() bool {
	if a.FriendQuery != nil {
		*a.FriendQuery = strings.TrimSpace(*a.FriendQuery)
		return a.FriendID == nil && a.FriendIDs == nil && *a.FriendQuery != "" && utf8.RuneCountInString(*a.FriendQuery) <= nameRunes
	}
	if a.FriendID != nil {
		if a.FriendIDs != nil {
			return false
		}
		a.FriendIDs = []string{*a.FriendID}
	}
	if len(a.FriendIDs) < 1 || len(a.FriendIDs) > maxOverlapFriends {
		return false
	}
	for i, id := range a.FriendIDs {
		id = strings.TrimSpace(id)
		if !validSteamID(id) || slices.Contains(a.FriendIDs[:i], id) {
			return false
		}
		a.FriendIDs[i] = id
	}
	return true
}

func (d *steamData) findFriend(ctx context.Context, id string, roster *friendRoster, query string) (player, problem) {
	if roster.truncated {
		return player{}, problemFriendListCap
	}
	result := d.readFriendPresence(ctx, id, roster)
	if result.Issue != "" {
		return player{}, result.Issue
	}
	var match player
	for _, p := range *result.Data {
		if !strings.EqualFold(p.Name, query) {
			continue
		}
		if match.SteamID != "" {
			return player{}, problemFriendNameDup
		}
		match = p
	}
	if match.SteamID == "" {
		return player{}, problemFriendName
	}
	return match, ""
}

const (
	libraryUnverified  = "not_verified"
	libraryUnavailable = "unavailable"
	libraryHidden      = "private_or_unavailable"
	libraryVisible     = "visible"
)

type overlapFriend struct {
	SteamID    string `json:"steam_id"`
	Name       string `json:"name,omitempty"`
	Visibility string `json:"library_visibility"`
}

func (d *steamData) coopOverlap(ctx context.Context, out map[string]any, t *text, args coopOverlapArgs) {
	account, roster := d.readFriendRoster(ctx)
	describeReading(out, "friends_list", roster, t)
	if roster.Data == nil {
		return
	}
	var name string
	if args.FriendQuery != nil {
		p, issue := d.findFriend(ctx, account, roster.Data, *args.FriendQuery)
		if issue != "" {
			describeReading(out, "friend_selection", reading[player]{Issue: issue}, t)
			return
		}
		args.FriendIDs, name = []string{p.SteamID}, p.Name
	}
	if roster.Data.truncated {
		describeReading(out, "friends_list", reading[friendRoster]{Issue: problemFriendListCap}, t)
	}
	friends := make([]overlapFriend, len(args.FriendIDs))
	verified := 0
	for i, id := range args.FriendIDs {
		f := overlapFriend{SteamID: id, Name: name, Visibility: libraryUnverified}
		if slices.Contains(roster.Data.ids, id) {
			f.Visibility = libraryUnavailable
			verified++
		}
		friends[i] = f
	}
	out["friends"], out["included_friend_count"] = friends, 0
	if verified < len(friends) {
		describeReading(out, "friend_selection", reading[player]{Issue: problemFriendUnverified}, t)
	}
	if verified == 0 {
		return
	}
	client, issue := d.readClient(ctx)
	if issue != "" {
		describeReading(out, "coop_overlap", reading[gameList]{Issue: issue}, t)
		return
	}
	own, peers := client.overlapLibraries(ctx, account, friends)
	describeReading(out, "library", own, t)
	visible := make([]*gameList, 0, verified)
	truncated := own.Data != nil && own.Data.Truncated
	reported := make(map[problem]bool)
	for i, read := range peers {
		if read.Data != nil {
			friends[i].Visibility = libraryVisible
			visible = append(visible, read.Data)
			truncated = truncated || read.Data.Truncated
		}
		if read.Issue == problemFriendLibrary || read.Issue == problemFriendLibraryQR {
			friends[i].Visibility = libraryHidden
		}
		if read.Issue != "" && !reported[read.Issue] {
			describeReading(out, "friend_library", read, t)
			reported[read.Issue] = true
		}
	}
	if truncated {
		describeReading(out, "coop_overlap", reading[gameList]{Issue: problemFriendLibraryCap}, t)
	}
	out["included_friend_count"] = len(visible)
	out["partial"] = truncated || own.Data == nil || len(visible) < len(friends)
	if own.Data == nil || len(visible) == 0 {
		return
	}
	shared := sharedGames(own.Data, visible)
	page, next, categoryIssue := client.coopPage(ctx, shared, args, d.config.CountryCode, t.storeLanguage)
	if categoryIssue != "" {
		describeReading(out, "store_categories", reading[gameList]{Issue: problemCoopStore}, t)
	}
	out["available"], out["offset"], out["limit"] = true, args.Offset, args.limit()
	out["games"], out["shared_count"] = page, len(shared)
	if next < len(shared) {
		out["next_offset"] = next
	}
}

func (c steamClient) overlapLibraries(ctx context.Context, account string, friends []overlapFriend) (reading[gameList], []reading[gameList]) {
	read := func(id string, friend bool) reading[gameList] {
		list, issue := c.games(ctx, id, false)
		if issue == problemGameDetails || issue == problemBadCall {
			switch {
			case c.access != "":
				issue = problemFriendLibraryQR
			case friend:
				issue = problemFriendLibrary
			}
		}
		return reading[gameList]{Data: list, Issue: issue}
	}
	// Read libraries directly. Snapshots and cache slots belong to the signed-in user's other reads.
	var own reading[gameList]
	peers := make([]reading[gameList], len(friends))
	var reads sync.WaitGroup
	reads.Go(func() { own = read(account, false) })
	for i, f := range friends {
		if f.Visibility != libraryUnverified {
			reads.Go(func() { peers[i] = read(f.SteamID, true) })
		}
	}
	reads.Wait()
	return own, peers
}

type sharedGame struct {
	AppID          uint32   `json:"appid"`
	Name           string   `json:"name,omitempty"`
	TotalPlaytime  uint64   `json:"total_playtime"`
	EveryonePlayed bool     `json:"everyone_played"`
	CoopSupport    string   `json:"coop_support"`
	Categories     []string `json:"multiplayer_categories,omitempty"`
}

func sharedGames(own *gameList, peers []*gameList) []sharedGame {
	common := make(map[uint32]sharedGame, len(own.Games))
	for _, g := range own.Games {
		common[g.AppID] = sharedGame{AppID: g.AppID, Name: g.Name, TotalPlaytime: uint64(g.PlaytimeForever), EveryonePlayed: g.PlaytimeForever > 0}
	}
	for _, library := range peers {
		next := make(map[uint32]sharedGame, min(len(common), len(library.Games)))
		for _, g := range library.Games {
			match, found := common[g.AppID]
			if !found {
				continue
			}
			if match.Name == "" {
				match.Name = g.Name
			}
			match.TotalPlaytime += uint64(g.PlaytimeForever)
			match.EveryonePlayed = match.EveryonePlayed && g.PlaytimeForever > 0
			next[g.AppID] = match
		}
		common = next
	}
	return slices.SortedFunc(maps.Values(common), func(a, b sharedGame) int {
		if byTime := cmp.Compare(b.TotalPlaytime, a.TotalPlaytime); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
}

const (
	coopSupported = "supported"
	coopNotListed = "not_listed"
	coopUnknown   = "unknown"
)

var multiplayerCategories = map[uint32]string{
	1: "Multi-player", 9: "Co-op", 24: "Shared/Split Screen", 27: "Cross-Platform Multiplayer",
	36: "Online PvP", 37: "Shared/Split Screen PvP", 38: "Online Co-op", 39: "Shared/Split Screen Co-op",
	47: "LAN PvP", 48: "LAN Co-op", 49: "PvP",
}

type storeCoopSupport struct {
	status     string
	categories []string
}

func classifyPlayerCategories(ids []uint32) storeCoopSupport {
	result := storeCoopSupport{status: coopUnknown}
	singlePlayerOnly := len(ids) > 0
	for _, id := range ids {
		if label := multiplayerCategories[id]; label != "" && !slices.Contains(result.categories, label) {
			result.categories = append(result.categories, label)
		}
		singlePlayerOnly = singlePlayerOnly && id == 2 // Single-player
	}
	switch {
	case len(result.categories) > 0:
		result.status = coopSupported
	case singlePlayerOnly:
		result.status = coopNotListed
	}
	return result
}

func (c steamClient) storeCoopCategories(ctx context.Context, games []sharedGame, country, language string) (map[uint32]storeCoopSupport, problem) {
	if err := ctx.Err(); err != nil {
		return nil, sanitizeError(err)
	}
	type appRef struct {
		AppID uint32 `json:"appid"`
	}
	ids := make([]appRef, 0, len(games))
	for _, game := range games {
		ids = append(ids, appRef{AppID: game.AppID})
	}
	query, _ := json.Marshal(map[string]any{
		"ids":     ids,
		"context": map[string]string{"country_code": country, "language": language},
	})
	var reply struct {
		Response *struct {
			Items []struct {
				AppID      uint32 `json:"appid"`
				Success    int    `json:"success"`
				Categories struct {
					Players []uint32 `json:"supported_player_categoryids"`
				} `json:"categories"`
			} `json:"store_items"`
		} `json:"response"`
	}
	if issue := c.get(ctx, apiBase+storeNamesPath, url.Values{"input_json": {string(query)}}, false, &reply); issue != "" {
		return nil, issue
	}
	if reply.Response == nil {
		return nil, problemResponse
	}
	result := make(map[uint32]storeCoopSupport, len(ids))
	for _, item := range reply.Response.Items {
		if item.Success == 1 && slices.Contains(ids, appRef{AppID: item.AppID}) {
			result[item.AppID] = classifyPlayerCategories(item.Categories.Players)
		}
	}
	return result, ""
}

func (c steamClient) coopPage(ctx context.Context, sorted []sharedGame, args coopOverlapArgs, country, language string) ([]sharedGame, int, problem) {
	coopOnly := args.CoopOnly == nil || *args.CoopOnly
	// The cursor counts examined entries in the raw intersection. A filtered page
	// can continue even if every game in this call's Store budget was single-player.
	limit, start := args.limit(), min(args.Offset, len(sorted))
	scan := limit
	if coopOnly {
		scan = maxCoopScan
	}
	page := make([]sharedGame, 0, limit)
	next := start
	var issue problem
	for batch := range slices.Chunk(sorted[start:min(len(sorted), start+scan)], storeBatchSize) {
		var support map[uint32]storeCoopSupport
		if issue == "" {
			support, issue = c.storeCoopCategories(ctx, batch, country, language)
		}
		for _, g := range batch {
			next++
			metadata := support[g.AppID]
			if metadata.status == "" {
				metadata.status = coopUnknown
			}
			if coopOnly && metadata.status == coopNotListed {
				continue
			}
			g.CoopSupport, g.Categories = metadata.status, metadata.categories
			page = append(page, g)
			if len(page) == limit {
				return page, next, issue
			}
		}
	}
	return page, next, issue
}
