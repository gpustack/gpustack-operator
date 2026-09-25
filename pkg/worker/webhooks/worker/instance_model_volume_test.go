package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func modelVolume(path, artifact string) workercore.InstanceAdditionalVolume {
	return workercore.InstanceAdditionalVolume{
		MountPath: path,
		Model:     &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: artifact}},
	}
}

func TestInstanceModelVolumeShape(t *testing.T) {
	cases := []struct {
		name    string
		volumes []workercore.InstanceAdditionalVolume
		wantErr bool
	}{
		{name: "a model volume", volumes: []workercore.InstanceAdditionalVolume{modelVolume("/models", "qwen")}},
		{name: "two at distinct paths", volumes: []workercore.InstanceAdditionalVolume{modelVolume("/a", "qwen"), modelVolume("/b", "llama")}},
		{name: "an empty artifact name", volumes: []workercore.InstanceAdditionalVolume{modelVolume("/models", "")}, wantErr: true},
		{
			name: "a model volume with a sub path",
			volumes: func() []workercore.InstanceAdditionalVolume {
				v := modelVolume("/models", "qwen")
				v.SubPath = "x"
				return []workercore.InstanceAdditionalVolume{v}
			}(),
			wantErr: true,
		},
		{
			name: "a model volume beside another source",
			volumes: func() []workercore.InstanceAdditionalVolume {
				v := modelVolume("/models", "qwen")
				v.Persistent = &core.LocalObjectReference{Name: "data"}
				return []workercore.InstanceAdditionalVolume{v}
			}(),
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := webhookInstance("a", "generic-type")
			inst.Spec.VolumeMount = "/workspace"
			inst.Spec.AdditionalVolumes = c.volumes

			errs := validateAdditionalVolumes(inst)
			assert.Equal(t, c.wantErr, len(errs) > 0, "%v", errs)
		})
	}
}

func TestInstanceModelVolumeArtifactSource(t *testing.T) {
	artifact := func(claim bool) *workercore.ModelArtifact {
		ma := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "qwen"}}
		if claim {
			ma.Spec.Source.PersistentVolumeClaim = &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "models"}
		} else {
			ma.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{Repository: "Qwen/Qwen2.5-7B-Instruct", Revision: "main"}
		}
		return ma
	}
	cases := []struct {
		name    string
		objs    []ctrlcli.Object
		wantErr bool
	}{
		{name: "a claim artifact", objs: []ctrlcli.Object{artifact(true)}},
		{name: "an artifact that does not exist yet"},
		{name: "a hub artifact", objs: []ctrlcli.Object{artifact(false)}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := webhookInstance("a", "generic-type")
			for _, o := range c.objs {
				o.SetNamespace(inst.Namespace)
			}
			w := newInstanceWebhook(c.objs...)
			inst.Spec.AdditionalVolumes = []workercore.InstanceAdditionalVolume{modelVolume("/models", "qwen")}

			errs, err := w.validateInstanceModelVolumes(context.Background(), inst)
			require.NoError(t, err)
			if !c.wantErr {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, "spec.additionalVolumes[0].model.artifactRef", errs[0].Field)
		})
	}
}
