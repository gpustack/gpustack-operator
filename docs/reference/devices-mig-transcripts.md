# NVIDIA MIG Transcripts

Evidence: recorded capture. Verbatim `kubectl` and `nvidia-smi` transcripts recorded on a live
Kubernetes cluster (`node-h100`, eight H100-80GB GPUs, operator defaults) across three MIG
configurations: all-logical (0 partitioned), all-physical (8 partitioned), and mixed (3 of 8
partitioned).

Captured `PT` `onceMaxRequest` values reflect an earlier calculation; current rules report `1` while
any GPU can host a partition, else `0`.

## Contents

- [All-logical configuration](#all-logical-configuration)
- [All-physical configuration](#all-physical-configuration)
- [Mixed configuration](#mixed-configuration)
- [Cleanup](#cleanup)

## All-logical configuration

All eight GPUs offer logical slicing and none offer hardware partitions:

```bash
kubectl get instancetypes
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          8/8 80/80 100/800 0/0      0/0       Active
```

Node allocatable keys (`1024 = 8 × 128` logical slices, `12800k = 8 × 1600k` credits,
`655360 MiB = 8 × 81920`):

```bash
kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
```

Captured output:

```text
{
  "nvidia.com/gpu": "8",
  "nvidia.com/gpu.partitioned": "0",
  "nvidia.com/gpu.shared": "80",
  "nvidia.com/gpu.sliced": "1024",
  "nvidia.com/gpu.sliced.cores-percentage": "102400",
  "nvidia.com/gpu.sliced.memory-mib": "655360",
  "nvidia.com/gpu.sliced.memory-percentage": "800",
  "nvidia.com/gpu.sliced.units": "12800k"
}
```

`Devices` capability and `InstanceType` *`status.detail.slicedDetail`*:

```bash
kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
```

Captured output:

```text
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices [0, 1, 2, 3, 4, 5, 6, 7]; partitioned indices -
  GPU 0 logicalSliced: {"coresPercentageOvercommit": true, "count": 128}
  …                    (GPUs 1-7 identical)
```

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64 -o jsonpath='{.status.detail.slicedDetail}' | jq
```

Captured output:

```text
{
  "logical": { "coresPercentageOvercommit": true, "count": 1024 },
  "physical": {}
}
```

## All-physical configuration

### Enable and detect MIG

Enable MIG mode on all eight GPUs (Hopper requires no GPU reset) and restart the Device Manager
`DaemonSet`:

```bash
# On node-h100 (via SSH), GPUs idle and driver-handle daemons stopped:
for i in $(seq 0 7); do sudo nvidia-smi -i "$i" -mig 1; done
```

Captured output:

```text
Enabled MIG Mode for GPU 00000000:8D:00.0
All done.
…                                                    (GPUs 1-7 identical)
```

```bash
kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
```

Captured output:

```text
daemonset.apps/gpustack-operator-device-manager-nvidia restarted
```

```bash
kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
```

Captured output:

```text
daemon set "gpustack-operator-device-manager-nvidia" successfully rolled out
```

After re-detection, logical views drop to zero and `PT` reports `1/56` (`8 × 7`):

```bash
kubectl get instancetypes
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          0/0 0/0 0/0 1/56           0/0       Active
```

```bash
kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
```

Captured output:

```text
{
  "nvidia.com/gpu": "0",
  "nvidia.com/gpu.partitioned": "56",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "56",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "32",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "24",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "16",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "8",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "8",
  "nvidia.com/gpu.partitioned.units": "12800k",
  "nvidia.com/gpu.shared": "0",
  "nvidia.com/gpu.sliced": "0"
}
```

```bash
kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
```

Captured output:

```text
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices -; partitioned indices [0, 1, 2, 3, 4, 5, 6, 7]
  GPU 0 partitioned: count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  …                  (GPUs 1-7 identical)
```

### Allocate 3g.40gb partition

Submitted `Pod` requesting one `3g.40gb` instance on the pool's `LocalQueue`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mig-demo
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-e4768a65ca0ce96b
spec:
  runtimeClassName: nvidia
  restartPolicy: Never
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - "sleep"
        - "86400"
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"              # one partitioned GPU
          nvidia.com/gpu.partitioned.mig-3g.40gb: "1"  # one 3g.40gb instance on it
```

Running `Pod` and in-container `nvidia-smi -L`:

```bash
kubectl get pod mig-demo
```

Captured output:

```text
NAME       READY   STATUS    RESTARTS   AGE
mig-demo   1/1     Running   0          8s
```

```bash
kubectl exec mig-demo -- nvidia-smi -L
```

Captured output:

```text
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

Post-allocation `InstanceType` and node allocatable keys (one GPU holds `3g.40gb` on slices 0–3):

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          0/0 0/0 0/0 7/52   0/0   Active
```

```bash
kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu\\.partitioned")))'
```

Captured output:

```text
{
  "nvidia.com/gpu.partitioned": "53",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "52",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "30",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "22",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "16",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "7",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "7",
  "nvidia.com/gpu.partitioned.units": "12800k"
}
```

Per-profile sum across seven untouched GPUs plus the carved GPU (`allocated + remaining`):

| Profile | 7 untouched GPUs | Carved GPU | Total |
|---|---|---|---|
| `1g.10gb` | `7 × 7 = 49` | slices 4–7 free → 3 | **52** |
| `1g.20gb` | `7 × 4 = 28` | slots `{4,2}`, `{6,2}` → 2 | **30** |
| `2g.20gb` | `7 × 3 = 21` | slot `{4,2}` → 1 | **22** |
| `3g.40gb` | `7 × 2 = 14` | 1 allocated + 1 remaining | **16** |
| `4g.40gb` | `7 × 1 = 7` | its one slot is taken → 0 | **7** |
| `7g.80gb` | `7 × 1 = 7` | needs all 8 slices → 0 | **7** |

## Mixed configuration

### Disable five GPUs

Disable MIG on GPUs 3–7 so GPUs 0–2 remain partitioned and GPUs 3–7 serve logical/whole-GPU
requests:

```bash
# On node-h100 (via SSH):
for i in 3 4 5 6 7; do sudo nvidia-smi -i "$i" -mig 0; done
kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia

sudo nvidia-smi --query-gpu=index,mig.mode.current --format=csv
```

Captured output:

```text
index, mig.mode.current
0, Enabled
1, Enabled
2, Enabled
3, Disabled
4, Disabled
5, Disabled
6, Disabled
7, Disabled
```

`InstanceType`, node allocatable keys, and `Devices` capability after re-detection:

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          5/5 50/50 100/500 7/21   0/0   Active
```

```bash
kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
```

Captured output:

```text
{
  "nvidia.com/gpu": "5",
  "nvidia.com/gpu.partitioned": "21",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "21",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "12",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "9",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "6",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "3",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "3",
  "nvidia.com/gpu.partitioned.units": "4800k",
  "nvidia.com/gpu.shared": "50",
  "nvidia.com/gpu.sliced": "640",
  "nvidia.com/gpu.sliced.cores-percentage": "64k",
  "nvidia.com/gpu.sliced.memory-mib": "409600",
  "nvidia.com/gpu.sliced.memory-percentage": "500",
  "nvidia.com/gpu.sliced.units": "8M"
}
```

```bash
kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
```

Captured output:

```text
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices [3, 4, 5, 6, 7]; partitioned indices [0, 1, 2]
  GPU 0 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 1 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 2 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 3 logicalSliced:   {"coresPercentageOvercommit": true, "count": 128}
  …                      (GPUs 4-7 identical)
```

### Disjoint workload placement

Submitting one partition request (`mixed-partition`, `3g.40gb`) and one logical-slice request
(`mixed-logical`, 50% slice) places them on disjoint GPU populations:

```bash
kubectl get pods
```

Captured output:

```text
NAME                  READY   STATUS    RESTARTS   AGE
mixed-logical         1/1     Running   0          5s
mixed-partition       1/1     Running   0          9s
```

```bash
kubectl exec mixed-partition -- nvidia-smi -L
```

Captured output:

```text
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

```bash
kubectl exec mixed-logical -- nvidia-smi -L
```

Captured output:

```text
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-4e24fa00-9f4f-68c4-bfcd-581cd3cb7de6)
```

```bash
kubectl get pod <name> -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/accelerator\.allocated}'
```

Captured output:

```text
mixed-partition  -> index=0  id=GPU-950792bf-…  profile=3g.40gb     # a PARTITIONED GPU
mixed-logical    -> index=5  id=GPU-4e24fa00-…  profile=-           # a WHOLE GPU
```

Resulting `InstanceType` (`EX 5→4`, `SH 50→40`, `SL 500→450`, `PT 21→17`) and partitioned keys:

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          4/4 40/40 100/450 7/17   0/0   Active
```

```bash
kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu\\.partitioned")))'
```

Captured output:

```text
{
  "nvidia.com/gpu.partitioned": "18",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "17",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "10",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "7",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "6",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "2",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "2",
  "nvidia.com/gpu.partitioned.units": "4800k"
}
```

## Cleanup

Disable MIG on GPUs 0–2, restart the Device Manager `DaemonSet`, and confirm the pool returns to
all-logical (`8/8 80/80 100/800 0/0`):

```bash
# On node-h100 (via SSH):
for i in 0 1 2; do sudo nvidia-smi -i "$i" -mig 0; done
```

Captured output:

```text
Disabled MIG Mode for GPU 00000000:8D:00.0
All done.
…
```

```bash
kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
```

Captured output:

```text
daemonset.apps/gpustack-operator-device-manager-nvidia restarted
```

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
```

Captured output:

```text
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          8/8 80/80 100/800 0/0   0/0   Active
```

---

**See also** — [NVIDIA MIG Operations](../modules/devices/nvidia-mig.md) (operational runbook and capacity accounting) · [Device Discovery](devices-discovery.md) (`Devices` ledger and partitioned allocation) · [Scheduling Chain](devices-scheduling.md) (Kueue capacity projections)
