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

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Scope   string `json:"s,omitempty"`
	Query   string `json:"q,omitempty"`
	Page    int    `json:"p,omitempty"`
	Project string `json:"r,omitempty"`
	IID     int64  `json:"i,omitempty"`
	Mode    string `json:"m,omitempty"`
	DraftID string `json:"d,omitempty"`
	Body    string `json:"b,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Page < 0 || st.IID < 0 || len(st.Project) > 255 || len(st.DraftID) > 128 || len([]rune(st.Body)) > 10000 {
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
		st.Query, st.Page, st.Mode, st.DraftID, st.Body = clipText(r.Values["q"], 200), 0, "list", "", ""
	case "scope":
		if r.Values["view"] != "assigned_to_me" && r.Values["view"] != "reviews_for_me" {
			return nil, errors.New("invalid review scope")
		}
		st.Scope, st.Page, st.Mode, st.DraftID, st.Body = r.Values["view"], 0, "list", "", ""
	case "more":
		st.Page++
	case "detail":
		project, err := url.QueryUnescape(r.Values["project"])
		if err != nil || validProjectID(project) != nil {
			return nil, errors.New("invalid project ID")
		}
		iid, err := strconv.ParseInt(r.Values["iid"], 10, 64)
		if err != nil || iid <= 0 {
			return nil, errors.New("invalid merge request IID")
		}
		st.Project, st.IID, st.Mode, st.DraftID, st.Body = project, iid, "detail", "", ""
	case "back":
		st.Mode, st.IID, st.Project, st.DraftID, st.Body = "list", 0, "", "", ""
	case "comment":
		st.Mode, st.DraftID, st.Body = "comment", randomID(), ""
	case "review-comment":
		st.Body = r.Values["body"]
		if err := validComment(st.Body); err != nil {
			return nil, err
		}
		st.Mode = "review-comment"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("comment draft exceeds page state limit")
		}
	case "edit-comment":
		st.Mode = "comment"
	case "approve", "unapprove":
		if st.IID <= 0 || validProjectID(st.Project) != nil || strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("project, merge request IID and draft are required")
		}
		raw, _ := json.Marshal(reviewArgs{Project: st.Project, IID: st.IID})
		out, err := a.doAction(ctx, "ui-"+r.Action+":"+st.DraftID, "gitlab_"+r.Action, raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-comment":
		if st.IID <= 0 || validProjectID(st.Project) != nil || strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("project, merge request IID and draft are required")
		}
		raw, _ := json.Marshal(commentArgs{Project: st.Project, IID: st.IID, Body: st.Body})
		out, err := a.doAction(ctx, "ui-comment:"+st.DraftID, "gitlab_comment", raw)
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

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	c, _, _ := a.deps()
	if c.base == "" || c.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	switch st.Mode {
	case "detail", "comment", "review-comment":
		if st.IID <= 0 || validProjectID(st.Project) != nil {
			return nil, errors.New("project and merge request IID are required")
		}
		if st.Mode == "comment" {
			return commentView(r, st), nil
		}
		if st.Mode == "review-comment" {
			if err := validComment(st.Body); err != nil {
				return nil, err
			}
			return reviewCommentView(r, st), nil
		}
		item, notes, next, err := a.readDetail(ctx, st.Project, st.IID)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		st.Body = ""
		return detailView(r, st, item, notes, next), nil
	}
	if st.Scope == "" {
		st.Scope = "assigned_to_me"
	}
	if st.Scope != "assigned_to_me" && st.Scope != "reviews_for_me" {
		return nil, errors.New("invalid review scope")
	}
	if len([]rune(st.Query)) > 200 || strings.ContainsRune(st.Query, 0) {
		return nil, errors.New("invalid merge request search")
	}
	if st.Page < 1 {
		st.Page = 1
	}
	items, more, err := a.collectMergeRequests(ctx, st.Scope, st.Query, st.Page)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.reviews, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{
		Name: "view", Title: t.open, Value: st.Scope, Action: wire.Action{ID: "scope"},
		Choices: []wire.Choice{{Value: "assigned_to_me", Title: t.assigned}, {Value: "reviews_for_me", Title: t.review}},
	}}
	section := wire.Section{}
	seenKeys := make(map[string]bool, len(items))
	for _, item := range items {
		if item.ProjectID <= 0 || item.IID <= 0 {
			continue
		}
		key := mrRowKey(item)
		if seenKeys[key] {
			continue
		}
		seenKeys[key] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: key, Title: fmt.Sprintf("!%d %s", item.IID, clipText(item.Title, 1800)), Subtitle: clipText(item.Author.Name+" · "+item.References.Full+" · "+item.SourceBranch, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"iid": itoa(item.IID), "project": url.QueryEscape(itoa(item.ProjectID))}}})
	}
	section.Title = fmt.Sprintf(t.results, len(section.Items))
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noMRs})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func mrRowKey(item mergeRequest) string {
	if item.ID > 0 {
		return "mr-" + itoa(item.ID)
	}
	return fmt.Sprintf("mr-%d-%d", item.ProjectID, item.IID)
}

// maxViewMergeRequests keeps the accumulated list within the SDK's component limit.
const maxViewMergeRequests = 250

// collectMergeRequests reads pages 1 through page so that Load more extends the list instead of replacing it,
// and reports whether GitLab has more merge requests that the cap still leaves room to show.
func (a *app) collectMergeRequests(ctx context.Context, scope, query string, page int) ([]mergeRequest, bool, error) {
	c, _, _ := a.deps()
	var out []mergeRequest
	seen := map[string]bool{}
	more := false
	for p := 1; p <= page && len(out) < maxViewMergeRequests; p++ {
		batch, err := c.mergeRequests(ctx, scope, query, p)
		if err != nil {
			return nil, false, err
		}
		for _, item := range batch.Items {
			if item.ProjectID <= 0 || item.IID <= 0 {
				continue
			}
			key := mrRowKey(item)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			if len(out) >= maxViewMergeRequests {
				break
			}
		}
		nextPage, nerr := strconv.Atoi(strings.TrimSpace(batch.Next))
		more = len(batch.Items) > 0 && nerr == nil && nextPage > p
		if !more {
			break
		}
	}
	return out, more && len(out) < maxViewMergeRequests, nil
}

func detailView(r *ext.UIRequest, st pageState, item mergeRequest, notes []note, next ...string) *wire.View {
	t := textsFor(r.Locale)
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = fmt.Sprintf("MR !%d", item.IID)
	}
	shortTitle := clipText(title, 1800)
	items := []wire.Component{
		{Type: wire.ComponentText, Text: shortTitle},
		{Type: wire.ComponentText, Text: t.author + ": " + clipText(item.Author.Name+" · "+item.SourceBranch+" → "+item.TargetBranch, 1800)},
	}
	if safeHTTPSLink(item.WebURL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.openMR, URL: item.WebURL})
	}
	items = append(items,
		wire.Component{Type: wire.ComponentButton, Title: t.approve, Action: &wire.Action{ID: "approve", Confirm: t.approve + "\n" + shortTitle}},
		wire.Component{Type: wire.ComponentButton, Title: t.unapprove, Action: &wire.Action{ID: "unapprove", Confirm: t.unapprove + "\n" + shortTitle}},
		wire.Component{Type: wire.ComponentButton, Title: t.comment, Action: &wire.Action{ID: "comment", Push: &wire.Push{Title: t.comment, State: ext.EncodeState(pageState{Scope: st.Scope, Query: st.Query, Page: st.Page, Project: st.Project, IID: st.IID, Mode: "comment", DraftID: randomID()})}}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	v := &wire.View{Title: fmt.Sprintf("MR !%d", item.IID), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.reviews, Items: items}}}
	if item.Description != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: capComponents(textComponents(clipText(item.Description, 60000)), maxDescriptionItems)})
	}
	if len(item.Reviewers) > 0 {
		section := wire.Section{Title: t.reviewers}
		for _, reviewer := range limitUsers(item.Reviewers, 100) {
			name := strings.TrimSpace(reviewer.Name)
			uname := strings.TrimSpace(reviewer.Username)
			if name == "" {
				name = uname
			}
			if name == "" {
				continue
			}
			sub := ""
			if uname != "" {
				sub = "@" + clipText(uname, 1800)
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Title: clipText(name, 1800), Subtitle: sub})
		}
		if len(section.Items) > 0 {
			v.Sections = append(v.Sections, section)
		}
	}
	hasNext := len(next) > 0 && strings.TrimSpace(next[0]) != ""
	budget := maxViewMergeRequests - viewItemCount(v)
	if hasNext && budget > 0 {
		budget--
	}
	section := wire.Section{Title: t.comments}
	for _, noteItem := range notes {
		if noteItem.System || budget < 2 {
			continue
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(authorLabel(noteItem.Author), 1800), Style: wire.TextStyleSecondary})
		budget--
		content := textComponents(clipText(noteItem.Body, 12000))
		if len(content) > budget {
			content = content[:budget]
			content[len(content)-1].Text = clipText(content[len(content)-1].Text, 1700) + " […]"
		}
		section.Items = append(section.Items, content...)
		budget -= len(content)
	}
	if len(section.Items) == 0 && budget > 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: t.noComments, Style: wire.TextStyleSecondary})
	}
	if hasNext {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText,
			Text: "Discussion is incomplete; next notes page: " + clipText(strings.TrimSpace(next[0]), 100), Style: wire.TextStyleSecondary})
	}
	if len(section.Items) > 0 {
		v.Sections = append(v.Sections, section)
	}
	return v
}

// maxDescriptionItems leaves the reviewers and the discussion room in the 250 components a merge request view keeps.
const maxDescriptionItems = 100

// capComponents keeps the first n components and shows the cut on the last one.
func capComponents(items []wire.Component, n int) []wire.Component {
	if len(items) <= n {
		return items
	}
	items = items[:n]
	items[n-1].Text = clipText(items[n-1].Text, 1700) + " […]"
	return items
}

func authorLabel(user user) string {
	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = user.Username
	}
	if name == "" {
		name = "GitLab user"
	}
	if user.Username != "" {
		name += " (@" + user.Username + ")"
	}
	return name
}

func safeHTTPSLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && len([]rune(raw)) <= wire.StringMost
}

func limitUsers(users []user, limit int) []user {
	if len(users) > limit {
		return users[:limit]
	}
	return users
}

func viewItemCount(v *wire.View) int {
	n := 0
	for _, section := range v.Sections {
		n += len(section.Items)
	}
	return n
}

func commentView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	return &wire.View{Title: t.comment, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "body", Label: t.body, Value: clipText(st.Body, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.reviewComment, Action: &wire.Action{ID: "review-comment", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}
}

func reviewCommentView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	items := capComponents(textComponents(st.Body), maxViewMergeRequests-2)
	items = append(items,
		wire.Component{Type: wire.ComponentButton, Title: t.confirmComment, Action: &wire.Action{ID: "confirm-comment", Confirm: t.confirmComment + "\n" + fmt.Sprintf("MR !%d", st.IID)}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "edit-comment"}},
	)
	return &wire.View{Title: t.reviewComment, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.reviewComment, Items: items}}}
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
		}
	}
	if len(out) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentText, Text: " ", Style: wire.TextStyleSecondary})
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
