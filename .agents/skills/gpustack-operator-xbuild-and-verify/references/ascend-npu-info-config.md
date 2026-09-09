# npu_info.config — fields, shm-id, and the allocator mapping

vcann-rt reads `/etc/enpu/vcann-rt/npu_info.config` (mode 0644). Six fields:

| Field | Meaning | Our allocator source |
|---|---|---|
| `physical-npu-id` | host NPU index (0 = first) | `Accelerator.Index` |
| `virtual-npu-id` | unique per physical NPU, from 0 | lowest-free scan over live pods |
| `aicore-quota` | compute time-slice percent | `floor(R*100)` (R = `.sliced.units`/D) |
| `memory-quota` | HBM cap in **MB** | `floor(memMB * R)` |
| `shm-id` | shared-mem id from the card VDie-ID | `Accelerator.ID` with spaces → `-` |
| `scheduling-policy` | 1=fixed-share, **2=elastic (default)**, 3=best-effort | fixed to `2` |

Renderer: `renderNPUInfoConfig` in `pkg/devicemanager/allocator/ascend/deviceplugin.go`.

## Getting the shm-id (VDie-ID) on hardware

```bash
npu-smi info -t board -i <NPU> -c <CHIP>     # e.g. -i 0 -c 0
#   VDie ID  : E0F4EE64 802061B1 6A691492 89528485 104301E3
```
shm-id = that value with spaces replaced by `-`:
`E0F4EE64-802061B1-6A691492-89528485-104301E3`. The hyphen-joined VDie-ID is what the
vcann-rt README prescribes and guarantees global uniqueness.

## Verifying it loaded

Run any process with libvruntime.so preloaded (e.g. `enpu-monitor`) at `ENPU_LOG_LEVEL=3`;
each field logs `Success to load config: <key>, value: <v>` and then
`Successfully to initialize vnpu device.` (full stack adds `Successfully to initialize all module.`
once a real ACL `rtSetDevice` hook fires).
