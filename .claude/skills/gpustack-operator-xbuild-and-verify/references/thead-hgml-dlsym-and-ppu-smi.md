# HGML, `dlsym` and `ppu-smi` — why the THead visibility hook is where it is

The THead counterpart to `nvidia-smi-and-sm-limit.md`. HAMi-core makes a slice visible to `nvidia-smi`
by defining the NVML symbols and letting `/etc/ld.so.preload` put them ahead of `libnvidia-ml.so`.
The same move against `ppu-smi` does **nothing**, and the reason decides the whole `hgml/` module.

## `ppu-smi` never looks a symbol up in the global scope

`ldd` on `ppu-smi` names no HGML library at all — only `libdl`, `libpthread`, `libm`, `libc`. It
`dlopen`s `libhgml.so` at runtime (the image sets `LD_LIBRARY_PATH` to the SDK lib directory, so the
bare soname resolves) and then calls `dlsym` **on that explicit handle** for every entry point.

That is the difference. A preloaded library sits earlier in the *global* search scope, which is what
`libnvidia-ml.so`'s callers go through. `dlsym(handle, name)` does not consult the global scope: it
searches the object `handle` refers to, and its dependencies. So a preload that merely **defines**
`hgmlDeviceGetMemoryInfo` is inert — nothing ever asks the global scope for that name.

Interposing **`dlsym` itself** works, because the call passes through the preloaded object first. The
hook sees the `(handle, symbol)` pair, fetches the real function through the caller's own handle, and
returns a wrapper instead:

```c
void *dlsym(void *handle, const char *symbol)
{
    resolve_real_dlsym();                       /* dlvsym(RTLD_NEXT, "dlsym", …) */
    if (strcmp(symbol, "hgmlDeviceGetMemoryInfo") == 0) {
        real_get_memory_info = real_dlsym(handle, symbol);
        return (void *)hook_get_memory_info;    /* the caller gets our wrapper */
    }
    return real_dlsym(handle, symbol);
}
```

Two details that are not optional:

- **Which library carries `dlsym` depends on the container the shim lands in, not the one it was built
  in.** glibc 2.34 moved `dlsym` into `libc.so.6` as `GLIBC_2.34`; before that it lived in
  `libdl.so.2` as `GLIBC_2.2.5`. The shim tries both versions in that order.
- **The shim must not link `-ldl`.** Its `DT_NEEDED` has to stay at `libc.so.6` or empty, so `dlsym`
  and `dlvsym` stay undefined in the object and resolve from whatever glibc the workload brought.

## Proving it without a card

The mechanism is decidable with no PPU present, and `cases/thead-case-2.sh` does exactly that: an
inline program `dlopen`s `libhgml.so` the way `ppu-smi` does, then uses `dladdr` on the pointer
`dlsym` returned to name the object it came from. Measured against the `2.1.1` libraries:

| preload | `hgmlDeviceGetMemoryInfo` resolves in |
| --- | --- |
| none | `libhgml.so` |
| `hgml_dlsym_hook.so` | **`hgml_dlsym_hook.so`** |
| `hgml_nohook.so` (defines the symbols, no `dlsym` hook) | `libhgml.so` |

The third row is the claim. Defining the HGML symbols alone leaves the lookup untouched.

What still needs hardware is only the end-to-end reading: with no driver, `ppu-smi` stops at
`init HGML error: driver is not loaded` before it resolves a memory getter, so nothing can show the
quota in its table without a card.

## Why the negative control must prove it loaded

A control arm that fails to load looks exactly like a control arm that loaded and correctly did
nothing: both show the physical figure. So `hgml_nohook.so` announces itself from a constructor, and
the case requires that marker before it accepts the arm's reading. Without it, a typo in the preload
path would produce a green control and a Gate 1 result that means nothing.

The same reasoning applies in reverse to the hook arm: matching the quota is not enough on its own,
the interception marker has to be there too, otherwise something else changed the number.

## The two memory getters

Both are public and must be covered **separately** — their shared helper is `FUNC LOCAL` and therefore
not interposable.

```c
hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory);
hgmlReturn_t hgmlDeviceGetMemoryInfo_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory);
```

`hgmlMemory_t` is `{total, free, used}`; `hgmlMemory_v2_t` is `{version, total, reserved, free, used}`.
The caller writes `.version` before the call and the contract returns it **unchanged**, so a wrapper
must not touch it — rewriting it breaks the caller's struct-version negotiation. `.reserved` is a
driver figure and is left alone for the same reason. The quota shape is `total = quota`,
`used = min(real used, quota)`, `free = total - used`.

## The vendor wrappers

The SDK ships `libhggc_wrapper.so` and `libcc_wrapper.so`. **No library names either in `DT_NEEDED`**
(87 ELFs scanned), so they are opt-in tools rather than part of the normal load. They still matter to
a `dlsym` hook, which is why Gate 1 has an arm that preloads `libhggc_wrapper.so` explicitly and
requires no recursion and no deadlock, timeout-bounded.

## Related

- `thead-ppu-sdk-and-glibc.md` — SDK layout, the `hgml.h` include constraint, the glibc floor.
- `thead-hggc-symbol-manifest.md` — the measured symbol surface, and why `libhgml.so` can neither
  observe nor enforce an allocation.
- `troubleshooting.md` — `ppu-smi` exits 0 even when it fails.
