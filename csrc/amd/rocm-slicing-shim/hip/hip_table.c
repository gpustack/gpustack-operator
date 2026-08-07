#include "common/vrocm_ledger.h"
#include "common/vrocm_log.h"
#include "common/vrocm_quota.h"
#include "hip/hip_resolve.h"
#include "hip/hip_table.h"

/* How many callers one entry reports before it goes quiet. Three is enough to see whether an entry
 * is reached by one object or several, which is the whole question, and small enough that a
 * workload allocating in a loop does not bury it. */
#define TRACE_LIMIT 3

static struct vrocm_entry *entries;
static volatile int entries_lock;

static void link_entry(struct vrocm_entry *entry)
{
    while (__sync_lock_test_and_set(&entries_lock, 1)) {
        /* The read is atomic because the writes are: `volatile` orders this thread's accesses and
         * says nothing about anyone else's, so a plain read of a word another thread is setting
         * with an atomic RMW is a data race however single an instruction it compiles to. Relaxed
         * is enough -- the acquire that matters is the test-and-set this loop returns to. */
        while (__atomic_load_n(&entries_lock, __ATOMIC_RELAXED)) {
            /* The same pause the card's lock spins on, for a far shorter hold: three lines, and
             * only on an entry's first call. Contention is a start-up shape rather than a steady
             * one -- a framework bringing up its worker threads reaches its first allocation on
             * all of them at once -- and it costs nothing to not burn a sibling's issue slots
             * while the one thread that holds this lock finishes. */
            VROCM_SPIN_PAUSE();
        }
    }
    if (entry->next == NULL && entries != entry) {
        entry->next = entries;
        /* PUBLISHED WITH A RELEASE, because the reader is not under this lock. The destructor
         * walks the list without taking it -- a spin it could never be sure of taking, since a
         * thread killed mid-link would leave the flag set and hang the process at exit -- so the
         * head has to carry the ordering itself. `next` is written above, so a reader that
         * acquires this head sees a chain that is already complete; nothing is ever unlinked or
         * freed, which is what makes walking it afterwards safe. */
        __atomic_store_n(&entries, entry, __ATOMIC_RELEASE);
    }
    __sync_lock_release(&entries_lock);
}

VROCM_INTERNAL void vrocm_entry_hit(struct vrocm_entry *entry, void *return_address)
{
    if (__sync_fetch_and_add(&entry->calls, 1) == 0) {
        link_entry(entry);
    }

    if (vrocm_log_level() < VROCM_LOG_DEBUG || entry->traced >= TRACE_LIMIT) {
        return;
    }
    entry->traced++;

    {
        const char *caller = vrocm_caller_of(return_address);

        /* A line naming libamdhip64 under an ALLOCATING entry is the signal that the one thing a
         * preload cannot do — decline to fire on a runtime-internal call — has started to matter.
         * Measured today it never appears: across a PyTorch run to OOM every call came from the
         * framework's own objects. See the escalation path in the spec's Risks. */
        vrocm_log(VROCM_LOG_DEBUG, "%s <- %s\n", entry->name,
                  caller != NULL ? caller : "(unknown)");
    }
}

VROCM_INTERNAL void vrocm_entry_denied(struct vrocm_entry *entry)
{
    __sync_fetch_and_add(&entry->denials, 1);
}

/* The load marker. Validating here rather than from a constructor in common/ keeps the order
 * explicit instead of dependent on link order, which is what F1 asks for: this is the shipped
 * library's own constructor, and it runs before any wrapper can. */
__attribute__((constructor)) static void vrocm_load(void)
{
    vrocm_log(VROCM_LOG_DEBUG, "loaded\n");
    vrocm_quota_validate();
}

/* The counter dump. Only entries that actually fired are linked, so the dump reads as "what this
 * process touched" rather than as a catalogue of everything the library could have touched —
 * which is the form the cases grep. */
__attribute__((destructor)) static void vrocm_unload(void)
{
    struct vrocm_entry *entry;
    int device;

    if (vrocm_log_level() < VROCM_LOG_DEBUG) {
        return;
    }
    /* The acquire that pairs with link_entry's release, and relaxed reads of the two counters,
     * which are incremented atomically by every wrapper: threads can still be running while a
     * destructor runs, so this dump is the one reader that races with the whole table. Every
     * figure here is a count, and a count read a few increments late is still the answer this
     * line is for. */
    for (entry = __atomic_load_n(&entries, __ATOMIC_ACQUIRE); entry != NULL; entry = entry->next) {
        vrocm_log(VROCM_LOG_DEBUG, "counter %s calls=%llu denials=%llu\n", entry->name,
                  __atomic_load_n(&entry->calls, __ATOMIC_RELAXED),
                  __atomic_load_n(&entry->denials, __ATOMIC_RELAXED));
    }
    for (device = 0; device < VROCM_MAX_DEVICES; device++) {
        unsigned long long quota = vrocm_ledger_quota(device);

        if (quota != 0) {
            vrocm_log(VROCM_LOG_DEBUG, "counter card %d quota=%llu used=%llu\n", device, quota,
                      vrocm_ledger_used(device));
        }
    }
    vrocm_log(VROCM_LOG_DEBUG, "counter tracking_refusals=%llu lock_epochs=%llu\n",
              vrocm_ledger_tracking_refusals(), vrocm_ledger_lock_epochs());
}
