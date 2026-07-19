# Lazarus

Lazarus is a small Go and SQLite service for Ansible Automation Platform (AAP). It saves what was running before maintenance, tracks each target through the work, and keeps a permanent history of accepted changes.

## Start here

1. Create a maintenance run with `POST /api/v1/maintenance`.
2. Save the discovery document outside Lazarus, then send it with `PUT /api/v1/maintenance/{id}/capture`.
3. Move the maintenance run into the next phase.
4. Move each target into that phase, run its external job, record an observation, and then save the resulting target state.
5. Advance the run when every target meets the phase requirements. Complete it after every target is restored or has an approved skip.

The [HTTP API guide](docs/api.md) contains request examples, lifecycle rules, phase requirements, retry behavior, and event filters. The [AAP example](examples/aap/README.md) shows how to connect these calls to a workflow. The [Ansible examples](examples/ansible/README.md) show the same flow from the command line.

## API overview

The authenticated API is served under `/api/v1`:

| Operation | Method and path |
| --- | --- |
| Create or list maintenance runs | `POST` or `GET /api/v1/maintenance` |
| Read or change a maintenance run | `GET` or `PATCH /api/v1/maintenance/{id}` |
| Read or save a discovery document | `GET` or `PUT /api/v1/maintenance/{id}/capture` |
| Read or change a target | `GET` or `PATCH /api/v1/maintenance/{id}/targets/{target}` |
| Record a target observation | `POST /api/v1/maintenance/{id}/targets/{target}/observations` |
| Read event history | `GET /api/v1/events` |
| Create or download a backup | `POST /api/v1/admin/backups`, `GET` or `HEAD /api/v1/admin/backups/{filename}` |

Every protected request uses a bearer token. Before changing a maintenance run or target, read it and copy its `ETag` response header into the write request's `If-Match` header. If another job made a change first, read the resource again and decide whether your change is still needed. Reuse the same `X-Request-ID` when retrying the same logical request.

The [API lifecycle](docs/api.md#lifecycle) defines the states, completion requirements, retries, administrator skips, and failed-run recovery. The [AAP guide](examples/aap/README.md) explains how to save discovery outside Lazarus, resume the same maintenance after a failure, and order work with `lock_key`.

## Get a release image

Each versioned release publishes a Linux image for AMD64 and ARM64 to
`ghcr.io/jayk56/lazarus`. Deploy by digest so the selected image cannot change:

```sh
podman pull ghcr.io/jayk56/lazarus@sha256:REPLACE_WITH_RELEASE_DIGEST
```

The same GitHub Release includes a multi-platform OCI archive for disconnected
environments, an SPDX software bill of materials (SBOM), build provenance, the
image digest, a vulnerability report, project and third-party license files,
and SHA-256 checksums. See
[Container distribution](docs/distribution.md) for download, verification,
internal-registry import, and release instructions.

## Run locally

```sh
umask 077
export LAZARUS_DB_PATH="$PWD/lazarus.db"
export LAZARUS_TOKEN_FILE="$PWD/tokens"
printf '%s\n' '{"role":"admin","token":"local-development-token","name":"local"}' > "$LAZARUS_TOKEN_FILE"
go run ./cmd/lazarus
```

Each nonblank token-file line is one JSON object with a `token`, the exact role `reader`, `operator`, or `admin`, and a nonempty caller `name` for logs and event history. Keep the file private. No other token formats are accepted.

| Variable | Default | Purpose |
| --- | --- | --- |
| `LAZARUS_ADDR` | `:8080` | Listen address. |
| `LAZARUS_DB_PATH` | `/var/lib/lazarus/lazarus.db` | SQLite database path. |
| `LAZARUS_TOKEN_FILE` | `/etc/lazarus/tokens` | Bearer-token file. |
| `LAZARUS_BACKUP_DIR` | sibling `backups` directory | Local backup directory. |
| `LAZARUS_BACKUP_KEEP` | `7` | Minimum retained backup-pair count. |
| `LAZARUS_BACKUP_MIN_AGE` | `24h` | Minimum age before a backup pair can be removed. |
| `LAZARUS_WRITE_TIMEOUT` | `2m` | HTTP write timeout. |
| `LAZARUS_SHUTDOWN_TIMEOUT` | `30s` | Time allowed for active requests to finish during shutdown. |
| `LAZARUS_TLS_CERT_FILE` / `LAZARUS_TLS_KEY_FILE` | unset | Set both to enable native TLS. |

`/healthz`, `/livez`, `/startupz`, `/readyz`, `/version`, and `/metrics` are public operational endpoints. Use the `verify` command to check a stopped database or downloaded backup. See [architecture and operating constraints](docs/architecture.md) for the integrity checks, storage model, and backup limits.

For AAP, set `LAZARUS_CAPTURE_CACHE_DIR` to mounted storage that survives AAP jobs. The capture playbook writes the exact discovery document there before calling Lazarus and also saves it as AAP workflow output. After a workflow failure, reopen the same maintenance and resume the appropriate phase with the saved capture and current ETags.

## Build and test

```sh
go test -count=1 ./...
go vet ./...
```

## Deploy and recover

Read the [architecture and operating constraints](docs/architecture.md), [OpenShift guide](docs/openshift.md), and [backup and restore guide](docs/backup-restore.md) before deployment or recovery.

Additional references:

- [Operations runbook](docs/operations.md)
- [Security](docs/security.md)
- [Container distribution](docs/distribution.md)
- [AAP configuration](examples/aap/README.md)
- [Ansible examples](examples/ansible/README.md)
- [AKS test procedure](docs/aks-test.md)
- [Validation checklist](docs/validation.md)
- [Infrastructure test record](docs/validation-record.md)

## License

Lazarus is licensed under the [Apache License 2.0](LICENSE).
