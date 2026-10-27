---
title: FAQ
weight: 8
---

## Does coachd coach me?

No. It is a gateway: it serves your Garmin data, keeps the goal and notes, and delivers workouts you approved. You and your AI agent decide the training. coachd's numbers are never verdicts, and its warnings never block.

## Which AI clients work?

Any MCP client that speaks streamable HTTP, spec revision 2026-07-28 or 2025-11-25. The maintainer uses claude.ai daily; setup for Claude Code and ChatGPT is documented. See [compatibility](/docs/reference/compatibility/#ai-clients).

## Why the official Personal API key, and not my Garmin password?

coachd reads your data through the official [Garmin Connect Personal API](https://developer.garmin.com/gc-developer-program/personal/) with a key you create, and never asks for your Garmin login. The key stays on the server: not to agents, logs, telemetry or the page. It is personal, so use it only for your own data (Garmin's Supplemental Terms §1). See [how coachd compares](/docs/comparison/).

## Why does approval need a passkey?

So that nothing an agent holds can put a workout on your watch. In Access mode an agent signed in through Managed OAuth carries the same Access identity as you, so a login alone can't tell you apart. A passkey can: approval needs a fresh passkey check on coachd's page, bound to that exact workout and its content.

## Can the agent change a workout after I approve it?

No. A proposal never changes once stored. The agent proposes a replacement, which you approve again; approving it withdraws the old one, and the watch app removes it.

## Why a watch app instead of Garmin Connect's calendar?

There is no official self-serve way to push workouts to the watch, so the Connect IQ [watch app](/docs/watch/) fetches approved workouts from coachd. You don't need it: an approved workout also downloads as a FIT file to copy to the watch, and many cycling workouts as a [Zwift workout](/docs/workouts/cycling/).

## Can it run without Docker?

coachd is a single static Go binary, but the documented install is the Docker image with `deploy/compose.yaml`, which also sets up the secrets, the private lane, monitoring and container hardening. Running it otherwise is not documented.

## Can I host it for friends?

No. One instance serves one runner, and the Garmin key may only read its owner's data. Each runner runs their own coachd with their own key.

## Does it work with an iPhone?

The watch app is tested with Android phones; iPhone is not yet verified. The approval page needs a browser with passkeys.

## Where does my data go?

See [data and privacy](/docs/reference/privacy/).

## Something doesn't work

See [troubleshooting](/docs/operations/troubleshooting/).
