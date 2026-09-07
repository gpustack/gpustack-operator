package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// newRenderDeployment builds a deployment whose single role every case then varies.
func newRenderDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Model:         workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine:        workercore.ModelDeploymentEngineVLLM,
			EngineVersion: "0.25.1",
			KVCache:       workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
			Roles: []workercore.ModelDeploymentRole{{
				Name:         "server",
				Replicas:     2,
				InstanceType: "h20-8x",
				Template:     &workercore.ModelDeploymentTemplate{Image: "vllm/vllm-openai:v0.25.1"},
			}},
		},
	}
	for _, m := range mutate {
		m(md)
	}

	return md
}

// newRenderInstanceType builds an accelerated InstanceType whose per-card unit resources every
// sizing case reads: 16 CPU and 64Gi of RAM for one whole card.
func newRenderInstanceType(mutate ...func(*worker.InstanceType)) *worker.InstanceType {
	it := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "h20-8x"},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "64Gi"},
			LocalStorage:  "512Gi",
		},
		Status: workercore.InstanceTypeStatus{
			// DELIBERATELY NOT FormatLocalQueueName("h20-8x"). The entrance is read from this field,
			// and a fixture spelling it the way a name-derived render would spell it could not tell
			// the two apart -- the assertion in _Identity would pass either way. This value is one
			// no derivation produces.
			Entrance: "queue-for-h20-8x",
			// The observed detail carries what image synthesis needs, so a role naming no image
			// still renders. A case that wants the unsynthesizable path clears one of these.
			Detail: workercore.InstanceTypeDetail{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
					RuntimeVersion:  "12.9",
					RuntimeVersions: []string{"12.9"},
				},
			},
		},
	}
	for _, m := range mutate {
		m(it)
	}

	return it
}

func renderOne(t *testing.T, md *workercore.ModelDeployment, it *worker.InstanceType) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		Ordinal:      0,
		InstanceType: it,
	})
	require.NoError(t, err)

	return pod
}

func envValue(pod *core.Pod, name string) (string, bool) {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}

	return "", false
}

// TestRenderModelDeploymentPod_Identity pins what makes a rendered Pod findable and schedulable:
// its name, the labels a Service selects on, the entrance label that routes it into the role's
// pool, the resource note a watch filters on, and the controller reference that makes it ours.
func TestRenderModelDeploymentPod_Identity(t *testing.T) {
	md := newRenderDeployment()
	pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		Ordinal:      3,
		InstanceType: newRenderInstanceType(),
	})
	require.NoError(t, err)

	assert.Equal(t, "qwen-server-3", pod.Name)
	assert.Equal(t, "team-a", pod.Namespace)

	assert.Equal(t, "model-deployment", pod.Labels[modelDeploymentLabelKeyName])
	assert.Equal(t, "qwen", pod.Labels[modelDeploymentLabelKeyInstance])
	assert.Equal(t, "server", pod.Labels[modelDeploymentLabelKeyComponent])

	// The InstanceType's PUBLISHED entrance, verbatim -- not FormatLocalQueueName of its name. The
	// fixture spells the two differently on purpose, so this asserts which one the render read.
	assert.Equal(t, "queue-for-h20-8x", pod.Labels[kueuectrlconst.QueueLabel],
		"the entrance label is read from the InstanceType's status, not derived from its name")
	assert.True(t, systemmeta.MatchResource(pod, ModelDeploymentResourceType))

	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "ModelDeployment", pod.OwnerReferences[0].Kind)
	assert.Equal(t, md.UID, pod.OwnerReferences[0].UID)
	assert.True(t, ptr.Deref(pod.OwnerReferences[0].Controller, false),
		"the reference must be a CONTROLLER reference, or the owned-Pod watch never fires")
}

// TestRenderModelDeploymentPod_EntranceLabelIsNotInTheSelector states why the two label sets are
// different. The selector is what a Service is created with and cannot change; the entrance label
// follows the role's InstanceType, which a spec update can move. A selector carrying it would
// orphan every replica already running the moment the type changed.
func TestRenderModelDeploymentPod_EntranceLabelIsNotInTheSelector(t *testing.T) {
	md := newRenderDeployment()
	selector := modelDeploymentSelectorLabels(md, &md.Spec.Roles[0])

	assert.NotContains(t, selector, kueuectrlconst.QueueLabel)

	pod := renderOne(t, md, newRenderInstanceType())
	for k, v := range selector {
		assert.Equal(t, v, pod.Labels[k], "a replica must carry every label its Service selects on")
	}
}

// TestRenderModelDeploymentPod_Command covers the whole argv, which the operator owns end to end
// because InstanceTemplate has Command and no Args: there is nowhere to put arguments beside an
// image's own entrypoint, so the append tier can only append to a command line the operator built.
func TestRenderModelDeploymentPod_Command(t *testing.T) {
	testCases := []struct {
		name        string
		engine      string
		extraArgs   []string
		connector   ModelDeploymentConnectorRender
		wantCommand []string
	}{
		{
			name:        "vllm base command, model positional",
			engine:      workercore.ModelDeploymentEngineVLLM,
			wantCommand: []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"},
		},
		{
			name:        "sglang base command names the model through a flag",
			engine:      workercore.ModelDeploymentEngineSGLang,
			wantCommand: []string{"python3", "-m", "sglang.launch_server", "--model-path", "Qwen/Qwen2.5-72B-Instruct"},
		},
		{
			name:      "connector arguments land before the user's",
			engine:    workercore.ModelDeploymentEngineVLLM,
			connector: ModelDeploymentConnectorRender{Args: []string{`--kv-transfer-config={}`}},
			extraArgs: []string{"--max-model-len=32768"},
			wantCommand: []string{
				"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct",
				`--kv-transfer-config={}`,
				"--max-model-len=32768",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine = tc.engine
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})

			pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
				Connector:    tc.connector,
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantCommand, pod.Spec.Containers[0].Command)
		})
	}
}

// TestRenderModelDeploymentPod_TakeOver pins the third tier. A role that replaces the command owns
// the argv, so the operator contributes no engine argument, no client environment and no mounted
// configuration — every one of which describes a file the replaced argv never names.
func TestRenderModelDeploymentPod_TakeOver(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=32768"}
		md.Spec.Roles[0].Template.Command = []string{"/bin/my-server", "--flag"}
	})

	pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
		Connector: ModelDeploymentConnectorRender{
			Args:         []string{`--kv-transfer-config={}`},
			Env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: inject.ConfigFilePath}},
			Volumes:      []core.Volume{{Name: inject.ConfigVolumeName}},
			VolumeMounts: []core.VolumeMount{{Name: inject.ConfigVolumeName, MountPath: inject.ConfigMountPath}},
			PodAnnotations: map[string]string{
				inject.ClientConfigAnnotationKey: `{"master_server_address":"master:50051"}`,
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/my-server", "--flag"}, pod.Spec.Containers[0].Command,
		"a replaced command is used verbatim; the operator appends nothing to it")

	_, ok := envValue(pod, "MOONCAKE_CONFIG_PATH")
	assert.False(t, ok, "the operator's client environment points at a file this argv never reads")

	assert.Empty(t, pod.Spec.Volumes, "and the file itself is not mounted either")
	assert.Empty(t, pod.Spec.Containers[0].VolumeMounts, "nor is it mounted into the container")

	// THE ANNOTATION IS WITHHELD TOO, and it is the piece most likely to be forgotten: it is not
	// part of the PodSpec, so every assertion above passes while it lands. On a take-over role it
	// would put the pool's address and the whole client document on a Pod the operator did not
	// configure -- a record of a wiring that is not there.
	assert.NotContains(t, pod.Annotations, inject.ClientConfigAnnotationKey,
		"a take-over role gets no part of the connector, including the annotation carrying it")
}

// TestRenderModelDeploymentPod_Env covers the merge across tiers: what the operator owns is
// rendered first and cannot be replaced, what it defaults yields to a user's value, and the
// template overlay wins over the role's own append tier.
func TestRenderModelDeploymentPod_Env(t *testing.T) {
	connector := ModelDeploymentConnectorRender{
		Env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: inject.ConfigFilePath}},
		DefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
	}

	testCases := []struct {
		name     string
		roleEnv  []workercore.InstanceEnvVar
		tmplEnv  []workercore.InstanceEnvVar
		wantEnv  map[string]string
		wantGone []string
	}{
		{
			name:    "nothing supplied — owned and defaulted both render",
			wantEnv: map[string]string{"MOONCAKE_CONFIG_PATH": inject.ConfigFilePath, "MC_TE_METRIC": "1"},
		},
		{
			name:    "a defaulted key yields to the user's value",
			roleEnv: []workercore.InstanceEnvVar{{Name: "MC_TE_METRIC", Value: "0"}},
			wantEnv: map[string]string{"MC_TE_METRIC": "0"},
		},
		{
			name:    "an owned key supplied anyway never displaces the operator's",
			roleEnv: []workercore.InstanceEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}},
			wantEnv: map[string]string{"MOONCAKE_CONFIG_PATH": inject.ConfigFilePath},
		},
		{
			name:    "the template overlay replaces the role's append tier by name",
			roleEnv: []workercore.InstanceEnvVar{{Name: "HF_HOME", Value: "/role"}},
			tmplEnv: []workercore.InstanceEnvVar{{Name: "HF_HOME", Value: "/template"}},
			wantEnv: map[string]string{"HF_HOME": "/template"},
		},
		{
			name:    "an ordinary user variable is passed through untouched",
			roleEnv: []workercore.InstanceEnvVar{{Name: "VLLM_LOGGING_LEVEL", Value: "DEBUG"}},
			wantEnv: map[string]string{"VLLM_LOGGING_LEVEL": "DEBUG"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = tc.roleEnv
				md.Spec.Roles[0].Template.Env = tc.tmplEnv
			})

			pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
				Connector:    connector,
			})
			require.NoError(t, err)

			for name, want := range tc.wantEnv {
				got, ok := envValue(pod, name)
				if assert.Truef(t, ok, "%s must be rendered", name) {
					assert.Equal(t, want, got, "value of %s", name)
				}
			}

			seen := make(map[string]int)
			for _, e := range pod.Spec.Containers[0].Env {
				seen[e.Name]++
			}
			for name, count := range seen {
				assert.Equalf(t, 1, count,
					"%s rendered %d times; a duplicate leaves no way to tell which value won", name, count)
			}
		})
	}
}

// TestDeriveModelDeploymentResources covers the half of a replica's request that is DERIVED rather
// than declared. CPU and memory are inexpressible on a role, so getting this wrong charges quota
// for something other than what runs — and nothing in the API would show it.
func TestDeriveModelDeploymentResources(t *testing.T) {
	testCases := []struct {
		name string

		resources *workercore.ModelDeploymentRoleResources
		instType  func(*worker.InstanceType)

		wantCPU, wantRAM string
		wantErr          string
	}{
		{
			name:      "whole cards scale the unit resources by the card count",
			resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("4"))},
			wantCPU:   "64", wantRAM: "256Gi",
		},
		{
			name:    "no accelerator at all still sizes one unit's worth of host",
			wantCPU: "16", wantRAM: "64Gi",
		},
		{
			name: "a memory slice takes that percentage of one card's worth",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantCPU:  "8", wantRAM: "32Gi",
		},
		{
			name: "a bare compute slice copies across, so the host follows the same fraction",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                      ptr.To(resource.MustParse("1")),
				AcceleratorSlicedCoresPercentage: 25,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantCPU:  "4", wantRAM: "16Gi",
		},
		{
			name: "a partition profile is anchored on the share of VRAM it occupies",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			instType: func(it *worker.InstanceType) {
				it.Status.Detail.Memory = "80Gi"
				it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 40 << 10, Count: 2},
				}
			},
			wantCPU: "8", wantRAM: "32Gi",
		},
		{
			name: "a slice against an uncomputed detail is retryable, never a whole card",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.Manufacturer = "" },
			wantErr:  "not ready yet",
		},
		{
			name: "a partition the detail cannot size yet is retryable too",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			instType: func(it *worker.InstanceType) {
				it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 0},
				}
			},
			wantErr: "cannot be sized",
		},
		{
			name:      "an unparseable unit is reported rather than rendered as zero",
			resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("1"))},
			instType:  func(it *worker.InstanceType) { it.Spec.UnitResources.CPU = "not-a-quantity" },
			wantErr:   "invalid CPU unit",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := newRenderInstanceType()
			if tc.instType != nil {
				tc.instType(it)
			}
			role := &workercore.ModelDeploymentRole{Name: "server", Resources: tc.resources}

			ress, err := deriveModelDeploymentResources(role, it)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			qtyEqual(t, qty(tc.wantCPU), ress.CPU, "cpu")
			qtyEqual(t, qty(tc.wantRAM), ress.RAM, "ram")
			qtyEqual(t, qty("15Gi"), ress.LocalStorage, "local storage")
		})
	}
}

// TestDeriveModelDeploymentResources_LocalStorage pins both sides of the ephemeral-storage default,
// which is a cap and not a constant: 15Gi is what a replica asks for, unless the InstanceType offers
// less, in which case asking for the default would make every replica unschedulable.
func TestDeriveModelDeploymentResources_LocalStorage(t *testing.T) {
	testCases := []struct {
		name        string
		offered     string
		wantStorage string
	}{
		{name: "the type offers more than the default", offered: "512Gi", wantStorage: "15Gi"},
		{name: "the type offers nothing at all", offered: "", wantStorage: "15Gi"},
		{name: "the type offers less than the default", offered: "8Gi", wantStorage: "8Gi"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := newRenderInstanceType(func(it *worker.InstanceType) { it.Spec.LocalStorage = tc.offered })
			role := &workercore.ModelDeploymentRole{Name: "server"}

			ress, err := deriveModelDeploymentResources(role, it)
			require.NoError(t, err)
			qtyEqual(t, qty(tc.wantStorage), ress.LocalStorage, "local storage")
		})
	}
}

// TestRenderModelDeploymentPod_AcceleratorRequest checks that the derived values and the declared
// accelerator reach the container through the one resource renderer both CRDs share.
func TestRenderModelDeploymentPod_AcceleratorRequest(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(resource.MustParse("2")),
		}
	})

	pod := renderOne(t, md, newRenderInstanceType())

	accName := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	limits := pod.Spec.Containers[0].Resources.Limits
	qtyEqual(t, qty("2"), limits[accName], "the declared card count reaches the container")
	qtyEqual(t, qty("32"), limits[core.ResourceCPU], "the host CPU is derived, not declared")
	qtyEqual(t, qty("128Gi"), limits[core.ResourceMemory], "the host memory is derived, not declared")
}

// TestRenderModelDeploymentPod_Ports pins that a replica is always reachable: a role naming no port
// still gets one, because the Service fronting the deployment needs a target.
func TestRenderModelDeploymentPod_Ports(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())
	require.Len(t, pod.Spec.Containers[0].Ports, 1)
	assert.Equal(t, int32(8000), pod.Spec.Containers[0].Ports[0].ContainerPort)
	assert.Equal(t, "http", pod.Spec.Containers[0].Ports[0].Name)

	md = newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Template.Ports = []workercore.InstancePort{
			{Port: 9000, Protocol: core.ProtocolTCP},
		}
	})
	pod = renderOne(t, md, newRenderInstanceType())
	require.Len(t, pod.Spec.Containers[0].Ports, 1)
	assert.Equal(t, int32(9000), pod.Spec.Containers[0].Ports[0].ContainerPort)
}

// TestRenderModelDeploymentPod_SynthesizesTheImage covers the role that names none.
//
// The operator used to refuse this outright, on the grounds that it builds the argv but never the
// image. It now assembles one from the engine and the observed hardware -- but only when the
// hardware HAS been observed, which is why the second case matters as much as the first: an
// InstanceType whose detail has not converged must not produce a tag with a hole in it.
func TestRenderModelDeploymentPod_SynthesizesTheImage(t *testing.T) {
	t.Run("no template at all still renders", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template = nil
		})

		pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
		})
		require.NoError(t, err, "a nil template is not an error once the image can be synthesized")
		assert.Equal(t, "gpustack/runner:cuda12.9-vllm0.25.1", pod.Spec.Containers[0].Image)
	})

	t.Run("a stated image wins over synthesis", func(t *testing.T) {
		md := newRenderDeployment()
		pod := renderOne(t, md, newRenderInstanceType())
		assert.NotEqual(t, "gpustack/runner:cuda12.9-vllm0.25.1", pod.Spec.Containers[0].Image,
			"synthesis must not overwrite a value the user stated")
	})

	t.Run("an unobserved runtime version refuses rather than rendering a hole", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template = nil
		})
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Detail.RuntimeVersion = ""
			it.Status.Detail.RuntimeVersions = nil
		})

		_, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "names no image and none could be synthesized")
		assert.Contains(t, err.Error(), "has not observed a runtime version yet",
			"the reason has to distinguish a wait from a refusal, because only one of them resolves")
	})

	t.Run("a manufacturer with no runner backend refuses", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template = nil
		})
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Detail.Manufacturer = nodefeature.ManufacturerCambricon
		})

		_, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no runner backend",
			"this one never resolves, so the message must name the manufacturer")
	})

	// THE FALLBACK IS WHAT MAKES THIS AN ERROR PATH RATHER THAN A DEFAULT. A Pod carrying no
	// queue-name label is not queued by Kueue at all -- the kubelet runs it directly, charging no
	// quota and passing none of the gates the chain applies. So an InstanceType that has not
	// published its entrance yet has to stop the render, exactly as an uncomputed accelerator detail
	// does, rather than yield a Pod that would run.
	t.Run("an instance type with no published entrance refuses", func(t *testing.T) {
		md := newRenderDeployment()
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Entrance = ""
		})

		pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Nil(t, pod, "no Pod may be returned, or a caller could create one with no queue")
		assert.Contains(t, err.Error(), "publishes no queue entrance yet")
		assert.Contains(t, err.Error(), "h20-8x",
			"the message must name the instance type, since the field is two objects away")
	})
}

// TestRenderModelDeploymentPod_ClientConfigMount pins where the rendered client configuration is
// mounted, because the owned MOONCAKE_CONFIG_PATH variable is the only pointer to it: a mount path
// that drifted from the constant the connector renders would leave the engine reading nothing.
//
// THE CONNECTOR COMES FROM SYNTHESIS RATHER THAN A LITERAL, and that is the point of this test now.
// What it pins is that the three pieces ARRIVE TOGETHER: the annotation holding the document, the
// projection whose fieldRef reads that annotation, and the mount. A literal here would satisfy every
// assertion below while synthesis produced a projection naming an annotation nobody sets -- which
// mounts an EMPTY FILE and starts an engine that uses no cache, with every symptom inside the
// container.
func TestRenderModelDeploymentPod_ClientConfigMount(t *testing.T) {
	md := newRenderDeployment()
	conn, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
		Connector:    conn,
	})
	require.NoError(t, err)

	require.Len(t, pod.Spec.Volumes, 1)
	require.NotNil(t, pod.Spec.Volumes[0].DownwardAPI,
		"the file is a projection of an annotation, not a ConfigMap: there is no second object")
	require.Len(t, pod.Spec.Volumes[0].DownwardAPI.Items, 1)

	ref := pod.Spec.Volumes[0].DownwardAPI.Items[0].FieldRef
	require.NotNil(t, ref)
	assert.Contains(t, ref.FieldPath, inject.ClientConfigAnnotationKey)
	assert.NotEmpty(t, pod.Annotations[inject.ClientConfigAnnotationKey],
		"the projection reads this annotation, so an absent or empty one mounts an empty file")

	mounts := pod.Spec.Containers[0].VolumeMounts
	require.Len(t, mounts, 1)
	assert.Equal(t, inject.ConfigMountPath, mounts[0].MountPath)
	assert.True(t, mounts[0].ReadOnly)
}

// TestRenderModelDeploymentPod_NoClientConfigWithoutOne pins the state a deployment is in before its
// Binding has been resolved: a replica renders and runs, with no mount and no volume, rather than
// referencing a ConfigMap nothing has written.
func TestRenderModelDeploymentPod_NoClientConfigWithoutOne(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())

	assert.Empty(t, pod.Spec.Volumes)
	assert.Empty(t, pod.Spec.Containers[0].VolumeMounts)
}

// TestModelDeploymentPodSpecHash_IsDeterministic states the property the whole rollout rests on: two
// renders of one spec must fingerprint identically, or the deployment would recreate every replica
// on every pass forever.
func TestModelDeploymentPodSpecHash_IsDeterministic(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Env = []workercore.InstanceEnvVar{
			{Name: "B", Value: "2"}, {Name: "A", Value: "1"},
		}
	})

	first := renderOne(t, md, newRenderInstanceType())
	second := renderOne(t, md, newRenderInstanceType())

	assert.Equal(t,
		first.Annotations[modelDeploymentPodSpecHashAnnotation],
		second.Annotations[modelDeploymentPodSpecHashAnnotation])
}

// TestModelDeploymentPodSpecHash_DoesNotCoverItself is the other half of that property. A
// fingerprint that covered the annotation holding it would differ from the one already stored on
// every pass, which reads exactly like a spec that keeps changing.
func TestModelDeploymentPodSpecHash_DoesNotCoverItself(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())

	stored := pod.Annotations[modelDeploymentPodSpecHashAnnotation]
	assert.Equal(t, stored, modelDeploymentPodSpecHash(pod),
		"rehashing a rendered Pod must reproduce the value it already carries")
}

// TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput walks the inputs a rollout must react
// to and asserts each one moves the fingerprint. A change that did not would leave replicas running
// a spec nobody asked for, with the deployment reporting itself Ready.
func TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput(t *testing.T) {
	base := renderOne(t, newRenderDeployment(), newRenderInstanceType())
	baseHash := base.Annotations[modelDeploymentPodSpecHashAnnotation]

	testCases := []struct {
		name    string
		mutate  func(*workercore.ModelDeployment)
		instype func(*worker.InstanceType)
		input   func(*ModelDeploymentRenderInput)
	}{
		{
			name:   "image",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0" },
		},
		{
			name:   "model",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Model.Name = "Qwen/Qwen3-32B" },
		},
		{
			name:   "extra arguments",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=1024"} },
		},
		{
			name: "environment",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = []workercore.InstanceEnvVar{{Name: "A", Value: "1"}}
			},
		},
		{
			name: "accelerator request",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(resource.MustParse("2")),
				}
			},
		},
		{
			// BOTH SIDES MOVE, because that is what naming another pool actually does: the
			// reconciler reads the InstanceType by the name the role carries
			// (getModelDeploymentInstanceType), so a different name is a different object and a
			// different published entrance. Mutating the name alone would render the SAME entrance
			// and assert nothing -- the entrance is no longer derived from the name.
			name:    "instance type — the entrance label moves with it",
			mutate:  func(md *workercore.ModelDeployment) { md.Spec.Roles[0].InstanceType = "h20-4x" },
			instype: func(it *worker.InstanceType) { it.Name, it.Status.Entrance = "h20-4x", "queue-for-h20-4x" },
		},
		{
			// The entrance alone, with the role's instanceType untouched: a pool whose LocalQueue
			// was renamed under a deployment that never changed. The replicas must be recreated
			// carrying the new one, or they stay routed at a queue that is gone.
			name:    "the published entrance, with the instance type name unchanged",
			instype: func(it *worker.InstanceType) { it.Status.Entrance = "queue-renamed" },
		},
		{
			name:    "unit resources — the derived host request moves without the spec moving",
			instype: func(it *worker.InstanceType) { it.Spec.UnitResources.CPU = "32" },
		},
		{
			name:  "the resolved connector arguments",
			input: func(in *ModelDeploymentRenderInput) { in.Connector.Args = []string{`--kv-transfer-config={}`} },
		},
		{
			name:  "the runtime class",
			input: func(in *ModelDeploymentRenderInput) { in.RuntimeClassName = "nvidia" },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			it := newRenderInstanceType()
			if tc.instype != nil {
				tc.instype(it)
			}

			in := ModelDeploymentRenderInput{
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: it,
			}
			if tc.input != nil {
				tc.input(&in)
			}

			pod, err := renderModelDeploymentPod(in)
			require.NoError(t, err)
			assert.NotEqual(t, baseHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])
		})
	}
}

// TestRenderModelDeploymentPod_ConfigChangeMovesTheSpecHash is the rollout property T14 exists for,
// and the one a ConfigMap carrier could not have delivered.
//
// A ConfigMap reaches a Pod as a NAME, so re-rendering its contents leaves core.PodSpec
// byte-identical while the hash's subject is {Labels, Annotations, PodSpec}. The hash would not
// move, no recreate would follow, the replicas would keep a stale client configuration -- and a
// check asserting the hash moved would have gone GREEN over exactly that. The annotation carrier
// puts the document itself inside the hash's subject, so this test is the difference between the
// two designs rather than a restatement of either.
func TestRenderModelDeploymentPod_ConfigChangeMovesTheSpecHash(t *testing.T) {
	md := newRenderDeployment()

	render := func(endpoint string) *core.Pod {
		cin := connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA)
		cin.MasterServerAddress = endpoint
		conn, err := SynthesizeModelDeploymentConnector(cin)
		require.NoError(t, err)

		pod, err := renderModelDeploymentPod(ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
			Connector:    conn,
		})
		require.NoError(t, err)

		return pod
	}

	before := render("master-a.gpustack-system.svc:50051")
	after := render("master-b.gpustack-system.svc:50051")

	assert.NotEqual(t,
		before.Annotations[modelDeploymentPodSpecHashAnnotation],
		after.Annotations[modelDeploymentPodSpecHashAnnotation],
		"a pool endpoint change has to move the hash, or the replicas keep a stale client config")

	// AND THE POD SPEC IS IDENTICAL ACROSS THE TWO, which is what makes the assertion above mean
	// something. The volume names an annotation rather than carrying the document, so the spec
	// cannot see this change at all: had the hash's subject been the spec alone, these two renders
	// would be indistinguishable and no recreate would ever follow a configuration change.
	assert.Equal(t, before.Spec, after.Spec,
		"the projection names an annotation, so the spec is blind to the content change")
}
