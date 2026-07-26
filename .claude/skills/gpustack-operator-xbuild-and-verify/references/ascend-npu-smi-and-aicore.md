# npu-smi visibility & AICore-quota analysis

## npu-smi shows the physical card, not the slice
Logical-slicing is **process-level API interception** (libvruntime.so hooks ACL/runtime calls in
the workload process). `npu-smi info` queries the driver/dcmi directly and is **not** rewritten
by vcann-rt, so inside an injected container it still reports the full physical card (e.g. HBM
`58874 / 65536 MB`), not the `memory-quota`. The sliced view is only visible via `enpu-monitor`.
This is the difference from *hard* slicing (dcmi `create_vdevice` vNPU), which `npu-smi` would
show as a real vNPU.

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
