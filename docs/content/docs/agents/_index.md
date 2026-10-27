---
title: Connect an agent
weight: 3
---

Agents talk to coachd over MCP at `/mcp` (spec revisions 2026-07-28 and 2025-11-25).

| Lane | URL | Sign-in |
|---|---|---|
| Public | `https://coachd.<domain>/mcp` | OAuth: Cloudflare Access, or coachd's own in built-in mode |
| Private | `https://raspberrypi-coachd.<tailnet>.ts.net/mcp` | a static token |

## Public lane

{{< tabs >}}
{{< tab name="claude.ai" >}}
**Settings → Connectors → Add custom connector**, with `https://coachd.<domain>/mcp`.
{{< /tab >}}
{{< tab name="Claude Code" >}}
```bash
claude mcp add --transport http coachd https://coachd.<domain>/mcp
```

Then `/mcp` in a session to sign in.
{{< /tab >}}
{{< tab name="ChatGPT" >}}
Add `https://coachd.<domain>/mcp` in ChatGPT's connector settings.
{{< /tab >}}
{{< /tabs >}}

The app then sends you to sign in:

- **Access mode:** the Access login; Managed OAuth issues the app's token. The app's redirect URI must be [allowed on the Access application](/docs/install/cloudflare-access/#turn-on-managed-oauth).
- **Built-in mode:** coachd's sign-in, then its consent page. Check the app's web address (for Claude, `claude.ai`), keep or clear the write permission, and approve with your passkey.

Any other client that follows the MCP authorization spec works the same way; one that can't identify itself with a client metadata document needs [dynamic client registration](/docs/install/builtin-auth/#dynamic-client-registration).

## Private lane

Over [Tailscale](/docs/install/tailscale/), with a token from the host:

```bash
docker compose exec coachd /coachd token create --name claude-code --scope read --ttl 90d
```

It prints the token once. For Claude Code:

```bash
claude mcp add --transport http coachd https://raspberrypi-coachd.<tailnet>.ts.net/mcp \
  --header "Authorization: Bearer <token>"
```

Other clients take the same URL and header. Manage tokens with [`token list` and `token revoke`](/docs/reference/cli/#token).

## Read or write

| Scope | Allows |
|---|---|
| read | every read tool: Garmin data, summaries, goal, notes, workouts |
| write | read, plus coachd's local writes: `request_sync`, the goal and notes, `propose_workout`, `cancel_proposal`, `send_message` |

In built-in mode the consent page sets the scope; in Access mode an app gets both. No scope can approve or deliver a workout. See [MCP tools](tools/).

## Next

See an [example week](/docs/agents/example/) of a runner and an agent, and give your agent the [starter instructions](/docs/agents/instructions/).
