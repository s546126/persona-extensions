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
	vanity      *cacheSlot[string]
	profile     *cacheSlot[player]
	library     *cacheSlot[gameList]
	recent      *cacheSlot[gameList]
	wishes      *cacheSlot[wishList]
	prices      *PriceHistoryStore
	playtime    *PlaytimeSnapshotStore
	lastAccount atomic.Pointer[reading[player]]
}

func newSteamData(cfg config, key, dir string, httpClient *http.Client, now func() time.Time) *steamData {
	cfg, setup := cfg.normalized()
	key = strings.TrimSpace(key)
	if setup == "" {
		switch {
		case key == "":
			setup = problemSetup
		case cfg.SteamID == key || strings.ContainsAny(key, "\r\n\x00"):
			setup = problemKey
		}
	}
	var cacheDir string
	if dir != "" {
		cacheDir = filepath.Join(dir, "steam-cache-v1")
	}
	return &steamData{
		client:   steamClient{http: httpClient, key: key},
		config:   cfg,
		setup:    setup,
		vanity:   newSlot[string](cacheDir, "vanity", now),
		profile:  newSlot[player](cacheDir, "profile", now),
		library:  newSlot[gameList](cacheDir, "library", now),
		recent:   newSlot[gameList](cacheDir, "recent", now),
		wishes:   newSlot[wishList](cacheDir, "wishlist", now),
		prices:   NewPriceHistoryStore(dir),
		playtime: NewPlaytimeSnapshotStore(dir),
	}
}

func (d *steamData) account(ctx context.Context, ttl time.Duration, refresh bool) (out reading[player]) {
	if d.setup != "" {
		return reading[player]{Issue: d.setup}
	}
	defer func() {
		if out.Issue != problemCanceled {
			d.lastAccount.Store(&out)
		}
	}()
	id := d.config.SteamID
	if !validSteamID(id) {
		r := d.vanity.read(ctx, id, profileTTL, refresh, func(ctx context.Context) (*string, problem) {
			return d.client.resolve(ctx, id)
		})
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

func (d *steamData) readPlayer(ctx context.Context, id string, ttl time.Duration, refresh bool) reading[player] {
	return d.profile.read(ctx, id, ttl, refresh, func(ctx context.Context) (*player, problem) {
		return d.client.profile(ctx, id)
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

func (d *steamData) ownedGames(ctx context.Context, p reading[player], refresh bool) reading[gameList] {
	if issue := publicProfile(p); issue != "" {
		return reading[gameList]{Issue: issue}
	}
	id := p.Data.SteamID
	return d.library.read(ctx, id, libraryTTL, refresh, func(ctx context.Context) (*gameList, problem) {
		list, issue := d.client.games(ctx, id, false)
		if issue == "" && list != nil && !list.Truncated && !p.Stale {
			_ = d.playtime.Record(id, snapshotPlaytime(list, d.library.now()))
		}
		return list, issue
	})
}

func (d *steamData) recentGames(ctx context.Context, p reading[player], refresh bool) reading[gameList] {
	if issue := publicProfile(p); issue != "" {
		return reading[gameList]{Issue: issue}
	}
	return d.recent.read(ctx, p.Data.SteamID, recentTTL, refresh, func(ctx context.Context) (*gameList, problem) {
		return d.client.games(ctx, p.Data.SteamID, true)
	})
}

func (d *steamData) wishlist(ctx context.Context, p reading[player], language string, refresh bool) reading[wishList] {
	if issue := publicProfile(p); issue != "" {
		return reading[wishList]{Issue: issue}
	}
	id, country := p.Data.SteamID, d.config.CountryCode
	return d.wishes.read(ctx, id+"/"+country+"/"+language, wishlistTTL, refresh, func(ctx context.Context) (*wishList, problem) {
		list, issue := d.client.wishlist(ctx, id, country, language)
		if list != nil && issue != problemCanceled && !p.Stale {
			_ = d.prices.Observe(id, country, d.wishes.now(), wishlistQuotes(list))
		}
		return list, issue
	})
}

func (d *steamData) achievements(ctx context.Context, p reading[player], appID uint32) reading[achievementProgress] {
	if issue := publicProfile(p); issue != "" {
		return reading[achievementProgress]{Issue: issue}
	}
	progress, issue := d.client.achievements(ctx, p.Data.SteamID, appID)
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
