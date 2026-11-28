package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	playtimeRecapName = "steam_playtime_recap"
	defaultRecapTop   = 5
	maxRecapTop       = 20

	problemRecapStore problem = "playtime_recap_history_unavailable"
)

type recapPeriod string

const (
	recapMonth   recapPeriod = "month"
	recapQuarter recapPeriod = "quarter"
	recapYear    recapPeriod = "year"
	recapAll     recapPeriod = "all"
)

var recapPeriods = [...]recapPeriod{recapMonth, recapQuarter, recapYear, recapAll}

var playtimeRecapTool = wire.ConnectorTool{
	Name:        playtimeRecapName,
	Description: "Review Steam playtime and backlog progress for the current UTC month, quarter, year, or all locally observed history. With no comparable snapshot, shows the library, last two weeks, and local annotations.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string","enum":["month","quarter","year","all"],"default":"month"},"top_n":{"type":"integer","minimum":1,"maximum":20,"default":5}}}`),
}

func monthStart(at time.Time) time.Time {
	y, m, _ := at.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

func (p recapPeriod) start(at time.Time) time.Time {
	start := monthStart(at)
	switch p {
	case recapQuarter:
		return start.AddDate(0, -((int(start.Month()) - 1) % 3), 0)
	case recapYear:
		return time.Date(start.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	case recapAll:
		return time.Time{}
	default:
		return start
	}
}

type recapGame struct {
	AppID   uint32 `json:"appid"`
	Name    string `json:"name"`
	Minutes uint32 `json:"minutes"`
}

type recapRecent struct {
	Playtime  uint64      `json:"playtime_2weeks"`
	TopGames  []recapGame `json:"top_games"`
	Truncated bool        `json:"truncated"`
}

type recapProgress struct {
	CountChanges map[string]int `json:"count_changes"`
	MovedInto    map[string]int `json:"moved_into"`
	AtStart      int            `json:"backlog_at_start"`
	Consumed     int            `json:"backlog_consumed"`
	Rate         *float64       `json:"backlog_consumption_rate"`
}

type recapDelta struct {
	From            time.Time      `json:"from"`
	To              time.Time      `json:"to"`
	PlaytimeMinutes uint64         `json:"playtime_minutes"`
	NewGames        int            `json:"new_games"`
	TopGames        []recapGame    `json:"top_games"`
	Progress        *recapProgress `json:"progress,omitempty"`
}

type playtimeRecap struct {
	Period      recapPeriod           `json:"period"`
	Available   bool                  `json:"available"`
	Library     *PlaytimeSnapshot     `json:"library,omitempty"`
	Truncated   bool                  `json:"library_truncated,omitempty"`
	Recent      *recapRecent          `json:"recent,omitempty"`
	Annotations map[BacklogStatus]int `json:"annotation_counts,omitempty"`
	Delta       *recapDelta           `json:"delta,omitempty"`
	Hint        string                `json:"hint,omitempty"`
	Warnings    []toolWarning         `json:"warnings,omitempty"`
}

func (r *playtimeRecap) warn(t *text, dataset string, issue problem, stale bool) {
	if issue != "" {
		r.Warnings = append(r.Warnings, toolWarning{dataset, issue, t.message(issue, stale), stale})
	}
}

func (a *app) callPlaytimeRecap(ctx context.Context, raw json.RawMessage) (playtimeRecap, error) {
	var args struct {
		Period *recapPeriod `json:"period"`
		Top    *int         `json:"top_n"`
	}
	t := textFor(a.locale)
	if !decodeArgs(raw, &args) || (args.Period != nil && !slices.Contains(recapPeriods[:], *args.Period)) ||
		(args.Top != nil && (*args.Top < 1 || *args.Top > maxRecapTop)) {
		return playtimeRecap{}, invalid(t.recap.invalidArgs)
	}
	period, top := recapMonth, defaultRecapTop
	if args.Period != nil {
		period = *args.Period
	}
	if args.Top != nil {
		top = *args.Top
	}
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	result := a.readPlaytimeRecap(ctx, d, t, period, top, false)
	if issue := d.sessionIssue(); issue != "" {
		result = playtimeRecap{Period: period}
		result.warn(t, "playtime_recap", issue, false)
	}
	return result, nil
}

func (a *app) readPlaytimeRecap(ctx context.Context, d *steamData, t *text, period recapPeriod, top int, refresh bool) playtimeRecap {
	result := playtimeRecap{Period: period}
	var account reading[player]
	if d.session == nil {
		account = d.account(ctx, profileTTL, refresh)
		result.warn(t, "profile", account.Issue, account.Stale)
	}
	id, issue := d.readID(account)
	if issue != "" {
		if issue != account.Issue {
			result.warn(t, "library", issue, false)
		}
		return result
	}
	var owned, recent reading[gameList]
	var reads sync.WaitGroup
	reads.Go(func() {
		owned = d.ownedGames(ctx, account, refresh)
		// A still-fresh library cache can cross midnight. Get a new observation for the new day.
		if !refresh && owned.Data != nil && owned.Issue == "" && !utcDay(owned.At).Equal(utcDay(d.library.now())) {
			owned = d.ownedGames(ctx, account, true)
		}
	})
	reads.Go(func() { recent = d.recentGames(ctx, account, refresh) })
	reads.Wait()
	result.warn(t, "library", owned.Issue, owned.Stale)
	result.warn(t, "recent_games", recent.Issue, recent.Stale)
	if r := recent.Data; r != nil {
		result.Available = true
		result.Recent = &recapRecent{Playtime: r.Playtime2Weeks, TopGames: make([]recapGame, 0, top), Truncated: r.Truncated}
		for _, g := range r.Games {
			if g.Playtime2Weeks == 0 || len(result.Recent.TopGames) == top {
				break // Steam's dedicated recent list is sorted by recent minutes.
			}
			result.Recent.TopGames = append(result.Recent.TopGames, recapGame{g.AppID, t.gameName(g.AppID, g.Name), g.Playtime2Weeks})
		}
	}
	if owned.Data == nil {
		return result
	}
	library := snapshotPlaytime(owned.Data, owned.At)
	result.Library, result.Available, result.Truncated = &library, true, owned.Data.Truncated
	marks, backlogIssue := a.backlogEntries(d, account)
	result.warn(t, "backlog", backlogIssue, false)
	current := recapSnapshot{At: owned.At, Minutes: make(map[uint32]uint32, len(owned.Data.Games))}
	if backlogIssue == "" {
		current.Statuses = make(map[uint32]BacklogStatus)
		result.Annotations = make(map[BacklogStatus]int, len(backlogStatuses))
		for _, status := range backlogStatuses {
			result.Annotations[status] = 0
		}
	}
	for _, g := range owned.Data.Games {
		current.Minutes[g.AppID] = g.PlaytimeForever
		if status := marks[g.AppID].Status; slices.Contains(backlogStatuses[:], status) {
			current.Statuses[g.AppID] = status
			result.Annotations[status]++
		}
	}
	result.Hint = t.recap.noBaseline
	if result.Truncated || owned.Issue != "" || account.Stale || !validSnapshotTime(current.At) || ctx.Err() != nil {
		return result
	}
	var history []recapSnapshot
	var historyErr error
	d.recordSnapshot(func() { history, historyErr = d.stores.playtime.observeRecap(id, current) })
	if historyErr != nil {
		result.warn(t, "playtime_history", problemRecapStore, false)
	}
	start, today := period.start(current.At), utcDay(current.At)
	for _, prior := range history {
		if prior.Minutes != nil && !prior.At.Before(start) && prior.At.Before(today) {
			delta := compareRecap(prior, current, owned.Data.Games, t, top)
			result.Delta, result.Hint = &delta, ""
			break
		}
	}
	return result
}

func compareRecap(prior, current recapSnapshot, games []game, t *text, top int) recapDelta {
	out := recapDelta{From: prior.At, To: current.At, TopGames: make([]recapGame, 0)}
	for _, g := range games {
		before, existed := prior.Minutes[g.AppID]
		if !existed {
			out.NewGames++
		}
		if g.PlaytimeForever > before {
			minutes := g.PlaytimeForever - before
			out.PlaytimeMinutes += uint64(minutes)
			out.TopGames = append(out.TopGames, recapGame{g.AppID, t.gameName(g.AppID, g.Name), minutes})
		}
	}
	slices.SortFunc(out.TopGames, func(a, b recapGame) int {
		if byTime := cmp.Compare(b.Minutes, a.Minutes); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
	out.TopGames = out.TopGames[:min(top, len(out.TopGames))]
	if prior.Statuses != nil && current.Statuses != nil {
		out.Progress = compareRecapProgress(prior, current)
	}
	return out
}

const recapBacklog = "backlog"

var recapBuckets = [...]string{recapBacklog, string(BacklogPlaying), string(BacklogCompleted), string(BacklogDropped)}

func (s recapSnapshot) bucket(id uint32) string {
	return recapBucket(s.Statuses[id], s.Minutes[id])
}

func recapBucket(status BacklogStatus, minutes uint32) string {
	switch status {
	case BacklogPlaying, BacklogCompleted, BacklogDropped:
		return string(status)
	case BacklogQueued, BacklogPaused, BacklogUnplayed:
		return recapBacklog
	}
	if minutes == 0 {
		return recapBacklog
	}
	return ""
}

func compareRecapProgress(prior, current recapSnapshot) *recapProgress {
	out := &recapProgress{CountChanges: make(map[string]int), MovedInto: make(map[string]int)}
	for _, key := range recapBuckets {
		out.CountChanges[key], out.MovedInto[key] = 0, 0
	}
	for id, minutes := range prior.Minutes {
		bucket := prior.bucket(id)
		if bucket != "" {
			out.CountChanges[bucket]--
		}
		if bucket == recapBacklog {
			out.AtStart++
			if after, stillOwned := current.Minutes[id]; stillOwned {
				next := current.bucket(id)
				if next == string(BacklogPlaying) || next == string(BacklogCompleted) || after > minutes {
					out.Consumed++
				}
			}
		}
	}
	for id := range current.Minutes {
		if bucket := current.bucket(id); bucket != "" {
			out.CountChanges[bucket]++
			if _, existed := prior.Minutes[id]; !existed || prior.bucket(id) != bucket {
				out.MovedInto[bucket]++
			}
		}
	}
	if out.AtStart > 0 {
		rate := float64(out.Consumed) / float64(out.AtStart)
		out.Rate = &rate
	}
	return out
}

type memoryDimension string

const (
	memorySourceID                      = "steam-gaming-profile"
	standingByteBudget                  = 2048
	memoryEvidenceLimit                 = 128
	memoryPlaystyle     memoryDimension = "playstyle"
	memoryActiveGames   memoryDimension = "active_games"
	memoryBacklog       memoryDimension = "backlog_progress"
	memoryMilestones    memoryDimension = "milestones"
)

var memoryDimensions = [...]memoryDimension{memoryPlaystyle, memoryActiveGames, memoryBacklog, memoryMilestones}

func (a *app) memoryEntries(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method != "entries" {
		return nil, ext.UnknownMethod("memory." + method)
	}
	var p wire.MemoryEntriesParams
	if !decodeArgs(raw, &p) || p.Limit < 1 || p.Limit > 500 || utf8.RuneCountInString(p.After) > 512 {
		return nil, invalid("Use a memory.entries limit from 1 to 500 and a cursor of at most 512 characters.")
	}
	if p.Contribution != memorySourceID {
		return nil, &ext.Error{Code: wire.ErrorCodeNoContribution, Message: "Unknown Steam gaming profile source."}
	}
	result := wire.MemoryEntriesResult{Entries: []wire.MemoryEntry{}, Tagged: true}
	limit := a.config.MemoryItemLimit()
	if limit == 0 || ctx.Err() != nil {
		return result, nil
	}
	d := a.memoryData()
	if d == nil {
		return result, nil
	}
	id := d.config.SteamID
	if !validSteamID(id) {
		resolved := d.vanity.peek(id, profileTTL)
		if resolved == nil || !validSteamID(*resolved) {
			return result, nil
		}
		id = *resolved
	}
	var owned, recent *gameList
	profileVisible := d.session != nil
	if !profileVisible {
		profile := d.profile.peek(id, maxStaleAge)
		profileVisible = profile != nil && profile.Public && profile.SteamID == id
	}
	if profileVisible {
		owned, recent = d.library.peek(id, maxStaleAge), d.recent.peek(id, maxStaleAge)
	}
	var marks map[uint32]BacklogEntry
	if d.config.exportsMemoryDimension(memoryActiveGames) || d.config.exportsMemoryDimension(memoryBacklog) || d.config.exportsMemoryDimension(memoryMilestones) {
		marks, _ = a.backlog.List(id)
	}
	entries := d.gamingProfile(id, textFor(a.locale), owned, recent, marks, limit)
	if len(entries) == 0 {
		return result, nil
	}
	page, err := pageGamingProfile(entries, p, result)
	if ctx.Err() != nil || (d.session != nil && a.memoryData() != d) {
		return result, nil
	}
	return page, err
}

// The Hub's memory read must not wait for secret restoration, check Steam,
// refresh credentials, or expire a login as a side effect of inspecting it.
func (a *app) memoryData() *steamData {
	if a.setup != "" && a.setup != problemAuthRequired {
		return nil
	}
	if a.qr == nil {
		if a.config.AuthMode == modeWebAPIKey && a.keyStatusIssue() == "" {
			return a.steamData
		}
		return nil
	}
	a.qr.mu.Lock()
	defer a.qr.mu.Unlock()
	if s := a.qr.session; s != nil && s.currentLocked() && s.refreshExpiryLocked().After(a.qr.now()) {
		return s.reads
	}
	return nil
}

func pageGamingProfile(entries []wire.MemoryEntry, p wire.MemoryEntriesParams, result wire.MemoryEntriesResult) (wire.MemoryEntriesResult, error) {
	start := 0
	var fingerprint string
	if p.After != "" || len(entries) > p.Limit {
		// Bind a cursor to the exact read so a cache update between pages cannot
		// skip entries, repeat entries, or mix different account profiles.
		raw, _ := json.Marshal(entries)
		fingerprint = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	if p.After != "" {
		version, offset, _ := strings.Cut(p.After, ":")
		n, err := strconv.Atoi(offset)
		if err != nil || n < 1 || n >= len(entries) || version != fingerprint || offset != strconv.Itoa(n) {
			return result, invalid("The Steam gaming profile cursor is invalid or out of date.")
		}
		start = n
	}
	end := min(start+p.Limit, len(entries))
	result.Entries, result.StandingBytes = entries[start:end], standingByteBudget
	if end < len(entries) {
		result.HasMore, result.After = true, fingerprint+":"+strconv.Itoa(end)
	}
	return result, nil
}

type observedGenres struct {
	at    time.Time
	names []string
}

type observedAchievement struct {
	at              time.Time
	unlocked, total int
}

func (p observedAchievement) nearComplete(now time.Time) bool {
	age := now.Sub(p.at)
	return p.total > 0 && p.unlocked*10 >= p.total*9 && age >= 0 && age <= maxStaleAge
}

// Only compact signals from foreground tool calls are retained. These stores
// have no disk path and are cleared with the owning QR session's other caches.
type memoryObservations struct {
	genres       snapshotFile[observedGenres]
	achievements snapshotFile[observedAchievement]
}

func (d *steamData) rememberAchievement(id string, p *achievementProgress) {
	if !d.config.exportsMemoryDimension(memoryMilestones) || p == nil || p.Total <= 0 || p.Total > maxGames || p.Unlocked < 0 || p.Unlocked > p.Total {
		return
	}
	d.recordSnapshot(func() {
		_ = d.memory.achievements.update(func(entries map[string]observedAchievement) (bool, error) {
			entries[id+"/"+appIDText(p.AppID)] = observedAchievement{d.library.now(), p.Unlocked, p.Total}
			trimOldest(entries, memoryEvidenceLimit, func(p observedAchievement) time.Time { return p.at })
			return true, nil
		})
	})
}

func (d *steamData) rememberGenres(games []discoveryGame) {
	d.recordSnapshot(func() {
		_ = d.memory.genres.update(func(entries map[string]observedGenres) (bool, error) {
			now := d.library.now()
			for _, game := range games {
				if game.Details == nil {
					continue
				}
				var names []string
				for _, genre := range game.Details.Genres {
					if label := d.memoryLabel(genre); label != "" && !slices.Contains(names, label) {
						names = append(names, label)
					}
				}
				entries[appIDText(game.AppID)] = observedGenres{now, names}
			}
			trimOldest(entries, memoryEvidenceLimit, func(g observedGenres) time.Time { return g.at })
			return true, nil
		})
	})
}

var privateMemoryLabel = regexp.MustCompile(`(?i)[0-9]{17,}|[a-f0-9]{32,}|eyj[a-z0-9_-]*\.[a-z0-9_-]+\.[a-z0-9_-]+`)

func (d *steamData) memoryLabel(raw string) string {
	label := redactCredentials(raw, d.client.key)
	if strings.Contains(label, "@") || strings.Contains(label, "[redacted]") || privateMemoryLabel.MatchString(label) {
		return ""
	}
	return cleanText(label, 60)
}

func gamingMemoryEntry(dimension memoryDimension, key, text string, rank int, standing bool, names ...string) wire.MemoryEntry {
	return wire.MemoryEntry{
		ID: string(dimension) + "." + key, Facet: string(dimension), Text: text,
		Status: "active", Rank: rank, Standing: standing, Subjects: append([]string{"Steam"}, names...),
	}
}

func (d *steamData) gamingProfile(id string, t *text, owned, recent *gameList, marks map[uint32]BacklogEntry, limit int) []wire.MemoryEntry {
	var games []backlogGame
	seen := make(map[uint32]bool)
	if owned != nil {
		games = backlogGames(owned, reading[gameList]{Data: recent}, marks)
		for _, g := range games {
			seen[g.AppID] = true
		}
	}
	if recent != nil {
		for _, g := range recent.Games {
			if !seen[g.AppID] {
				games = append(games, backlogGame{game: g, Status: marks[g.AppID].Status})
			}
		}
	}
	for i := range games {
		games[i].Name = d.memoryLabel(games[i].Name)
	}
	games = slices.DeleteFunc(games, func(g backlogGame) bool { return g.Name == "" })
	entries := make([]wire.MemoryEntry, 0)
	if d.config.exportsMemoryDimension(memoryPlaystyle) {
		entries = append(entries, d.playstyleEntries(t, games)...)
	}
	if d.config.exportsMemoryDimension(memoryBacklog) && marks != nil && (owned != nil && len(owned.Games) > 0 || len(marks) > 0) {
		entries = append(entries, backlogMemoryEntry(t, owned, marks))
	}
	if d.config.exportsMemoryDimension(memoryActiveGames) {
		entries = append(entries, activeMemoryEntries(t, games, limit)...)
	}
	if d.config.exportsMemoryDimension(memoryMilestones) {
		entries = append(entries, d.milestoneEntries(id, t, games, limit)...)
	}
	return boundMemoryEntries(entries, limit)
}

func (d *steamData) playstyleEntries(t *text, games []backlogGame) []wire.MemoryEntry {
	var genres map[string]observedGenres
	_ = d.memory.genres.view(func(entries map[string]observedGenres) { genres = maps.Clone(entries) })
	slices.SortFunc(games, func(a, b backlogGame) int {
		if byTime := cmp.Compare(b.PlaytimeForever, a.PlaytimeForever); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
	var favorites []string
	weights := make(map[string]uint64)
	now := d.library.now()
	for _, g := range games {
		if g.PlaytimeForever == 0 {
			break
		}
		if len(favorites) < 3 && !slices.Contains(favorites, g.Name) {
			favorites = append(favorites, g.Name)
		}
		cached := genres[appIDText(g.AppID)]
		if age := now.Sub(cached.at); age < 0 || age > maxStaleAge {
			continue
		}
		for _, genre := range cached.names {
			weights[genre] += uint64(g.PlaytimeForever)
		}
	}
	var entries []wire.MemoryEntry
	if len(favorites) > 0 {
		entries = append(entries, gamingMemoryEntry(memoryPlaystyle, "favorites", fmt.Sprintf(t.memory.favorites, strings.Join(favorites, t.memory.separator)), 960, true, favorites...))
	}
	if len(weights) > 0 {
		names := slices.SortedFunc(maps.Keys(weights), func(a, b string) int {
			if byWeight := cmp.Compare(weights[b], weights[a]); byWeight != 0 {
				return byWeight
			}
			return cmp.Compare(a, b)
		})
		names = names[:min(3, len(names))]
		entries = append(entries, gamingMemoryEntry(memoryPlaystyle, "genres", fmt.Sprintf(t.memory.genres, strings.Join(names, t.memory.separator)), 940, true, names...))
	}
	return entries
}

func backlogMemoryEntry(t *text, owned *gameList, marks map[uint32]BacklogEntry) wire.MemoryEntry {
	counts := make(map[string]int, len(recapBuckets))
	format := t.memory.backlog
	if owned == nil {
		format = t.memory.annotations
		for _, mark := range marks {
			counts[recapBucket(mark.Status, 0)]++
		}
	} else {
		for _, g := range owned.Games {
			counts[recapBucket(marks[g.AppID].Status, g.PlaytimeForever)]++
		}
	}
	text := fmt.Sprintf(format, counts[recapBacklog], counts[string(BacklogPlaying)], counts[string(BacklogCompleted)], counts[string(BacklogDropped)])
	return gamingMemoryEntry(memoryBacklog, "summary", text, 850, false)
}

func activeMemoryEntries(t *text, games []backlogGame, limit int) []wire.MemoryEntry {
	slices.SortFunc(games, func(a, b backlogGame) int {
		if (a.Status == BacklogPlaying) != (b.Status == BacklogPlaying) {
			if a.Status == BacklogPlaying {
				return -1
			}
			return 1
		}
		return compareRecent(a.game, b.game)
	})
	var entries []wire.MemoryEntry
	for _, g := range games {
		if g.Status == BacklogCompleted || g.Status == BacklogDropped || (g.Status != BacklogPlaying && g.Playtime2Weeks == 0) {
			continue
		}
		format := t.memory.recent
		if g.Status == BacklogPlaying {
			format = t.memory.playing
		}
		entries = append(entries, gamingMemoryEntry(memoryActiveGames, appIDText(g.AppID), fmt.Sprintf(format, g.Name), max(100, 820-10*len(entries)), false, g.Name))
		if len(entries) == limit {
			break
		}
	}
	return entries
}

func (d *steamData) milestoneEntries(id string, t *text, games []backlogGame, limit int) []wire.MemoryEntry {
	now := d.library.now()
	var progresses map[string]observedAchievement
	_ = d.memory.achievements.view(func(entries map[string]observedAchievement) { progresses = maps.Clone(entries) })
	scores := make(map[uint32]observedAchievement)
	for key, p := range progresses {
		if raw, ok := strings.CutPrefix(key, id+"/"); ok && p.nearComplete(now) {
			if appID, err := strconv.ParseUint(raw, 10, 32); err == nil {
				scores[uint32(appID)] = p
			}
		}
	}
	score := func(g backlogGame) int {
		level := 0
		if scores[g.AppID].total > 0 {
			level++
		}
		if g.Status == BacklogCompleted {
			level += 2
		}
		return level
	}
	slices.SortFunc(games, func(a, b backlogGame) int {
		if signal := cmp.Compare(score(b), score(a)); signal != 0 {
			return signal
		}
		if byTime := cmp.Compare(b.PlaytimeForever, a.PlaytimeForever); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
	var entries []wire.MemoryEntry
	for _, g := range games {
		var milestones []string
		if g.Status == BacklogCompleted {
			milestones = append(milestones, t.memory.completed)
		}
		if p := scores[g.AppID]; p.total > 0 {
			milestones = append(milestones, fmt.Sprintf(t.memory.achievements, p.unlocked, p.total))
		}
		if g.PlaytimeForever >= twentyHours {
			milestones = append(milestones, fmt.Sprintf(t.memory.hours, g.PlaytimeForever/60))
		}
		if len(milestones) == 0 {
			break
		}
		text := fmt.Sprintf(t.memory.milestone, g.Name, strings.Join(milestones, t.memory.separator))
		entries = append(entries, gamingMemoryEntry(memoryMilestones, appIDText(g.AppID), text, max(90, 815-10*len(entries)), false, g.Name))
		if len(entries) == limit {
			break
		}
	}
	return entries
}

func boundMemoryEntries(entries []wire.MemoryEntry, limit int) []wire.MemoryEntry {
	slices.SortFunc(entries, func(a, b wire.MemoryEntry) int {
		if a.Standing != b.Standing {
			if a.Standing {
				return -1
			}
			return 1
		}
		if order := cmp.Compare(b.Rank, a.Rank); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	remaining := standingByteBudget
	bounded := entries[:0]
	for _, e := range entries {
		if len(e.Text) <= remaining {
			bounded = append(bounded, e)
			remaining -= len(e.Text)
		}
		if len(bounded) == limit {
			break
		}
	}
	return bounded
}
