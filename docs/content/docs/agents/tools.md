---
title: MCP tools
weight: 1
---

Every tool coachd offers an agent. Write tools need the write scope and change only coachd's own store: none can approve, deliver or withdraw a workout. Values are measurements, not advice; fields under `garmin_derived` are Garmin's proprietary estimates.

## Garmin data

| Tool | Kind | What it returns |
|---|---|---|
| `get_status` | read | Sync freshness per data type (Garmin's, and `intervals_activities` and `intervals_profile` with [intervals.icu](/docs/install/intervals/)), how much history is stored, days until the Garmin key expires, and the workout proposals waiting for your approval. |
| `list_activities` | read | Activity summaries for a date range (at most 366 days), newest first, optionally one sport, a page at a time. `source` says where each came from: `garmin` or `intervals_icu`; an intervals.icu activity has a negative `id` and a null `garmin_derived`. `avg_power_w` is the average power when the source has it. `intervals_derived` holds intervals.icu's own training load, normalized power and intensity factor, null for a Garmin activity and never on Garmin's scales. |
| `get_activity` | read | One activity. Detail `summary` (default); `laps` adds per-lap splits (with `avg_power_w`); `series` adds heart rate, power (`power_w`), pace, distance and altitude, reduced to at most 600 points; power is null when the file has none. For a structured workout, `laps` and `series` add the planned and executed steps and the delivered proposal it carried out. `perceived` is your own post-activity rating. |
| `get_daily_health` | read | Per-day sleep, HRV, resting heart rate, SpO2 and weight, at most 92 days. Sleep score, HRV status and baseline, Body Battery and stress are under `garmin_derived`. `checkin` is your [check-in](#check-in-and-profile) for the day, null when there is none; a date with a check-in but no Garmin data is listed with null health fields. |
| `get_summaries` | read | Numbers computed at ingest, at most 366 days: weekly volume per sport (Monday-start weeks) and per-day rolling HRV, resting heart rate, sleep and longest-run figures. |
| `request_sync` | write | Ask coachd to sync from Garmin, and intervals.icu when configured, now. Rate-limited (`COACHD_SYNC_MIN_INTERVAL`, 10 min by default). |

## Goal and notes

| Tool | Kind | What it does |
|---|---|---|
| `get_goal` | read | Your active goal: the event's name, date, sport, distance and elevation gain, a free-text target and notes. |
| `set_goal` | write | Set the goal; the previous one is archived. Returns facts for planning: weeks to go, recent weekly distance and elevation, the longest run in 30 days. |
| `get_notes` | read | Durable notes by category (injury, availability, preference, other). Expired notes are left out unless asked for. |
| `upsert_note` | write | Create a note, or replace one an agent wrote. `valid_until` is its last day. At most 50 notes are kept. |
| `delete_note` | write | Delete a note an agent wrote. |

Agents can edit or delete only notes that agents wrote. Your own notes are read-only to them.

## Check-in and profile

| Tool | Kind | What it does |
|---|---|---|
| `log_checkin` | write | Store your check-in for a date: today or up to 6 days back in your timezone, never a future date. Every field is optional, at least one is required. A second check-in for a date replaces the first whole; there is no delete, so a wrong entry is corrected by writing the right one. Returns the stored check-in. |
| `get_profile` | read | Your settings from [intervals.icu](/docs/install/intervals/), read from coachd's store: `sport_settings`, one entry per sport-settings row, each with `source` (`intervals_icu`) and `updated_on`. An empty list when intervals.icu is off or nothing is stored. |

The check-in scales have fixed ends, so two agents write comparable numbers:

| Field | Values | Ends |
|---|---|---|
| `soreness` | 1 to 5 | 1 none, 5 severe |
| `fatigue` | 1 to 5 | 1 fresh, 5 exhausted |
| `stress` | 1 to 5 | 1 calm, 5 very stressed |
| `mood` | 1 to 5 | 1 low, 5 great |
| `motivation` | 1 to 5 | 1 none, 5 high |
| `sleep_quality` | 1 to 5 | 1 poor, 5 great |
| `pain` | up to 5 entries | each `{area, side, level}`, `level` 0 (none) to 10 (worst) |
| `note` | up to 300 characters | your own words |

`pain.area` is one of `foot`, `ankle`, `achilles`, `calf`, `shin`, `knee`, `hamstring`, `quad`, `hip`, `groin`, `glute`, `lower_back`, `upper_back`, `neck`, `shoulder`, `arm`, `other`. `pain.side` is `left`, `right`, `both` or `none`.

A check-in reads back with `reported_via` `agent`, `updated_at`, and the note as `note_untrusted`. coachd stores what the agent was told and judges nothing; the tool description tells the agent to record only what you said.

`get_profile` entries carry `sports` (intervals.icu activity types), `ftp_w` and `indoor_ftp_w` in watts, `threshold_hr_bpm` and `max_hr_bpm` in beats per minute, `threshold_speed_mps` in metres per second, `power_zone_upper_pct_ftp` (zone upper bounds, percent of FTP) and `hr_zone_upper_bpm` (zone upper bounds, beats per minute), lowest zone first. A value you have not set is null. `updated_on` is the date intervals.icu last changed the entry, so an agent can see how old a threshold is.

These tools added fields to existing outputs, so an AI client connected before the upgrade holds the old definitions: reconnect it.

## Workouts

| Tool | Kind | What it does |
|---|---|---|
| `propose_workout` | write | Propose a structured workout for today up to 14 days ahead. coachd checks physical plausibility only and stores a plausible one as `pending`, with warnings. |
| `cancel_proposal` | write | Cancel a proposal you have not decided on yet. |
| `list_workouts` | read | Proposals in a date range (at most 62 days), with their status, warnings, what they replace and, once delivered, the activities that carried them out. |

A workout has a sport (running, trail running, walking, hiking, cycling, indoor cycling, strength or other) and ordered steps: warmup, active, recovery, rest, cooldown, and repeats nested up to two levels. A step lasts a time, a distance, until you press lap, or until heart rate crosses a threshold. Its target is heart rate, pace, cadence, a free-text effort cue, or, for cycling, power in % of FTP or in watts.

Warnings never block. They cover a longest-run spike, several workouts on one day, heart rate above your 90-day maximum, a cycling workout Zwift cannot take, stale data and thin history.

To change a proposal, the agent proposes a new one with `replaces` set to its id. Statuses: `pending`, `approved`, `delivering`, `delivered`, `delivery_failed`, `withdrawn`, `declined`, `cancelled`, `superseded` and `expired`. A proposal that nobody approves stays `pending` and expires at the end of its date.

## Watch messages

| Tool | Kind | What it does |
|---|---|---|
| `send_message` | write | Store a message of up to 300 characters, shown on the watch app and on coachd's page. |

The watch app shows it and, with background checks on, notifies you within about 15 minutes. The page lists the newest 5 from the last 7 days under **Messages**, marked as written by an agent. With [ntfy](/docs/install/notifications/#ntfy) configured you also get a push that says only "Your agent left a message", with a link to the page; the text never goes to ntfy. At most one such push goes out per 15 minutes: a message inside that window is stored and shown but not pushed. A message is display-only: it cannot carry, start or change a workout. coachd keeps the newest 20; the watch shows those from the last 7 days.

## Untrusted text

Activity names and descriptions from Garmin and intervals.icu come back as `name_untrusted`: anyone who can name an activity could try to instruct the agent through it. The check-in note comes back as `note_untrusted`, and so do agent-written rationale, names and workout text. Agents should treat them as data, never as instructions.
