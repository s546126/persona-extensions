package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Project string `json:"p,omitempty"`
	Repo    string `json:"r,omitempty"`
	Query   string `json:"q,omitempty"`
	Start   int    `json:"s,omitempty"`
	PRID    int64  `json:"i,omitempty"`
	Mode    string `json:"m,omitempty"`
	DraftID string `json:"d,omitempty"`
}

const maxQueryChars = 200

func validMode(mode string) bool {
	switch mode {
	case "", "list", "detail":
		return true
	default:
		return false
	}
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || utf8.RuneCountInString(st.Query) > maxQueryChars || st.Start < 0 || st.PRID < 0 || len(st.Project) > 128 || len(st.Repo) > 128 || len(st.DraftID) > 128 {
		return pageState{}, errors.New("invalid page state")
	}
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, r, st)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	switch r.Action {
	case "search":
		st.Query, st.Start, st.Mode = clipText(r.Values["q"], maxQueryChars), 0, "list"
	case "scope":
		if err := validProjectRepo(r.Values["project"], r.Values["repo"]); err != nil {
			return nil, err
		}
		st.Project, st.Repo, st.Start, st.Mode = r.Values["project"], r.Values["repo"], 0, "list"
	case "more":
		st.Start += 50
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid PR ID")
		}
		st.PRID, st.Mode = id, "detail"
	case "back":
		st.Mode, st.PRID, st.DraftID = "list", 0, ""
	case "approve", "unapprove":
		if st.PRID <= 0 || strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("PR ID and draft ID are required")
		}
		raw := []byte(`{"project":` + quote(st.Project) + `,"repo":` + quote(st.Repo) + `,"id":` + strconv.FormatInt(st.PRID, 10) + `}`)
		out, err := a.doAction(ctx, "ui-"+r.Action+":"+st.DraftID, "bitbucket_"+r.Action, raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

// maxViewPullRequests keeps the accumulated list within the SDK's component limit.
const maxViewPullRequests = 250

// collectPullRequests reads pages from start 0 through start so that Load more extends the list instead of
// replacing it, and reports whether Bitbucket has more pull requests that the cap still leaves room to show.
func (a *app) collectPullRequests(ctx context.Context, project, repo string, start int) ([]pullRequest, bool, error) {
	var out []pullRequest
	more := false
	for pos := 0; pos <= start && len(out) < maxViewPullRequests; pos += 50 {
		page, err := a.client.pullRequests(ctx, project, repo, pos)
		if err != nil {
			return nil, false, err
		}
		out = append(out, page.Values...)
		more = !page.IsLastPage && page.NextPageStart != nil
		if !more {
			break
		}
	}
	if len(out) > maxViewPullRequests {
		out = out[:maxViewPullRequests]
	}
	return out, more && len(out) < maxViewPullRequests, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "detail" {
		if st.Project == "" || st.Repo == "" {
			return nil, errors.New("project and repository are required for PR details")
		}
		item, err := a.client.pullRequest(ctx, st.Project, st.Repo, st.PRID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		activities, err := a.client.activities(ctx, st.Project, st.Repo, st.PRID)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item, activities), nil
	}
	items, more, err := a.collectPullRequests(ctx, st.Project, st.Repo, st.Start)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.reviews, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(items))}
	seen := map[int64]bool{}
	for _, item := range items {
		if item.ID <= 0 || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if st.Query != "" && !strings.Contains(strings.ToLower(item.Title), strings.ToLower(st.Query)) {
			continue
		}
		if len(section.Items) >= maxViewPullRequests {
			break
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "pr-" + strconv.FormatInt(item.ID, 10), Title: clipText(fmt.Sprintf("#%d %s", item.ID, item.Title), 1800), Subtitle: clipText(item.Author.User.DisplayName+" · "+item.FromRef.DisplayID, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": strconv.FormatInt(item.ID, 10)}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noPRs})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	if utf8.RuneCountInString(raw) > wire.StringMost {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item pullRequest, activities []activity) *wire.View {
	t := textsFor(r.Locale)
	var items []wire.Component
	if title := clipText(strings.TrimSpace(item.Title), 1800); title != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: title})
	}
	items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.author+": "+item.Author.User.DisplayName+" · "+item.FromRef.DisplayID+" → "+item.ToRef.DisplayID, 1800)})
	if len(item.Links.Self) > 0 && httpsLink(item.Links.Self[0].Href) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.openPR, URL: item.Links.Self[0].Href})
	}
	items = append(items,
		wire.Component{Type: wire.ComponentButton, Title: t.approve, Action: &wire.Action{ID: "approve", Confirm: clipText(t.approve+"\n"+item.Title, wire.StringMost)}},
		wire.Component{Type: wire.ComponentButton, Title: t.unapprove, Action: &wire.Action{ID: "unapprove", Confirm: clipText(t.unapprove+"\n"+item.Title, wire.StringMost)}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	v := &wire.View{Title: fmt.Sprintf("PR #%d", item.ID), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.reviews, Items: items}}}
	used := len(items)
	if item.Description != "" {
		description := capComponents(textComponents(item.Description), maxDescriptionItems)
		used += len(description)
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: description})
	}
	if len(item.Reviewers) > 0 {
		section := wire.Section{Title: t.reviewers}
		for _, reviewer := range item.Reviewers {
			name := strings.TrimSpace(reviewer.User.DisplayName)
			if name == "" {
				name = strings.TrimSpace(reviewer.User.Name)
			}
			if name == "" {
				name = t.reviewers
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Title: clipText(name, 1800), Subtitle: clipText(fmt.Sprintf("approved=%v · %s", reviewer.Approved, reviewer.Status), 1800)})
		}
		section.Items = section.Items[:min(len(section.Items), maxReviewerItems)]
		used += len(section.Items)
		v.Sections = append(v.Sections, section)
	}
	if len(activities) > 0 {
		section := wire.Section{Title: t.comments}
		for _, activity := range activities {
			if activity.Comment.Text == "" {
				continue
			}
			author := strings.TrimSpace(activity.Comment.Author.DisplayName)
			if author == "" {
				author = strings.TrimSpace(activity.Comment.Author.Name)
			}
			if author != "" {
				section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(author, 1800), Style: wire.TextStyleSecondary})
			}
			section.Items = append(section.Items, textComponents(activity.Comment.Text)...)
		}
		if len(section.Items) > 0 {
			section.Items = capComponents(section.Items, maxViewItems-used)
			v.Sections = append(v.Sections, section)
		}
	}
	return v
}

const (
	// maxViewItems keeps the pull request view within the SDK's 300 component limit, whatever the pull request holds.
	maxViewItems = 250
	// maxDescriptionItems and maxReviewerItems leave the comments room when the description or the reviewers are many.
	maxDescriptionItems = 100
	maxReviewerItems    = 20
)

// capComponents keeps the first n components and shows the cut on the last one.
func capComponents(items []wire.Component, n int) []wire.Component {
	if len(items) <= n {
		return items
	}
	items = items[:n]
	items[n-1].Text = clipText(items[n-1].Text, 1700) + " […]"
	return items
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 2000)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
		}
	}
	return out
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func quote(text string) string {
	out, _ := json.Marshal(text)
	return string(out)
}
