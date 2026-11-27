package main

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

const (
	defaultMagpieURL = "http://127.0.0.1:3425"
	quotaPath        = "/v1/magpie/quotas"
	maxQuotaBody     = 4 << 20
	maxWindowMinutes = 366 * 24 * 60
)

type magpieAccount struct {
	Provider  string         `json:"provider"`
	Kind      string         `json:"kind"`
	Plan      string         `json:"plan"`
	Alias     string         `json:"alias"`
	User      string         `json:"user"`
	Name      string         `json:"name"`
	From      string         `json:"from"`
	ReadAt    string         `json:"readAt"`
	AsOf      string         `json:"asOf"`
	Error     string         `json:"error"`
	ReadError string         `json:"readError"`
	Windows   []magpieWindow `json:"windows"`
}

type magpieWindow struct {
	Name      string   `json:"name"`
	Window    string   `json:"window"`
	Pool      string   `json:"pool"`
	Period    int64    `json:"period"`
	Used      *float64 `json:"used"`
	Balance   *float64 `json:"balance"`
	ResetsAt  string   `json:"resetsAt"`
	Text      bool     `json:"text"`
	Unlimited bool     `json:"unlimited"`
}

type account struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	Name      string     `json:"name"`
	Plan      string     `json:"plan,omitempty"`
	SampledAt *time.Time `json:"sampled_at,omitempty"`
	Notice    string     `json:"notice,omitempty"`
	Windows   []window   `json:"windows"`
}

type window struct {
	Label         string     `json:"label"`
	WindowMinutes int        `json:"window_minutes"`
	Used          float64    `json:"used_percent"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Model         string     `json:"model,omitempty"`
	Group         string     `json:"group,omitempty"`
	Title         string     `json:"title,omitempty"`
}

type mapOptions struct {
	includeProviders map[string]bool
	excludeProviders map[string]bool
	anonymousNames   bool
}

func main() {
	ext.Main(program())
}

func program() ext.Program {
	var base, quotaKey string
	var remote bool
	var opts mapOptions
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var secret []byte
	return ext.Program{
		ID: "magpie-usage", Version: "0.1.0", Manifest: manifest,
		Init: func(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
			cfg := struct {
				Local            string   `json:"local_address"`
				Remote           string   `json:"remote_address"`
				IncludeProviders []string `json:"include_providers"`
				ExcludeProviders []string `json:"exclude_providers"`
				AnonymousNames   bool     `json:"anonymous_names"`
			}{Local: defaultMagpieURL}
			if err := in.Config(&cfg); err != nil {
				return nil, errors.New("invalid Magpie configuration")
			}
			base, remote, quotaKey = cfg.Local, false, ""
			if strings.TrimSpace(cfg.Remote) != "" {
				base, remote = cfg.Remote, true
				quotaKey = strings.TrimSpace(in.Secrets["magpie_quota_key"])
			}
			opts = buildMapOptions(cfg.IncludeProviders, cfg.ExcludeProviders, cfg.AnonymousNames)
			var err error
			dir := in.Info().DataDir
			if dir == "" {
				// The standalone Persona conformance runner provides the directory through the environment.
				dir = os.Getenv("PERSONA_EXTENSION_DATA")
			}
			secret, err = accountSecret(dir)
			if err != nil {
				return nil, errors.New("cannot initialize Magpie account aliases")
			}
			return nil, nil
		},
		Namespaces: map[string]ext.Handler{"usage": func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
			if method != "query" {
				return nil, ext.UnknownMethod(method)
			}
			var p struct {
				Contribution string `json:"contribution"`
				Lang         string `json:"lang"`
			}
			if json.Unmarshal(raw, &p) != nil || p.Contribution != "magpie" {
				return nil, errors.New("invalid Magpie usage query")
			}
			body, scope, err := fetchQuotas(ctx, client, base, remote, quotaKey)
			if err != nil {
				return nil, err
			}
			accounts, err := mapQuotas(body, secret, scope, p.Lang, opts)
			if err != nil {
				return nil, err
			}
			return struct {
				Accounts []account `json:"accounts"`
			}{accounts}, nil
		}},
	}
}

func buildMapOptions(include, exclude []string, anonymous bool) mapOptions {
	opts := mapOptions{anonymousNames: anonymous}
	for _, raw := range include {
		if p := normalizeProvider(raw); p != "" {
			if opts.includeProviders == nil {
				opts.includeProviders = make(map[string]bool)
			}
			opts.includeProviders[p] = true
		}
	}
	for _, raw := range exclude {
		if p := normalizeProvider(raw); p != "" {
			if opts.excludeProviders == nil {
				opts.excludeProviders = make(map[string]bool)
			}
			opts.excludeProviders[p] = true
		}
	}
	return opts
}

func (o mapOptions) allowProvider(provider string) bool {
	if len(o.includeProviders) > 0 && !o.includeProviders[provider] {
		return false
	}
	if o.excludeProviders[provider] {
		return false
	}
	return true
}

func fetchQuotas(ctx context.Context, client *http.Client, base string, remote bool, key string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", errors.New("invalid Magpie address")
	}
	host := strings.ToLower(u.Hostname())
	local := host == "host.persona.internal" || host == "localhost" || net.ParseIP(host).IsLoopback()
	switch {
	case !remote && local && (u.Scheme == "http" || u.Scheme == "https"):
		key = "" // an optional remote key must never go to the host's local service
	case remote && !local && u.Scheme == "https" && key != "":
	case remote:
		return "", "", errors.New("remote Magpie requires HTTPS and a dedicated quota key")
	default:
		return "", "", errors.New("local Magpie requires a loopback address")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.RawPath = ""
	scope := u.String()
	if host == "host.persona.internal" {
		// The sandbox's proxy dials the host's 127.0.0.1. Keep the same identity scope when the sandbox is toggled.
		loopback := *u
		loopback.Host = "127.0.0.1"
		if u.Port() != "" {
			loopback.Host += ":" + u.Port()
		}
		scope = loopback.String()
	}
	u.Path += quotaPath
	u.RawQuery = "kept=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", errors.New("invalid Magpie quota request")
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", errors.New("cannot reach Magpie for subscription usage")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("Magpie quota endpoint did not return usage")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
	if err != nil || len(body) > maxQuotaBody {
		return "", "", errors.New("invalid Magpie quota response body")
	}
	return string(body), scope, nil
}

func mapQuotas(body string, secret []byte, scope, lang string, opts mapOptions) ([]account, error) {
	var data struct {
		Data []magpieAccount `json:"data"`
	}
	if json.Unmarshal([]byte(body), &data) != nil || data.Data == nil {
		return nil, errors.New("invalid Magpie quota response")
	}
	out := []account{}
	seen := make(map[string]bool)
	for _, src := range data.Data {
		if src.Kind != "subscription" {
			continue
		}
		provider := normalizeProvider(src.Provider)
		if provider == "" || !opts.allowProvider(provider) {
			continue
		}
		identity := strings.TrimSpace(src.User)
		if identity == "" {
			identity = strings.TrimSpace(src.Alias)
		}
		if identity == "" {
			identity = strings.TrimSpace(src.Name)
		}
		if identity == "" {
			continue
		}
		parsedWindows := parseAccountWindows(src.Windows)
		if len(parsedWindows) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(scope + "\x00" + provider + "\x00" + identity))
		if from := strings.TrimSpace(src.From); from != "" {
			mac.Write([]byte("\x00" + from))
		}
		id := hex.EncodeToString(mac.Sum(nil)[:16])
		if seen[id] {
			continue
		}
		seen[id] = true
		a := account{
			ID:       id,
			Provider: provider,
			Name:     displayName(src, id, opts.anonymousNames),
			Plan:     normalizePlan(src.Plan, opts.anonymousNames),
			Windows:  []window{},
		}
		a.SampledAt = parseTime(src.ReadAt)
		if a.SampledAt == nil {
			a.SampledAt = parseTime(src.AsOf)
		}
		// Persona defaults an undated sample to the poll time. Only send numerical windows with a known sample time.
		if a.SampledAt != nil {
			a.Windows = parsedWindows
		}
		switch {
		case len(a.Windows) == 0:
			a.Notice = localized(lang, "Subscription usage unavailable", "暂无订阅额度读数", "サブスクリプションの使用量を取得できません")
		case src.Error != "" || src.ReadError != "":
			a.Notice = localized(lang, "Magpie is showing a saved reading", "Magpie 显示的是上次读数", "Magpie の保存済みの値を表示しています")
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b account) int {
		return cmp.Or(
			strings.Compare(a.Provider, b.Provider),
			strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			strings.Compare(a.ID, b.ID),
		)
	})
	return out, nil
}

// normalizeProvider converts a Magpie provider identifier into Persona's ^[a-z0-9][a-z0-9_-]{0,31}$ contract.
func normalizeProvider(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || strings.HasSuffix(s, "/~keys") {
		return ""
	}
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevSep = false
		case r == '_' || r == '-':
			if b.Len() > 0 && !prevSep {
				b.WriteRune(r)
				prevSep = true
			}
		default:
			if b.Len() > 0 && !prevSep {
				b.WriteByte('-')
				prevSep = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-_")
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "-_")
	}
	if out == "agy" {
		return "antigravity"
	}
	return out
}

func displayName(src magpieAccount, id string, anonymous bool) string {
	if anonymous {
		return "Magpie " + id[:12]
	}
	for _, candidate := range []string{src.Alias, src.User, src.Name} {
		if s := cleanLine(candidate); s != "" {
			return s
		}
	}
	return "Magpie " + id[:12]
}

var canonicalPlans = map[string]string{
	"free":       "Free",
	"starter":    "Starter",
	"basic":      "Basic",
	"lite":       "Lite",
	"hobby":      "Hobby",
	"standard":   "Standard",
	"individual": "Individual",
	"plus":       "Plus",
	"pro":        "Pro",
	"pro+":       "Pro+",
	"promax":     "Promax",
	"pro max":    "Promax",
	"max":        "Max",
	"ultra":      "Ultra",
	"team":       "Team",
	"teams":      "Team",
	"business":   "Business",
	"enterprise": "Enterprise",
	"vip":        "VIP",
	"svip":       "SVIP",
	"edu":        "Edu",
	"student":    "Student",
	"go":         "Go",
}

var tierMultiplierRE = regexp.MustCompile(`^\d+x$`)

func normalizePlan(raw string, anonymous bool) string {
	cleaned := cleanLine(raw)
	if cleaned == "" {
		return ""
	}
	spaced := strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(cleaned)), " ")
	lower := strings.ToLower(spaced)
	if p, ok := canonicalPlans[lower]; ok {
		return p
	}
	parts := strings.Fields(lower)
	if len(parts) == 2 {
		if base, ok := canonicalPlans[parts[0]]; ok {
			if tier, ok := canonicalPlans[parts[1]]; ok {
				return base + " " + tier
			}
			if tierMultiplierRE.MatchString(parts[1]) {
				return base + " " + parts[1]
			}
		}
	}
	if anonymous {
		return ""
	}
	switch lower {
	case "no plan", "none", "unknown", "signed in", "已登录":
		return ""
	}
	words := strings.Fields(spaced)
	for i, w := range words {
		lw := strings.ToLower(w)
		if canon, ok := canonicalPlans[lw]; ok {
			words[i] = canon
			continue
		}
		if tierMultiplierRE.MatchString(lw) {
			words[i] = lw
			continue
		}
		words[i] = titleWord(w)
	}
	return strings.Join(words, " ")
}

func titleWord(w string) string {
	if w == "" {
		return ""
	}
	runes := []rune(w)
	hasUpper, hasLower := false, false
	for _, r := range runes {
		if unicode.IsUpper(r) {
			hasUpper = true
		} else if unicode.IsLower(r) {
			hasLower = true
		}
	}
	if hasUpper && !hasLower && len(runes) <= 4 {
		return w // keep short acronyms like AI, GLM, API
	}
	if hasUpper && hasLower {
		return w // keep existing camel/mixed case
	}
	runes[0] = unicode.ToUpper(runes[0])
	for i := 1; i < len(runes); i++ {
		runes[i] = unicode.ToLower(runes[i])
	}
	return string(runes)
}

type parsedWindow struct {
	win   window
	scope string
	pool  string
}

func parseAccountWindows(raw []magpieWindow) []window {
	var items []parsedWindow
	scopeCount := make(map[string]int)
	for _, w := range raw {
		pw, ok := parseMagpieWindow(w)
		if !ok {
			continue
		}
		items = append(items, pw)
		if pw.scope != "" {
			scopeCount[strings.ToLower(pw.scope)]++
		}
	}
	if len(items) == 0 {
		return nil
	}
	for i := range items {
		pw := &items[i]
		if pw.pool != "" {
			pw.win.Group = pw.pool
		} else if pw.scope != "" && scopeCount[strings.ToLower(pw.scope)] > 1 {
			pw.win.Group = pw.scope
		}
	}
	// Keep ungrouped windows first in Magpie's order, then group contiguous pooled/grouped windows
	// ordered shortest-first within each group so Persona draws each group together.
	type winKey struct {
		minutes int
		model   string
	}
	seen := make(map[winKey]bool, len(items))
	var ungrouped []window
	var groupOrder []string
	grouped := make(map[string][]window)
	for _, pw := range items {
		k := winKey{minutes: pw.win.WindowMinutes, model: strings.ToLower(pw.win.Model)}
		if seen[k] {
			continue
		}
		seen[k] = true
		if pw.win.Group == "" {
			ungrouped = append(ungrouped, pw.win)
			continue
		}
		gk := strings.ToLower(pw.win.Group)
		if _, exists := grouped[gk]; !exists {
			groupOrder = append(groupOrder, gk)
		}
		grouped[gk] = append(grouped[gk], pw.win)
	}
	out := make([]window, 0, len(seen))
	out = append(out, ungrouped...)
	for _, gk := range groupOrder {
		ws := grouped[gk]
		slices.SortStableFunc(ws, func(a, b window) int {
			return cmp.Or(cmp.Compare(a.WindowMinutes, b.WindowMinutes), strings.Compare(a.Title, b.Title))
		})
		out = append(out, ws...)
	}
	return out
}

func parseMagpieWindow(w magpieWindow) (parsedWindow, bool) {
	if w.Used == nil || w.Text || w.Unlimited || w.Balance != nil {
		return parsedWindow{}, false
	}
	if math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) {
		return parsedWindow{}, false
	}
	pool := cleanLine(w.Pool)
	var parts []string
	for _, p := range strings.Split(w.Name, "·") {
		if c := cleanLine(p); c != "" {
			parts = append(parts, c)
		}
	}

	minutes := 0
	if w.Period > 0 {
		m := int(math.Round(float64(w.Period) / 60))
		if m > 0 && m <= maxWindowMinutes {
			minutes = normalizeMonthMinutes(m)
		}
	}
	if minutes == 0 && strings.TrimSpace(w.Window) != "" {
		for _, p := range strings.Split(w.Window, "·") {
			if m, ok := parseSpanMinutes(p); ok && (minutes == 0 || m < minutes) {
				minutes = m
			}
		}
	}

	var nonSpanParts []string
	for _, p := range parts {
		if m, ok := parseSpanMinutes(p); ok {
			if minutes == 0 || m < minutes {
				minutes = m
			}
		} else {
			nonSpanParts = append(nonSpanParts, p)
		}
	}

	var scope string
	if minutes == 0 && len(parts) == 1 {
		if m, qual, ok := splitEmbeddedSpan(parts[0]); ok {
			minutes = m
			scope = qual
		}
	}
	if minutes <= 0 {
		return parsedWindow{}, false
	}

	switch {
	case strings.TrimSpace(w.Window) != "" && pool != "":
		scope = pool
	case scope != "":
	case len(nonSpanParts) > 0:
		scope = strings.Join(nonSpanParts, " · ")
	case pool != "":
		scope = pool
	}

	used := math.Min(100, math.Max(0, *w.Used))
	win := window{
		Label:         windowLabel(minutes),
		WindowMinutes: minutes,
		Used:          used,
		ResetsAt:      parseTime(w.ResetsAt),
	}
	if scope != "" {
		win.Model = scope
		win.Title = scope
	}
	return parsedWindow{win: win, scope: scope, pool: pool}, true
}

func normalizeMonthMinutes(minutes int) int {
	if minutes >= 28*24*60 && minutes <= 31*24*60 {
		return 30 * 24 * 60
	}
	return minutes
}

var spanUnitRE = regexp.MustCompile(`^(\d+)\s*(m|mins?|minutes?|h|hrs?|hours?|d|days?|w|wks?|weeks?|mo|months?)$`)

func parseSpanMinutes(s string) (int, bool) {
	n := strings.ToLower(strings.TrimSpace(s))
	if n == "" {
		return 0, false
	}
	switch n {
	case "hourly", "hour":
		return 60, true
	case "daily", "day":
		return 24 * 60, true
	case "weekly", "week":
		return 7 * 24 * 60, true
	case "monthly", "month":
		return 30 * 24 * 60, true
	case "five_hour", "five hour", "five hours", "five-hour", "5-hour":
		return 5 * 60, true
	case "seven_day", "seven day", "seven days", "seven-day", "7-day":
		return 7 * 24 * 60, true
	}
	m := spanUnitRE.FindStringSubmatch(n)
	if m == nil {
		return 0, false
	}
	count, err := strconv.Atoi(m[1])
	if err != nil || count <= 0 {
		return 0, false
	}
	var unit int
	switch m[2] {
	case "m", "min", "mins", "minute", "minutes":
		unit = 1
	case "h", "hr", "hrs", "hour", "hours":
		unit = 60
	case "d", "day", "days":
		unit = 24 * 60
	case "w", "wk", "wks", "week", "weeks":
		unit = 7 * 24 * 60
	case "mo", "month", "months":
		unit = 30 * 24 * 60
	default:
		return 0, false
	}
	if count > maxWindowMinutes/unit {
		return 0, false
	}
	return normalizeMonthMinutes(count * unit), true
}

// splitEmbeddedSpan extracts a period and model/pool qualifier from single-segment names without "·",
// such as "Weekly Sonnet", "MCP Monthly", "Opus (7d)", or "5h Opus".
func splitEmbeddedSpan(s string) (int, string, bool) {
	if open := strings.LastIndex(s, "("); open > 0 && strings.HasSuffix(s, ")") {
		outer := cleanLine(s[:open])
		inner := cleanLine(s[open+1 : len(s)-1])
		if m, ok := parseSpanMinutes(inner); ok && validQualifier(outer) {
			return m, outer, true
		}
		if m, ok := parseSpanMinutes(outer); ok && validQualifier(inner) {
			return m, inner, true
		}
	}
	words := strings.Fields(s)
	if len(words) < 2 {
		return 0, "", false
	}
	// Try 2-word and 1-word spans at the beginning or end.
	for _, spanLen := range []int{2, 1} {
		if len(words) <= spanLen {
			continue
		}
		if m, ok := parseSpanMinutes(strings.Join(words[:spanLen], " ")); ok {
			qual := strings.TrimLeft(strings.Join(words[spanLen:], " "), "-/: ")
			if validQualifier(qual) {
				return m, qual, true
			}
		}
		if m, ok := parseSpanMinutes(strings.Join(words[len(words)-spanLen:], " ")); ok {
			qual := strings.TrimRight(strings.Join(words[:len(words)-spanLen], " "), "-/: ")
			if validQualifier(qual) {
				return m, qual, true
			}
		}
	}
	return 0, "", false
}

func validQualifier(q string) bool {
	q = cleanLine(q)
	if q == "" {
		return false
	}
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "and", "or", "limits", "limit":
		return false
	}
	switch words[len(words)-1] {
	case "and", "or":
		return false
	}
	return true
}

func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func cleanLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func localized(lang, en, zh, ja string) string {
	switch lang {
	case "zh-Hans":
		return zh
	case "ja":
		return ja
	default:
		return en
	}
}

// Store a per-installation secret so account IDs persist across reads and cannot be guessed from usernames.
func accountSecret(dir string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("missing extension data directory")
	}
	path := filepath.Join(dir, "account-alias-secret")
	read := func() ([]byte, error) {
		secret, err := os.ReadFile(path)
		if err == nil && len(secret) != 32 {
			return nil, errors.New("invalid account alias secret")
		}
		return secret, err
	}
	if secret, err := read(); !os.IsNotExist(err) {
		return secret, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".account-alias-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(secret)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	if err := os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return nil, err
	}
	return read()
}
