package ascendproduct

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDriver answers the two node-level reads the resolver makes, and counts them, so a test can
// establish both what was resolved and how often the node was asked.
type fakeDriver struct {
	mainboardID  uint32
	superPodType uint32
	mainboardErr error
	superPodErr  error

	mainboardCalls int
	superPodCalls  int
}

func (d *fakeDriver) MainboardID(_, _ int32) (uint32, error) {
	d.mainboardCalls++
	if d.mainboardErr != nil {
		return 0, d.mainboardErr
	}
	return d.mainboardID, nil
}

func (d *fakeDriver) SuperPodType(_, _ int32) (uint32, error) {
	d.superPodCalls++
	if d.superPodErr != nil {
		return 0, d.superPodErr
	}
	return d.superPodType, nil
}

// The vendor's own rule, restated: a super pod names its own shape, except on the two inference
// cards, which are recognized by the mainboard the chip is mounted on and never reach the super-pod
// query at all.
func TestResolver_Resolve(t *testing.T) {
	cases := []struct {
		name         string
		mainboardID  uint32
		superPodType uint32
		want         Product
		// wantSuperPodRead separates the two ways a product is established: a card recognized by its
		// mainboard must not also be asked what super pod it is in.
		wantSuperPodRead bool
	}{
		{name: "8p server", superPodType: 0, want: Product{Type: TypeServer8P, Code: 0}, wantSuperPodRead: true},
		{name: "1d pod", superPodType: 1, want: Product{Type: TypePod1D, Code: 1}, wantSuperPodRead: true},
		{name: "2d pod", superPodType: 2, want: Product{Type: TypePod2D, Code: 2}, wantSuperPodRead: true},
		{name: "16p server", superPodType: 3, want: Product{Type: TypeServer16P, Code: 3}, wantSuperPodRead: true},
		{name: "32p server", superPodType: 4, want: Product{Type: TypeServer32P, Code: 4}, wantSuperPodRead: true},
		{name: "1p card", mainboardID: 0x68, want: Product{Type: TypeCard1P, Code: 5}},
		{name: "4p card", mainboardID: 0x6c, want: Product{Type: TypeCard4P, Code: 6}},
		// A number this package has no name for is an answer, not a failure: the code comes back so
		// the caller can say which product it was, and there is nothing left to retry.
		{name: "unknown product code", superPodType: 99, want: Product{Type: "", Code: 99}, wantSuperPodRead: true},
		// A training baseboard is not one of the two card mainboards, so it falls through to the
		// super pod like every other product.
		{
			name: "training baseboard falls through", mainboardID: 0x44, superPodType: 1,
			want: Product{Type: TypePod1D, Code: 1}, wantSuperPodRead: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDriver{mainboardID: c.mainboardID, superPodType: c.superPodType}
			r := NewResolver(d)

			got, err := r.Resolve(3, 0)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.wantSuperPodRead, d.superPodCalls == 1, "the super pod was consulted")

			// The node cannot change shape while the device-manager runs, so a second call reads
			// nothing and answers the same.
			got2, err := r.Resolve(3, 0)
			require.NoError(t, err)
			assert.Equal(t, got, got2)
			assert.Equal(t, 1, d.mainboardCalls, "the node is read once")
			assert.LessOrEqual(t, d.superPodCalls, 1, "and so is the super pod")
		})
	}
}

// A read that failed says nothing about the node, so it is not remembered: the next caller asks
// again rather than carrying the failure for the lifetime of the process.
func TestResolver_FailedReadIsNotRemembered(t *testing.T) {
	cases := []struct {
		name   string
		driver *fakeDriver
	}{
		{name: "mainboard unreadable", driver: &fakeDriver{mainboardErr: errors.New("dcmi refused")}},
		{name: "super pod unreadable", driver: &fakeDriver{superPodErr: errors.New("dcmi refused")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewResolver(c.driver)

			_, err := r.Resolve(3, 0)
			require.Error(t, err)

			_, err = r.Resolve(3, 0)
			require.Error(t, err)
			assert.Equal(t, 2, c.driver.mainboardCalls, "a failed read is asked again")
		})
	}
}

// The node is addressed by the pair the caller passed, not by some number this package chose.
func TestResolver_AddressesTheCallersDevice(t *testing.T) {
	d := &addressRecordingDriver{fakeDriver: fakeDriver{superPodType: 1}}
	r := NewResolver(d)

	_, err := r.Resolve(3, 0)
	require.NoError(t, err)

	require.NotEmpty(t, d.addresses, "the node was read")
	for _, addr := range d.addresses {
		assert.Equal(t, [2]int32{3, 0}, addr)
	}
}

// addressRecordingDriver records the (card, device) pair each read was addressed to.
type addressRecordingDriver struct {
	fakeDriver
	addresses [][2]int32
}

func (d *addressRecordingDriver) MainboardID(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeDriver.MainboardID(cardID, deviceID)
}

func (d *addressRecordingDriver) SuperPodType(cardID, deviceID int32) (uint32, error) {
	d.addresses = append(d.addresses, [2]int32{cardID, deviceID})
	return d.fakeDriver.SuperPodType(cardID, deviceID)
}
