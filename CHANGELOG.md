# Changelog

Notable changes to coachd. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/). While the version is 0.x, a minor release may change configuration or behaviour; read its notes before upgrading.

The release workflow publishes the section whose heading matches the pushed tag as the release notes.

## [Unreleased]

### Added

- **README walkthrough:** an animated GIF of the whole loop (data in, the agent proposes, you approve with a passkey, the watch runs it, the results go back). Mocked with a made-up runner; its source and render script are in `docs/demo/`.

## [0.5.1] - 2026-10-27

### Added

- **Demo mode** (`docs/design/demo-mode.md`): `COACHD_DEMO=true` runs `serve` on made-up, deterministic Garmin data, with no Garmin key, so coachd can be tried with an agent in a few minutes ([Try it with demo data](https://stefiix92.github.io/coachd/docs/install/demo/)). About 26 weeks of a plausible runner's activities, with real FIT files, plus sleep, HRV, resting heart rate, Body Battery and weight. A new migration marks a demo data directory: demo mode refuses a real one, a real coachd refuses a demo one, and restore refuses to swap one for the other.
- **Documentation site** at [stefiix92.github.io/coachd](https://stefiix92.github.io/coachd/): install, agents, workouts, the watch app, operations and reference, with search, built with Hugo and the Hextra theme from `docs/` and published to GitHub Pages by the new `docs` workflow. Every page is also served as Markdown, with an `llms.txt` index, for agents reading the docs.
- **New documentation pages:** troubleshooting, FAQ, compatibility (AI clients, watches and phones), data and privacy, coachd compared with alternatives, an example week of a runner and an agent, and starter instructions for your agent. The README gains the overview diagram.
- **Watch app CI:** the new `watch` workflow builds the watch app for every supported watch and runs its simulator tests on Linux; with a `CIQ_DEVELOPER_KEY` secret, releases also attach the Connect IQ Store package. It needs the `GARMIN_USERNAME` and `GARMIN_PASSWORD` secrets for Garmin's device files and skips with a notice without them.

### Changed

- The install guide (`deploy/README.md`), the watch app guide (`watch/README.md`) and the Grafana guide (`deploy/grafana/README.md`) moved to the documentation site; those files now point to its pages. The README is shorter and links to the site.

### Security

- Built with Go 1.27.3, which fixes [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809): `crypto/x509` now distrusts compromised intermediate certificates. govulncheck reports the affected code reachable from passkey sign-in. Images up to 0.5.0 were built with Go 1.27.2; upgrade to the next release.

### Fixed

- **FIT files with an empty header CRC:** some Zwift rides write a 14-byte FIT header whose optional CRC is 0x0000. The FIT decoder coachd uses then computed the file CRC without the header and rejected these valid files, so the ride had no laps, series or perceived effort ("fit detail failed", error class `other`). coachd now checks FIT integrity as Garmin's FIT SDK does; truncated and damaged files are still rejected. Affected rides fill in on the next sync after upgrading.

## [0.5.0] - 2026-10-26

### Added

- **Trainer workouts on the watch** (`docs/design/trainer-workouts.md`):
  - **Watt targets:** a `power` step target takes `low_w` and `high_w` (1 to 1500 W) instead of `low_pct_ftp` and `high_pct_ftp`, for cycling. The FIT file carries them as watts, the page and the watch show `power 200–220 W`, and `get_activity` reads them back from a ride's planned steps. A workout with a watt step is not exportable to Zwift, which takes % of FTP.
  - **Indoor cycling:** the new workout sport `indoor_cycling` is a ride on a smart trainer. The FIT file marks it indoor, so the watch starts it as an indoor ride, where a paired trainer can follow the power targets. It is stored and listed as cycling and exports to Zwift like any cycling workout.

## [0.4.0] - 2026-10-25

### Added

- **Watch app v2** (`docs/design/watch-v2.md`):
  - The main screen shows the next workout: its day, name, sport, duration and distance; the sync status moves to a footer.
  - START opens a menu that lists the workouts from today on; selecting one shows its steps, notes and the agent's rationale, and START there begins it.
  - **Agent messages:** the new MCP tool `send_message` (1 to 300 characters, write scope) stores a short display-only message. The watch shows the latest five from the last seven days under Messages, with an unread count on the main screen. Messages go nowhere else, not to ntfy.
  - **Background checks:** every 15 minutes while paired, the watch asks coachd for new messages and workouts and notifies you (a notification on fēnix 7 and later, an "open coachd?" prompt on older watches). It never downloads in the background: open the app to sync. A new **Notifications** setting in the Connect IQ app turns it off.
  - **Glance** on fēnix 7 and later: the next workout and unread messages.
  - The app needs two new permissions, Background and Notifications.
- **Device API:** `GET /device/v1/workouts/{id}` (a workout's detail) and `GET /device/v1/messages`, both read-only. An older watch app ignores them, and the new app works against an older coachd without details or messages.
- **Planned vs actual** (`docs/design/workout-outcome.md`):
  - For an activity recorded from a structured workout on the watch, `get_activity` with detail `laps` or `series` adds `workout`: the steps the watch followed, and each executed step with its time, distance, heart rate, pace, power and, for a heart-rate or pace range, the seconds inside, below and above it. Numbers only, computed from the activity's FIT file at ingest.
  - `workout.proposal_id` links the activity to the delivered proposal it carried out, and `list_workouts` gains `activity_ids` on each workout. The link needs the steps the watch recorded to equal the workout coachd delivered, and the dates to be within 3 days.
  - Garmin Coach and Garmin Connect workouts get the steps too, without a link. Zwift-recorded rides do not: their files carry no step index.
  - Not yet checked on a workout delivered by the coachd watch app: if the watch rewrites the steps, the steps still show and only the link is missing.
- **Approve the week** (`docs/design/approve-week.md`): when several workouts are waiting, **Review all** (`/review`) shows each one in full, then approves all of them with one passkey prompt.
  - The passkey ceremony binds every workout's ID and content hash; the approval is all or nothing in one transaction, and any change since the page was shown refuses it with the day of the workout that changed.
  - Left out, with the reason: workouts whose date has passed, whose content does not match its hash, or that another waiting workout replaces; at most 20 at a time.
  - The home page links it when two or more are waiting, and a waiting workout's page says how many more are waiting.
- **Renew the Garmin key on the page** (`docs/design/garmin-key-renewal.md`): **Garmin key** (`/garmin`, linked from the home page, which warns 14 days before expiry) takes a new key and its validity (30, 90 or 180 days, or 1 year). coachd checks it with Garmin, stores it in the data directory (`garmin-key.json`, mode 0600) and switches at once, without a restart; `get_status` and the key-expiry metric follow. If Garmin refuses the key, nothing changes. The key is never shown, logged or backed up, and the audit log records `garmin_key_renewed` with its outcome.
- **Notes that expire** (`docs/design/expiring-notes.md`): `upsert_note` takes an optional `valid_until` (today up to 366 days ahead) for facts that stop being true. After that day `get_notes` leaves the note out unless `include_expired` is true, and reports `expired_count`; each note shows `valid_until` and `expired`. Expired notes are never deleted automatically and still count toward the 50-note cap; the "notes full" error says how many agent notes are expired. The notes page shows the date and fades expired notes.
- **Post-run webhook** (`docs/design/post-run-webhook.md`): with `COACHD_WEBHOOK_URL` set, coachd POSTs `{"event":"activity.stored","activity_id",...,"date","sport","proposal_id"}` (IDs only) when an activity that started in the last 48 hours is stored, after its FIT file and per-step numbers are in, so your own automation can wake the agent for a post-run review. Optional bearer token from `COACHD_WEBHOOK_TOKEN_FILE`; retries like ntfy; new metric `coachd_webhook_sends_total{outcome}`.
- **Your post-activity rating** (`docs/design/perceived-effort.md`): `list_activities` and `get_activity` show `perceived: {feel_pct, rpe}`, the "How did you feel?" and RPE answers you give on the watch after a workout, read from the FIT file (the Personal API's JSON does not carry them). Null until the FIT file is read, or when you skipped the questions. Migration 0013 resets the detail cache once more so they fill in.

### Changed

- The watch app's status no longer shows the workout count after a sync; the main screen shows "Last sync HH:MM" instead.
- **Upgrade note:** the activity detail cache (laps, series and now workouts) is reset by a migration and rebuilds from the stored FIT files: newest first, 20 activities per FIT drain, and on each `get_activity` that misses it.

### Fixed

- Proposals that expired were not logged when the stale-delivery check after them failed, for example during shutdown. This also made a release test flaky.

## [0.3.0] - 2026-10-25

### Added

- **Power targets for cycling:** `propose_workout` takes a `power` step target in % of FTP (`low_pct_ftp`, `high_pct_ftp`, 20 to 300, cycling only). The approval page shows it, and the FIT file carries it to the watch.
- **Download for Zwift:** an approved cycling workout whose steps all have a time duration downloads as a Zwift workout file (`.zwo`) from its page; copy it into Zwift's Workouts folder on a Mac or PC (see the README).
  - Power steps become steady-state blocks at the middle of the band; a two-step power repeat becomes a Zwift interval set. Heart-rate, pace, cadence and effort targets become free-ride blocks with an on-screen message.
  - The first download of either file, FIT or `.zwo`, marks the workout delivered.
  - The `.zwo` files were not tried in Zwift before this release. Text is kept to plain ASCII to be safe. If Zwift does not list a downloaded workout, please open an issue.
- **Warning `zwift_not_exportable`:** a cycling workout with a distance, open or heart-rate-bounded step gets a warning naming the step. It never blocks.
- **Telemetry:** workout downloads carry a `format` attribute (`fit` or `zwo`).

### Changed

- The cadence target's schema text says it is rpm for cycling.

## [0.2.0] - 2026-10-25

### Changed

- **Runner page redesign:** a new look for every page, still without JavaScript frameworks or a build step.
  - The coachd logo, a vector drawing, in the header and on the sign-in page, and its navy and green colours throughout; on the dark theme the logo's navy turns light. The browser tab shows the logo on a white tile (SVG, with a PNG for browsers without SVG icons); the home-screen icon is the logo on white.
  - Navigation tabs with icons that fit a phone screen and mark the current page; Connected apps is a tab in builtin mode.
  - Proposals appear as cards with a date badge, a warning count or a coloured status.
  - The home page shows a countdown to the goal.
  - Each workout step has a colour for its kind (warm-up, active, recovery, rest, cool-down) and guides for repeats.
  - On a phone the Approve and Decline buttons stay in view while the workout scrolls.
  - Agent-written text keeps its label, now with its own colour and a ✦ mark.
  - Notes fold their edit form away.
  - Sign-in, enrolment, consent and errors are centred cards.
  - Light and dark themes use `light-dark()`, and page changes cross-fade in browsers that support view transitions.
- **Watch app:**
  - The coachd URL is a setting in the Connect IQ phone app instead of being compiled in, so one Connect IQ Store app serves every runner. Sideloaded builds use `COACHD_PUBLIC_URL` as its default.
  - The pairing is bound to that URL: changing it forgets the token and the pairing in progress before the app talks to another server, and the pairing screen shows the host.
  - Supports fēnix and Forerunner models with app workouts (`watch/devices.sh`).
  - `build.sh store` and `build.sh store-beta` build the Store packages; sideload, test and beta builds use a separate dev app id ("coachd dev").
  - An installed watch app pairs again once after updating.

## [0.1.0] - 2026-10-24

First public release.

### Added

- **Data in:**
  - Garmin Connect Personal API ingest and backfill: activities with their original FIT files, sleep, HRV, resting HR, Body Battery and stress.
  - Deterministic summaries: numbers only, never verdicts.
- **MCP server** (spec 2026-07-28 and 2025-11-25):
  - read tools;
  - `request_sync`;
  - goal and notes;
  - `propose_workout` with sanity checks and warnings, and `cancel_proposal`.
- **Approval and delivery:**
  - The approval page with passkey (WebAuthn) approval.
  - Minimal ntfy notifications.
  - FIT download.
  - Connect IQ watch app with device pairing and delivery confirmation.
- **Network lanes:**
  - The private lane over Tailscale, with scoped static tokens.
  - The public lane in Access mode (Cloudflare Access with Managed OAuth).
  - The public lane in built-in mode: passkey sessions and coachd's own OAuth 2.1 server, with CIMD, opt-in DCR, PKCE, rotating refresh tokens and reuse detection.
- **Operations:**
  - backups and restore;
  - OpenTelemetry with an allowlist-only redaction policy;
  - a local Grafana profile, dashboards and alerts;
  - a hardened compose stack.
- **Releases:** signed multi-arch images on ghcr.io, with an SBOM and build provenance.
