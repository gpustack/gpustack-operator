package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// artifactDeployment is modelDeployment naming an artifact, with the one role mutated.
func artifactDeployment(engine string, mutate func(*workercore.ModelDeploymentRole)) *workercore.ModelDeployment {
	md := modelDeployment(engine, role(mutate))
	md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "qwen"}

	return md
}

func fieldsOf(errs field.ErrorList) []string {
	fields := make([]string, 0, len(errs))
	for _, e := range errs {
		fields = append(fields, e.Field)
	}

	return fields
}

func TestValidateModelDeploymentServedModelName(t *testing.T) {
	const served = "Qwen/Qwen2.5-72B-Instruct"
	cases := []struct {
		name      string
		engine    string
		args      []string
		command   []string
		noRef     bool
		wantField string
	}{
		{name: "absent", engine: workercore.ModelDeploymentEngineVLLM},
		{name: "the served name", engine: workercore.ModelDeploymentEngineVLLM, args: []string{"--served-model-name", served}},
		{name: "the served name inline", engine: workercore.ModelDeploymentEngineSGLang, args: []string{"--served-model-name=" + served}},
		{
			name: "another name on vLLM", engine: workercore.ModelDeploymentEngineVLLM,
			args: []string{"--served-model-name", "other"}, wantField: "spec.roles[0].extraArgs",
		},
		{
			name: "another name on SGLang", engine: workercore.ModelDeploymentEngineSGLang,
			args: []string{"--served-model-name", "other"}, wantField: "spec.roles[0].extraArgs",
		},
		{
			name: "a second name on vLLM", engine: workercore.ModelDeploymentEngineVLLM,
			args: []string{"--served-model-name", served, "alias", "--max-model-len", "4096"}, wantField: "spec.roles[0].extraArgs",
		},
		{
			name: "the underscore spelling vLLM rewrites", engine: workercore.ModelDeploymentEngineVLLM,
			args: []string{"--served_model_name=other"}, wantField: "spec.roles[0].extraArgs",
		},
		{
			name: "held without an artifact too", engine: workercore.ModelDeploymentEngineVLLM, noRef: true,
			args: []string{"--served-model-name", "other"}, wantField: "spec.roles[0].extraArgs",
		},
		{
			name: "a take-over role is not judged", engine: workercore.ModelDeploymentEngineVLLM,
			command: []string{"vllm", "serve", "x", "--served-model-name", "other"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := artifactDeployment(c.engine, func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs, r.Command = c.args, c.command
			})
			if c.noRef {
				md.Spec.Model.ArtifactRef = nil
			}

			errs := validateModelDeploymentRoleServedModelName(md, &md.Spec.Roles[0], field.NewPath("spec", "roles").Index(0))
			if c.wantField == "" {
				assert.Empty(t, errs)
				return
			}
			assert.Equal(t, []string{c.wantField}, fieldsOf(errs))
		})
	}
}

func TestValidateModelDeploymentArtifactOwnedKeys(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		args      []string
		env       []workercore.ModelDeploymentEnvVar
		command   []string
		noRef     bool
		wantField []string
	}{
		{name: "nothing owned", engine: workercore.ModelDeploymentEngineVLLM, args: []string{"--max-model-len", "4096"}},
		{
			name: "vLLM model, revision and download dir", engine: workercore.ModelDeploymentEngineVLLM,
			args:      []string{"--revision", "v2", "--model=/x", "--download-dir", "/tmp"},
			wantField: []string{"spec.roles[0].extraArgs[0]", "spec.roles[0].extraArgs[2]", "spec.roles[0].extraArgs[3]"},
		},
		{
			name: "vLLM tokenizer revision", engine: workercore.ModelDeploymentEngineVLLM,
			args: []string{"--tokenizer-revision=v2"}, wantField: []string{"spec.roles[0].extraArgs[0]"},
		},
		{
			name: "SGLang model path", engine: workercore.ModelDeploymentEngineSGLang,
			args: []string{"--model-path", "/x"}, wantField: []string{"spec.roles[0].extraArgs[0]"},
		},
		{
			name: "the Hub environment", engine: workercore.ModelDeploymentEngineVLLM,
			env: []workercore.ModelDeploymentEnvVar{
				{Name: "HF_TOKEN", Value: "x"}, {Name: "HTTPS_PROXY", Value: "http://p"}, {Name: "HF_HOME", Value: "/h"},
			},
			wantField: []string{"spec.roles[0].env[0]", "spec.roles[0].env[2]"},
		},
		{
			name: "without an artifact a revision is the user's", engine: workercore.ModelDeploymentEngineVLLM, noRef: true,
			args: []string{"--revision", "v2"}, env: []workercore.ModelDeploymentEnvVar{{Name: "HF_TOKEN", Value: "x"}},
		},
		{
			name: "a take-over role is not judged", engine: workercore.ModelDeploymentEngineVLLM,
			command: []string{"vllm", "serve", "x", "--revision", "v2"},
			env:     []workercore.ModelDeploymentEnvVar{{Name: "HF_TOKEN", Value: "x"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := artifactDeployment(c.engine, func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs, r.Env, r.Command = c.args, c.env, c.command
			})
			if c.noRef {
				md.Spec.Model.ArtifactRef = nil
			}

			errs := validateModelDeploymentRoleArtifactKeys(md, &md.Spec.Roles[0], field.NewPath("spec", "roles").Index(0))
			if len(c.wantField) == 0 {
				assert.Empty(t, errs)
				return
			}
			assert.Equal(t, c.wantField, fieldsOf(errs))
		})
	}
}

func TestValidateModelDeploymentReservedMountPaths(t *testing.T) {
	cm := &core.LocalObjectReference{Name: "extra"}
	cases := []struct {
		name  string
		path  string
		noRef bool
		want  bool
	}{
		{name: "an unrelated path", path: "/data"},
		{name: "a sibling sharing a prefix", path: "/var/lib/gpustack/models"},
		{name: "the weights", path: "/var/lib/gpustack/model", want: true},
		{name: "inside the weights", path: "/var/lib/gpustack/model/tokenizer", want: true},
		{name: "around the weights", path: "/var/lib/gpustack", want: true},
		{name: "the cache", path: "/var/lib/gpustack/model-cache", want: true},
		{name: "without an artifact nothing is reserved", path: "/var/lib/gpustack/model", noRef: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := artifactDeployment(workercore.ModelDeploymentEngineVLLM, func(r *workercore.ModelDeploymentRole) {
				r.AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{{MountPath: c.path, ConfigMap: cm}}
			})
			if c.noRef {
				md.Spec.Model.ArtifactRef = nil
			}

			errs := validateModelDeploymentRoleReservedMountPaths(md, &md.Spec.Roles[0], field.NewPath("spec", "roles").Index(0))
			if !c.want {
				assert.Empty(t, errs)
				return
			}
			assert.Equal(t, []string{"spec.roles[0].additionalVolumes[0].mountPath"}, fieldsOf(errs))
		})
	}
}

func TestValidateModelDeploymentArtifactRefIsFrozen(t *testing.T) {
	cases := []struct {
		name string
		old  *core.LocalObjectReference
		new  *core.LocalObjectReference
		want bool
	}{
		{name: "unchanged", old: &core.LocalObjectReference{Name: "a"}, new: &core.LocalObjectReference{Name: "a"}},
		{name: "re-pointed", old: &core.LocalObjectReference{Name: "a"}, new: &core.LocalObjectReference{Name: "b"}, want: true},
		{name: "added", new: &core.LocalObjectReference{Name: "a"}, want: true},
		{name: "removed", old: &core.LocalObjectReference{Name: "a"}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
			old.Spec.Model.ArtifactRef = c.old
			updated := old.DeepCopy()
			updated.Spec.Model.ArtifactRef = c.new

			errs := validateModelDeploymentIdentity(updated, old)
			if !c.want {
				assert.Empty(t, errs)
				return
			}
			assert.Equal(t, []string{"spec.model"}, fieldsOf(errs))
		})
	}
}

func TestValidateModelDeploymentServedModelNamesRatchets(t *testing.T) {
	stored := artifactDeployment(workercore.ModelDeploymentEngineVLLM, func(r *workercore.ModelDeploymentRole) {
		r.ExtraArgs = []string{"--served-model-name", "other"}
	})
	cases := []struct {
		name string
		edit func(*workercore.ModelDeployment)
		want bool
	}{
		{name: "a scale keeps the stored name", edit: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 5 }},
		{name: "a new wrong name is refused", edit: func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ExtraArgs = []string{"--served-model-name", "third"}
		}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			updated := stored.DeepCopy()
			c.edit(updated)
			errs := validateModelDeploymentServedModelNames(stored, updated)
			assert.Equal(t, c.want, len(errs) > 0, "%v", errs)
		})
	}
	assert.NotEmpty(t, validateModelDeploymentServedModelNames(nil, stored), "a create is judged")
}
