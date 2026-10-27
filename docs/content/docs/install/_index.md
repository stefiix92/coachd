---
title: Install
weight: 2
---

From an empty Docker host to a working coachd: Garmin data in, an agent connected, and your passkey enrolled. Just looking? [Try it with demo data](demo/) first, without a Garmin key.

`deploy/compose.yaml` runs coachd with Tailscale (the private lane), Grafana Alloy, VictoriaMetrics and VictoriaLogs. coachd publishes one port, its public listener on `127.0.0.1:8080`: a Cloudflare Tunnel (`cloudflared` on the host) or a reverse proxy on the host forwards your hostname to it. Run every command in these pages from `deploy/`.

{{% steps %}}

### Get the stack files

```bash
git clone https://github.com/stefiix92/coachd.git
cd coachd/deploy
cp .env.example .env
```

Set `COACHD_TIMEZONE` in `.env`. Tokens never go in `.env`: they are [secret files](secrets/). Every setting is in the [configuration reference](/docs/reference/configuration/).

### Encrypt the data disk

The `coachd-data` volume holds health data, raw Garmin JSON and FIT files, and token hashes. It **must be on an encrypted disk**: put Docker's data root (or just this volume) on LUKS. coachd does not manage disk encryption.

### Get the image

The signed release image, or your own build: [Get the image](image/).

### Create the secrets

The Garmin key, the Tailscale auth key and the ntfy token: [Secrets](secrets/).

### Add your Garmin key

[Garmin key](garmin-key/).

### Open the public lane

Pick one:

- [Cloudflare Access](cloudflare-access/): Cloudflare does the login and MFA, and signs in the AI apps.
- [Built-in auth](builtin-auth/): coachd does it with your passkey and its own OAuth 2.1 server. No Cloudflare account needed.

### Open the private lane

[Tailscale](tailscale/), for agents on your own devices.

### Enrol your passkey

[Enrol a passkey](passkey/). It approves every workout.

### Connect an agent

[Claude, ChatGPT or a local client](/docs/agents/).

{{% /steps %}}

## Optional extras

{{< cards >}}
  {{< card link="notifications" title="ntfy and the post-run webhook" icon="bell" subtitle="A phone notice when a workout is ready; an event for your automation when a run arrives." >}}
  {{< card link="../watch" title="Watch app" icon="clock" subtitle="Approved workouts copied onto your watch." >}}
  {{< card link="../operations/monitoring" title="Monitoring" icon="chart-square-bar" subtitle="A local Grafana with dashboards and alerts, or Grafana Cloud." >}}
  {{< card link="../operations/backups" title="Off-site backups" icon="cloud-upload" subtitle="restic to S3, B2 or SFTP." >}}
{{< /cards >}}
