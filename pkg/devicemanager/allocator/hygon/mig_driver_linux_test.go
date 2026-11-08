package hygon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/dmi"
)

func TestMigDriverVendorErrors(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		ret       dmi.Return
		inUse     bool
	}{
		{name: "device lookup unavailable", operation: "device", ret: dmi.ERROR_LIBRARY_NOT_FOUND},
		{name: "inventory unavailable", operation: "list", ret: dmi.ERROR_LIBRARY_NOT_FOUND},
		{name: "partition in use", operation: "destroy", ret: dmi.ERROR_IN_USE, inUse: true},
		{name: "teardown denied", operation: "destroy", ret: dmi.ERROR_NO_PERMISSION},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			driver := &dmiMigDriver{initRet: tc.ret}
			var err error
			switch tc.operation {
			case "device":
				_, err = driver.device("0000:01:00.0")
			case "list":
				_, err = driver.ListInstances()
			case "destroy":
				err = migDestroyError("0000:01:00.0", "gpu instance", tc.ret)
			default:
				t.Fatalf("unknown operation %q", tc.operation)
			}
			require.ErrorIs(t, err, tc.ret)
			if tc.inUse {
				require.ErrorIs(t, err, errInstanceInUse)
			} else {
				require.NotErrorIs(t, err, errInstanceInUse)
			}
		})
	}
}
