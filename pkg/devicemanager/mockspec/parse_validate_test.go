package mockspec

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestParseAndResolveNode(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		nodeName string
		check    func(t *testing.T, resolved *ResolvedNodeSpec)
	}{
		{
			name: "flat yaml preset with count truncation, group override, card overrides, and faults",
			input: `
preset: nvidia-h100
count: 3
fault:
  initError:
    failCount: 2
    message: transient nvml init error
groupOverride:
  driverVersion: "575.57.08"
cardOverrides:
  0:
    metrics:
      memoryUsage: 40960
      coresUtilization: 85
      temperature: 68
      powerUsage: 520
  1:
    queryState: unqueryable
  2:
    unhealthy: true
    physicalSliced:
      profiles:
        - name: 1g.10gb
          memoryMib: 9856
          computeSlices: 1
          memorySlices: 1
          count: 7
          placements:
            - {start: 0, length: 1}
`,
			nodeName: "worker-1",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				mfr := resolved.Manufacturers[nodefeature.ManufacturerNVIDIA]
				require.NotNil(t, mfr)
				assert.True(t, mfr.Fault.InitError.Enabled)
				assert.Equal(t, 2, mfr.Fault.InitError.FailCount)
				assert.Equal(t, "transient nvml init error", mfr.Fault.InitError.Message)

				require.Len(t, mfr.Slots, 3)
				assert.Equal(t, "575.57.08", mfr.Slots[0].Group.DriverVersion)
				assert.Equal(t, QueryStateOK, mfr.Slots[0].QueryState)
				assert.Equal(t, uint64(40960), mfr.Slots[0].Metrics.MemoryUsage)
				assert.Equal(t, device.CalculateUtilization(uint64(40960), uint64(81559)), mfr.Slots[0].Metrics.MemoryUtilization)
				assert.Equal(t, uint32(85), mfr.Slots[0].Metrics.CoresUtilization)

				assert.Equal(t, QueryStateUnqueryable, mfr.Slots[1].QueryState)

				assert.Equal(t, QueryStateOK, mfr.Slots[2].QueryState)
				assert.True(t, mfr.Slots[2].Accelerator.Status.Unhealthy)
				assert.Equal(t, int32(0), mfr.Slots[2].Accelerator.Status.LogicalSliced.Count)
				assert.Equal(t, int32(7), mfr.Slots[2].Accelerator.Status.PhysicalSliced.Count)

				groups := mfr.BuildGroups()
				require.Len(t, groups, 1)
				assert.Equal(t, int32(256), groups[0].AcceleratorSlicedDetail.Logical.Count)
				assert.Equal(t, int32(7), groups[0].AcceleratorSlicedDetail.Physical.Count)
			},
		},
		{
			name: "flat json preset with count expansion synthesizes distinct ids, indexes, and bdfs",
			input: `{
  "preset": "nvidia-rtx-5090d",
  "count": 4,
  "interfaces": [],
  "rdmaDevices": [
    {
      "name": "mlx5_0",
      "driver": "mlx5_core",
      "pciBusId": "0000:10:00.0",
      "numaAffinity": "0",
      "ports": [
        {
          "number": 1,
          "linkLayer": "InfiniBand",
          "state": "ACTIVE",
          "physicalState": "LINK_UP",
          "rate": "400 Gb/sec (4X NDR)",
          "netDevs": ["ib0"]
        }
      ]
    },
    {
      "name": "rdmap16s0",
      "driver": "efa",
      "pciBusId": "0000:11:00.0"
    }
  ]
}`,
			nodeName: "",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				mfr := resolved.Manufacturers[nodefeature.ManufacturerNVIDIA]
				require.NotNil(t, mfr)
				require.Len(t, mfr.Slots, 4)

				seenIDs := make(map[string]bool)
				seenBDFs := make(map[string]bool)
				for i, slot := range mfr.Slots {
					assert.Equal(t, uint32(i), slot.Index)
					assert.Equal(t, uint32(i), slot.Accelerator.Index)
					assert.False(t, seenIDs[slot.Accelerator.ID], "duplicate ID %s", slot.Accelerator.ID)
					seenIDs[slot.Accelerator.ID] = true
					assert.False(t, seenBDFs[slot.Accelerator.Topology.PciBusID], "duplicate BDF %s", slot.Accelerator.Topology.PciBusID)
					seenBDFs[slot.Accelerator.Topology.PciBusID] = true
					assert.Equal(t, []uint32{uint32(i)}, slot.Accelerator.PhysicalIndexes)
				}

				assert.True(t, resolved.HasInterfaces())
				assert.Empty(t, resolved.Interfaces)
				assert.True(t, resolved.HasRDMADevices())
				require.Len(t, resolved.RDMADevices, 2)
				assert.Equal(t, device.RDMADeviceOwnerGPUStack, resolved.RDMADevices[0].Owner)
				assert.Equal(t, "0000:10:00.0", resolved.RDMADevices[0].AllocationUnit)
				assert.Equal(t, device.RDMADeviceOwnerExternal, resolved.RDMADevices[1].Owner)
			},
		},
		{
			name: "multi-vendor node with presets list, processes, partitions, and string/bool fault shorthands",
			input: `
presets:
  - preset: amd-rx-7800-xt
    count: 2
manufacturers:
  nvidia:
    preset: nvidia-h100-mig
    count: 1
    fault:
      initError: true
      countError: "count failed"
      totalFailure: true
    processes:
      - id: GPU-912151ec-e9f1-fc26-2897-89f4077de4b9
        memoryReason: unsupported
        processes:
          - pid: 4242
            memoryBytes: 1073741824
            coresPercent: 50
    partitions:
      - deviceId: GPU-912151ec-e9f1-fc26-2897-89f4077de4b9
        id: MIG-667481b3-d832-5bc3-81ee-2f27a4f6cdf2
        profile: 1g.10gb
        placements:
          - {start: 0, length: 1}
        memoryTotalBytes: 10334765056
        memoryUsedBytes: 536870912
        coresReason: unsupported
interfaces:
  - name: eth0
    bus: pci
    pciBusId: "0000:08:00.0"
    pciRootId: "0000:00:01.0"
    up: true
    rdma: true
    rdmaDevice: mlx5_0
    link:
      state: OK
`,
			nodeName: "hetero-node",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				assert.Equal(t, []string{nodefeature.ManufacturerAMD, nodefeature.ManufacturerNVIDIA}, resolved.ManufacturerNames())

				nv := resolved.Manufacturers[nodefeature.ManufacturerNVIDIA]
				require.NotNil(t, nv)
				assert.True(t, nv.Fault.InitError.Enabled)
				assert.Equal(t, 0, nv.Fault.InitError.FailCount)
				assert.True(t, nv.Fault.CountError.Enabled)
				assert.Equal(t, "count failed", nv.Fault.CountError.Message)
				assert.True(t, nv.Fault.TotalFailure.Enabled)

				require.Len(t, nv.Processes, 1)
				proc, ok := nv.Processes["GPU-912151ec-e9f1-fc26-2897-89f4077de4b9"]
				require.True(t, ok)
				assert.Equal(t, device.AcceleratorProcessReasonUnsupported, proc.MemoryReason)
				require.Len(t, proc.Processes, 1)
				assert.Equal(t, uint32(4242), proc.Processes[0].PID)

				require.Len(t, nv.Partitions, 1)
				assert.Equal(t, "MIG-667481b3-d832-5bc3-81ee-2f27a4f6cdf2", nv.Partitions[0].ID)
				assert.Equal(t, device.AcceleratorProcessReasonUnsupported, nv.Partitions[0].CoresReason)

				require.True(t, resolved.HasInterfaces())
				require.Len(t, resolved.Interfaces, 1)
				assert.Equal(t, workercore.DeviceInterfaceLinkStateOK, resolved.Interfaces[0].Link.State)
			},
		},
		{
			name: "multi-node default fallback, targeted node override, and explicit cpu-only empty node",
			input: `
default:
  preset: ascend-910b2
  count: 2
nodes:
  worker-fault:
    preset: ascend-910b2
    count: 2
    cardOverrides:
      1:
        queryState: fatalError
  worker-zero:
    preset: ascend-910b2
    count: 0
  worker-cpu: {}
`,
			nodeName: "worker-unlisted",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				// Unlisted node falls back to default.
				asc := resolved.Manufacturers[nodefeature.ManufacturerAscend]
				require.NotNil(t, asc)
				require.Len(t, asc.Slots, 2)
			},
		},
		{
			name: "multi-node targeted worker-fault override",
			input: `
default:
  preset: ascend-910b2
  count: 2
nodes:
  worker-fault:
    preset: ascend-910b2
    count: 2
    cardOverrides:
      1:
        queryState: fatalError
  worker-zero:
    preset: ascend-910b2
    count: 0
  worker-cpu: {}
`,
			nodeName: "worker-fault",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				asc := resolved.Manufacturers[nodefeature.ManufacturerAscend]
				require.NotNil(t, asc)
				require.Len(t, asc.Slots, 2)
				assert.Equal(t, QueryStateOK, asc.Slots[0].QueryState)
				assert.Equal(t, QueryStateFatalError, asc.Slots[1].QueryState)
			},
		},
		{
			name: "multi-node explicit empty node does not inherit default",
			input: `
default:
  preset: ascend-910b2
  count: 2
nodes:
  worker-cpu: {}
`,
			nodeName: "worker-cpu",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				assert.Empty(t, resolved.Manufacturers)
				assert.False(t, resolved.HasInterfaces())
				assert.False(t, resolved.HasRDMADevices())
			},
		},
		{
			name: "explicit custom groups without preset",
			input: `
groups:
  - manufacturer: cambricon
    name: MLU370-X8
    memory: 24576
    cores: 16
    driverVersion: "5.10"
    runtimeVersion: "1.15"
    accelerators:
      - id: MLU-custom-0
        index: 0
        logicalSliced:
          count: 16
        topology:
          pciBusId: "0000:01:00.0"
          pciRootId: "0000:00:01.0"
          pciClass: "1200"
          numaAffinity: "0"
`,
			nodeName: "custom-node",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				cam := resolved.Manufacturers[nodefeature.ManufacturerCambricon]
				require.NotNil(t, cam)
				groups := cam.BuildGroups()
				require.Len(t, groups, 1)
				assert.Equal(t, "mlu370-x8", groups[0].ID)
				assert.Equal(t, int32(16), groups[0].AcceleratorSlicedDetail.Logical.Count)
			},
		},
		{
			name: "multi-vendor count expansion physicalIndexes synthesis, object fault forms, and metrics group",
			input: `
manufacturers:
  amd:
    preset: amd-mi300x
    count: 4
    fault:
      initError: "amdsmi missing"
      countError:
        enabled: true
        message: "count query failed"
      totalFailure:
        enabled: false
  ascend:
    preset: ascend-310p3
    count: 4
  thead:
    preset: thead-ppu-mig
    count: 10
  hygon:
    preset: hygon-k100-ai
    count: 0
`,
			nodeName: "scale-node",
			check: func(t *testing.T, resolved *ResolvedNodeSpec) {
				amdSpec := resolved.Manufacturers[nodefeature.ManufacturerAMD]
				require.NotNil(t, amdSpec)
				assert.True(t, amdSpec.Fault.InitError.Enabled)
				assert.Equal(t, "amdsmi missing", amdSpec.Fault.InitError.Message)
				assert.True(t, amdSpec.Fault.CountError.Enabled)
				assert.Equal(t, "count query failed", amdSpec.Fault.CountError.Message)
				assert.False(t, amdSpec.Fault.TotalFailure.Enabled)
				require.Len(t, amdSpec.Slots, 4)
				assert.Equal(t, []uint32{3, 131}, amdSpec.Slots[3].Accelerator.PhysicalIndexes)

				now := time.Now()
				mg := amdSpec.BuildMetricsGroup(now)
				assert.Equal(t, nodefeature.ManufacturerAMD, mg.Manufacturer)
				assert.Equal(t, now, mg.Timestamp)
				require.Len(t, mg.Accelerators, 4)

				ascSpec := resolved.Manufacturers[nodefeature.ManufacturerAscend]
				require.Len(t, ascSpec.Slots, 4)
				assert.Equal(t, []uint32{3, 3, 0}, ascSpec.Slots[3].Accelerator.PhysicalIndexes)

				thSpec := resolved.Manufacturers[nodefeature.ManufacturerTHead]
				require.Len(t, thSpec.Slots, 10)
				assert.Equal(t, []uint32{10}, thSpec.Slots[9].Accelerator.PhysicalIndexes)

				hySpec := resolved.Manufacturers[nodefeature.ManufacturerHygon]
				assert.Empty(t, hySpec.Slots)
				assert.Nil(t, hySpec.BuildGroups())
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.input))
			require.NoError(t, err)
			resolved := doc.ResolveNode(tc.nodeName)
			require.NotNil(t, resolved)
			tc.check(t, resolved)
		})
	}
}

func TestParseValidationErrors(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "empty document",
			input:   "   \n  ",
			wantErr: "empty",
		},
		{
			name: "divergent physical profile geometry within one group rejected",
			input: `
preset: nvidia-h100-mig
count: 2
cardOverrides:
  1:
    physicalSliced:
      profiles:
        - name: 1g.10gb
          memoryMib: 12000
          computeSlices: 1
          memorySlices: 1
          count: 7
          placements:
            - {start: 0, length: 1}
`,
			wantErr: "diverges",
		},
		{
			name:    "unknown field rejected",
			input:   "preset: nvidia-h100\nunknownField: true\n",
			wantErr: "unknownField",
		},
		{
			name: "mixed flat and multi-node fields rejected",
			input: `
preset: nvidia-h100
default:
  preset: amd-mi300x
`,
			wantErr: "cannot mix",
		},
		{
			name:    "unknown manufacturer in groups",
			input:   "groups:\n  - manufacturer: voodoo\n    name: V1\n    memory: 1024\n    accelerators:\n      - id: GPU-0\n        index: 0\n",
			wantErr: "manufacturer",
		},
		{
			name:    "unknown manufacturer key in manufacturers map",
			input:   "manufacturers:\n  3dfx:\n    preset: nvidia-h100\n",
			wantErr: "manufacturer",
		},
		{
			name:    "unknown preset name",
			input:   "preset: nvidia-rtx-9999\n",
			wantErr: "unknown preset",
		},
		{
			name:    "preset manufacturer mismatch",
			input:   "manufacturers:\n  amd:\n    preset: nvidia-h100\n",
			wantErr: "manufacturer",
		},
		{
			name:    "negative count",
			input:   "preset: nvidia-h100\ncount: -1\n",
			wantErr: "count",
		},
		{
			name:    "negative initError failCount",
			input:   "preset: nvidia-h100\nfault:\n  initError:\n    failCount: -1\n",
			wantErr: "failCount",
		},
		{
			name: "negative card index",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - id: GPU-0
        index: -1
`,
			wantErr: "index",
		},
		{
			name: "duplicate card index within manufacturer",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
      - id: GPU-1
        index: 0
`,
			wantErr: "duplicate",
		},
		{
			name: "duplicate accelerator id",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - id: GPU-dup
        index: 0
      - id: GPU-dup
        index: 1
`,
			wantErr: "duplicate",
		},
		{
			name: "missing group name",
			input: `
groups:
  - manufacturer: nvidia
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
`,
			wantErr: "name",
		},
		{
			name: "zero group memory",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 0
    accelerators:
      - id: GPU-0
        index: 0
`,
			wantErr: "memory",
		},
		{
			name: "missing accelerator id",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - index: 0
`,
			wantErr: "id",
		},
		{
			name:    "invalid queryState",
			input:   "preset: nvidia-h100\ncardOverrides:\n  0:\n    queryState: broken\n",
			wantErr: "queryState",
		},
		{
			name:    "cardOverrides targeting out-of-range index",
			input:   "preset: nvidia-h100\ncount: 2\ncardOverrides:\n  5:\n    unhealthy: true\n",
			wantErr: "cardOverrides",
		},
		{
			name: "simultaneous logical and physical slicing on one card",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
        logicalSliced:
          count: 16
        physicalSliced:
          profiles:
            - name: 1g.10gb
              memoryMib: 9856
              computeSlices: 1
              memorySlices: 1
              count: 7
              placements:
                - {start: 0, length: 1}
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "partition profile on non-partitionable manufacturer",
			input: `
groups:
  - manufacturer: amd
    name: AMD Instinct MI300X OAM
    memory: 196592
    accelerators:
      - id: GPU-0
        index: 0
        physicalSliced:
          profiles:
            - name: 1g.10gb
              memoryMib: 9856
              computeSlices: 1
              memorySlices: 1
              count: 7
              placements:
                - {start: 0, length: 1}
`,
			wantErr: "partition",
		},
		{
			name: "partition profile with empty placements",
			input: `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
        physicalSliced:
          profiles:
            - name: 1g.10gb
              memoryMib: 9856
              computeSlices: 1
              memorySlices: 1
              count: 7
`,
			wantErr: "placement",
		},
		{
			name:    "metrics utilization above 100",
			input:   "preset: nvidia-h100\ncardOverrides:\n  0:\n    metrics:\n      coresUtilization: 105\n",
			wantErr: "Utilization",
		},
		{
			name: "invalid process reason",
			input: `
preset: nvidia-h100
processes:
  - id: GPU-78073210-c690-bb74-d538-87bba36db45d
    memoryReason: bogus_reason
`,
			wantErr: "memoryReason",
		},
		{
			name: "invalid rdma owner",
			input: `
preset: nvidia-h100
rdmaDevices:
  - name: mlx5_0
    owner: SomebodyElse
`,
			wantErr: "owner",
		},
		{
			name: "invalid interface link state",
			input: `
preset: nvidia-h100
interfaces:
  - name: eth0
    rdma: true
    rdmaDevice: mlx5_0
    link:
      state: HalfWorking
`,
			wantErr: "state",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
