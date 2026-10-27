---
title: Compared with alternatives
linkTitle: Comparison
weight: 9
---

Other ways to plan training around a Garmin watch, by category, and the choices coachd makes.

| | Who plans | How it reads your Garmin data | How workouts reach the watch |
|---|---|---|---|
| **Garmin Coach, daily suggested workouts** | Garmin's plans and suggestions, adjusted to your data | built in | built in |
| **Training platforms** such as intervals.icu or TrainingPeaks | you or your coach, in the platform's calendar | a Garmin Connect link you authorise | the platform sends them to Garmin Connect |
| **Community Garmin MCP servers** | your AI agent | many log in with your Garmin email and password through an unofficial library | some upload them to Garmin Connect |
| **coachd** | you and your AI agent | the official Personal API, with your own key | the coachd watch app, a FIT file or a Zwift file, after you approve |

Several community MCP servers build on [python-garminconnect](https://github.com/cyberjunky/python-garminconnect), which describes itself as an unofficial client for Garmin's web services.

## coachd's choices

- **The official API.** coachd reads your data with a Personal API key you create, and never asks for your Garmin password. The key stays on your server.
- **You approve every workout.** An agent can only propose. Nothing reaches the watch until you approve it with your passkey on coachd's page, and coachd enforces that itself.
- **Any MCP agent.** Claude, ChatGPT, or a local model: coachd has no chat UI and no LLM of its own.
- **Self-hosted.** One instance per runner, on your server.
- **No coaching built in.** coachd gives numbers and warnings, never verdicts. The plan comes from you and your agent.

## What it asks of you

- A Docker host, a hostname, and some setup: see [install](/docs/install/).
- A Garmin Personal API key, renewed when it expires.
- Patience with an early project: Garmin's Personal API is new and its rate limits are undocumented.
