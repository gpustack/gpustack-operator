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

## Extension APIs / startup order

- **Extension APIService not `Available`** — the aggregated apiserver isn't ready; check the worker
  logs. Startup order matters: controllers start only after the extension APIs report ready
  (see `docs/architecture.md`).

- **`clusterqueues.kueue.x-k8s.io "…" not found` in worker logs** — benign transient while the
  scheduling chain materializes (a level-based reconcile races ahead of object creation and retries).
  Not an install error; confirm the chain settles (`resourceflavors`/`clusterqueues` appear).

## Teardown

- **Teardown hangs deleting kueue CRDs** — `helm uninstall gpustack-kueue` removes the controller, but
  its ResourceFlavor/ClusterQueue CRs keep the `kueue.x-k8s.io/resource-in-use` finalizer (and
  Instances keep `gpustack.ai/controlled`), so `kubectl delete crd` waits forever. `teardown.sh` strips
  those finalizers first; if a run predates that, patch by hand:
  `kubectl patch <resourceflavor|clusterqueue|instance>/<name> --type=merge -p '{"metadata":{"finalizers":[]}}'`.

- **Leftover sub-releases after `helm uninstall`** — `helm uninstall gpustack-operator` only removes the
  operator release; the worker-installed Kueue/NFD/CSI are separate releases. That is exactly what
  `teardown.sh` removes next.

- The `gpustack-system` namespace is intentionally **kept** — deleting it can hang in `Terminating` on
  the orphaned aggregated APIServices. Never delete the user's pre-existing namespaces or resources.
