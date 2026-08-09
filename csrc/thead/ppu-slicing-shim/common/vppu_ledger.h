/*
 * vppu_ledger.h — the cross-process ledger, its lock, and the usage region's layout.
 *
 * WHY A REGION AND NOT A LOCAL TABLE. A container's quota belongs to the container, not to
 * one process in it: with a process-local ledger two processes sharing one card are each
 * granted the whole figure, so a 4 GiB slice hands out 8 GiB. The accounting therefore lives
 * in a file-mapped region every process in the container maps.
 *
 * WHY A FILE AND NOT ONLY MEMORY. The region doubles as the usage surface. A slice's quota
 * and its consumption appear in no vendor field — `ppu-smi` has no maximum-SM column, exactly
 * as `nvidia-smi` has none — so without something to read, a compute cap can only be inferred
 * from an init log or a stress test. flexai's ledger is stateless and its documented answer is
 * a separate CLI; there is nothing to scrape, which is why that shape is rejected here.
 *
 * THE LAYOUT IS A CONTRACT. `tools/`, and later a metrics scraper, parse this region with no
 * access to this library's symbols, so it carries a magic and a layout version and the
 * assertions below pin every offset. A reader that finds a version it does not know must
 * refuse rather than misparse — that is what keeps the next added field from being a breaking
 * change.
 *
 * BYTE ORDER is the host's. Writer and readers are processes in one container on one machine;
 * a portable encoding would buy nothing and cost every field an accessor.
 *
 * THE LOCK IS PER CARD, taken as an fcntl() record lock on one byte of the arena below.
 *   - fcntl rather than a pthread mutex or a POSIX semaphore in the region: those need
 *     -lpthread on the glibc 2.17 floor this library must load against, which would put a
 *     second entry in DT_NEEDED and break the "nothing but libc.so.6" guarantee case 1
 *     asserts. flock() would work too but locks the whole file, i.e. the whole container.
 *   - per card rather than per container because the lock is HELD ACROSS THE VENDOR'S REAL
 *     ALLOCATION — that is what closes the check-then-allocate race — so a driver call that
 *     hangs stops one card's allocations instead of every process in the container. The
 *     holder writes its pid into the card's slot so `tools/` can name it.
 *   - a record lock is per process, so it does not exclude sibling threads. An in-process
 *     spinlock covers that half; both are taken in one order, and re-entry from the vendor's
 *     own call back into an interposed entry is counted rather than deadlocked.
 *   - the read side takes no lock at all. A usage figure that is one allocation stale is
 *     worth far more than a reader that can wedge on a hung allocation.
 */
#ifndef VPPU_COMMON_VPPU_LEDGER_H
#define VPPU_COMMON_VPPU_LEDGER_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "vppu.h"
#include "vppu_pid.h"

/* Eight bytes, no terminator, so `strings` on the region file identifies it. The length is
 * named because it is also the byte range the creation lock is taken on. */
#define VPPU_REGION_MAGIC "VPPUREGN"
#define VPPU_REGION_MAGIC_BYTES 8
#define VPPU_REGION_VERSION 1u

/* Per card, because a slot is only ever written while holding that card's lock, which makes
 * the whole table free of any cross-card synchronisation. Sized for a container's processes,
 * not a host's: a workload's main process plus its data loaders. */
#define VPPU_MAX_PROCESSES_PER_DEVICE 32

#define VPPU_ENV_LEDGER_PATH "HGGC_LEDGER_PATH"

/* /dev/shm because it is a tmpfs present in every container and shared by every process in
 * it, and because the allocator already mounts it read-write for the Ascend reader tool.
 *
 * THE DEFAULT IS ONLY SAFE FOR ONE CONTAINER, and the allocator therefore always sets the
 * variable — to a per-container directory under the pod work dir, for the reason in the
 * deviceplugin comment: this region is addressed by a CONTAINER-LOCAL card index, so a shared
 * location lets two containers' index 0 charge one slot.
 *
 * /dev/shm is container-private by default and two ordinary configurations make it shared:
 * `hostIPC: true` makes it the host's, so every container on the node meets in one region; and an
 * `emptyDir{medium: Memory}` mounted at /dev/shm is shared by a Pod's containers, which is the
 * usual answer to a data loader that finds the default 64 MiB too small. In either case two
 * containers charge two DIFFERENT physical cards into the same slot and nothing says so — the
 * quota is silently wrong rather than visibly absent. Treat this default as a convenience for a
 * single container run by hand, never as a substitute for the allocator setting the variable.
 * The AMD shim's `VROCM_LEDGER_PATH` carries the same shape and the same warning. */
#define VPPU_LEDGER_DEFAULT_PATH "/dev/shm/vppu-ledger"

/* One process's charge against one card. `pid` is 0 for a free slot; a slot whose process no
 * longer exists is reclaimed rather than left to hold the quota down forever. */
struct vppu_process_charge {
    int32_t pid;
    uint32_t reserved;
    uint64_t memory_bytes;
};

/* One card's quota, its consumption, and who is charged for it.
 *
 * `memory_used_bytes` is the sum of the live entries in `processes`. Keeping both is
 * deliberate: the aggregate makes the admission decision one read on the hot path, and the
 * per-process breakdown is what lets a dead process's charge be identified and reclaimed,
 * and what `tools/` prints.
 *
 * `memory_quota_bytes` and `sm_limit_percent` are refreshed from the environment on every
 * admission, never written once by whichever process created the region. That is the
 * difference from HAMi-core, where the first creator's limit is frozen into the cache and
 * changing it means deleting the file.
 *
 * `sm_util_percent` is what the compute controller last measured for this container, and
 * `control` is the loop that produced it. Both are published rather than kept private: the
 * gains are deliberately not inherited from the reference implementation, so the loop has to be
 * observable on hardware nobody has profiled. `control` occupies the four words layout version
 * 1 reserved for exactly this, so filling them in changes no offset. */
struct vppu_device_usage {
    uint64_t memory_quota_bytes;
    uint64_t memory_used_bytes;
    uint32_t sm_limit_percent;
    uint32_t sm_util_percent;
    int32_t lock_holder_pid;
    uint32_t reserved0;
    struct vppu_pid_state control;
    struct vppu_process_charge processes[VPPU_MAX_PROCESSES_PER_DEVICE];
};

struct vppu_region {
    char magic[VPPU_REGION_MAGIC_BYTES];
    uint32_t layout_version;
    uint32_t header_bytes; /* offset of devices[], so a newer header stays skippable */
    uint32_t device_slots;
    uint32_t process_slots;
    uint32_t reserved0;
    uint32_t reserved1;
    /* The record-lock arena: one byte per card, locked by offset and never read as data.
     * Its position is FROZEN at layout version 1 and may never move, because two processes
     * running different layout versions must still take the same byte for the same card —
     * otherwise they lock different offsets and exclude nobody. */
    unsigned char lock_arena[VPPU_MAX_DEVICES];
    struct vppu_device_usage devices[VPPU_MAX_DEVICES];
};

/* The contract, asserted rather than described. Every one of these is a documented offset
 * some other reader will hard-code, so a field reordered by accident must fail the build. */
_Static_assert(sizeof(struct vppu_process_charge) == 16, "process charge layout changed");
_Static_assert(sizeof(struct vppu_device_usage) == 576, "device usage layout changed");
_Static_assert(offsetof(struct vppu_device_usage, control) == 32,
               "the controller words may not move");
_Static_assert(offsetof(struct vppu_device_usage, processes) == 64,
               "process table offset changed");
_Static_assert(offsetof(struct vppu_region, magic) == 0, "magic must open the region");
_Static_assert(offsetof(struct vppu_region, lock_arena) == 32, "the lock arena may not move");
_Static_assert(offsetof(struct vppu_region, devices) == 96, "device table offset changed");
_Static_assert(sizeof(struct vppu_region) == 36960, "region size changed");

/* vppu_ledger_lock — take one card's lock, mapping the region on first use.
 *
 * Returns false when the region is unusable (unmappable, foreign, or a layout version this
 * build does not know) or when the caller already holds a DIFFERENT card. A false answer is
 * a refusal, never a free pass: the caller must deny.
 *
 * Re-entry on the same card is counted, so a vendor call made under this lock that reaches
 * back into another interposed entry does not deadlock against itself. Every successful call
 * must be matched by exactly one vppu_ledger_unlock().
 */
VPPU_INTERNAL bool vppu_ledger_lock(int device);
VPPU_INTERNAL void vppu_ledger_unlock(int device);

/* vppu_ledger_holding — whether this thread currently holds a card's lock.
 *
 * The compute controller asks because it must never make a caller wait while that caller is
 * inside an admission: the vendor's own allocation runs under the card's lock, and if it reaches
 * an interposed launch, throttling there would hold the lock for a whole control window and
 * stall every other process's allocations on that card. */
VPPU_INTERNAL bool vppu_ledger_holding(void);

/* vppu_ledger_used — one card's accounted total. Lock-free by design; see the header note. */
VPPU_INTERNAL unsigned long long vppu_ledger_used(int device);

/* vppu_ledger_note_config — record the figures this admission was decided against, so a
 * reader sees the quota actually in force rather than the one the region was created with.
 * Must be called with the card's lock held. */
VPPU_INTERNAL void vppu_ledger_note_config(int device, unsigned long long quota_bytes,
                                           unsigned int sm_percent);

/* vppu_ledger_note_util — record what the compute controller last measured for this container.
 * Lock-free like the read side: it is a diagnostic figure, and one window stale is worth more
 * than a control step that can block behind an allocation. */
VPPU_INTERNAL void vppu_ledger_note_util(int device, unsigned int sm_percent);

/* vppu_ledger_control — one card's controller words, or NULL when the region is unusable.
 *
 * A pointer INTO the shared mapping, deliberately: the compute controller reads it on every
 * launch and updates it once a window, so it cannot afford this file's lock — a record lock is a
 * system call, and a launch is not. The words are shared memory, so the controller synchronises
 * on them with atomics; what this directory owns is that they exist, are shared by every process
 * in the container, and never move. What they MEAN belongs to vppu_pid.h. */
VPPU_INTERNAL struct vppu_pid_state *vppu_ledger_control(int device);

/* vppu_ledger_has_process — whether one pid holds a slot in this card's table.
 *
 * This is how the compute controller tells its own container's utilisation samples from a
 * neighbouring container's: the region is per container, so the pids in it are this container's
 * by construction. A pid-namespace comparison could not answer that — a host pid may well be a
 * valid pid in this namespace too. */
VPPU_INTERNAL bool vppu_ledger_has_process(int device, int pid);

/* vppu_ledger_charge — add to this process's charge against one card.
 *
 * Returns false when the card's process table is full, which the caller must treat as a
 * refusal: an allocation that cannot be attributed cannot be reclaimed either, and letting
 * it through unaccounted is how a quota quietly stops being one. Must be called with the
 * card's lock held. */
VPPU_INTERNAL bool vppu_ledger_charge(int device, unsigned long long bytes);

/* vppu_ledger_refund — give bytes back. Clamped at zero rather than wrapping, because a
 * double free must not hand the container an unbounded quota. Must be called with the card's
 * lock held. */
VPPU_INTERNAL void vppu_ledger_refund(int device, unsigned long long bytes);

/* vppu_ledger_reclaim — drop the charges of processes that no longer exist and re-derive the
 * card's total from what is left; returns the bytes recovered.
 *
 * Called only when an admission would otherwise be refused, which is both where it matters
 * and rare enough that the liveness sweep costs nothing on the hot path. Without it a
 * process killed mid-allocation holds its charge for as long as the region file lives, which
 * is HAMi-core's stale-cache problem arriving by another route. Must be called with the
 * card's lock held. */
VPPU_INTERNAL unsigned long long vppu_ledger_reclaim(int device);

/* The key -> (card, bytes) map that lets a free return its bytes.
 *
 * PROCESS-LOCAL on purpose, and this is the one part of the ledger that must not be shared:
 * a device pointer is a value in one process's address space, so two processes can hold the
 * same one on different cards, and a shared table keyed by it would let one process's free
 * refund another's allocation. What has to be shared is the total, and that is what the
 * region carries. */
VPPU_INTERNAL void vppu_alloc_record(unsigned long long key, int device,
                                     unsigned long long bytes);

/* vppu_alloc_take — remove a key's record and report what it held. False when the key was
 * never recorded, which a caller must treat as "nothing to refund" rather than as an error:
 * an allocation made before this library loaded is exactly that case. */
VPPU_INTERNAL bool vppu_alloc_take(unsigned long long key, int *device,
                                   unsigned long long *bytes);

/* vppu_alloc_overflows — records dropped because the table was full. Reported in the counter
 * dump so a refund that never lands is distinguishable from a quota finding. */
VPPU_INTERNAL unsigned long long vppu_alloc_overflows(void);

#endif /* VPPU_COMMON_VPPU_LEDGER_H */
