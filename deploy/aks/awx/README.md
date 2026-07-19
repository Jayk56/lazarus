# AWX pilot on AKS

This pilot runs AWX 24.6.1 through AWX Operator 2.19.1. It uses a private
ClusterIP service and an 8 GiB `managed-csi` Azure Disk for PostgreSQL. Access
the UI and API only through a local port forward.

The operator's supported data-volume initializer gives the non-root PostgreSQL
process (UID 26) ownership of its Azure Disk directory before startup.

The operator image is pinned to its multi-platform digest. AWX application
images are pinned to the version selected by that operator release. The AWX
project synchronizes from Git, so the projects directory is not persistent.
Workflow history, job artifacts, credentials, and configuration live in the
PostgreSQL volume.

Operator metrics are disabled in this pilot. The upstream 2.19.1 metrics proxy
references a retired GCR image, and no monitoring system in this test consumes
that endpoint.

AWX Operator does not publish a Kubernetes compatibility matrix. Version 2.19.1
uses current Kubernetes API families, but it predates Kubernetes 1.34 and is not
certified for AKS. Treat the install, project sync, credential injection, and
test workflow as required compatibility checks.

## Install

Create the namespace and an administrator password without writing the password
to a manifest or shell history:

```sh
kubectl create namespace awx
kubectl -n awx create secret generic awx-admin-password \
  --from-file=password=/path/to/mode-0600-password-file
kubectl apply -k deploy/aks/awx/operator
kubectl -n awx rollout status deployment/awx-operator-controller-manager \
  --timeout=5m
kubectl apply -f deploy/aks/awx/awx.yaml
kubectl -n awx wait --for=condition=successful awx/awx --timeout=20m
kubectl -n awx wait --for=condition=available deployment/awx-web \
  --timeout=10m
kubectl -n awx wait --for=condition=available deployment/awx-task \
  --timeout=10m
```

Confirm the PostgreSQL claim is bound, then open a local connection:

```sh
kubectl -n awx get pods,pvc,awx
kubectl -n awx port-forward service/awx-service 18081:80
```

Open `http://127.0.0.1:18081` and sign in as `admin` with the password from the
local file. Do not expose this pilot through a public load balancer or ingress.

## Persistence boundary

The managed PostgreSQL volume is the durable AWX record for this pilot. AWX job
artifacts preserve the discovered Lazarus capture outside the Lazarus volume,
but this does not replace an enterprise backup for AWX. Before keeping the pilot
beyond a disposable test, create and restore an AWX Operator backup that
includes the database and generated secrets.
