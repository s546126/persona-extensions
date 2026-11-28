package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	discoverySkillID  = "steam-game-discovery"
	discoverySkillDir = "assistant/steam-discovery-skills"
)

//go:embed skills/steam-game-discovery/SKILL.md
var discoverySkill []byte

// The Hub reads this data contribution when rendering skills. Publish it only
// while both the contribution switch and the discovery token gate are on.
func syncDiscoverySkill(dataDir string, enabled bool) error {
	if dataDir == "" {
		return nil
	}
	dir := filepath.Join(dataDir, discoverySkillDir, discoverySkillID)
	path := filepath.Join(dir, "SKILL.md")
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() == int64(len(discoverySkill)) {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, discoverySkill) {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".skill-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(discoverySkill)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

const (
	storeTagsTool   = "steam_store_tags"
	storeSearchTool = "steam_store_search"
	gameDetailsTool = "steam_game_details"

	tagCatalogPath  = "/tagdata/populartags/"
	storeSearchPath = "/search/results/"
	appReviewsPath  = "/appreviews/"
	reviewsAPIPath  = "/IUserReviewsService/GetAppReviews/v1/"

	discoveryBudget      = 8 * time.Second
	searchTimeout        = 3 * time.Second
	accountLookupTimeout = 3 * time.Second
	tagCatalogTimeout    = 1200 * time.Millisecond
	legacyReviewTimeout  = time.Second
	gameLookupTimeout    = 2500 * time.Millisecond
	searchTTL            = 5 * time.Minute
	searchMaxStaleAge    = 30 * time.Minute
	tagCatalogTTL        = 8 * time.Hour
	discoveryCacheSize   = 64
	detailBatchLimit     = 10
	detailConcurrency    = 4
	searchTagLimit       = 8
	detailTagLimit       = 8
	tagNameRunes         = 60
	tagCatalogBytes      = 128 << 10
	searchResponseBytes  = 512 << 10
	gameResponseBytes    = 512 << 10
	reviewResponseBytes  = 128 << 10
)

const (
	problemDiscoveryDisabled problem = "discovery_disabled"
	problemDiscoveryTagArgs  problem = "invalid_store_tags_args"
	problemDiscoverySearch   problem = "invalid_store_search_args"
	problemDiscoveryDetails  problem = "invalid_game_details_args"
	problemUnknownStoreTag   problem = "unknown_store_tag"
	problemBuiltInTags       problem = "built_in_tags"
	problemStoreSearchFailed problem = "store_search_unavailable"
	problemPartialLibrary    problem = "partial_library"
)

func discoveryTools(limit int) []wire.ConnectorTool {
	const appID = `{"type":"integer","minimum":1,"maximum":4294967295}`
	const language = `"language":{"type":"string","description":"Steam language code such as english or schinese. Defaults to the Hub locale."}`
	resultLimit := fmt.Sprintf(`"limit":{"type":"integer","minimum":1,"maximum":%d,"default":%d}`, limit, limit)
	return []wire.ConnectorTool{
		{
			Name:        storeTagsTool,
			Description: "Find localized Steam Store tags and their numeric IDs. Use a short query for each gameplay or mood term.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Tag name or ID, up to 100 characters."},` + language + "," + resultLimit + `}}`),
		},
		{
			Name:        storeSearchTool,
			Description: "Search Steam Store games by title or combined tags, budget, sale, platforms and language. Check warnings and account flags before recommending games.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{
				"query":{"type":"string","description":"Game title or keywords, up to 100 characters."},
				"tags":{"type":"array","items":{"type":["integer","string"]},"description":"Up to eight Steam tag IDs or exact localized or English names. All tags are combined."},
				"max_price":{"type":"number","minimum":0,"description":"Maximum current price in the configured Store currency (e.g. 19.99)."},
				"discounted_only":{"type":"boolean","default":false},
				"platforms":{"type":"array","items":{"type":"string","enum":["windows","macos","linux","steam_deck"]},"description":"Require all listed platforms. Steam Deck accepts Verified or Playable."},
				"language":{"type":"string","description":"Steam language code such as english or schinese. When provided, require game support and localize Store results."},
				"sort":{"type":"string","enum":["relevance","reviews","price_asc","release_date"],"default":"relevance"},
				"exclude_owned":{"type":"boolean","default":false,"description":"Exclude titles confirmed in the current library. Account flags are null when unknown."},
				` + resultLimit + `}}`),
		},
		{
			Name:        gameDetailsTool,
			Description: "Read localized descriptions, tags and features, Steam Deck status, regional prices, and overall and rolling 30-day review statistics for a shortlist of games.",
			InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{
				"appid":%s,"appids":{"type":"array","description":"Provide appid or a list of 1 to %d appids.", "items":%s},%s}}`, appID, min(limit, detailBatchLimit), appID, language)),
		},
	}
}

type steamDiscovery struct {
	client steamClient
	now    func() time.Time
	tags   discoveryCache[[]discoveryTag]
	search discoveryCache[discoveryPage]
}

func newSteamDiscovery(client *http.Client, now func() time.Time) *steamDiscovery {
	return &steamDiscovery{client: steamClient{http: client}, now: now}
}

// Reuse the existing request coalescing, retry delay and bounded stale reads.
// Public results live in memory and never include account membership.
type discoveryCache[T any] struct {
	mu    sync.Mutex
	slots map[string]*cacheSlot[T]
	order []string
}

func (c *discoveryCache[T]) slot(key string, now func() time.Time) *cacheSlot[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.slots[key]; s != nil {
		return s
	}
	if c.slots == nil {
		c.slots = make(map[string]*cacheSlot[T])
	}
	if len(c.order) == discoveryCacheSize {
		delete(c.slots, c.order[0])
		c.order = c.order[1:]
	}
	s := newSlot[T]("", "", now, nil)
	c.slots[key] = s
	c.order = append(c.order, key)
	return s
}

func (d *steamDiscovery) get(ctx context.Context, budget time.Duration, maxBytes int64, target string, params url.Values, out any) problem {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return d.client.readBoundedJSON(ctx, target, params, nil, false, maxBytes, out)
}

type discoveryOptions struct {
	Language string `json:"language"`
	Limit    *int   `json:"limit"`
}

func (o discoveryOptions) resultLimit(configured int) (int, bool) {
	if o.Limit == nil {
		return configured, true
	}
	return min(*o.Limit, configured), *o.Limit > 0 && *o.Limit <= maxToolLimit
}

// Steam's Store endpoints use API language names. Accept the common Hub locale aliases too.
const discoveryLanguages = " arabic brazilian bulgarian czech danish dutch english finnish french german greek hungarian indonesian italian japanese koreana latam malay norwegian polish portuguese romanian russian schinese spanish swedish tchinese thai turkish ukrainian vietnamese "

var discoveryLocaleAliases = map[string]string{
	"ar": "arabic", "bg": "bulgarian", "cs": "czech", "da": "danish", "de": "german", "el": "greek",
	"en": "english", "es": "spanish", "es-419": "latam", "fi": "finnish", "fr": "french", "hu": "hungarian",
	"id": "indonesian", "it": "italian", "ja": "japanese", "ko": "koreana", "ms": "malay", "nl": "dutch",
	"no": "norwegian", "pl": "polish", "pt": "portuguese", "pt-br": "brazilian", "ro": "romanian",
	"ru": "russian", "sv": "swedish", "th": "thai", "tr": "turkish", "uk": "ukrainian", "vi": "vietnamese",
	"zh": "schinese", "zh-hans": "schinese", "zh-hans-cn": "schinese", "zh-cn": "schinese",
	"zh-hant": "tchinese", "zh-hant-tw": "tchinese", "zh-tw": "tchinese", "zh-hk": "tchinese",
}

func discoveryLanguage(input, fallback string) (string, bool) {
	language := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(input), "_", "-"))
	if language == "" {
		return fallback, true
	}
	if alias := discoveryLocaleAliases[language]; alias != "" {
		return alias, true
	}
	if base, _, ok := strings.Cut(language, "-"); ok && discoveryLocaleAliases[base] != "" {
		return discoveryLocaleAliases[base], true
	}
	return language, !strings.Contains(language, " ") && strings.Contains(discoveryLanguages, " "+language+" ")
}

func (a *app) callDiscoveryTool(ctx context.Context, name string, raw json.RawMessage) (wire.ConnectorCallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryBudget)
	defer cancel()
	t := textFor(a.locale)
	fallbackLanguage, ok := discoveryLanguage(a.locale, t.storeLanguage)
	if !ok {
		fallbackLanguage = t.storeLanguage
	}
	country := a.config.CountryCode
	if name != storeTagsTool && (len(country) != 2 || !strings.Contains(isoCountries, " "+country+" ")) {
		return wire.ConnectorCallResult{}, invalid(t.message(problemCountry, false))
	}
	limit := a.config.DiscoveryResultLimit()
	out := map[string]any{"available": false}
	var err error
	switch name {
	case storeTagsTool:
		err = a.discovery.listTags(ctx, raw, fallbackLanguage, limit, t, out)
	case storeSearchTool:
		err = a.searchStore(ctx, raw, country, fallbackLanguage, limit, t, out)
	case gameDetailsTool:
		var memory *steamData
		if a.config.exportsMemoryDimension(memoryPlaystyle) {
			memory = a.memoryData()
		}
		err = a.discovery.gameDetails(ctx, raw, country, fallbackLanguage, min(limit, detailBatchLimit), t, out)
		if games, ok := out["games"].([]discoveryGame); err == nil && ok && memory != nil {
			memory.rememberGenres(games)
		}
	}
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	return encodeSteamTool(out, nil)
}

type discoveryTag struct {
	ID   uint32 `json:"tagid"`
	Name string `json:"name,omitempty"`
}

// A small set of common Store tags for cold starts during catalog outages.
var commonStoreTags = []struct {
	id               uint32
	english, chinese string
}{
	{492, "Indie", "独立"}, {19, "Action", "动作"}, {21, "Adventure", "冒险"},
	{597, "Casual", "休闲"}, {599, "Simulation", "模拟"}, {9, "Strategy", "策略"},
	{122, "RPG", "角色扮演"}, {4182, "Singleplayer", "单人"}, {3859, "Multiplayer", "多人"},
	{1685, "Co-op", "合作"}, {3843, "Online Co-Op", "在线合作"}, {3841, "Local Co-Op", "本地合作"},
	{1742, "Story Rich", "剧情丰富"}, {1664, "Puzzle", "解谜"}, {1654, "Relaxing", "放松"},
	{97376, "Cozy", "温馨惬意"}, {1716, "Roguelike", "类 Rogue"}, {3959, "Roguelite", "轻度 Rogue"},
	{32322, "Deckbuilding", "牌组构建"}, {1695, "Open World", "开放世界"}, {3834, "Exploration", "探索"},
	{1662, "Survival", "生存"}, {3810, "Sandbox", "沙盒"}, {1734, "Fast-Paced", "快节奏"},
	{4234, "Short", "短篇"}, {7481, "Controller", "控制器"}, {87918, "Farming Sim", "农场模拟"},
}

func tagNameKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func builtInTagLanguage(language string) string {
	if language == "schinese" {
		return language
	}
	return "english"
}

func builtInTags(language string) []discoveryTag {
	tags := make([]discoveryTag, 0, len(commonStoreTags))
	for _, tag := range commonStoreTags {
		name := tag.english
		if language == "schinese" {
			name = tag.chinese
		}
		tags = append(tags, discoveryTag{tag.id, name})
	}
	return tags
}

func (d *steamDiscovery) catalog(ctx context.Context, language string) reading[[]discoveryTag] {
	return d.tags.slot(language, d.now).read(ctx, language, tagCatalogTTL, false, func(ctx context.Context) (*[]discoveryTag, problem) {
		var reply []discoveryTag
		if p := d.get(ctx, tagCatalogTimeout, tagCatalogBytes, storeBase+tagCatalogPath+language, nil, &reply); p != "" {
			return nil, p
		}
		tags := make([]discoveryTag, 0, len(reply))
		seen := make(map[uint32]struct{}, len(reply))
		for _, tag := range reply {
			tag.Name = cleanText(redactCredentials(tag.Name), tagNameRunes)
			if _, ok := seen[tag.ID]; tag.ID == 0 || tag.Name == "" || ok {
				continue
			}
			seen[tag.ID] = struct{}{}
			tags = append(tags, tag)
		}
		if len(tags) == 0 {
			return nil, problemResponse
		}
		return &tags, ""
	})
}

func (d *steamDiscovery) listTags(ctx context.Context, raw json.RawMessage, fallback string, limit int, t *text, out map[string]any) error {
	var args struct {
		discoveryOptions
		Query string `json:"query"`
	}
	if !decodeArgs(raw, &args) || utf8.RuneCountInString(args.Query) > maxQueryRunes {
		return invalid(t.message(problemDiscoveryTagArgs, false))
	}
	language, validLanguage := discoveryLanguage(args.Language, fallback)
	limit, validLimit := args.resultLimit(limit)
	if !validLanguage || !validLimit {
		return invalid(t.message(problemDiscoveryTagArgs, false))
	}
	tags := d.catalog(ctx, language)
	describeReading(out, "tags", tags, t)
	source, actualLanguage := "steam_store", language
	catalog := []discoveryTag(nil)
	if tags.Data != nil {
		catalog = *tags.Data
	} else {
		source, actualLanguage = "built_in", builtInTagLanguage(language)
		catalog = builtInTags(actualLanguage)
		describeReading(out, "tags", reading[[]discoveryTag]{Issue: problemBuiltInTags}, t)
	}
	query := tagNameKey(args.Query)
	matches := make([]discoveryTag, 0, limit)
	total := 0
	for _, tag := range catalog {
		if strings.Contains(tagNameKey(tag.Name), query) || appIDText(tag.ID) == query {
			total++
			if len(matches) < limit {
				matches = append(matches, tag)
			}
		}
	}
	out["available"], out["tags"], out["source"] = true, matches, source
	out["language"], out["matched_count"] = actualLanguage, total
	return nil
}

type discoverySearchArgs struct {
	discoveryOptions
	Query          string            `json:"query"`
	Tags           []json.RawMessage `json:"tags"`
	MaxPrice       *float64          `json:"max_price"`
	DiscountedOnly bool              `json:"discounted_only"`
	Platforms      []string          `json:"platforms"`
	Sort           string            `json:"sort"`
	ExcludeOwned   bool              `json:"exclude_owned"`
}

var discoverySorts = map[string]string{
	"": "", "relevance": "", "reviews": "Reviews_DESC", "price_asc": "Price_ASC", "release_date": "Released_DESC",
}

func (a *app) searchStore(ctx context.Context, raw json.RawMessage, country, fallback string, limit int, t *text, out map[string]any) error {
	var args discoverySearchArgs
	if !decodeArgs(raw, &args) || utf8.RuneCountInString(args.Query) > maxQueryRunes ||
		len(args.Tags) > searchTagLimit || len(args.Platforms) > 4 {
		return invalid(t.message(problemDiscoverySearch, false))
	}
	language, validLanguage := discoveryLanguage(args.Language, fallback)
	limit, validLimit := args.resultLimit(limit)
	sort, validSort := discoverySorts[args.Sort]
	if !validLanguage || !validLimit || !validSort {
		return invalid(t.message(problemDiscoverySearch, false))
	}
	query := url.Values{
		"query": {""}, "infinite": {"1"}, "start": {"0"}, "count": {strconv.Itoa(limit)},
		"category1": {"998"}, "cc": {country}, "l": {language},
	}
	if term := strings.TrimSpace(args.Query); term != "" {
		query.Set("term", term)
	}
	if strings.TrimSpace(args.Language) != "" {
		query.Set("supportedlang", language)
	}
	if sort != "" {
		query.Set("sort_by", sort)
	}
	var maxPrice uint32
	if args.MaxPrice != nil {
		scaled := *args.MaxPrice * 100
		rounded := math.Round(scaled)
		if scaled < 0 || rounded > math.MaxUint32 || math.Abs(scaled-rounded) > 0.000001 {
			return invalid(t.message(problemDiscoverySearch, false))
		}
		maxPrice = uint32(rounded)
		if maxPrice == 0 {
			query.Set("maxprice", "free")
		} else {
			query.Set("maxprice", strconv.FormatFloat(*args.MaxPrice, 'f', -1, 64))
		}
	}
	if args.DiscountedOnly {
		query.Set("specials", "1")
	}
	for _, platform := range args.Platforms {
		switch platform {
		case "windows", "macos", "linux":
		case "steam_deck":
			query.Set("deck_compatibility", "2,3")
		default:
			return invalid(t.message(problemDiscoverySearch, false))
		}
	}
	// Steam combines native OS values as alternatives. Ask for one required OS on
	// the server and verify all selected desktop platforms from the returned rows.
	for _, p := range []struct{ name, code string }{{"macos", "mac"}, {"linux", "linux"}, {"windows", "win"}} {
		if slices.Contains(args.Platforms, p.name) {
			query.Set("os", p.code)
			break
		}
	}
	tagIDs, err := a.discovery.resolveTags(ctx, args.Tags, language, t, out)
	if err != nil {
		return err
	}
	if len(tagIDs) != 0 {
		query.Set("tags", strings.Join(tagIDs, ","))
	}
	page := a.discovery.searchPage(ctx, query, limit)
	if page.Stale && a.discovery.now().Sub(page.At) > searchMaxStaleAge {
		page.Data, page.Stale = nil, false
	}
	describeReading(out, "search", page, t)
	games := make([]discoveryCandidate, 0, limit)
	out["games"], out["country_code"], out["language"], out["limit"] = games, country, language, limit
	if page.Data == nil {
		describeReading(out, "search", reading[discoveryPage]{Issue: problemStoreSearchFailed}, t)
		return nil
	}
	for _, game := range page.Data.Games {
		if args.MaxPrice != nil && (game.Price == nil || game.Price.Final > maxPrice) ||
			args.DiscountedOnly && (game.Price == nil || game.Price.DiscountPercent == 0) ||
			!game.supports(args.Platforms) {
			continue
		}
		games = append(games, game)
	}
	if len(games) > 0 {
		games = a.annotateCandidates(ctx, games, args.ExcludeOwned, t, out)
	}
	out["available"], out["games"], out["truncated"], out["as_of"] = true, games, page.Data.Truncated, page.At
	return nil
}

func (d *steamDiscovery) resolveTags(ctx context.Context, input []json.RawMessage, language string, t *text, out map[string]any) ([]string, error) {
	var ids []uint32
	var names []string
	for _, raw := range input {
		var id uint32
		if json.Unmarshal(raw, &id) == nil && id != 0 {
			ids = append(ids, id)
			continue
		}
		var name string
		if json.Unmarshal(raw, &name) != nil || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > tagNameRunes {
			return nil, invalid(t.message(problemDiscoverySearch, false))
		}
		name = tagNameKey(name)
		if digits.MatchString(name) {
			n, err := strconv.ParseUint(name, 10, 32)
			if err != nil || n == 0 {
				return nil, invalid(t.message(problemDiscoverySearch, false))
			}
			ids = append(ids, uint32(n))
		} else {
			names = append(names, name)
		}
	}
	if len(names) != 0 {
		local := d.catalog(ctx, language)
		describeReading(out, "tags", local, t)
		if local.Data == nil {
			describeReading(out, "tags", reading[[]discoveryTag]{Issue: problemBuiltInTags}, t)
		}
		byName := make(map[string]uint32)
		add := func(tags []discoveryTag) {
			for _, tag := range tags {
				key := tagNameKey(tag.Name)
				if byName[key] == 0 {
					byName[key] = tag.ID
				}
			}
		}
		if local.Data != nil {
			add(*local.Data)
		}
		add(builtInTags("schinese"))
		add(builtInTags("english"))
		for _, name := range names {
			if byName[name] == 0 && language != "english" {
				english := d.catalog(ctx, "english")
				if english.Data != nil {
					add(*english.Data)
				}
			}
			if byName[name] == 0 {
				return nil, invalid(t.message(problemUnknownStoreTag, false))
			}
			ids = append(ids, byName[name])
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	resolved := make([]string, 0, len(ids))
	for _, id := range ids {
		resolved = append(resolved, appIDText(id))
	}
	return resolved, nil
}

type discoveryPage struct {
	Games     []discoveryCandidate
	Truncated bool
}

type discoveryCandidate struct {
	AppID      uint32                `json:"appid"`
	Name       string                `json:"name"`
	URL        string                `json:"url"`
	Platforms  []string              `json:"platforms,omitempty"`
	Price      *discoverySearchPrice `json:"price,omitempty"`
	Owned      *bool                 `json:"owned"`
	InWishlist *bool                 `json:"in_wishlist"`
}

type discoverySearchPrice struct {
	Final           uint32 `json:"final"`
	FinalFormatted  string `json:"final_formatted,omitempty"`
	DiscountPercent int    `json:"discount_percent"`
}

func (g discoveryCandidate) supports(platforms []string) bool {
	for _, platform := range platforms {
		if !slices.Contains(g.Platforms, platform) {
			return false
		}
	}
	return true
}

func (d *steamDiscovery) searchPage(ctx context.Context, query url.Values, limit int) reading[discoveryPage] {
	// The Store frontend's search honors combined filters and sorts. The
	// /api/storesearch title lookup does not offer that contract.
	key := query.Encode()
	return d.search.slot(key, d.now).read(ctx, key, searchTTL, false, func(ctx context.Context) (*discoveryPage, problem) {
		var reply struct {
			Success int     `json:"success"`
			Total   *int    `json:"total_count"`
			HTML    *string `json:"results_html"`
		}
		if p := d.get(ctx, searchTimeout, searchResponseBytes, storeBase+storeSearchPath, query, &reply); p != "" {
			return nil, p
		}
		if reply.Success != 1 || reply.Total == nil || *reply.Total < 0 || reply.HTML == nil {
			return nil, problemResponse
		}
		document, err := html.Parse(strings.NewReader(*reply.HTML))
		if err != nil {
			return nil, problemResponse
		}
		page := &discoveryPage{Truncated: *reply.Total > limit, Games: make([]discoveryCandidate, 0, limit)}
		seen := make(map[uint32]struct{}, limit)
		deckFilter := query.Has("deck_compatibility")
		for node := range document.Descendants() {
			if node.DataAtom != atom.A || !hasStoreClass(node, "search_result_row") {
				continue
			}
			id, err := strconv.ParseUint(storeAttr(node, "data-ds-appid"), 10, 32)
			if err != nil || id == 0 || storeAttr(node, "data-ds-itemkey") != "App_"+appIDText(uint32(id)) {
				continue
			}
			if _, ok := seen[uint32(id)]; ok {
				continue
			}
			if len(page.Games) == limit {
				page.Truncated = true
				break
			}
			if game := parseSearchRow(node, uint32(id), deckFilter); game.Name != "" {
				seen[game.AppID] = struct{}{}
				page.Games = append(page.Games, game)
			}
		}
		return page, ""
	})
}

func parseSearchRow(row *html.Node, id uint32, deckFilter bool) discoveryCandidate {
	game := discoveryCandidate{AppID: id, URL: storeLink(id)}
	var price discoverySearchPrice
	var priceKnown bool
	for node := range row.Descendants() {
		if node.Type != html.ElementNode {
			continue
		}
		switch {
		case hasStoreClass(node, "title"):
			game.Name = cleanText(redactCredentials(storeNodeText(node)), nameRunes)
		case hasStoreClass(node, "platform_img"):
			for _, platform := range []struct{ class, name string }{{"win", "windows"}, {"mac", "macos"}, {"linux", "linux"}} {
				if hasStoreClass(node, platform.class) && !slices.Contains(game.Platforms, platform.name) {
					game.Platforms = append(game.Platforms, platform.name)
				}
			}
		case hasStoreClass(node, "search_price_discount_combined"):
			if value, err := strconv.ParseUint(storeAttr(node, "data-price-final"), 10, 32); err == nil {
				priceKnown, price.Final = true, uint32(value)
			}
		case hasStoreClass(node, "search_discount_block"):
			if percent, err := strconv.Atoi(storeAttr(node, "data-discount")); err == nil && percent >= 0 && percent <= 100 {
				price.DiscountPercent = percent
			}
		case hasStoreClass(node, "discount_final_price"):
			price.FinalFormatted = cleanText(redactCredentials(storeNodeText(node)), 40)
		}
	}
	// Steam also sends a zero cents attribute for rows without a price quote.
	if priceKnown && (price.Final != 0 || price.FinalFormatted != "") {
		game.Price = &price
	}
	if deckFilter {
		game.Platforms = append(game.Platforms, "steam_deck")
	}
	return game
}

func storeAttr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func hasStoreClass(node *html.Node, class string) bool {
	for field := range strings.FieldsSeq(storeAttr(node, "class")) {
		if field == class {
			return true
		}
	}
	return false
}

func storeNodeText(node *html.Node) string {
	var text strings.Builder
	for child := range node.Descendants() {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
		}
	}
	return strings.Join(strings.Fields(text.String()), " ")
}

func (a *app) annotateCandidates(ctx context.Context, games []discoveryCandidate, excludeOwned bool, t *text, out map[string]any) []discoveryCandidate {
	ctx, stop := context.WithTimeout(ctx, accountLookupTimeout)
	defer stop()
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	var p reading[player]
	if d.session == nil {
		p = d.account(ctx, profileTTL, false)
	}
	if p.Stale {
		describeReading(out, "account", reading[player]{Issue: p.Issue}, t)
		return games
	}
	id, issue := d.readID(p)
	if issue != "" {
		describeReading(out, "account", reading[player]{Issue: issue}, t)
		return games
	}
	var library reading[gameList]
	var wishes reading[map[uint32]struct{}]
	var reads sync.WaitGroup
	reads.Go(func() { library = d.ownedGames(ctx, p, false) })
	reads.Go(func() { wishes = d.discoveryWishIDs(ctx, id) })
	reads.Wait()
	if issue := d.sessionIssue(); issue != "" {
		describeReading(out, "account", reading[player]{Issue: issue}, t)
		return games
	}
	describeReading(out, "library", reading[gameList]{Issue: library.Issue}, t)
	describeReading(out, "wishlist", reading[map[uint32]struct{}]{Issue: wishes.Issue}, t)
	var owned map[uint32]struct{}
	complete := false
	if library.Data != nil && !library.Stale {
		owned = make(map[uint32]struct{}, len(library.Data.Games))
		for _, game := range library.Data.Games {
			owned[game.AppID] = struct{}{}
		}
		complete = !library.Data.Truncated
		if !complete {
			describeReading(out, "library", reading[gameList]{Issue: problemPartialLibrary}, t)
		}
	}
	filtered := games[:0]
	for _, game := range games {
		_, isOwned := owned[game.AppID]
		if isOwned && excludeOwned {
			continue
		}
		if isOwned || complete {
			game.Owned = &isOwned
		}
		if wishes.Data != nil && !wishes.Stale {
			_, inWishlist := (*wishes.Data)[game.AppID]
			game.InWishlist = &inWishlist
		}
		filtered = append(filtered, game)
	}
	return filtered
}

func (d *steamData) discoveryWishIDs(ctx context.Context, id string) reading[map[uint32]struct{}] {
	return d.wishIDs.read(ctx, id, wishlistTTL, false, func(ctx context.Context) (*map[uint32]struct{}, problem) {
		client, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		items, issue := client.wishlistItems(ctx, id)
		if issue != "" {
			return nil, issue
		}
		ids := make(map[uint32]struct{}, len(items))
		for _, item := range items {
			ids[item.AppID] = struct{}{}
		}
		return &ids, ""
	})
}

type discoveryGame struct {
	AppID     uint32             `json:"appid"`
	URL       string             `json:"url"`
	Details   *discoveryMetadata `json:"details,omitempty"`
	Tags      []discoveryTag     `json:"tags,omitempty"`
	SteamDeck string             `json:"steam_deck,omitempty"`
	Overall   *discoveryReviews  `json:"overall_reviews,omitempty"`
	Recent    *discoveryReviews  `json:"recent_30_days,omitempty"`
	Warnings  []toolWarning      `json:"warnings,omitempty"`
}

type discoveryMetadata struct {
	Name             string              `json:"name"`
	Type             string              `json:"type,omitempty"`
	ShortDescription string              `json:"short_description,omitempty"`
	Genres           []string            `json:"genres,omitempty"`
	Categories       []discoveryCategory `json:"categories,omitempty"`
	Platforms        []string            `json:"platforms,omitempty"`
	ReleaseDate      *discoveryRelease   `json:"release_date,omitempty"`
	Features         *discoveryFeatures  `json:"features,omitempty"`
	IsFree           bool                `json:"is_free"`
	Price            *storePrice         `json:"price,omitempty"`
}

type discoveryCategory struct {
	ID   uint32 `json:"id"`
	Name string `json:"description"`
}

type discoveryRelease struct {
	Date       string `json:"date"`
	ComingSoon bool   `json:"coming_soon"`
}

type discoveryFeatures struct {
	Controller        string `json:"controller_support,omitempty"`
	SinglePlayer      bool   `json:"single_player"`
	Multiplayer       bool   `json:"multiplayer"`
	OnlineMultiplayer bool   `json:"online_multiplayer"`
	LocalMultiplayer  bool   `json:"local_multiplayer"`
	Coop              bool   `json:"coop"`
	OnlineCoop        bool   `json:"online_coop"`
	LocalCoop         bool   `json:"local_coop"`
	LANCoop           bool   `json:"lan_coop"`
}

func (d *steamDiscovery) gameDetails(ctx context.Context, raw json.RawMessage, country, fallback string, limit int, t *text, out map[string]any) error {
	var args struct {
		AppID    *uint32  `json:"appid"`
		AppIDs   []uint32 `json:"appids"`
		Language string   `json:"language"`
	}
	language, validLanguage := "", false
	if decodeArgs(raw, &args) {
		language, validLanguage = discoveryLanguage(args.Language, fallback)
	}
	if !validLanguage || args.AppID != nil && args.AppIDs != nil {
		return invalid(fmt.Sprintf(t.message(problemDiscoveryDetails, false), limit))
	}
	ids := args.AppIDs
	if args.AppID != nil {
		ids = []uint32{*args.AppID}
	}
	if len(ids) == 0 || len(ids) > limit || slices.Contains(ids, 0) {
		return invalid(fmt.Sprintf(t.message(problemDiscoveryDetails, false), limit))
	}
	games := make([]discoveryGame, 0, len(ids))
	selected := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(selected, id) {
			selected = append(selected, id)
			games = append(games, discoveryGame{AppID: id, URL: storeLink(id)})
		}
	}
	var browse map[uint32]discoveryBrowseItem
	var browseIssue problem
	var catalog reading[[]discoveryTag]
	var reads sync.WaitGroup
	reads.Go(func() {
		browse, browseIssue = d.browseGames(ctx, selected, country, language)
		for _, item := range browse {
			if len(item.TagIDs) > 0 {
				catalog = d.catalog(ctx, language)
				break
			}
		}
	})
	workers := make(chan struct{}, detailConcurrency)
	end := d.now().Unix()
	for i := range games {
		reads.Go(func() {
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
				d.readGame(ctx, &games[i], country, language, end, t)
			case <-ctx.Done():
				games[i].warn("details", sanitizeError(ctx.Err()), t)
			}
		})
	}
	reads.Wait()

	describeReading(out, "store_browse", reading[discoveryBrowseItem]{Issue: browseIssue}, t)
	describeReading(out, "tags", catalog, t)
	if catalog.Issue != "" && catalog.Data == nil {
		describeReading(out, "tags", reading[[]discoveryTag]{Issue: problemBuiltInTags}, t)
		out["tag_language"] = builtInTagLanguage(language)
	}
	tagNames := make(map[uint32]string)
	if catalog.Data != nil {
		for _, tag := range *catalog.Data {
			tagNames[tag.ID] = tag.Name
		}
	}
	for _, tag := range builtInTags(builtInTagLanguage(language)) {
		if tagNames[tag.ID] == "" {
			tagNames[tag.ID] = tag.Name
		}
	}
	available := false
	for i := range games {
		g := &games[i]
		store, found := browse[g.AppID]
		if found {
			g.SteamDeck = deckStatus(store.Platforms.SteamDeck)
			for _, id := range store.TagIDs[:min(detailTagLimit, len(store.TagIDs))] {
				if id != 0 && !slices.ContainsFunc(g.Tags, func(tag discoveryTag) bool { return id == tag.ID }) {
					g.Tags = append(g.Tags, discoveryTag{id, tagNames[id]})
				}
			}
		} else if browseIssue == "" {
			g.warn("store_browse", problemMissing, t)
		}
		available = available || g.Details != nil || found || g.Overall != nil || g.Recent != nil
	}
	out["available"], out["games"], out["country_code"], out["language"] = available, games, country, language
	return nil
}

func (g *discoveryGame) warn(dataset string, issue problem, t *text) {
	if issue != "" {
		g.Warnings = append(g.Warnings, toolWarning{Dataset: dataset, Code: issue, Message: t.message(issue, false)})
	}
}

func (d *steamDiscovery) readGame(ctx context.Context, g *discoveryGame, country, language string, end int64, t *text) {
	var detailsIssue, overallIssue, recentIssue problem
	var reads sync.WaitGroup
	reads.Go(func() { g.Details, detailsIssue = d.gameMetadata(ctx, g.AppID, country, language) })
	reads.Go(func() { g.Overall, overallIssue = d.overallReviews(ctx, g.AppID, language) })
	reads.Go(func() { g.Recent, recentIssue = d.gameReviews(ctx, g.AppID, language, end) })
	reads.Wait()
	dataset := "details"
	if g.Details != nil {
		dataset = "price"
	}
	g.warn(dataset, detailsIssue, t)
	g.warn("overall_reviews", overallIssue, t)
	g.warn("recent_30_days", recentIssue, t)
}

func (d *steamDiscovery) gameMetadata(ctx context.Context, id uint32, country, language string) (*discoveryMetadata, problem) {
	appID := appIDText(id)
	query := url.Values{"appids": {appID}, "cc": {country}, "l": {language}, "filters": {
		"basic,genres,categories,release_date,platforms,price_overview",
	}}
	var reply map[string]struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if p := d.get(ctx, gameLookupTimeout, gameResponseBytes, storeBase+storePricesPath, query, &reply); p != "" {
		return nil, p
	}
	item := reply[appID]
	if item.Success == nil {
		return nil, problemResponse
	}
	if !*item.Success {
		return nil, problemMissing
	}
	var data struct {
		AppID            uint32              `json:"steam_appid"`
		Name             string              `json:"name"`
		Type             string              `json:"type"`
		ShortDescription string              `json:"short_description"`
		Controller       string              `json:"controller_support"`
		IsFree           bool                `json:"is_free"`
		ReleaseDate      *discoveryRelease   `json:"release_date"`
		Categories       []discoveryCategory `json:"categories"`
		Genres           []struct {
			Name string `json:"description"`
		} `json:"genres"`
		Platforms struct{ Windows, Mac, Linux bool }
	}
	if json.Unmarshal(item.Data, &data) != nil || data.AppID != id || strings.TrimSpace(data.Name) == "" {
		return nil, problemResponse
	}
	game := &discoveryMetadata{
		Name: d.client.text(data.Name), Type: cleanText(redactCredentials(data.Type), 20), IsFree: data.IsFree,
		ShortDescription: shortStoreDescription(data.ShortDescription), ReleaseDate: data.ReleaseDate,
	}
	if game.ReleaseDate != nil {
		game.ReleaseDate.Date = cleanText(redactCredentials(game.ReleaseDate.Date), 40)
	}
	for _, genre := range data.Genres[:min(8, len(data.Genres))] {
		if name := cleanText(redactCredentials(genre.Name), tagNameRunes); name != "" {
			game.Genres = append(game.Genres, name)
		}
	}
	for _, category := range data.Categories[:min(24, len(data.Categories))] {
		if category.Name = cleanText(redactCredentials(category.Name), tagNameRunes); category.ID != 0 && category.Name != "" {
			game.Categories = append(game.Categories, category)
		}
	}
	for _, platform := range []struct {
		name      string
		supported bool
	}{{"windows", data.Platforms.Windows}, {"macos", data.Platforms.Mac}, {"linux", data.Platforms.Linux}} {
		if platform.supported {
			game.Platforms = append(game.Platforms, platform.name)
		}
	}
	game.Features = storeFeatures(data.Categories, data.Controller)
	price, valid := d.client.parsePrice(item.Data)
	game.Price = price
	if !valid {
		return game, problemResponse
	}
	return game, ""
}

func shortStoreDescription(raw string) string {
	tokens := html.NewTokenizer(strings.NewReader(raw))
	var text strings.Builder
	for kind := tokens.Next(); kind != html.ErrorToken; kind = tokens.Next() {
		if kind == html.TextToken {
			text.Write(tokens.Text())
		} else {
			text.WriteByte(' ')
		}
	}
	return cleanText(redactCredentials(strings.Join(strings.Fields(text.String()), " ")), 500)
}

func storeFeatures(categories []discoveryCategory, controller string) *discoveryFeatures {
	if categories == nil && controller == "" {
		return nil
	}
	f := &discoveryFeatures{}
	if controller == "full" || controller == "partial" {
		f.Controller = controller
	}
	for _, category := range categories {
		switch category.ID {
		case 1, 27, 47: // Multi-player, cross-platform multiplayer, LAN PvP
			f.Multiplayer = true
		case 2:
			f.SinglePlayer = true
		case 9:
			f.Coop = true
		case 28:
			f.Controller = "full"
		case 18:
			if f.Controller == "" {
				f.Controller = "partial"
			}
		case 36, 38: // Online PvP / Co-op
			f.OnlineMultiplayer = true
			f.OnlineCoop = f.OnlineCoop || category.ID == 38
		case 37, 39: // Shared/split-screen PvP / Co-op
			f.LocalMultiplayer = true
			f.LocalCoop = f.LocalCoop || category.ID == 39
		case 48:
			f.LANCoop = true
		}
	}
	f.Coop = f.Coop || f.OnlineCoop || f.LocalCoop || f.LANCoop
	f.Multiplayer = f.Multiplayer || f.Coop || f.OnlineMultiplayer || f.LocalMultiplayer
	return f
}

type discoveryBrowseItem struct {
	AppID     uint32   `json:"appid"`
	Success   int      `json:"success"`
	TagIDs    []uint32 `json:"tagids"`
	Platforms struct {
		SteamDeck int `json:"steam_deck_compat_category"`
	} `json:"platforms"`
}

func (d *steamDiscovery) browseGames(ctx context.Context, apps []uint32, country, language string) (map[uint32]discoveryBrowseItem, problem) {
	ids := make([]map[string]uint32, 0, len(apps))
	for _, id := range apps {
		ids = append(ids, map[string]uint32{"appid": id})
	}
	input, _ := json.Marshal(map[string]any{
		"ids":          ids,
		"context":      map[string]string{"country_code": country, "language": language},
		"data_request": map[string]any{"include_tag_count": detailTagLimit, "include_platforms": true},
	})
	var reply struct {
		Response struct {
			Items []discoveryBrowseItem `json:"store_items"`
		} `json:"response"`
	}
	if p := d.get(ctx, gameLookupTimeout, gameResponseBytes, apiBase+storeNamesPath, url.Values{"input_json": {string(input)}}, &reply); p != "" {
		return nil, p
	}
	if reply.Response.Items == nil {
		return nil, problemResponse
	}
	items := make(map[uint32]discoveryBrowseItem, len(apps))
	for _, item := range reply.Response.Items {
		if item.Success == 1 && slices.Contains(apps, item.AppID) {
			items[item.AppID] = item
		}
	}
	return items, ""
}

func deckStatus(category int) string {
	switch category {
	case 1:
		return "unsupported"
	case 2:
		return "playable"
	case 3:
		return "verified"
	default:
		return "unknown"
	}
}

type discoveryReviews struct {
	Count           uint32   `json:"count"`
	PositivePercent *float64 `json:"positive_percent,omitempty"`
	Summary         string   `json:"summary,omitempty"`
}

type steamReviewSummary struct {
	Total    *uint32 `json:"total_reviews"`
	Positive *uint32 `json:"total_positive"`
	Label    string  `json:"review_score_desc"`
}

func (s steamReviewSummary) stats() *discoveryReviews {
	if s.Total == nil || s.Positive == nil && *s.Total > 0 || s.Positive != nil && *s.Positive > *s.Total {
		return nil
	}
	out := &discoveryReviews{Count: *s.Total, Summary: cleanText(redactCredentials(s.Label), 80)}
	if out.Count > 0 {
		percent := math.Round(1000*float64(*s.Positive)/float64(out.Count)) / 10
		out.PositivePercent = &percent
	}
	return out
}

func (d *steamDiscovery) overallReviews(ctx context.Context, appID uint32, language string) (*discoveryReviews, problem) {
	var reply struct {
		Success int                `json:"success"`
		Summary steamReviewSummary `json:"query_summary"`
	}
	query := url.Values{"json": {"1"}, "filter": {"recent"}, "language": {"all"}, "l": {language},
		"purchase_type": {"steam"}, "num_per_page": {"1"}}
	if issue := d.get(ctx, legacyReviewTimeout, reviewResponseBytes, storeBase+appReviewsPath+appIDText(appID), query, &reply); issue == "" && reply.Success == 1 {
		if stats := reply.Summary.stats(); stats != nil {
			return stats, ""
		}
	}
	// Valve deprecated the Store route. Use its official replacement on API
	// failures or redirects, which the credential-safe HTTP client refuses.
	return d.gameReviews(ctx, appID, language, 0)
}

func (d *steamDiscovery) gameReviews(ctx context.Context, appID uint32, language string, end int64) (*discoveryReviews, problem) {
	input := map[string]any{
		"appid": appID, "filter": 1, "languages": []string{"all"}, "num_per_page": 1, "display_language": language,
	}
	if end != 0 {
		input["date_range_start"] = end - 30*24*60*60
		input["date_range_end"] = end
	}
	payload, _ := json.Marshal(input)
	var reply struct {
		Response struct {
			Summary  steamReviewSummary `json:"query_summary"`
			Matching *uint32            `json:"total_matching"`
		} `json:"response"`
	}
	query := url.Values{"input_json": {string(payload)}}
	if p := d.get(ctx, gameLookupTimeout, reviewResponseBytes, apiBase+reviewsAPIPath, query, &reply); p != "" {
		return nil, p
	}
	stats := reply.Response.Summary.stats()
	if stats == nil && reply.Response.Matching != nil && *reply.Response.Matching == 0 && reply.Response.Summary.Total == nil {
		stats = &discoveryReviews{}
	}
	if stats == nil || end != 0 && (reply.Response.Matching == nil || stats.Count != *reply.Response.Matching) {
		return nil, problemResponse
	}
	return stats, ""
}
