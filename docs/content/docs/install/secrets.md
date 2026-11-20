---
title: Secrets
weight: 2
---

Secrets are files in `deploy/secrets/` (git-ignored), mounted at `/run/secrets/<name>`. Each container runs as its own user and reads only its own files, and coachd refuses a key file that group or others can read.

## The helper

Every secret file is written the same way: no trailing newline, then owned by the container's user with mode `0400`. Define this once in your shell, in `deploy/`:

```bash
put_secret() {  # put_secret <file> <owner:group>
  local v
  printf '%s (hidden; Enter for empty): ' "$1" >&2
  IFS= read -rs v; echo >&2
  rm -f "secrets/$1"
  (umask 077; printf '%s' "$v" > "secrets/$1")
  docker run --rm --network none -v "$PWD/secrets:/s" --entrypoint sh \
    tailscale/tailscale:v1.104.2@sha256:b11583ad2e93e4671128538c88cf11f460444a5d7e449311dd1933a400ab849d \
    -c "chown $2 /s/$1 && chmod 0400 /s/$1"
}
```

It reads the value at a hidden prompt (so it stays out of shell history) and hands the file to its owner with a throwaway container: no sudo, no network. The image is the Tailscale one compose already uses. Run it again to replace a secret.

## The files

| File | Owner | Needed | Set up in |
|---|---|---|---|
| `garmin_api_key` | `65532:65532` | always | [Garmin key](../garmin-key/) |
| `ntfy_token` | `65532:65532` | always; may be empty | [ntfy](../notifications/#ntfy) |
| `tailscale_authkey` | `0:0` | always | [Tailscale](../tailscale/) |
| `intervals_api_key` | `65532:65532` | with intervals.icu | [intervals.icu](../intervals/) |
| `grafana_cloud_otlp_token`, `grafana_cloud_pyroscope_token` | `473:473` | with Grafana Cloud | [Grafana Cloud](/docs/operations/grafana-cloud/) |
| `grafana_cloud_pdc_token` | `30000:30000` | with Grafana Cloud | [Grafana Cloud](/docs/operations/grafana-cloud/) |
| `grafana_admin_password` | `472:472` | with local monitoring | [Local Grafana](/docs/operations/monitoring/#local-grafana) |
| `grafana_alert_url` | `472:472` | with local monitoring; may be empty | [Local Grafana](/docs/operations/monitoring/#local-grafana) |
| `restic_password` | `0:0` | with off-site backups | [restic](/docs/operations/backups/#off-site-with-restic) |

For example:

```bash
put_secret garmin_api_key 65532:65532
put_secret ntfy_token 65532:65532        # Enter for empty if you don't use ntfy
```

Compose mounts every file its services use, so `docker compose up` fails with "bind source path does not exist" until each one exists, even for a feature you don't use. Never put tokens in `.env`.
