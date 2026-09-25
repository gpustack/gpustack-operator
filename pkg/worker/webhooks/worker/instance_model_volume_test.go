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

// TestInstanceModelVolumeAdmitsEveryArtifactSource pins that a hub artifact is admitted: the node
// delivers it, and whether the node can is a runtime fact the Instance waits on in its status. The
// shape rules still refuse, which is the baseline that shows admission ran.
func TestInstanceModelVolumeAdmitsEveryArtifactSource(t *testing.T) {
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
		subPath string
		wantErr bool
	}{
		{name: "a claim artifact", objs: []ctrlcli.Object{artifact(true)}},
		{name: "a hub artifact", objs: []ctrlcli.Object{artifact(false)}},
		{name: "an artifact that does not exist yet"},
		{name: "a hub artifact with a sub path", objs: []ctrlcli.Object{artifact(false)}, subPath: "x", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := webhookInstance("a", "missing")
			inst.Spec.Stop = true
			for _, o := range c.objs {
				o.SetNamespace(inst.Namespace)
			}
			v := modelVolume("/models", "qwen")
			v.SubPath = c.subPath
			inst.Spec.AdditionalVolumes = []workercore.InstanceAdditionalVolume{v}

			_, err := newInstanceWebhook(c.objs...).ValidateCreate(context.Background(), inst)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
