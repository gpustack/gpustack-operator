---
name: gpustack-operator-chart-e2e
description: "Verify the GPUStack Operator **Helm chart** end to end on a reachable local cluster (k3s / docker-desktop): build & load the dev image, `helm install` the chart, assert the worker/device-manager roll out, the versions are consistent (image tag ↔ bundled chart tgz ↔ `gpustack-operator --version`), then `helm uninstall` and assert zero leftovers. Proactively offer this when a branch ahead of main changes the chart, the in-cluster app-installation code, or the image build. Examples: \"verify the chart installs and uninstalls cleanly\", \"does helm install of the operator work\", \"check the chart version is right\", \"test my chart change end to end\", \"did my kuberess change break the runtime install\"."
allowed-tools: "Bash(kubectl get*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(helm version*), Bash(helm status*), Bash(helm list*), Bash(helm template*), Bash(helm lint*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Read"
model: sonnet
---

# GPUStack Operator — Helm chart E2E verification

Install the operator **from its Helm chart** onto a **local** cluster and verify it installs, runs, and
uninstalls cleanly — with the **right version** baked in. This is the chart/version counterpart to the
`gpustack-operator-e2e` skill (which exercises the deep NFD → Worker → Kueue scheduling-chain behavior);
keep the deep behavioral assertions there and the install/uninstall + version contract here. See
[development.md](../../../docs/development.md) for build/package and the `make … chart` targets.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context and proceed only after the user confirms it is
  the intended local cluster. Never run `kubectl config use-context`.
- Build the image **locally only** — never push (`PACKAGE_PUSH` stays `false`).
- Touch only objects this skill creates (the Helm release, its cleanup). **Never** modify or delete the
  user's pre-existing namespaces/resources.
- Every mutating step (build/load, install, uninstall/cleanup) is confirmed before running.

## When to run

On invocation, detect whether the branch's changes warrant a chart E2E:

```bash
git diff --name-only origin/main...HEAD
```

If any path matches a high-impact surface, ask the user (with `AskUserQuestion`) whether to run it,
naming the surface that changed:

| Surface | Path glob |
|---|---|
| Helm chart | `deploy/gpustack-operator/chart/**` |
| In-cluster app installation | `pkg/worker/kuberess/**` |
| Image build / chart packaging | `pack/gpustack-operator/Dockerfile`, `hack/package.sh` |

If nothing matches, say so and run only on explicit request.

## Preflight (read-only)

```bash
command -v kubectl helm docker || echo "missing a required tool"   # helm is required on the host here

kubectl config current-context
kubectl cluster-info
kubectl get nodes -o wide
```

Confirm with the user that the context is a local `k3s` / `docker-desktop` cluster. Do not continue
otherwise. Optionally sanity-check the chart offline first: `helm lint deploy/gpustack-operator/chart`.

## 1. Build & load the dev image (confirm)

```bash
TAG=dev-$(git rev-parse --short HEAD)
PACKAGE_TAG="$TAG" make package   # builds gpustack/gpustack-operator:$TAG, never pushes
```

The operator binary is recompiled whenever the commit changes (the Dockerfile's `GPUSTACK_GIT_COMMIT`
build-arg busts that layer). A **per-commit tag** + `imagePullPolicy: IfNotPresent` is what forces the
kubelet to run the new image (a fixed `:dev` tag matches by name, so a stale cached `:dev` would keep
running). Load it into the cluster runtime:

- **docker-desktop** — the node shares the docker image store; no import needed.
- **k3s** (containerd): `docker save "gpustack/gpustack-operator:$TAG" | sudo k3s ctr images import -`

## 2. helm install the chart (confirm)

```bash
NS=gpustack-system
helm install gpustack-operator deploy/gpustack-operator/chart \
  -n "$NS" --create-namespace \
  --set image.tag="$TAG" \
  --set image.pullPolicy=IfNotPresent
```

`image.tag` defaults to `v<Chart.AppVersion>`; pinning it to the per-build `$TAG` is what points the
worker and device-manager at your local image. The chart renders the worker + the per-manufacturer
device-manager DaemonSets and passes `--disable-applications=device-manager` to the worker (so it does
not also install them at runtime). The worker still self-installs Kueue / NFD / CSI at runtime.

To also exercise the gated post-delete cleanup hook in-cluster, add `--set cleanupOnUninstall=true`
(otherwise §4 runs the cleanup script from the host).

**To exercise the device-manager *runtime* install** (the version-critical path — see §3 note), add
`--set deviceManager.enabled=false`. Then the chart does not render the DaemonSets and the worker
instead installs them from the bundled `gpustack-operator-<ver>.tgz` as a **separate** Helm release
`gpustack-operator-device-manager`. With the default `deviceManager.enabled=true` the chart renders the
DaemonSets directly and that runtime release is not created.

## 3. Verify install + version consistency (mandatory)

All assertions are level-based polling — safe to re-run.

```bash
NS=gpustack-system; REL=gpustack-operator; WORKER=deploy/gpustack-operator-worker

# 1. Release is deployed and the worker rolls out.
helm status "$REL" -n "$NS" | grep -i 'STATUS:'
kubectl -n "$NS" rollout status "$WORKER" --timeout=300s
kubectl -n "$NS" get daemonset | grep device-manager || true   # one per manufacturer (0 pods on a GPU-less node is fine)

# 1b. INLINED CHARTS — the worker installs the charts bundled in the image as separate Helm releases.
#     Poll (they install a few seconds after the worker is Ready); each must be STATUS=deployed:
#       gpustack-kueue, gpustack-node-feature-discovery, gpustack-csi-driver-nfs, gpustack-csi-driver-s3
#       + gpustack-operator-device-manager  (only when installed with deviceManager.enabled=false)
helm list -n "$NS"
kubectl -n "$NS" logs "$WORKER" | grep 'installed: deployed'   # one line per inlined release, no install errors
kubectl -n "$NS" get pods | grep -Ei 'kueue|node-feature|csi'  # backing pods Running

# 2. CRDs established and aggregated APIs Available.
kubectl get crd instances.worker.gpustack.ai devices.worker.gpustack.ai
kubectl get apiservices v1.gpustack.ai v1.worker.gpustack.ai \
  -o custom-columns=NAME:.metadata.name,AVAILABLE:'.status.conditions[?(@.type=="Available")].status'

# 3. VERSION CONSISTENCY — the three views must agree.
#    (a) the running binary is built from HEAD (not a stale cached image):
want=$(git rev-parse HEAD)
got=$(kubectl -n "$NS" exec "$WORKER" -- gpustack-operator --version 2>/dev/null | grep -oiE '[0-9a-f]{40}')
[ "$want" = "$got" ] && echo "revision OK: $got" || echo "STALE IMAGE: running [$got] != HEAD [$want]"

#    (b) the chart tgz bundled in the image matches the binary version. The Dockerfile derives the
#        tgz version from `gpustack-operator --version` (strip "v", else 0.0.0); they must be equal.
ver=$(kubectl -n "$NS" exec "$WORKER" -- gpustack-operator --version 2>/dev/null \
        | awk '{for (i=1;i<NF;i++) if ($i=="version") print $(i+1)}')
ver=${ver#v}; [[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || ver=0.0.0
tgz=$(kubectl -n "$NS" exec "$WORKER" -- sh -c 'ls /etc/gpustack/charts/gpustack-operator-*.tgz' 2>/dev/null \
        | sed -E 's#.*/gpustack-operator-(.*)\.tgz#\1#')
[ "$ver" = "$tgz" ] && echo "chart version OK: $tgz" \
  || echo "VERSION MISMATCH: binary [$ver] != bundled chart tgz [$tgz] — see Dockerfile packaging / build cache"

#    (c) the deployed image tag is the one you built.
kubectl -n "$NS" get "$WORKER" -o jsonpath='{.spec.template.spec.containers[0].image}'; echo
```

Step 3b is the core chart-version check: it is exactly the tgz the worker would `helm install` for the
device-manager at runtime (`deviceManagerChartVersion()` in `pkg/worker/kuberess/apps_gpustack_device_manager.go`
mirrors the same strip-`v`/`0.0.0` logic), so a mismatch here is a release/cache bug, not a cosmetic one.

If any step is empty/false, stop and diagnose:

```bash
kubectl -n gpustack-system logs deploy/gpustack-operator-worker --tail=200
kubectl -n gpustack-system describe deploy/gpustack-operator-worker
```

## 4. Uninstall & assert zero leftovers (confirm)

```bash
NS=gpustack-system
helm uninstall gpustack-operator -n "$NS"          # waits for the post-delete hook if cleanupOnUninstall=true

# Remove what helm uninstall leaves behind (worker-installed sub-releases, their CRDs,
# finalizers, runtime-registered APIServices/webhooks). Single source of truth, also used by the hook.
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"
```

Then assert the cluster is clean (each should be empty):

```bash
helm list -n gpustack-system | grep -E 'gpustack|kueue|nfd|node-feature|csi' || echo "no leftover releases"
kubectl get crd | grep -E 'gpustack\.ai|kueue\.x-k8s\.io|nfd\.k8s\.io' || echo "no leftover CRDs"
kubectl get apiservice | grep gpustack || echo "no leftover apiservices"
kubectl get clusterrolebinding | grep gpustack || echo "no leftover bindings"
```

The `gpustack-system` namespace is **kept** (deleting it can hang on orphaned aggregated APIServices).
Never delete the user's pre-existing namespaces or resources.

## 5. Optional — mock a release to verify the version/cache path

The chart version is only as trustworthy as the build. Confirm a release version actually reaches the
binary **and** the bundled chart tgz, even with a warm build cache (the risk: the builder cache key
tracks the commit, so re-building an already-built commit with a *different* version could otherwise
serve a stale binary stamped `dev` and a chart packaged `0.0.0`). `make package` passes the resolved
version as the `GPUSTACK_GIT_VERSION` build-arg, which the builder both stamps and folds into its cache
key — so a version change forces a rebuild.

```bash
# §1 already built once with the cache warm. Now force a release-like version and rebuild:
VERSION=v9.9.9 PACKAGE_TAG=dev-rel make package
docker run --rm gpustack/gpustack-operator:dev-rel gpustack-operator --version   # expect: ... v9.9.9 ...
docker run --rm gpustack/gpustack-operator:dev-rel ls /etc/gpustack/charts/      # expect: gpustack-operator-9.9.9.tgz
```

If `--version` shows `dev` or the tgz is `gpustack-operator-0.0.0.tgz`, the build cache served a stale
binary — the version did not bust the cache key. The realistic release trigger is a git tag rather than
`VERSION=`; on a **clean** tree `git tag v9.9.9 && make package` (then `git tag -d v9.9.9`) reproduces
the same path, since the build derives the version from `git tag -l --contains HEAD`.

## Troubleshooting

- **`ImagePullBackOff`** — the dev image isn't in the cluster runtime, or `image.pullPolicy` isn't
  `IfNotPresent`. Re-load the image (§1) and reinstall with `--set image.pullPolicy=IfNotPresent`.
- **`--version` ≠ HEAD (§3 step 3a)** — stale image. Commit first (the `GPUSTACK_GIT_COMMIT` build-arg
  recompiles per commit), rebuild with a fresh `TAG`, reload, and `helm upgrade`/reinstall.
- **Version mismatch (§3 step 3b)** — the bundled tgz version disagrees with the binary. Run §5 to
  reproduce against a mock tag; if it persists with a warm cache, it is the build-cache/version issue.
  Its concrete runtime symptom: with `deviceManager.enabled=false`, the worker's runtime
  device-manager install **fails to find `gpustack-operator-<ver>.tgz`** (the version it computes via
  `deviceManagerChartVersion()` no longer matches the packaged tgz). A healthy `gpustack-operator-device-manager`
  release in `helm list` confirms the version is consistent end to end.
- **`clusterqueues.kueue.x-k8s.io "…" not found` in worker logs** — benign transient while the
  scheduling chain materializes (a level-based reconcile races ahead of object creation and retries).
  It is not an install error; confirm the chain settles (`resourceflavors`/`clusterqueues` appear).
- **`helm uninstall` hangs / CRDs stuck `Terminating`** — Kueue CRs hold a
  `kueue.x-k8s.io/resource-in-use` finalizer (and Instances `gpustack.ai/controlled`) that only their
  now-removed controllers clear. `cleanup.sh` strips them; re-run it, or patch by hand:
  `kubectl patch <res>/<name> --type=merge -p '{"metadata":{"finalizers":[]}}'`.
- **Leftover sub-releases after uninstall** — `helm uninstall gpustack-operator` only removes the
  operator release; the worker-installed Kueue/NFD/CSI are separate releases. That is exactly what
  `cleanup.sh` (or the `cleanupOnUninstall=true` hook) removes.
