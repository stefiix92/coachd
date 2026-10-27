---
title: Data and privacy
weight: 4
---

coachd keeps your data on your server, in the `coachd-data` volume (`/data` in the container), which belongs on an [encrypted disk](/docs/install/#encrypt-the-data-disk). How it is protected is in the [security model](/docs/reference/security/).

## What coachd stores

| What | Where | Kept |
|---|---|---|
| Garmin data: activities, sleep, HRV, resting HR, SpO2, weight, Body Battery, stress, and the raw JSON as Garmin sent it | `coachd.db` | indefinitely; an activity deleted in Garmin Connect disappears from coachd, but its raw JSON and FIT file stay |
| Original FIT files, and laps, series and executed steps read from them | `fit/`, `coachd.db` | indefinitely |
| Totals computed at ingest | `coachd.db` | recomputed as data arrives |
| The goal (earlier goals archived) and notes | `coachd.db` | until deleted; expired notes are not deleted |
| Agent messages for the watch | `coachd.db` | the newest 20 |
| Workout proposals: steps, the agent's rationale, warnings, status | `coachd.db` | indefinitely |
| Audit log: tool calls, approvals, authentication failures | `coachd.db` | 365 days |
| Credentials: token hashes, passkey public keys, sign-in sessions, paired watches, app connections | `coachd.db` | until revoked or expired; expired sessions, links and OAuth state are purged daily |
| Database snapshots | `backups/` | the newest 7 |
| The link-signing key, and a Garmin key renewed on the page | `nonce.key`, `garmin-key.json` | until replaced |

Logs and metrics stay on the host: VictoriaLogs and VictoriaMetrics keep 30 days (logs at most 5 GiB), and container logs rotate at 30 MB. They carry IDs, timings and outcomes only, never health data, chat or note text: see [telemetry](/docs/reference/security/#telemetry).

## What leaves the server

| To | What | When |
|---|---|---|
| Garmin | API requests with your key; Garmin returns your data | every sync, hourly by default |
| Your AI agent and its provider | whatever the agent reads through the [MCP tools](/docs/agents/tools/) | when it calls a tool; claude.ai and ChatGPT call from their own servers |
| Your watch, through Garmin Connect on your phone | approved workouts with their steps, notes and the agent's reasons, and agent messages | when the app syncs or checks |
| ntfy | "Workout for Tue 3 Nov ready to review" and a link; in built-in mode, notices about app connections. Never workout or health data. | with `COACHD_NTFY_URL` set |
| Your webhook | activity ID, date, sport and proposal ID | with `COACHD_WEBHOOK_URL` set |
| Cloudflare | public-lane traffic, decrypted at its edge | with a Cloudflare Tunnel or Access |
| Grafana Cloud | traces and profiles; it reads metrics and logs through PDC | with [Grafana Cloud](/docs/operations/grafana-cloud/) on |
| Your restic repository | database snapshots and FIT files | with [off-site backups](/docs/operations/backups/#off-site-with-restic) |

Besides these, coachd only fetches Cloudflare Access's signing keys (Access mode) and an app's client metadata document (built-in mode). It has no analytics and no update checks. What an agent's provider does with what it read is up to that provider's terms.

## Delete everything

From `deploy/`, with the same `COMPOSE_FILE` you run:

```bash
docker compose down -v
```

This stops the stack and deletes its volumes: coachd's data and snapshots, Tailscale's state, metrics, logs and, with local monitoring, Grafana's. Then:

1. Delete `deploy/secrets/`, `.env` and `restic.env`.
2. Delete the restic repository at your storage provider, if you made one.
3. Remove coachd from your AI apps, your tailnet and your watch.

Chats you had with an agent stay with its provider.
