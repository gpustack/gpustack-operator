package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// retirementSchemaValidator is the REAL generated CRD validator, not a stand-in. The point of these
// controls is what a real API server does with the bytes this writer emits, so the schema the
// server would use is the one the project generates.
func retirementSchemaValidator(t *testing.T) *apiextensions.JSONSchemaProps {
	t.Helper()

	crd := workercore.GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd)
	require.Len(t, crd.Spec.Versions, 1)
	schema := new(apiextensions.JSONSchemaProps)
	require.NoError(t,
		extension.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil))

	return schema
}

func newRetirementObjectValidator(t *testing.T) apiservervalidation.SchemaValidator {
	t.Helper()

	validator, _, err := apiservervalidation.NewSchemaValidator(retirementSchemaValidator(t))
	require.NoError(t, err)

	return validator
}

// storedDeploymentJSON is an object as a server can hold it: a role status carrying fields the
// current typed struct no longer models, which is what made a full-object status write fail on
// something the operation never said.
func storedDeploymentJSON(t *testing.T, legacy bool) []byte {
	t.Helper()

	role := map[string]any{
		"name": "s0", "desired": 1, "ready": 1, "quotaReserved": 1,
		"unmanaged": false, "kind": "Server", "assignedFlavors": []any{"gpu-a"},
	}
	if !legacy {
		role["parallelism"] = map[string]any{
			"declared":      map[string]any{"tensorParallel": 2, "pipelineParallel": 1},
			"modes":         map[string]any{"tensor": true},
			"loadBalance":   "Internal",
			"source":        map[string]any{"kind": "ExtraArgs", "complete": true},
			"tensorBalance": "Block",
		}
		role["endpoints"] = map[string]any{
			"eligible": 1,
			"serving":  map[string]any{"state": "Confirmed", "value": 1},
		}
	}
	stored := map[string]any{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind":       "ModelDeployment",
		"metadata": map[string]any{
			"name": "demo", "namespace": "default", "uid": "deployment-uid", "generation": 1,
		},
		"spec": map[string]any{
			"model":  map[string]any{"name": "qwen-demo"},
			"engine": map[string]any{"name": "vLLM"},
			"roles":  []any{map[string]any{"name": "s0", "instanceType": "gpu-small", "kind": "Server", "size": 1}},
		},
		"status": map[string]any{
			"roleSummary": "S1",
			"roles":       []any{role},
			"retirement": map[string]any{
				"roleName": "s0", "replicaOrdinal": 0, "observedGeneration": 1,
				"targetWorkloadUID": "workload-uid", "targetMemberUIDs": []any{"member-uid"},
				"state": "Draining", "reason": "draining",
				"startedAt":      "2026-10-01T00:00:00Z",
				"deadline":       "2026-10-01T01:00:00Z",
				"phaseStartedAt": "2026-10-01T00:00:00Z",
			},
		},
	}
	raw, err := json.Marshal(stored)
	require.NoError(t, err)

	return raw
}

// TestRetirementStatusWriteStaysValidAgainstAStoredLegacyObject reads the bytes the writer emits
// and hands them to the schema a real API server would use.
// emitted bytes are merged into a separately retained stored object and the result is checked
// against the real generated schema. A role status that omits fields the current typed struct
// models is exactly the object a real server can still be holding, and a write that only moves the
// operation one phase on must not be refused for a field it never mentioned.
func TestRetirementStatusWriteStaysValidAgainstAStoredLegacyObject(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "a stored object missing optional role fields"
		if !legacy {
			name = "a stored object carrying the current typed role fields"
		}
		t.Run(name, func(t *testing.T) {
			validator := newRetirementObjectValidator(t)
			stored := storedDeploymentJSON(t, legacy)

			// The raw stored object is a valid control, so a later failure cannot be the fixture.
			var control map[string]any
			require.NoError(t, json.Unmarshal(stored, &control))
			require.Empty(t, apiservervalidation.ValidateCustomResource(
				field.NewPath("object"), control, validator), "the raw stored object is valid")

			// The writer's own view of the object, and the reservation it is about to assert.
			md := new(workercore.ModelDeployment)
			require.NoError(t, json.Unmarshal(stored, md))
			previous := md.Status.Retirement.DeepCopy()
			next := previous.DeepCopy()
			next.State = workercore.ModelDeploymentRetirementStateDeleting
			next.Reason = "the target's queues are empty"

			var emitted []byte
			cli := newModelDeploymentClient(md)
			watcher, ok := cli.(ctrlcli.WithWatch)
			require.True(t, ok)
			counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
					obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
				) error {
					require.Equal(t, "status", sub, "the retirement write is a status write")
					var patchErr error
					emitted, patchErr = patch.Data(obj)
					require.NoError(t, patchErr)

					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &ModelDeploymentReconciler{Client: counting, APIReader: counting}
			require.NoError(t, r.writeModelDeploymentRetirement(
				context.Background(), md, previous, next))
			require.NotEmpty(t, emitted, "the writer must actually emit a patch")

			// The emitted delta carries the reservation and the version, and nothing else. A
			// full-object body would be the defect this writer exists to avoid.
			var wire map[string]any
			require.NoError(t, json.Unmarshal(emitted, &wire))
			require.Contains(t, wire, "metadata", "the patch is optimistic and says which version")
			status := wire["status"].(map[string]any)
			require.Equal(t, []string{"retirement"},
				keysOf(status), "the write says only what this operation asserts")
			require.Equal(t, "Deleting", status["retirement"].(map[string]any)["state"])

			// Merging it into the stored object is what a server does, and the result has to be
			// valid against the real schema.
			merge, err := jsonpatch.MergePatch(stored, emitted)
			require.NoError(t, err)
			var merged map[string]any
			require.NoError(t, json.Unmarshal(merge, &merged))
			require.Empty(t, apiservervalidation.ValidateCustomResource(
				field.NewPath("object"), merged, validator),
				"the merged object must be accepted by the real generated CRD validator")
			require.Equal(t, "Deleting",
				merged["status"].(map[string]any)["retirement"].(map[string]any)["state"])
		})
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

// storedRetirementObject reads the object back under its own key, so a control can see what the
// server holds rather than what this pass still believes.
func storedRetirementObject(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) *workercore.ModelDeployment {
	t.Helper()

	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), stored))

	return stored
}

// seededOperation returns an operation that exists on the server, with a real resourceVersion, so
// the optimistic lock has something real to compare against.
func seededOperation(
	t *testing.T, state workercore.ModelDeploymentRetirementState,
) (ctrlcli.Client, *workercore.ModelDeployment, *workercore.ModelDeploymentRetirementStatus) {
	t.Helper()

	md := new(workercore.ModelDeployment)
	md.Name, md.Namespace = "demo", "default"
	md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
		RoleName: "s0", State: state, TargetWorkloadUID: "workload-uid",
		TargetMemberUIDs: []string{"member-uid"},
	}
	cli := newModelDeploymentClient(md)
	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), stored))
	require.NotEmpty(t, stored.ResourceVersion, "the operation has to carry a real version")

	return cli, stored, stored.Status.Retirement.DeepCopy()
}

// TestRetirementStatusWriteClearsWithAnExplicitNull pins the clear. A patch that simply omits the
// field leaves the stored reservation in place, which is the opposite of clearing it, and a pass
// that asserts nothing new has to write nothing at all.
func TestRetirementStatusWriteClearsWithAnExplicitNull(t *testing.T) {
	cli, md, previous := seededOperation(t, workercore.ModelDeploymentRetirementStateCompleted)

	var emitted []byte
	watcher, ok := cli.(ctrlcli.WithWatch)
	require.True(t, ok)
	counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
			obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
		) error {
			var patchErr error
			emitted, patchErr = patch.Data(obj)
			require.NoError(t, patchErr)

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &ModelDeploymentReconciler{Client: counting, APIReader: counting}
	require.NoError(t, r.writeModelDeploymentRetirement(context.Background(), md, previous, nil))

	require.NotEmpty(t, emitted)
	require.Contains(t, string(emitted), `"retirement":null`,
		"clearing is an explicit null, not an absent field")

	assert.Nil(t, storedRetirementObject(t, cli, md).Status.Retirement,
		"the persisted reservation is actually absent")
}

// TestRetirementStatusWriteIsARealOptimisticLock drives the conflict the way a server produces it:
// the stored object moves after this pass read it, so the version on the wire is one the server has
// moved past. An injected error alone would not show the version was ever carried.
func TestRetirementStatusWriteIsARealOptimisticLock(t *testing.T) {
	newOperation := func(t *testing.T) (*ModelDeploymentReconciler, *workercore.ModelDeployment,
		*workercore.ModelDeploymentRetirementStatus, *workercore.ModelDeploymentRetirementStatus,
	) {
		t.Helper()

		cli, md, previous := seededOperation(t, workercore.ModelDeploymentRetirementStateDraining)
		next := previous.DeepCopy()
		next.State = workercore.ModelDeploymentRetirementStateDeleting

		return &ModelDeploymentReconciler{Client: cli, APIReader: cli}, md, previous, next
	}

	t.Run("a stored object that moved refuses the write", func(t *testing.T) {
		r, md, previous, next := newOperation(t)
		stored := storedRetirementObject(t, r.Client, md)

		// The stored object moves on, so its version is no longer the one this pass read.
		stored.Annotations = map[string]string{"moved": "true"}
		require.NoError(t, r.Client.Update(context.Background(), stored))
		moved := storedRetirementObject(t, r.Client, md)
		require.NotEqual(t, md.ResourceVersion, moved.ResourceVersion,
			"the stored version has to have moved for this to mean anything")

		var wire []byte
		watcher, ok := r.Client.(ctrlcli.WithWatch)
		require.True(t, ok)
		counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
				obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
			) error {
				var patchErr error
				wire, patchErr = patch.Data(obj)
				require.NoError(t, patchErr)

				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		conflicted := &ModelDeploymentReconciler{Client: counting, APIReader: counting}
		err := conflicted.writeModelDeploymentRetirement(context.Background(), md, previous, next)

		require.Error(t, err, "a write against a version the server moved past is refused")
		assert.True(t, apierrors.IsConflict(err), "and it is a real conflict, not an injected error")
		require.NotEmpty(t, wire)
		assert.Contains(t, string(wire), `"resourceVersion":"`+md.ResourceVersion+`"`,
			"the emitted wire carries the version this pass actually read")

		after := storedRetirementObject(t, r.Client, md)
		assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining,
			after.Status.Retirement.State, "the operation did not move on an unpersisted write")
	})

	t.Run("an unmoved object accepts the write", func(t *testing.T) {
		r, md, previous, next := newOperation(t)
		var wire []byte
		watcher, ok := r.Client.(ctrlcli.WithWatch)
		require.True(t, ok)
		counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
				obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
			) error {
				var patchErr error
				wire, patchErr = patch.Data(obj)
				require.NoError(t, patchErr)

				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		landed := &ModelDeploymentReconciler{Client: counting, APIReader: counting}
		require.NoError(t, landed.writeModelDeploymentRetirement(
			context.Background(), md, previous, next))

		require.NotEmpty(t, wire)
		after := storedRetirementObject(t, r.Client, md)
		assert.Equal(t, workercore.ModelDeploymentRetirementStateDeleting,
			after.Status.Retirement.State, "the matching-version write lands")
		assert.NotEmpty(t, md.ResourceVersion, "and the object carries the version it produced")
	})
}

// TestRetirementStatusWriteIsSilentWhenNothingChanged pins the no-op. A controller that runs on
// every event must not rewrite the object on every pass, and a reservation that is already clear
// has nothing to say.
func TestRetirementStatusWriteIsSilentWhenNothingChanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous *workercore.ModelDeploymentRetirementStatus
		next     *workercore.ModelDeploymentRetirementStatus
	}{
		{
			name:     "the same reservation is written again",
			previous: &workercore.ModelDeploymentRetirementStatus{RoleName: "s0", State: workercore.ModelDeploymentRetirementStateDraining},
			next:     &workercore.ModelDeploymentRetirementStatus{RoleName: "s0", State: workercore.ModelDeploymentRetirementStateDraining},
		},
		{name: "no reservation to clear", previous: nil, next: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, md, _ := seededOperation(t, workercore.ModelDeploymentRetirementStateDraining)
			before := storedRetirementObject(t, cli, md)

			writes := 0
			watcher, ok := cli.(ctrlcli.WithWatch)
			require.True(t, ok)
			counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
					obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
				) error {
					writes++

					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &ModelDeploymentReconciler{Client: counting, APIReader: counting}
			require.NoError(t, r.writeModelDeploymentRetirement(
				context.Background(), md, tc.previous, tc.next))

			assert.Zero(t, writes, "a pass that asserts nothing new writes nothing")
			after := storedRetirementObject(t, cli, md)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
				"and the stored object was not touched")
		})
	}
}

// TestEarlyRetirementWritersEmitOnlyTheirOwnDelta drives the three production writers through the
// paths that actually reach them and reads the bytes the API server would receive.
//
// A helper tested on its own proves it composes a delta. It does not prove the writers that depend
// on it use it, and it says nothing about the wire a real reconciliation emits. So the subject here
// is the wire: every status write the protocol makes, captured from a real client, against a stored
// object that is deliberately a legacy one.
func TestEarlyRetirementWritersEmitOnlyTheirOwnDelta(t *testing.T) {
	// A stored object the current typed struct does not fully model. Every write below is merged
	// into this, so a writer that still sent a full typed status would show up here as an object
	// the real validator refuses.
	legacy := storedDeploymentJSON(t, true)
	validator := newRetirementObjectValidator(t)

	for _, tc := range []struct {
		name     string
		subject  string
		prepared func(t *testing.T) (*retirementGPUProtocolFixture, *ModelDeploymentReconciler)
	}{
		{
			name:    "an admission the pass reaches on its own",
			subject: "a replica this pass no longer declares",
			prepared: func(t *testing.T) (*retirementGPUProtocolFixture, *ModelDeploymentReconciler) {
				f := newRetirementGPUProtocolFixture(t)

				return f, f.reconciler()
			},
		},
		{
			name:    "phase progress after the operation is already reserved",
			subject: "a reservation the protocol walks forward one phase at a time",
			prepared: func(t *testing.T) (*retirementGPUProtocolFixture, *ModelDeploymentReconciler) {
				f := newRetirementGPUProtocolFixture(t)
				f.advanceToDraining(t)

				return f, f.reconciler()
			},
		},
		{
			name:    "the durable clear after the release converges",
			subject: "a settled operation whose reservation is removed for good",
			prepared: func(t *testing.T) (*retirementGPUProtocolFixture, *ModelDeploymentReconciler) {
				f := newRetirementGPUProtocolFixture(t)
				f.advanceToDraining(t)
				r := f.reconciler()
				for range 3 {
					_, err := reconcileModelDeploymentWith(t, r)
					require.NoError(t, err)
				}
				// The captured node reports the card free, which is what lets the operation settle.
				convergeRetirementLedger(t, f)

				return f, f.reconciler()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r := tc.prepared(t)
			base := f.client

			var writes [][]byte
			f.client = ctrlinterceptor.NewClient(base.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
					obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
				) error {
					if _, ok := obj.(*workercore.ModelDeployment); ok && sub == "status" {
						data, err := patch.Data(obj)
						require.NoError(t, err)
						writes = append(writes, data)
					}

					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r.Client, r.APIReader = f.client, f.client
			_, err := reconcileModelDeploymentWith(t, r)
			f.client = base
			require.NoError(t, err)

			require.NotEmpty(t, writes, "the pass must actually reach a retirement writer")
			for _, wire := range writes {
				assertValidRetirementDelta(t, validator, legacy, wire)
			}
		})
	}
}

// convergeRetirementLedger reports the target's node as holding nothing, which is the observation
// the settlement path requires before it clears the operation.
func convergeRetirementLedger(t *testing.T, f *retirementGPUProtocolFixture) {
	t.Helper()

	devs := new(workercore.Devices)
	require.NoError(t, f.client.Get(context.Background(),
		ctrlcli.ObjectKey{Name: f.target.Spec.NodeName}, devs))
	status, _, err := deviceplugin.BuildDesiredStatusStrict(
		ctrllogDiscard(), devs, &core.PodList{})
	require.NoError(t, err)
	devs.Status = status
	require.NoError(t, f.client.Status().Update(context.Background(), devs))
}

// assertValidRetirementDelta is the shared reading of one emitted write: the delta names the
// version it is based on, says only what the operation asserts, and leaves a stored object the
// server still holds acceptable to the real schema.
func assertValidRetirementDelta(
	t *testing.T, validator apiservervalidation.SchemaValidator, stored, wire []byte,
) {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(wire, &body))
	require.Contains(t, body, "metadata", "an optimistic write says which version it is based on")
	require.NotEmpty(t, body["metadata"].(map[string]any)["resourceVersion"])

	status, ok := body["status"].(map[string]any)
	require.True(t, ok, "a retirement write carries a status delta")
	require.Equal(t, []string{"retirement"}, keysOf(status),
		"the write says only what this operation asserts")

	merged, err := jsonpatch.MergePatch(stored, wire)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(merged, &object))
	require.Empty(t, apiservervalidation.ValidateCustomResource(
		field.NewPath("object"), object, validator),
		"the stored object must stay acceptable to the real generated CRD schema after the write")
}

// TestTheConsumedRetryDirectiveWritesThroughTheSameWriter reaches the retry writer, which the
// protocol never does on its own, and reads the write it makes.
//
// A token is consumed by persisting it, so this write is the only thing that decides whether a
// retry is honored. It has to be a retirement delta for the same reason every other write is: a
// full-object status write here would fail on a legacy object and the directive would never take
// effect, leaving the operation aborted.
func TestTheConsumedRetryDirectiveWritesThroughTheSameWriter(t *testing.T) {
	md := retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Annotations = map[string]string{
			modelDeploymentRetirementRetryAnnotation: "server:1:token-b",
		}
	})
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"}))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	stored := getModelDeployment(t, cli)
	// The write decodes the server's answer into this object, so the version it is based on is read
	// before the write rather than after it.
	read := stored.ResourceVersion

	var writes [][]byte
	watcher, ok := cli.(ctrlcli.WithWatch)
	require.True(t, ok)
	counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
			obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
		) error {
			if _, isMD := obj.(*workercore.ModelDeployment); isMD && sub == "status" {
				data, err := patch.Data(obj)
				require.NoError(t, err)
				writes = append(writes, data)
			}

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.Client, r.APIReader = counting, counting

	consumed := r.consumeModelDeploymentRetry(context.Background(), stored,
		&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement})
	require.True(t, consumed, "a fresh directive on an aborted operation is honoured")

	require.NotEmpty(t, writes, "consuming a token is a real write, not an in-memory decision")
	after := getModelDeployment(t, cli)
	require.NotNil(t, after.Status.Retirement)
	assert.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted, after.Status.Retirement.State)
	assert.Equal(t, "token-b", after.Status.Retirement.LastConsumedRetryToken,
		"the token is durable once the write lands")
	assert.NotContains(t, after.Annotations, modelDeploymentRetirementRetryAnnotation,
		"and the directive is cleared only after that")

	var wire map[string]any
	require.NoError(t, json.Unmarshal(writes[0], &wire))
	status, ok := wire["status"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []string{"retirement"}, keysOf(status),
		"the consumed token travels in a retirement delta and nothing else")
	based, ok := wire["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, read, based["resourceVersion"],
		"and it is based on the version this pass read")
}

// TestAConflictedRetirementWriteLeavesTheLocalOperationAuthoritative separates the two failures a
// write can have, because they are not the same defect. A failure the client raises before the
// server sees anything must leave the caller's object alone, and so must a Conflict raised after a
// real stored version has moved: in both cases the object must still describe what the server
// holds, because that is what the rest of the pass reads to decide whether a token was consumed and
// whether a delete is allowed. A writer that assigned the new value before patching would leave the
// caller holding a phase the server never accepted.
func TestAConflictedRetirementWriteLeavesTheLocalOperationAuthoritative(t *testing.T) {
	newOperation := func(t *testing.T) (ctrlcli.Client, *workercore.ModelDeployment,
		*workercore.ModelDeploymentRetirementStatus, *workercore.ModelDeploymentRetirementStatus,
	) {
		t.Helper()

		cli, md, previous := seededOperation(t, workercore.ModelDeploymentRetirementStateDraining)
		next := previous.DeepCopy()
		next.State = workercore.ModelDeploymentRetirementStateDeleting

		return cli, md, previous, next
	}

	t.Run("a stored object that moved is refused and nothing local moves", func(t *testing.T) {
		cli, md, previous, next := newOperation(t)
		before := md.DeepCopy()

		stored := storedRetirementObject(t, cli, md)
		stored.Annotations = map[string]string{"concurrent": "update"}
		require.NoError(t, cli.Update(context.Background(), stored))
		require.NotEqual(t, before.ResourceVersion, stored.ResourceVersion,
			"the stored version has to have moved for the conflict to mean anything")

		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		err := r.writeModelDeploymentRetirement(context.Background(), md, previous, next)

		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err), "and it is a real conflict, not an injected error")
		assert.Equal(t, before.Status.Retirement, md.Status.Retirement,
			"a refused write must not leave an unpersisted phase on the caller's object")
		assert.Equal(t, before.ResourceVersion, md.ResourceVersion,
			"and the version it carries is the one the server still holds")
		assert.Equal(t, before.Status.Retirement, storedRetirementObject(t, cli, md).Status.Retirement,
			"the server kept the phase it had")
	})

	t.Run("a matching version lands and reports the version it produced", func(t *testing.T) {
		cli, md, previous, next := newOperation(t)
		before := md.ResourceVersion

		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		require.NoError(t, r.writeModelDeploymentRetirement(context.Background(), md, previous, next))

		assert.Equal(t, next, md.Status.Retirement, "the accepted write is the one the caller holds")
		assert.NotEqual(t, before, md.ResourceVersion, "and it carries the version the server produced")
		assert.Equal(t, next, storedRetirementObject(t, cli, md).Status.Retirement)
	})
}

// TestTheConsumedRetryDirectiveSurvivesAConflictedStatusWrite is the retry path under the same
// fault, and it is the case with real consequences: consuming a token is what resumes an aborted
// operation, and clearing the directive is what stops it being consumed twice. A token that was
// never persisted must leave the operation aborted, the directive in place, and the plan untouched,
// or the next pass will either retry something that did not happen or refuse one that did.
func TestTheConsumedRetryDirectiveSurvivesAConflictedStatusWrite(t *testing.T) {
	aborted := func(t *testing.T) ctrlcli.Client {
		t.Helper()

		md := retirementDeployment(func(md *workercore.ModelDeployment) {
			md.Annotations = map[string]string{
				modelDeploymentRetirementRetryAnnotation: "server:1:token-c",
			}
		})
		md = reserve(md, retirementReservation(
			workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"}))

		return newModelDeploymentClient(md.DeepCopy())
	}

	t.Run("a refused write consumes nothing and clears nothing", func(t *testing.T) {
		cli := aborted(t)
		r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
		stored := getModelDeployment(t, cli)

		moved := storedRetirementObject(t, cli, stored)
		moved.Annotations = map[string]string{
			modelDeploymentRetirementRetryAnnotation: "server:1:token-c", "concurrent": "update",
		}
		require.NoError(t, cli.Update(context.Background(), moved))

		plan := &modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement}
		consumed := r.consumeModelDeploymentRetry(context.Background(), stored, plan)

		assert.False(t, consumed, "an unpersisted token is not consumed")
		assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, stored.Status.Retirement.State,
			"the local operation is still the one the server holds")
		assert.Empty(t, stored.Status.Retirement.LastConsumedRetryToken,
			"and the token was never taken")
		assert.Equal(t, plan.Reservation, stored.Status.Retirement,
			"the plan was not advanced either")
		after := getModelDeployment(t, cli)
		assert.Contains(t, after.Annotations, modelDeploymentRetirementRetryAnnotation,
			"the directive stays, because it was never consumed")
	})

	t.Run("an accepted write consumes the token and the clear rides its version", func(t *testing.T) {
		cli := aborted(t)
		r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
		stored := getModelDeployment(t, cli)

		var statusVersion, clearVersion string
		watcher, ok := cli.(ctrlcli.WithWatch)
		require.True(t, ok)
		counting := ctrlinterceptor.NewClient(watcher, ctrlinterceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string,
				obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
			) error {
				if md, isMD := obj.(*workercore.ModelDeployment); isMD && sub == "status" {
					statusVersion = md.ResourceVersion
				}

				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			// The directive clear is an ordinary metadata patch, so it arrives here rather than on
			// the status subresource. Its base version is what the pass is standing on.
			Patch: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object,
				patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
			) error {
				clearVersion = obj.GetResourceVersion()

				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		r.Client, r.APIReader = counting, counting

		consumed := r.consumeModelDeploymentRetry(context.Background(), stored,
			&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement})
		require.True(t, consumed)

		require.NotEmpty(t, statusVersion, "the token is persisted before the directive is cleared")
		assert.Equal(t, stored.ResourceVersion, clearVersion,
			"the annotation patch is based on the version the status write returned")
		after := getModelDeployment(t, cli)
		assert.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted, after.Status.Retirement.State)
		assert.Equal(t, "token-c", after.Status.Retirement.LastConsumedRetryToken)
		assert.NotContains(t, after.Annotations, modelDeploymentRetirementRetryAnnotation,
			"and only then is the directive cleared")
	})
}

// TestTheReachedPersistWriteStaysValidAgainstAStoredLegacyObject drives the production persist
// caller against a stored object an older version could have written, and hands the result to the
// schema a real API server would use. The stored object is checked on its own first, so a later
// failure cannot be the fixture, and the emitted patch is merged the way a server merges it rather
// than replaced, because a patch is a delta. Both interceptors are installed: an update is the
// pre-existing writer shape and a patch is the current one, so a regression back to a full-object
// write is still observed instead of silently passing because nothing was intercepted.
//
// The subject is a write that only moves the operation one phase on. A stored role status that
// omits fields the current typed struct models is exactly what a real server can still be holding,
// and such a write must not be refused for a field it never mentioned.
func TestTheReachedPersistWriteStaysValidAgainstAStoredLegacyObject(t *testing.T) {
	crd := workercore.GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd)
	require.Len(t, crd.Spec.Versions, 1)
	schema := new(apiextensions.JSONSchemaProps)
	require.NoError(t, extension.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil))
	validator, _, err := apiservervalidation.NewSchemaValidator(schema)
	require.NoError(t, err)
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			var stored map[string]any
			require.NoError(t, json.Unmarshal([]byte(`{
  "apiVersion": "worker.gpustack.ai/v1alpha1",
  "kind": "ModelDeployment",
  "metadata": {
    "name": "demo",
    "namespace": "default",
    "uid": "deployment-uid",
    "generation": 1
  },
  "spec": {
    "model": {
      "name": "qwen-demo",
      "artifactRef": {
        "name": "weights"
      }
    },
    "engine": {
      "name": "vLLM"
    },
    "roles": [
      {
        "name": "s0",
        "instanceType": "gpu-small",
        "kind": "Server",
        "size": 1
      }
    ]
  },
  "status": {
    "roleSummary": "S1",
    "roles": [
      {
        "name": "s0",
        "desired": 1,
        "ready": 1,
        "quotaReserved": 1,
        "unmanaged": false,
        "kind": "Server",
        "assignedFlavors": [
          "gpu-a"
        ],
        "parallelism": {
          "declared": {
            "tensorParallel": 2,
            "pipelineParallel": 1
          },
          "modes": {
            "tensor": true
          },
          "loadBalance": "Internal",
          "source": {
            "kind": "ExtraArgs",
            "complete": true
          }
        },
        "endpoints": {
          "eligible": 1,
          "serving": {
            "state": "Confirmed",
            "value": 1
          }
        }
      }
    ],
    "retirement": {
      "roleName": "s0",
      "replicaOrdinal": 0,
      "observedGeneration": 1,
      "targetWorkloadUID": "workload-uid",
      "targetMemberUIDs": [
        "member-uid"
      ],
      "state": "Admitted",
      "startedAt": "2026-10-01T00:00:00Z",
      "deadline": "2026-10-01T01:00:00Z",
      "phaseStartedAt": "2026-10-01T00:00:00Z"
    }
  }
}`), &stored))
			status := stored["status"].(map[string]any)
			reservationJSON, err := json.Marshal(status["retirement"])
			require.NoError(t, err)
			next := new(workercore.ModelDeploymentRetirementStatus)
			require.NoError(t, json.Unmarshal(reservationJSON, next))
			delete(status, "retirement")
			if legacy {
				role := status["roles"].([]any)[0].(map[string]any)
				delete(role, "parallelism")
				delete(role, "endpoints")
			}
			require.Empty(t, apiservervalidation.ValidateCustomResource(field.NewPath("object"), stored, validator), "raw stored control must actually be valid")
			raw, err := json.Marshal(stored)
			require.NoError(t, err)
			md := new(workercore.ModelDeployment)
			require.NoError(t, json.Unmarshal(raw, md))
			writes := 0
			funcs := ctrlinterceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
					require.Equal(t, "status", sub)
					writes++
					wire, err := json.Marshal(obj)
					require.NoError(t, err)
					var submitted map[string]any
					require.NoError(t, json.Unmarshal(wire, &submitted))
					stored["status"] = submitted["status"]
					errs := apiservervalidation.ValidateCustomResource(field.NewPath("object"), stored, validator)
					if len(errs) > 0 {
						return errs.ToAggregate()
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption) error {
					require.Equal(t, "status", sub)
					require.Equal(t, ctrlcli.MergeFrom(md).Type(), patch.Type())
					writes++
					data, err := patch.Data(obj)
					require.NoError(t, err)
					var actualPatch map[string]any
					require.NoError(t, json.Unmarshal(data, &actualPatch))
					metadata, ok := actualPatch["metadata"].(map[string]any)
					require.True(t, ok, "actual optimistic patch must carry metadata")
					require.Equal(t, stored["metadata"].(map[string]any)["resourceVersion"], metadata["resourceVersion"], "compare actual emitted RV with the independently fresh-read base")
					require.Equal(t, md.ResourceVersion, metadata["resourceVersion"])
					base, err := json.Marshal(stored)
					require.NoError(t, err)
					merged, err := jsonpatch.MergePatch(base, data)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(merged, &stored))
					errs := apiservervalidation.ValidateCustomResource(field.NewPath("object"), stored, validator)
					if len(errs) > 0 {
						return errs.ToAggregate()
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}
			cli := newRetirementInterceptedClient(funcs, md)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), md))
			require.NotEmpty(t, md.ResourceVersion)
			stored["metadata"].(map[string]any)["resourceVersion"] = md.ResourceVersion
			r := &ModelDeploymentReconciler{Client: cli}
			err = r.persistModelDeploymentRetirement(context.Background(), md, nil, &modelDeploymentRetirementPlan{Reservation: next})
			require.NoError(t, err, "actual reached retirement status write must remain accepted by the real generated CRD validator")
			require.Equal(t, 1, writes, "no write is a vacuous compatibility check")
			require.NotNil(t, stored["status"].(map[string]any)["retirement"])
		})
	}
}
