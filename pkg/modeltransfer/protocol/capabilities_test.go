package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validCapabilities() BackendCapabilities {
	return BackendCapabilities{
		Requested:         []string{"ucx", "tcp"},
		Applied:           "tcp",
		Effective:         "tcp",
		EffectiveObserved: true,
		Rejected:          []RejectedOption{{Option: "ucx", Reason: "no verbs devices"}},
		Formats:           []string{"safetensors"},
		MemoryTypes:       []string{"host"},
		MaxRequestBytes:   1 << 30,
		AlignmentBytes:    4096,
		TensorOwnership:   OwnershipOwned,
		DeviceVisibility:  []string{"host"},
		Transport:         TransportSelection{Control: "tcp://10.0.0.1:9100", Payload: []string{"tcp://10.0.0.1:9101"}},
	}
}

func TestBackendCapabilitiesValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*BackendCapabilities)
		wantErr bool
	}{
		{"valid", func(*BackendCapabilities) {}, false},
		{"rejected option applied", func(c *BackendCapabilities) { c.Applied = "ucx" }, true},
		{"rejected option not requested", func(c *BackendCapabilities) {
			c.Rejected = []RejectedOption{{Option: "sm", Reason: "not requested"}}
		}, true},
		{"applied not requested", func(c *BackendCapabilities) { c.Applied = "sm" }, true},
		{"unobserved effective named", func(c *BackendCapabilities) {
			c.EffectiveObserved = false
		}, true},
		{"observed effective unknown", func(c *BackendCapabilities) {
			c.Effective = "unknown"
		}, true},
		{"empty requested", func(c *BackendCapabilities) { c.Requested = nil }, true},
		{"zero max request bytes", func(c *BackendCapabilities) { c.MaxRequestBytes = 0 }, true},
		{"alignment not power of two", func(c *BackendCapabilities) { c.AlignmentBytes = 3 }, true},
		{"zero alignment", func(c *BackendCapabilities) { c.AlignmentBytes = 0 }, true},
		{"empty formats", func(c *BackendCapabilities) { c.Formats = nil }, true},
		{"empty memory types", func(c *BackendCapabilities) { c.MemoryTypes = nil }, true},
		{"empty device visibility", func(c *BackendCapabilities) { c.DeviceVisibility = nil }, true},
		{"unknown ownership", func(c *BackendCapabilities) { c.TensorOwnership = "shared" }, true},
		{"empty control path", func(c *BackendCapabilities) {
			c.Transport = TransportSelection{Control: "", Payload: []string{"tcp://data"}}
		}, true},
		{"empty payload path", func(c *BackendCapabilities) {
			c.Transport = TransportSelection{Control: "tcp://ctl"}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := validCapabilities()
			tc.mutate(&caps)
			err := caps.Validate()
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, KindInvalidCapabilities, KindOf(err))
		})
	}
}

func TestBackendCapabilitiesUnknownEffectiveValidWhenUnobserved(t *testing.T) {
	caps := validCapabilities()
	caps.EffectiveObserved = false
	caps.Effective = "unknown"
	require.NoError(t, caps.Validate())
}

func TestLocalBackendCapabilities(t *testing.T) {
	caps := LocalBackendCapabilities()
	require.NoError(t, caps.Validate())
	assert.Equal(t, []string{"local"}, caps.Requested)
	assert.Equal(t, "local", caps.Applied)
	assert.Equal(t, "local", caps.Effective)
	assert.True(t, caps.EffectiveObserved)
	assert.Empty(t, caps.Rejected)
	assert.Equal(t, []string{"safetensors"}, caps.Formats)
	assert.Equal(t, []string{"host"}, caps.MemoryTypes)
	assert.Equal(t, OwnershipOwned, caps.TensorOwnership)
	assert.Equal(t, []string{"host"}, caps.DeviceVisibility)
}
