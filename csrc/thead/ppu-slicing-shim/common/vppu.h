/*
 * vppu.h — the vocabulary every part of libvppu.so shares: the log channel, the level that
 * gates it, and the device bound that keeps a vendor-supplied index from becoming an array
 * offset.
 *
 * Nothing here may name an `hg*`, `hggc*` or `hgml*` type. That rule is what makes this
 * directory compile and run without the SDK and without a device, which is what makes the
 * ledger arithmetic and the quota parsing testable at all — everything else in this library
 * can only be exercised on a PPU host inside the vendor's image.
 */
#ifndef VPPU_COMMON_VPPU_H
#define VPPU_COMMON_VPPU_H

#include <stdio.h>

/* Hidden, not exported. This code is linked INTO a preloaded library whose visible surface
 * must be exactly the vendor entry points it interposes: a common/ symbol reaching the
 * global namespace would itself be interposable by the workload, and the two halves of the
 * library would interpose each other's copy once both are loaded into one process. Case 1
 * asserts the absence rather than trusting the attribute. */
#define VPPU_INTERNAL __attribute__((visibility("hidden")))

/* Bounded by choice: an index arrives from a device handle, so a bogus value must never
 * become an array offset. The largest PPU host seen so far carries 16 cards.
 *
 * Frozen by the region contract. The lock arena is sized by this constant and its byte
 * offsets are what two processes take their locks on, so raising it in one build and not
 * another would leave them locking different bytes and excluding nobody. */
#define VPPU_MAX_DEVICES 64

/* The tag is the library name rather than the project name — HAMi-core tags with its
 * project (`[HAMI-core Msg …]`) — because every gate case decides its rows by grepping this
 * out of output interleaved with the vendor's own, where short and unique matters more. */
#define VPPU_TAG "[vppu] "

#define VPPU_ENV_LOG_LEVEL "LIBHGGC_LOG_LEVEL"

/* 1 is the default because a denial is the one line that answers "why was my allocation
 * refused" and it is rare — unlike HAMi-core's level 1, which is per-call chatter and which
 * GPUStack therefore turns off. Level 2 carries the load markers and the counter dump, which
 * the gate cases read, so they pin it. */
#define VPPU_LOG_DENY 1
#define VPPU_LOG_DEBUG 2

VPPU_INTERNAL int vppu_log_level(void);

#define vppu_log(lvl, ...)                          \
    do {                                            \
        if (vppu_log_level() >= (lvl)) {            \
            fprintf(stderr, VPPU_TAG __VA_ARGS__);  \
        }                                           \
    } while (0)

#endif /* VPPU_COMMON_VPPU_H */
