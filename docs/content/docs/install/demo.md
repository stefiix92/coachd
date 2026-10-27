---
title: Try it with demo data
weight: 9
---

Run coachd on made-up Garmin data and connect an agent, without a Garmin key. It needs only Docker.

{{% steps %}}

### Start coachd

```bash
docker run -d --name coachd-demo \
  -e COACHD_DEMO=true -e COACHD_TIMEZONE=Europe/Bratislava \
  -p 127.0.0.1:8081:8081 -v coachd-demo:/data \
  ghcr.io/stefiix92/coachd:<version>
```

Take `<version>` from the [releases](https://github.com/stefiix92/coachd/releases): demo mode needs 0.5.1 or later. Set your own timezone. Port 8081 is the private lane, published on this machine only.

coachd fills in 26 weeks of a made-up runner: runs with a weekly long run, strength, indoor rides, sleep, HRV and weight. The last two weeks arrive at once, the rest within a few minutes.

### Create a token

```bash
docker exec coachd-demo /coachd token create --name demo --scope write
```

It prints the token once.

### Connect an agent

For Claude Code:

```bash
claude mcp add --transport http coachd-demo http://127.0.0.1:8081/mcp \
  --header "Authorization: Bearer <token>"
```

Other clients take the same URL and header. Ask how last week went, set a goal, propose a workout. A proposed workout waits for your approval, which needs a passkey on the page.

{{% /steps %}}

## Approve workouts

Browsers accept passkeys on `http://localhost`, so [built-in auth](/docs/install/builtin-auth/) works locally. Replace the container (the volume keeps the data) with one that also serves the page:

```bash
docker rm -f coachd-demo
docker run -d --name coachd-demo \
  -e COACHD_DEMO=true -e COACHD_TIMEZONE=Europe/Bratislava \
  -e COACHD_PUBLIC_AUTH=builtin -e COACHD_PUBLIC_URL=http://localhost:8080 \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 -v coachd-demo:/data \
  ghcr.io/stefiix92/coachd:<version>
docker exec coachd-demo /coachd enroll
```

Open the link it prints on this machine to enrol a passkey. Proposed workouts then wait at `http://localhost:8080`. coachd logs that plain http is for development only.

## Demo data stays apart

coachd marks the data directory as demo. In demo mode it refuses a data directory that a real coachd created, and a real coachd refuses a demo one, so made-up data never mixes with yours. Backups keep the mark: a demo backup restores only into a demo or empty data directory. Install for real on a new volume.

To remove the demo:

```bash
docker rm -f coachd-demo && docker volume rm coachd-demo
```
