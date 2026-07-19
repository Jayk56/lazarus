# Ansible/AAP examples

These playbooks call Lazarus with `ansible.builtin.uri`. Set the API root,
operator token, maintenance ID, and a capture cache that survives later jobs:

```sh
export LAZARUS_URL=https://lazarus.example/api/v1
export LAZARUS_TOKEN='read-from-a-secret-manager'
export MAINTENANCE_ID='maintenance-example-001'
export LAZARUS_CAPTURE_CACHE_DIR='/approved/lazarus-captures'
```

AAP should inject tokens with protected credentials, never surveys or committed
variables. Administrator actions use `LAZARUS_ADMIN_TOKEN`; backup export also
needs `LAZARUS_BACKUP_EXPORT_DIR`. Add a private CA to the execution environment
or pass `lazarus_ca_path`. TLS verification stays enabled.

## Normal flow

`maintenance_state.yml` sets the maintenance state, including completion and
failure. `target_state.yml` sets a target state or performs an administrator skip.
Both playbooks read the current ETag (the API's version check), do nothing when
the requested state is already set, retry only `500`/`503`, and accept a `412`
only when a re-read proves that the requested state is already present. A
maintenance phase change also changes target ETags, so run `target_state.yml`
after entering its required phase.

```sh
ansible-playbook create.yml -e change_ticket=CHG-1234 -e workflow_version=example-workflow
ansible-playbook capture.yml
ansible-playbook maintenance_state.yml -e maintenance_state=stopping
ansible-playbook target_state.yml -e target_id=web-01 -e target_state=stopping
# Run the external component-stop job. It must be safe to run more than once.
ansible-playbook observe.yml -e target_id=web-01 \
  -e '{"observation":{"source":"aap","check":"component-stop","observed_state":"stopped"}}'
ansible-playbook target_state.yml -e target_id=web-01 -e target_state=stopped
ansible-playbook maintenance_state.yml -e maintenance_state=stopped
ansible-playbook maintenance_state.yml -e maintenance_state=waiting
ansible-playbook maintenance_state.yml -e maintenance_state=starting
ansible-playbook target_state.yml -e target_id=web-01 -e target_state=starting
# Run the external component-start/health job. It must be safe to run more than once.
ansible-playbook observe.yml -e target_id=web-01 \
  -e '{"observation":{"source":"aap","check":"component-health","observed_state":"healthy"}}'
ansible-playbook target_state.yml -e target_id=web-01 -e target_state=healthy
ansible-playbook maintenance_state.yml -e maintenance_state=completed
```

Targets captured as `stopped` stay stopped and need no start step. If discovery
finds a `degraded` or `unknown` target, an administrator must skip it explicitly:

```sh
LAZARUS_ADMIN_TOKEN='read-from-a-secret-manager' \
ansible-playbook target_state.yml \
  -e target_id=REPLACE_WITH_DEGRADED_OR_UNKNOWN_TARGET -e target_state=skipped \
  -e justification='CHG-1234 owner accepted the unavailable health signal'
```

Targets with different `lock_key` values may run at the same time. Process
targets that share a lock key one at a time, even when their target IDs differ.

## Capture and failure recovery

Replace the static `capture_document` in `capture.yml` with the output of your
discovery job. The playbook saves that document before its API call, always uses
the saved file, and never overwrites it. The same document is also saved in the
AAP job output. Every target needs `target_id`, `lock_key`, and `original_state`.
If a workflow fails, never run discovery again and never create a replacement
maintenance.

Record failure on the existing run, then use an administrator token and
justification to reopen it at the phase where work should resume. Target
changes remain limited to the matching maintenance phase:

```sh
ansible-playbook maintenance_state.yml -e maintenance_state=failed
LAZARUS_ADMIN_TOKEN='read-from-a-secret-manager' \
ansible-playbook maintenance_state.yml -e maintenance_state=stopping \
  -e justification='CHG-1234 resume after fixing the component stop job'
```

Resume the appropriate phase and external job. Relaunching the complete workflow
is unsafe because it can repeat component work or discovery. External stop/start
jobs must be safe to run more than once.

`observe.yml` sends the observation object with the current target ETag. Its
request ID is `aap-observe-` plus a SHA-256 value built from `awx_job_id`,
maintenance ID, target ID, target ETag, and check name. Retries use the same ID;
another job or phase records separate evidence.

`backup.yml` calls `POST /admin/backups`, downloads the database and manifest to
storage outside the Lazarus volume, and fails if event history is missing
`backup.created`.

See the [API lifecycle](../../docs/api.md#lifecycle) for state requirements and reopen rules.
