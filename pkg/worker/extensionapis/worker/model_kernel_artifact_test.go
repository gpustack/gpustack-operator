package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestModelKernelArtifactHandlerAndTable(t *testing.T) {
	_, hasStatus := any(&worker.ModelKernelArtifact{}).(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "v1 ModelKernelArtifact must serve no writable status subresource")

	h := new(ModelKernelArtifactHandler)
	assert.IsType(t, &worker.ModelKernelArtifact{}, h.New())
	assert.IsType(t, &worker.ModelKernelArtifactList{}, h.NewList())
	assert.IsType(t, &workercore.ModelKernelArtifactList{}, h.NewListForProxy())

	public := &worker.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-kernels"},
	}
	coreObj := h.CastObjectTo(public)
	require.NotNil(t, coreObj)
	assert.Equal(t, "qwen-kernels", coreObj.Name)
	back := h.CastObjectFrom(context.Background(), coreObj)
	require.NotNil(t, back)
	assert.Equal(t, "qwen-kernels", back.Name)

	table, err := modelKernelArtifactTable()
	require.NoError(t, err)

	digest := "sha256:" + strings.Repeat("a", 64)
	artifact := &worker.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-kernels"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest: digest,
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Architecture: "sm_90a",
			},
		},
		Status: workercore.ModelKernelArtifactStatus{
			Resolved: &workercore.ModelKernelArtifactResolved{SizeBytes: 4096},
			Nodes:    &workercore.ModelArtifactNodes{Ready: 2},
			Conditions: []gpustack.Condition{
				{Type: workercore.ModelKernelArtifactConditionTypeResolved, Status: meta.ConditionTrue},
			},
		},
	}

	got, err := table.ConvertToTable(context.Background(), artifact, nil)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{
		"Name", "Source", "Engine", "Arch", "Size", "Ready", "Resolved", "Quarantined", "Age",
	}, headers)
}

func TestModelKernelArtifactHandler_BeforeOnCreate(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("b", 64)
	compat := workercore.ModelKernelArtifactCompatibility{
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Architecture: "sm_90a",
	}

	cases := []struct {
		name          string
		source        workercore.ModelKernelArtifactSource
		wantForbidden bool
	}{
		{
			name: "nodeStore source is forbidden on v1 create",
			source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         digest,
					DispatchManifestDigest: digest,
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-1",
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-forger",
						UID:  "forger-uid-1",
					},
				},
			},
			wantForbidden: true,
		},
		{
			name: "huggingFace source is permitted on v1 create",
			source: workercore.ModelKernelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{
					Repository: "gpustack/qwen-kernels",
					Revision:   strings.Repeat("1", 40),
				},
			},
		},
		{
			name: "image source is permitted on v1 create",
			source: workercore.ModelKernelArtifactSource{
				Image: &workercore.ModelArtifactImageSource{
					Reference: "registry.example.com/kernels:v1",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			table, err := modelKernelArtifactTable()
			require.NoError(t, err)
			h := &ModelKernelArtifactHandler{ObjectInfo: &worker.ModelKernelArtifact{}}
			h.CurdOperations = extensionapi.WithCurdProxy[
				*worker.ModelKernelArtifact, *worker.ModelKernelArtifactList,
				*workercore.ModelKernelArtifact, *workercore.ModelKernelArtifactList,
			](table, h, fakeCli, fakeCli)

			obj := &worker.ModelKernelArtifact{
				ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "artifact-1"},
				Spec: workercore.ModelKernelArtifactSpec{
					Source:        tc.source,
					Compatibility: compat,
				},
			}

			gvr := worker.SchemeGroupVersionResource("modelkernelartifacts")
			reqCtx := genericapirequest.WithRequestInfo(
				genericapirequest.WithNamespace(ctx, "team-a"),
				&genericapirequest.RequestInfo{
					APIGroup:   gvr.Group,
					APIVersion: gvr.Version,
					Resource:   gvr.Resource,
				},
			)
			err = h.BeforeOnCreate(reqCtx, obj, &ctrlcli.CreateOptions{})
			if tc.wantForbidden {
				require.Error(t, err)
				assert.True(t, kerrors.IsForbidden(err))
				assert.Contains(t, err.Error(), workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden)

				_, proxyErr := h.Create(reqCtx, obj, nil, &meta.CreateOptions{})
				require.Error(t, proxyErr)
				assert.True(t, kerrors.IsForbidden(proxyErr))
				assert.Contains(t, proxyErr.Error(), workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden)

				stored := new(workercore.ModelKernelArtifact)
				getErr := fakeCli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "artifact-1"}, stored)
				assert.True(t, kerrors.IsNotFound(getErr))
				return
			}

			require.NoError(t, err)
			created, proxyErr := h.Create(reqCtx, obj, nil, &meta.CreateOptions{})
			require.NoError(t, proxyErr)
			require.NotNil(t, created)

			stored := new(workercore.ModelKernelArtifact)
			require.NoError(t, fakeCli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "artifact-1"}, stored))
		})
	}

	h := new(ModelKernelArtifactHandler)
	assert.NoError(t, h.BeforeOnCreate(ctx, nil, &ctrlcli.CreateOptions{}))
}
