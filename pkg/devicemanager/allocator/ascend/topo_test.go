package ascend

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	productascend "gpustack.ai/gpustack/pkg/devicemanager/product/ascend"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// fakeProductDriver answers the two node-level reads productascend.Resolver makes. The rule it
// applies to them is that package's and is tested there; what this file establishes is what the
// allocator does with the answer.
type fakeProductDriver struct {
	mainboardID  uint32
	superPodType uint32
	mainboardErr error
	superPodErr  error
}

func (d *fakeProductDriver) MainboardID(_, _ int32) (uint32, error) {
	if d.mainboardErr != nil {
		return 0, d.mainboardErr
	}
	return d.mainboardID, nil
}

func (d *fakeProductDriver) SuperPodType(_, _ int32) (uint32, error) {
	if d.superPodErr != nil {
		return 0, d.superPodErr
	}
	return d.superPodType, nil
}

// newFakeProductResolver builds the resolver a server fixture needs. Its default answers a product
// with a topology file, so a test that is not about the topology env is unaffected by it.
func newFakeProductResolver() *productascend.Resolver {
	return productascend.NewResolver(&fakeProductDriver{})
}

// The vendor's own product-to-file table, restated: every shape this allocator knows names exactly
// one of the seven files the driver package ships, and no two shapes name the same one.
func TestHcclTopoFilePaths(t *testing.T) {
	const topoDir = "/usr/local/Ascend/driver/topo/950/"

	cases := []struct {
		product productascend.Type
		want    string
	}{
		{product: productascend.TypeServer8P, want: topoDir + "atlas_850_1.json"},
		{product: productascend.TypePod1D, want: topoDir + "atlas_950_1.json"},
		{product: productascend.TypePod2D, want: topoDir + "atlas_950_2.json"},
		{product: productascend.TypeServer16P, want: topoDir + "atlas_850_2.json"},
		{product: productascend.TypeServer32P, want: topoDir + "atlas_850_3.json"},
		{product: productascend.TypeCard1P, want: topoDir + "atlas_350_1.json"},
		{product: productascend.TypeCard4P, want: topoDir + "atlas_350_3.json"},
	}
	for _, c := range cases {
		t.Run(string(c.product), func(t *testing.T) {
			assert.Equal(t, c.want, hcclTopoFilePaths[c.product])
		})
	}
	assert.Len(t, hcclTopoFilePaths, len(cases), "every shape in the table is pinned above")
}

// The env only exists on A5, and only when the node answered. Every other outcome leaves the
// allocation intact and simply carries no topology hint.
func TestGetContainerAllocateResponse_HcclTopoFilePath(t *testing.T) {
	cases := []struct {
		name   string
		family string
		driver *fakeProductDriver
		want   string // "" == the response must carry no topology env at all
	}{
		{
			name:   "950 carries the file its super pod names",
			family: family950,
			driver: &fakeProductDriver{superPodType: 2},
			want:   "/usr/local/Ascend/driver/topo/950/atlas_950_2.json",
		},
		{
			name:   "950 carries the file its mainboard names",
			family: family950,
			driver: &fakeProductDriver{mainboardID: 0x6c},
			want:   "/usr/local/Ascend/driver/topo/950/atlas_350_3.json",
		},
		{
			name:   "910B carries none",
			family: "910B",
			driver: &fakeProductDriver{superPodType: 2},
		},
		{
			name:   "a product with no file still allocates",
			family: family950,
			driver: &fakeProductDriver{superPodType: 99},
		},
		{
			name:   "an unreadable super pod type still allocates",
			family: family950,
			driver: &fakeProductDriver{superPodErr: errors.New("dcmi refused")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: workercore.DeviceAllocationModeExclusive,
				},
				product: productascend.NewResolver(c.driver),
			}
			devs := ascendDevicesFixture()
			devs.Spec.Groups[0].Family = c.family

			pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-topo")}}
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs,
				map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
			require.NoError(t, err)

			got, carried := resp.Envs[hcclTopoFilePathEnv]
			require.Equal(t, c.want != "", carried, "the response carries a topology env")
			if c.want != "" {
				assert.Equal(t, c.want, got)
			}
			// Whatever happened to the topology hint, the injection the container is actually
			// started with is intact.
			assert.NotEmpty(t, resp.Envs["ASCEND_VISIBLE_DEVICES"])
		})
	}
}

// The node is addressed by the dcmi card and device the detector recorded, not by the accelerator's
// physical id: reading the wrong slots would query another device on a host where they differ.
func TestGetContainerAllocateResponse_HcclTopoAddressesTheDcmiDevice(t *testing.T) {
	d := &addressRecordingProductDriver{fakeProductDriver: fakeProductDriver{superPodType: 1}}
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
		product: productascend.NewResolver(d),
	}
	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Family = family950
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{7, 3, 0}

	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-topo-address")}}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)

	require.NotEmpty(t, d.addresses, "the node was read")
	for _, addr := range d.addresses {
		assert.Equal(t, [2]int32{3, 0}, addr, "the dcmi card and device, not the physical id")
	}
}

// addressRecordingProductDriver records the (card, device) pair each read was addressed to.
type addressRecordingProductDriver struct {
	fakeProductDriver
	addresses [][2]int32
}

func (d *addressRecordingProductDriver) MainboardID(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeProductDriver.MainboardID(cardID, deviceID)
}

func (d *addressRecordingProductDriver) SuperPodType(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeProductDriver.SuperPodType(cardID, deviceID)
}
