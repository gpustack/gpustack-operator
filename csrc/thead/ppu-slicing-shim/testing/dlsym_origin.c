/*
 * dlsym_origin.c — Gate 1's mechanism probe: which object won a symbol?
 *
 * It resolves the two HGML memory getters exactly the way `ppu-smi` does — `dlopen` on
 * `libhgml.so`, then `dlsym` on that explicit handle — and reports, per symbol, the object the
 * returned address actually lives in. `dladdr` is what makes the answer unambiguous: it names
 * the containing object rather than asking the caller to infer it from behaviour, which is the
 * whole reason this exists beside the visibility rows. A shim can look like it worked because
 * the figure happened to match; only the origin says which library the call went to.
 *
 * That question needs no PPU, so cases/thead-case-2.sh runs it as its hardware-free half: with
 * the hook preloaded both getters must come out of the shim, with the `dlsym`-less control
 * preloaded they must still come out of `libhgml.so` — defining the HGML symbols alone is inert
 * against a caller that resolves on an explicit handle.
 *
 * It links `libdl` and no SDK: it names the vendor library as a string and never includes a
 * vendor header, so it builds in any image that has the library at runtime.
 *
 * Output is one `MECH <symbol> origin=<object>` line per symbol, so the case parses it rather
 * than scraping prose.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <stdio.h>

static void report(void *handle, const char *symbol)
{
    void *fn = dlsym(handle, symbol);
    Dl_info info;
    const char *origin = "unresolved";

    if (fn != NULL && dladdr(fn, &info) != 0 && info.dli_fname != NULL) {
        origin = info.dli_fname;
    }
    printf("MECH %s origin=%s\n", symbol, origin);
}

int main(void)
{
    void *handle = dlopen("libhgml.so", RTLD_NOW);

    if (handle == NULL) {
        printf("MECH dlopen failed: %s\n", dlerror());
        return 1;
    }
    report(handle, "hgmlDeviceGetMemoryInfo");
    report(handle, "hgmlDeviceGetMemoryInfo_v2");
    return 0;
}
