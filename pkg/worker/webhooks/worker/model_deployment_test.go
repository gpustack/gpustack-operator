package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
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

// newModelDeploymentWebhookWith builds the handler over two independent fakes, so a case can put an
// InstanceType behind the API reader that the cache does not have -- the state the two-stage read
// exists for, and one a single shared fake cannot express.
// newModelDeploymentWebhookWith builds a handler over one API-server view. There is no second,
// cached view to pass: the defaulter reads through on purpose, so a fixture offering a stale one
// would model a client this handler does not have.
func newModelDeploymentWebhookWith(live []ctrlcli.Object) *ModelDeploymentWebhook {
	return &ModelDeploymentWebhook{
		APIReader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(live...).Build(),
	}
}

func acceleratableInstanceType(name string, acceleratable bool) *worker.InstanceType {
	return &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec:       workercore.InstanceTypeSpec{Acceleratable: acceleratable},
	}
}

func roleWithAccelerator(name string, accel *resource.Quantity) workercore.ModelDeploymentRole {
	role := workercore.ModelDeploymentRole{Name: name, Replicas: 1, InstanceType: "h20-8x"}
	if accel != nil {
		role.Resources = &workercore.ModelDeploymentRoleResources{Accelerator: accel}
	}

	return role
}

// TestModelDeploymentWebhook_Default covers the one value this API defaults from another object.
//
// A role that names no accelerator count on an acceleratable InstanceType gets one card. The point
// is not the size of the replica: a role requesting no accelerator requests nothing an accelerated
// ClusterQueue covers, and one such role alongside any second role leaves the deployment queued
// forever -- measured: a mixed deployment is refused exactly like an all-uncovered one.
func TestModelDeploymentWebhook_Default(t *testing.T) {
	testCases := []struct {
		name          string
		acceleratable bool
		roles         []workercore.ModelDeploymentRole
		want          []*resource.Quantity // per role, nil meaning "left unset"
	}{
		{
			name:          "an unset count on an acceleratable type becomes one card",
			acceleratable: true,
			roles:         []workercore.ModelDeploymentRole{roleWithAccelerator("server", nil)},
			want:          []*resource.Quantity{resource.NewQuantity(1, resource.DecimalSI)},
		},
		{
			// THE SHAPE THE DEFAULT EXISTS FOR. Two roles requesting nothing the queue covers is
			// what makes the scheduler spin; one such role is admitted, so a single-role case
			// cannot show this.
			name:          "every role of a multi-role deployment is defaulted",
			acceleratable: true,
			roles: []workercore.ModelDeploymentRole{
				roleWithAccelerator("prefill", nil), roleWithAccelerator("decode", nil),
			},
			want: []*resource.Quantity{
				resource.NewQuantity(1, resource.DecimalSI), resource.NewQuantity(1, resource.DecimalSI),
			},
		},
		{
			// AN EXPLICIT ZERO IS A VALUE THE USER WROTE. It is kept even though it reaches the
			// state above, because replacing it would make the request stop meaning what it says.
			name:          "an explicit zero is kept",
			acceleratable: true,
			roles:         []workercore.ModelDeploymentRole{roleWithAccelerator("server", resource.NewQuantity(0, resource.DecimalSI))},
			want:          []*resource.Quantity{resource.NewQuantity(0, resource.DecimalSI)},
		},
		{
			name:          "a stated count is not touched",
			acceleratable: true,
			roles:         []workercore.ModelDeploymentRole{roleWithAccelerator("server", resource.NewQuantity(4, resource.DecimalSI))},
			want:          []*resource.Quantity{resource.NewQuantity(4, resource.DecimalSI)},
		},
		{
			// THE BASELINE FOR EVERY CASE ABOVE. A default applied unconditionally satisfies them
			// and fails here, which is the only reason they say anything about acceleratability.
			name:          "a non-acceleratable type is left alone",
			acceleratable: false,
			roles:         []workercore.ModelDeploymentRole{roleWithAccelerator("server", nil)},
			want:          []*resource.Quantity{nil},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := acceleratableInstanceType("h20-8x", tc.acceleratable)
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{it})
			md := modelDeployment(workercore.ModelDeploymentEngineVLLM, tc.roles...)

			require.NoError(t, w.Default(context.Background(), md))

			require.Len(t, md.Spec.Roles, len(tc.want))
			for i, want := range tc.want {
				got := md.Spec.Roles[i].Resources
				if want == nil {
					if got != nil {
						assert.Nil(t, got.Accelerator, "role %d must keep an unset count", i)
					}

					continue
				}
				require.NotNil(t, got, "role %d lost its resources", i)
				require.NotNil(t, got.Accelerator, "role %d was not defaulted", i)
				assert.Zero(t, want.Cmp(*got.Accelerator),
					"role %d: want %s, got %s", i, want, got.Accelerator)
			}
		})
	}
}

// TestModelDeploymentWebhook_DefaultReadsTheAPIServer pins where the read goes and what a miss says.
//
// The handler holds no cached client, so a type the informers have not seen is still found and a
// type that is genuinely gone is reported as the NAME it is. Those two send a reader to different
// places, which is the whole reason the message is asserted rather than the error's existence.
func TestModelDeploymentWebhook_DefaultReadsTheAPIServer(t *testing.T) {
	it := acceleratableInstanceType("h20-8x", true)

	t.Run("a type only the API server has is still found", func(t *testing.T) {
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{it})
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM, roleWithAccelerator("server", nil))

		require.NoError(t, w.Default(context.Background(), md))
		require.NotNil(t, md.Spec.Roles[0].Resources)
		require.NotNil(t, md.Spec.Roles[0].Resources.Accelerator)
		assert.Equal(t, int64(1), md.Spec.Roles[0].Resources.Accelerator.Value())
	})

	t.Run("a type the API server does not have is reported as the name it is", func(t *testing.T) {
		w := newModelDeploymentWebhookWith(nil)
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM, roleWithAccelerator("server", nil))

		err := w.Default(context.Background(), md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "h20-8x",
			"the message must name the instance type so a reader checks the spelling, not the cluster")
		assert.Contains(t, err.Error(), "spec.roles[0].instanceType",
			"and name the field that carries it")
	})
}

// TestModelDeploymentWebhook_DefaultLeavesAnEmptyTypeToValidation pins what the defaulter does with
// a name the API would refuse.
//
// Mutating admission runs before the schema, so a role naming no instance type reaches the defaulter
// first. Looking that name up fails on the REQUEST rather than on the object, which would deny the
// write with a message about reading the cluster — sending a reader to check the cluster when the
// answer is a missing field.
func TestModelDeploymentWebhook_DefaultLeavesAnEmptyTypeToValidation(t *testing.T) {
	w := newModelDeploymentWebhookWith(nil)
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
		Name: "server", Replicas: 1, InstanceType: "",
	})

	require.NoError(t, w.Default(context.Background(), md),
		"an empty instance type is the schema's to report, not the defaulter's")
	if r := md.Spec.Roles[0].Resources; r != nil {
		assert.Nil(t, r.Accelerator, "and nothing is defaulted against a type that was never read")
	}
}

// TestModelDeploymentWebhook_DefaultDeclinesAnObjectBeingDeleted pins the one refusal nothing could
// recover from.
//
// This handler opts out of the shared deletion guard so it keeps validating updates while a
// deployment is being deleted, and that opt-out is one decision covering defaulting too. The
// deployment's own finalizer holds it until teardown releases the Binding, and teardown releases the
// finalizer with an UPDATE -- so a defaulter that refuses an absent InstanceType would refuse THAT
// update whenever the type was deleted first, leaving an object nothing can release.
func TestModelDeploymentWebhook_DefaultDeclinesAnObjectBeingDeleted(t *testing.T) {
	w := newModelDeploymentWebhookWith(nil)

	now := meta.Now()
	deleting := modelDeployment(workercore.ModelDeploymentEngineVLLM, roleWithAccelerator("server", nil))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test.gpustack.ai/hold"}

	require.NoError(t, w.Default(context.Background(), deleting),
		"the update that clears the finalizer must not be refused for a type that is already gone")
	if r := deleting.Spec.Roles[0].Resources; r != nil {
		assert.Nil(t, r.Accelerator, "and nothing is defaulted onto an object that is going away")
	}

	// THE BASELINE THE CASE ABOVE NEEDS: a defaulter that declined every object would pass it too.
	live := modelDeployment(workercore.ModelDeploymentEngineVLLM, roleWithAccelerator("server", nil))
	require.Error(t, w.Default(context.Background(), live),
		"while the same shape that is not being deleted is still refused")
}

// TestModelDeploymentWebhook_DefaultKeepsTheRepairEditReachable bounds what a missing InstanceType
// costs a deployment that already exists.
//
// Refusing the write blocks every update to that deployment for as long as the type is absent. What
// keeps that bounded is WHICH name is read -- the one the incoming object carries -- so the edit
// that points the role at a type that does exist is admitted rather than refused along with it. The
// two halves run against the same cluster, so the refusal is shown to track the name and not an
// empty cluster.
func TestModelDeploymentWebhook_DefaultKeepsTheRepairEditReachable(t *testing.T) {
	it := acceleratableInstanceType("h20-8x", true)
	w := newModelDeploymentWebhookWith([]ctrlcli.Object{it})

	absent := modelDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
		Name: "server", Replicas: 1, InstanceType: "retired-type",
	})
	require.Error(t, w.Default(context.Background(), absent),
		"a role naming a type neither read has is still refused")

	repaired := modelDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
		Name: "server", Replicas: 1, InstanceType: "h20-8x",
	})
	require.NoError(t, w.Default(context.Background(), repaired),
		"and the edit that repairs the reference is not refused with it")
	require.NotNil(t, repaired.Spec.Roles[0].Resources)
	require.NotNil(t, repaired.Spec.Roles[0].Resources.Accelerator,
		"the repaired role is defaulted on the way through")
}

// modelDeploymentWithEveryField builds a deployment carrying a value in every field of spec, so a
// case in the identity table CHANGES a value instead of creating the struct that holds it. Those
// are different edits and only the first tests what the table claims to: comparing nil against a
// populated struct passes for a rule that refuses everything.
func modelDeploymentWithEveryField() *workercore.ModelDeployment {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) {
			r.Kind = workercore.ModelDeploymentRoleKindServer
			r.Resources = &workercore.ModelDeploymentRoleResources{
				Accelerator: resource.NewQuantity(1, resource.DecimalSI),
			}
			r.ExtraArgs = []string{"--max-model-len=8192"}
			r.Env = []workercore.InstanceEnvVar{{Name: "HF_HOME", Value: "/cache"}}
			r.Template = &workercore.ModelDeploymentTemplate{
				Image:             "vllm/vllm-openai:v0.25.1",
				ImagePullPolicy:   core.PullIfNotPresent,
				ImagePullSecret:   &core.LocalObjectReference{Name: "registry"},
				Command:           []string{"/bin/serve"},
				Ports:             []workercore.InstancePort{{Port: 8000}},
				Env:               []workercore.InstanceEnvVar{{Name: "VLLM_LOG", Value: "info"}},
				AdditionalVolumes: []workercore.InstanceAdditionalVolume{{MountPath: "/data"}},
			}
		}),
	)
	// The connector's one legal value is a schema default and a schema enum, with no Go constant to
	// name it; the literal is what a stored object carries.
	md.Spec.KVCache.Connector = "auto"

	return md
}

// TestValidateModelDeploymentIdentity walks every field of spec, one case per field.
//
// BOTH DIRECTIONS ARE REQUIRED AND THAT IS THE POINT OF THE TABLE. A rule that refuses every update
// passes the refusal cases; a rule that refuses nothing passes the acceptance cases; and today's
// code, which has no rule at all, passes the acceptance cases too. Only the two halves together
// distinguish the rule from either. A refusal case asserts the TYPED ERROR ON ITS OWN PATH rather
// than that something was refused, because any other rule refusing the object would satisfy a
// weaker assertion just as convincingly.
func TestValidateModelDeploymentIdentity(t *testing.T) {
	accel2 := resource.NewQuantity(2, resource.DecimalSI)

	cases := []struct {
		name string
		edit func(*workercore.ModelDeployment)
		// refuse is the field path the refusal must name, or "" to require acceptance.
		refuse string
	}{
		// Frozen: what makes this deployment the deployment it is.
		{"model", func(md *workercore.ModelDeployment) { md.Spec.Model.Name = "Qwen/Qwen3-32B" }, "spec.model"},
		{"engine", func(md *workercore.ModelDeployment) {
			md.Spec.Engine = workercore.ModelDeploymentEngineSGLang
		}, "spec.engine"},
		{"kvcache_pool_ref", func(md *workercore.ModelDeployment) {
			md.Spec.KVCache.PoolRef.Name = "another-kv"
		}, "spec.kvCache"},
		{"kvcache_connector", func(md *workercore.ModelDeployment) {
			md.Spec.KVCache.Connector = "mooncake"
		}, "spec.kvCache"},
		{"role_kind", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		}, "spec.roles[0].kind"},
		{"role_instance_type", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].InstanceType = "a100-8x"
		}, "spec.roles[0].instanceType"},
		{"role_resources", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Resources.Accelerator = accel2
		}, "spec.roles[0].resources"},
		{"role_template_command", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.Command = []string{"/bin/other"}
		}, "spec.roles[0].template.command"},
		{"role_added", func(md *workercore.ModelDeployment) {
			md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "decode"
			}))
		}, "spec.roles[1].name"},
		{"role_removed", func(md *workercore.ModelDeployment) {
			md.Spec.Roles = nil
		}, "spec.roles"},

		// Editable: how the deployment is being run right now.
		{"engine_version", func(md *workercore.ModelDeployment) { md.Spec.EngineVersion = "0.26.0" }, ""},
		{"role_replicas", func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 8 }, ""},
		{"role_extra_args", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=16384"}
		}, ""},
		{"role_env", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Env = []workercore.InstanceEnvVar{{Name: "HF_HOME", Value: "/other"}}
		}, ""},
		{"template_image", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
		}, ""},
		{"template_image_pull_policy", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.ImagePullPolicy = core.PullAlways
		}, ""},
		{"template_image_pull_secret", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.ImagePullSecret = &core.LocalObjectReference{Name: "other-registry"}
		}, ""},
		{"template_privileged", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.Privileged = true
		}, ""},
		{"template_ports", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.Ports = []workercore.InstancePort{{Port: 9000}}
		}, ""},
		{"template_env", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.Env = []workercore.InstanceEnvVar{{Name: "VLLM_LOG", Value: "debug"}}
		}, ""},
		{"template_additional_volumes", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Template.AdditionalVolumes = []workercore.InstanceAdditionalVolume{{MountPath: "/other"}}
		}, ""},

		// The no-op every controller and GitOps agent performs constantly.
		{"identical_reapply", func(*workercore.ModelDeployment) {}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := modelDeploymentWithEveryField()
			md := old.DeepCopy()
			tc.edit(md)

			errs := validateModelDeploymentIdentity(md, old)
			if tc.refuse == "" {
				assert.Empty(t, errs, "this field says how the deployment is run, not which one it is")

				return
			}

			require.Len(t, errs, 1, "one changed field is one refusal, on its own path")
			assert.Equal(t, tc.refuse, errs[0].Field, "the refusal names the field the user edited")
			assert.Equal(t, field.ErrorTypeInvalid, errs[0].Type)
			assert.Contains(t, errs[0].Detail, "describes a different deployment",
				"the message states the rule, so a reader knows to create rather than to look for a conflict")
		})
	}
}

// TestValidateModelDeploymentIdentity_RolesAreMatchedByName pins the comparison against a reordered
// list. roles is a listType=map keyed by name, so the two orders are the same object to the API; a
// positional comparison would refuse a declarative apply that changed nothing.
func TestValidateModelDeploymentIdentity_RolesAreMatchedByName(t *testing.T) {
	old := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) {
			r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
		}),
		role(func(r *workercore.ModelDeploymentRole) {
			r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
		}),
	)

	reordered := old.DeepCopy()
	reordered.Spec.Roles[0], reordered.Spec.Roles[1] = reordered.Spec.Roles[1], reordered.Spec.Roles[0]

	assert.Empty(t, validateModelDeploymentIdentity(reordered, old),
		"a reordered listType=map is the same set of roles")

	// The negative baseline: the same reorder, with one frozen field actually changed, is still
	// refused and still names the role's own index in the INCOMING list.
	moved := reordered.DeepCopy()
	moved.Spec.Roles[0].InstanceType = "a100-8x" // this is "decode" after the swap
	errs := validateModelDeploymentIdentity(moved, old)
	require.Len(t, errs, 1)
	assert.Equal(t, "spec.roles[0].instanceType", errs[0].Field)
}

// TestValidateModelDeploymentIdentity_DoesNotRunOnCreate pins that there is nothing to compare
// against on create, so the rule contributes no refusal there.
func TestValidateModelDeploymentIdentity_DoesNotRunOnCreate(t *testing.T) {
	assert.Nil(t, validateModelDeploymentIdentity(modelDeploymentWithEveryField(), nil))
}

// TestModelDeploymentWebhook_ValidateUpdateStatesTheRuleNotTheMechanism pins the wording a user
// meets. The freeze was first argued as "shrink the surface two writers can disagree on" and that
// reason is no longer the one: a message giving it would send an operator hunting for a locking
// problem that does not exist.
func TestModelDeploymentWebhook_ValidateUpdateStatesTheRuleNotTheMechanism(t *testing.T) {
	r := newModelDeploymentWebhookWith(nil)
	old := modelDeploymentWithEveryField()
	md := old.DeepCopy()
	md.Spec.Engine = workercore.ModelDeploymentEngineSGLang

	_, err := r.ValidateUpdate(context.Background(), old, md)
	require.Error(t, err)

	got := err.Error()
	assert.True(t, errsContain(got, "describes a different deployment"), got)
	for _, wrongReason := range []string{"conflict", "concurrent", "lock", "race"} {
		assert.False(t, errsContain(got, wrongReason),
			"the message must not send the reader looking for %q", wrongReason)
	}
}

// TestModelDeploymentWebhook_ValidateUpdateAllowsMetadataAndTheDeletionWindow covers the two edits
// that must keep working, including the one that releases the object.
func TestModelDeploymentWebhook_ValidateUpdateAllowsMetadataAndTheDeletionWindow(t *testing.T) {
	r := newModelDeploymentWebhookWith(nil)

	t.Run("metadata_only_edit", func(t *testing.T) {
		old := modelDeploymentWithEveryField()
		md := old.DeepCopy()
		md.Labels = map[string]string{"team": "a"}
		md.Annotations = map[string]string{"note": "scaled for the demo"}

		_, err := r.ValidateUpdate(context.Background(), old, md)
		assert.NoError(t, err, "this operator writes labels itself; freezing them would break it")
	})

	t.Run("edit_during_deletion", func(t *testing.T) {
		old := modelDeploymentWithEveryField()
		old.DeletionTimestamp = ptr.To(meta.Now())
		old.Finalizers = []string{"worker.gpustack.ai/model-deployment"}
		md := old.DeepCopy()
		md.Finalizers = nil

		_, err := r.ValidateUpdate(context.Background(), old, md)
		assert.NoError(t, err, "a rule aimed at spec must not trap the edit that releases the object")
	})
}
