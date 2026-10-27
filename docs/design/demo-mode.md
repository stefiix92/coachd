# coachd: demo mode

Status: design, 2026-10-27. Extends `coachd-design.md` §5 (Data in). Tags as in the main design:
**[owner]** = owner decision, **[call]** = designer's call, **[builder]** = left to the implementer.

## 1. Intent

Trying coachd today needs a Garmin Personal API key, and `serve` refuses to start without one. Demo
mode lets someone run the real server on synthetic Garmin data and connect an agent in a few
minutes. Everything after ingest is unchanged: lanes, tokens, built-in auth, passkeys, proposals
and the device API.

## 2. Goals, non-goals, anti-goals

**Goals**
- `COACHD_DEMO=true` starts `serve` without the Garmin key settings, on deterministic demo data.
- The data looks like a plausible runner's, so every MCP tool has something to return.

**Non-goals**
- A hosted demo, sample conversations, or a page banner.
- Configurable personas or data volume.

**Anti-goals**
- Demo data in a real data directory, or real data in a demo one.
- Secrets on serve's stdout, which is the JSON log stream: demo mode creates no token. The user
  runs `coachd token create` as usual. **[call]**
- Coaching logic in the generator. It writes numbers, not verdicts.

## 3. Switch

`COACHD_DEMO=true` (`true`, `false` or empty; anything else is an error), parsed in
`internal/config`. **[call]** With it, `serve` uses `internal/demo` instead of the Garmin client and
does not require `COACHD_GARMIN_API_KEY_FILE` or `COACHD_GARMIN_KEY_EXPIRES`. It refuses them when
set, so a real deployment's settings never start a demo by mistake. **[builder]** It logs one
warning, `demo mode: synthetic Garmin data, no Garmin calls`, with no keys beyond `component`.

## 4. Synthetic Garmin

`internal/demo` implements `ingest.Garmin`. **[call]**

- **Deterministic.** Every value is a pure function of the calendar date (and the runner's
  timezone, for start times), from a fixed splitmix64 generator rather than `math/rand`, so a Go
  release cannot change the data. **[builder]** The same date gives the same activities, IDs, health
  values and FIT bytes, so idempotent ingest works as with Garmin.
- **History.** Data reaches back 182 days from today. **[builder]** A backfill of the default 730
  days stores about 26 weeks, enough for every summary, and the FIT drain takes a few minutes, not
  ten.
- **As the day goes by.** An activity is listed once it has ended; a day's sleep, HRV, daily summary
  and weight from 07:00 local. **[builder]**
- **The runner.** Strength on Monday, intervals on Tuesday, easy on Wednesday, tempo or easy on
  Thursday, an indoor ride every third Friday, easy or recovery on Saturday and the long run on
  Sunday, some skipped. Four-week blocks with a lighter last week; every third block's long runs are
  trail runs. Daily sleep, HRV (last night, weekly average, baseline and status), resting heart
  rate, stress, Body Battery and SpO2 follow the previous night and day; weight on most Mondays and
  Thursdays.
- **Names** are neutral and obviously fake: "Demo easy run", "Demo intervals". They still travel as
  `name_untrusted`. **[call]**
- **IDs** are `90 000 000 000 + day number × 10 + slot`: stable, decodable to date and slot, far
  above Garmin's. **[builder]** `ActivityExists` is true for a finished generated activity and false
  for any other ID; `DownloadActivityFIT` returns `garmin.ErrNotFound` for those.
- **Payloads** are built as Garmin's JSON and decoded into the `internal/garmin` DTOs, so the raw
  JSON stored and the mapped values always agree. **[builder]**
- **FIT files** are real activity files written with `muktihari/fit`: `file_id`, timer events, one
  record per second (heart rate, distance, speed, altitude, cadence, power for rides), laps (per km,
  per workout segment or per 10 minutes), the session with the runner's feel and RPE on about two
  in three activities, and the activity message. They come from the same samples as the summary,
  and `internal/fitdata` decodes them. **[call]**

## 5. Garmin key

There is no key. Demo mode holds none (`garminkey.NoKey`, which reads nothing from the data
directory and cannot renew) and reports the expiry as 2099-12-31 to `get_status` and the key gauge,
so no expiry warning or alert fires. **[call]** The demo Garmin does not implement
`garminkey.Exchanger`, so the page offers no key renewal. **[call]**

## 6. Keeping demo and real data apart

A demo instance must never write into a real data directory, and a real one must never start on a
demo directory. **[call]**

- **The mark** is a row in a new `instance` table (`key 'mode' = 'demo'`, migration 0014), written
  when demo mode starts. **[builder]** No table held such a fact before; a row in the database, not a
  marker file, travels with backups and restores and cannot be separated from the data it marks.
- **At start**, before the store is opened and migrated, `serve` reads the mark with a plain SQLite
  connection. A data directory without `coachd.db` (or with an empty one and no `-wal`) is new, and
  either mode may claim it. Otherwise demo mode refuses an unmarked database and real mode refuses
  a marked one, each with an error that says what to do: use an empty data directory, or (for a
  demo directory) set `COACHD_DEMO=true`.
- **Restore** refuses a snapshot whose mark differs from the current `coachd.db`'s, before anything
  is moved. **[builder]** A demo snapshot restores into a demo or an empty directory; there, serve's
  check at start still applies.
- So the demo starts `serve` first and creates tokens after: a `coachd.db` created by `token create`
  before the first demo start is unmarked, and demo mode refuses it.

## 7. Docs

A short "Try it with demo data" page: `docker run` of the release image with `COACHD_DEMO=true`,
publishing the private listener on the host's loopback only (`127.0.0.1:8081`), then `token create`
and `claude mcp add` over `http://127.0.0.1:8081/mcp`. **[owner]** (2026-10-27: a demo-only exception to
"never publish :8081".) The deployment stack is unchanged
and still publishes only `127.0.0.1:8080`; this throwaway demo is the one place the docs publish
`:8081`, on loopback. `:9090` stays unpublished.

## 8. Tests

- Config: the values of `COACHD_DEMO`.
- Demo: determinism across calls and days, the history window and paging, appearance during the
  day, plausibility, every payload through the ingest mappers, FIT files through `fitdata` agreeing
  with the summary, unknown IDs.
- Store: the mark, reading it without migrating, snapshots reporting it.
- Serve: an end-to-end demo start with an MCP agent on the private lane (activities, FIT detail,
  daily health, key expiry, one allowlisted log line, no token logged); both refusals; an empty
  file; the key settings refused in demo mode.
- Restore: demo into real and real into demo refused, demo into empty and demo allowed.
