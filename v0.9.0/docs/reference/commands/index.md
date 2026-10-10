# Command Reference

The `gpustack-operator` binary provides long-running services and one-shot commands. The services are what a
deployment runs; the one-shots are what you run by hand on a node, and the first two sections choose
between them.

## Contents

- [Command summary](#command-summary)
- [Choosing a one-shot command](#choosing-a-one-shot-command)
- [Global flags](#global-flags)
- [worker](#worker)
- [worker-gateway](#worker-gateway)
- [device-manager serve](#device-manager-serve)
- [model-manager](#model-manager)
- [device-manager detect](#device-manager-detect)
- [device-manager monitor](#device-manager-monitor)
- [device-manager preflight](#device-manager-preflight)

## Command summary

| Command | Alias | Shape | Who runs it |
|---|---|---|---|
| `worker` | `w` | service | the operator Deployment, one per cluster |
| `worker-gateway` | `wg` | service | a sidecar beside `worker` |
| `device-manager serve` | `dm serve` | service | the Device Manager DaemonSet, one per node |
| `model-manager` | `mm` | service | the Model Manager DaemonSet, one per node |
| `device-manager detect` | `dm detect` | one-shot | a person, on a node |
| `device-manager monitor` | `dm monitor` | one-shot | a person, on a node |
| `device-manager preflight` | `dm preflight` | one-shot | a person, on a node |

## Choosing a one-shot command

The one-shot commands answer different questions; only `preflight` changes node state.

| Question | Command | Node effect |
|---|---|---|
| What accelerators are here, in full? | `detect` | nothing — a pure read |
| What are they doing right now? | `monitor` | nothing — a pure read |
| Can they actually be sliced, and managed while sliced? | `preflight` | starts containers, stages libraries, asks a driver mode on |

`detect` and `monitor` are reads no flag can turn into an action. `preflight` is the one that acts:
it starts probe containers, stages the injected libraries onto the host, and, where a
manufacturer's sharing mode is off, asks the driver to turn it on to see whether it takes, putting
it back either way. `--dry-run` is how it is made a read.

The probe containers it starts are themselves **not** privileged: they get the allocator's injection
and nothing more, because the isolation that injection establishes is the thing being measured.

> **Why `detect` is not folded into `preflight`** — `detect` emits the whole ledger, typed as the
> `Devices` CRD's own `spec.groups`, so it can be diffed against what the cluster recorded.
> `preflight`'s detection block is a verdict and a count, deliberately not a second ledger. A reader
> who wants both runs both.

## Global flags

Every command accepts these. They come from klog and are omitted from the per-command tables below.

| Flag | Effect |
|---|---|
| `-v` | log verbosity, `0` unless you pass one. The DaemonSet runs at `-v=2`; `-v=3` adds the per-decision lines, including why a container runtime did or did not resolve |
| `--vmodule` | per-file verbosity, `pattern=N` comma separated |
| `--logtostderr` | log to standard error rather than files (default true) |
| `--add_dir_header`, `--skip_headers`, `--log_backtrace_at` | log line shape |
| `--print-cmdline` | print the effective command line, including what was read from the environment |
| `--version` | the binary's version |

## worker

It is the control plane: it profiles node capacity, runs the controllers that materialize the
Kueue scheduling chain, serves the aggregated extension APIs and the admission webhooks, and installs
the applications a cluster needs.

Runs as the operator Deployment. Flags below are the ones that change behaviour rather than tune a
connection; see [Settings](/gpustack-operator/v0.9.0/docs/reference/settings/index.md) for what is configured through the `Setting` CR instead.

| Flag | Default | Effect |
|---|---|---|
| `--manufacturer` | all manufacturers | which manufacturers to detect |
| `--disable-applications` | none | applications to skip installing, or `*` for all. See [Installation Modes](/gpustack-operator/v0.9.0/docs/operate/installation-modes/index.md) |
| `--secure-port` | `31443` | the HTTPS port |
| `--bind-address` | `0.0.0.0` | the address to serve on |
| `--cert-dir` | — | where `tls.crt` and `tls.key` are |
| `--disable-auths` | `false` | skip authentication and authorization |
| `--kube-leader-election` | `true` | leader election, for multi-instance deployments |
| `--kube-leader-election-id` | `worker.gpustack.ai` | the election's unique ID |
| `--kube-leader-lease`, `--kube-leader-renew-timeout` | `15s`, `10s` | how long leadership is held and renewed within |
| `--kube-conn-qps`, `--kube-conn-burst`, `--kube-conn-timeout` | `200`, `400`, `5m` | loopback API client limits |
| `--informer-cache-resync-period` | `1h` | informer resync period |
| `--gopool-worker-factor` | `100` | goroutine pool size, per CPU core |
| `--audit-log-path`, `--audit-policy-file`, `--audit-webhook-config-file` | — | request auditing |
| `--cors-allowed-origins` | all | allowed cross-domain origins, as anchored regular expressions |
| `--model-manager-service-account` | `gpustack-operator-model-manager` | the ServiceAccount, in the operator namespace, the model-manager plugin runs as; only it may write a `NodeModelStore`'s status |

```bash
gpustack-operator worker --secure-port=31443 --disable-applications=kueue
```

## worker-gateway

Aggregates resources from upstream clusters and mirrors them for `worker` to read. Runs beside
`worker`; by default the two speak over a unix socket rather than a port.

| Flag | Default | Effect |
|---|---|---|
| `--bind-unix-path` | `/var/lib/gpustack/gpustack-operator-worker-gateway.sock` | the socket to serve on. When set, `--bind-address` and `--secure-port` are ignored |
| `--worker-conn-mode` | `gpustack-api` | how the worker is reached: `gpustack-api` or `loopback` |
| `--worker-conn-gpustack-api-port` | `30080` | the GPUStack server's port in `gpustack-api` mode |
| `--worker-conn-gpustack-api-scheme` | derived from the port | `http` or `https` |
| `--worker-readiness-check-timeout` | `30s` | one readiness check of a worker's API services. Must not exceed `--kube-conn-timeout`, which otherwise bounds it |
| `--secure-port`, `--bind-address`, `--cert-dir` | — | serving, when not on a socket |
| `--kube-conn-*`, `--informer-cache-resync-period`, `--gopool-worker-factor` | as `worker` | client and cache tuning |

```bash
gpustack-operator worker-gateway --worker-conn-mode=loopback
```

## device-manager serve

It is the per-node agent: it detects accelerators, keeps the `Devices` ledger current, samples
utilization, registers with the kubelet as a device plugin and injects the vendor runtime into
allocated containers.

Runs as the Device Manager DaemonSet. The `--no-*` mode flags are how a node is restricted to a
subset of the allocation modes its hardware would otherwise offer.

| Flag | Default | Effect |
|---|---|---|
| `--manufacturer` | all manufacturers | which manufacturers to detect |
| `--no-sliced` | `false` | do not create logically sliced devices |
| `--no-partitioned` | `false` | do not create hardware-partitioned devices |
| `--no-shared` | `false` | do not create shared devices |
| `--no-pci-check` | `false` | skip the PCI check the detect pass makes |
| `--no-fast-failed` | `false` | publish a first pass that found nothing rather than holding it back and detecting again. Only applies when `--manufacturer` names exactly one |
| `--monitor-period` | `15s` | how often utilization is sampled; only the latest sample is kept |
| `--kube-socket` | `/var/lib/kubelet/device-plugins/kubelet.sock` | the kubelet socket to register on |
| `--secure-port` | `32443` | the HTTPS port |
| `--bind-address`, `--cert-dir`, `--kube-conn-*`, `--informer-cache-resync-period`, `--gopool-worker-factor` | as `worker` | serving, client and cache tuning |

```bash
gpustack-operator device-manager serve --manufacturer=nvidia --no-partitioned
```

## model-manager

The per-node model cache. A CSI node plugin serving inline ephemeral volumes of the driver
`model.csi.gpustack.ai`: it authorizes each mount against the Pod's namespace, downloads and
verifies a Hugging Face artifact's files once per node, and writes that node's `NodeModelStore`
status. See [Node Model Store](/gpustack-operator/v0.9.0/docs/modules/model-delivery/node-store/index.md).

Runs as the Model Manager DaemonSet beside the `node-driver-registrar` sidecar. Its configuration
comes from its node's `NodeModelStore.spec`, not from flags.

| Flag | Default | Effect |
|---|---|---|
| `--node-name` | — | the node it runs on, also its `NodeModelStore`'s name |
| `--kubelet-dir` | `/var/lib/kubelet` | kubelet's root directory, whose pods directory holds the mount targets |
| `--cache-root` | `/var/lib/gpustack/models` | the node's cache directory |
| `--csi-socket` | `<kubelet-dir>/plugins/model.csi.gpustack.ai/csi.sock` | the Unix socket the CSI services listen on |
| `--secure-port` | `32444` | the HTTPS port: metrics, readiness and liveness |
| `--bind-address`, `--cert-dir`, `--kube-conn-*`, `--informer-cache-resync-period`, `--gopool-worker-factor` | as `worker` | serving, client and cache tuning |

```bash
gpustack-operator model-manager --node-name="$(hostname)" --cache-root=/mnt/models
```

## device-manager detect

Runs one detect pass and prints the result. It is a pure read: nothing is written, nothing is
started, and no flag changes that.

The output is the `Devices` CRD's own `spec.groups`, so it can be compared directly against what the
cluster recorded; `kubectl get devices <node> -o yaml` beside it is a diff.

| Flag | Default | Effect |
|---|---|---|
| `--manufacturer` | all manufacturers | which manufacturers to detect |
| `--no-pci-check` | `false` | skip the PCI check |
| `--no-fast-failed` | `false` | publish a first pass that found nothing rather than detecting again |

```bash
docker run --rm --privileged -v /dev:/dev -v /sys:/sys \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager detect --manufacturer=amd
```

```yaml
- id: radeon-rx-7800-xt
  manufacturer: amd
  name: AMD Radeon RX 7800 XT
  memory: 16368
  cores: 60
  driverVersion: 6.16.13
  runtimeVersion: "7.2"
  computeCapability: gfx1101
  family: GC 11.0.0
  accelerators:
    - id: GPU-5c88007d760374f3
      index: 0
      physicalIndexes:
        - 1
        - 128
      topology:
        pciBusId: "0000:04:00.0"
        numaAffinity: "0"
        cpuAffinity: 0-13
      status:
        unhealthy: false
        logicalSliced:
            coresPercentageOvercommit: true
            count: 128
```

> **A vendor's user-space libraries have to be reachable.** AMD has no container runtime that
> injects them, so a run without `/opt/rocm` mounted finds no accelerator on a node that has two.
> `preflight`'s host cross-check is what tells that apart from a node with none; see
> [below](#device-manager-preflight).

## device-manager monitor

Takes one utilization sample and prints it. A pure read, like `detect`, and it accepts the same
flags.

```bash
docker run --rm --privileged -v /dev:/dev -v /sys:/sys \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager monitor --manufacturer=amd
```

```yaml
- manufacturer: amd
  timestamp: 2026-08-29T08:01:43.565038175+08:00
  accelerators:
    - id: GPU-5c88007d760374f3
      memory: 16368
      memoryUsage: 174
      memoryUtilization: 1
      coresUtilization: 0
      temperature: 44
      powerUsage: 8
      unhealthy: false
```

The same figures reach a cluster through the Instance metrics surfaces; see [Instance
Metrics](instance-metrics.md).

## device-manager preflight

It answers whether the allocation modes this node's allocators offer actually work here. `detect`
cannot answer that question, because a capability can be declared and still not work.

This is the only command an operator runs by hand that acts on the node. See
[Preflight](/gpustack-operator/v0.9.0/docs/modules/devices/preflight/index.md) for the procedure (what to mount, what it starts and
removes, how to read each row).

| Flag | Default | Effect |
|---|---|---|
| `--manufacturer` | all manufacturers | which manufacturers to ask about. Every one asked about is reported, including those nothing is read for |
| `--dry-run` | `false` | print the container steps instead of taking them, and write nothing to the host. Each answer reports the complete invocation — and, because nothing was written, names the staging it would have done: the library tree, and whatever the manufacturer's responder renders. Both have to exist before the printed command runs |
| `--probe-image` | derived per family | the image the probe containers run. Required for a family with no default, and the way to run in an air-gapped environment |
| `--host-root` | `/host` | where the host's root filesystem is mounted into this container |
| `--runtime` | resolved | the host runtime to drive, overriding what was resolved. One of `docker`, `nerdctl`, `ctr`; anything else is refused before the pass starts. An escape hatch: a runtime that the host does not carry drops every container step to being emitted |
| `--no-pci-check` | `false` | skip the PCI check |

### Row states and depths

Each row carries a state, a depth and, where a container ran, the evidence it ran on.

| State | Meaning | Exit code |
|---|---|---|
| `ok` | the capability works, at the depth stated | 0 |
| `not-declared` | the accelerator does not offer it, so there is nothing to check | 0 |
| `unavailable` | it is offered and this pass did not establish it | **non-zero** |

| Depth | Method |
|---|---|
| `declared` | read from the driver |
| `simulated` | the allocator produced the injection, and it was not run |
| `measured` | a container ran with that injection and its output was read |

### A measured example

Taken from a run on two RX 7800 XT cards:

```yaml
- accelerator: GPU-5c88007d760374f3
  capability: sliced-runtime-loaded
  state: ok
  depth: measured
  detail: the slicing runtime the injection mounts is loaded in the container
  command: /usr/sbin/chroot /host docker run --rm --label gpustack.ai/preflight=true … ubuntu:24.04 …
  evidence: |
    gpustack-preflight-maps-begin
    …
    7205a176b000-7205a176d000 r--p 00000000 fc:00 13006695   /usr/local/vrocm/libvrocm.so
```

The verdict is the mapping line, not the shim's own greeting: a library that failed to load prints a
message naming the same path, so only `/proc/self/maps` says it is actually there.

The same row under `--dry-run` carries the `command` and no `evidence`, and its depth is
`simulated`: the allocator's intent, proven, with nothing run to confirm it took.

**Not every row on that node reaches `measured`.** `sliced-quota-in-force` on the same two cards is
`simulated`, because the vendor reader there reports nothing until an allocation has been charged and
the probe allocates nothing: the injection sets a cap and no container was observed under it. A row
is `measured` only where something was read back, never where the intent alone is known.

### Runtime resolution

Resolved from the kubelet's own CRI endpoint wherever the host names one, because that is what
starts a container on this node in production. A host that names none (a bare machine, or a
distribution keeping that configuration elsewhere) falls through to probing `docker`, `nerdctl`,
`ctr`.

The sources are ordered, and the first that names an endpoint answers, so a standard kubelet
configuration wins over a distribution drop-in rather than conflicting with it. The conflict that
does stop a run is **within one source**: a glob matching two directories that disagree, where
neither is the kubelet's own by position. The steps are emitted instead, naming the endpoints in
conflict, and `--runtime` is how you settle it.

`ctr` is never driven: `ctr run` has no `--device`, and its only alternative grants every device, so
what it measured would not be the isolation the injection established. A node resolving to it gets
the step emitted for `nerdctl` instead, against the socket and namespace `ctr` resolved, or for
`docker` where `nerdctl` cannot pass this manufacturer's vendor runtime either.

```bash
docker run --rm --privileged --network=host --runtime nvidia \
  -e NVIDIA_VISIBLE_DEVICES=all \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=nvidia --dry-run
```

`--network=host` is not optional: the host's CLI is entered through a chroot, which changes the root
and not the network namespace, so an image pull would read the host's resolver and find nothing
answering.

---

**See also** — [Preflight](/gpustack-operator/v0.9.0/docs/modules/devices/preflight/index.md) for the procedure and the per-manufacturer
tables · [Settings](/gpustack-operator/v0.9.0/docs/reference/settings/index.md) for what is configured through the `Setting` CR ·
[Internals](/gpustack-operator/v0.9.0/docs/contribute/internals/index.md) for startup order and the invariants these commands rest
on.
**Next** → [Preflight](/gpustack-operator/v0.9.0/docs/modules/devices/preflight/index.md)
