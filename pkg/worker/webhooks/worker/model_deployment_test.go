package worker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
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

// numberedRoles builds n roles that differ only in name, for the cases about how many there may be.
// Every other rule passes on them, so a refusal is the count rule's and nothing else's.
func numberedRoles(n int) []workercore.ModelDeploymentRole {
	roles := make([]workercore.ModelDeploymentRole, 0, n)
	for i := range n {
		roles = append(roles, role(func(r *workercore.ModelDeploymentRole) {
			r.Name = fmt.Sprintf("role-%d", i)
		}))
	}

	return roles
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
		},
		{
			// The bound is Kueue's, so the refusal has to say so: a user who reads only the number
			// files a bug here, and a user who reads whose number it is goes and looks at the
			// Workload their roles become.
			name:        "roles_eleven",
			md:          modelDeployment(workercore.ModelDeploymentEngineVLLM, numberedRoles(11)...),
			wantMessage: "Kueue caps Workload.spec.podSets at 10",
		},
		{
			name: "roles_ten",
			md:   modelDeployment(workercore.ModelDeploymentEngineVLLM, numberedRoles(10)...),
		},
		{
			// A duplicate name does not collide, it MERGES: the name is the PodSet name, so two
			// roles sharing one become a single PodSet whose count is their sum.
			name: "role_names_duplicate",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "worker" }),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "worker" }),
			),
			wantMessage: "grouping both roles into one PodSet whose count is their sum",
		},
		{
			// The Service fronting a role is named <deployment>-<role>, and a Service name is a DNS
			// LABEL -- 63 characters, where an object name runs to 253. Both halves are legal
			// separately, which is why neither field's own maxLength can catch this.
			name: "role_service_name_too_long",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					// "qwen-72b" is 8, plus the hyphen, so 55 here is the first length that overflows.
					r.Name = strings.Repeat("d", 55)
				}),
			),
			wantMessage: "which is not a valid Service name: must be no more than 63 characters",
		},
		{
			// The boundary itself must be ACCEPTED, or the case above would also pass against a rule
			// that refuses one character too early.
			name: "role_service_name_at_the_bound",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = strings.Repeat("d", 54) }),
			),
		},
		{
			// A LENGTH CHECK ALONE MISSES THIS. An object name is a DNS SUBDOMAIN, so a dotted
			// deployment name is legal and its combined Service name is not -- well inside 63
			// characters, which is why the rule has to check the shape rather than the size.
			name: "role_service_name_from_a_dotted_deployment",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }))
				md.Name = "team.model.serving"

				return md
			}(),
			wantMessage: "which is not a valid Service name",
		},
		{
			// One Workload carries one queue name, and the queue name comes from the instanceType.
			// The assertion is on the sentence that names the GAP, not on the one that says "pick one
			// type": wanting different hardware is why a user reaches for a second instanceType, and
			// there is no field for it any more — so a message that only says "pick one" would read
			// as though the goal were reachable another way.
			name: "role_instance_types_differ",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.InstanceType = "decode", "a100-8x"
				}),
			),
			wantMessage: "is not possible today",
		},
		{
			// THE OUTLIER IS FIRST, which is the arrangement a roles[0]-anchored comparison reports
			// backwards: it blames the two roles that already agree with each other and says nothing
			// about the one that has to change. Both types have to appear in the message, because
			// which one the user meant to keep is not knowable here.
			//
			// The case above cannot catch that -- its outlier is last, so anchoring on roles[0]
			// happens to name the right role and both implementations pass it.
			name: "role_instance_types_differ_with_the_outlier_first",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.InstanceType = "prefill", "a100-8x"
				}),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode2" }),
			),
			// The PATH is asserted beside the content: an error on spec.roles rather than on
			// spec.roles[N].instanceType is what "no single role is at fault" means, and it is the
			// half a substring of the type names alone would not pin.
			wantMessage: `spec.roles: Invalid value: "a100-8x, h20-8x"`,
		},
		{
			// The rule is vacuous at length 1, asserted so the single-role behavior cannot regress
			// into needing a second role to agree with.
			name: "role_instance_type_single",
			md:   modelDeployment(workercore.ModelDeploymentEngineVLLM),
		},
		{
			name: "role_kinds_prefill_and_decode",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
				}),
			),
		},
		{
			// "One plain server plus a prefiller" is not a shape anything consumes, and rendering a
			// transfer configuration for it would mean inventing what it means.
			name: "role_kinds_server_beside_prefill",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "server", workercore.ModelDeploymentRoleKindServer
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
				}),
			),
			wantMessage: "cannot be combined with another kind",
		},
		{
			// The kind is unset, which the schema defaults to server. The rules have to read the
			// zero value the same way, or an object built in Go and the same object round-tripped
			// through the API server would be judged differently.
			name: "role_kinds_unset_beside_prefill",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "server" }),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
				}),
			),
			wantMessage: "cannot be combined with another kind",
		},
		{
			// SGLang's store configuration has no prefill/decode equivalent, so the refusal names
			// the engine: the kind is legal, and this engine is what has no term for it.
			name: "role_kind_unsupported_by_engine",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
				}),
			),
			wantMessage: `engine "sglang" has no rendering term for kind "prefill"`,
		},
		{
			// The same engine with the kind it does render is accepted, so the case above fails for
			// the engine and not because two roles or a kind field are refused outright.
			name: "role_kind_server_on_sglang",
			md:   modelDeployment(workercore.ModelDeploymentEngineSGLang),
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
			errs := validateModelDeployment(tc.md, nil)

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

// TestValidateModelDeployment_TwoRolesPassTheWholePath is the seam the single-role spec left, now
// spent.
//
// It used to call the rules that outlive that version WITHOUT the length predicate, to prove the
// restriction was one deletable call. It now calls the FULL path and expects the same two roles to
// pass, which is what that seam was for: the predicate is gone and nothing took its place.
func TestValidateModelDeployment_TwoRolesPassTheWholePath(t *testing.T) {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
	)

	assert.Empty(t, validateModelDeploymentRoles(md), "the per-role rules are unchanged by the lift")
	assert.Empty(t, validateModelDeployment(md, nil), "nothing refuses two roles any more")
}

func errsContain(aggregate, want string) bool {
	return strings.Contains(aggregate, want)
}

func TestValidateModelDeploymentRoleServiceNames_ExemptsRolesTheObjectAlreadyHad(t *testing.T) {
	tooLong := strings.Repeat("d", 55)

	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = tooLong }))

	testCases := []struct {
		name     string
		existing sets.Set[string]
		refused  bool
		why      string
	}{
		{
			name:    "on create, nothing is grandfathered",
			refused: true,
			why:     "the mistake is being made now, where the message can still help",
		},
		{
			name:     "on update, a role the object already had is left alone",
			existing: sets.New(tooLong),
			why:      "refusing it would strand the object: md.Name cannot be shortened and every edit runs through here",
		},
		{
			name:     "on update, a role that is new is still refused",
			existing: sets.New("some-other-role"),
			refused:  true,
			why:      "the exemption is for what was stored, not for the update verb",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelDeploymentRoleServiceNames(md, tc.existing)
			if !tc.refused {
				assert.Empty(t, errs, tc.why)

				return
			}
			require.Len(t, errs, 1, tc.why)
			assert.Contains(t, errs[0].Error(), "not a valid Service name", tc.why)
		})
	}
}
