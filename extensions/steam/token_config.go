package main

import "slices"

// TokenConfig decodes the token-related settings from the flat Hub configuration.
// Callers use the gates and effective limits before starting work that consumes tokens.
type TokenConfig struct {
	InboxEventsEnabled       bool              `json:"inbox_events_enabled"`
	InboxMaxEventsPerDay     int               `json:"inbox_max_events_per_day"`
	PatchNotesSummaryEnabled bool              `json:"patch_notes_summary_enabled"`
	SaleRoundupEnabled       bool              `json:"sale_roundup_enabled"`
	MemoryExportEnabled      bool              `json:"memory_export_enabled"`
	MemoryExportDimensions   []memoryDimension `json:"memory_export_dimensions"`
	MemoryMaxItems           int               `json:"memory_max_items"`
	DiscoveryToolsEnabled    *bool             `json:"discovery_tools_enabled"`
	DiscoveryMaxResults      int               `json:"discovery_max_results"`
}

func (c TokenConfig) AllowsInboxEvents() bool { return c.InboxEventsEnabled }

func (c TokenConfig) AllowsPatchNotesSummary() bool { return c.PatchNotesSummaryEnabled }

// AllowsSaleRoundup gates the wishlist roundup delivered through the Inbox.
func (c TokenConfig) AllowsSaleRoundup() bool { return c.AllowsInboxEvents() && c.SaleRoundupEnabled }

func (c TokenConfig) AllowsMemoryExport() bool {
	return slices.ContainsFunc(memoryDimensions[:], c.exportsMemoryDimension)
}

func (c TokenConfig) exportsMemoryDimension(d memoryDimension) bool {
	return c.MemoryExportEnabled && (c.MemoryExportDimensions == nil || slices.Contains(c.MemoryExportDimensions, d))
}

// AllowsDiscoveryTools gates both advertising and invoking high-context discovery tools and skills.
func (c TokenConfig) AllowsDiscoveryTools() bool {
	return c.DiscoveryToolsEnabled == nil || *c.DiscoveryToolsEnabled
}

func (c TokenConfig) InboxEventLimit() int {
	return enabledLimit(c.AllowsInboxEvents(), c.InboxMaxEventsPerDay, 5, 50)
}

func (c TokenConfig) MemoryItemLimit() int {
	return enabledLimit(c.AllowsMemoryExport(), c.MemoryMaxItems, 20, 100)
}

func (c TokenConfig) DiscoveryResultLimit() int {
	return enabledLimit(c.AllowsDiscoveryTools(), c.DiscoveryMaxResults, 15, 50)
}

// A disabled feature has a zero budget. Missing and out-of-range limits use the documented default.
func enabledLimit(enabled bool, configured, fallback, maximum int) int {
	if !enabled {
		return 0
	}
	if configured < 1 || configured > maximum {
		return fallback
	}
	return configured
}
