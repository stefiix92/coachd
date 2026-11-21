---
title: ntfy and webhook
weight: 8
---

## ntfy

coachd sends an [ntfy](https://ntfy.sh) notice when a workout is ready to review ("Workout for Tue 28 Oct ready to review"), with a **Review** action that opens it on the page. The notice carries only the date, never workout or health data. In built-in mode it also tells you when an app connects, or is revoked for reusing a token.

1. Create a topic with default access `deny-all` and an access token for coachd, and subscribe your phone.
2. Set the topic URL and store the token:

   ```bash {filename=".env"}
   COACHD_NTFY_URL=https://ntfy.sh/<secret-topic>
   ```

   ```bash
   put_secret ntfy_token 65532:65532     # the helper from Secrets
   docker compose up -d --force-recreate coachd
   ```

ntfy needs the public lane and `COACHD_PUBLIC_URL`. coachd reads the token only at start, so recreate it after changing the token.

### Nudges

With `COACHD_NUDGES=true` (default `false`, needs `COACHD_NTFY_URL`) coachd also sends one fixed-text notice for each of three moments, with a link to the page:

| Moment | Text |
|---|---|
| Last night's sleep is stored | Last night's sleep data is in |
| Yesterday's delivered workout has no matching activity | Yesterday's workout has no matching activity |
| A week has closed (Monday) | Last week's numbers are complete |

They carry no number, name or note. The events behind them are described in [Proactive events](/docs/agents/proactive/).

### Agent messages

When your agent calls `send_message`, ntfy gets "Your agent left a message" with a link to the page, where the message is listed. The message text is never sent to ntfy. At most one such push goes out per 15 minutes.

## Post-run webhook

With `COACHD_WEBHOOK_URL` set, coachd POSTs an event to your own automation when a recent activity (started in the last 48 hours) is stored, with its FIT file and per-step numbers in. It carries IDs, a date and a sport only:

```json
{"event":"activity.stored","activity_id":24745625144,"date":"2026-10-26","sport":"running","proposal_id":"cc7b5c9c..."}
```

`proposal_id` is the delivered workout the activity carried out, or `null`. Point it at n8n, Home Assistant or anything that can start your agent: the agent then calls `get_activity` with detail `laps` to review the run, and can `send_message` to the watch.

- Retries 5xx and network errors twice (after 5 s and 25 s); gives up on other 4xx.
- It doesn't make coachd poll Garmin more often: the activity arrives on the next hourly sync, or sooner when the agent calls `request_sync`.
- Treat the URL as a secret (an unguessable path). Plain `http://` works for a LAN receiver, with a warning in the log.

The same webhook can also send `health.day_ready`, `workout.unmatched` and `week.closed`: list them in `COACHD_WEBHOOK_EVENTS` (default `activity.stored`, so nothing changes until you opt in). See [Proactive events](/docs/agents/proactive/).

For a bearer token as well, store it with `put_secret webhook_token 65532:65532` and mount it with a compose override:

```yaml {filename="compose.override.yaml"}
services:
  coachd:
    environment:
      COACHD_WEBHOOK_TOKEN_FILE: /run/secrets/webhook_token
    secrets: [webhook_token]
secrets:
  webhook_token:
    file: ./secrets/webhook_token
```
