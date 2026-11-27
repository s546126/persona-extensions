package main

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	defaultToolLimit = 20
	maxToolLimit     = 50
	pageProperties   = `"offset":{"type":"integer","minimum":0,"maximum":20000,"default":0},"limit":{"type":"integer","minimum":1,"maximum":50,"default":20}`
)

var tools = []wire.ConnectorTool{
	{
		Name: "steam_profile", Description: "Read the configured Steam player's profile, online or in-game state, and visible owned game count.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
	{
		Name: "steam_recent_games", Description: "Read Steam games played in the last two weeks, recent and lifetime minutes, and the two-week total. Use next_offset to read more.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `}}`),
	},
	{
		Name: "steam_library", Description: "List owned Steam games or search by title or AppID. Sort by lifetime playtime (descending) or name. Use next_offset to read more.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `,"query":{"type":"string","description":"Title or AppID, up to 100 characters."},"sort":{"type":"string","enum":["playtime","name"],"default":"playtime"}}}`),
	},
	{
		Name: "steam_achievements", Description: "Read unlocked and total Steam achievements for one AppID, subject to the player's Game details privacy settings.",
		InputSchema: json.RawMessage(`{"type":"object","required":["appid"],"properties":{"appid":{"type":"integer","minimum":1,"maximum":4294967295}}}`),
	},
	{
		Name: "steam_wishlist", Description: "Read the visible Steam wishlist with Store price and discount in the configured country. Use next_offset to read more; check prices_checked before interpreting the discount count.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `}}`),
	},
}

type listArgs struct {
	Offset int  `json:"offset"`
	Limit  *int `json:"limit"`
}

func (p listArgs) limit() int {
	if p.Limit != nil {
		return *p.Limit
	}
	return defaultToolLimit
}

func (p listArgs) valid() bool {
	return p.Offset >= 0 && p.Offset <= maxGames && p.limit() >= 1 && p.limit() <= maxToolLimit
}

type toolWarning struct {
	Dataset string  `json:"dataset"`
	Code    problem `json:"code"`
	Message string  `json:"message"`
	Stale   bool    `json:"stale,omitempty"`
}

func invalid(message string) error {
	return &ext.Error{Code: "invalid", Message: message}
}

func decodeArgs(raw json.RawMessage, dest any) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	if raw[0] != '{' || !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dest) == nil
}

func describeReading[T any](out map[string]any, dataset string, reading reading[T], t *text) {
	if reading.Issue != "" {
		warnings, _ := out["warnings"].([]toolWarning)
		out["warnings"] = append(warnings, toolWarning{dataset, reading.Issue, t.message(reading.Issue, reading.Stale), reading.Stale})
	}
}

func setPage[T any](out map[string]any, key string, items []T, p listArgs) {
	start := min(len(items), p.Offset)
	end := min(len(items), start+p.limit())
	out[key] = items[start:end]
	out["offset"], out["limit"], out["matched_count"] = p.Offset, p.limit(), len(items)
	if end < len(items) {
		out["next_offset"] = end
	}
}

func (a *app) connector(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var call wire.ConnectorCallParams
	if len(params) != 0 && json.Unmarshal(params, &call) != nil {
		return nil, invalid("Invalid Steam connector parameters.")
	}
	if call.Contribution != "" && call.Contribution != extensionID {
		return nil, &ext.Error{Code: wire.ErrorCodeNoContribution, Message: "Unknown Steam connector."}
	}
	switch method {
	case "status":
		return a.status(), nil
	case "tools.list":
		return wire.ConnectorTools{Tools: tools}, nil
	case "tools.call":
		return a.callTool(ctx, call.Name, call.Args)
	default:
		return nil, ext.UnknownMethod("connector")
	}
}

func (a *app) status() wire.ConnectorStatus {
	out := wire.ConnectorStatus{State: wire.ConnectorStateOk, Via: extensionID}
	issue := a.setup
	if p := a.lastAccount.Load(); p != nil {
		out.LastSync = p.At
		if p.Data != nil {
			out.Account = p.Data.Name
		}
		// Keep the read tools callable during outages and for private profiles.
		if p.Issue == problemInvalidID || p.Issue == problemKey {
			issue = p.Issue
		}
	}
	switch issue {
	case "":
	case problemSetup, problemInvalidID, problemCountry:
		out.State = wire.ConnectorStateNotConfigured
	case problemKey:
		out.State = wire.ConnectorStateNotSignedIn
	default:
		out.State = wire.ConnectorStateError
	}
	out.Error = textFor(a.locale).message(issue, false)
	return out
}

func (a *app) toolAccount(ctx context.Context, out map[string]any, t *text) reading[player] {
	p := a.account(ctx, profileTTL, false)
	if p.Stale {
		describeReading(out, "profile", p, t)
	}
	return p
}

func (a *app) callTool(ctx context.Context, name string, raw json.RawMessage) (wire.ConnectorCallResult, error) {
	const badPaging = "Use an offset from 0 to 20000 and a limit from 1 to 50."
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	out := map[string]any{"available": false}
	t := textFor(a.locale)
	switch name {
	case "steam_profile":
		var args struct{}
		if !decodeArgs(raw, &args) {
			return wire.ConnectorCallResult{}, invalid("steam_profile takes no arguments.")
		}
		p := a.account(ctx, presenceTTL, false)
		describeReading(out, "profile", p, t)
		if p.Data != nil {
			out["available"], out["profile"] = true, p.Data
			games := a.ownedGames(ctx, p, false)
			describeReading(out, "library", games, t)
			if games.Data != nil {
				out["owned_game_count"] = games.Data.Total
			}
		}
	case "steam_recent_games":
		var args listArgs
		if !decodeArgs(raw, &args) || !args.valid() {
			return wire.ConnectorCallResult{}, invalid(badPaging)
		}
		p := a.toolAccount(ctx, out, t)
		r := a.recentGames(ctx, p, false)
		describeReading(out, "recent_games", r, t)
		if r.Data != nil {
			out["available"], out["playtime_2weeks"], out["truncated"] = true, r.Data.Playtime2Weeks, r.Data.Truncated
			setPage(out, "games", r.Data.Games, args)
		}
	case "steam_library":
		var args struct {
			listArgs
			Query string `json:"query"`
			Sort  string `json:"sort"`
		}
		if !decodeArgs(raw, &args) || !args.valid() {
			return wire.ConnectorCallResult{}, invalid("Use a query of at most 100 characters, sort by playtime or name, and specify bounded pagination.")
		}
		if args.Sort == "" {
			args.Sort = sortPlaytime
		}
		if utf8.RuneCountInString(args.Query) > maxQueryRunes || (args.Sort != sortName && args.Sort != sortPlaytime) {
			return wire.ConnectorCallResult{}, invalid("Use a query of at most 100 characters and sort by playtime or name.")
		}
		p := a.toolAccount(ctx, out, t)
		r := a.ownedGames(ctx, p, false)
		describeReading(out, "library", r, t)
		if r.Data != nil {
			out["available"], out["owned_game_count"], out["truncated"] = true, r.Data.Total, r.Data.Truncated
			setPage(out, "games", libraryGames(r.Data.Games, args.Query, args.Sort), args.listArgs)
		}
	case "steam_achievements":
		var args struct {
			AppID uint32 `json:"appid"`
		}
		if !decodeArgs(raw, &args) || args.AppID == 0 {
			return wire.ConnectorCallResult{}, invalid("Provide a positive integer Steam AppID.")
		}
		p := a.toolAccount(ctx, out, t)
		r := a.achievements(ctx, p, args.AppID)
		describeReading(out, "achievements", r, t)
		if r.Data != nil {
			out["available"], out["progress"] = true, r.Data
		}
	case "steam_wishlist":
		var args listArgs
		if !decodeArgs(raw, &args) || !args.valid() {
			return wire.ConnectorCallResult{}, invalid(badPaging)
		}
		p := a.toolAccount(ctx, out, t)
		r := a.wishlist(ctx, p, t.storeLanguage, false)
		describeReading(out, "wishlist", r, t)
		out["country_code"] = a.config.CountryCode
		if r.Data != nil {
			out["available"], out["wishlist_count"] = true, r.Data.Total
			out["discounted_count"], out["prices_checked"] = r.Data.Discounted, r.Data.Checked
			out["truncated"] = r.Data.Total > len(r.Data.Items)
			setPage(out, "items", r.Data.Items, args)
		}
	default:
		return wire.ConnectorCallResult{}, invalid("Unknown Steam read tool.")
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return wire.ConnectorCallResult{}, &ext.Error{Code: "internal", Message: "Could not encode Steam data."}
	}
	return wire.ConnectorCallResult{Text: string(payload)}, nil
}
