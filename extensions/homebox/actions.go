package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
)

type createArgs struct {
	EntityTypeID string   `json:"entity_type_id"`
	LocationID   string   `json:"location_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Quantity     float64  `json:"quantity"`
	TagIDs       []string `json:"tag_ids,omitempty"`
}

// maxDescriptionChars is the longest item description: the validation, the tool schema and the page state check use it.
const maxDescriptionChars = 1000

// maxNameChars is the longest item name: the validation, the tool schema and the page state check use it.
const maxNameChars = 255

func (in createArgs) validate() error {
	if !validEntityID(in.EntityTypeID) || !validEntityID(in.LocationID) || strings.TrimSpace(in.Name) == "" || len([]rune(in.Name)) > maxNameChars || len([]rune(in.Description)) > maxDescriptionChars || math.IsNaN(in.Quantity) || math.IsInf(in.Quantity, 0) || in.Quantity <= 0 || in.Quantity > 1000000 || len(in.TagIDs) > 20 {
		return errors.New("valid type/location UUIDs, a bounded name/description, positive quantity and at most 20 tags are required")
	}
	if strings.ContainsFunc(in.Name, unicode.IsControl) || strings.ContainsFunc(in.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("item text contains unsupported control characters")
	}
	seen := map[string]bool{}
	for _, id := range in.TagIDs {
		if !validEntityID(id) || seen[id] {
			return errors.New("tag IDs must be unique UUIDs returned by Homebox")
		}
		seen[id] = true
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "homebox_create_item" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("Create Homebox item\nType: %s\nParent location: %s\nName: %s\nQuantity: %g", in.EntityTypeID, in.LocationID, in.Name, in.Quantity)
	if in.Description != "" {
		summary += "\nDescription: " + in.Description
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "homebox_create_item" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	keyData, _ := json.Marshal(in)
	text, err := a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
		item, err := a.client.createItem(ctx, in.EntityTypeID, in.LocationID, in.Name, in.Description, in.Quantity, in.TagIDs)
		if err != nil {
			return "", err
		}
		return "Created Homebox item " + item.ID + ": " + item.Name, nil
	})
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if a.hub != nil {
		_ = a.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
