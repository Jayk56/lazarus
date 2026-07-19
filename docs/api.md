# HTTP API

The API is served below `/api/v1`. It records maintenance work: create a run,
save the state you found, move the run through its phases, and record checks as
you go.

Every `/api/v1` request needs `Authorization: Bearer <token>`. The public
operational endpoints—`/healthz`, `/livez`, `/startupz`, `/readyz`, `/version`,
and `/metrics`—do not. Roles build on one another: `reader` can read; `operator`
can also create runs, save captures, and change run or target state; and `admin`
can also approve skips, reopen failed runs, and manage backups.

Every response includes `X-Request-ID` and `Cache-Control: no-store` for API
paths. Send your own request ID when you need to retry a write; the service
generates one when the header is missing or invalid. A request ID may be at
most 128 bytes and must not contain a newline.

## Use the API

1. `POST /api/v1/maintenance` creates a run in `new`.
2. `PUT /api/v1/maintenance/{id}/capture` saves the state discovered before
   maintenance. Save the same JSON document outside Lazarus so a failed
   workflow can send it again without running discovery again.
3. Move the run to `stopping`, update targets, then move through `stopped`,
   `waiting`, and `starting` as the work proceeds.
4. Record checks with the observations endpoint and move each target to its
   next state.
5. Move the run to `completed` after all targets pass the completion rules.

Use `GET /api/v1/maintenance` to find a run, or `GET /api/v1/events` to follow
the complete event history. The examples under `examples/` show this sequence
from Ansible Automation Platform.

## Versions, retries, and errors

The maintenance run has a `version`. It changes when the run changes,
when the capture creates targets, and whenever a target changes. A target has its
own `version`; changing the run also changes every target version. The service
returns each version as a quoted decimal `ETag`:

The `ETag` is a version token. It prevents an older job from overwriting a
change made by a newer job.

- `GET /api/v1/maintenance/{id}` returns the maintenance `ETag`.
- `GET /api/v1/maintenance/{id}/targets/{target}` returns the target `ETag`.
- A target write also returns `X-Maintenance-ETag` with the new maintenance
  version.

Send `If-Match` on every maintenance `PATCH`, target `PATCH`, and observation
`POST`. It may contain a tag such as `"2"`, a comma-separated list of tags, or
`*`. Weak tags that begin with `W/` do not match. Lazarus checks the version as
part of the write, so an outdated request cannot overwrite a newer change.

| Status | Meaning |
| --- | --- |
| `400` | Invalid JSON, query, or `If-Match` syntax. The JSON body has `error`; validation conflicts also include `message`. |
| `401` | The bearer token is missing or invalid. The response includes `WWW-Authenticate: Bearer realm="lazarus"`. |
| `403` | The token's role cannot perform the operation. |
| `404` | The path or resource does not exist. |
| `405` | The path exists but does not support that method; `Allow` lists supported methods. |
| `409` | The request conflicts with the current state, lock, capture, or request ID. The body includes diagnostic fields when available. |
| `412` | The supplied `If-Match` does not match the current version. |
| `428` | `If-Match` was required but not supplied. |
| `500` | An unexpected server error. |
| `503` | The service is unavailable or draining. |

After `500` or `503`, reuse the same request ID only for an operation documented
as safe to repeat. Otherwise, read the resource and confirm whether the change
is still needed before trying again. Capture and observation retries are
described below.

## Lifecycle

The run states are:

```text
new -> captured -> stopping -> stopped -> waiting -> starting -> completed
new -> cancelled
captured -> cancelled   (only before any non-skip target change)
new|captured|stopping|stopped|waiting|starting -> failed
failed -> new                                             (no capture)
failed -> captured|stopping|stopped|waiting|starting       (with capture)
```

Reopening a failed run requires an administrator and a non-empty
`justification`. Reopen leaves the saved capture, target states, and existing
events unchanged. It changes the run state, advances the run and target
versions, updates their timestamps, and appends a `maintenance.reopened` event.
Failed runs keep their lock keys. Completed runs and safe cancellations release
them.

`failed`, `cancelled`, and `completed` reject new target changes. Resending an
observation with the same request ID and body still succeeds when Lazarus has
already saved it, even if the run has since finished.

Target state is based on the saved `original_state`:

- `running`: `captured -> stopping -> stopped -> starting -> healthy`.
- `stopped`: starts in `stopped`; normally leave it there or have an
  administrator skip it. The completion rule requires `stopped` or `skipped`.
- `degraded` or `unknown`: starts in `captured` and must be skipped by an
  administrator before the run can stop or complete.
- `stopping` and `starting` may enter `failed`; a failed target can retry the
  phase that failed.
- `skipped` requires an administrator and a non-empty justification.

The run must be `stopping` for stop-phase target changes and `starting` for
start-phase changes. A run may enter `stopped` only when running and originally
stopped targets are `stopped` or `skipped`, and degraded or unknown targets are
`skipped`. Completion requires at least one captured target; running targets
must be `healthy` or `skipped`, originally stopped targets must be `stopped` or
`skipped`, and degraded or unknown targets must be `skipped`.

## Maintenance runs

### Create a run

`POST /api/v1/maintenance` (`operator`)

```json
{
  "maintenance_id": "maintenance-example-001",
  "change_ticket": "CHG-1234",
  "workflow_version": "example-workflow",
  "metadata": {"owner": "platform"}
}
```

`maintenance_id` is required. `change_ticket`, `workflow_version`, and
`metadata` are optional. `workflow_version` is a caller-supplied label for the
automation used in this run; it is not the API version. `metadata` must be a
JSON object. The response is
`201` with the new maintenance run and an empty `targets` array. The `ETag`
is the initial maintenance version. The identity
fields cannot be changed later.

### List runs

`GET /api/v1/maintenance` (`reader`)

Results are newest first. Supported exact filters are `state`,
`maintenance_id`, `change_ticket`, and `workflow_version`. `limit` is 1–200
(default 100). When `next_cursor` is present, return it unchanged as `cursor` to
fetch the next page; do not parse or construct cursor values. Unknown query
parameters and invalid limits return `400`. The response contains `items` and,
when more results exist, `next_cursor`.

### Read a run

`GET /api/v1/maintenance/{id}` (`reader`)

Returns the maintenance run and every captured target in one response. The
`ETag` is the current maintenance version.

### Change the run state

`PATCH /api/v1/maintenance/{id}` (`operator`)

Send the maintenance `ETag` in `If-Match`:

```json
{"state": "stopping", "detail": {"workflow_step": "stop-components"}}
```

`state` is required. `detail` is optional and must be a JSON object. The
response is `200` with the updated record and targets plus the new `ETag`.

Use the same endpoint to mark a run `failed`, `completed`, or `cancelled`, or
to reopen a failed run. Reopen requires `admin` and a top-level justification:

```json
{
  "state": "stopping",
  "justification": "CHG-1234 owner approved resuming the interrupted stop phase.",
  "detail": {"resume_job": "84217"}
}
```

An uncaptured failed run can reopen only to `new`. A captured failed run can
reopen to `captured`, `stopping`, `stopped`, `waiting`, or `starting`.

## Capture and targets

### Save the discovered state

`PUT /api/v1/maintenance/{id}/capture` (`operator`)

The body is the discovery document as one JSON object, up to 2 MiB:

```json
{
  "captured_by": "aap",
  "targets": [
    {
      "target_id": "was-a",
      "lock_key": "was-cell:prod:node-a",
      "kind": "websphere-jvm",
      "host": "node-a.example",
      "original_state": "running"
    },
    {
      "target_id": "dmgr",
      "lock_key": "was-cell:prod:dmgr",
      "original_state": "stopped"
    }
  ]
}
```

Each target needs a unique `target_id`, a non-empty stable `lock_key`, and an
`original_state` of `running`, `stopped`, `degraded`, or `unknown`. Targets in
one run may share a lock key; another active or failed run may not use a key
already in use. The saved capture, target identities, and lock keys cannot be
edited.

The first request returns `201`. Sending the same JSON again (ignoring object
key order and whitespace) returns `200` with the saved capture and does not
create new targets or events. Different JSON returns `409`. `GET
/api/v1/maintenance/{id}/capture` (`reader`) returns the saved capture, its
timestamp, payload, and SHA-256 hash.

Keep the discovery document in the workflow system or object storage before
calling this endpoint. If a workflow fails, send that exact document again; do
not run discovery after components have started or stopped.

### Read a target

`GET /api/v1/maintenance/{id}/targets/{target}` (`reader`)

Returns the target and its `ETag`. Every captured target is addressable,
including `degraded` and `unknown` targets.

### Change a target

`PATCH /api/v1/maintenance/{id}/targets/{target}` (`operator`)

Send the target `ETag` in `If-Match`:

```json
{"state": "stopped", "detail": {"check": "process-absent"}}
```

The response is `200` with the updated target, its new `ETag`, and
`X-Maintenance-ETag` for the new run version. Work on different targets can
run at the same time. The workflow must process targets that share a `lock_key`
one at a time.

An administrator can record a waiver with `skipped`:

```json
{
  "state": "skipped",
  "justification": "The owner accepted degraded discovery under CHG-1234."
}
```

## Record observations

`POST /api/v1/maintenance/{id}/targets/{target}/observations` (`operator`)

Send the current target `ETag` and a JSON object up to 64 KiB:

```json
{"source": "aap", "check": "websphere-health", "observed_state": "healthy"}
```

Success returns `204` with no body. Lazarus detects duplicates by
`maintenance_id`, `target_id`, and `X-Request-ID`. Reusing the same request ID
with the same JSON (ignoring key order and whitespace) succeeds without adding
another event, even if the target version or run state has changed. Reusing it
with different JSON returns `409`. Observations do not change target or
maintenance versions.

## Event history

`GET /api/v1/events` (`reader`)

Returns events newest first. Filters are exact and combine with AND:

`maintenance_id`, `target_id`, `event_type`, `resource_type`, `resource_id`,
`actor`, `role`, `request_id`, `cursor`, and `limit`.

`target_id` requires `maintenance_id`. `limit` is 1–200 (default 100). To fetch
the next page, return `next_cursor` unchanged as `cursor`; do not parse or create
cursor values. The response contains `items` and an optional `next_cursor`.
Each event includes its ID, type, time, request ID, actor, role,
resource identity, optional maintenance and target IDs, optional before/after
state and version fields, and a JSON `payload`. When `aggregate_version` is
present, it is the maintenance run version immediately after that event.

## Backups

| Endpoint | Role | Result |
| --- | --- | --- |
| `POST /api/v1/admin/backups` | `admin` | Creates and verifies a local SQLite backup and its JSON manifest. Returns `201` with `BackupManifest`. Reusing the same request ID as the same authenticated caller returns the existing backup while it is retained. |
| `GET /api/v1/admin/backups/{filename}` | `admin` | Validates the named backup, records the download event, and streams the database or manifest. |
| `HEAD /api/v1/admin/backups/{filename}` | `admin` | Performs the same validation and event recording as `GET`, but returns headers only. |

Database downloads include `Content-Disposition`, `Content-Length`, `ETag`,
and a base64 `Digest: sha-256=...`. Manifest downloads use
`Content-Type: application/json`. A missing or invalid backup file returns
`404` or a diagnostic error. Restore is an offline command; see
[backup and restore](backup-restore.md).

For the design rationale and operational limits, see
[architecture](architecture.md).
