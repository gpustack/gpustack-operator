package v1alpha1

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// reservedProtobufNumbers records, per message type, the protobuf field numbers that were
// removed and must never be reused. go-to-protobuf regenerates generated.proto without any
// `reserved` declaration (a hand-added one is clobbered on the next `make generate`), so this
// test — not the .proto — is the durable guard against silent field-number reuse.
//
// The list grows as fields are removed by later tasks of this spec: DevicesGroup 11
// (AcceleratorsFeature) is deferred one release; InstanceTypeSpec 3/4/5/8/9,
// InstanceTypeAccelerator 6, and v1 InstanceTypeFlavorSpec 8 are added here as those fields
// are removed.
var reservedProtobufNumbers = []struct {
	typ      reflect.Type
	reserved []int
}{
	{reflect.TypeOf(Accelerator{}), []int{5}},
	{reflect.TypeOf(InstanceTypeAccelerator{}), []int{4}},
}

func TestReservedProtobufNumbersUnused(t *testing.T) {
	for _, tc := range reservedProtobufNumbers {
		used := usedProtobufNumbers(tc.typ)
		for _, n := range tc.reserved {
			if field, ok := used[n]; ok {
				t.Errorf("%s: protobuf field number %d is reserved but used by field %q",
					tc.typ.Name(), n, field)
			}
		}
	}
}

// usedProtobufNumbers maps each live field's protobuf number to its Go field name. The tag
// format is "<wire>,<number>,<opts>,name=<name>", so the number is the second comma element.
func usedProtobufNumbers(typ reflect.Type) map[int]string {
	used := make(map[int]string, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		parts := strings.Split(f.Tag.Get("protobuf"), ",")
		if len(parts) < 2 {
			continue
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		used[n] = f.Name
	}
	return used
}
