package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "Europe/Bratislava"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.PublicAddr != ":8080" || c.PrivateAddr != ":8081" || c.OpsAddr != ":9090" {
		t.Errorf("addr/dir defaults wrong: %+v", c)
	}
	if c.PollInterval != time.Hour || c.BackfillDays != 730 || c.BackfillDelay != time.Second || c.SyncMinInterval != 10*time.Minute {
		t.Errorf("duration defaults wrong: %+v", c)
	}
	if c.TZ == nil || c.TZ.String() != "Europe/Bratislava" {
		t.Errorf("TZ = %v", c.TZ)
	}
	if c.GarminKeyExpires != (civil.Date{}) {
		t.Errorf("expires should be zero: %v", c.GarminKeyExpires)
	}
}

func TestLoadRequiresTimezone(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil || !strings.Contains(err.Error(), "COACHD_TIMEZONE") {
		t.Fatalf("err = %v", err)
	}
	_, err = Load(env(map[string]string{"COACHD_TIMEZONE": "Not/AZone"}))
	if err == nil || !strings.Contains(err.Error(), "COACHD_TIMEZONE") {
		t.Fatalf("bad zone err = %v", err)
	}
}

func TestLoadAccessAllOrNothing(t *testing.T) {
	base := map[string]string{"COACHD_TIMEZONE": "UTC"}
	c, err := Load(env(base))
	if err != nil || c.AccessTeamDomain != "" {
		t.Fatalf("none set: %v %+v", err, c)
	}
	partial := map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_ACCESS_AUD": "aud"}
	if _, err := Load(env(partial)); err == nil {
		t.Fatal("partial access config should error")
	}
	full := map[string]string{
		"COACHD_TIMEZONE": "UTC", "COACHD_ACCESS_AUD": "aud", "COACHD_PUBLIC_URL": "https://coachd.example.com",
		"COACHD_ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com", "COACHD_ACCESS_ALLOWED_EMAIL": "a@b.c",
	}
	c, err = Load(env(full))
	if err != nil || c.AccessAUD != "aud" || c.AccessAllowedEmail != "a@b.c" {
		t.Fatalf("full: %v %+v", err, c)
	}
}

func TestReadSecretFileRejectsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, []byte("s3cret\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := ReadSecretFile(p)
	if err == nil {
		t.Fatal("0640 should be rejected")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks contents: %v", err)
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		got, err := ReadSecretFile(p)
		if err != nil || got != "s3cret" {
			t.Errorf("mode %o: %q %v", mode, got, err)
		}
	}
	if _, err := ReadSecretFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file should error")
	}
}

func TestLoadGarminKeyFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{
		"COACHD_TIMEZONE": "UTC", "COACHD_GARMIN_API_KEY_FILE": p, "COACHD_GARMIN_KEY_EXPIRES": "2027-01-31",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.GarminAPIKey != "abc123" || c.GarminKeyExpires.String() != "2027-01-31" {
		t.Fatalf("%q %v", c.GarminAPIKey, c.GarminKeyExpires)
	}
}

func TestLoadInvalidValuesNameVariable(t *testing.T) {
	tests := []struct{ k, v string }{
		{"COACHD_POLL_INTERVAL", "soon"},
		{"COACHD_POLL_INTERVAL", "0s"},
		{"COACHD_POLL_INTERVAL", "-1m"},
		{"COACHD_BACKFILL_DELAY", "x"},
		{"COACHD_BACKFILL_DELAY", "-1s"},
		{"COACHD_SYNC_MIN_INTERVAL", "x"},
		{"COACHD_SYNC_MIN_INTERVAL", "0"},
		{"COACHD_SYNC_MIN_INTERVAL", "-10m"},
		{"COACHD_BACKFILL_DAYS", "abc"},
		{"COACHD_BACKFILL_DAYS", "0"},
		{"COACHD_GARMIN_KEY_EXPIRES", "tomorrow"},
		{"COACHD_GARMIN_API_KEY_FILE", "/nonexistent/key"},
	}
	for _, tc := range tests {
		_, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", tc.k: tc.v}))
		if err == nil || !strings.Contains(err.Error(), tc.k) {
			t.Errorf("%s=%s: err = %v", tc.k, tc.v, err)
		}
	}
}

func TestLoadZeroBackfillDelayAllowed(t *testing.T) {
	c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_BACKFILL_DELAY": "0s"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BackfillDelay != 0 {
		t.Fatalf("BackfillDelay = %v, want 0", c.BackfillDelay)
	}
}

// compose passes the tuning variables through as ${VAR:-}: set but empty
// must mean the built-in default, not an error.
func TestLoadEmptyTuningUsesDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_POLL_INTERVAL": "",
		"COACHD_BACKFILL_DAYS": "", "COACHD_BACKFILL_DELAY": "", "COACHD_SYNC_MIN_INTERVAL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != time.Hour || c.BackfillDays != 730 || c.BackfillDelay != time.Second || c.SyncMinInterval != 10*time.Minute {
		t.Errorf("empty tuning did not give defaults: %+v", c)
	}
}

func baseEnv(extra map[string]string) func(string) string {
	m := map[string]string{"COACHD_TIMEZONE": "UTC"}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func TestPublicURLValidation(t *testing.T) {
	ok := map[string]string{
		"https://coachd.example.com":      "https://coachd.example.com",
		"https://coachd.example.com/":     "https://coachd.example.com",
		"https://coachd.example.com:8443": "https://coachd.example.com:8443",
		"http://localhost:8080":           "http://localhost:8080",
		"http://localhost":                "http://localhost",
		"http://127.0.0.1:8080/":          "http://127.0.0.1:8080",
		// The host is lowercased: the origin and the RP ID must agree.
		"https://Coachd.Example.COM": "https://coachd.example.com",
		"HTTPS://coachd.example.com": "https://coachd.example.com",
		"http://LocalHost:8080":      "http://localhost:8080",
	}
	for in, want := range ok {
		c, err := Load(baseEnv(map[string]string{"COACHD_PUBLIC_URL": in}))
		if err != nil {
			t.Errorf("%q: %v", in, err)
		} else if c.PublicURL != want {
			t.Errorf("%q: %q want %q", in, c.PublicURL, want)
		}
	}
	bad := []string{
		"http://coachd.example.com", "ftp://x.example.com", "coachd.example.com", "https://",
		"https://x.example.com/path", "https://x.example.com/?q=1", "https://x.example.com/#f",
		"https://x.example.com?q=1", "http://localhost.evil.com", "https://user@x.example.com",
		"https://x.example.com:", "https://x.example.com:/", "http://localhost:",
	}
	for _, in := range bad {
		_, err := Load(baseEnv(map[string]string{"COACHD_PUBLIC_URL": in}))
		if err == nil || !strings.Contains(err.Error(), "COACHD_PUBLIC_URL") {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	if c, err := Load(baseEnv(nil)); err != nil || c.PublicURL != "" {
		t.Errorf("unset: %v %q", err, c.PublicURL)
	}
}

func TestPublicURLRequiredWithAccess(t *testing.T) {
	access := map[string]string{
		"COACHD_ACCESS_AUD": "aud", "COACHD_ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com", "COACHD_ACCESS_ALLOWED_EMAIL": "a@b.c",
	}
	if _, err := Load(baseEnv(access)); err == nil || !strings.Contains(err.Error(), "COACHD_PUBLIC_URL") {
		t.Fatalf("err = %v", err)
	}
	access["COACHD_PUBLIC_URL"] = "https://coachd.example.com"
	if _, err := Load(baseEnv(access)); err != nil {
		t.Fatal(err)
	}
}

// publicLane is the environment of an enabled public lane.
var publicLane = map[string]string{
	"COACHD_ACCESS_AUD": "aud", "COACHD_ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com", "COACHD_ACCESS_ALLOWED_EMAIL": "a@b.c",
	"COACHD_PUBLIC_URL": "https://coachd.example.com",
}

// withPublicLane returns extra plus the public-lane settings.
func withPublicLane(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range publicLane {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// ntfy links to the page, which only the public lane serves.
func TestNtfyNeedsPublicLane(t *testing.T) {
	const topic = "https://ntfy.example.com/topic"
	for name, extra := range map[string]map[string]string{
		"nothing":         {},
		"public URL only": {"COACHD_PUBLIC_URL": "https://coachd.example.com"},
	} {
		extra["COACHD_NTFY_URL"] = topic
		_, err := Load(baseEnv(extra))
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, v := range []string{"COACHD_NTFY_URL", "COACHD_ACCESS_TEAM_DOMAIN", "COACHD_ACCESS_AUD", "COACHD_ACCESS_ALLOWED_EMAIL", "COACHD_PUBLIC_URL"} {
			if !strings.Contains(err.Error(), v) {
				t.Errorf("%s: error %q does not name %s", name, err, v)
			}
		}
	}
	c, err := Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_URL": topic})))
	if err != nil || c.NtfyURL != topic {
		t.Fatalf("%v %+v", err, c)
	}
	for _, u := range []string{"ntfy.example.com/x", "ftp://x/y", "https://"} {
		_, err := Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_URL": u})))
		if err == nil || !strings.Contains(err.Error(), "COACHD_NTFY_URL") {
			t.Errorf("%q: err = %v", u, err)
		}
	}
}

func TestNtfyEmptyTokenFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ntfy_token")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_TOKEN_FILE": p, "COACHD_NTFY_URL": "https://n.example.com/t"})))
	if err != nil || c.NtfyToken != "" {
		t.Fatalf("%v %q", err, c.NtfyToken)
	}
	if err := os.WriteFile(p, []byte("tk_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_TOKEN_FILE": p, "COACHD_NTFY_URL": "https://n.example.com/t"}))); err != nil || c.NtfyToken != "tk_abc" {
		t.Fatalf("%v %q", err, c.NtfyToken)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_TOKEN_FILE": p, "COACHD_NTFY_URL": "https://n.example.com/t"}))); err == nil || !strings.Contains(err.Error(), "COACHD_NTFY_TOKEN_FILE") {
		t.Fatalf("err = %v", err)
	}
}

func TestNtfyTokenFileReadOnlyWithURL(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Load(baseEnv(map[string]string{"COACHD_NTFY_TOKEN_FILE": missing})); err != nil {
		t.Fatalf("URL unset must ignore the token file: %v", err)
	}
	_, err := Load(baseEnv(withPublicLane(map[string]string{"COACHD_NTFY_TOKEN_FILE": missing,
		"COACHD_NTFY_URL": "https://ntfy.example.com/t"})))
	if err == nil || !strings.Contains(err.Error(), "COACHD_NTFY_TOKEN_FILE") {
		t.Fatalf("err = %v", err)
	}
}

func TestParsePublicURLExported(t *testing.T) {
	if got, err := ParsePublicURL("https://Coachd.Example.com/"); err != nil || got != "https://coachd.example.com" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ParsePublicURL("https://h:"); err == nil || !strings.Contains(err.Error(), "COACHD_PUBLIC_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceAPIConfig(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{{"on", true, false}, {"off", false, false}, {"", false, false}, {"yes", false, true}} {
		c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "Europe/Bratislava", "COACHD_DEVICE_API": tc.val}))
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "COACHD_DEVICE_API") {
				t.Errorf("%q: err = %v, want one naming COACHD_DEVICE_API", tc.val, err)
			}
			continue
		}
		if err != nil || c.DeviceAPI != tc.want {
			t.Errorf("%q: DeviceAPI = %v, err %v, want %v", tc.val, c != nil && c.DeviceAPI, err, tc.want)
		}
	}
}

func TestPublicAuthModes(t *testing.T) {
	const url = "https://c.example.com"
	a3 := map[string]string{
		"COACHD_ACCESS_TEAM_DOMAIN":   "t.cloudflareaccess.com",
		"COACHD_ACCESS_AUD":           "aud",
		"COACHD_ACCESS_ALLOWED_EMAIL": "me@example.com",
	}
	mk := func(parts ...map[string]string) map[string]string {
		m := map[string]string{}
		for _, p := range parts {
			for k, v := range p {
				m[k] = v
			}
		}
		return m
	}
	u := map[string]string{"COACHD_PUBLIC_URL": url}
	builtin := map[string]string{"COACHD_PUBLIC_AUTH": "builtin"}
	cases := []struct {
		name    string
		env     map[string]string
		want    PublicAuth
		dcr     bool
		errHas  string
		wantErr bool
	}{
		{name: "none", env: mk(), want: PublicAuthNone},
		{name: "implicit access", env: mk(a3, u), want: PublicAuthAccess},
		{name: "explicit access", env: mk(a3, u, map[string]string{"COACHD_PUBLIC_AUTH": "access"}), want: PublicAuthAccess},
		{name: "access without url", env: mk(a3, map[string]string{"COACHD_PUBLIC_AUTH": "access"}), wantErr: true, errHas: "COACHD_PUBLIC_URL"},
		{name: "access with two vars", env: mk(u, map[string]string{"COACHD_PUBLIC_AUTH": "access", "COACHD_ACCESS_AUD": "a", "COACHD_ACCESS_TEAM_DOMAIN": "t"}), wantErr: true},
		{name: "builtin", env: mk(builtin, u), want: PublicAuthBuiltin},
		{name: "builtin localhost", env: mk(builtin, map[string]string{"COACHD_PUBLIC_URL": "http://localhost:8080"}), want: PublicAuthBuiltin},
		{name: "builtin 127.0.0.1", env: mk(builtin, map[string]string{"COACHD_PUBLIC_URL": "http://127.0.0.1:8080"}), wantErr: true,
			errHas: "COACHD_PUBLIC_URL: builtin mode needs https://, or http://localhost for development (WebAuthn does not accept an IP address)"},
		{name: "builtin 127.0.0.1 no port", env: mk(builtin, map[string]string{"COACHD_PUBLIC_URL": "http://127.0.0.1"}), wantErr: true,
			errHas: "WebAuthn does not accept an IP address"},
		{name: "access 127.0.0.1", env: mk(a3, map[string]string{"COACHD_PUBLIC_URL": "http://127.0.0.1:8080"}), want: PublicAuthAccess},
		{name: "builtin without url", env: mk(builtin), wantErr: true, errHas: "COACHD_PUBLIC_URL"},
		{name: "builtin with one access var", env: mk(builtin, u, map[string]string{"COACHD_ACCESS_AUD": "a"}), wantErr: true, errHas: "COACHD_PUBLIC_AUTH=builtin: leave COACHD_ACCESS_* empty"},
		{name: "builtin with all access vars", env: mk(builtin, u, a3), wantErr: true, errHas: "leave COACHD_ACCESS_* empty"},
		{name: "unknown mode", env: mk(map[string]string{"COACHD_PUBLIC_AUTH": "cloudflare"}), wantErr: true, errHas: "COACHD_PUBLIC_AUTH"},
		{name: "dcr on", env: mk(builtin, u, map[string]string{"COACHD_OAUTH_DCR": "on"}), want: PublicAuthBuiltin, dcr: true},
		{name: "dcr off", env: mk(builtin, u, map[string]string{"COACHD_OAUTH_DCR": "off"}), want: PublicAuthBuiltin},
		{name: "dcr without builtin", env: mk(a3, u, map[string]string{"COACHD_OAUTH_DCR": "on"}), wantErr: true, errHas: "COACHD_OAUTH_DCR"},
		{name: "dcr bad value", env: mk(builtin, u, map[string]string{"COACHD_OAUTH_DCR": "yes"}), wantErr: true, errHas: "COACHD_OAUTH_DCR: must be on or off"},
		{name: "ntfy with builtin", env: mk(builtin, u, map[string]string{"COACHD_NTFY_URL": "https://ntfy.sh/x"}), want: PublicAuthBuiltin},
		{name: "ntfy with neither", env: mk(u, map[string]string{"COACHD_NTFY_URL": "https://ntfy.sh/x"}), wantErr: true, errHas: "COACHD_NTFY_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(baseEnv(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("no error, cfg %+v", c)
				}
				if tc.errHas != "" && !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("error %q lacks %q", err, tc.errHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.PublicAuth != tc.want || c.OAuthDCR != tc.dcr {
				t.Fatalf("PublicAuth=%q OAuthDCR=%v, want %q %v", c.PublicAuth, c.OAuthDCR, tc.want, tc.dcr)
			}
		})
	}
}

func TestWebhookConfig(t *testing.T) {
	base := map[string]string{"COACHD_TIMEZONE": "UTC"}
	c, err := Load(env(base))
	if err != nil || c.WebhookURL != "" {
		t.Fatalf("unset: %v %q", err, c.WebhookURL)
	}
	dir := t.TempDir()
	tok := filepath.Join(dir, "webhook_token")
	if err := os.WriteFile(tok, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://n8n.example.com/webhook/abc", "http://homeassistant.local:8123/api/webhook/xyz"} {
		c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_WEBHOOK_URL": u, "COACHD_WEBHOOK_TOKEN_FILE": tok}))
		if err != nil || c.WebhookURL != u || c.WebhookToken != "secret-token" {
			t.Fatalf("%s: %v %q %q", u, err, c.WebhookURL, c.WebhookToken)
		}
	}
	for _, u := range []string{"ftp://x.example.com/h", "/relative", "https://user:pw@x.example.com/h", "https://"} {
		_, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_WEBHOOK_URL": u}))
		if err == nil || !strings.Contains(err.Error(), "COACHD_WEBHOOK_URL") {
			t.Errorf("%q: err = %v", u, err)
		} else if strings.Contains(err.Error(), "pw") {
			t.Errorf("%q: the error quotes the URL: %v", u, err)
		}
	}
	// A token file without a URL is ignored, as compose may set the path.
	if c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "UTC", "COACHD_WEBHOOK_TOKEN_FILE": "/missing"})); err != nil || c.WebhookToken != "" {
		t.Fatalf("token without URL: %v", err)
	}
}

func TestDemoConfig(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{{"true", true, false}, {"false", false, false}, {"", false, false}, {"on", false, true}, {"1", false, true}} {
		c, err := Load(env(map[string]string{"COACHD_TIMEZONE": "Europe/Bratislava", "COACHD_DEMO": tc.val}))
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "COACHD_DEMO") {
				t.Errorf("%q: err = %v, want one naming COACHD_DEMO", tc.val, err)
			}
			continue
		}
		if err != nil || c.Demo != tc.want {
			t.Errorf("%q: Demo = %v, err %v, want %v", tc.val, c != nil && c.Demo, err, tc.want)
		}
	}
}
