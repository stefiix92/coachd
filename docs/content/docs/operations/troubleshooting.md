---
title: Troubleshooting
weight: 5
---

Symptoms, causes and fixes. coachd's log is `docker compose logs coachd`; it carries IDs, outcomes and error classes, never health data. For the watch app's own messages, see [pairing](/docs/watch/pairing/) and [status messages](/docs/watch/status-messages/).

## Garmin sync

`get_status` shows each data type's `last_outcome` and `age_hours`, and the key's `days_left`. `request_sync` answers with an `outcome` and `next_allowed_at`.

| Symptom | Cause | Fix |
|---|---|---|
| `garmin_rate_limited`; log `garmin rate limited` | Garmin answered 429. coachd pauses every Garmin call until its `Retry-After` (at most 24 h). | Wait: `next_allowed_at` says until when. |
| `request_sync` says `rate_limited` | The agent asked again within `COACHD_SYNC_MIN_INTERVAL` (10 min). | Wait until `next_allowed_at`. |
| `request_sync` says `already_running` | A sync is already running. | Ask again shortly. |
| `auth_failed` | Garmin refused the key: expired or revoked. `garmin_key.days_left` is negative once expired. | [Renew the key](/docs/install/garmin-key/#renew-the-key) on the page. |
| A `stale_data` warning on a proposal | No successful sync for over 24 h. | Check `get_status` and the log; the `GarminSyncFailing` [alert](/docs/operations/monitoring/#alerts) fires after 3 h. |
| `get_activity` returns no laps or series | `has_fit` is false: the FIT file isn't stored yet. | It downloads after the activity syncs; ask again later. |
| "Garmin refused this key; nothing changed" on **Garmin key** | The new key is wrong or not active yet. | Copy it again from Garmin's page. coachd keeps the key it had. |

## Starting coachd

coachd checks its settings at start and exits with a message naming the setting.

| Message | Fix |
|---|---|
| `bind source path does not exist` (from compose) | A secret file is missing. Create every file in [the table](/docs/install/secrets/#the-files), empty where allowed. |
| `secret file … has mode …; group/other access must be removed` | Write it again with [`put_secret`](/docs/install/secrets/#the-helper). |
| `COACHD_PUBLIC_AUTH=builtin: leave COACHD_ACCESS_* empty` | Empty the three `COACHD_ACCESS_*` settings. |
| `COACHD_PUBLIC_URL: builtin mode needs https://, or http://localhost for development` | Use your `https://` hostname; an IP address doesn't work with passkeys. |
| `COACHD_NTFY_URL requires the public lane` | Set `COACHD_PUBLIC_URL` and a public lane mode, or leave ntfy off. |

`docker compose ps` shows coachd unhealthy when its `/livez` probe fails: read the log.

## Connecting an agent

| Symptom | Cause | Fix |
|---|---|---|
| claude.ai: "Couldn't register with … sign-in service" | Access mode: the app's redirect URI isn't allowed. | Add it under [Managed OAuth](/docs/install/cloudflare-access/#turn-on-managed-oauth). |
| claude.ai or ChatGPT can't connect, but your browser can | Bot Fight Mode or a WAF challenge blocks their servers. | See [Expose coachd](/docs/install/builtin-auth/#expose-coachd). |
| "coachd could not identify the app that asked to connect" | Built-in mode: the app doesn't identify itself with a client metadata document coachd can fetch, and DCR is off. | Turn on [DCR](/docs/install/builtin-auth/#dynamic-client-registration) for that app, or use the private lane. |
| "The app did not give a return address registered for it" | The app's redirect URI doesn't match the one it registered. | Start the connection again from the app. |
| "No passkey is enrolled, so nothing can approve this connection" | No passkey yet. | [Enrol one](/docs/install/passkey/), then connect again. |
| "This connection request has expired" | The consent page was open too long. | Start again from the app. |
| An app stops working; ntfy says coachd revoked a connection | Built-in mode: a token was reused, so coachd revoked the connection. | Reconnect the app; check **Connected apps**. |
| Private lane: 401 | The token expired (`--ttl`, 90 days by default) or was revoked. | `token list`, then create a new one. |
| A write tool fails with "insufficient scope" | The token or app has read scope only. | Create a `--scope write` token, or reconnect and keep write on the consent page. |

## Passkeys and approval

| Symptom | Cause | Fix |
|---|---|---|
| "enrolment link invalid or expired" | The link is older than 10 minutes or was used. In Access mode, a link opened while signed out loses its token. | Run `coachd enroll` again; in Access mode sign in first, then open the link in that browser. |
| Signing in or approving on a new device | That device has no passkey for coachd. | Run `coachd enroll` and open the link on the new device. Revoke old ones on **Passkeys**. |
| "this approval expired; reload the page" | An approval is valid for 24 h at most, and never past the workout's date. | Reload and approve. |
| "this proposal is cancelled", or another status | It changed after the page loaded: the agent cancelled it, or it was decided in another tab. | Reload: you approve exactly what you see. |
| "this workout's date has passed" | A `pending` proposal expires at the end of its date. | Ask the agent for a new one. |
| No ntfy notice | `COACHD_NTFY_URL` unset, or a new token not picked up (it is read only at start). Log: `notification failed`. | See [ntfy](/docs/install/notifications/#ntfy); `docker compose up -d --force-recreate coachd`. |

## The watch

| Symptom | Cause | Fix |
|---|---|---|
| An approved workout isn't on the watch | The app downloads only when you open it. Background checks notify, never download. | Open the app, or **Sync now**. It fetches workouts from today to 14 days ahead. |
| Status `delivery_failed` | The watch fetched it but didn't confirm within 15 minutes. | The next sync retries. |
| The same workout twice | A FIT file copied over USB and the app's own copy. | Delete the USB copy: see [one delivery path](/docs/workouts/#one-delivery-path-per-workout). |
| A prompt instead of a notification, no glance | Notifications and the glance need fēnix 7 or later. | See [compatibility](/docs/reference/compatibility/#watches). |

For an alert, see what it means under [Alerts](/docs/operations/monitoring/#alerts).

Still stuck? [Open an issue](https://github.com/stefiix92/coachd/issues/new/choose) with your version, your public lane mode and the log lines, with tokens and health data removed.
