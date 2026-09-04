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
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// fakeTopoDriver answers the two node-level reads the topology resolver makes, and counts them, so
// a test can establish both what was resolved and how often the node was asked.
type fakeTopoDriver struct {
	mainboardID  uint32
	superPodType uint32
	mainboardErr error
	superPodErr  error

	mainboardCalls int
	superPodCalls  int
}

func (d *fakeTopoDriver) MainboardID(_, _ int32) (uint32, error) {
	d.mainboardCalls++
	if d.mainboardErr != nil {
		return 0, d.mainboardErr
	}
	return d.mainboardID, nil
}

func (d *fakeTopoDriver) SuperPodType(_, _ int32) (uint32, error) {
	d.superPodCalls++
	if d.superPodErr != nil {
		return 0, d.superPodErr
	}
	return d.superPodType, nil
}

// The vendor's own product-type-to-file table, restated: a super pod names its own shape, except on
// the two inference cards, which are recognized by the mainboard the chip is mounted on and never
// reach the super-pod query at all.
func TestHcclTopoResolver_Resolve(t *testing.T) {
	const topoDir = "/usr/local/Ascend/driver/topo/950/"

	cases := []struct {
		name         string
		mainboardID  uint32
		superPodType uint32
		want         string
		// wantSuperPodRead separates the two ways a product type is established: a card recognized
		// by its mainboard must not also be asked what super pod it is in.
		wantSuperPodRead bool
	}{
		{name: "8p server", superPodType: 0, want: topoDir + "atlas_850_1.json", wantSuperPodRead: true},
		{name: "1d pod", superPodType: 1, want: topoDir + "atlas_950_1.json", wantSuperPodRead: true},
		{name: "2d pod", superPodType: 2, want: topoDir + "atlas_950_2.json", wantSuperPodRead: true},
		{name: "16p server", superPodType: 3, want: topoDir + "atlas_850_2.json", wantSuperPodRead: true},
		{name: "32p server", superPodType: 4, want: topoDir + "atlas_850_3.json", wantSuperPodRead: true},
		{name: "1p card", mainboardID: 0x68, want: topoDir + "atlas_350_1.json"},
		{name: "4p card", mainboardID: 0x6c, want: topoDir + "atlas_350_3.json"},
		// A product type the vendor ships no file for is an answer, not a failure: there is nothing
		// to inject and nothing left to retry.
		{name: "unknown product type", superPodType: 99, want: "", wantSuperPodRead: true},
		// A training baseboard is not one of the two card mainboards, so it falls through to the
		// super pod like every other product.
		{
			name: "training baseboard falls through", mainboardID: 0x44, superPodType: 1,
			want: topoDir + "atlas_950_1.json", wantSuperPodRead: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeTopoDriver{mainboardID: c.mainboardID, superPodType: c.superPodType}
			r := newHcclTopoResolver(d)

			path, productType, err := r.Resolve(3, 0)
			require.NoError(t, err)
			assert.Equal(t, c.want, path)
			assert.Equal(t, c.wantSuperPodRead, d.superPodCalls == 1, "the super pod was consulted")

			// The node cannot change shape while the device-manager runs, so a second allocation
			// reads nothing and answers the same.
			path2, productType2, err := r.Resolve(3, 0)
			require.NoError(t, err)
			assert.Equal(t, path, path2)
			assert.Equal(t, productType, productType2)
			assert.Equal(t, 1, d.mainboardCalls, "the node is read once")
			assert.LessOrEqual(t, d.superPodCalls, 1, "and so is the super pod")
		})
	}
}

// A read that failed says nothing about the node, so it is not remembered: the next allocation asks
// again rather than carrying the failure for the lifetime of the process.
func TestHcclTopoResolver_FailedReadIsNotRemembered(t *testing.T) {
	cases := []struct {
		name   string
		driver *fakeTopoDriver
	}{
		{name: "mainboard unreadable", driver: &fakeTopoDriver{mainboardErr: errors.New("dcmi refused")}},
		{name: "super pod unreadable", driver: &fakeTopoDriver{superPodErr: errors.New("dcmi refused")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHcclTopoResolver(c.driver)

			_, _, err := r.Resolve(3, 0)
			require.Error(t, err)

			_, _, err = r.Resolve(3, 0)
			require.Error(t, err)
			assert.Equal(t, 2, c.driver.mainboardCalls, "a failed read is asked again")
		})
	}
}

// The env only exists on A5, and only when the node answered. Every other outcome leaves the
// allocation intact and simply carries no topology hint.
func TestGetContainerAllocateResponse_HcclTopoFilePath(t *testing.T) {
	cases := []struct {
		name   string
		family string
		driver *fakeTopoDriver
		want   string // "" == the response must carry no topology env at all
	}{
		{
			name:   "950 carries the file its super pod names",
			family: "950",
			driver: &fakeTopoDriver{superPodType: 2},
			want:   "/usr/local/Ascend/driver/topo/950/atlas_950_2.json",
		},
		{
			name:   "950 carries the file its mainboard names",
			family: "950",
			driver: &fakeTopoDriver{mainboardID: 0x6c},
			want:   "/usr/local/Ascend/driver/topo/950/atlas_350_3.json",
		},
		{
			name:   "910B carries none",
			family: "910B",
			driver: &fakeTopoDriver{superPodType: 2},
		},
		{
			name:   "an unreadable super pod type still allocates",
			family: "950",
			driver: &fakeTopoDriver{superPodErr: errors.New("dcmi refused")},
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
				topo: newHcclTopoResolver(c.driver),
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
	d := &addressRecordingTopoDriver{fakeTopoDriver: fakeTopoDriver{superPodType: 1}}
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
		topo: newHcclTopoResolver(d),
	}
	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Family = "950"
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

// addressRecordingTopoDriver records the (card, device) pair each read was addressed to.
type addressRecordingTopoDriver struct {
	fakeTopoDriver
	addresses [][2]int32
}

func (d *addressRecordingTopoDriver) MainboardID(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeTopoDriver.MainboardID(cardID, deviceID)
}

func (d *addressRecordingTopoDriver) SuperPodType(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeTopoDriver.SuperPodType(cardID, deviceID)
}
