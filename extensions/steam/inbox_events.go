package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	wishlistPollInterval = 6 * time.Hour
	wishlistDeliveryTTL  = 24 * time.Hour
	roundupMinDeals      = 3
	roundupTopDeals      = 5
	defaultDiscount      = 50
	maxTargetRules       = 20
	maxTargetText        = 640

	wishlistPriceKind    = "wishlist_price"
	wishlistGiveawayKind = "wishlist_giveaway"
	wishlistReleaseKind  = "wishlist_release"
	wishlistDemoKind     = "wishlist_demo"
	wishlistRoundupKind  = "wishlist_sale_roundup"
	wishlistDealsName    = "steam_wishlist_deals"
)

var wishlistDealsTool = wire.ConnectorTool{
	Name:        wishlistDealsName,
	Description: "Read current Steam wishlist discounts, target prices met, local price lows and limited-time giveaways in the configured region.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50,"default":20}}}`),
}

type wishlistStoreItem struct {
	AppID      uint32 `json:"appid"`
	Name       string `json:"-"`
	IsFree     bool   `json:"is_free"`
	ComingSoon *bool  `json:"is_coming_soon"`
	Release    *struct {
		ComingSoon *bool  `json:"is_coming_soon"`
		SteamDate  uint32 `json:"steam_release_date"`
		IsPreload  bool   `json:"is_preload"`
		Limited    bool   `json:"limited_launch_active"`
	} `json:"release"`
	Related struct {
		Demos         []wishlistDemo `json:"demos"`
		Standalone    []wishlistDemo `json:"standalone_demos"`
		DemoIDs       []uint32       `json:"demo_appid"`
		StandaloneIDs []uint32       `json:"standalone_demo_appid"`
	} `json:"related_items"`
	FreeWeekend *struct {
		Start uint32 `json:"start_time"`
		End   uint32 `json:"end_time"`
	} `json:"free_weekend"`
	BestPurchase struct {
		IsFreeToKeep  bool        `json:"is_free_to_keep"`
		FreeToKeepEnd uint32      `json:"free_to_keep_ends"`
		Final         json.Number `json:"final_price_in_cents"`
		Discount      int         `json:"discount_pct"`
		Discounts     []struct {
			End uint32 `json:"discount_end_date"`
		} `json:"active_discounts"`
	} `json:"best_purchase_option"`
}

type wishlistDemo struct {
	AppID uint32 `json:"appid"`
}

func (s wishlistStoreItem) releaseState(at time.Time) (state string, date uint32) {
	comingSoon := s.ComingSoon
	if r := s.Release; r != nil {
		if r.IsPreload || r.Limited || r.ComingSoon != nil && *r.ComingSoon {
			return "upcoming", 0
		}
		if comingSoon == nil {
			comingSoon = r.ComingSoon
		}
		date = r.SteamDate
	}
	if comingSoon != nil && *comingSoon || date != 0 && int64(date) > at.Unix() {
		return "upcoming", 0
	}
	if date != 0 || comingSoon != nil {
		return "released", date
	}
	return "", 0
}

func (s wishlistStoreItem) demoIDs() []uint32 {
	ids := slices.Concat(s.Related.DemoIDs, s.Related.StandaloneIDs)
	for _, demos := range [][]wishlistDemo{s.Related.Demos, s.Related.Standalone} {
		for _, demo := range demos {
			ids = append(ids, demo.AppID)
		}
	}
	ids = slices.DeleteFunc(ids, func(id uint32) bool { return id == 0 || id == s.AppID })
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (s wishlistStoreItem) promotion(p *storePrice, at time.Time) (giveaway bool, end uint32) {
	option := s.BestPurchase
	now := at.Unix()
	if option.IsFreeToKeep && (option.FreeToKeepEnd == 0 || int64(option.FreeToKeepEnd) > now) {
		return true, option.FreeToKeepEnd
	}
	if p == nil || p.DiscountPercent == 0 {
		return false, 0
	}
	weekend := s.FreeWeekend
	if p.DiscountPercent == 100 && p.Final == 0 && !s.IsFree && !option.IsFreeToKeep &&
		(weekend == nil || int64(weekend.Start) > now || weekend.End != 0 && int64(weekend.End) <= now) {
		giveaway = true
	}
	final, err := strconv.ParseUint(string(option.Final), 10, 32)
	if err == nil && uint32(final) == p.Final && option.Discount == p.DiscountPercent {
		for _, discount := range option.Discounts {
			if int64(discount.End) > now && (end == 0 || discount.End < end) {
				end = discount.End
			}
		}
	}
	return giveaway, end
}

type wishlistTargetKey struct {
	AppID    uint32
	Currency string
}

func (c config) wishlistTargets() map[wishlistTargetKey]uint32 {
	if len(c.WishlistTargetPrices) > maxTargetText {
		return nil
	}
	rules := strings.SplitN(c.WishlistTargetPrices, ",", maxTargetRules+1)
	if len(rules) > maxTargetRules {
		return nil
	}
	targets := make(map[wishlistTargetKey]uint32)
	for _, rule := range rules {
		parts := strings.Split(strings.TrimSpace(rule), ":")
		if len(parts) != 3 || len(parts[1]) != 3 || strings.Trim(parts[1], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			continue
		}
		id, idErr := strconv.ParseUint(parts[0], 10, 32)
		amount, priceErr := strconv.ParseUint(parts[2], 10, 32)
		if idErr == nil && id != 0 && priceErr == nil {
			targets[wishlistTargetKey{uint32(id), parts[1]}] = uint32(amount)
		}
	}
	return targets
}

func (c config) wishlistTrigger(trigger string) bool {
	return c.WishlistEventTriggers == nil || slices.Contains(c.WishlistEventTriggers, trigger)
}

type wishlistItemState struct {
	Seen        time.Time `json:"seen"`
	PriceAt     time.Time `json:"price_at,omitzero"`
	SaleStarted time.Time `json:"sale_started,omitzero"`
	SaleEnd     uint32    `json:"sale_end,omitempty"`
	Giveaway    bool      `json:"giveaway,omitempty"`
	Release     string    `json:"release,omitempty"`
}

type wishlistWatchState struct {
	Namespace string                       `json:"namespace"`
	Items     map[uint32]wishlistItemState `json:"items"`
	Roundup   time.Time                    `json:"roundup,omitzero"`
}

func (s *wishlistItemState) observeSale(at time.Time, selling, checked bool, end uint32) {
	if selling {
		if s.SaleStarted.IsZero() || end != 0 && s.SaleEnd != 0 && int64(s.SaleEnd) <= at.Unix() {
			s.SaleStarted, s.SaleEnd = at, 0
		}
		if end != 0 {
			s.SaleEnd = end
		}
	} else if checked {
		s.SaleStarted, s.SaleEnd = time.Time{}, 0
	}
}

type wishlistDeal struct {
	wishItem
	Giveaway bool   `json:"giveaway,omitempty"`
	NewLow   bool   `json:"observed_new_low,omitempty"`
	Target   bool   `json:"target_price_met,omitempty"`
	URL      string `json:"url,omitempty"`
}

func (d wishlistDeal) paidDiscount() bool {
	return !d.Giveaway && d.Price != nil && d.Price.Final > 0 && d.Price.DiscountPercent > 0 && d.Price.DiscountPercent < 100
}

func wishlistNewLow(p *storePrice, history PriceHistory, after, at time.Time) bool {
	return p != nil && p.Currency == history.Latest.Currency && !history.ObservedLowestAt.IsZero() && !history.Latest.At.After(at) &&
		(p.Final < history.ObservedLowestFinal || p.Final == history.ObservedLowestFinal &&
			history.ObservedLowestImproved && history.ObservedLowestAt.After(after))
}

func compareWishlistDeals(a, b wishlistDeal) int {
	if a.Giveaway != b.Giveaway {
		if a.Giveaway {
			return -1
		}
		return 1
	}
	if a.NewLow != b.NewLow {
		if a.NewLow {
			return -1
		}
		return 1
	}
	if byDiscount := cmp.Compare(b.discount(), a.discount()); byDiscount != 0 {
		return byDiscount
	}
	return cmp.Compare(a.AppID, b.AppID)
}

func (d wishlistDeal) discount() int {
	if d.Price == nil {
		return 0
	}
	return d.Price.DiscountPercent
}

type wishlistCandidate struct {
	kind, key, version string
	ends               uint32
	thing              wire.ConnectorInboxThing
}

type wishlistDelivery struct {
	Thing wire.ConnectorInboxThing `json:"thing"`
	Until time.Time                `json:"until"`
}

func (t *text) wishlistNotice(kind, key, version string, ends uint32, item wishItem, preview string) wishlistCandidate {
	label := t.inboxPrice
	switch kind {
	case wishlistGiveawayKind:
		label = t.inboxGiveaway
	case wishlistReleaseKind:
		label = t.inboxRelease
	case wishlistDemoKind:
		label = t.inboxDemo
	}
	name := item.Name
	if name == "" {
		name = fmt.Sprintf(t.game, item.AppID)
	}
	return wishlistCandidate{kind: kind, key: key, version: version, ends: ends, thing: wire.ConnectorInboxThing{
		Kind: kind, Label: label, Title: name + " · " + label, Sender: "Steam", Preview: preview, URL: storeLink(item.AppID),
		Tier: wire.ConnectorTierAction, Extra: &wire.ConnectorThingExtra{RuleTier: wire.ConnectorTierAction},
	}}
}

func (t *text) wishlistPrice(deal wishlistDeal) string {
	p := deal.Price
	price := p.FinalFormatted
	if price == "" {
		price = fmt.Sprintf(t.inboxUnits, p.Final, p.Currency)
	}
	if p.DiscountPercent > 0 {
		price += " · " + fmt.Sprintf(t.discount, p.DiscountPercent)
	}
	return price
}

func (t *text) wishlistRoundup(country string, started time.Time, deals []wishlistDeal) wishlistCandidate {
	top := deals[:min(len(deals), roundupTopDeals)]
	preview := fmt.Sprintf(t.inboxTop, len(deals), country, len(top))
	rows := make([]string, 0, len(top))
	for i, deal := range top {
		name := deal.Name
		if name == "" {
			name = fmt.Sprintf(t.game, deal.AppID)
		}
		price := t.wishlistPrice(deal)
		if deal.NewLow {
			price += " · " + t.inboxNewLow
		}
		if deal.Target {
			price += " · " + t.inboxTarget
		}
		rows = append(rows, fmt.Sprintf("%d. %s · %s · %s", i+1, name, price, storeLink(deal.AppID)))
	}
	return wishlistCandidate{kind: wishlistRoundupKind, key: "deals", version: started.Format(time.RFC3339Nano), thing: wire.ConnectorInboxThing{
		Kind: wishlistRoundupKind, Label: t.inboxRoundup, Title: t.inboxRoundup, Sender: "Steam",
		Preview: preview, Context: strings.Join(rows, "\n"), URL: storeBase + "/specials/",
		Tier: wire.ConnectorTierAction, Extra: &wire.ConnectorThingExtra{RuleTier: wire.ConnectorTierAction},
	}}
}

func (state *wishlistWatchState) candidates(cfg config, t *text, at, previous time.Time, list *wishList, complete bool, history map[uint32]PriceHistory) []wishlistCandidate {
	threshold := enabledLimit(true, cfg.WishlistDiscountThreshold, defaultDiscount, 100)
	targets := cfg.wishlistTargets()
	var giveaways, releasesAndDemos, priceAlerts, saleAlerts []wishlistCandidate
	var sales []wishlistDeal
	var uncertainSales int
	var ongoingSale bool
	for _, item := range list.Items {
		id := appIDText(item.AppID)
		meta := list.store[item.AppID]
		old := state.Items[item.AppID]
		giveaway, ends := meta.promotion(item.Price, at)
		deal := wishlistDeal{wishItem: item, Giveaway: giveaway}
		if p := item.Price; p != nil {
			deal.NewLow = cfg.wishlistTrigger("local_low") && wishlistNewLow(p, history[item.AppID], old.PriceAt, at)
			target, ok := targets[wishlistTargetKey{item.AppID, p.Currency}]
			deal.Target = ok && p.Final <= target
			old.PriceAt = at
		}
		sale := deal.paidDiscount()
		checked := item.Price != nil && item.Price.DiscountPercent == 0
		if old.Giveaway && meta.AppID == 0 && (old.SaleEnd == 0 || int64(old.SaleEnd) > at.Unix()) {
			// AppDetails can still list the full price during a free-to-keep promotion.
			checked = false
		}
		old.observeSale(at, sale || giveaway, checked, ends)
		if giveaway || sale || checked {
			old.Giveaway = giveaway
		}
		old.Seen = at
		window := old.SaleStarted.Format(time.RFC3339Nano)
		if sale {
			sales = append(sales, deal)
			ongoingSale = ongoingSale || !old.SaleStarted.After(previous)
		} else if item.Price == nil && !old.Giveaway && !old.SaleStarted.IsZero() && (old.SaleEnd == 0 || int64(old.SaleEnd) > at.Unix()) {
			uncertainSales++
		}
		if giveaway && cfg.wishlistTrigger("giveaway") {
			giveaways = append(giveaways, t.wishlistNotice(wishlistGiveawayKind, id, window, ends, item, t.inboxGiveawayHint))
		}
		if release, date := meta.releaseState(at); release != "" {
			if cfg.wishlistTrigger("release") && old.Release == "upcoming" && release == "released" {
				version := strconv.FormatUint(uint64(date), 10)
				releasesAndDemos = append(releasesAndDemos, t.wishlistNotice(wishlistReleaseKind, id, version, 0, item, t.inboxReleaseHint))
			}
			old.Release = release
		}
		if cfg.wishlistTrigger("demo") {
			for _, demo := range meta.demoIDs() {
				releasesAndDemos = append(releasesAndDemos, t.wishlistNotice(wishlistDemoKind, id, appIDText(demo), 0, item, t.inboxDemoHint))
			}
		}
		state.Items[item.AppID] = old

		p := deal.Price
		if p == nil || giveaway || p.DiscountPercent == 100 {
			continue
		}
		var reasons []string
		if sale && cfg.wishlistTrigger("discount") && p.DiscountPercent >= threshold {
			reasons = append(reasons, fmt.Sprintf(t.inboxThreshold, threshold))
		}
		if deal.Target {
			reasons = append(reasons, t.inboxTarget)
		}
		if deal.NewLow {
			reasons = append(reasons, t.inboxNewLow)
		}
		if len(reasons) != 0 {
			key := fmt.Sprintf("%s/%s/%d", id, p.Currency, p.Final)
			candidate := t.wishlistNotice(wishlistPriceKind, key, window, ends, item, t.wishlistPrice(deal)+" · "+strings.Join(reasons, " · "))
			if sale {
				saleAlerts = append(saleAlerts, candidate)
			} else {
				priceAlerts = append(priceAlerts, candidate)
			}
		}
	}
	trimOldest(state.Items, maxPriceHistories, func(item wishlistItemState) time.Time { return item.Seen })

	if cfg.AllowsSaleRoundup() && len(sales) >= roundupMinDeals {
		if state.Roundup.IsZero() || !ongoingSale {
			state.Roundup = at
		}
		slices.SortFunc(sales, compareWishlistDeals)
		giveaways = append(giveaways, t.wishlistRoundup(cfg.CountryCode, state.Roundup, sales))
		saleAlerts = nil
	}
	if complete && len(sales)+uncertainSales < roundupMinDeals {
		state.Roundup = time.Time{}
	}
	return slices.Concat(giveaways, releasesAndDemos, saleAlerts, priceAlerts)
}

func (s *EventCursorStore) observeWishlist(id string, cfg config, t *text, at time.Time, list *wishList, complete bool, histories map[uint32]PriceHistory) (out []wishlistDelivery, created int, err error) {
	limit := cfg.InboxEventLimit()
	if limit == 0 {
		return nil, 0, nil
	}
	scope, err := priceScope(id, cfg.CountryCode)
	if err != nil {
		return nil, 0, err
	}
	at = at.UTC()
	if !validSnapshotTime(at) {
		return nil, 0, fmt.Errorf("invalid Steam wishlist observation time")
	}
	// Reserve the daily budget and save the observation and replayable rows in the same transaction.
	err = s.file.update(func(entries map[string]eventCursorRecord) (bool, error) {
		saved := entries[scope]
		if at.Before(saved.SeenAt) {
			out = wishlistDeliveries(entries, id, cfg.CountryCode, at)
			return false, nil
		}
		var state wishlistWatchState
		if saved.Wishlist != nil && saved.Wishlist.Namespace != "" && saved.Wishlist.Items != nil {
			state = *saved.Wishlist
			state.Items = maps.Clone(state.Items)
		} else {
			state = wishlistWatchState{Namespace: rand.Text(), Items: make(map[uint32]wishlistItemState)}
		}
		candidates := state.candidates(cfg, t, at, saved.SeenAt, list, complete, histories)
		keys := make([]string, len(candidates))
		for i, event := range candidates {
			key, err := eventCursorKey(id, event.kind, cfg.CountryCode+"/"+event.key, event.version)
			if err != nil {
				return false, err
			}
			keys[i] = key
		}
		pruneEventCursors(entries, at)
		day := utcDay(at)
		accountPrefix := id + "\x00"
		remaining, today := limit, 0
		for key, entry := range entries {
			if !entry.SentAt.Before(day) {
				today++
				if strings.HasPrefix(key, accountPrefix) {
					remaining--
				}
			}
		}
		remaining = min(remaining, maxEventCursors-1-today)
		for i, event := range candidates {
			key := keys[i]
			if existing, ok := entries[key]; ok {
				existing.SeenAt = at
				entries[key] = existing
				continue
			}
			if remaining <= 0 {
				continue
			}
			delivery := event.delivery(state.Namespace, key[len(accountPrefix):], at)
			entries[key] = eventCursorRecord{SentAt: at, Delivery: &delivery}
			remaining--
			created++
		}
		// Reserve today's cursors and the current observation within the store's size bound.
		delete(entries, scope)
		trimOldest(entries, maxEventCursors-1, func(r eventCursorRecord) time.Time {
			if !r.SentAt.Before(day) {
				return r.SentAt.Add(eventCursorTTL)
			}
			return r.lastSeen()
		})
		entries[scope] = eventCursorRecord{SeenAt: at, Wishlist: &state}
		out = wishlistDeliveries(entries, id, cfg.CountryCode, at)
		return true, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, created, nil
}

func wishlistDeliveries(entries map[string]eventCursorRecord, id, country string, at time.Time) []wishlistDelivery {
	var out []wishlistDelivery
	for key, entry := range entries {
		if entry.Delivery == nil || !at.Before(entry.Delivery.Until) || !strings.HasPrefix(key, id+"\x00") {
			continue
		}
		parts := strings.SplitN(key, "\x00", 4)
		if len(parts) == 4 && strings.HasPrefix(parts[2], country+"/") {
			out = append(out, *entry.Delivery)
		}
	}
	slices.SortFunc(out, func(a, b wishlistDelivery) int {
		if order := b.Thing.At.Compare(a.Thing.At); order != 0 {
			return order
		}
		return strings.Compare(a.Thing.Thread, b.Thing.Thread)
	})
	return out
}

func (event wishlistCandidate) delivery(namespace, key string, at time.Time) wishlistDelivery {
	thing := event.thing
	hash := sha256.Sum256([]byte(namespace + "\x00" + key))
	thing.Thread = fmt.Sprintf("wishlist:%x", hash[:16])
	thing.At, thing.Last = at, at.Format(time.RFC3339Nano)
	until := at.Add(wishlistDeliveryTTL)
	if event.ends != 0 {
		if ends := time.Unix(int64(event.ends), 0).UTC(); ends.Before(until) {
			until = ends
		}
	}
	return wishlistDelivery{Thing: thing, Until: until}
}

func (d *steamData) startWishlistInbox(ctx context.Context, hub *ext.Hub, t *text) {
	if !d.config.AllowsInboxEvents() || d.setup != "" {
		return
	}
	go func() {
		ticker := time.NewTicker(wishlistPollInterval)
		defer ticker.Stop()
		for ctx.Err() == nil {
			d.scanWishlistInbox(ctx, t, func() { _ = hub.ConnectorChanged(extensionID, wire.ConnectorScopeInbox) })
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *steamData) readWishlistSignals(ctx context.Context, t *text) (string, *wishList, problem) {
	var p reading[player]
	if d.session == nil {
		p = d.account(ctx, profileTTL, true)
		if p.Stale {
			return "", nil, p.Issue
		}
	}
	id, issue := d.readID(p)
	if issue != "" {
		return "", nil, issue
	}
	c, issue := d.readClient(ctx)
	if issue != "" {
		return "", nil, issue
	}
	list, issue := c.loadWishlist(ctx, id, d.config.CountryCode, t.storeLanguage, true)
	if list != nil {
		for i := range list.Items {
			item := &list.Items[i]
			item.Name = redactCredentials(item.Name, id)
			if item.Price != nil {
				item.Price.FinalFormatted = redactCredentials(item.Price.FinalFormatted, id)
			}
		}
	}
	return id, list, issue
}

func (d *steamData) scanWishlistInbox(ctx context.Context, t *text, notify func()) {
	if !d.config.AllowsInboxEvents() || d.setup != "" || ctx.Err() != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, readBudget)
	id, list, issue := d.readWishlistSignals(readCtx, t)
	cancel()
	d.recordSnapshot(func() {
		if ctx.Err() != nil {
			return
		}
		if list == nil {
			d.inbox.Store(nil)
			return
		}
		at := d.wishes.now().UTC()
		quotes := wishlistQuotes(list)
		history, _ := d.stores.prices.Histories(id, d.config.CountryCode, slices.Collect(maps.Keys(quotes))...)
		deliveries, created, err := d.stores.cursors.observeWishlist(id, d.config, t, at, list, issue == "" && list.Total == len(list.Items), history)
		if err != nil {
			d.inbox.Store(nil)
			return
		}
		_ = d.stores.prices.Observe(id, d.config.CountryCode, at, quotes)
		d.inbox.Store(&deliveries)
		if created != 0 && ctx.Err() == nil {
			notify()
		}
	})
}

func (a *app) collectWishlistInbox() wire.ConnectorCollectResult {
	d := a.steamData
	if !d.config.AllowsInboxEvents() {
		return wire.ConnectorCollectResult{}
	}
	if a.qr != nil {
		a.qr.mu.Lock()
		defer a.qr.mu.Unlock()
		s := a.qr.session
		if s == nil || !s.currentLocked() || !s.tokens.refreshExpiry.After(a.qr.now()) {
			return wire.ConnectorCollectResult{}
		}
		d = s.reads
	}
	var out wire.ConnectorCollectResult
	if deliveries := d.inbox.Load(); deliveries != nil {
		at := d.wishes.now()
		for _, event := range *deliveries {
			if at.Before(event.Until) && (d.config.AllowsSaleRoundup() || event.Thing.Kind != wishlistRoundupKind) {
				out.Things = append(out.Things, event.Thing)
			}
		}
	}
	return out
}

func (a *app) wishlistDeals(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	args := struct {
		Limit int `json:"limit"`
	}{Limit: defaultToolLimit}
	if !decodeArgs(raw, &args) || args.Limit < 1 || args.Limit > maxToolLimit {
		return nil, invalid("Use a limit from 1 to 50.")
	}
	d, ctx, cancel := a.readContext(ctx)
	defer cancel()
	t := textFor(a.locale)
	out := map[string]any{"available": false, "country_code": d.config.CountryCode}
	id, list, issue := d.readWishlistSignals(ctx, t)
	if sessionIssue := d.sessionIssue(); sessionIssue != "" {
		list, issue = nil, sessionIssue
	}
	describeReading(out, "wishlist", reading[wishList]{Issue: issue}, t)
	if list == nil {
		return out, nil
	}
	at := d.wishes.now().UTC()
	quotes := wishlistQuotes(list)
	histories, _ := d.stores.prices.Histories(id, d.config.CountryCode, slices.Collect(maps.Keys(quotes))...)
	targets := d.config.wishlistTargets()
	deals := make([]wishlistDeal, 0)
	for _, item := range list.Items {
		giveaway, _ := list.store[item.AppID].promotion(item.Price, at)
		deal := wishlistDeal{wishItem: item, Giveaway: giveaway, URL: storeLink(item.AppID)}
		if p := item.Price; p != nil && p.DiscountPercent < 100 {
			deal.NewLow = wishlistNewLow(p, histories[item.AppID], time.Time{}, at)
			target, ok := targets[wishlistTargetKey{item.AppID, p.Currency}]
			deal.Target = ok && p.Final <= target
		}
		if deal.Giveaway || deal.NewLow || deal.Target || deal.paidDiscount() {
			deals = append(deals, deal)
		}
	}
	if ctx.Err() == nil {
		d.recordSnapshot(func() { _ = d.stores.prices.Observe(id, d.config.CountryCode, at, quotes) })
	}
	if issue := d.sessionIssue(); issue != "" {
		out = map[string]any{"available": false}
		describeReading(out, "wishlist", reading[wishList]{Issue: issue}, t)
		return out, nil
	}
	slices.SortFunc(deals, compareWishlistDeals)
	out["available"], out["wishlist_count"], out["prices_checked"] = true, list.Total, list.Checked
	out["matched_count"], out["truncated"] = len(deals), list.Total > len(list.Items) || len(deals) > args.Limit
	out["deals"] = deals[:min(len(deals), args.Limit)]
	return out, nil
}
