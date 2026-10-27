---
title: Backups and restore
weight: 1
---

## Automatic backups

coachd snapshots its database at start and then daily, keeping the newest 7 in `/data/backups` (`coachd-<UTC timestamp>.db` plus a `.json` manifest with the SHA-256 of the database and every FIT file). The `BackupStale` [alert](../monitoring/#alerts) fires when the newest is over 48 h old. For a snapshot now:

```bash
docker compose exec coachd /coachd backup
```

Not in a snapshot: `deploy/secrets/` and `.env` (keep them in a password manager), a Garmin key renewed on the page, and `nonce.key` (recreated on start; links in flight stop working).

{{< callout type="warning" >}}
If the clock was wrong when a backup was written, so its name is more than 1 h in the future, coachd backs up at every hourly check and pushes the older dailies out of the 7. Fix the clock and delete that backup.
{{< /callout >}}

## Restore

```bash
docker compose stop coachd
docker compose run --rm --no-deps coachd restore /data/backups/coachd-<ts>.db
docker compose start coachd
```

`restore` refuses, before touching anything, while coachd runs, and for a corrupt or foreign snapshot, one from a newer schema, one that doesn't match its manifest, or a [demo](/docs/install/demo/) snapshot over real data (or the reverse). Don't run other coachd commands during a restore. The previous database moves to `/data/pre-restore-<ts>/`; delete it once you're satisfied.

**Kept across a restore:** every revocation (agent tokens, passkeys, watches) and every used approval link, so nothing revoked works again and no approval link works twice. Sign-in sessions and, in built-in mode, app connections are cleared: sign in and reconnect your apps.

**Rolled back to the snapshot:** proposals and their states (a workout approved later is pending again), notes, the goal, and tokens, passkeys and watches created later (create or re-pair them).

If the previous database can't be read, the restore warns and goes ahead: check `token list` and `device list`, and revoke again anything revoked after the snapshot.

### Undo a restore

The coachd image has no shell, so use a throwaway container to move the previous files back:

```bash
docker compose stop coachd
T=tailscale/tailscale:v1.104.2@sha256:b11583ad2e93e4671128538c88cf11f460444a5d7e449311dd1933a400ab849d
docker run --rm --network none -v coachd_coachd-data:/data --entrypoint sh "$T" -c \
  'cd /data && rm -f coachd.db coachd.db-wal coachd.db-shm && mv pre-restore-<ts>/coachd.db* .'
docker compose start coachd
```

## Off-site with restic

`restic/restic.sh` runs a pinned restic image over the volume, read-only, and copies `/data/backups` and `/data/fit`.

1. Create `restic.env` in `deploy/` (mode `0600`): `RESTIC_REPOSITORY=...` plus your provider's credentials (S3, B2, SFTP, ...).
2. Store the repository password: `put_secret restic_password 0:0` ([helper](/docs/install/secrets/)). **Losing it loses the backups**: keep a copy in your password manager.
3. Initialise and run it:

   ```bash
   ./restic/restic.sh init
   ./restic/restic.sh backup      # also prunes: 7 daily, 4 weekly, 12 monthly
   ./restic/restic.sh snapshots
   ```

4. Schedule it, from `deploy/`, for example in root's crontab:

   ```text
   30 3 * * *  cd /path/to/coachd/deploy && ./restic/restic.sh backup >> /var/log/coachd-restic.log 2>&1
   30 4 * * 0  cd /path/to/coachd/deploy && ./restic/restic.sh check  >> /var/log/coachd-restic.log 2>&1
   ```

## Disaster recovery

On a new or wiped host. Don't start coachd until the last step: on an empty volume it would write a backup of an empty database, newer than every real one.

{{% steps %}}

### Create the volume

With the stack files, `secrets/` and `.env` in place:

```bash
docker compose run --rm --no-deps coachd version
```

This creates the `coachd_coachd-data` volume without starting coachd.

### Fetch and copy the latest restic snapshot

```bash
./restic/restic.sh restore-latest /srv/coachd-restore     # an existing directory
docker run --rm --network none -v coachd_coachd-data:/data \
  -v /srv/coachd-restore/data:/restore:ro --entrypoint sh "$T" -c \
  'cp -a /restore/fit /restore/backups /data/ && chown -R 65532:65532 /data/fit /data/backups'
```

`T` is the throwaway image from [Undo a restore](#undo-a-restore).

### Restore the newest snapshot and start

List `/data/backups` (`docker run --rm -v coachd_coachd-data:/data:ro --entrypoint ls "$T" -l /data/backups`), delete any snapshot written after the disaster, then:

```bash
docker compose run --rm --no-deps coachd restore /data/backups/coachd-<ts>.db
docker compose up -d
```

{{% /steps %}}
