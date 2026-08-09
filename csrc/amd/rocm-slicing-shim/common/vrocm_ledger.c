#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include "vrocm_ledger.h"
#include "vrocm_log.h"
#include "vrocm_quota.h"

/* The process-local key -> (card, bytes) map.
 *
 * PROCESS-LOCAL on purpose, and this is the one part of the ledger that must not be shared: a
 * device pointer is a value in one process's address space, so two processes can hold the same
 * one on different cards, and a shared table keyed by it would let one process's free refund
 * another's allocation. What has to be shared is the total, and that is what the region carries.
 *
 * Fixed capacity, because allocating to track an allocation is a recursion this library cannot
 * afford: the interposed entry would call malloc, which is fine, but a shim that grows its own
 * table under the runtime's allocator is a failure mode nobody wants to debug at 3am. A table
 * that fills is a refusal, never a silent drop -- see the fail-closed note in the header. */
#define KEY_SLOTS 16384

enum { SLOT_FREE = 0, SLOT_RESERVED, SLOT_LIVE };

struct key_slot {
    unsigned long long key;
    unsigned long long bytes;
    int device;
    int state;
};

static struct key_slot keys[KEY_SLOTS];
static volatile int keys_lock;

static unsigned long long lock_epochs;
static unsigned long long tracking_refusals;

/* Latched so the "this process has no usable configuration" line is said once rather than on
 * every allocation the refusal covers -- see the refusal in vrocm_ledger_admit. */
static int config_refusal_said;

/* One flag per card, because fcntl() record locks are held by the PROCESS: two threads of one
 * process both succeed on the same byte, so the record lock alone would not exclude siblings. */
static volatile int device_lock[VROCM_MAX_DEVICES];

/* Re-entrancy is counted per thread, so a runtime call made under the card's lock that reaches
 * back into another interposed entry does not deadlock against itself. */
static __thread int held_device = -1;
static __thread unsigned held_depth;

static void spin_lock(volatile int *flag)
{
    while (__sync_lock_test_and_set(flag, 1)) {
        /* Read atomically because it is written atomically: `volatile` orders this thread's
         * accesses and says nothing about anyone else's, so a plain read of a word another thread
         * is setting with an atomic RMW is a data race however single an instruction it compiles
         * to. Relaxed suffices -- the acquire is the test-and-set this loop returns to. */
        while (__atomic_load_n(flag, __ATOMIC_RELAXED)) {
            /* A spin rather than a wait, because sched_yield() would cost a syscall on a path
             * this design keeps free of them -- but a spin with a pause in it, and here that is
             * not a formality. The waiter can be held for the length of the card's whole
             * critical section, since the holder may itself be blocked in F_SETLKW behind
             * another process's driver call: this loop is not always the handful of cycles that
             * a counter's lock makes it. */
            VROCM_SPIN_PAUSE();
        }
    }
}

static void spin_unlock(volatile int *flag)
{
    __sync_lock_release(flag);
}

/* ---- the region ---------------------------------------------------------------------- */

static struct vrocm_region *region_map;
static int region_fd = -1;

/* The latch is a state machine rather than a flag because opening is not instantaneous: the body
 * below blocks in F_SETLKW for as long as another PROCESS takes to initialise the file. A thread
 * arriving during that window has to be able to tell "not decided yet" from "decided, and the
 * answer is no" -- reading the first as the second refuses an allocation on an empty card. */
enum { REGION_UNTRIED = 0, REGION_USABLE, REGION_UNUSABLE, REGION_OPENING };

static volatile int region_state;

/* ---- fork ------------------------------------------------------------------------------- */

/* Everything declared above is process-local, and fork() hands the child a copy of all of it.
 * Two of those copies are actively harmful:
 *
 *  - A SPINLOCK INHERITED IN THE HELD STATE has no owner in the child, because fork carries over
 *    only the calling thread. `spin_lock` would spin on it forever, at 100% of a core, with no
 *    error and no log line. The window is not narrow: `device_lock` is held across the runtime's
 *    real allocation, and `keys_lock` across a 16384-slot scan taken on every allocation and
 *    every free. The re-entrancy counters are worse than a hang -- a child that inherits
 *    `held_depth` > 0 believes it already holds a card lock it does not hold, and proceeds to
 *    account with no lock at all. The region's latch is the same shape: a fork landing while
 *    another thread is inside `region_create` leaves the child a latch reading "in progress"
 *    with no thread left to finish it, so a child that kept it would wait on an answer that can
 *    never arrive. Sent back to untried, the child opens its own.
 *  - THE KEY MAP INHERITED WITH THE PARENT'S LIVE ENTRIES lets the child refund charges it never
 *    made: an inherited pointer freed in the child takes the parent's bytes off the card total
 *    and off a slot the child then keys to its own pid, and the next sweep re-derives the total
 *    from those slots and makes the loss permanent.
 *
 * The usual answer is pthread_atfork(), which this library cannot call: the pthread names would
 * raise its glibc floor above the GLIBC_2.4 that the .symver pins exist to hold. So the check is
 * made where the state is used. The owning pid is recorded, and a pid that no longer matches
 * means this process is a child that has not reset yet.
 *
 * THE REGION ITSELF IS NOT RESET, only its latch. The mapping is MAP_SHARED, so the child seeing
 * the same bytes as the parent is the design rather than a bug, and its descriptor survives the
 * fork. POSIX does not pass record locks to a child, so the child genuinely holds none -- which
 * is exactly what clearing `device_lock` assumes. */
static int owner_pid;

static void fork_reset(void)
{
    int self = (int)getpid();

    if (owner_pid == self) {
        return;
    }
    if (owner_pid == 0) {
        /* First use in this process. The state is still as the loader left it. */
        owner_pid = self;
        return;
    }

    if (region_state == REGION_OPENING) {
        region_state = REGION_UNTRIED;
    }
    memset((void *)device_lock, 0, sizeof(device_lock));
    keys_lock = 0;
    memset(keys, 0, sizeof(keys));
    held_device = -1;
    held_depth = 0;
    /* Counters, not state: a child reporting the parent's tallies would describe work it never
     * did, and both are read back as this process's own. The "said once" latch goes with them --
     * the line names the process, so a child that inherited it silenced would be refused with no
     * reason of its own. */
    lock_epochs = 0;
    tracking_refusals = 0;
    config_refusal_said = 0;
    owner_pid = self;
}

/* record_lock — one fcntl() record lock, on `len` bytes at `offset`. Separated from its callers
 * because the creation lock and the per-card lock differ only in where they point. */
static bool record_lock(int fd, int type, off_t offset, off_t len)
{
    struct flock fl;

    memset(&fl, 0, sizeof(fl));
    fl.l_type = (short)type;
    fl.l_whence = SEEK_SET;
    fl.l_start = offset;
    fl.l_len = len;

    while (fcntl(fd, F_SETLKW, &fl) < 0) {
        if (errno == EINTR) {
            continue;
        }
        return false;
    }
    return true;
}

/* region_create — map the region, creating and initialising it if this process is the first.
 *
 * Runs exactly once per process, on the one thread `region_open` elects; it does no latching of
 * its own.
 *
 * The file is created 0600, which means whichever container reaches the path first owns it: a
 * sibling running as a different uid is refused at open() and accounts nothing. That is the
 * conservative direction -- a wider mode would let any uid on the node rewrite another
 * container's totals -- and the shape it assumes is one uid per ledger path. */
static struct vrocm_region *region_create(void)
{
    const char *path;
    struct vrocm_region *map;

    path = vrocm_quota_ledger_path();
    if (path == NULL) {
        vrocm_log(VROCM_LOG_DENY, "%s is unset; nothing can be accounted\n",
                  VROCM_ENV_LEDGER_PATH);
        return NULL;
    }

    region_fd = open(path, O_RDWR | O_CREAT | O_CLOEXEC, 0600);
    if (region_fd < 0) {
        vrocm_log(VROCM_LOG_DENY, "cannot open ledger %s\n", path);
        return NULL;
    }
    if (ftruncate(region_fd, (off_t)sizeof(struct vrocm_region)) < 0) {
        vrocm_log(VROCM_LOG_DENY, "cannot size ledger %s\n", path);
        close(region_fd);
        region_fd = -1;
        return NULL;
    }

    map = mmap(NULL, sizeof(struct vrocm_region), PROT_READ | PROT_WRITE, MAP_SHARED, region_fd, 0);
    if (map == MAP_FAILED) {
        vrocm_log(VROCM_LOG_DENY, "cannot map ledger %s\n", path);
        close(region_fd);
        region_fd = -1;
        return NULL;
    }

    /* Initialisation races two ways -- two processes creating at once, and one reading while the
     * other writes the header -- so both sides take the same lock on the magic bytes. A freshly
     * created file is all zeroes, which is exactly the state "no magic yet". */
    if (!record_lock(region_fd, F_WRLCK, 0, VROCM_REGION_MAGIC_BYTES)) {
        vrocm_log(VROCM_LOG_DENY, "cannot lock ledger %s for initialisation\n", path);
        munmap(map, sizeof(struct vrocm_region));
        close(region_fd);
        region_fd = -1;
        return NULL;
    }
    if (memcmp(map->magic, VROCM_REGION_MAGIC, VROCM_REGION_MAGIC_BYTES) != 0) {
        static const char zeroes[VROCM_REGION_MAGIC_BYTES];

        /* An all-zero magic is a file this process just created, and initialising it is the whole
         * point. Any OTHER magic is somebody else's file: overwriting it would silently destroy
         * whatever it was, and the path is operator-supplied, so a typo must be refused rather
         * than acted on. */
        if (memcmp(map->magic, zeroes, VROCM_REGION_MAGIC_BYTES) != 0) {
            record_lock(region_fd, F_UNLCK, 0, VROCM_REGION_MAGIC_BYTES);
            vrocm_log(VROCM_LOG_DENY, "%s is not a ledger; refusing to overwrite it\n", path);
            munmap(map, sizeof(struct vrocm_region));
            close(region_fd);
            region_fd = -1;
            return NULL;
        }
        map->layout_version = VROCM_REGION_VERSION;
        map->header_bytes = (uint32_t)offsetof(struct vrocm_region, devices);
        map->device_slots = VROCM_MAX_DEVICES;
        map->process_slots = VROCM_MAX_PROCESSES_PER_DEVICE;
        /* Written LAST, so a reader that sees the magic sees a header that is already complete. */
        memcpy(map->magic, VROCM_REGION_MAGIC, VROCM_REGION_MAGIC_BYTES);
    }
    record_lock(region_fd, F_UNLCK, 0, VROCM_REGION_MAGIC_BYTES);

    /* A version this build does not know is a refusal, not a best effort: the offsets it would
     * read are exactly the ones that may have moved. */
    if (map->layout_version != VROCM_REGION_VERSION) {
        vrocm_log(VROCM_LOG_DENY, "ledger %s is layout version %u, this build speaks %u\n", path,
                  (unsigned)map->layout_version, VROCM_REGION_VERSION);
        munmap(map, sizeof(struct vrocm_region));
        close(region_fd);
        region_fd = -1;
        return NULL;
    }

    vrocm_log(VROCM_LOG_DEBUG, "ledger %s mapped (layout %u)\n", path, VROCM_REGION_VERSION);
    return map;
}

/* region_open — the region, or NULL if this process cannot use one.
 *
 * Latched, so a broken configuration costs one open() rather than one per allocation and a usable
 * one costs no repeated syscalls on the hot path. One thread is elected to do the work and the
 * rest wait for its answer: opening twice would map the region twice and, because POSIX ends a
 * file's record locks for the WHOLE process as soon as ANY descriptor on it is closed, the loser
 * closing its own fd would drop the card locks the winner is holding. */
static struct vrocm_region *region_open(void)
{
    struct vrocm_region *map;
    int state;

    for (;;) {
        state = __sync_val_compare_and_swap(&region_state, REGION_UNTRIED, REGION_OPENING);
        if (state == REGION_UNTRIED) {
            break; /* elected */
        }
        if (state == REGION_USABLE) {
            return region_map;
        }
        if (state == REGION_UNUSABLE) {
            return NULL;
        }
        /* Another thread is inside region_create(), where it can sit in F_SETLKW for as long as
         * a different process takes to initialise the file. Its answer is the one this thread
         * wants, so wait for it rather than treat "undecided" as "unusable". */
        while (region_state == REGION_OPENING) {
        }
    }

    map = region_create();
    region_map = map;
    /* A full barrier, so a thread that sees the settled state also sees `region_map`. */
    __sync_val_compare_and_swap(&region_state, REGION_OPENING,
                                map != NULL ? REGION_USABLE : REGION_UNUSABLE);
    return map;
}

/* ---- the per-card lock --------------------------------------------------------------- */

static bool device_valid(int device)
{
    return device >= 0 && device < VROCM_MAX_DEVICES;
}

static bool ledger_lock(int device)
{
    if (!device_valid(device)) {
        return false;
    }
    if (held_depth > 0) {
        /* Re-entry on the same card is counted. Re-entry on a DIFFERENT card is refused rather
         * than nested, because two cards taken in two orders by two threads is a deadlock, and
         * no path in this library legitimately needs both at once. */
        if (held_device != device) {
            return false;
        }
        held_depth++;
        return true;
    }

    spin_lock(&device_lock[device]);
    if (!record_lock(region_fd, F_WRLCK,
                     (off_t)offsetof(struct vrocm_region, lock_arena) + device, 1)) {
        spin_unlock(&device_lock[device]);
        return false;
    }

    held_device = device;
    held_depth = 1;
    lock_epochs++;
    region_map->devices[device].lock_holder_pid = (int32_t)getpid();
    return true;
}

static void ledger_unlock(int device)
{
    if (held_depth == 0 || held_device != device) {
        return;
    }
    if (--held_depth > 0) {
        return;
    }

    region_map->devices[device].lock_holder_pid = 0;
    held_device = -1;
    record_lock(region_fd, F_UNLCK, (off_t)offsetof(struct vrocm_region, lock_arena) + device, 1);
    spin_unlock(&device_lock[device]);
}

VROCM_INTERNAL bool vrocm_ledger_holding(int *device)
{
    fork_reset();
    if (held_depth == 0) {
        return false;
    }
    if (device != NULL) {
        *device = held_device;
    }
    return true;
}

/* ---- the key map --------------------------------------------------------------------- */

static int slot_reserve(void)
{
    int i;

    spin_lock(&keys_lock);
    for (i = 0; i < KEY_SLOTS; i++) {
        if (keys[i].state == SLOT_FREE) {
            keys[i].state = SLOT_RESERVED;
            spin_unlock(&keys_lock);
            return i;
        }
    }
    spin_unlock(&keys_lock);
    return -1;
}

static void slot_commit(int index, unsigned long long key, int device, unsigned long long bytes)
{
    spin_lock(&keys_lock);
    keys[index].key = key;
    keys[index].device = device;
    keys[index].bytes = bytes;
    keys[index].state = SLOT_LIVE;
    spin_unlock(&keys_lock);
}

static void slot_free(int index)
{
    spin_lock(&keys_lock);
    memset(&keys[index], 0, sizeof(keys[index]));
    spin_unlock(&keys_lock);
}

/* Linear, and deliberately so: a free walks 16384 slots of two words each, which is a few
 * microseconds against a driver call that is orders of magnitude slower, and an index would be
 * one more structure to keep consistent under the same lock for no measurable gain. */
static bool slot_take(unsigned long long key, int *device, unsigned long long *bytes)
{
    int i;

    spin_lock(&keys_lock);
    for (i = 0; i < KEY_SLOTS; i++) {
        if (keys[i].state == SLOT_LIVE && keys[i].key == key) {
            *device = keys[i].device;
            *bytes = keys[i].bytes;
            memset(&keys[i], 0, sizeof(keys[i]));
            spin_unlock(&keys_lock);
            return true;
        }
    }
    spin_unlock(&keys_lock);
    return false;
}

/* ---- charges ------------------------------------------------------------------------- */

/* charge_slot — this pid's slot on one card, claiming a free one if it has none yet. NULL when
 * the card's process table is full, which the caller must treat as a refusal for the same reason
 * a full key map is one. Must be called with the card's lock held. */
static struct vrocm_process_charge *charge_slot(struct vrocm_device_usage *usage, int pid)
{
    struct vrocm_process_charge *free_slot = NULL;
    int i;

    for (i = 0; i < VROCM_MAX_PROCESSES_PER_DEVICE; i++) {
        if (usage->processes[i].pid == pid) {
            return &usage->processes[i];
        }
        if (free_slot == NULL && usage->processes[i].pid == 0) {
            free_slot = &usage->processes[i];
        }
    }
    if (free_slot != NULL) {
        free_slot->pid = (int32_t)pid;
        free_slot->memory_bytes = 0;
    }
    return free_slot;
}

VROCM_INTERNAL unsigned long long vrocm_ledger_reclaim(int device)
{
    struct vrocm_device_usage *usage;
    unsigned long long before, total = 0;
    int i;

    if (region_map == NULL || !device_valid(device)) {
        return 0;
    }
    usage = &region_map->devices[device];
    before = usage->memory_used_bytes;

    for (i = 0; i < VROCM_MAX_PROCESSES_PER_DEVICE; i++) {
        struct vrocm_process_charge *slot = &usage->processes[i];

        if (slot->pid == 0) {
            continue;
        }
        /* kill(pid, 0) is answered in THIS process's pid namespace, and a Pod's containers each
         * have their own unless the Pod asks for shareProcessNamespace. So the answer is exact
         * for an entry this process wrote and a guess for one written from another namespace:
         * absent here, and a live charge is reclaimed; present here by coincidence, and a dead
         * one is kept. The sweep is therefore sound within a namespace, which is the shape one
         * ledger path per container gives it. */
        if (kill((pid_t)slot->pid, 0) < 0 && errno == ESRCH) {
            vrocm_log(VROCM_LOG_DEBUG, "card %d: reclaiming %llu bytes from dead pid %d\n", device,
                      (unsigned long long)slot->memory_bytes, (int)slot->pid);
            memset(slot, 0, sizeof(*slot));
            continue;
        }
        total += slot->memory_bytes;
    }

    /* Re-derived from the live slots rather than decremented, so a total that had already drifted
     * is corrected by the sweep instead of carried forward. */
    usage->memory_used_bytes = total;
    return before > total ? before - total : 0;
}

/* over_quota — `used + bytes > quota`, asked without forming the sum.
 *
 * THE SUM IS THE PROBLEM. `bytes` is whatever the workload passed to the runtime, so a request
 * near the top of the range wraps the addition and lands under the figure -- and the answer
 * "this fits" is the one direction this design must never reach by accident. The subtraction
 * cannot wrap in its turn, because it is only reached once `used <= quota` has been settled. */
static bool over_quota(unsigned long long used, unsigned long long bytes, unsigned long long quota)
{
    return used > quota || bytes > quota - used;
}

VROCM_INTERNAL enum vrocm_admit vrocm_ledger_admit(int device, unsigned long long bytes,
                                                   vrocm_alloc_fn alloc, void *ctx)
{
    struct vrocm_device_usage *usage;
    struct vrocm_process_charge *slot;
    unsigned long long quota, key = 0, charged;
    int index;

    fork_reset();
    if (!vrocm_quota_usable()) {
        /* Said HERE, and once. The load-time complaint had to go quiet for a process that was
         * never configured -- this library loads into every process in the container, and a
         * start-up script's `sed` is not a workload -- but a process that actually asks for
         * memory and is refused deserves the reason, and it is the only one that does. */
        if (__atomic_exchange_n(&config_refusal_said, 1, __ATOMIC_RELAXED) == 0) {
            vrocm_log(VROCM_LOG_DENY,
                      "no usable quota configuration; every allocation in this process is"
                      " refused\n");
        }
        return VROCM_ADMIT_DENIED_CONFIG;
    }
    if (!device_valid(device) || alloc == NULL) {
        return VROCM_ADMIT_DENIED_CONFIG;
    }
    quota = vrocm_quota_memory_bytes(device);
    if (quota == 0) {
        return VROCM_ADMIT_DENIED_CONFIG;
    }
    if (region_open() == NULL) {
        return VROCM_ADMIT_DENIED_CONFIG;
    }

    /* Reserved BEFORE the lock and before the runtime is ever called: an allocation this build
     * cannot record is refused outright, so there is no successful allocation to undo. */
    index = slot_reserve();
    if (index < 0) {
        tracking_refusals++;
        vrocm_log(VROCM_LOG_DENY, "card %d: no tracking slot left for %llu bytes; refusing\n",
                  device, bytes);
        return VROCM_ADMIT_DENIED_TRACKING;
    }

    if (!ledger_lock(device)) {
        slot_free(index);
        return VROCM_ADMIT_DENIED_CONFIG;
    }

    usage = &region_map->devices[device];
    /* Re-read every time, so a container restarted with a different figure is decided against
     * the figure it was actually given rather than the one the region was created with. */
    usage->memory_quota_bytes = quota;

    if (over_quota(usage->memory_used_bytes, bytes, quota)) {
        /* Only now, because the sweep is a syscall per live slot and the hot path should not pay
         * for it while the card is comfortably under its figure. */
        vrocm_ledger_reclaim(device);
    }

    if (over_quota(usage->memory_used_bytes, bytes, quota)) {
        unsigned long long used = usage->memory_used_bytes;

        ledger_unlock(device);
        slot_free(index);
        vrocm_log(VROCM_LOG_DENY, "card %d: %llu bytes refused, %llu of %llu already held\n",
                  device, bytes, used, quota);
        return VROCM_ADMIT_DENIED_QUOTA;
    }

    slot = charge_slot(usage, (int)getpid());
    if (slot == NULL) {
        ledger_unlock(device);
        slot_free(index);
        tracking_refusals++;
        vrocm_log(VROCM_LOG_DENY, "card %d: process table full; refusing %llu bytes\n", device,
                  bytes);
        return VROCM_ADMIT_DENIED_TRACKING;
    }

    /* THE LOCK IS STILL HELD, and this is the line that whole design exists for: the runtime's
     * real allocation happens between the check above and the charge below, so no other process
     * can decide against the same free figure and hand out the same bytes twice. */
    charged = bytes;
    if (!alloc(ctx, &key, &charged)) {
        ledger_unlock(device);
        slot_free(index);
        return VROCM_ADMIT_ALLOC_FAILED;
    }

    /* A successful allocation that produced no key is a zero-size one: the runtime answers with
     * success and a null pointer, and the free that matches it is `hipFree(NULL)`, which is
     * defined to do nothing. No call will ever arrive to give this slot back, so committing one
     * leaks a slot for every zero-size request -- and a framework that makes them in a loop, as
     * one does over a batch that happens to be empty, exhausts the 16384-slot map and the card
     * begins refusing real allocations while it is empty. Measured before this guard: 17001
     * zero-size requests, then 64 MiB against a 4096 MiB quota on an unused card, refused with
     * hipErrorOutOfMemory. Nothing was taken from the card either, so there is nothing to charge;
     * charging bytes that no free can ever refund would leak the quota instead of the map. */
    if (key == 0) {
        ledger_unlock(device);
        slot_free(index);
        return VROCM_ADMIT_OK;
    }

    /* The allocation may have taken more than it was admitted for -- a pitched allocation is
     * decided on the caller's width and settled on the runtime's stride. It is charged for what
     * it took, and the overrun is REPORTED rather than refused: freeing a successful allocation
     * behind the caller's back would break a working workload over padding it never asked for.
     * The card can therefore sit slightly over its figure, which the next admission sees and
     * refuses on. */
    if (charged > bytes) {
        vrocm_log(VROCM_LOG_DENY,
                  "card %d: admitted %llu bytes, allocation took %llu; charging what it took\n",
                  device, bytes, charged);
    }

    usage->memory_used_bytes += charged;
    slot->memory_bytes += charged;
    slot_commit(index, key, device, charged);
    ledger_unlock(device);
    return VROCM_ADMIT_OK;
}

VROCM_INTERNAL bool vrocm_ledger_release(unsigned long long key, int *device,
                                         unsigned long long *bytes)
{
    struct vrocm_device_usage *usage;
    struct vrocm_process_charge *slot;
    unsigned long long held;
    int card;

    fork_reset();
    if (!slot_take(key, &card, &held)) {
        return false;
    }
    if (device != NULL) {
        *device = card;
    }
    if (bytes != NULL) {
        *bytes = held;
    }
    if (region_open() == NULL || !ledger_lock(card)) {
        /* The key is gone either way; reporting the refund is all that is lost, and reporting it
         * into a region we cannot lock would be worse. */
        return true;
    }

    usage = &region_map->devices[card];
    /* Clamped rather than wrapped: a double free must not hand the container an unbounded quota. */
    usage->memory_used_bytes = usage->memory_used_bytes > held ? usage->memory_used_bytes - held : 0;
    slot = charge_slot(usage, (int)getpid());
    if (slot != NULL) {
        slot->memory_bytes = slot->memory_bytes > held ? slot->memory_bytes - held : 0;
        if (slot->memory_bytes == 0) {
            memset(slot, 0, sizeof(*slot));
        }
    }
    ledger_unlock(card);
    return true;
}

VROCM_INTERNAL unsigned long long vrocm_ledger_used(int device)
{
    fork_reset();
    if (region_open() == NULL || !device_valid(device)) {
        return 0;
    }
    return region_map->devices[device].memory_used_bytes;
}

VROCM_INTERNAL unsigned long long vrocm_ledger_quota(int device)
{
    fork_reset();
    if (region_open() == NULL || !device_valid(device)) {
        return 0;
    }
    return region_map->devices[device].memory_quota_bytes;
}

VROCM_INTERNAL unsigned long long vrocm_ledger_lock_epochs(void)
{
    return lock_epochs;
}

VROCM_INTERNAL unsigned long long vrocm_ledger_tracking_refusals(void)
{
    return tracking_refusals;
}
