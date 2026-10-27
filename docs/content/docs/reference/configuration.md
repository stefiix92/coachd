---
title: Configuration
weight: 1
---

Settings live in `deploy/.env` (from [`.env.example`](https://github.com/stefiix92/coachd/blob/main/deploy/.env.example)); tokens live in [secret files](/docs/install/secrets/). Unset or empty means the default.

## coachd

| Setting | Default | Meaning |
|---|---|---|
| `COACHD_TIMEZONE` | required | Your timezone, for example `Europe/Bratislava`. Days, weeks and workout dates follow it. |
| `COACHD_GARMIN_KEY_EXPIRES` | required | Expiry date of the Garmin key in `secrets/garmin_api_key` (`YYYY-MM-DD`); coachd warns before it. A key [renewed on the page](/docs/install/garmin-key/#renew-the-key) applies if it expires later. |
| `COACHD_DEMO` | `false` | `true` runs on made-up Garmin data, with no Garmin key: [Try it with demo data](/docs/install/demo/). Leave the Garmin key settings unset with it. A demo data directory and a real one never mix: each mode refuses the other's. |
| `COACHD_IMAGE` | `coachd:local` | The image to run, for example `ghcr.io/stefiix92/coachd:<version>@sha256:<digest>`. Empty builds and runs `coachd:local`. See [Get the image](/docs/install/image/). |
| `COACHD_VERSION` | `dev` | The version string baked into the image by `docker compose build`. |

## Public lane

| Setting | Default | Meaning |
|---|---|---|
| `COACHD_PUBLIC_AUTH` | see meaning | `access` or `builtin`. Empty means `access` when all three `COACHD_ACCESS_*` are set; otherwise there is no page and no public `/mcp`: the public listener serves only the watch API if `COACHD_DEVICE_API=on`, else it is disabled. |
| `COACHD_PUBLIC_URL` | none | Public origin of the runner page: `https://<host>[:port]`, no path. Required in Access and built-in mode. It is the WebAuthn origin (passkeys are bound to this host), and in built-in mode every OAuth URL coachd emits is built from it. `http` is accepted only for `http://localhost[:port]`, for development. |
| `COACHD_ACCESS_TEAM_DOMAIN` | none | Access mode: `<team>.cloudflareaccess.com`. |
| `COACHD_ACCESS_AUD` | none | Access mode: the Access application's AUD tag. |
| `COACHD_ACCESS_ALLOWED_EMAIL` | none | Access mode: your email. Set all three `COACHD_ACCESS_*` together or leave all empty; leave them empty in built-in mode. |
| `COACHD_OAUTH_DCR` | off | Built-in mode: `on` allows [dynamic client registration](/docs/install/builtin-auth/#dynamic-client-registration) for apps that cannot identify themselves with a client metadata document URL. Claude and ChatGPT do not need it. |

## Optional features

| Setting | Default | Meaning |
|---|---|---|
| `COACHD_NTFY_URL` | off | Full ntfy topic URL, for example `https://ntfy.sh/<secret-topic>`. Needs the public lane and `COACHD_PUBLIC_URL`. See [ntfy](/docs/install/notifications/#ntfy). |
| `COACHD_WEBHOOK_URL` | off | Where to POST the [post-run webhook](/docs/install/notifications/#post-run-webhook). Treat it as a secret. |
| `COACHD_DEVICE_API` | off | `on` serves `/device/v1/*` on the public listener for the [watch app](/docs/watch/#server-setup). Anything other than `on`, `off` or empty is an error. |

## Tuning

| Setting | Default | Meaning |
|---|---|---|
| `COACHD_POLL_INTERVAL` | `1h` | Scheduled sync interval (Go duration, > 0). |
| `COACHD_BACKFILL_DAYS` | `730` | Days of history to backfill on first start (positive integer). |
| `COACHD_BACKFILL_DELAY` | `1s` | Pause between paced Garmin requests (Go duration, >= 0): backfill requests, FIT downloads, long-outage catch-up chunks and deletion-reconcile probes. |
| `COACHD_SYNC_MIN_INTERVAL` | `10m` | Minimum interval between agent `request_sync` calls (Go duration, > 0). |

## Grafana

| Setting | Meaning |
|---|---|
| `COMPOSE_FILE` | The single on/off switch for the overrides: `compose.yaml:compose.cloud.yaml` ([Grafana Cloud](/docs/operations/grafana-cloud/)), `compose.yaml:compose.local.yaml` ([local Grafana](/docs/operations/monitoring/#local-grafana)) or `compose.yaml:compose.cloud.yaml:compose.local.yaml`. When turning one off again, run `docker compose up -d --remove-orphans`. |
| `GRAFANA_CLOUD_OTLP_ENDPOINT`, `GRAFANA_CLOUD_OTLP_USER` | Grafana Cloud OTLP endpoint and user, for traces. Read only with the cloud override. |
| `GRAFANA_CLOUD_PYROSCOPE_URL`, `GRAFANA_CLOUD_PYROSCOPE_USER` | Grafana Cloud Pyroscope URL and user, for profiles. Read only with the cloud override. |
| `GRAFANA_PDC_CLUSTER`, `GRAFANA_PDC_HOSTED_GRAFANA_ID` | Private Data Source Connect cluster and hosted Grafana ID. Read only with the cloud override. |
| `GRAFANA_ROOT_URL` | The local Grafana's URL on the Tailscale node, for example `https://raspberrypi-coachd.<tailnet>.ts.net/grafana/`. Required with the local override. |

## Set by compose

`compose.yaml` also sets settings you don't change: the secret file paths (`COACHD_GARMIN_API_KEY_FILE`, `COACHD_NTFY_TOKEN_FILE`, and `COACHD_WEBHOOK_TOKEN_FILE` from a [compose override](/docs/install/notifications/#post-run-webhook)), the data directory (`COACHD_DATA_DIR`, `/data`), the listeners (`COACHD_PUBLIC_ADDR` `:8080`, `COACHD_PRIVATE_ADDR` `:8081`, `COACHD_OPS_ADDR` `:9090` for `/livez`, `/readyz` and pprof) and the telemetry endpoint (Alloy).
