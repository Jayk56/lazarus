# Infrastructure validation record

This public record summarizes the V1 deployment evidence described in
[Validation](validation.md). The test deployments used immutable, digest-pinned
container images built from the release-candidate source. Registry coordinates,
digests, run identifiers, and other environment-specific evidence are retained
outside the repository.

## V1 AKS and AWX end-to-end pilot

### Environment

- Azure Kubernetes Service running Kubernetes 1.35.5 on amd64. The pilot ran
  on one node with cluster autoscaling configured to permit a second node.
- Lazarus used one 1 GiB `managed-csi` Azure Disk with
  `ReadWriteOncePod` access.
- AWX PostgreSQL used one 8 GiB `managed-csi` Azure Disk with
  `ReadWriteOnce` access. AWX projects synchronized from public Git.
- Stock AWX 24.6.1 was installed by AWX Operator 2.19.1.
- Lazarus, AWX, and both deterministic SSH fixture services used private
  ClusterIP Services. NetworkPolicy allowed Lazarus access from AWX and
  fixture SSH access only from the AWX namespace.

### Validation results

- Public source-validation CI passed for the happy-path and recovery-test
  revisions. The previously reported high-severity `ansible-core` validation
  dependency alert was also resolved.
- Lazarus, AWX web/task, PostgreSQL, and both fixture pods remained Ready with
  no restarts observed after either workflow. Both persistent volume claims
  were Bound, and the post-run Helm test passed.
- AWX synchronized the tested source, imported the two fixture inventory
  hosts, and provisioned nine job templates, a 17-node happy-path workflow,
  and a separate 22-node failure-recovery workflow.
- The successful workflow executed all 16 happy-path nodes; its terminal
  failure node did not run, and the final assertion passed.
- Happy-path maintenance completed at version 39 with strong ETag `"39"`.
  The final aggregate contained eight healthy originally-running targets at
  version 10 and two originally-stopped targets at version 6.
- Happy-path history contained 55 events: 7 maintenance events, 32 target
  transitions, and 16 target observations. The immutable capture contained
  all 10 targets and matched the discovery artifact propagated by AWX.
- The workflow demonstrated the intended barriers and ordering: six
  application and service processes stopped before the node agent; the
  deployment manager stopped last and started first; the node agent started
  second; and application and service processes were restored in reverse
  order.
- Final external state matched discovery: three web processes, three service
  processes, the node agent, and the deployment manager were running; two web
  processes remained stopped. The pilot remained on one node without
  autoscaler intervention.

### Failure and recovery result

- The recovery workflow injected a real remote-command failure after
  `web-01` completed its stop and after `web-02` entered Lazarus state
  `stopping`. The command addressed an unknown fixture service, so the
  external `web-02` process remained running.
- The initial failure handler marked the same maintenance `failed` at version
  7. The failure assertion verified the immutable capture, exact partial API
  and external state, and 8 events: 4 maintenance events, 3 target
  transitions, and 1 observation, with no reopen event.
- A separate administrator credential reopened that maintenance from
  `failed` version 7 to `stopping` version 8 with a non-empty justification.
  The audit history recorded one `maintenance.reopened` event with the
  administrator role. Seed, create, discovery, and capture did not repeat.
- The resumed stop batch idempotently replayed the already-recorded `web-01`
  observation, completed `web-02`, and continued the original stop/start
  sequence. The final recovery assertion passed.
- Recovered maintenance completed at version 41 with strong ETag `"41"`.
  The final aggregate contained eight healthy originally-running targets at
  version 12 and two originally-stopped targets at version 8.
- Recovery history contained 57 events: 9 maintenance events, 32 target
  transitions, and 16 observations. It retained the single immutable
  10-target capture and contained exactly one operator failure transition and
  one administrator reopen.
- Independent fixture inspection confirmed that all originally-running
  processes were running again and the two originally-stopped web processes
  remained stopped. The cluster remained on one node without autoscaler
  intervention.

### Known gaps

- This validates stock AWX and deterministic stand-in services, not a licensed
  AAP Controller or real WebSphere commands.
- The AKS test profile used internal HTTP. Ingress, public DNS, certificate
  issuance, TLS trust distribution, and external identity integration were
  not tested.
- Node loss, Azure Disk detach/reattach, zone loss, Lazarus backup/restore, and
  AWX Operator backup/restore were not exercised.
- AWX artifacts were durable in its PostgreSQL volume for this pilot, but no
  off-cluster AWX backup was configured.
- AWX Operator 2.19.1 does not publish certification for Kubernetes 1.35.
  This successful run is compatibility evidence for the tested topology, not
  a vendor support statement.
