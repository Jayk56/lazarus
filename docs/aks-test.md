# Test Lazarus on disposable AKS

`deploy/aks/values.test.yaml` targets AKS Kubernetes 1.35.5 with an Azure Disk `managed-csi` volume. The test covers the image, chart, API, and common Kubernetes storage behavior. It does not cover OpenShift security policy, Routes, or service certificates.

Follow the [operating constraints](architecture.md#operating-constraints). This profile uses an unencrypted ClusterIP on port 8080 and `fsGroup: 65532` for a short-lived test only. Access that port only through a local `kubectl port-forward`.

## Test procedure

1. Create a dedicated resource group, AKS cluster, and registry. Record their exact names so you can remove the complete test environment.
2. Build the image remotely, record its exact digest, and render the chart with `deploy/aks/values.test.yaml` plus that repository and digest.
3. Create a namespace and token Secret from a mode-`0600` local token file, install the chart, and wait for the pod and `ReadWriteOncePod` Azure Disk claim.
4. Run `helm test`. For API calls from the workstation, start:

   ```sh
   kubectl -n lazarus port-forward service/lazarus 18080:8080
   export LAZARUS_URL=http://127.0.0.1:18080/api/v1
   export MAINTENANCE_ID=aks-example-001
   ```

5. Exercise create/list/read, resending the same capture, lock conflicts, outdated maintenance and target ETags, maintenance and target state rules, observations, event filters and paging, failed-run reopen, administrator skip, and `POST /admin/backups`. After the first run completes, confirm a new run can reuse its target IDs and lock keys.
6. Delete the pod and verify that the volume reattaches and the saved record remains. Perform a chart upgrade and confirm that the replacement pod starts only after the current pod stops.
7. Scale Lazarus to zero before a recovery pod mounts the claim. Verify a backup, restore it to a different claim, and start one test instance against the restored copy. Never overwrite the only live test database.
8. Record volume attach and detach time, restart time, database mode, backup time, validation results, and restore time.
9. Delete the exact test resource group and verify it no longer exists.
