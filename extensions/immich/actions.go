package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type favoriteArgs struct {
	AssetID  string `json:"asset_id"`
	Favorite *bool  `json:"favorite"`
}

func (in favoriteArgs) validate() error {
	if !validAssetID(in.AssetID) {
		return errors.New("asset ID must be a UUID returned by Immich")
	}
	if in.Favorite == nil {
		return errors.New("favorite is required")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "immich_set_favorite" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in favoriteArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": fmt.Sprintf("Update Immich asset %s\nFavorite: %v\nOnly isFavorite is changed", in.AssetID, *in.Favorite)}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "immich_set_favorite" {
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
		if err := a.client.favorite(ctx, in.AssetID, fav); err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated Immich asset %s; favorite: %v", in.AssetID, fav), nil
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
