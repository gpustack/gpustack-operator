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
	"gpustack.ai/gpustack/pkg/kubediscovery"
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

func newTestImageArtifact(reference string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			Image: &workercore.ModelArtifactImageSource{Reference: reference},
		}},
	}
}

func newTestModelScopeArtifact(repository, revision string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			ModelScope: &workercore.ModelArtifactHubSource{Repository: repository, Revision: revision},
		}},
	}
}

// TestModelArtifactWebhookImageSource covers the image member's shape rules and its capability
// gate, both directions, through the swappable version seam: the snapshot's Configure ignores
// later calls, so a test cannot re-point the snapshot itself.
func TestModelArtifactWebhookImageSource(t *testing.T) {
	defaultVersion := modelArtifactClusterVersion
	t.Cleanup(func() { modelArtifactClusterVersion = defaultVersion })
	supported := func() kubediscovery.Version { return kubediscovery.Version{GitVersion: "v1.35.5"} }
	unsupported := func() kubediscovery.Version { return kubediscovery.Version{GitVersion: "v1.32.9"} }

	cases := []struct {
		name      string
		in        *workercore.ModelArtifact
		reference string
		version   func() kubediscovery.Version
		wantField string
	}{
		{name: "a digest-pinned reference", reference: "registry.example.com/team/qwen@sha256:" + strings.Repeat("a", 64), version: supported},
		{name: "a host and a port", reference: "localhost:5500/qwen@sha256:" + strings.Repeat("a", 64), version: supported},
		{name: "a bare repository", reference: "qwen@sha256:" + strings.Repeat("a", 64), version: supported},
		{
			name: "an image source beside a hub source", version: supported, wantField: "spec.source",
			reference: "",
			in: func() *workercore.ModelArtifact {
				ma := newTestImageArtifact("registry/qwen@sha256:" + strings.Repeat("a", 64))
				ma.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{Repository: "owner/repo", Revision: "main"}
				return ma
			}(),
		},
		{name: "the image volume gate off", reference: "registry/qwen@sha256:" + strings.Repeat("a", 64), version: unsupported, wantField: "spec.source.image"},
		{
			name: "an unreadable cluster version", reference: "registry/qwen@sha256:" + strings.Repeat("a", 64),
			version:   func() kubediscovery.Version { return kubediscovery.Version{GitVersion: ""} },
			wantField: "spec.source.image",
		},
		{name: "a tag instead of a digest", reference: "registry.example.com/team/qwen:v1", version: supported, wantField: "spec.source.image.reference"},
		{name: "no digest at all", reference: "registry.example.com/team/qwen", version: supported, wantField: "spec.source.image.reference"},
		{name: "an uppercase digest", reference: "registry/qwen@sha256:" + strings.Repeat("A", 64), version: supported, wantField: "spec.source.image.reference"},
		{name: "a short digest", reference: "registry/qwen@sha256:" + strings.Repeat("a", 63), version: supported, wantField: "spec.source.image.reference"},
		{name: "the wrong algorithm", reference: "registry/qwen@sha512:" + strings.Repeat("a", 128), version: supported, wantField: "spec.source.image.reference"},
		{name: "an empty reference", reference: "", version: supported, wantField: "spec.source.image.reference"},
		{name: "only a digest", reference: "@sha256:" + strings.Repeat("a", 64), version: supported, wantField: "spec.source.image.reference"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelArtifactClusterVersion = c.version
			in := c.in
			if in == nil {
				in = newTestImageArtifact(c.reference)
			}

			_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(), in)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
		})
	}

	t.Run("patterns are refused on an image source", func(t *testing.T) {
		modelArtifactClusterVersion = supported
		_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(),
			withPatterns(newTestImageArtifact("registry/qwen@sha256:"+strings.Repeat("a", 64)), []string{"*.json"}, nil))
		require.Error(t, err)
		assertInvalidField(t, err, "spec.allowPatterns")
	})

	t.Run("the gate refusal names the cluster's version and what is missing", func(t *testing.T) {
		modelArtifactClusterVersion = unsupported
		_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(),
			newTestImageArtifact("registry/qwen@sha256:"+strings.Repeat("a", 64)))
		require.Error(t, err)
		status, ok := err.(kerrors.APIStatus)
		require.True(t, ok)
		message := status.Status().Details.Causes[0].Message
		assert.Contains(t, message, "does not serve image volumes")
		assert.Contains(t, message, "v1.32.9")
	})
}

func TestModelArtifactWebhookDefault(t *testing.T) {
	cases := []struct {
		name string
		in   *workercore.ModelArtifact
		want string
	}{
		{name: "an unset revision becomes main", in: newTestHubArtifact("Qwen/Qwen2.5-7B-Instruct", ""), want: "main"},
		{name: "a stated revision is kept", in: newTestHubArtifact("Qwen/Qwen2.5-7B-Instruct", "v1.0"), want: "v1.0"},
		{name: "a ModelScope revision becomes master", in: newTestModelScopeArtifact("Qwen/Qwen2.5-7B-Instruct", ""), want: "master"},
		{name: "a stated ModelScope revision is kept", in: newTestModelScopeArtifact("Qwen/Qwen2.5-7B-Instruct", "v1.0"), want: "v1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, new(ModelArtifactWebhook).Default(context.Background(), c.in))
			if c.in.Spec.Source.HuggingFace != nil {
				assert.Equal(t, c.want, c.in.Spec.Source.HuggingFace.Revision)
			} else {
				assert.Equal(t, c.want, c.in.Spec.Source.ModelScope.Revision)
			}
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
		{name: "a ModelScope repository", in: newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", "master")},
		{name: "a ModelScope bare name", in: newTestModelScopeArtifact("qwen", "master")},
		{name: "a ModelScope commit", in: newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", strings.Repeat("a", 40))},
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
			name: "a hub source beside a hub source",
			in: func() *workercore.ModelArtifact {
				ma := newTestHubArtifact("owner/repo", "main")
				ma.Spec.Source.ModelScope = &workercore.ModelArtifactHubSource{Repository: "qwen/Qwen2.5-7B-Instruct", Revision: "master"}
				return ma
			}(),
			wantField: "spec.source",
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
		{name: "a ModelScope empty repository", in: newTestModelScopeArtifact("", "master"), wantField: "spec.source.modelScope.repository"},
		{name: "a ModelScope three parts", in: newTestModelScopeArtifact("a/b/c", "master"), wantField: "spec.source.modelScope.repository"},
		{name: "a ModelScope URL", in: newTestModelScopeArtifact("https://modelscope.cn/qwen", "master"), wantField: "spec.source.modelScope.repository"},
		{name: "a ModelScope empty revision", in: newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", ""), wantField: "spec.source.modelScope.revision"},
		{name: "a ModelScope whitespace revision", in: newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", "ma in"), wantField: "spec.source.modelScope.revision"},
		{name: "an empty claim", in: newTestClaimArtifact("", ""), wantField: "spec.source.persistentVolumeClaim.claimName"},
		{name: "an absolute path", in: newTestClaimArtifact("models", "/qwen"), wantField: "spec.source.persistentVolumeClaim.path"},
		{name: "a dot-dot element", in: newTestClaimArtifact("models", "qwen/../other"), wantField: "spec.source.persistentVolumeClaim.path"},
		{name: "patterns on a hub source", in: withPatterns(newTestHubArtifact("owner/repo", "main"),
			[]string{"*.safetensors", "*.json"}, []string{"original/"})},
		{name: "patterns on a ModelScope source", in: withPatterns(newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", "master"),
			[]string{"*.safetensors"}, []string{"original/"})},
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

	t.Run("the union refusal names every accepted member", func(t *testing.T) {
		ma := newTestHubArtifact("owner/repo", "main")
		ma.Spec.Source.PersistentVolumeClaim = &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "c"}

		_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(), ma)
		require.Error(t, err)
		status, ok := err.(kerrors.APIStatus)
		require.True(t, ok)
		assert.Contains(t, status.Status().Details.Causes[0].Message, "modelScope")
	})
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

// TestModelArtifactWebhookExpectedDigest covers the anchor's admission rules: accepted on a hub
// source in the exact digest shape, refused on the sources whose identity does not come from a
// manifest, and immutable with the spec.
func TestModelArtifactWebhookExpectedDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	// The image-volume gate would otherwise answer before the anchor rule does; the snapshot's
	// Configure ignores later calls, so the seam is swapped here as the image test does.
	defaultVersion := modelArtifactClusterVersion
	t.Cleanup(func() { modelArtifactClusterVersion = defaultVersion })
	modelArtifactClusterVersion = func() kubediscovery.Version {
		return kubediscovery.Version{GitVersion: "v1.35.5"}
	}

	cases := []struct {
		name      string
		in        *workercore.ModelArtifact
		wantField string
	}{
		{name: "a Hugging Face artifact with an anchor", in: withDigest(newTestHubArtifact("owner/repo", "main"), good)},
		{name: "a ModelScope artifact with an anchor", in: withDigest(newTestModelScopeArtifact("qwen/Qwen2.5-7B-Instruct", "master"), good)},
		{
			name: "a claim source is refused the anchor", wantField: "spec.expectedDigest",
			in: withDigest(newTestClaimArtifact("models", ""), good),
		},
		{
			name: "an image source is refused the anchor", wantField: "spec.expectedDigest",
			in: withDigest(newTestImageArtifact("registry/qwen@sha256:"+strings.Repeat("b", 64)), good),
		},
		{name: "an uppercase digest", wantField: "spec.expectedDigest", in: withDigest(newTestHubArtifact("owner/repo", "main"), "sha256:"+strings.Repeat("A", 64))},
		{name: "a short digest", wantField: "spec.expectedDigest", in: withDigest(newTestHubArtifact("owner/repo", "main"), "sha256:"+strings.Repeat("a", 63))},
		{name: "no algorithm prefix", wantField: "spec.expectedDigest", in: withDigest(newTestHubArtifact("owner/repo", "main"), strings.Repeat("a", 64))},
		{name: "the wrong algorithm", wantField: "spec.expectedDigest", in: withDigest(newTestHubArtifact("owner/repo", "main"), "sha512:"+strings.Repeat("a", 128))},
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

	t.Run("the claim refusal says the anchor is a hub source's assertion", func(t *testing.T) {
		_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(),
			withDigest(newTestClaimArtifact("models", ""), good))
		require.Error(t, err)
		status, ok := err.(kerrors.APIStatus)
		require.True(t, ok)
		message := status.Status().Details.Causes[0].Message
		assert.Contains(t, message, "mount time")
		assert.Contains(t, message, "identity is the claim")
	})

	t.Run("the image refusal names the reference's digest as the identity", func(t *testing.T) {
		_, err := new(ModelArtifactWebhook).ValidateCreate(context.Background(),
			withDigest(newTestImageArtifact("registry/qwen@sha256:"+strings.Repeat("b", 64)), good))
		require.Error(t, err)
		status, ok := err.(kerrors.APIStatus)
		require.True(t, ok)
		message := status.Status().Details.Causes[0].Message
		assert.Contains(t, message, "reference's digest")
	})

	t.Run("an update changing only the anchor is a spec edit", func(t *testing.T) {
		old := newTestHubArtifact("owner/repo", "main")
		updated := old.DeepCopy()
		updated.Spec.ExpectedDigest = good

		_, err := new(ModelArtifactWebhook).ValidateUpdate(context.Background(), old, updated)
		require.Error(t, err)
		assertInvalidField(t, err, "spec")
	})
	t.Run("an update keeping the anchor is metadata only", func(t *testing.T) {
		old := withDigest(newTestHubArtifact("owner/repo", "main"), good)
		updated := old.DeepCopy()
		updated.Labels = map[string]string{"a": "b"}

		_, err := new(ModelArtifactWebhook).ValidateUpdate(context.Background(), old, updated)
		require.NoError(t, err)
	})
}

func withDigest(ma *workercore.ModelArtifact, digest string) *workercore.ModelArtifact {
	ma.Spec.ExpectedDigest = digest
	return ma
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
