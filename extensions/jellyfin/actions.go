package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type favoriteArgs struct {
	ItemID   string `json:"item_id"`
	Favorite *bool  `json:"favorite"`
}

func (in favoriteArgs) validate() error {
	if !validMediaID(in.ItemID) {
		return errors.New("item ID must be a Jellyfin media UUID")
	}
	if in.Favorite == nil {
		return errors.New("favorite is required")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "jellyfin_set_favorite" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in favoriteArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": fmt.Sprintf("Update Jellyfin item %s\nFavorite: %v", in.ItemID, *in.Favorite)}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "jellyfin_set_favorite" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in favoriteArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	fav := *in.Favorite
	keyData, _ := json.Marshal(in)
	text, err := a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
		me, err := a.client.currentUser(ctx)
		if err != nil {
			return "", err
		}
		if err := a.client.favorite(ctx, me.ID, in.ItemID, fav); err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated Jellyfin item %s; favorite: %v", in.ItemID, fav), nil
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
