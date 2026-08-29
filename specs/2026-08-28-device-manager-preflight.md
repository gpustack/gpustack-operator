# Spec: Device Manager Preflight — Self-Contained Verification of What a Node Can Serve

Status: Shipped
Type: Feature

## Summary

Today the only way to learn what a node can actually do with its accelerators is to install the whole
chain — NFD, the Device Manager DaemonSet, Kueue, a workload — and read the failure when something does
not work. Detection is answerable on its own (`device-manager detect`), but everything past it is not:
whether the devices can be sliced, whether a slice really binds a quota rather than reporting the whole
card, and whether the behaviours GPUStack layers on top of a slice — an SSH-enabled Instance's sidecar
sharing its owner's accelerator, two tenants coexisting on one card — hold on this hardware. Each of
those is answered today by a Pod that started and then failed, with an error naming neither the
accelerator nor the capability.

This spec adds a `preflight` subcommand that makes the **operator image verify itself**: everything
needed to answer those questions already ships in it, so the answer costs **one container run** on a
bare host and needs no Kubernetes, no `Devices` object and no device-manager serving.

It reaches the host the way Koordinator's `koord-device-daemon` does: the host root is bind-mounted
into the container, and anything that must run *as the host* is invoked through `chroot` into it. That
one mechanism gives the command the host's own container CLI — so it starts sibling containers with no
socket handed over and no CLI shipped in our image — and the host's own vendor CLI, so it can tell
*"this machine has no accelerators"* from *"this machine has eight and your container cannot see
them"*. There is no second command for the user to run.

Where a behaviour cannot be fully exercised in the environment at hand, preflight performs the
**simulated** action — driving the allocator's own code to build the allocation and its injection, and
asserting on what that produced — and labels the result *simulated* rather than *measured*, so an
assumption is never read as evidence.

Motivated by [#134](https://github.com/gpustack/gpustack-operator/issues/134), which asked for the
allocation-time precondition read; this spec takes the wider goal that issue was a case of.

## Motivation

### Goals

- **Self-contained verification.** Everything needed to answer the questions below ships in the
  operator image. No cluster, no CRDs, no NFD labels, no kubelet registration, and no running
  device-manager — the libraries are reused, the serving is not.

- **Answer three questions, in order**, for every manufacturer asked about:

  1. **Can the devices be detected here?** Driver present, library loadable, bus enumerable, and what
     the detector makes of what it found — cross-checked against what the host's own vendor CLI
     reports, so a container that cannot see hardware that is plainly there says exactly that. This is
     the floor: every later answer is meaningless without it.
  2. **Can the devices be sliced?** Logical and physical slicing, at two depths — what the driver
     *declares* about the capability, read through the same seams the allocator uses at `Allocate`
     time; and whether a container *actually gets* a slice, with a quota that binds.
  3. **Can the devices be managed while sliced?** The behaviours GPUStack layers on a slice — an
     SSH-enabled Instance's sidecar co-allocated onto its owner's accelerator, two tenants coexisting
     on one card — each exercised, or simulated and labelled as such.

- **One command, no follow-ups.** The user runs one `docker run` / `nerdctl run` and everything that
  can happen, happens. Nothing is handed back to them to paste and re-run in the normal path.

- **Reach the host through the host, not through a hole punched for us.** The host root is bind-mounted
  and entered with `chroot`, so preflight uses the host's *own* container CLI and the host's *own*
  vendor CLI. No runtime socket is mounted, no CLI is shipped in our image, and a parsed CLI is always
  the exact version that matches the daemon it is talking to.

- **Honest labelling.** Every answer says whether it was **measured** (something ran and was observed),
  **simulated** (the allocator's own code produced the artifact and it was asserted, but nothing was
  changed on the hardware) or **declared** (the driver was asked and answered). A reader can always
  tell which. *Simulated* is only truthful because the vendor's driver seam is substituted for the
  pass — several allocators write host state at `Allocate` — so a manufacturer with no such seam has
  no simulated depth at all, and says so.

- **Manufacturer-neutral.** Nothing in the command's design privileges a manufacturer. Each contributes
  through one seam, and a manufacturer that cannot answer a question says so in words rather than
  passing by omission.

- **Both container runtimes.** `docker` and `nerdctl`/containerd, including a containerd socket at a
  non-default path, resolved from the host's own filesystem.

- **Measured on hardware.** Acceptance requires real runs on NVIDIA, AMD and Ascend hosts, with the
  command line and its output recorded on the pull request.

### Non-Goals

- **Not folded into `detect`.** `detect` is a pure read that is safe to run anywhere. Preflight can
  turn on host driver state and can start privileged containers, so it must not hide behind a name that
  promises a read. `detect` remains the answer to question 1 and preflight reuses it.

- **Not a health check, a controller, or a Kubernetes Job.** One-shot CLI. The point is bring-up
  *before* there is a cluster, so needing one would defeat it.

- **Not a replacement for the e2e suite or for the slicing-shim regression cases.** Three tiers already
  exist and their subjects do not overlap: CI builds every `xbuild-*` stage on each pull request, so it
  gates whether the shims **compile**; the `gpustack-operator-xbuild-and-verify` skill runs white-box
  cases against a *built stage image* on real hardware, so it gates whether the shims we compiled
  **behave** — asserting on their own debug output, symbol manifests and mask conformance; this command
  gates whether **a user's host** can serve what the product offers, black-box and product-level. CI
  cannot reach the second tier (it has no accelerators) and the skill must not reach the third (it is
  someone else's machine). See [The boundary with the xbuild
  skill](#the-boundary-with-the-xbuild-skill).

- **No hardware partition is created this round.** Physical slicing is answered at the *declared* depth
  and, where a partition already exists, at the *measured* depth. Creating one changes host state that
  needs reliable teardown, and a preflight killed mid-run would leave an ownerless partition behind.

- **Preflight's own code never runs in host context.** It stays in the container, with its own
  toolchain and libraries; only the host's own executables are invoked through `chroot`. Copying our
  binary onto the host and running it there would make one command line serve every manufacturer, but
  it stakes the whole command on the host's glibc being new enough — a silent loader failure that reads
  like "no devices". Rejected for now, recorded in [Alternatives](#alternatives).

- **Not a guarantee about allocation time.** It reports what the host says and does now.

- **No new Kubernetes API, CRD or status field.** The result goes to stdout and nowhere else.

## Proposal

An operator on a bare accelerator host runs one command and learns what that node can serve, and for
anything it cannot serve, why. A developer landing support for new hardware runs it to find out, before
writing a single manifest, which of the three questions that hardware answers.

### The three questions

| # | Question | Depth it can be answered at | What it costs |
|---|---|---|---|
| Q1 | Are the devices detected? | *declared* — the detect pass, plus the host's own vendor CLI as a cross-check | one process |
| Q2 | Can they be sliced? | *declared* (driver read) → *measured* (a container gets a slice) | declared: one process. measured: one container per accelerator |
| Q3 | Can they be managed while sliced? | *simulated* (the allocator's own artifacts) → *measured* (containers co-allocated and observed) | simulated: one process. measured: two containers per accelerator |

Each question is answerable on its own, and a deeper depth is opt-in. A run that can only reach
*declared* is a useful run; it just says so.

### How it reaches the host

One mechanism, taken from Koordinator's `koord-device-daemon`
(`pkg/device-daemon/resource/utils.go`, which runs `chroot /hostfs nvidia-smi …`): the host root is
bind-mounted into the container, and anything that must run as the host is invoked with `chroot` into
that root. Measured on two hosts before this spec was written — see
[Notes](#notes--constraints--caveats) for the exact findings.

- **The host's container CLI.** `chroot <hostroot> docker …` / `nerdctl …` / `ctr -a <socket> …` uses
  the binary and configuration the host already has, talking to the daemon it already belongs to. No
  socket is mounted into our container, nothing is shipped in our image, and the CLI's output format
  always matches the daemon's version.
- **The host's vendor CLI.** `chroot <hostroot> nvidia-smi -L`, `npu-smi info`, `rocm-smi` and their
  siblings answer even when our container has no device mounts at all, which is what lets Q1
  distinguish absent hardware from a mis-mounted container.
- **The host's filesystem.** Staging the injected library tree is a write through the same root.

**Preflight's own code does not go through it.** Our binary is not reachable inside the chroot, and
copying it there would stake everything on the host's glibc. The in-process reads — the cgo bindings
behind Q1's detect pass and Q2's declared depth — keep running in the container with the device and
toolkit mounts the DaemonSet already documents. The split is the same one Koordinator draws.

### The boundary with the xbuild skill

The repository already verifies the slicing shims at two tiers, and this command is a third. Naming
the boundary here is what keeps the three from growing into each other.

| | CI (`ci.yml` → `_image.yml`) | `gpustack-operator-xbuild-and-verify` | this command |
|---|---|---|---|
| subject | our Dockerfile | the shim **we compiled** | the **user's host** |
| answers | does it build | does what we built behave | can this machine serve what we offer |
| who runs it | a robot, every pull request | a maintainer, when `pack/` or `csrc/` changes | an operator, at bring-up |
| the variable | our code | our code | **their hardware** |
| assertions | exit codes | white-box — the shim's own debug output, symbol manifests, mask conformance, `dlsym` hooks, reclaim under `SIGKILL` | black-box — the quota binds, the sidecar sees what its owner holds |
| target image | — | a built `xbuild-*` stage image | a vendor runtime image |
| ships? | — | no, it lives in `.claude/skills/` | yes, in the operator binary |

CI cannot reach the second tier: it has no accelerators. The skill must not reach the third: it is
someone else's machine, and its cases are tied to a build stage that a user has no reason to have.

**One thing is genuinely shared, and its direction is settled: the injection contract.** The skill's
cases reproduce the allocator's injection *by hand* — `cases/ascend-case-2.sh` writes the six fields
of `renderNPUInfoConfig` and cites that function by name; the NVIDIA case hand-writes `ld.so.preload`
and the two quota variables. Checked while writing this spec, the copy is still faithful. Nothing
enforces that: on the day the allocator emits a seventh field, those cases stay green while verifying
a contract that no longer exists.

This command derives the injection from the allocator's own code (F11) and can print it as a runnable
command (F10). That makes it the one place the contract is authored, and gives the skill something to
consume instead of a copy to maintain. It merges nothing, and it retires the drift.

### User Stories

#### Story 1

As an operator bringing up a new accelerator node, I want one command that tells me whether the node
can be detected, sliced and managed, so that I learn it from a table rather than from a Pod that
started and then failed with an error naming neither the accelerator nor the capability.

#### Story 2

As an operator who got "no accelerators found", I want to be told whether the machine has none or
whether my container could not see the ones it has, so that I stop debugging the wrong layer.

#### Story 3

As a developer adding support for new hardware, I want to know which of the three questions that
hardware answers before I write any manifest, so that the unknowns are found on a bare host rather than
three layers up in a scheduling chain.

#### Story 4

As an operator whose node runs containerd and no docker, I want the same command to work unchanged, so
that the most common Kubernetes node shape is not the unsupported one.

#### Story 5

As an operator triaging a node carrying live workloads, I want the command to change nothing unless I
say so, and to tell me plainly which of its answers were measured and which were assumed, so that
running it is never itself a risk and reading it never overstates what is known.

### Core Features & Acceptance Criteria

#### F1 — the `preflight` subcommand

A sibling of `detect` and `monitor` under `device-manager`, never a flag on either.

- `gpustack-operator device-manager preflight --help` lists it and its flags.
- It follows the same option shape as its siblings — `Validate` → `Complete` → `Config.Apply` — and
  emits one YAML document on stdout.
- It accepts `--manufacturer` and `--no-pci-check` with the meanings `detect` gives them.
- It needs no Kubernetes access, no `Devices` object, and starts no device-plugin server.

#### F2 — Q1, detection and the host cross-check

- Reuses the detect pass rather than reimplementing it, so the two can never disagree.
- Reports, per manufacturer: detected / detected nothing / the pass could not measure — three different
  facts, never collapsed.
- **Accelerators that answered but carry no group name are a failure, not a detection.** The name is
  what the group's id is made of, and the whole scheduling chain names itself after that id, so an
  unnamed group is a node that cannot serve however many accelerators it counted. Counting them and
  stopping there reports such a node as healthy — measured on a Hygon node whose HSA runtime did not
  load, where the only thing telling it apart from a healthy one was a leading space in `" x8"`.
- **Cross-checks against the host's own vendor CLI** through the host root. When our detect pass finds
  nothing and the host's CLI finds devices, the answer says so explicitly and names the mounts the
  container is missing — the single most common bring-up mistake, and one `detect` cannot diagnose
  today.
- A manufacturer that fails here is reported with its remaining questions marked unanswerable, not
  silently skipped.

#### F3 — Q2 declared: the driver read

- One row per accelerator per capability, carrying the accelerator's ID, the capability's name in the
  manufacturer's own vocabulary, the state, a detail when the driver answered, and a reason when it did
  not.
- Three states, exhaustive and mutually exclusive, each with a different consequence for the allocation
  the capability guards:

  | state | meaning | what an allocation does |
  |---|---|---|
  | `ok` | the capability was read and the accelerator can serve the mode | proceeds |
  | `unavailable` | the driver could not be asked — entry point missing, library not loaded, no privilege | is refused |
  | `not-declared` | there is no such capability here to read or to set | proceeds without it |

- The classification mirrors the allocator's own, so a preflight row and the allocation it predicts
  cannot disagree.
- The reason is the driver's own message. A row that is not `ok` never carries an empty reason.
- **`not-declared` is a verdict about the hardware, so it is never reached from the node's own record
  alone.** For the partitioned capability that record is `AcceleratorPhysicalSliced.Profiles`, and the
  detect pass publishes it empty for an accelerator that offers no profile *and* for one whose
  catalogue it could not read — the failure is logged and the inventory is published short. The
  preflighter therefore asks the driver even where nothing is declared, and it is a different question
  than the one detection asked: the allocator's own accessor fails outright on a profile id it cannot
  read, where detection skipped it. An error there is `unavailable`, and so is the contradiction of an
  accelerator that declares nothing while the driver reports live partitions on it. `not-declared` is
  left to mean what it says, and the two ways a short inventory arrives stop exiting zero.

#### F4 — establishing a driver mode rather than only reading it

- A capability that is *off* is not the same fact as a capability that *cannot be turned on*, and
  only the second is a node that will fail an allocation. So where a manufacturer's sharing mode is
  read as off, it is asked on — exactly where the allocator would ask, and **not** where the
  allocator refuses to touch the device: never on a generation that declares no such capability, and
  never on a driver whose entry point is absent.
- **It is put straight back.** The window is a read and a restore apart, and the row says the flag
  was asked on and returned. A restore that failed says so in the row rather than leaving the reader
  to discover the card was left enabled.
- Nothing is established that has no reversal. MetaX's subdevice model is a construction rather than
  a flag, so that manufacturer stays a pure read.
- **`--dry-run` withholds the ask**, and the row says the mode was read but not established. Asking
  is a write however briefly the mode is held, and a restore that fails leaves the card enabled — so
  a flag promising to write nothing to the host has to reach the manufacturer, not just the container
  step. The caller cannot withhold it on the manufacturer's behalf: only the manufacturer knows which
  of its reads is also an action.

#### F5 — Q2 measured: a container gets a slice

- For each accelerator reported servable, start a container **through the host's own runtime CLI** with
  the vendor runtime image and the injection an allocation would emit — environment, mounts and device
  nodes — and assert inside it that the slicing runtime loaded and that the memory the vendor's own
  tool reports is the requested quota rather than the whole card's figure.
- The container's own output is carried into the result as the evidence for the verdict — including
  where the container could not be started, since whatever it managed to print is the operator's lead.
- **A container that could not be started is not `unavailable`.** It is an environment this pass could
  not measure in, not an accelerator that cannot be sliced, and only the second may exit non-zero: an
  air-gapped node, or one whose image pull fails for want of host networking, is not a broken node.
- Every container preflight starts is removed, including on failure. So is everything a responder
  rendered onto the host — but the tree it is promoted into is preflight's own
  (`deviceplugin.OperatorPreflightDir`), not the pod directory beside it. An allocator reads the pod
  directory as its record of what other Pods hold, so an entry there under a Pod UID no kubelet
  scheduled would be counted as occupancy and shift a real allocation off its placement. Removing
  what was promoted is therefore hygiene rather than correctness, which it has to be: the removal is
  a deferred call, and no deferred call runs after a SIGKILL.
- **The two assertions are two rows, and both are answered against what the injection itself says.**
  They fail separately and mean different things — a runtime that did not load leaves the container
  the whole card and no cap at all, while a cap that was applied but not observed is a slice that is
  probably enforced and was not seen to be — so one row carrying both would have to pick which to
  report. Neither row parses a vendor tool's grammar: the first looks for the shared objects *this*
  injection mounts in the container's own `/proc/self/maps`, and the second for the memory figure
  *this* injection set. That is what makes a verdict specific to the injection under test rather than
  to a plausible-looking output, and it is what keeps four output formats out of this package.
- **What runs inside the container is one shell line, not a vendor command alone.** `/proc/self/maps`
  is read first because `/etc/ld.so.preload` puts the slicing runtime into every dynamically linked
  program's address space, so `cat` reading its own maps is load evidence in any image with no vendor
  tool at all. Standard error is merged in because the slicing runtime says which cap it read there
  and nowhere else — which is why preflight adds the runtime's own log-level variable on top of the
  injection. And the reader's exit status is swallowed into a line of output: every one of these
  monitors exits non-zero in a container that has allocated nothing, which is exactly the container a
  preflight starts, so treating it as a verdict would report a working slice as a broken node.
- **Two manufacturers need the vendor OCI runtime passed through** (`--runtime nvidia`,
  `--runtime ascend`): their injection names devices through a visibility variable and relies on that
  runtime's hook to bring the user-space driver in. Without it the container gets device nodes and no
  driver, the slicing runtime fails to initialize against a library that is not there, and a node
  where slicing works is reported as one where it does not.
- **Only the logical-sliced mode is measured.** A partitioned probe would need a hardware partition
  that does not exist yet, and creating one is `ActuatePhysicalSliced` — the one method F11 forbids
  driving. Partitioned therefore has no measured depth this round, consistently with MIG being read
  and never created.
- The per-manufacturer values this needs — the log-level variable, the reader, the vendor runtime and
  where the cap is carried — are taken from that manufacturer's case under
  `.claude/skills/gpustack-operator-xbuild-and-verify/cases/`, which measured them on its own
  hardware. A test pins the log levels against those cases and the readers against the container
  paths the manufacturer's allocator mounts them at.

#### F6 — Q3: management behaviours on a slice

Each behaviour is a named case, answered at the deepest depth the environment permits:

- **Sidecar visibility** — the SSH-enabled Instance shape. The allocator serves an internal
  `<manufacturer>.visibility` request by reusing the device(s) the owner container already holds,
  naming the partition for a partition-backed owner and the accelerator otherwise. Preflight drives the
  same two allocations in the same order the kubelet makes them and asserts the second names **nothing
  the first was not granted** — *simulated* on the artifacts alone, or *measured* by starting both
  containers and observing what each sees. Containment rather than equality, and the difference is not
  a relaxation: what this case guards against is a sidecar reaching an accelerator its owner never
  held, which is the sidecar naming something extra. Naming *fewer* things is a different fact, and a
  legitimate one — measured on AMD hardware, the owner carries `ROCR_VISIBLE_DEVICES` because its
  sliced response adds it while the visibility response the sidecar is served does not, and both name
  the same card through the same device nodes. An equality test would report that working node as
  broken. The shortfall is still reported, as a detail on a passing row rather than a failure.
- **Co-tenancy** — two independent slices on one accelerator, allocated and (at the measured depth)
  started together, each seeing its own quota.
- **The barrier the two tenants meet at is per accelerator, and is emptied before they start.** A
  marker in it is the whole evidence of the overlap, and a tenant reports the overlap the instant it
  sees its peer's — so a marker outliving its run is an overlap that never happened, believed by a
  container that has met nobody. One shared directory reported `measured` co-tenancy for every card
  after the first, on a real two-card host, and a run killed before its sweep would have done the same
  to the next run's first card.

- A behaviour that cannot be reached at the measured depth is reported at the simulated depth with the
  reason it could not go deeper — never as a failure, and never as a pass.
- **Sidecar visibility never reaches the measured depth, and that is a property of the probe rather
  than of the node.** A measured sidecar needs the owner's container still running when the sidecar
  starts, and every container this command starts is one-shot: it prints its evidence and exits, so
  there is no owner left to co-allocate from. The pair is driven at the simulated depth — one
  responder, one redirect, the owner first and then the sidecar handed the owner's allocated map
  verbatim, which is what `ResourceServer.allocateVisibility` does — and the row says why it stopped
  there. Co-tenancy does reach measured: both of its containers are started at once, and only an
  overlap establishes that two slices can hold one accelerator together.
- **A partition-backed owner is stated, not driven**, for two independent reasons. The synthetic
  request cannot express one: `GetAcceleratableResourceName` yields the bare `.partitioned` key with
  no profile, and `partitionProfileOf` matches only the `.partitioned.<kind>-<profile>` shape. And
  the response it would need, `GetPhysicalSlicedVisibilityResponse`, lives on `PhysicalSlicedResponder`
  — the interface F11 forbids asserting to, because it also carries `ActuatePhysicalSliced`. So a
  partition-backed accelerator gets its two rows at the declared depth, saying in words that the
  sidecar is served the partition its owner holds and that this pass does not drive that capability.

#### F7 — the host-context seam

- The host root is bind-mounted; its path is a flag with a default, so a caller can place it elsewhere.
- Host executables are invoked through `chroot` into that root, never assumed to exist in our image.
- The container must share the host's network namespace. `chroot` changes the root and **not** the
  network namespace, so a host resolver reached over loopback is unreachable without it, and any host
  CLI that must pull an image fails on DNS. Preflight **detects this before it bites** and says so,
  rather than surfacing it as an opaque pull failure.
- A host root that is absent or not a host root is a named failure that says what was looked for.

#### F8 — depth labelling

- Every answer carries how it was reached: `declared`, `simulated` or `measured`.
- Nothing infers a deeper label than it earned. A simulated sidecar check never reads as measured.

#### F9 — the runtime resolution

- Resolves the runtime **on the host**, from the kubelet's own CRI endpoint wherever the host names
  one: that is what starts a container on this node in production, and reproducing production is the
  whole point of the container depth. A node carrying both `docker` and `containerd`, with a kubelet
  talking to `containerd`, would otherwise be probed `docker`-first and every container answer would
  describe a path no workload takes.
- The endpoint is read from the kubelet's files through the mounted host root, since this container
  shares no PID namespace with the host. The two standard paths are read first, then a drop-in under
  the distribution's own tree — a distribution that embeds the kubelet writes neither standard path.
  A host that names no endpoint falls through to probing `docker`, then `nerdctl`, then `ctr`, which
  is the honest answer for the bare machine this command is designed for.
- `--runtime` overrides the resolution, and names one of the three runtimes this command drives. One
  of the three that the host does not carry is accepted, so the no-runtime path stays exercisable; a
  name outside them is refused at flag validation, because the resolution cannot tell a typo from a
  host without that runtime and answers the second by emitting the steps and exiting zero — the right
  answer to a node this command cannot reach, and the wrong one to `--runtime dokcer`.
- Whichever way it resolved, a host that names a runtime nothing here can drive says so **in the
  resolution's own words**. "Every probe came up empty" and "the kubelet names a runtime nothing here
  can drive" are different facts about the node, and only the second tells its reader what to install.
- **A host naming more than one endpoint gets neither.** Two distribution trees are two
  configurations and only one belongs to the kubelet that is running; choosing would drive the probe
  against a socket this node's workloads may never touch, and would do it silently. The conflict is
  named and the steps are emitted — the same outcome as a host nothing here can read.
- The endpoint is parsed as the kubelet parses it: comments are not settings, and a repeated setting
  is applied last-wins. A key with nothing after it is absent rather than a value.
- A containerd socket at a non-default path is usable, since a k3s or RKE2 node carries one. Both
  containerd CLIs — `nerdctl` as well as `ctr` — are pointed at the resolved socket explicitly rather
  than left to their own default, which on such a node is a path that does not exist.
- For containerd, the namespace is explicit and named in the output, so a container preflight started
  is never left where another component collects it. This applies to `nerdctl` for the same reason it
  applies to `ctr`: its default namespace belongs to whichever component already owns it.
- No runtime on the host is a named outcome that says what was probed, and it drops the affected steps
  to the emit fallback (F10) — never a silent skip and never a result that reads as passing.
- **`ctr` cannot start a container preflight, and its steps are emitted rather than run.** `ctr run`
  offers `--env`, `--mount`, `--annotation` and `--privileged`, and no flag at all that passes a device
  node, so the only way to reach an accelerator through it is `--privileged` — which grants every
  device on the host and would report an isolation the injection never established. That is a measured
  answer that measured the wrong thing, so it is refused. `ctr` stays a resolved runtime for everything
  that does not start a container, and the emitted command names `nerdctl` against the socket and
  namespace `ctr` resolved, so it is runnable as printed on that same node.

#### F10 — emit: the command as an output in its own right

Emit is not only a fallback. It is how the injection contract leaves this command, and it has a named
consumer, so it carries acceptance criteria of its own.

- **It is the fallback** for the two cases where preflight cannot take a step itself — no container
  runtime on the host, and no host root to enter. It then prints the exact command that would have
  taken the step, and the row says the step was emitted rather than run. That is never reported as a
  failure of the node.
- **It is selectable explicitly**, for someone who wants to see what would run before letting it run,
  and for the consumer below.
- **What it prints is complete and runnable as printed** — image, mounts, devices, environment and the
  assertion — because something other than a human will run it.
- **Emit and act share one construction.** The command is built once and either executed or printed,
  never written twice; a test pins that the two agree.

#### F11 — the injection contract has one source

- The injection preflight applies is **the allocator's own**, not a copy of it: preflight builds the
  arguments a kubelet `Allocate` would supply — a synthetic Pod, container, `Devices` and allocated
  map — and calls the manufacturer's real `ContainerAllocateResponder` /
  `LogicalSlicedResponder` / `PhysicalSlicedResponder`. Those interfaces take plain values and nothing
  else, so this needs no kubelet, no gRPC and no Kubernetes client.
- **Only `GetContainerAllocateResponse` is driven.** Every one of the nine allocators implements it;
  the sliced interfaces are implemented by three of them and one of those methods,
  `ActuatePhysicalSliced`, is what creates a hardware partition. Driving the universal entry point
  alone gives one shape for every manufacturer.

  It is a rule and not a property of the type. The seam returns the allocator's own server — which
  is the point of it — and that server implements more than it is meant to be asked for here.
  `LogicalSlicedResponder` may be asserted for and is safe: it renders files, under the paths the
  redirect neutralizes, and touches no hardware. `PhysicalSlicedResponder` may not be, and nothing
  in the repository does.

- **What the universal entry point covers differs per manufacturer, and two of them are worth
  naming.** Ascend renders its logical slice *inside* `GetContainerAllocateResponse`, dispatching on
  the allocation mode, so driving that one method covers its slicing. AMD does not: its
  `GetContainerAllocateResponse` never reads the allocation mode at all, so it returns the same
  injection for every mode and AMD's slicing is reachable only through `LogicalSlicedResponder`.
  T-Head's never touches its partition driver for any mode, so substituting that driver is
  defence-in-depth there rather than something the driven path exercises. None of this is a defect;
  it is the reason a per-manufacturer vertical exists at all, and each one says which of the three
  cases it is.

- **The seam that carries the injection lives in `pkg/deviceplugin`, not `pkg/device`.** The
  injection is a `deviceplugin.ContainerAllocateResponse`, and `pkg/deviceplugin` imports
  `pkg/device` rather than the reverse, so an interface in `pkg/device` returning one would be an
  import cycle. A manufacturer's preflighter therefore returns *its own responder* — the production
  interface, not a preflight-shaped copy of it — and the runner calls it. There is no
  preflight-specific injection code path to drift.

- **The pass changes nothing because the vendor's driver seam is substituted for it.** Several
  allocators write host state inside their responder — Ascend's turns on the container-share flag,
  Cambricon's creates an sMLU instance — so calling the responder over a real driver would make the
  simulated depth an action. Each manufacturer's preflight code therefore lives *inside* that
  manufacturer's package, where the existing seam (the one its own tests substitute) can be replaced
  with a recording stand-in. A manufacturer with no such seam has no simulated depth, and F12 applies.

- **Substituting the driver is not enough on its own, and the doors are not the same for every
  manufacturer.** A responder can write host state through a second door: Ascend's sliced path
  renders `npu_info.config` under `deviceplugin.OperatorLibDir`/`OperatorPodsDir`, which is why its
  own tests redirect those package variables. NVIDIA's sliced path creates one more — HAMi-core's
  cross-process lock directory, `/tmp/vgpulock`, world-writable, held in a variable private to the
  NVIDIA package that no shared helper can reach.

  Iluvatar turned out to carry the same private lock path as NVIDIA, which is the point: the set is
  not knowable from outside the manufacturer's package.

  So the seam hands the caller a **restore** alongside the responder: each manufacturer redirects
  what it alone knows about, and the caller defers the undo. A caller cannot be asked to know what
  to redirect, and a manufacturer that redirected without restoring would leave the rest of the
  process pointing at a directory that no longer exists. The consequence belongs to F10 and F14: an
  injection built at the simulated depth carries those redirected mount paths, so emitting it or
  running it needs the staged paths instead.

  **A private path is handed to the redirect rather than moved by hand, because restoring it is only
  half of what has to happen to it.** The other half is the rewrite: the caller holds the injection
  and has to address every mount as the host does, and it cannot rewrite a path it was never told
  about. Measured on hardware — NVIDIA's lock directory came out of a dry run as
  `/tmp/gpustack-preflight-1194651507/vgpulock`, so the emitted command mounted a directory that
  exists on no node. Anyone running it would get an empty one created on the spot, coordinating with
  nothing, which is exactly the failure the co-tenancy row claims to rule out. The redirect therefore
  takes the private paths, moves them, restores them, and reports each one so the caller can put it
  back — following the chain when a second redirect opens over the first, since the path is one
  package variable rather than one per redirect.

  **Each manufacturer carries a test that drives the seam with no outer redirect of its own.** Every
  other test around the seam opens one before calling, which masks a seam that had stopped
  redirecting — so without this one, the promise is unpinned even though the suite is green.
- A test pins that the injection preflight reports equals the one the responder produces for the same
  request, so the two cannot drift.
- Emit (F10) is how that derived injection becomes available to anything outside this process — see
  [The boundary with the xbuild skill](#the-boundary-with-the-xbuild-skill) for the consumer this
  exists for.

#### F16 — one report a reader can compare across manufacturers

- **Every check names the allocation mode it is a precondition *for*.** Capability stays the vendor's
  own word, because an operator debugging an Ascend node searches for `container-share` and not for a
  name this package invented — but a vendor's word cannot be compared with another vendor's.
  `container-share` and `cu-mask-topology` are two vendors' words for one question, *can this
  accelerator be logically sliced?*, and without the mode beside them a reader cannot tell that, nor
  see which mode nothing on the node answered for.
- The mode is rendered from the allocator's own enum rather than written by hand, so a mode renamed
  there is renamed in the report. It is carried as a string: the enum is a `uint32` with no marshaller
  of its own, and putting it in the report verbatim prints `mode: 3`.
- **A check that names no mode is reported as naming none**, and says so in its reason. Inventing one
  would file the row under a mode nobody established it belongs to; leaving it blank would let it be
  read as comparable to the rest. The registry is a static map a vertical adds a line to, so this is
  enforced at the boundary rather than trusted to each manufacturer.

#### F17 — a manufacturer that crashes does not take the run down

- **The whole value of this command is answering on a node that may be broken**, and everything it
  reaches for a manufacturer is cgo over a vendor driver a half-installed node can leave in any state.
  One nil dereference in one vendor's library would otherwise hand the operator a stack trace instead
  of the other eight verdicts — on exactly the node they most needed them for.
- A panic is contained to the manufacturer that raised it. That group discards whatever the crashed
  pass had filled in and reports **one `unavailable` row per accelerator it was asked about**, plus a
  note carrying what the panic said; the stack trace goes to the log, not the document.
- The group is rebuilt rather than patched: whatever the crashed pass had filled in describes a
  reading that never completed, and reporting half of it as a verdict is worse than reporting none.
- **The rows are what make it exit non-zero, so they are not optional.** `Failed` reads states, not
  words: a group carrying a completed detection, no rows and a note would be a manufacturer whose
  vendor code died while the command exited zero — the one outcome automation cannot see past, and
  worse than the crash itself.
- This is a floor, not a licence. A panic is a bug in the vertical that raised it, and the note says
  so in terms that ask for it to be reported.

#### F12 — manufacturers and capabilities with nothing to check

- A manufacturer whose allocator has no driver seam reports, in words, that nothing is read for it.
- The same applies to a capability an accelerator does not declare, a manufacturer with no accelerator
  detected, and one whose detect pass could not measure — four different facts, four different
  sentences.
- An empty result is never emitted for any of them: an empty list reads as a node that passed.

#### F13 — the probe image

- A per-manufacturer default, derived from the accelerator family the detect pass reported, since one
  tag cannot be right for every family of a manufacturer.
- `--probe-image` overrides it.
- A default that cannot be resolved for the detected family is a named failure that points at
  `--probe-image`, not a guess and not a false negative.
- **Having a container probe and having a default image are different things, and T-Head has the
  first without the second.** A probe image has to carry a glibc no older than the one that
  manufacturer's shim was compiled against, and this repository builds T-Head's in a vendor devel
  image whose floor it does not pin — where AMD's is compiled in a ROCm image but links only glibc,
  which is what let its default be a plain Ubuntu tag. So a T-Head measured run names `--probe-image`
  until T11 establishes that floor on the hardware. That is the flag doing its job, not a gap.

#### F14 — staging the injected libraries

- The injection mounts the slicing runtime from a host path that an init container normally stages from
  the image. A standalone container run has no init container.
- Preflight stages the in-image tree onto that host path through the mounted host root, or drops the
  affected steps to the emit fallback naming what it could not write. It never produces an injection
  whose mounts point at nothing.
- **It is the live tree, deliberately, and that is also the sharpest edge this command has.** The
  probe has to load what an allocation loads; staging into a directory of preflight's own would
  measure a copy no allocation mounts, and would print an emitted command naming a path production
  never uses. The cost is that a write outlives the run: on a node with a device-manager already
  installed, a preflight run from a *different image version* replaces that version's preload
  libraries for every allocation afterwards — a working node changed by a command that presents itself
  as reading one. Nothing in the code prevents it; the operator page carries the rule (run the tag the
  device-manager runs) and the risk register below carries the hazard. Staging under a preflight-owned
  root and rewriting the probe's mounts to it is the alternative, and it trades this hazard for a
  measurement one step further from the thing being predicted — **an open decision, not a settled
  one**.
- **Staging is part of taking the step, so `--dry-run` stages nothing.** `--dry-run` exists to show what
  would run before anything does, and writing a tree onto the host is something. The row for an
  emitted step therefore names the one thing its reader still has to do; the three *fallback* emits
  — no runtime, no host root, `ctr` — did stage, and say nothing about it.
- **The simulated pass's own rendered files are staged the same way.** A responder answers inside the
  redirect its own package opened, so what it renders lands in a scratch directory that is removed the
  moment the redirect is restored — that is what keeps the simulated depth from being an action. A
  container started against the injection as the responder built it would mount paths that no longer
  exist, so what the responder rendered is copied onto the host through the mounted host root and
  every host path in the injection is rewritten onto the location the host knows it by. A path under
  neither redirected root comes through untouched. The copy lands under
  `deviceplugin.OperatorPreflightDir` and the rewrite points there, so nothing preflight writes ever
  enters the tree an allocator reads.

#### F15 — output and exit code

- One YAML document on stdout, encoded like `detect`'s, with every requested manufacturer present
  whatever it had to report, and every group carrying the time it was read.
- Exit non-zero when any answer is a failure — a capability `unavailable`, or a case that ran and did
  not pass.
- `not-declared`, "nothing is checked here", "no accelerator detected", "could not go deeper" and
  "emitted for you to run" are **answers, not failures**, and do not affect the exit code.

#### F16 — documentation

- `docs/architecture/device-discovery.md`, in the allocator section: what preflight reads, the three
  states, the three depths, how it reaches the host, and why it is not part of `detect`.
- An operator page under `docs/operation/` carrying the runnable command line, what each mount and flag
  is for, and what the command starts and removes.
- A reference page under `docs/reference/` covering **every** command the binary offers, not only this
  one. The repository documents none of them today, so a user has no way to learn what the binary can
  do short of `--help`; adding a command an operator is meant to run by hand without that page would
  leave the most useful thing here the least findable.
- A pointer from the bug-report issue template to that page, asking for the `preflight` output in the
  reporter's environment block. Most of what this command answers is exactly what a
  "my accelerator is not working" report is missing, so the template is where it reaches the people
  who need it — and it turns a report into a diagnosis the reporter can often finish themselves.

### Verification

Acceptance is not met by unit tests alone: no unit test can establish that a preload loads or that a
quota binds. Three hardware runs are required, with the command lines and full output recorded on the
pull request. The hosts' addresses stay out of this spec and every other committed artifact.

| host | shape | what it establishes | what it cannot |
|---|---|---|---|
| **NVIDIA** | one consumer card, **no Kubernetes at all**, carrying **both** docker and nerdctl | the bare-host target scenario end to end; the host-context seam driven against **both** runtimes on one machine; Q1 including the cross-check, Q2 declared + measured, Q3 sidecar and co-tenancy | physical slicing beyond `not-declared` — a consumer card has no MIG, which is itself the correct answer to assert |
| **AMD** | two cards, its own single-node k3s, docker plus containerd at **both** the default and the k3s socket path | multi-card co-tenancy; containerd socket selection at a non-default path, with the resolved socket carried into the command as `--address`; `--runtime ctr` taking the emit path rather than starting a container (F9); Q1–Q3 | — |
| **Ascend** | eight cards, **arm64**, docker only | the toggle-and-restore on a capability that is genuinely off; a non-amd64 architecture; Q1–Q3 | the nerdctl arm of the runtime resolution |

Together the three cover both runtimes, both architectures, single and multi card, cluster and
no-cluster, and all three depths.

### Notes / Constraints / Caveats

- **The host-context mechanism was measured before this spec was written**, on the NVIDIA and AMD hosts
  above, from a stock `alpine:3` container carrying neither a container CLI nor any vendor library:

  - `chroot <hostroot> docker version` and `chroot <hostroot> nerdctl --version` both answer with the
    host's own versions;
  - `chroot <hostroot> docker run …` and `chroot <hostroot> nerdctl run …` both start **sibling**
    containers, with **no runtime socket mounted**;
  - `chroot <hostroot> ctr -a /run/k3s/containerd/containerd.sock version` reaches a **non-default**
    containerd socket;
  - `chroot <hostroot> nvidia-smi -L` and `chroot <hostroot> rocm-smi` report real cards **with no
    `/dev` mount at all**, which is what makes F2's cross-check possible;
  - `chroot` itself does **not** require `--privileged` — the default container capability set already
    carries `CAP_SYS_CHROOT`.

- **Host networking is required, not optional.** `chroot` changes the root and not the network
  namespace. A host whose `/etc/resolv.conf` points at a loopback resolver — the systemd-resolved
  default — leaves a chrooted process reading the host's resolver address from inside the container's
  network namespace, where nothing is listening; measured, a host CLI asked to pull an image dies on
  DNS. Sharing the host's network namespace fixes it. This is why Koordinator's device-daemon
  DaemonSet carries `hostNetwork: true`.

- **The privilege posture is unchanged, not escalated.** Mounting a container runtime's socket already
  grants host root — anything holding it can start a privileged container mounting the host root
  itself. Entering a bind-mounted host root is the same grant, arrived at more honestly, and it removes
  the socket mount rather than adding to it.

- **Real-world edges of mounting the host root.** SELinux-enforcing hosts need the mount relabelled;
  some hardened hosts forbid mounting `/` into a container at all. Both are named failures with the
  remedy, not silent ones.

- **A slice needs a vendor runtime image for some manufacturers and not for others.** Ascend's injected
  `ld.so.preload` resolves against libraries only a CANN image carries, so a bare base exits `127`;
  AMD's shim links only glibc, so any base works. The defaults must reflect that difference rather than
  assume one rule.

- **A capability absent from an API generation is a normal case, not an anomaly.** Manufacturers ship
  generations whose API declares no counterpart for something an earlier one had — Ascend's V2 DCMI
  generation declares no container-share flag, and other manufacturers will do the same. That is
  exactly what the `not-declared` state is for, and it is why the depth labels matter: on such a
  generation only the measured depth can say whether the behaviour works anyway.

- **Language and idiom.** Go with cobra, following `newDetectCommand`'s shape exactly: an `Options`
  with `AddFlags`/`Validate`/`Complete`, a `Config.Apply`, and `yaml.NewEncoder(os.Stdout)` with
  `SetIndent(4)`.

- **No new Go module dependency, and no new binary in the image.** Host executables are invoked through
  the host root; nothing is linked and nothing is shipped.

- **The vendor driver reads are linux-only** (`_linux.go` / `_other.go`), and no CI job builds or tests
  linux Go. The seam is verified by compiling in a `golang:1.26` container.

- **A first, unspecified driver-read implementation exists** on branch `feat/devicemanager-preflight`.
  It is input to this spec, not a commitment: what survives is decided by the plan, not by having been
  written first.

### Boundaries

- **Always:**
  - name every write in the rows, and undo the ones that are not the answer: the default stages the
    library tree, drives each responder, toggles a driver mode and puts it back, and starts probe
    containers — `--dry-run` is the read-only mode, and it is the only one;
  - act on nothing beyond the manufacturers and capabilities the user asked about;
  - label every answer with the depth it was reached at;
  - report every requested manufacturer and every capability, including the ones nothing is read for,
    in words;
  - carry the driver's, the host CLI's or the container's own words as the reason;
  - remove everything preflight started, including on failure;
  - keep a preflight answer and the allocation it predicts in agreement.

- **Ask first:**
  - any run that establishes a driver mode on real hardware, rather than only reading it;
  - any measured-depth run on a node carrying live workloads, since it starts privileged containers;
  - any change to the chart's mounts or to the operator image's contents.

- **Never:**
  - fold this into `detect`;
  - report something with nothing to check as passing;
  - create a hardware partition this round;
  - leave a container behind;
  - label a simulated answer as measured;
  - run preflight's own binary in host context;
  - write a host address into this spec, the docs, a commit message or a pull-request body.

### Risks and Mitigations

- **A detector that failed to enumerate reads the same as a node with no such hardware** → several
  detectors log the failed device-count call and return `(nil, nil)` regardless, so the manufacturer
  never reaches `unmeasured`, `detection` reports `not-declared`, and the run exits zero on a node
  whose driver is broken — the exact node this command exists for. → *Mitigation this round:* partial,
  and only by side effect. The host cross-check catches it for the three manufacturers that have one
  (NVIDIA, Ascend, AMD) by finding cards the container could not; the other six carry no host CLI here
  and are missed entirely. The fix belongs in the detectors, where it also changes what `detect`
  reports, so it is not made here.

- **The driver-mode toggle races a live allocation** → Ascend's `container-share` and Cambricon's sMLU
  mode are read, asked on, and put back within one call, and nothing serialises that against a
  device-manager allocating on the same card. An allocation landing in the window is turned off under;
  one that observes preflight's `on` may skip its own write and then lose it. → *Mitigation this
  round:* none in code — the toggle is documented as a write, and the runbook says to run preflight
  before the node serves. Coordinating the two needs a lock both processes take, which is its own
  piece of work. The eight-card Ascend acceptance run was made on a node carrying live workloads and
  did not hit it, which is luck rather than evidence.

- **Two preflights on one node fail each other** → a different race from the one above, and this one is
  closed. Every probe container carries one label, so the second pass's stale-container sweep removes
  the first pass's *live* probes — and an accelerator whose probe was killed mid-measurement reports as
  unable to slice, which is a healthy node failed by a second operator rather than by its hardware. The
  two also share one rendered-artifact tree and one barrier root, both keyed by a Pod UID this command
  fabricates the same way every time. → *Mitigation:* a host lock, taken before the sweep and **refused
  rather than queued** — the useful answer to "a preflight is already running here" is that sentence,
  not a prompt that hangs until the other one finishes. It is a descriptor lock rather than a lock
  file's existence, because the failure this command is built around is a pass that dies without
  cleaning up: the kernel releases a descriptor whatever killed the process, while an existence check
  would leave a node permanently refusing to be preflighted after one SIGKILL. A pass with **no usable
  host root** has nowhere to put the lock, and is downgraded to a dry run rather than allowed to proceed
  unlocked — the mode toggles are direct driver calls that an unmountable host root does not stop, and
  staging writes into whatever path it was pointed at regardless: measured on an AMD host, a pass given
  a path that was merely a directory created eighteen entries under it.

- **Preflight run from an image the installed device-manager does not match** → staging replaces the
  live preload-library tree, so that image's libraries serve every allocation afterwards, on a node
  that was working and was only meant to be read. → *Mitigation this round:* documentation only. The
  operator page states the rule and names the write as one that outlives the run; nothing in the code
  refuses it, and nothing detects that the tree already there came from another version. **Left open**
  because the alternatives are not free: staging under a preflight-owned root moves the measurement
  one step away from what an allocation mounts and makes an emitted command name a path production
  never uses, while refusing to overwrite an existing tree measures whatever is on the node — the more
  faithful answer — but can then mount a tree missing a file this image's own injection names, which
  is the confusing failure staging exists to prevent. Choosing between them is a decision about what
  this command is for, not a defect to patch.

- **A container that died before its first line reads as one that never started** → the probe script
  opens by printing a marker, and a marker-less failure has to be sorted into "the runtime refused"
  (emit the step) or "it ran and something in it died" (report it). The injected preload loads into
  every process in the container, the shell included, so a shim that aborts as it is loaded leaves no
  marker at all — measured with a library whose constructor calls `abort`: exit 139, no stdout, no
  stderr. → *Mitigation this round:* partial. The exit status is consulted, against the status each
  runtime keeps for its own refusal — measured, because the two disagree: docker answers 125 for an
  image it could not pull, nerdctl answers 1 for that *and* for a container it could not create, and
  both pass a container's own status through unchanged. **What that leaves open is nerdctl's 1**,
  which is also a status a container process can exit with, so a slicing failure that exits 1 under
  nerdctl is still read as a refusal. Closing it means keeping enough runtime state to tell "never
  started" from "started and exited 1" — `create`/`start`/`inspect` rather than one `run --rm` — and
  that changes the shape of the command this command prints, which is its own piece of work.

- **A sidecar that reaches nothing reads as one that reaches its owner's accelerator** → the
  visibility check asks for containment, because a sidecar naming *more* than its owner is the escape
  F6 guards against, and naming fewer is legitimate: measured on AMD, the owner carries
  `ROCR_VISIBLE_DEVICES` that its sliced response adds and the sidecar's visibility response does
  not. → *Mitigation this round:* none. Containment accepts **any** non-empty subset, so a sidecar
  granted only a shared control node — `/dev/kfd`, which every card on the host shares — and none of
  the owner's accelerator-specific nodes is reported `ok` while reaching no accelerator at all.
  Separating the two needs a notion of which carriers actually identify an accelerator and which are
  shared or redundant, per manufacturer, which is a classification this round does not have.

- **The simulated artifacts drift from what the allocator actually emits** → a preflight passes and the
  allocation it predicted fails, which is worse than having no preflight. → *Mitigation:* drive the
  allocator's own code to produce the artifacts rather than hand-copying them, and make "the two agree"
  a test rather than a convention.

- **A container preflight started outlives it** on a crash or a kill → a privileged container is left
  running on the node. → *Mitigation:* a fixed label on everything preflight starts, plus a sweep of
  stale ones at start, in addition to removal on exit. The sweep covers every runtime that could be
  holding one, not only the one this pass resolved, and it addresses each of them the way this command
  would have addressed it had it resolved it — which is where an earlier pass under `--runtime <name>`
  put its containers. Neither half is optional: probes are created in a containerd namespace of this
  command's own choosing, so a containerd CLI asked without it looks in `default`, finds none of them
  however many are there, and reports success.
- **A sweep that could not be completed is a failure of the run, not a log line.** A leftover it did
  not remove still holds its accelerator, so the slice measured against that card comes back short and
  the report names the card as the fault — the exact failure the sweep exists to prevent, reached
  through the sweep's own silence. Each runtime whose sweep failed is named on one
  `stale-container-sweep` row per accelerator, `unavailable`, so it reaches the exit code rather than
  only the document.
- **What separates a failure from an absence is the list, not the runtime.** The sweep is two host
  executions — list by label, then remove what the list returned — precisely so the two can be told
  apart; asked as one shell line, both come back as one non-zero status.
  - A **removal** that failed is a failure wherever it happens, including on a runtime this pass did
    not resolve: the list already said the container is there, and the accelerator it holds is the
    same physical card whichever CLI started it.
  - A **list** that failed on the runtime this pass resolved is a failure too — that is the runtime
    the probes are about to run through, and one that cannot be asked what it holds is not one to
    measure through.
  - A **list** that failed on any other runtime is an absence established rather than a failure
    tolerated. It was addressed exactly as this command would have addressed it had it resolved it,
    so a CLI that cannot answer that address is one no earlier pass started a probe through either.
    Measured on an Ascend host: it carries `nerdctl` and a containerd socket at the RKE2 path, but the
    daemon behind that socket refuses the connection — the socket file being present is why the test
    cannot be "did we find a socket". Failing on it would hang a permanent red row on all eight of
    that node's healthy accelerators, on every run, and a red light that is always on is one nobody
    reads.

- **The measured depth is a real capability grant** — it starts privileged containers with device
  nodes. → *Mitigation:* opt-in; the operator page states exactly what gets started, with what, and
  what is removed.

- **The host root mount is refused or relabelled** by a hardened or SELinux-enforcing host → every
  host-context step fails at once. → *Mitigation:* probe the host root before using it, name the
  remedy, and fall back to emit rather than reporting the node as broken.

- **Host networking is forgotten** → host CLIs that pull images die on DNS, with an error that names
  neither the cause nor the fix. → *Mitigation:* detect the mismatched network namespace up front and
  say so, rather than letting the pull failure speak for it.

- **The default probe image cannot be pulled** in an air-gapped environment, or has no tag for the
  detected family. → *Mitigation:* `--probe-image`, and a named failure rather than a false negative.

- **A container in the wrong containerd namespace is collected mid-run** by another component. →
  *Mitigation:* an explicit dedicated namespace, named in the output.

- **Parsing a host CLI's text output** is still parsing text. → *Mitigation:* it is now always the
  version that matches its own daemon, which removes the skew between our shipped CLI and the host's;
  use JSON output where the tool offers it, and keep the parsed surface to the few fields a verdict
  needs. Concretely: an accelerator is counted by the lines carrying a per-CLI marker, never by
  counting lines, which would count the header rules these tools draw — and a manufacturer whose
  output shape is not established is left out rather than guessed at, since a wrong marker counts
  zero and a zero reads as "the host sees nothing either", which is the one answer that sends an
  operator to debug the wrong layer.

- **Three depths multiply the code paths.** → *Mitigation:* each depth is one stage with one input and
  one output type; a deeper stage consumes the shallower one's result and never re-derives it.

## Design Details

### Commands

Build, lint and test, from the worktree root:

```bash
make lint                      # whole-module golangci-lint --fix pass; run by the lead only
make lint docs                 # the docs gate, as CI runs it
make test                      # the full suite

# one package or one test
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run TestPreflight ./pkg/devicemanager/preflight/
```

The linux-only seams compile nowhere in CI, so they are checked in a container:

```bash
docker run --rm -v "$PWD":/src:ro -w /src \
  -v /tmp/gocache-linux:/root/.cache/go-build -v /tmp/gomodcache-linux:/go/pkg/mod \
  -e CGO_ENABLED=1 -e GOFLAGS=-mod=mod -e GODEBUG=gotypesalias=0 \
  golang:1.26 bash -c 'go build ./pkg/... ./cmd/...'
```

**Environment.** Build and unit tests run locally on darwin. Acceptance runs over ssh on the three
hosts in [Verification](#verification); the binary or image is built locally or on the host that is
also a native builder, never assuming a Go toolchain on the target.

The shape the command is meant to be run in, on a bare host — the host root mount and host networking
are what the host-context seam needs, and the per-manufacturer mounts are what preflight's own
in-process reads need:

```bash
<runtime> run --rm --privileged --network=host \
  -v /:/host \
  -v /dev:/dev -v /sys:/sys \
  <manufacturer driver and toolkit mounts, read-only> \
  <operator-image> \
  gpustack-operator device-manager preflight --manufacturer=<mfr>
```

A run with only `-v /:/host --network=host` still answers Q1 through the host's own vendor CLI, and
says which mounts the rest of the questions need.

### Project Structure

```
pkg/device/
  types.go                       # the shared vocabulary: the states, the depths, a check, a group,
                                 # and the optional companion interface manufacturers implement --
                                 # mirroring how AcceleratorProcessDetector companions Detector
pkg/devicemanager/
  cmd.go                         # newPreflightCommand, beside newDetectCommand
  preflight/
    option.go  config.go         # the Options/Config pair, as detector has
    preflight.go                 # the runner: detect once, then per manufacturer per question
    preflight_linux.go           # the manufacturer registry (linux)
    preflight_other.go           # the empty registry (everywhere else)
    hostexec*.go                 # the chroot-into-host-root seam, and the one construction
                                 # behind running a command and printing it
  allocator/<mfr>/
    preflight.go                 # one per manufacturer with a driver seam, beside that seam
```

### Code Style

A manufacturer implements one method and returns no error — every failure belongs to the accelerator it
happened on, so one unreadable device never hides the rest of the node:

```go
// AcceleratorPreflighter is an optional companion to Allocator, implemented only by the
// manufacturers whose allocator has a driver seam to read a precondition through.
//
// It is deliberately not part of Allocator. A manufacturer with no such seam simply does not
// implement it, which keeps "nothing is checked here" a compile-time fact that the caller
// reports in words, rather than a method that has to answer a hopeful ok at runtime.
type AcceleratorPreflighter interface {
	// PreflightAccelerator reads each precondition its manufacturer's allocator would read at
	// allocation time, over the groups given -- which are this manufacturer's and no others.
	PreflightAccelerator(groups DevicesGroupList) PreflightGroup
}
```

Conventions carried from the surrounding code: snake_case multi-word file names; exported APIs
documented with behaviour, expectations and constraints; errors handled explicitly and never panicked
on; table-driven tests with a shared execution loop, asserting observable final state; fakes over real
dependencies at every seam.

### Implementation Plan

Twelve tasks. T1 and T2 start together; T5 splits into four that run together; T6 and T7 run beside
T5. T6, T8 and T9 are serialized by real dependencies — each extends the runner's orchestration, and
each needs what the one before it produced. T12 is last because it documents what the other eleven
built and quotes the output T11 recorded.

`pkg/device/types.go` carries the shared vocabulary, and four tasks extend it: T1 establishes it, and
T3, T8 and T9 each add the field their own question needs — the host's view of Q1, the evidence a
measured check carries, the named case Q3 answers in. They therefore all name it in `Owns:`. That
costs no parallelism, because no two of the four are ever in flight together: T3 is blocked by T1, T8
by T3 through T6, and T9 by T8. The alternative — having T1 declare every field up front — would put
types nothing constructs in the tree for eight tasks, which is the speculation this repository's
conventions rule out.

The manufacturer registry in `preflight_linux.go` is a static map, matching the allocator registry
beside it. It is T1's file and no manufacturer task edits it: a manufacturer is listed there as its
vertical lands, which keeps T5's four splits disjoint — four workers each appending one line to one
map literal would conflict for no reason — and keeps every commit compiling on both platforms, since
a manufacturer cannot be listed before its own package carries a preflighter.

- [x] **T1 · Vocabulary, runner and Q1 — the tracer bullet**
      Blocked by: None
      Owns: `pkg/device/types.go`, `pkg/devicemanager/preflight/{option,config,preflight,preflight_linux,preflight_other}*.go`, `pkg/devicemanager/cmd.go`
      Gate: review
      Acceptance: `device-manager preflight --manufacturer=<mfr>` emits one YAML document carrying a
      group for every requested manufacturer; Q1 is answered from the detect pass, reused not
      reimplemented; the four reasons a group can be empty read as four distinct sentences; the exit
      code is non-zero only for a failure answer.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/... ./pkg/device/...`

- [x] **T2 · Prefactor: the synthetic allocation request**
      Blocked by: None
      Owns: `pkg/deviceplugin/preflight_request*.go`
      Acceptance: given detected groups, a manufacturer, an allocation mode and a quota, returns the
      `(*core.Pod, *core.Container, *workercore.Devices, map[Resource]int32)` quadruple the three
      responder interfaces take, with the resource names `pkg/nodefeature` produces. It is the only
      place a request is fabricated, so every manufacturer's simulated depth asks the same question.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/deviceplugin/`

- [x] **T3 · The host-context seam, and Q1's host cross-check**
      Blocked by: T1
      Owns: `pkg/devicemanager/preflight/hostexec*.go`, `pkg/device/types.go`
      Gate: review
      Acceptance: resolves the host root from a flag with a default and refuses a wrong one by name;
      executes through `/usr/sbin/chroot` by absolute path (it is not in `/usr/bin`); resolves the
      host runtime `docker` → `nerdctl` → `ctr` with an explicit socket, `--runtime` overriding
      including with a name that is absent; detects a network-namespace mismatch **before** it bites
      and names it rather than letting a DNS failure speak; Q1 reports "the host's own CLI sees N and
      this container sees none" and names the mounts that are missing.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/` with
      the exec seam faked; on hardware in T11.

- [x] **T4 · Ascend vertical: declared, establish-and-restore, simulated**
      Blocked by: T1, T2
      Owns: `pkg/devicemanager/allocator/ascend/preflight*.go`,
      `pkg/deviceplugin/preflight_injection*.go`
      Gate: review
      Acceptance: the three-state classifier is table-tested in the shape of `TestShareFlagUndeclared`;
      the establish step writes exactly where `ensureShareEnabled` writes and nowhere else — never on a
      generation that declares no flag, never on an absent entry point; the simulated depth builds the
      vendor's own server with a **recording** share driver *and* redirected host paths, calls the
      real `GetContainerAllocateResponse` and changes nothing — asserted by the recording driver
      having taken no write and the redirected directory holding what the responder wrote; an
      anti-drift test asserts the injection the preflighter reports equals the responder's for the
      same request. This task establishes the shape the other four copy.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/ascend/`

- [x] **T5 · The remaining four verticals** — splittable into four tasks with disjoint `Owns:`
      Blocked by: T4
      Owns: `pkg/devicemanager/allocator/{amd,cambricon,nvidia,thead}/preflight*.go`
      Acceptance: each follows T4's shape. AMD validates through `Topology.Validate` and reports two
      states, not three — its allocator refuses every topology failure through one path, and a third
      state would let a preflight row disagree with the allocation it predicts. Cambricon's
      establish step mirrors `ensureSMLUModeEnabled` and its simulated depth substitutes the sMLU driver,
      which otherwise creates an instance; because that instance's device node feeds the response
      payload, its anti-drift test pins parameter-level equivalence for the sliced mode and
      byte-equality for the rest. NVIDIA and T-Head read the partition subtree, create nothing, and
      establish nothing at all: their only allocation-time write actuates a partition, which
      would leave an ownerless one behind. Each carries its own anti-drift test, driving both
      responders inside one redirect window — the host root is an input to the injection, not part
      of its identity.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/{amd,cambricon,nvidia,thead}/`

- [x] **T5b · MetaX's vertical, and the three manufacturers with nothing to read**
      Blocked by: T5
      Owns: `pkg/devicemanager/allocator/{metax,hygon,iluvatar,mthreads}/preflight*.go`
      Gate: review
      Acceptance: **MetaX gets a full vertical in T4's shape.** It was missed when the five were
      chosen — the list came from an exploratory registry rather than from the allocator sources —
      and it is the manufacturer that most needed one: `sgpuManager` is a substitutable interface
      like the others, and its responder writes the *device's own sysfs* (`model=sgpu`,
      `sched_class`), so driving it unsubstituted reconfigures a card. **Hygon, Iluvatar and
      MThreads get injection-only preflighters**: their allocators read no driver at allocation
      time, so they implement `PreflightAccelerator` to say exactly that in their own words and
      `PreflightResponder` to serve the injection, which is what the simulated and measured depths
      need. No registry change is required, and none should be made: a manufacturer says what it has
      to say, rather than the runner inferring it from an absence.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/{metax,hygon,iluvatar,mthreads}/`

- [x] **T6 · Emit: one construction, two sinks**
      Blocked by: T3, T4
      Owns: `pkg/devicemanager/preflight/emit*.go`, `pkg/devicemanager/preflight/preflight.go`
      Acceptance: the command is built once and either executed or printed; a test asserts the executed
      argv and the printed line come from that one construction; what is printed is complete and
      runnable as printed, because something other than a human will run it — which means the mount
      paths are the staged ones from T7, never the redirected ones a simulated pass built the
      injection with.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/`

- [x] **T7 · Probe image resolution and library staging**
      Blocked by: T3
      Owns: `pkg/devicemanager/preflight/{probeimage,stage}*.go`
      Acceptance: a per-manufacturer default derived from the family the detect pass reported;
      `--probe-image` overrides it; a default that cannot be resolved for the detected family names the
      flag rather than guessing; staging writes the in-image tree through the host root, or drops the
      affected steps to emit naming what it could not write.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/`

- [x] **T8 · Q2 measured: run the container, assert the quota**
      Blocked by: T6, T7
      Owns: `pkg/devicemanager/preflight/measure*.go`, `pkg/devicemanager/preflight/preflight.go`,
      `pkg/device/types.go`
      Gate: review
      Acceptance: one container per servable accelerator, started through the host's own CLI with the
      injection from T4/T5; asserts the slicing runtime loaded and that the memory and compute reported
      inside are the quota rather than the whole card; every container carries a fixed label and is
      removed including on failure, and stale ones are swept at start; the container's own output is
      the evidence in the result.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/` with
      the exec seam faked; on hardware in T11.

- [x] **T9 · Q3: sidecar visibility and co-tenancy**
      Blocked by: T5, T8
      Owns: `pkg/devicemanager/preflight/manage*.go`, `pkg/devicemanager/preflight/preflight.go`,
      `pkg/device/types.go`
      Acceptance: the owner and the sidecar allocations are driven in the order the kubelet makes them,
      and the second names nothing the first was not granted — the partition for a partition-backed
      owner, the accelerator otherwise; co-tenancy places two slices on one accelerator, each seeing
      its own quota; each case is reported at the depth it reached, with the reason it went no deeper.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/preflight/`; on
      hardware in T11.

- [x] **T10 · Documentation**
      Blocked by: T8
      Owns: `docs/architecture/device-discovery.md`, `docs/operation/preflight.md`, `docs/README.md`
      Acceptance: the allocator section gains what preflight reads, the three states, the three depths
      and why it is not part of `detect`; a new operator page carries the runnable command line for
      both runtimes, what each mount and flag is for, and what the command starts and removes; the page
      is registered in the index with a label matching its H1.
      Verify: `make lint docs`

- [x] **T11 · Hardware acceptance**
      Blocked by: T9, T10
      Owns: nothing — verification only
      Gate: review
      Acceptance: **each manufacturer with a container probe is run on its own hardware, and for each
      the command line and its full output are recorded.** Nothing is inferred from another
      manufacturer: the per-manufacturer values Q2 measured depends on — the vendor runtime, the log
      level, the reader and where the cap is carried — are a different answer per manufacturer, and only
      a run on that manufacturer's hardware establishes any of them.
      Each host also carries what only it can exercise: the NVIDIA host exercises the private-path
      rewrite and the management behaviours; the AMD host exercises multi-card co-tenancy and the
      library-mount remedy; the Ascend host exercises the toggle-and-restore on a capability that is
      genuinely off, and eight accelerators, on arm64.
      The acceptance covers the manufacturers there is hardware for. Nine are registered and four can
      be run here — the three with a container probe, plus Hygon, whose tier has none — so the rest —
      T-Head included — rest on the reads and the verification cases their verticals were built from,
      exactly as they did before this task.
      Verify: `--dry-run` on each host; the recorded command lines and outputs.

      **What the runs established.** NVIDIA — one RTX 4090 D, `mig-partitioning` `not-declared`, the
      four sliced and management rows `ok` at `simulated`, and every mount addressed as the host
      addresses it. AMD — two RX 7800 XT, both cards' `cu-mask-topology` read from the driver, the
      per-card injections carrying disjoint `HSA_CU_MASK` halves. Ascend — eight 910B2, all eight
      `container-share` rows reporting the flag asked on and put back, confirmed by re-running: a
      third pass still reads `disabled` on all eight, which it could not if any earlier pass had left
      one on. A dry run added nothing under `OperatorPodsDir` on any host.

      **The kubelet branch was covered by standing a cluster up for it.** None of the three hosts ran
      a kubelet, so the CRI-endpoint resolution never fired and the acceptance would have rested on
      unit tests alone. A single-node k3s cluster on the AMD host (`testing/infra/clusters/k3s`), plus
      `nerdctl` to drive its containerd, closed that: the resolution answered
      `nerdctl --address /run/k3s/containerd/containerd.sock --namespace gpustack-preflight`, and the
      measured depth ran through it — the path a workload on that node actually takes. Both cards
      reported `sliced-runtime-loaded`, `sliced-quota-in-force` and `co-tenancy` `ok` at `measured`,
      with `libvrocm.so` mapped in the container's address space and `8184` — half of 16368 — read
      back from inside it. (The quota half of that is withdrawn below: the figure came from the
      shim's own banner rather than from the vendor reader.) The cluster was torn down afterwards.

      **The cluster paid for itself immediately**, exposing three defects a bare host could not:
      neither standard kubelet path exists on k3s, so the resolution fell through to the probe order
      and picked docker; `--dry-run` reported only that it was a dry run, hiding that the printed
      command was a fallback in a dialect that node's kubelet does not use; and the fallback's reason
      was composed rather than reported, telling a host carrying docker and ctr that it carried
      neither.

      **A Hygon node established the injection-only tier and the containment.** Eight BW DCUs, the
      first hardware any of the three injection-only manufacturers has been run on. Asked about all
      nine at once, the run reported eight `not-declared` and Hygon's thirty-three rows `ok`, exiting
      zero with a silent stderr: the tier reports what it can and the eight manufacturers with no card
      on that node cost it nothing. `--dry-run` and the real run agreed row for row, which is what a
      tier that starts no container should produce and had never been shown to; the device nodes the
      injections named were each present on the host, `/dev/mkfd` among them, which only this
      manufacturer grants; and nothing was left under `OperatorPodsDir` afterwards. Run twice — as the
      host's own binary against `--host-root=/`, and through the documented container entering the
      host root — with identical output.

      **It also established that Hygon slicing could not have worked on that node at all.** Hygon
      keeps its HSA runtime in the hyhal tree, and `binding/hsa` looked only where ROCm puts one, so
      the runtime never loaded under the mounts the DaemonSet already makes — `binding/rsmi` had
      carried the hyhal path all along, `binding/hsa` had not. The detector reads the group's name
      from the HSA agent's product name and its cores from that agent's compute-unit count, so the
      node published a group with no name, no id, and zero cores; and zero cores is what made this
      more than a naming defect, because every sliced allocation on it then resolved to zero compute
      units and was refused. Preflight is what surfaced it: thirty-two rows `unavailable`, exit one.
      With `binding/hsa` naming the hyhal tree the same node reports `bw` / `BW` / `80` and all
      thirty-three rows `ok`. Out of scope as ordered, in scope as found — this branch is what put a
      preflight on that node.

      **And it found one defect in this branch's own detection block.** Those thirty-two rows named
      the consequence, `zero compute units`, while the detection row above them read `state: ok` with
      a detail of `' x8'` — a count, a leading space where the name should be, and nothing pointing at
      the cause. Detection now fails on a group it cannot name, with a reason that says what an
      unnamed group costs. Verified from both sides on that node: without the hyhal mount
      `unavailable` and exit one, with it `ok` and exit zero.

      **A second round of runs re-established the acceptance after the review fixes.** Twenty-six
      findings were folded into this branch after the runs above, so all four hosts were re-run rather
      than the fixes being rested on their tests. Twelve of the twenty-six are exercised by a run.

      Hygon — the note a manufacturer that reads no driver writes is present alongside its thirty-two
      simulated rows. Established as an A/B against a binary carrying the pre-fix form: the two
      reports differ by exactly one line, which is the note.

      Ascend — all eight `container-share` flags read off before, between and after six real runs, and
      `-v=4` logged the on-and-back-off pair for every card, so the restore is a real write rather than
      a vacuous one. `--runtime=nerdctl` produced thirty-two probe commands, every one of them in
      docker's dialect carrying `--runtime ascend`, none carrying containerd addressing.

      AMD — two RX 7800 XT reached `measured` on `sliced-runtime-loaded`, `sliced-quota-in-force` and
      `co-tenancy`, the shim found in the mapping section while the same string also appears outside
      it, which is the section discipline doing the thing it was added for. A CRI-O endpoint was named
      unsupported, and a containerd endpoint with no nerdctl resolved to `ctr` and kept its socket.

      **Only the first of those two cards' `co-tenancy` rows is evidence.** A later review found the
      barrier directory was shared by every accelerator and never emptied, so the second card's
      tenants met the first card's leftover markers the moment they started and reported an overlap
      that had not happened. The defect is fixed — one directory per accelerator, cleared before its
      tenants start — but the run above predates the fix, and a second card's row from it establishes
      nothing. The first card's row is unaffected: its barrier was empty when its tenants started.

      **And none of the `sliced-quota-in-force` rows above is evidence either.** A second review
      finding: the vendor reader inherits `LD_PRELOAD` — it has to, since the question is what a
      capped process sees — and it inherited the raised log level too, so the shim loaded inside the
      reader's process and printed its configured cap into the reader's own section. The section
      split cannot separate those: one process printed both. AMD's `8184` was the shim's banner
      (`[vrocm] VROCM_DEVICE_MEMORY_LIMIT_0 = 8184 MiB`) while `rocm-monitor`'s own body named no cap
      at all, and NVIDIA's `12282` reached that section by the same route. The fix unsets the log
      variables after the mappings are read and before the reader runs, leaving the shim loaded and
      silent — but every quota row recorded here predates it and has to be re-run to mean anything.

      **Twice now a defect in this command has produced the evidence that would have caught it.**
      The barrier reused across cards, and the reader's section carrying the injection's own figure
      back. Both passed on hardware, both were found by reading rather than by running, and both had
      been written up here as establishing something. A measured row is worth what its isolation is
      worth, and neither of these had been argued through before it was cited.

      NVIDIA — a probe that started and then exited non-zero under the injected runtime read
      `unavailable` at `measured`, while an image that could not be pulled stayed `ok` at `simulated`.
      A container given the device nodes but not the vendor libraries read `unavailable`, the host at
      one accelerator against the container's zero. A fake `nvidia-smi` printing one card and three
      MIG partitions counted one. Staging over an existing file moved its mode both ways, `0755` to
      `0644` among them, which only a copy carrying the source's mode can do.

      **The Ascend run found one more defect, in this branch's own fallback.** `ctr` returned nerdctl
      before the vendor-runtime clause ran at all, so an Ascend node carrying containerd and no
      nerdctl was printed a nerdctl command with no `--runtime ascend` in it — the defect the review
      fixed for the direct nerdctl path, reached by the other road. The fallback now clears the same
      bar: nerdctl where nerdctl has a door to the vendor runtime, docker where it has none, and the
      reason names `ctr`, because `ctr` is what was resolved. The AMD run pins the other half — a
      manufacturer needing no vendor runtime stays on nerdctl and keeps the socket it resolved.

      **The partitioned mode was established on an H100.** Until that run the `mig-partitioning` row
      had only ever been observed `not-declared`, on a card with no MIG at all, so the half that
      reports a readable subtree had no hardware behind it. With MIG enabled the row reads `ok` /
      `declared`, and the instance count was pinned by three points rather than one: 0, then 1, then 3
      after a heterogeneous pair of profiles was added — so the figure is read from the driver and is
      not a constant. Restoring the card returned a report byte-identical to the baseline. Two things
      came out of it that no other host could have shown. **A partitioned card reports no sliced
      rows:** enabling MIG took the report from five checks to three, dropping `sliced-runtime-loaded`
      and `sliced-quota-in-force` entirely and flipping `co-tenancy` to the partitioned mode — the two
      groups are mutually exclusive on one accelerator, which nothing had recorded. **And the detect
      pass's profile list came back clean:** six profiles, every one named, the driver's `MIG ` prefix
      stripped, the `+me` variants dropped, and no `GetGpuInstance` failure — the three NVML quirks
      this binding is written around did not fire on this generation.

      **What is still not established on hardware.** The two manufacturers with a driver seam and no
      hardware here (Cambricon, MetaX), whose toggle-and-restore is symmetric to Ascend's but is not
      the same code; the T-Head PPU half of the partitioned mode; and a staging copy interrupted
      midway, which no host can be asked to do on demand. `ctr` still starts no container, by design,
      but both branches of what it prints are now established.
      Recorded on the pull request, with no host address in any committed artifact.
      Verify: the recorded runs — one command line and one output per manufacturer run.

- [x] **T12 · The command reference, and the issue template that points at it**
      Blocked by: T10, T11
      Owns: `docs/reference/commands.md`, `docs/README.md`,
      `.github/ISSUE_TEMPLATE/BUG_REPORT.md`
      Acceptance: a new reference page documents **every** command the binary offers — `worker`,
      `worker-gateway`, and `device-manager serve|detect|monitor|preflight` — with, for each, what it
      does, who runs it and when, the flags that change its behaviour, and a runnable invocation. The
      repository documents none of them today, so this is the first place a user can learn what the
      binary can do. `preflight` gets the fullest treatment, since it is the only one an operator runs
      by hand on a node, and its sample output is copied from T11's recorded runs rather than
      invented. The page stays a **reference** — what each command is — and links to
      `docs/operation/preflight.md` for the procedure, which is the split the rest of `docs/`
      already draws.
      It must also answer the question a reader will have on seeing three one-shot commands: **which
      one to run, and why the safe one is separate.** `detect` and `monitor` are pure reads that no
      flag can turn into an action; `preflight` starts privileged containers, stages libraries onto
      the host, and asks a driver mode on to see whether it takes — putting it back either way.
      `--dry-run` is how it is made a read. `detect` emits the full ledger, typed as the `Devices` CRD's
      own `spec.groups` so it can be diffed against what the cluster recorded, while preflight's
      detection is a verdict and a count and deliberately not a second ledger. A reader who wants
      both runs both, and the page says so rather than leaving them to find out.
      `.github/ISSUE_TEMPLATE/BUG_REPORT.md` gains a pointer to the page and a line in
      its Environment block asking for the `device-manager preflight` output, by **absolute** URL:
      an issue body is rendered outside the tree, and `check-docs.sh` does not cover `.github/`, so a
      relative link there is both unresolvable and unguarded. The page is registered in
      `docs/README.md` with a label matching its H1.
      Verify: `make lint docs`

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. Every seam this plan drives already exists and is already substituted by the tests around it:
each manufacturer's driver seam is replaceable from inside its own package (the pattern
`newSlicedServerWithShare` uses), and `deviceplugin.OperatorLibDir` / `OperatorPodsDir` are package
variables an existing helper already redirects to a temporary directory. What this plan adds is a
**production** caller for both.

#### Unit tests

Baselines measured on this branch, 2026-08-28:

- `pkg/devicemanager/preflight`: new package — target ≥ 80%
- `pkg/deviceplugin`: `2026-08-28` - `84.3%` — hold
- `pkg/device`: `2026-08-28` - `82.2%` — hold (types only)
- `pkg/devicemanager/allocator/ascend`: `2026-08-28` - `80.7%` — hold or better
- `pkg/devicemanager/allocator/amd`: `2026-08-28` - `80.3%` — hold
- `pkg/devicemanager/allocator/cambricon`: `2026-08-28` - `67.2%` — improve, the new preflighter's own
  tests are the opportunity
- `pkg/devicemanager/allocator/nvidia`: `2026-08-28` - `85.1%` — hold
- `pkg/devicemanager/allocator/thead`: `2026-08-28` - `86.6%` — hold

Named cases:

- the three-state classifier, per manufacturer that has one, table-driven over every return code the
  path can carry;
- the establish contract: it writes exactly where the allocator writes, and nowhere else, and puts it back — one case per
  outcome the allocator distinguishes;
- the dispatch table: four reasons a group can be empty, four distinct sentences, and none of them
  reachable by omission;
- the exit-code mapping: which answers are failures and which are answers;
- emit and act agree, from one construction;
- host-root resolution and runtime resolution, including the named-but-absent runtime;
- probe-image resolution per family, including the family with no resolvable default.

#### Integration tests

- **The anti-drift test**, per manufacturer, is the one that matters: the injection the preflighter
  reports must equal the one that manufacturer's responder produces for the same synthetic request. It
  holds two promises at once — that a preflight answer and the allocation it predicts cannot disagree,
  and that the xbuild skill can eventually consume the emitted command instead of maintaining a copy.
- **The linux seam compiles**: `go build ./pkg/... ./cmd/...` inside a `golang:1.26` container. Every
  manufacturer preflighter sits behind a `_linux.go` / `_other.go` pair, and no CI job builds linux Go,
  so nothing else would catch a break there.

#### e2e tests

The project's e2e suite runs against a live cluster, and this command's entire purpose is the state
*before* one exists — so that suite cannot cover it, and adding a case there would test a different
thing under the same name. Its end-to-end evidence is T11: one host per manufacturer that has a
container probe and hardware to run it on, covering both container runtimes, both architectures,
single and multi card, cluster and no-cluster, and all three depths, with the command line and full
output recorded on the pull request for every one.

## Alternatives

- **Fold the checks into `detect`.** Rejected: `detect` is a pure read an operator can run anywhere,
  and this establishes driver state and starts privileged containers. Putting either behind a name
  that promises a read would make a diagnostic change the node.

- **Fold `detect` into `preflight`** — the same merge in the other direction, considered once
  preflight existed. Rejected, and the reason is the one above restated: `detect`'s read-only-ness is
  a **property**, while preflight's is a **conditional**. Preflight starts privileged containers,
  stages libraries and asks a driver mode on to see whether it takes; `--dry-run` is what withholds
  all three, and a runbook or a copied command line that drops the flag turns a diagnostic into an
  action. No flag can do that to `detect`. On a node carrying live workloads that difference is worth
  a command. Two further reasons: the two produce different
  artifacts for different readers — `detect` emits the full ledger, typed as the `Devices` CRD's own
  `spec.groups` so it can be diffed against what the cluster recorded, while preflight's detection
  is a verdict and a count — and `monitor` is a third sibling of the same family, so merging one and
  not the other would leave the CLI less coherent rather than more. The two cannot drift in any
  case: preflight already reuses detect's pass rather than reimplementing it.

  The real concern behind the question — that learning *what is on this node* and *whether it works*
  should not take two runs — was weighed and deliberately left unaddressed in code: preflight does
  **not** gain a flag to carry the ledger. It is answered in the command reference (T12) instead, by
  saying plainly which command answers which question and why the safe one is separate.

- **Mount the runtime socket and ship `docker` + `nerdctl` in the operator image.** Rejected after
  measuring the host-context alternative: it needs a socket mount per runtime, a chart change to
  provide it, two CLIs in the image, and it parses output from a CLI whose version is ours rather than
  the host's. Entering the host root costs one mount and gives all of it for free.

- **Copy preflight's own binary into the host root and run it there.** It is the only way to make one
  command line serve every manufacturer with no per-manufacturer mounts, which is genuinely
  attractive. Rejected for now: our binary is cgo and dynamically linked, so a host with an older glibc
  fails at the loader — silently, and in a way that reads like "no devices". Reconsider if the mount
  list becomes the dominant friction.

- **`nsenter` into the host's mount namespace** instead of `chroot` into a bind-mounted root — what
  Koordinator's *koordlet* does. Rejected: it additionally requires seeing the host's PID 1, so it
  needs host PID namespace sharing on top of everything else, and it buys nothing over `chroot` for
  running a host executable. Koordinator's *device-daemon*, which is the component doing our job,
  chose `chroot` as well.

- **Official Go clients for both runtimes.** Rejected: docker's client is reasonable, but containerd's
  is low-level enough that using it means reimplementing `nerdctl` here, for a diagnostic. It also
  reintroduces the version skew the host's own CLI removes.

- **A Kubernetes Job that runs the checks in-cluster.** Rejected: the point is bring-up before there is
  a cluster, and a Job cannot answer any question earlier than a real workload can.

- **Reimplement detection rather than reuse the detect pass.** Rejected: two implementations of the
  same question will disagree, and the one the operator reads will be the wrong one.

## Open Questions

> **Settled while planning.** *How the simulated depth obtains the allocator's artifacts* was the
> largest question this spec carried. It is answered: the three responder interfaces take plain values
> and nothing else, so preflight drives the real ones with a synthetic request (F11, T2) — no narrower
> seam, no drift. Grounding it also surfaced that several responders write host state, which is why the
> vendor's driver seam is substituted for the pass and why a manufacturer without one has no simulated
> depth. Both are now stated in Goals and F11.

1. **How far the host cross-check should go.** Q1 is clearly worth it. Whether the host's vendor CLI
   should also be consulted at Q2's declared depth — where it could confirm a slice the driver read
   reported — is a judgement about how much CLI parsing is worth carrying.

2. **How far Q3 should go for a partition-backed owner.** The sidecar must name the partition rather
   than the parent accelerator, but no partition is created this round, so the case is only reachable
   on a host that already carries one.

3. **Whether Q2's measured depth should assert compute throttling** as well, or stop at memory and
   visibility for the first cut. Throttling needs a saturating load and a measurement window, which is
   a much longer case.

4. **Which manufacturers' default probe images can be resolved offline.** A default that always needs a
   pull makes the command useless in the environments that most need it.

5. **When the xbuild skill switches to consuming F10's emitted command** instead of its hand-written
   injection, and for which of its cases. It is follow-on work, not part of this spec, but it is the
   reason F10 has a consumer contract rather than only a fallback role — so the emitted form must be
   good enough for it before that follow-on can start.

6. **Whether the skill's `scripts/preflight.sh` should be renamed.** It answers a different question
   (can this target run the skill's cases, buildx included) and two things called "preflight" in one
   repository will be confused for each other. Cheap to fix, blocks nothing.
