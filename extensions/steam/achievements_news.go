package main

import (
	"cmp"
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	achievementHunterName  = "steam_achievement_hunter"
	globalAchievementsPath = "/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v2/"
	keyAchievementSchema   = "/ISteamUserStats/GetSchemaForGame/v2/"
	gameAchievementsPath   = "/IPlayerService/GetGameAchievements/v1/"
	userAchievementsPath   = "/IPlayerService/GetUserAchievements/v1/"

	publicCacheSize       = 16
	globalAchievementsTTL = time.Hour
	achievementIDBytes    = 160

	problemGlobalAchievements     problem = "global_achievements_unavailable"
	problemAchievementSchema      problem = "achievement_schema_unavailable"
	problemAchievementDetails     problem = "achievement_details_unavailable"
	problemAchievementUnlockTimes problem = "achievement_unlock_times_unavailable"
)

var achievementHunterTool = wire.ConnectorTool{
	Name:        achievementHunterName,
	Description: "Rank a Steam game's locked achievements by global unlock rate and list dated personal unlocks. When Steam shares fewer personal details, use the global easiest and rarest lists as a reference. Respect the hidden flag. Use next_offset to read more.",
	InputSchema: json.RawMessage(`{"type":"object","required":["appid"],"properties":{"appid":{"type":"integer","minimum":1,"maximum":4294967295},` + pageProperties + `}}`),
}

type steamPublicData struct {
	client  steamClient
	now     func() time.Time
	rates   publicCache[uint32, map[string]float64]
	schemas publicCache[achievementSchemaKey, achievementSchema]
	news    publicCache[newsCacheKey, []gameNewsItem]
	names   publicCache[storeNameKey, string]
}

// Public datasets use bounded, memory-only AppID caches. Each slot shares an
// in-flight fetch and handles expiry and transient failures like account reads.
type publicCache[K comparable, T any] struct {
	mu    sync.Mutex
	keys  []K
	slots map[K]*cacheSlot[T]
}

func (c *publicCache[K, T]) slot(key K, now func() time.Time) *cacheSlot[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i := slices.Index(c.keys, key); i >= 0 {
		copy(c.keys[i:], c.keys[i+1:])
		c.keys[len(c.keys)-1] = key
		return c.slots[key]
	}
	if c.slots == nil {
		c.slots = make(map[K]*cacheSlot[T], publicCacheSize)
	}
	if len(c.keys) == publicCacheSize {
		delete(c.slots, c.keys[0])
		c.keys = c.keys[1:]
	}
	slot := newSlot[T]("", "", now, nil)
	c.keys = append(c.keys, key)
	c.slots[key] = slot
	return slot
}

func (c steamClient) globalAchievementRates(ctx context.Context, appID uint32) (*map[string]float64, problem) {
	var reply struct {
		Percentages *struct {
			Achievements []struct {
				ID      string          `json:"name"`
				Percent json.RawMessage `json:"percent"`
			} `json:"achievements"`
		} `json:"achievementpercentages"`
	}
	query := url.Values{"gameid": {appIDText(appID)}}
	switch issue := c.get(ctx, apiBase+globalAchievementsPath, query, false, &reply); issue {
	case "":
	case problemDenied, problemBadCall, problemMissing:
		return nil, problemGlobalAchievements
	default:
		return nil, issue
	}
	if reply.Percentages == nil || reply.Percentages.Achievements == nil {
		return nil, problemGlobalAchievements
	}
	items := reply.Percentages.Achievements
	if len(items) > maxGames {
		return nil, problemResponse
	}
	rates := make(map[string]float64, len(items))
	for _, item := range items {
		// Steam has returned both JSON numbers and quoted decimal percentages.
		percent, err := strconv.ParseFloat(strings.Trim(string(item.Percent), `"`), 64)
		if err == nil && percent >= 0 && percent <= 100 && validAchievementID(item.ID) {
			rates[item.ID] = percent
		}
	}
	if len(rates) == 0 && len(items) > 0 {
		return nil, problemResponse
	}
	return &rates, ""
}

func validAchievementID(id string) bool { return id != "" && len(id) <= achievementIDBytes }

type achievementSchemaKey struct {
	appID    uint32
	language string
}

type achievementDefinition struct {
	name, description string
	hidden            bool
}

type achievementSchema struct {
	game        string
	definitions map[string]achievementDefinition
	idsByKey    map[uint32]string
	hash        uint32
}

func (a *app) hunterSchema(ctx context.Context, key achievementSchemaKey) reading[achievementSchema] {
	p := a.publicData
	return p.schemas.slot(key, p.now).read(ctx, "", profileTTL, false, func(ctx context.Context) (*achievementSchema, problem) {
		if a.config.AuthMode == modeWebAPIKey && a.setup == "" {
			if schema, _ := a.client.legacyAchievementSchema(ctx, key); schema != nil {
				return schema, ""
			}
		}
		return p.client.playerServiceAchievementSchema(ctx, key)
	})
}

func (c steamClient) legacyAchievementSchema(ctx context.Context, key achievementSchemaKey) (*achievementSchema, problem) {
	var reply struct {
		Game *struct {
			Name  string `json:"gameName"`
			Stats struct {
				Achievements []struct {
					ID          string `json:"name"`
					Name        string `json:"displayName"`
					Description string `json:"description"`
					Hidden      int    `json:"hidden"`
				} `json:"achievements"`
			} `json:"availableGameStats"`
		} `json:"game"`
	}
	query := url.Values{"appid": {appIDText(key.appID)}, "l": {key.language}}
	if issue := c.get(ctx, apiBase+keyAchievementSchema, query, true, &reply); issue != "" {
		return nil, issue
	}
	if reply.Game == nil || reply.Game.Stats.Achievements == nil {
		return nil, problemAchievementSchema
	}
	items := reply.Game.Stats.Achievements
	if len(items) > maxGames {
		return nil, problemResponse
	}
	schema := &achievementSchema{game: c.text(reply.Game.Name), definitions: make(map[string]achievementDefinition, len(items))}
	for _, item := range items {
		if validAchievementID(item.ID) {
			schema.definitions[item.ID] = achievementDefinition{c.text(item.Name), c.text(item.Description), item.Hidden != 0}
		}
	}
	return schema, ""
}

func (c steamClient) playerServiceAchievementSchema(ctx context.Context, key achievementSchemaKey) (*achievementSchema, problem) {
	var reply struct {
		Response *struct {
			SchemaHash   uint32 `json:"schema_hash"`
			Achievements []struct {
				ID          string  `json:"internal_name"`
				Key         *uint32 `json:"internal_key"`
				Name        string  `json:"localized_name"`
				Description string  `json:"localized_desc"`
				Hidden      bool    `json:"hidden"`
				Archived    bool    `json:"archived"`
			} `json:"achievements"`
		} `json:"response"`
	}
	query := url.Values{"appid": {appIDText(key.appID)}, "language": {key.language}}
	switch issue := c.get(ctx, apiBase+gameAchievementsPath, query, false, &reply); issue {
	case "":
	case problemDenied, problemBadCall, problemMissing:
		return nil, problemAchievementSchema
	default:
		return nil, issue
	}
	if reply.Response == nil || reply.Response.Achievements == nil {
		return nil, problemAchievementSchema
	}
	items := reply.Response.Achievements
	if len(items) > maxGames {
		return nil, problemResponse
	}
	schema := &achievementSchema{
		definitions: make(map[string]achievementDefinition, len(items)),
		idsByKey:    make(map[uint32]string, len(items)),
		hash:        reply.Response.SchemaHash,
	}
	for _, item := range items {
		if !item.Archived && validAchievementID(item.ID) {
			schema.definitions[item.ID] = achievementDefinition{c.text(item.Name), c.text(item.Description), item.Hidden}
			if item.Key != nil {
				schema.idsByKey[*item.Key] = item.ID
			}
		}
	}
	return schema, ""
}

type legacyAchievement struct {
	ID       string `json:"apiname"`
	Achieved *int   `json:"achieved"`
	At       int64  `json:"unlocktime"`
}

type legacyAchievements struct {
	Game         string              `json:"gameName"`
	Success      bool                `json:"success"`
	Achievements []legacyAchievement `json:"achievements"`
}

func (c steamClient) legacyPlayerAchievements(ctx context.Context, id string, appID uint32) (*legacyAchievements, problem) {
	var reply struct {
		PlayerStats *legacyAchievements `json:"playerstats"`
	}
	query := url.Values{"steamid": {id}, "appid": {appIDText(appID)}}
	switch issue := c.get(ctx, apiBase+achievementsPath, query, true, &reply); issue {
	case "":
	case problemDenied, problemBadCall, problemMissing:
		return nil, problemAchievements
	default:
		return nil, issue
	}
	if reply.PlayerStats == nil {
		return nil, problemResponse
	}
	if !reply.PlayerStats.Success {
		return nil, problemAchievements
	}
	reply.PlayerStats.Game = c.text(reply.PlayerStats.Game)
	return reply.PlayerStats, ""
}

func (ps *legacyAchievements) progress(appID uint32) *achievementProgress {
	progress := &achievementProgress{AppID: appID, Game: ps.Game, Total: len(ps.Achievements)}
	for _, item := range ps.Achievements {
		if item.Achieved != nil && *item.Achieved == 1 {
			progress.Unlocked++
		}
	}
	return progress
}

type qrUserAchievements struct {
	SchemaHash   uint32 `json:"schema_hash"`
	Achievements []struct {
		Key      *uint32 `json:"internal_key"`
		Unlocked bool    `json:"unlocked"`
		At       int64   `json:"unlock_time"`
	} `json:"achievements"`
}

func (c steamClient) userAchievements(ctx context.Context, id string, appID uint32) (*qrUserAchievements, problem) {
	var reply struct {
		Response *qrUserAchievements `json:"response"`
	}
	query := url.Values{"steamid": {id}, "appid": {appIDText(appID)}}
	switch issue := c.get(ctx, apiBase+userAchievementsPath, query, true, &reply); issue {
	case "":
	case problemDenied, problemBadCall, problemMissing:
		return nil, problemAchievementDetails
	default:
		return nil, issue
	}
	if reply.Response == nil || reply.Response.Achievements == nil {
		return nil, problemAchievementDetails
	}
	if len(reply.Response.Achievements) > maxGames {
		return nil, problemResponse
	}
	return reply.Response, ""
}

type achievementPersonalRead struct {
	account  reading[player]
	progress reading[achievementProgress]
	legacy   *legacyAchievements
	qr       reading[qrUserAchievements]
}

func (d *steamData) hunterPersonal(ctx context.Context, appID uint32, language string) (r achievementPersonalRead) {
	if d.session == nil {
		r.account = d.account(ctx, profileTTL, false)
	}
	id, issue := d.readID(r.account)
	if issue != "" {
		r.progress.Issue = issue
		return r
	}
	c, issue := d.readClient(ctx)
	if issue != "" {
		r.progress.Issue = issue
		return r
	}
	defer func() { d.rememberAchievement(id, r.progress.Data) }()
	if c.access == "" {
		r.legacy, r.progress.Issue = c.legacyPlayerAchievements(ctx, id, appID)
		if r.legacy != nil && r.legacy.Achievements != nil {
			r.progress.Data = r.legacy.progress(appID)
		}
		return r
	}
	var reads sync.WaitGroup
	reads.Go(func() {
		r.progress.Data, r.progress.Issue = c.achievementTotals(ctx, id, appID, language)
	})
	reads.Go(func() {
		r.qr.Data, r.qr.Issue = c.userAchievements(ctx, id, appID)
	})
	reads.Wait()
	return r
}

type achievementState struct {
	unlocked bool
	at       int64
}

func (r achievementPersonalRead) states(schema *achievementSchema) map[string]achievementState {
	if r.legacy != nil && r.legacy.Achievements != nil {
		items := r.legacy.Achievements
		if len(items) > maxGames {
			return nil
		}
		states := make(map[string]achievementState, len(items))
		for _, item := range items {
			if validAchievementID(item.ID) && item.Achieved != nil && (*item.Achieved == 0 || *item.Achieved == 1) {
				states[item.ID] = achievementState{*item.Achieved == 1, item.At}
			}
		}
		if len(states) != len(items) {
			return nil
		}
		return states
	}
	qr := r.qr.Data
	if qr == nil || schema == nil || (qr.SchemaHash != 0 && schema.hash != 0 && qr.SchemaHash != schema.hash) {
		return nil
	}
	states := make(map[string]achievementState, len(qr.Achievements))
	for _, item := range qr.Achievements {
		if item.Key != nil {
			if id, ok := schema.idsByKey[*item.Key]; ok {
				states[id] = achievementState{item.Unlocked, item.At}
			}
		}
	}
	if len(states) == len(schema.definitions) {
		return states
	}
	// Fill missing locked rows only when per-game totals confirm the roster
	// and every unlocked achievement has been identified.
	p := r.progress.Data
	if (len(states) == 0 && len(qr.Achievements) > 0) || p == nil || p.Total != len(schema.definitions) || p.Unlocked != unlockedCount(states) {
		return nil
	}
	for id := range schema.definitions {
		if _, ok := states[id]; !ok {
			states[id] = achievementState{}
		}
	}
	return states
}

func unlockedCount(states map[string]achievementState) (count int) {
	for _, state := range states {
		if state.unlocked {
			count++
		}
	}
	return count
}

type rankedAchievement struct {
	APIName       string   `json:"api_name,omitempty"`
	Name          string   `json:"name,omitempty"`
	Description   string   `json:"description,omitempty"`
	Hidden        *bool    `json:"hidden"`
	GlobalPercent *float64 `json:"global_percent"`
	UnlockTime    int64    `json:"unlock_time,omitempty"`
	id            string
}

func (c steamClient) rankedAchievement(id string, rate *float64, at int64, schema *achievementSchema) rankedAchievement {
	item := rankedAchievement{id: id, GlobalPercent: rate, UnlockTime: at}
	if schema != nil {
		if def, ok := schema.definitions[id]; ok {
			item.Hidden = &def.hidden
			if !def.hidden || at > 0 {
				item.APIName, item.Name, item.Description = c.text(id), def.name, def.description
			}
		}
	}
	return item
}

func compareGlobalRate(a, b rankedAchievement) int {
	switch {
	case a.GlobalPercent == nil && b.GlobalPercent != nil:
		return 1
	case b.GlobalPercent == nil && a.GlobalPercent != nil:
		return -1
	case a.GlobalPercent != nil && b.GlobalPercent != nil:
		if rate := cmp.Compare(*b.GlobalPercent, *a.GlobalPercent); rate != 0 {
			return rate
		}
	}
	return cmp.Compare(a.id, b.id)
}

func putAchievementPage(out map[string]any, args listArgs, key string, rows []rankedAchievement) {
	start := min(len(rows), args.Offset)
	end := min(len(rows), start+args.limit())
	out[key] = rows[start:end]
	if end < len(rows) {
		out["next_offset"] = end
	}
}

func (a *app) achievementHunter(parent context.Context, raw json.RawMessage) (map[string]any, error) {
	var args struct {
		listArgs
		AppID uint32 `json:"appid"`
	}
	if !decodeArgs(raw, &args) || args.AppID == 0 || !args.valid() {
		return nil, invalid("Provide a positive integer Steam AppID, an offset from 0 to 20000, and a limit from 1 to 50.")
	}
	ctx, cancel := context.WithTimeout(parent, readBudget)
	defer cancel()
	t, p := textFor(a.locale), a.publicData
	var global reading[map[string]float64]
	var schema reading[achievementSchema]
	var personal achievementPersonalRead
	var account *steamData
	var reads sync.WaitGroup
	reads.Go(func() {
		global = p.rates.slot(args.AppID, p.now).read(ctx, "", globalAchievementsTTL, false, func(ctx context.Context) (*map[string]float64, problem) {
			return p.client.globalAchievementRates(ctx, args.AppID)
		})
	})
	reads.Go(func() {
		schema = a.hunterSchema(ctx, achievementSchemaKey{args.AppID, t.storeLanguage})
	})
	reads.Go(func() {
		d, accountCtx, finish := a.readContext(ctx)
		defer finish()
		account, personal = d, d.hunterPersonal(accountCtx, args.AppID, t.storeLanguage)
	})
	reads.Wait()
	out := p.client.achievementHunterResult(args.AppID, args.listArgs, personal, schema, global, t)
	if issue := account.sessionIssue(); issue != "" {
		personal = achievementPersonalRead{progress: reading[achievementProgress]{Issue: issue}}
		out = p.client.achievementHunterResult(args.AppID, args.listArgs, personal, schema, global, t)
	}
	return out, nil
}

func (c steamClient) achievementHunterResult(appID uint32, args listArgs, personal achievementPersonalRead,
	schema reading[achievementSchema], global reading[map[string]float64], t *text) map[string]any {
	var rates map[string]float64
	if global.Data != nil {
		rates = *global.Data
	}
	states := personal.states(schema.Data)
	progress := map[string]any{"appid": appID, "unlocked": nil, "total": nil}
	if schema.Data != nil {
		progress["total"] = len(schema.Data.definitions)
		if schema.Data.game != "" {
			progress["game"] = schema.Data.game
		}
	}
	if rates != nil {
		total, _ := progress["total"].(int)
		progress["total"] = max(total, len(rates))
	}
	if personal.progress.Data != nil {
		value := personal.progress.Data
		progress["unlocked"], progress["total"] = value.Unlocked, value.Total
		if value.Game != "" {
			progress["game"] = value.Game
		}
	}
	if states != nil {
		progress["unlocked"], progress["total"] = unlockedCount(states), len(states)
	}
	out := map[string]any{
		"available": progress["total"] != nil, "personal_details": states != nil, "progress": progress,
		"limit": args.limit(), "offset": args.Offset,
	}
	if personal.account.Stale {
		describeReading(out, "profile", personal.account, t)
	}
	if states == nil {
		describeReading(out, "achievement_progress", personal.progress, t)
	}
	describeReading(out, "achievement_schema", schema, t)
	describeReading(out, "global_achievements", global, t)
	if states == nil {
		issue := personal.qr.Issue
		if issue == "" {
			issue = problemAchievementDetails
		}
		describeReading(out, "achievement_details", reading[qrUserAchievements]{Issue: issue}, t)
		c.addGlobalAchievementRanks(out, args, rates, schema.Data)
	} else {
		c.addPersonalAchievementRanks(out, args, states, rates, schema.Data, t)
	}
	return out
}

func (c steamClient) addGlobalAchievementRanks(out map[string]any, args listArgs, rates map[string]float64, schema *achievementSchema) {
	items := make([]rankedAchievement, 0, len(rates))
	for id, rate := range rates {
		items = append(items, c.rankedAchievement(id, &rate, 0, schema))
	}
	slices.SortFunc(items, compareGlobalRate)
	putAchievementPage(out, args, "global_easiest", items)
	rarest := slices.Clone(items)
	slices.Reverse(rarest)
	putAchievementPage(out, args, "global_rarest", rarest)
}

func (c steamClient) addPersonalAchievementRanks(out map[string]any, args listArgs, states map[string]achievementState,
	rates map[string]float64, schema *achievementSchema, t *text) {
	locked := make([]rankedAchievement, 0)
	timeline := make([]rankedAchievement, 0)
	missingTimes := false
	for id, state := range states {
		var rate *float64
		if value, ok := rates[id]; ok {
			rate = &value
		}
		if !state.unlocked {
			locked = append(locked, c.rankedAchievement(id, rate, 0, schema))
		} else if state.at > 0 {
			timeline = append(timeline, c.rankedAchievement(id, rate, state.at, schema))
		} else {
			missingTimes = true
		}
	}
	slices.SortFunc(locked, compareGlobalRate)
	slices.SortFunc(timeline, func(a, b rankedAchievement) int {
		if byTime := cmp.Compare(b.UnlockTime, a.UnlockTime); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.id, b.id)
	})
	putAchievementPage(out, args, "locked", locked)
	putAchievementPage(out, args, "timeline", timeline)
	if missingTimes {
		describeReading(out, "achievement_timeline", reading[achievementState]{Issue: problemAchievementUnlockTimes}, t)
	}
}

const (
	gameNewsName     = "steam_game_news"
	newsForAppPath   = "/ISteamNews/GetNewsForApp/v2/"
	officialNewsFeed = "steam_community_announcements"
	defaultNewsLimit = 10
	maxNewsLimit     = 20
	defaultNewsGames = 4
	maxNewsGames     = 5
	newsFetchCount   = 40
	newsExcerptRunes = 700
	newsTTL          = 10 * time.Minute

	problemGameNews problem = "game_news_unavailable"
)

var gameNewsTool = wire.ConnectorTool{
	Name:        gameNewsName,
	Description: "Read Steam's latest official game updates, patches and DLC announcements for one AppID or the account's recent or most-played games. Text excerpts are available when AI update summaries are enabled in Steam settings. Set official_only to false to include other posts.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{
		"appid":{"type":"integer","minimum":1,"maximum":4294967295},
		"limit":{"type":"integer","minimum":1,"maximum":20,"default":10},
		"games_limit":{"type":"integer","minimum":1,"maximum":5,"default":4,"description":"Number of games to check when AppID is omitted."},
		"official_only":{"type":"boolean","default":true}}}`),
}

type newsCacheKey struct {
	appID        uint32
	officialOnly bool
	summary      bool
}

type storeNameKey struct {
	appID             uint32
	country, language string
}

type steamNewsPost struct {
	GID      string          `json:"gid"`
	AppID    uint32          `json:"appid"`
	Title    string          `json:"title"`
	Date     int64           `json:"date"`
	FeedName string          `json:"feedname"`
	Label    string          `json:"feedlabel"`
	URL      string          `json:"url"`
	Tags     []string        `json:"tags"`
	Contents json.RawMessage `json:"contents"`
}

type gameNewsItem struct {
	AppID    uint32 `json:"appid"`
	GameName string `json:"game_name"`
	GID      string `json:"gid"`
	Title    string `json:"title"`
	Date     int64  `json:"date"`
	Label    string `json:"feed_label"`
	URL      string `json:"url"`
	Excerpt  string `json:"excerpt,omitempty"`
}

var (
	communityUpdate = regexp.MustCompile(`(?i)\bcommunity\s+update\b|社区更新`)
	updateTitle     = regexp.MustCompile(`(?i)\b(?:patch(?:es|notes)?|hotfix(?:es)?|updates?|dlcs?|expansions?|release notes|changelog)\b|\bv\d+(?:\.\d+)+\b|補丁|补丁|更新|热修|熱修|资料片|資料片|扩展包|擴展包`)
	htmlNoise       = regexp.MustCompile(`(?is)<!--.*?(?:-->|$)|<(?:script|style)\b[^>]*>.*?(?:</(?:script|style)\s*>|$)`)
	htmlMarkup      = regexp.MustCompile(`(?is)</?[a-z][a-z0-9:_-]*(?:[^<>"']|"[^"]*"|'[^']*')*>`)
	steamMedia      = regexp.MustCompile(`(?is)\[(?:img|previewyoutube|video|youtube)(?:=[^\]]*)?\].*?(?:\[/(?:img|previewyoutube|video|youtube)\]|$)`)
	steamFormatting = regexp.MustCompile(`(?i)\[(?:/?(?:h[1-6]|b|i|u|s|spoiler|noparse|url|list|olist|quote|code|table|tr|th|td|hr|br|strike|p)(?:[= ][^\]]*)?|\*)\]`)
)

func (p steamNewsPost) isUpdate(title string) bool {
	for _, tag := range p.Tags {
		switch strings.ToLower(tag) {
		case "patchnotes", "patch_notes", "hotfix", "majorupdate", "major_update", "regular_update", "update", "dlc", "expansion":
			return true
		}
	}
	return updateTitle.MatchString(communityUpdate.ReplaceAllString(title, ""))
}

func (c steamClient) newsExcerpt(body string) string {
	body = html.UnescapeString(body)
	body = htmlNoise.ReplaceAllString(body, " ")
	body = htmlMarkup.ReplaceAllString(body, " ")
	body = steamMedia.ReplaceAllString(body, " ")
	body = steamFormatting.ReplaceAllString(body, " ")
	return cleanText(strings.Join(strings.Fields(redactCredentials(body, c.key, c.access, c.refresh)), " "), newsExcerptRunes)
}

func (c steamClient) newsURL(raw string) string {
	if len(raw) > 512 || redactCredentials(raw, c.key, c.access, c.refresh) != raw {
		return ""
	}
	link, err := url.Parse(raw)
	if err != nil || link.Scheme != "https" || link.Hostname() == "" || link.User != nil {
		return ""
	}
	return link.String()
}

func (c steamClient) fetchGameNews(ctx context.Context, key newsCacheKey) (*[]gameNewsItem, problem) {
	query := url.Values{"appid": {appIDText(key.appID)}, "count": {strconv.Itoa(newsFetchCount)}, "maxlength": {"1"}}
	if key.officialOnly {
		query.Set("feeds", officialNewsFeed)
	}
	if key.summary {
		query.Set("maxlength", "2000")
	}
	var reply struct {
		AppNews *struct {
			AppID uint32          `json:"appid"`
			Posts []steamNewsPost `json:"newsitems"`
		} `json:"appnews"`
	}
	switch issue := c.get(ctx, apiBase+newsForAppPath, query, false, &reply); issue {
	case "":
	case problemBadCall, problemDenied, problemMissing:
		return nil, problemGameNews
	default:
		return nil, issue
	}
	if reply.AppNews == nil || reply.AppNews.AppID != key.appID || reply.AppNews.Posts == nil {
		return nil, problemResponse
	}
	posts := reply.AppNews.Posts[:min(len(reply.AppNews.Posts), newsFetchCount)]
	items := make([]gameNewsItem, 0, len(posts))
	for _, post := range posts {
		gid, err := strconv.ParseUint(post.GID, 10, 64)
		title := c.text(html.UnescapeString(post.Title))
		if err != nil || gid == 0 || post.Date <= 0 || title == "" || (post.AppID != 0 && post.AppID != key.appID) {
			continue
		}
		official := post.FeedName == officialNewsFeed
		if key.officialOnly && (!official || !post.isUpdate(title)) {
			continue
		}
		item := gameNewsItem{
			AppID: key.appID, GID: strconv.FormatUint(gid, 10), Title: title, Date: post.Date, Label: c.text(post.Label),
		}
		if official {
			item.URL = storeBase + "/news/app/" + appIDText(key.appID) + "/view/" + item.GID
		} else {
			item.URL = c.newsURL(post.URL)
		}
		// Steam sends full content for maxlength=0. Even when it returns more
		// than requested, only summary mode decodes and stores a text excerpt.
		if key.summary {
			var body string
			if json.Unmarshal(post.Contents, &body) == nil {
				item.Excerpt = c.newsExcerpt(body)
			}
		}
		items = append(items, item)
	}
	return &items, ""
}

type gameNewsOptions struct {
	limit, gamesLimit int
	officialOnly      bool
	summary           bool
}

type gameNewsResult struct {
	Available      bool           `json:"available"`
	SummaryEnabled bool           `json:"summary_enabled"`
	Updates        []gameNewsItem `json:"updates"`
	Warnings       []toolWarning  `json:"warnings,omitempty"`
}

func newGameNewsResult(summary bool) gameNewsResult {
	return gameNewsResult{SummaryEnabled: summary, Updates: []gameNewsItem{}}
}

func (r *gameNewsResult) warn(t *text, dataset string, issue problem, stale bool) {
	if issue != "" {
		r.Warnings = append(r.Warnings, toolWarning{dataset, issue, t.message(issue, stale), stale})
	}
}

func (a *app) gameNews(parent context.Context, raw json.RawMessage) (gameNewsResult, error) {
	var args struct {
		AppID        *uint32 `json:"appid"`
		Limit        *int    `json:"limit"`
		GamesLimit   *int    `json:"games_limit"`
		OfficialOnly *bool   `json:"official_only"`
	}
	if !decodeArgs(raw, &args) || (args.AppID != nil && *args.AppID == 0) {
		return gameNewsResult{}, invalid("Provide a positive integer Steam AppID or omit it to read updates for played games.")
	}
	options := gameNewsOptions{
		limit: defaultNewsLimit, gamesLimit: defaultNewsGames, officialOnly: args.OfficialOnly == nil || *args.OfficialOnly,
		summary: a.config.TokenConfig.AllowsPatchNotesSummary(),
	}
	if args.Limit != nil {
		options.limit = *args.Limit
	}
	if args.GamesLimit != nil {
		options.gamesLimit = *args.GamesLimit
	}
	if options.limit < 1 || options.limit > maxNewsLimit || options.gamesLimit < 1 || options.gamesLimit > maxNewsGames {
		return gameNewsResult{}, invalid("Use a news limit from 1 to 20 and a games_limit from 1 to 5.")
	}
	t := textFor(a.locale)
	if args.AppID != nil {
		ctx, cancel := context.WithTimeout(parent, readBudget)
		defer cancel()
		result := newGameNewsResult(options.summary)
		a.publicData.collectGameNews(ctx, []game{{AppID: *args.AppID}}, options, a.config.CountryCode, t, &result)
		return result, nil
	}
	d, ctx, cancel := a.readContext(parent)
	defer cancel()
	result := a.accountGameNews(ctx, d, t, options)
	if issue := d.sessionIssue(); issue != "" {
		result = newGameNewsResult(options.summary)
		result.warn(t, "game_news", issue, false)
	}
	return result, nil
}

func (a *app) accountGameNews(ctx context.Context, d *steamData, t *text, options gameNewsOptions) gameNewsResult {
	result := newGameNewsResult(options.summary)
	var account reading[player]
	if d.session == nil {
		account = d.account(ctx, profileTTL, false)
		if account.Stale {
			result.warn(t, "profile", account.Issue, true)
		}
	}
	if _, issue := d.readID(account); issue != "" {
		result.warn(t, "game_news", issue, false)
		return result
	}
	recent := d.recentGames(ctx, account, false)
	result.warn(t, "recent_games", recent.Issue, recent.Stale)
	var games []game
	if recent.Data != nil {
		games = recent.Data.Games[:min(len(recent.Data.Games), options.gamesLimit)]
	}
	if len(games) == 0 {
		library := d.ownedGames(ctx, account, false)
		result.warn(t, "library", library.Issue, library.Stale)
		if library.Data != nil {
			for _, g := range libraryGames(library.Data.Games, "", sortPlaytime) {
				if len(games) == options.gamesLimit || g.PlaytimeForever == 0 {
					break
				}
				games = append(games, g)
			}
		}
		if len(games) == 0 {
			result.Available = recent.Data != nil || library.Data != nil
			return result
		}
	}
	a.publicData.collectGameNews(ctx, games, options, d.config.CountryCode, t, &result)
	return result
}

func (p *steamPublicData) collectGameNews(ctx context.Context, games []game, options gameNewsOptions,
	country string, t *text, result *gameNewsResult) {
	replies := make([]reading[[]gameNewsItem], len(games))
	names := make([]string, len(games))
	var reads sync.WaitGroup
	for i, g := range games {
		reads.Go(func() {
			key := newsCacheKey{g.AppID, options.officialOnly, options.summary}
			reply := p.news.slot(key, p.now).read(ctx, "", newsTTL, false, func(ctx context.Context) (*[]gameNewsItem, problem) {
				return p.client.fetchGameNews(ctx, key)
			})
			replies[i] = reply
			if reply.Data != nil && len(*reply.Data) > 0 {
				names[i] = p.newsGameName(ctx, g, country, t)
			}
		})
	}
	reads.Wait()
	for i, reply := range replies {
		result.warn(t, "game_news/"+appIDText(games[i].AppID), reply.Issue, reply.Stale)
		if reply.Data == nil {
			continue
		}
		result.Available = true
		for _, item := range *reply.Data {
			item.GameName = names[i]
			if !result.SummaryEnabled {
				item.Excerpt = ""
			}
			result.Updates = append(result.Updates, item)
		}
	}
	slices.SortStableFunc(result.Updates, func(a, b gameNewsItem) int { return cmp.Compare(b.Date, a.Date) })
	seen := make(map[string]bool, len(result.Updates))
	result.Updates = slices.DeleteFunc(result.Updates, func(item gameNewsItem) bool {
		duplicate := seen[item.GID]
		seen[item.GID] = true
		return duplicate
	})
	result.Updates = result.Updates[:min(len(result.Updates), options.limit)]
}

func (p *steamPublicData) newsGameName(ctx context.Context, g game, country string, t *text) string {
	if g.Name != "" {
		return g.Name
	}
	key := storeNameKey{g.AppID, country, t.storeLanguage}
	r := p.names.slot(key, p.now).read(ctx, "", profileTTL, false, func(ctx context.Context) (*string, problem) {
		name := p.client.storeNames(ctx, []uint32{g.AppID}, country, t.storeLanguage)[g.AppID]
		if name == "" {
			return nil, problemResponse
		}
		return &name, ""
	})
	if r.Data != nil {
		return *r.Data
	}
	return t.gameName(g.AppID, "")
}
