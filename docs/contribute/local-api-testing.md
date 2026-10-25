# Local API Testing

A disposable kind cluster on the Mac proves that a control plane can reach a Mac-hosted worker.
The recipe creates one cluster, one kubeconfig and one node image, and removes only what it
created.

## Contents

- [Scope](#scope)
- [Prerequisites](#prerequisites)
- [Commands](#commands)
- [Cluster shape](#cluster-shape)
- [Network probe](#network-probe)
- [Isolation](#isolation)
- [Cleanup](#cleanup)
- [Tests](#tests)
- [Limitations](#limitations)

## Scope

The recipe proves three things.

- A disposable cluster runs the pinned Kubernetes version on native arm64.
- The control plane enables aggregator routing and excludes the EndpointSlice mirroring controller.
- A concrete Mac address answers a plain HTTP request from inside the control-plane container.

It does not prove worker aggregation. The authenticated worker, the aggregation request and the
serving certificate belong to a later stage of this upgrade, and a later stage adds the command
that exercises them to the same script.

## Prerequisites

- macOS on arm64. The recipe refuses any other host.
- Docker Engine serving `linux/arm64`.
- kind v0.33.0 or later. The build needs the `--type` and `--base-image` flags.
- `curl` and `python3` on the host.

The recipe downloads its own kubectl and builds its own node image, so it needs network access to
`dl.k8s.io` and to the container registry.

## Commands

Run the commands in order from the repository root.

```sh
bash testing/infra/local_worker/run.sh preflight
bash testing/infra/local_worker/run.sh prepare
bash testing/infra/local_worker/run.sh network
bash testing/infra/local_worker/run.sh cleanup
```

`preflight` checks the host, the tools, the cluster name and the node image situation, and it
creates nothing except the task-local kubectl. `prepare` builds or reuses the node image, creates
the cluster and proves its shape. `network` proves the reachability. `cleanup` removes the cluster
and the files.

Every command accepts these environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `LOCAL_WORKER_CLUSTER_NAME` | `gpustack-local-worker` | The kind cluster name. |
| `LOCAL_WORKER_NAMESPACE` | `gpustack-local-worker-check` | The namespace the mirroring fixture is created in. |
| `LOCAL_WORKER_STATE_DIR` | `.dist/local_worker` | The kubeconfig, the kubectl and the ownership record. |
| `LOCAL_WORKER_NODE_IMAGE` | `gpustack-upgrade-node:v1.36.5-arm64` | The node image tag. |
| `LOCAL_WORKER_BASE_IMAGE` | `kindest/base:v20260820-69b56db7` | The base image the node image is built on. |
| `LOCAL_WORKER_RELEASE_TARBALL` | `.dist/local-worker-cache/kubernetes-server-linux-arm64.tar.gz` | The release tarball cache, outside the state directory. |

The recipe pins the cluster and its kubectl to `v1.36.5`. `LOCAL_WORKER_KUBERNETES_VERSION` and
`LOCAL_WORKER_KUBECTL_VERSION` are those two pins. The base image, the release tarball and the check
that the cluster reports the pinned version name the same version, so a different value leaves a
cluster that no longer proves what this page documents.

## Cluster shape

The cluster is one control-plane node. The kubeadm arguments are name and value lists, the shape
the `kubeadm.k8s.io/v1beta4` API expects.

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: enable-aggregator-routing
      value: "true"
controllerManager:
  extraArgs:
    - name: controllers
      value: "*,-endpointslicemirroring"
```

The `*` prefix keeps every other controller. The exclusion token is the alias the controller
manager registers for the mirroring controller, and the recipe reads it back from the rendered
static Pod instead of trusting the configuration.

| Setting | Value | Where the recipe reads it |
|---|---|---|
| Kubernetes version | `v1.36.5` | The version the server reports. |
| Node architecture | `arm64` | The node status. |
| Aggregator routing | `enable-aggregator-routing=true` | The `kube-apiserver` static Pod command and args. |
| EndpointSlice mirroring | `controllers=*,-endpointslicemirroring` | The `kube-controller-manager` static Pod command and args. |

Both static Pods must also be running. A component that rejects an argument exits, and a
component that is not running mirrors nothing, so a missing slice would otherwise pass for the
wrong reason.

The exclusion is then observed rather than assumed. The recipe creates a Service without a
selector and a core `Endpoints` object without the skip-mirror label, which are the conditions the
mirroring controller acts on, and waits a bounded window for a mirrored `EndpointSlice` that must
not appear.

A second Service carries a selector, so the `EndpointSlice` controller owns its slices; its slice
has to appear in the same window. That control shows that `EndpointSlice` creation and
observation work in this cluster, so the absent slice is the excluded controller and not a dead
fixture.

A read of the slices that fails ends the observation and fails the command. A failed read is never
counted as an absent slice, because the absence would rest on a read that never answered.

## Network probe

The probe is deliberately plain. The recipe resolves `host.docker.internal` inside the node,
binds a temporary listener on the Mac, and asks the control-plane container for a unique token
over plain HTTP.

```sh
curl --fail --silent --show-error --max-time 15 http://<address>:<port>/
```

The listener serves a token generated for that run and answers requests until the command ends
or a two-minute deadline passes, so a stray connection cannot consume the probe. The recipe
records the address it used and removes the listener and its files when the command ends.

No certificate, no subject alternative name and no identity are involved. The probe therefore
proves reachability only. A serving certificate that carries the address, an authenticated worker
and an aggregation request are the boundary a later stage owns.

## Isolation

Every kubectl and kind invocation names the task-local kubeconfig and the task-local context. The
recipe exports `KUBECONFIG` to the same path and refuses to run when that value points outside the
state directory.

The default kubeconfig is never written. The recipe records its fingerprint when it creates the
cluster and compares it again afterwards.

The cluster name is unique to the run. `prepare` refuses to continue when a cluster already holds
that name and the recipe has no record of creating it, so a foreign cluster is never adopted.

The ownership record in the state directory holds the cluster name, the immutable identifier of
the node container, the kubeconfig path, the node image identifier and the fingerprint of the
default kubeconfig.

## Cleanup

`cleanup` removes the cluster only when the ownership record is complete, the record names the
cluster this run asked about, and the live node container carries the recorded identifier. A
record that is missing, damaged or names another cluster stops the removal instead of being read
as an absent record, and so does a live node container that does not match.

The fingerprint of the default kubeconfig is compared again after the cluster is gone, before
the record that holds it is removed.

The node image stays. It is a build cache, and a rebuild costs a full release download.

The mirroring fixture lives in its own namespace inside that cluster. The recipe creates that
namespace itself and removes it when the proof ends, and also when an assertion fails, so a failed
proof leaves no fixture behind. A namespace that already existed is never adopted and never
deleted: the proof fails instead.

## Tests

```sh
bash testing/infra/local_worker/test_recipe.sh
```

The fast group calls the validation predicates with declarative inputs and needs no cluster. The
live group runs the recipe end to end, then proves that cleanup refuses a cluster it cannot prove
it created, a damaged ownership record and a record that names another cluster, and that a
repeated cleanup succeeds without changing anything. Set `LOCAL_WORKER_TEST_FAST_ONLY=true` to
run the fast group alone.

## Limitations

- The proof is network-only. No worker starts, no aggregation request is made and no serving
  certificate is issued.
- The recipe runs on macOS arm64 only.
- The node image build needs the release tarball. kind downloads it with a deadline, and a slow
  link can exceed it. Build from a checksum-verified local copy of the same tarball with
  `--type file` when that happens; the resulting image is the same release. A cached copy whose
  checksum no longer matches the published one is ignored, and the build downloads its own.

---

**See also** — [Development](development.md) (build, lint and test commands) ·
[Internals](internals.md) (the invariants the code keeps) ·
[Scheduling Chain](../modules/devices/scheduling.md)

**Next** → [All documentation](../README.md) — pick the next page on the contributor path.
