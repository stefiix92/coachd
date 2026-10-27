# Security policy

coachd handles health data and controls what reaches a runner's watch, so security reports are very welcome.

## Reporting a vulnerability

Please report privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Don't open a public issue, discussion or pull request for a vulnerability.

Include what you can:
- the affected version or commit;
- the mode it applies to: Access mode, built-in auth, the private lane or the watch API;
- steps to reproduce, and the impact you expect.

Never include real tokens, passkeys, Garmin keys or health data. Redact them, or use a throwaway instance.

This is a one-person project, so responses are best effort: expect an acknowledgement within a week. Fixes land on `main` first. Please give a reasonable window before disclosing publicly.

## Supported versions

The latest release and `main`. Fixes ship in the next release; older releases get no backports while coachd is 0.x.

## In scope

- Anything that lets an agent credential approve, deliver or withdraw a workout, or reach the runner's page.
- Authentication or authorization bypasses on either lane, the OAuth endpoints or the watch API.
- SSRF, open redirects, CSRF, XSS or session issues.
- Leaks of tokens, keys, health data or personal data into logs, metrics, traces or responses.

## Out of scope

- Garmin, Cloudflare, Tailscale, ntfy and the AI clients themselves. Report those to their vendors.
- Problems that need root on the host, or a misconfiguration the install guide warns against.
