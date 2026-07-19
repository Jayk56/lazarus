# Run Lazarus from AAP

`configure.yml` creates the job templates and workflow defined in `workflow.yml`.
The example uses `infra.aap_configuration` 4.6.0 and keeps the Lazarus API calls,
observations, approval, and component jobs visible in the workflow.

The survey supplies `lazarus_url`, a stable `maintenance_id`, change ticket, and
workflow version. A protected `Lazarus API token` credential injects
`LAZARUS_TOKEN`. A separate administrator credential injects
`LAZARUS_ADMIN_TOKEN` for reopen, skip, and backup. Keep both tokens in
Controller or a secret manager; never put them in surveys or committed files.

The operator credential injector is:

```yaml
inputs:
  fields:
    - {id: lazarus_token, type: string, label: Lazarus bearer token, secret: true}
  required: [lazarus_token]
injectors:
  env:
    LAZARUS_TOKEN: "{{ lazarus_token }}"
```

Define the administrator credential with input `lazarus_admin_token` injected as
`LAZARUS_ADMIN_TOKEN`. Limit that credential with Controller RBAC and approvals.

Set the organization, project, credentials, persistent
`lazarus_capture_cache_dir`, and external stop/start job-template names before
running `configure.yml`. The external templates must be reviewed and safe to run
more than once. Put the capture cache on storage that survives an execution
environment restart.

Install `collections/requirements.yml` through the private Automation Hub used
by your AAP organization. Its Red Hat controller dependencies are not available
from the public Galaxy service. Then validate and apply the configuration from
an approved AAP execution environment:

```sh
ansible-galaxy collection install -r collections/requirements.yml \
  --server automation_hub
export AAP_HOSTNAME=https://aap.example
export AAP_TOKEN='read-from-a-secret-manager'
ansible-playbook --syntax-check configure.yml
ansible-playbook configure.yml \
  -e lazarus_organization='Platform Operations' \
  -e lazarus_project='Lazarus automation' \
  -e lazarus_credential='Lazarus API token' \
  -e lazarus_admin_credential='Lazarus API administrator token' \
  -e lazarus_capture_cache_dir='/approved/lazarus-captures' \
  -e lazarus_stop_job_template='Stop and verify stack' \
  -e lazarus_start_job_template='Start and verify stack'
```

## Recover a failed run

Every failure path after creation records `failed` on the same maintenance.
Do not relaunch the whole workflow: it may repeat discovery or target changes.
Launch `Lazarus - reopen failed maintenance` with the same `maintenance_id`, the
phase where work should resume, and a non-empty `justification`. Then resume at
that workflow phase without create or capture. Target changes remain limited to
the matching maintenance phase. Keep the cached discovery document and the
events already recorded for the run.

Parallelize only targets with different `lock_key` values. A `lock_key` groups
resources that must not run at the same time. Chain targets sharing a lock key,
even when their target IDs differ. Put dependencies around the external
component jobs in the workflow graph.

For WebSphere ND, capture the set of JVMs that were running before the stop. Keep
the deployment manager (DMGR) up while stopping application servers; stop node
agents next, and stop DMGR last only when needed. Start DMGR first, then node
agents and synchronization, then only the application servers recorded as
running at capture time. Use observations for current health and target locks for
ordering.

The example includes one web target. Replace the static sample capture with the
output of your discovery job, then add the rest of the cell as dependency nodes
around the same playbooks. After a `412`, the target-state playbook reads the
target again and stops unless the requested state is already present. Read the
maintenance phase before choosing which workflow step to resume.
