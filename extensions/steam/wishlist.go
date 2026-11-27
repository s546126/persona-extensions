package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"sync"
)

const (
	maxWishlistItems = 1_000
	storeBatchSize   = 50
)

func (c steamClient) wishlist(ctx context.Context, id, country, language string) (*wishList, problem) {
	var reply struct {
		Response *struct {
			Items []wishItem `json:"items"`
		} `json:"response"`
	}
	switch p := c.get(ctx, apiBase+wishlistPath, url.Values{"steamid": {id}}, false, &reply); p {
	case "":
	case problemDenied, problemMissing, problemBadCall:
		return nil, problemWishlist
	default:
		return nil, p
	}
	if reply.Response == nil || reply.Response.Items == nil {
		return nil, problemWishlist
	}

	// The wishlist endpoint supplies app IDs and priority. Store data is read separately without credentials.
	items := make([]wishItem, 0, len(reply.Response.Items))
	seen := make(map[uint32]struct{}, len(reply.Response.Items))
	for _, item := range reply.Response.Items {
		if _, ok := seen[item.AppID]; item.AppID == 0 || ok {
			continue
		}
		seen[item.AppID] = struct{}{}
		items = append(items, wishItem{AppID: item.AppID, Priority: item.Priority})
	}
	slices.SortFunc(items, func(a, b wishItem) int {
		if order := cmp.Compare(a.Priority, b.Priority); order != 0 {
			return order
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
	list := &wishList{Total: len(items), Items: slices.Clone(items[:min(len(items), maxWishlistItems)])}
	ids := make([]uint32, len(list.Items))
	for i, item := range list.Items {
		ids[i] = item.AppID
	}

	var names map[uint32]string
	var prices map[uint32]*storePrice
	var priceProblem problem
	var reads sync.WaitGroup
	reads.Go(func() { names = c.storeNames(ctx, ids, country, language) })
	reads.Go(func() { prices, priceProblem = c.storePrices(ctx, ids, country) })
	reads.Wait()

	list.Checked = len(prices)
	for i := range list.Items {
		item := &list.Items[i]
		item.Name = names[item.AppID]
		item.Price, item.PriceChecked = prices[item.AppID]
		if item.Price != nil && item.Price.DiscountPercent > 0 {
			list.Discounted++
		}
	}
	return list, priceProblem
}

// StoreBrowse provides display names in batches. App IDs still identify unlisted or unavailable games.
func (c steamClient) storeNames(ctx context.Context, ids []uint32, country, language string) map[uint32]string {
	names := make(map[uint32]string, len(ids))
	type storeID struct {
		AppID uint32 `json:"appid"`
	}
	for batch := range slices.Chunk(ids, storeBatchSize) {
		apps := make([]storeID, 0, len(batch))
		for _, id := range batch {
			apps = append(apps, storeID{AppID: id})
		}
		query, _ := json.Marshal(map[string]any{
			"ids":     apps,
			"context": map[string]string{"country_code": country, "language": language},
		})
		var reply struct {
			Response struct {
				Items []struct {
					AppID   uint32 `json:"appid"`
					Name    string `json:"name"`
					Success int    `json:"success"`
				} `json:"store_items"`
			} `json:"response"`
		}
		if c.get(ctx, apiBase+storeNamesPath, url.Values{"input_json": {string(query)}}, false, &reply) != "" {
			break
		}
		for _, item := range reply.Response.Items {
			if item.Success == 1 && slices.Contains(batch, item.AppID) {
				names[item.AppID] = c.text(item.Name)
			}
		}
	}
	return names
}

// AppDetails supports multiple app IDs when asking for price_overview alone. A nil quote in the map means
// the Store replied for that app but did not list a price in the selected country.
func (c steamClient) storePrices(ctx context.Context, ids []uint32, country string) (map[uint32]*storePrice, problem) {
	prices := make(map[uint32]*storePrice, len(ids))
	var incomplete problem
	for batch := range slices.Chunk(ids, storeBatchSize) {
		apps := make([]string, 0, len(batch))
		for _, id := range batch {
			apps = append(apps, appIDText(id))
		}
		var reply map[string]struct {
			Success *bool           `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		params := url.Values{"appids": {strings.Join(apps, ",")}, "cc": {country}, "filters": {"price_overview"}}
		if p := c.get(ctx, storeBase+storePricesPath, params, false, &reply); p != "" {
			return prices, p
		}
		for _, id := range batch {
			item := reply[appIDText(id)]
			if item.Success == nil {
				incomplete = problemResponse
				continue
			}
			if !*item.Success {
				prices[id] = nil
				continue
			}
			price, valid := c.parsePrice(item.Data)
			if !valid {
				incomplete = problemResponse
				continue
			}
			prices[id] = price
		}
	}
	return prices, incomplete
}

func (c steamClient) parsePrice(data json.RawMessage) (*storePrice, bool) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[]")) {
		return nil, true
	}
	var reply *struct {
		Price *struct {
			Currency  string  `json:"currency"`
			Final     *uint32 `json:"final"`
			Formatted string  `json:"final_formatted"`
			Discount  int     `json:"discount_percent"`
		} `json:"price_overview"`
	}
	if json.Unmarshal(data, &reply) != nil || reply == nil {
		return nil, false
	}
	if reply.Price == nil {
		return nil, true
	}
	p := reply.Price
	if p.Final == nil || len(p.Currency) != 3 || strings.IndexFunc(p.Currency, func(r rune) bool {
		return r < 'A' || r > 'Z'
	}) >= 0 || p.Discount < 0 || p.Discount > 100 {
		return nil, false
	}
	return &storePrice{Currency: p.Currency, Final: *p.Final, FinalFormatted: c.text(p.Formatted), DiscountPercent: p.Discount}, true
}
