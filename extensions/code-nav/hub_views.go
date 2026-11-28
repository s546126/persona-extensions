package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Hub UI renderers for the three v2 workflows:
//  1. Visual Onboarding / Setup UI (machine selection, code root management, live probing & suggestions)
//  2. Per-Machine Language Server Probe & Config UI (status, toggle, custom path, idle eviction policy)
//  3. Git Diff View & Click-to-Jump-to-Source UI (unstaged/staged/branch diff + clickable hunks/lines)

const (
	maxDiffListRows = 120
	maxDiffJumpRows = 80
)

func (a *hubApp) setupSections(ctx context.Context, r *ext.UIRequest, t text, s *state, enabled []string, forceProbe bool, note string) []wire.Section {
	ps := a.getStore().get()
	var sections []wire.Section

	intro := wire.Section{Key: "setup-intro-sec"}
	intro.Items = append(intro.Items,
		wire.Component{Type: wire.ComponentText, Key: "setup-title", Style: wire.TextStyleHeadline, Text: clip(t.setupHeadline, headlineMax)},
		wire.Component{Type: wire.ComponentText, Key: "setup-desc", Style: wire.TextStyleSecondary, Text: clip(t.setupIntro, 2000)},
	)
	if note != "" {
		intro.Items = append(intro.Items, wire.Component{Type: wire.ComponentText, Key: "setup-note", Style: wire.TextStyleCaption, Text: clip(note, 2000)})
	}
	sections = append(sections, intro)

	// Step 1: Target machines (Hub local + configured nodes + discovered paired machines).
	machSec := wire.Section{Key: "setup-machines", Title: clip(t.setupStepMachines, 200)}
	hubBadge := &wire.Badge{Text: t.setupBadgeDisabled, Tone: wire.ToneNeutral}
	if ps.UseHub {
		hubBadge = &wire.Badge{Text: t.setupBadgeEnabled, Tone: wire.TonePositive}
	}
	hubRow := wire.Component{
		Type:     wire.ComponentRow,
		Key:      "setup-m-hub",
		Title:    t.hubMachine,
		Subtitle: t.setupHubDesc,
		Icon:     wire.NavIconHome,
		Badge:    hubBadge,
		Action:   &wire.Action{ID: "toggle_hub"},
	}
	if r.UIVersion >= 6 {
		hubRow.Control = &wire.RowControl{
			Kind:   wire.RowControlKindSwitch,
			Name:   "hub_enabled",
			On:     ps.UseHub,
			Action: wire.Action{ID: "toggle_hub"},
		}
	}
	machSec.Items = append(machSec.Items, hubRow)

	if !ps.UseHub {
		machSec.Items = append(machSec.Items, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "setup-hub-btn",
			Title:  t.setupHubEnable,
			Action: &wire.Action{ID: "toggle_hub", Values: map[string]string{"hub_enabled": "true"}},
		})
	}

	remoteConfigured := a.configuredRemotes()
	for i, m := range remoteConfigured {
		if i >= 30 {
			break
		}
		badge := &wire.Badge{Text: t.setupBadgeEnabled, Tone: wire.TonePositive}
		machSec.Items = append(machSec.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("setup-node:%d", i),
			Title:    clip(m, 200),
			Subtitle: t.setupNodeEnabled,
			Icon:     wire.NavIconCode,
			Badge:    badge,
			Action:   &wire.Action{ID: "machine", Values: map[string]string{"machine": m}},
		})
	}

	// Optional discovery of paired computers not yet authorized in extension settings.
	if paired := a.pairedMachines(ctx); len(paired) > 0 {
		shown := 0
		for _, pm := range paired {
			if pm.ID == "" || slices.Contains(remoteConfigured, pm.ID) || shown >= 20 {
				continue
			}
			label := pm.Name
			if label == "" {
				label = pm.ID
			}
			stBadge := &wire.Badge{Text: t.setupOffline, Tone: wire.ToneNeutral}
			if pm.Online {
				stBadge = &wire.Badge{Text: t.setupOnline, Tone: wire.TonePositive}
			}
			sub := t.setupNodePairedHint
			if pm.OS != "" {
				sub = pm.OS + " · " + sub
			}
			machSec.Items = append(machSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("setup-paired:%d", shown),
				Title:    clip(label, 200),
				Subtitle: clip(sub, 300),
				Icon:     wire.NavIconWifi,
				Badge:    stBadge,
			})
			shown++
		}
	}
	sections = append(sections, machSec)

	// Step 2: Code root directories on the active target machine.
	target := s.M
	if target == "" && len(enabled) > 0 {
		target = enabled[0]
	}
	if target == "" {
		emptySec := wire.Section{Key: "setup-wait", Items: []wire.Component{{
			Type:   wire.ComponentEmpty,
			Key:    "setup-no-target",
			Title:  t.setupNoTarget,
			Detail: t.setupNoTargetHint,
			Icon:   wire.NavIconSettings,
			Button: &wire.EmptyButton{
				Title:  t.setupHubEnable,
				Action: wire.Action{ID: "toggle_hub", Values: map[string]string{"hub_enabled": "true"}},
			},
		}}}
		return append(sections, emptySec)
	}

	var pr probeResult
	probeErr := a.callMachine(ctx, target, "probe", map[string]any{"force": forceProbe}, &pr)
	rootSec := wire.Section{Key: "setup-roots", Title: clip(fmt.Sprintf(t.setupStepRoots, a.machineLabel(t, target)), 200)}
	if probeErr != nil {
		rootSec.Items = append(rootSec.Items, errorEmpty("setup-probe-err", t.noComputer, probeErr))
		sections = append(sections, rootSec)
		return sections
	}

	if len(pr.Roots) == 0 {
		rootSec.Items = append(rootSec.Items, wire.Component{
			Type:  wire.ComponentText,
			Key:   "setup-roots-empty",
			Style: wire.TextStyleSecondary,
			Text:  t.setupRootEmpty,
		})
	} else {
		for i, rp := range pr.Roots {
			if i >= maxRootsLimit {
				break
			}
			sub, badge := rootStatusDisplay(t, rp)
			row := wire.Component{
				Type:     wire.ComponentRow,
				Key:      fmt.Sprintf("setup-root:%d", i),
				Title:    clip(rp.Input, 300),
				Subtitle: clip(sub, 300),
				Icon:     wire.NavIconFolder,
				Badge:    badge,
			}
			if r.UIVersion >= 6 {
				row.Actions = []wire.RowAction{{
					Title:  t.setupRootRemove,
					Role:   "destructive",
					Action: wire.Action{ID: "remove_root", Confirm: t.setupRootRemoveConfirm, Values: map[string]string{"root": rp.Input}},
				}}
			} else {
				row.Action = &wire.Action{ID: "remove_root", Confirm: t.setupRootRemoveConfirm, Values: map[string]string{"root": rp.Input}}
			}
			rootSec.Items = append(rootSec.Items, row)
		}
	}

	// Quick-add detected folder suggestions.
	if len(pr.Suggestions) > 0 {
		rootSec.Items = append(rootSec.Items, wire.Component{
			Type:  wire.ComponentText,
			Key:   "setup-sug-hint",
			Style: wire.TextStyleCaption,
			Text:  t.setupRootSuggestions,
		})
		for i, sug := range pr.Suggestions {
			if i >= 6 {
				break
			}
			rootSec.Items = append(rootSec.Items, wire.Component{
				Type:   wire.ComponentButton,
				Key:    fmt.Sprintf("setup-sug:%d", i),
				Title:  clip(fmt.Sprintf(t.setupAddSuggestion, sug.Path, sug.Repos), 80),
				Action: &wire.Action{ID: "add_root", Values: map[string]string{"root_path": sug.Path}},
			})
		}
	}

	rootSec.Items = append(rootSec.Items,
		wire.Component{
			Type:        wire.ComponentField,
			Key:         "setup-root-field",
			Name:        "root_path",
			Label:       t.setupRootLabel,
			Kind:        wire.FieldKindText,
			Placeholder: t.setupRootPlaceholder,
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "setup-root-add",
			Title:  t.setupRootAdd,
			Action: &wire.Action{ID: "add_root", Submit: true},
		},
	)
	sections = append(sections, rootSec)

	// Step 3: Live summary stats + next steps.
	readyLSPs := 0
	for _, lp := range pr.Languages {
		if lp.Status == "ready" {
			readyLSPs++
		}
	}
	repoTone := wire.ToneNeutral
	if pr.TotalRepos > 0 {
		repoTone = wire.TonePositive
	}
	lspTone := wire.ToneCaution
	if readyLSPs > 0 {
		lspTone = wire.TonePositive
	}
	nextSec := wire.Section{Key: "setup-next", Title: clip(t.setupStepNext, 200)}
	nextSec.Items = append(nextSec.Items,
		wire.Component{
			Type: wire.ComponentStatGrid,
			Key:  "setup-stats",
			Stats: []wire.Stat{
				{Label: t.setupStatMachines, Value: strconv.Itoa(len(enabled)), Tone: wire.TonePositive},
				{Label: t.setupStatRoots, Value: strconv.Itoa(len(pr.Roots))},
				{Label: t.setupStatRepos, Value: strconv.Itoa(pr.TotalRepos), Tone: repoTone},
				{Label: t.setupStatLSP, Value: fmt.Sprintf("%d/%d", readyLSPs, len(pr.Languages)), Tone: lspTone},
			},
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "setup-reprobe",
			Title:  t.setupReprobe,
			Action: &wire.Action{ID: "probe_refresh"},
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "setup-to-lsp",
			Title:  t.setupToLSP,
			Action: &wire.Action{ID: "tab", Values: map[string]string{"tab": "lsp"}},
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "setup-finish",
			Title:  t.setupFinish,
			Action: &wire.Action{ID: "finish_setup"},
		},
	)
	sections = append(sections, nextSec)
	return sections
}

func rootStatusDisplay(t text, rp rootProbe) (string, *wire.Badge) {
	resolved := ""
	if rp.Resolved != "" && rp.Resolved != rp.Input {
		resolved = rp.Resolved + " · "
	}
	switch rp.Status {
	case "ok":
		tone := wire.ToneNeutral
		if rp.Repos > 0 {
			tone = wire.TonePositive
		}
		return resolved + fmt.Sprintf(t.setupRootOK, rp.Repos), &wire.Badge{Text: fmt.Sprintf(t.setupRootReposCount, rp.Repos), Tone: tone}
	case "permission_denied":
		return resolved + t.setupRootPerm, &wire.Badge{Text: t.setupBadgeDenied, Tone: wire.ToneDanger}
	case "forbidden":
		return resolved + t.setupRootForbidden, &wire.Badge{Text: t.setupBadgeBlocked, Tone: wire.ToneDanger}
	case "not_dir":
		return resolved + t.setupRootNotDir, &wire.Badge{Text: t.setupBadgeMissing, Tone: wire.ToneCaution}
	default:
		return resolved + t.setupRootMissing, &wire.Badge{Text: t.setupBadgeMissing, Tone: wire.ToneCaution}
	}
}

func (a *hubApp) lspSections(ctx context.Context, r *ext.UIRequest, t text, s *state, enabled []string, forceProbe bool, note string) []wire.Section {
	if len(enabled) == 0 || s.M == "" {
		return []wire.Section{{Key: "lsp-empty-sec", Items: []wire.Component{{
			Type:   wire.ComponentEmpty,
			Key:    "lsp-no-machine",
			Title:  t.lspNotReadyEmpty,
			Detail: t.lspNotReadyHint,
			Icon:   wire.NavIconSettings,
			Button: &wire.EmptyButton{
				Title:  t.setupOpenBtn,
				Action: wire.Action{ID: "tab", Values: map[string]string{"tab": "setup"}},
			},
		}}}}
	}

	var pr probeResult
	if err := a.callMachine(ctx, s.M, "probe", map[string]any{"force": forceProbe}, &pr); err != nil {
		return []wire.Section{{Key: "lsp-err-sec", Items: []wire.Component{errorEmpty("lsp-probe-err", t.noComputer, err)}}}
	}

	readyCount, runningTotal := 0, 0
	for _, lp := range pr.Languages {
		if lp.Status == "ready" {
			readyCount++
		}
		runningTotal += lp.Running
	}

	head := wire.Section{Key: "lsp-head"}
	title := fmt.Sprintf(t.lspHeadline, a.machineLabel(t, s.M))
	if pr.OS != "" {
		title += " · " + pr.OS
	}
	head.Items = append(head.Items,
		wire.Component{Type: wire.ComponentText, Key: "lsp-title", Style: wire.TextStyleHeadline, Text: clip(title, headlineMax)},
		wire.Component{Type: wire.ComponentText, Key: "lsp-sub", Style: wire.TextStyleSecondary, Text: clip(t.lspSubtitle, 2000)},
		wire.Component{
			Type: wire.ComponentStatGrid,
			Key:  "lsp-stats",
			Stats: []wire.Stat{
				{Label: t.lspStatReady, Value: fmt.Sprintf("%d/%d", readyCount, len(pr.Languages)), Tone: toneIf(readyCount > 0, wire.TonePositive, wire.ToneCaution)},
				{Label: t.lspStatRunning, Value: strconv.Itoa(runningTotal)},
				{Label: t.lspStatIdle, Value: fmt.Sprintf(t.lspIdleUnit, max(1, pr.IdleMinutes))},
				{Label: t.lspStatRepos, Value: strconv.Itoa(pr.TotalRepos)},
			},
		},
		wire.Component{Type: wire.ComponentButton, Key: "lsp-reprobe", Title: t.lspReprobe, Action: &wire.Action{ID: "probe_refresh"}},
	)
	if runningTotal > 0 {
		head.Items = append(head.Items, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "lsp-reap",
			Title:  t.lspReapBtn,
			Action: &wire.Action{ID: "reap_lsp"},
		})
	}
	if note != "" {
		head.Items = append(head.Items, wire.Component{Type: wire.ComponentText, Key: "lsp-note", Style: wire.TextStyleCaption, Text: clip(note, 2000)})
	}

	// Language status list with inline toggle switches (UI >= 6) and row tap to edit custom path.
	selectedLang := s.E
	if !isManagedLang(selectedLang) {
		selectedLang = "go"
	}
	var selectedProbe lspProbe
	listSec := wire.Section{Key: "lsp-list", Title: clip(t.lspSectionList, 200)}
	for _, lp := range pr.Languages {
		if lp.Lang == selectedLang {
			selectedProbe = lp
		}
		sub, badge := lspProbeDisplay(t, lp)
		row := wire.Component{
			Type:     wire.ComponentRow,
			Key:      "lsp-row:" + lp.Lang,
			Title:    clip(languageGroupTitle(lp.Lang), 200),
			Subtitle: clip(sub, 300),
			Icon:     wire.NavIconCode,
			Badge:    badge,
			Action:   &wire.Action{ID: "select_lang", Values: map[string]string{"lang": lp.Lang}},
		}
		if r.UIVersion >= 6 {
			row.Control = &wire.RowControl{
				Kind:   wire.RowControlKindSwitch,
				Name:   "lang_en_" + lp.Lang,
				On:     lp.Enabled,
				Action: wire.Action{ID: "toggle_lang", Values: map[string]string{"lang": lp.Lang}},
			}
		}
		listSec.Items = append(listSec.Items, row)
	}
	if selectedProbe.Lang == "" && len(pr.Languages) > 0 {
		selectedProbe = pr.Languages[0]
		selectedLang = selectedProbe.Lang
	}

	// Selected language editor section.
	editSec := wire.Section{Key: "lsp-edit", Title: clip(fmt.Sprintf(t.lspSectionEdit, languageGroupTitle(selectedLang)), 200)}
	infoParts := []string{}
	if selectedProbe.CustomPath != "" {
		infoParts = append(infoParts, t.lspCustomTag+": "+selectedProbe.CustomPath)
	} else {
		infoParts = append(infoParts, t.lspAutoTag)
	}
	if selectedProbe.Binary != "" {
		infoParts = append(infoParts, selectedProbe.Binary)
	}
	if selectedProbe.Version != "" {
		infoParts = append(infoParts, selectedProbe.Version)
	}
	if selectedProbe.Error != "" {
		infoParts = append(infoParts, selectedProbe.Error)
	}
	editSec.Items = append(editSec.Items, wire.Component{
		Type:  wire.ComponentText,
		Key:   "lsp-edit-info",
		Style: wire.TextStyleSecondary,
		Text:  clip(strings.Join(infoParts, " · "), 2000),
	})
	if hint := t.install[selectedLang]; hint != "" {
		editSec.Items = append(editSec.Items, wire.Component{
			Type:  wire.ComponentText,
			Key:   "lsp-edit-install",
			Style: wire.TextStyleCaption,
			Text:  clip(hint, 2000),
		})
	}
	editSec.Items = append(editSec.Items,
		wire.Component{
			Type:        wire.ComponentField,
			Key:         "lsp-path-field",
			Name:        "lsp_path",
			Label:       t.lspPathLabel,
			Kind:        wire.FieldKindText,
			Value:       clip(selectedProbe.CustomPath, 512),
			Placeholder: t.lspPathPlaceholder,
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "lsp-save-btn",
			Title:  t.lspSaveBtn,
			Action: &wire.Action{ID: "save_lang", Submit: true, Values: map[string]string{"lang": selectedLang}},
		},
	)
	if selectedProbe.CustomPath != "" {
		editSec.Items = append(editSec.Items, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "lsp-reset-btn",
			Title:  t.lspResetBtn,
			Action: &wire.Action{ID: "reset_lang", Values: map[string]string{"lang": selectedLang}},
		})
	}
	toggleTitle := t.lspDisableBtn
	nextOn := "false"
	if !selectedProbe.Enabled {
		toggleTitle = t.lspEnableBtn
		nextOn = "true"
	}
	editSec.Items = append(editSec.Items, wire.Component{
		Type:   wire.ComponentButton,
		Key:    "lsp-toggle-btn",
		Title:  toggleTitle,
		Action: &wire.Action{ID: "toggle_lang", Values: map[string]string{"lang": selectedLang, "enabled": nextOn}},
	})

	// Idle eviction policy section.
	idleSec := wire.Section{Key: "lsp-idle", Title: clip(t.lspSectionIdle, 200)}
	idleSec.Items = append(idleSec.Items,
		wire.Component{
			Type:        wire.ComponentField,
			Key:         "lsp-idle-field",
			Name:        "idle_minutes",
			Label:       t.lspIdleLabel,
			Kind:        wire.FieldKindNumber,
			Value:       strconv.Itoa(max(1, pr.IdleMinutes)),
			Placeholder: "10",
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "lsp-idle-save",
			Title:  t.lspIdleSaveBtn,
			Action: &wire.Action{ID: "save_idle", Submit: true},
		},
	)

	return []wire.Section{head, listSec, editSec, idleSec}
}

func lspProbeDisplay(t text, lp lspProbe) (string, *wire.Badge) {
	switch lp.Status {
	case "ready":
		var parts []string
		if lp.Version != "" {
			parts = append(parts, lp.Version)
		}
		if lp.Binary != "" {
			parts = append(parts, lp.Binary)
		}
		if lp.Running > 0 {
			parts = append(parts, fmt.Sprintf(t.lspRunningTag, lp.Running))
		}
		return strings.Join(parts, " · "), &wire.Badge{Text: t.lspBadgeReady, Tone: wire.TonePositive}
	case "disabled":
		sub := t.lspBadgeDisabled
		if lp.Binary != "" {
			sub += " · " + lp.Binary
		}
		return sub, &wire.Badge{Text: t.lspBadgeDisabled, Tone: wire.ToneNeutral}
	case "invalid_path":
		sub := lp.CustomPath
		if lp.Error != "" {
			sub += " · " + lp.Error
		}
		return sub, &wire.Badge{Text: t.lspBadgeBad, Tone: wire.ToneDanger}
	default:
		hint := t.install[lp.Lang]
		if hint == "" {
			hint = t.lspNoInstallHint
		}
		return hint, &wire.Badge{Text: t.lspBadgeMissing, Tone: wire.ToneCaution}
	}
}

func toneIf(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func diffModeControl(t text, s *state) wire.Component {
	mode := normalizeDiffMode(s.DM)
	return wire.Component{
		Type:   wire.ComponentSegmented,
		Key:    "diff-mode",
		Name:   "diff_mode",
		Value:  mode,
		Action: &wire.Action{ID: "diff_mode"},
		Choices: []wire.Choice{
			{Value: diffModeUnstaged, Title: t.diffModeUnstaged},
			{Value: diffModeStaged, Title: t.diffModeStaged},
			{Value: diffModeBranch, Title: t.diffModeBranch},
		},
	}
}

func diffStatusBadge(t text, status string) *wire.Badge {
	switch status {
	case wire.FileStatusAdded:
		return &wire.Badge{Text: t.diffBadgeAdded, Tone: wire.TonePositive}
	case wire.FileStatusDeleted:
		return &wire.Badge{Text: t.diffBadgeDeleted, Tone: wire.ToneDanger}
	case wire.FileStatusRenamed:
		return &wire.Badge{Text: t.diffBadgeRename, Tone: wire.ToneNeutral}
	default:
		return &wire.Badge{Text: t.diffBadgeModified, Tone: wire.ToneCaution}
	}
}

// diffView renders either the repository's changed-files list or one file's diff + clickable hunk/line jump targets.
func (a *hubApp) diffView(t text, r *ext.UIRequest, s *state, nc func(string, any, any) error) []wire.Component {
	mode := normalizeDiffMode(s.DM)
	out := []wire.Component{diffModeControl(t, s)}
	if s.P != "" {
		out = append(out, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "diff-back-code",
			Title:  t.backToCode,
			Action: &wire.Action{ID: "close_list"},
		})
	}

	if s.DF != "" {
		return append(out, a.diffFileDetail(t, s, mode, nc)...)
	}

	var st diffStatusResult
	if err := nc("diff_status", map[string]any{"repo": s.R, "mode": mode}, &st); err != nil {
		return append(out, errorEmpty("diff-status-err", t.diffError, err))
	}
	if !st.IsGit {
		return append(out, wire.Component{
			Type:   wire.ComponentEmpty,
			Key:    "diff-not-git",
			Title:  t.diffNotGit,
			Detail: t.diffNotGitHint,
			Icon:   wire.NavIconBranch,
		})
	}
	if st.NoBase {
		return append(out, wire.Component{
			Type:   wire.ComponentEmpty,
			Key:    "diff-no-base",
			Title:  t.diffNoBase,
			Detail: t.diffNoBaseHint,
			Icon:   wire.NavIconBranch,
		})
	}

	summaryParts := []string{}
	if st.Branch != "" {
		summaryParts = append(summaryParts, fmt.Sprintf(t.diffSummaryBranch, st.Branch))
	}
	if st.BaseRef != "" {
		summaryParts = append(summaryParts, fmt.Sprintf(t.diffSummaryBase, st.BaseRef))
	}
	summaryParts = append(summaryParts, fmt.Sprintf(t.diffSummaryFiles, len(st.Files), st.TotalAdd, st.TotalDel))
	out = append(out, wire.Component{
		Type:  wire.ComponentText,
		Key:   "diff-summary",
		Style: wire.TextStyleSecondary,
		Text:  clip(strings.Join(summaryParts, " · "), 2000),
	})

	if len(st.Files) == 0 {
		return append(out, wire.Component{
			Type:   wire.ComponentEmpty,
			Key:    "diff-empty",
			Title:  t.diffEmpty,
			Detail: t.diffEmptyHint,
			Icon:   wire.NavIconCheck,
		})
	}

	for i, f := range st.Files {
		if i >= maxDiffListRows {
			break
		}
		var subParts []string
		if f.Binary {
			subParts = append(subParts, t.diffBinaryFile)
		} else {
			subParts = append(subParts, fmt.Sprintf("+%d -%d", f.Additions, f.Deletions))
		}
		if f.Untracked {
			subParts = append(subParts, t.diffUntracked)
		}
		if f.OldPath != "" && f.OldPath != f.Path {
			subParts = append(subParts, fmt.Sprintf(t.diffRenamedFrom, f.OldPath))
		}
		row := wire.Component{
			Type:     wire.ComponentRow,
			Key:      fmt.Sprintf("diff-f:%d", i),
			Title:    clip(f.Path, 300),
			Subtitle: clip(strings.Join(subParts, " · "), 300),
			Icon:     wire.NavIconFile,
			Badge:    diffStatusBadge(t, f.Status),
			Action:   &wire.Action{ID: "diff_file", Values: map[string]string{"path": f.Path}},
		}
		if r.UIVersion >= 6 && i < maxMenus {
			acts := []wire.RowAction{{
				Title:  t.diffActionView,
				Action: wire.Action{ID: "diff_file", Values: map[string]string{"path": f.Path}},
			}}
			if f.Status != wire.FileStatusDeleted && !f.Binary {
				acts = append(acts, wire.RowAction{
					Title: t.diffActionJump,
					Action: wire.Action{ID: "diff_jump", Values: map[string]string{
						"path": f.Path,
						"line": strconv.Itoa(max(1, f.FirstLine)),
					}},
				})
			}
			row.Actions = acts
		}
		out = append(out, row)
	}
	return out
}

func (a *hubApp) diffFileDetail(t text, s *state, mode string, nc func(string, any, any) error) []wire.Component {
	var df diffFileResult
	if err := nc("diff_file", map[string]any{"repo": s.R, "mode": mode, "path": s.DF}, &df); err != nil {
		return []wire.Component{
			{Type: wire.ComponentButton, Key: "diff-back-list", Title: t.diffBackList, Action: &wire.Action{ID: "diff_list"}},
			errorEmpty("diff-file-err", t.diffError, err),
		}
	}

	out := []wire.Component{
		{Type: wire.ComponentButton, Key: "diff-back-list", Title: t.diffBackList, Action: &wire.Action{ID: "diff_list"}},
	}

	canJump := df.AbsPath != "" && df.Status != wire.FileStatusDeleted && !df.Binary
	firstLine, firstCol := 1, 1
	if len(df.Hunks) > 0 {
		firstLine = max(1, df.Hunks[0].JumpLine)
		firstCol = max(1, df.Hunks[0].JumpCol)
	}
	if canJump {
		out = append(out, wire.Component{
			Type:  wire.ComponentButton,
			Key:   "diff-open-src",
			Title: t.diffOpenSource,
			Action: &wire.Action{ID: "diff_jump", Values: map[string]string{
				"path": df.Path,
				"line": strconv.Itoa(firstLine),
				"col":  strconv.Itoa(firstCol),
			}},
		})
	} else if df.Status == wire.FileStatusDeleted {
		out = append(out, wire.Component{
			Type:  wire.ComponentText,
			Key:   "diff-deleted-note",
			Style: wire.TextStyleCaption,
			Text:  t.diffDeletedNoSrc,
		})
	}

	if df.Truncated {
		out = append(out, wire.Component{
			Type:  wire.ComponentText,
			Key:   "diff-trunc-note",
			Style: wire.TextStyleCaption,
			Text:  t.diffTruncated,
		})
	}

	// Native DiffComponent for visual reading of hunks.
	dc := wire.Component{
		Type:      wire.ComponentDiff,
		Key:       "diff-comp",
		Path:      clip(df.Path, 4096),
		Status:    df.Status,
		Language:  df.Language,
		Additions: df.Additions,
		Deletions: df.Deletions,
		Binary:    df.Binary,
	}
	if df.OldPath != "" && df.OldPath != df.Path {
		dc.OldPath = clip(df.OldPath, 4096)
	}
	if !df.Binary && len(df.Hunks) > 0 {
		totalLines := 0
		for _, h := range df.Hunks {
			if len(dc.Hunks) >= maxDiffHunks || totalLines >= maxDiffLines {
				break
			}
			var safeLines []string
			for _, ln := range h.Lines {
				if totalLines >= maxDiffLines {
					break
				}
				if ln != "" && !strings.ContainsAny(ln[:1], " +-\\") {
					ln = " " + ln
				}
				safeLines = append(safeLines, cutRunes(ln, maxDiffLineBytes))
				totalLines++
			}
			dc.Hunks = append(dc.Hunks, wire.Hunk{
				Header:   clip(h.Header, 2000),
				OldStart: max(0, h.OldStart),
				NewStart: max(0, h.NewStart),
				Lines:    safeLines,
			})
		}
	}
	out = append(out, dc)

	// Clickable hunk & modified-line jump rows so tapping any changed location jumps directly into full source code.
	if canJump && len(df.Hunks) > 0 {
		out = append(out, wire.Component{
			Type:  wire.ComponentText,
			Key:   "diff-jump-head",
			Style: wire.TextStyleHeadline,
			Text:  clip(t.diffJumpTitle, headlineMax),
		})
		rows := 0
		for hi, h := range df.Hunks {
			if rows >= maxDiffJumpRows {
				break
			}
			hunkMeta := fmt.Sprintf("%s · +%d -%d", h.Header, h.Added, h.Deleted)
			if len(h.Changed) > 0 {
				for li, cl := range h.Changed {
					if rows >= maxDiffJumpRows {
						break
					}
					badge := &wire.Badge{Text: t.diffHunkBadgeAdd, Tone: wire.TonePositive}
					if cl.Kind == "-" {
						badge = &wire.Badge{Text: t.diffHunkBadgeDel, Tone: wire.ToneCaution}
					}
					out = append(out, wire.Component{
						Type:     wire.ComponentRow,
						Key:      fmt.Sprintf("diff-h:%d:%d", hi, li),
						Title:    clip(fmt.Sprintf(t.diffJumpLineFmt, cl.Line, cl.Preview), 200),
						Subtitle: clip(hunkMeta, 300),
						Icon:     wire.NavIconCode,
						Badge:    badge,
						Action: &wire.Action{ID: "diff_jump", Values: map[string]string{
							"path": df.Path,
							"line": strconv.Itoa(max(1, cl.Line)),
							"col":  strconv.Itoa(max(1, cl.Col)),
						}},
					})
					rows++
				}
			} else {
				preview := h.Preview
				if preview == "" {
					preview = h.Header
				}
				out = append(out, wire.Component{
					Type:     wire.ComponentRow,
					Key:      fmt.Sprintf("diff-h:%d", hi),
					Title:    clip(fmt.Sprintf(t.diffJumpLineFmt, max(1, h.JumpLine), preview), 200),
					Subtitle: clip(hunkMeta, 300),
					Icon:     wire.NavIconCode,
					Badge:    &wire.Badge{Text: t.diffHunkBadgeAdd, Tone: wire.TonePositive},
					Action: &wire.Action{ID: "diff_jump", Values: map[string]string{
						"path": df.Path,
						"line": strconv.Itoa(max(1, h.JumpLine)),
						"col":  strconv.Itoa(max(1, h.JumpCol)),
					}},
				})
				rows++
			}
		}
	}
	return out
}
