# npu-smi visibility & AICore-quota analysis

## npu-smi and the slice: upstream hides it, GPUStack shows it
Logical-slicing is **process-level API interception** (libvruntime.so hooks ACL/runtime calls in
the workload process). Upstream vcann-rt hooks the `rt*` layer only, so `npu-smi info` is **not**
rewritten by it and reports the full physical card (e.g. HBM `58874 / 65536 MB`) instead of the
`memory-quota`, leaving `enpu-monitor` as the only sliced view. This is the difference from
*hard* slicing (dcmi `create_vdevice` vNPU), which `npu-smi` would show as a real vNPU.

GPUStack closes that gap with a vendored patch at the layer `npu-smi` actually uses — see **The
shipped fix** below. Everything between here and there is why the `rt*` layer alone cannot, and
is still the map to read before changing any of it.

### Three layers — and which one npu-smi is on (measured, ASCEND-CASE 4)
The reason is a **layer mismatch, not an impossibility**, and the three layers are easy to
conflate:

| layer | library | who uses it |
|---|---|---|
| CANN runtime `rt*` | `libruntime.so` (behind `libascendcl.so`) | **what vcann-rt interposes** — `libvruntime.so` defines **80** global rt-prefixed FUNCs (65 `rt[A-Z]` + 15 lowercase-s `rts*` STARS/RTS entries such as `rtsLaunchKernelWithConfig`, `rtsModelExecute`) and, **upstream**, zero `dcmi_*`/`dsmi_*` — our vendored patch adds exactly one `dsmi_*`, see *The shipped fix*. `hami-vnpu-core`'s `libvnpu.so` interposes the same layer (10 `rt*`, incl. `rtMemGetInfoEx`) |
| `dcmi` | `libdcmi.so` (host driver, injected) | vcann-rt is a **client**: 8 *weak undefined* `dcmi_*` imports (`dcmi_init`, `dcmi_get_device_utilization_rate`, …). It calls dcmi; it does not shadow it |
| `dsmi` | `libdrvdsmi_host.so` (host driver, injected) | **what `npu-smi` uses** |

`ldd` on the injected in-container `npu-smi` (`/usr/local/bin/npu-smi`, placed there by
`ascend-docker-cli`; the host copy is `/usr/local/sbin/npu-smi`):

```
libc_sec.so        libdrvdsmi_host.so   libmmpa.so   libascend_hal.so   (+ glibc)
```

**Neither `libruntime.so` nor `libdcmi.so`.** So vcann-rt's 80 rt-layer hooks are simply never on
`npu-smi`'s call path — which is why `enpu-monitor` exists as a separate reader rather than
`npu-smi` being rewritten.

### npu-smi CAN be rewritten — at the dsmi layer
`npu-smi` imports **86** `dsmi_*` symbols as *undefined*, bound through the PLT at load time, so
symbol interposition reaches it. Among them the memory/utilisation getters
(`dsmi_get_hbm_info`, `dsmi_get_memory_info_v2`, `dsmi_get_device_utilization_rate`), all defined
global-`T` in `libdrvdsmi_host.so`. And `npu-smi` is mode `555`, **not** setuid — a setuid binary
would make the loader ignore `LD_PRELOAD` entirely.

Measured with a throwaway interposer that halves `dsmi_get_hbm_info` (ASCEND-CASE 4 builds it
in-container; **container-scoped only, never the host's `/etc/ld.so.preload`**):

```
HBM Capacity(MB) : 65536   ->   32768        # -t usages
HBM-Usage(MB)    : 3426 / 65536  ->  1713 / 32768   # the npu-smi info table follows too
```

Both activation routes work: per-process `LD_PRELOAD` and the container's own
`/etc/ld.so.preload`. What a real shim would **still** have to deal with, from the same run:

- `HBM Usage Rate(%)` is `used/total`; halving both preserved it. A quota shim would have to set
  `total := quota` *and* `used := this container's usage` to keep it honest — which is what the
  shipped hook below does.
- `Memory-Usage(MB)` comes from `dsmi_get_memory_info_v2`, which is imported by `npu-smi` and
  defined by the driver but declared in **no** header — an undocumented ABI to guess at.
- The rest of the table (power, temperature, Bus-Id, `Hugepages-Usage`, the per-process table) is
  card-wide and has no honest per-slice value.

### The shipped fix: `dsmi_get_hbm_info` in libvruntime.so, gated by `ENPU_DSMI_HOOK`
`pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch` adds a
strong `dsmi_get_hbm_info` to `libvruntime.so` (and only there, not to `enpu-monitor`). It needs no
new library, mount or preload entry: a sliced container already preloads that library into every
process, `npu-smi` included — it simply defined nothing `npu-smi` calls.

| field | with the gate on |
|---|---|
| `HBM Capacity(MB)`, and the total in `HBM-Usage(MB)` | the container's `memory-quota`, clamped to the card's real total — the clamp `__enpu_global_init_post` spends `rtMemGetInfoEx` on, done here from the hook's own passthrough |
| the used half of `HBM-Usage(MB)`, `HBM Usage Rate(%)` | the slice's own usage: the same dcmi per-process sum `enpu-monitor` prints and the limiter enforces against, so the two tools agree and the rate stays honest |
| `Memory-Usage(MB)` | **unchanged**, card-wide — it comes from `dsmi_get_memory_info_v2`, whose ABI no header declares |
| power, temperature, Bus-Id, `Hugepages-Usage`, the per-process table | **unchanged**, card-wide — a slice has no honest value for them |

The result is deliberately a **mixed** view, the shape GPUStack already ships on NVIDIA (HAMi-core
hooks NVML, so `nvidia-smi` reports the virtual total while power/temp stay card-wide).

The gate is shaped like vcann-rt's own `ENPU_LOG_LEVEL`: absent ⇒ **off**, so a bare vcann-rt user
sees no behaviour change; `1`/`true`/`on` ⇒ on; `0`/`false`/`off` ⇒ off; anything else ⇒ off plus
one line on stderr. The device-manager injects `ENPU_DSMI_HOOK=1` into sliced Ascend containers and
never overwrites a value the container already declares, so a user opt-out wins.

Every call passes through to the driver first, and only the sliced device is rewritten: a process
with no `/etc/enpu/vcann-rt/npu_info.config`, or a query for a device that is not the sliced one,
gets the driver's own numbers. Identifying "the sliced one" needs the numbering table below — the
caller names the device by *logic* id and the config carries the *physical* one — so the hook maps
with `dsmi_get_phyid_from_logicid` (declared at `dsmi_common_interface.h:2268`) and reports the card
untouched if that mapping is unavailable. Because `npu-smi` has no `libruntime.so`, it also cannot
use `enpu_soc_init` to learn the SoC: it probes the weak `dcmi_get_card_id_device_id_from_phyid` to
pick the dcmi family, and falls back to reporting the quota with the card's usage if the dcmi ids
will not resolve.
ASCEND-CASE 1 asserts the symbol is present; ASCEND-CASE 4 asserts both halves of the gate.

### Card numbering inside a container: the interfaces disagree
All measured on one container scoped to physical card 1 (`ASCEND_VISIBLE_DEVICES=1`):

| interface | numbering | evidence |
|---|---|---|
| `npu-smi` CLI (`-i`) | **physical** | `-i 0` fails with `Invalid card id`, `-i 1` is correct; `npu-smi info -l` reports `NPU ID : 1`, and only `/dev/davinci1` is present |
| `dsmi` device arguments — what `npu-smi` calls underneath | **container-local logic id**: 0 for the only visible card | the hook is handed `dsmi_get_hbm_info(0, …)` on that container; `dsmi_get_phyid_from_logicid(0)` maps it back to 1 |
| `dcmi`, i.e. `physical-npu-id` in `npu_info.config` | **physical** | `enpu-monitor` initializes with `physical-npu-id=1`; with `=0` it fails — `get card info failed`, `enpu_device_init … err:-8008 npu:0` |
| ACL | renumbers to 0 | the same card is `device 0` to `acl.rt.set_device` |

So the `npu-smi` CLI and vcann-rt's `physical-npu-id` agree, while the dsmi ids underneath do not:
anything comparing the two has to translate, which is why the dsmi hook calls
`dsmi_get_phyid_from_logicid` instead of matching the raw argument (a raw match works only on
card 0 — it silently no-ops on every other card). Cases 2/3/4 rely on all of this.

## AICore (compute) quota — mechanism and current status
`core_limiter` (src/core_limiter.c) throttles compute by wrapping **each hooked task/kernel
launch**: a launch must acquire a mutex that a scheduler thread holds/releases according to the
time-slice quota (`VNPU_SCHEULE_PERIOD` × `aicore-quota%`).

It resolves the runtime launch functions via `dlsym(RTLD_NEXT, name)` over a **cross-CANN-version
superset** table (src/include/runtime_hook.h). On **CANN 8.5.0** some entries warn:
```
Failed to find function rtFftsPlusTaskLaunch / rtStarsTaskLaunch / rtModelExecuteAsync /
rtBarrierTaskLaunch / rtCpuKernelLaunch ... because the runtime version ... is different ...
```
These symbols **do not exist in CANN 8.5.0 at all** (they are newer-CANN / STARS / FFTS+
dispatch entry points); the 8.5.0-relevant ones (`rtKernelLaunch`, `rtKernelLaunchWithHandle*`,
`rtFftsTaskLaunch`, `rtStreamSynchronize`) **do** resolve. So the warnings are **benign version
coverage noise**, not a stale pin or driver mismatch — on the CANN-9 build targets those symbols
exist and would hook (no warning).

Under a real ACL workload the full stack initializes (`Successfully to initialize all module.`),
including the core_limiter scheduler thread.

### Gap (not yet verified)
End-to-end AICore throttle (does the workload's compute actually get capped to `aicore-quota%`?)
was **not** confirmed on the 910B2: the low-level `acl.blas.hgemm` probe would not dispatch
(`ret 100000`, pyACL param strictness) and the base CANN image has no torch_npu. To confirm,
run a real model (torch_npu / vLLM) with `aicore-quota` set and sample `npu-smi info` AICore%
with vs. without injection. **Memory-quota enforcement IS verified (ASCEND-CASE 3).**
