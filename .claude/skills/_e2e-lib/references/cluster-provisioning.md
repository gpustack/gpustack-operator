# Cluster provisioning (shared)

How a run gets the Kubernetes cluster it verifies against, when it is allowed to create one, what it
must do to the nodes before deploying, and how it gives the cluster back. Read this only when Phase 0
finds no usable existing cluster — **a cluster the user already has is always the cheaper, faster,
default choice.**

The lead owns every decision here. The scripts (`provision.sh`, `destroy.sh`, `cluster-auth.sh`) are
mechanics; they never prompt.

## Decision order

1. **Bring-your-own first.** `preflight.sh` shows the active context; `kubectl config get-contexts`
   enumerates the rest. Offer the user their own contexts and stop there if one is reachable and
   suitable. No provisioning, no cost, no destroy obligation.
2. **Provisioning is a separate, explicit opt-in.** Never fold "shall I provision one?" into the
   context confirmation, and never provision because no context happened to be reachable. Two
   confirmations: *(a)* provision at all, *(b)* which modality, asked **after** the user has been told
   what that modality costs.
3. **What this run provisions, this run destroys.** Record the fact in `run-log.md` the moment
   `provision.sh` succeeds (see *Recording*), because it is the single obligation that must survive a
   context compaction.
4. **What the user brought is never destroyed.** For a user's cluster the whole teardown is the
   in-cluster `teardown.sh`; `destroy.sh` must not run.

## Modalities

Modules live in `testing/infra/clusters/<modality>`; each has a `README.md` (prerequisites, variables,
outputs) and a `variables.tf` — **read the module's own README before provisioning it**, it is
normative and this table is only the triage summary.

| Modality | What it provisions | Cost | Cloud CLI it needs (at plan, apply **and destroy**) |
|---|---|---|---|
| `k3s` | K3s over SSH onto servers **you already own** (embedded etcd, containerd runtime classes from each node's `daemon.json`) | **cheap** — no cloud bill; you are already paying for the servers | none; `ssh` + passwordless sudo on each node |
| `eks` | A managed cluster + VPC + a CPU node group and per-key GPU node groups (GPU groups start scaled to zero), addons, kubeconfig merged after apply | **real money per hour**, and the VPC/NAT bills even while GPU groups are at zero | `aws` (`aws sts get-caller-identity` must pass) |
| `nebius` | A managed cluster + network/subnet/security group + a CPU node group and per-key GPU node groups, kubeconfig merged after apply | **real money per hour**, GPU nodes are the expensive part | `nebius` + `jq` (a live compatibility-matrix call, see below) |

State the cost asymmetry to the user in those terms before asking which modality. "It is a few
commands either way" is not the point — one of these choices starts an hourly bill on accelerator
hardware.

## Hard-won lessons (do not rediscover these)

- **The cloud credential is a teardown dependency, not just a create dependency.** A module can resolve
  inputs from a **live cloud API call at plan, apply AND destroy time** (the `nebius` module resolves
  each GPU group's `os`/`drivers_preset` from the compatibility matrix through a `data.external`, so
  the CLI and `jq` must be authenticated and on `PATH` for `terraform destroy` too). An expired
  credential therefore fails the destroy and **strands paid hardware**. Run `cluster-auth.sh <modality>`
  **before provisioning and again before teardown**; `provision.sh` and `destroy.sh` both refuse to run
  when it fails. A long run outlives a short-lived token — assume it expired and re-check.
- **The Kubernetes minor version can be constrained by GPU driver-preset availability, not by the
  operator.** A managed modality only offers certain driver presets per Kubernetes version, and older
  presets are dropped as new versions land. Pick `release` from what the matrix offers for the GPU
  platform you want, then note in the report that the version was chosen by the *driver* constraint —
  otherwise the next run reads it as an operator requirement.
- **A managed GPU node may already run a vendor device plugin as a static Pod.** See *Node preparation*.
- **A remote cluster makes `kubectl` hang, not fail.** A public API endpoint plus an exec-credential
  plugin jitters, and a stalled call inside a case's poll loop costs minutes and blames the wrong step.
  Put `scripts/kubectl-shim` first on `PATH` for the whole run — see *Remote cluster / kubectl* in
  `troubleshooting.md`.
- **A managed provider health-checks the GPUs and will shut the node down under you — putting a card
  into a partitioning mode is enough to trip it.** A multi-GPU node's health check probes the NVLink
  topology matrix and expects `N × (N-1)` healthy pairs. A card in a partitioning mode drops out of that
  matrix, the probe reads short, the node condition flips, and the provider's agent **cordons and then
  shuts the node down**:

  ```
  NebiusGPUError = True   reason GPUHealthCheckFailedMK8S
    {"errors":[{"nvidia_smi_topo.status":"found OK count: 49, expected: 56"}]}   # 8 cards: 7x7 vs 8x7
  taints: node.kubernetes.io/unschedulable | node.cloudprovider.kubernetes.io/shutdown
          | node.kubernetes.io/unreachable:NoExecute
  ```

  It is a **sampling race, not a cumulative effect**: many partition cases can toggle MIG for an hour
  without incident and then one gets caught, because the check simply has to sample while a card is
  partitioned. So it cannot be avoided by being quick — only survived. Practical consequences:

  - **Expect a spurious case failure with no product meaning.** The first symptom is usually unrelated
    Pods failing to be created at all — `failed calling webhook "mpod.kb.io": … connection refused`,
    because the Kueue controller is being evicted off the dying node. A case that never created its Pod
    reports a hold with an **empty reason**; treat that shape as environmental until proven otherwise,
    and re-run rather than triage it as a defect.
  - **Auto-remediation usually returns the node** (here: instance `STARTING`, Ready and self-uncordoned
    about 7 minutes later). Wait for `Ready=True` *and* `.spec.unschedulable` empty before resuming.
  - **Write results to disk per step**, so a shutdown costs only the step in flight.
  - Check the node conditions **before** blaming the operator whenever several cases fail together.
- **Node preparation does not survive a node restart.** A provider-triggered reboot brings back anything
  merely *stopped* — use `systemctl disable --now`, not `stop`, and **re-verify the prep after any
  restart** rather than assuming it held. Filesystem changes (a moved static-Pod manifest) do survive.
- **A remote cluster cannot take a locally loaded image.** `build-load.sh` only works where the node
  shares the host's image store (docker-desktop) or the image can be imported into its containerd (a
  local k3s). For a provisioned cloud cluster, Phase 3 must **package and push** to a registry the nodes
  can pull from and deploy with `--set image.repository=… --set image.pullPolicy=Always` — the image-ref
  ↔ chart-values contract is in `../../gpustack-operator-e2e/references/packaged-image-deploy.md`.
  Same-tag rebuilds must be re-pinned by `@sha256:` digest, or the kubelet keeps the cached content.

## Node preparation (named step, before deploying the operator)

A managed GPU node frequently ships accelerator tooling the operator must not compete with. Neither of
these is removable with `kubectl delete` — a static Pod is owned by the node's kubelet, and the
monitoring service is a host unit:

- **A vendor device plugin as a static Pod** under `/etc/kubernetes/manifests/`. Its mirror Pod
  reappears immediately when deleted through the API, and it advertises the same accelerator resource
  the GPUStack device plugin does — two advertisers of one resource make every card-level assertion in
  the suite meaningless.
- **A DCGM (or equivalent vendor telemetry) service** holding open driver handles, which blocks a MIG
  mode switch — the hardware-partition cases cannot toggle the card while it runs.

Disable both **before** Phase 3 deploys the operator, over the node's SSH address supplied inline at run
time (never written into a file, same idiom as `MIG_NODE_SSH`):

```bash
ssh <node> 'sudo ls -1 /etc/kubernetes/manifests/'
ssh <node> 'sudo mkdir -p /etc/kubernetes/manifests.disabled && \
            sudo mv /etc/kubernetes/manifests/<plugin>.yaml /etc/kubernetes/manifests.disabled/'
ssh <node> 'sudo systemctl disable --now <dcgm-service>'
# confirm the mirror Pod is gone and the resource is no longer double-advertised
kubectl get pods -A -o wide | grep <node>
kubectl get node <node> -o jsonpath='{.status.capacity}'
```

Record every mutation in the report as an **environment mutation not yet restored**. Restoration rule:

- **Provisioned cluster** — the destroy removes the node; no restore needed, but keep it recorded so a
  run that ends without destroying still knows what it changed.
- **User's cluster** — restoring is **mandatory** and belongs in Phase 8 next to the in-cluster
  teardown: move the manifest back, re-enable the service, confirm the mirror Pod returns.

## Recording (reproducibility without secrets)

Write into `run-log.md` at provisioning time, and keep it current:

- modality + module path, and the **`terraform` inputs that shape the cluster** — `release`, the CPU and
  GPU instance-type maps, boot disk size/type, and why that `release` was chosen;
- the resulting kube **context name**, and the flag **`provisioned_by_this_run: yes`**;
- node preparation performed, and whether it is restored;
- the destroy command that discharges the obligation.

**Never** write into any file: host addresses, IPs, SSH targets, cloud project/account ids, or key
paths that identify a host. Those are passed inline to the live command only; in the report they are
`<addr>` / `<project-id>` placeholders. A reader with the same credentials can reproduce the run from
the recorded inputs plus their own ids.

## Mechanics

```bash
# read-only, before provisioning AND again before destroying
bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh <modality>

# mutating + billable — confirmed twice by the lead; extra args go to plan and apply
bash .claude/skills/_e2e-lib/scripts/provision.sh <modality> -var='<k>=<v>' ...

# mutating + irreversible — only for a cluster THIS run provisioned
bash .claude/skills/_e2e-lib/scripts/destroy.sh <modality>
```

- `provision.sh` runs `init` → `plan` → `apply` and prints the resulting context. Each module merges
  its cluster into `~/.kube/config` as a new context (k3s also makes it current). **That is the one
  legitimate context change in a run** — the "never switch context" rule forbids moving among the
  *user's* contexts, not adopting the one this run just created.
- `destroy.sh` needs no `-var` (each module reuses the last successful apply's inputs from an
  auto-loaded snapshot) and **verifies the state is empty afterwards**; a non-empty state exits
  non-zero and means the cluster may still be running and billing. Re-run it until state is empty —
  the run is not over before then.
