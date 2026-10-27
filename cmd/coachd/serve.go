package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/backup"
	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/config"
	"github.com/stefiix92/coachd/internal/demo"
	"github.com/stefiix92/coachd/internal/deviceapi"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/garminkey"
	"github.com/stefiix92/coachd/internal/ingest"
	"github.com/stefiix92/coachd/internal/mcpserver"
	"github.com/stefiix92/coachd/internal/notify"
	"github.com/stefiix92/coachd/internal/oauth"
	"github.com/stefiix92/coachd/internal/passkey"
	"github.com/stefiix92/coachd/internal/proposals"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/web"
	"github.com/stefiix92/coachd/internal/workout"
)

const (
	shutdownTimeout = 10 * time.Second
	auditRetention  = 365 * 24 * time.Hour
	auditPurgeEvery = 24 * time.Hour

	backupCheckEvery = time.Hour

	proposalExpiryEvery = 10 * time.Minute

	auditQueueSize    = 256
	auditDrainTimeout = 5 * time.Second

	// auditSamplePerHour caps sampled auth failure audit rows in builtin
	// mode (spec §6, I1).
	auditSamplePerHour = 60
)

// Addrs are the addresses the listeners actually bound; Public is empty when
// the public lane is disabled.
type Addrs struct{ Public, Private, Ops string }

// serveMain runs `coachd serve` with the real environment and signals.
func serveMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second signal then kills the process
	cfg, err := config.Load(os.Getenv)
	if err == nil {
		err = validateServeConfig(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "coachd serve:", err)
		return 1
	}
	tel, err := telemetry.Setup(ctx, version, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coachd serve: telemetry setup failed")
		return 1
	}
	keys, g, err := garminSource(cfg, tel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coachd serve:", err)
		return 1
	}
	code := 0
	if err := runServe(ctx, cfg, g, keys, tel, nil); err != nil {
		tel.Logger.Error("serve failed", "component", "serve", "error_class", fmt.Sprintf("%T", err))
		fmt.Fprintln(os.Stderr, "coachd serve:", err)
		code = 1
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tel.Shutdown(sctx)
	return code
}

// garminSource returns the Garmin key holder and the Garmin that ingest
// reads: in demo mode a keyless holder and the synthetic Garmin, which reads
// nothing from the data directory; otherwise the configured key, or one
// renewed on the page, whichever expires later, and the Garmin client.
func garminSource(cfg *config.Config, tel *telemetry.Telemetry) (*garminkey.Holder, ingest.Garmin, error) {
	if cfg.Demo {
		return garminkey.NoKey(demo.KeyExpires), demo.New(cfg.TZ, time.Now), nil
	}
	keys, err := garminkey.Load(cfg.DataDir, garminkey.Key{Value: cfg.GarminAPIKey, Expires: cfg.GarminKeyExpires})
	if err != nil {
		return nil, nil, err
	}
	if today := civil.Of(time.Now(), cfg.TZ); keys.Expires().Before(today) {
		tel.Logger.Warn("garmin api key expired", "component", "serve", "calendar_date", keys.Expires().String())
	}
	return keys, garmin.New(garmin.DefaultBaseURL, keys.Value(), nil, tel.Metrics), nil
}

// validateServeConfig checks the settings only serve needs. Demo mode needs
// no Garmin key and refuses one, so a real deployment's settings never start
// a demo by mistake.
func validateServeConfig(cfg *config.Config) error {
	if cfg.Demo {
		if cfg.GarminAPIKey != "" || cfg.GarminKeyExpires != (civil.Date{}) {
			return errors.New("COACHD_DEMO=true: leave COACHD_GARMIN_API_KEY_FILE and COACHD_GARMIN_KEY_EXPIRES unset (demo mode never calls Garmin)")
		}
		return nil
	}
	if cfg.GarminAPIKey == "" {
		return errors.New("COACHD_GARMIN_API_KEY_FILE is required (file holding the Garmin Personal API key)")
	}
	if cfg.GarminKeyExpires == (civil.Date{}) {
		return errors.New("COACHD_GARMIN_KEY_EXPIRES is required (YYYY-MM-DD)")
	}
	return nil
}

// runServe wires the store, ingest, MCP and listeners and blocks until ctx is
// cancelled (clean shutdown returns nil) or a listener fails. ready, if
// non-nil, receives the bound addresses once every listener accepts.
// keys holds the Garmin key g uses; the page renews it when g can exchange
// a new key (garminkey.Exchanger).
func runServe(ctx context.Context, cfg *config.Config, g ingest.Garmin, keys *garminkey.Holder, tel *telemetry.Telemetry, ready func(Addrs)) error {
	log := tel.Logger.With("component", "serve")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	// Held until return (after db.Close), so a restore cannot swap the
	// database under a running serve.
	releaseData, err := backup.LockData(cfg.DataDir)
	if err != nil {
		if errors.Is(err, backup.ErrLocked) {
			return fmt.Errorf("data directory %s is in use by another coachd process (serve or restore)", cfg.DataDir)
		}
		return fmt.Errorf("lock data dir: %w", err)
	}
	defer releaseData()
	dbPath := filepath.Join(cfg.DataDir, "coachd.db")
	if err := checkDataMode(ctx, cfg.DataDir, dbPath, cfg.Demo); err != nil {
		return err
	}
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()
	if cfg.Demo {
		if err := db.MarkDemo(ctx); err != nil {
			return err
		}
		log.Warn("demo mode: synthetic Garmin data, no Garmin calls")
	}
	// Outside builtin mode no OAuth grant may survive (spec §3, M13):
	// switching to Access and back never revives one.
	if cfg.PublicAuth != config.PublicAuthBuiltin {
		n, err := db.DeleteOAuthState(ctx)
		if err != nil {
			return fmt.Errorf("delete oauth state: %w", err)
		}
		log.Info("oauth state deleted", "count", n)
	}
	aq := store.NewAuditQueue(db, tel.Logger, auditQueueSize)
	closeAudit := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), auditDrainTimeout)
		defer ccancel()
		if err := aq.Close(cctx); err != nil {
			log.Warn("audit queue drain failed", "error_class", fmt.Sprintf("%T", err))
		}
	}
	defer closeAudit() // runs before db.Close on early returns; Close is idempotent

	if err := telemetry.RegisterGauges(otel.GetMeterProvider(),
		ingest.NewGauges(db, keys.Expires, cfg.TZ, cfg.DataDir, time.Now)); err != nil {
		return fmt.Errorf("register gauges: %w", err)
	}

	ingestOpts := ingest.Options{
		TZ: cfg.TZ, DataDir: cfg.DataDir, BackfillDays: cfg.BackfillDays, BackfillDelay: cfg.BackfillDelay,
		SyncMinInterval: cfg.SyncMinInterval, PollInterval: cfg.PollInterval, Now: time.Now,
	}
	var webhook *notify.Webhook
	if cfg.WebhookURL != "" {
		webhook = notify.NewWebhook(cfg.WebhookURL, cfg.WebhookToken, db, nil, tel.Metrics, tel.Logger, tel.Tracer)
		ingestOpts.Events = webhook // only when configured: a nil *Webhook in the interface is not nil
		if strings.HasPrefix(cfg.WebhookURL, "http://") {
			log.Warn("webhook URL is plain http: its token travels in clear text")
		}
	}
	svc := ingest.New(db, g, tel, ingestOpts)
	propDeps := proposals.Deps{DB: db, TZ: cfg.TZ, Now: time.Now, Limits: workout.DefaultLimits, Log: tel.Logger, Metrics: tel.Metrics}
	var ntfy *notify.Ntfy
	if cfg.NtfyURL != "" {
		ntfy = notify.NewNtfy(cfg.NtfyURL, cfg.NtfyToken, cfg.PublicURL, nil, tel.Metrics, tel.Logger, tel.Tracer)
		propDeps.Notifier = ntfy // only when configured: a nil *Ntfy in the interface is not nil
	} else {
		log.Info("ntfy disabled")
	}
	propSvc := proposals.New(propDeps)
	mcpHandler := mcpserver.Handler(mcpserver.New(mcpserver.Deps{
		DB: db, Sync: svc, Proposals: propSvc, TZ: cfg.TZ, Now: time.Now, KeyExpires: keys.Expires,
		DataDir: cfg.DataDir, Metrics: tel.Metrics, Audit: aq, Log: tel.Logger, Tracer: tel.Tracer,
	}, version))
	// The page has its own limiter: Managed OAuth agents share the runner's
	// Access subject, and their /mcp traffic must not starve the page.
	mcpLim, webLim := auth.NewLimiter(120, 30), auth.NewLimiter(120, 30)
	lane := func(l auth.Lane, verify func(*http.Request) (auth.Identity, error), webHandler http.Handler) http.Handler {
		if webHandler != nil {
			webHandler = auth.Middleware(l, verify, webLim, aq, tel.Metrics)(webHandler)
		}
		return laneMux(l, auth.Middleware(l, verify, mcpLim, aq, tel.Metrics)(mcpHandler), webHandler, otel.GetMeterProvider())
	}

	type listener struct {
		name, addr string
		h          http.Handler
	}
	var ls []listener
	// The public page, in builtin mode also the public /mcp and the OAuth
	// endpoints; nil when the page is not served. config.Load resolves the
	// three Access settings to PublicAuthAccess and refuses them in builtin
	// mode, so the two branches below never both apply.
	var page, publicMCP, oauthHandler http.Handler
	builtin := cfg.PublicAuth == config.PublicAuthBuiltin
	if builtin || cfg.AccessTeamDomain != "" {
		// The ceremony key exists only when the page is served; in builtin
		// mode it also keys the OAuth request tokens and DCR client ids.
		key, err := passkey.LoadOrCreateKey(filepath.Join(cfg.DataDir, "nonce.key"))
		if err != nil {
			return fmt.Errorf("ceremony key: %w", err)
		}
		pk, err := passkey.New(cfg.PublicURL, passkey.NewSigner(key), db, time.Now)
		if err != nil {
			return fmt.Errorf("passkeys: %w", err)
		}
		webDeps := web.Deps{DB: db, Passkeys: pk, TZ: cfg.TZ, Now: time.Now,
			Log: tel.Logger, Metrics: tel.Metrics, Tracer: tel.Tracer}
		if ex, ok := g.(garminkey.Exchanger); ok {
			webDeps.GarminKey = keyRenewer{keys: keys, g: ex}
		}
		if builtin {
			if strings.HasPrefix(cfg.PublicURL, "http://") {
				log.Warn("public URL is plain http: development only")
			}
			od := oauth.Deps{DB: db, NonceKey: key, PublicURL: cfg.PublicURL, DCR: cfg.OAuthDCR,
				Now: time.Now, Log: tel.Logger, Metrics: tel.Metrics}
			if ntfy != nil {
				od.Notifier = ntfy // only when configured: a nil *Ntfy in the interface is not nil
			}
			oauthSrv, err := oauth.New(od)
			if err != nil {
				return fmt.Errorf("oauth: %w", err)
			}
			// One sampler for the page's failed finishes and /mcp's failed bearer checks.
			sampler := auth.NewAuditSampler(auditSamplePerHour, time.Now)
			webDeps.Builtin, webDeps.OAuth, webDeps.Sampler = true, oauthSrv, sampler
			ws, err := web.New(webDeps)
			if err != nil {
				return fmt.Errorf("web page: %w", err)
			}
			// No auth.Middleware around the page: its session gate is the
			// gate (spec §4.1). /mcp takes only this server's access tokens.
			opts := laneOTel(auth.LanePublic, otel.GetMeterProvider())
			page = otelhttp.NewHandler(ws.Handler(), "web", opts...)
			publicMCP = otelhttp.NewHandler(auth.MiddlewareWith(auth.MiddlewareOptions{
				Lane: auth.LanePublic, Verify: oauthSrv.VerifyAccess, Limiter: mcpLim, Auditor: aq, Metrics: tel.Metrics,
				Challenge: oauthSrv.Challenge, Sampler: sampler, SkipAuditMissing: true,
			})(mcpHandler), "mcp", opts...)
			oauthHandler = oauthSrv.Handler()
		} else {
			av := auth.NewAccessVerifier(ctx, cfg.AccessTeamDomain, cfg.AccessAUD, cfg.AccessAllowedEmail)
			ws, err := web.New(webDeps)
			if err != nil {
				return fmt.Errorf("web page: %w", err)
			}
			page = lane(auth.LanePublic, av.Verify, ws.Handler()) // also serves /mcp, behind Access
		}
	}
	var deviceHandler http.Handler
	if cfg.DeviceAPI {
		deviceHandler = deviceapi.New(deviceapi.Deps{DB: db, TZ: cfg.TZ, Now: time.Now,
			Log: tel.Logger, Metrics: tel.Metrics, Tracer: tel.Tracer}).Handler()
		log.Info("device api enabled")
	} else {
		log.Info("device api disabled")
	}
	if page != nil || deviceHandler != nil {
		ls = append(ls, listener{"public", cfg.PublicAddr,
			publicMux(page, publicMCP, oauthHandler, deviceHandler, otel.GetMeterProvider())})
	} else {
		log.Info("public lane disabled")
	}
	ls = append(ls,
		listener{"private", cfg.PrivateAddr, lane(auth.LanePrivate, auth.NewTokenVerifier(db, time.Now).Verify, nil)},
		listener{"ops", cfg.OpsAddr, opsMux(db)})

	// Bind everything before serving so ready reports real ports.
	var addrs Addrs
	lns := make([]net.Listener, len(ls))
	for i, l := range ls {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			for _, p := range lns[:i] {
				p.Close()
			}
			return fmt.Errorf("listen %s: %w", l.name, err)
		}
		lns[i] = ln
		switch l.name {
		case "public":
			addrs.Public = ln.Addr().String()
		case "private":
			addrs.Private = ln.Addr().String()
		default:
			addrs.Ops = ln.Addr().String()
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(4)
	if webhook != nil {
		wg.Add(1)
		go func() { defer wg.Done(); webhook.Run(runCtx) }()
	}
	if ntfy != nil {
		wg.Add(1)
		go func() { defer wg.Done(); ntfy.Run(runCtx) }()
	}
	go func() { defer wg.Done(); svc.Run(runCtx) }()
	go func() { defer wg.Done(); housekeepingLoop(runCtx, db, log) }()
	go func() { defer wg.Done(); propSvc.RunExpiry(runCtx, proposalExpiryEvery) }()
	go func() {
		defer wg.Done()
		backupLoop(runCtx, db, cfg.DataDir, version, time.Now, backupCheckEvery, tel.Metrics, log)
	}()

	servers := make([]*http.Server, len(ls))
	errc := make(chan error, len(ls))
	for i, l := range ls {
		servers[i] = &http.Server{
			Handler: l.h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		}
		log.Info(l.name+" listener started", "addr", lns[i].Addr().String())
		go func() { errc <- servers[i].Serve(lns[i]) }()
	}
	if ready != nil {
		ready(addrs)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("listener failed: %w", err)
		}
	}

	log.Info("shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer scancel()
	var sw sync.WaitGroup
	for _, s := range servers {
		sw.Add(1)
		go func() {
			defer sw.Done()
			if s.Shutdown(sctx) != nil {
				_ = s.Close() // deadline hit: cancel in-flight request contexts
			}
		}()
	}
	sw.Wait()
	cancel()
	wg.Wait()
	closeAudit() // after servers and ingest stopped, before db.Close
	return serveErr
}

// checkDataMode keeps demo data and the runner's Garmin data apart
// (docs/design/demo-mode.md): demo mode refuses a coachd.db it did not
// create, and real mode refuses a demo one. It reads the mark without
// migrating the database. A data directory without a database is new, and
// either mode may claim it.
func checkDataMode(ctx context.Context, dataDir, dbPath string, demo bool) error {
	if isNewDB(dbPath) {
		return nil
	}
	marked, err := store.ReadDemo(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	switch {
	case demo && !marked:
		return fmt.Errorf("COACHD_DEMO=true, but data directory %s holds a coachd database that is not a demo one, "+
			"and demo mode never writes into real data: point COACHD_DATA_DIR at an empty directory (with Docker, a new volume)", dataDir)
	case !demo && marked:
		return fmt.Errorf("data directory %s holds demo data (it was created with COACHD_DEMO=true), "+
			"and coachd never mixes it with Garmin data: set COACHD_DEMO=true to keep trying the demo, "+
			"or point COACHD_DATA_DIR at an empty directory (with Docker, a new volume)", dataDir)
	}
	return nil
}

// isNewDB reports whether no database exists at path yet: no file, or an
// empty one without a -wal beside it.
func isNewDB(path string) bool {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil || fi.Size() > 0 {
		return false
	}
	wal, err := os.Stat(path + "-wal")
	return errors.Is(err, fs.ErrNotExist) || (err == nil && wal.Size() == 0)
}

// laneMux serves /mcp, plus the runner page at / when webHandler is non-nil
// (public lane only), instrumented with laneOTel.
func laneMux(l auth.Lane, h, webHandler http.Handler, mp metric.MeterProvider) http.Handler {
	opts := laneOTel(l, mp)
	mux := http.NewServeMux()
	mux.Handle("/mcp", otelhttp.NewHandler(h, "mcp", opts...))
	if webHandler != nil {
		mux.Handle("/", otelhttp.NewHandler(webHandler, "web", opts...))
	}
	return mux
}

// laneOTel is the otelhttp options of a lane: the server name is fixed so a
// client-chosen Host header never becomes a metric label or span attribute,
// and the public lane ignores inbound trace context (it is pre-auth).
func laneOTel(l auth.Lane, mp metric.MeterProvider) []otelhttp.Option {
	opts := []otelhttp.Option{otelhttp.WithServerName("coachd-" + string(l)), otelhttp.WithMeterProvider(mp)}
	if l == auth.LanePublic {
		opts = append(opts, otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
	}
	return opts
}

// oauthPaths are the OAuth server's routes on the public listener (spec
// §4.1): the metadata documents and every other /.well-known/ path (a JSON
// 404, so discovery probes never get the login page), the token,
// registration and revocation endpoints, and the legacy /authorize, /token
// and /register (JSON 404s). /oauth/authorize and its consent POSTs are the
// page's.
var oauthPaths = []string{"/.well-known/", "/oauth/token", "/oauth/register", "/oauth/revoke",
	"/authorize", "/token", "/register"}

// publicMux composes the public listener. page serves / (nil: the default
// 404); in Access mode it is the Access lane, which also serves /mcp. In
// builtin mode mcp serves /mcp and oauthH the oauthPaths, outside the page's
// session gate and cross-origin protection; both are nil in Access mode.
// /device/v1/ goes straight to the device API, outside the Access middleware
// (ruling R-M3-1: the watch cannot follow an Access login; the device token
// is checked by the handler) and outside auth.Middleware, which would audit
// the raw path, and a pairing id travels in it. The device and OAuth span
// names are fixed so a path never names a span.
func publicMux(page, mcp, oauthH, device http.Handler, mp metric.MeterProvider) http.Handler {
	mux := http.NewServeMux()
	if page != nil {
		mux.Handle("/", page)
	}
	if mcp != nil {
		mux.Handle("/mcp", mcp)
	}
	if oauthH != nil {
		h := otelhttp.NewHandler(oauthH, "oauth", append(laneOTel(auth.LanePublic, mp),
			otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return "oauth" }))...)
		for _, p := range oauthPaths {
			mux.Handle(p, h)
		}
	}
	if device != nil {
		mux.Handle("/device/v1/", otelhttp.NewHandler(device, "device", append(laneOTel(auth.LanePublic, mp),
			otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return "device" }))...))
	}
	return mux
}

// opsMux serves liveness, readiness and pprof. It is never wrapped with auth
// and must never be published.
func opsMux(db *store.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// housekeepingLoop runs housekeep at start and daily, until ctx ends.
func housekeepingLoop(ctx context.Context, db *store.DB, log *slog.Logger) {
	t := time.NewTicker(auditPurgeEvery)
	defer t.Stop()
	for {
		housekeep(ctx, db, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// housekeep purges audit rows older than a year, expired runner auth rows
// (enrolments, sessions) and expired OAuth state.
func housekeep(ctx context.Context, db *store.DB, log *slog.Logger) {
	if n, err := db.PurgeAudit(ctx, time.Now().Add(-auditRetention)); err == nil {
		log.Info("audit purged", "count", n)
	} else if ctx.Err() == nil {
		log.Warn("audit purge failed", "error_class", fmt.Sprintf("%T", err), "error", err)
	}
	if n, err := db.PurgeRunnerAuth(ctx, time.Now()); err == nil {
		log.Info("runner auth purged", "count", n)
	} else if ctx.Err() == nil {
		log.Warn("runner auth purge failed", "error_class", fmt.Sprintf("%T", err), "error", err)
	}
	if n, err := db.PurgeOAuth(ctx, time.Now()); err == nil {
		log.Info("oauth purged", "count", n)
	} else if ctx.Err() == nil {
		log.Warn("oauth purge failed", "error_class", fmt.Sprintf("%T", err), "error", err)
	}
}

// backupLoop runs backupCheck at start and then every `every`, until ctx
// ends. m may be nil.
func backupLoop(ctx context.Context, db *store.DB, dataDir, version string, now func() time.Time, every time.Duration, m *telemetry.Metrics, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		backupCheck(ctx, db, dataDir, version, now(), m, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// backupCheck writes a backup when the newest is missing, at least
// backup.Interval old, or more than 1 h in the future (a wrong clock named
// it). A skipped, busy or failed run is only logged and counted; the next
// check retries. A run cut short by ctx is neither. m may be nil.
func backupCheck(ctx context.Context, db *store.DB, dataDir, version string, now time.Time, m *telemetry.Metrics, log *slog.Logger) {
	count := func(outcome string) {
		if m != nil {
			m.BackupRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
		}
	}
	fail := func(err error) {
		count("failed")
		log.Warn("backup failed", "error_class", fmt.Sprintf("%T", err), "error", err)
	}
	newest, ok, err := backup.Newest(dataDir)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			fail(err)
		}
	case !ok || now.Sub(newest) >= backup.Interval || newest.Sub(now) > time.Hour:
		start := time.Now()
		res, err := backup.Run(ctx, db, dataDir, version, now)
		switch {
		case err == nil:
			count("ok")
			log.Info("backup written", "count", res.FITFiles, "duration_ms", time.Since(start).Milliseconds())
		case errors.Is(err, backup.ErrBusy):
			count("busy")
			log.Info("backup skipped: busy")
		case ctx.Err() == nil:
			fail(err)
		}
	}
}

// keyRenewer is the page's web.GarminKey: the holder, renewing through the
// Garmin client.
type keyRenewer struct {
	keys *garminkey.Holder
	g    garminkey.Exchanger
}

func (k keyRenewer) Expires() civil.Date { return k.keys.Expires() }

func (k keyRenewer) Renew(ctx context.Context, key string, expires civil.Date) error {
	return k.keys.Renew(ctx, k.g, garminkey.Key{Value: key, Expires: expires})
}
