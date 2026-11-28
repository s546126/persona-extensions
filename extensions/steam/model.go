package main

type problem string

const (
	problemSetup              problem = "not_configured"
	problemInvalidID          problem = "invalid_steam_id"
	problemCountry            problem = "invalid_country_code"
	problemMode               problem = "invalid_auth_mode"
	problemKey                problem = "api_key_rejected"
	problemPrivate            problem = "private_profile"
	problemProfileUnavailable problem = "steam_profile_unavailable"
	problemGameDetails        problem = "game_details_unavailable"
	problemAchievements       problem = "achievements_unavailable"
	problemWishlist           problem = "wishlist_unavailable"
	problemRateLimited        problem = "rate_limited"
	problemTimeout            problem = "timeout"
	problemNetwork            problem = "steam_unavailable"
	problemResponse           problem = "unexpected_steam_response"
	problemCanceled           problem = "canceled"
	problemAuthPending        problem = "steam_sign_in_pending"
	problemAuthRefused        problem = "steam_sign_in_refused"
	problemAuthExpired        problem = "steam_session_expired"
	problemAuthRequired       problem = "steam_sign_in_required"
	problemAuthTimeout        problem = "steam_qr_expired"
	problemAuthLoad           problem = "steam_session_load_failed"
	problemAuthSave           problem = "steam_session_save_failed"
	problemAuthDelete         problem = "steam_session_delete_failed"
	problemAuthClearing       problem = "steam_sign_out_pending"

	// HTTP outcomes interpreted at the endpoint that knows what they mean.
	problemDenied  problem = "access_denied"
	problemMissing problem = "not_found"
	problemBadCall problem = "bad_request"
)

func (p problem) temporary() bool {
	switch p {
	case problemRateLimited, problemTimeout, problemNetwork, problemResponse:
		return true
	default:
		return false
	}
}

type player struct {
	SteamID string `json:"steam_id"`
	Name    string `json:"name"`
	Public  bool   `json:"public"`
	State   string `json:"state"`
	Playing string `json:"playing,omitempty"`
	PlayApp uint32 `json:"playing_appid,omitempty"`
}

type game struct {
	AppID           uint32 `json:"appid"`
	Name            string `json:"name,omitempty"`
	Playtime2Weeks  uint32 `json:"playtime_2weeks"`
	PlaytimeForever uint32 `json:"playtime_forever"`
}

type gameList struct {
	Games          []game `json:"games"`
	Total          int    `json:"total"`
	Playtime2Weeks uint64 `json:"playtime_2weeks,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

type achievementProgress struct {
	AppID    uint32 `json:"appid"`
	Game     string `json:"game,omitempty"`
	Unlocked int    `json:"unlocked"`
	Total    int    `json:"total"`
}

type storePrice struct {
	Currency        string `json:"currency"`
	Final           uint32 `json:"final"`
	FinalFormatted  string `json:"final_formatted,omitempty"`
	DiscountPercent int    `json:"discount_percent"`
}

type wishItem struct {
	AppID        uint32      `json:"appid"`
	Priority     uint32      `json:"priority"`
	Name         string      `json:"name,omitempty"`
	PriceChecked bool        `json:"price_checked"`
	Price        *storePrice `json:"price,omitempty"`
}

type wishList struct {
	Items      []wishItem `json:"items"`
	Total      int        `json:"total"`
	Checked    int        `json:"prices_checked"`
	Discounted int        `json:"discounted_count"`
	store      map[uint32]wishlistStoreItem
}
