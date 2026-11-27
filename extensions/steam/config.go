package main

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	steamIDBase    = 76561197960265728
	defaultCountry = "US"
	keyName        = "steam_api_key"
)

var (
	digits     = regexp.MustCompile(`^[0-9]+$`)
	vanityName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

type config struct {
	SteamID     string `json:"steam_id"`
	CountryCode string `json:"country_code"`
	TokenConfig
}

func (c config) normalized() (config, problem) {
	c.SteamID = strings.TrimSpace(c.SteamID)
	c.CountryCode = strings.ToUpper(strings.TrimSpace(c.CountryCode))
	if c.CountryCode == "" {
		c.CountryCode = defaultCountry
	}
	switch {
	case c.SteamID == "":
		return c, problemSetup
	case digits.MatchString(c.SteamID):
		if !validSteamID(c.SteamID) {
			return c, problemInvalidID
		}
	case !vanityName.MatchString(c.SteamID):
		return c, problemInvalidID
	}
	if len(c.CountryCode) != 2 || !strings.Contains(isoCountries, " "+c.CountryCode+" ") {
		return c, problemCountry
	}
	return c, ""
}

func validSteamID(s string) bool {
	if len(s) != 17 || !digits.MatchString(s) {
		return false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n > steamIDBase && n <= steamIDBase+(1<<32)-1
}

// ISO 3166-1 alpha-2 assignments, delimited to match complete country codes.
const isoCountries = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ " +
	"BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS " +
	"BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN " +
	"CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE " +
	"EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF " +
	"GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM " +
	"HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM " +
	"JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC " +
	"LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK " +
	"ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA " +
	"NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG " +
	"PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
	"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS " +
	"ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO " +
	"TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI " +
	"VN VU WF WS YE YT ZA ZM ZW "
