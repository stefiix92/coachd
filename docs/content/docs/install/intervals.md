---
title: intervals.icu
weight: 3
---

coachd can also pull activities from [intervals.icu](https://intervals.icu), for sessions that never reach Garmin Connect. MyWhoosh sends finished rides to intervals.icu by itself, so a MyWhoosh ride then shows up in `list_activities`, `get_activity` and the summaries with no work per ride. Anything else that reaches intervals.icu but not Garmin (Wahoo, Zwift without a Garmin link, a file dropped in Dropbox) comes through the same way.

It is off unless the key is set. Like the Garmin key, it never leaves the server: not to agents, logs, telemetry or the page.

## Connect intervals.icu

1. In intervals.icu's settings, under **Developer Settings**, create an API key.
2. Link MyWhoosh with intervals.icu there, so finished rides upload by themselves, and check that your last ride shows up on intervals.icu.
3. Store the key and mount it with a compose override:

   ```bash
   put_secret intervals_api_key 65532:65532     # the helper from Secrets
   ```

   ```yaml {filename="compose.override.yaml"}
   services:
     coachd:
       environment:
         COACHD_INTERVALS_API_KEY_FILE: /run/secrets/intervals_api_key
       secrets: [intervals_api_key]
   secrets:
     intervals_api_key:
       file: ./secrets/intervals_api_key
   ```

   ```bash
   docker compose up -d --force-recreate coachd
   ```

   If you already have a `compose.override.yaml` (for the [webhook token](/docs/install/notifications/#post-run-webhook)), merge the two. If `.env` sets `COMPOSE_FILE`, add `:compose.override.yaml` to it: compose reads the override by itself only without `COMPOSE_FILE`.

coachd reads the key only at start. The first sync reaches back `COACHD_BACKFILL_DAYS` (730 days by default) in one call; the FIT files then arrive one by one, `COACHD_BACKFILL_DELAY` apart. Later syncs look at the last 14 days, or further back after a gap. Demo mode ignores the key, with a line in the log.

## What comes through

- Every activity in intervals.icu except those that came from Strava (intervals.icu passes on only an empty stub for them) and those that came from Garmin Connect (coachd has them from Garmin already).
- Its original FIT file, decoded like Garmin's: laps, series, heart rate and power. An activity whose original is not a FIT file (GPX, TCX) gets a summary only.
- Not intervals.icu's own training load and fitness numbers: coachd computes its own, and your agent judges.

To an agent, such an activity has `source` `intervals_icu` and a negative `id` (`i55751783` on intervals.icu is `-55751783` in coachd), and its `garmin_derived` is all null. `get_status` lists the data type `intervals_activities`. An activity of the last 14 days that you delete in intervals.icu is removed from coachd at a following sync.

## One session, two copies

When both sources hold the same session, the richer copy is shown and the other is hidden. Two activities are the same session when they have the same sport and the time both cover is at least half of the shorter one. Of such a pair:

1. The copy with power wins over the copy without.
2. Else the copy with heart rate wins over the copy without.
3. Else Garmin's copy wins: it carries Garmin's derived values and the workout the watch followed.

So a MyWhoosh ride that the watch also recorded, with heart rate only, shows as the intervals.icu copy with power; Garmin's derived values for that ride are then not shown. The hidden copy stays stored, with its raw data and FIT file, and is shown again as soon as the other one goes away.

## When it fails

intervals.icu and Garmin sync independently: a failure of one never stops the other, and a rate limit from intervals.icu pauses only intervals.icu calls. `get_status` shows `auth_failed` for a rejected key and `rate_limited` while paused. The `GarminSyncFailing` [alert](/docs/operations/monitoring/#alerts) leaves intervals.icu out; the dashboard's **Last success age by data type** panel shows it.

To turn it off, remove the override and run `docker compose up -d --force-recreate coachd`. Activities already stored stay; `get_status` stops listing `intervals_activities`. Turned on again, the first sync reaches back `COACHD_BACKFILL_DAYS` again in one call, updating what is already stored rather than adding it twice.
