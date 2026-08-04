/*
 * ppu_monitor.c — print one container's slice: both quotas, both usages, per card.
 *
 * WHY A READER EXISTS AT ALL. A slice's memory quota can at least be inferred from `ppu-smi`,
 * because the visibility shim rewrites what that reports. Its COMPUTE cap cannot: `ppu-smi` has
 * no maximum-SM column, exactly as `nvidia-smi` has none, so without this tool the only evidence
 * of a compute limit is a line in the shim's init log or a stress test that runs into it. The
 * Ascend backend reached the same conclusion and answers it the same way — `enpu-monitor`, built
 * beside the preload and mounted into the container.
 *
 * WHY IT READS THE REGION AND NOT THE SHIM. Every symbol in the shim is hidden on purpose: a
 * preloaded library that exported its own seam would be interposable by the workload it polices.
 * So there is nothing to call, and that is the right shape anyway — the same file this reads is
 * what a metrics scraper will read, and a scraper cannot be asked to preload a slicing library
 * into itself. The layout is a contract for exactly that reason; it is written down in
 * `.claude/skills/gpustack-operator-xbuild-and-verify/references/thead-usage-region.md`.
 *
 * WHY IT DOES NOT LINK common/vppu_ledger.c. That file's read helper maps the region LAZILY, which
 * means it CREATES one when none exists, and its other entries take the card's fcntl lock. A
 * reader must do neither: creating a region would conjure a slice into existence for anything that
 * merely looked at the container, and taking the lock would let a monitor block behind a vendor
 * allocation that hung — the one thing the region's read side is designed never to do. So this
 * takes the struct definition and its `_Static_assert`s from the header, and does its own
 * read-only mmap. Being a second parser of the same layout is the point of having a contract, and
 * the assertions in that header are what fail the build if a field moves under it.
 *
 * IT NEEDS NEITHER THE SDK NOR A DEVICE, and the recipe is where that is enforced: `build.sh`
 * compiles this with no SDK include path and links no vendor library, so it runs in a container
 * that has neither. Case 1 checks the resulting DT_NEEDED rather than trusting the recipe.
 *
 * WHAT IT DELIBERATELY DOES NOT PRINT is the throttle as a fraction. The controller's allowance is
 * in the region but the window it is a fraction OF is not: the period is the container's own
 * configuration (`HGGC_SM_CONTROL_PERIOD_MS`), and a reader that cannot see that environment would
 * print a confident wrong percentage. So `allow_us` is reported raw, to be read against the period
 * whoever configured it knows.
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

#include "common/vppu_ledger.h"

#define MIB (1024ULL * 1024ULL)

/* Resolved exactly as the writer resolves it, empty string included: a reader that disagreed
 * with the shim about which file to open would report another container's slice, or none. */
static const char *region_path(void)
{
    const char *path = getenv(VPPU_ENV_LEDGER_PATH);

    return (path != NULL && *path != '\0') ? path : VPPU_LEDGER_DEFAULT_PATH;
}

/* A card is the container's if the region has ever recorded anything about it. Quota alone is
 * not the test: a card whose quota was cleared while a charge stands is exactly the state worth
 * seeing. */
static bool card_is_held(const struct vppu_device_usage *card)
{
    return card->memory_quota_bytes != 0ULL || card->memory_used_bytes != 0ULL;
}

/* One line per card, one indented line per charged process, key=value throughout — the same
 * output serves a human and the scraper that replaces `od` recipes. The unit is in the KEY so a
 * value never has to be stripped, and the exact byte figures ride along on the same line because
 * a sub-MiB charge is real and rounds to zero MiB. */
static void print_card(unsigned int index, const struct vppu_device_usage *card,
                       unsigned int process_slots)
{
    unsigned long long quota = (unsigned long long)card->memory_quota_bytes;
    unsigned long long used = (unsigned long long)card->memory_used_bytes;
    unsigned long long unused = (used < quota) ? quota - used : 0ULL;

    printf("card=%u mem_quota_mib=%llu mem_used_mib=%llu mem_free_mib=%llu sm_limit_pct=%u "
           "sm_util_pct=%u allow_us=%llu lock_pid=%d mem_quota_bytes=%llu mem_used_bytes=%llu\n",
           index, quota / MIB, used / MIB, unused / MIB, card->sm_limit_percent,
           card->sm_util_percent, (unsigned long long)(card->control.allow_ns / 1000ULL),
           card->lock_holder_pid, quota, used);

    for (unsigned int slot = 0; slot < process_slots; slot++) {
        const struct vppu_process_charge *charge = &card->processes[slot];

        if (charge->pid == 0) {
            continue;
        }
        printf("  proc pid=%d mem_mib=%llu mem_bytes=%llu\n", charge->pid,
               (unsigned long long)charge->memory_bytes / MIB,
               (unsigned long long)charge->memory_bytes);
    }
}

int main(void)
{
    const char *path = region_path();
    int fd = open(path, O_RDONLY | O_CLOEXEC);

    if (fd < 0) {
        fprintf(stderr, "ppu-monitor: %s: %s\n", path, strerror(errno));
        fprintf(stderr, "ppu-monitor: no usage region — nothing in this container has been sliced,"
                        " or %s names another path\n", VPPU_ENV_LEDGER_PATH);
        return 1;
    }

    /* The header is read before the file is sized or mapped, so an unknown VERSION is reported
     * as one instead of as a size mismatch: a future layout is allowed to be a different size,
     * and telling its reader "too small" would send them looking in the wrong place. */
    unsigned char head[VPPU_REGION_MAGIC_BYTES + sizeof(uint32_t)];
    ssize_t got = pread(fd, head, sizeof(head), 0);
    if (got < 0) {
        /* A read that FAILED is exit 1, not 2: the contract above files "the path is unreadable"
         * with "there is no region", and reporting an I/O error as "too small" would send a
         * scraper looking for a truncated file that is not the problem. */
        fprintf(stderr, "ppu-monitor: %s: cannot read the region header: %s\n", path,
                strerror(errno));
        close(fd);
        return 1;
    }
    if (got != (ssize_t)sizeof(head)) {
        fprintf(stderr, "ppu-monitor: %s: too small to carry a region header\n", path);
        close(fd);
        return 2;
    }
    if (memcmp(head, VPPU_REGION_MAGIC, VPPU_REGION_MAGIC_BYTES) != 0) {
        fprintf(stderr, "ppu-monitor: %s: not a usage region, no %s magic at offset 0\n", path,
                VPPU_REGION_MAGIC);
        close(fd);
        return 2;
    }

    uint32_t version = 0;
    memcpy(&version, head + VPPU_REGION_MAGIC_BYTES, sizeof(version));
    if (version != VPPU_REGION_VERSION) {
        fprintf(stderr,
                "ppu-monitor: %s: layout version %u, this reader knows %u — refusing rather than"
                " guessing at the offsets\n",
                path, version, (unsigned int)VPPU_REGION_VERSION);
        close(fd);
        return 2;
    }

    /* Sized before mapping, because a mapping that runs past the end of the file faults on
     * access rather than failing here. */
    struct stat st;
    if (fstat(fd, &st) != 0 || (size_t)st.st_size < sizeof(struct vppu_region)) {
        fprintf(stderr, "ppu-monitor: %s: %llu bytes, a version %u region is %zu\n", path,
                (unsigned long long)st.st_size, version, sizeof(struct vppu_region));
        close(fd);
        return 2;
    }

    void *mapped = mmap(NULL, sizeof(struct vppu_region), PROT_READ, MAP_SHARED, fd, 0);
    close(fd);
    if (mapped == MAP_FAILED) {
        fprintf(stderr, "ppu-monitor: %s: cannot map: %s\n", path, strerror(errno));
        return 1;
    }
    const struct vppu_region *region = mapped;

    /* The counts are checked rather than trusted, and a mismatch is a refusal: they size the
     * offsets every field below is read at, and the lock arena's bytes are taken by card index,
     * so a writer that disagrees about them is not locking what this reader thinks it is. */
    if (region->header_bytes != (uint32_t)offsetof(struct vppu_region, devices)
        || region->device_slots != VPPU_MAX_DEVICES
        || region->process_slots != VPPU_MAX_PROCESSES_PER_DEVICE) {
        fprintf(stderr,
                "ppu-monitor: %s: header %u bytes, %u cards, %u processes per card — this reader"
                " was built for %zu/%u/%u, refusing\n",
                path, region->header_bytes, region->device_slots, region->process_slots,
                offsetof(struct vppu_region, devices), (unsigned int)VPPU_MAX_DEVICES,
                (unsigned int)VPPU_MAX_PROCESSES_PER_DEVICE);
        return 2;
    }

    printf("region path=%s version=%u cards=%u procs=%u\n", path, version, region->device_slots,
           region->process_slots);

    unsigned int printed = 0;
    for (unsigned int index = 0; index < region->device_slots; index++) {
        if (!card_is_held(&region->devices[index])) {
            continue;
        }
        print_card(index, &region->devices[index], region->process_slots);
        printed++;
    }
    if (printed == 0) {
        printf("# no card charged yet: a card appears here the first time an allocation on it is"
               " admitted\n");
    }
    return 0;
}
