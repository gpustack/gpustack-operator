package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestModelDeploymentTableRendersTheColumns(t *testing.T) {
	table, err := modelDeploymentTable()
	require.NoError(t, err)

	md := &worker.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "sglang-epd-llmd"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: "SGLang"},
		},
		Status: workercore.ModelDeploymentStatus{
			RoleSummary: "1R1E1P1D",
			Phase:       "Ready",
			Endpoint:    "http://sglang-epd-llmd-router.default.svc:8000",
		},
	}
	got, err := table.ConvertToTable(context.Background(), md, nil)
	require.NoError(t, err)

	require.Len(t, got.Rows, 1)
	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	assert.Equal(t, []string{
		"sglang-epd-llmd",
		"SGLang",
		"1R1E1P1D",
		"Ready",
		"http://sglang-epd-llmd-router.default.svc:8000",
		"<unknown>",
	}, cells)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{"Name", "Engine", "Roles", "Phase", "Endpoint", "Age"}, headers)
}

func newTestModelDeploymentProxyHandler(
	t *testing.T, cli ctrlcli.WithWatch, reader ctrlcli.Reader,
) *ModelDeploymentHandler {
	t.Helper()
	table, err := modelDeploymentTable()
	require.NoError(t, err)
	h := &ModelDeploymentHandler{
		ObjectInfo: &worker.ModelDeployment{},
		apiReader:  reader,
	}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelDeployment, *worker.ModelDeploymentList,
		*workercore.ModelDeployment, *workercore.ModelDeploymentList,
	](table, h, cli, reader)
	return h
}

func testModelDeploymentRequestContext(ns string) context.Context {
	gvr := worker.SchemeGroupVersionResource("modeldeployments")
	return genericapirequest.WithRequestInfo(
		genericapirequest.WithNamespace(context.Background(), ns),
		&genericapirequest.RequestInfo{
			APIGroup:   gvr.Group,
			APIVersion: gvr.Version,
			Resource:   gvr.Resource,
			Namespace:  ns,
		},
	)
}

func newProxyTestCoreModelDeployment() *workercore.ModelDeployment {
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "qwen-pd",
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B"},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: "vllm/vllm-openai:v0.8.5",
			},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name:            "decode",
					Kind:            workercore.ModelDeploymentRoleKindDecode,
					Replicas:        2,
					AcceleratorType: "h20-8x",
				},
			},
		},
	}
}

func TestModelDeploymentHandler_ForgerProxyFenceOnCreate(t *testing.T) {
	_, hasCreateAdvice := any(new(ModelDeploymentHandler)).(extensionapi.CurdProxyOnCreateBeforeAdvice[*worker.ModelDeployment])
	assert.True(t, hasCreateAdvice, "ModelDeploymentHandler must implement CurdProxyOnCreateBeforeAdvice")
	_, hasUpdateAdvice := any(new(ModelDeploymentHandler)).(extensionapi.CurdProxyOnUpdateBeforeAdvice[*worker.ModelDeployment])
	assert.True(t, hasUpdateAdvice, "ModelDeploymentHandler must implement CurdProxyOnUpdateBeforeAdvice")

	cases := []struct {
		name          string
		annotations   map[string]string
		wantForbidden bool
	}{
		{
			name: "plain ModelDeployment without forger annotations is permitted",
		},
		{
			name: "ModelDeployment with unrelated annotations is permitted",
			annotations: map[string]string{
				"app.kubernetes.io/part-of": "serving",
			},
		},
		{
			name: "forger-binding annotation on v1 create is forbidden",
			annotations: map[string]string{
				workercore.ModelDeploymentForgerBindingAnnotationKey: `{"forgerName":"qwen-forger","forgerUID":"uid-1","term":1}`,
			},
			wantForbidden: true,
		},
		{
			name: "empty forger-binding annotation key on v1 create is forbidden",
			annotations: map[string]string{
				workercore.ModelDeploymentForgerBindingAnnotationKey: "",
			},
			wantForbidden: true,
		},
		{
			name: "forger-decision annotation on v1 create is forbidden",
			annotations: map[string]string{
				workercore.ModelDeploymentForgerDecisionAnnotationKey: `{"decisionID":"d-1","term":1}`,
			},
			wantForbidden: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			h := newTestModelDeploymentProxyHandler(t, fakeCli, fakeCli)
			reqCtx := testModelDeploymentRequestContext("team-a")

			coreMD := newProxyTestCoreModelDeployment()
			coreMD.Annotations = tc.annotations
			pubMD := (*worker.ModelDeployment)(coreMD)

			_, err := h.Create(reqCtx, pubMD, nil, &meta.CreateOptions{})
			if tc.wantForbidden {
				require.Error(t, err)
				assert.True(t, kerrors.IsForbidden(err), "expected Forbidden, got %v", err)
				stored := new(workercore.ModelDeployment)
				assert.True(t, kerrors.IsNotFound(fakeCli.Get(context.Background(), ctrlcli.ObjectKey{
					Namespace: "team-a", Name: "qwen-pd",
				}, stored)))
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestModelDeploymentHandler_ForgerProxyFenceOnUpdate(t *testing.T) {
	liveForger := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "qwen-forger",
			UID:       "forger-uid-1",
		},
	}
	recreatedForger := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "qwen-forger",
			UID:       "forger-uid-2",
		},
	}
	validBinding := `{"forgerName":"qwen-forger","forgerUID":"forger-uid-1","term":1}`

	cases := []struct {
		name          string
		extraObjs     []ctrlcli.Object
		setupExisting func(*workercore.ModelDeployment)
		mutateUpdate  func(*worker.ModelDeployment)
		wantForbidden bool
	}{
		{
			name: "unbound deployment allows setting kernelArtifactRef on v1 update",
			setupExisting: func(*workercore.ModelDeployment) {
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-1"}
			},
		},
		{
			name: "unbound deployment forbids adding forger-binding on v1 update",
			setupExisting: func(*workercore.ModelDeployment) {
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			wantForbidden: true,
		},
		{
			name: "unbound deployment forbids adding forger-decision on v1 update",
			setupExisting: func(*workercore.ModelDeployment) {
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerDecisionAnnotationKey: `{"decisionID":"d-1","term":1}`,
				}
			},
			wantForbidden: true,
		},
		{
			name: "unbound deployment allows clearing leftover forger-decision annotation",
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerDecisionAnnotationKey: `{"decisionID":"d-1","term":1}`,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				delete(md.Annotations, workercore.ModelDeploymentForgerDecisionAnnotationKey)
			},
		},
		{
			name:      "bound deployment forbids tenant bypass setting kernelArtifactRef + matching forger-decision",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-evil"}
				md.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey] = `{"decisionID":"x","term":1}`
			},
			wantForbidden: true,
		},
		{
			name:      "bound deployment forbids changing kernelArtifactRef alone",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-evil"}
			},
			wantForbidden: true,
		},
		{
			name:      "bound deployment forbids clearing kernelArtifactRef",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].KernelArtifactRef = nil
			},
			wantForbidden: true,
		},
		{
			name:      "bound deployment forbids role-set rename or addition",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].Name = "prefill"
			},
			wantForbidden: true,
		},
		{
			name:      "bound deployment forbids modifying forger-binding",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] = `{"forgerName":"other","forgerUID":"uid-9","term":2}`
			},
			wantForbidden: true,
		},
		{
			name: "bound deployment forbids replacing orphaned binding with corrupt JSON",
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] = "{not-json"
			},
			wantForbidden: true,
		},
		{
			name:      "bound deployment allows editing orthogonal fields such as replicas, image, and pause",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 4
				md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.9.0"
				md.Spec.Roles[0].Pause = true
			},
		},
		{
			name:      "bound deployment forbids removing forger-binding while owning forger still exists",
			extraObjs: []ctrlcli.Object{liveForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				delete(md.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)
			},
			wantForbidden: true,
		},
		{
			name: "orphan escape hatch allows removing forger-binding when owning forger was deleted",
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey:  validBinding,
					workercore.ModelDeploymentForgerDecisionAnnotationKey: `{"decisionID":"d-1","term":1}`,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				delete(md.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)
				delete(md.Annotations, workercore.ModelDeploymentForgerDecisionAnnotationKey)
			},
		},
		{
			name:      "orphan escape hatch allows removing forger-binding when owning forger was recreated with new UID",
			extraObjs: []ctrlcli.Object{recreatedForger},
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				delete(md.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)
			},
		},
		{
			name: "orphan escape hatch still forbids mutating kernelArtifactRef in the same unbind write",
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				delete(md.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)
				md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
			},
			wantForbidden: true,
		},
		{
			name: "corrupt existing forger-binding refuses v1 update",
			setupExisting: func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey: "{corrupt",
				}
			},
			mutateUpdate: func(md *worker.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 3
			},
			wantForbidden: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := newProxyTestCoreModelDeployment()
			tc.setupExisting(existing)

			objs := append([]ctrlcli.Object{existing}, tc.extraObjs...)
			fakeCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
			h := newTestModelDeploymentProxyHandler(t, fakeCli, fakeCli)
			reqCtx := testModelDeploymentRequestContext("team-a")

			stored := new(workercore.ModelDeployment)
			require.NoError(t, fakeCli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(existing), stored))

			updated := (*worker.ModelDeployment)(stored.DeepCopy())
			tc.mutateUpdate(updated)

			_, _, err := h.Update(reqCtx, updated.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, &meta.UpdateOptions{})
			if tc.wantForbidden {
				require.Error(t, err)
				assert.True(t, kerrors.IsForbidden(err), "expected Forbidden, got %v", err)
				return
			}
			require.NoError(t, err)
		})
	}

	t.Run("nil apiReader or reader failure fails closed with InternalError", func(t *testing.T) {
		existing := newProxyTestCoreModelDeployment()
		existing.Annotations = map[string]string{
			workercore.ModelDeploymentForgerBindingAnnotationKey: validBinding,
		}
		fakeCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(existing).Build()

		// Nil apiReader on handler fails closed when OnUpdate runs.
		table, err := modelDeploymentTable()
		require.NoError(t, err)
		nilReaderHandler := &ModelDeploymentHandler{ObjectInfo: &worker.ModelDeployment{}}
		nilReaderHandler.CurdOperations = extensionapi.WithCurdProxy[
			*worker.ModelDeployment, *worker.ModelDeploymentList,
			*workercore.ModelDeployment, *workercore.ModelDeploymentList,
		](table, nilReaderHandler, fakeCli, fakeCli)

		stored := new(workercore.ModelDeployment)
		require.NoError(t, fakeCli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(existing), stored))
		updated := (*worker.ModelDeployment)(stored.DeepCopy())
		delete(updated.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)

		reqCtx := testModelDeploymentRequestContext("team-a")
		_, _, err = nilReaderHandler.Update(reqCtx, updated.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, &meta.UpdateOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsInternalError(err), "expected InternalError, got %v", err)

		// Reader error when checking orphan ModelDeploymentForger fails closed with InternalError.
		errReader := ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(existing).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, client ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
					if _, isForger := obj.(*workercore.ModelDeploymentForger); isForger {
						return errors.New("etcd timeout")
					}
					return client.Get(ctx, key, obj, opts...)
				},
			}).
			Build()
		errHandler := newTestModelDeploymentProxyHandler(t, fakeCli, errReader)
		_, _, err = errHandler.Update(reqCtx, updated.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, &meta.UpdateOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsInternalError(err), "expected InternalError, got %v", err)
	})
}
