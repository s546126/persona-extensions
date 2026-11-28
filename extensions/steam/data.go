package main

import (
	"cmp"
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	sortPlaytime  = "playtime"
	sortName      = "name"
	maxQueryRunes = 100
	readBudget    = 12 * time.Second
	httpTimeout   = 4 * time.Second
)

type steamData struct {
	client      steamClient
	config      config
	setup       problem
	session     *qrSession
	vanity      *cacheSlot[string]
	profile     *cacheSlot[player]
	library     *cacheSlot[gameList]
	recent      *cacheSlot[gameList]
	wishes      *cacheSlot[wishList]
	wishIDs     *cacheSlot[map[uint32]struct{}]
	friendList  *cacheSlot[friendRoster]
	friendState *cacheSlot[[]player]
	stores      steamStores
	memory      memoryObservations
	lastAccount atomic.Pointer[reading[player]]
	inbox       atomic.Pointer[[]wishlistDelivery]
}

func newSteamData(cfg config, key, dir string, httpClient *http.Client, now func() time.Time) *steamData {
	key = strings.TrimSpace(key)
	cfg, setup := cfg.normalized(key)
	if cfg.AuthMode == modeQR || setup != "" {
		if setup == "" {
			setup = problemAuthRequired
		}
		return &steamData{config: cfg, setup: setup, client: steamClient{key: key}}
	}
	return newAccountData(cfg, steamClient{http: httpClient, key: key}, dir, now, nil)
}

func newAccountData(cfg config, client steamClient, dataDir string, now func() time.Time, session *qrSession) *steamData {
	if cfg.AuthMode != modeWebAPIKey || session != nil {
		dataDir = ""
	}
	var cacheDir string
	if dataDir != "" {
		cacheDir = filepath.Join(dataDir, "steam-cache-v1")
	}
	d := &steamData{
		client:  client,
		config:  cfg,
		session: session,
		vanity:  newSlot[string](cacheDir, "vanity", now, session),
		profile: newSlot[player](cacheDir, "profile", now, session),
		library: newSlot[gameList](cacheDir, "library", now, session),
		recent:  newSlot[gameList](cacheDir, "recent", now, session),
		wishes:  newSlot[wishList](cacheDir, "wishlist", now, session),
		wishIDs: newSlot[map[uint32]struct{}]("", "", now, session),
		stores:  newSteamStores(dataDir),
	}
	if cfg.FriendsRadarEnabled {
		d.friendList = newSlot[friendRoster]("", "", now, session)
		d.friendState = newSlot[[]player]("", "", now, session)
		d.friendList.freshOnly, d.friendState.freshOnly = true, true
	}
	return d
}

// A UI or tool call uses one credential generation from its first read to its
// final response, even if the user signs in to a different account mid-call.
func (a *app) readContext(parent context.Context) (*steamData, context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, readBudget)
	if a.qr == nil || a.setup != "" && a.setup != problemAuthRequired {
		return a.steamData, ctx, cancel
	}
	if !a.qr.waitLoaded(ctx) {
		return &steamData{config: a.config, setup: sanitizeError(ctx.Err())}, ctx, cancel
	}
	a.qr.mu.Lock()
	s := a.qr.sessionLocked()
	issue := a.qr.issue
	if a.qr.login != nil {
		issue = problemAuthPending
	}
	if s == nil {
		a.qr.mu.Unlock()
		return &steamData{config: a.config, setup: issue}, ctx, cancel
	}
	d := s.reads
	a.qr.mu.Unlock()
	stop := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	return d, ctx, func() {
		stop()
		cancel()
	}
}

func (d *steamData) sessionIssue() problem {
	if d.session == nil {
		return ""
	}
	a := d.session.auth
	a.mu.Lock()
	defer a.mu.Unlock()
	if d.session.currentLocked() && a.sessionLocked() == d.session {
		return ""
	}
	if a.issue == problemAuthExpired {
		return problemAuthExpired
	}
	return problemAuthRequired
}

func (d *steamData) readClient(ctx context.Context) (steamClient, problem) {
	c := d.client
	if d.session != nil {
		t, issue := d.session.access(ctx)
		if issue != "" {
			return steamClient{}, issue
		}
		c.access, c.refresh = t.access, t.secret.RefreshToken
	}
	return c, ""
}

// The session lock also guards lookup and publication of these in-memory slots.
func (d *steamData) clearLocked() {
	d.profile.clear()
	d.library.clear()
	d.recent.clear()
	d.wishes.clear()
	d.wishIDs.clear()
	if d.friendList != nil {
		d.friendList.clear()
		d.friendState.clear()
	}
	d.stores.clearMemory()
	d.memory.genres.clearMemory()
	d.memory.achievements.clearMemory()
	d.inbox.Store(nil)
}

func (d *steamData) recordSnapshot(record func()) {
	if s := d.session; s != nil {
		a := s.auth
		a.mu.Lock()
		defer a.mu.Unlock()
		if !s.currentLocked() || a.sessionLocked() != s {
			return
		}
	}
	record()
}

func (d *steamData) account(ctx context.Context, ttl time.Duration, refresh bool) (out reading[player]) {
	if d.setup != "" {
		return reading[player]{Issue: d.setup}
	}
	if d.session != nil {
		return d.readPlayer(ctx, d.config.SteamID, ttl, refresh)
	}
	defer func() {
		if out.Issue != problemCanceled {
			d.lastAccount.Store(&out)
		}
	}()
	id := d.config.SteamID
	if !validSteamID(id) {
		r := d.resolveVanity(ctx, refresh)
		if r.Data == nil {
			return reading[player]{Issue: r.Issue}
		}
		id = *r.Data
		// A failed vanity lookup may be using the former account. Keep the whole read visibly stale.
		p := d.readPlayer(ctx, id, ttl, refresh)
		if p.Data != nil && r.Stale && p.Issue == "" {
			p.Stale, p.Issue = true, r.Issue
		}
		return p
	}
	return d.readPlayer(ctx, id, ttl, refresh)
}

func (d *steamData) resolveVanity(ctx context.Context, refresh bool) reading[string] {
	name := d.config.SteamID
	return d.vanity.read(ctx, name, profileTTL, refresh, func(ctx context.Context) (*string, problem) {
		return d.client.resolve(ctx, name)
	})
}

func (d *steamData) readPlayer(ctx context.Context, id string, ttl time.Duration, refresh bool) reading[player] {
	return d.profile.read(ctx, id, ttl, refresh, func(ctx context.Context) (*player, problem) {
		c, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		return c.profile(ctx, id)
	})
}

func publicProfile(p reading[player]) problem {
	if p.Data == nil {
		if p.Issue == "" {
			return problemResponse
		}
		return p.Issue
	}
	if !p.Data.Public {
		return problemPrivate
	}
	return ""
}

func (d *steamData) readID(p reading[player]) (string, problem) {
	if d.setup != "" {
		return "", d.setup
	}
	if d.session != nil {
		return d.config.SteamID, ""
	}
	if issue := publicProfile(p); issue != "" {
		return "", issue
	}
	return p.Data.SteamID, ""
}

func (d *steamData) ownedGames(ctx context.Context, p reading[player], refresh bool) reading[gameList] {
	return d.readGames(ctx, p, d.library, libraryTTL, refresh, false)
}

func (d *steamData) recentGames(ctx context.Context, p reading[player], refresh bool) reading[gameList] {
	return d.readGames(ctx, p, d.recent, recentTTL, refresh, true)
}

func (d *steamData) readGames(ctx context.Context, p reading[player], slot *cacheSlot[gameList], ttl time.Duration, refresh, recent bool) reading[gameList] {
	id, issue := d.readID(p)
	if issue != "" {
		return reading[gameList]{Issue: issue}
	}
	return slot.read(ctx, id, ttl, refresh, func(ctx context.Context) (*gameList, problem) {
		c, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		list, issue := c.games(ctx, id, recent)
		if !recent && issue == "" && list != nil && !list.Truncated && !p.Stale {
			snapshot := snapshotPlaytime(list, slot.now())
			d.recordSnapshot(func() { _ = d.stores.playtime.Record(id, snapshot) })
		}
		return list, issue
	})
}

func (d *steamData) wishlist(ctx context.Context, p reading[player], language string, refresh bool) reading[wishList] {
	id, issue := d.readID(p)
	if issue != "" {
		return reading[wishList]{Issue: issue}
	}
	country := d.config.CountryCode
	return d.wishes.read(ctx, id+"/"+country+"/"+language, wishlistTTL, refresh, func(ctx context.Context) (*wishList, problem) {
		c, issue := d.readClient(ctx)
		if issue != "" {
			return nil, issue
		}
		list, issue := c.wishlist(ctx, id, country, language)
		if list != nil && issue != problemCanceled && !p.Stale {
			at, quotes := d.wishes.now(), wishlistQuotes(list)
			d.recordSnapshot(func() { _ = d.stores.prices.Observe(id, country, at, quotes) })
		}
		return list, issue
	})
}

func (d *steamData) achievements(ctx context.Context, p reading[player], appID uint32, language string) reading[achievementProgress] {
	id, issue := d.readID(p)
	if issue != "" {
		return reading[achievementProgress]{Issue: issue}
	}
	c, issue := d.readClient(ctx)
	if issue != "" {
		return reading[achievementProgress]{Issue: issue}
	}
	progress, issue := c.achievements(ctx, id, appID, language)
	d.rememberAchievement(id, progress)
	return reading[achievementProgress]{Data: progress, Issue: issue}
}

func compareName(a, b game) int {
	if byName := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); byName != 0 {
		return byName
	}
	return cmp.Compare(a.AppID, b.AppID)
}

func compareRecent(a, b game) int {
	if byTime := cmp.Compare(b.Playtime2Weeks, a.Playtime2Weeks); byTime != 0 {
		return byTime
	}
	return compareName(a, b)
}

func libraryGames(list []game, query, sort string) []game {
	query = strings.ToLower(strings.TrimSpace(query))
	matches := make([]game, 0, len(list))
	for _, g := range list {
		if query == "" || strings.Contains(strings.ToLower(g.Name), query) || appIDText(g.AppID) == query {
			matches = append(matches, g)
		}
	}
	slices.SortFunc(matches, func(a, b game) int {
		if sort == sortPlaytime && a.PlaytimeForever != b.PlaytimeForever {
			return cmp.Compare(b.PlaytimeForever, a.PlaytimeForever)
		}
		return compareName(a, b)
	})
	return matches
}
