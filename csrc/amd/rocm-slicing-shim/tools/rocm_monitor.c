/*
 * rocm_monitor.c — print one container's slice: the VRAM quota in force and what is charged
 * against it, per card.
 *
 * WHY A READER EXISTS AT ALL. Nothing on an AMD host reports a slice. `rocm-smi` and `amd-smi`
 * read sysfs and the DRM nodes rather than HIP, so they are not reached by the preload at all —
 * measured, they report the physical card's full 15.984 GiB inside a container capped at 4 GiB.
 * Without this tool the only evidence of a memory cap is a line in the shim's init log or a
 * workload running into it. The THead and Ascend backends reached the same conclusion and answer
 * it the same way, with `ppu-monitor` and `enpu-monitor` built beside their preloads.
 *
 * WHY IT READS THE REGION AND NOT THE SHIM. Every symbol in the shim is hidden on purpose: a
 * preloaded library that exported its own seam would be interposable by the workload it polices.
 * So there is nothing to call, and that is the right shape anyway — the same file this reads is
 * what a metrics scraper will read, and a scraper cannot be asked to preload a slicing library
 * into itself. The layout is a contract for exactly that reason.
 *
 * WHY IT DOES NOT LINK common/vrocm_ledger.c. That file maps the region LAZILY, which means it
 * CREATES one when none exists, and its other entries take the card's fcntl lock. A reader must do
 * neither: creating a region would conjure a slice into existence for anything that merely looked
 * at the container, and taking the lock would let a monitor block behind a driver allocation that
 * hung — the one thing the region's read side is designed never to do. So this takes the struct
 * definitions and their `_Static_assert`s from the header and does its own read-only mmap. Being a
 * second parser of the same layout is the point of having a contract, and those assertions are
 * what fail the build if a field moves under it.
 *
 * IT NEEDS NEITHER ROCm NOR A DEVICE, and the recipe is where that is enforced: `build.sh`
 * compiles this with no ROCm include path and links no vendor library, so it runs in a container
 * that has neither.
 *
 * WHAT IT DELIBERATELY DOES NOT PRINT IS THE COMPUTE CAP, because it is not in the region and
 * could not honestly be put there. Compute is enforced by the platform through a CU mask that this
 * library never sees and does not own; what the container was given lives in its own `HSA_CU_MASK`
 * and whether the hardware honoured it is a question only `rocm-cumask-check` can answer, by
 * running a kernel and reading back the CUs it landed on. A figure printed here would be this
 * library repeating a number it has no way to verify.
 *
 * WHAT IT CANNOT SHOW is a card the container holds and has never allocated on. The region records
 * a card the first time an admission touches it, so an untouched card is indistinguishable here
 * from one the container does not hold.
 *
 * EXIT CODES, because a scraper has to tell these apart: 0 the region was parsed; 1 there is no
 * region to read (nothing in this container has been sliced yet, or the path is unreadable); 2 the
 * file exists and this reader may not parse it — a foreign magic, a layout version it does not
 * know, or slot counts it was not built for. Refusing is the contract; a reader that guessed at an
 * unknown version would report figures out of the wrong offsets.
 */
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "common/vrocm_ledger.h"
#include "common/vrocm_quota.h"

#define MIB (1024ULL * 1024ULL)

#define TOOL "rocm-monitor"

static void print_card(unsigned index, const struct vrocm_device_usage *usage, unsigned procs)
{
    unsigned long long quota = usage->memory_quota_bytes;
    unsigned long long used = usage->memory_used_bytes;
    unsigned long long freeb = quota > used ? quota - used : 0;
    unsigned slot;

    printf("card=%u mem_quota_mib=%llu mem_used_mib=%llu mem_free_mib=%llu lock_holder_pid=%d\n",
           index, quota / MIB, used / MIB, freeb / MIB, (int)usage->lock_holder_pid);

    for (slot = 0; slot < procs; slot++) {
        const struct vrocm_process_charge *charge = &usage->processes[slot];

        if (charge->pid == 0) {
            continue;
        }
        printf("  proc pid=%d mem_mib=%llu mem_bytes=%llu\n", (int)charge->pid,
               (unsigned long long)charge->memory_bytes / MIB,
               (unsigned long long)charge->memory_bytes);
    }
}

int main(int argc, char **argv)
{
    const char *path;
    /* The WHOLE region, not just its head: every check below runs against a copy read in one
     * go, so the size test that guards it is the size test for every byte later mapped. Named
     * for what it is because a reader who took it for a header read the guard as a header-sized
     * one and reported a truncated file as a SIGBUS waiting to happen.
     */
    struct vrocm_region snapshot;
    struct vrocm_region *region;
    struct stat st;
    unsigned charged = 0;
    unsigned index;
    int fd;

    /* argv[1] first, so an operator can point this at another container's region from the host;
     * otherwise the region the shim in this container would write: the variable, and the same
     * default when it is unset or empty.
     *
     * Resolved here rather than by calling the shim's own resolver, because this tool links no
     * object from common/ -- see the note at the top of this file. The two must agree, so the
     * NAME and the DEFAULT both come from the shared header and neither is spelled twice. */
    path = (argc > 1) ? argv[1] : getenv(VROCM_ENV_LEDGER_PATH);
    if (path == NULL || *path == '\0') {
        path = VROCM_LEDGER_DEFAULT_PATH;
    }

    /* O_RDONLY and nothing else. Opening with O_CREAT here is the whole failure this tool is
     * shaped to avoid: it would leave an empty region behind for every container somebody merely
     * looked at, and the next reader could not tell that from a slice that had allocated nothing. */
    fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        fprintf(stderr, TOOL ": %s: %s\n", path, strerror(errno));
        fprintf(stderr, TOOL ": no usage region — nothing in this container has been sliced, or the"
                        " path is wrong. Nothing was created.\n");
        return 1;
    }

    if (fstat(fd, &st) != 0) {
        fprintf(stderr, TOOL ": %s: %s\n", path, strerror(errno));
        close(fd);
        return 1;
    }
    if ((size_t)st.st_size < sizeof(snapshot)) {
        fprintf(stderr, TOOL ": %s: %llu bytes, too small to be a region — one is %llu bytes and"
                        " every one of them is read before any is mapped\n",
                path, (unsigned long long)st.st_size, (unsigned long long)sizeof(snapshot));
        close(fd);
        return 2;
    }
    /* LOOPED, because one read() is not one answer. It is allowed to hand back fewer bytes than
     * it was asked for, and what it returns does not say whether the region was short or the call
     * was -- the fstat above has already settled the first, so reading on is what tells them
     * apart. A single call that took the difference for a malformed region would report a healthy
     * ledger as unparseable to whatever scrapes this. */
    {
        size_t got = 0;

        while (got < sizeof(snapshot)) {
            ssize_t n = read(fd, (char *)&snapshot + got, sizeof(snapshot) - got);

            if (n > 0) {
                got += (size_t)n;
                continue;
            }
            if (n < 0 && errno == EINTR) {
                continue;
            }
            fprintf(stderr, TOOL ": %s: cannot read the region: %s\n", path,
                    n < 0 ? strerror(errno) : "it ended before the region did");
            close(fd);
            return 2;
        }
    }

    if (memcmp(snapshot.magic, VROCM_REGION_MAGIC, VROCM_REGION_MAGIC_BYTES) != 0) {
        fprintf(stderr, TOOL ": %s: not a usage region, no %s magic at offset 0\n", path,
                VROCM_REGION_MAGIC);
        close(fd);
        return 2;
    }
    if (snapshot.layout_version != VROCM_REGION_VERSION) {
        fprintf(stderr,
                TOOL ": %s: layout version %u, this reader speaks %u. Refusing rather than reading"
                     " figures out of offsets that may have moved.\n",
                path, (unsigned)snapshot.layout_version, VROCM_REGION_VERSION);
        close(fd);
        return 2;
    }
    /* The version alone is not enough: two builds can agree on the layout and disagree on how many
     * slots it holds, and this reader indexes a fixed-size table. */
    if (snapshot.device_slots != VROCM_MAX_DEVICES ||
        snapshot.process_slots != VROCM_MAX_PROCESSES_PER_DEVICE) {
        fprintf(stderr, TOOL ": %s: %u cards x %u processes, this reader was built for %u x %u\n",
                path, (unsigned)snapshot.device_slots, (unsigned)snapshot.process_slots,
                VROCM_MAX_DEVICES, VROCM_MAX_PROCESSES_PER_DEVICE);
        close(fd);
        return 2;
    }

    region = mmap(NULL, sizeof(*region), PROT_READ, MAP_SHARED, fd, 0);
    close(fd);
    if (region == MAP_FAILED) {
        fprintf(stderr, TOOL ": %s: cannot map: %s\n", path, strerror(errno));
        return 2;
    }

    printf("region path=%s version=%u cards=%u procs=%u\n", path, VROCM_REGION_VERSION,
           (unsigned)snapshot.device_slots, (unsigned)snapshot.process_slots);

    /* Read without taking the card's lock, deliberately — see the header. A figure one allocation
     * stale is worth far more than a reader that can wedge behind a hung allocation. */
    for (index = 0; index < VROCM_MAX_DEVICES; index++) {
        const struct vrocm_device_usage *usage = &region->devices[index];

        if (usage->memory_quota_bytes == 0 && usage->memory_used_bytes == 0) {
            continue;
        }
        print_card(index, usage, VROCM_MAX_PROCESSES_PER_DEVICE);
        charged++;
    }
    if (charged == 0) {
        printf("# no card charged yet: a card appears here the first time an allocation on it is"
               " admitted\n");
    }

    munmap(region, sizeof(*region));
    return 0;
}
