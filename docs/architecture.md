# Architecture

Lazarus is one Go service backed by SQLite. AAP calls a small HTTP API. A state change updates current state and adds its event in one database transaction. Observations and backup downloads add events without changing state. A backup manifest reports whether its creation event was saved.

## Developer model

### Data model

The service uses four application tables:

| Table | Purpose |
| --- | --- |
| `maintenance` | Run identity, lifecycle state, and the version of the full run. |
| `maintenance_captures` | One discovery document per run. It cannot be changed after it is saved. |
| `targets` | Current state for every captured target, its version, and its stable lock identity. |
| `journal_events` | Permanent records for creation, capture, state changes, observations, and administrator actions. |

The API reads current state from the first three tables. `GET /api/v1/events` reads the permanent history from `journal_events`, with filters and a cursor for fetching the next page.

Every captured target gets a `target_id` that is unique inside its maintenance run. Its immutable `lock_key` identifies the real operational resource across runs. A run may contain more than one target with the same lock key, but another active or failed run cannot capture that key until the first run completes or is safely cancelled.

### Lifecycle

The maintenance lifecycle is:

```text
new --capture--> captured -> stopping -> stopped -> waiting -> starting -> completed
 \-> cancelled
new or an active phase -> failed
```

A captured run can be cancelled until any target is moved to a normal state; skips do not count. Failed runs keep their capture, target states, and locks. An administrator can reopen a failed run with a justification and choose the phase where automation should resume. Ordinary target changes are still limited to the matching maintenance phase; administrator skips are allowed in any active phase. Completed and safely cancelled runs release their locks.

Target behavior depends on its immutable `original_state`:

- `running` starts as `captured` and follows `captured -> stopping -> stopped -> starting -> healthy`;
- `stopped` starts and remains `stopped`;
- `degraded` and `unknown` start as `captured` and require an administrator skip before the run can stop or complete;
- a target in `stopping` or `starting` can enter `failed`, and a failed target can retry through the same phase;
- an administrator can move any unfinished target in an active or reopened run to `skipped` with a justification.

Target changes are checked against the maintenance phase. Moving a target to `stopping`, `stopped`, or a stop-phase failure requires the run to be `stopping`. Moving it to `starting`, `healthy`, or a start-phase failure requires the run to be `starting`. A target captured as originally stopped already begins in `stopped` and needs no stop change. An administrator can skip a target in any active phase before the run finishes.

Before a run can enter `stopped`, running and originally stopped targets must be `stopped` or `skipped`, while degraded and unknown targets must be `skipped`. Before a run can complete, it must contain at least one target; running targets must be `healthy` or `skipped`, stopped targets must be `stopped` or `skipped`, and degraded or unknown targets must be `skipped`. The [API guide](api.md#lifecycle) defines these rules.

## Architecture decisions

### State and concurrency

`maintenance.version` changes whenever the capture, maintenance state, or any target state changes. Each target also has its own version. `PATCH` requests and observations require the `ETag` returned when that maintenance run or target was read.

A maintenance phase change advances every target version. Automation must read a target after entering the required phase and use that new target ETag. An outdated stop or start request then fails with `412 Precondition Failed` instead of applying to the wrong phase.

Lazarus processes one write at a time through one SQLite writer connection. It validates the request, changes the current state, adds the event, and commits all three steps together. SQLite rules prevent events from being changed or deleted. The `verify` command rebuilds the expected state from the event history, checks the saved discovery document, and confirms that two runs still holding locks—including failed runs—do not claim the same lock.

### Operating constraints

On startup, Lazarus runs SQLite `quick_check(1)`, a fast integrity check, and verifies the required database objects before it listens. The check still grows with database size, so test it against the deployment startup budget. Lazarus never deletes or recreates a damaged database. The `verify` command checks all database pages, relationships, saved-discovery hashes, event history, lifecycle rules, completion requirements, and lock conflicts.

The service uses three database connections:

- a primary connection for normal reads and one write at a time;
- an independent read-only connection for readiness checks;
- a separate connection for creating a consistent SQLite backup.

This keeps readiness and ordinary API reads available while a backup runs. A backup is published as a verified `.db` and manifest pair and can be downloaded through the administrator API. Backups first live on the same persistent volume as the database, so export every recovery pair to approved off-volume storage.

Only one Lazarus server instance may use the database at a time:

- run one pod with a `Recreate` rollout;
- use reliable block storage and `ReadWriteOncePod` when supported;
- keep the database, `-wal`, and `-shm` files together;
- never mount the live volume into a second writer or external backup process;
- let the platform assign the container user and writable volume group.

This design accepts downtime during rollout, node recovery, and volume attachment. Local backups do not survive loss of the volume or cluster. Export them off-volume and test restoration on a separate volume. If continuous availability or multiple writers are required, use a replicated database service.

### Security

Lazarus reads one JSON bearer-token record per line from a mounted file. Roles build on one another: `operator` includes reader access, and `admin` includes both. Only an administrator can reopen failed maintenance, skip targets, or create and export backups. Token values are never logged or written to the event history. Store token files and TLS keys as protected secrets. See the [security guide](security.md).
