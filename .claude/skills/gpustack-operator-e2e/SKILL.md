---
name: gpustack-operator-e2e
description: "Run a local end-to-end (E2E) verification of the GPUStack Operator on a reachable local cluster (k3s / docker-desktop): build & load the :dev image, deploy via ytt, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Bash(kubectl get*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(git diff*), Bash(command -v*), Read"
model: sonnet
---

# GPUStack Operator — local E2E verification

Deploy the operator onto a **local** cluster and verify the four-stage scheduling chain end to end:
NFD labels nodes → Device Manager detects accelerators → Worker profiles capacity → four controllers
materialize Kueue `ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue`. See
[architecture.md](../../../docs/architecture.md) for the chain and [development.md](../../../docs/development.md)
for build/package.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context and proceed only after the user confirms it
  is the intended local cluster. If a different context is needed, **stop and ask** — never run
  `kubectl config use-context`.
- Build the image **locally only** — never push (`PACKAGE_PUSH` stays `false`).
- Touch only objects this skill creates (the `ytt` deployment, injected fake labels, test workloads).
  **Never** modify or delete the user's pre-existing namespaces/resources.
- Every mutating step (build/load, deploy, inject, teardown) is confirmed before running.

## When to run

On invocation, detect whether the branch's changes warrant E2E:

```bash
git diff --name-only origin/main...HEAD
```

If any path matches a high-impact surface, ask the user (with `AskUserQuestion`) whether to run E2E,
naming the surface that changed:

| Surface | Path glob |
|---|---|
| Controller reconcile | `pkg/worker/controllers/**` |
| Admission webhooks | `pkg/*/webhooks/**` |
| Extension apiserver | `pkg/worker/extensionapis/**`, `api/**`, `pkg/extensionapi/**` |
| App installation | `pkg/worker/kuberess/**` |

If nothing matches, say so and run only on explicit request.

## Preflight (read-only)

```bash
# Required host tools (helm runs *inside* the operator image, so host helm is informational only).
command -v kubectl ytt docker || echo "missing a required tool"
command -v helm || echo "helm not on host (ok — used inside the image)"

# Show the active context and confirm it is the intended LOCAL cluster before any mutation.
kubectl config current-context
kubectl cluster-info
kubectl get nodes -o wide
```

Confirm with the user that the context is a local `k3s` / `docker-desktop` cluster. Do not continue
otherwise.

## 1. Build & load the image (confirm)

`make package` builds `gpustack/gpustack-operator:dev` for `linux/$(uname -m)`. The operator binary is
recompiled whenever the commit changes — the Dockerfile's `GPUSTACK_GIT_COMMIT` build-arg busts that
layer — so the registry build cache is safe to keep (base layers stay warm, no full rebuild). **Tag
the image uniquely per build**: a fixed `:dev` tag plus `imagePullPolicy: IfNotPresent` lets the
kubelet keep running a previously cached `:dev` even after you rebuild (it matches by name, not
digest); a per-commit tag forces the new image.

```bash
TAG=dev-$(git rev-parse --short HEAD)
PACKAGE_TAG="$TAG" make package   # builds gpustack/gpustack-operator:$TAG
```

Load it into the cluster's runtime:

- **docker-desktop** — the K8s node usually shares the docker image store; no import needed.
- **k3s** (containerd, separate store):

  ```bash
  docker save "gpustack/gpustack-operator:$TAG" | sudo k3s ctr images import -
  ```

If pods later report `ErrImagePull` / `ImagePullBackOff` even though the image is built, the cluster
runtime does not share the docker store — use the explicit import path above for your runtime.

## 2. Deploy the operator (confirm)

```bash
ytt -f deploy/gpustack-operator/ytt | kubectl apply -f -
```

Default namespace is `gpustack-system` (override with `-v namespaceName=...`). The manifest pins
`image: …:dev` with `imagePullPolicy: Always`, so the kubelet would try to pull from a registry and
fail even though the image is loaded locally. **Point it at your per-build tag and use `IfNotPresent`:**

```bash
kubectl -n gpustack-system set image deploy/gpustack-operator-worker main="gpustack/gpustack-operator:$TAG"
kubectl -n gpustack-system patch deployment gpustack-operator-worker --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
```

## 3. Verify the CPU-only chain (mandatory)

All assertions are level-based polling — safe to re-run. On a GPU-less local cluster the
per-manufacturer Device-Manager DaemonSets exist but schedule **zero** pods (their node selector
needs a PCI accelerator label) — that is expected; the mandatory phase covers the general (CPU-only)
chain only.

```bash
NS=gpustack-system

# 1. Operator Deployment becomes Available.
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker --timeout=300s

# 1b. CRITICAL — confirm the RUNNING binary is built from your HEAD, not a stale cached image.
#     The image label can claim HEAD while the embedded binary is an older cached build (see §1).
want=$(git rev-parse HEAD)
got=$(kubectl -n "$NS" exec deploy/gpustack-operator-worker -- gpustack-operator --version 2>/dev/null | grep -oiE '[0-9a-f]{40}')
[ "$want" = "$got" ] && echo "revision OK: $got" \
  || echo "STALE IMAGE: running [$got], expected [$want] — rebuild (commit first) and redeploy with the new TAG"

# 2. Aggregated extension APIs are registered and Available.
kubectl get apiservices v1.gpustack.ai v1.worker.gpustack.ai \
  -o custom-columns=NAME:.metadata.name,AVAILABLE:'.status.conditions[?(@.type=="Available")].status'

# 3. CRDs are established.
kubectl get crd instances.worker.gpustack.ai devices.worker.gpustack.ai

# 4. The operator self-installed NFD and Kueue; wait for their pods to be Ready.
#    (Names/namespaces come from the in-cluster Helm releases — discover, then wait.)
kubectl get pods -A | grep -Ei 'nfd|node-feature|kueue'
#    Soft check: DM DaemonSets exist (0 pods on a GPU-less node is expected).
kubectl get daemonset -A | grep -i device-manager || true

# 5. NFD labeled the node(s) with CPU identity, and marked GPU-less nodes non-acceleratable.
kubectl get nodes -o json | \
  grep -Eo '"feature\.gpustack\.ai/(cpu-[a-z]+|acceleratable)"[^,]*' | sort -u

# 6. The Worker derived general capacity labels (NodeFeature <node>-gpustack-worker).
kubectl get nodefeatures -A -o json | grep -Eo '"general\.feature\.gpustack\.ai/[^"]+"' | sort -u | head

# 7. The four controllers materialized the general chain (names are prefixed gpustack--).
kubectl get resourceflavors.kueue.x-k8s.io -o name | grep 'gpustack--'
kubectl get clusterqueues.kueue.x-k8s.io     -o name | grep 'gpustack--'
kubectl get cohorts.kueue.x-k8s.io           -o name | grep 'gpustack--'
kubectl get localqueues.kueue.x-k8s.io -A     -o name | grep 'gpustack-fnv64-'
```

Each step asserts a concrete object/label. If one is empty, stop and diagnose that stage:

```bash
kubectl -n gpustack-system logs deploy/gpustack-operator-worker --tail=200
kubectl -n gpustack-system describe deploy/gpustack-operator-worker
```

## 4. Instance lifecycle & drain-recycle (run when the change touches the Instance controller/webhook)

`pkg/worker/controllers/worker/instance.go` and `pkg/worker/webhooks/worker/instance.go` are **not**
covered by §3. When a change touches either, verify the **Instance ↔ InstanceType** contract on a real
cluster — the unit tests cannot (blind spot below). Core behavior: when the `InstanceType` a *running*
Instance references is drained — its backing `ClusterQueue` goes `HoldAndDrain` so the InstanceType
reports `Inactive`, or the type is removed — the `InstanceReconciler` must **stop** the Instance
(`spec.stop=true`), *not* recreate its Pod; it may restart only once a live InstanceType exists again.

Why a real cluster is required:

- `InstanceType` is a live projection of a `ClusterQueue` (`instance_type.go`); its `status.phase`
  comes from `apistatus.GetSummaryOfClusterQueue` — `Active` condition `True`→`Active`, `False`→`Inactive`.
- The Instance's Pod carries the Kueue `kueue.x-k8s.io/queue-name` label, so it is admission-managed:
  `HoldAndDrain` evicts the Pod → `pod==nil` → the reconciler re-evaluates and stops the Instance.
- **Unit-test blind spot:** the fake client cannot store the aggregated `InstanceType`, so
  `Get(InstanceType)` returns NotFound and the "Inactive" unit test silently degrades into the
  "type gone" path. The `phase==Inactive` branch is exercised **only** here, on a real cluster.

**Drain injection on a CPU-only cluster (no accelerator needed):** bump the `ram` capacity label in the
Worker-authored `<node>-gpustack-worker` NodeFeature. The node then matches a *new* general profile and
the *old* profile (the one the Instance's InstanceType is built from) drains. This is stable:
`ConstructNodeCapacityLabels` prefers the node's existing capacity label over `Status.Capacity`
(`pkg/nodefeature/helper.go`), and `NodeFeatureReconciler` watches Node-label changes only (not the
NodeFeature), so the edit is not reconciled away.

```bash
NS=gpustack-system; NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
IT=$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[?(@.status.phase=="Active")].metadata.name}' | awk '{print $1}')
echo "active InstanceType: $IT on node $NODE"

# 1. Create a running Instance referencing the Active InstanceType. alpine is kept alive (sleep) so its
#    Kueue Workload holds quota; the ephemeral volume lets non-type validation pass.
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: gpustack-e2e-instance, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

# 2. Assert the Pod is created AND admitted by Kueue (holding quota is what lets HoldAndDrain evict it).
kubectl -n default get pod gpustack-e2e-instance -o jsonpath='{.status.phase}'; echo
kubectl -n default get workloads.kueue.x-k8s.io \
  -o custom-columns=NAME:.metadata.name,ADMITTED:'.status.conditions[?(@.type=="Admitted")].status'
# spec.stop must be unset/false here — the Instance is running and its InstanceType is Active.
kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}'; echo

# 3. Drain: bump the general ram label so the node matches a new profile and the old one drains.
gKey=$(kubectl get node "$NODE" -o json | grep -oE '"general\.feature\.gpustack\.ai/[a-z0-9-]+\.ram"' | head -1 | sed -E 's#.*/(.*)\.ram"#\1#')
old=$(kubectl -n "$NS" get nodefeature "${NODE}-gpustack-worker" -o jsonpath="{.spec.labels.general\.feature\.gpustack\.ai/${gKey}\.ram}")
new=32Gi; [ "$old" = "32Gi" ] && new=24Gi   # any different even Gi value forces a new profile
echo "draining: ram ${old} -> ${new} (gKey=${gKey})"
kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
  -p "{\"spec\":{\"labels\":{\"general.feature.gpustack.ai/${gKey}.ram\":\"${new}\"}}}"
```

Poll the chain, then assert the Instance is **stopped, not recreated**:

```bash
# Old flavor draining, old ClusterQueue HoldAndDrain, old InstanceType Inactive, new profile Active.
kubectl get resourceflavors.kueue.x-k8s.io -o custom-columns=NAME:.metadata.name,DRAIN:'.metadata.annotations.schedule\.gpustack\.ai/drain' | grep gpustack--
kubectl get clusterqueues.kueue.x-k8s.io   -o custom-columns=NAME:.metadata.name,STOP:.spec.stopPolicy | grep gpustack--
kubectl get instancetypes.worker.gpustack.ai -o custom-columns=NAME:.metadata.name,PHASE:.status.phase

# THE assertion: the Instance whose InstanceType is now Inactive gets stopped.
kubectl -n default get instance gpustack-e2e-instance \
  -o custom-columns=STOP:.spec.stop,PHASE:.status.phase    # expect: true / Stopped
kubectl -n default get pod gpustack-e2e-instance || echo "pod evicted (expected)"
# Ground truth in the logs — proves the phase==Inactive branch ran, not some other path:
kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=400 | grep "stop instance as inactive instance type"
```

A buggy `Phase != Inactive` condition would **recreate the Pod instead of stopping the Instance** —
that is the regression this test exists to catch. (A harmless `create service … spec.ports: Required
value` error appears because the test Instance declares no ports; it is unrelated to the drain path.)

Restore when keeping the deployment (skip if doing a full §6 teardown):

```bash
kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
  -p "{\"spec\":{\"labels\":{\"general.feature.gpustack.ai/${gKey}.ram\":\"${old}\"}}}"
kubectl -n default delete instance gpustack-e2e-instance
```

## 5. Optional — simulated accelerator & drain-recycle (accelerated chain)

This exercises the accelerated chain and the drain-recycle behavior (the `ResourceFlavor` tombstone,
`ClusterQueue` `HoldAndDrain`, and `Cohort` reclaim — see [architecture.md](../../../docs/architecture.md)
Stage 4) on a GPU-less cluster **by approximation**. There is no real Device-Manager `Devices` CR or
device-plugin allocation, so this validates the controller/label algebra, not physical device
handling. Run only if the change under test touches the accelerated or drain paths.

1. **Inject** a fake accelerator on one node. The Worker derives accelerated profiles from the
   `acceleratable.feature.gpustack.ai/*` labels that NFD merges onto the node from the DM's
   `<node>-gpustack-device-manager` NodeFeature. The cleanest simulation is to create a NodeFeature
   carrying those labels (manufacturer, `<aKey>`, `.count`, memory/cores) and let NFD merge it.
   **Confirm NFD's label-merge config and the exact label set against `pkg/nodefeature` on first run**
   before relying on this — the precise keys/ownership are what to validate empirically here.

2. **Assert the accelerated chain** appears (names carry the `--<aKey>-<acc>d` segment):

   ```bash
   kubectl get resourceflavors.kueue.x-k8s.io -o name | grep -E 'gpustack--.*--.*-[0-9]+d'
   kubectl get clusterqueues.kueue.x-k8s.io   -o name | grep -E 'gpustack--.*--.*-[0-9]+d'
   ```

3. **Drain**: remove the injected labels/NodeFeature so the profile no longer matches any node, then
   assert (poll — the controllers reconcile asynchronously):

   ```bash
   # ResourceFlavor is NOT deleted — it is a draining, zero-quota tombstone.
   kubectl get resourceflavors.kueue.x-k8s.io -o json | \
     grep -B2 '"schedule.gpustack.ai/drain": *"true"'

   # ClusterQueue switches to HoldAndDrain (removed only after no reservation remains).
   kubectl get clusterqueues.kueue.x-k8s.io \
     -o custom-columns=NAME:.metadata.name,STOP:.spec.stopPolicy | grep HoldAndDrain

   # Cohort is reclaimed only once no node AND no ClusterQueue still reference it.
   kubectl get cohorts.kueue.x-k8s.io -o name | grep 'gpustack--' || echo "cohort reclaimed"
   ```

## 6. Teardown (ask first)

When verification finishes, **ask the user** (with `AskUserQuestion`) whether to clean up or keep the
deployment for inspection.

If cleaning up — uninstall via Helm and remove the operator's objects. The `gpustack-system`
namespace is **kept** (deleting it can hang in `Terminating` on the orphaned aggregated APIServices):

```bash
# §4 test Instance (delete before NodeFeature so its Pod/Workload drain cleanly).
kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found

# Injected fake NodeFeature(s) and test workloads. Deleting the Worker-authored
# <node>-gpustack-worker NodeFeature also discards any §4 ram edit; the operator
# rebuilds it from the Node on the next reconcile.
kubectl -n gpustack-system delete nodefeature --all

# Operator-installed apps (Helm), then their CRDs.
helm uninstall gpustack-csi-driver-nfs -n gpustack-system
helm uninstall gpustack-csi-driver-s3 -n gpustack-system
helm uninstall gpustack-node-feature-discovery -n gpustack-system
kubectl get crd | grep nodefeature | cut -d ' ' -f 1 | xargs -I{} kubectl delete crd {}
helm uninstall gpustack-kueue -n gpustack-system
# Kueue CRs hold a kueue.x-k8s.io/resource-in-use finalizer that only the (now
# uninstalled) controller clears — strip it, or `kubectl delete crd` hangs forever.
for k in resourceflavors clusterqueues cohorts; do
  kubectl get "$k.kueue.x-k8s.io" -o name 2>/dev/null | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}'
done
kubectl get crd | grep kueue | cut -d ' ' -f 1 | xargs -I{} kubectl delete crd {}

# The operator's own workloads, webhooks, CRDs, and aggregated APIServices.
kubectl -n gpustack-system get daemonset  | grep gpustack-operator | cut -d ' ' -f 1 | xargs -I{} kubectl -n gpustack-system delete daemonset {}
kubectl get mutatingwebhookconfigurations   | grep gpustack | cut -d ' ' -f 1 | xargs -I{} kubectl delete mutatingwebhookconfigurations {}
kubectl get validatingwebhookconfigurations | grep gpustack | cut -d ' ' -f 1 | xargs -I{} kubectl delete validatingwebhookconfigurations {}
kubectl -n gpustack-system get deployment | grep gpustack-operator | cut -d ' ' -f 1 | xargs -I{} kubectl -n gpustack-system delete deployment {}
kubectl -n gpustack-system get job        | grep gpustack-operator | cut -d ' ' -f 1 | xargs -I{} kubectl -n gpustack-system delete job {}
kubectl -n gpustack-system get svc        | grep gpustack-operator | cut -d ' ' -f 1 | xargs -I{} kubectl -n gpustack-system delete svc {}
kubectl get crd        | grep gpustack | cut -d ' ' -f 1 | xargs -I{} kubectl delete crd {}
kubectl get apiservice | grep gpustack | cut -d ' ' -f 1 | xargs -I{} kubectl delete apiservice {}
```

Never delete the user's pre-existing namespaces or resources.

## Troubleshooting

- **`ImagePullBackOff` on `gpustack-operator-worker`** — image not in the cluster runtime, or
  `imagePullPolicy` still `Always`. Re-do the load (§1) and the patch (§2).
- **Extension APIService not `Available`** — the aggregated apiserver isn't ready; check the worker
  logs. Startup order matters: controllers start only after the extension APIs report ready
  (see [architecture.md](../../../docs/architecture.md)).
- **Teardown hangs deleting kueue CRDs** — `helm uninstall gpustack-kueue` removes the controller, but
  its ResourceFlavor/ClusterQueue CRs keep the `kueue.x-k8s.io/resource-in-use` finalizer, so `kubectl
  delete crd` waits forever. The teardown strips those finalizers first; if a run predates that fix,
  patch them by hand: `kubectl patch <resourceflavor|clusterqueue>/<name> --type=merge -p '{"metadata":{"finalizers":[]}}'`.
- **No Kueue objects appear** — confirm NFD and Kueue pods are Ready (§3 step 4) and that nodes carry
  the `feature.gpustack.ai/cpu-*` labels (§3 step 5); the chain is driven entirely by those labels.
- **§4 ram edit reverts / old profile never drains** — the patch must land on the
  `<node>-gpustack-worker` NodeFeature (Worker-authored), not on the Node directly (NFD overwrites Node
  labels). Confirm NFD merged it: `kubectl get node <node> -o json | grep '<gKey>.ram'` should show the
  new value. If a new profile never appears, the chosen value matched the old one (must differ and be
  even Gi).
- **§4 Instance not stopped after drain** — check the Pod was actually admitted (held quota) before the
  drain; an unadmitted/finished Workload leaves nothing for `HoldAndDrain` to evict, so `pod==nil` may
  never recur. Keep the container alive (`sleep`). If the Instance's InstanceType went straight to
  *gone* rather than *Inactive*, the `phase==Inactive` branch was skipped — re-run and assert
  `Inactive` is observed while the ClusterQueue is `HoldAndDrain`.
- **Behavior matches old code / `--version` ≠ HEAD (§3 step 1b)** — you're running a stale image.
  Commit your change (the `GPUSTACK_GIT_COMMIT` build-arg recompiles per commit), rebuild with a fresh
  `TAG=dev-$(git rev-parse --short HEAD)`, reload, and redeploy pointing at the new tag. (Symptom seen
  in practice: an orphaned `ResourceFlavor` getting *deleted* instead of marked
  `schedule.gpustack.ai/drain=true` — i.e. pre-drain-recycle behavior.)
