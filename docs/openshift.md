# OpenShift 4.22 deployment

This guide installs Lazarus on OpenShift Container Platform 4.22. Review the [operating constraints](architecture.md#operating-constraints) before choosing storage or changing the chart's pod and rollout settings.

Run the [validation checklist](validation.md) on the target cluster. The commands below do not prove how that cluster handles its security policy, storage driver (CSI), service certificate (service CA), external HTTPS address (Route), incoming traffic, or image approval.

## Before you begin

Record the namespace, exact signed image digest, block-backed storage option (`StorageClass`), requested capacity, Route hostname, AAP access path, accepted data-loss window, and expected recovery time. Size the volume using the formula in [operations.md](operations.md); the production example starts at 20 GiB.

Start with a published Lazarus release or copy its recursive OCI bundle into the
approved internal registry. Verify the release checksums, SBOM, provenance, and
signing identity as described in [Container distribution](distribution.md), run
the organization's vulnerability scan, and configure admission rules to allow
only the approved digest and signing identity.

The chart tells Helm to keep the volume claim during uninstall. The cluster's reclaim policy separately controls whether the underlying volume is deleted when its claim is removed. The cluster owner must set and verify `Retain`.

Keep the chart's one-replica, `Recreate`, and `ReadWriteOncePod` settings described in the [operating constraints](architecture.md#operating-constraints). After a rollout or node failure, the replacement may wait for the old pod to stop and for the storage driver to move the volume. Measure and record that recovery time.

## Install

Copy `deploy/openshift/values.production.example.yaml` to the ignored `values.local.yaml` file, replace every placeholder, and keep the Route disabled during the initial install.

Create the namespace and obtain the token file from the enterprise secret manager. Keep the local file private with mode `0600`. Each non-blank line must be one JSON object with a caller `name`, `role`, and `token`. The chart mounts it with mode `0440` so OpenShift's assigned container user and group can read it without granting write access or access to other users.

```sh
oc new-project lazarus
umask 077
mkdir -p .local
# Write one or more JSON token records into .local/tokens, one per line:
printf '%s\n' '{"role":"operator","token":"<token>","name":"aap"}' > .local/tokens
oc -n lazarus create secret generic lazarus-api-tokens \
  --from-file=tokens=.local/tokens \
  --dry-run=client -o yaml | oc apply -f -

helm lint deploy/helm/lazarus -f values.local.yaml
helm template lazarus deploy/helm/lazarus -n lazarus -f values.local.yaml > .local/rendered.yaml
helm upgrade --install lazarus deploy/helm/lazarus \
  -n lazarus -f values.local.yaml --wait --timeout 10m
```

Verify one pod is Ready, OpenShift admitted it under the restricted security policy, and the claim uses `ReadWriteOncePod` with the expected storage driver:

```sh
oc -n lazarus rollout status deployment/lazarus --timeout=10m
oc -n lazarus get pod -l app.kubernetes.io/name=lazarus
oc -n lazarus get pod -l app.kubernetes.io/name=lazarus \
  -o jsonpath='{.items[0].metadata.annotations.openshift\.io/scc}{"\n"}'
oc -n lazarus get pvc lazarus -o wide
```

Find the exact persistent volume, set its reclaim policy to `Retain`, and verify the result:

```sh
LAZARUS_PV="$(oc -n lazarus get pvc lazarus -o jsonpath='{.spec.volumeName}')"
oc get pv "$LAZARUS_PV" -o custom-columns=NAME:.metadata.name,CLASS:.spec.storageClassName,RECLAIM:.spec.persistentVolumeReclaimPolicy
oc patch pv "$LAZARUS_PV" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
oc get pv "$LAZARUS_PV" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}{"\n"}'
```

## Enable the HTTPS Route

With `serviceTLS.enabled: true` and no custom TLS Secret, OpenShift creates a service certificate and its signing certificate authority (CA). Lazarus loads that certificate at startup. The re-encrypt Route accepts external HTTPS and also verifies HTTPS to Lazarus. Extract the CA, enable the Route, and configure the Route to trust it:

```sh
oc -n lazarus wait --for=jsonpath='{.data.service-ca\.crt}' configmap/lazarus-service-ca --timeout=5m
oc -n lazarus get configmap lazarus-service-ca \
  -o jsonpath='{.data.service-ca\.crt}' > .local/service-ca.crt
helm upgrade lazarus deploy/helm/lazarus -n lazarus -f values.local.yaml \
  --set route.enabled=true \
  --set-file route.tls.destinationCACertificate=.local/service-ca.crt \
  --wait --timeout 10m
oc -n lazarus get route lazarus
```

Restart Lazarus after the service certificate changes so the process loads the new certificate and key. Lazarus and its healthcheck client require TLS 1.2 or newer. If the service CA changes, extract it again and update the Route and monitoring configuration.

## Network and monitoring

The supplied network policy allows the OpenShift ingress router and, when enabled, user-workload monitoring. AAP outside the cluster should use the TLS Route. If AAP connects from another namespace, add only that namespace and its pods through `networkPolicy.additionalIngressPeers`. Lazarus does not need outbound network access while running, so the policy blocks it.

When using a custom TLS Secret with a Prometheus `ServiceMonitor`, set `serviceTLS.caConfigMap.name/key` to the CA that signed the certificate. The cluster administrator must enable user-workload monitoring separately.

## Smoke and rollout gate

Run the chart connectivity test, then use the [Ansible examples](../examples/ansible/README.md) to create a run, capture starting state, and read it back. Configure `LAZARUS_CAPTURE_CACHE_DIR` on mounted storage that survives AAP jobs so AAP saves the exact discovery document before submission and also includes it in the AAP job output. Use the Route URL ending in `/api/v1`; the AAP execution environment must trust the Route certificate.

```sh
helm test lazarus -n lazarus --logs
```

Before production use, delete and recreate the pod and confirm the saved state remains. Create and verify a backup, then use the administrator download endpoint or `backup.yml` to export the `.db` and matching manifest to approved storage outside the Lazarus volume. Restore it to a different volume, and record the attach, backup, validation, export, and restore times. The snapshot examples use `namespace: lazarus` and example claim names; replace them with the release's actual names. Stop Lazarus before taking a `VolumeSnapshot`, and keep the source volume until validation is approved.

Rehearse pod replacement with a restored database as large as the expected production database and record how long startup takes. The default startup probe allows five minutes (`probes.startupFailureThreshold: 150` at `probes.startupPeriodSeconds: 2`). Increase that limit when measurements show startup needs more time. See [architecture](architecture.md#operating-constraints) for the startup checks.

Set the Route annotation `haproxy.router.openshift.io/timeout`, Lazarus `config.httpWriteTimeout`, and the AAP request timeout above the measured backup duration. The production values example uses a `3m` Route limit around the service's `2m` response limit; tune both from measurements and keep them bounded. Set `config.backupMinAge` to the minimum recovery-point age you need to keep during a burst; a young pair may temporarily keep the local count above `backupRetention`. See the [operations runbook](operations.md) for timeout and retention guidance.

Do not add a fixed user ID, volume group, or security-policy annotation. OpenShift assigns allowed values under its restricted security policy, as required by the [operating constraints](architecture.md#operating-constraints).
