# AMD HIP symbol manifest — `rocm/dev-ubuntu-22.04:7.2.4`

The symbol surface the ROCm slicing shim has to cover, re-established against the `libamdhip64`
**inside the build image** rather than a host's copy, and checked in so a later reader can re-run one
command and compare. An aggregate count nobody can reproduce is not evidence.

- Image: `rocm/dev-ubuntu-22.04:7.2.4`
- Digest: `sha256:a50a8547101ac9e7c35848ff609a56335214b789b468c9eb05dcd006a4a38c25`
- Library path inside the image: `/opt/rocm/lib/libamdhip64.so.7`
- ROCm version: `7.2.4` (the runtime's soname is `libamdhip64.so.7`; the two axes do not track each
  other, which is why the resolver asks for the soname and not for a ROCm version)

**Coverage is a difference between two sets, and only one of them can be read off the product.** That
sentence is the whole reason this page exists, and it was learned the expensive way: five entry points
that hand out device memory were missing from the interception table, and none of them was found by
review or by reasoning about which APIs a framework "probably" uses. Each was found by listing what the
runtime exports, subtracting what is interposed, and then **measuring** what was left. The generated
block below is that subtraction, kept runnable.

## How to regenerate

The block below the `<!-- generated -->` marker is the verbatim output of this command. Re-running it
against the same digest reproduces it byte for byte; a diff means either the image or a claim changed.

```bash
IMG=rocm/dev-ubuntu-22.04:7.2.4
CTR=${CTR:-docker}      # nerdctl works too; add --namespace k8s.io on a k3s/rke2 host
"${CTR}" run --rm -i --platform linux/amd64 "${IMG}" bash -s <<'EOF'
set -u
L=/opt/rocm/lib/libamdhip64.so.7

# The names this product interposes. Kept here rather than read out of the built object so the
# command runs against nothing but the image; AMD-CASE 1 asserts the same set out of
# libvrocm.so, so a drift between the two surfaces there rather than passing quietly.
INTERPOSED="hipMemGetInfo hipDeviceTotalMem
hipGetDeviceProperties hipGetDevicePropertiesR0600 hipGetDevicePropertiesR0000
hipMalloc hipMallocManaged hipExtMallocWithFlags hipMallocPitch hipMalloc3D
hipMallocArray hipMalloc3DArray hipHostMalloc
hipMemCreate hipMemRelease hipMemAllocPitch hipArrayCreate hipArray3DCreate hipArrayDestroy
hipMallocAsync hipMallocFromPoolAsync hipMemPoolImportPointer
hipFree hipFreeArray hipFreeAsync hipHostFree"

# entry <name> — the exported definition of one name: version tag, address, size. Three of these
# names sit at three DISTINCT addresses, which is the fact the whole page turns on.
entry() {
  readelf -W --dyn-syms "${L}" |
    awk -v want="$1" '$5 == "GLOBAL" && $7 != "UND" {
        n = $8; sub(/@.*/, "", n)
        if (n == want) { tag = $8; sub(/^[^@]*/, "", tag); printf "%-12s %s %6s\n", tag, $2, $3 }
      }' | sort -u | head -1
}

# family <regex> — every exported definition whose name matches, name@@tag, sorted.
family() {
  readelf -W --dyn-syms "${L}" |
    awk -v re="$1" '$5 == "GLOBAL" && $7 != "UND" && $8 ~ re { print $8 }' | sort -u
}

# The DENOMINATOR, and it is deliberately far wider than the interposed set. A narrow pattern here
# is how a door goes unnoticed: the first version of this page matched only names beginning
# hipMalloc/hipFree/hipMem, and every one of hipMemCreate, hipMemAllocPitch, hipArrayCreate and
# hipArray3DCreate — all four measured handing out memory under a quota — fell outside it. The net
# is now every exported name that could plausibly hand out, map or release device memory, and the
# section below it is what has to be reasoned about one name at a time.
MEMORY='^hip(Malloc|Free|Host|ExtMalloc|MemGetInfo|DeviceTotalMem|GetDeviceProperties|Mem(Create|Release|Map|Unmap|Address|SetAccess|AllocHost|AllocPitch|Retain|Import|Export|Pool)|Array|MipmappedArray|Ipc(Open|Close)MemHandle|ImportExternalMemory|ExternalMemoryGetMappedBuffer|GraphAddMem|DrvGraphAddMem|GraphicsMapResources|GraphicsResourceGetMappedPointer)'

echo "rocm-version: $(cat /opt/rocm/.info/version)"
echo "library:      $(readlink -f "${L}")"
echo "soname:       $(readelf -d "${L}" | sed -n 's/.*Library soname: \[\(.*\)\]/\1/p')"
echo
echo "### Counts"
echo
echo "exported-definitions: $(readelf -W --dyn-syms "${L}" | awk '$5 == "GLOBAL" && $7 != "UND"' | wc -l)"
echo "version-tagged:       $(readelf -W --dyn-syms "${L}" | grep -c '@@hip_')"
echo
echo "version-tag histogram:"
readelf -W --dyn-syms "${L}" | grep -oE '@@hip_[0-9.]+' | sort | uniq -c | sort -k1,1rn -k2,2 | sed 's/^/  /'
echo
echo "### The interposed entry points"
echo
printf '  %-30s %-12s %-16s %6s\n' NAME VERSION ADDRESS SIZE
for n in ${INTERPOSED}; do printf '  %-30s %s\n' "${n}" "$(entry "${n}")"; done
echo
echo "### Every exported allocating, freeing and capacity name"
echo
family "${MEMORY}" | sed 's/^/  /'
echo
echo "### ...of those, the ones NOT interposed"
echo
printf '%s\n' ${INTERPOSED} | sort -u > /tmp/interposed
family "${MEMORY}" | sed 's/@.*//' | sort -u > /tmp/exported
comm -13 /tmp/interposed /tmp/exported | sed 's/^/  /'
echo
echo "### The stream-ordered and pool family, as the runtime exports it"
echo
family '^hip(MallocAsync|MallocFromPoolAsync|FreeAsync|MemPool)' | sed 's/^/  /'
EOF
```

## What each wrapper substitutes

| Name | Version | What the wrapper does |
| --- | --- | --- |
| `hipMemGetInfo` | `@@hip_4.2` | calls through, then reports `total` as the quota and `free` as the **smaller** of what the slice has left and what the card has left — the card's figure still bounds it, so a framework is not promised memory the neighbours already took |
| `hipDeviceTotalMem` | `@@hip_4.2` | calls through, then reports the quota |
| `hipGetDevicePropertiesR0600` | `@@hip_6.0` | calls through, then rewrites `totalGlobalMem`. **This is the one that fires**, because ROCm 6+ headers rewrite every source call to it |
| `hipGetDevicePropertiesR0000` | `@@hip_4.2` | same, against the pre-6.0 struct — handing it the 6.0 struct would write 32 bytes past where a pre-6.0 caller expects |
| `hipGetDeviceProperties` | `@@hip_4.2` | same as R0000, and measured never called by anything built against ROCm 6+ headers. Wrapped anyway: it is a real exported entry a 4.x-era binary still reaches |
| `hipMalloc` · `hipMallocManaged` · `hipExtMallocWithFlags` | `@@hip_4.2` | admit, allocate and charge under one hold of the card's lock; a refusal is `hipErrorOutOfMemory` |
| `hipMallocPitch` · `hipMalloc3D` · `hipMemAllocPitch` | `@@hip_4.2` | the same, then **reconcile** to `pitch × height (× depth)` inside the same hold — the runtime picks the stride and only reports it on the way back. `hipMemAllocPitch` is the driver-API half, a separate symbol |
| `hipMallocArray` · `hipMalloc3DArray` | `@@hip_4.2` | charged from the channel description times the extent, since an array's size is not a parameter. Approximate by construction: the runtime may pad and does not say by how much |
| `hipArrayCreate` · `hipArray3DCreate` | `@@hip_4.2` | the driver-API halves of the same family; the size arrives as a format enum and a channel count instead of a channel description |
| `hipMemCreate` | `@@hip_6.0` | charged, keyed on the **handle** rather than a pointer — the address is chosen later by `hipMemMap`, and memory created and never mapped is still memory the card gave out. The card charged is the one `prop.location.id` names, not the calling thread's |
| `hipHostMalloc` | `@@hip_4.2` | **counted, never charged.** Pinned host pages are system RAM; charging them against a card's figure would refuse device allocations over host pressure |
| `hipMallocAsync` · `hipMallocFromPoolAsync` | `@@hip_5.1` | admitted and charged exactly like the classic family. Not a layer over `hipMalloc` — see below |
| `hipMemPoolImportPointer` | `@@hip_5.1` | counted, charged nothing: the allocation belongs to the exporting process and is already charged there |
| `hipFree` · `hipFreeArray` · `hipArrayDestroy` · `hipHostFree` | `@@hip_4.2` | refund by pointer or array handle. Either array free accepts an array from either array create, so both refund on the same key |
| `hipMemRelease` | `@@hip_6.0` | refunds the `hipMemCreate` handle |
| `hipFreeAsync` | `@@hip_5.1` | refunds when the free is **issued**, not when the stream reaches it — the alternative waits for a synchronisation this library never sees |

## Facts the surface settles

- **`hipGetDeviceProperties` is three distinct implementations, not one name with aliases.** The
  block below shows three different addresses, and the plain name's definition is **9 bytes** where
  the two suffixed ones are 44 — it is a thunk. ROCm 6+ headers macro-map the plain source name onto
  `…R0600`, so a wrapper written against the plain name compiles, links, loads and virtualises
  nothing. AMD-CASE 2's second control arm is exactly this experiment: with only the plain name
  interposed, the name binds into the control object and the call still reports the physical figure.
- **`hipMemGetInfo` does not cover the property entries.** AMD-CASE 2's first control arm interposes
  it alone and `hipDeviceProp_t.totalGlobalMem` still carries the card's real capacity. Without that
  arm, the full run passing would say nothing about which of the four wrappers did the work.
- **The pool family is a second door into the same memory, not a layer over `hipMalloc`.** With only
  the classic family wrapped, `hipMallocFromPoolAsync` took another 10 GiB on RDNA and 50 GiB on CDNA
  out of a card whose quota was 2 GiB. `hipDeviceGetDefaultMemPool` hands any caller a usable pool
  with no special privilege, and PyTorch's mempool path goes straight through it.
- **The runtime API and the driver API are two sets of doors into one room.** `hipMallocPitch` and
  `hipMemAllocPitch` are different exported symbols at different addresses; so are `hipMallocArray`
  and `hipArrayCreate`, and `hipFreeArray` and `hipArrayDestroy`. Measured, each driver-API half
  satisfied a 512 MiB request under a 64 MiB quota in the same run where its runtime-API twin was
  refused one. Covering one half of a pair is not covering the family.
- **The virtual-memory-management family allocates without any `hipMalloc` in it, and it is what a
  tuned framework uses.** PyTorch's expandable-segments allocator reserves an address range once and
  then creates and maps physical memory into it as the arena grows — `hipMemCreate`, `hipMemMap`,
  `hipMemSetAccess`. Measured, the whole sequence took 512 MiB under a 64 MiB quota and reported no
  error at any step. Only `hipMemCreate` allocates; the rest move addresses, so wrapping them would
  count one allocation several times.
- **Five doors, one method.** `hipMalloc3D`, `hipMemCreate`, `hipMemAllocPitch`, `hipArrayCreate`
  and `hipArray3DCreate` were all found the same way and none of them by review. The first pass of
  this page also got the method half wrong: its denominator was a narrow name pattern, and all four
  of the last ones fell outside it. **The width of the net is part of the claim.**

## The difference, reasoned one group at a time

The generated block ends with every name in the net that is **not** interposed. It is long, and most
of it is inert. What follows is why — a name that is not on this page under a heading is a name
nobody has thought about.

- **Address-space and permission operations — no memory changes hands.** `hipMemAddressReserve`,
  `hipMemAddressFree`, `hipMemMap`, `hipMemUnmap`, `hipMemSetAccess`, `hipMemMapArrayAsync`,
  `hipMemRetainAllocationHandle`. `hipMemCreate` is the single allocating point of that family;
  charging any of these would bill one allocation more than once, and a handle mapped at two
  addresses is still one allocation.
- **Pool lifecycle, not pool allocation.** `hipMemPoolCreate`, `hipMemPoolDestroy`,
  `hipMemPoolTrimTo`, `hipMemPoolSetAccess`, `hipMemPoolSetAttribute` and the pool export/import
  pair. Creating a pool takes nothing; `hipMallocFromPoolAsync` is where a pool hands memory out,
  and it is charged.
- **Host memory — not device memory, so not interposed.** `hipMallocHost`, `hipHostAlloc`,
  `hipMemAllocHost`, `hipFreeHost`, `hipHostRegister`, `hipHostUnregister`. Pinned system RAM is not
  device VRAM, so there is nothing on these paths to account. `hipHostMalloc` and `hipHostFree` are
  interposed all the same — counted, never charged — and AMD-CASE 3's `host` row is what keeps the
  second half of that true rather than assumed: it asks for twice the quota and requires success.
- **Memory this process did not allocate.** `hipIpcOpenMemHandle`, `hipIpcCloseMemHandle`,
  `hipImportExternalMemory`, `hipExternalMemoryGetMappedBuffer`, `hipMemImportFromShareableHandle`,
  `hipGraphicsMapResources`, `hipGraphicsResourceGetMappedPointer`. Each maps memory another
  process or another API already allocated and already paid for. Charging it would bill the same
  bytes twice, and — worse — recording it would let this container's free refund a charge it never
  made. This is the same policy `hipMemPoolImportPointer` follows, and it is a policy rather than an
  oversight.
- **Queries and descriptors.** `hipArrayGetDescriptor`, `hipArray3DGetDescriptor`,
  `hipArrayGetInfo`, `hipHostGetDevicePointer`, `hipHostGetFlags`, `hipMemPoolGetAccess`,
  `hipMemPoolGetAttribute`, `hipMipmappedArrayGetLevel`. They report; they do not allocate.
- **Copy and fill graph nodes.** `hipGraphAddMemcpyNode` and its variants, `hipGraphAddMemsetNode`,
  and the `hipDrvGraphAdd*` forms of both. They move or fill bytes that are already allocated.
- **Unsupported on the hardware this work has.** `hipMallocMipmappedArray`,
  `hipFreeMipmappedArray`, `hipMipmappedArrayCreate`, `hipMipmappedArrayDestroy` return
  `hipErrorNotSupported` (801) on `gfx1101` whatever the size. A wrapper for them could not be
  exercised, so writing one would be code no case can reach. They are named here so the next person
  with a part that supports them knows where to look — and the check is one command: run the
  measurement below with the mipmap arm and see whether it still returns 801.
- **`hipGraphAddMemAllocNode` — measured OPEN, and deliberately left so in this iteration.** A graph
  memory-allocation node took 512 MiB under a 64 MiB quota: the node is added at capture time, the
  memory is taken at **launch**, and one graph may be launched any number of times. Charging at
  node-add is therefore wrong, and charging at launch means wrapping `hipGraphLaunch` and walking
  the instantiated graph for its allocation and free nodes, with `hipDeviceGraphMemTrim` and
  `hipGraphAddMemFreeNode` deciding when the memory comes back. That is a different shape of problem
  from every other entry here — the accounting is no longer one call in, one call out — and getting
  it half right would produce charges nobody can reconcile. It is recorded as a known boundary with
  its measurement, not as an unknown.

## Measuring the difference yourself

Every claim above about a name being open or closed came from the same shape of experiment: set a
quota far below the request, take that one path, and read the status **and** the library's own
counter. On the target, with the tree staged:

```bash
# The nine families and four driver-API halves are AMD-CASE 3. For a name not yet covered, the
# smallest experiment is a program that takes that one path, run under:
#   VROCM_DEVICE_MEMORY_LIMIT_0=64  VROCM_LEDGER_PATH=<a fresh file>  LIBVROCM_LOG_LEVEL=2
# with /etc/ld.so.preload naming libvrocm.so, asking for 512 MiB.
# Open  = rc 0 and no `counter <entry> ... denials=` line.
# Closed = rc 2 (hipErrorOutOfMemory) and `denials=1` on that entry's counter.
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-3.sh
```

<!-- generated -->

```text
rocm-version: 7.2.4
library:      /opt/rocm-7.2.4/lib/libamdhip64.so.7.2.70204
soname:       libamdhip64.so.7

### Counts

exported-definitions: 540
version-tagged:       525

version-tag histogram:
      262 @@hip_4.2
       46 @@hip_4.5
       41 @@hip_5.3
       32 @@hip_5.1
       28 @@hip_4.3
       19 @@hip_5.2
       19 @@hip_7.1
       15 @@hip_4.4
       14 @@hip_6.2
       11 @@hip_6.4
       10 @@hip_7.2
        7 @@hip_5.5
        5 @@hip_5.0
        5 @@hip_5.6
        5 @@hip_6.0
        3 @@hip_6.1
        3 @@hip_6.5

### The interposed entry points

  NAME                           VERSION      ADDRESS            SIZE
  hipMemGetInfo                  @@hip_4.2    00000000003885a0     44
  hipDeviceTotalMem              @@hip_4.2    0000000000386090     44
  hipGetDeviceProperties         @@hip_4.2    0000000000089260      9
  hipGetDevicePropertiesR0600    @@hip_6.0    00000000003867b0     44
  hipGetDevicePropertiesR0000    @@hip_4.2    00000000003867e0     44
  hipMalloc                      @@hip_4.2    00000000003880f0     44
  hipMallocManaged               @@hip_4.2    0000000000388250     46
  hipExtMallocWithFlags          @@hip_4.2    0000000000386480     46
  hipMallocPitch                 @@hip_4.2    00000000003882c0     64
  hipMalloc3D                    @@hip_4.2    0000000000388120     26
  hipMallocArray                 @@hip_4.2    0000000000388170     64
  hipMalloc3DArray               @@hip_4.2    0000000000388140     46
  hipHostMalloc                  @@hip_4.2    0000000000387d10     46
  hipMemCreate                   @@hip_5.1    0000000000388460     64
  hipMemRelease                  @@hip_5.1    00000000003889e0     26
  hipMemAllocPitch               @@hip_4.2    0000000000388420     64
  hipArrayCreate                 @@hip_4.2    00000000003855b0     41
  hipArray3DCreate               @@hip_4.2    0000000000385550     41
  hipArrayDestroy                @@hip_4.3    00000000003855e0     23
  hipMallocAsync                 @@hip_5.1    00000000003881b0     46
  hipMallocFromPoolAsync         @@hip_5.1    00000000003881e0     64
  hipMemPoolImportPointer        @@hip_5.1    00000000003887d0     46
  hipFree                        @@hip_4.2    0000000000386540     26
  hipFreeArray                   @@hip_4.2    0000000000386560     26
  hipFreeAsync                   @@hip_5.1    0000000000386580     44
  hipHostFree                    @@hip_4.2    0000000000387c90     26

### Every exported allocating, freeing and capacity name

  hipArray3DCreate@@hip_4.2
  hipArray3DGetDescriptor@@hip_5.6
  hipArrayCreate@@hip_4.2
  hipArrayDestroy@@hip_4.3
  hipArrayGetDescriptor@@hip_5.6
  hipArrayGetInfo@@hip_5.6
  hipDeviceTotalMem@@hip_4.2
  hipDrvGraphAddMemFreeNode@@hip_6.2
  hipDrvGraphAddMemcpyNode@@hip_5.6
  hipDrvGraphAddMemsetNode@@hip_5.6
  hipExtMallocWithFlags@@hip_4.2
  hipExternalMemoryGetMappedBuffer@@hip_4.3
  hipFree@@hip_4.2
  hipFreeArray@@hip_4.2
  hipFreeAsync@@hip_5.1
  hipFreeHost@@hip_4.2
  hipFreeMipmappedArray@@hip_4.2
  hipGetDeviceProperties@@hip_4.2
  hipGetDevicePropertiesR0000@@hip_4.2
  hipGetDevicePropertiesR0600@@hip_6.0
  hipGraphAddMemAllocNode@@hip_5.5
  hipGraphAddMemFreeNode@@hip_5.5
  hipGraphAddMemcpyNode1D@@hip_4.3
  hipGraphAddMemcpyNode@@hip_4.3
  hipGraphAddMemcpyNodeFromSymbol@@hip_4.5
  hipGraphAddMemcpyNodeToSymbol@@hip_4.5
  hipGraphAddMemsetNode@@hip_4.3
  hipGraphicsMapResources@@hip_4.3
  hipGraphicsResourceGetMappedPointer@@hip_4.3
  hipHostAlloc@@hip_4.2
  hipHostFree@@hip_4.2
  hipHostGetDevicePointer@@hip_4.2
  hipHostGetFlags@@hip_4.2
  hipHostMalloc@@hip_4.2
  hipHostRegister@@hip_4.2
  hipHostUnregister@@hip_4.2
  hipImportExternalMemory@@hip_4.3
  hipIpcCloseMemHandle@@hip_4.2
  hipIpcOpenMemHandle@@hip_4.2
  hipMalloc3D@@hip_4.2
  hipMalloc3DArray@@hip_4.2
  hipMalloc@@hip_4.2
  hipMallocArray@@hip_4.2
  hipMallocAsync@@hip_5.1
  hipMallocFromPoolAsync@@hip_5.1
  hipMallocHost@@hip_4.2
  hipMallocManaged@@hip_4.2
  hipMallocMipmappedArray@@hip_4.2
  hipMallocPitch@@hip_4.2
  hipMemAddressFree@@hip_5.1
  hipMemAddressReserve@@hip_5.1
  hipMemAllocHost@@hip_4.2
  hipMemAllocPitch@@hip_4.2
  hipMemCreate@@hip_5.1
  hipMemExportToShareableHandle@@hip_5.1
  hipMemGetInfo@@hip_4.2
  hipMemImportFromShareableHandle@@hip_5.1
  hipMemMap@@hip_5.1
  hipMemMapArrayAsync@@hip_5.1
  hipMemPoolCreate@@hip_5.1
  hipMemPoolDestroy@@hip_5.1
  hipMemPoolExportPointer@@hip_5.1
  hipMemPoolExportToShareableHandle@@hip_5.1
  hipMemPoolGetAccess@@hip_5.1
  hipMemPoolGetAttribute@@hip_5.1
  hipMemPoolImportFromShareableHandle@@hip_5.1
  hipMemPoolImportPointer@@hip_5.1
  hipMemPoolSetAccess@@hip_5.1
  hipMemPoolSetAttribute@@hip_5.1
  hipMemPoolTrimTo@@hip_5.1
  hipMemRelease@@hip_5.1
  hipMemRetainAllocationHandle@@hip_5.1
  hipMemSetAccess@@hip_5.1
  hipMemUnmap@@hip_5.1
  hipMipmappedArrayCreate@@hip_4.2
  hipMipmappedArrayDestroy@@hip_4.2
  hipMipmappedArrayGetLevel@@hip_4.2

### ...of those, the ones NOT interposed

  hipArray3DGetDescriptor
  hipArrayGetDescriptor
  hipArrayGetInfo
  hipDrvGraphAddMemFreeNode
  hipDrvGraphAddMemcpyNode
  hipDrvGraphAddMemsetNode
  hipExternalMemoryGetMappedBuffer
  hipFreeHost
  hipFreeMipmappedArray
  hipGraphAddMemAllocNode
  hipGraphAddMemFreeNode
  hipGraphAddMemcpyNode
  hipGraphAddMemcpyNode1D
  hipGraphAddMemcpyNodeFromSymbol
  hipGraphAddMemcpyNodeToSymbol
  hipGraphAddMemsetNode
  hipGraphicsMapResources
  hipGraphicsResourceGetMappedPointer
  hipHostAlloc
  hipHostGetDevicePointer
  hipHostGetFlags
  hipHostRegister
  hipHostUnregister
  hipImportExternalMemory
  hipIpcCloseMemHandle
  hipIpcOpenMemHandle
  hipMallocHost
  hipMallocMipmappedArray
  hipMemAddressFree
  hipMemAddressReserve
  hipMemAllocHost
  hipMemExportToShareableHandle
  hipMemImportFromShareableHandle
  hipMemMap
  hipMemMapArrayAsync
  hipMemPoolCreate
  hipMemPoolDestroy
  hipMemPoolExportPointer
  hipMemPoolExportToShareableHandle
  hipMemPoolGetAccess
  hipMemPoolGetAttribute
  hipMemPoolImportFromShareableHandle
  hipMemPoolSetAccess
  hipMemPoolSetAttribute
  hipMemPoolTrimTo
  hipMemRetainAllocationHandle
  hipMemSetAccess
  hipMemUnmap
  hipMipmappedArrayCreate
  hipMipmappedArrayDestroy
  hipMipmappedArrayGetLevel

### The stream-ordered and pool family, as the runtime exports it

  hipFreeAsync@@hip_5.1
  hipMallocAsync@@hip_5.1
  hipMallocFromPoolAsync@@hip_5.1
  hipMemPoolCreate@@hip_5.1
  hipMemPoolDestroy@@hip_5.1
  hipMemPoolExportPointer@@hip_5.1
  hipMemPoolExportToShareableHandle@@hip_5.1
  hipMemPoolGetAccess@@hip_5.1
  hipMemPoolGetAttribute@@hip_5.1
  hipMemPoolImportFromShareableHandle@@hip_5.1
  hipMemPoolImportPointer@@hip_5.1
  hipMemPoolSetAccess@@hip_5.1
  hipMemPoolSetAttribute@@hip_5.1
  hipMemPoolTrimTo@@hip_5.1
```
