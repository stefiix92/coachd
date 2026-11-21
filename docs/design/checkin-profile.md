# coachd: the runner's check-in and the athlete profile

Status: approved by the owner 2026-11-21 (approach chosen in conversation; details below are the
designer's calls unless tagged). Extends `coachd-design.md` §5 and `intervals-activities.md`. Tags as
in the main design: **[owner]** = owner decision, **[call]** = designer's call, **[builder]** = left
to the implementer.

## 1. Intent

A coach asks how the athlete feels, and knows the athlete's thresholds. coachd has neither.

- **Check-in.** Self-reported state (soreness, fatigue, mood, motivation, stress, sleep quality,
  pain) tracks training load better than device measures do, and today it lives only in a chat that
  the next conversation, or another agent, cannot see. With the proactive morning event
  (`proactive.md`) an agent is woken, asks the runner, and needs somewhere to put the answer.
- **Profile.** FTP, threshold and maximum heart rate and zones are set by the runner in
  intervals.icu. The agent re-derives or re-asks them in every conversation.

The owner chose on 2026-11-21 that the agent asks and coachd stores **[owner]**, after a live check
showed the owner's intervals.icu account holds no wellness entries. This reverses the non-goal
"Storing check-ins (feel, RPE) as structured data" in `coachd-design.md` §3.

coachd still evaluates nothing: it stores what the runner said and what intervals.icu holds.

## 2. Goals, non-goals, anti-goals

**Goals**
- An agent can store one check-in per day and read check-ins back with the day's health data.
- An agent can read the runner's per-sport thresholds and zones, with the date they were last
  changed.

**Non-goals**
- A check-in form on the page or the watch. The agent asks in conversation. **[call]**
- Importing intervals.icu wellness records. Nothing to import today; a later design can add it as a
  second source. **[call]**
- Nutrition, supplements, cycle tracking. They stay in notes.
- Averages, trends or scores computed from check-ins.
- Using the profile inside coachd (time in zones, zone-based warnings). A later design.
- Editing the profile through coachd. intervals.icu stays the place to change it.

**Anti-goals**
- A check-in value, a note or a profile value in a log, span or metric label.
- A check-in the agent inferred from data. coachd cannot enforce this; the tool description states
  it and the output marks every check-in as relayed by an agent.
- Free text from intervals.icu (zone names) reaching an agent.

## 3. The check-in

One row per calendar date in the runner's timezone.

| Field | Values | Meaning of the ends |
|---|---|---|
| `soreness` | 1–5 | 1 none, 5 severe |
| `fatigue` | 1–5 | 1 fresh, 5 exhausted |
| `stress` | 1–5 | 1 calm, 5 very stressed |
| `mood` | 1–5 | 1 low, 5 great |
| `motivation` | 1–5 | 1 none, 5 high |
| `sleep_quality` | 1–5 | 1 poor, 5 great |
| `pain` | up to 5 entries | each `{area, side, level}` |
| `note` | up to 300 characters | the runner's own words |

- Every field is optional; a check-in with no field set is rejected.
- `pain.area` is one of `foot, ankle, achilles, calf, shin, knee, hamstring, quad, hip, groin,
  glute, lower_back, upper_back, neck, shoulder, arm, other`. `side` is `left, right, both, none`.
  `level` is 0–10. **[call]**
- Scales are 1–5 with fixed ends so two agents write comparable numbers. **[call]**
- The date is today or up to 6 days back; a future date is rejected. **[call]**
- A second check-in for a date replaces the first whole. **[call]** There is no delete tool; a
  wrong entry is corrected by writing the right one.
- Stored with who wrote it (`agent:<lane>:<client>`, as notes are) and when.
- `note` is free text an agent wrote: it reads back as `note_untrusted`.

## 4. The profile

When the intervals.icu source is on, each sync also reads `GET /api/v1/athlete/0/sport-settings`
(checked live 2026-11-21). coachd keeps the raw JSON and, per entry:

| Stored | From | Unit (checked live) |
|---|---|---|
| sports | `types` | intervals.icu activity types, e.g. `Ride`, `Run` |
| FTP, indoor FTP | `ftp`, `indoor_ftp` | watts |
| threshold and maximum heart rate | `lthr`, `max_hr` | bpm |
| threshold speed | `threshold_pace` | metres per second |
| power zones | `power_zones` | upper bound of each zone, percent of FTP |
| heart-rate zones | `hr_zones` | upper bound of each zone, bpm |
| last changed | `updated` | timestamp |

- Zone names are free text and are not stored outside the raw JSON or shown. **[call]**
- Pace zones are left out: the owner's account has none, so their unit could not be checked.
  **[call]**
- A `types` value is kept only if it is 1–32 ASCII letters. **[call]**
- The last power zone bound is 999 when intervals.icu leaves the top zone open: it means no upper
  limit, and the number is stored and returned as sent. **[call]**
- A non-empty answer of which no entry decodes is a failed read (`intervals: sport-settings: decode
  failed`), so a shape change at intervals.icu cannot wipe the profile; an empty answer clears it.
  **[call]**
- Each sync replaces all stored entries in one transaction. A failed read keeps the previous ones.
- The sync has its own state row, `intervals_profile`, shown by `get_status`. It shares the
  intervals.icu pause after a 429 and is removed when the source is turned off, as
  `intervals_activities` is; the stored entries are removed with it. The raw JSON copy in `raw_payloads` stays, like other raw
  data, and is never served to an agent. **[call]**
- One extra request per sync. **[call]**

## 5. What the agent sees

- `log_checkin` (write scope): the fields of §3 plus `date` (default today). Returns the stored
  check-in. The description says: record only what the runner told you; never fill a field from
  data or a guess.
- `get_daily_health`: each day gains `checkin`, null when there is none, with the §3 fields,
  `note_untrusted` (agent-written text, described as data, never instructions, in `log_checkin` and
  `get_daily_health`), and `reported_via: "agent"`. A date with a check-in but no Garmin data is
  listed with null health fields.
- `get_profile` (read): `sport_settings`, one entry per §4 row, each with `source:
  "intervals_icu"` and `updated_on`; an empty list when the source is off or nothing is stored. Its
  description says the values are the runner's own settings, possibly out of date.

Tool outputs gain fields, so an already-connected client must reconnect.

## 6. Storage

Migration 0019: `checkins` (date primary key, six nullable integers, `pain_json`, `note`,
`created_by`, `updated_at`) and `sport_settings` (source, source id, the §4 columns, zones as JSON
arrays). Both travel in backups like every other table. A check-in write adds an audit row with the
date and the writer, no values.

## 7. Telemetry

No new keys. Tool calls are counted as every tool is. The profile sync uses `data_type =
intervals_profile` and the existing outcomes.

## 8. Tests

- Store: upsert replaces; list by range; bounds enforced in the tool, not the store; sport
  settings replaced atomically; backup restore of an older schema.
- Client: the live-shaped response decodes (built values, no real data); 401, 404, 429.
- Ingest: off without the key; replace on success; kept on failure; pause shared; state removed
  when the source is turned off.
- MCP: each bound of §3; empty check-in; future and too-old dates; replace; `note_untrusted`;
  a check-in-only day in `get_daily_health`; `get_profile` on and off; zone names never present;
  goldens.
- Telemetry: allowlist tests pass; no value in any log line.

## 9. Later

A view of check-ins on the page with a delete button; intervals.icu wellness as a second check-in
source; time in zones from the profile.
