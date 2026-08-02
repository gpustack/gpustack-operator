# E2E troubleshooting (shared)

Common failure modes for both `gpustack-operator-e2e` and `gpustack-operator-chart-e2e`.
Skill-specific symptoms live in each skill's own `references/`.

## Image / rollout

- **`ImagePullBackOff` on `gpustack-operator-worker`** — the dev image isn't in the cluster runtime,
  or `image.pullPolicy` is still `Always`. Re-run `build-load.sh <TAG>` and reinstall with
  `--set image.tag=<TAG> --set image.pullPolicy=IfNotPresent`. On k3s the node uses containerd's own
  store, so `docker save … | sudo k3s ctr images import -` is required (build-load.sh does this when
  it detects a k3s context).

- **Behavior matches old code / `--version` ≠ HEAD** — you're running a stale image. The image label
  can claim HEAD while the embedded binary is an older cached build. Commit your change first (the
  Dockerfile's `GPUSTACK_GIT_COMMIT` build-arg recompiles per commit), then rebuild with a fresh
  `TAG=dev-$(git rev-parse --short HEAD)`, reload, and redeploy pointing at the new tag.

## Cluster capability / accelerator detection

`nvidia.com/gpu` (and `.sliced` / `.shared`) **allocatable is advertised by the GPUStack DeviceManager
itself — it IS the device plugin, doing the same job as the vendor's.** So it is a valid "is an
accelerator schedulable on this node" signal, with one caveat about timing:

- It appears only once the node's `gpustack-operator-device-manager-<vendor>` DaemonSet pod is
  **Running/Ready** and has registered. It is legitimately absent on a real GPU node when the operator is
  not installed yet, or while that pod is `Init`/`CrashLoopBackOff`/not-Ready. **Do not conclude
  "GPU-less" from an empty `nvidia.com/gpu` that you read before the operator (or its device-manager) was
  up.** That was the actual mis-call once: a *pre-deploy* `kubectl get nodes …:.status.allocatable.nvidia\.com/gpu`
  showed `<none>` (correct — no plugin yet) and got carried forward as "this cluster has no GPUs", when
  the nodes were g5/g4dn cards all along.

- The **hardware-presence** signal that does NOT depend on the device-plugin being up is NFD's
  `acceleratable.feature.gpustack.ai/<manufacturer>[-<id>]=true` node labels (NFD PCI-vendor detection
  runs regardless of the DeviceManager). Use those for "does this node have accelerator hardware"; use
  `nvidia.com/gpu` allocatable (with the device-manager Ready) for "is it schedulable now". The Devices
  ledger (`kubectl get devices.worker.gpustack.ai`) and accelerated ResourceFlavors (`gpustack--…-Nd`)
  are operator-derived signals too.

  ```
  kubectl get nodes -o json | python3 -c "
  import json,sys
  for n in json.load(sys.stdin)['items']:
      L=n['metadata']['labels']; a=n.get('status',{}).get('allocatable',{}); name=n['metadata']['name']
      hw=[k.split('/')[-1] for k in L if k.startswith('acceleratable.feature.gpustack.ai/') and '.' not in k.split('/')[-1]]
      sched=[k for k in a if k.endswith('/gpu') or k.endswith('/npu')]
      print(name, L.get('node.kubernetes.io/instance-type','?'), 'hw=', hw or '-', 'schedulable=', sched or '-')"
  ```

- **Before calling an operator-owned ResourceFlavor/ClusterQueue an "orphan", enumerate the live nodes'
  gpustack feature keys and check the object's `creationTimestamp`.** An RF/CQ whose `${gKey}`/`${aKey}`
  matches a live node's `general.`/`acceleratable.` labels is a legitimate pool, not a leftover — a
  heterogeneous cluster (a CPU node plus g5/g4dn GPU nodes) legitimately yields several CPU keys and
  several accelerator pools, so pool names that differ from the first node you looked at are expected. A
  `creationTimestamp` right after your own `helm install` also rules out "prior run" residue.

## Remote cluster / kubectl

- **A case stalls with no output, then a later step fails for an unrelated reason** — on a remote
  cluster (public API endpoint plus an exec-credential plugin) a jittering connection does not make
  `kubectl` fail, it makes it **hang**: the credential plugin re-execs on token expiry and the call
  never returns. A 3-second poll inside a case becomes a multi-minute stall, and the case that
  eventually times out is rarely the one that was actually wrong. Locally this never happens, so no
  case guards against it.

  Put the shim first on `PATH` for the whole run — it bounds every ordinary call with
  `--request-timeout` (so a stall becomes a fast error the case's own poll loop retries) and retries
  a read-only verb on a transport failure:

  ```bash
  PATH="$(pwd)/.claude/skills/_e2e-lib/scripts/kubectl-shim:$PATH"
  ```

  It deliberately does **not** retry a mutation (a re-sent create/delete turns a blip into an
  `AlreadyExists`), does not retry a `NotFound` (that is a real answer, and the poll loops asking for
  one are the suite's hottest calls), and leaves streaming or waiting calls — `exec`, `logs -f`,
  `wait`, `delete`, `port-forward` — completely untouched. Knobs: `E2E_KUBECTL_TIMEOUT` (30s),
  `E2E_KUBECTL_RETRIES` (2), `E2E_KUBECTL_BACKOFF` (2).

- **Everything lands on the wrong cluster** — the run was pinned to a context that is not the current
  one and something dropped the pin. Do not fix it by switching the user's current context; the pin is
  the `KUBECONFIG=<path>` from `kube-context.sh` and it has to be on **every** command (each tool call
  is a fresh shell). It composes with the shim above:

  ```bash
  KUBECONFIG=<path> PATH="$(pwd)/.claude/skills/_e2e-lib/scripts/kubectl-shim:$PATH" \
    bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh <NS>
  ```

## Extension APIs / startup order

- **Extension APIService not `Available`** — the aggregated apiserver isn't ready; check the worker
  logs. Startup order matters: controllers start only after the extension APIs report ready
  (see `docs/architecture/internals.md`, *Worker startup order matters*).

- **`clusterqueues.kueue.x-k8s.io "…" not found` in worker logs** — benign transient while the
  scheduling chain materializes (a level-based reconcile races ahead of object creation and retries).
  Not an install error; confirm the chain settles (`resourceflavors`/`clusterqueues` appear).

- **Worker exits with `install applications: ... invalid ownership metadata`** — something asked the
  worker to install applications while a chart release already deploys it. Both installs render the
  same chart, so their objects overlap and Helm refuses the worker's install; the object it names is
  whichever shared one it maps first (a ServiceAccount, typically). Even handing back only
  `device-manager` overlaps, because the cluster-scoped `gpustack-cpu-info` NodeFeatureRule has no
  switch and is in both renders. The two install modes are exclusive: keep
  `worker.disableApplications: ["*"]` wherever this chart deploys the worker, and stand image mode up
  on a cluster with no chart release (chart-e2e CASE 6).

## Component log verbosity

- **The log says nothing about the decision you are chasing** — an empty log is not evidence the code
  did not run. Several paths log above the verbosity the deployment runs, and the device plugin is the
  sharpest case: its `ResourceServer`s are built with `Logger: logger.V(3)`
  (`pkg/devicemanager/allocator/allocator.go`) while the DaemonSet runs `-v=2`, so **every** `Allocate`
  and `GetPreferredAllocation` decision — which card a slice landed on, and why — is discarded at the
  default verbosity. That is exactly what hid a per-card placement bug from a whole e2e run.

  Every component registers `PUT /debug/flags/v` on its own secure port, so verbosity can be raised
  and dropped again without restarting the pod. The device-manager's port is 32443, and its
  DaemonSets are per-manufacturer:

  ```bash
  DM=$(kubectl -n "$NS" get pod -o name | grep device-manager-nvidia | head -1)
  # raise
  kubectl -n "$NS" exec "$DM" -- curl -sk -X PUT -H "Host: 127.0.0.1" -d '4' https://127.0.0.1:32443/debug/flags/v
  # → successfully set klog.logging.verbosity to 4
  #
  # ... now create the workload you want to trace, then read the log ...
  #
  # revert to the DaemonSet's own -v
  kubectl -n "$NS" exec "$DM" -- curl -sk -X PUT -H "Host: 127.0.0.1" -d '2' https://127.0.0.1:32443/debug/flags/v
  ```

  **`-H "Host: 127.0.0.1"` is mandatory.** `httpx.LoopbackAccessHandlerFunc` compares `r.Host` against
  the bare strings `127.0.0.1` / `localhost` / `::1`, and an ordinary request carries the port
  (`127.0.0.1:32443`), so without the override the guard rejects it with a plain **404** that reads
  like a missing route. With the header but a `GET`, the handler answers `406 unsupported http method`
  — that is the guard passing, i.e. proof the route is there.

  **Raise it before creating the workload**, and revert when done. These lines fire only on an
  allocation, so a quiet window after the fact proves nothing, and leaving `v=4` on floods the log for
  every later case.

  `docs/development.md` (*Runtime log verbosity*) is the developer-facing version, with the code
  references for each component's registration and port.

## Teardown

- **Teardown hangs deleting kueue CRDs** — the uninstall removes Kueue's controller (it is a subchart
  of the operator release now, so one uninstall takes both), but its ResourceFlavor/ClusterQueue CRs
  keep the `kueue.x-k8s.io/resource-in-use` finalizer (and Instances keep `gpustack.ai/controlled`),
  so `kubectl delete crd` waits forever. `teardown.sh` strips those finalizers first; if a run
  predates that, patch by hand:
  `kubectl patch <resourceflavor|clusterqueue|instance>/<name> --type=merge -p '{"metadata":{"finalizers":[]}}'`.

- **Leftovers after `helm uninstall`** — the release takes Kueue / NFD / the CSI drivers with it, but
  not: a release the worker installed from inside its own image, the pre-subchart per-application
  releases, the Secrets nothing owns (the worker's cert cache, `gpustack-settings`), or the objects a
  *failed* migration hook keeps on purpose. That is exactly what `teardown.sh` removes next.

- The `gpustack-system` namespace is intentionally **kept** — deleting it can hang in `Terminating` on
  the orphaned aggregated APIServices. Never delete the user's pre-existing namespaces or resources.
