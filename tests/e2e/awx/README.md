# Deterministic AWX end-to-end fixture

This repository-only black-box fixture tests SSH delegation, immutable
discovery capture, exact API ETags, lifecycle barriers, deterministic
observation replay, a real remote-command failure and administrator recovery,
ordered stop/start batches, and final event/history assertions. It is not a
production workflow or an AWX install.

The happy path is deliberately linear and disallows simultaneous launches:

```text
seed -> create -> discover -> capture -> maintenance stopping
  -> stop web/service -> stop nodeagent -> stop dmgr
  -> maintenance stopped -> waiting -> starting
  -> start dmgr -> start nodeagent -> start service/web in reverse order
  -> completed -> assert
```

`discover` through `completed` share one `mark_failed` node. `seed` and
`create` do not mark a possibly unrelated run failed, and the final assertion
does not try to mutate a completed run.

The separate failure-recovery workflow creates and captures one fresh
maintenance, then deliberately substitutes an unknown fixture service only
for `web-02` in the first stop batch:

```text
seed -> create -> discover -> capture -> maintenance stopping
  -> stop web-01 -> web-02 stopping -> remote command fails
  -> mark the same maintenance failed -> assert the partial checkpoint
  -> administrator reopens the same maintenance to stopping
  -> resume the idempotent stop batch -> finish the normal sequence
  -> completed -> assert the recovery profile
```

It does not reseed, create another maintenance, rediscover, or recapture after
the injected failure. Only that injected stop failure reaches the checkpoint
assertion and reopen. Discovery failure ends without mutating a possibly
uncaptured run; capture or pre-injection transition failure may mark the run
failed but ends there. Failures after reopen use a distinct terminal
`mark_failed_recovery` node.

## Fixture hosts

This fixture provides two disposable SSH hosts for an AWX end-to-end run:

* `node-server` has `web01`, `web02`, and `web03` running; `web04` and
  `web05` stopped; and `service01`, `service02`, `service03`, and `nodeagent`
  running.
* `dmgr-server` has `dmgr` running.

The services are deliberately not real application servers.  The image stores
their state in a small state file and exposes `/usr/local/bin/fixture-service`
for deterministic setup and inspection.  It also provides a `systemctl`
compatibility shim so an Ansible component job can use the normal `start`,
`stop`, and `is-active` commands.  Repeating any state change is safe.

Lazarus target IDs use `web-01` through `web-05`, `service-01` through
`service-03`, `nodeagent`, and `dmgr`. The first three web processes, all three
service processes, nodeagent, and dmgr are originally running. `web-04` and
`web-05` are originally stopped and are never transitioned or observed.

All node-server targets share the run-scoped lock
`awx-e2e:<maintenance-id>:node-server`; dmgr uses the corresponding dmgr lock.
The workflow processes each batch sequentially. These run-scoped locks keep
parallel test runs independent; production automation should use stable
resource lock keys.

## Stock AWX 24.6.1 bootstrap

[configure-awx.yml](configure-awx.yml) is the live-pilot bootstrap for stock
AWX. It uses the pinned `awx.awx` collection to create or reconcile:

- the `Default` organization without creating any AWX Demo objects;
- an unambiguously fixture-owned `Lazarus AWX EE (24.6.1)` execution
  environment using
  `quay.io/ansible/awx-ee:24.6.1`;
- the SCM project at the explicitly supplied repository URL and branch;
- the fixture inventory and an SCM inventory source for
  `tests/e2e/awx/inventory.example.yml`;
- separate secret custom credential types and credentials that inject only
  `LAZARUS_TOKEN` or only `LAZARUS_ADMIN_TOKEN`, plus the fixture Machine
  credential;
- all nine job templates; and
- the 17-node happy workflow and 22-node failure-recovery workflow, including
  exact success/failure edges and separate launch surveys.

AWX's `create_preload_data: false` switch is installation-time configuration,
not an `awx.awx.organization` option. Confirm that setting on the AWX 24.6.1
installation before bootstrapping. This playbook does not modify the AWX
deployment or delete pre-existing Demo objects.

The Controller OAuth token must belong to an AWX superuser because creating a
custom credential type requires that permission. The SCM revision must contain
this fixture. Public Git needs no SCM credential; a private repository requires
an environment-owned Source Control credential and a corresponding extension
to the project task.

Install the test-specific collection into a local ignored directory:

```sh
ansible-galaxy collection install \
  -r tests/e2e/awx/collections/requirements.yml \
  -p .local/awx-collections
export ANSIBLE_COLLECTIONS_PATH="$PWD/.local/awx-collections"
```

Supply all connection and secret inputs outside the repository, then run the
bootstrap:

```sh
export CONTROLLER_HOST='https://awx.example.invalid'
export CONTROLLER_OAUTH_TOKEN='REDACTED'
export CONTROLLER_VERIFY_SSL='true'
export AWX_PROJECT_SCM_URL='https://github.com/Jayk56/lazarus.git'
export AWX_PROJECT_SCM_BRANCH='main'
export LAZARUS_TOKEN='REDACTED-OPERATOR'
export LAZARUS_ADMIN_TOKEN='REDACTED-DIFFERENT-ADMIN'
export AWX_MACHINE_USERNAME='awx-fixture'
export AWX_MACHINE_SSH_KEY_FILE='/absolute/path/to/private-key'

ansible-playbook --syntax-check tests/e2e/awx/configure-awx.yml
ansible-playbook tests/e2e/awx/configure-awx.yml
```

Use exactly one of `AWX_MACHINE_SSH_KEY_FILE` or `AWX_MACHINE_SSH_KEY`.
`CONTROLLER_VERIFY_SSL` defaults to `true`; keep it enabled and put the AWX and
Lazarus trust chains in the bootstrap environment and AWX execution
environment respectively. The bootstrap has `no_log` on every task that reads
or writes API tokens and SSH private-key material.

`LAZARUS_TOKEN` must identify an operator; `LAZARUS_ADMIN_TOKEN` must be a
different, narrowly controlled administrator token because failed-run reopen
is an administrator-only API operation. The administrator credential is
attached only to the reopen job template and is never attached to seed,
ordinary lifecycle, target, or assertion jobs. The bootstrap creates the
objects but does not assign Controller RBAC. Grant the recovery workflow's
Execute role and the administrator credential's Use role only to the approved
recovery team; ordinary fixture operators should receive only the happy
workflow and ordinary credential permissions.

The API URL and maintenance identity are not environment variables in the job
templates. They, along with change ticket and workflow version, come from the
workflow survey and propagate as AWX extra vars.

## AAP configuration alternative

[configure.yml](configure.yml) retains the existing
`infra.aap_configuration` path for an AAP Controller where the organization,
project, inventory, execution environment, operator API credential,
administrator credential, and Machine credential already exist. The named
administrator credential must inject only `LAZARUS_ADMIN_TOKEN`; keep it
attached only to the reopen template as defined in [workflow.yml](workflow.yml).
Install its pinned collection, update the prerequisite names in
[workflow.yml](workflow.yml), and create the templates and workflow:

```sh
ansible-galaxy collection install --no-deps \
  -r examples/aap/collections/requirements.yml
export AAP_HOSTNAME='https://controller.example.invalid'
export AAP_TOKEN='REDACTED'
ansible-playbook --syntax-check tests/e2e/awx/configure.yml
ansible-playbook tests/e2e/awx/configure.yml
```

The syntax check for `configure.yml` requires the `ansible.controller`
collection available in the approved AAP execution environment. The ordinary
fixture playbooks use built-in modules and run in `make validate-source`.
`configure-awx.yml` instead requires `awx.awx` 24.6.1 from the test-specific
requirements file.

## Expected results

Launch each workflow with a different, fresh maintenance ID. The happy
workflow's final assertion requires:

- maintenance state `completed`, version `39`, and ETag `"39"`;
- every originally-running target `healthy` at version `10`;
- `web-04` and `web-05` still `stopped` at version `6`;
- 55 filtered events: 7 maintenance events, 32 target transitions, and 16
  observations;
- no target events for `web-04` or `web-05`;
- the API capture equal to the complete discovery artifact propagated by AWX;
- every external fixture service restored to its seeded state.

The first stop transition also sends an intentionally stale target ETag and
requires `412` with no side effect. Every real transition GETs and round-trips
the current opaque ETag; observations reuse stable request IDs derived only
from maintenance ID, target ID, and final phase.

These counts are an executable contract for the current numeric-version API.
If the version model changes intentionally, update the API tests and this
fixture together.

The failure-recovery workflow first asserts the exact injected checkpoint:

- the same maintenance is `failed` at version `7` and ETag `"7"`;
- `web-01` is stopped at target version `5`, while `web-02` is still API state
  `stopping` at version `4` and its external fixture process is still running;
- all other targets and external processes remain at the expected captured or
  original state;
- the immutable API capture still equals the propagated discovery artifact;
- 8 events exist: 4 maintenance events, 3 target transitions, and 1
  observation, with no reopen event yet.

The required non-empty survey justification authorizes an administrator PATCH
from `failed` back to `stopping` under a distinct reopen request-ID scope. The
resumed stop batch safely replays `web-01`, continues from `web-02`, and then
finishes the unchanged sequence. Its final recovery assertion requires:

- maintenance `completed` at version `41` and ETag `"41"`;
- every originally-running target `healthy` at version `12`;
- `web-04` and `web-05` still `stopped` at version `8`;
- 57 events: 9 maintenance events, 32 target transitions, and 16 observations;
- the unchanged capture and exactly one administrator
  `maintenance.reopened` audit event.

The discovery node publishes the complete immutable document with `set_stats`.
The capture and final assertion jobs consume that AWX workflow artifact; no
shared execution-environment filesystem is required.

On an unexpected failure, retain the same maintenance ID, discover-job
artifact, filtered `/events`, and AWX job output. Do not relaunch either full
workflow or rediscover after a component has changed state. The deterministic
recovery is part of its single workflow launch; it is not a second manual run
against the failed maintenance.

## Build the image

Build the image and make it available to the cluster used by the test.  The
manifest uses the neutral local tag `lazarus-awx-fixture:dev`; change that tag
with a local kustomize overlay or `kubectl set image` when using a registry.

```sh
docker build -t lazarus-awx-fixture:dev tests/e2e/awx/fixture-image
```

## Provide SSH authentication

The Kubernetes manifests intentionally do not contain a private key,
authorized key, password, or cluster address.  Create the required Secret from
the public key that corresponds to the private key configured in AWX:

```sh
kubectl apply -f tests/e2e/awx/kubernetes/namespace.yaml
kubectl -n awx-fixture create secret generic awx-fixture-ssh \
  --from-file=authorized_keys=/path/to/fixture-authorized-key
```

The key file should contain one or more OpenSSH `authorized_keys` lines.  Keep
the private key in the AWX credential or an external secret manager.  The
fixture image accepts the SSH user `awx-fixture`; the service DNS names are
`node-server.awx-fixture.svc` and `dmgr-server.awx-fixture.svc` when the sample
namespace is used. The pods generate ephemeral SSH host keys, so this test
inventory disables host-key verification. Production inventories should use
stable, verified host keys.

## Deploy

Apply the kustomization after creating the Secret. It creates the namespace,
one Deployment per logical host, one ClusterIP Service per host, and a network
policy that permits SSH only from pods in the `awx` namespace.

```sh
kubectl apply -k tests/e2e/awx/kubernetes
kubectl -n awx-fixture rollout status deployment/node-server
kubectl -n awx-fixture rollout status deployment/dmgr-server
```

If the cluster cannot use a locally built image, apply a small overlay that
sets `lazarus-awx-fixture` to the approved registry image.  Do not add
credentials or cluster-specific addresses to this directory.

## Inspect or reset a host

The profile is selected by `FIXTURE_PROFILE=node` or `FIXTURE_PROFILE=dmgr` in
each Deployment.  For debugging, run the CLI inside the corresponding pod:

```sh
kubectl -n awx-fixture exec deploy/node-server -- \
  /usr/local/bin/fixture-service list --json
kubectl -n awx-fixture exec deploy/node-server -- \
  /usr/local/bin/fixture-service set web04 running
kubectl -n awx-fixture exec deploy/node-server -- \
  /usr/local/bin/fixture-service seed
```

`seed` restores the profile's initial state.  The state directory is an
`emptyDir` mounted at `/var/lib/awx-fixture`, so it survives a process restart
but is reset when the pod is recreated.

## Known limits

- The manifests are disposable test fixtures and do not install AWX, Lazarus,
  a registry, ingress, certificates, or persistent AWX project storage.
- The neutral `lazarus-awx-fixture:dev` reference must be replaced by a
  deploy-time overlay when the cluster cannot load local images.
- Syntax and manifest checks do not prove Controller credentials, SSH routing,
  private-CA trust, storage mounts, or target-cluster admission behavior.
- A successful ordered workflow demonstrates this graph's serialization; it is
  not a general concurrent lock-contention or AWX scaling test.
