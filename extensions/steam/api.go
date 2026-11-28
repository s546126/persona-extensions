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
	playerLinkPath   = "/IPlayerService/GetPlayerLinkDetails/v1/"
	ownedPath        = "/IPlayerService/GetOwnedGames/v1/"
	recentPath       = "/IPlayerService/GetRecentlyPlayedGames/v1/"
	achievementsPath = "/ISteamUserStats/GetPlayerAchievements/v1/"
	progressPath     = "/IPlayerService/GetAchievementsProgress/v1/"
	wishlistPath     = "/IWishlistService/GetWishlist/v1/"
	storeNamesPath   = "/IStoreBrowseService/GetItems/v1/"
	storePricesPath  = "/api/appdetails"

	maxAPIBytes = 16 << 20
	maxGames    = 20_000
	nameRunes   = 120
)

type steamClient struct {
	http    *http.Client
	key     string
	access  string
	refresh string // redaction only; never sent to a read endpoint
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
	return c.readJSON(ctx, target, params, nil, authenticated, out)
}

func (c steamClient) readJSON(ctx context.Context, target string, params, form url.Values, authenticated bool, out any) problem {
	return c.readBoundedJSON(ctx, target, params, form, authenticated, maxAPIBytes, out)
}

func (c steamClient) readBoundedJSON(ctx context.Context, target string, params, form url.Values, authenticated bool, maxBytes int64, out any) problem {
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method, body = http.MethodPost, strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil ||
		(req.URL.Host != apiHost && req.URL.Host != storeHost) ||
		params.Has("key") || params.Has("access_token") || form.Has("key") || form.Has("access_token") {
		return problemResponse
	}
	req.URL.RawQuery = params.Encode()
	req.Header.Set("Accept", "application/json")
	if authenticated {
		if req.URL.Host != apiHost {
			return problemResponse
		}
		switch {
		case c.access != "":
			q := req.URL.Query()
			q.Set("access_token", c.access)
			req.URL.RawQuery = q.Encode()
		case c.key != "":
			req.Header.Set("X-WebAPI-Key", c.key)
		default:
			return problemAuthRequired
		}
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return sanitizeError(err)
	}
	defer resp.Body.Close()
	if p := httpProblem(resp.StatusCode); p != "" {
		return p
	}
	if c.access != "" && authenticated {
		if header := resp.Header.Get("X-EResult"); header != "" {
			result, err := strconv.Atoi(header)
			if err != nil {
				return problemResponse
			}
			switch issue := authResult(result); issue {
			case "":
			case problemAuthExpired:
				return problemDenied
			case problemAuthPending, problemAuthRefused:
				return problemResponse
			default:
				return issue
			}
		}
	}
	if resp.ContentLength > maxBytes {
		return problemResponse
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return sanitizeError(err)
	}
	if int64(len(raw)) > maxBytes || json.Unmarshal(raw, out) != nil {
		return problemResponse
	}
	return ""
}

func httpProblem(status int) problem {
	switch status {
	case http.StatusOK:
		return ""
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
		if status >= 500 {
			return problemNetwork
		}
		return problemResponse
	}
}

// Never format a transport error. In particular, *url.Error can contain the
// entire credential-bearing URL, and its wrapped error may contain it again.
func sanitizeError(err error) problem {
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
	return cleanText(redactCredentials(s, c.key, c.access, c.refresh), nameRunes)
}

func redactCredentials(s string, secrets ...string) string {
	lower := strings.ToLower(s)
	for _, marker := range []string{"https://s.team/q/", "access_token=", "refresh_token=", "request_id="} {
		if strings.Contains(lower, marker) {
			return "[redacted]"
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
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
	if c.access != "" {
		return c.playerLink(ctx, id)
	}
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
			c.setPresence(p, &item.State, item.GameID, item.Game)
		}
		return p, ""
	}
	return nil, problemInvalidID
}

func (c steamClient) playerLink(ctx context.Context, id string) (*player, problem) {
	var reply struct {
		Response *struct {
			Accounts []struct {
				Public *struct {
					SteamID    json.Number `json:"steamid"`
					Name       string      `json:"persona_name"`
					Visibility int         `json:"visibility_state"`
				} `json:"public_data"`
				Private *struct {
					State  *int        `json:"persona_state"`
					GameID json.Number `json:"game_id"`
					Game   string      `json:"game_extra_info"`
				} `json:"private_data"`
			} `json:"accounts"`
		} `json:"response"`
	}
	switch issue := c.get(ctx, apiBase+playerLinkPath, url.Values{"steamids[0]": {id}}, true, &reply); issue {
	case "":
	case problemDenied, problemMissing, problemBadCall:
		return nil, problemProfileUnavailable
	default:
		return nil, issue
	}
	if reply.Response == nil {
		return nil, problemResponse
	}
	for _, account := range reply.Response.Accounts {
		if account.Public == nil || account.Public.SteamID.String() != id {
			continue
		}
		p := &player{SteamID: id, Name: c.text(account.Public.Name), Public: account.Public.Visibility == 3, State: "unknown"}
		if private := account.Private; private != nil {
			c.setPresence(p, private.State, private.GameID.String(), private.Game)
		}
		return p, ""
	}
	return nil, problemProfileUnavailable
}

func (c steamClient) setPresence(p *player, state *int, gameID, name string) {
	states := [...]string{"offline", "online", "busy", "away", "snooze", "looking_to_trade", "looking_to_play", "invisible"}
	if state != nil && *state >= 0 && *state < len(states) {
		p.State = states[*state]
	}
	if appID, err := strconv.ParseUint(gameID, 10, 32); err == nil {
		p.PlayApp = uint32(appID)
	}
	p.Playing = c.text(name)
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

func (c steamClient) achievements(ctx context.Context, id string, appID uint32, language string) (*achievementProgress, problem) {
	if c.access != "" {
		return c.achievementTotals(ctx, id, appID, language)
	}
	ps, issue := c.legacyPlayerAchievements(ctx, id, appID)
	if issue != "" {
		return nil, issue
	}
	return ps.progress(appID), ""
}

func (c steamClient) achievementTotals(ctx context.Context, id string, appID uint32, language string) (*achievementProgress, problem) {
	input, _ := json.Marshal(struct {
		SteamID  string   `json:"steamid"`
		Language string   `json:"language"`
		AppIDs   []uint32 `json:"appids"`
	}{id, language, []uint32{appID}})
	var reply struct {
		Response *struct {
			Progress []struct {
				AppID    uint32 `json:"appid"`
				Unlocked int    `json:"unlocked"`
				Total    *int   `json:"total"`
			} `json:"achievement_progress"`
		} `json:"response"`
	}
	switch issue := c.readJSON(ctx, apiBase+progressPath, nil, url.Values{"input_json": {string(input)}}, true, &reply); issue {
	case "":
	case problemDenied, problemMissing, problemBadCall:
		return nil, problemAchievements
	default:
		return nil, issue
	}
	if reply.Response != nil {
		for _, p := range reply.Response.Progress {
			if p.AppID == appID && p.Total != nil && p.Unlocked >= 0 && p.Unlocked <= *p.Total {
				return &achievementProgress{AppID: appID, Unlocked: p.Unlocked, Total: *p.Total}, ""
			}
		}
	}
	return nil, problemAchievements
}

func appIDText(id uint32) string { return strconv.FormatUint(uint64(id), 10) }

func storeLink(id uint32) string { return storeBase + "/app/" + appIDText(id) + "/" }
