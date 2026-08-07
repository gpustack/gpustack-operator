/*
 * vrocm_hwid.h — what a wave can read about where it is physically running.
 *
 * WHY THIS IS ITS OWN MODULE. Two artifacts need this decode and neither may own it: the mask probe
 * under tools/ and the soak under testing/ both report occupancy, and they must report the SAME
 * occupancy or the pair is worthless — a soak that disagreed with the probe about which units it
 * ran on would look like a mask failure. Held in one header, a correction to a bit position reaches
 * both or neither.
 *
 * WHY NOT common/. That directory's rule is that it carries no GPU vocabulary at all, which is what
 * lets its unit tests build and run with no ROCm installed and no device present. This file is
 * device code by definition. It belongs beside neither the ledger nor the interposer, so it gets
 * its own directory — which also states the boundary plainly: `libvrocm.so` includes nothing from
 * here, and must not, because the product carries no kernel.
 *
 * THE REGISTER AND THE UNIT BOTH DIFFER BY ARCHITECTURE, and that is the whole content of this
 * file. On gfx9 a wave reports `HW_ID`, plus `XCC_ID` on the parts that have more than one XCC, and
 * the unit that means anything is the CU. On gfx10 and newer the register is `HW_ID1` and the unit
 * is the WGP — measured, `SIMD_ID` there only ever reports 0 or 1, so the two CUs of a WGP are not
 * distinguishable from inside a wave. That is not a gap: the WGP is exactly RDNA's allocation atom,
 * so each architecture is read in the unit it allocates in.
 *
 * The field positions were read off hardware rather than taken from a document, and the readings
 * that pin them are in `references/amd-cumask-conformance.md`: on RDNA the WGP count tracked the
 * mask exactly (unmasked 30, `0:0-29` 15, `0:0-13` 7, `0:0-1` 1), and on CDNA the per-XCC CU counts
 * matched every row of the conformance table.
 */
#ifndef VROCM_HWID_H
#define VROCM_HWID_H

/* The slot table is the full cross product of the four coordinates, so a decode never has to know
 * which part it is running on: XCC_ID is four bits, SE_ID three, SA/SH one, and the CU or WGP index
 * four. 4096 unsigned ints is 16 KiB of device memory, which is nothing against the point of not
 * having a per-architecture table size. */
#define VROCM_HWID_MAX_XCC 16u
#define VROCM_HWID_MAX_SE 8u
#define VROCM_HWID_MAX_SH 2u
#define VROCM_HWID_MAX_UNIT 16u
#define VROCM_HWID_SLOTS \
    (VROCM_HWID_MAX_XCC * VROCM_HWID_MAX_SE * VROCM_HWID_MAX_SH * VROCM_HWID_MAX_UNIT)

/* vrocm_hwid_slot_xcc — which XCC a slot index belongs to, for the host side's per-XCC tally. */
#define VROCM_HWID_SLOT_XCC(slot) \
    ((slot) / (VROCM_HWID_MAX_SE * VROCM_HWID_MAX_SH * VROCM_HWID_MAX_UNIT))

/* s_getreg packs the register id, the bit offset and the width into one immediate, so the whole
 * descriptor has to be a constant expression rather than a runtime value. */
#define VROCM_GETREG(id) __builtin_amdgcn_s_getreg(((id) & 0x3f) | (0 << 6) | ((32 - 1) << 11))

#define VROCM_HWREG_HW_ID 4   /* gfx9:   CU_ID[11:8], SH_ID[12], SE_ID[15:13] */
#define VROCM_HWREG_XCC_ID 20 /* gfx94x/gfx95x only */
#define VROCM_HWREG_HW_ID1 23 /* gfx10+: SIMD_ID[9:8], WGP_ID[13:10], SA_ID[16], SE_ID[20:18] */

#define VROCM_HWID_SLOT(xcc, se, sh, unit)                                          \
    (((((xcc) * VROCM_HWID_MAX_SE) + (se)) * VROCM_HWID_MAX_SH + (sh)) * VROCM_HWID_MAX_UNIT + \
     (unit))

/* vrocm_hwid_slot — where this wave is physically running, as one index into the slot table.
 *
 * The architecture is chosen by the macros the compiler defines for the target it is building, not
 * by anything the host decides: `__GFX9__` covers CDNA and everything else here is gfx10 or newer.
 * A fat binary carries one code object per architecture and the loader picks, so a single build
 * reads the right register on every card it may be pointed at. */
__device__ static inline unsigned int vrocm_hwid_slot(void)
{
#if defined(__GFX9__)
    unsigned int hw = VROCM_GETREG(VROCM_HWREG_HW_ID);
    unsigned int unit = (hw >> 8) & 0xfu;  /* CU_ID — the unit CDNA allocates in */
    unsigned int sh = (hw >> 12) & 0x1u;   /* SH_ID */
    unsigned int se = (hw >> 13) & 0x7u;   /* SE_ID, per XCC */
#if defined(__gfx940__) || defined(__gfx941__) || defined(__gfx942__) || defined(__gfx950__)
    unsigned int xcc = VROCM_GETREG(VROCM_HWREG_XCC_ID) & 0xfu;
#else
    /* Single-XCC gfx9 has no XCC_ID register, and reading one that does not exist is not a
     * diagnosable mistake — it returns whatever the encoding happens to select. */
    unsigned int xcc = 0u;
#endif
#else
    unsigned int hw = VROCM_GETREG(VROCM_HWREG_HW_ID1);
    unsigned int unit = (hw >> 10) & 0xfu; /* WGP_ID — the unit RDNA allocates in */
    unsigned int sh = (hw >> 16) & 0x1u;   /* SA_ID */
    unsigned int se = (hw >> 18) & 0x7u;   /* SE_ID */
    unsigned int xcc = 0u;
#endif
    return VROCM_HWID_SLOT(xcc, se, sh, unit);
}

#endif /* VROCM_HWID_H */
