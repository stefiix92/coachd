package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	alertsPath       = "grafana/alerts/coachd-alerts.yaml"
	dashboardPath    = "grafana/dashboards/coachd.json"
	composeLocalPath = "compose.local.yaml"
	servePath        = "tailscale/serve.json"
	serveLocalPath   = "tailscale/serve-local.json"
)

func nonCommentLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestAlertRulesNoPlaceholdersOrDollar(t *testing.T) {
	for i, l := range nonCommentLines(t, alertsPath) {
		if strings.Contains(l, "REPLACE_WITH") || strings.Contains(l, "$") {
			t.Errorf("line %d has a placeholder or $: %q", i+1, l)
		}
	}
}

func TestAlertRulesCoverSpec(t *testing.T) {
	lines := nonCommentLines(t, alertsPath)
	for _, title := range []string{"GarminSyncFailing", "GarminKeyExpiring", "AuthFailureSpike", "DataDiskFull", "WorkoutDeliveryFailed", "BackupStale"} {
		n := 0
		for _, l := range lines {
			if strings.TrimSpace(l) == "title: "+title {
				n++
			}
		}
		if n != 1 {
			t.Errorf("rule %s appears %d times, want 1", title, n)
		}
	}
}

func TestAlertRulesFileHasNoNotifications(t *testing.T) {
	for _, l := range nonCommentLines(t, alertsPath) {
		if strings.HasPrefix(l, "contactPoints:") || strings.HasPrefix(l, "policies:") {
			t.Errorf("rules file must not hold %q", l)
		}
	}
}

// rules returns each alert rule's trimmed non-comment lines by title.
func rules(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	var cur []string
	flush := func() {
		for _, l := range cur {
			if title, ok := strings.CutPrefix(l, "title: "); ok {
				out[title] = cur
			}
		}
	}
	for _, l := range nonCommentLines(t, alertsPath) {
		v := strings.TrimSpace(l)
		if strings.HasPrefix(v, "- uid: ") {
			flush()
			cur = nil
		}
		cur = append(cur, v)
	}
	flush()
	return out
}

func TestAlertRulesDataSources(t *testing.T) {
	n := 0
	for i, l := range nonCommentLines(t, alertsPath) {
		v, ok := strings.CutPrefix(strings.TrimSpace(l), "datasourceUid: ")
		if !ok {
			continue
		}
		n++
		if v != "coachd-vm" && v != "__expr__" {
			t.Errorf("line %d: datasourceUid %q, want coachd-vm or __expr__", i+1, v)
		}
	}
	if n == 0 {
		t.Fatal("no datasourceUid lines")
	}
}

// TestAlertRuleSemantics pins the thresholds and no-data handling the
// docs' alert table (docs/content/docs/operations/monitoring.md) documents.
func TestAlertRuleSemantics(t *testing.T) {
	rs := rules(t)
	for title, want := range map[string][]string{
		// An intervals.icu outage alone must not fire an alert named for Garmin.
		"GarminSyncFailing": {`expr: 'max(coachd_garmin_sync_last_success_age_seconds{data_type!="intervals_activities"})'`,
			"- evaluator: {type: gt, params: [10800]}", "noDataState: Alerting"},
		"GarminKeyExpiring": {"- evaluator: {type: lt, params: [8]}"},
		"BackupStale":       {"- evaluator: {type: gt, params: [172800]}", "for: 10m", "noDataState: Alerting"},
	} {
		r, ok := rs[title]
		if !ok {
			t.Errorf("no rule %s", title)
			continue
		}
		for _, w := range want {
			if !contains(r, w) {
				t.Errorf("%s lacks %q", title, w)
			}
		}
	}
}

func TestDashboardBackupAgeReducesLastNotNull(t *testing.T) {
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Options struct {
				ReduceOptions struct {
					Calcs []string `json:"calcs"`
				} `json:"reduceOptions"`
			} `json:"options"`
		} `json:"panels"`
	}
	b, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Panels {
		if p.Title == "Last backup age" {
			if got := p.Options.ReduceOptions.Calcs; !reflect.DeepEqual(got, []string{"lastNotNull"}) {
				t.Errorf("reduceOptions.calcs %q, want [lastNotNull]", got)
			}
			return
		}
	}
	t.Error("no top-level \"Last backup age\" panel")
}

type panel struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	GridPos struct {
		H, W, X, Y int
	} `json:"gridPos"`
	Targets []struct {
		Datasource struct {
			UID string `json:"uid"`
		} `json:"datasource"`
	} `json:"targets"`
	Panels []panel `json:"panels"`
}

func flatten(ps []panel) []panel {
	var out []panel
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatten(p.Panels)...)
	}
	return out
}

func TestDashboard(t *testing.T) {
	b, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []panel `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	all := flatten(d.Panels)
	ids := map[int]bool{}
	titles := map[string]bool{}
	for _, p := range all {
		if ids[p.ID] {
			t.Errorf("duplicate panel id %d", p.ID)
		}
		ids[p.ID] = true
		titles[p.Title] = true
		for _, tg := range p.Targets {
			switch tg.Datasource.UID {
			case "${ds_prometheus}", "${ds_vlogs}", "${ds_tempo}":
			default:
				t.Errorf("panel %d %q: target datasource %q", p.ID, p.Title, tg.Datasource.UID)
			}
		}
	}
	for i, a := range all {
		for _, c := range all[i+1:] {
			ag, cg := a.GridPos, c.GridPos
			if ag.X < cg.X+cg.W && cg.X < ag.X+ag.W && ag.Y < cg.Y+cg.H && cg.Y < ag.Y+ag.H {
				t.Errorf("panels %d %q and %d %q overlap", a.ID, a.Title, c.ID, c.Title)
			}
		}
	}
	for _, want := range []string{"Last backup age", "Backup runs by outcome", "Proposal warnings by reason", "Device API requests by endpoint and outcome", "Watch last seen"} {
		if !titles[want] {
			t.Errorf("missing panel %q", want)
		}
	}
}

// TestDashboardHasOAuthPanel: the builtin auth panel (spec §9) charts
// coachd.oauth.requests by endpoint and outcome.
func TestDashboardHasOAuthPanel(t *testing.T) {
	b, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Panels {
		if p.Title != "OAuth requests by endpoint and outcome" {
			continue
		}
		if len(p.Targets) != 1 || !strings.Contains(p.Targets[0].Expr, "coachd_oauth_requests_total") ||
			!strings.Contains(p.Targets[0].Expr, "by (endpoint, outcome)") {
			t.Fatalf("targets %+v", p.Targets)
		}
		return
	}
	t.Fatal(`no top-level "OAuth requests by endpoint and outcome" panel`)
}

// TestComposeForwardsEnvExample: every COACHD_ setting in .env.example
// reaches compose.yaml (coachd only sees the variables compose forwards),
// and the builtin auth settings are documented.
func TestComposeForwardsEnvExample(t *testing.T) {
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, l := range strings.Split(string(env), "\n") {
		k, _, ok := strings.Cut(strings.TrimPrefix(l, "#"), "=")
		if ok && strings.HasPrefix(k, "COACHD_") && !strings.ContainsAny(k, " \t") {
			keys[k] = true
			if !strings.Contains(string(compose), "${"+k) {
				t.Errorf("%s is in .env.example but compose.yaml does not forward it", k)
			}
		}
	}
	for _, want := range []string{"COACHD_PUBLIC_AUTH", "COACHD_OAUTH_DCR"} {
		if !keys[want] {
			t.Errorf(".env.example lacks %s", want)
		}
	}
}

func indent(l string) int {
	return len(l) - len(strings.TrimLeft(l, " "))
}

// TestOnlyCoachdPublishesAPort: across every compose file, the only item under
// any ports: key is coachd's loopback publish for the host cloudflared.
func TestOnlyCoachdPublishesAPort(t *testing.T) {
	const allowed = `"127.0.0.1:8080:8080"`
	files, err := filepath.Glob("compose*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("found compose files %v, want compose.yaml, compose.cloud.yaml and compose.local.yaml", files)
	}
	published := 0
	for _, f := range files {
		lines := nonCommentLines(t, f)
		for i, l := range lines {
			key := strings.TrimSpace(l)
			if !strings.HasPrefix(key, "ports:") {
				continue
			}
			if rest := strings.TrimSpace(strings.TrimPrefix(key, "ports:")); rest != "" {
				t.Errorf("%s: inline ports %q: only a block list with %s is allowed", f, rest, allowed)
			}
			for _, item := range lines[i+1:] {
				v := strings.TrimSpace(item)
				if v == "" {
					continue
				}
				// The block ends at the first line that is neither deeper than the key
				// nor a list item at the key's own indentation.
				if indent(item) < indent(l) || indent(item) == indent(l) && !strings.HasPrefix(v, "- ") {
					break
				}
				if v = strings.TrimSpace(strings.TrimPrefix(v, "- ")); v != allowed {
					t.Errorf("%s: published port %q, only %s is allowed", f, v, allowed)
					continue
				}
				published++
			}
		}
	}
	if published != 1 {
		t.Errorf("%s published %d times, want exactly once (coachd)", allowed, published)
	}
}

// block returns the raw non-comment lines nested under one entry of a
// top-level section, e.g. services/grafana or networks/grafana-data.
func block(t *testing.T, path, section, name string) []string {
	t.Helper()
	lines := nonCommentLines(t, path)
	inSection, entryIndent := false, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if indent(l) == 0 {
			inSection, entryIndent = l == section+":", -1
			continue
		}
		if !inSection {
			continue
		}
		if entryIndent < 0 {
			entryIndent = indent(l)
		}
		if indent(l) != entryIndent || strings.TrimSpace(l) != name+":" {
			continue
		}
		var out []string
		for _, b := range lines[i+1:] {
			if strings.TrimSpace(b) != "" && indent(b) <= entryIndent {
				break
			}
			out = append(out, b)
		}
		return out
	}
	t.Fatalf("%s: no %s entry %q", path, section, name)
	return nil
}

// keys returns the block's own keys (trimmed lines at its first line's
// indentation), not keys nested deeper.
func keys(b []string) []string {
	ind := -1
	var out []string
	for _, l := range b {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if ind < 0 {
			ind = indent(l)
		}
		if indent(l) == ind {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// sub returns the trimmed lines nested under the block's own key k.
func sub(b []string, k string) []string {
	ind := -1
	for i, l := range b {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if ind < 0 {
			ind = indent(l)
		}
		if indent(l) != ind || strings.TrimSpace(l) != k+":" {
			continue
		}
		var out []string
		for _, s := range b[i+1:] {
			v := strings.TrimSpace(s)
			if v == "" {
				continue
			}
			if indent(s) < ind || indent(s) == ind && !strings.HasPrefix(v, "- ") {
				break
			}
			out = append(out, v)
		}
		return out
	}
	return nil
}

func contains(ls []string, want string) bool {
	for _, l := range ls {
		if l == want {
			return true
		}
	}
	return false
}

func TestLocalGrafanaHardened(t *testing.T) {
	b := block(t, composeLocalPath, "services", "grafana")
	ks := keys(b)
	for _, want := range []string{"read_only: true", "cap_drop: [ALL]", `user: "472:472"`} {
		if !contains(ks, want) {
			t.Errorf("grafana service lacks the key %q", want)
		}
	}
	var noNewPrivs, memLimit, pinned bool
	for _, k := range ks {
		noNewPrivs = noNewPrivs || strings.HasPrefix(k, "security_opt:") && strings.Contains(k, "no-new-privileges:true")
		memLimit = memLimit || strings.HasPrefix(k, "mem_limit:")
		pinned = pinned || strings.HasPrefix(k, "image:") && strings.Contains(k, "@sha256:")
	}
	if !noNewPrivs {
		t.Error("grafana service lacks security_opt no-new-privileges:true")
	}
	if !memLimit {
		t.Error("grafana service lacks mem_limit")
	}
	if !pinned {
		t.Error("grafana image is not pinned by digest (@sha256:)")
	}
	for _, l := range b {
		if strings.HasPrefix(strings.TrimSpace(l), "ports:") {
			t.Errorf("grafana service publishes ports: %q", strings.TrimSpace(l))
		}
	}
	logging := sub(b, "logging")
	for _, want := range []string{"driver: local", `max-size: "10m"`, `max-file: "3"`} {
		if !contains(logging, want) {
			t.Errorf("grafana logging block %q lacks %q", logging, want)
		}
	}
	if tmpfs := sub(b, "tmpfs"); !reflect.DeepEqual(tmpfs, []string{"- /tmp:size=16m,mode=1777"}) {
		t.Errorf("grafana tmpfs %q, want [- /tmp:size=16m,mode=1777]", tmpfs)
	}
}

// TestLocalGrafanaNetworks: Grafana reaches VM and VL on grafana-data and is
// reached by tailscale on grafana-ui; tailscale never joins grafana-data and
// VM/VL never join grafana-ui or an egress network. Both are internal.
func TestLocalGrafanaNetworks(t *testing.T) {
	want := map[string]string{
		"grafana":         "networks: [grafana-data, grafana-ui, grafana-egress]",
		"victoriametrics": "networks: [internal, grafana-data]",
		"victorialogs":    "networks: [internal, grafana-data]",
		"tailscale":       "networks: [internal, ts-egress, grafana-ui]",
	}
	for svc, w := range want {
		var got []string
		for _, k := range keys(block(t, composeLocalPath, "services", svc)) {
			if strings.HasPrefix(k, "networks:") {
				got = append(got, k)
			}
		}
		if len(got) != 1 || got[0] != w {
			t.Errorf("%s: networks %q, want [%q]", svc, got, w)
		}
	}
	for _, n := range []string{"grafana-data", "grafana-ui"} {
		if got := keys(block(t, composeLocalPath, "networks", n)); !reflect.DeepEqual(got, []string{"internal: true"}) {
			t.Errorf("network %s: %q, want [internal: true]", n, got)
		}
	}
}

// TestNoNetworkMode: no compose file sets network_mode. host (or another
// container's namespace) would bypass the network isolation and the no-ports rule.
func TestNoNetworkMode(t *testing.T) {
	files, err := filepath.Glob("compose*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		for _, l := range nonCommentLines(t, f) {
			if strings.HasPrefix(strings.TrimSpace(l), "network_mode:") {
				t.Errorf("%s: %q", f, strings.TrimSpace(l))
			}
		}
	}
}

// TestEntrypointURLClassHasNoDollar: Grafana expands $VAR in provisioning
// values, so a $ in the alert URL could pull an env var (the admin password)
// into the webhook URL.
func TestEntrypointURLClassHasNoDollar(t *testing.T) {
	n := 0
	for _, l := range nonCommentLines(t, "grafana/local/entrypoint.sh") {
		i := strings.Index(l, "^https://[")
		if i < 0 {
			continue
		}
		n++
		class := l[i+len("^https://["):]
		if j := strings.Index(class, "]+"); j >= 0 {
			class = class[:j]
		}
		if strings.Contains(class, "$") {
			t.Errorf("URL character class allows $: %q", class)
		}
	}
	if n != 1 {
		t.Errorf("found %d URL validation patterns, want 1", n)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func hasKey(v any, key string) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, c := range v {
			if k == key || hasKey(c, key) {
				return true
			}
		}
	case []any:
		for _, c := range v {
			if hasKey(c, key) {
				return true
			}
		}
	}
	return false
}

// handlers returns the Handlers map of the single Web host in a serve config.
func handlers(t *testing.T, path string, cfg map[string]any) map[string]any {
	t.Helper()
	web, _ := cfg["Web"].(map[string]any)
	if len(web) != 1 {
		t.Fatalf("%s: want one Web host, got %d", path, len(web))
	}
	for _, host := range web {
		h, _ := host.(map[string]any)["Handlers"].(map[string]any)
		return h
	}
	return nil
}

func proxyOf(h map[string]any, mount string) string {
	m, _ := h[mount].(map[string]any)
	p, _ := m["Proxy"].(string)
	return p
}

func TestServeLocalKeepsMCPAndNoFunnel(t *testing.T) {
	local := readJSON(t, serveLocalPath)
	if hasKey(local, "AllowFunnel") {
		t.Error("serve-local.json has an AllowFunnel key")
	}
	h := handlers(t, serveLocalPath, local)
	if p := proxyOf(h, "/"); p != "http://coachd:8081" {
		t.Errorf(`"/" proxies to %q, want http://coachd:8081`, p)
	}
	if p := proxyOf(h, "/grafana/"); !strings.HasPrefix(p, "http://grafana:3000/") {
		t.Errorf(`"/grafana/" proxies to %q, want http://grafana:3000/...`, p)
	}
	// Apart from the /grafana/ handler, serve-local.json is serve.json.
	delete(h, "/grafana/")
	if base := readJSON(t, servePath); !reflect.DeepEqual(local, base) {
		t.Errorf("serve-local.json without /grafana/ differs from serve.json:\n%v\n%v", local, base)
	}
}

func uids(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, l := range nonCommentLines(t, path) {
		v := strings.TrimPrefix(strings.TrimSpace(l), "- ")
		if u, ok := strings.CutPrefix(v, "uid: "); ok {
			out = append(out, u)
		}
	}
	return out
}

// TestNotificationsOffDeletesTemplateContactPoint: with an empty alert URL the
// entrypoint deletes the contact point the template creates, by the same UID.
func TestNotificationsOffDeletesTemplateContactPoint(t *testing.T) {
	tmpl := uids(t, "grafana/local/notifications.yaml.tmpl")
	off := uids(t, "grafana/local/entrypoint.sh")
	if len(tmpl) != 1 || !reflect.DeepEqual(off, tmpl) {
		t.Errorf("entrypoint deletes contact points %q, template creates %q", off, tmpl)
	}
}

// runEntrypoint runs grafana/local/entrypoint.sh with sh against a copy of
// the mounted files under a temp dir, the alert URL secret holding secret,
// and Grafana's /run.sh replaced by exit 0. It returns the script's stderr,
// the assembled alerting directory and the exit error.
func runEntrypoint(t *testing.T, secret string) (stderr, alerting string, err error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	abs := func(rel string) string {
		p, err := filepath.Abs(rel)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for link, target := range map[string]string{
		"provisioning":              "grafana/local/provisioning",
		"notifications.yaml.tmpl":   "grafana/local/notifications.yaml.tmpl",
		"alerts/coachd-alerts.yaml": alertsPath,
	} {
		p := filepath.Join(src, link)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(abs(target), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "secret"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("grafana/local/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for old, repl := range map[string]string{
		"src=/etc/coachd-grafana\n":               "src=" + src + "\n",
		"dst=/tmp/provisioning\n":                 "dst=" + filepath.Join(tmp, "dst") + "\n",
		"secret=/run/secrets/grafana_alert_url\n": "secret=" + filepath.Join(tmp, "secret") + "\n",
		"exec /run.sh \"$@\"\n":                   "exit 0\n",
	} {
		if strings.Count(script, old) != 1 {
			t.Fatalf("entrypoint.sh: want exactly one %q", old)
		}
		script = strings.Replace(script, old, repl, 1)
	}
	var errb bytes.Buffer
	cmd := exec.Command(sh, "-c", script)
	cmd.Stderr = &errb
	err = cmd.Run()
	return errb.String(), filepath.Join(tmp, "dst", "alerting"), err
}

const badAlertURL = "coachd: secrets/grafana_alert_url must be one https:// URL (see https://stefiix92.github.io/coachd/docs/operations/monitoring/)\n"

func TestEntrypointAlertURL(t *testing.T) {
	const url = "https://ntfy.example/topic-abc?x=1"
	for name, secret := range map[string]string{
		"bare":                   url,
		"surrounding whitespace": " \t" + url + " \r\n",
		"blank lines around":     "\n\n" + url + "\r\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			stderr, alerting, err := runEntrypoint(t, secret)
			if err != nil {
				t.Fatalf("exit %v, stderr %q", err, stderr)
			}
			b, err := os.ReadFile(filepath.Join(alerting, "notifications.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "'"+url+"'") || strings.Contains(string(b), "__COACHD_ALERT_URL__") {
				t.Fatalf("notifications.yaml does not carry the URL:\n%s", b)
			}
		})
	}
	for name, secret := range map[string]string{
		"internal space": "https://ntfy.example/x y",
		"internal tab":   "https://ntfy.example/x\ty",
		"http":           "http://ntfy.example/x",
		"two lines":      url + "\n" + url + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			stderr, alerting, err := runEntrypoint(t, secret)
			if err == nil || stderr != badAlertURL {
				t.Fatalf("exit %v, stderr %q, want exit 1 with %q", err, stderr, badAlertURL)
			}
			if _, err := os.Stat(filepath.Join(alerting, "notifications.yaml")); !os.IsNotExist(err) {
				t.Fatalf("notifications.yaml written (err=%v)", err)
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		stderr, alerting, err := runEntrypoint(t, " \r\n")
		if err != nil || !strings.Contains(stderr, "alert notifications are off") {
			t.Fatalf("exit %v, stderr %q", err, stderr)
		}
		if _, err := os.Stat(filepath.Join(alerting, "notifications-off.yaml")); err != nil {
			t.Fatal(err)
		}
	})
}

// TestResticEnvModeWarning runs restic/restic.sh with a stub docker: a
// restic.env readable by group or others draws a warning on stderr (not
// fatal, contents never printed); mode 0600 draws none.
func TestResticEnvModeWarning(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script, err := filepath.Abs("restic/restic.sh")
	if err != nil {
		t.Fatal(err)
	}
	const warning = "restic.sh: warning: restic.env is readable by group or others; run chmod 0600 restic.env\n"
	for mode, want := range map[os.FileMode]string{0o600: "", 0o640: warning, 0o604: warning, 0o644: warning} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		for _, d := range []string{bin, filepath.Join(dir, "secrets")} {
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		env := filepath.Join(dir, "restic.env")
		if err := os.WriteFile(env, []byte("RESTIC_REPOSITORY=s3:example/repo-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(env, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "secrets", "restic_password"), []byte("pw"), 0o600); err != nil {
			t.Fatal(err)
		}
		var errb bytes.Buffer
		cmd := exec.Command(sh, script, "snapshots")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("mode %o: %v, stderr %q", mode, err, errb.String())
		}
		if errb.String() != want {
			t.Errorf("mode %o: stderr %q, want %q", mode, errb.String(), want)
		}
	}
}
