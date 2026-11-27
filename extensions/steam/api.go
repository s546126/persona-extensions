package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	apiHost   = "api.steampowered.com"
	storeHost = "store.steampowered.com"
	apiBase   = "https://" + apiHost
	storeBase = "https://" + storeHost

	vanityPath       = "/ISteamUser/ResolveVanityURL/v1/"
	playerPath       = "/ISteamUser/GetPlayerSummaries/v2/"
	ownedPath        = "/IPlayerService/GetOwnedGames/v1/"
	recentPath       = "/IPlayerService/GetRecentlyPlayedGames/v1/"
	achievementsPath = "/ISteamUserStats/GetPlayerAchievements/v1/"
	wishlistPath     = "/IWishlistService/GetWishlist/v1/"
	storeNamesPath   = "/IStoreBrowseService/GetItems/v1/"
	storePricesPath  = "/api/appdetails"

	maxAPIBytes = 16 << 20
	maxGames    = 20_000
	nameRunes   = 120
)

type steamClient struct {
	http *http.Client
	key  string
}

func steamHTTP(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// The credential is a custom header that the default redirect policy could forward.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c steamClient) get(ctx context.Context, target string, params url.Values, authenticated bool, out any) problem {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil ||
		(req.URL.Host != apiHost && req.URL.Host != storeHost) {
		return problemResponse
	}
	req.URL.RawQuery = params.Encode()
	req.Header.Set("Accept", "application/json")
	if authenticated {
		if req.URL.Host != apiHost {
			return problemResponse
		}
		req.Header.Set("X-WebAPI-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return requestProblem(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return problemRateLimited
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return problemTimeout
	case http.StatusUnauthorized, http.StatusForbidden:
		return problemDenied
	case http.StatusNotFound:
		return problemMissing
	case http.StatusBadRequest:
		return problemBadCall
	default:
		if resp.StatusCode >= 500 {
			return problemNetwork
		}
		return problemResponse
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	if err != nil {
		return requestProblem(err)
	}
	if len(raw) > maxAPIBytes || json.Unmarshal(raw, out) != nil {
		return problemResponse
	}
	return ""
}

func requestProblem(err error) problem {
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return problemCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return problemTimeout
	default:
		return problemNetwork
	}
}

func cleanText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}

func (c steamClient) text(s string) string {
	if c.key != "" {
		s = strings.ReplaceAll(s, c.key, "[redacted]")
	}
	return cleanText(s, nameRunes)
}

func (c steamClient) resolve(ctx context.Context, name string) (*string, problem) {
	var reply struct {
		Response *struct {
			SteamID string `json:"steamid"`
			Success int    `json:"success"`
		} `json:"response"`
	}
	switch p := c.get(ctx, apiBase+vanityPath, url.Values{"vanityurl": {name}}, true, &reply); p {
	case "":
	case problemDenied:
		return nil, problemKey
	case problemBadCall, problemMissing:
		return nil, problemInvalidID
	default:
		return nil, p
	}
	if reply.Response == nil {
		return nil, problemResponse
	}
	if reply.Response.Success != 1 || !validSteamID(reply.Response.SteamID) {
		return nil, problemInvalidID
	}
	return &reply.Response.SteamID, ""
}

func (c steamClient) profile(ctx context.Context, id string) (*player, problem) {
	var reply struct {
		Response *struct {
			Players []struct {
				SteamID    string `json:"steamid"`
				Name       string `json:"personaname"`
				Visibility int    `json:"communityvisibilitystate"`
				State      int    `json:"personastate"`
				Game       string `json:"gameextrainfo"`
				GameID     string `json:"gameid"`
			} `json:"players"`
		} `json:"response"`
	}
	switch p := c.get(ctx, apiBase+playerPath, url.Values{"steamids": {id}}, true, &reply); p {
	case "":
	case problemDenied:
		return nil, problemKey
	case problemBadCall, problemMissing:
		return nil, problemInvalidID
	default:
		return nil, p
	}
	if reply.Response == nil || reply.Response.Players == nil {
		return nil, problemResponse
	}
	for _, item := range reply.Response.Players {
		if item.SteamID != id {
			continue
		}
		p := &player{SteamID: id, Name: c.text(item.Name), Public: item.Visibility == 3, State: "unknown"}
		if p.Public {
			states := [...]string{"offline", "online", "busy", "away", "snooze", "looking_to_trade", "looking_to_play", "invisible"}
			if item.State >= 0 && item.State < len(states) {
				p.State = states[item.State]
			}
			if appID, err := strconv.ParseUint(item.GameID, 10, 32); err == nil {
				p.PlayApp = uint32(appID)
			}
			p.Playing = c.text(item.Game)
		}
		return p, ""
	}
	return nil, problemInvalidID
}

func (c steamClient) games(ctx context.Context, id string, recent bool) (*gameList, problem) {
	path := ownedPath
	params := url.Values{"steamid": {id}}
	if recent {
		path = recentPath
	} else {
		params.Set("include_appinfo", "1")
		params.Set("include_played_free_games", "1")
	}
	var reply struct {
		Response *struct {
			GameCount  *int   `json:"game_count"`
			TotalCount *int   `json:"total_count"`
			Games      []game `json:"games"`
		} `json:"response"`
	}
	switch p := c.get(ctx, apiBase+path, params, true, &reply); p {
	case "":
	case problemDenied, problemMissing:
		return nil, problemGameDetails
	default:
		return nil, p
	}
	if reply.Response == nil {
		return nil, problemResponse
	}
	r := reply.Response
	count := r.GameCount
	if recent {
		count = r.TotalCount
	}
	if count == nil && r.Games == nil {
		return nil, problemGameDetails
	}
	if count != nil && *count < 0 {
		return nil, problemResponse
	}
	list := &gameList{Games: make([]game, 0, min(len(r.Games), maxGames))}
	seen := make(map[uint32]struct{}, len(r.Games))
	for _, g := range r.Games {
		if _, ok := seen[g.AppID]; g.AppID == 0 || ok {
			continue
		}
		seen[g.AppID] = struct{}{}
		if recent {
			list.Playtime2Weeks += uint64(g.Playtime2Weeks)
		}
		if len(list.Games) < maxGames {
			g.Name = c.text(g.Name)
			list.Games = append(list.Games, g)
		}
	}
	list.Total = len(seen)
	if count != nil {
		list.Total = max(list.Total, *count)
	}
	list.Truncated = list.Total > len(list.Games)
	if recent {
		slices.SortFunc(list.Games, compareRecent)
	}
	return list, ""
}

func (c steamClient) achievements(ctx context.Context, id string, appID uint32) (*achievementProgress, problem) {
	var reply struct {
		PlayerStats *struct {
			Game         string `json:"gameName"`
			Success      bool   `json:"success"`
			Achievements []struct {
				Achieved int `json:"achieved"`
			} `json:"achievements"`
		} `json:"playerstats"`
	}
	params := url.Values{"steamid": {id}, "appid": {appIDText(appID)}}
	switch p := c.get(ctx, apiBase+achievementsPath, params, true, &reply); p {
	case "":
	case problemDenied, problemMissing, problemBadCall:
		return nil, problemAchievements
	default:
		return nil, p
	}
	if reply.PlayerStats == nil {
		return nil, problemResponse
	}
	ps := reply.PlayerStats
	if !ps.Success {
		return nil, problemAchievements
	}
	progress := &achievementProgress{AppID: appID, Game: c.text(ps.Game), Total: len(ps.Achievements)}
	for _, a := range ps.Achievements {
		if a.Achieved == 1 {
			progress.Unlocked++
		}
	}
	return progress, ""
}

func appIDText(id uint32) string { return strconv.FormatUint(uint64(id), 10) }

func storeLink(id uint32) string { return storeBase + "/app/" + appIDText(id) + "/" }
