---
title: Compatibility
weight: 5
---

The AI clients, watches and phones coachd works with, and how well each is tested.

## AI clients

A client needs MCP over streamable HTTP, spec revision 2026-07-28 or 2025-11-25. Setup for each lane is in [Connect an agent](/docs/agents/).

| Client | Lane | Status |
|---|---|---|
| claude.ai custom connector | public | Access mode: works, the maintainer's daily client. Built-in mode: untested, please report. |
| Claude Code | public or private | Untested, please report. |
| ChatGPT | public | Untested, please report. |
| Cursor, Open WebUI, Gemini, local models | private, or public if the client follows the MCP authorization spec | Untested, please report. |

In [built-in mode](/docs/install/builtin-auth/), claude.ai, Claude Code and ChatGPT identify themselves with a client metadata document, so they don't need dynamic client registration. Wider live testing of clients against built-in mode is still to do.

Tried a client? Tell us how it went with the [client report form](https://github.com/stefiix92/coachd/issues/new?template=client.yml), working or not.

## Watches

The [watch app](/docs/watch/) runs on fēnix and Forerunner models with Connect IQ 3.0 or later, app workouts and FIT downloads, which its sync needs. Background checks and the glance differ by model:

| Models | New message or workout | Glance |
|---|---|---|
| fēnix 7, 7S, 7X, 7 Pro, 7S Pro, 7X Pro | notification | yes |
| fēnix 8, 8 Pro, 8 Solar | notification | yes |
| fēnix 9, 9 Pro, 9 Pro Solar | notification | yes |
| fēnix E | notification | yes |
| fēnix 5, 5S, 5X, 5 Plus, 5S Plus, 5X Plus | "open coachd?" prompt | no |
| fēnix 6, 6S, 6 Pro, 6S Pro, 6X Pro | "open coachd?" prompt | no |
| fēnix Chronos | "open coachd?" prompt | no |
| Forerunner 55, 245, 245 Music, 645, 645 Music, 745, 935, 945, 945 LTE | "open coachd?" prompt | no |

The exact list is `<iq:products>` in [`watch/manifest.xml`](https://github.com/stefiix92/coachd/blob/main/watch/manifest.xml). The maintainer's watch is a fēnix 7S Pro. Newer Forerunner models are not in the list yet; if yours is missing, [open an issue](https://github.com/stefiix92/coachd/issues/new/choose).

Without the app, any watch that takes a FIT workout copied to `GARMIN/NewFiles` can use the [FIT download](/docs/workouts/).

## Phones

The watch app reaches coachd through Garmin Connect on your phone. It is tested with Android phones; iPhone is not yet verified.

The approval page needs a browser with passkeys (WebAuthn).
