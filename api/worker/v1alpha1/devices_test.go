package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeviceAllocationModeString pins the enum's wire values alongside their names. The mode is a
// protobuf varint persisted in the Devices status and in each Pod's allocation annotation, so
// inserting a member renumbers every member after it and silently reinterprets records written by
// an earlier build. Nothing in the type system catches that, which is why the numbers are asserted
// literally rather than derived from the constants.
func TestDeviceAllocationModeString(t *testing.T) {
	cases := []struct {
		name  string
		mode  DeviceAllocationMode
		value uint32
		want  string
	}{
		{name: "none", mode: DeviceAllocationModeNone, value: 0, want: "None"},
		{name: "exclusive", mode: DeviceAllocationModeExclusive, value: 1, want: "Exclusive"},
		{name: "shared", mode: DeviceAllocationModeShared, value: 2, want: "Shared"},
		{name: "sliced", mode: DeviceAllocationModeSliced, value: 3, want: "Sliced"},
		{name: "partitioned", mode: DeviceAllocationModePartitioned, value: 4, want: "Partitioned"},
		{name: "visibility", mode: DeviceAllocationModeVisibility, value: 5, want: "Visibility"},
	}

	seen := make(map[string]DeviceAllocationMode, len(cases))
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.value, uint32(c.mode), "the persisted wire value must not drift")
			assert.Equal(t, c.want, c.mode.String())
		})
		seen[c.want] = c.mode
	}
	assert.Len(t, seen, len(cases), "every mode must render a distinct name")

	// An unknown value degrades to None rather than to a neighbour's name, so a record written
	// by a build that knew more modes is not mistaken for one this build understands.
	assert.Equal(t, "None", DeviceAllocationMode(99).String())
}
