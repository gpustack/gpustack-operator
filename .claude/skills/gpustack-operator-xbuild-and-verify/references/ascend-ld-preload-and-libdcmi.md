# ld.so.preload + the libdcmi requirement

## How the injection activates
Soft-slicing activates by mounting an `ld.so.preload` file to the container's
`/etc/ld.so.preload` (never the host `/etc` — that would preload on the host and hit host
workloads). Every dynamically-linked process in the container then loads the listed libraries.

The shipped asset is `pack/gpustack-operator/rootfs/etc/gpustack/lib/ascend/ld.so.preload`.
It lists, **in order**:

```
/usr/local/dcmi/libdcmi.so
/usr/local/Ascend/driver/lib64/driver/libdcmi.so
/opt/enpu/vcann-rt/lib/libvruntime.so
```

`ld.so.preload` has no comment syntax — every non-empty token is a library path; a missing
path is silently skipped (so listing both libdcmi candidates is safe).

## Why libdcmi MUST be preloaded (real-hardware finding)
vcann-rt declares the dcmi entry points `__attribute__((weak))` and the build drops
`libdcmi.so` from the artifact's `NEEDED` (`--as-needed`, dcmi is host-injected, not in the
toolkit). At runtime a weak undefined symbol resolves to **NULL unless libdcmi is actually
loaded** into the process — calling it then segfaults.

libdcmi is injected by the Ascend container runtime at two paths:
- `/usr/local/dcmi/libdcmi.so`
- `/usr/local/Ascend/driver/lib64/driver/libdcmi.so` (this dir is on `LD_LIBRARY_PATH`)

…but it is **not auto-loaded**: it is neither `NEEDED` by anything nor in `ld.so.cache`, and
`LD_LIBRARY_PATH` only governs *where* libraries are found, not *which* are loaded. Verified
that even a full `acl.init()` + `aclrtSetDevice(0)` does **not** pull libdcmi in. Therefore:
- `enpu-monitor` (calls dcmi directly) → SIGSEGV (exit 139) without libdcmi loaded;
- the preloaded `libvruntime.so` (its `aclrtMalloc` memory-quota hook calls dcmi) → would
  segfault the workload on its first NPU allocation.

Listing the libdcmi paths **before** libvruntime.so in `ld.so.preload` fixes both. Same
SONAME (`libdcmi.so`) ⇒ the loader dedupes, so listing two candidates loads only one.

## Also required in the workload container
libvruntime.so / enpu-monitor `NEEDED` = `libc_sec.so` + `libascendcl.so`. Both ship with CANN
in the same toolkit `lib64` (on `LD_LIBRARY_PATH`), so any CANN-based workload image satisfies
them. `libc_sec.so` is CANN's name for the securec / libboundscheck library — a non-CANN image
(plain ubuntu/alpine) lacks it and the preload fails (`cannot open shared object file`).
