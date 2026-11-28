package main

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	backlogSummaryTool = "steam_backlog_summary"
	backlogMarkTool    = "steam_backlog_mark"
	tonightPickTool    = "steam_tonight_pick"

	neverPlayed  = "never_played"
	triedUnder2h = "tried_under_2h"
	longTerm     = "long_term"
	twoHours     = 120
	twentyHours  = 1200

	moodAny      = "any"
	moodQuick    = "quick_session"
	moodContinue = "continue_playing"
	moodBacklog  = "clear_backlog"
	moodHidden   = "hidden_gem"

	defaultTonightMinutes = 60
	maxTonightMinutes     = 24 * 60
	defaultTonightLimit   = 5
	maxTonightLimit       = 20

	problemBacklogStore problem = "backlog_store_unavailable"
	backlogStatusSchema         = `"type":"string","enum":["unplayed","playing","queued","paused","completed","dropped"]`
)

var backlogBuckets = [...]string{
	neverPlayed, triedUnder2h, string(BacklogPlaying), longTerm,
	string(BacklogQueued), string(BacklogPaused), string(BacklogCompleted), string(BacklogDropped),
}

var backlogTools = []wire.ConnectorTool{
	{
		Name: backlogSummaryTool,
		Description: "Count Steam library games by playtime, recent activity and local backlog status. Counts can overlap. " +
			"Pass a bucket, status or pagination to list titles. Use next_offset for the next page.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + pageProperties + `,
			"bucket":{"type":"string","enum":["never_played","tried_under_2h","playing","long_term","queued","paused","completed","dropped"]},
			"status":{` + backlogStatusSchema + `}}}`),
	},
	{
		Name: backlogMarkTool,
		Description: "Save a personal backlog annotation on this device for one Steam AppID. Set a status and optionally update the note and tags. " +
			"Use clear:true alone with the AppID to remove the annotation. Omitted note and tags are kept.",
		InputSchema: json.RawMessage(`{"type":"object","required":["appid"],"properties":{
			"appid":{"type":"integer","minimum":1,"maximum":4294967295},
			"status":{` + backlogStatusSchema + `},
			"note":{"type":"string","description":"Up to 240 characters. An empty string clears the note."},
			"tags":{"type":"array","description":"Up to eight nonblank tags of up to 32 characters each. An empty array clears the tags.","items":{"type":"string"}},
			"clear":{"type":"boolean","default":false}}}`),
	},
	{
		Name: tonightPickTool,
		Description: "Rank owned Steam games for the time and mood you have tonight. Optionally filter by local tag or title. " +
			"Reasons use your playtime and local status.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"available_minutes":{"type":"integer","minimum":1,"maximum":1440,"default":60},
			"mood":{"type":"string","enum":["any","quick_session","continue_playing","clear_backlog","hidden_gem"],"default":"any"},
			"tag":{"type":"string","description":"Exact tag, up to 32 characters."},
			"query":{"type":"string","description":"Title substring, up to 100 characters."},
			"include_completed":{"type":"boolean","default":false},
			"limit":{"type":"integer","minimum":1,"maximum":20,"default":5}}}`),
	},
}

type backlogGame struct {
	game
	Status BacklogStatus `json:"status,omitempty"`
}

// Use the dedicated recent list when it covers the whole two-week window.
// With a partial or unavailable recent list, retain library minutes for missing titles.
func backlogGames(owned *gameList, recent reading[gameList], entries map[uint32]BacklogEntry) []backlogGame {
	minutes := make(map[uint32]uint32)
	complete := recent.Data != nil && !recent.Stale && !recent.Data.Truncated
	if recent.Data != nil {
		for _, g := range recent.Data.Games {
			minutes[g.AppID] = g.Playtime2Weeks
		}
	}
	games := make([]backlogGame, 0, len(owned.Games))
	for _, g := range owned.Games {
		if played, ok := minutes[g.AppID]; ok || complete {
			g.Playtime2Weeks = played
		}
		games = append(games, backlogGame{game: g, Status: entries[g.AppID].Status})
	}
	return games
}

func (g backlogGame) inBucket(bucket string) bool {
	switch bucket {
	case neverPlayed:
		return g.PlaytimeForever == 0 && g.Status != BacklogCompleted && g.Status != BacklogDropped
	case triedUnder2h:
		return g.PlaytimeForever > 0 && g.PlaytimeForever < twoHours
	case string(BacklogPlaying):
		return g.Status == BacklogPlaying || g.Playtime2Weeks > 0
	case longTerm:
		return g.PlaytimeForever >= twentyHours
	default:
		return string(g.Status) == bucket
	}
}

func backlogCounts(games []backlogGame) map[string]int {
	counts := make(map[string]int, len(backlogBuckets))
	for _, bucket := range backlogBuckets {
		counts[bucket] = 0
		for _, g := range games {
			if g.inBucket(bucket) {
				counts[bucket]++
			}
		}
	}
	return counts
}

type backlogSummaryArgs struct {
	Offset *int          `json:"offset"`
	Limit  *int          `json:"limit"`
	Bucket string        `json:"bucket"`
	Status BacklogStatus `json:"status"`
}

func (args backlogSummaryArgs) matches(games []backlogGame) []backlogGame {
	matches := make([]backlogGame, 0, len(games))
	for _, g := range games {
		if (args.Bucket == "" || g.inBucket(args.Bucket)) && (args.Status == "" || g.Status == args.Status) {
			matches = append(matches, g)
		}
	}
	slices.SortFunc(matches, func(a, b backlogGame) int { return compareName(a.game, b.game) })
	return matches
}

type backlogMarkArgs struct {
	AppID  uint32         `json:"appid"`
	Status *BacklogStatus `json:"status"`
	Note   *string        `json:"note"`
	Tags   *[]string      `json:"tags"`
	Clear  bool           `json:"clear"`
}

func (args backlogMarkArgs) apply(entry BacklogEntry) BacklogEntry {
	entry.Status = *args.Status
	if args.Note != nil {
		entry.Note = *args.Note
	}
	if args.Tags != nil {
		entry.Tags = *args.Tags
	}
	return entry
}

func (args backlogMarkArgs) validate() error {
	if args.AppID == 0 {
		return invalid("Provide a positive integer Steam AppID.")
	}
	if args.Clear {
		if args.Status != nil || args.Note != nil || args.Tags != nil {
			return invalid("Use clear:true with only the AppID.")
		}
		return nil
	}
	if args.Status == nil {
		return invalid("Provide a backlog status or clear:true.")
	}
	if err := validateBacklogEntry(args.apply(BacklogEntry{})); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func (d *steamData) backlogMarkID(ctx context.Context) (string, problem) {
	if d.setup != "" {
		return "", d.setup
	}
	if id := d.config.SteamID; validSteamID(id) {
		return id, ""
	}
	// A vanity name can change owners. Resolve it when writing to a SteamID.
	r := d.resolveVanity(ctx, true)
	if r.Issue != "" {
		return "", r.Issue
	}
	return *r.Data, ""
}

func (a *app) markBacklog(ctx context.Context, d *steamData, id string, args backlogMarkArgs) (*BacklogEntry, problem) {
	var result *BacklogEntry
	var issue problem
	err := a.backlog.edit(id, args.AppID, func(entry BacklogEntry) (*BacklogEntry, error) {
		// Check after acquiring the writer lock to reject edits delayed past QR sign-out.
		if issue = d.sessionIssue(); issue != "" {
			return nil, context.Canceled
		}
		if err := ctx.Err(); err != nil {
			issue = sanitizeError(err)
			return nil, err
		}
		if !args.Clear {
			next := args.apply(entry)
			result = &next
		}
		return result, nil
	})
	if issue != "" {
		return nil, issue
	}
	if err != nil {
		return nil, problemBacklogStore
	}
	return result, ""
}

func (a *app) backlogEntries(d *steamData, p reading[player]) (map[uint32]BacklogEntry, problem) {
	id, issue := d.readID(p)
	if issue != "" {
		return nil, issue
	}
	entries, err := a.backlog.List(id)
	if err != nil {
		return nil, problemBacklogStore
	}
	return entries, ""
}

func (a *app) readBacklog(ctx context.Context, d *steamData, t *text, out map[string]any) ([]backlogGame, map[uint32]BacklogEntry, bool) {
	p := d.toolAccount(ctx, out, t)
	owned := d.ownedGames(ctx, p, false)
	describeReading(out, "library", owned, t)
	if owned.Data == nil {
		return nil, nil, false
	}
	entries, issue := a.backlogEntries(d, p)
	if issue != "" {
		describeReading(out, "backlog", reading[BacklogEntry]{Issue: issue}, t)
		return nil, nil, false
	}
	var recent reading[gameList]
	if len(owned.Data.Games) > 0 {
		recent = d.recentGames(ctx, p, false)
		describeReading(out, "recent_games", recent, t)
	}
	out["available"], out["truncated"] = true, owned.Data.Truncated
	out["owned_game_count"] = owned.Data.Total
	return backlogGames(owned.Data, recent, entries), entries, true
}

func (a *app) callBacklogTool(ctx context.Context, d *steamData, t *text, name string, raw json.RawMessage, out map[string]any) error {
	switch name {
	case backlogSummaryTool:
		var args backlogSummaryArgs
		if !decodeArgs(raw, &args) ||
			args.Bucket != "" && !slices.Contains(backlogBuckets[:], args.Bucket) ||
			args.Status != "" && !slices.Contains(backlogStatuses[:], args.Status) {
			return invalid("Use a supported backlog bucket or status and bounded pagination.")
		}
		page := listArgs{Limit: args.Limit}
		if args.Offset != nil {
			page.Offset = *args.Offset
		}
		if !page.valid() {
			return invalid("Use an offset from 0 to 20000 and a limit from 1 to 50.")
		}
		games, _, ok := a.readBacklog(ctx, d, t, out)
		if ok {
			out["counts"], out["analyzed_game_count"] = backlogCounts(games), len(games)
			if args.Bucket != "" || args.Status != "" || args.Offset != nil || args.Limit != nil {
				setPage(out, "games", args.matches(games), page)
			}
		}
	case backlogMarkTool:
		var args backlogMarkArgs
		if !utf8.Valid(raw) || !decodeArgs(raw, &args) {
			return invalid("Invalid Steam backlog annotation.")
		}
		if err := args.validate(); err != nil {
			return err
		}
		id, issue := d.backlogMarkID(ctx)
		var entry *BacklogEntry
		if issue == "" {
			entry, issue = a.markBacklog(ctx, d, id, args)
		}
		if issue != "" {
			describeReading(out, "backlog", reading[BacklogEntry]{Issue: issue}, t)
			return nil
		}
		out["available"], out["appid"], out["annotation"] = true, args.AppID, entry
	case tonightPickTool:
		args := tonightArgs{AvailableMinutes: defaultTonightMinutes, Mood: moodAny, Limit: defaultTonightLimit}
		if !decodeArgs(raw, &args) || !args.valid() {
			return invalid("Use a supported mood, 1–1440 available minutes, at most 32 characters for tag and 100 for query, and a limit of 1–20.")
		}
		games, entries, ok := a.readBacklog(ctx, d, t, out)
		if ok {
			out["available_minutes"], out["mood"] = args.AvailableMinutes, args.Mood
			out["picks"] = tonightPicks(games, entries, args, &t.backlog)
		}
	}
	return nil
}

type tonightArgs struct {
	AvailableMinutes int    `json:"available_minutes"`
	Mood             string `json:"mood"`
	Tag              string `json:"tag"`
	Query            string `json:"query"`
	IncludeCompleted bool   `json:"include_completed"`
	Limit            int    `json:"limit"`
}

func (args tonightArgs) valid() bool {
	switch args.Mood {
	case moodAny, moodQuick, moodContinue, moodBacklog, moodHidden:
	default:
		return false
	}
	return args.AvailableMinutes >= 1 && args.AvailableMinutes <= maxTonightMinutes &&
		args.Limit >= 1 && args.Limit <= maxTonightLimit &&
		(args.Tag == "" || strings.TrimSpace(args.Tag) != "") &&
		utf8.RuneCountInString(args.Tag) <= maxBacklogTagRunes && utf8.RuneCountInString(args.Query) <= maxQueryRunes
}

type tonightPick struct {
	backlogGame
	Reason string `json:"reason"`
	score  int
}

func tonightPicks(games []backlogGame, entries map[uint32]BacklogEntry, args tonightArgs, t *backlogText) []tonightPick {
	tag := strings.TrimSpace(args.Tag)
	query := strings.ToLower(strings.TrimSpace(args.Query))
	picks := make([]tonightPick, 0, min(len(games), args.Limit))
	for _, g := range games {
		if g.Status == BacklogDropped || (g.Status == BacklogCompleted && !args.IncludeCompleted) ||
			!strings.Contains(strings.ToLower(g.Name), query) {
			continue
		}
		if tag != "" && !slices.ContainsFunc(entries[g.AppID].Tags, func(value string) bool {
			return strings.EqualFold(strings.TrimSpace(value), tag)
		}) {
			continue
		}
		score, reason := scoreTonight(g, args, t)
		picks = append(picks, tonightPick{backlogGame: g, Reason: reason, score: score})
	}
	slices.SortFunc(picks, func(a, b tonightPick) int {
		if byScore := cmp.Compare(b.score, a.score); byScore != 0 {
			return byScore
		}
		return compareName(a.game, b.game)
	})
	return picks[:min(len(picks), args.Limit)]
}

func scoreTonight(g backlogGame, args tonightArgs, t *backlogText) (int, string) {
	score, strongest, reason := 0, 0, t.pickFamiliar
	add := func(weight int, signal string) {
		score += weight
		if weight > strongest {
			strongest, reason = weight, signal
		}
	}
	active := g.Playtime2Weeks > 0
	unplayed, tried := g.inBucket(neverPlayed), g.inBucket(triedUnder2h)
	recorded := g.PlaytimeForever
	if active {
		recorded = g.Playtime2Weeks
		add(24+int(min(recorded, 60))/10, t.pickRecent)
	}
	// Steam reports aggregate minutes. This signal favors games with a small observed time commitment.
	fits := recorded > 0 && recorded <= uint32(args.AvailableMinutes)
	if fits {
		add(12, t.pickTime)
	}
	switch g.Status {
	case BacklogPlaying:
		add(36, t.pickPlaying)
	case BacklogQueued:
		add(30, t.pickQueued)
	case BacklogPaused:
		add(8, t.pickPaused)
	}
	switch {
	case unplayed:
		add(18, t.pickUnplayed)
	case tried:
		add(14, t.pickTried)
	case g.inBucket(longTerm):
		add(10, t.pickLongTerm)
	}

	switch args.Mood {
	case moodQuick:
		if fits {
			add(64, t.pickTime)
		}
	case moodContinue:
		if g.Status == BacklogPlaying {
			add(80, t.pickPlaying)
		} else if active {
			add(50, t.pickRecent)
		}
	case moodBacklog:
		if g.Status == BacklogQueued {
			add(75, t.pickQueued)
		} else if unplayed {
			add(50, t.pickUnplayed)
		}
	case moodHidden:
		if tried && !active {
			add(85, t.pickTried)
		}
	}
	if g.Status == BacklogCompleted {
		return score - 60, t.pickCompleted
	}
	return score, reason
}
