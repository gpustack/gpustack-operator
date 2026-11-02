package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	kubemeta "gpustack.ai/gpustack/pkg/kubemeta"
)

// devicesObjectSchema returns the schema of one path under the Devices CRD, walking each
// named step; a "[]" step descends into the list's items. It exists so the pins below name
// the contract by its path rather than by walking generated structures by hand at every
// assertion.
func devicesObjectSchema(t *testing.T, steps ...string) extension.JSONSchemaProps {
	t.Helper()

	crd := GetCustomResourceDefinitions()["Devices"]
	require.NotNil(t, crd, "Devices is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, step := range steps {
		if step == "[]" {
			require.NotNil(t, schema.Items, "the schema has no items at %q", step)
			require.NotNil(t, schema.Items.Schema, "the schema has no item schema at %q", step)
			schema = schema.Items.Schema
			continue
		}
		child, ok := schema.Properties[step]
		require.True(t, ok, "the schema has no %q under the walked path", step)
		schema = &child
	}
	return *schema
}

// TestDevicesStatusInterfacesSchemaShape pins the public selection surface of Devices.status:
// the status.rdma draft stays removed, and its replacement is an optional interfaces object
// whose rdmaSelectionPolicy carries the resolved selection's policy reference plus its
// exclusions, beside the root RDMASelectionResolved condition on the shared Condition type.
// The reference is an optional object whose name, uid and generation are required — they are
// what tells a same-name replacement from an edit of the same policy — and carries no kind or
// namespace because the referenced policy is cluster-scoped and is the only kind referenced.
// Groups is untouched, which also proves the schema walker itself still walks a healthy schema.
func TestDevicesStatusInterfacesSchemaShape(t *testing.T) {
	statusSchema := devicesObjectSchema(t, "status")

	assert.NotContains(t, statusSchema.Properties, "rdma",
		"status.rdma was the rejected draft; the replacement is status.interfaces plus root conditions")
	assert.Contains(t, statusSchema.Properties, "interfaces",
		"the schema has no status.interfaces")
	assert.NotContains(t, statusSchema.Required, "interfaces",
		"an absent status means no report, so interfaces can never be required")

	interfaces := devicesObjectSchema(t, "status", "interfaces")
	for _, field := range []string{"devices", "ports", "conditions", "inventoryComplete"} {
		assert.NotContains(t, interfaces.Properties, field,
			"status.interfaces is feature status keyed by feature; %q here would grow a second inventory or a nested condition set", field)
	}

	policy := devicesObjectSchema(t, "status", "interfaces", "rdmaSelectionPolicy")
	assert.NotContains(t, interfaces.Required, "rdmaSelectionPolicy",
		"an unreported selection must stay absent rather than forced empty")
	for _, field := range []string{"policyRef", "excluded"} {
		assert.Contains(t, policy.Properties, field,
			"the schema has no status.interfaces.rdmaSelectionPolicy.%s", field)
		assert.NotContains(t, policy.Required, field,
			"status.interfaces.rdmaSelectionPolicy.%s must stay optional: a missing reference alone is not success or default", field)
	}
	for _, field := range []string{
		"inventoryRevision", "lastSuccessfulTime", "message",
		"desiredRevision", "appliedRevision", "selectedUnits", "exclusions", "conditions",
		"ownerIdentity", "ownerProtocolVersion", "desiredRef", "appliedRef",
	} {
		assert.NotContains(t, policy.Properties, field,
			"status.interfaces.rdmaSelectionPolicy carries the resolved reference and exclusions only; %q was a draft's field", field)
	}

	ref := devicesObjectSchema(t, "status", "interfaces", "rdmaSelectionPolicy", "policyRef")
	for _, field := range []string{"name", "uid", "generation"} {
		assert.Contains(t, ref.Required, field,
			"a reference to a policy that cannot be told from another one is not a reference: %s is required", field)
	}
	for _, field := range []string{"kind", "namespace"} {
		assert.NotContains(t, ref.Properties, field,
			"the reference names only the cluster-scoped DevicesRDMASelectionPolicy; %q would imply a target it does not have", field)
	}
	uid := ref.Properties["uid"]
	require.NotNil(t, uid.MinLength, "the schema has no minLength on policyRef.uid")
	assert.Equal(t, int64(1), *uid.MinLength,
		"an empty uid would reduce the reference to a name")
	name := ref.Properties["name"]
	require.NotNil(t, name.MinLength, "the schema has no minLength on policyRef.name")
	assert.Equal(t, int64(1), *name.MinLength)
	generation := ref.Properties["generation"]
	require.NotNil(t, generation.Minimum, "the schema has no minimum on policyRef.generation")
	assert.Equal(t, 1.0, *generation.Minimum,
		"a policy carries generation 1 from its first write, so 0 is not a real reference value")

	excluded := devicesObjectSchema(t, "status", "interfaces", "rdmaSelectionPolicy", "excluded")
	require.NotNil(t, excluded.XListType, "the schema has no list type on excluded")
	assert.Equal(t, "map", *excluded.XListType,
		"exclusions are keyed by HCA name, so a writer patches its own entries")
	assert.Equal(t, []string{"name"}, excluded.XListMapKeys)
	excludedItem := devicesObjectSchema(t, "status", "interfaces", "rdmaSelectionPolicy", "excluded", "[]")
	assert.Contains(t, excludedItem.Required, "name",
		"an exclusion without the excluded HCA's name says nothing")
	assert.NotContains(t, excludedItem.Required, "reason",
		"the reason is optional: the exclusion itself is the fact")

	conditions, ok := statusSchema.Properties["conditions"]
	require.True(t, ok, "the schema has no status.conditions")
	assert.NotContains(t, statusSchema.Required, "conditions",
		"an unreported condition set must stay absent rather than forced empty")
	require.NotNil(t, conditions.XListType, "the schema has no list type on status.conditions")
	assert.Equal(t, "map", *conditions.XListType,
		"conditions are a list keyed by type, so writers patch their own entries")
	assert.Equal(t, []string{"type"}, conditions.XListMapKeys,
		"conditions are keyed by type alone")
	conditionItem := devicesObjectSchema(t, "status", "conditions", "[]")
	for _, field := range []string{"type", "status", "lastTransitionTime"} {
		assert.Contains(t, conditionItem.Required, field,
			"the shared Condition's %s is required and must stay so", field)
	}
	assert.NotContains(t, conditionItem.Required, "observedGeneration",
		"observedGeneration stays optional; when populated it is the Devices generation")
	conditionStatus := conditionItem.Properties["status"]
	values := make([]string, 0, len(conditionStatus.Enum))
	for _, value := range conditionStatus.Enum {
		values = append(values, string(value.Raw))
	}
	assert.ElementsMatch(t, []string{`"True"`, `"False"`, `"Unknown"`}, values,
		"conditions use the shared Condition's status vocabulary")

	groups := devicesObjectSchema(t, "status", "groups")
	require.NotNil(t, groups.XListType, "the schema has no list type on status.groups")
	assert.Equal(t, "map", *groups.XListType)
	assert.ElementsMatch(t, []string{"id", "manufacturer"}, groups.XListMapKeys,
		"status.groups is the existing allocation inventory and must not move under this change")
	groupItem := devicesObjectSchema(t, "status", "groups", "[]")
	assert.ElementsMatch(t, []string{"id", "manufacturer"}, groupItem.Required)
}

// TestDevicesStatusConditionsPatchConvention pins that the root conditions carry the same
// strategic-merge and list-map conventions as the repo's other root condition sets, so a
// status writer patches its own condition types without clobbering another writer's.
func TestDevicesStatusConditionsPatchConvention(t *testing.T) {
	statusField, ok := reflect.TypeOf(DevicesStatus{}).FieldByName("Conditions")
	require.True(t, ok, "DevicesStatus has no Conditions field")

	nodeModelStoreField, ok := reflect.TypeOf(NodeModelStoreStatus{}).FieldByName("Conditions")
	require.True(t, ok, "NodeModelStoreStatus has no Conditions field")

	assert.Equal(t, "type", statusField.Tag.Get("patchMergeKey"),
		"root conditions merge on type, or one writer's update replaces another's")
	assert.Equal(t, "merge", statusField.Tag.Get("patchStrategy"))
	assert.Equal(t, "conditions,omitempty", statusField.Tag.Get("json"),
		"the condition set stays optional on the wire")
	assert.Equal(t, nodeModelStoreField.Tag.Get("patchMergeKey"), statusField.Tag.Get("patchMergeKey"),
		"the Devices root condition set must use the same patch conventions as NodeModelStoreStatus")
	assert.Equal(t, nodeModelStoreField.Tag.Get("patchStrategy"), statusField.Tag.Get("patchStrategy"))
	assert.Equal(t, reflect.TypeOf([]gpustack.Condition{}), statusField.Type,
		"root conditions use the shared api/v1 Condition type")
}

// TestDevicesStatusInterfacesWireDocument pins that a status written in the published shape
// survives decoding and re-encoding across JSON and protobuf: no field of the selection is a
// server-side derivation, so what a client wrote is what the next reader gets, including the
// reference and the root condition. It walks raw documents so the pin holds however the Go
// fields evolve.
func TestDevicesStatusInterfacesWireDocument(t *testing.T) {
	raw := []byte(`{
		"groups": [{"id": "gpu", "manufacturer": "nvidia", "accelerators": []}],
		"interfaces": {"rdmaSelectionPolicy": {
			"policyRef": {"name": "compute-hcas", "uid": "uid-a", "generation": 3},
			"excluded": [{"name": "mlx5_1", "reason": "ExcludedByPolicy"}]
		}},
		"conditions": [
			{"type": "RDMASelectionResolved", "status": "True", "reason": "PolicyResolved",
			 "lastTransitionTime": "2026-10-30T13:00:00Z"}
		]
	}`)

	var status DevicesStatus
	require.NoError(t, json.Unmarshal(raw, &status))

	out, err := json.Marshal(&status)
	require.NoError(t, err)
	for _, key := range []string{
		`"interfaces"`, `"rdmaSelectionPolicy"`, `"policyRef"`,
		`"excluded"`, `"conditions"`, `"RDMASelectionResolved"`,
	} {
		assert.Contains(t, string(out), key, "re-encoding dropped %s", key)
	}
	assert.NotContains(t, string(out), `"rdma"`,
		"the removed status.rdma draft must not reappear on the wire")
	assert.NotContains(t, string(out), `"desiredRef"`,
		"the retired desiredRef key must not reappear on the wire")
	assert.NotContains(t, string(out), `"appliedRef"`,
		"the retired appliedRef key must not reappear on the wire")

	obj := &Devices{Status: status}
	protoRaw, err := obj.Marshal()
	require.NoError(t, err)
	fromProto := &Devices{}
	require.NoError(t, fromProto.Unmarshal(protoRaw))
	assert.True(t, kubemeta.DeepEqual(obj.Status, fromProto.Status),
		"the protobuf roundtrip must preserve the status semantically")

	roundTripped, err := json.Marshal(&fromProto.Status)
	require.NoError(t, err)
	assert.Contains(t, string(roundTripped), `"policyRef"`,
		"the protobuf roundtrip must preserve the selection reference")
}

// TestDevicesSelectionWireStates pins the wire contract of the selection across the states a
// reader has to tell apart. Every case is declarative data — a status fixture plus the JSON
// keys that must and must not survive — and one shared runner round-trips each through JSON
// and protobuf, comparing the decoded object field by field, timestamps on instants, because
// nil and empty repeated fields collapse on the wire and only a state carried by a field or a
// condition survives the collapse.
func TestDevicesSelectionWireStates(t *testing.T) {
	now := meta.NewTime(time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)).Rfc3339Copy()
	condition := func(typ, status, reason string, observed int64) gpustack.Condition {
		return gpustack.Condition{
			Type:               typ,
			Status:             meta.ConditionStatus(status),
			Reason:             reason,
			ObservedGeneration: observed,
			LastTransitionTime: now,
		}
	}
	pendingResolved := DevicesStatus{
		Interfaces: &DevicesInterfacesStatus{
			RDMASelectionPolicy: &DevicesRDMASelectionPolicyStatus{
				PolicyRef: &DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
				Excluded:  []DevicesRDMAExclusion{{Name: "mlx5_1", Reason: "ExcludedByPolicy"}},
			},
		},
		Conditions: []gpustack.Condition{
			condition("RDMASelectionResolved", "True", "PolicyResolved", 7),
		},
	}

	cases := []struct {
		name       string
		status     DevicesStatus
		wantKeys   []string
		unwantKeys []string
	}{
		{
			name:       "unreported status omits every selection key",
			status:     DevicesStatus{},
			unwantKeys: []string{`"interfaces"`, `"rdma"`, `"conditions"`},
		},
		{
			name:   "a resolved selection carries its reference and condition",
			status: pendingResolved,
			wantKeys: []string{
				`"generation":3`, `"policyRef"`,
				`"RDMASelectionResolved"`, `"PolicyResolved"`,
			},
		},
		{
			name: "same name with another uid is another policy",
			status: DevicesStatus{
				Interfaces: &DevicesInterfacesStatus{
					RDMASelectionPolicy: &DevicesRDMASelectionPolicyStatus{
						PolicyRef: &DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-b", Generation: 1},
					},
				},
			},
			wantKeys:   []string{`"uid":"uid-b"`},
			unwantKeys: []string{`"uid":"uid-a"`},
		},
		{
			name: "an unresolved selection carries no reference provenance",
			status: DevicesStatus{
				Conditions: []gpustack.Condition{
					condition("RDMASelectionResolved", "Unknown", "ResolutionUnknown", 7),
				},
			},
			wantKeys:   []string{`"ResolutionUnknown"`},
			unwantKeys: []string{`"policyRef"`, `"excluded"`},
		},
		{
			name: "a resolved default is told by the condition, not a reference",
			status: DevicesStatus{
				Conditions: []gpustack.Condition{
					condition("RDMASelectionResolved", "True", "NoPolicy", 7),
				},
			},
			wantKeys:   []string{`"RDMASelectionResolved"`},
			unwantKeys: []string{`"policyRef"`},
		},
		{
			name: "a reference alone carries no condition",
			status: DevicesStatus{
				Interfaces: &DevicesInterfacesStatus{
					RDMASelectionPolicy: &DevicesRDMASelectionPolicyStatus{
						PolicyRef: &DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 2},
					},
				},
			},
			wantKeys:   []string{`"policyRef"`},
			unwantKeys: []string{`"conditions"`},
		},
		{
			name: "empty excluded collapses on the wire",
			status: DevicesStatus{
				Interfaces: &DevicesInterfacesStatus{
					RDMASelectionPolicy: &DevicesRDMASelectionPolicyStatus{
						PolicyRef: &DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 2},
						Excluded:  []DevicesRDMAExclusion{},
					},
				},
			},
			wantKeys:   []string{`"policyRef"`},
			unwantKeys: []string{`"excluded"`},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			obj := &Devices{Status: c.status}

			raw, err := json.Marshal(obj)
			require.NoError(t, err)
			for _, key := range c.wantKeys {
				assert.Contains(t, string(raw), key, "the wire dropped %s", key)
			}
			for _, key := range c.unwantKeys {
				assert.NotContains(t, string(raw), key,
					"nil and empty repeated fields collapse on the wire; %s must not read as a state", key)
			}

			var fromJSON Devices
			require.NoError(t, json.Unmarshal(raw, &fromJSON))
			assertDevicesStatusEqual(t, c.status, fromJSON.Status)

			protoRaw, err := obj.Marshal()
			require.NoError(t, err)
			fromProto := &Devices{}
			require.NoError(t, fromProto.Unmarshal(protoRaw))
			assertDevicesStatusEqual(t, c.status, fromProto.Status)
			assert.True(t, kubemeta.DeepEqual(obj.Status, fromProto.Status),
				"the protobuf roundtrip must preserve the status semantically")
		})
	}
}

// assertDevicesStatusEqual compares a written status against a decoded one field by field.
// The root conditions are compared FIRST, for every state — they are independent of the
// interfaces subtree, and the conditions-only fixtures are exactly the ones where Interfaces
// is nil. Exclusion lists and groups are compared semantically, so the wire's nil/empty
// collapse does not fail a roundtrip that preserved the state; condition timestamps are
// compared on instants, because the wire renders carry no wall-clock location.
func assertDevicesStatusEqual(t *testing.T, want, got DevicesStatus) {
	t.Helper()

	assert.True(t, kubemeta.DeepEqual(want.Groups, got.Groups),
		"groups must roundtrip semantically")

	require.Len(t, got.Conditions, len(want.Conditions),
		"the condition set must roundtrip whole")
	for i, wantCondition := range want.Conditions {
		gotCondition := got.Conditions[i]
		assert.Equal(t, wantCondition.Type, gotCondition.Type)
		assert.Equal(t, wantCondition.Status, gotCondition.Status)
		assert.Equal(t, wantCondition.Reason, gotCondition.Reason)
		assert.Equal(t, wantCondition.Message, gotCondition.Message)
		assert.Equal(t, wantCondition.ObservedGeneration, gotCondition.ObservedGeneration)
		assert.True(t, wantCondition.LastTransitionTime.Time.Equal(gotCondition.LastTransitionTime.Time),
			"condition %s must roundtrip to the same instant, not the same rendering", wantCondition.Type)
	}

	if want.Interfaces == nil {
		assert.Nil(t, got.Interfaces, "an unwritten interfaces status must decode as absent")
		return
	}
	require.NotNil(t, got.Interfaces, "a written interfaces status must survive the roundtrip")
	if want.Interfaces.RDMASelectionPolicy == nil {
		assert.Nil(t, got.Interfaces.RDMASelectionPolicy,
			"an unreported selection must decode as absent")
		return
	}
	require.NotNil(t, got.Interfaces.RDMASelectionPolicy,
		"a written selection must survive the roundtrip")
	wantPolicy, gotPolicy := want.Interfaces.RDMASelectionPolicy, got.Interfaces.RDMASelectionPolicy
	assertRefsEqual(t, wantPolicy.PolicyRef, gotPolicy.PolicyRef)
	assert.True(t, kubemeta.DeepEqual(wantPolicy.Excluded, gotPolicy.Excluded),
		"exclusions must roundtrip semantically: nil and empty are the same state")
}

// assertRefsEqual compares one policy reference, nil against nil and field against field.
func assertRefsEqual(t *testing.T, want, got *DevicesRDMASelectionPolicyReference) {
	t.Helper()

	if want == nil {
		assert.Nil(t, got, "an absent reference must decode as absent")
		return
	}
	require.NotNil(t, got, "a written reference must survive the roundtrip")
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.UID, got.UID)
	assert.Equal(t, want.Generation, got.Generation)
}

// TestDevicesSelectionDeepCopyCutsReferenceFields pins that a copied Devices shares no backing
// arrays or reference objects with the original through the selection, so a reader holding a
// copy cannot move a slice or a reference the next writer stores.
func TestDevicesSelectionDeepCopyCutsReferenceFields(t *testing.T) {
	now := meta.NewTime(time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC))
	obj := &Devices{Status: DevicesStatus{
		Interfaces: &DevicesInterfacesStatus{
			RDMASelectionPolicy: &DevicesRDMASelectionPolicyStatus{
				PolicyRef: &DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
				Excluded:  []DevicesRDMAExclusion{{Name: "mlx5_1", Reason: "ExcludedByPolicy"}},
			},
		},
		Conditions: []gpustack.Condition{{
			Type: "RDMASelectionResolved", Status: meta.ConditionTrue, Reason: "PolicyResolved",
			LastTransitionTime: now,
		}},
	}}

	got := obj.DeepCopy()
	got.Status.Interfaces.RDMASelectionPolicy.Excluded[0].Reason = "mutated"
	got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation = 99
	got.Status.Conditions[0].Reason = "mutated"
	got.Status.Conditions[0].LastTransitionTime = meta.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))

	assert.Equal(t, "ExcludedByPolicy", obj.Status.Interfaces.RDMASelectionPolicy.Excluded[0].Reason)
	assert.Equal(t, int64(3), obj.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation)
	assert.Equal(t, "PolicyResolved", obj.Status.Conditions[0].Reason)
	assert.True(t, now.Time.Equal(obj.Status.Conditions[0].LastTransitionTime.Time),
		"the original's transition time must not move with the copy")
}

func TestDeviceInterfaceLinkStateEnum(t *testing.T) {
	crd := GetCustomResourceDefinitions()["Devices"]
	require.NotNil(t, crd)
	require.Len(t, crd.Spec.Versions, 1)
	for _, path := range [][]string{
		{"spec", "interfaces", "[]", "link", "state"},
		{"spec", "interfaces", "[]", "virtualFunctions", "[]", "link", "state"},
	} {
		t.Run(path[3], func(t *testing.T) {
			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, part := range path {
				if part == "[]" {
					require.NotNil(t, schema.Items)
					require.NotNil(t, schema.Items.Schema)
					schema = schema.Items.Schema
					continue
				}
				child, ok := schema.Properties[part]
				require.True(t, ok, "missing %s", part)
				schema = &child
			}
			values := make([]string, 0, len(schema.Enum))
			for _, value := range schema.Enum {
				values = append(values, string(value.Raw))
			}
			assert.ElementsMatch(t, []string{`"OK"`, `"Unverified"`, `"Failed"`}, values)
			for _, old := range []string{`"ok"`, `"unverified"`, `"failed"`} {
				assert.NotContains(t, values, old)
			}
		})
	}
}

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
