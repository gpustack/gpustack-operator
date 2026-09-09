# AMD CU-mask conformance

The measured behaviour of `HSA_CU_MASK` on the two AMD GPU architectures GPUStack slices, as the
fixture the mask derivation is tested against.

Every figure here is a **hardware readout**, not a throughput inference: `rocm-cumask-check` launches
its own kernel, each wave reads its own physical identity out of `HW_ID` — plus `XCC_ID` on
multi-XCC parts — and the tool reports the set of units its waves actually ran on. That distinction
is the whole reason this page exists; see *Why occupancy and not throughput* below.

Read this with `csrc/amd/rocm-slicing-shim/tools/rocm_cumask_check.c`, which implements the rules,
and with the operator's Go-side derivation, which must reproduce the `mask` column of both tables.

## The one thing to get right first

**The derivation branches on `NUM_XCC`, and the two branches share no arithmetic.** Both are
correct-looking, and each is silently wrong on the other architecture:

- Carrying RDNA's WGP-pairing rule onto CDNA does not fail — it doubles every slice.
- Carrying CDNA's atom onto RDNA splits WGP pairs, and a split pair discards the whole mask.

A second trap sits in the topology itself: the reported shader-engine count is **device-wide and
already multiplied by the XCC count**. A card reporting `NUM_SHADER_ENGINES=32` with
`NUM_XCC=8` has four shader engines per XCC, not 32. Every per-XCC quantity is obtained by dividing.

Topology comes from the HSA agent-info API and never from KFD sysfs — `COMPUTE_UNIT_COUNT`
(`0xA002`), `NUM_SHADER_ENGINES` (`0xA00C`), `NUM_SHADER_ARRAYS_PER_SE` (`0xA00D`) and `NUM_XCC`
(`0xA111`). Both sources agree on both architectures, so this is about contract stability rather
than correctness; but on a card running under SR-IOV, `rocm-smi` could not complete a libdrm query
at all while the agent-info path returned every field.

## Table A — RDNA

Measured on a 60 CU / 30 WGP / 3 SE / 2 SA-per-SE / 1 XCC `gfx1101` card, ROCm 7.2.4.

```
W = CU / 2                       # WGP count
n = round(W * pct / 100)         # requested WGPs
n = max(S, floor(n / S) * S)     # align DOWN to a multiple of the shader-engine count
mask = "<idx>:" + ranges(each selected WGP w expanded to CUs {2w, 2w+1})
```

| requested | WGPs before align | after align | mask | WGPs occupied |
| --- | --- | --- | --- | --- |
| 10 % | 3 | 3 | `0:0-5` | 3 |
| 20 % | 6 | 6 | `0:0-11` | 6 |
| 25 % | 8 (7.5 → 8) | 6 | `0:0-11` | 6 |
| 50 % | 15 | 15 | `0:0-29` | 15 |
| 75 % | 23 (22.5 → 23) | 21 | `0:0-41` | 21 |
| 100 % | 30 | 30 | `0:0-59` | 30 |

The 25 % and 75 % rows are the ones that matter. The naive derivation — CU count straight from a
percentage, ascending first-fit, emit the range — produces `0:0-14` and `0:0-44`, and both leave the
container on the **whole card**.

### The unit compared on RDNA is the WGP, not the CU

`HW_ID1`'s `SIMD_ID` field only ever reports 0 or 1 there, measured, so the two CUs of a WGP are not
distinguishable from inside a wave. That is not a gap in the probe: the WGP is exactly RDNA's
allocation atom, so the architecture is compared in the unit it allocates in.

| register | field | bits |
| --- | --- | --- |
| `HW_ID1` (reg 23) | `SIMD_ID` | `[9:8]` |
| | `WGP_ID` | `[13:10]` |
| | `SA_ID` | `[16]` |
| | `SE_ID` | `[20:18]` |

### Negative rows — RDNA

| construction | why it fails | occupied |
| --- | --- | --- |
| `HSA_CU_MASK=0:0-14` | 15 CUs splits WGP 7; an orphaned CU invalidates the whole set | 30 of 30 WGPs |
| `ROC_GLOBAL_CU_MASK=0xC0000000` | every bit sits at or above the 30-WGP width, so all are ignored | 30 of 30 WGPs |
| `HSA_CU_MASK=GPU-<hex>:0-29` | a `GPU_list` that is not a decimal index is dropped, segment and all | 30 of 30 WGPs |

`0:0-13`, one CU **smaller** than the first row, correctly confines the container to 7 WGPs. The
rule is pair alignment, not the parity of the element count.

`ROC_GLOBAL_CU_MASK` is a hexadecimal bitmask whose **bit `i` is WGP `i`** here — the same asymmetry
that makes `hipDeviceProp_t.multiProcessorCount` report 30 on this 60-CU card. A valid one behaves:
`0x7fff` measured 15 WGPs occupied, matching `HSA_CU_MASK=0:0-29` exactly.

## Table B — CDNA

Measured on a 304 CU / 32 SE / 1 SA-per-SE / 8 XCC `gfx942` card, ROCm 7.2.4. "CU/XCC" is what the
hardware registers reported from inside the kernel, so it is occupancy, not intent.

```
X = NUM_XCC
n = round(CU * pct / 100)        # requested CUs
n = floor(n / X) * X             # align DOWN to whole "one CU in every XCC" atoms
reject the request when n < X    # a sub-atom mask does NOT clamp -- it fails open
mask = "<idx>:" + ranges(n bit indices, in whole atoms {b, b+X, ...})
```

| requested | CUs before align | after align | mask | CU/XCC occupied |
| --- | --- | --- | --- | --- |
| 1 % | 3 | reject (< 8) | — | — |
| 2.63 % | 8 | 8 | `0:0-7` | 1 each |
| 5 % | 15 | 8 | `0:0-7` | 1 each |
| 5.26 % | 16 | 16 | `0:0-15` | 2 each |
| 10.5 % | 32 | 32 | `0:0-31` | 4 each |
| 50 % | 152 | 152 | `0:0-151` | 19 each |
| 100 % | 304 | 304 | `0:0-303` | 38 each |

The 5 % row shows the cost of the atom: 15 CUs of budget buy 8 CUs of card, because the seven that
do not complete an atom cannot be spent. The 1 % row must be **refused** rather than rounded — see
the negative rows.

### The bit mapping, read out of the hardware

```
bit i  ->  XCC = i mod X ,  SE = (i div X) mod (SE / X) ,  CU = i div (X * (SE / X))
```

So eight consecutive bits are eight **different** XCCs with one CU each — never eight CUs of one
XCC. The unit compared is the CU.

| register | field | bits |
| --- | --- | --- |
| `HW_ID` (reg 4) | `CU_ID` | `[11:8]` |
| | `SH_ID` | `[12]` |
| | `SE_ID` | `[15:13]` |
| `XCC_ID` (reg 20) | `XCC_ID` | `[3:0]` |

### Negative rows — CDNA

| construction | why it fails | occupied |
| --- | --- | --- |
| `HSA_CU_MASK=0:0` | one bit reaches one XCC; the other seven receive no mask and run unmasked | 267 of 304 CUs |
| `HSA_CU_MASK=0:0-3` | four bits reach four XCCs; the other four run unmasked | 156 of 304 CUs |
| `HSA_CU_MASK=0:0,8,16,24` | all four bits are `≡ 0 mod 8`, so they all land on XCC 0 | 270 of 304 CUs |
| `HSA_CU_MASK=0:304-400` | every bit is at or above the CU count | 304 of 304 CUs |
| `HSA_CU_MASK=GPU-<hex>:0-151` | a `GPU_list` that is not a decimal index is dropped whole | 304 of 304 CUs |
| `--percent 1` | 3 CUs is below one 8-CU atom; the derivation refuses rather than clamping | (refused) |

**The first three are what a throughput-only probe would pass.** `0:0` measures a perfectly
plausible 3.7 % of the card's throughput while the container reaches 267 CUs, because the makespan
is set by the most constrained XCC and says nothing about the other seven.

### Disjointness is a property of the atoms, not of the bit sets

Two tenants given the "obviously disjoint" ranges `0:0-3` and `0:4-7` were each measured occupying
**156 CUs**, overlapping on 152 of them, while the ledger believed it had handed out 4 CUs each.
Given XCC-covering masks instead — `0:0-7` and `0:8-15` — each occupied exactly 8 CUs, they shared
nothing, and each measured its solo throughput.

## Why occupancy and not throughput

A CU mask fails **open**. The runtime that rejects one returns no error, logs no line and changes no
return code, so the cost of a wrong mask is not a smaller slice but the loss of all isolation — and
on a multi-XCC part the throughput reading stays plausible while it happens. Counting the physical
units the waves ran on is the only signal that separates the three cases a node has to tell apart:
a mask that took effect, a mask that was discarded, and a mask that was honoured on some XCCs only.

## Running the probe

```bash
rocm-cumask-check                     # derive a 50 % mask for device 0 and verify it
rocm-cumask-check --percent 25        # any integer percentage
HSA_CU_MASK=0:0-14 rocm-cumask-check  # verify a mask already in the environment
```

With no mask in the environment the tool derives one, sets `HSA_CU_MASK` and re-execs — ROCr reads
that variable while it initialises, so a probe that set it in-process would measure the environment
it started with. With a mask already set it verifies that mask as it stands, which is how each
negative row above is reproduced.

Exit codes: **0** the mask took effect as asked · **1** it did not · **2** the probe could not run
(no agent, a request below one atom, a malformed argument).

## Regenerating this page

```bash
# on the target host, inside a ROCm image with the devices passed through
csrc/amd/rocm-slicing-shim/build.sh tool rocm_cumask_check
for pct in 10 20 25 50 75 100; do ./rocm-cumask-check --percent "${pct}"; done
for m in 0:0-14 GPU-b3a1f0d2c4e5:0-29; do HSA_CU_MASK="${m}" ./rocm-cumask-check; done
ROC_GLOBAL_CU_MASK=0xC0000000 ./rocm-cumask-check
```

The CDNA rows come from the same commands on a `gfx942` host, with table B's masks and percentages.
Re-measure rather than edit when a new architecture or a new ROCm release arrives: every row here
had a plausible wrong answer that only hardware ruled out.
