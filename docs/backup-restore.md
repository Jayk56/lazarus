# Backup and restore

Use the admin backup API to create a verified recovery point, export it away from the Lazarus volume, and restore it only while Lazarus is stopped.

## Create a backup

An `admin` caller creates a backup with `POST /api/v1/admin/backups`. Lazarus writes a separate SQLite database file in the configured backup directory and checks it before reporting success:

- `integrity_check` confirms the database file can be read and its internal structure is sound;
- `foreign_key_check` confirms referenced records exist;
- `application_check` confirms the saved maintenance and target states agree;
- a SHA-256 checksum and byte count are written to a matching `.manifest.json` file;
- `audit_recorded` reports whether event history includes `backup.created`. If it is false, the files can still be valid. Export the pair, alert the operator that its creation event is missing, and do not create repeated backups automatically.

Lazarus retains at least the configured number of complete backup pairs and removes eligible older pairs by filename. `backupMinAge` protects a complete pair until it reaches the configured age, so a burst can temporarily leave more than the configured count. Backups first live on the same persistent volume as the main database. As described in the [operating constraints](architecture.md#operating-constraints), local copies do not protect against loss of that volume or cluster.

Lazarus runs the full checks twice before making the `.db` and `.manifest.json` files available. Set the Route, proxy, Lazarus `config.httpWriteTimeout`, and AAP request timeouts above the longest measured backup time. The chart defaults are two minutes for responses, 30 seconds for shutdown, and a 45-second pod grace period. Keep the response timeout bounded. Lazarus ignores and cleans up incomplete files during the next backup; a complete pair remains valid. See [architecture](architecture.md#operating-constraints) for how backup work is isolated from API traffic.

Use one stable `X-Request-ID` when retrying the same backup request. While the pair is retained, the same authenticated caller receives the existing checked manifest instead of creating another backup. After retention removes that pair, the request creates a new backup. Use a new request ID when you intentionally want a new recovery point.

## Export the pair

After the create request succeeds, an administrator can download either file with `GET /api/v1/admin/backups/{filename}`. `HEAD` performs the same checks and records the same access, but returns headers without the body. Supply only the file name, never a path. Lazarus accepts a published `lazarus-*.db` or its matching `.manifest.json`, checks the pair again, records `backup.downloaded` in event history, and then sends the file. Authentication is required.

Use the AAP backup playbook or another approved client to download both files to approved storage outside the Lazarus volume, then move the pair to approved object or archive storage. Copy only a completed `.db` and matching `.manifest.json` pair. Never copy the live database by itself, and never include token or TLS secret files. If export fails while Lazarus and its volume remain available, retry the download. The local pair cannot help if the service, volume, availability zone, or cluster is lost. If `audit_recorded` is false, alert the operator that event history is missing `backup.created`, even though the backup checks passed.

## Verify a backup

Run inside the image or a controlled recovery pod:

```sh
/lazarus verify --database /var/lib/lazarus/backups/lazarus-....db
```

The command reads an existing backup without changing it and reports the SQLite version and three check results. It rejects a missing or empty file and never creates a database. Startup performs a shorter check; this command checks the complete database, relationships, saved discovery documents, lifecycle rules, and event history. It accepts only a Lazarus database.

## Restore a backup

Restore must be performed while Lazarus is stopped. Scale Lazarus to zero and confirm no pod or job mounts the source or destination volume. Restore to a new volume when possible:

```sh
/lazarus restore \
  --database /restore/lazarus.db \
  --backup /restore-source/lazarus-....db \
  --manifest /restore-source/lazarus-....db.manifest.json
```

The command compares the filename, byte count, and SHA-256 checksum with the `.manifest.json` file, then runs all three database checks. It does not overwrite an existing destination unless `--replace` is present. With `--replace`, it first moves the existing database and any companion files to a timestamped rollback set. The validated backup is put in place only after those steps succeed.

The source must be a completed backup file. Restore rejects a source that has SQLite `-wal`, `-shm`, or `-journal` companion files so a live database cannot be used by mistake.

After restore:

1. Run `verify` against the new path.
2. Start one Lazarus instance and wait for `/readyz`.
3. Read a known maintenance run, then perform an approved test write.
4. Keep the source volume and rollback copy until the owner signs off.

Storage snapshots are an additional recovery option, not a replacement for the built-in backup or off-volume export. Stop Lazarus before taking one, restore it to a new volume, and run the same validation and API checks. A snapshot or recovery pod must not mount the live volume while Lazarus is running; see the [operating constraints](architecture.md#operating-constraints).
