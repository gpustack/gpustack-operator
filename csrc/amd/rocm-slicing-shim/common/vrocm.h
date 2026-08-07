/*
 * vrocm.h — the vocabulary every part of libvrocm.so shares: the internal-visibility marker
 * and the device bound that keeps a runtime-supplied index from becoming an array offset.
 *
 * NOTHING HERE MAY NAME A `hip*` OR `hsa*` TYPE, and neither may anything else in common/.
 * That rule is what makes this directory compile and run with no ROCm installed and no device
 * present, which is what makes the ledger arithmetic and the quota parsing testable at all —
 * everything else in this tree can only be exercised on an AMD host.
 *
 * NOTHING HERE MAY CALL `pthread_*` OR `sem_*` either. Those symbols carry GLIBC_2.34, and this
 * library is preloaded into workload images whose glibc may be much older; the product's ceiling
 * is GLIBC_2.4 and build.sh asserts it. In-process exclusion is therefore a compiler-atomic
 * spinlock and cross-process exclusion is an fcntl() record lock, both of which predate 2.4.
 */
#ifndef VROCM_COMMON_VROCM_H
#define VROCM_COMMON_VROCM_H

/* Hidden, not exported. This code is linked INTO a preloaded library whose visible surface must
 * be exactly the HIP entry points it interposes: a common/ symbol reaching the global namespace
 * would itself be interposable by the workload, and would collide with any other preloaded
 * library carrying a symbol of the same name. Case 1 asserts the absence rather than trusting
 * the attribute. */
#define VROCM_INTERNAL __attribute__((visibility("hidden")))

/* Bounded by choice: an index arrives from a device handle, so a bogus value must never become
 * an array offset. The largest AMD host seen so far carries 8 cards; 64 leaves room without
 * making the region large.
 *
 * Frozen by the region contract. The lock arena is sized by this constant and its byte offsets
 * are what two processes take their record locks on, so raising it in one build and not another
 * would leave them locking different bytes and excluding nobody. */
#define VROCM_MAX_DEVICES 64

/* How this tree waits on a lock it holds no thread for. Neither a syscall nor a wait: the
 * instruction tells the core that the loop around it is a lock spin, which drains the speculated
 * loads the loop would otherwise pile up, drops the power it burns, and hands the other SMT
 * sibling the issue slots it needs to reach the unlock. Stated once here rather than beside each
 * spin, so the tree cannot end up with one loop that pauses and one that does not. */
#if defined(__x86_64__) || defined(__i386__)
#define VROCM_SPIN_PAUSE() __builtin_ia32_pause()
#else
#define VROCM_SPIN_PAUSE() ((void)0)
#endif

#endif /* VROCM_COMMON_VROCM_H */
