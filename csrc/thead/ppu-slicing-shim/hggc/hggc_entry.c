/*
 * hggc_entry.c — the entry-point resolvers, so a caller cannot ask the driver for an
 * uninterposed function pointer.
 *
 * WHY THESE ARE PART OF THE QUOTA. Interposing hgMemAlloc only covers callers that reach it
 * through the dynamic linker. libhggc.so also exports hgGetProcAddress, hgGetProcAddress_v2
 * and hgGetExportTable, and a caller that asks one of those for an allocation entry gets the
 * driver's own address and calls it directly — past every wrapper in hggc_mem.c. The runtime
 * layer is exactly such a caller: this is how libhggcrt binds the driver entries it needs.
 *
 * HOW THE SUBSTITUTION DECIDES. hgGetProcAddress takes a base name plus a version and picks
 * the versioned symbol itself: "hgMemAlloc" at version 13000 resolves to hgMemAlloc_v2, and
 * at an older version to the v1 form. Rather than reimplement that choice from the name and
 * the version — which would have to be re-derived for every SDK release — the pointer the
 * driver just returned is compared against the vendor entry behind each of this library's own
 * definitions. If it is one of them, ours is handed out instead. Address comparison answers
 * the question the caller actually asked, whatever the vendor's versioning rules are.
 *
 * WHAT 2.1.1 ACTUALLY DOES, measured rather than assumed: its resolver returns the INTERPOSED
 * address already — it resolves through the linker like any other caller, so a preloaded
 * definition wins there too, and an entry taken through it is charged with no substitution
 * needed. That is a property of this driver, not of the interface: NVIDIA's counterpart
 * returns the driver's internal implementation, which is why the substitution is here at all.
 * Keeping it costs one comparison per resolved symbol and turns a future SDK that changes its
 * mind into a non-event instead of a silent hole in the quota. Which of the two happened is
 * visible in the log, never inferred — see substitute().
 *
 * hgGetExportTable IS OBSERVED, NOT REWRITTEN. It hands back an opaque table of vendor
 * function pointers whose layout is private and undocumented; guessing at offsets to swap
 * pointers inside it would corrupt the runtime rather than account for it. So the request is
 * logged with the table's identifier instead — that way, if a memory path ever shows "not
 * refused and no counter moved", the record shows whether a table was fetched that could
 * explain it. This is the one acknowledged gap in the coverage claim, and the counters are
 * what keep it from being a silent one.
 */
#define _GNU_SOURCE

#include <dlfcn.h>

/* hggc.h includes only <stdlib.h> and <stdint.h>, so it supplies neither NULL nor bool for
 * its own declarations. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>

#include "common/vppu.h"
#include "hggc/hggc_quota.h"

/* substitute — make the pointer the driver hands out for a covered entry this library's own
 * definition of it.
 *
 * BOTH OUTCOMES ARE LOGGED, and that is the point. Measured against SDK 2.1.1, this driver's
 * resolver already returns the interposed address — it resolves through the linker like any
 * other caller — so the replacing branch never fires there and only the "already" line prints.
 * A function that stayed silent in that case would be indistinguishable from one whose
 * matching is broken and never recognises anything, which is the failure this guard exists to
 * catch in the first place. One line always prints for a covered symbol and neither prints for
 * an uncovered one, so the log says which of the two happened.
 *
 * Silence for an uncovered symbol is correct: a caller asking for an entry this quota does not
 * cover must get the vendor's address unchanged, not a failure. */
static void substitute(const char *symbol, void **pfn)
{
    if (pfn == NULL || *pfn == NULL) {
        return;
    }

    for (int i = 0; i < VPPU_ENTRY_COUNT; i++) {
        enum vppu_entry entry = (enum vppu_entry)i;
        const char *name = vppu_hggc_name(entry);
        void *ours = vppu_hggc_self(entry);

        if (ours != NULL && *pfn == ours) {
            vppu_log(VPPU_LOG_DEBUG, "resolved %s as %s: already the interposed entry\n",
                     symbol, name);
            return;
        }
        if (ours == NULL || *pfn != vppu_hggc_next(entry)) {
            continue;
        }

        vppu_log(VPPU_LOG_DEBUG, "resolved %s as %s: handed out the interposed entry\n",
                 symbol, name);
        *pfn = ours;
        return;
    }
}

HGresult HGGCAPI hgGetProcAddress(const char *symbol, void **pfn, int hggcVersion,
                                  hguint64_t flags,
                                  HGdriverProcAddressQueryResult *symbolStatus)
{
    vppu_hggc_count(VPPU_GET_PROC_ADDRESS);

    HGresult (*real)(const char *, void **, int, hguint64_t,
                     HGdriverProcAddressQueryResult *) = vppu_hggc_next(VPPU_GET_PROC_ADDRESS);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(symbol, pfn, hggcVersion, flags, symbolStatus);
    if (rc == HGGC_SUCCESS) {
        substitute(symbol, pfn);
    }
    return rc;
}

/* The v1 resolver takes four parameters where the current one takes five, so it needs its own
 * definition rather than a forward to the above. The #undef is what reaches the plain exported
 * name at all: hggc.h maps it onto hgGetProcAddress_v2, which the definition above uses, so
 * this must come after it. */
#undef hgGetProcAddress

HGresult HGGCAPI hgGetProcAddress(const char *symbol, void **pfn, int hggcVersion,
                                  hguint64_t flags)
{
    vppu_hggc_count(VPPU_GET_PROC_ADDRESS_V1);

    HGresult (*real)(const char *, void **, int, hguint64_t) =
        vppu_hggc_next(VPPU_GET_PROC_ADDRESS_V1);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(symbol, pfn, hggcVersion, flags);
    if (rc == HGGC_SUCCESS) {
        substitute(symbol, pfn);
    }
    return rc;
}

/* report_table — name the table that was handed out, in hex, since the identifier is a raw
 * 16-byte value with no printable form of its own. */
static void report_table(const HGuuid *id)
{
    static const char digits[] = "0123456789abcdef";
    char text[2 * sizeof(id->bytes) + 1];
    size_t i;

    if (id == NULL) {
        vppu_log(VPPU_LOG_DEBUG, "hgGetExportTable: an unnamed table was handed out — entries "
                                 "reached through it are not interposed\n");
        return;
    }

    for (i = 0; i < sizeof(id->bytes); i++) {
        unsigned char byte = (unsigned char)id->bytes[i];

        text[2 * i] = digits[byte >> 4];
        text[2 * i + 1] = digits[byte & 0x0fU];
    }
    text[2 * i] = '\0';

    vppu_log(VPPU_LOG_DEBUG, "hgGetExportTable %s: an opaque table was handed out — entries "
                             "reached through it are not interposed\n",
             text);
}

HGresult HGGCAPI hgGetExportTable(const void **ppExportTable, const HGuuid *pExportTableId)
{
    vppu_hggc_count(VPPU_GET_EXPORT_TABLE);

    HGresult (*real)(const void **, const HGuuid *) = vppu_hggc_next(VPPU_GET_EXPORT_TABLE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(ppExportTable, pExportTableId);
    if (rc == HGGC_SUCCESS) {
        report_table(pExportTableId);
    }
    return rc;
}
