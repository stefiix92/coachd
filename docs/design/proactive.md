# coachd: proactive events and messages

Status: approved by the owner 2026-11-21. Extends `post-run-webhook.md` and `watch-v2.md` (agent
messages). Tags as in the main design: **[owner]** = owner decision, **[call]** = designer's call,
**[builder]** = left to the implementer.

## 1. Intent

A coach starts the conversation: after a bad night, after a skipped session, at the end of a week.
coachd today tells the runner's automation about one moment only (an activity arrived), and an agent
can answer only on the watch.

coachd becomes proactive in the sense the owner agreed on 2026-11-21: it **wakes the runner's agent**
at more moments, and gives the agent a way to **reach the runner's phone and page**. **[owner]** It
still has no LLM, holds no LLM key and writes no coaching text. **[owner]** This replaces the
non-goal "Starting the morning conversation" in `coachd-design.md` §3: coachd still does not talk,
but it now says when there is something to talk about.

Where no agent is wired up, coachd can send fixed, numbers-free nudges itself. **[owner]**

## 2. Goals, non-goals, anti-goals

**Goals**
- Three new webhook events, with dates and IDs only: `health.day_ready`, `workout.unmatched`,
  `week.closed`.
- An agent message reaches the watch (as today), the runner's page, and the phone as an ntfy push.
- Optional ntfy nudges for the same three moments.
- Events and nudges change nothing until the runner opts in. Agent messages on the page, and the
  message push for everyone with ntfy configured, are on after the upgrade.

**Non-goals**
- An LLM, an LLM key, prompts or a scheduler for agents inside coachd. A companion container that
  runs an agent from these events is a separate project.
- Events about thresholds on health values ("HRV low for 3 days"). That is evaluation and belongs to
  the agent, which is woken daily and can look. **[call]**
- A message inbox with read state, replies or threads.

**Anti-goals**
- Health values, names, note text or agent text in a webhook event or an ntfy push.
- A verdict word in a nudge.
- A flood: a restart, a backfill or a week of downtime must send no burst.
- An agent that can make the phone buzz without limit.
- An existing `activity.stored` receiver getting events it did not ask for.

## 3. Events

| Event | Fires when | Body |
|---|---|---|
| `activity.stored` | unchanged (`post-run-webhook.md`) | unchanged, byte for byte |
| `health.day_ready` | today's `daily_health` row first has a sleep duration above zero | `{"event":"health.day_ready","date":"2026-11-21"}` |
| `workout.unmatched` | at or after 06:00 local, once yesterday's activities are in (below), a proposal dated yesterday is `delivered` and no activity carried it out | `{"event":"workout.unmatched","date":"2026-11-20","proposal_id":"…"}` |
| `week.closed` | Monday at or after 06:00 local | `{"event":"week.closed","date":"2026-11-16"}` (the Monday that started the closed week) |

- `workout.unmatched`, not "missed": coachd knows only that no stored activity matches. **[call]** A
  proposal that was approved but never reached the watch expires and sends nothing. **[call]**
- 06:00 gives a late evening activity time to sync. **[call]**
- Yesterday's activities are in when both hold: (a) the `activities` sync state shows a successful
  Garmin activities sync at or after 06:00 local today, and (b) no activity dated yesterday or today
  still has its FIT file `pending` (the link to a proposal needs the FIT's recorded steps,
  `workout-outcome.md`). Until both hold, `workout.unmatched` is not evaluated: nothing is sent or
  marked, and the next check tries again. This orders the check; it evaluates nothing. **[call]**
- Each event fires once per key (the date, or the proposal ID), remembered in the database, so a
  restart repeats nothing. Events are evaluated only for today (`health.day_ready`), yesterday
  (`workout.unmatched`) and, on a Monday, the week just closed, so downtime sends no burst when
  coachd comes back: at most one `health.day_ready`, one `workout.unmatched` per proposal delivered
  for yesterday, and one `week.closed`. **[call]**
- `week.closed` is checked only on Mondays. If coachd is down all Monday, that week's event is not
  sent, also not on Tuesday. **[call]**
- An event nobody is configured to receive (not in `COACHD_WEBHOOK_EVENTS`, and nudges off) is not
  marked, so turning it on later starts with the current day or week. When nobody receives any of the
  three, no check runs. **[call]**
- A check runs every 5 minutes, independent of the sync. **[call]**

## 4. Webhook

`COACHD_WEBHOOK_EVENTS` is a comma-separated list of the four event names. Its default is
`activity.stored`, so an existing receiver sees no change. **[call]** An unknown name is a start
error. The webhook receives day, workout and week events only when its list holds them; an event
neither it nor a nudge receives is not marked (§3). Delivery, retries, queue and headers are those
of `post-run-webhook.md` §5. The span for the new events is `event.webhook` with `event`, `outcome`
and, when present, `proposal_id`. `event` joins the telemetry allowlist: its values are the four
fixed names.

## 5. Messages

`send_message` keeps its input and output. A stored message now also:

- shows on the home page under "Messages": the newest 5 from the last 7 days, newest first, marked
  as written by an agent. The text is escaped like any other agent text.
- sends an ntfy push when ntfy is configured: the fixed text "Your agent left a message" with a link
  to the page. The message text never goes to ntfy. **[call]**

At most one message push per 15 minutes; a message inside that window is stored and shown but not
pushed. **[call]** The window starts only when a push was actually queued, and a push held back by
the limit is not counted in metrics. The tool description says all of this.

## 6. Nudges

`COACHD_NUDGES=true` (default `false`) needs `COACHD_NTFY_URL`; without it coachd does not start.
Each of the three new events then also sends one ntfy push with a link to the page:

| Event | Text |
|---|---|
| `health.day_ready` | "Last night's sleep data is in" |
| `workout.unmatched` | "Yesterday's workout has no matching activity" |
| `week.closed` | "Last week's numbers are complete" |

Nudges and the webhook are independent: either, both or neither.

A nudge and the message push use the span `event.notify`. A nudge's span and logs carry `event`
(and `proposal_id` for `workout.unmatched`); the message push carries no `event` key.

## 7. Documentation

A new page `docs/content/docs/agents/proactive.md`: the events, the two switches, and one worked
recipe that turns an event into a headless agent run which reads coachd and answers with
`send_message`. The exact command line of the agent platform is the builder's to verify against that
platform's current documentation. **[builder]**

## 8. Tests

- Triggers: each event fires once at its moment and never twice; nothing before 06:00; a restart
  with marks present sends nothing; a matched workout sends nothing; a first run on a database with
  months of history sends only today's, yesterday's and the closed week's events; `workout.unmatched`
  waits for a morning activities sync and for pending FIT files; an event no sink wants is not
  marked; a sleep duration of zero is not a ready day.
- Webhook: the three bodies exactly; `activity.stored` body unchanged; an event outside
  `COACHD_WEBHOOK_EVENTS` is not sent.
- Ntfy: the fixed texts and links; no message text in a request; the 15-minute limit.
- Page: messages listed, escaped, none older than 7 days.
- Config: the list parser, the default, an unknown name, nudges without ntfy.
- Telemetry: the allowlist test still passes with `event`.

## 9. Later, separate designs

In this order **[call]**: (1) memory — season plan, coach journal, athlete profile; (2) senses —
daily check-in, pain log, time in zones, best efforts; (3) a week view on the page. Each reads or
feeds what this design adds and needs its own approval.
