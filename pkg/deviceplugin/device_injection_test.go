package deviceplugin

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
)

// testInjectionConfig is one manufacturer's vocabulary, standing in for every manufacturer's: nothing
// in the resolver reads these three strings for anything but composing what it emits.
var testInjectionConfig = InjectionConfig{
	Manufacturer:      "nvidia",
	CDIKind:           testCDIKind,
	VisibleDevicesEnv: "NVIDIA_VISIBLE_DEVICES",
}

// redirectNodeFacts points both the specification directories and the engine-configuration directory
// at one temporary tree, and returns its root. Fixture files go under "etc/cdi", "run/cdi" and
// "engine".
func redirectNodeFacts(t *testing.T) string {
	t.Helper()
	root := redirectCDISpecDirs(t)
	t.Setenv(ContainerdConfigDirEnv, filepath.Join(root, "engine"))

	return root
}

func TestInjectionStrategyEnv(t *testing.T) {
	assert.Equal(t, "GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY", InjectionStrategyEnv("nvidia"))
	assert.Equal(t, "GPUSTACK_CAMBRICON_DEVICE_INJECTION_STRATEGY", InjectionStrategyEnv("cambricon"))
}

func TestParseInjectionStrategy(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    InjectionStrategy
		wantErr bool
	}{
		{name: "unset keeps today's behaviour", value: "", want: InjectionEnvvar},
		{name: "whitespace is trimmed", value: "  envvar  ", want: InjectionEnvvar},
		{name: "the environment variable channel", value: "envvar", want: InjectionEnvvar},
		{name: "the CDI channel", value: "cdi-annotations", want: InjectionCDI},
		{name: "detection", value: "auto", want: InjectionAuto},
		{
			// A deployment mistake, not something to paper over: the two spellings differ by which
			// channel carries the grant, so guessing one would be guessing whether the container gets an
			// accelerator at all.
			name:    "an unrecognized value is refused rather than defaulted",
			value:   "cdi",
			wantErr: true,
		},
		{name: "the typed CRI field is not an option", value: "cdi-cri", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseInjectionStrategy(c.value)
			if c.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.value)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestNewInjectionResolverReadsTheEnvironment(t *testing.T) {
	env := InjectionStrategyEnv(testInjectionConfig.Manufacturer)

	t.Setenv(env, "cdi-annotations")
	r, err := NewInjectionResolver(testInjectionConfig)
	require.NoError(t, err)
	assert.Equal(t, InjectionCDI, r.strategy)

	t.Setenv(env, "nonsense")
	_, err = NewInjectionResolver(testInjectionConfig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), env, "the message names the variable that has to be fixed")

	assert.Equal(t, InjectionEnvvar, DefaultInjectionResolver(testInjectionConfig).strategy)
}

// testPodWithRuntimeClass builds a Pod naming a runtime handler, or naming none when the name is
// empty.
func testPodWithRuntimeClass(name string) *core.Pod {
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default"}}
	if name != "" {
		pod.Spec.RuntimeClassName = &name
	}

	return pod
}

// TestResolveAuto covers what auto is for: it takes the CDI channel only when every fact it needs says
// so, and otherwise keeps today's behavior with the reason recorded. Each refusal below is a node
// configuration on which a CDI request would reach nothing.
func TestResolveAuto(t *testing.T) {
	const engineRunc = "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
		"  default_runtime_name = \"runc\"\n"

	cases := []struct {
		name         string
		files        map[string]string
		runtimeClass string
		ids          []string
		want         InjectionStrategy
		wantEvidence string
	}{
		{
			name: "every fact agrees",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpecYAML,
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionCDI,
			wantEvidence: "name every granted accelerator",
		},
		{
			// The node whose generator was run by hand keeps its only specification here, and the engine
			// reads it, so this resolver has to as well or auto never engages on such a node.
			name: "a specification in the static directory counts",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"etc/cdi/nvidia.yaml": testCDISpecYAML,
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionCDI,
			wantEvidence: "name every granted accelerator",
		},
		{
			// The Pod asked for a handler, and this resolver cannot read that handler's configuration to
			// know what it will inject. Leave it to the handler.
			name: "the pod names a runtime class",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpecYAML,
			},
			runtimeClass: "nvidia",
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "runtimeClassName",
		},
		{
			name:         "the engine configuration cannot be read",
			files:        map[string]string{"run/cdi/nvidia.yaml": testCDISpecYAML},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "could not be read",
		},
		{
			// A CDI request here would be a second injection path for one container, because the default
			// handler already reads the environment variable.
			name: "the engine's default handler is a vendor runtime",
			files: map[string]string{
				"engine/config.toml": "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"nvidia\"\n",
				"run/cdi/nvidia.yaml": testCDISpecYAML,
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "vendor runtime",
		},
		{
			name: "the engine does not resolve CDI",
			files: map[string]string{
				"engine/config.toml": "version = 2\n\n[plugins.\"io.containerd.grpc.v1.cri\"]\n" +
					"  enable_cdi = false\n",
				"run/cdi/nvidia.yaml": testCDISpecYAML,
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "does not resolve CDI",
		},
		{
			// The specification exists but was generated with a naming strategy that carries no
			// accelerator identifier. Falling back to the index or to "all" would widen the grant.
			name: "no specification names the accelerator by the identifier we would request",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpec(testCDIKind, `"0"`, "all"),
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "no loaded CDI specification names",
		},
		{
			// A request naming a device the specifications do not carry fails the whole container, so a
			// partial match is worth no more than none.
			name: "only some of the granted accelerators are named",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpecYAML,
			},
			ids:          []string{testCDIUUID0, testCDIUUID1},
			want:         InjectionEnvvar,
			wantEvidence: "no loaded CDI specification names",
		},
		{
			name:         "an empty CDI directory is not evidence of CDI",
			files:        map[string]string{"engine/config.toml": engineRunc, "run/cdi/.keep": ""},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "no loaded CDI specification names",
		},
		{
			// A specification that could not be parsed is an unknown, and auto only moves a node off
			// today's behavior on a positive fact. Without this the malformed file would read as "the
			// node names nothing", which is an answer rather than a gap.
			name: "a specification that could not be parsed is an unknown, not an absence",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpec(testCDIKind, testCDIUUID1),
				"run/cdi/broken.yaml": "\tthis: is: not: yaml\n",
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionEnvvar,
			wantEvidence: "could not be read",
		},
		{
			// The other side of the same coin. These directories are shared with every other
			// manufacturer's generator, so a malformed specification describing hardware this operator
			// has never heard of must not be able to switch a working node back: a name that was found
			// is a positive fact, and no file that could not be read can unfind it.
			name: "an unreadable specification does not undo a name that was found",
			files: map[string]string{
				"engine/config.toml":  engineRunc,
				"run/cdi/nvidia.yaml": testCDISpecYAML,
				"etc/cdi/broken.yaml": "\tsome: other: vendor\n",
			},
			ids:          []string{testCDIUUID0},
			want:         InjectionCDI,
			wantEvidence: "name every granted accelerator",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			root := redirectNodeFacts(t)
			for rel, content := range c.files {
				writeTestFile(t, filepath.Join(root, rel), content)
			}

			r := &InjectionResolver{cfg: testInjectionConfig, strategy: InjectionAuto}
			got, evidence := r.resolve(testPodWithRuntimeClass(c.runtimeClass), c.ids)
			assert.Equal(t, c.want, got)
			assert.Contains(t, evidence, c.wantEvidence)
		})
	}
}

// TestApplyChannels pins that exactly one channel ever carries the grant. Two live injection paths for
// one container is how a container ends up holding hardware nobody granted it.
func TestApplyChannels(t *testing.T) {
	const cdiAnnotation = "cdi.k8s.io/gpustack-nvidia"

	newResolver := func(s InjectionStrategy) *InjectionResolver {
		return &InjectionResolver{cfg: testInjectionConfig, strategy: s}
	}

	t.Run("envvar carries the identifiers and no annotation", func(t *testing.T) {
		redirectNodeFacts(t)
		resp := &ContainerAllocateResponse{}

		require.NoError(t, newResolver(InjectionEnvvar).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0, testCDIUUID1}))
		assert.Equal(t, testCDIUUID0+","+testCDIUUID1, resp.Envs["NVIDIA_VISIBLE_DEVICES"])
		assert.Empty(t, resp.Annotations)
	})

	t.Run("cdi-annotations carries the names and no visible-devices env", func(t *testing.T) {
		root := redirectNodeFacts(t)
		writeTestFile(t, filepath.Join(root, "run/cdi/nvidia.yaml"), testCDISpecYAML)
		resp := &ContainerAllocateResponse{}

		require.NoError(t, newResolver(InjectionCDI).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0}))
		assert.Equal(t, map[string]string{cdiAnnotation: testCDIKind + "=" + testCDIUUID0}, resp.Annotations)
		assert.NotContains(t, resp.Envs, "NVIDIA_VISIBLE_DEVICES",
			"the legacy hook must not be handed a second chance to inject")
	})

	t.Run("cdi-annotations preserves the envs it is applied over", func(t *testing.T) {
		root := redirectNodeFacts(t)
		writeTestFile(t, filepath.Join(root, "run/cdi/nvidia.yaml"), testCDISpecYAML)
		resp := &ContainerAllocateResponse{Envs: map[string]string{"CUDA_DEVICE_SM_LIMIT": "25"}}

		require.NoError(t, newResolver(InjectionCDI).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0}))
		assert.Equal(t, "25", resp.Envs["CUDA_DEVICE_SM_LIMIT"])
	})

	t.Run("nothing readable is handed to the engine rather than refused", func(t *testing.T) {
		// A node whose specification directories are not mounted into this device manager presents an
		// empty set. That is absence of evidence, and refusing on it would break a working node; the
		// engine fails closed on a name it cannot resolve anyway.
		redirectNodeFacts(t)
		resp := &ContainerAllocateResponse{}

		require.NoError(t, newResolver(InjectionCDI).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0}))
		assert.Equal(t, testCDIKind+"="+testCDIUUID0, resp.Annotations[cdiAnnotation])
	})

	t.Run("an unreadable specification elsewhere does not fail a name that was found", func(t *testing.T) {
		// Explicitly configured, so validation runs — but a malformed file belonging to some other
		// vendor must not turn a name this DID find into an Allocate failure.
		root := redirectNodeFacts(t)
		writeTestFile(t, filepath.Join(root, "run/cdi/nvidia.yaml"), testCDISpecYAML)
		writeTestFile(t, filepath.Join(root, "etc/cdi/broken.yaml"), "\tsome: other: vendor\n")
		resp := &ContainerAllocateResponse{}

		require.NoError(t, newResolver(InjectionCDI).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0}))
		assert.Equal(t, testCDIKind+"="+testCDIUUID0, resp.Annotations[cdiAnnotation])
	})

	t.Run("an unnamed accelerator fails the allocate rather than widening it", func(t *testing.T) {
		root := redirectNodeFacts(t)
		writeTestFile(t, filepath.Join(root, "run/cdi/nvidia.yaml"),
			testCDISpec(testCDIKind, `"0"`, "all"))
		resp := &ContainerAllocateResponse{}

		err := newResolver(InjectionCDI).Apply(klog.Background(), resp,
			testPodWithRuntimeClass(""), []string{testCDIUUID0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), testCDIUUID0)
		assert.Empty(t, resp.Annotations)
		assert.Empty(t, resp.Envs, "no fall back to the index or to all")
	})
}
