# Lazarus operations runbook

Read the [operating constraints](architecture.md#operating-constraints) before changing storage, pod count, or rollout settings. This runbook covers the routine checks, recovery steps, and production change gate.

## Routine checks

1. Confirm one Lazarus pod is running and Ready.
2. Check `GET /readyz`, `GET /version`, and `GET /metrics`. `/version` reports only the service version; run `/lazarus verify --database ...` when you need full SQLite or application validation.
3. Review `GET /api/v1/events?limit=100` for unexpected state changes and administrator actions.
4. Check free space on the persistent volume. The main `.db` file and its `-wal` and `-shm` companion files must remain together.
5. Create a backup with `POST /api/v1/admin/backups`. Confirm all three checks are `ok`, confirm the SHA-256 checksum and `audit_recorded: true`, download the `.db` and matching manifest to storage outside the Lazarus volume, and regularly test a restore on a separate volume. If `audit_recorded` is false, the backup is valid but its creation is missing from event history; alert the operator.

## Graceful shutdown and rollout

When Kubernetes asks the pod to stop, Lazarus stops reporting Ready, cancels unfinished requests, and waits up to the shutdown deadline for them to finish. A backup published before cancellation remains valid; Lazarus removes an incomplete backup during the next backup request. The default request limits are 10 seconds for headers, 30 seconds for reading a request, two minutes for writing a response, and 120 seconds for an idle connection. Shutdown allows 30 seconds plus a five-second fallback inside the chart's 45-second pod grace period. The chart rejects settings that leave too little shutdown time. Keep the Route, proxy, and AAP timeouts longer than the measured backup time, but keep every timeout bounded. If the process does not stop, preserve the persistent volume and investigate before forcing deletion.

Before changing the image or chart, create and verify a backup. Keep the chart's rollout and storage-ownership settings described in the [operating constraints](architecture.md#operating-constraints). Do not run a separate database or backup job against the volume while Lazarus is running.

## Incident triage

| Symptom | First actions |
| --- | --- |
| `/readyz` returns `503` | Check logs, volume attachment, the database path, and `verify --database`. Do not delete the database. |
| Capture returns `409` | Read the diagnostic reason. For a content conflict, send the saved capture from persistent storage; for a lock conflict, resolve or reopen the maintenance holding that lock. Use a new ID only for a genuinely separate run before any component work. |
| State change returns `412` | Read the run or target again, use its new ETag, and confirm whether another job already made the change. |
| `SQLITE_BUSY` (database is busy) or request timeouts | Confirm only one Lazarus pod is using the volume, then check storage delay and free space. |
| Volume is full | Stop writes, preserve the volume, note the database and backup sizes, and follow the [backup and restore guide](backup-restore.md). |
| Node or volume will not attach | Follow the storage provider's recovery procedure. Keep Lazarus unavailable until the original volume is safely attached. |

## Recover a failed workflow

Once a run is `failed`, `cancelled`, or `completed`, Lazarus rejects new target changes and observations. Resending an observation already saved with the same request ID and body remains safe.

An administrator can reopen only a failed run using `PATCH /api/v1/maintenance/{id}`, the current maintenance ETag, and a non-empty justification. A failure before capture can reopen only to `new`. A captured failure can reopen to `captured`, `stopping`, `stopped`, `waiting`, or `starting`; choose the phase where work should resume. The saved capture cannot change, and the locks, target states, and existing events remain. Reopen adds a new event and advances the run and target versions.

After reopening the run, an administrator can record a target that cannot recover with `PATCH /api/v1/maintenance/{id}/targets/{target}` using state `skipped` and a justification.

AAP should configure `LAZARUS_CAPTURE_CACHE_DIR` on mounted storage that survives AAP jobs. The capture playbook saves the exact discovery document before submission and also stores it as AAP job output. If the cache is unavailable during a retry, retrieve that output and send the same document again; never run discovery against a partly stopped environment.

Do not relaunch the complete AAP workflow after a failure: external stop or start jobs may run again, and discovery would observe a partly changed environment. Reopen the same maintenance with an administrator justification, then resume at the appropriate phase without running create or discovery again. Component jobs must be safe to run more than once. Process targets with the same `lock_key` one at a time; targets with different lock keys may run at the same time. The admin backup playbook exports each checked pair to approved storage outside the Lazarus volume.

## Deployment checklist

Complete this checklist before the production change window:

1. Record the owner, accepted data-loss window, expected recovery time, and maintenance window in the change ticket.
2. Confirm the selected storage meets the [operating constraints](architecture.md#operating-constraints). Record the StorageClass and persistent-volume reclaim policy; `Retain` is preferred.
3. Call `/readyz` and `/version`; save the reported service version. Run offline `verify` separately when recording SQLite and application validation.
4. Check the configured backup count, `backupMinAge`, and free volume space. Confirm the newest local pair's SHA-256 checksum, three validation results, and `audit_recorded: true`, then confirm an off-volume export exists.
5. Confirm the Route, Lazarus, and AAP request timeouts exceed the measured backup time. Confirm the shutdown timeout plus the five-second fallback is shorter than the pod termination grace period. Do not start a rollout during a backup.
6. Restore the newest backup to a separate volume, run `verify --database`, start a test instance, and perform an API read. Keep the source volume untouched.
7. Review event history for failed or cancelled runs, and request logs for `412`/`If-Match` conflicts. Rejected version checks do not add write events. Confirm AAP credentials grant only the needed role.
8. Confirm the rollback owner and escalation path. Stop the deployment if storage or restore behavior is uncertain.

## Volume sizing and backup cleanup

Use measured peak values, not the empty-database size. A conservative starting formula is:

```text
volume bytes >= (number of backups + 2) * largest database size
               + largest observed WAL size
               + 20% free-space margin
```

The two extra copies cover the live database and one backup or restore in progress. With seven 1 GiB backups and a 512 MiB peak WAL file, the formula gives about 11.4 GiB. Request at least 12 GiB and leave more room as event history grows. The production example requests 20 GiB.

Before creating a backup, Lazarus removes its own unfinished temporary files and incomplete backup pairs. A complete backup consists of a `.db` file and its `.manifest.json` verification file. Retention removes pairs beyond the configured count once they reach the minimum age; it never deletes event history. Investigate repeated leftovers or storage errors, and never clean these files manually while a backup or restore is running.
