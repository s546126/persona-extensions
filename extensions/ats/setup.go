package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The home page before setup lets the user pick, on the page itself, the computer and the folder, then save them
// with one tap. The host lets no extension write its own settings (the settings form saves them, with the
// computer's confirmation, and the program on that computer reads the folder allowlist from them), so the picks
// are a draft the page shows back, and saving opens this extension's settings with the very values to set there.

// Action ids of the setup steps.
const (
	actSetupMachine = "setup-machine" // setup-machine:<computer id>
	actSetupRepo    = "setup-repo"    // setup-repo:<folder> for a suggestion; else the field's value {"repo": …}
	actSetupSave    = "setup-save"
)

// repoSuggestions are common folders offered as one-tap choices.
var repoSuggestions = []string{"~/code", "~/src"}

// setupDraft is what the user picked on the page and has not saved in the settings yet.
type setupDraft struct {
	Machine string
	Repo    string
}

// setupInfo is what the page needs before setup: the draft, the computers it can offer and whether the list came.
type setupInfo struct {
	draft    setupDraft
	machines []hubMachine // online computers ATS can work on, in the hub's order
	names    map[string]string
	listed   bool
}

// picked is the computer the page shows as chosen: the user's pick, or the only one there is.
func (s setupInfo) picked() string {
	if s.draft.Machine != "" && slices.ContainsFunc(s.machines, func(m hubMachine) bool { return m.ID == s.draft.Machine }) {
		return s.draft.Machine
	}
	if len(s.machines) == 1 {
		return s.machines[0].ID
	}
	return ""
}

func (s setupInfo) name(id string) string { return cmpOr(s.names[id], id) }

// usableMachine says whether ATS can work on a computer: one of the systems it has a program for, confirmed by its
// owner (trust "pending" is not). A computer whose system the hub does not know yet is offered; the settings form
// decides in the end.
func usableMachine(m hubMachine) bool {
	if strings.EqualFold(m.Trust, "pending") {
		return false
	}
	switch strings.ToLower(m.OS) {
	case "", "darwin", "macos", "mac", "linux":
		return true
	}
	return false
}

// setupInfo reads the computers for the page before setup. A hub that does not answer leaves the list empty and
// the page says so; it never blocks the page for long.
func (a *atsApp) setupInfo(ctx context.Context) setupInfo {
	a.mu.Lock()
	info := setupInfo{draft: a.draft, names: map[string]string{}}
	api := a.api
	a.mu.Unlock()
	if api == nil {
		return info
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ms, err := api.Machines(cctx)
	if err != nil {
		return info
	}
	info.listed = true
	for _, m := range ms {
		info.names[m.ID] = m.Name
		if m.Online && usableMachine(m) {
			info.machines = append(info.machines, m)
		}
	}
	return info
}

// cleanRepo checks a folder typed on the page: an absolute path or one under ~, on one line.
func cleanRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", fmt.Errorf("先写一个文件夹，比如 ~/code")
	case len(s) > 512 || strings.ContainsAny(s, "\r\n\t"):
		return "", fmt.Errorf("这个文件夹写得不对，写一行就好，比如 ~/code")
	case !strings.HasPrefix(s, "/") && s != "~" && !strings.HasPrefix(s, "~/"):
		return "", fmt.Errorf("要写完整的文件夹，以 ~/ 或 / 开头，比如 ~/code")
	}
	return s, nil
}

// setupAction handles a setup step's button; ok is false for an action that is not one.
func (a *atsApp) setupAction(kind, target string, values map[string]string) (res *wire.ActionResult, ok bool) {
	switch kind {
	case actSetupMachine:
		if strings.TrimSpace(target) == "" {
			return &wire.ActionResult{Toast: "没选上：这台电脑没有名字，刷新一下再选。"}, true
		}
		a.mu.Lock()
		a.draft.Machine = target
		// The button sends the page's field too: a folder typed but not yet taken is kept, not wiped by the redraw.
		res := &wire.ActionResult{}
		if typed := strings.TrimSpace(values["repo"]); typed != "" && typed != a.draft.Repo {
			if repo, err := cleanRepo(typed); err == nil {
				a.draft.Repo = repo
			} else {
				res.Toast = "电脑选好了；文件夹没记下：" + err.Error()
			}
		}
		a.mu.Unlock()
		return res, true
	case actSetupRepo:
		// A suggestion carries its folder in the action id (an app before UI version 4 sends no action values);
		// the field's button sends the field.
		typed := target
		if typed == "" {
			typed = values["repo"]
		}
		repo, err := cleanRepo(typed)
		if err != nil {
			return &wire.ActionResult{Toast: err.Error()}, true
		}
		a.mu.Lock()
		a.draft.Repo = repo
		a.mu.Unlock()
		return &wire.ActionResult{}, true
	case actSetupSave:
		return &wire.ActionResult{OpenURL: "persona://settings/extensions/" + url.PathEscape(a.id)}, true
	}
	return nil, false
}

// setupSections are the steps before setup: pick a computer, pick a folder, save. A step already in the settings
// is marked done.
func setupSections(cfg ExtConfig, info setupInfo) []wire.Section {
	done := &wire.Badge{Text: "做好了", Tone: wire.TonePositive}
	var out []wire.Section

	// Step 1: the computer.
	var step1 []wire.Component
	picked := info.picked()
	switch {
	case len(cfg.Machines) > 0:
		names := make([]string, 0, len(cfg.Machines))
		for _, id := range cfg.Machines {
			names = append(names, info.name(id))
		}
		step1 = append(step1, wire.Component{Type: wire.ComponentRow, Key: "setup-machines", Title: truncateRunes("在 "+strings.Join(names, "、")+" 上干活", detailTextMost), Badge: done})
	case len(info.machines) == 0:
		text := "现在没有在线的苹果电脑或 Linux 电脑：打开要用的那台电脑上的 Persona，再刷新这一页。"
		if !info.listed {
			text = "还没读到你的电脑，过几秒刷新一下这一页。"
		}
		step1 = append(step1, wire.Component{Type: wire.ComponentText, Key: "setup-machines", Text: text, Tone: wire.ToneCaution})
	default:
		for _, m := range info.machines {
			name := truncateRunes(cmpOr(m.Name, m.ID), 40)
			if m.ID == picked {
				step1 = append(step1, wire.Component{Type: wire.ComponentRow, Key: "setup-machine-" + m.ID, Title: name, Subtitle: osWords(m.OS),
					Badge: &wire.Badge{Text: "就用这台", Tone: wire.TonePositive}})
				continue
			}
			step1 = append(step1, wire.Component{Type: wire.ComponentButton, Key: "setup-machine-" + m.ID, Title: "用这台：" + name,
				Action: &wire.Action{ID: actSetupMachine + ":" + m.ID, Submit: true}})
		}
		if len(info.machines) == 1 {
			step1 = append(step1, wire.Component{Type: wire.ComponentText, Key: "setup-machine-note", Style: wire.TextStyleSecondary,
				Text: "只有这一台在线，先帮你选了它。"})
		}
	}
	out = append(out, wire.Section{Title: "第 1 步：选一台干活的电脑", Items: step1})

	// Step 2: the folder.
	var step2 []wire.Component
	if len(cfg.Repos) > 0 {
		step2 = append(step2, wire.Component{Type: wire.ComponentRow, Key: "setup-repos", Title: truncateRunes("能动 "+strings.Join(cfg.Repos, "、"), detailTextMost), Badge: done})
	} else {
		step2 = append(step2,
			wire.Component{Type: wire.ComponentField, Kind: wire.FieldKindText, Key: "setup-repo", Name: "repo", Label: "项目所在的文件夹",
				Value: info.draft.Repo, Placeholder: "~/code"},
			wire.Component{Type: wire.ComponentText, Key: "setup-repo-why", Style: wire.TextStyleSecondary,
				Text: "它只动这个文件夹里的项目，别处的文件碰不到。"},
		)
		for _, s := range repoSuggestions {
			if s == info.draft.Repo {
				continue
			}
			step2 = append(step2, wire.Component{Type: wire.ComponentButton, Key: "setup-repo-" + strings.TrimPrefix(s, "~/"), Title: "用 " + s,
				Action: &wire.Action{ID: actSetupRepo + ":" + s}})
		}
		step2 = append(step2, wire.Component{Type: wire.ComponentButton, Key: "setup-repo-use", Title: "用这个文件夹",
			Action: &wire.Action{ID: actSetupRepo, Submit: true}})
	}
	out = append(out, wire.Section{Title: "第 2 步：告诉它能动哪个文件夹", Items: step2})

	// Step 3: save in the settings, with the values to set there.
	var todo []string
	if len(cfg.Machines) == 0 {
		if picked != "" {
			todo = append(todo, "打开「"+info.name(picked)+"」")
		} else {
			todo = append(todo, "打开要用的电脑")
		}
	}
	if len(cfg.Repos) == 0 {
		if info.draft.Repo != "" {
			todo = append(todo, "文件夹填「"+info.draft.Repo+"」")
		} else {
			todo = append(todo, "填上文件夹")
		}
	}
	out = append(out, wire.Section{Title: "第 3 步：保存", Items: []wire.Component{
		{Type: wire.ComponentText, Key: "setup-next", Text: truncateRunes("在设置里"+strings.Join(todo, "，")+"，点保存；回来这页就能用了。", detailTextMost)},
		{Type: wire.ComponentButton, Key: "setup-save", Title: "去设置里保存", Action: &wire.Action{ID: actSetupSave}},
	}, Footer: "Persona 不让扩展自己改设置，所以最后要你点一下保存（在「" + settingsPath + "」）。"})
	return out
}

// osWords names a computer's system for the setup list.
func osWords(os string) string {
	switch os {
	case "darwin":
		return "苹果电脑"
	case "linux":
		return "Linux 电脑"
	}
	return ""
}
