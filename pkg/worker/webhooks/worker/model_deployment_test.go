package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelDeployment builds a valid single-role deployment for the given engine, which every case then
// makes invalid in exactly one way.
func modelDeployment(engine string, roles ...workercore.ModelDeploymentRole) *workercore.ModelDeployment {
	if len(roles) == 0 {
		roles = []workercore.ModelDeploymentRole{{
			Name:         "server",
			Replicas:     4,
			InstanceType: "h20-8x",
		}}
	}

	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen-72b", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Model:         workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine:        engine,
			EngineVersion: "0.25.1",
			KVCache:       workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
			Roles:         roles,
		},
	}
}

func role(mutate func(*workercore.ModelDeploymentRole)) workercore.ModelDeploymentRole {
	r := workercore.ModelDeploymentRole{Name: "server", Replicas: 4, InstanceType: "h20-8x"}
	mutate(&r)

	return r
}

func TestValidateModelDeployment(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		// wantMessage is a substring the refusal must carry. Empty means the case must be accepted.
		// A refusal is asserted by its message and not merely by its existence, because the whole
		// point of refusing here rather than in the schema is that the message is actionable.
		wantMessage string
	}{
		{
			name: "roles_one",
			md:   modelDeployment(workercore.ModelDeploymentEngineVLLM),
		},
		{
			name: "roles_two",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
			),
			wantMessage: "specs/*-pd-atomic-admission.md",
		},
		{
			name: "extra_args_owned_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs = []string{`--kv-transfer-config={"kv_connector":"Other"}`}
			})),
			wantMessage: "--kv-transfer-config",
		},
		{
			name: "extra_args_owned_key_sglang",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang, role(func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs = []string{"--hicache-storage-backend-extra-config", "{}"}
			})),
			wantMessage: "--hicache-storage-backend-extra-config",
		},
		{
			// Ownership is per (engine, key): a vLLM-owned key is an ordinary user argument on
			// SGLang, and refusing it there would refuse something harmless.
			name: "extra_args_owned_key_wrong_engine",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang, role(func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs = []string{`--kv-transfer-config={"kv_connector":"MooncakeStoreConnector"}`}
			})),
		},
		{
			name: "extra_args_unowned_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs = []string{"--max-model-len=32768"}
			})),
		},
		{
			name: "env_owned_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.InstanceEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}}
			})),
			wantMessage: "MOONCAKE_CONFIG_PATH",
		},
		{
			// The config-path variable is owned per engine too: SGLang reads its own, and
			// MOONCAKE_CONFIG_PATH is not what the operator rendered for it.
			name: "env_owned_key_wrong_engine",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.InstanceEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}}
			})),
		},
		{
			name: "env_defaulted_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.InstanceEnvVar{{Name: "MC_TE_METRIC", Value: "0"}}
			})),
		},
		{
			// The overlay tier is the path the rule originally missed: the renderer merges
			// template.env together with env, so an owned key here used to pass admission and be
			// dropped silently at render time.
			//
			// wantMessage asserts the PATH, not just the variable name. Asserting only
			// "MOONCAKE_CONFIG_PATH" would also pass if the append-tier rule fired on a
			// differently-placed value, and would pass with the overlay rule deleted -- the case
			// has to fail for the one reason it exists.
			name: "env_owned_key_in_template",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{
					Image: "vllm/vllm-openai:latest",
					Env:   []workercore.InstanceEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}},
				}
			})),
			wantMessage: "roles[0].template.env[0]",
		},
		{
			// A role that took over the command line is refused too, because the renderer drops
			// owned keys unconditionally. Admission and rendering must agree on the set: whichever
			// way they disagree, the result is a value the user wrote and nothing reads.
			name: "env_owned_key_in_template_take_over",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{
					Image:   "vllm/vllm-openai:latest",
					Command: []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
					Env:     []workercore.InstanceEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}},
				}
			})),
			wantMessage: "roles[0].template.env[0]",
		},
		{
			name: "env_unowned_key_in_template",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{
					Image: "vllm/vllm-openai:latest",
					Env:   []workercore.InstanceEnvVar{{Name: "HF_HOME", Value: "/weights"}},
				}
			})),
		},
		{
			// `required` makes the key present, not the value non-empty, and this field's type is
			// upstream's LocalObjectReference -- so a minLength marker cannot reach it and the
			// schema accepts `poolRef: {name: ""}` in full.
			name: "pool_ref_name_empty",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.KVCache.PoolRef.Name = ""

				return md
			}(),
			wantMessage: "spec.kvCache.poolRef.name",
		},
		{
			// The refusal must name the structured field that DOES decide the request. Naming only
			// instanceType would send a user to a field that cannot express a card count.
			name: "template_resources",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{
					Image:     "vllm/vllm-openai:latest",
					Resources: &workercore.InstanceResources{},
				}
			})),
			wantMessage: "roles[0].resources",
		},
		{
			name: "resources_accelerator_only",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(resource.MustParse("1")),
				}
			})),
		},
		{
			name: "resources_sliced_percentages",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator:                       ptr.To(resource.MustParse("1")),
					AcceleratorSlicedMemoryPercentage: 50,
					AcceleratorSlicedCoresPercentage:  50,
				}
			})),
		},
		{
			name: "resources_partition_profile_only",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator:                   ptr.To(resource.MustParse("1")),
					AcceleratorPartitionedProfile: "3g.40gb",
				}
			})),
		},
		{
			// One accelerator cannot serve both, and the renderer resolves the pair by precedence —
			// so accepting this would silently discard the percentages.
			name: "resources_partition_and_slice_together",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator:                       ptr.To(resource.MustParse("1")),
					AcceleratorSlicedMemoryPercentage: 50,
					AcceleratorPartitionedProfile:     "3g.40gb",
				}
			})),
			wantMessage: "cannot both apply to one accelerator",
		},
		{
			name: "resources_partition_and_cores_together",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator:                      ptr.To(resource.MustParse("1")),
					AcceleratorSlicedCoresPercentage: 25,
					AcceleratorPartitionedProfile:    "3g.40gb",
				}
			})),
			wantMessage: "cannot both apply to one accelerator",
		},
		{
			// A CPU-only replica is legitimate for a small model, so an absent resources block is
			// not a refusal.
			name: "resources_absent",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Resources = nil
			})),
		},
		{
			name: "template_command",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{
					Image:   "vllm/vllm-openai:latest",
					Command: []string{"vllm", "serve", "/models/qwen"},
				}
			})),
		},
		{
			// A template with no resources is the ordinary overlay, and must not be caught by the
			// resources rule reading a nil pointer as a set one.
			name: "template_without_resources",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Template = &workercore.ModelDeploymentTemplate{Image: "vllm/vllm-openai:latest"}
			})),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelDeployment(tc.md)

			if tc.wantMessage == "" {
				assert.Empty(t, errs, "expected acceptance")

				return
			}

			require.NotEmpty(t, errs, "expected a refusal")
			assert.True(t, errsContain(errs.ToAggregate().Error(), tc.wantMessage),
				"refusal must name %q, got: %s", tc.wantMessage, errs.ToAggregate().Error())
		})
	}
}

// TestValidateModelDeploymentRoles_TwoRolesWithoutTheLengthRule is the seam the next spec inherits.
//
// It calls the rules that outlive this version WITHOUT the length predicate and expects two roles to
// pass, so that lifting the restriction really is deleting one call. Asserting the seam here is the
// difference between a separable predicate and one that merely looks separable.
func TestValidateModelDeploymentRoles_TwoRolesWithoutTheLengthRule(t *testing.T) {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
	)

	assert.Empty(t, validateModelDeploymentRoles(md))
	assert.NotEmpty(t, validateModelDeploymentSingleRole(md),
		"the length rule is the only thing refusing two roles")
}

// TestValidateModelDeploymentSingleRole_NamesTheSpec pins the message shape the acceptance criteria
// ask for: the refusal must point at a plan, not merely decline.
func TestValidateModelDeploymentSingleRole_NamesTheSpec(t *testing.T) {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
	)

	errs := validateModelDeploymentSingleRole(md)
	require.Len(t, errs, 1)

	message := errs[0].Error()
	assert.Contains(t, message, "pd-atomic-admission")
	assert.Contains(t, message, "not supported by this version")
}

func errsContain(aggregate, want string) bool {
	return strings.Contains(aggregate, want)
}
