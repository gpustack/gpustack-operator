/*
 * vrocm_ledger.h — the cross-process ledger, its lock, the admission that ties them together,
 * and the usage region's layout.
 *
 * WHY A REGION AND NOT A LOCAL TABLE. A container's quota belongs to the container, not to one
 * process in it: with a process-local ledger two processes sharing one card are each granted the
 * whole figure, so a 4 GiB slice hands out 8 GiB. Measured — two processes against one 4 GiB
 * card quota take 3 GiB and 1 GiB through the region, and 4 GiB each without it.
 *
 * WHY A FILE AND NOT ONLY MEMORY. The region doubles as the usage surface. A slice's quota and
 * its consumption appear in no vendor field — `rocm-smi` reads sysfs and the DRM nodes rather
 * than HIP, so it is not reached by this library at all and reports the physical card under any
 * quota — so without something to read, a memory cap can only be inferred from an init log.
 * `tools/rocm-monitor` reads this file, and a metrics scraper will later read the same bytes.
 *
 * THE LAYOUT IS A CONTRACT. Readers parse this region with no access to this library's symbols,
 * so it carries a magic and a layout version and the assertions below pin every offset. A reader
 * that finds a version it does not know must refuse rather than misparse — that is what keeps
 * the next added field from being a breaking change.
 *
 * BYTE ORDER is the host's. Writer and readers are processes in one container on one machine; a
 * portable encoding would buy nothing and cost every field an accessor.
 *
 * THE LOCK IS PER CARD, taken as an fcntl() record lock on one byte of the arena below.
 *   - fcntl rather than a pthread mutex or a POSIX semaphore in the region: those carry
 *     GLIBC_2.34, which would lift the product's floor off GLIBC_2.4 and lock it out of older
 *     workload images. flock() would work too, but locks the whole file — i.e. every card.
 *   - per card rather than per container because the lock is HELD ACROSS THE RUNTIME'S REAL
 *     ALLOCATION — that is what closes the check-then-allocate race — so a driver call that
 *     hangs stops one card's allocations instead of every process in the container. The holder
 *     writes its pid into the card's slot so a reader can name it.
 *   - a record lock is per PROCESS, so it does not exclude sibling threads. An in-process
 *     spinlock covers that half; both are taken in one order, and re-entry from the runtime's
 *     own call back into an interposed entry is COUNTED rather than deadlocked.
 *   - the read side takes no lock at all. A usage figure that is one allocation stale is worth
 *     far more than a reader that can wedge behind a hung allocation.
 *   - a fork() while the lock is held gives the child a COPY of the spinlock flag and none of the
 *     lock: record locks are not inherited, and fork carries over only the calling thread, so the
 *     child's copy is set with nobody left to release it. The ledger notices the pid has changed
 *     and resets its process-local state -- the flags, the key map, the re-entrancy counters --
 *     before touching any of it. The region is MAP_SHARED and is deliberately kept.
 */
#ifndef VROCM_COMMON_VROCM_LEDGER_H
#define VROCM_COMMON_VROCM_LEDGER_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "vrocm.h"

/* Eight bytes, no terminator, so `strings` on the region file identifies it. The length is named
 * because it is also the byte range the creation lock is taken on. */
#define VROCM_REGION_MAGIC "VROCMRGN"
#define VROCM_REGION_MAGIC_BYTES 8
#define VROCM_REGION_VERSION 1u

/* Per card, because a slot is only ever written while holding that card's lock, which makes the
 * whole table free of any cross-card synchronisation. Sized for a container's processes, not a
 * host's: a workload's main process plus its data loaders. */
#define VROCM_MAX_PROCESSES_PER_DEVICE 32

/* One process's charge against one card. `pid` is 0 for a free slot; a slot whose process no
 * longer exists is reclaimed rather than left holding the quota down forever. */
struct vrocm_process_charge {
    int32_t pid;
    uint32_t reserved;
    uint64_t memory_bytes;
};

/* One card's quota, its consumption, and who is charged for it.
 *
 * `memory_used_bytes` is the sum of the live entries in `processes`. Keeping both is deliberate:
 * the aggregate makes the admission decision one read on the hot path, and the per-process
 * breakdown is what lets a dead process's charge be identified and dropped, and what a reader
 * prints.
 *
 * `memory_quota_bytes` is refreshed from the environment on every admission, never written once
 * by whichever process created the region — see vrocm_quota.h for why that direction matters. */
struct vrocm_device_usage {
    uint64_t memory_quota_bytes;
    uint64_t memory_used_bytes;
    int32_t lock_holder_pid;
    uint32_t reserved0;
    uint64_t reserved1;
    struct vrocm_process_charge processes[VROCM_MAX_PROCESSES_PER_DEVICE];
};

struct vrocm_region {
    char magic[VROCM_REGION_MAGIC_BYTES];
    uint32_t layout_version;
    uint32_t header_bytes; /* offset of devices[], so a newer header stays skippable */
    uint32_t device_slots;
    uint32_t process_slots;
    uint32_t reserved0;
    uint32_t reserved1;
    /* The record-lock arena: one byte per card, locked by offset and never read as data. Its
     * position is FROZEN at layout version 1 and may never move, because two processes running
     * different layout versions must still take the same byte for the same card — otherwise they
     * lock different offsets and exclude nobody. */
    unsigned char lock_arena[VROCM_MAX_DEVICES];
    struct vrocm_device_usage devices[VROCM_MAX_DEVICES];
};

/* The contract, asserted rather than described. Every one of these is a documented offset some
 * other reader will hard-code, so a field reordered by accident must fail the build. */
_Static_assert(sizeof(struct vrocm_process_charge) == 16, "process charge layout changed");
_Static_assert(sizeof(struct vrocm_device_usage) == 544, "device usage layout changed");
_Static_assert(offsetof(struct vrocm_device_usage, processes) == 32,
               "process table offset changed");
_Static_assert(offsetof(struct vrocm_region, magic) == 0, "magic must open the region");
_Static_assert(offsetof(struct vrocm_region, lock_arena) == 32, "the lock arena may not move");
_Static_assert(offsetof(struct vrocm_region, devices) == 96, "device table offset changed");
_Static_assert(sizeof(struct vrocm_region) == 34912, "region size changed");

/* How an admission ended. Reported rather than collapsed to a bool because the three refusals
 * are three different operator problems — a quota that is too small, a configuration that never
 * arrived, and a table this build sized too tightly — and a caller that could not tell them
 * apart would log one message for all three. */
enum vrocm_admit {
    VROCM_ADMIT_OK = 0,
    VROCM_ADMIT_DENIED_QUOTA,     /* the charge would cross the card's figure */
    VROCM_ADMIT_DENIED_CONFIG,    /* no usable configuration, or the region is unusable */
    VROCM_ADMIT_DENIED_TRACKING,  /* nowhere left to record the charge -- see fail-closed, below */
    VROCM_ADMIT_ALLOC_FAILED,     /* the runtime itself refused; nothing was charged */
};

/* The caller's real allocation, invoked with the card's lock HELD.
 *
 * A function pointer rather than the caller sequencing lock/check/allocate/charge itself,
 * because that sequence is the one part of this design a reviewer cannot verify by reading the
 * caller: releasing the lock between the check and the charge reintroduces exactly the race the
 * lock exists to close, and it does so invisibly. Keeping the sequence here also keeps common/
 * free of every `hip*` type — the callback closes over them, this file never names one.
 *
 * `key` is whatever the caller will later present to refund the charge; for HIP that is the
 * returned device pointer. Returning false means the runtime refused.
 *
 * A key of ZERO means the allocation succeeded and produced nothing to refund, which is what a
 * zero-size request does: the runtime answers with success and a null pointer, and freeing a null
 * pointer is defined to do nothing. Nothing is charged and no slot is kept for it -- a slot kept
 * for an allocation no free can ever match is a slot kept for the life of the process.
 *
 * `bytes` is IN-OUT: in, what the admission was decided against; out, what the allocation
 * actually took. They differ for a pitched allocation, where the runtime picks the stride and the
 * caller only knows the width it asked for. Revising it here rather than through a second call is
 * what keeps the reconciliation under the SAME lock as the check — a second call would have to
 * take the lock again, and by then another process could have decided against the stale total. */
typedef bool (*vrocm_alloc_fn)(void *ctx, unsigned long long *key, unsigned long long *bytes);

/* vrocm_ledger_admit — the whole admission, under ONE acquisition of the card's lock.
 *
 * In order: reserve somewhere to record the charge, re-read the card's quota from the
 * environment, sweep dead processes if the charge would not otherwise fit, decide, call
 * `alloc`, and commit the charge against the key it produced.
 *
 * THE TRACKING RESERVATION COMES FIRST, and that ordering is the fail-closed part. An
 * allocation this build cannot record is one it can never refund either, so letting it through
 * unaccounted is how a quota quietly stops being one; reserving before allocating means such a
 * request is refused without ever reaching the runtime, so there is nothing to undo. */
VROCM_INTERNAL enum vrocm_admit vrocm_ledger_admit(int device, unsigned long long bytes,
                                                   vrocm_alloc_fn alloc, void *ctx);

/* vrocm_ledger_release — refund the charge recorded against `key`, and report what it held.
 *
 * False when the key was never recorded, which a caller must treat as "nothing to refund"
 * rather than as an error: an allocation made before this library loaded is exactly that case,
 * and so is a pointer the workload invented. */
VROCM_INTERNAL bool vrocm_ledger_release(unsigned long long key, int *device,
                                         unsigned long long *bytes);

/* vrocm_ledger_used — one card's accounted total. Lock-free by design; see the header note. */
VROCM_INTERNAL unsigned long long vrocm_ledger_used(int device);

/* vrocm_ledger_quota — the figure the card's last admission was decided against, as a reader
 * sees it. Lock-free, and 0 when the region is unusable or the card has seen no admission. */
VROCM_INTERNAL unsigned long long vrocm_ledger_quota(int device);

/* vrocm_ledger_reclaim — drop the charges of processes that no longer exist and re-derive the
 * card's total from what is left; returns the bytes recovered.
 *
 * Called only when an admission would otherwise be refused, which is both where it matters and
 * rare enough that the liveness sweep costs nothing on the hot path. Without it a process killed
 * mid-allocation holds its charge for as long as the region file lives — measured: a process
 * SIGKILLed while holding 4 GiB of a 6 GiB quota left the next process able to claim only 2.
 * Must be called with the card's lock held. */
VROCM_INTERNAL unsigned long long vrocm_ledger_reclaim(int device);

/* vrocm_ledger_holding — whether this thread currently holds a card's lock, and which.
 *
 * Exists for the callback: an allocation callback that reached back into another interposed
 * entry would be re-entering under the lock, and the entry it reaches needs to know that rather
 * than deadlock or double-charge. */
VROCM_INTERNAL bool vrocm_ledger_holding(int *device);

/* vrocm_ledger_lock_epochs — how many times a card's lock has been taken from unheld, across
 * this process's lifetime.
 *
 * Diagnostic in production and load-bearing in test: it is what lets a unit test assert that a
 * whole admission cost exactly ONE acquisition, which is the only way to prove the check and the
 * charge were not separated. Re-entrant acquisitions do not count. */
VROCM_INTERNAL unsigned long long vrocm_ledger_lock_epochs(void);

/* vrocm_ledger_tracking_refusals — admissions refused for want of a tracking slot. Reported in
 * the counter dump so a container that outgrew the table is distinguishable from one that is
 * merely at its quota. */
VROCM_INTERNAL unsigned long long vrocm_ledger_tracking_refusals(void);

#endif /* VROCM_COMMON_VROCM_LEDGER_H */
