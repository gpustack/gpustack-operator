package mock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

func TestDetectInterfacesAndRDMADevices(t *testing.T) {
	dir := t.TempDir()
	specYAML := `
default:
  preset: nvidia-h100
  count: 1
nodes:
  node-empty-net:
    preset: nvidia-h100
    count: 1
    interfaces: []
    rdmaDevices: []
  node-rdma-ok:
    preset: nvidia-h100
    count: 1
    interfaces:
      - name: eth0
        up: true
        mtu: 9000
        rdma: true
        pciBusId: "0000:1a:00.0"
        rdmaDevice: mlx5_0
        link:
          state: OK
    rdmaDevices:
      - name: mlx5_0
        driver: mlx5_core
        pciBusId: "0000:1a:00.0"
        numaAffinity: "0"
        ports:
          - number: 1
            netDevs: ["eth0"]
            linkLayer: Ethernet
            state: ACTIVE
            physicalState: LinkUp
            rate: "200 Gb/sec (4X NDR)"
  node-net-err:
    preset: nvidia-h100
    count: 1
    interfacesError: "sysfs net unreadable"
    rdmaError: "infiniband class unreadable"
`
	specPath := writeSpecFile(t, dir, specYAML)
	store, err := mockspec.NewStore(specPath)
	require.NoError(t, err)

	t.Run("nil and uninitialized stores", func(t *testing.T) {
		ifaces, ok, err := DetectInterfaces("any", nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Nil(t, ifaces)

		rdma, ok, err := DetectRDMADevices("any", nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Nil(t, rdma)

		var uninit mockspec.Store
		_, ok, err = DetectInterfaces("any", &uninit)
		require.Error(t, err)
		assert.True(t, ok)

		_, ok, err = DetectRDMADevices("any", &uninit)
		require.Error(t, err)
		assert.True(t, ok)
	})

	testCases := []struct {
		name             string
		nodeName         string
		wantIfacesConfig bool
		wantIfacesLen    int
		wantIfacesErr    string
		wantRDMAConfig   bool
		wantRDMALen      int
		wantRDMAErr      string
	}{
		{
			name:             "default node omits interfaces and rdmaDevices",
			nodeName:         "node-default",
			wantIfacesConfig: false,
			wantRDMAConfig:   false,
		},
		{
			name:             "explicit empty lists report configured true with zero items",
			nodeName:         "node-empty-net",
			wantIfacesConfig: true,
			wantIfacesLen:    0,
			wantRDMAConfig:   true,
			wantRDMALen:      0,
		},
		{
			name:             "configured interfaces and rdmaDevices returned with isolated copies",
			nodeName:         "node-rdma-ok",
			wantIfacesConfig: true,
			wantIfacesLen:    1,
			wantRDMAConfig:   true,
			wantRDMALen:      1,
		},
		{
			name:             "configured interfacesError and rdmaError propagate errors",
			nodeName:         "node-net-err",
			wantIfacesConfig: true,
			wantIfacesErr:    "sysfs net unreadable",
			wantRDMAConfig:   true,
			wantRDMAErr:      "infiniband class unreadable",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ifaces, ok, err := DetectInterfaces(tc.nodeName, store)
			assert.Equal(t, tc.wantIfacesConfig, ok)
			if tc.wantIfacesErr != "" {
				require.ErrorContains(t, err, tc.wantIfacesErr)
			} else {
				require.NoError(t, err)
				if tc.wantIfacesConfig {
					require.NotNil(t, ifaces)
					assert.Len(t, ifaces, tc.wantIfacesLen)
					if tc.wantIfacesLen > 0 {
						assert.Equal(t, "eth0", ifaces[0].Name)
						require.NotNil(t, ifaces[0].Link)
						assert.Equal(t, workercore.DeviceInterfaceLinkStateOK, ifaces[0].Link.State)
					}
				} else {
					assert.Nil(t, ifaces)
				}
			}

			rdmaDevs, ok, err := DetectRDMADevices(tc.nodeName, store)
			assert.Equal(t, tc.wantRDMAConfig, ok)
			if tc.wantRDMAErr != "" {
				require.ErrorContains(t, err, tc.wantRDMAErr)
			} else {
				require.NoError(t, err)
				if tc.wantRDMAConfig {
					require.NotNil(t, rdmaDevs)
					assert.Len(t, rdmaDevs, tc.wantRDMALen)
					if tc.wantRDMALen > 0 {
						assert.Equal(t, "mlx5_0", rdmaDevs[0].Name)
						assert.Equal(t, device.RDMADeviceOwnerGPUStack, rdmaDevs[0].Owner)
						assert.Equal(t, "0000:1a:00.0", rdmaDevs[0].AllocationUnit)
						require.Len(t, rdmaDevs[0].Ports, 1)
						assert.Equal(t, uint32(1), rdmaDevs[0].Ports[0].Number)
					}
				} else {
					assert.Nil(t, rdmaDevs)
				}
			}
		})
	}

	t.Run("malformed hot-reload retains last valid interfaces and rdmaDevices", func(t *testing.T) {
		updateSpecFile(t, specPath, `not: [valid`)

		ifaces, ok, err := DetectInterfaces("node-rdma-ok", store)
		require.NoError(t, err)
		assert.True(t, ok)
		require.Len(t, ifaces, 1)

		rdmaDevs, ok, err := DetectRDMADevices("node-rdma-ok", store)
		require.NoError(t, err)
		assert.True(t, ok)
		require.Len(t, rdmaDevs, 1)
	})
}
