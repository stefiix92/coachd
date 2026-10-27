---
title: Enrol a passkey
weight: 7
---

Your passkey approves workouts and, in built-in mode, signs you in. Once the public lane works:

```bash
docker compose exec coachd /coachd enroll
```

It prints a link valid for 10 minutes.

- **Access mode:** first sign in at `<COACHD_PUBLIC_URL>/` through Access, then open the link in that same browser. The link's token is in the URL fragment, which Access's login redirect drops, so a link opened while signed out arrives without it.
- **Built-in mode:** just open the link; enrolling signs you in.

Passkeys are bound to `COACHD_PUBLIC_URL`. Enrol another with `coachd enroll` on the host; revoke one on the page.
