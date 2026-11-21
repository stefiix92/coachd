# coachd: activities from intervals.icu

Status: design, 2026-11-18. Extends `coachd-design.md` §5 (Data in). Tags as in the main design:
**[owner]** = owner decision, **[call]** = designer's call, **[builder]** = left to the implementer.

## 1. Intent

The owner rides indoors on MyWhoosh. MyWhoosh does not send rides to Garmin Connect, so coachd never
sees them and the agent plans with holes in the training history.

MyWhoosh sends finished rides to intervals.icu by itself. intervals.icu has an open API with a
personal API key and serves each activity's original file. After this change coachd also pulls
activities from intervals.icu, so a MyWhoosh ride shows up in `list_activities`, `get_activity` and
the summaries with no work per ride. **[owner]**: no manual step per ride is acceptable.

The same path brings in any activity that reaches intervals.icu but not Garmin (Wahoo, Zwift
without a Garmin link, a file dropped in Dropbox). It is not specific to MyWhoosh. **[call]**

Trainer rides are judged by power, and coachd's laps and series carry none today. So laps and series
gain power, for every source. **[call]**

## 2. Goals, non-goals, anti-goals

**Goals**
- Pull activities from intervals.icu on every sync, with their original FIT file, and store them
  as activities beside Garmin's. **[owner]**
- Never show one session twice: when both sources hold it, the richer copy is shown. **[owner]**
- Average power per lap and power in the series, from the FIT file, for all activities. **[call]**
- The agent sees where an activity came from. **[call]**

**Non-goals**
- Strava, by API or otherwise. Strava's API Policy (effective 2026-06-01) forbids using its data
  in an AI application's context (§5.3), exposing it through an MCP server (§5.16b) and keeping it
  longer than seven days (§6.2). **[owner]** dropped it for that reason. intervals.icu itself
  returns only an empty stub for Strava-sourced activities; coachd skips them.
- Daily health data (sleep, HRV, weight) from intervals.icu. Garmin stays the only source.
- Sending planned workouts to intervals.icu or MyWhoosh.
- Planned vs actual for these rides. A MyWhoosh file carries no coachd workout, so the existing
  matching finds nothing; that is correct.
- intervals.icu OAuth, webhooks, or more than one athlete. Personal API key and polling only.
- Changing the key on the page. It is a secret file, set once; it does not expire.
- Cadence or running power.
- Decoding GPX or TCX. (An activity whose original is not FIT gets laps and series from the FIT
  file intervals.icu generates, §9a.)

**Anti-goals**
- The intervals.icu key in any log, span, metric, audit row, page, MCP result or backup.
- An intervals.icu failure hurting Garmin sync, or the other way round.
- A copy lost. The copy that is not shown stays stored, with its raw JSON and FIT file, and is
  shown again as soon as the other one goes away.
- Verdicts. intervals.icu's own load and fitness numbers (training load, CTL, intensity) are not
  taken in. **[call]** coachd computes its own numbers and the agent judges.

## 3. Terms

Read 2026-11-18. The intervals.icu API Terms (effective 2025-10-23) license the API "for any lawful
purpose, including commercial use", with no clause on AI use or on storing data. Rate limit for API
keys: 2500 requests per rolling 15 minutes and 5000 per day; a 429 carries `Retry-After`. coachd
makes one list call per sync (one per 90 days on the first) and one file call per new activity.

## 4. Configuration

- `COACHD_INTERVALS_API_KEY_FILE`: path to a file holding the key, read with the same secret-file
  checks as the Garmin key. Unset means the feature is off and nothing in this document runs.
  **[call]**
- `COACHD_INTERVALS_API_KEY`: the key itself, as an alternative (**[owner]**, 2026-11-20, for a
  set-up with no compose override). Surrounding whitespace is trimmed; empty means unset. Setting
  both is a start-up error. The value is server-side all the same, but anyone who can read `.env`
  or run `docker inspect` on the host can read it; the file is the stricter choice.
- The athlete ID is `0` (intervals.icu's "the key's own athlete"), so there is no second setting.
- Ignored with a start-up log line when `COACHD_DEMO=true`. **[call]**

## 5. The client

A new package `internal/intervals`, shaped like `internal/garmin`: no store, no I/O but HTTP.

- Base URL `https://intervals.icu`, HTTP basic auth with user `API_KEY` and the key as password.
- A `User-Agent` of `coachd/<version>` (intervals.icu's Cloudflare challenges default library
  agents). No contact details in it.
- `ListActivities(from, to)`: `GET /api/v1/athlete/0/activities?oldest=<date>&newest=<date>`.
- `DownloadFile(id)`: `GET /api/v1/activity/{id}/file`. The body is the original file,
  gzip-compressed; the client returns it decompressed, capped at the size the Garmin FIT download
  allows. **[builder]**: confirm by a real call whether gzip arrives as `Content-Encoding` or as a
  `.gz` body, and handle what is there.
- `ActivityExists(id)`: `GET /api/v1/activity/{id}`; false only on 404.
- Errors as in `internal/garmin`: unauthorized (401, 403), not found, rate limited with the retry
  time from `Retry-After`, and a status error that carries only the endpoint and status.

The field names in §6 come from intervals.icu's OpenAPI document (`/api/v1/docs`) as summarised on
2026-11-18, not from a live call. **[builder]** checks each against that document before coding the
DTO, and the owner supplies one real list response and one real file from a MyWhoosh ride as test
fixtures before the plan's first task. **[owner]**, to do.

## 6. What is imported

An intervals.icu activity is stored when all of these hold:

1. Its ID is `i` followed by digits. (Strava stubs have other IDs and no data.)
2. `source` is neither `STRAVA` nor `GARMIN_CONNECT`. A Garmin activity reaches coachd from Garmin.
3. It has a start time and a positive elapsed time.
Rule 2 and the duplicate rule below keep one session from showing twice.

**Duplicates: the richer copy wins** **[owner]**

After both sources have synced, coachd looks at the sync window for pairs of one Garmin and one
intervals.icu activity that are the same session: same coachd sport, and the time both cover
(`[start, start + duration]` in UTC, from the stored `StartUTC` and `DurationS`) is at least half of
the shorter one. **[call]** So a walk recorded on the watch, or a ride that only touches the next
one, is not a duplicate.

Of such a pair, one is shown and the other hidden:

1. The copy with power wins over the copy without. Power is known from the list entry: Garmin's
   `avgPower`, intervals.icu's average watts.
2. Else the copy with heart rate wins over the copy without.
3. Else Garmin's copy wins: it carries Garmin's derived values and the workout the watch followed.

**[call]** for the order of 1 to 3; the owner said "the richer one". Numbers only, no judgement of
the session.

The hidden copy is marked with the ID of the copy that hides it (§7). Every read, summary and
planned-vs-actual match skips it, exactly as they skip a soft-deleted activity. The step is
recomputed for the window on every sync, so it is idempotent and it reverses itself: when the
winner is deleted at its source, or a late copy turns out richer, the marks follow. The two copies
can carry different local dates: a mark whose winner is dated outside the window stays as long as
the winner is not deleted. A hidden copy's FIT file is still downloaded.

Two things are accepted **[call]**: when the intervals.icu copy wins, Garmin's derived values for
that session are not shown; and the post-activity webhook may fire for a copy that a later, richer
copy then hides (its reader fetches the activity anyway).

**Mapping**

| coachd | intervals.icu |
|---|---|
| `source` | `intervals_icu` |
| `source_id` | `id`, as given (`i55751783`) |
| start, date, week | `start_date` (UTC) and `start_date_local`, as for Garmin |
| `type_key` | `type`, unchanged |
| sport | `Ride`, `VirtualRide`, `GravelRide`, `MountainBikeRide`, `EBikeRide` → cycling; `Run`, `VirtualRun` → running; `TrailRun` → trail_running; `Walk` → walking; `Hike` → hiking; `WeightTraining` → strength; anything else → other |
| name (untrusted) | `name` |
| distance, elapsed, moving, elevation gain | `distance`, `elapsed_time`, `moving_time`, `total_elevation_gain` |
| average and maximum heart rate | `average_heartrate`, `max_heartrate` |
| manual | `source` is `MANUAL` |
| Garmin's derived values | empty |
| FIT status | `pending` when `file_type` is `fit`, else `unavailable` |

The name is untrusted free text and is labelled exactly as a Garmin activity name is.

## 7. Storage

- Migration: `activities` gains `source TEXT NOT NULL DEFAULT 'garmin'` and `source_id TEXT`, with a
  unique index on `(source, source_id)` where `source_id` is not null.
- `activities.id` stays the one activity ID everywhere (detail rows, FIT paths, cursors, MCP). A
  Garmin activity keeps its Garmin ID. An intervals.icu activity gets the **negative** of the digits
  in its ID: `i55751783` → `-55751783`. **[call]** Garmin and intervals.icu both use large positive
  integers that could collide; the sign keeps the two apart, is stable across syncs (so the upsert
  stays idempotent) and needs no change to any table that refers to an activity. **[builder]**
  checks that nothing assumes `id > 0` (cursor encoding, FIT file names, URL and tool input
  validation, the device API) and fixes what does.
- The raw list entry is kept as `raw_json`, as for Garmin.
- `activities` gains `has_power INTEGER NOT NULL DEFAULT 0`, set from the source's list entry
  (average power above zero) on every upsert, and filled for existing Garmin rows from their raw
  JSON by the migration. The duplicate rule reads it.
- `activities` also gains `hidden_by INTEGER`, the ID of the richer copy, null for most rows. "Live"
  becomes: not soft-deleted and not hidden. It is separate from `deleted_at` because Garmin's upsert
  clears a soft delete on every sync.
- Every query that exists for Garmin's sake is limited to `source = 'garmin'`: the reconcile
  listing (`ActivityIDsInRange`), the FIT worker's Garmin queue, and the backfill. Queries that
  serve reads and summaries (`ListActivities`, `WeeklyVolume`, `Runs`, `MaxHR`) take every source.
- Backups cover the database and FIT files, so these activities are in them with no change.

## 8. Sync

A new data type `intervals_activities`, after `activities` in the sync order (so the duplicate step sees
fresh Garmin data), present only when the key is configured. It has its own `sync_state` row, outcome and
span like every other type.

- **Window**: as for Garmin activities (`today − 14`, or two days before the last success if that is
  earlier). With no success yet, the window starts at `today − COACHD_BACKFILL_DAYS`: that is the
  backfill. **[call]** FIT files then arrive through the worker at its usual pace.
- **List calls**: the window is listed in calls of at most 90 days, oldest first,
  `COACHD_BACKFILL_DELAY` apart, so a first window of two years stays inside the client's body cap
  and timeout; a normal window is one call. The first failed call ends the listing: what earlier
  calls stored stays stored, and the listing counts as complete (for the deletion probe and the
  type's success) only when every call succeeded. Each call asks for the day after its last day as
  `newest`, since it is unconfirmed whether intervals.icu reads `newest` as the start of that day;
  an activity listed twice is stored once, and the deletion probe and the duplicate step keep to
  the window itself.
- **Independence**: a Garmin 429 still skips the remaining Garmin types, as today, but not this
  one, and an intervals.icu 429 pauses only
  intervals.icu calls, until `Retry-After` (capped as Garmin's pause is). An unauthorized answer is
  outcome `auth_failed`. Its rate-limited outcome is `rate_limited`; Garmin's types keep
  `garmin_rate_limited`, and the Garmin rate-limit counter counts only Garmin. **[call]**
  **[builder]** checks the alert rules and dashboards in `deploy/` for anything keyed on the old
  string or on the set of data types.
- **Deleted in intervals.icu**: after a complete listing, a stored live intervals.icu activity in
  the window whose ID is not in the raw list response (before the rules of §6) is probed with `ActivityExists`. A 404 soft-deletes it with an
  `activity_deleted` audit row; any other answer keeps it. At most ten probes per sync.
  **[call]** No canary is needed as for Garmin: the listing itself just succeeded with the same key.
- **Restored**: an activity listed again after a soft delete is live again, as for Garmin.
- **Duplicates**: the step of §6 runs after this type, when it and `activities` both finished
  without error in this sync; otherwise the marks stay as they were. It also runs over each chunk
  of Garmin's activities backfill once the chunk is stored, so an older session both sources hold
  is shown once too; the backfill's closing recompute covers those dates.
- **FIT worker**: one queue, both sources. Each pending activity is downloaded from its own source;
  a pause on one source leaves the other draining. The file goes through the same integrity check,
  decoding and detail storage as a Garmin FIT. Its file is named by `source_id`
  (`fit/<yyyy>/i55751783.fit`), not by the negative ID, so no file name starts with a dash.
  **[call]** A 404 or a non-FIT body is
  `unavailable`.
- **Events**: the post-activity webhook fires for a recent intervals.icu activity exactly as for a
  Garmin one.
- Summaries recompute after the sync as today; these activities count in weekly volume by sport.

## 9. Power in laps and series

- `fitdata.Lap` gains `AvgPowerW *int` (lap `avg_power`; nil when the file has none) and
  `fitdata.Point` gains `PowerW *int` (record `power`). Downsampling treats power as it treats heart
  rate.
- `get_activity` shows them as `avg_power_w` on a lap and `power_w` on a series point, null when
  absent. The tool description says so.
- The cached detail changes shape, so the migration also runs `DELETE FROM activity_detail`; the
  cache rebuilds from stored FIT files as `store/detail.go` describes. The golden test is updated.

## 9a. Power and intervals.icu's estimates on the summary (0.6.2)

Added after the first week of use, on the agent's feedback. **[owner]**, 2026-11-21.

- **Average power on the summary:** `avg_power_w` on every activity, from the list entry of either
  source (Garmin `avgPower`, intervals.icu `icu_average_watts`); null when absent or zero. It is a
  measurement, so it sits beside heart rate, not under a derived block.
- **`intervals_derived`:** `training_load` (`icu_training_load`), `normalized_power_w`
  (`icu_weighted_avg_watts`) and `intensity_factor` (`icu_intensity` / 100, so 0.65). All null for
  a Garmin activity, as `garmin_derived` is for an intervals.icu one. They are intervals.icu's
  estimates on intervals.icu's scales and depend on the FTP set there: coachd passes them on as
  numbers, never adds them to Garmin's values, and computes nothing from them. **[call]** No weekly
  sum: coachd sums no training load for Garmin either; the agent adds up what it needs.
- **Generated FIT as fallback:** when the original file is no usable FIT (a GPX, a TCX, a 404), the
  FIT worker asks for `GET /api/v1/activity/{id}/fit-file`, the FIT file intervals.icu builds from
  the activity's streams, and stores that one under the same path. So an activity is `pending`
  when its list entry has any `file_type`, and `unavailable` only without one (a manual entry) or
  when neither file is a usable FIT. Checked live: a GPX upload gave one lap and a full series
  with heart rate, altitude and distance. The stored file is then not the original, which stays
  on intervals.icu. **[call]**
- Migration `0017` adds the columns, fills them from the stored raw JSON and sets `unavailable`
  intervals.icu activities that have a `file_type` back to `pending`.

## 10. What the agent sees

- `list_activities` and `get_activity`: a new field `source`, `garmin` or `intervals_icu`. The `id`
  description becomes "coachd activity ID" and says intervals.icu activities have negative IDs.
  `garmin_derived` is all null for an intervals.icu activity.
- `avg_power_w` and `intervals_derived` on both tools (§9a).
- `get_status`: the new data type appears among the sync states when configured.
- `request_sync` runs it with the rest.
- No new tool, and no agent path to the key or to intervals.icu itself.

## 11. Telemetry

- No new attribute keys. `data_type` takes the new value; `activity_id` may be negative.
- The allowlist tests cover the new log lines and spans.
- The key and the `Authorization` header never reach a log or span; client errors carry only
  endpoint and status, as Garmin's do. A test asserts the key is absent from logs on every error
  path.

## 12. Docs

- User docs: a page on connecting intervals.icu (create the key, link MyWhoosh in intervals.icu,
  set the variable), the overlap rule, and that Strava-sourced activities never come through. The
  tools reference gains `source`, `avg_power_w` and `power_w`.
- `deploy/.env.example`, `deploy/compose.yaml` (the secret), `CHANGELOG.md`.
- `CLAUDE.md`: this document in the Status list; the server-side-key invariant names the
  intervals.icu key beside Garmin's; "Upserts are keyed by Garmin activity ID" becomes "by activity
  ID (Garmin's, or the negated intervals.icu ID)".

## 13. Approaches considered

1. **intervals.icu pull, original FIT** (chosen, **[owner]**). Automatic, terms allow it, reuses the
   whole FIT path, so these rides look like any other activity to the agent.
2. **Strava API pull.** The owner's first wish, because Strava collects from many platforms.
   Forbidden by Strava's policy three ways (§2 non-goals). Dropped.
3. **Strava's own MCP connector beside coachd.** Allowed, but needs a Strava subscription, and the
   rides never enter coachd's store or summaries. The owner chose not to build guidance for it.
4. **FIT upload on coachd's page**, or importing the file into Garmin Connect. Both need a manual
   step per ride. Rejected by the owner.
5. **MyWhoosh's unofficial API**, directly or through a tool that re-uploads to Garmin Connect
   posing as a Garmin device. Undocumented, needs the account password, and unofficial Garmin
   upload is out of scope for coachd. Dropped.

## 14. Tests

- Client: auth header, list and file requests, gzip handling, 401/403/404/429 with `Retry-After`,
  the size cap, the key absent from every error string.
- Mapping: each sport, the ID rule, a Strava stub and a Garmin-sourced entry skipped, manual,
  non-FIT file type.
- Duplicates: power beats none, then heart rate, then Garmin; a different sport or a short overlap
  is no duplicate; the hidden copy is absent from lists, summaries and `get_activity`; marks move
  when a richer copy arrives late and clear when the winner is deleted; the step is idempotent; a
  Garmin upsert does not unhide.
- Sync: off without the key; independent outcomes and pauses in both directions; first-sync window;
  idempotent re-sync; deletion probe (404 deletes, error keeps, cap of ten).
- Store: migration on a populated database; the Garmin reconcile never sees an intervals.icu
  activity; negative IDs through list cursors and FIT paths.
- FIT: the owner's MyWhoosh fixture decodes with laps, heart rate and power; a Garmin FIT now
  yields lap and series power.
- MCP: `source`, `avg_power_w`, `power_w` in the outputs and schemas.

## 15. Left to the builder

- DTO field names, after checking the OpenAPI document and the owner's fixtures.
- Where the source dispatch lives in the FIT worker, and how the ingest `Service` takes the optional
  client.
- Whether the home page shows an intervals.icu line; if it does, only "connected" and the last
  sync time.
- Wording of docs and log messages.

