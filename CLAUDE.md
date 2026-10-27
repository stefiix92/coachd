# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Implemented: M1 (data gateway), M2 (context, proposals, passkey approval, ntfy, FIT download), M3 (Connect IQ watch app), M4a (backups and restore, local monitoring, alerts) and built-in auth (coachd's own OAuth 2.1 server and passkey page gate, as an alternative to Cloudflare Access). The approved design is `docs/design/coachd-design.md`, with `docs/design/builtin-auth.md` for built-in auth, `docs/design/watch-store.md` for the watch app's Connect IQ Store distribution, `docs/design/watch-v2.md` for the watch app's screens, agent messages and background notifications, `docs/design/workout-outcome.md` for planned vs actual (executed workout steps and their link to the delivered proposal), `docs/design/approve-week.md` for approving every waiting workout with one passkey, `docs/design/garmin-key-renewal.md` for renewing the Garmin key on the page, `docs/design/expiring-notes.md` for notes with an end date, `docs/design/post-run-webhook.md` for the post-run webhook, `docs/design/perceived-effort.md` for the runner's post-activity rating, `docs/design/trainer-workouts.md` for watt targets and indoor cycling workouts and `docs/design/demo-mode.md` for running on synthetic Garmin data without a key (**[owner]** = owner decision, **[call]** = designer's call, **[builder]** = left to the implementer). Read them before changing behaviour.

## Commands

- Test: `go test -race ./...`
- Vet: `go vet ./...`
- Format check: `gofmt -l .` (must print nothing)
- Build: `CGO_ENABLED=0 go build ./cmd/coachd` (Pi image: add `GOOS=linux GOARCH=arm64`)
- Run: `./coachd serve` (needs `COACHD_TIMEZONE` and the Garmin key settings, or `COACHD_DEMO=true` for synthetic data; see `deploy/.env.example`); other subcommands: `token`, `enroll`, `device`, `backup`, `restore`, `healthcheck`, `version`
- Compose check: `docker compose -f deploy/compose.yaml config` (add `-f deploy/compose.cloud.yaml` for the cloud override)
- Perf: `COACHD_PERF=1 go test -run Perf ./cmd/coachd/`
- FIT decode benchmark: `go test -bench Decode36k -benchmem ./internal/fitdata/`
- Docs site: `hugo server` in `docs/` (Hugo extended; Hextra theme as a Go module in `docs/go.mod`). User docs live in `docs/content/` and publish to https://stefiix92.github.io/coachd/ via `.github/workflows/docs.yml`; update them with behaviour or configuration changes.

## What coachd is

A **gateway, not a coach.** It does three things:

1. Pulls Garmin data and serves it to any AI agent over MCP.
2. Stores the goal and notes that the agent sets.
3. Delivers agent-proposed workouts to the watch after the runner approves them out of band.

All training evaluation and planning belongs to the AI agent and the runner. Never add coaching logic, readiness verdicts or blocking training rules to coachd. Warnings are fine; blocks are not.

## Stack

- **Language and core libraries:** Go (current stable), single binary. Official MCP Go SDK v1.8+, `modernc.org/sqlite` (pure Go; keep builds cgo-free for static multi-arch images), `muktihari/fit`, `go-webauthn`. Stdlib `net/http` and `html/template`, with no npm or frontend build step.
- **Watch app:** Connect IQ (Monkey C), in `watch/`.
- **Deployment:** Docker multi-arch (arm64 and amd64), via `deploy/compose.yaml` with tailscale, Alloy, VictoriaMetrics and VictoriaLogs (plus the Grafana PDC agent via `compose.cloud.yaml`). cloudflared runs on the host, not in compose (owner decision 2026-10-23).

## Invariants

- **No agent path to the watch.** No MCP tool or agent-credentialed API may approve, deliver, withdraw, or alter an approved workout.
  - Agent messages (`send_message`) are display-only text for the watch app: they never carry, start or change a workout.
  - Approval needs a WebAuthn passkey on coachd's web page.
  - Approval nonces are single-use and HMAC-signed, bound to the proposal ID plus a content hash. An approve-all nonce is bound to every proposal ID plus its content hash in the set it approves, all or nothing.
  - Enforce this in coachd; never rely on client-side confirmation or tool annotations.
- **Proposals are immutable after `pending`.** Changes are new proposals with `replaces`. Every state transition goes to the audit log.
- **Summaries are pure and deterministic, numbers only.** They are computed at ingest. Never output verdict words like "ready" or "should".
- **Ingest is idempotent.** Upserts are keyed by Garmin activity ID or calendar date. Keep raw JSON and original FIT files.
- **The Garmin Personal API key stays server-side.** Never send it to agents, logs, telemetry or the web page (Supplemental Terms §1).
  - Tokens last 24 h with no refresh: re-exchange the key daily.
  - Poll conservatively; honour 429 and `Retry-After`.
- **Garmin free text** (activity names and descriptions) is untrusted, a prompt-injection vector. Label it as untrusted or omit it.
- **Telemetry redaction is an allowlist.** Logs, span attributes and metric labels carry only declared keys: IDs, dates, outcomes, durations, counts, error classes. Never health values, chat or note text, or secrets. Tests must enforce this.
- **No published host ports except coachd's public listener on `127.0.0.1:8080`, for a tunnel or reverse proxy on the host (owner decision 2026-10-23).** Never publish :8081 or :9090. One exception (owner decision 2026-10-27): the demo docs' `docker run` publishes the private lane on `127.0.0.1:8081`, for a demo-mode instance only (`docs/design/demo-mode.md` §7).
  - Public access only through Cloudflare Access (access mode: validate `Cf-Access-Jwt-Assertion` on every request) or coachd's built-in OAuth and passkeys (builtin mode). In builtin mode only login, enrolment, OAuth metadata, authorize, token, revoke, register and `/device/v1` are reachable without a session or token, and each checks its own credential. Agent tokens work only on `/mcp`.
  - Private access only via Tailscale (no Funnel) with hashed, scoped static tokens.
- **MCP serves both spec 2026-07-28 and 2025-11-25.** The 2026-07-28 revision requires `StreamableHTTPOptions.Stateless = true` plus `ServerOptions.SupportedProtocolVersions`.
- **coachd only knows Alloy's OTLP endpoint.** Routing per signal lives in Alloy config.

## Out of scope

- Coaching logic, weather, a built-in LLM or chat UI.
- Structured check-ins.
- Multi-user or hosted operation (would need Garmin's Developer Program).
- Unofficial Garmin Connect upload.
- Pushing completed activities to Garmin, gear tracking, medical or nutrition advice.
