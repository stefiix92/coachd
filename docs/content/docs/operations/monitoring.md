---
title: Monitoring
weight: 2
---

coachd sends OpenTelemetry to Grafana Alloy, which keeps metrics in VictoriaMetrics and logs in VictoriaLogs (30 days, at most 5 GiB), on the host. Traces and profiles go to [Grafana Cloud](../grafana-cloud/) if you turn it on, and are dropped otherwise.

Telemetry carries IDs, timings and outcomes only: never health data, chat or note text, or tokens.

To look at it, run the **local Grafana** below, reached over Tailscale, or [Grafana Cloud](../grafana-cloud/).

## Local Grafana

`compose.local.yaml` adds a Grafana with coachd's dashboard, the six [alert rules](#alerts) and both data sources provisioned from [`deploy/grafana/`](https://github.com/stefiix92/coachd/tree/main/deploy/grafana). It publishes no port: it is served on the Tailscale node at `https://raspberrypi-coachd.<tailnet>.ts.net/grafana/`, under the same ACL grant as the MCP lane.

{{% steps %}}

### Create its secrets

```bash
put_secret grafana_admin_password 472:472    # the helper from Secrets
put_secret grafana_alert_url 472:472         # Enter for empty: no alert notifications
```

The alert URL is your ntfy topic URL with `?template=grafana` appended, so ntfy renders Grafana's alerts readably. For a protected topic, add ntfy's `auth` query parameter: the base64 of `Bearer <token>`, without `=` padding.

{{< callout type="warning" >}}
The alert URL must be one `https://` URL, without quotes, spaces, backslashes, pipes or `$`, or Grafana refuses to start. (Grafana expands `$VAR` in provisioned values, so a `$` could leak its environment into the URL.)
{{< /callout >}}

### Turn it on

```bash {filename=".env"}
GRAFANA_ROOT_URL=https://raspberrypi-coachd.<tailnet>.ts.net/grafana/
COMPOSE_FILE=compose.yaml:compose.local.yaml     # or compose.yaml:compose.cloud.yaml:compose.local.yaml
```

Then `docker compose up -d`. This also recreates tailscale to serve `/grafana/`.

### Sign in

As `admin`, with the password you stored.

{{% /steps %}}

- **The admin password** is read from its file only when Grafana creates its database. Later, change it in Grafana.
- **A new alert URL** takes effect after `docker compose up -d --force-recreate grafana`. The notification policy is file-managed: changes made in the UI don't survive a restart.
- **To reset Grafana** if its database gets out of step with the files: `docker compose rm -sf grafana && docker volume rm coachd_grafana-data && docker compose up -d grafana`.
- **To turn it off**, drop `compose.local.yaml` from `COMPOSE_FILE` and run `docker compose up -d --remove-orphans`.

The first start downloads the pinned VictoriaLogs plugin from grafana.com, so it takes a while. Otherwise Grafana makes no outbound calls of its own (update checks, usage reporting and the plugin catalog are off) apart from alert notifications.

## Alerts

| Alert | Fires when |
|---|---|
| `GarminSyncFailing` (critical) | a Garmin data type hasn't synced for 3 h, or coachd stopped reporting ([intervals.icu](/docs/install/intervals/) is left out) |
| `GarminKeyExpiring` | the Garmin key expires within 7 days |
| `AuthFailureSpike` | more than 20 authentication failures in 10 min on the public or private lane |
| `WorkoutDeliveryFailed` | a workout the watch fetched wasn't confirmed within 15 min |
| `BackupStale` | no backup in 48 h, or none at all |
| `DataDiskFull` (critical) | the data disk is over 85% full |

The rules are in [`deploy/grafana/alerts/coachd-alerts.yaml`](https://github.com/stefiix92/coachd/blob/main/deploy/grafana/alerts/coachd-alerts.yaml), each with its PromQL. A dead coachd or Alloy fires `GarminSyncFailing`, `BackupStale` and `DataDiskFull`, since their series disappear.
