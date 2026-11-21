# coachd: design

Date: 2026-10-23. Status: approved by the owner on 2026-10-23.

This document supersedes the earlier coach-centric design in `README.md` (metrics engine, guardrail validator, ACWR, `schedule_workout` agent tool, Python). README and CLAUDE.md get updated to match once this is approved.

Labels: **[owner]** means decided by the owner, **[call]** means designer's call (owner may override), and **[builder]** means left to the builder.

---

## 1. Intent

The owner is a runner, not a coach, and is training for an ultramarathon in Istria. They trained with Garmin Coach and, separately, chatted with ChatGPT about training reviews, how they felt, the weather, injuries and other problems. The ChatGPT conversations produced faster progress and were enjoyable. The conversation is the valuable part.

coachd makes that conversation work with real data and a real watch. **coachd is a gateway, not a coach.** [owner] It has three jobs:

1. **Data in.** Pull the runner's Garmin data and serve it to any AI agent.
2. **Context.** Store the goal (set by the agent) and a few durable notes, so any agent or new chat starts informed.
3. **Workout out.** Take a structured workout proposed by the agent and deliver it to the watch, but only after the runner explicitly approves it.

All training evaluation, coaching and planning happens in the AI agent. The runner and the agent have the final say. [owner] Claude Duet starts the morning conversation, pulls data from coachd, and the runner chats there. [owner] Any MCP-capable agent can be used. [owner]

### Daily flow

1. In the morning, Claude Duet opens a chat and calls coachd for last night's sleep, HRV, recent runs, the goal and notes.
2. The runner and the agent talk it through: how they feel, the weather, niggles.
3. The agent calls `propose_workout`. coachd checks the workout is well-formed, attaches warnings, stores it as pending, and sends the runner a phone notification.
4. The runner taps the notification and sees the workout and its warnings on coachd's approval page. They approve with a passkey or decline.
5. coachd makes the approved workout available to the watch, and records and reports delivery.

## 2. Who and where

- **Self-hosted.** One instance serves one runner. [owner]
- **Packaging.** A Docker image deployable anywhere: Raspberry Pi 4/5 (arm64) and amd64. [owner]
- **Reference setup** (the author's, used for live checks):
  - Raspberry Pi, with Grafana in Grafana Cloud.
  - Garmin fēnix 7S Pro and an Android phone.
  - Morning chat in Claude (a claude.ai custom connector).
- **SaaS.** Possible later, only if it is secure and legal. [owner] The likely path is one isolated instance per customer, not a multi-tenant rewrite. It would need Garmin's partner program and legal review, because the Personal API only covers the key owner's data. [call]
  - Nothing in v1 is multi-tenant.
  - Nothing in v1 may assume it is the only coachd in the world: no hard-coded hostnames and no global singletons in external services.

## 3. Goals, non-goals, anti-goals

### Goals
- Any MCP-capable agent can read the runner's Garmin data quickly, in agent-friendly shapes.
- The agent can set the goal and read and write notes.
- The agent can propose a workout. The runner approves out of band with a passkey, the workout reaches the watch, and delivery is confirmed.
- Security in every layer. [owner]
- Monitoring through metrics, logs, traces and profiles, good enough to debug production issues. [owner]
- Fast and modern. [owner] Targets [call]:
  - p95 under 50 ms for read tools served from the local store on a Pi 4.
  - coachd process under 64 MB RSS.
  - Cold start under 1 s.

### Non-goals (v1)
- Coaching logic: readiness verdicts, plan generation, intensity decisions, hard training blocks. [owner]
- Weather. The agent fetches it itself. [call]
- coachd's own chat UI, its own LLM, or any LLM API keys. [owner]
- Starting the morning conversation. Claude Duet does this. [owner] (2026-10-25: an optional post-run webhook with IDs only tells the runner's own automation that an activity arrived; coachd still starts nothing itself, `post-run-webhook.md`.) (2026-11-21: coachd now also says when there is something to talk about, with webhook events for a ready day, an unmatched workout and a closed week, optional fixed-text ntfy nudges, and a push for an agent message; it still starts no conversation and writes no text, `proactive.md`.)
- Storing check-ins (feel, RPE) as structured data. They live in the agent's conversation, and the agent can write durable facts into notes. [call]
- Multi-user or hosted operation.
- Pushing completed activities into Garmin, gear tracking, medical or nutrition advice.
- Non-Garmin data sources.
- Unofficial Garmin Connect web upload. [owner, via approved stack]

### Anti-goals: failures even if everything "works"
- A workout reaches the watch without the runner's explicit, out-of-band approval.
- An agent, or text injected into an agent, can approve, deliver, or change a workout after approval.
- Health values, chat content, tokens or secrets appear in logs, metric labels, traces or profiles.
- Health data leaves the host except to the agent the runner connected.
- Any coachd or monitoring port is reachable from the internet without authentication.
- A workout changes silently after the runner approved it.

## 4. Architecture

```
                         Host (Pi / any Docker host)
┌───────────────────────────────────────────────────────────────────────┐
│ coachd (single Go binary, single process)                             │
│                                                                       │
│  garmin ──▶ store (SQLite WAL + raw FIT/JSON) ──▶ summaries           │
│                   │                                                   │
│                   ├──▶ context (goal, notes)                          │
│                   │                                                   │
│                   └──▶ workouts (schema, sanity, warnings, lifecycle) │
│                                │                                      │
│        ┌───────────────┬───────┴──────────┬─────────────────┐         │
│        ▼               ▼                  ▼                 ▼         │
│      mcp            web (approval,     delivery          telemetry    │
│   (agents)          notes, passkeys)   (FIT, watch API)   (OTel)      │
└────────┬───────────────┬──────────────────┬─────────────────┬─────────┘
         │               │                  │                 │
   cloudflared (host; Tunnel + Access) ntfy (notify)      Alloy ──▶ VictoriaMetrics, VictoriaLogs (host)
   tailscale (private lane)                                    └─▶ Grafana Cloud: Tempo, Pyroscope
         │                                                     PDC agent ◀── Grafana Cloud queries VM/VL
   Claude / Duet / ChatGPT / Gemini / Cursor / Open WebUI / local
```

- **One binary.** It holds the ingest scheduler, MCP server, web server and telemetry. There is no separate cron or systemd timer. [call]
- **Connect IQ watch app.** A separate sub-project in the same repo (`watch/`, Monkey C). [call]

## 5. Data in: Garmin

**Source: the Garmin Connect Personal API** (launched 2026-10-21). [owner, approved stack]
- The runner creates a key at `connect.garmin.com/app/settings/personalApiToken`. Access is self-serve.
- Each day coachd exchanges the key for an OAuth2 access token. Tokens last 24 h and there are no refresh tokens.
- Keys expire after 30, 90 or 180 days, or 1 year. The runner renews the key on coachd's page, without a restart (`garmin-key-renewal.md`, 2026-10-25). coachd exposes days remaining:
  - as a metric, with an alert at 7 days;
  - in `get_status`, so the agent can remind the runner;
  - on the web page.
- The API is GET only, with no webhooks, and its rate limits are undocumented.
  - Poll conservatively with exponential backoff.
  - Honour 429 responses and the `Retry-After` header.
  - Never hammer the API on failure.
- Per-endpoint date-range caps apply: steps and stress 28 d, sleep and Body Battery 31 d, v2 daily summaries 1 y. Chunk requests to fit.

**Data stored:**
- Activities: the JSON summary plus the original FIT file on disk.
- Daily health: sleep with scores, HRV (status, baseline, overnight values), resting HR and its 7-day average, Body Battery, stress, SpO2, weight.
- Per-activity Garmin metrics: training load, training effect, VO2max, TSS.
- The runner's post-activity rating from the FIT session (feel 0–100, RPE 0–10), when the watch asked (`perceived-effort.md`).
- For an activity recorded from a structured workout: the steps the watch followed and the steps as executed, with time inside, below and above each heart-rate or pace range, decoded from the FIT file at ingest (`workout-outcome.md`).

Keep the raw JSON payload next to the parsed rows so data can be re-parsed later without re-fetching. [call]

**Cadence** [call]:
- Hourly background poll.
- A `request_sync` tool the agent can call, rate-limited to once per 10 min, so the morning chat gets last night's data.
- Every sync records the time of the freshest data.

**Backfill:**
- On first run, page back through the Personal API as far as it allows (target: 2 years).
- `coachd import <garmin-export.zip>` imports the Garmin account data export: original FITs plus wellness JSON. This is the fallback for older history.

**Idempotency:** upserts are keyed by Garmin activity ID (activities) and calendar date (daily health). Re-running any sync or import is safe.

**Deletions** (owner-authorized follow-up, 2026-10-23): coachd mirrors activities deleted in Garmin Connect.
- After a fully fetched incremental window, a stored activity Garmin no longer lists is probed on the single-activity endpoint (at most 10 per sync).
- Only a confirmed 404, verified against an activity Garmin just listed, marks it deleted. Any doubt keeps it.
- It is a soft delete: hidden from every tool and summary, with its raw JSON, detail and original FIT file kept, and an audit row written. If Garmin lists it again, it is restored.

**Untrusted text:** activity names and descriptions are free text that the runner, or anyone they share with, can write, which makes them a prompt-injection vector.
- Descriptions are not served by default.
- Names are served in a field explicitly marked as untrusted user text. [call]

**Garmin's derived values** (Body Battery, training effect and so on) are served as Garmin's numbers, clearly labelled. coachd never relabels them as its own.

## 6. Summaries: numbers, not verdicts

Deterministic aggregations, so the agent does not do arithmetic over hundreds of records. [call] They are computed at ingest time and stored, so reads are lookups.

- **Volume:**
  - Weekly and daily volume per sport: distance, moving time, elevation gain.
  - Weeks run Monday–Sunday in the runner's configured timezone.
- **Long runs:** the longest run (distance and duration) in the trailing 7, 30 and 90 days.
- **HRV:**
  - 7-day rolling average of overnight HRV.
  - The runner's normal band: rolling 60-day mean ± 1 SD of the 7-day average. [builder may refine]
  - Where today sits relative to that band.
- **Resting HR:** 7-day average against the 60-day baseline.
- **Sleep duration:** 7-day average. Sleep stages are not summarised because they are unreliable.
- **Data coverage:** weeks of history, gaps, and the freshest timestamp for each data type.

Summaries are pure functions over the store, with unit tests. They never output words like "ready", "fatigued" or "should".

## 7. Context: goal and notes

**Goal** [owner: the agent sets it, coachd stores it and gives guidance at goal-setting]
- **Fields:**
  - name, date, sport, distance (km), elevation gain (m);
  - target and notes (free text, length-limited).
- **Active goal:** one at a time. Setting a new goal archives the old one, so history is kept. [call]
- **`set_goal` returns guidance facts** [call on content]:
  - weeks until the goal date;
  - last 4 weeks' average weekly volume and elevation;
  - longest run in the last 30 days;
  - weeks of data coverage.

  Facts only, no recommendations.

**Notes** [call]
- **What they hold:** a small set of durable facts the agent should always know.
  - Categories: injury, availability, preference, other.
  - Each note carries text, the creator (agent or runner), and created and updated times.
  - Optionally `valid_until`, the last day it holds; after that `get_notes` leaves it out unless asked, and counts it (`expiring-notes.md`, 2026-10-25).
- **Limits:** about 50 notes, about 500 characters each.
- **Access:** the agent can create, update and delete notes through MCP. The runner can view, edit and delete them on the web page.
  - *Note (2026-10-23, builder decision pending owner confirmation):* agents can edit and delete only notes written by agents. The runner's notes are edited by the runner on coachd's page. This closes an authorship-laundering hole: otherwise an agent could rewrite a runner's note and have its text served as the runner's.
  - *Note (2026-10-23, builder decision pending owner confirmation):* when the runner edits a note on the page, the note is relabelled `created_by: "runner"`, so agent-written text edited by the runner reads as the runner's, and agents can no longer touch it.
- **Trust:** notes are served back to agents as runner context. Notes written by an agent are labelled as such.

## 8. Data out: workouts

### Workout model [call]
Modelled on the FIT workout structure so encoding is lossless:
- name, sport (running, trail running, walking/hiking, cycling, strength, other), scheduled date, optional notes;
- an ordered list of steps.

| Step part | Values |
|---|---|
| Kind | warmup, active, recovery, rest, cooldown, repeat (with child steps and a count) |
| Duration | time, distance, open (lap button), or heart rate above/below a value |
| Target | none, heart-rate range (bpm), pace range, cadence range, power range (% of FTP or watts, cycling and indoor cycling only; `trainer-workouts.md`), or a free-text effort cue |
| Notes | optional |

The JSON Schema is defined once and used both as the MCP tool input schema and for server-side validation.

### Sanity checks: reject, with a reason the agent can act on
These are integrity checks, not coaching:
- The workout matches the schema.
- Values are physically plausible. Defaults are generous and configurable; [builder] picks the exact defaults within these ranges:
  - total duration 1 min–10 h;
  - pace 2:30–25:00 min/km;
  - heart rate 40–230 bpm;
  - power 20–300 % of FTP, cycling only (`zwift-workouts.md`, 2026-10-25), or 1–1500 W (`trainer-workouts.md`, 2026-10-26);
  - at most 100 steps after expanding repeats.
- The date is today or up to 14 days ahead.
- *Note (2026-10-23, builder decision pending owner confirmation):*
  - **Pace bounds are per sport and fixed in code, not configurable:** running, trail running and other 2:30–25:00 min/km; walking and hiking 5:00–40:00 min/km; cycling 1:00–25:00 min/km. Strength takes no pace targets and no distance durations.
  - **The 1-minute minimum total** applies only when every step has a time or distance duration. Open and heart-rate-bounded steps count as 0 s; the 10-hour maximum applies to the known part.
  - **The spike warning** (under Warnings) also fires when there is no run baseline, as a factual message such as "no running activity recorded in the last 30 days; planned 8.0 km".

### Warnings: never block
Warnings are returned to the agent and shown on the approval page.
- **Spike:** planned distance or duration is more than 110% of the longest run in the last 30 days. [call, from the Frandsen 2025 BJSM cohort; single study, hence a warning]
- **Same day:** another workout is already approved or delivered for that date. This is informational, since doubles are allowed.
- **Above max HR:** a heart-rate target is above the highest HR recorded in 90 days.
- **Stale data:** the last successful Garmin sync is more than 24 h old.
- **Thin history:** less than 6 weeks of data.
- **Not exportable to Zwift:** a cycling workout has a step without a time duration, so it cannot be a Zwift workout (`zwift-workouts.md`).

Warnings are deterministic code, not LLM text. Final say rests with the runner and the agent. [owner]

### Proposal lifecycle
```
propose ─▶ invalid (rejected, reason returned)
        └▶ pending ─▶ approved ─▶ delivering ─▶ delivered
                  │       │      └▶ delivery_failed (retryable)
                  │       ├▶ withdrawn (runner, from approved or delivered)
                  │       └▶ expired (date passed before delivery)
                  ├▶ declined
                  ├▶ cancelled (agent cancels a pending proposal)
                  ├▶ superseded (a proposal naming it in `replaces` was approved)
                  └▶ expired (end of the scheduled date, runner's timezone)
```
- **Several workouts per date are allowed** (doubles, run plus strength). [call] A new proposal never displaces another implicitly. To change a planned workout, the agent proposes a new one with `replaces: <id>`.
  - When the replacement is approved, the replaced proposal becomes `superseded` if it was still pending, or `withdrawn` if it was already approved or delivered.
  - The approval page shows both workouts side by side.
- **The runner can withdraw** an approved or delivered workout from the web page: one tap, audited. [call] Agents cannot withdraw; they can only propose a replacement.
- Proposal IDs are random (≥128 bit).
- A proposal stores a hash of its content.
- After `pending`, the content is immutable. Any change is a new proposal.
- Every transition is written to the audit log with a timestamp and actor: the agent's client ID, the runner, or the system.
- *Note (2026-10-23, builder decision pending owner confirmation):* with the watch app's delivery states (M3), the runner can withdraw a workout from `approved`, `delivering`, `delivery_failed` and `delivered`. Approving a replacement withdraws a replaced proposal in any of those states (a pending one is still superseded). Agents may propose a replacement for a proposal that is `pending` or in any of those states, and the same-day warning counts workouts in those states. The FIT download on the page works in all four: it moves `approved`, `delivering` and `delivery_failed` to `delivered` and serves a `delivered` workout again. The Zwift download (`.zwo`, cycling only, `zwift-workouts.md`) follows the same rules; the first download of either file delivers.

### Approval [owner: notification, then the runner approves; mechanism is a call]
1. **Notification via ntfy** [call]:
   - The body is minimal: "Workout for Tue 28 Oct ready to review". No metrics or health data.
   - One `view` action opens the approval page.
   - Topic access is `deny-all` plus a token. The server is ntfy.sh, or self-hosted, set by config.
2. **Approval page:** served by coachd through Cloudflare Tunnel and protected by Cloudflare Access (browser login), or, in builtin mode, by a passkey session (`builtin-auth.md`, owner 2026-10-24).
   - It shows the full workout, warnings, and what the agent said it is for (the proposal's `rationale` field, labelled as agent text).
   - Buttons: Approve and Decline.
3. **Approve** requires a WebAuthn passkey assertion with user verification (Face ID or fingerprint). Decline is one tap.
4. **The approval nonce:**
   - single-use, HMAC-signed;
   - bound to the proposal ID and content hash;
   - expires after 24 h or at proposal expiry.

   The server re-checks that the proposal is still `pending` and its hash unchanged before transitioning.

   *Note (2026-10-25, approve the week, `approve-week.md`):* a review page approves every approvable pending proposal with one assertion. Its nonce is single-use and HMAC-signed like any other, and bound to every proposal ID plus its content hash in the set; the approval is all or nothing.
5. **Passkey enrolment:** `docker exec coachd coachd enroll` prints a one-time enrolment URL valid for 10 minutes. It is never written to logs. More than one passkey is allowed, and passkeys can be revoked on the web page.

*Note (2026-10-23, builder decision pending owner confirmation):*
- **Passkey for every runner write (R59).** Cloudflare Access Managed OAuth hands agent clients a `Cf-Access-Jwt-Assertion` for the runner's own identity on the same hostname, so the Access login alone cannot tell the runner from an agent. GET pages need Access only. Every POST also needs a passkey: a fresh assertion bound to the action (approve, login) or a passkey session (decline, withdraw, FIT download, notes, passkey revoke, logout). Sessions last 12 h and are bound to one passkey.
- **Stateless ceremony token.** Instead of server-side ceremony state, login, enrolment and approval challenges are `base64url(json).base64url(HMAC-SHA256)` tokens signed with `${COACHD_DATA_DIR}/nonce.key`; the WebAuthn challenge is the single-use key. Approve expires at the earlier of 24 h and the end of the proposal's date.

No MCP tool and no API reachable by agent credentials can approve, deliver, or alter an approved workout. This is enforced in coachd itself, regardless of the client's own confirmation prompts, because tool annotations are only hints.

### Delivery [owner, approved stack: Connect IQ watch app as the main path, FIT file as the fallback]
- **FIT file (milestone 2):** an approved workout is encoded as a FIT workout file and downloadable from the approval page. The runner copies it to the watch over USB (`GARMIN/NewFiles`). The proposal is marked `delivered` when the file is downloaded. [call]
  - *Note (2026-10-23, builder decision pending owner confirmation):* the FIT is encoded on demand from the stored proposal and never stored (`FileId.TimeCreated` is the proposal's creation time, so the bytes are deterministic). The download is a `POST /p/{id}/fit` (it needs a passkey session and CSRF protection) and the first download moves `approved` to `delivered`; later downloads serve the same bytes without a transition.
- **Connect IQ watch app (milestone 3):**
  - **Sync model** [call]: the watch app syncs a desired set. It asks coachd for every approved or delivered workout from today to +14 days and downloads any it lacks, as FIT through the phone connection. It removes stored workouts that are no longer in the set (withdrawn, superseded, or in the past).
  - `PersistedContent.getAppWorkouts()` lists the workouts on the watch, and `toIntent()` launches one in the native workout player.
  - The app acknowledges each download, and coachd marks the proposal `delivered`.
  - **Auth** [call]: a per-device token.
    - Enrolment: the watch shows a short code, and the runner confirms it on coachd's web page.
    - This works for sideloaded and Store installs alike. Garmin Connect Mobile app settings only work for Store installs; the Store app uses them for the coachd URL (`watch-store.md`).
    - Tokens are revocable.
  - **Distribution:** sideloaded for the owner in M3. Since 2026-10-24: the Connect IQ Store, one public listing under the owner's account, with the coachd URL as a phone setting; sideloading stays for developers. See `watch-store.md`. [owner 2026-10-24]
  - **Reaching coachd through Cloudflare:** the watch-API path either uses a Cloudflare Access service token (headers set in `makeWebRequest`) or bypasses Access and relies on coachd's device token. [builder decides after testing; either way, coachd validates the device token]
  - Requires Connect IQ API level 3.0+ on the watch.
- **Failure:** delivery failure or timeout moves the proposal to `delivery_failed`, sends an alert, and allows a retry.
  - *Note (2026-10-23, builder decision pending owner confirmation):* **R-M3-1, Access bypass.** `/device/v1/*` is served on the public listener outside coachd's Access middleware and is authenticated only by the device token; coachd validates the token in every case. It is opt-in (`COACHD_DEVICE_API=on`); with it on and Access unset, the public listener binds and serves only `/device/v1/*`. Once an Access app exists, the owner creates a **separate** self-hosted Access application scoped to the path `coachd.<domain>/device/v1` with a single Bypass (Everyone) policy (otherwise Access redirects the watch to a login page, error -1002). Access policies have no path condition, so a Bypass policy on the main coachd application would turn Access off for the whole hostname; it is never added there. coachd still fails closed without a JWT outside `/device/v1/*`, so that mistake breaks MCP and the page rather than exposing them. A service-token header is not used, because its secret would be embedded in the app.
  - *Note (2026-10-23, builder decision pending owner confirmation):* the endpoints are `POST /device/v1/pairings`, `GET /device/v1/pairings/{id}`, `GET /device/v1/workouts` (the desired set: `approved`, `delivering`, `delivery_failed` or `delivered`, dated today to +14, at most 30), `GET /device/v1/workouts/{id}/fit` and `POST /device/v1/workouts/{id}/ack`. The desired-set window is today to today+14 in the configured timezone. The sync runs in the foreground (on launch and "Sync now"), with no background service. A FIT fetch moves a proposal to `delivering`; the ack moves it to `delivered`. A `delivering` proposal with no ack and a latest fetch more than 15 minutes old moves to `delivery_failed` (stale rule, swept every 10 minutes); the next fetch retries. Pairing is confirmed either on the page (passkey session) or by `coachd device pair <code>` in a host shell. Per device the rate limit is 60/min with burst 70. The open pairing endpoints have separate global buckets: create 10/min with burst 5, poll 60/min with burst 30 (two watches polling every 3 s).
  - *Note (2026-10-25, watch app v2, `watch-v2.md`):* two read-only endpoints join them, `GET /device/v1/workouts/{id}` (a desired workout's detail; records nothing) and `GET /device/v1/messages` (the agent's messages). The app also checks in the background every 15 minutes to notify; the sync itself stays in the foreground.
- **After delivery** (2026-10-25, `workout-outcome.md`): an activity whose recorded workout steps equal a delivered proposal's FIT steps, dated within 3 days of it, links to that proposal. The link is computed on read, never stored, and changes nothing in the lifecycle.

## 9. Agent interface (MCP)

- **Transport:** streamable HTTP.
- **Spec versions:** serve both MCP 2026-07-28 (stateless) and 2025-11-25, because most clients have not moved to the newer revision yet.

### Tools [call on names and shapes; builder may refine parameters]

| Tool | Kind | Purpose |
|---|---|---|
| `get_status` | read | Sync freshness by data type, data coverage, Garmin key days left, pending proposals |
| `get_daily_health(from, to)` | read | Sleep, HRV, resting HR, Body Battery, stress, SpO2, weight per day |
| `list_activities(from, to, sport?)` | read | Activity summaries |
| `get_activity(id, detail)` | read | Laps, splits, HR/pace/elevation series at reduced resolution. Never the raw full-resolution stream by default. For a structured workout, the planned and executed steps and the delivered proposal it carried out (`workout-outcome.md`). |
| `get_summaries(from, to)` | read | §6 numbers |
| `get_goal` / `get_notes` | read | Context |
| `list_workouts(from, to, status?)` | read | Proposals and their states; for a delivered one, the activities that carried it out |
| `set_goal(...)` | write (local) | Returns the §7 guidance facts |
| `upsert_note` / `delete_note` | write (local) | Notes |
| `propose_workout(workout, rationale, replaces?)` | write (local) | Returns proposal ID, verdict, warnings. Triggers the notification. |
| `cancel_proposal(id)` | write (local) | Pending proposals only |
| `request_sync()` | write (local) | Rate-limited Garmin sync |
| `send_message(text)` | write (local) | A short display-only message for the watch app (`watch-v2.md` §3) |

- **Annotations:** read tools carry `readOnlyHint: true`. Write tools carry `destructiveHint: false`.
- **No external-write tool exists.**
- **Output format:**
  - JSON with units in the field names (`distance_km`, `hr_bpm`).
  - ISO dates in the runner's timezone.
  - Paginated, with response size capped.
- **Scopes:** `coach:read` and `coach:write`.
  - Private-lane static tokens can be read-only.
  - On the public lane in Access mode every client gets both scopes, because Access Managed OAuth authenticates the runner, not the client. [call] This is acceptable because no tool can approve, deliver or withdraw.
  - In builtin mode each client gets its own grant, and the runner may grant read only (`builtin-auth.md` §5.3).
- **Client attribution:** best effort. Use a client identifier from the Access token if one is present. Otherwise record the lane plus the client's self-declared name, labelled unverified. Lack of per-client attribution alone is not a reason to build coachd's own authorization server. [call] (Builtin mode, added for the open-source release, has per-client grants.)
- **Rate limits:** per identity and lane.

## 10. Security

**Network exposure:**
- **Public lane: Cloudflare Tunnel (`cloudflared` on the host, not a container; owner decision 2026-10-23) plus Cloudflare Access, or coachd's built-in auth (builtin mode: passkey sessions plus coachd's own OAuth 2.1 authorization server, `builtin-auth.md`; owner decision 2026-10-24). The bullets below describe Access mode.**
  - **Users:** remote agents that connect from vendor clouds (claude.ai, Claude Duet, ChatGPT), and the approval page.
  - **MCP sign-in:** Access Managed OAuth acts as the OAuth authorization server.
  - **Access policy:** a single allowed identity with MFA at the identity provider.
  - **Request checks:** coachd validates `Cf-Access-Jwt-Assertion` (signature, `aud`, `iss`, `exp`) on every request. It never trusts the tunnel alone.
- **Device API exception (R-M3-1).** `/device/v1/*` is the one public path not checked against Access JWTs. It is opt-in (`COACHD_DEVICE_API=on`) and authenticated by a revocable per-device token (only its SHA-256 is stored); it can only read the desired set, download FIT files in it and acknowledge. It cannot approve, decline or withdraw anything. The private lane never serves it. In Cloudflare it is a separate path-scoped Access application (`coachd.<domain>/device/v1`) with a single Bypass policy; the main application never gets a Bypass policy, which would disable Access for the whole hostname.
- **Private lane: Tailscale, without Funnel,** for local clients: Claude Code, Cursor, Open WebUI and local models.
  - These use scoped static tokens: stored hashed, with expiry, rotatable, and with the creation date shown.
- **No published host ports except one (owner decision 2026-10-23).** coachd's public listener is published on `127.0.0.1:8080` only, for the host cloudflared. The private (:8081) and ops (:9090) listeners stay on the internal Docker network. Monitoring stores and Alloy are also internal only.
- **Own authorization server:** built as builtin mode for the open-source release, so that no Cloudflare account is needed (`builtin-auth.md`).

**Secrets:**
- **What they are:** the Garmin key, ntfy token, HMAC key, and Tailscale and Cloudflare credentials. The Grafana Cloud token belongs to Alloy.
- **How they are supplied:** as Docker secret files with mode `0600`, never as image layers or command-line arguments.
- **Never logged, traced or exported.**

**Data at rest** [call]:
- SQLite and FIT files live on a single `/data` volume. Deployment docs require an encrypted disk (LUKS or equivalent).
- No app-level database encryption in v1, because SQLCipher would need cgo and break the static multi-arch build.

**Container:**
- Distroless static base, non-root UID, read-only root filesystem.
- All capabilities dropped, `no-new-privileges`, default seccomp profile. `/data` is the only writable path.

**Supply chain:**
- **Images:** multi-arch, built in CI. Signed with cosign keyless signing (OIDC), with an attached SBOM.
- **Scanning in CI:** `govulncheck` and an image scanner.
- **Dependencies:** pinned by digest and kept current with Renovate or Dependabot.
- **Size:** minimal dependency count is a review criterion.

**Input handling:**
- Every MCP and HTTP input is schema-validated and size-bounded.
- Per-client rate limits.
- Garmin free text is labelled as untrusted (§5).
- No tool sends data anywhere outside coachd.

**Audit log** [call]:
- A separate append-only table, retained for 1 year.
- Records tool calls (client ID, tool, outcome, no payloads), proposal transitions, approvals and declines, authentication failures, enrolments and revocations.
- Viewable on the web page.

**Backups** [call]:
- `coachd backup` writes a consistent SQLite snapshot (`VACUUM INTO`) plus a FIT manifest to `/data/backups`.
- Encrypted off-site copies use restic (documented recipe).
- The age of the last backup is a metric with an alert.
- *Note (2026-10-24, builder decision pending owner confirmation):*
  - **Restore:** `coachd restore <snapshot.db>` replaces the database from a snapshot. It requires `serve` stopped (a data-directory lock) and moves the previous database to `/data/pre-restore-<ts>/`.
  - **Retention:** `serve` backs up at start and then when the newest backup is 24 h old, and keeps the newest 7.
  - **Excluded:** `nonce.key` and the secrets (`deploy/secrets/`, `.env`) are not backed up; the key is recreated and in-flight ceremonies stop working.
  - **Refusals:** a snapshot whose manifest hash does not match, one from a newer schema, one with a non-empty `-wal` beside it, and a corrupt or non-coachd file are refused before anything changes.
  - **Carry-forward:** a restore keeps the previous database's revocations (tokens, passkeys, watches) and used approval nonces, so approval nonces stay single-use; it clears web sessions and pending enrolments and writes a `backup_restored` audit row. An unreadable previous database only produces a warning.

**Data leaving the host:**
- To the AI agent the runner connected. This is the purpose of coachd.
- Scrubbed traces and profiles to Grafana Cloud.
- Minimal notification text to ntfy.
- Nothing else.

## 11. Observability [owner: signals and storage; call: the specifics]

| Signal | Stored | Path |
|---|---|---|
| Metrics | VictoriaMetrics on the host | coachd OTel SDK → Alloy (OTLP) → remote write |
| Logs | VictoriaLogs on the host | coachd structured JSON logs → Alloy → VictoriaLogs |
| Traces | Grafana Cloud Tempo | coachd OTel SDK → Alloy → Grafana Cloud |
| Profiles | Grafana Cloud Pyroscope | Go pprof → Alloy → Grafana Cloud |

- **Viewing:** Grafana Cloud queries VictoriaMetrics and VictoriaLogs through the Private Data Source Connect agent, which runs on the host as an outbound tunnel. All verified 2026-10-23:
  - VictoriaMetrics uses the core Prometheus data source, which officially supports PDC.
  - VictoriaLogs uses the `victoriametrics-logs-datasource` plugin. It installs on Grafana Cloud and the vendor states PDC support. Grafana's own PDC list doesn't include it, so test it in M1.
  - Tempo trace-to-logs supports VictoriaLogs from Grafana 12.2, with a custom query such as `trace_id:="${__trace.traceId}"`.
- **Alloy exporters:**
  - `prometheus.remote_write` to VictoriaMetrics `/api/v1/write`.
  - `loki.write` to VictoriaLogs `/insert/loki/api/v1/push`, or `otelcol.exporter.otlphttp` to `/insert/opentelemetry`.
- **Configurability:** coachd only knows Alloy's OTLP endpoint. Routing per signal is entirely Alloy config, so other users can send everything to the cloud or keep everything local.

**Instrumentation:**
- **Metrics:**
  - Garmin: sync runs, results and duration per data type; API latency, errors and 429s; freshest data age; key days left.
  - MCP: calls per tool, client and outcome, with latency.
  - Workouts: proposals by state, warnings by type, approval latency.
  - Delivery and notifications: delivery outcomes, notification send outcomes.
  - Authentication: failures by lane.
  - Housekeeping: DB size, backup age.
  - Go runtime metrics.
- **Traces:**
  - One trace per Garmin sync.
  - One trace per MCP call.
  - A proposal's whole life (propose → notify → approve → deliver) linked by span links to the proposal ID, so one view shows where a workout got stuck.
- **Logs:** structured JSON. Every line in request context carries the trace ID.
- **Profiles:** CPU, heap and goroutine profiles.

**Redaction:** log fields and span attributes pass an allowlist. Only declared keys survive (IDs, dates, outcomes, durations, counts, error classes). Unit tests fail if a health value, token or free-text field reaches a log or span.

**Health:** `/livez` and `/readyz`, on the internal network only. Docker's healthcheck uses them.

**Dashboards and alerts as code in the repo:**
- One coachd dashboard.
- **Alert rules:**
  - sync failing for more than 3 h;
  - Garmin key expiring within 7 d;
  - delivery failed;
  - spike in authentication failures;
  - backup older than 48 h;
  - disk above 85%.

Alerts go from Grafana Cloud alerting to ntfy. Provisioning tooling: [builder].

*Note (2026-10-24, builder decision pending owner confirmation):*
- **Local profile:** `compose.local.yaml` adds a self-hosted Grafana, reached over Tailscale at `/grafana/`. No host port is published.
- **Egress:** the one new egress is the first-start download of the VictoriaLogs plugin (plus alert notifications to the runner's ntfy server).
- **Cloud-only signals:** traces and profiles stay in Grafana Cloud; the local dashboard's traces panel is empty.
- **Alert rules** use threshold nodes and the data-source UID `coachd-vm`.
- **New metrics:** `coachd.backup.runs{outcome}`, `coachd.backup.age`, `coachd.device.requests{endpoint,outcome}` and `coachd.device.last_seen_age{device_id}`.

**Resource budget on a Pi 4 (4 GB)** [call]:
- coachd under 64 MB RSS.
- The whole stack (coachd, Alloy, VictoriaMetrics, VictoriaLogs, PDC agent, tailscale) under 1 GB.
- Default retention 30 days for metrics and logs, configurable.

## 12. Tech stack [owner approved the proposal]

- **Language:** Go, current stable release.
- **MCP:** the official Go SDK (`github.com/modelcontextprotocol/go-sdk`), v1.8.0 or later (verified 2026-10-23).
  - v1.7.0 added the 2026-07-28 spec.
  - **Gotcha:** 2026-07-28 is only served when `StreamableHTTPOptions.Stateless = true`. Otherwise clients negotiate down to 2025-11-25. Use `ServerOptions.SupportedProtocolVersions` to serve both.
  - OAuth helpers: `auth.RequireBearerToken`, `auth.ProtectedResourceMetadataHandler` (RFC 9728) and `MatchesResource` (RFC 8707 audience).
  - Several `MCPGODEBUG` flags are removed in v1.9; don't depend on them.
- **Storage:** SQLite in WAL mode via `modernc.org/sqlite` (pure Go, no cgo, so static multi-arch builds work). Migrations are embedded in the binary. [call]
- **FIT encode and decode:** `github.com/muktihari/fit`. [call; builder verifies workout-file encoding]
- **Passkeys:** `github.com/go-webauthn/webauthn`.
- **Web:**
  - stdlib `net/http` routing and `html/template`, server-rendered;
  - vanilla JavaScript only for the WebAuthn ceremony;
  - no npm and no frontend build step.
- **Telemetry:** OpenTelemetry Go SDK. Pyroscope Go SDK, or Alloy scraping an internal pprof endpoint. [builder]
- **Watch app:** Monkey C with the Connect IQ SDK.
- **Packaging:**
  - One multi-arch image (linux/arm64 and linux/amd64).
  - A `compose.yaml` with coachd, tailscale, Alloy, VictoriaMetrics and VictoriaLogs, plus a `compose.cloud.yaml` override that adds the PDC agent and Grafana Cloud export. cloudflared runs on the host (owner decision 2026-10-23).
  - An optional fully local monitoring profile for runners without Grafana Cloud (milestone 4).

## 13. Milestones [call]

1. **M1, data gateway:**
   - Garmin Personal API ingest, backfill and export import; the store; summaries.
   - MCP read tools plus `request_sync`.
   - Both network lanes with authentication, including verifying Claude (including Duet) and ChatGPT connect through Access Managed OAuth.
   - Telemetry, redaction, Alloy routing, and a basic dashboard and alerts.

   Done when: a morning chat in Claude (a claude.ai custom connector) reads real data from a self-hosted instance. Once Claude Duet is available, verify that it can use the coachd connector.
2. **M2, context and workouts:**
   - goal and notes;
   - `propose_workout` with sanity checks and warnings;
   - lifecycle and audit log;
   - ntfy, the approval page, and passkey enrolment;
   - FIT download delivery.
3. **M3, watch app:** the Connect IQ app, device enrolment, delivery acknowledgement.
4. **M4, for other runners:**
   - signed multi-arch releases;
   - install docs;
   - the local monitoring profile;
   - the backup recipe;
   - complete dashboards and alerts.

## 14. Known risks and open verifications

- **Garmin terms.** The owner reviewed the Personal API Supplemental Terms (last updated 2026-10-01) on 2026-10-23. Summary, not legal advice:
  - **Key:** personal. It may be used only in applications the key owner develops or operates for their own use, and must never be shared or disclosed to a third party. coachd keeps the key server-side only. It never reaches agents, logs, telemetry or the web page.
  - **Rate limits:** Garmin may impose and change them without notice and throttle or suspend access. Hence conservative polling and backoff (§5).
  - **Violations:** may lead to revocation of the key.
  - **Sharing:** the terms have no clause restricting where the owner sends their own data. The onward-sharing restriction comes from the Developer Program terms, which on their face don't govern the Personal API. So the public lane to cloud agents goes ahead. [owner, via review]
  - **Self-hosting:** each self-hosting runner uses their own key, which fits.
  - **Hosting for others:** a hosted coachd for others would breach §1 and needs the Developer Program (§2).
  - **Not reviewed:** the general Garmin Terms of Use, which these supplement.
- **Personal API:**
  - Undocumented rate limits (the token endpoint returns no rate-limit headers).
  - Token exchange verified against the real API on 2026-10-23: `POST /personalapitoken-service/personal-api/personal-api-key/token` with `Authorization: ApiKey <key>` returns HTTP 200, `Content-Type: text/plain`, and a raw compact RS256 JWT as the body (not JSON). Lifetime is 24 h (`exp - iat = 86400`). Claims: `client_ecosystem`, `client_id`, `client_type`, `exp`, `garmin_guid`, `iat`, `iss`, `jti`, `managed_status`, `revocation_eligibility`, `scope`. coachd reads only `exp`, unverified, for cache timing.
  - Unknown whether FIT files are stripped of proprietary fields.
  - The API is two days old, so expect changes.
- **Connect IQ:**
  - Which watch models support `PersistedContent` app workouts. The owner's fēnix 7S Pro is a current Connect IQ device and expected to support them. Confirm against the SDK device list and the simulator at the start of M3, before writing the app. The M3 target is the fēnix 7S Pro with Android.
  - Behaviour when the phone is disconnected.
  - Getting the watch through Cloudflare Access.
- **Claude Duet** (announced 2026-10-21): proactive morning brief and check-ins every few hours, using the "connected apps" from the Claude account.
  - It is a research preview with a waitlist, **US only**, on Pro and Max plans only.
  - **Unverified:** whether user-added custom remote MCP connectors work in Duet. Anthropic only names marketplace apps.
  - Fallback if Duet is unavailable or can't use coachd: the runner opens the morning chat in Claude or ChatGPT. coachd is unaffected, because it is passive either way.
  - Like claude.ai custom connectors, Duet connects from Anthropic's cloud, so it needs the public lane.
- **ChatGPT:** needs `offline_access` and refresh tokens from the authorization server. Write actions may be limited by plan. It is second priority after Claude, the owner's client, but still verified in M1.
- **The spike warning** rests on one cohort study. It is a warning, never a block.

## 15. Left to the builder
- Repo layout and package structure.
- Database schema.
- Exact tool parameter shapes beyond §9.
- Exact sanity-bound defaults within §8.
- Polling backoff details.
- Dashboard contents beyond §11.
- CI pipeline.
- Grafana provisioning tooling.
- The watch-API Access approach (§8).
