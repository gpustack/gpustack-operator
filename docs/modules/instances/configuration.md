# GPU Instance Configuration

An `Instance` combines compute sizing from an `InstanceType` with container settings, optional node
pinning, SSH or registry credentials, and workspace or model volumes.

## Contents

- [Resource requests](#resource-requests)
- [Node pinning](#node-pinning)
- [SSH access](#ssh-access)
- [Private registries](#private-registries)
- [Workspace and persistent volumes](#workspace-and-persistent-volumes)
- [Additional volumes](#additional-volumes)
- [Custom InstanceType](#custom-instancetype)

## Resource requests

Configure whole accelerators, a logical slice, or a hardware partition in *`spec.resources`*. See
[Accelerator Requests](../devices/requests.md#instance-api-requests) for per-mode validation rules.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: sliced-workspace
  namespace: team-a
spec:
  type: gpustack--nvidia-a10g-linux-amd64  # required; immutable while running
  image: ubuntu:24.04  # required; immutable while running
  imagePullPolicy: IfNotPresent  # optional, default: IfNotPresent; enum: Always, IfNotPresent, Never
  command:
    - sleep
    - infinity
  privileged: false  # optional, default: false; gated by instance-privileged-allowed
  env:
    - name: WORKSPACE_ENV
      value: dev
  ports:
    - name: http
      port: 8080
      protocol: TCP  # optional, default: TCP; enum: TCP, UDP, SCTP
  resources:
    accelerator: "1"  # must be "1" for sliced or partitioned requests
    acceleratorSlicedMemoryPercentage: 50  # optional, 0..100; copies to cores if cores omitted
    acceleratorSlicedCoresPercentage: 50  # optional, 0..100; copies to memory if memory omitted
    # acceleratorPartitionedProfile: 3g.40gb  # optional; mutually exclusive with slice percentages
    # cpu: "4"  # optional; capped by unitResources.cpu * unit share
    # ram: 32Gi  # optional; capped by unitResources.ram * unit share
    localStorage: 20Gi  # optional, default: 15Gi; capped by InstanceType spec.localStorage
  volumeMount: /workspace  # optional, default: /workspace
  volume:  # required; permanently immutable
    ephemeral:
      capacity: 10Gi
```

- **Running immutability**: *`spec.volume`* is permanently frozen. All other template fields,
  *`nodeName`*, and *`additionalVolumes`* freeze while running and become editable when `spec.stop: true`
  ([Workspace stop and restart](operations.md#workspace-stop-and-restart)).
- **Privileged gate**: Requesting `spec.privileged: true` requires the `instance-privileged-allowed`
  `Setting` (`false` by default). Already-privileged `Instance`s stay editable if the `Setting` is
  disabled later.

## Node pinning

Set *`spec.nodeName`* to pin an `Instance` to one Kubernetes `Node` while keeping Kueue quota and
scheduler checks active:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: sliced-workspace
   namespace: team-a
 spec:
   type: gpustack--nvidia-a10g-linux-amd64
+  nodeName: gpu-node-01  # optional; max 253 chars, editable while stopped
```

The controller renders *`spec.nodeName`* as `nodeSelector["kubernetes.io/hostname"]` on the backing
`Pod` (using the `Node`'s hostname label), never as `pod.spec.nodeName`. *`status.nodeName`* reports
where the `Pod` runs.

Admission checks only that the named `Node` exists on `CREATE` (including when `spec.stop: true`).
Pinning a `Node` outside the `InstanceType` pool, or starting an `Instance` whose pinned `Node` was
later removed, is admitted and leaves the `Pod` `Pending`.

## SSH access

Create an `InstanceSSHPublicKey` in the `Instance` namespace and reference it in
*`spec.sshPublicKey`*. Each `InstanceSSHPublicKey` maps to a same-named `Secret` storing
`authorized_keys`.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceSSHPublicKey
metadata:
  name: workspace-key
  namespace: team-a
spec:
  displayName: Team A key  # optional; max 64 chars
  data: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExamplePublicKey user@host  # SSH public key line
---
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: ssh-workspace
  namespace: team-a
spec:
  type: gpustack--nvidia-a10g-linux-amd64
  image: ubuntu:24.04
  command:
    - tail
    - -f
    - /dev/null
  sshPublicKey:
    name: workspace-key  # mounts authorized_keys and adds sshd sidecar
  resources:
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 20
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

When *`spec.sshPublicKey`* is set, the operator adds an `sshd` sidecar (`instance-ssh-server-image`
`Setting`) that enters `main`'s namespaces. On accelerated workspaces, `sshd` requests
`device.gpustack.ai/<manufacturer>.visibility` matching `main`'s card count so the device plugin
grants narrow cgroup access to `main`'s allocated device or partition without extra quota
([Pod request examples](../devices/requests.md#pod-request-examples)).

Once `PHASE` is `Ready`, forward port `22` and connect as `root`:

```bash
kubectl -n team-a wait --for=jsonpath='{.status.phase}'=Ready instance/ssh-workspace --timeout=5m
kubectl -n team-a port-forward pod/ssh-workspace 2222:22
```

```bash
ssh -i ~/.ssh/id_ed25519 -p 2222 root@127.0.0.1
```

To expose SSH without port-forwarding, add `port: 22` under *`spec.ports`* and connect to the
host address in *`status.accessAddresses`* (`instance-access-static-address` or
`instance-access-wildcard-dns` `Setting`s) on the allocated NodePort in *`status.ports`*.

## Private registries

Create an `InstanceImagePullSecret` in the same namespace and reference it from
*`spec.imagePullSecret`*. It maps to a same-named `kubernetes.io/dockerconfigjson` `Secret`.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceImagePullSecret
metadata:
  name: registry-creds
  namespace: team-a
spec:
  registry: registry.example.com  # required on create/update; write-only
  username: robot-team-a  # required on create/update; write-only
  password: <registry-token>  # required on create/update; write-only
  email: ops@example.com  # optional; write-only
```

Set `spec.imagePullSecret.name: registry-creds` on the `Instance`. Credential fields are write-only
and must be supplied together on every create or update.

## Workspace and persistent volumes

*`spec.volume`* mounts at *`spec.volumeMount`* (default `/workspace`) and accepts either
*`ephemeral`* (`EmptyDir` capped at *`capacity`*) or *`persistent`* (an `InstancePersistentVolume`
in the same namespace). Both cannot be set together.

An `InstancePersistentVolume` maps to a same-named `PersistentVolumeClaim` backed either by an
existing Kubernetes `StorageClass` or by a cluster-scoped `InstancePersistentVolumeType` (which
creates a `StorageClass` for NFS or S3):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstancePersistentVolumeType  # cluster-scoped; creates a StorageClass
metadata:
  name: shared-nfs
spec:
  nfs:  # set exactly one of nfs or s3
    server: 10.0.0.25  # required; immutable
    share: /exports/workspaces  # required, default: /; immutable
    subDirectory: ${pvc.metadata.namespace}/${pvc.metadata.name}  # optional; immutable
    mountPermissions: "0755"  # optional octal string
  # s3:
  #   endpoint: https://s3.example.com  # required http or https URL
  #   region: us-east-1  # optional; required for AWS
  #   accessKey: <access-key>  # write-only
  #   secretKey: <secret-key>  # write-only
  #   bucket: team-workspaces  # optional; immutable
  #   prefix: ${pvc.metadata.namespace}/${pvc.metadata.name}  # optional; immutable
  #   mounter: geesefs  # optional, default: geesefs; enum: geesefs, rclone, s3fs; immutable
---
apiVersion: worker.gpustack.ai/v1
kind: InstancePersistentVolume  # namespaced; creates a PVC
metadata:
  name: team-a-home
  namespace: team-a
spec:
  type: shared-nfs  # StorageClass name; required only when no default StorageClass exists; immutable
  capacity: 50Gi  # optional, default: 20Gi; immutable
  accessMode: ReadWriteMany  # optional; enum: ReadWriteOnce, ReadWriteMany, ReadOnlyMany, ReadWriteOncePod
```

Reference the volume on `Instance` with `spec.volume.persistent.name: team-a-home`. With
`instance-persistent-volume-placement` `true` (default), the controller injects bound PV node
affinity into the `Instance` `Pod` at creation and waits in `Starting` while an immediate-binding
claim is unbound.

## Additional volumes

Use *`spec.additionalVolumes`* to mount extra volumes into the `main` container (also visible in SSH
sessions). Each entry requires a unique canonical absolute *`mountPath`* distinct from
*`spec.volumeMount`* and exactly one source:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: model-workspace
  namespace: team-a
spec:
  type: gpustack--nvidia-a10g-linux-amd64
  image: ubuntu:24.04
  command:
    - sleep
    - infinity
  volume:
    ephemeral:
      capacity: 10Gi
  additionalVolumes:  # optional; atomic list, editable while stopped
    - mountPath: /data
      readOnly: true  # optional, default: false
      subPath: datasets  # optional; relative path without ".."
      persistent:
        name: team-a-home  # InstancePersistentVolume in this namespace
    - mountPath: /etc/workspace-config
      configMap:
        name: app-config  # ConfigMap in this namespace (or secret.name for a Secret)
    - mountPath: /models/qwen
      model:
        artifactRef:
          name: qwen-7b  # ModelArtifact in this namespace; always read-only, no subPath
    # - mountPath: /host-cache
    #   hostPath:  # requires instance-host-path-volume-allowed: "true"
    #     path: /var/cache/shared
    #     type: Directory
```

| Source | Target resource | Constraints |
|---|---|---|
| *`persistent`* | `InstancePersistentVolume` | Same namespace; DNS-1123 name; respects PV node affinity. |
| *`configMap`* / *`secret`* | `ConfigMap` / `Secret` | Same namespace; DNS-1123 name; missing objects surface on `Pod` events. |
| *`model`* | `ModelArtifact` | Always read-only; *`subPath`* forbidden; mounts PVC directly, hub via `model.csi.gpustack.ai`, or OCI image via kubelet `>= 1.35` and containerd `>= 2.1` ([Model Artifact](../model-delivery/artifact.md#workload-consumption)). |
| *`hostPath`* | Node path | Canonical absolute path; adding or widening a grant requires `instance-host-path-volume-allowed: "true"` (default `false`). |

## Custom InstanceType

Create a custom `InstanceType` when you disable `instance-type-derived-from-node` or want custom
CPU and RAM sizing beside an existing derived pool. Inspect `kubectl get instancetypeflavors` to
read the pool's *`acceleratorGroup`* (or *`generalGroup`*), *`os`*, and *`arch`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType  # cluster-scoped, short name instype
metadata:
  name: a10g-highmem
spec:
  displayName: NVIDIA A10G High RAM  # optional; editable
  description: Custom sibling with 128Gi unit RAM  # optional; editable
  inactive: false  # optional, default: false; editable (holds new queue admissions)
  acceleratable: true  # required; immutable
  acceleratorGroup: nvidia-a10g  # required when acceleratable is true; immutable
  generalGroup: generic  # optional, default: generic; immutable
  os: linux  # required; immutable
  arch: amd64  # required; immutable
  unitResources:  # required; immutable
    cpu: "16"  # positive integer without unit suffix ("1" required when acceleratable is false)
    ram: 128Gi  # positive integer with case-sensitive "Gi" suffix
  localStorage: 200Gi  # required; positive integer with case-sensitive "Gi" suffix; immutable
```

- **Shared pool queue**: A custom sibling with the same (*`acceleratable`*, *`acceleratorGroup`*,
  *`generalGroup`*, *`os`*, *`arch`*) shares the pool's canonical `ClusterQueue` without
  interrupting running `Instance`s.
- **CPU-only types**: Set `acceleratable: false`, omit *`acceleratorGroup`*, and set
  `unitResources.cpu: "1"` (supply a CPU *`generalGroup`* only when
  `instance-type-aware-cpu-manufacturer` is enabled).
- **Inactive and empty queues**: A new empty queue starts on `Hold` until its first flavor syncs.
  On a populated pool, admin-driven `Hold` activates only when every `InstanceType` and
  `AcceleratorType` fronting that pool sets `spec.inactive: true`.
- **Drain recovery**: With `instance-type-drain-when-no-flavors=true` (default), a marked migration
  drains before emptying and restores the saved stop policy when nodes return without backfilling
  `spec.inactive`. Outside migration, an external `Hold` on a populated active pool clears to `None`,
  while an external `HoldAndDrain` backfills `spec.inactive: true`. The `InstanceType` stays intact.
- **Deleting a sibling**: Deleting one custom `InstanceType` persists `spec.stop: true` on its own
  `Instance`s. With other active siblings, the shared `ClusterQueue` stays active; with only inactive
  siblings on an active, non-deleting queue outside migration, the controller sets `Hold`; if it was
  the last fronting object on the pool, the controller requests `ClusterQueue` deletion.

---

**See also** — [GPU Instances](_index.md) (overview and unit resource presets) · [Accelerator Requests](../devices/requests.md) (slice and partition request rules) · [Model Artifact](../model-delivery/artifact.md) (cached model weights for `model` volumes)

**Next** → [GPU Instance Operations](operations.md) — stop and start workspaces, isolate tenant networks, and query subresources.
