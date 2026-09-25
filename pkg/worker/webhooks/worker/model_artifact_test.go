package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func newTestHubArtifact(repository, revision string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			HuggingFace: &workercore.ModelArtifactHubSource{Repository: repository, Revision: revision},
		}},
	}
}

func newTestClaimArtifact(claim, path string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			PersistentVolumeClaim: &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: claim, Path: path},
		}},
	}
}

func TestModelArtifactWebhookDefault(t *testing.T) {
	cases := []struct {
		name string
		in   *workercore.ModelArtifact
		want string
	}{
		{name: "an unset revision becomes main", in: newTestHubArtifact("Qwen/Qwen2.5-7B-Instruct", ""), want: "main"},
		{name: "a stated revision is kept", in: newTestHubArtifact("Qwen/Qwen2.5-7B-Instruct", "v1.0"), want: "v1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, new(ModelArtifactWebhook).Default(context.Background(), c.in))
			assert.Equal(t, c.want, c.in.Spec.Source.HuggingFace.Revision)
		})
	}
}

func TestModelArtifactWebhookValidateCreate(t *testing.T) {
	cases := []struct {
		name      string
		in        *workercore.ModelArtifact
		wantField string
	}{
		{name: "an owner and a name", in: newTestHubArtifact("Qwen/Qwen2.5-7B-Instruct", "main")},
		{name: "a bare canonical name", in: newTestHubArtifact("gpt2", "main")},
		{name: "a commit", in: newTestHubArtifact("owner/repo", strings.Repeat("a", 40))},
		{name: "a pull-request ref", in: newTestHubArtifact("owner/repo", "refs/pr/1")},
		{name: "a claim at its root", in: newTestClaimArtifact("models", "")},
		{name: "a claim directory", in: newTestClaimArtifact("models", "qwen/7b")},
		{
			name:      "no source",
			in:        &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "x"}},
			wantField: "spec.source",
		},
		{
			name: "two sources",
			in: func() *workercore.ModelArtifact {
				ma := newTestHubArtifact("owner/repo", "main")
				ma.Spec.Source.PersistentVolumeClaim = &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "c"}
				return ma
			}(),
			wantField: "spec.source",
		},
		{
			name: "ModelScope",
			in: &workercore.ModelArtifact{Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
				ModelScope: &workercore.ModelArtifactHubSource{Repository: "qwen/Qwen2.5-7B-Instruct", Revision: "master"},
			}}},
			wantField: "spec.source.modelScope",
		},
		{name: "an empty repository", in: newTestHubArtifact("", "main"), wantField: "spec.source.huggingFace.repository"},
		{name: "three parts", in: newTestHubArtifact("a/b/c", "main"), wantField: "spec.source.huggingFace.repository"},
		{name: "a leading dash", in: newTestHubArtifact("owner/-repo", "main"), wantField: "spec.source.huggingFace.repository"},
		{name: "a double dot", in: newTestHubArtifact("owner/re..po", "main"), wantField: "spec.source.huggingFace.repository"},
		{name: "a URL", in: newTestHubArtifact("https://huggingface.co/owner/repo", "main"), wantField: "spec.source.huggingFace.repository"},
		{
			name:      "a part longer than 96",
			in:        newTestHubArtifact("owner/"+strings.Repeat("a", 97), "main"),
			wantField: "spec.source.huggingFace.repository",
		},
		{name: "an empty revision", in: newTestHubArtifact("owner/repo", ""), wantField: "spec.source.huggingFace.revision"},
		{name: "whitespace in the revision", in: newTestHubArtifact("owner/repo", "ma in"), wantField: "spec.source.huggingFace.revision"},
		{name: "a control character in the revision", in: newTestHubArtifact("owner/repo", "ma\x00in"), wantField: "spec.source.huggingFace.revision"},
		{name: "an empty claim", in: newTestClaimArtifact("", ""), wantField: "spec.source.persistentVolumeClaim.claimName"},
		{name: "an absolute path", in: newTestClaimArtifact("models", "/qwen"), wantField: "spec.source.persistentVolumeClaim.path"},
		{name: "a dot-dot element", in: newTestClaimArtifact("models", "qwen/../other"), wantField: "spec.source.persistentVolumeClaim.path"},
		{name: "patterns on a hub source", in: withPatterns(newTestHubArtifact("owner/repo", "main"),
			[]string{"*.safetensors", "*.json"}, []string{"original/"})},
		{
			name: "allow patterns on a claim", in: withPatterns(newTestClaimArtifact("models", ""), []string{"*.json"}, nil),
			wantField: "spec.allowPatterns",
		},
		{
			name: "ignore patterns on a claim", in: withPatterns(newTestClaimArtifact("models", ""), nil, []string{"*.bin"}),
			wantField: "spec.ignorePatterns",
		},
		{name: "more than 32 patterns", in: withPatterns(newTestHubArtifact("owner/repo", "main"),
			make33Patterns(), nil), wantField: "spec.allowPatterns"},
		{
			name: "an empty pattern", in: withPatterns(newTestHubArtifact("owner/repo", "main"), []string{"*.json", ""}, nil),
			wantField: "spec.allowPatterns[1]",
		},
		{name: "a pattern longer than 256", in: withPatterns(newTestHubArtifact("owner/repo", "main"), nil,
			[]string{strings.Repeat("a", 257)}), wantField: "spec.ignorePatterns[0]"},
		{name: "a control character in a pattern", in: withPatterns(newTestHubArtifact("owner/repo", "main"), nil,
			[]string{"a b", "a\tb"}), wantField: "spec.ignorePatterns[1]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(), c.in)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
		})
	}
}

func TestModelArtifactWebhookValidateUpdate(t *testing.T) {
	cases := []struct {
		name      string
		edit      func(*workercore.ModelArtifact)
		wantField string
	}{
		{name: "metadata changes", edit: func(ma *workercore.ModelArtifact) { ma.Labels = map[string]string{"a": "b"} }},
		{name: "status changes", edit: func(ma *workercore.ModelArtifact) {
			ma.Status.Resolved = &workercore.ModelArtifactResolved{Revision: strings.Repeat("a", 40)}
		}},
		{name: "a revision change", edit: func(ma *workercore.ModelArtifact) {
			ma.Spec.Source.HuggingFace.Revision = "v2"
		}, wantField: "spec"},
		{name: "a new Secret", edit: func(ma *workercore.ModelArtifact) {
			ma.Spec.Source.HuggingFace.SecretRef = &core.LocalObjectReference{Name: "hf-token"}
		}, wantField: "spec"},
		{name: "a pattern added", edit: func(ma *workercore.ModelArtifact) {
			ma.Spec.IgnorePatterns = []string{"original/"}
		}, wantField: "spec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := newTestHubArtifact("owner/repo", "main")
			updated := old.DeepCopy()
			c.edit(updated)

			_, err := new(ModelArtifactWebhook).ValidateUpdate(context.Background(), old, updated)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
		})
	}
}

// assertInvalidField asserts that err is an Invalid status whose causes name exactly the field.
func assertInvalidField(t *testing.T, err error, want string) {
	t.Helper()
	status, ok := err.(kerrors.APIStatus)
	require.True(t, ok, "not an API status: %v", err)
	require.True(t, kerrors.IsInvalid(err), "not Invalid: %v", err)
	causes := status.Status().Details.Causes
	fields := make([]string, 0, len(causes))
	for _, cause := range causes {
		fields = append(fields, cause.Field)
	}
	assert.Equal(t, []string{want}, fields)
}

func TestModelArtifactWebhookUpdateRatchets(t *testing.T) {
	t.Run("an unchanged spec a later rule would refuse is not judged again", func(t *testing.T) {
		old := newTestHubArtifact("a/b/c", "main")
		updated := old.DeepCopy()
		updated.Finalizers = []string{"worker.gpustack.ai/model-artifact-protection"}

		_, err := new(ModelArtifactWebhook).ValidateUpdate(context.Background(), old, updated)
		require.NoError(t, err)
	})
	t.Run("a replace omitting the defaulted revision is defaulted, not refused", func(t *testing.T) {
		old := newTestHubArtifact("owner/repo", "main")
		updated := newTestHubArtifact("owner/repo", "")

		require.NoError(t, new(ModelArtifactWebhook).Default(context.Background(), updated))
		_, err := new(ModelArtifactWebhook).ValidateUpdate(context.Background(), old, updated)
		require.NoError(t, err)
	})
}

func withPatterns(ma *workercore.ModelArtifact, allow, ignore []string) *workercore.ModelArtifact {
	ma.Spec.AllowPatterns, ma.Spec.IgnorePatterns = allow, ignore
	return ma
}

func make33Patterns() []string {
	patterns := make([]string, 33)
	for i := range patterns {
		patterns[i] = "*.p" + strings.Repeat("x", i)
	}
	return patterns
}
