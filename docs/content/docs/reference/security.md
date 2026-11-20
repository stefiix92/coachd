---
title: Security model
weight: 3
---

coachd handles health data and controls what reaches a runner's watch. To report a vulnerability, follow the [security policy](https://github.com/stefiix92/coachd/blob/main/SECURITY.md): privately, never in a public issue.

## Invariants

- **No agent path to the watch.** No MCP tool or agent credential can approve, deliver, withdraw or alter an approved workout. Approval needs your passkey on coachd's page, with a single-use, HMAC-signed nonce bound to the proposal and a hash of its content (for a week: to every proposal in the set, all or nothing). coachd enforces this itself, never relying on the AI app.
- **The Garmin key stays server-side**, as does the [intervals.icu](/docs/install/intervals/) key. Neither reaches agents, logs, telemetry or the page.
- **Untrusted text stays labelled.** Garmin activity names and descriptions, [intervals.icu](/docs/install/intervals/) activity names, agent rationale and OAuth client names are shown as untrusted.
- **One runner per instance.** No multi-user or hosted features.

## Access

- **Public lane:** either [Cloudflare Access](/docs/install/cloudflare-access/) (coachd validates the Access JWT on every request) or [built-in auth](/docs/install/builtin-auth/) (passkey sessions for the page, coachd's own OAuth 2.1 server with PKCE, rotating refresh tokens and reuse detection for agents). In both, every write on the page needs your passkey. In built-in mode only login, enrolment, OAuth metadata, authorize, token, revoke, register and `/device/v1` are reachable without a session or token, and each checks its own credential. Agent tokens work only on `/mcp`.
- **Private lane:** [Tailscale](/docs/install/tailscale/) (no Funnel) with hashed, scoped static tokens.
- **Watch:** a device token per paired watch, revocable at once.
- **No open ports to the network.** The only published port is coachd's public listener on loopback (`127.0.0.1:8080`), for a tunnel or reverse proxy on the host. The private listener (8081) and the ops listener (9090, unauthenticated pprof) are never published.

## Data

- Secrets live in Docker secret files, each owned by the one container that reads it.
- Data sits on an [encrypted disk](/docs/install/#encrypt-the-data-disk).
- `nonce.key`, the HMAC key for approval and login links, lives in `/data` and is not backed up: losing it only invalidates links in flight (and, in built-in mode, makes DCR apps register again).
- An **audit log** records every tool call, approval and authentication failure.

## Telemetry

Logs, metrics, traces and profiles carry IDs, timings and outcomes only: never health data, chat or note content, or tokens. The redaction is an allowlist of declared keys, enforced by tests.

## Containers and networks

- Release images are signed with Sigstore (keyless, from the release workflow) and carry an SBOM and build provenance. Every image in the compose files is pinned by digest.
- Every service drops all capabilities and runs with `no-new-privileges` and a read-only root filesystem. coachd is distroless and non-root.
- Each service with internet access has its own egress network, and the internal networks join only services that talk to each other: Grafana and the PDC agent reach the metric and log stores, never coachd, Alloy or Tailscale.
- Container logs rotate at 30 MB per service; VictoriaLogs is capped at 30 days and 5 GiB, so a log flood can't fill the disk.

## Known risks

- **Garmin Personal API:** launched 2026-10-21; rate limits are undocumented and may change. The key is personal and for your own use only (Supplemental Terms §1); hosting coachd for other people would need Garmin's Developer Program.
- **No official self-serve way to push workouts to the watch.** Hence the Connect IQ app, with FIT files as the fallback.
- **The spike warning** rests on a single cohort study (Frandsen 2025). It warns and never blocks.
- **The PDC agent's token** is visible in its process arguments on the host: see [Grafana Cloud](/docs/operations/grafana-cloud/#the-pdc-agent).
