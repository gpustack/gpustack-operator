#define _GNU_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <signal.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include "vppu_ledger.h"

/* ---------------------------------------------------------------------------------------
 * In-process locks
 *
 * An fcntl() record lock is held by the PROCESS, so it does not exclude sibling threads.
 * These flags do. They are GCC atomics on a single byte rather than a pthread mutex for the
 * reason the header gives: -lpthread on the glibc 2.17 floor would show up in DT_NEEDED.
 * ------------------------------------------------------------------------------------- */

static void spin_take(unsigned char *flag)
{
    unsigned spins = 0;

    while (__atomic_test_and_set(flag, __ATOMIC_ACQUIRE)) {
        /* Yield first, sleep once yielding has clearly not helped: the holder may be inside
         * the vendor's allocation, which is long enough that spinning burns a core. */
        if (++spins < 128u) {
            sched_yield();
        } else {
            usleep(200);
        }
    }
}

static void spin_drop(unsigned char *flag)
{
    __atomic_clear(flag, __ATOMIC_RELEASE);
}

/* ---------------------------------------------------------------------------------------
 * Process-local state, and surviving fork()
 *
 * Everything in this block is duplicated by fork() — including a spinlock that another THREAD
 * held at the moment of the fork, which the child would then wait on forever, because the
 * holder does not exist in the child. That is not an exotic shape: a data loader started with
 * the fork method does exactly this on every epoch, from a process whose other threads are
 * allocating.
 *
 * pthread_atfork would be the usual answer and is unavailable here — it would put -lpthread in
 * DT_NEEDED, which the shipped library may not carry. So the state is stamped with the pid that
 * owns it and reset on first use in a new process.
 *
 * That reset is SERIALISED, and it has to be: several threads reaching their first ledger call at
 * the same time all see a pid that is not theirs, and a second thread clearing the spinlocks while
 * the first already holds one corrupts the lock rather than adopting anything. It is not only a
 * fork story — a fresh process whose threads allocate concurrently at startup is the same shape,
 * which is why the pid alone was not enough. Exactly one thread wins the ticket and resets; the
 * others wait for it to say so.
 *
 * The mapping itself is deliberately NOT reset: it is MAP_SHARED on an inherited descriptor, so
 * it is still the same region and still correct. The record locks are not inherited either,
 * which is what makes clearing the in-process flags safe — the child's own next lock attempt
 * blocks on the parent's record lock, exactly as another process would.
 * ------------------------------------------------------------------------------------- */

static unsigned char region_spin;
static unsigned char card_spin[VPPU_MAX_DEVICES];
static unsigned char alloc_spin;

/* Which card this thread holds, and how deep. Thread-local because the record lock is
 * process-wide: unlocking on the vendor's nested call would release a lock the outer
 * allocation still depends on. Per THREAD rather than per process because a sibling thread must
 * not mistake another's lock for its own re-entry and skip taking one.
 *
 * initial-exec, not the default general-dynamic model, and this is not a micro-optimisation:
 * general-dynamic TLS in a shared object resolves through __tls_get_addr, which puts
 * ld-linux-x86-64.so.2 in DT_NEEDED — and DT_NEEDED being nothing but libc.so.6 is a hard
 * constraint this library is asserted against. initial-exec is correct here because this
 * library only ever arrives through LD_PRELOAD or /etc/ld.so.preload, so it is always in the
 * initial exec set; it is never dlopen()ed, which is the one case the model would break. */
static __thread int held_device __attribute__((tls_model("initial-exec"))) = -1;
static __thread unsigned held_depth __attribute__((tls_model("initial-exec")));

/* This process's slot in each card's table, once it has one. Compared by pid rather than
 * trusted outright, so a slot inherited across fork() or left by a process whose pid has
 * since been reused is not mistaken for ours. */
static int my_slot_index[VPPU_MAX_DEVICES];
static int32_t my_slot_pid[VPPU_MAX_DEVICES];

static struct vppu_region *region_map;
static bool region_failed;
static int region_fd = -1;

static void reset_alloc_table(void);

static void adopt_state(void)
{
    /* Two words rather than one: `claimed` is the ticket exactly one thread wins, `adopted` is the
     * announcement that the reset below has finished. With a single word a thread that lost the
     * race would return while the winner was still clearing the spinlocks, and then take a lock
     * about to be zeroed under it. */
    static int claimed;
    static int adopted;
    int self = (int)getpid();

    if (__atomic_load_n(&adopted, __ATOMIC_ACQUIRE) == self) {
        return;
    }

    int seen = __atomic_load_n(&claimed, __ATOMIC_RELAXED);
    if (seen != self
        && __atomic_compare_exchange_n(&claimed, &seen, self, false, __ATOMIC_ACQ_REL,
                                       __ATOMIC_RELAXED)) {
        region_spin = 0;
        alloc_spin = 0;
        memset(card_spin, 0, sizeof(card_spin));
        held_device = -1;
        held_depth = 0;
        memset(my_slot_pid, 0, sizeof(my_slot_pid));

        /* The parent's allocations are not this process's to free: a device pointer inherited
         * across fork() addresses a context the child does not have. Dropping the records keeps a
         * free of one of them from crediting a charge the child never made. */
        reset_alloc_table();

        __atomic_store_n(&adopted, self, __ATOMIC_RELEASE);
        return;
    }

    /* Another thread holds the ticket. What it has left to do is a handful of stores with no
     * syscall and no lock in them, so yielding until it announces is cheaper than any structure
     * that could wait more politely — and this runs once per process, not per allocation. */
    while (__atomic_load_n(&adopted, __ATOMIC_ACQUIRE) != self) {
        sched_yield();
    }
}

static const char *ledger_path(void)
{
    const char *path = getenv(VPPU_ENV_LEDGER_PATH);

    return (path != NULL && *path != '\0') ? path : VPPU_LEDGER_DEFAULT_PATH;
}

/* range_lock — block until this process holds one byte range of the ledger file
 * exclusively. Only ever exclusive: the read side of this library takes no lock. */
static bool range_lock(int fd, off_t offset, off_t len)
{
    struct flock fl;

    memset(&fl, 0, sizeof(fl));
    fl.l_type = F_WRLCK;
    fl.l_whence = SEEK_SET;
    fl.l_start = offset;
    fl.l_len = len;

    while (fcntl(fd, F_SETLKW, &fl) != 0) {
        if (errno != EINTR) {
            return false;
        }
    }
    return true;
}

static void range_unlock(int fd, off_t offset, off_t len)
{
    struct flock fl;

    memset(&fl, 0, sizeof(fl));
    fl.l_type = F_UNLCK;
    fl.l_whence = SEEK_SET;
    fl.l_start = offset;
    fl.l_len = len;
    (void)fcntl(fd, F_SETLK, &fl);
}

static bool all_zero(const void *base, size_t len)
{
    const unsigned char *bytes = base;

    for (size_t i = 0; i < len; i++) {
        if (bytes[i] != 0) {
            return false;
        }
    }
    return true;
}

/* region_open — map the ledger, creating and stamping it if this is the first process.
 *
 * The magic-and-version check is the reason a scraper can be written against this file at
 * all: a region stamped by a layout this build does not know is REFUSED rather than
 * misparsed, and a file that exists but is not ours is never overwritten. */
/* opens_with_magic — whether a file too short to hold the region nonetheless begins with our
 * magic. That is the one short file region_open() may grow, and it is not hypothetical: a process
 * that created the file and died before stamping it leaves exactly that. */
static bool opens_with_magic(int fd)
{
    char magic[VPPU_REGION_MAGIC_BYTES];

    return pread(fd, magic, sizeof(magic), 0) == (ssize_t)sizeof(magic)
           && memcmp(magic, VPPU_REGION_MAGIC, sizeof(magic)) == 0;
}

static struct vppu_region *region_open(void)
{
    const char *path = ledger_path();

    /* Created narrow then widened, so the mode does not depend on the creating process's
     * umask: every process in the container has to be able to map it, and they need not all
     * run as the same user. */
    int fd = open(path, O_RDWR | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
    if (fd >= 0) {
        (void)fchmod(fd, 0666);
    } else if (errno == EEXIST) {
        fd = open(path, O_RDWR | O_CLOEXEC);
    }
    if (fd < 0) {
        vppu_log(VPPU_LOG_DENY, "cannot open the ledger %s: %s\n", path, strerror(errno));
        return NULL;
    }

    /* The magic's own bytes are the creation lock. Offset 0 is frozen by the layout, so two
     * processes racing to create the region agree on where to serialise even if they were
     * built against different versions of it. */
    if (!range_lock(fd, 0, VPPU_REGION_MAGIC_BYTES)) {
        vppu_log(VPPU_LOG_DENY, "cannot lock the ledger %s: %s\n", path, strerror(errno));
        close(fd);
        return NULL;
    }

    struct vppu_region *region = NULL;
    struct stat st;
    if (fstat(fd, &st) != 0) {
        vppu_log(VPPU_LOG_DENY, "cannot size the ledger %s: %s\n", path, strerror(errno));
        goto done;
    }
    /* A file that already holds something is JUDGED BEFORE IT IS RESIZED. Growing it first and
     * refusing it afterwards still modified it, and HGGC_LEDGER_PATH is a configured path — the
     * one that gets typed wrong names something else that mattered. The only short file this may
     * grow is one that is empty (this process just created it) or one that already opens with our
     * magic (a region whose creator died between the ftruncate and the stamp). */
    if ((size_t)st.st_size < sizeof(*region)) {
        if (st.st_size > 0 && !opens_with_magic(fd)) {
            vppu_log(VPPU_LOG_DENY, "%s is not a vppu ledger — refusing to resize it\n", path);
            goto done;
        }
        if (ftruncate(fd, (off_t)sizeof(*region)) != 0) {
            vppu_log(VPPU_LOG_DENY, "cannot grow the ledger %s: %s\n", path, strerror(errno));
            goto done;
        }
    }

    void *mapped = mmap(NULL, sizeof(*region), PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (mapped == MAP_FAILED) {
        vppu_log(VPPU_LOG_DENY, "cannot map the ledger %s: %s\n", path, strerror(errno));
        goto done;
    }
    region = mapped;

    if (memcmp(region->magic, VPPU_REGION_MAGIC, sizeof(region->magic)) == 0) {
        if (region->layout_version != VPPU_REGION_VERSION) {
            vppu_log(VPPU_LOG_DENY,
                     "the ledger %s is layout version %u and this build speaks %u — "
                     "refusing it rather than misparsing it\n",
                     path, region->layout_version, VPPU_REGION_VERSION);
            munmap(region, sizeof(*region));
            region = NULL;
        }
        goto done;
    }

    if (!all_zero(region, sizeof(*region))) {
        vppu_log(VPPU_LOG_DENY, "%s is not a vppu ledger — refusing to overwrite it\n", path);
        munmap(region, sizeof(*region));
        region = NULL;
        goto done;
    }

    region->layout_version = VPPU_REGION_VERSION;
    region->header_bytes = (uint32_t)offsetof(struct vppu_region, devices);
    region->device_slots = VPPU_MAX_DEVICES;
    region->process_slots = VPPU_MAX_PROCESSES_PER_DEVICE;
    /* The magic goes last, after a barrier: it is what marks the region valid to a reader
     * that follows the documented layout without taking this lock. */
    __atomic_thread_fence(__ATOMIC_RELEASE);
    memcpy(region->magic, VPPU_REGION_MAGIC, sizeof(region->magic));

done:
    range_unlock(fd, 0, VPPU_REGION_MAGIC_BYTES);
    if (region == NULL) {
        close(fd);
        return NULL;
    }
    region_fd = fd;
    return region;
}

/* region — the mapped ledger, or NULL once mapping it has failed.
 *
 * Mapped lazily rather than from a constructor: this library is preloaded into every process
 * in the container, and one that never allocates should never create the file. The failure is
 * latched so a broken ledger costs one open() per process rather than one per allocation. */
static struct vppu_region *region(void)
{
    adopt_state();
    spin_take(&region_spin);
    if (region_map == NULL && !region_failed) {
        region_map = region_open();
        region_failed = (region_map == NULL);
    }
    struct vppu_region *mapped = region_map;
    spin_drop(&region_spin);
    return mapped;
}

/* ---------------------------------------------------------------------------------------
 * The per-card lock
 * ------------------------------------------------------------------------------------- */

static off_t lock_offset(int device)
{
    return (off_t)offsetof(struct vppu_region, lock_arena) + device;
}

bool vppu_ledger_lock(int device)
{
    adopt_state();
    if (device < 0 || device >= VPPU_MAX_DEVICES) {
        return false;
    }
    if (held_device == device) {
        held_depth++;
        return true;
    }
    /* Holding one card and asking for another would need a lock order this library has no
     * reason to define — no interposed entry touches two cards. Refusing is fail-closed. */
    if (held_device >= 0) {
        vppu_log(VPPU_LOG_DEBUG, "declining the ledger for device %d while holding device %d\n",
                 device, held_device);
        return false;
    }
    if (region() == NULL) {
        return false;
    }

    spin_take(&card_spin[device]);
    if (!range_lock(region_fd, lock_offset(device), 1)) {
        vppu_log(VPPU_LOG_DENY, "cannot lock device %d in the ledger: %s\n", device,
                 strerror(errno));
        spin_drop(&card_spin[device]);
        return false;
    }

    held_device = device;
    held_depth = 1;
    region_map->devices[device].lock_holder_pid = (int32_t)getpid();
    return true;
}

bool vppu_ledger_holding(void)
{
    /* Adopted first: fork() copies this thread's "I hold card N" flag into a child where the
     * lock does not exist, and a caller asking this question before its first lock would
     * otherwise be told it holds one. */
    adopt_state();
    return held_device >= 0;
}

void vppu_ledger_unlock(int device)
{
    if (held_device != device || held_depth == 0) {
        return;
    }
    if (--held_depth > 0) {
        return;
    }

    region_map->devices[device].lock_holder_pid = 0;
    range_unlock(region_fd, lock_offset(device), 1);
    held_device = -1;
    spin_drop(&card_spin[device]);
}

/* ---------------------------------------------------------------------------------------
 * Charges
 * ------------------------------------------------------------------------------------- */

static bool pid_alive(int32_t pid)
{
    if (pid == (int32_t)getpid()) {
        return true;
    }
    if (kill((pid_t)pid, 0) == 0) {
        return true;
    }
    /* EPERM means it exists and belongs to someone else; only ESRCH means it is gone. */
    return errno != ESRCH;
}

/* forget_slot — release a slot and take its bytes out of the card's total. Used when the slot
 * belonged to a process that no longer exists, so the bytes are not owed to anyone. */
static void forget_slot(struct vppu_device_usage *device, struct vppu_process_charge *slot)
{
    device->memory_used_bytes = (slot->memory_bytes > device->memory_used_bytes)
                                    ? 0ULL
                                    : device->memory_used_bytes - slot->memory_bytes;
    slot->pid = 0;
    slot->memory_bytes = 0ULL;
}

/* my_slot — this process's charge slot for one card. Must be called with the card's lock
 * held; returns NULL when the table is full and `create` was asked for. */
static struct vppu_process_charge *my_slot(int device, bool create)
{
    struct vppu_device_usage *usage = &region_map->devices[device];
    int32_t self = (int32_t)getpid();

    if (my_slot_pid[device] == self) {
        return &usage->processes[my_slot_index[device]];
    }

    int reusable = -1;
    for (int i = 0; i < VPPU_MAX_PROCESSES_PER_DEVICE; i++) {
        struct vppu_process_charge *slot = &usage->processes[i];

        if (slot->pid == self) {
            /* Ours by pid, but this process never recorded it: the pid was reused, so the
             * charge behind it belongs to a process that is gone. */
            forget_slot(usage, slot);
            slot->pid = self;
            my_slot_pid[device] = self;
            my_slot_index[device] = i;
            return slot;
        }
        if (reusable < 0 && (slot->pid == 0 || !pid_alive(slot->pid))) {
            reusable = i;
        }
    }

    if (!create || reusable < 0) {
        return NULL;
    }

    struct vppu_process_charge *slot = &usage->processes[reusable];
    forget_slot(usage, slot);
    slot->pid = self;
    my_slot_pid[device] = self;
    my_slot_index[device] = reusable;
    return slot;
}

unsigned long long vppu_ledger_used(int device)
{
    if (device < 0 || device >= VPPU_MAX_DEVICES || region() == NULL) {
        return 0ULL;
    }
    return region_map->devices[device].memory_used_bytes;
}

void vppu_ledger_note_config(int device, unsigned long long quota_bytes,
                            unsigned int sm_percent)
{
    if (region_map == NULL || device < 0 || device >= VPPU_MAX_DEVICES) {
        return;
    }
    region_map->devices[device].memory_quota_bytes = quota_bytes;
    region_map->devices[device].sm_limit_percent = sm_percent;
}

void vppu_ledger_note_util(int device, unsigned int sm_percent)
{
    if (device < 0 || device >= VPPU_MAX_DEVICES || region() == NULL) {
        return;
    }
    region_map->devices[device].sm_util_percent = sm_percent;
}

struct vppu_pid_state *vppu_ledger_control(int device)
{
    if (device < 0 || device >= VPPU_MAX_DEVICES || region() == NULL) {
        return NULL;
    }
    return &region_map->devices[device].control;
}

bool vppu_ledger_has_process(int device, int pid)
{
    if (device < 0 || device >= VPPU_MAX_DEVICES || pid <= 0 || region() == NULL) {
        return false;
    }

    const struct vppu_device_usage *usage = &region_map->devices[device];
    for (int i = 0; i < VPPU_MAX_PROCESSES_PER_DEVICE; i++) {
        if (usage->processes[i].pid == (int32_t)pid) {
            return true;
        }
    }
    return false;
}

bool vppu_ledger_charge(int device, unsigned long long bytes)
{
    if (region_map == NULL || device < 0 || device >= VPPU_MAX_DEVICES) {
        return false;
    }

    struct vppu_process_charge *slot = my_slot(device, true);
    if (slot == NULL) {
        vppu_log(VPPU_LOG_DENY,
                 "device %d has no free ledger slot — %d processes already hold a charge\n",
                 device, VPPU_MAX_PROCESSES_PER_DEVICE);
        return false;
    }

    slot->memory_bytes += bytes;
    region_map->devices[device].memory_used_bytes += bytes;
    return true;
}

void vppu_ledger_refund(int device, unsigned long long bytes)
{
    if (region_map == NULL || device < 0 || device >= VPPU_MAX_DEVICES) {
        return;
    }

    struct vppu_device_usage *usage = &region_map->devices[device];
    struct vppu_process_charge *slot = my_slot(device, false);

    /* Clamped on both figures rather than wrapped: a double free must not hand the container
     * an unbounded quota, which is what a wrapped subtraction does. */
    if (slot != NULL) {
        slot->memory_bytes = (bytes > slot->memory_bytes) ? 0ULL : slot->memory_bytes - bytes;
    }
    usage->memory_used_bytes =
        (bytes > usage->memory_used_bytes) ? 0ULL : usage->memory_used_bytes - bytes;
}

unsigned long long vppu_ledger_reclaim(int device)
{
    if (region_map == NULL || device < 0 || device >= VPPU_MAX_DEVICES) {
        return 0ULL;
    }

    struct vppu_device_usage *usage = &region_map->devices[device];
    unsigned long long live = 0ULL;
    for (int i = 0; i < VPPU_MAX_PROCESSES_PER_DEVICE; i++) {
        struct vppu_process_charge *slot = &usage->processes[i];

        if (slot->pid == 0) {
            continue;
        }
        if (!pid_alive(slot->pid)) {
            slot->pid = 0;
            slot->memory_bytes = 0ULL;
            continue;
        }
        live += slot->memory_bytes;
    }

    /* The total is re-derived rather than decremented, so any drift between it and the
     * per-process breakdown is corrected here too. */
    unsigned long long before = usage->memory_used_bytes;
    usage->memory_used_bytes = live;
    return (before > live) ? before - live : 0ULL;
}

/* ---------------------------------------------------------------------------------------
 * The process-local key -> (card, bytes) map
 * ------------------------------------------------------------------------------------- */

#define VPPU_ALLOC_SLOTS 1024u

/* Two reserved key values. EMPTY ends a probe chain; DEAD marks a slot whose entry was taken.
 * A freed slot MUST NOT go back to EMPTY: with open addressing that truncates the chain of
 * every key which probed past it, so the next free of a colliding key finds nothing and its
 * bytes stay charged for the life of the process. */
#define VPPU_ALLOC_EMPTY 0ULL
#define VPPU_ALLOC_DEAD 0xFFFFFFFFFFFFFFFFULL

struct vppu_alloc_slot {
    unsigned long long key;
    unsigned long long bytes;
    int device;
};

static struct vppu_alloc_slot alloc_table[VPPU_ALLOC_SLOTS];
static unsigned long long alloc_overflows;

static void reset_alloc_table(void)
{
    memset(alloc_table, 0, sizeof(alloc_table));
    alloc_overflows = 0ULL;
}

/* alloc_home — device pointers and VMM handles are page-aligned, so key % SLOTS is 0 for
 * essentially every allocation and the table would degenerate into a single chain out of slot
 * 0. Mix the key so the slots are actually used. */
static unsigned int alloc_home(unsigned long long key)
{
    unsigned long long mixed = key * 0x9E3779B97F4A7C15ULL;

    return (unsigned int)((mixed >> 32) % VPPU_ALLOC_SLOTS);
}

static bool alloc_reserved(unsigned long long key)
{
    return key == VPPU_ALLOC_EMPTY || key == VPPU_ALLOC_DEAD;
}

void vppu_alloc_record(unsigned long long key, int device, unsigned long long bytes)
{
    adopt_state();
    if (alloc_reserved(key)) {
        return;
    }

    spin_take(&alloc_spin);
    unsigned int home = alloc_home(key);
    for (unsigned int probe = 0; probe < VPPU_ALLOC_SLOTS; probe++) {
        struct vppu_alloc_slot *slot = &alloc_table[(home + probe) % VPPU_ALLOC_SLOTS];

        if (slot->key == VPPU_ALLOC_EMPTY || slot->key == VPPU_ALLOC_DEAD) {
            slot->key = key;
            slot->bytes = bytes;
            slot->device = device;
            spin_drop(&alloc_spin);
            return;
        }
    }

    /* Full table: the charge stands and the record is dropped, because a denial caused by
     * this library's own bookkeeping would be reported as a quota finding. Counted so the
     * counter dump can say it happened. */
    alloc_overflows++;
    spin_drop(&alloc_spin);
}

bool vppu_alloc_take(unsigned long long key, int *device, unsigned long long *bytes)
{
    adopt_state();
    if (alloc_reserved(key)) {
        return false;
    }

    spin_take(&alloc_spin);
    unsigned int home = alloc_home(key);
    for (unsigned int probe = 0; probe < VPPU_ALLOC_SLOTS; probe++) {
        struct vppu_alloc_slot *slot = &alloc_table[(home + probe) % VPPU_ALLOC_SLOTS];

        if (slot->key == key) {
            *device = slot->device;
            *bytes = slot->bytes;
            slot->key = VPPU_ALLOC_DEAD;
            slot->bytes = 0ULL;
            slot->device = -1;
            spin_drop(&alloc_spin);
            return true;
        }
        if (slot->key == VPPU_ALLOC_EMPTY) {
            break;
        }
    }
    spin_drop(&alloc_spin);
    return false;
}

unsigned long long vppu_alloc_overflows(void)
{
    return alloc_overflows;
}
