# Security

Use the following boundaries when deploying and operating Lazarus.

Report product vulnerabilities through the private process in
[SECURITY.md](../SECURITY.md), not through a public issue.

## Access

All `/api/v1` operations require a bearer token from the mounted token file. A `reader` can view data, an `operator` can create runs and change run or target state, and an `admin` can create and download backups, reopen failed runs, and approve target skips. The six public operational endpoints—`/healthz`, `/livez`, `/startupz`, `/readyz`, `/version`, and `/metrics`—do not require a token.

Lazarus never writes token values to logs or event history. Callers control observation and detail fields, and Lazarus may copy them into event history, so never place secrets in an API field.

The mounted token file contains one JSON object per line, for example `{"role":"operator","token":"...","name":"aap"}`. Each record must use only the fields `role`, `token`, and `name`; `name` identifies the caller in logs and event history. The file must not be writable or executable by its group and must have no permissions for other users (`0600` locally; the chart mounts the Secret with mode `0440` so the assigned pod group can read it). Tokens are loaded at startup, so restart the pod after rotating the Secret. Never place real tokens in values files, examples, logs, or backups.

Roles apply to the whole service. Any configured reader can read every maintenance run, and any operator can change every run. Maintenance IDs do not limit access. Give tokens only to trusted AAP workflows and administrators.

## TLS and network access

Set `LAZARUS_TLS_CERT_FILE` and `LAZARUS_TLS_KEY_FILE` together to make Lazarus serve HTTPS directly. Otherwise, terminate TLS at the OpenShift Route or an approved proxy. Allow network access only from AAP, the ingress router, and monitoring. The supplied `NetworkPolicy` is a starting point and must be checked against the namespace labels in the target cluster.

The native server and healthcheck client require TLS 1.2 or newer. Lazarus reads TLS files only at startup. Restart it after the service certificate changes. If the signing CA changes, also update the Route and monitoring trust. Never expose an unencrypted service port outside a trusted in-cluster proxy path. Backup files require the administrator download endpoint, and restore runs only as a command while the service is stopped.

## Stored data

The database and backup files may contain maintenance details and host names. Enable storage encryption, restrict access to the persistent volume, and use a `Retain` reclaim policy so an uninstall or recovery action does not remove it unexpectedly. Keep token files and TLS keys in separate Kubernetes Secrets with access limited to the Lazarus pod. Export only the checked `.db` and manifest pair to approved storage outside the Lazarus volume. Lazarus records the download as `backup.downloaded`. The [operating constraints](architecture.md#operating-constraints) describe the backup and volume-loss limits.

## Pod permissions

The chart runs Lazarus as a non-root user assigned by the platform, removes extra Linux capabilities, blocks privilege escalation, applies the platform's default system-call filter, and does not mount a Kubernetes service-account token. Deploy the approved image by exact digest. Lazarus does not need cluster-wide permissions; it needs only its data volume and its own network traffic.

## Limits of bearer tokens

A bearer token proves that a caller has a Lazarus role; it does not identify the individual operator or record an approval. Use AAP access controls, approval steps, and change tickets around token use. Anyone with an operator token can change target state. A node or storage administrator may be able to read the volume, so use platform encryption and access auditing.
