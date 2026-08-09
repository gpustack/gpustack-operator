/*
 * hip_table.h — what every interposed entry shares: a counter, a caller-origin diagnostic, and a
 * place in the dump.
 *
 * ENTRIES SELF-REGISTER FROM THE TRANSLATION UNIT THEY LIVE IN. There is no central list of
 * names here, and that is deliberate: a table listing every entry in one file would make each
 * family's change a diff against every other family's, and the families are the natural unit —
 * the reported-capacity entries, the classic allocating entries and the pool entries have nothing
 * to say to each other. What this file owns is the MECHANISM; what each family owns is its own
 * entries.
 *
 * THE COUNTERS ARE NOT DECORATION. `LIBVROCM_LOG_LEVEL=2` dumps them at exit, and the verification
 * cases decide rows by reading that dump — "the pool family was reached at all" is a question no
 * other output answers.
 */
#ifndef VROCM_HIP_HIP_TABLE_H
#define VROCM_HIP_HIP_TABLE_H

#include "common/vrocm.h"

struct vrocm_entry {
    const char *name;
    unsigned long long calls;
    unsigned long long denials;
    unsigned traced;           /* how many callers this entry has already reported */
    struct vrocm_entry *next;  /* linked on first hit, so the dump can walk what actually fired */
};

/* vrocm_entry_hit — count one call, and at level 2 name the object that made it.
 *
 * `return_address` is the caller's, from `__builtin_return_address(0)` at the wrapper's own top
 * frame. Only the first few firings per entry are reported: the point is to notice WHICH objects
 * call an entry, and a workload that allocates in a loop would otherwise bury it. */
VROCM_INTERNAL void vrocm_entry_hit(struct vrocm_entry *entry, void *return_address);

/* vrocm_entry_denied — count one refusal, so the dump distinguishes an entry that was never
 * reached from one that was reached and refused. */
VROCM_INTERNAL void vrocm_entry_denied(struct vrocm_entry *entry);

/* VROCM_EXPORT — an interposed entry point, visible in the dynamic symbol table.
 *
 * Stated on every wrapper rather than inherited. The library is compiled -fvisibility=hidden so
 * that common/ stays internal, and under that flag a definition is exported only if some
 * declaration gave it default visibility -- which, for these names, means only if ROCm's own
 * header happens to declare it. Two of them do not: the plain `hipGetDeviceProperties` is macro-
 * mapped away before the header can declare it, and `hipGetDevicePropertiesR0000` is not declared
 * at all. Both compiled clean, exported nothing, and would have interposed nothing. */
#define VROCM_EXPORT __attribute__((visibility("default")))

/* VROCM_ENTRY / VROCM_HIT — declare one entry beside its wrapper and count a call into it.
 *
 * `__builtin_return_address(0)` has to be taken in the wrapper itself, not inside a helper, which
 * is why this is a macro. */
#define VROCM_ENTRY(fn) static struct vrocm_entry vrocm_entry_##fn = { .name = #fn }
#define VROCM_HIT(fn) vrocm_entry_hit(&vrocm_entry_##fn, __builtin_return_address(0))
#define VROCM_DENIED(fn) vrocm_entry_denied(&vrocm_entry_##fn)

#endif /* VROCM_HIP_HIP_TABLE_H */
