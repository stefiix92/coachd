---
title: Proactive events
weight: 3
---

coachd has no LLM and writes no coaching text. It can, though, tell your own automation when there is something worth talking about, and it gives your agent a way to reach your phone. This page covers the events, the two switches and a recipe that turns an event into an agent run.

## Events

The webhook ([ntfy and webhook](/docs/install/notifications/#post-run-webhook)) can send four events. Bodies carry dates and IDs only: no health value, name or note.

| Event | Fires when | Body |
|---|---|---|
| `activity.stored` | A recent activity is stored. | Unchanged: `{"event":"activity.stored","activity_id":…,"date":…,"sport":…,"proposal_id":…}` |
| `health.day_ready` | Today's sleep duration is first stored. | `{"event":"health.day_ready","date":"2026-11-21"}` |
| `workout.unmatched` | At or after 06:00 local, once yesterday's activities are in, a workout dated yesterday is `delivered` and no activity carried it out. | `{"event":"workout.unmatched","date":"2026-11-20","proposal_id":"…"}` |
| `week.closed` | Monday at or after 06:00 local. | `{"event":"week.closed","date":"2026-11-16"}`: the Monday that started the week that closed. |

- Each event fires once per date (or per proposal ID). It is remembered in the database, so a restart repeats nothing.
- coachd checks every 5 minutes, independent of the Garmin sync, and looks only at today, yesterday and the week just closed. After downtime you get at most one of each kind, never a burst.
- `workout.unmatched` waits until yesterday's activities are in: a Garmin activity sync has succeeded at or after 06:00 local today, and no activity from yesterday or today still waits for its FIT file to download (coachd matches a run to its workout by the steps in that file). Until then coachd sends nothing and checks again 5 minutes later.
- `workout.unmatched` says only that no stored activity matches. An activity that reaches Garmin after that sync (a watch that synced late) is not seen, so the agent should call `request_sync` and look again before it says the workout was missed. A workout that was approved but never reached the watch expires and sends nothing.

## Switches

| Setting | Default | Meaning |
|---|---|---|
| `COACHD_WEBHOOK_EVENTS` | `activity.stored` | Which events the webhook sends. An existing receiver sees no change until you list more. |
| `COACHD_NUDGES` | `false` | `true` also sends a fixed-text ntfy push for the three new events. Needs `COACHD_NTFY_URL`. |

```bash {filename=".env"}
COACHD_WEBHOOK_URL=https://automation.example/hooks/<secret>
COACHD_WEBHOOK_EVENTS=activity.stored,health.day_ready,workout.unmatched,week.closed
```

The webhook and nudges are independent: either, both or neither. The nudge texts are in [ntfy](/docs/install/notifications/#nudges).

coachd remembers only the events something receives. An event that is neither in `COACHD_WEBHOOK_EVENTS` nor nudged is not recorded, so when you turn it on later it starts with the current day or week: listing `week.closed` on a Monday morning still sends that Monday's event.

## Messages

An agent's `send_message` shows on the watch and on coachd's page. With ntfy configured it also sends a push that says only "Your agent left a message", with a link to the page. The text itself never leaves coachd. At most one such push goes out per 15 minutes.

## Recipe: event to agent run to message

The shape is the same for any agent platform:

1. **Receiver.** Run any automation that accepts the webhook POST (n8n, Home Assistant, a small script behind your reverse proxy). It reads `event` and `date` from the body.
2. **Agent run.** The automation starts a headless agent run with the coachd MCP server connected, and passes a prompt that names the event and the date. The exact command, and how to give the run an MCP server and a coachd token, depend on your platform: check its documentation.
3. **Read.** The agent reads coachd for everything else, because an event carries nothing but a date and IDs.
4. **Reply.** The agent calls `send_message` with a short note (at most 300 characters). That reaches your watch, your page and, through ntfy, your phone.

What the prompt should contain, per event:

| Event | Prompt says | The agent reads |
|---|---|---|
| `health.day_ready` | The event and date. Ask for a short morning note on today's plan, and to stay silent when nothing needs saying. | `get_daily_health` for the date and the days before, `get_summaries`, `get_notes`, `list_workouts` for today. |
| `workout.unmatched` | The event, the date and the proposal ID. Say that the activity may not have synced yet. | `request_sync`, then `list_activities` for the date and `get_activity` with detail `laps`; `list_workouts` for the proposal. Ask me what happened if there is still no activity. |
| `week.closed` | The event and the Monday that started the week. | `get_summaries` for that week and the weeks before, `list_workouts`, `get_notes`, `get_goal`. |
| `activity.stored` | The event, `activity_id` and `proposal_id`. | `get_activity` with detail `laps`; compare `workout.executed` with the plan. |

Tell the agent that it may only propose workouts, never approve them: you approve on coachd's page with your passkey. Keep the token you give the run to the write scope it needs, and treat the receiver URL as a secret.
