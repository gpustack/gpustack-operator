# THead HGGC/HGML symbol manifest — `gpustack/thead-ppu-devel:2.1.1`

The symbol surface the THead logical-slicing modules have to cover, re-established against the
libraries **inside the devel image** rather than a host's copies, and checked in so a later reader can
re-run one command and compare. An aggregate count nobody can reproduce is not evidence.

- Image: `gpustack/thead-ppu-devel:2.1.1`
- Digest: `sha256:5f83fd14370d0dc12929a815962f34bc7ca630b382eb8c330621f25add32da6d`
- Library path inside the image: `${PPU_HOME}/targets/x86_64-linux/lib`
- SDK generation: `hggcrt_version:v3` (package version `2.1.1`; the two axes do not track each other)

## How to regenerate

The block below the `<!-- generated -->` marker is the verbatim output of this command. Re-running it
against the same digest reproduces it byte for byte; a diff means either the image or a claim changed.

```bash
IMG=gpustack/thead-ppu-devel:2.1.1
CTR=${CTR:-docker}      # nerdctl works too; add --namespace k8s.io on a k3s/rke2 host
"${CTR}" run --rm -i --platform linux/amd64 "${IMG}" bash -s <<'EOF'
set -u
L="${PPU_HOME}/targets/x86_64-linux/lib"
exported() {
  readelf -W --dyn-syms "$1" \
    | awk '{ n = $8; sub(/@.*/, "", n);
             if ($5 == "GLOBAL" && $6 == "DEFAULT" && $7 != "UND" && n ~ /^hg/) print n }' \
    | sort -u
}
exported "${L}/libhggc.so"    > /tmp/hggc.txt
exported "${L}/libhgml.so"    > /tmp/hgml.txt
exported "${L}/libhggcrt.13.0.so" > /tmp/hggcrt.txt

echo "sdk-version: $(tr '\n' ' ' < "${PPU_HOME}/VERSION.txt")"
echo
echo "### Library inventory"
echo
printf '%-26s %12s  %s\n' NAME BYTES ROLE
for n in libhggcrt.13.0.so libhggc.so libhggc_wrapper.so libhgml.so libalippu.so libuki.so libhgpti.so; do
  printf '%-26s %12s  %s\n' "${n}" "$(stat -c '%s' "${L}/${n}")" \
    "$(readelf -d "${L}/${n}" | awk '/NEEDED/ {gsub(/[][]/,"",$NF); printf "%s ", $NF}')"
done
echo
echo "### Counts, libhggc.so (the driver layer)"
echo
echo "exported-hg-symbols: $(wc -l < /tmp/hggc.txt)"
echo "suffixed-variants:   $(grep -cE '_(v[0-9]+)(_(ptds|ptsz))?$|_(ptds|ptsz)$' /tmp/hggc.txt)"
echo "base-names:          $(sed -E 's/_(v[0-9]+)?(_?(ptds|ptsz))?$//' /tmp/hggc.txt | sort -u | wc -l)"
echo "launch-entries:      $(grep -ciE 'launch' /tmp/hggc.txt)"
echo
echo "suffix-histogram:"
grep -oE '_(v[0-9]+)(_(ptds|ptsz))?$|_(ptds|ptsz)$' /tmp/hggc.txt | sort | uniq -c | sort -rn | sed 's/^/  /'
echo
echo "### Launch entries, libhggc.so"
echo
grep -iE 'launch' /tmp/hggc.txt | sed 's/^/  /'
echo
echo "### Memory-path entries, libhggc.so"
echo
grep -E '^hgMem(Alloc|Free|GetInfo|Create|Map|Unmap|Release|Pool)' /tmp/hggc.txt | sed 's/^/  /'
echo
echo "### Entry-table exports, libhggc.so"
echo
grep -E 'GetProcAddress|GetExportTable' /tmp/hggc.txt | sed 's/^/  /'
echo
echo "### Visibility entries, libhgml.so"
echo
grep -E '^hgml(DeviceGetMemoryInfo|DeviceGetProcessUtilization|DeviceGetComputeRunningProcesses|DeviceGetCount|DeviceGetHandleByIndex|Init|Shutdown|ErrorString)' /tmp/hgml.txt | sed 's/^/  /'
echo
echo "### Runtime-layer allocation entries, libhggcrt.13.0.so"
echo
grep -E '^hggc(Malloc|Free|MemGetInfo)' /tmp/hggcrt.txt | sed 's/^/  /'
echo
echo "### All exported hg* symbols, libhggc.so"
echo
sed 's/^/  /' /tmp/hggc.txt
EOF
```

## What the numbers corrected

Three of the counts the research produced from a **host's** copies survive against the image, and one
does not:

| Claim | Host-derived | Image `2.1.1` | Verdict |
| --- | --- | --- | --- |
| exported `hg*` symbols in `libhggc.so` | 620 | 620 | holds |
| suffixed variants | 183 | 183 | holds |
| base names after stripping suffixes | 437 | 437 | holds |
| launch entries | 23 | **16** | corrected |

The launch figure was the only one that moved. By the measure recorded above — exported `hg*` symbols in
`libhggc.so` whose name contains `launch`, case-insensitively — the driver layer has 16, made of 10 base
names and their `_ptsz` variants. Anything claiming 23 is either counting a different library (the
runtime layer has its own `hggcLaunch*` family) or a different SDK generation.

## Facts the layout settles

- **`hggcMalloc` is not in the driver layer.** It lives in `libhggcrt.13.0.so`, the runtime layer, next
  to `hggcFree`, `hggcMallocAsync`, `hggcMallocFromPoolAsync` and `hggcMemGetInfo`. The driver-layer
  counterpart is `hgMemAlloc_v2`. This is what makes Gate 2 a real experiment: the workload calls the
  runtime entry, the shim interposes the driver entry, and the shim's counter is what proves the call
  crossed from one to the other. A shim that interposed `hggcMalloc` itself would prove nothing about
  `libhggc.so`.
- **`libhggcrt` reaches the driver layer through `DT_NEEDED`**, so plain symbol interposition works
  there and no `dlsym` hook is needed — the opposite of HGML, which `ppu-smi` reaches by `dlopen` plus
  `dlsym` on the explicit handle.
- **Both the plain and the `_v2` name are exported** for `hgMemAlloc`, `hgMemFree` and `hgMemGetInfo`.
  `hggc.h` maps the plain source name onto the `_v2` symbol the way `cuda.h` maps `cuMemAlloc`, so a
  shim written against the header interposes only the `_v2` form. The plain forms are the v1 ABI and do
  **not** share the v2 signature (`HGdeviceptr_v1` is `unsigned int`), so covering them means writing
  the v1 prototypes deliberately, not reusing the v2 ones. Consequence for Gate 2: when **no** counter
  moved, that means the shim watched a name nobody called — it must never be read as the allocation
  bypassing `libhggc.so`.
- **The vendor wrapper is `libhggc_wrapper.so`**, not `libhg_wrapper.so`, and `libcc_wrapper.so` is a
  second one. Neither is named in any library's `DT_NEEDED`, so both are opt-in.
- **`hgGetProcAddress`, `hgGetProcAddress_v2` and `hgGetExportTable` are all present**, which is why the
  `hggc/` module has to cover them: a caller that resolves an entry point through one of these bypasses
  any interposition of the entry point itself.
- **`libhgml.so` does not link HGGC.** Its `DT_NEEDED` is `libdl`, `libpthread`, `libuki`, `libc` — it
  reaches the driver through UKI, which is why an HGML-only shim can neither observe nor enforce an
  allocation.
- `hgmlDeviceGetProcessUtilization` exists in exactly one version, while
  `hgmlDeviceGetComputeRunningProcesses` has three (`_v2`, `_v3`). Gate 3 uses both: the first for the
  supported-at-runtime question, the second because a process merely holding a context appears there
  even when utilisation accounting reports nothing.

<!-- generated -->

```text
sdk-version: ppu_sdk_detection_magic hggcrt_version:v3 

### Library inventory

NAME                              BYTES  ROLE
libhggcrt.13.0.so               7504064  libdl.so.2 libpthread.so.0 libalippu.so libhggc.so libstdc++.so.6 libgcc_s.so.1 libc.so.6 
libhggc.so                     41757384  libalippu.so libpthread.so.0 librt.so.1 libdl.so.2 libstdc++.so.6 libgcc_s.so.1 libc.so.6 
libhggc_wrapper.so              4875800  libdl.so.2 libstdc++.so.6 libpthread.so.0 libgcc_s.so.1 libc.so.6 
libhgml.so                      2296912  libdl.so.2 libpthread.so.0 libuki.so libc.so.6 
libalippu.so                    1948752  libuki.so libdl.so.2 libpthread.so.0 librt.so.1 libstdc++.so.6 libgcc_s.so.1 libc.so.6 
libuki.so                        822608  libpthread.so.0 libdl.so.2 libstdc++.so.6 libgcc_s.so.1 libc.so.6 
libhgpti.so                     1968608  libpthread.so.0 libdl.so.2 libperfworks.so libstdc++.so.6 libgcc_s.so.1 libc.so.6 ld-linux-x86-64.so.2 

### Counts, libhggc.so (the driver layer)

exported-hg-symbols: 620
suffixed-variants:   183
base-names:          437
launch-entries:      16

suffix-histogram:
       84 _v2
       55 _ptsz
       19 _v2_ptsz
       17 _v2_ptds
        3 _v3
        3 _ptds
        1 _v4
        1 _v3_ptsz

### Launch entries, libhggc.so

  hgGraphLaunch
  hgGraphLaunch_ptsz
  hgLaunch
  hgLaunchCooperativeKernel
  hgLaunchCooperativeKernelMultiDevice
  hgLaunchCooperativeKernel_ptsz
  hgLaunchGrid
  hgLaunchGridAsync
  hgLaunchHostFunc
  hgLaunchHostFunc_ptsz
  hgLaunchKernel
  hgLaunchKernelEx
  hgLaunchKernelExAD
  hgLaunchKernelExAD_ptsz
  hgLaunchKernelEx_ptsz
  hgLaunchKernel_ptsz

### Memory-path entries, libhggc.so

  hgMemAlloc
  hgMemAllocAsync
  hgMemAllocAsync_ptsz
  hgMemAllocFromPoolAsync
  hgMemAllocFromPoolAsync_ptsz
  hgMemAllocHost
  hgMemAllocHost_v2
  hgMemAllocManaged
  hgMemAllocPitch
  hgMemAllocPitch_v2
  hgMemAlloc_v2
  hgMemCreate
  hgMemFree
  hgMemFreeAsync
  hgMemFreeAsync_ptsz
  hgMemFreeHost
  hgMemFree_v2
  hgMemGetInfo
  hgMemGetInfo_v2
  hgMemMap
  hgMemMapArrayAsync
  hgMemMapArrayAsync_ptsz
  hgMemPoolCreate
  hgMemPoolDestroy
  hgMemPoolExportPointer
  hgMemPoolExportToShareableHandle
  hgMemPoolGetAccess
  hgMemPoolGetAttribute
  hgMemPoolImportFromShareableHandle
  hgMemPoolImportPointer
  hgMemPoolSetAccess
  hgMemPoolSetAttribute
  hgMemPoolTrimTo
  hgMemRelease
  hgMemUnmap

### Entry-table exports, libhggc.so

  hgGetExportTable
  hgGetProcAddress
  hgGetProcAddress_v2

### Visibility entries, libhgml.so

  hgmlDeviceGetComputeRunningProcesses
  hgmlDeviceGetComputeRunningProcesses_v2
  hgmlDeviceGetComputeRunningProcesses_v3
  hgmlDeviceGetCount
  hgmlDeviceGetCount_v2
  hgmlDeviceGetHandleByIndex
  hgmlDeviceGetHandleByIndex_v2
  hgmlDeviceGetMemoryInfo
  hgmlDeviceGetMemoryInfo_v2
  hgmlDeviceGetProcessUtilization
  hgmlErrorString
  hgmlInit
  hgmlInitWithFlags
  hgmlInit_v2
  hgmlShutdown

### Runtime-layer allocation entries, libhggcrt.13.0.so

  hggcFree
  hggcFreeArray
  hggcFreeAsync
  hggcFreeAsync_ptsz
  hggcFreeHost
  hggcFreeMipmappedArray
  hggcMalloc
  hggcMalloc3D
  hggcMalloc3DArray
  hggcMallocArray
  hggcMallocAsync
  hggcMallocAsync_ptsz
  hggcMallocFromPoolAsync
  hggcMallocFromPoolAsync_ptsz
  hggcMallocHost
  hggcMallocManaged
  hggcMallocMipmappedArray
  hggcMallocPitch
  hggcMemGetInfo

### All exported hg* symbols, libhggc.so

  hgArray3DCreate
  hgArray3DCreate_v2
  hgArray3DGetDescriptor
  hgArray3DGetDescriptor_v2
  hgArrayCreate
  hgArrayCreate_v2
  hgArrayDestroy
  hgArrayGetDescriptor
  hgArrayGetDescriptor_v2
  hgArrayGetMemoryRequirements
  hgArrayGetPlane
  hgArrayGetSparseProperties
  hgCheckpointProcessCheckpoint
  hgCheckpointProcessGetRestoreThreadId
  hgCheckpointProcessGetState
  hgCheckpointProcessLock
  hgCheckpointProcessRestore
  hgCheckpointProcessUnlock
  hgCoredumpGetAttribute
  hgCoredumpGetAttributeGlobal
  hgCoredumpSetAttribute
  hgCoredumpSetAttributeGlobal
  hgCtxAttach
  hgCtxCreate
  hgCtxCreate_v2
  hgCtxCreate_v3
  hgCtxCreate_v4
  hgCtxDestroy
  hgCtxDestroy_v2
  hgCtxDetach
  hgCtxDisablePeerAccess
  hgCtxEnablePeerAccess
  hgCtxFromGreenCtx
  hgCtxGetApiVersion
  hgCtxGetCacheConfig
  hgCtxGetCurrent
  hgCtxGetDevResource
  hgCtxGetDevice
  hgCtxGetDevice_v2
  hgCtxGetExecAffinity
  hgCtxGetFlags
  hgCtxGetId
  hgCtxGetLimit
  hgCtxGetSharedMemConfig
  hgCtxGetStreamPriorityRange
  hgCtxPopCurrent
  hgCtxPopCurrent_v2
  hgCtxPushCurrent
  hgCtxPushCurrent_v2
  hgCtxRecordEvent
  hgCtxResetPersistingL2Cache
  hgCtxSetCacheConfig
  hgCtxSetCurrent
  hgCtxSetFlags
  hgCtxSetLimit
  hgCtxSetSharedMemConfig
  hgCtxSynchronize
  hgCtxSynchronize_v2
  hgCtxWaitEvent
  hgDevResourceGenerateDesc
  hgDevSmResourceSplitByCount
  hgDeviceCanAccessPeer
  hgDeviceComputeCapability
  hgDeviceGet
  hgDeviceGetAttribute
  hgDeviceGetAttributeAD
  hgDeviceGetByPCIBusId
  hgDeviceGetCount
  hgDeviceGetDefaultMemPool
  hgDeviceGetDevResource
  hgDeviceGetExecAffinitySupport
  hgDeviceGetGraphMemAttribute
  hgDeviceGetHgSciSyncAttributes
  hgDeviceGetHostAtomicCapabilities
  hgDeviceGetLuid
  hgDeviceGetMemPool
  hgDeviceGetName
  hgDeviceGetP2PAtomicCapabilities
  hgDeviceGetP2PAttribute
  hgDeviceGetPCIBusId
  hgDeviceGetProperties
  hgDeviceGetTexture1DLinearMaxWidth
  hgDeviceGetUuid
  hgDeviceGetUuid_v2
  hgDeviceGraphMemTrim
  hgDevicePrimaryCtxGetState
  hgDevicePrimaryCtxRelease
  hgDevicePrimaryCtxRelease_v2
  hgDevicePrimaryCtxReset
  hgDevicePrimaryCtxReset_v2
  hgDevicePrimaryCtxRetain
  hgDevicePrimaryCtxSetFlags
  hgDevicePrimaryCtxSetFlags_v2
  hgDeviceRegisterAsyncNotification
  hgDeviceSetGraphMemAttribute
  hgDeviceSetMemPool
  hgDeviceTotalMem
  hgDeviceTotalMem_v2
  hgDeviceUnregisterAsyncNotification
  hgDriverGetVersion
  hgDriverGetVersionAD
  hgEventCreate
  hgEventDestroy
  hgEventDestroy_v2
  hgEventElapsedTime
  hgEventElapsedTime_v2
  hgEventQuery
  hgEventRecord
  hgEventRecordWithFlags
  hgEventRecordWithFlags_ptsz
  hgEventRecord_ptsz
  hgEventSynchronize
  hgFlushGPUDirectRDMAWrites
  hgFuncGetAttribute
  hgFuncGetModule
  hgFuncGetName
  hgFuncGetParamInfo
  hgFuncIsLoaded
  hgFuncLoad
  hgFuncSetAttribute
  hgFuncSetBlockShape
  hgFuncSetCacheConfig
  hgFuncSetSharedMemConfig
  hgFuncSetSharedSize
  hgGetErrorName
  hgGetErrorString
  hgGetExportTable
  hgGetProcAddress
  hgGetProcAddress_v2
  hgGraphAddBatchMemOpNode
  hgGraphAddChildGraphNode
  hgGraphAddDependencies
  hgGraphAddDependencies_v2
  hgGraphAddEmptyNode
  hgGraphAddEventRecordNode
  hgGraphAddEventWaitNode
  hgGraphAddHostNode
  hgGraphAddKernelNode
  hgGraphAddKernelNode_v2
  hgGraphAddMemAllocNode
  hgGraphAddMemFreeNode
  hgGraphAddMemcpyNode
  hgGraphAddMemsetNode
  hgGraphAddNode
  hgGraphAddNode_v2
  hgGraphBatchMemOpNodeGetParams
  hgGraphBatchMemOpNodeSetParams
  hgGraphChildGraphNodeGetGraph
  hgGraphClone
  hgGraphConditionalHandleCreate
  hgGraphCreate
  hgGraphDebugDotPrint
  hgGraphDestroy
  hgGraphDestroyNode
  hgGraphEventRecordNodeGetEvent
  hgGraphEventRecordNodeSetEvent
  hgGraphEventWaitNodeGetEvent
  hgGraphEventWaitNodeSetEvent
  hgGraphExecBatchMemOpNodeSetParams
  hgGraphExecChildGraphNodeSetParams
  hgGraphExecDestroy
  hgGraphExecEventRecordNodeSetEvent
  hgGraphExecEventWaitNodeSetEvent
  hgGraphExecGetFlags
  hgGraphExecHostNodeSetParams
  hgGraphExecKernelNodeSetParams
  hgGraphExecKernelNodeSetParams_v2
  hgGraphExecMemcpyNodeSetParams
  hgGraphExecMemsetNodeSetParams
  hgGraphExecNodeSetParams
  hgGraphExecUpdate
  hgGraphExecUpdate_v2
  hgGraphGetEdges
  hgGraphGetEdges_v2
  hgGraphGetNodes
  hgGraphGetRootNodes
  hgGraphHostNodeGetParams
  hgGraphHostNodeSetParams
  hgGraphInstantiate
  hgGraphInstantiateWithFlags
  hgGraphInstantiateWithParams
  hgGraphInstantiateWithParams_ptsz
  hgGraphInstantiate_v2
  hgGraphKernelNodeCopyAttributes
  hgGraphKernelNodeGetAttribute
  hgGraphKernelNodeGetParams
  hgGraphKernelNodeGetParams_v2
  hgGraphKernelNodeSetAttribute
  hgGraphKernelNodeSetParams
  hgGraphKernelNodeSetParams_v2
  hgGraphLaunch
  hgGraphLaunch_ptsz
  hgGraphMemAllocNodeGetParams
  hgGraphMemFreeNodeGetParams
  hgGraphMemcpyNodeGetParams
  hgGraphMemcpyNodeSetParams
  hgGraphMemsetNodeGetParams
  hgGraphMemsetNodeSetParams
  hgGraphNodeFindInClone
  hgGraphNodeGetDependencies
  hgGraphNodeGetDependencies_v2
  hgGraphNodeGetDependentNodes
  hgGraphNodeGetDependentNodes_v2
  hgGraphNodeGetEnabled
  hgGraphNodeGetType
  hgGraphNodeSetEnabled
  hgGraphNodeSetParams
  hgGraphReleaseUserObject
  hgGraphRemoveDependencies
  hgGraphRemoveDependencies_v2
  hgGraphRetainUserObject
  hgGraphUpload
  hgGraphUpload_ptsz
  hgGraphicsMapResources
  hgGraphicsResourceGetMappedPointer
  hgGraphicsResourceSetMapFlags
  hgGraphicsUnmapResources
  hgGreenCtxCreate
  hgGreenCtxDestroy
  hgGreenCtxGetDevResource
  hgGreenCtxGetId
  hgGreenCtxRecordEvent
  hgGreenCtxStreamCreate
  hgGreenCtxWaitEvent
  hgInit
  hgIpcCloseMemHandle
  hgIpcGetEventHandle
  hgIpcGetMemHandle
  hgIpcOpenEventHandle
  hgIpcOpenMemHandle
  hgIpcOpenMemHandle_v2
  hgKernelGetAttribute
  hgKernelGetFunction
  hgKernelGetLibrary
  hgKernelGetName
  hgKernelGetParamInfo
  hgKernelSetAttribute
  hgKernelSetCacheConfig
  hgLaunch
  hgLaunchCooperativeKernel
  hgLaunchCooperativeKernelMultiDevice
  hgLaunchCooperativeKernel_ptsz
  hgLaunchGrid
  hgLaunchGridAsync
  hgLaunchHostFunc
  hgLaunchHostFunc_ptsz
  hgLaunchKernel
  hgLaunchKernelEx
  hgLaunchKernelExAD
  hgLaunchKernelExAD_ptsz
  hgLaunchKernelEx_ptsz
  hgLaunchKernel_ptsz
  hgLibraryEnumerateKernels
  hgLibraryGetGlobal
  hgLibraryGetKernel
  hgLibraryGetKernelCount
  hgLibraryGetManaged
  hgLibraryGetModule
  hgLibraryGetUnifiedFunction
  hgLibraryLoadData
  hgLibraryLoadFromFile
  hgLibraryUnload
  hgLinkAddData
  hgLinkAddData_v2
  hgLinkAddFile
  hgLinkAddFile_v2
  hgLinkComplete
  hgLinkCreate
  hgLinkCreate_v2
  hgLinkDestroy
  hgLogsCurrent
  hgLogsDumpToFile
  hgLogsDumpToMemory
  hgLogsRegisterCallback
  hgLogsUnregisterCallback
  hgMemAddressFree
  hgMemAddressReserve
  hgMemAdvise
  hgMemAdvise_v2
  hgMemAlloc
  hgMemAllocAsync
  hgMemAllocAsync_ptsz
  hgMemAllocFromPoolAsync
  hgMemAllocFromPoolAsync_ptsz
  hgMemAllocHost
  hgMemAllocHost_v2
  hgMemAllocManaged
  hgMemAllocPitch
  hgMemAllocPitch_v2
  hgMemAlloc_v2
  hgMemBatchDecompressAsync
  hgMemBatchDecompressAsync_ptsz
  hgMemCreate
  hgMemDiscardAndPrefetchBatchAsync
  hgMemDiscardAndPrefetchBatchAsync_ptsz
  hgMemDiscardBatchAsync
  hgMemDiscardBatchAsync_ptsz
  hgMemExportToShareableHandle
  hgMemFree
  hgMemFreeAsync
  hgMemFreeAsync_ptsz
  hgMemFreeHost
  hgMemFree_v2
  hgMemGetAccess
  hgMemGetAddressRange
  hgMemGetAddressRange_v2
  hgMemGetAllocationGranularity
  hgMemGetAllocationPropertiesFromHandle
  hgMemGetDefaultMemPool
  hgMemGetHandleForAddressRange
  hgMemGetInfo
  hgMemGetInfo_v2
  hgMemGetMemPool
  hgMemHostAlloc
  hgMemHostGetDevicePointer
  hgMemHostGetDevicePointer_v2
  hgMemHostGetFlags
  hgMemHostRegister
  hgMemHostRegister_v2
  hgMemHostUnregister
  hgMemImportFromShareableHandle
  hgMemMap
  hgMemMapArrayAsync
  hgMemMapArrayAsync_ptsz
  hgMemPoolCreate
  hgMemPoolDestroy
  hgMemPoolExportPointer
  hgMemPoolExportToShareableHandle
  hgMemPoolGetAccess
  hgMemPoolGetAttribute
  hgMemPoolImportFromShareableHandle
  hgMemPoolImportPointer
  hgMemPoolSetAccess
  hgMemPoolSetAttribute
  hgMemPoolTrimTo
  hgMemPrefetchAsync
  hgMemPrefetchAsync_ptsz
  hgMemPrefetchAsync_v2
  hgMemPrefetchAsync_v2_ptsz
  hgMemPrefetchBatchAsync
  hgMemPrefetchBatchAsync_ptsz
  hgMemRangeGetAttribute
  hgMemRangeGetAttributes
  hgMemRelease
  hgMemRetainAllocationHandle
  hgMemSetAccess
  hgMemSetMemPool
  hgMemUnmap
  hgMemcpy
  hgMemcpy2D
  hgMemcpy2DAsync
  hgMemcpy2DAsync_v2
  hgMemcpy2DAsync_v2_ptsz
  hgMemcpy2DUnaligned
  hgMemcpy2DUnaligned_v2
  hgMemcpy2DUnaligned_v2_ptds
  hgMemcpy2D_v2
  hgMemcpy2D_v2_ptds
  hgMemcpy3D
  hgMemcpy3DAsync
  hgMemcpy3DAsync_v2
  hgMemcpy3DAsync_v2_ptsz
  hgMemcpy3DBatchAsync
  hgMemcpy3DBatchAsync_ptsz
  hgMemcpy3DBatchAsync_v2
  hgMemcpy3DBatchAsync_v2_ptsz
  hgMemcpy3DPeer
  hgMemcpy3DPeerAsync
  hgMemcpy3DPeerAsync_ptsz
  hgMemcpy3DPeer_ptds
  hgMemcpy3D_v2
  hgMemcpy3D_v2_ptds
  hgMemcpyAsync
  hgMemcpyAsync_ptsz
  hgMemcpyAtoA
  hgMemcpyAtoA_v2
  hgMemcpyAtoA_v2_ptds
  hgMemcpyAtoD
  hgMemcpyAtoD_v2
  hgMemcpyAtoD_v2_ptds
  hgMemcpyAtoH
  hgMemcpyAtoHAsync
  hgMemcpyAtoHAsync_v2
  hgMemcpyAtoHAsync_v2_ptsz
  hgMemcpyAtoH_v2
  hgMemcpyAtoH_v2_ptds
  hgMemcpyBatchAsync
  hgMemcpyBatchAsync_ptsz
  hgMemcpyBatchAsync_v2
  hgMemcpyBatchAsync_v2_ptsz
  hgMemcpyDtoA
  hgMemcpyDtoA_v2
  hgMemcpyDtoA_v2_ptds
  hgMemcpyDtoD
  hgMemcpyDtoDAsync
  hgMemcpyDtoDAsync_v2
  hgMemcpyDtoDAsync_v2_ptsz
  hgMemcpyDtoD_v2
  hgMemcpyDtoD_v2_ptds
  hgMemcpyDtoH
  hgMemcpyDtoHAsync
  hgMemcpyDtoHAsync_v2
  hgMemcpyDtoHAsync_v2_ptsz
  hgMemcpyDtoH_v2
  hgMemcpyDtoH_v2_ptds
  hgMemcpyHtoA
  hgMemcpyHtoAAsync
  hgMemcpyHtoAAsync_v2
  hgMemcpyHtoAAsync_v2_ptsz
  hgMemcpyHtoA_v2
  hgMemcpyHtoA_v2_ptds
  hgMemcpyHtoD
  hgMemcpyHtoDAsync
  hgMemcpyHtoDAsync_v2
  hgMemcpyHtoDAsync_v2_ptsz
  hgMemcpyHtoD_v2
  hgMemcpyHtoD_v2_ptds
  hgMemcpyPeer
  hgMemcpyPeerAsync
  hgMemcpyPeerAsync_ptsz
  hgMemcpyPeer_ptds
  hgMemcpy_ptds
  hgMemsetD16
  hgMemsetD16Async
  hgMemsetD16Async_ptsz
  hgMemsetD16_v2
  hgMemsetD16_v2_ptds
  hgMemsetD2D16
  hgMemsetD2D16Async
  hgMemsetD2D16Async_ptsz
  hgMemsetD2D16_v2
  hgMemsetD2D16_v2_ptds
  hgMemsetD2D32
  hgMemsetD2D32Async
  hgMemsetD2D32Async_ptsz
  hgMemsetD2D32_v2
  hgMemsetD2D32_v2_ptds
  hgMemsetD2D8
  hgMemsetD2D8Async
  hgMemsetD2D8Async_ptsz
  hgMemsetD2D8_v2
  hgMemsetD2D8_v2_ptds
  hgMemsetD32
  hgMemsetD32Async
  hgMemsetD32Async_ptsz
  hgMemsetD32_v2
  hgMemsetD32_v2_ptds
  hgMemsetD8
  hgMemsetD8Async
  hgMemsetD8Async_ptsz
  hgMemsetD8_v2
  hgMemsetD8_v2_ptds
  hgMipmappedArrayCreate
  hgMipmappedArrayDestroy
  hgMipmappedArrayGetLevel
  hgMipmappedArrayGetMemoryRequirements
  hgMipmappedArrayGetSparseProperties
  hgModuleEnumerateFunctions
  hgModuleGetFunction
  hgModuleGetFunctionCount
  hgModuleGetGlobal
  hgModuleGetGlobal_v2
  hgModuleGetLoadingMode
  hgModuleGetSurfRef
  hgModuleGetTexRef
  hgModuleLoad
  hgModuleLoadData
  hgModuleLoadDataEx
  hgModuleLoadFatBinary
  hgModuleUnload
  hgMulticastAddDevice
  hgMulticastBindAddr
  hgMulticastBindMem
  hgMulticastCreate
  hgMulticastGetGranularity
  hgMulticastUnbind
  hgOccupancyAvailableDynamicSMemPerBlock
  hgOccupancyMaxActiveBlocksPerMultiprocessor
  hgOccupancyMaxActiveBlocksPerMultiprocessorWithFlags
  hgOccupancyMaxActiveClusters
  hgOccupancyMaxPotentialBlockSize
  hgOccupancyMaxPotentialBlockSizeWithFlags
  hgOccupancyMaxPotentialClusterSize
  hgParamSetSize
  hgParamSetTexRef
  hgParamSetf
  hgParamSeti
  hgParamSetv
  hgPointerGetAttribute
  hgPointerGetAttributes
  hgPointerSetAttribute
  hgProfilerInitialize
  hgProfilerStart
  hgProfilerStop
  hgSignalExternalSemaphoresAsync
  hgStreamAddCallback
  hgStreamAddCallback_ptsz
  hgStreamAttachMemAsync
  hgStreamAttachMemAsync_ptsz
  hgStreamBatchMemOp
  hgStreamBatchMemOp_ptsz
  hgStreamBatchMemOp_v2
  hgStreamBatchMemOp_v2_ptsz
  hgStreamBeginCapture
  hgStreamBeginCaptureToGraph
  hgStreamBeginCaptureToGraph_ptsz
  hgStreamBeginCapture_ptsz
  hgStreamBeginCapture_v2
  hgStreamBeginCapture_v2_ptsz
  hgStreamCopyAttributes
  hgStreamCopyAttributes_ptsz
  hgStreamCreate
  hgStreamCreateWithPriority
  hgStreamDestroy
  hgStreamDestroy_v2
  hgStreamEndCapture
  hgStreamEndCapture_ptsz
  hgStreamGetAttribute
  hgStreamGetAttribute_ptsz
  hgStreamGetCaptureInfo
  hgStreamGetCaptureInfo_ptsz
  hgStreamGetCaptureInfo_v2
  hgStreamGetCaptureInfo_v2_ptsz
  hgStreamGetCaptureInfo_v3
  hgStreamGetCaptureInfo_v3_ptsz
  hgStreamGetCtx
  hgStreamGetCtx_ptsz
  hgStreamGetCtx_v2
  hgStreamGetCtx_v2_ptsz
  hgStreamGetDevice
  hgStreamGetDevice_ptsz
  hgStreamGetFlags
  hgStreamGetFlags_ptsz
  hgStreamGetGreenCtx
  hgStreamGetId
  hgStreamGetId_ptsz
  hgStreamGetPriority
  hgStreamGetPriority_ptsz
  hgStreamIsCapturing
  hgStreamIsCapturing_ptsz
  hgStreamQuery
  hgStreamQuery_ptsz
  hgStreamSetAttribute
  hgStreamSetAttributeAD
  hgStreamSetAttributeAD_ptsz
  hgStreamSetAttribute_ptsz
  hgStreamSynchronize
  hgStreamSynchronize_ptsz
  hgStreamUpdateCaptureDependencies
  hgStreamUpdateCaptureDependencies_ptsz
  hgStreamUpdateCaptureDependencies_v2
  hgStreamUpdateCaptureDependencies_v2_ptsz
  hgStreamWaitEvent
  hgStreamWaitEvent_ptsz
  hgStreamWaitValue32
  hgStreamWaitValue32_ptsz
  hgStreamWaitValue32_v2
  hgStreamWaitValue32_v2_ptsz
  hgStreamWaitValue64
  hgStreamWaitValue64_ptsz
  hgStreamWaitValue64_v2
  hgStreamWaitValue64_v2_ptsz
  hgStreamWriteValue32
  hgStreamWriteValue32_ptsz
  hgStreamWriteValue32_v2
  hgStreamWriteValue32_v2_ptsz
  hgStreamWriteValue64
  hgStreamWriteValue64_ptsz
  hgStreamWriteValue64_v2
  hgStreamWriteValue64_v2_ptsz
  hgSurfObjectCreate
  hgSurfObjectDestroy
  hgSurfObjectGetResourceDesc
  hgSurfRefGetArray
  hgSurfRefSetArray
  hgTensorMapEncodeIm2col
  hgTensorMapEncodeIm2colWide
  hgTensorMapEncodeTiled
  hgTensorMapReplaceAddress
  hgTexObjectCreate
  hgTexObjectDestroy
  hgTexObjectGetResourceDesc
  hgTexObjectGetResourceViewDesc
  hgTexObjectGetTextureDesc
  hgTexRefCreate
  hgTexRefDestroy
  hgTexRefGetAddress
  hgTexRefGetAddressMode
  hgTexRefGetAddress_v2
  hgTexRefGetArray
  hgTexRefGetBorderColor
  hgTexRefGetFilterMode
  hgTexRefGetFlags
  hgTexRefGetFormat
  hgTexRefGetMaxAnisotropy
  hgTexRefGetMipmapFilterMode
  hgTexRefGetMipmapLevelBias
  hgTexRefGetMipmapLevelClamp
  hgTexRefGetMipmappedArray
  hgTexRefSetAddress
  hgTexRefSetAddress2D
  hgTexRefSetAddress2D_v2
  hgTexRefSetAddress2D_v3
  hgTexRefSetAddressMode
  hgTexRefSetAddress_v2
  hgTexRefSetArray
  hgTexRefSetBorderColor
  hgTexRefSetFilterMode
  hgTexRefSetFlags
  hgTexRefSetFormat
  hgTexRefSetMaxAnisotropy
  hgTexRefSetMipmapFilterMode
  hgTexRefSetMipmapLevelBias
  hgTexRefSetMipmapLevelClamp
  hgTexRefSetMipmappedArray
  hgThreadExchangeStreamCaptureMode
  hgUserObjectCreate
  hgUserObjectRelease
  hgUserObjectRetain
  hgWaitExternalSemaphoresAsync
```
