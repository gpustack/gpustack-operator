# Spec: Preflight and Detect Unhealthy Accelerator Reporting Across All Manufacturers

Status: Building
Blocked on: implementation and verification of T2–T3
Type: Feature

## Summary

On an accelerator node where one or more cards are unhealthy (for example, an 8-card node with 1 card reporting uncorrectable DRAM ECC errors or a failed vendor health state), in-cluster `Devices` and `device-manager detect` enumerate all 8 cards and mark the faulty card with `status.unhealthy: true`. Today, `device-manager preflight` ignores `Accelerator.Status.Unhealthy`. It reports `detection.state: ok` with `detail: "<Model> x8"`, emits no health failure row in `checks`, and attempts driver toggles and container slice probes against the unhealthy card.

This spec makes `device-manager preflight` report `Unhealthy` accelerators consistently with `Devices` and `device-manager detect` across all 9 supported manufacturers (`nvidia`, `ascend`, `amd`, `hygon`, `thead`, `cambricon`, `iluvatar`, `metax`, `mthreads`). `detection` continues to count all detected accelerators and annotates its `detail` with the unhealthy count when any card is unhealthy. Each unhealthy card receives a dedicated `device-health` check row with `state: unavailable` and `depth: declared`, and is excluded from downstream vendor driver checks, slice probes, and management checks while healthy cards on the same node continue to be preflighted.

## Motivation

### Goals

- **Consistent 8-card inventory with explicit `Unhealthy` reporting across all three entry points.** Across all 9 manufacturers, an enumerable card whose vendor health or ECC check fails must remain in `DevicesGroup.Accelerators` with `status.unhealthy: true` so that in-cluster `Devices`, `device-manager detect`, and `device-manager preflight` all report the same total card count and identify the same unhealthy card(s).
- **Clear `preflight` detection summary and per-card failure check.** When a group contains unhealthy cards, `detection` keeps `state: ok` and the full `accelerators` count, appends the unhealthy count to `detection.detail` (for example, `NVIDIA A100 x8 (1 unhealthy)`), and emits one `PreflightCheck` per unhealthy card (`capability: device-health`, `mode: unnamed`, `state: unavailable`, `depth: declared`).
- **Non-zero `preflight` exit code on unhealthy hardware.** Because `Failed()` inspects `Checks` for `PreflightStateUnavailable`, any `device-health` row with `state: unavailable` fails the preflight report and returns a non-zero exit code for bring-up automation.
- **Isolate unhealthy cards from downstream probes without hiding healthy cards.** Skip vendor driver precondition reads (`pf.PreflightAccelerator`), container slice measurements (`measureSliced`), and management checks (`checkManagement`) for unhealthy cards, while still running those checks for all healthy cards in the same group.
- **Support in-Pod `kubectl exec` inspection.** Ensure an operator running `gpustack-operator device-manager detect` or `gpustack-operator device-manager preflight --manufacturer=<manufacturer>` inside a running `gpustack-operator-device-manager-<manufacturer>` DaemonSet Pod sees the full card inventory and unhealthy status without disrupting live workloads.

### Non-Goals

- **No changes to the 9 vendor detectors' health criteria.** Each detector already sets `status.unhealthy: true` without dropping the card:
  - `ascend`: `GetHealth() >= 2`, falling back to `GetEccInfo` (`Enable_flag > 0 && Double_bit_error_cnt > 0`).
  - `cambricon`: `GetCardHealthStateV()` (`Health == 0`).
  - `iluvatar`: `GetHealth() != HealthOK`.
  - `nvidia`, `thead`, `mthreads`: `GetMemoryErrorCounter` / `GetEccErrorCounter` (`MEMORY_ERROR_TYPE_UNCORRECTED`, `VOLATILE_ECC`, `MEMORY_LOCATION_DRAM`) `> 0`.
  - `amd`: `GetGpuEccCount(GPU_BLOCK_UMC).Uncorrectable_count > 0`.
  - `hygon`: `GetEccCount(GPU_BLOCK_UMC).Uncorrectable_err > 0`.
  - `metax`: `GetTotalEccErrors().DramUE > 0`.
- **No new Kubernetes CRD fields or device-plugin changes.** `Devices` and `pkg/deviceplugin/server.go` already carry and enforce `status.unhealthy`.
- **No recovery of cards that fail basic identity enumeration.** A card whose handle, UUID, PCI bus ID, product name, or total memory cannot be read by the vendor driver cannot be named or grouped and continues to be skipped during enumeration.

## Proposal

When `device-manager preflight` runs on a node (either on a bare host with `-v /:/host` or via `kubectl exec` inside a running `gpustack-operator-device-manager-<manufacturer>` Pod):

1. `DetectAccelerator` enumerates all reachable accelerators for each requested manufacturer and populates `Accelerator.Status.Unhealthy`.
2. `detection(groups, unmeasured)` sums all accelerators in `groups` into `Detection.Accelerators`. For each group whose `Accelerators` slice contains one or more cards with `Status.Unhealthy == true`, the group fragment in `Detection.Detail` formats as `<Name> x<Total> (<Unhealthy> unhealthy)` (and `<Name> x<Total>` when all cards in the group are healthy). `Detection.State` remains `PreflightStateOK` as long as detection succeeded and groups are named.
3. Before invoking the manufacturer's `AcceleratorPreflighter`, `measureSliced`, or `checkManagement`, `preflight()` partitions each group's accelerators into unhealthy and healthy sets:
   - For every accelerator with `Status.Unhealthy == true`, `preflight()` emits a `PreflightCheck` row:
     - `Accelerator`: `accel.ID`
     - `Capability`: `"device-health"`
     - `Mode`: `device.PreflightModeUnnamed` (`"unnamed"`)
     - `State`: `device.PreflightStateUnavailable` (`"unavailable"`)
     - `Depth`: `device.PreflightDepthDeclared` (`"declared"`)
     - `Reason`: explains that the detect pass reported the accelerator as unhealthy, so the device plugin withholds it from allocations and preflight skipped its capability checks.
   - `preflight()` constructs a filtered `healthyGroups` list (retaining only healthy accelerators and omitting groups that have no healthy accelerators left) and passes `healthyGroups` to `pf.PreflightAccelerator`, `anyLogicallySliceable`, `measureSliced`, and `checkManagement`.
4. `Failed(grpList)` includes the `device-health` check in its failure list (`<manufacturer>: device-health on <id>: <reason>`), causing `Report()` to return a non-zero error after printing the YAML document.

### User Stories

#### Story 1
As a cluster administrator, I want in-cluster `Devices`, `device-manager detect`, and `device-manager preflight` to report all detected accelerators on a node (for example, 8 cards total) and explicitly identify any `Unhealthy` card (for example, 1 `Unhealthy`) across all 9 manufacturers, so that I see a consistent hardware inventory and health status before bring-up and in production.

#### Story 2
As an operations engineer, I want `device-manager preflight` to emit an `unavailable` check row for each `Unhealthy` accelerator, skip container slice probes on that faulty card, and exit non-zero, so that node bring-up automation rejects unhealthy hardware without hanging or failing inside probe containers on a broken card.

#### Story 3
As an operations engineer, I want to run `gpustack-operator device-manager detect` or `gpustack-operator device-manager preflight` directly via `kubectl exec` inside a `gpustack-operator-device-manager-<manufacturer>` Pod, so that I can inspect all cards and their health status on a live node without querying Kubernetes API objects or logs.

### Core Features & Acceptance Criteria

- **F1: Consistent 9-manufacturer `Unhealthy` retention in `DetectAccelerator`.**
  - *Acceptance:* Unit tests across all 9 vendor detectors verify that when a device's health or uncorrectable ECC check triggers on 1 of N enumerable cards, `DetectAccelerator` returns all N cards in `DevicesGroup.Accelerators` with `Status.Unhealthy == true` on the faulty card and `Status.Unhealthy == false` on the remaining N-1 cards.
- **F2: `Detection.Detail` surfaces unhealthy card counts without failing `Detection.State`.**
  - *Acceptance:* Given a group `"NVIDIA A100"` with 8 accelerators where 1 has `Status.Unhealthy == true`, `detection()` returns `State: PreflightStateOK`, `Depth: PreflightDepthDeclared`, `Accelerators: 8`, and `Detail: "NVIDIA A100 x8 (1 unhealthy)"`. When all 8 are healthy, `Detail` remains `"NVIDIA A100 x8"`.
- **F3: Per-accelerator `device-health` check for unhealthy accelerators.**
  - *Acceptance:* For each accelerator with `Status.Unhealthy == true`, `preflight()` emits a `PreflightCheck` with `Accelerator: <ID>`, `Capability: "device-health"`, `Mode: "unnamed"`, `State: PreflightStateUnavailable`, `Depth: PreflightDepthDeclared`, and a non-empty `Reason`.
  - *Acceptance:* `device-health` checks are emitted even for manufacturers whose `AcceleratorPreflighter` reads no driver state (`hygon`, `iluvatar`, `mthreads`) and even when all accelerators on the node are unhealthy.
- **F4: Healthy-only filtering for downstream preflight checks and library staging.**
  - *Acceptance:* Unhealthy accelerators are excluded from the `DevicesGroupList` passed to `pf.PreflightAccelerator`, `anyLogicallySliceable`, `measureSliced`, and `checkManagement`.
  - *Acceptance:* On an 8-card node with 1 unhealthy card and 7 healthy sliceable cards, `pf.PreflightAccelerator`, `measureSliced`, and `checkManagement` run on the 7 healthy cards only; `stageLibFor` still stages the preload library tree for the 7 healthy cards.
  - *Acceptance:* On a node where **all** detected accelerators of a manufacturer are unhealthy, `pf.PreflightAccelerator`, `stageLibFor`, `measureSliced`, and `checkManagement` do not run for those unhealthy cards, and the group's `Checks` contains the per-accelerator `device-health` rows (plus any `stale-container-sweep` rows if sweep failed).
- **F5: Non-zero exit via `Failed()` and documentation update.**
  - *Acceptance:* `Failed(grpList)` returns `<manufacturer>: device-health on <acceleratorID>: <reason>` for each unhealthy accelerator.
  - *Acceptance:* `docs/modules/devices/preflight.md` documents the `device-health` row, the `(<N> unhealthy)` detection detail format, and how to run `detect` and `preflight` inside a `gpustack-operator-device-manager-<manufacturer>` Pod via `kubectl exec`.

### Notes / Constraints / Caveats

- **Why `Detection.State` stays `PreflightStateOK` when a card is unhealthy:** In `pkg/devicemanager/preflight/preflight.go`, `Detection.State == PreflightStateUnavailable` means the detect pass itself failed (for example, the driver could not be measured, the group could not be named, or the container saw fewer cards than the host CLI), which causes `preflight()` to return early with `noteDetectionFailed` and skip all checks. Keeping `Detection.State == PreflightStateOK` lets `preflight()` evaluate the remaining healthy cards while the `device-health` check row (`State: PreflightStateUnavailable`) still guarantees a non-zero exit code via `Failed()`.
- **Why `device-health` uses `Mode: PreflightModeUnnamed`:** An unhealthy card is withheld from **every** allocation mode (`exclusive`, `shared`, `sliced`, `partitioned`, and `visibility`) in `pkg/deviceplugin/server.go`. Filing `device-health` under `PreflightModeUnnamed` (`"unnamed"`) matches `stale-container-sweep` and `preflight-panicked`, which also apply across the whole accelerator rather than a single allocation mode.
- **Why `device-health` rows are added outside the vendor preflighter loop that appends the unnamed-mode warning:** In `preflight.go`, checks returned by `pf.PreflightAccelerator` with an empty `Mode` have `" (this check names no allocation mode...)"` appended to their `Reason`. Runner-generated checks (`device-health`, `stale-container-sweep`, `preflight-panicked`) set `Mode: device.PreflightModeUnnamed` directly and do not receive that warning suffix.
- **Running inside a `device-manager` DaemonSet Pod:** The `gpustack-operator-device-manager-<manufacturer>` DaemonSet mounts `/dev`, `/sys`, and vendor driver paths, but does not mount `/` at `/host`. When an operator runs `kubectl exec <pod> -- gpustack-operator device-manager preflight --manufacturer=<manufacturer>`, `p.host.Validate()` detects that `/host` is absent and automatically downgrades the run to `--dry-run`. This executes `DetectAccelerator`, reports `detection` and `device-health`, runs read-only driver checks, and simulates slice/management injections without locking the node, toggling driver modes, or starting containers.

### Boundaries

- **Always:** Keep every enumerable accelerator (where handle, UUID, PCI info, name, and total memory succeed) in `DevicesGroup.Accelerators` across all 9 manufacturers; run `make lint` and `make lint docs` before completing implementation.
- **Ask first:** Changing any vendor detector's hardware health or ECC threshold, or modifying Kubernetes CRD schemas under `api/`.
- **Never:** Start probe containers or toggle driver modes on an accelerator with `Status.Unhealthy == true`.

### Risks and Mitigations

- **Risk:** All accelerators of a manufacturer on a node are unhealthy, leaving the filtered `healthyGroups` list empty and triggering `noteNoCheck` or skipping `read.Note`. → **Mitigation:** Emit `device-health` checks for all unhealthy accelerators before checking `len(grp.Checks) == 0`, and skip `pf.PreflightAccelerator` / `measureSliced` / `checkManagement` when `len(healthyGroups) == 0` so `grp.Checks` carries the `device-health` rows cleanly.
- **Risk:** Vendor preflighters with a `Note` (such as `hygon`, `iluvatar`, `mthreads`) lose their `Note` or emit misleading notes when some or all cards are unhealthy. → **Mitigation:** When at least one healthy card exists, preserve `read.Note` from `pf.PreflightAccelerator(healthyGroups)`. When all cards are unhealthy, `grp.Checks` is non-empty (containing the `device-health` rows), so no fallback `noteNoCheck` is needed.
- **Risk:** `crossCheckHost` compares `view.Accelerators` (from `nvidia-smi -L`, `npu-smi info -l`, or `rocm-smi --showuniqueid`) against `detection.Accelerators`. If `detection.Accelerators` excluded unhealthy cards, `crossCheckHost` would falsely report missing container mounts on a node with an unhealthy card. → **Mitigation:** `detection.Accelerators` continues to count all detected accelerators (healthy and unhealthy alike), matching the host CLI count.

## Design Details

### Commands

```bash
# Run unit tests for preflight and all 9 vendor detectors
go test ./pkg/devicemanager/preflight/... ./pkg/devicemanager/detector/...

# Run Go and shell linters
make lint

# Run documentation linter (when docs/modules/devices/preflight.md is updated)
make lint docs
```

### Project Structure

```text
pkg/
  device/
    types.go                             # PreflightGroup, PreflightDetection, PreflightCheck types
  devicemanager/
    detector/
      {amd,ascend,cambricon,hygon,iluvatar,metax,mthreads,nvidia,thead}/
        device.go                        # Per-vendor DetectAccelerator and MonitorAccelerator health logic
    preflight/
      preflight.go                       # PreflightAccelerator, detection, unhealthyChecks, healthyGroups, Failed
      preflight_test.go                  # Unit tests for detection, unhealthy checks, and healthy-only filtering
docs/
  modules/
    devices/
      preflight.md                       # Preflight user guide and in-Pod kubectl exec usage
```

### Code Style

Follow the existing table-driven test and plain-comment conventions in [`pkg/devicemanager/preflight/preflight.go`](../pkg/devicemanager/preflight/preflight.go):

```go
// capDeviceHealth is the capability an unhealthy accelerator reports under. Like
// capPreflightPanicked and capStaleSweep, it is this package's own word: the detect pass already
// asked the vendor library whether the card is healthy, and an unhealthy card is withheld from
// every allocation mode by the device plugin.
const capDeviceHealth = "device-health"
```

- Keep comments plain and short: no emoji, no decorative symbols, no circled digits, and no spec task identifiers.
- Handle errors explicitly and keep functions focused on one responsibility.

### Implementation Plan

- [x] **T1 · Preflight unhealthy detection summary, `device-health` checks, and healthy-only filtering** `[completed]`
  - `Blocked by`: None
  - `Owns`: `pkg/devicemanager/preflight/preflight.go`, `pkg/devicemanager/preflight/preflight_test.go`
  - **Work**:
    - Define `const capDeviceHealth = "device-health"` in `pkg/devicemanager/preflight/preflight.go`.
    - Update `detection(groups, unmeasured)` so that each group with one or more `Status.Unhealthy == true` cards formats its fragment in `Detection.Detail` as `<Name> x<Total> (<Unhealthy> unhealthy)` while keeping `Detection.State == PreflightStateOK` and counting all detected accelerators in `Detection.Accelerators`.
    - Add a helper in `pkg/devicemanager/preflight/preflight.go` that splits `groups` into unhealthy `PreflightCheck` rows (`Capability: capDeviceHealth`, `Mode: device.PreflightModeUnnamed`, `State: device.PreflightStateUnavailable`, `Depth: device.PreflightDepthDeclared`) and a filtered `healthyGroups` slice containing only healthy accelerators.
    - Pass `healthyGroups` to `pf.PreflightAccelerator`, `anyLogicallySliceable`, `measureSliced`, and `checkManagement` so unhealthy cards are never probed or toggled, and handle the all-unhealthy case cleanly without emitting `noteNoCheck`.
    - Add table-driven unit tests in `pkg/devicemanager/preflight/preflight_test.go` covering `Detection.Detail` formatting, `device-health` check emission, healthy-only filtering for sliceable and management checks, all-unhealthy groups, and `Failed()` error reporting.
  - `Verify`: `go test -v ./pkg/devicemanager/preflight -run 'TestPreflight|TestFailed'`

- [ ] **T2 · Detector health-evaluation helpers and unhealthy-retention unit tests across all 9 manufacturers** `[pending]`
  - `Blocked by`: None
  - `Owns`: `pkg/devicemanager/detector/detector_test.go`, `pkg/devicemanager/detector/amd/device.go`, `pkg/devicemanager/detector/amd/device_test.go`, `pkg/devicemanager/detector/ascend/device_test.go`, `pkg/devicemanager/detector/cambricon/device_test.go`, `pkg/devicemanager/detector/hygon/device.go`, `pkg/devicemanager/detector/hygon/device_test.go`, `pkg/devicemanager/detector/iluvatar/device.go`, `pkg/devicemanager/detector/iluvatar/device_test.go`, `pkg/devicemanager/detector/metax/device.go`, `pkg/devicemanager/detector/metax/device_test.go`, `pkg/devicemanager/detector/mthreads/device.go`, `pkg/devicemanager/detector/mthreads/device_test.go`, `pkg/devicemanager/detector/nvidia/device.go`, `pkg/devicemanager/detector/nvidia/device_test.go`, `pkg/devicemanager/detector/thead/device.go`, `pkg/devicemanager/detector/thead/device_test.go`
  - **Work**:
    - Extract pure health-evaluation helpers (matching `evaluateDeviceUnhealthy` in `ascend` and `evaluateCardUnhealthy` in `cambricon`) in the remaining 7 vendor detector packages (`amd`, `hygon`, `iluvatar`, `metax`, `mthreads`, `nvidia`, `thead`) without changing any health or ECC threshold, and call them from both `DetectAccelerator` and `MonitorAccelerator`.
    - Add table-driven unit tests in each of the 9 vendor detector packages verifying healthy, unhealthy, and unreadable/unsupported return codes.
    - Add a unit test in `pkg/devicemanager/detector/detector_test.go` verifying that an 8-card group with 1 `Status.Unhealthy == true` card across all 9 manufacturers retains all 8 cards through `alignDeviceGroups` with `Status.Unhealthy` preserved on the faulty card.
  - `Verify`: `go test -v ./pkg/devicemanager/detector/... -run 'TestEvaluate|TestAlignDeviceGroups'`

- [ ] **T3 · Preflight documentation for `device-health` and in-Pod `kubectl exec` inspection** `[pending]`
  - `Blocked by`: T1
  - `Owns`: `docs/modules/devices/preflight.md`
  - **Work**:
    - Document the `device-health` capability row (`mode: unnamed`, `state: unavailable`, `depth: declared`) and the `<Model> x<Total> (<Unhealthy> unhealthy)` detection detail format in `docs/modules/devices/preflight.md`.
    - Document how to inspect node accelerators and health status directly via `kubectl exec` into `gpustack-operator-device-manager-<manufacturer>` using `gpustack-operator device-manager detect` and `gpustack-operator device-manager preflight --manufacturer=<manufacturer>`.
  - `Verify`: `make lint docs`

### Test Plan

#### Prerequisite testing updates

None. Existing fake detector, fake preflighter, and fake container runtime fixtures in `pkg/devicemanager/preflight/preflight_test.go` and `pkg/devicemanager/detector/detector_test.go` support constructing `Accelerator` slices with `Status.Unhealthy: true`.

#### Unit tests

- **F1 (9-manufacturer detector health evaluation and card retention):**
  - `pkg/devicemanager/detector/{amd,ascend,cambricon,hygon,iluvatar,metax,mthreads,nvidia,thead}/*_test.go`: verify each manufacturer's health/ECC helper returns `true` when the vendor error condition is met and `false` when healthy or when the vendor query returns an unsupported/unreadable status.
  - `pkg/devicemanager/detector/detector_test.go` (`TestAlignDeviceGroups`): verify that an 8-card group containing 1 unhealthy card retains all 8 cards and preserves `Status.Unhealthy == true` on the faulty card.
- **F2 (`Detection.Detail` unhealthy count formatting):**
  - `pkg/devicemanager/preflight/preflight_test.go` (`TestPreflight_Detection`): verify that an 8-card group with 1 unhealthy card produces `State: PreflightStateOK`, `Accelerators: 8`, and `Detail: "<Model> x8 (1 unhealthy)"`, and that multi-group nodes annotate only the group(s) with unhealthy cards.
- **F3 & F4 (`device-health` check emission and healthy-only filtering):**
  - `pkg/devicemanager/preflight/preflight_test.go`: verify that `preflight()` emits one `device-health` check (`Mode: "unnamed"`, `State: "unavailable"`, `Depth: "declared"`) per unhealthy card, excludes unhealthy cards from `pf.PreflightAccelerator`, `measureSliced`, and `checkManagement`, still stages preload libraries and probes the remaining healthy cards, and handles nodes where all cards in a group are unhealthy without emitting `noteNoCheck`.
- **F5 (`Failed()` non-zero exit reporting):**
  - `pkg/devicemanager/preflight/preflight_test.go` (`TestFailed`): verify that `Failed()` includes `<manufacturer>: device-health on <id>: <reason>` so `Report()` exits non-zero when an unhealthy card is present.

#### Integration tests

- `pkg/devicemanager/preflight/preflight_test.go`: exercise `PreflightAccelerator` end-to-end with a fake runtime and mixed healthy/unhealthy sliceable accelerators to confirm probe containers are created only for healthy accelerator indices while the final YAML report includes both the `device-health` failure for the unhealthy card and the measured slice checks for the healthy cards.

#### e2e tests

None required. In-cluster `Devices` and `deviceplugin` behavior is unchanged, and `make lint` plus `make lint docs` validate all code, unit tests, and documentation references.

## Alternatives

- **Mark `Detection.State = PreflightStateUnavailable` when any accelerator is unhealthy.**
  Rejected because `preflight()` treats `Detection.State == PreflightStateUnavailable` as an unrecoverable detection failure (`noteDetectionFailed`) and skips all checks for the manufacturer. A single unhealthy card on an 8-card node would hide the preflight results of the other 7 healthy cards and would not produce a per-accelerator `PreflightCheck` naming the faulty card's ID.
- **Run vendor driver checks and container slice probes on unhealthy accelerators alongside `device-health`.**
  Rejected because `pkg/deviceplugin/server.go` already withholds unhealthy cards from kubelet allocations. Toggling driver modes or starting probe containers on a card with uncorrectable memory or health errors risks driver hangs, container timeouts, and secondary probe failures that obscure the hardware fault.
- **Add `nvmlDeviceGetRemappedRows` / `hgmlDeviceGetRemappedRows` health checks in this change.**
  Rejected for now to keep the scope focused on surfacing the existing 9-manufacturer `Status.Unhealthy` signal consistently across `Devices`, `detect`, and `preflight` without introducing unmeasured C binding changes across older GPU architectures.

## Open Questions

None.
