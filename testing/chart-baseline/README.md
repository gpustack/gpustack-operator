# Chart values baseline

The values that `pkg/worker/kuberess` feeds to Helm for each bundled application, captured
before those Go values templates are replaced by chart values. One file per application:

| file | rendered from |
| --- | --- |
| `kueue.yaml` | `apps_kueue.go`, release `gpustack-kueue`, chart `kueue` 0.17.6/0.18.2 |
| `node-feature-discovery.yaml` | `apps_node_feature_discovery.go`, release `gpustack-node-feature-discovery`, chart 0.18.3 |
| `csi-driver-nfs.yaml` | `apps_csi_driver_nfs.go`, release `gpustack-csi-driver-nfs`, chart 4.13.2 |
| `csi-driver-s3.yaml` | `apps_csi_driver_s3.go`, release `gpustack-csi-driver-s3`, chart 0.43.7 |
| `device-manager.yaml` | `apps_gpustack_device_manager.go`, release `gpustack-operator-device-manager`, the operator chart |

These files are the parity oracle for that migration: the chart's values have to reproduce
them, so they outlive the test that produced them.

## Regenerating

```bash
go test ./pkg/worker/kuberess/ -run TestDumpChartValuesBaseline -args -update
```

Without `-update` the same test verifies the committed files instead of rewriting them.
Never edit a file here by hand.

## What the capture is pinned to

Nothing is read from a cluster or from the settings API. The rendering context is the one
`InstallApplications` builds, held at the stock defaults an unconfigured install produces:
blank container registry and namespace (each template applies its own `docker.io` /
`gpustack` fallback), no image pull secrets, `IfNotPresent` pull policy, the
`gpustack-system` namespace, and every known accelerator manufacturer — the same set as the
operator chart's `manufacturers` default.

Two branch choices are worth stating, because neither is visible in the output:

- **Kueue** is captured once. The Kubernetes-version branch in `installKueue` selects the
  chart tarball, not the values, so both branches render this file. Cert-manager is treated
  as absent, which is the `enableCertManager: false` configuration the chart pins.
- **The device-manager** is captured with the running worker's image undetected, the branch
  taken outside the cluster. It renders the superset of keys: the chart composes its image
  from the global registry and namespace instead of pinning the worker's own reference.

## Reading a diff against these files

The capture is the value tree Helm receives, encoded canonically with `sigs.k8s.io/yaml`:
keys sorted, comments and key order from the Go templates dropped. Quoting follows the
encoder, so a scalar is quoted only where YAML would otherwise read it as a number — the
PCI vendor IDs come out as a mix of `"1002"` and `10de`, and are strings either way.
Compare parsed values, not bytes.

One environment caveat: `pkg/nodefeature` resolves `GPUSTACK_<MANUFACTURER>_PCI_VENDOR_ID`,
`_ACCELERATABLE_RESOURCE_NAME` and `_PARTITION_KIND` when the package is initialized, which
is too early for the test to pin. With any of those exported, a regenerated capture is
skewed; the verifying mode of the test fails loudly on such a machine rather than accepting
it silently.
