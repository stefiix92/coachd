---
title: Design records
weight: 6
---

coachd's behaviour is decided in design records, kept in the repository under [`docs/design/`](https://github.com/stefiix92/coachd/tree/main/docs/design). Read the relevant one before changing behaviour. In them, **[owner]** marks an owner decision, **[call]** the designer's call and **[builder]** a choice left to the implementer.

| Record | Covers |
|---|---|
| [coachd design](https://github.com/stefiix92/coachd/blob/main/docs/design/coachd-design.md) | The approved overall design: data in, context, proposals and approval, delivery, lanes, telemetry. |
| [Built-in auth](https://github.com/stefiix92/coachd/blob/main/docs/design/builtin-auth.md) | coachd's own OAuth 2.1 server and passkey page gate, as an alternative to Cloudflare Access. |
| [Watch store](https://github.com/stefiix92/coachd/blob/main/docs/design/watch-store.md) | Distributing the watch app through the Connect IQ Store. |
| [Watch v2](https://github.com/stefiix92/coachd/blob/main/docs/design/watch-v2.md) | The watch app's screens, agent messages and background notifications. |
| [Workout outcome](https://github.com/stefiix92/coachd/blob/main/docs/design/workout-outcome.md) | Planned vs actual: executed workout steps and their link to the delivered proposal. |
| [Approve week](https://github.com/stefiix92/coachd/blob/main/docs/design/approve-week.md) | Approving every waiting workout with one passkey. |
| [Garmin key renewal](https://github.com/stefiix92/coachd/blob/main/docs/design/garmin-key-renewal.md) | Renewing the Garmin key on the page. |
| [Expiring notes](https://github.com/stefiix92/coachd/blob/main/docs/design/expiring-notes.md) | Notes with an end date. |
| [Post-run webhook](https://github.com/stefiix92/coachd/blob/main/docs/design/post-run-webhook.md) | The event coachd sends when a run arrives. |
| [Perceived effort](https://github.com/stefiix92/coachd/blob/main/docs/design/perceived-effort.md) | The runner's post-activity rating. |
| [Zwift workouts](https://github.com/stefiix92/coachd/blob/main/docs/design/zwift-workouts.md) | Power targets in % of FTP and `.zwo` downloads. |
| [Trainer workouts](https://github.com/stefiix92/coachd/blob/main/docs/design/trainer-workouts.md) | Watt targets and indoor cycling workouts. |
