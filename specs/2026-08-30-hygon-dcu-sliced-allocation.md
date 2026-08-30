# Spec: Hygon DCU logical slicing — from injection-only to measured

Status: Shipped
Type: Feature

> **This spec establishes a capability that already shipped, and fixes what establishing it found.**
> `specs/2026-07-18-mthreads-hygon-sliced-injection.md` delivered the Hygon sliced path: the detector's
> `Status.LogicalSliced`, the allocator's `Sliced` server, and the `vdev<N>.conf` ledger in
> `pkg/devicemanager/allocator/hygon/vdev.go`. Nobody had ever run it against a DCU. This spec does,
> and the run changed three things: it found a defect that breaks the second sliced Pod on any node, it
> disproved the assumption the preflight probe would have been built on, and it moved Hygon's
> `device-manager preflight` rows from the `simulated` depth to `measured`.

## Summary

`device-manager preflight` answers two questions per accelerator, at one of three depths. For
Hygon the second question — *can this card actually be sliced?* — stopped at `simulated`: the
allocator produced an injection and nothing had ever looked inside a container to see what that
injection did. Hygon shared that tier with Iluvatar and MThreads, and it was the only one of the
three with hardware available.

Establishing the answer required hardware because every plausible design for it was wrong:

- **The slice is invisible to the vendor's own SMI tool.** Under a full injection capping a card at
  1024 MiB, `hy-smi` reports the physical 65520 MiB. It answers from the DMI layer; the cap binds
  the HSA/HIP runtime a workload uses. A probe built on `hy-smi` would have reported every sliced
  accelerator as unsliced.
- **The slice is invisible to a process that does not use it.** The driver publishes a per-slice
  record under the kfd vgpu sysfs, but only for a process that has initialized HIP. A probe that
  merely read a file found nothing and reported every healthy accelerator as having no slicing
  runtime — measured, as the first draft of this work.
- **There is no log level to raise.** The other four manufacturers preload a library this repository
  builds, so preflight raises its log level to make it state the cap it read. Hygon's slicing
  runtime is the vendor's DTK/hyhal user space; its vgpu diagnostics go through hylog, which has an
  API to set the level and no environment variable. Recorded as a fact rather than worked around.

What the run also found is a defect that no single-Pod test could see. The vendor runtime requires
`vdev_id` to equal the ordinal in the file name it read the record from, and rejects a mismatch by
leaving the container **with no accelerator at all**. `allocateVdev` drew that id from a node-wide
pool while naming the file from the container-local device index — two numbers that agree only
until a node holds a second sliced Pod.

## Motivation

### Goals

- Reproduce the Hygon injection by hand on a DCU and land the numbered cases that did so, so the
  values `preflight` asserts have a second, independent reader.
- Move both Hygon rows to the `measured` depth, or record honestly why they cannot go there.
- Fix what the reproduction found.
- Run the per-process read (`MonitorAcceleratorProcesses`) against a real driver, which had never
  happened.

### Non-Goals

- **A Hygon backend for `gpustack-operator-xbuild-and-verify`.** That skill is *build*-and-verify:
  each of its four backends compiles an artifact this repository owns and then verifies it. Hygon
  owns no such artifact — `csrc/` carries only `amd` and `thead`, and the operator image has zero
  `xbuild-hygon` stages — so its cases verify without building. They are added as cases; no backend
  and no build step is introduced, because adding one would change what that skill is.
- **A default probe image for Hygon.** It stays with THead in `ResolveProbeImage`'s default branch:
  `--probe-image` is required. See Boundaries.
- **Iluvatar and MThreads.** Same tier, no hardware. They keep the `simulated` rows that say so —
  with one change they inherit rather than are the subject of: an injection carrying nothing is now
  a failure on the no-probe branch, which after this spec is the whole of what they get. See
  "Landed here, not part of this deliverable".
- **Hygon's hardware vdevice path.** `hy-smi` carries `--create-vdevices` / `--destroy-vdevices`,
  a driver-level partitioning scheme parallel to MIG. It is not what the operator's logical slicing
  uses and is untouched here.

## Proposal

### The record, and the two rules that are invisible in it

The slice is one file. `vdevConf.render` emits eight fields, and the vendor's parser
(`libhsa-runtime64.so`, built from `ROCT-Thunk-Interface_6.3_docker/src/virtual.c`) reads them from
`/etc/vdev/docker/vdev<N>.conf` with a `%[^:]:%s` scan — colon-separated, unlike Ascend's
`key=value`:

```
PciBusId: 0000:09:00.0
cu_mask: 0x000000000000ffff
cu_mask: 0x0000000000000000
cu_count: 16
mem: 1024 MiB
device_id: 0
vdev_id: 0
pipe_id: 0
enable: 1
```

Two consistency rules are enforced by the parser and stated nowhere in the file. Both are named in
its own diagnostics, which is how they were found:

| Rule | Parser diagnostic | Consequence of breaking it |
|---|---|---|
| `cu_count` == popcount(`cu_mask`) | `Parse cu_count field failed ... inconsistent with hamming weight of cu mask field` | record rejected |
| `vdev_id` == the `N` in `vdev<N>.conf` | `Parse vdev_id field failed ... inconsistent with configuration file associated value` | **container gets no accelerator** |

A third rule has no diagnostic and was found by running into it: `pipe_id` must be unique among the
live slices of one card. A collision costs the second container its accelerator, silently.

`vdevConf.render` already satisfied the first rule (`cuCount: mask.count()`).

### The defect

`allocateVdev` named the file from the container-local accelerator index and drew `vdev_id` from a
node-wide pool of ids read off every on-disk record:

```go
selfPath := filepath.Join(vdevHostDir, fmt.Sprintf("vdev%d.conf", i))   // container-local
vdevID, err := lowestFreeSlot(usedVdev, maxVdevID)                      // node-wide
```

Every Pod numbers its own confs from zero, so the first sliced Pod on a node gets `vdev0.conf`
carrying `vdev_id: 0` and works. The second scans the first's record, finds 0 taken, and writes
`vdev0.conf` carrying `vdev_id: 1` — which the runtime rejects. The Pod runs; its container has no
DCU.

The fix is that the vdev id *is* the ordinal, which the caller already passes as `deviceID`. Node-wide
uniqueness is not needed to compensate: two containers each holding `vdev_id 0` on one card run side
by side, the runtime telling them apart by container id and numbering its own instances
(`0x<gpu_id>@0` and `@1`). Pipe ids are the ones that must not collide, and they are still drawn
from the scan.

### The preflight probe

Three changes, all in `pkg/devicemanager/preflight/measure.go`:

1. **`sliceProbe.LoadEvidence`** — a string the reader prints only when the slicing runtime took
   effect, replacing the mapped-object test for a manufacturer whose injection carries no shared
   object of ours. Judged by the existing clause, Hygon's injection mounts zero `.so` files and
   every healthy accelerator would be reported as having no slicing runtime.
2. **`sliceProbe.MemoryQuotaConfigFile`** and `cutConfigField` — the cap carrier is a mounted
   *directory* holding the record, and the record separates on a colon and carries a unit
   (`mem: 32760 MiB`) where Ascend's separates on `=` and does not. `cutConfigField` splits on
   whichever of the two characters comes first, so Hygon's `PciBusId: 0000:09:00.0` — read by the
   same loop — does not split in the wrong place.
3. **`stageLibFor` skips a load-evidence manufacturer.** There is no tree in this image to stage for
   Hygon; attempting one fails, and a staging failure forces both container questions to be emitted
   rather than run. Every accelerator would have stopped at `simulated` over a library that was
   never part of its injection.

The reader has to *cause* what it then reads:

```sh
conf=/etc/vdev/docker/vdev0.conf                        # this container's own slice
set -- $(awk -F'[.: \t]+' '/^PciBusId:/ { print "0x"$3, "0x"$4, $5; exit }' "$conf" 2>/dev/null)
loc=$(( ${1:-0} * 256 + ${2:-0} * 8 + ${3:-0} ))        # bus<<8 | device<<3 | function
pipe=$(awk '/^pipe_id:/ { print $2; exit }' "$conf" 2>/dev/null); mine='*'
[ "$loc" != 0 ] && [ -n "$pipe" ] && for n in /sys/class/kfd/kfd/topology/nodes/*/; do
  grep -qx "location_id $loc" "$n/properties" 2>/dev/null || continue
  g=$(cat "$n/gpu_id" 2>/dev/null); [ "${g:-0}" != 0 ] || continue
  mine=$(printf '0x%x@%s' "$g" "$pipe"); break         # the record's own directory name
done
before=$(grep -h '^Indentifier:' /sys/devices/virtual/kfd/kfd/vgpu/*/entry 2>/dev/null)
claim=/gpustack-preflight-barrier; [ -d "$claim" ] || claim=$(mktemp -d 2>/dev/null || echo /tmp)
log=$(mktemp 2>/dev/null || echo /dev/null)
LD_LIBRARY_PATH=/opt/hygondriver/hip/lib:/opt/hygondriver/lib:/opt/hyhal/lib \
  /opt/hygondriver/bin/BandwidthTest >"$log" 2>&1 & hip=$!; i=0; new=""; dead=0
while [ -z "$new" ]; do
  for e in /sys/devices/virtual/kfd/kfd/vgpu/$mine/entry; do
    [ -r "$e" ] || continue; id=$(grep -h '^Indentifier:' "$e" 2>/dev/null)
    [ -n "$id" ] || continue; case "$before" in *"$id"*) continue;; esac
    mkdir "$claim/claimed-${id#Indentifier:}" 2>/dev/null || continue; new="$e"; break
  done
  [ -n "$new" ] && break
  [ "$dead" = 1 ] && break                          # the sweep runs before the liveness test, and
  kill -0 "$hip" 2>/dev/null || { dead=1; continue; }  # once more after it fails: a client that has
  [ "$i" -lt 100 ] || break                         # just finished still has a record for a second
  sleep 0.1; i=$((i+1))
done
if [ -n "$new" ]; then
  awk -F: '{ print } /^Vram limit/ { printf "Vram limit MiB: %d\n", $2 / 1048576 }' "$new"
elif ! kill -0 "$hip" 2>/dev/null; then
  wait "$hip" 2>/dev/null; echo gpustack-preflight-client-exit-$?; tail -n 3 "$log" 2>/dev/null
fi
kill "$hip" 2>/dev/null || true
```

`BandwidthTest` is the vendor's own and comes from `/opt/dtk`, which the allocator already mounts at
`/opt/hygondriver` — so it is there whatever the probe image carries. `LD_LIBRARY_PATH` is what makes
that true rather than nearly true: running it out of the mounted tree finds the binary but not its
`libgalaxyhip`, which a DTK-based image happens to supply from its own `/opt/dtk` and another image
does not.

It is set on the command rather than through `LogEnv`, whose variables are unset before the reader
runs. `BandwidthTest`'s own line reports the cap as `Mem=1.0GB` — rounded, and in GB — so the figure
is taken from the driver's record, where it is exact and in bytes.

**The reader reads its own record, not the node's.** The kfd vgpu sysfs is node-wide, so a reader that
took any record would let a slice somebody else holds — on any card, started at any time — supply both
the load evidence and the quota figure. Two things narrow it.

*The record has a name, and the container can compute it.* A record's directory is
`0x<gpu_id>@<pipe_id>`, and the slice this container was handed names both halves: its `PciBusId` maps
to the gpu id through the kfd topology's `location_id`, and its `pipe_id` is unique among the card's
live slices. Measured — the same card published `0x563e@3` under an injection carrying `pipe_id: 3`
and `0x563e@5` under one carrying 5, `0x563e` being the gpu id the topology gives for `0000:09:00.0`.
A part that will not resolve leaves the sweep on every record, which the second narrowing covers.

*The record must be new.* The name is of a slice, not of a moment: the driver keeps a record about a
second past its process, so this container's own name can still be its predecessor's instance. Every
record carries an `Indentifier` (the driver's spelling) unique to the holding process, so the set of
them is taken before the client starts and only an unseen one counts.

**And a client that dies says so.** The reader ends by tidying up, so its container exits zero
whatever became of the client it started — a library that would not resolve reaches the judge as an
absent record and nothing else, which is the same shape as a driver that refused the slice. The exit
status is therefore printed where the judge can name it, only on the path where no record appeared.

**Co-tenancy is what the name is worth.** Two readers run at once behind one barrier, and an unseen
record is not necessarily either one's own. Before the name was computed both tenants snapshotted
before either client had published and both then took the first record to appear — the same one.
Measured, four of eight accelerators had both tenants reporting a single identifier, and because two
co-tenants are asked for equal caps by construction, each found the figure it was looking for and the
row reported two slices it had not seen. With the name, each tenant looks only where its own pipe id
puts it: measured over three consecutive runs, all eight accelerators reach `measured`, the two
tenants reading `Vgpu device:0` and `Vgpu device:1` on every one of them.

That last figure also retires an inference this spec previously carried. Under the earlier reader the
shallow rows were read as the driver publishing one record between two live clients; it publishes two,
and what was shallow was the reader unable to tell them apart.

A record is still *claimed* before it is read, by creating a directory named for it under the barrier
the two tenants already share: `mkdir` is atomic, so only one takes any given record and the loser
goes on looking. Two names cannot collide, so this now guards the fallback path — a reader that could
not compute its name is back to taking the first record it sees, beside a peer doing the same. A probe
with no peer claims against a directory of its own. The co-tenancy judge refuses `measured` when both
tenants print the identical record, so a claim that stops working shows up as an unobserved overlap
rather than as co-tenancy.

### Core Features & Acceptance Criteria

| # | Claim | How it was established |
|---|---|---|
| 1 | The rendered record is the record the parser accepts | HYGON-CASE 1 — field order, `key: value` shape, both consistency rules, and the parser's own strings for all eight field names, the config directory and both diagnostics |
| 2 | A sliced container reports the quota, not the card | HYGON-CASE 2 — `total_memory` 65520 → 1024 MiB, `multi_processor_count` 80 → 8 |
| 3 | `vdev_id` must match its file name | HYGON-CASE 2 — the same record named `vdev0.conf` while carrying `vdev_id 1` loses the device; named `vdev1.conf` it works |
| 4 | The memory cap is enforced, not just reported | HYGON-CASE 3 — 512 MiB allocates, 2048 and 8192 MiB fail with `total capacity of 1024.00 MiB`, both far inside a 65520 MiB card |
| 5 | The CU mask bounds compute | HYGON-CASE 4 — 8/20/40/80 CU → 45545/90897/205506/344958 GFLOPS; a full mask matches the unsliced card's 344443, so the narrow figures are isolation and not overhead |
| 6 | Two slices share one card, independently | HYGON-CASE 5 — 1024 MiB and 2048 MiB co-resident behind a barrier, both allocating, both records carrying `vdev_id 0`, driver listing `0x563e@0` and `@1` |
| 7 | A colliding `pipe_id` costs the second container its device | HYGON-CASE 5 |
| 8 | Both sliced preflight rows reach `measured` | `sliced-runtime-loaded` and `sliced-quota-in-force` `ok` at `measured` on 8/8 accelerators, every pass, exit code 0 |
| 9 | The per-process read reaches the driver and returns real figures — the adapter layer only | See below |

### The per-process read (`MonitorAcceleratorProcesses`)

Verified against a BW card carrying a sliced container running a matmul. The adapter's three-call
shape is load-bearing rather than defensive:

| Call | `vram_usage` | `cu_occupancy` |
|---|---|---|
| `rsmi_compute_process_info_get` (host-wide enumeration) | **0** | **0** |
| `rsmi_compute_process_info_by_pid_get` | 378146816 | **0** |
| `rsmi_compute_process_info_by_device_get` (what the adapter uses) | 378146816 | 2 |
| kernel, `/sys/class/kfd/kfd/proc/<pid>/` | `vram_<gpu_id>` = 378146816 | `stats_<gpu_id>/cu_occupancy` = 2 |

The host-wide enumeration is a list of processes, not a measurement. An adapter that trusted its
rows — the obvious single-call simplification — would still compile, still return a row per holder,
and report every process on every card as using nothing: a monitoring failure that looks exactly
like an idle cluster. `cu_occupancy` on this revision is a real figure, not `KFD_STATS_INVALID`.

No case covers this. The only per-process reader available on the host is `hy-smi --showpids`, which
is the vendor's Python binding over the same library; a case asserting its output would establish
that RSMI can answer, not that our adapter reads it correctly, and would need a container-id-to-pid
mapping that only holds for one container runtime. The hardware evidence lives in this spec and in
the adapter's own doc comment instead.

**This does not close the Hygon half of #96, and the row above says which half it is.** That issue's
acceptance is the *observable* path — a logically sliced Instance read through the subresource, the
exporter and `/monitor/snapshot`, `coresUtilizationPercent` present-and-plausible or absent-with-a-
reason but never zero, `gpustack_accelerator_process_capability` publishing a reason that matches
what the driver answered, and the "On hardware" column of `docs/reference/instance-metrics.md`
updated. What is established here is the layer beneath all of them: the library resolves, answers,
and answers correctly. The matrix therefore still reads `—` for Hygon, which is right until an
Instance has been run on such a node.

The one prediction it does retire is the one #96 singled out: Hygon's `⚠️ driver-dependent` marking
on `coresUtilizationPercent` was a guess that its GFX revision might return the sentinel AMD's RDNA3
does. On this revision it does not — the figure is real. Whether another Hygon revision behaves the
same is still unknown, so the marking stays.

### Notes / Constraints / Caveats

- **The record is correlated by process, not by card.** The reader reports the one instance its own
  client created, so another workload's slice cannot supply the evidence. What it does not establish
  is the card's whole state: the rows say this container got the slice it asked for, not how many
  others the card carries or what they were given.
- **`cu_occupancy` is an instantaneous sample.** A workload between kernels reads 0, which is a
  measurement and not an absence — distinct from `KFD_STATS_INVALID`, which the adapter reports as
  absent.
- **`--probe-image` is required.** The probe needs a working `sh`; a DTK image is not required (a
  non-DTK image on a different glibc runs it once `LD_LIBRARY_PATH` names the mounted trees), but
  no default is claimed for a family this repository has not surveyed.
- **The reader starts a HIP client.** `BandwidthTest` allocates on the card for as long as the probe
  runs. It is killed when the reader finishes and the container is `--rm`.

### Boundaries

- The vdev id fix changes an on-disk record's content, not its path. A record written by an earlier
  build whose id does not match its file name is **replaced** rather than reused — reusing it would
  republish the mismatch the runtime refuses.
- `maxVdevID` remains the range check on a parsed record and the bound on a usable ordinal. It is no
  longer a pool.
- Nothing in the Hygon detector changed. The 80-CU figure the mask is derived from is
  `simd_count / simd_per_cu` as the detector already reads it.

### Landed here, not part of this deliverable

Three commits on this branch close unresolved review threads from the already-merged PR that shipped
`device-manager preflight` (`specs/2026-08-28-device-manager-preflight.md`). They are **not** Hygon
work and nothing above asks for them; they are recorded here so the branch carries no unaccounted-for
change, and so a reader looking for why shared preflight behaviour moved finds it.

| Change | What it fixes | Who it affects |
|---|---|---|
| `cmd.go` answers a cancelled context instead of the rows | an interrupted pass reported killed child commands as `unavailable` accelerators — a document saying the node cannot slice, produced by Ctrl-C | every manufacturer |
| `injectsNothing` on the no-probe branch | an empty `ContainerAllocateResponse` was reported `ok` under a detail saying the allocator produced the injection | the no-probe branch: Cambricon, MetaX, Iluvatar, MThreads |
| the `state` column defined independently of the driver | two pages defined `ok` as "the capability was read" and `unavailable` as "the driver could not be asked"; this spec's own hardware run is the counter-example to both | readers of the docs, and `specs/2026-08-28-device-manager-preflight.md`, corrected in place |

The second is the one with a causal link to the work above rather than merely sharing a branch with
it: Hygon leaving the injection-only tier is what leaves Iluvatar and MThreads alone on a branch that
reported what a responder produced without looking at it.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| A future change re-pools vdev ids and breaks the second Pod on a node silently | `TestAllocateVdev_VdevIDIsTheFileOrdinal` and `TestAllocateVdev_ConcurrentDisjoint` both fail; HYGON-CASE 2 fails on hardware |
| A future change drops the pipe id pool | HYGON-CASE 5's negative half fails |
| The driver stops publishing the vgpu instance, or renames the field | HYGON-CASE 5 asserts both `Vgpu device:` and `Vram limit:<bytes>`; `TestSliceProbeLogLevelsMatchTheVerificationCases` fails if the table's load evidence stops matching what a case asserts |
| The `sliced-runtime-loaded` clause is judged by mapped objects again | `TestJudgeProbeOutput` carries three load-evidence rows, one of which requires the marker to be found in the reader's section only |
| The probe image carries no `sh`, or no client the reader can start | Reported as `unavailable` at `measured`, naming the container's exit status or the client's. `containerRan` reads a marker-less failure as a container that RAN unless the status is the one the runtime keeps for itself (docker 125, nerdctl 1), and a missing `sh` is 127 — deliberately, because the alternative lets a slicing runtime that aborts as it loads read as an environment limit. The row names what to fix; `--probe-image` is the fix |

## Design Details

### Commands

```sh
# ---- Go verification, local (the whole module builds and tests on darwin) ----
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -mod=mod -race \
  ./pkg/devicemanager/allocator/hygon/... ./pkg/devicemanager/preflight/...
make lint

# ---- the cases, against a DCU host ----
XB_MODE=ssh XB_HOST=<user>@<host> .claude/skills/gpustack-operator-xbuild-and-verify/cases/hygon-case-1.sh
# ...2 through 5; case 1 needs /opt/hyhal but no accelerator.

# ---- preflight, on the DCU host ----
gpustack-operator device-manager preflight --manufacturer hygon --host-root / \
  --runtime docker --probe-image <a DTK-compatible image>
```

### Project Structure

```
pkg/devicemanager/allocator/hygon/vdev.go            vdev_id is the file ordinal; pipe ids stay pooled
pkg/devicemanager/allocator/hygon/vdev_test.go       the ordinal contract, replacing the pool's
pkg/devicemanager/preflight/measure.go               LoadEvidence, MemoryQuotaConfigFile,
                                                     cutConfigField, the hygon probe, stageLibFor
pkg/devicemanager/preflight/measure_test.go          load-evidence and directory-carrier rows
pkg/devicemanager/detector/hygon/process.go          the doc comment: verified, and why three calls
docs/operation/preflight.md                          the tier table and the per-manufacturer table
.claude/skills/gpustack-operator-xbuild-and-verify/SKILL.md   the hygon cases, knobs and hard rule
.claude/skills/gpustack-operator-xbuild-and-verify/cases/hygon-case-{1..5}.sh

# "Landed here, not part of this deliverable" -- see that section
pkg/devicemanager/cmd.go                             a cancelled pass is answered, not reported
pkg/devicemanager/preflight/measure.go               injectsNothing, on the no-probe branch
docs/architecture/device-discovery.md                the state column, defined without the driver
specs/2026-08-28-device-manager-preflight.md         the same correction, in the spec that set it
```

### Test Plan

Unit tests carry the contracts; the cases carry the hardware. Every fix landed with a mutation that
compiled, turned the relevant test red, and was restored from a backup file:

| Mutation | Caught by |
|---|---|
| `vdevID := deviceID + 1` | 8 tests, including both new ordinal tests |
| load evidence searched in the whole output rather than the reader's section | `TestJudgeProbeOutput/load_evidence_outside_the_reader's_section_does_not_count` |
| `cutConfigField` always splits on `=` | `TestMemoryQuota/a_directory_carrier_...` |
| the unit suffix is not stripped from the value | the same row |
| `stageLibFor` stages for a load-evidence manufacturer | `TestStageLibFor_SkipsAManufacturerWithNoTreeOfOurs` |

## Alternatives

- **Judge Hygon by the mapped `libhsa-runtime64.so`.** The injection mounts `/opt/hyhal`, so the
  vendor runtime does appear in a HIP client's address space, and the existing clause could have
  been extended to name an expected object instead of deriving one from the mounts. Rejected because
  it proves the library was loaded, not that the slice was taken up: the same object is mapped by a
  process on an unsliced card. The driver's instance record only exists for a process in vgpu mode.
- **Make the vdev id pool per-card instead of node-wide.** Still diverges from the file name as soon
  as one Pod holds two slices of different cards. The ordinal is the only value that cannot diverge.
- **Give Hygon a default probe image.** Deferred with THead, which is in the same position.

## Open Questions

- Iluvatar and MThreads remain injection-only with no hardware to change that. Their rows say so.
- Hygon's hardware vdevice scheme (`hy-smi --create-vdevices`, a driver-level partitioning parallel
  to the `.partitioned` mode other vendors expose) is unexplored. Whether it should back a
  `Partitioned` mode for Hygon is a separate question this spec does not open.
