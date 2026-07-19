# Validation checklist

Do not treat a source tree, image, or cluster as validated until you record the exact source revision and image digest with the results below.

## Source gate

Run against the source revision you will build:

```sh
make validate-source
```

This runs formatting, tests, the race detector, vet, all Helm profiles, the
negative digest-schema check, YAML parsing, OpenAPI validation, shell and GitHub
Actions checks, and syntax checks for every executable playbook in
`examples/ansible`. Install the pinned Python dependencies from
`scripts/validation-requirements.txt` first. Run `make aap-syntax-check`
separately in the approved AAP execution environment after installing
`examples/aap/collections/requirements.yml` through Red Hat Automation Hub.

The database/API suite must cover:

- the database identity, event history, and `verify` command;
- create/list/read, sending the same capture again, a record for every captured target, and lock conflicts;
- maintenance and target ETags, including target invalidation on a phase change;
- target changes allowed only in the correct phase, requirements for stopping and completion, cancellation, failure, justified reopen, and justified skip;
- direct observation bodies, safe retries with one request ID, rejection after a run finishes, and success when the same observation was already saved;
- cursor and filter behavior for `GET /api/v1/events`;
- one JSON token record per line, role enforcement, TLS minimums, backup retention, export, verify, and restore;
- backup and readiness work remaining independent of the sole writer connection.

## Release gate

For a published image, follow the verification and disconnected-import steps in
[Container distribution](distribution.md). Record the immutable release, source
commit, image digest, checksum result, GitHub and Cosign verification results,
SBOM review, vulnerability-scan result, internal-registry digest, and a Helm
render that uses the approved digest. Confirm `LICENSE` and
`THIRD_PARTY_LICENSES` are present in the release, container, and packaged Helm
chart, and that the OCI license identifier matches the release metadata. A
tag-only or `latest` deployment does not pass this gate.

## Deployment gate

For the exact signed image digest:

1. Install it on the target storage class and verify one Ready pod, `ReadWriteOncePod`, and `Recreate` behavior.
2. Run create, resend the saved capture, stopping, a stopped observation, stopped, waiting, starting, a healthy observation, and completion through the checked-in playbooks.
3. Exercise `401`, `403`, stale `412`, capture/lock `409`, failed-run reopen, and administrator skip.
4. Confirm event filters and cursors return the expected history.
5. Create a backup, export both files off-volume, recreate the pod, and confirm state persists.
6. Restore the exported pair to a separate volume, run offline `verify`, start Lazarus, and read the completed run.
7. Record startup, attach, backup, export, and restore times against the operational budgets.

AAP validation must also demonstrate that external component jobs are safe to run more than once, targets sharing a `lock_key` run one at a time, the discovery cache survives the execution environment, and recovery reopens the same maintenance and resumes the correct phase. Relaunching the full workflow after failure is not an accepted recovery test.

Before OpenShift 4.22 production use, complete the [OpenShift deployment guide](openshift.md), including restricted security policy, platform-assigned user and group IDs, service certificates, the HTTPS Route, traffic and monitoring labels, storage-driver attach behavior, enterprise image approval, the full authenticated workflow, off-volume export, and separate-volume restore.

Store the exact results for each tested image separately. See the [infrastructure test record](validation-record.md) for the format and currently recorded evidence.
