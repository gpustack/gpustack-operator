# npu-smi visibility & AICore-quota analysis

## npu-smi shows the physical card, not the slice
Logical-slicing is **process-level API interception** (libvruntime.so hooks ACL/runtime calls in
the workload process). `npu-smi info` is **not** rewritten by vcann-rt, so inside an injected
container it still reports the full physical card (e.g. HBM `58874 / 65536 MB`), not the
`memory-quota`. The sliced view is only visible via `enpu-monitor`. This is the difference from
*hard* slicing (dcmi `create_vdevice` vNPU), which `npu-smi` would show as a real vNPU.

### Three layers — and which one npu-smi is on (measured, ASCEND-CASE 4)
The reason is a **layer mismatch, not an impossibility**, and the three layers are easy to
conflate:

| layer | library | who uses it |
|---|---|---|
| CANN runtime `rt*` | `libruntime.so` (behind `libascendcl.so`) | **what vcann-rt interposes** — `libvruntime.so` defines **80** global rt-prefixed FUNCs (65 `rt[A-Z]` + 15 lowercase-s `rts*` STARS/RTS entries such as `rtsLaunchKernelWithConfig`, `rtsModelExecute`) and zero `dcmi_*`/`dsmi_*`. `hami-vnpu-core`'s `libvnpu.so` interposes the same layer (10 `rt*`, incl. `rtMemGetInfoEx`) |
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
`/etc/ld.so.preload`. What a real shim would **still** not solve, from the same run:

- `HBM Usage Rate(%)` is `used/total`; halving both preserved it. A quota shim would have to set
  `total := quota` *and* `used := this container's usage` to keep it honest.
- `Memory-Usage(MB)` comes from `dsmi_get_memory_info_v2`, which is imported by `npu-smi` and
  defined by the driver but declared in **no** header — an undocumented ABI to guess at.
- The rest of the table (power, temperature, Bus-Id, `Hugepages-Usage`, the per-process table) is
  card-wide and has no honest per-slice value.

### Card numbering inside a container: two schemes
With `ASCEND_VISIBLE_DEVICES=<n>`, **`npu-smi` keeps the physical NPU id** — on a container scoped
to card 1, `npu-smi info -t usages -i 0` fails with `Invalid card id` and `-i 1` is correct
(`npu-smi info -l` reports `NPU ID : 1`, and only `/dev/davinci1` is present). **ACL renumbers**:
the same card is `device 0` to `acl.rt.set_device`. Cases 2/3/4 rely on both.

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
