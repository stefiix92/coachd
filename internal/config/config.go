// Package config loads coachd settings from environment variables and
// secret files.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

// Config holds all runtime settings. The Garmin key and expiry are required
// only by `serve` outside demo mode, which validates them itself.
type Config struct {
	DataDir                          string
	TZ                               *time.Location
	PublicAddr, PrivateAddr, OpsAddr string
	GarminAPIKey                     string
	GarminKeyExpires                 civil.Date
	AccessTeamDomain, AccessAUD      string
	AccessAllowedEmail               string
	PollInterval, BackfillDelay      time.Duration
	SyncMinInterval                  time.Duration
	BackfillDays                     int
	// PublicURL is the runner page's origin (no trailing slash); NtfyURL is
	// the full ntfy topic URL and NtfyToken its optional bearer token.
	PublicURL, NtfyURL, NtfyToken string
	// DeviceAPI (COACHD_DEVICE_API=on) serves /device/v1/* on the public
	// listener for the watch app, outside Cloudflare Access (ruling R-M3-1).
	DeviceAPI bool
	// PublicAuth is the resolved public-listener auth mode; OAuthDCR enables
	// dynamic client registration (builtin mode only).
	PublicAuth PublicAuth
	OAuthDCR   bool
	// WebhookURL (COACHD_WEBHOOK_URL) receives an event when a recent
	// activity is stored; WebhookToken, from COACHD_WEBHOOK_TOKEN_FILE, is
	// its optional bearer token. Both are secrets: never logged.
	WebhookURL, WebhookToken string
	// Demo (COACHD_DEMO=true) makes serve ingest synthetic data instead of
	// calling Garmin, so coachd can be tried without a Garmin key.
	Demo bool
}

// PublicAuth selects how the public listener authenticates requests.
type PublicAuth string

const (
	PublicAuthNone    PublicAuth = ""
	PublicAuthAccess  PublicAuth = "access"
	PublicAuthBuiltin PublicAuth = "builtin"
)

// Load builds a Config from getenv (typically os.Getenv).
func Load(getenv func(string) string) (*Config, error) {
	str := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	c := &Config{
		DataDir:            str("COACHD_DATA_DIR", "/data"),
		PublicAddr:         str("COACHD_PUBLIC_ADDR", ":8080"),
		PrivateAddr:        str("COACHD_PRIVATE_ADDR", ":8081"),
		OpsAddr:            str("COACHD_OPS_ADDR", ":9090"),
		AccessTeamDomain:   getenv("COACHD_ACCESS_TEAM_DOMAIN"),
		AccessAUD:          getenv("COACHD_ACCESS_AUD"),
		AccessAllowedEmail: getenv("COACHD_ACCESS_ALLOWED_EMAIL"),
	}

	switch v := getenv("COACHD_DEVICE_API"); v {
	case "on":
		c.DeviceAPI = true
	case "", "off":
	default:
		return nil, fmt.Errorf("COACHD_DEVICE_API: must be on or off")
	}
	switch getenv("COACHD_DEMO") {
	case "true":
		c.Demo = true
	case "", "false":
	default:
		return nil, fmt.Errorf("COACHD_DEMO: must be true or false")
	}

	tz := getenv("COACHD_TIMEZONE")
	if tz == "" {
		return nil, fmt.Errorf("COACHD_TIMEZONE is required (IANA name, e.g. Europe/Bratislava)")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("COACHD_TIMEZONE: %w", err)
	}
	c.TZ = loc

	set := 0
	for _, v := range []string{c.AccessTeamDomain, c.AccessAUD, c.AccessAllowedEmail} {
		if v != "" {
			set++
		}
	}
	mode := PublicAuth(getenv("COACHD_PUBLIC_AUTH"))
	switch mode {
	case PublicAuthNone, PublicAuthAccess, PublicAuthBuiltin:
	default:
		return nil, fmt.Errorf("COACHD_PUBLIC_AUTH: must be access or builtin")
	}
	if mode == PublicAuthBuiltin && set != 0 {
		return nil, fmt.Errorf("COACHD_PUBLIC_AUTH=builtin: leave COACHD_ACCESS_* empty")
	}
	if set != 0 && set != 3 {
		return nil, fmt.Errorf("COACHD_ACCESS_TEAM_DOMAIN, COACHD_ACCESS_AUD and COACHD_ACCESS_ALLOWED_EMAIL must be set together or not at all")
	}

	if c.PublicURL, err = ParsePublicURL(getenv("COACHD_PUBLIC_URL")); err != nil {
		return nil, err
	}
	if mode == PublicAuthNone && set == 3 {
		mode = PublicAuthAccess
	}
	if mode == PublicAuthAccess && set != 3 {
		return nil, fmt.Errorf("COACHD_PUBLIC_AUTH=access: set COACHD_ACCESS_TEAM_DOMAIN, COACHD_ACCESS_AUD and COACHD_ACCESS_ALLOWED_EMAIL")
	}
	if c.PublicURL == "" && mode == PublicAuthBuiltin {
		return nil, fmt.Errorf("COACHD_PUBLIC_URL is required when COACHD_PUBLIC_AUTH=builtin (e.g. https://coachd.example.com)")
	}
	if c.PublicURL == "" && mode == PublicAuthAccess {
		return nil, fmt.Errorf("COACHD_PUBLIC_URL is required when the Access settings are set (e.g. https://coachd.example.com)")
	}
	// ParsePublicURL allows http only on localhost and 127.0.0.1. The
	// builtin page needs passkeys, and WebAuthn does not accept an IP
	// address as the relying party (spec §3).
	if mode == PublicAuthBuiltin && strings.HasPrefix(c.PublicURL, "http://127.0.0.1") {
		return nil, fmt.Errorf("COACHD_PUBLIC_URL: builtin mode needs https://, or http://localhost for development (WebAuthn does not accept an IP address)")
	}
	c.PublicAuth = mode
	switch getenv("COACHD_OAUTH_DCR") {
	case "", "off":
	case "on":
		if mode != PublicAuthBuiltin {
			return nil, fmt.Errorf("COACHD_OAUTH_DCR: requires COACHD_PUBLIC_AUTH=builtin")
		}
		c.OAuthDCR = true
	default:
		return nil, fmt.Errorf("COACHD_OAUTH_DCR: must be on or off")
	}
	if v := getenv("COACHD_NTFY_URL"); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("COACHD_NTFY_URL: must be an absolute http(s) URL")
		}
		// The notification links to the page, which only the public lane serves.
		if mode == PublicAuthNone || c.PublicURL == "" {
			return nil, fmt.Errorf("COACHD_NTFY_URL requires the public lane: set COACHD_PUBLIC_URL and either " +
				"COACHD_ACCESS_TEAM_DOMAIN, COACHD_ACCESS_AUD and COACHD_ACCESS_ALLOWED_EMAIL, or COACHD_PUBLIC_AUTH=builtin " +
				"(the notification links to the page)")
		}
		c.NtfyURL = v
	}
	// Compose always sets the token file path; ignore it unless ntfy is on.
	if p := getenv("COACHD_NTFY_TOKEN_FILE"); p != "" && c.NtfyURL != "" {
		if c.NtfyToken, err = ReadSecretFile(p); err != nil {
			return nil, fmt.Errorf("COACHD_NTFY_TOKEN_FILE: %w", err)
		}
	}
	if v := getenv("COACHD_WEBHOOK_URL"); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("COACHD_WEBHOOK_URL: must be an absolute http(s) URL without user info")
		}
		c.WebhookURL = v
	}
	if p := getenv("COACHD_WEBHOOK_TOKEN_FILE"); p != "" && c.WebhookURL != "" {
		if c.WebhookToken, err = ReadSecretFile(p); err != nil {
			return nil, fmt.Errorf("COACHD_WEBHOOK_TOKEN_FILE: %w", err)
		}
	}

	// Intervals must be positive (time.NewTicker panics otherwise); a delay may be zero.
	for _, d := range []struct {
		name      string
		def       time.Duration
		dst       *time.Duration
		allowZero bool
	}{
		{"COACHD_POLL_INTERVAL", time.Hour, &c.PollInterval, false},
		{"COACHD_BACKFILL_DELAY", time.Second, &c.BackfillDelay, true},
		{"COACHD_SYNC_MIN_INTERVAL", 10 * time.Minute, &c.SyncMinInterval, false},
	} {
		*d.dst = d.def
		if v := getenv(d.name); v != "" {
			if *d.dst, err = time.ParseDuration(v); err != nil {
				return nil, fmt.Errorf("%s: %w", d.name, err)
			}
		}
		switch {
		case *d.dst < 0:
			return nil, fmt.Errorf("%s: must not be negative", d.name)
		case *d.dst == 0 && !d.allowZero:
			return nil, fmt.Errorf("%s: must be positive", d.name)
		}
	}

	c.BackfillDays = 730
	if v := getenv("COACHD_BACKFILL_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("COACHD_BACKFILL_DAYS: must be a positive integer")
		}
		c.BackfillDays = n
	}

	if p := getenv("COACHD_GARMIN_API_KEY_FILE"); p != "" {
		if c.GarminAPIKey, err = ReadSecretFile(p); err != nil {
			return nil, fmt.Errorf("COACHD_GARMIN_API_KEY_FILE: %w", err)
		}
	}
	if v := getenv("COACHD_GARMIN_KEY_EXPIRES"); v != "" {
		if c.GarminKeyExpires, err = civil.Parse(v); err != nil {
			return nil, fmt.Errorf("COACHD_GARMIN_KEY_EXPIRES: %w", err)
		}
	}
	return c, nil
}

// ParsePublicURL validates COACHD_PUBLIC_URL: https://host[:port], or plain
// http only for localhost and 127.0.0.1, with no userinfo, empty port, path,
// query or fragment. It returns the value with the host lowercased (the
// WebAuthn origin and RP ID must agree) and no trailing slash; "" means unset.
func ParsePublicURL(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	bad := fmt.Errorf("COACHD_PUBLIC_URL: must be https://<host>[:port] (http only for localhost or 127.0.0.1) with no path, query or fragment")
	if strings.ContainsAny(v, "?#") {
		return "", bad
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		strings.HasSuffix(u.Host, ":") {
		return "", bad
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
	case "http":
		if h := strings.ToLower(u.Hostname()); h != "localhost" && h != "127.0.0.1" {
			return "", bad
		}
	default:
		return "", bad
	}
	return u.Scheme + "://" + host, nil
}

// ReadSecretFile reads a secret, refusing files readable by group or others.
// Trailing whitespace is trimmed. Errors never include file contents.
func ReadSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file %s has mode %04o; group/other access must be removed (chmod 600)", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), " \t\r\n"), nil
}
