# Preflight Operations

> **Purpose** — the runbook for `device-manager preflight`: the command line for both container
> runtimes, what each mount and flag is for, and what the command starts, writes and removes.
> **Audience** operators · **Prerequisites** [Device
> Discovery](../architecture/device-discovery.md#preflight-the-preconditions-read-before-a-workload-does)
> · **Read time** ~9 min

One container run on a bare host answers what that node can serve. It needs no cluster, no CRDs, no
NFD labels and no running device-manager. What the answers mean — the three states, the three depths
and why this is not part of `detect` — is on the linked page; this one is the procedure.

## Contents

- [Before you run it](#before-you-run-it)
- [The command line](#the-command-line)
- [What each mount is for](#what-each-mount-is-for)
- [What each flag is for](#what-each-flag-is-for)
- [What the command starts, writes and removes](#what-the-command-starts-writes-and-removes)
- [When a step is emitted instead of run](#when-a-step-is-emitted-instead-of-run)
- [What you get, by manufacturer](#what-you-get-by-manufacturer)
- [Reading the result](#reading-the-result)

## Before you run it

**There is one mode: the run reproduces what the device-manager does in production.** Three things
touch the node while it does, so decide them before running where live workloads are:

- the probe containers, which hold an accelerator for as long as they run;
- the preload-library tree, copied onto the host where an init container would have put it;
- **a driver mode asked on and put straight back**, on the two manufacturers whose slicing depends on
  one. A mode that is off is not a node that cannot serve — the allocator turns it on itself when a
  slice lands — so reading it answers nothing, and asking the driver is the only way to know. The
  toggle happens only where the mode was already off, so nothing on the node is sharing that
  accelerator and nothing can notice the window. A restore that fails is reported, loudly, on the
  row: the accelerator is left on and the row is the only thing that can send someone to turn it off.

Pass `--dry-run` to see all of it without any of it happening.

A probe container is **not** privileged. It gets exactly the device nodes, mounts and environment the
allocator's own injection names, because what it is there to measure is the isolation that injection
establishes — and a privileged container would be handed every device on the host instead.

Preflight needs `CAP_SYS_CHROOT` to enter the host root, which the default container capability set
already carries. `--privileged` is for the driver reads and the device nodes, not for the `chroot`.

## The command line

**Find your manufacturer below and run the block as it stands.** Each one is complete and needs
nothing filled in. Add `--dry-run` to any of them to see every step without taking one.

The blocks differ only in their last vendor argument, which is what that manufacturer's library
loads from — the same host path the device-manager DaemonSet mounts for it, at the same access, which
is what makes this run reproduce production rather than resemble it.

**On containerd**, swap `docker run` for `nerdctl run`; it resolves its own socket and namespace on
the host it is invoked on. The two blocks carrying `--runtime` are the exception, and each says what
to use instead.

> **A vendor path that is not there is created, not refused.** `docker run -v` creates a missing
> source directory as an empty one, so a typo — or a driver installed somewhere else — mounts nothing
> over the right place, and the detect pass reports zero accelerators: the same answer a node with no
> hardware gives. Check the path exists on the host before reading the result.

### AMD

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /opt/rocm:/opt/rocm:ro -v /opt/rocm/lib:/opt/rocm/lib:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=amd
```

The second mount is not a duplicate of the first. ROCm's packaging puts a symlink somewhere in this
path on every install, and where it falls decides whether the mount survives it: one at or above the
mount source is resolved by the host and is harmless, while one *inside* the mounted tree dangles in
the container. Naming the library directory as its own source has the host resolve it first.

> **Both shapes are ordinary.** On a single-version host `/opt/rocm` is itself the link. On one
> carrying two ROCm versions, `/opt/rocm/lib` is — and mounting `/opt/rocm` alone then loads no ROCm
> library at all, so a node with an accelerator reports as having none. The device-manager DaemonSet
> mounts both for the same reason.

> **To pin one ROCm version on a host carrying two**, mount the versioned tree in place of both —
> `-v /opt/rocm/core-<ver>:/opt/rocm:ro` — which is how the same host is preflighted against 7.14
> and 10.0 separately.

### Ascend

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /usr/local/Ascend:/usr/local/Ascend:ro -v /usr/local/dcmi:/usr/local/dcmi:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=ascend
```

No `--runtime ascend` here, although [Vendor Prerequisites](../vendor-prerequisites.md) requires that
runtime in production: what it injects is device *nodes*, and this command is already `--privileged`
with `/dev` mounted whole. The driver is the mount above. The Ascend *probe* containers do need it,
and get it themselves — under `nerdctl` they cannot, and are emitted for you to run instead.

### Cambricon

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /usr/local/neuware:/usr/local/neuware:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=cambricon
```

### Hygon

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /opt/hyhal:/opt/hyhal:ro -v /opt/dtk:/opt/dtk:ro \
    -v /etc/dmi_mig_config:/etc/dmi_mig_config \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=hygon
```

The third mount is the only writable one on this page: the vendor library materializes a partition by
writing that registry, and the DaemonSet mounts it writable for the same reason. Add
`--probe-image <image>` to measure the two slice rows — no default is claimed for a Hygon family, and
what the image has to carry is in [If you are in the "container probe, no driver read"
tier](#if-you-are-in-the-container-probe-no-driver-read-tier).

### Iluvatar

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /usr/local/corex:/usr/local/corex:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=iluvatar
```

No `--runtime iluvatar`, for the same reason Ascend needs none: `ix-container-runtime` injects device
nodes, which `--privileged` and `/dev` already cover, and the driver is the mount above.

### MetaX

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /opt/mxdriver:/opt/mxdriver:ro -v /opt/maca:/opt/maca:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=metax
```

### Moore Threads

```bash
docker run --rm --privileged --network=host --runtime mthreads \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=mthreads
```

Nothing to mount, and a `--runtime` instead: the user-space driver here is injected by the vendor
container runtime rather than installed at a path you can bind-mount. `mthreads` is the handler that
runtime registered with your container engine, and the same name the chart's RuntimeClass carries.
Under `nerdctl`, whose `--runtime` names an OCI shim, that flag is not the door.

### NVIDIA

```bash
docker run --rm --privileged --network=host --runtime nvidia \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=nvidia
```

Nothing to mount, for the same reason as Moore Threads above. Under `nerdctl` use `--gpus all` in
place of `--runtime nvidia`, whose `--runtime` names an OCI shim and rejects the handler name.

**The flag is not optional.** Measured on a host with one RTX 4090: the identical command under
`--runtime runc` reports `accelerators: 0` against a host whose own `nvidia-smi` reports 1, and exits
1; under `--runtime nvidia` it reports `ok`, `accelerators: 1`, and exits 0.

### T-Head

```bash
docker run --rm --privileged --network=host \
    -v /:/host -v /dev:/dev -v /sys:/sys \
    -v /usr/local/PPU_SDK:/usr/local/PPU_SDK:ro \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight --manufacturer=thead
```

Add `--probe-image <image>` to measure the slice rows: no default is claimed for a PPU family, because
the workload container brings its own SDK and there is no generation to match an image against.

### Every manufacturer at once

```bash
docker run --rm --network=host -v /:/host \
    gpustack/gpustack-operator:latest \
    gpustack-operator device-manager preflight
```

**With no vendor mounts at all**, the run still reports every manufacturer and names the mounts the
rest of the questions need. Only NVIDIA, Ascend and AMD carry a host CLI to cross-check that
detection against, so for the other six a detection of zero is this container's own view and not the
host's — the *Host cross-check* column below says which is which.

## What each mount is for

| Mount | What it is for |
|---|---|
| `-v /:/host` | the host's own root. Preflight enters it with `chroot` to run the host's container CLI and the host's vendor CLI, and stages the preload libraries through it. Move it with `--host-root` |
| `--network=host` | `chroot` changes the root and not the network namespace, so a host CLI entered through it reads the host's `/etc/resolv.conf` inside this container's namespace. Without it, any host CLI asked to pull an image fails on DNS |
| `-v /dev:/dev`, `-v /sys:/sys` | what preflight's own in-process detect pass and driver reads need. They stay in container context — only the host's executables go through the `chroot` |
| the vendor arguments for your manufacturer | what the manufacturer's library needs in order to load in this container — a host path to bind-mount, or a `--runtime` for the two whose driver is injected rather than installed. Without them the detect pass finds nothing while the host's CLI finds cards, which is exactly the discrepancy the result reports. Yours is in [The command line](#the-command-line) |

> **Why the host root rather than a mounted runtime socket** — mounting a runtime socket already
> grants host root, and needs one mount per runtime plus a CLI of ours in the image whose version
> need not match the daemon it talks to. Entering the host root costs one mount and gives the host's
> own CLIs, at their own versions.

## What each flag is for

| Flag | What it does |
|---|---|
| `--manufacturer` | which manufacturers to ask about, comma separated. Every one asked about is reported, including the ones nothing is read for. Defaults to all of them |
| `--no-pci-check` | as on `detect` — skip the PCI check the detect pass makes |
| `--dry-run` | print the container steps instead of taking them. It writes nothing to the host at all — no library tree, nothing a responder rendered, no driver mode — so a printed step names what its reader has to stage first, and a capability that could only be established by asking the driver reports that it was not asked |
| `--probe-image` | the image the probe containers run, overriding the per-family default. Required for a family that has no default, and the way to run a probe in an air-gapped environment |
| `--host-root` | where the host's root filesystem is mounted into this container. Defaults to `/host` |
| `--runtime` | the host container runtime to drive, overriding what was resolved. One of `docker`, `nerdctl`, `ctr`; anything else is refused before the pass starts, so a typo is a usage error rather than a run that quietly established nothing. An escape hatch: one of the three that the host does not carry drops every container step to being emitted |

**The runtime is resolved from the kubelet's own CRI endpoint** wherever the host states one, read
from `/var/lib/kubelet/kubeadm-flags.env` or `/var/lib/kubelet/config.yaml` through the mounted host
root. That is what starts a container on this node in production, and reproducing production is the
point.

> **Why not simply probe** — a host carrying both `docker` and `containerd`, with a kubelet talking
> to `containerd`, would be probed `docker`-first and every container answer would then describe a
> path no workload takes. A host that states no endpoint — a bare machine before a cluster exists,
> or a distribution keeping that configuration elsewhere — falls through to probing `docker`, then
> `nerdctl`, then `ctr`.

## What the command starts, writes and removes

**It starts one probe container per accelerator that can host a logical slice**, and — on a
manufacturer whose co-tenancy is measured — **two more for that accelerator afterwards**, started
together to see whether one card carries two slices at once.

Each one runs the vendor image resolved for that accelerator's family, with the injection an
allocation would emit, and prints its own address map and the vendor reader's output. The container
is named for its accelerator, so two cards never render their artifacts over one path.

**NVIDIA and Ascend probes are handed to a vendor runtime**, without which the container gets device
nodes and no user-space driver — and the flag for that differs by CLI. `docker` takes
`--runtime nvidia` / `--runtime ascend`, naming an entry in its own daemon configuration; `nerdctl`'s
`--runtime` names an OCI shim instead and rejects those names outright, so its door to the NVIDIA
hook is `--gpus`. A pair with no known door — `nerdctl` and Ascend — is emitted rather than run.

**Everything it starts is removed.** Every probe container carries the label
`gpustack.ai/preflight=true` and is started with `--rm`, and a run sweeps containers carrying that
label before it starts any of its own — so one left behind by a run that was killed is collected by
the next.

On containerd the containers are created in the namespace `gpustack-preflight`, named in the result,
and the CLI is pointed at the socket that was resolved rather than at its own default.

**The run writes to the host in four places, and only the first two outlive it:**

| Write | Where | Removed when |
|---|---|---|
| the manufacturer's preload-library tree, copied out of this image | `/var/lib/gpustack/operator/lib/<manufacturer>` | **never** — it is what a device-manager init container normally stages, and a probe container has none |
| whatever the allocator's own responder rendered for a container | `/var/lib/gpustack/operator/preflight/` | at the end of the pass. What lands here is the responder's own business: Ascend writes an `npu_info.config`, Hygon a `vdev.conf`, and a manufacturer whose responder renders nothing writes nothing |
| a lock, so two runs cannot sweep each other's probes | `/var/lib/gpustack/operator/preflight/lock` | **never** — the file stays; the lock on it is held by the running process and released by the kernel when that process ends, whatever ended it |
| a driver mode asked on, to tell "off" from "cannot be turned on" | the host's driver state | immediately, in the same breath — unless the restore itself fails, which the row then says |

> **Run it before the node serves workloads.** The mode above is put back within the same call, but
> nothing serialises that against a device-manager allocating on the same card — an allocation landing
> in that window can be switched off under. Ascend and Cambricon are the two manufacturers that write.

> **Run the image that matches the installed device-manager.** The library tree in the first row is
> the one real allocations mount, not a copy of it, and staging replaces the files already there. On a
> node with a device-manager installed, running preflight from a *different* image version therefore
> leaves that version's preload libraries behind for every allocation afterwards — a working node,
> silently changed by a command that reads. On a node being brought up there is nothing there yet and
> nothing to disturb, which is what this command is for. If you must run it against an installed
> node, use the same tag the device-manager runs, and treat any other tag as an operation that
> changes the node.

> **Why they go in a directory of their own** — the neighbouring `.../operator/pods` is what an
> allocator reads as its record of what other Pods hold, and an entry there under a Pod UID no
> kubelet scheduled would be counted as occupancy. Preflight never writes into it, so a run killed
> before it can clean up costs a later allocation nothing.

## When a step is emitted instead of run

A container step that cannot be taken is **printed complete**, and the row says it was emitted. That
is an answer, not a failure of the node, and it does not affect the exit code. Whether it runs as
printed depends on the case: the two below that write nothing to the host name what you have to put
there first, and the `ctr` row names a CLI this host does not have. Five cases reach it:

| Case | What is printed |
|---|---|
| `--dry-run` | the command, plus the two things its reader still has to do: stage the library tree, and let a responder render whatever it renders — a dry run writes neither |
| the library tree could not be staged | the command, naming what could not be written. It is not runnable until that is fixed, and the row says so — but the command is what an operator needs in order to stage the tree by hand and take the step themselves |
| no container runtime on the host | the command, naming what was probed |
| no host root to enter | the command, naming the marker directory that was looked for and absent |
| a runtime that cannot pass the vendor runtime (`ctr`, or `nerdctl` for Ascend) | the same command **rewritten in a dialect that can** — `nerdctl` against the socket `ctr` resolved, or `docker` with its `--runtime` flag and the containerd addressing dropped. A `ctr` resolved on its own was reached *because* nerdctl is absent, and the row says so: install nerdctl, or take the step from a host that has one |

> **Why `ctr` never starts a probe** — `ctr run` has no flag that passes a device node, so the only
> way to reach an accelerator through it is `--privileged`, which grants every device on the host.
> That would report an isolation the injection never established: a measured answer that measured the
> wrong thing.

## What you get, by manufacturer

**Find your manufacturer first.** How much this command can tell you depends entirely on how much
your allocator reads before it hands a device out, and that differs by vendor. There are four tiers,
and knowing which one you are in is the difference between reading the result and being puzzled by it.

**Every manufacturer is asked for the capability rows.** All nine allocators can be asked to produce an
injection without one being served, so every one of them answers *what would this allocation grant*.
What differs by tier is the two things an injection alone cannot establish: whether a driver said
anything **before** it, and whether a container was started **after** it.

**How many of those rows come back is a fact about the accelerator, not the tier.** The two sliced
rows are produced only for an accelerator declaring logical slicing; a partition-backed one — a card
with MIG on — reports the two management rows and no sliced rows, because a partitioned accelerator
serves no other mode. One declaring neither family reports none of the four.

**An absent row is a family this accelerator does not offer**, not a result that went missing.

| Tier | Manufacturers | A driver-read row | The four capability rows reach | Deepest answer |
|---|---|---|---|---|
| **Full** | NVIDIA, Ascend, AMD, T-Head | yes | a real container, except `sidecar-visibility` | `measured` |
| **Container probe, no driver read** | Hygon | no — a `note` says why instead | a real container, except `sidecar-visibility` | `measured` |
| **Driver read + injection** | Cambricon, MetaX | yes | the injection only; no container is started | `simulated` |
| **Injection only** | Iluvatar, MThreads | no — a `note` says why instead | the injection only; no container is started | `simulated` |

The two middle tiers are the reminder that the two questions are independent. Cambricon and MetaX
read a driver and start no container; Hygon starts a container and reads no driver. Neither is
"further along" than the other — they answer different halves.

`sidecar-visibility` is `simulated` on **every** tier, including the full one: measuring it would need
the owner container still running when the sidecar starts, and the probe containers are one-shots that
exit as soon as they have printed their evidence.

### If you are in the "injection only" tier

**Your rows stop one step short of the full tier's, and the missing step is the driver read.** Your
allocator consults no driver when it serves an allocation: the memory cap and compute weight come from
the container's own resource request, and the host kmod plus the vendor container runtime enforce
them. There is no precondition to check ahead of time — which is a fact about your stack, not a gap in
this command, and the `note` on your group says so in your vendor's own terms.

What you do get:

- **The capability rows your accelerators offer, at `simulated`** — the allocator really did produce the injection, and
  the row names what it grants: the device nodes, the mounts, the environment. What is *not*
  established is what that injection looks like from inside a container, because no container was
  started. See [#138](https://github.com/gpustack/gpustack-operator/issues/138) for the work that
  would close that; Hygon left this tier that way, and the two sections below are what changed.
- **The host cross-check** on your detection — the one thing `detect` cannot do. From inside a
  container with no device mounts, "this machine has no accelerators" and "this machine has eight you
  cannot reach" are the same sentence. This enters the host and asks its own vendor CLI, so a
  detection of zero on a host that sees eight comes back naming the mounts you are missing. Only
  NVIDIA, Ascend and AMD carry a host CLI to cross-check against; the rest of the table's
  *Host cross-check* column says so.
- **The `note`**, which says in that manufacturer's own terms why no precondition exists — so you can
  tell "nobody implemented this" from "there is nothing here to implement".

If a slice does not work on your node, the place to look is the vendor container runtime and the
resource request, not a driver flag.

### If you are in the "container probe, no driver read" tier

**Hygon is here, and it is the tier where the two sliced rows mean the most.** Its allocator reads no
driver — the paragraph above applies to the `note` on your group unchanged — but a container *is*
started, so `sliced-runtime-loaded` and `sliced-quota-in-force` come back at `measured`.

Three things are worth knowing before you read those rows:

- **`--probe-image` is required.** No default is claimed for a Hygon family. The image does not have
  to be a DTK one — the probe runs the vendor's own `BandwidthTest` out of `/opt/dtk`, which your
  allocator already mounts — but it is not "any image" either. It needs `sh`, `cat`, `grep`, `awk`,
  `mkdir`, `sleep` and `kill`, plus a C library that can load a dynamically linked glibc binary.
  `mkdir` is load-bearing: the reader claims the driver record it is about to read, and an image
  without it reports a healthy accelerator as unavailable. Measured on a glibc image without DTK.
- **The evidence is the driver's record, not a log line.** The other measured manufacturers preload a
  library GPUStack builds and raise its log level to make it state the cap. Hygon's slicing runtime is
  the vendor's own DTK/hyhal user space, whose vgpu diagnostics have an API to set the level and no
  environment variable — so there is nothing to turn on from outside. What answers instead is the
  per-slice record the driver publishes under the kfd vgpu sysfs, which exists only for a process
  that entered vgpu mode.
- **`hy-smi` will not confirm a slice for you.** It answers from the DMI layer and reports the
  physical card: under a container capped at 1024 MiB it still prints the card's full VRAM. That is
  not a broken slice — the cap binds the HSA/HIP runtime a workload uses. To see a slice by hand, ask
  a HIP client.

### The full table

| Manufacturer | Detection reads | Host cross-check | Driver-read row | For which mode | Container probe |
|---|---|---|---|---|---|
| NVIDIA | NVML | `nvidia-smi -L` | `mig-partitioning` | `partitioned` | ✅ |
| Ascend | DCMI | `npu-smi info -l` | `container-share` | `sliced` | ✅ |
| AMD | RSMI, AMDSMI, HSA | `rocm-smi --showuniqueid` | `cu-mask-topology` | `sliced` | ✅ |
| T-Head | HGML | | `mig-partitioning` | `partitioned` | ✅ |
| Cambricon | CNDev | | `smlu-mode` | `sliced` | |
| MetaX | MXSML, sGPU sysfs registry | | `sgpu-mode` | `sliced` | |
| Hygon | RSMI, AMDGPU, HSA | | | | ✅ |
| Iluvatar | IXML | | | | |
| MThreads | MTML | | | | |

**An empty cell is "this manufacturer has none"**, not a value left out.

> **Read the `mode` column, not the capability name.** Every row in a report carries a `mode`, and it
> is what makes two manufacturers comparable: `container-share` and `cu-mask-topology` are different
> vendors' words for the same question — *can this accelerator be logically sliced?* The capability
> name stays the vendor's own so that searching for it finds their documentation; `mode` is what tells
> you two rows answer the same thing, and which mode nothing answered for on your node.

**An empty host cross-check is an answer, not a gap.** A manufacturer with none has no vendor CLI
whose output shape this command has established, and the column says so rather than guessing: a wrong
match counts zero, and a zero reads as "the host sees nothing either" — the one answer that sends an
operator to debug the wrong layer.

### What the driver-read row means, per manufacturer

| Row | `ok` means | `not-declared` means | `unavailable` means |
|---|---|---|---|
| `mig-partitioning` | the accelerator declares a physical-slice profile | no profile is declared, so MIG is off or absent — a consumer card always reads this way | the driver could not be asked |
| `container-share` | the flag is on, or was off and the driver accepted turning it on | this generation has no such flag | dcmi has no such entry point, or refused |
| `cu-mask-topology` | the HSA topology reads and validates, and the detail names the compute units and the allocation atom | — | HSA could not be initialized, or no agent names the card |
| `smlu-mode` | the mode is on, or was off and the driver accepted turning it on | the accelerator advertises no logical-slicing capacity | cnDev has no sMLU API, or refused |
| `sgpu-mode` | the registry reads; the detail says whether the card already hosts a subdevice | — | the registry could not be read |

Two of these are **established rather than read** — `container-share` and `smlu-mode`. Where the mode
is off, the driver is asked to turn it on and it is put straight back; the detail says which
happened, and says so explicitly if the restore failed and the accelerator was left on.

Under `--dry-run` the ask is withheld, because asking is a write however briefly the mode is held.
The row then reports the mode as read and says it was not established — which is a different answer
from a capability that was checked and found working.

### The slice rows, and what the probe needs

Five manufacturers have a container probe. Each starts one container per sliceable accelerator, and
the four rows below are what they add.

| Manufacturer | Probe image | Vendor runtime | Reader inside | Where the cap is |
|---|---|---|---|---|
| NVIDIA | `nvidia/cuda:<CUDA major>` | `--runtime nvidia` (docker) · `--gpus all` (nerdctl) | `nvidia-smi --query-gpu=memory.total` | `CUDA_DEVICE_MEMORY_LIMIT_<i>` |
| Ascend | `quay.io/ascend/cann:<CANN major>-<family>` | `--runtime ascend` (docker only) | `enpu-monitor` | `npu_info.config`, key `memory-quota` |
| AMD | `ubuntu:24.04` | none | `rocm-monitor` | `VROCM_DEVICE_MEMORY_LIMIT_<i>` |
| T-Head | none — pass `--probe-image` | none | `ppu-monitor` | `HGGC_DEVICE_MEMORY_LIMIT_<i>` |
| Hygon | none — pass `--probe-image` | none | `BandwidthTest`, then the kfd vgpu record it causes | `vdev0.conf`, key `mem` |

The rows themselves are the same four everywhere:

| Row | What it establishes |
|---|---|
| `sliced-runtime-loaded` | every shared object the injection mounts is in the container's own address space — or, where the injection mounts none, that the driver recorded a slicing instance for the container. `unavailable` means the slice was **not** established: usually a container that got the whole accelerator and no cap at all, and for a driver-record manufacturer also a probe whose client could not run. The row's `reason` and `evidence` say which |
| `sliced-quota-in-force` | the container reported back the cap the injection set, rather than the accelerator's own figure |
| `sidecar-visibility` | the `sshd` sidecar's allocation names what the owner was granted, and nothing the owner was not |
| `co-tenancy` | two independent slices were placed on one accelerator, each with its own geometry |

Hygon is the row that mounts no shared object of ours, so its `sliced-runtime-loaded` is answered by
the driver's own per-slice record rather than by the container's address space — the tier above
describes what that costs and what the probe image has to carry.

A slice asks for **half the accelerator**, so the figure to expect inside is half what the host
reports: a 24564 MiB card caps at 12282, a 65536 MiB one at 32768. Half rather than a sliver because
the figure is the evidence, and a cap near the card's own size could be read as the card.

## Reading the result

One YAML document goes to stdout, with **two sections**:

```yaml
accelerators:            # one group per manufacturer asked about
  - manufacturer: nvidia
    detection: {...}
    checks: [...]
network:                 # the node's RDMA links — belongs to no manufacturer
  timestamp: ...
  checks: [...]
```

Each `accelerators` group carries the time it was read, the detection answer, and a row per
accelerator per capability — each row carrying
[the state and the depth](../architecture/device-discovery.md#preflight-the-preconditions-read-before-a-workload-does)
it reached, the driver's or the container's own words, and the container command where one was
involved.

The `network` section carries one row per RDMA-capable interface — and per RDMA-capable virtual
function, named `<interface>/<vf bus id>`, since on an SR-IOV node those are every RDMA device there
is. Each row gives the name, the RDMA device and the
[link verdict](../architecture/network-topology.md#the-rdma-link-is-checked-because-a-bound-device-is-not-a-working-link).

The rows come from the same pass that produces the node's published record. That makes the two agree
on **how** a link is judged, not on what it says: this is a fresh read taken when you invoke it, so
a link that changed since the last detect pass shows here first.

An entry with no RDMA device **and no link verdict** gets no row: `rdma: false` on its own settles
the question, while a verdict without a device is the unreadable-tree case this section exists to
surface. A section with no rows carries a `note` saying which of the two reasons applies: the
enumeration failed, or the node has no RDMA hardware.

**The exit code is non-zero only for an `unavailable` accelerator answer.** A capability this
generation does not declare, a manufacturer nothing is checked for, a node carrying none of its
hardware, an answer that went no deeper and a step that was emitted are all answers, and a run that
reports them has done its job.

**A broken RDMA link does not fail the run.** It withholds a node label, which changes what a
flavor selects rather than what an allocator can hand out.

> **Why** — this exit code answers whether the node can serve the allocation modes its allocators
> offer, and a down link stops none of them. The reasoning is recorded in the feature's spec.

The document is written whether or not the node passed, so a failing run is still readable rather
than leaving the exit code as the only thing to debug from.

**Only one preflight runs on a node at a time.** Every probe container carries the same label, so a
second run's stale sweep would remove the first run's *live* probes — and an accelerator whose probe
was killed mid-measurement reports as unable to slice. A second run therefore refuses before it
sweeps anything, reports every manufacturer `unavailable` naming the lock, and exits non-zero.
`--dry-run` is exempt: it starts nothing and writes nothing.

**A failed detection stops that manufacturer's other two questions.** Detection is the floor the rest
stands on, so when it reads `unavailable` — the container cannot see hardware the host reports, or the
accelerators answered but could not be named — the group carries its detection block, no rows, and a
`note` saying the remaining questions are unanswerable. Fix what the detection `reason` names and run
it again; rows about accelerators the report cannot identify would not have helped.

**One manufacturer's crash does not take the others down.** Everything reached for a manufacturer is
vendor code over a driver a half-installed node can leave in any state — which is the node this
command exists to be run on.

A panic in one is contained to that one: its group keeps the detection block it had already answered,
discards whatever the crashed pass had filled in, and reports one `unavailable` row per accelerator
plus a `note` with what the panic said. The other eight are read and reported as usual.

```yaml
- manufacturer: cambricon
  checks:
    - accelerator: MLU-0
      capability: preflight-panicked
      state: unavailable
      reason: this manufacturer's preflight panicked and was contained, so none of this
        accelerator's preconditions were established: runtime error: invalid memory address
  note: 'this manufacturer''s preflight panicked and was contained: whatever it had established
    is discarded, and every accelerator it was asked about is reported unavailable below; the
    rest of this run is unaffected: runtime error: invalid memory address'
```

The rows are what make it **exit non-zero**: a crash that left only a note would be a run that
verified nothing and said it passed. Treat it as a bug worth reporting, with the `-v=3` output
attached — the stack trace goes to the log rather than the document.

**A sweep that could not be completed fails the accelerators it could not clear.** Before anything is
started, the run removes every container left behind by an earlier one — see [What the command
starts, writes and removes](#what-the-command-starts-writes-and-removes). Where that sweep fails, the
run carries one `stale-container-sweep` row per accelerator:

```yaml
- accelerator: GPU-0
  capability: stale-container-sweep
  state: unavailable
  reason: 'the stale-container sweep could not be completed, so a container an earlier run left
    behind may still be holding this accelerator and every answer measured against it here is
    unsafe to trust: docker: exit status 1: permission denied'
```

It is a failure and not a warning because of what a leftover does: it still holds its accelerator, so
the slice measured against it comes back short and the report blames the card. A run that swept
nothing, measured anyway and exited zero would state the opposite of the truth about hardware that is
fine. Clear the leftovers by hand — `docker ps -aq --filter label=gpustack.ai/preflight=true`, and the
same for `nerdctl` with `--namespace gpustack-preflight` — then run it again.

**Not every unhappy sweep is one of these.** The sweep lists by label and then removes what it found,
and the two are told apart:

| What failed | On which runtime | Reported as |
|---|---|---|
| the removal | any | a failure — the container was seen and would not go away |
| the listing | the one this run drives | a failure — the probes are about to run through a runtime that cannot be asked what it holds |
| the listing | any other | nothing — a runtime this command cannot reach is one no earlier run started a probe through either |

The last row is why a node carrying `nerdctl` beside a containerd that is not listening stays green:
the CLI is there, its socket file may even be there, but nothing can be behind it.

---

**See also** — [Device
Discovery](../architecture/device-discovery.md#preflight-the-preconditions-read-before-a-workload-does)
(what preflight reads and what the states and depths mean) · [Vendor
Prerequisites](../vendor-prerequisites.md) (the driver and toolkit paths per manufacturer) ·
[Accelerator Requests](../accelerator-requests.md) (the modes these answers guard)

**Next** → [NVIDIA MIG Operations](nvidia-mig.md) — the runbook for the one mode preflight reads and
never creates.
