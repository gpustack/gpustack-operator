package worker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
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
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name: engine, Version: "0.25.1",
			},
			KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
			Roles:   roles,
		},
	}
}

func role(mutate func(*workercore.ModelDeploymentRole)) workercore.ModelDeploymentRole {
	r := workercore.ModelDeploymentRole{Name: "server", Replicas: 4, InstanceType: "h20-8x"}
	mutate(&r)

	return r
}

func TestValidateModelDeploymentHostAccess(t *testing.T) {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
	md.Spec.Roles[0].Privileged = true
	md.Spec.Roles[0].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{{
		MountPath: "/driver", HostPath: &core.HostPathVolumeSource{Path: "/usr/local/driver"},
	}}
	testCases := []struct {
		name              string
		old               *workercore.ModelDeployment
		privilegedAllowed bool
		hostPathAllowed   bool
		wantFields        []string
	}{
		{name: "closed gates reject both", wantFields: []string{"spec.roles[0].privileged", "spec.roles[0].additionalVolumes[0].hostPath"}},
		{name: "open gates admit the same deployment", privilegedAllowed: true, hostPathAllowed: true},
		{name: "privileged gate is independent", privilegedAllowed: true, wantFields: []string{"spec.roles[0].additionalVolumes[0].hostPath"}},
		{name: "host path gate is independent", hostPathAllowed: true, wantFields: []string{"spec.roles[0].privileged"}},
		{name: "held access survives closed gates", old: md.DeepCopy()},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelDeploymentHostAccess(tc.old, md, tc.privilegedAllowed, tc.hostPathAllowed)
			var fields []string
			for _, err := range errs {
				assert.Equal(t, field.ErrorTypeForbidden, err.Type)
				fields = append(fields, err.Field)
			}
			assert.Equal(t, tc.wantFields, fields)
		})
	}
}

// routedModelDeployment builds a one-server-role deployment carrying spec.router, so the router cases
// differ from one another in the router alone and never in the roles.
func routedModelDeployment(mutate func(*workercore.ModelDeploymentRouter)) *workercore.ModelDeployment {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
	router := &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	if mutate != nil {
		mutate(router)
	}
	md.Spec.Router = router

	return md
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

// cards builds a resources block asking for n accelerator cards per member.
func cards(n int64) *workercore.ModelDeploymentRoleResources {
	return &workercore.ModelDeploymentRoleResources{
		Accelerator: resource.NewQuantity(n, resource.DecimalSI),
	}
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
			// The bound is THIS PROJECT'S, so the refusal has to say so. It was Kueue's while every
			// role was one PodSet of a single Workload, and a message naming Kueue would now send a
			// user to look at a Workload their roles no longer become -- each replica carries its
			// own. Naming this operator sends them to ask for the limit to be raised instead,
			// which is where that decision now lives.
			name:        "roles_eleven",
			md:          modelDeployment(workercore.ModelDeploymentEngineVLLM, numberedRoles(11)...),
			wantMessage: "a shape this operator does not serve",
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
			wantMessage: "group that declares a single member",
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
			// REPLACES the case that asserted this was refused, rather than dropping it: this input
			// shape would otherwise have no coverage at all, and it is now the shape the design
			// exists to enable. One Workload still carries one queue name -- what changed is that
			// the roles become two pod groups instead of one, so two queue names are no longer a
			// contradiction. That two groups are actually rendered is asserted where the rendering
			// lives, in TestModelDeployment_TwoTypesAreCreatedAsTwoGroupsInOnePass; what belongs
			// here is that admission lets the shape through.
			name: "role_instance_types_differ",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.InstanceType = "decode", "a100-8x"
				}),
			),
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
			// REPLACES the outlier-first refusal case. Its value was never the refusal: it was the
			// ARRANGEMENT -- an outlier at index 0, which a roles[0]-anchored comparison reports
			// backwards. Keeping the arrangement keeps that coverage pointed at whatever rule reads
			// the set next, and three roles over two types is exactly the input the grouping keys on.
		},
		{
			// The rule is vacuous at length 1, asserted so the single-role behavior cannot regress
			// into needing a second role to agree with.
			name: "role_instance_type_single",
			md:   modelDeployment(workercore.ModelDeploymentEngineVLLM),
		},
		{
			// The legal value is ACCEPTED, not merely defaulted: a role stating one instance of one
			// Pod is the shape every deployment has, and refusing it would make the field unusable
			// rather than merely capped.
			name: "role_size_one",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.ReplicaSize = 1 }),
			),
		},
		{
			// A size above one is ACCEPTED ON CREATE, which is the whole of what create has to say
			// about it: an instance of several Pods is a shape the renderer builds, not a request
			// some later rule has to talk the user out of. What cannot happen to it is a change,
			// and that is an update rule, asserted against a stored object rather than here.
			name: "role_size_two",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.ReplicaSize = 2 }),
			),
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
			// SGLang renders the split as its own disaggregation arguments, so this pair is now
			// admitted. The row is kept as the witness: the rule below still stands, and this is
			// what says it stopped answering for this engine.
			name: "role_kind_prefill_and_decode_on_sglang",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
				}),
			),
		},
		{
			// The rule that used to refuse the row above, on the shape it still answers for: a kind
			// no renderer has a term for. The schema's enum keeps this value off the API, so the
			// object is built here in Go -- which is also how an engine added to the renderer
			// without a support-table entry would reach the rule, and is why it is kept.
			name: "role_kind_with_no_rendering_term",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "router", "router"
				}),
			),
			wantMessage: `has no rendering term for kind "router"`,
		},
		{
			// The same engine with the kind it does render is accepted, so the case above fails for
			// the engine and not because two roles or a kind field are refused outright.
			name: "role_kind_server_on_sglang",
			md:   modelDeployment(workercore.ModelDeploymentEngineSGLang),
		},
		{
			// Nothing consuming these roles expresses a second prefiller, so the extra role would
			// render into a configuration nothing downstream can reach.
			name: "role_kinds_two_prefills",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill-a", workercore.ModelDeploymentRoleKindPrefill
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "prefill-b", workercore.ModelDeploymentRoleKindPrefill
				}),
			),
			wantMessage: `kind "prefill" is already declared by role "prefill-a"`,
		},
		{
			// The other non-server kind, so the rule is not a check spelled against prefill alone.
			name: "role_kinds_two_decodes",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "decode-a", workercore.ModelDeploymentRoleKindDecode
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "decode-b", workercore.ModelDeploymentRoleKindDecode
				}),
			),
			wantMessage: `kind "decode" is already declared by role "decode-a"`,
		},
		{
			// THE CASE THE RULE EXISTS TO NOT CATCH, and the one the two above cannot stand without: a
			// table holding only repeated non-server kinds passes just as well against a rule refusing
			// ANY repeated kind, which would refuse this. A set of servers is a set of equals and
			// something in front of them can pick between them.
			name: "role_kinds_two_servers",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "server-a", workercore.ModelDeploymentRoleKindServer
				}),
				role(func(r *workercore.ModelDeploymentRole) {
					r.Name, r.Kind = "server-b", workercore.ModelDeploymentRoleKindServer
				}),
			),
		},
		{
			// Three, so the exemption is not "a second server is tolerated" read as a bound.
			name: "role_kinds_three_servers",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "server-a" }),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "server-b" }),
				role(func(r *workercore.ModelDeploymentRole) { r.Name = "server-c" }),
			),
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
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}}
			})),
			wantMessage: "MOONCAKE_CONFIG_PATH",
		},
		{
			// The config-path variable is owned per engine too: SGLang reads its own, and
			// MOONCAKE_CONFIG_PATH is not what the operator rendered for it.
			name: "env_owned_key_wrong_engine",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}}
			})),
		},
		{
			name: "env_defaulted_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "MC_TE_METRIC", Value: "0"}}
			})),
		},
		{
			// The rank keys are owned WHATEVER THE ENGINE, because they describe the Pod's shape
			// rather than anything an engine reads by name. Refusing them is not tidiness: the
			// index is rendered as a fieldRef, a user entry of that name would be merged onto it by
			// value, and an EnvVar carrying both a value and a source is refused by the API server
			// -- so an unowned key here turns a legal deployment into one that cannot render.
			name: "env_rank_key_is_owned_on_every_engine",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang, role(func(r *workercore.ModelDeploymentRole) {
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "GPUSTACK_MEMBER_INDEX", Value: "0"}}
			})),
			wantMessage: "GPUSTACK_MEMBER_INDEX",
		},
		{
			// Owned at size one as well, where nothing renders it. A rule that switched on with a
			// field value would let the key through on create and refuse it only once an instance
			// was widened -- and `size` is frozen, so that edit is a new deployment away.
			name: "env_rank_key_is_owned_at_size_one",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.ReplicaSize = 1
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "GPUSTACK_REPLICA_SIZE", Value: "8"}}
			})),
			wantMessage: "GPUSTACK_REPLICA_SIZE",
		},
		{
			// A role that took over the command line is refused too, because the renderer drops
			// owned keys unconditionally. Admission and rendering must agree on the set: whichever
			// way they disagree, the result is a value the user wrote and nothing reads.
			name: "env_owned_key_take_over",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Image = "vllm/vllm-openai:latest"
				r.Command = []string{"python", "-m", "vllm.entrypoints.openai.api_server"}
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}}
			})),
			wantMessage: "roles[0].env[0]",
		},
		{
			name: "env_unowned_key",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Image = "vllm/vllm-openai:latest"
				r.Env = []workercore.ModelDeploymentEnvVar{{Name: "HF_HOME", Value: "/weights"}}
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
			name: "role_command",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Image = "vllm/vllm-openai:latest"
				r.Command = []string{"vllm", "serve", "/models/qwen"}
			})),
		},
		{
			// Every field set, and all of them legal: this operator runs the router, so there is no mode
			// in which any of them is meaningless. The case is here because the type once carried a mode
			// that made three of them conditional, and its removal has to be visible as acceptance rather
			// than as an absent test.
			name: "router_with_every_field",
			md: routedModelDeployment(func(r *workercore.ModelDeploymentRouter) {
				r.Replicas, r.Image, r.ExtraArgs = ptr.To(int32(2)), "ghcr.io/example/router:v1", []string{"--verbose"}
			}),
		},
		{
			name: "router_extra_args_owned_key",
			md: routedModelDeployment(func(r *workercore.ModelDeploymentRouter) {
				r.ExtraArgs = []string{"--endpoint-selector=mine"}
			}),
			wantMessage: "spec.router.extraArgs[0]",
		},
		{
			name: "router_secure_serving_is_owned",
			md: routedModelDeployment(func(r *workercore.ModelDeploymentRouter) {
				r.ExtraArgs = []string{"--secure-serving=true"}
			}),
			wantMessage: "spec.router.extraArgs[0]",
		},
		{
			// The other half of the east-west boundary. Asserted beside the case above rather than
			// left to it, because the two flags are refused together and a catalog that lost one of
			// them would still pass every assertion the other one carries.
			name: "router_metrics_endpoint_auth_is_owned",
			md: routedModelDeployment(func(r *workercore.ModelDeploymentRouter) {
				r.ExtraArgs = []string{"--metrics-endpoint-auth=true"}
			}),
			wantMessage: "spec.router.extraArgs[0]",
		},
		{
			name: "router_extra_args_unowned_key",
			md: routedModelDeployment(func(r *workercore.ModelDeploymentRouter) {
				r.ExtraArgs = []string{"--zap-log-level=debug"}
			}),
		},
		{
			name: "router_metric_unavailable",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = "engine-without-metrics"
				return md
			}(),
			wantMessage: `metric "queued requests" required by router "llm-d-router" is unavailable on engine "engine-without-metrics"`,
		},
		{
			name: "router_roles_use_different_serving_ports",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill"
						r.Ports = []workercore.ModelDeploymentPort{{Port: 8000}}
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode"
						r.Ports = []workercore.ModelDeploymentPort{{Port: 8100}}
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			}(),
			wantMessage: "every routed role must use the same serving port; got [8000 8100]",
		},
		{
			name: "router_role_serving_port_is_not_tcp",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8000, Protocol: core.ProtocolUDP}}
				return md
			}(),
			wantMessage: "every routed role must use TCP serving ports",
		},
		{
			name: "router_role_uses_tls",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].ExtraArgs = []string{"--ssl-keyfile=/tls/key.pem"}
				return md
			}(),
			wantMessage: "managed router supports plaintext engine endpoints only",
		},
		{
			// The ZMQ publisher is synthesized onto the same container, so the collision is two
			// processes binding one port -- which the replica reports as a crash, not admission.
			name: "router_role_declares_kv_events_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 5557, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 5557, which the operator reserves`,
		},
		{
			name: "router_role_declares_kv_replay_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8000, Protocol: core.ProtocolTCP}, {Port: 5558, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 5558, which the operator reserves`,
		},
		{
			// The bootstrap server runs on the PREFILLER, so that is the role the port is reserved
			// on. The decode half below it is what says the reservation follows the listener, which
			// renders on a prefiller of a declared pair alone.
			name: "router_role_declares_mooncake_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
					r.Name = "decode"
					r.Kind = workercore.ModelDeploymentRoleKindDecode
				}))
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 8998, which the operator reserves`,
		},
		{
			// THE ROW THAT SAYS THE RESERVATION DOES NOT FOLLOW A ROUTER NAME. The leg binding this
			// listener renders under every router on a vendor that can drive it, and this rule
			// cannot read the vendor, so a rule keyed to one router releases the port here while
			// the render still binds it -- two container ports numbered alike, which admission is
			// the last place able to refuse. The row above covers the same declaration under the
			// other router, so the pair pins that the two agree rather than that either one passes.
			name: "vllm_router_prefill_role_declares_mooncake_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
					r.Name = "decode"
					r.Kind = workercore.ModelDeploymentRoleKindDecode
				}))
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 8998, which the operator reserves`,
		},
		{
			// THE PAIR DISCRIMINATOR ON THE vLLM SIDE: the leg renders on a prefiller of a declared
			// pair alone, so a lone prefiller under a router binds no bootstrap server and keeps
			// the port free -- the reservation reads the same pair axis the render reads, because
			// unlike the vendor it is knowable here.
			name: "lone_prefill_under_a_router_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// A server role binds no bootstrap listener -- the transfer leg is rendered on the two
			// halves alone -- so the same declaration is an ordinary port. Reserving it here would
			// refuse a port nothing uses.
			name: "router_server_role_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// The mirror on the other engine, which the vLLM-only rule released: SGLang renders its
			// own bootstrap registry on a prefiller of a declared pair, under any router and under
			// none.
			name: "sglang_prefill_role_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
					r.Name = "decode"
					r.Kind = workercore.ModelDeploymentRoleKindDecode
				}))
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 8998, which the operator reserves`,
		},
		{
			// And a decoder on that engine does not serve the registry, so it is released there.
			name: "sglang_decode_role_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// THE ROW THAT SAYS THE RESERVATION DOES NOT FOLLOW A ROUTER AT ALL. SGLang's bootstrap
			// registry follows the role of a declared pair, and a prefiller attached to a shared
			// pool binds it with nothing routing the halves -- so the same declaration is refused
			// on an unrouted deployment too, where the router-gated walk never ran.
			name: "unrouted_sglang_prefill_of_a_pair_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Router = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
					r.Name = "decode"
					r.Kind = workercore.ModelDeploymentRoleKindDecode
				}))
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
			wantMessage: `role "server" declares port 8998, which the operator reserves`,
		},
		{
			// THE PAIR DISCRIMINATOR ON THE SGLANG SIDE: without the decode half the registry does
			// not render either -- the split follows the pair on this engine -- so an unrouted lone
			// prefiller feeding a shared pool keeps the port free.
			name: "unrouted_sglang_lone_prefill_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Router = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// The two vLLM listeners render only under a router -- the event publisher feeds one
			// router's data layer, and the bootstrap server follows a routed pair -- so an unrouted
			// vLLM role reserves nothing and the same declaration is an ordinary port.
			name: "unrouted_vllm_prefill_role_declares_the_bootstrap_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Router = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 8998, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// The reserved ports are vLLM's synthesized listeners; on another engine nothing binds
			// them and the same declaration is an ordinary port.
			name: "router_role_declares_5557_on_sglang",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 5557, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// A take-over role owns its command line and the render synthesizes no listener onto
			// it, so a reserved number is an ordinary port there too.
			name: "router_take_over_role_declares_kv_events_port",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles[0].Command = []string{"/bin/my-server"}
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 5557, Protocol: core.ProtocolTCP}}
				return md
			}(),
		},
		{
			// A direct decode role is fronted by a proxy that takes the serving port, so a role that
			// pins its model server to that same port fails the render on every pass -- permanently.
			name: "router_direct_decode_binds_the_serving_port",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
						r.ExtraArgs = []string{"--port=8000"}
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			}(),
			wantMessage: `role "decode" passes --port=8000, the port its Service publishes`,
		},
		{
			// The same flag in the split spelling, so the check cannot be ducked by formatting.
			name: "router_direct_decode_binds_the_serving_port_split_spelling",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
						r.ExtraArgs = []string{"--port", "8000"}
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			}(),
			wantMessage: `role "decode" passes --port=8000, the port its Service publishes`,
		},
		{
			// Any other value is the proxy's own target port and is exactly what the render wants.
			name: "router_direct_decode_binds_another_port",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
						r.ExtraArgs = []string{"--port=8200"}
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			}(),
		},
		{
			// A take-over decode role gets no proxy, so its --port is the whole command line's own
			// business and naming the serving port is correct rather than fatal.
			name: "router_unmanaged_decode_binds_the_serving_port",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "prefill", workercore.ModelDeploymentRoleKindPrefill
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name, r.Kind = "decode", workercore.ModelDeploymentRoleKindDecode
						r.Command = []string{"/bin/my-server", "--port=8000"}
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			}(),
		},
		{
			name: "kv_cache_absent",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.KVCache = nil
				return md
			}(),
		},
		{
			// A deployment whose roles are all servers may be routed. A router here is east-west
			// traffic management rather than a prefill/decode pairer: one that scores on a cache view
			// picks between several servers in a way a Service cannot.
			name: "router_in_front_of_servers_only",
			md: func() *workercore.ModelDeployment {
				md := routedModelDeployment(nil)
				md.Spec.Roles = append(md.Spec.Roles,
					role(func(r *workercore.ModelDeploymentRole) { r.Name = "server-b" }))

				return md
			}(),
		},
		{
			name: "extra_args_parallel_degree_missing_value",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--max-model-len=8192", "--tensor-parallel-size"}
				})),
			wantMessage: "the --tensor-parallel-size declaration has no value",
		},
		{
			name: "extra_args_parallel_degree_not_an_integer",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=two"}
				})),
			wantMessage: `the --tensor-parallel-size value "two" is not an integer`,
		},
		{
			name: "extra_args_parallel_degree_out_of_range",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=99999999999999999999"}
				})),
			wantMessage: `the --tensor-parallel-size value "99999999999999999999" is out of range`,
		},
		{
			name: "extra_args_parallel_degree_below_bound",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--pipeline-parallel-size=0"}
				})),
			wantMessage: "the --pipeline-parallel-size value 0 is below 1",
		},
		{
			// The variable is vLLM's second spelling of the data-parallel degree, and it is on the
			// books exactly when no flag drives data parallelism -- as here.
			name: "extra_args_parallel_dp_env_malformed",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "two"}}
				})),
			wantMessage: `the VLLM_DP_SIZE value "two" is not an integer`,
		},
		{
			// The engine's own precedence resolves a doubly stated DP: the flag drives data
			// parallelism, so the malformed variable is never read and costs nothing.
			name: "extra_args_parallel_dp_env_shadowed_by_flag",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--data-parallel-size=2"}
					r.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "two"}}
				})),
		},
		{
			// A take-over role's books are its command: a broken degree on the very line that
			// runs is refused the same as a managed role's.
			name: "extra_args_parallel_takeover_command_is_parsed",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Command = []string{
						"python", "-m", "vllm.entrypoints.openai.api_server", "--tensor-parallel-size=two",
					}
				})),
			wantMessage: `the --tensor-parallel-size value "two" is not an integer`,
		},
		{
			// The inert extra arguments beside a take-over command are no stream at all: nobody
			// parses them, not even to refuse them.
			name: "extra_args_parallel_inert_beside_takeover_is_not_parsed",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Command = []string{"python", "-m", "vllm.entrypoints.openai.api_server"}
					r.ExtraArgs = []string{"--tensor-parallel-size=two"}
				})),
		},
		{
			name: "parallel_width_fits_the_cards",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = cards(2)
				})),
		},
		{
			name: "parallel_width_exceeds_the_cards",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = cards(1)
				})),
			wantMessage: "the engine needs 2 per member",
		},
		{
			// The width is per member: two members of one card each are not a two-card member.
			name: "parallel_width_size_rescues_nothing",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = cards(1)
					r.ReplicaSize = 2
				})),
			wantMessage: "does not rescue it",
		},
		{
			name: "parallel_width_data_parallel_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2", "--data-parallel-size=2"}
					r.Resources = cards(3)
				})),
			wantMessage: "3 cards cannot hold the width the role declares (tensor-parallel 2, data-parallel 2): the engine needs 4 per member",
		},
		{
			name: "parallel_width_data_parallel_multiplies_admitted",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2", "--data-parallel-size=2"}
					r.Resources = cards(4)
				})),
		},
		{
			name: "parallel_width_pipeline_parallel_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--pipeline-parallel-size=2"}
					r.Resources = cards(1)
				})),
			wantMessage: "the engine needs 2 per member",
		},
		{
			name: "parallel_width_sglang_pipeline_parallel_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--pp-size=2"}
					r.Resources = cards(1)
				})),
			wantMessage: "the engine needs 2 per member",
		},
		{
			// Prefill context parallelism is not one of the modes that narrows the SGLang
			// width, so the data-parallel width still multiplies beside it.
			name: "parallel_width_sglang_prefill_cp_does_not_narrow",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--enable-prefill-cp"}
					r.Resources = cards(3)
				})),
			wantMessage: "the engine needs 4 per member",
		},
		{
			// A declared zero local share is the engine's own sentinel for DP specified
			// externally; the check reads it as undeclared, so the width still multiplies.
			name: "parallel_width_declared_zero_local_still_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{
						"--tensor-parallel-size=2", "--data-parallel-size=2", "--data-parallel-size-local=0",
					}
					r.Resources = cards(3)
				})),
			wantMessage: "the engine needs 4 per member",
		},
		{
			// A declared local share of one places exactly one DP rank on the member, so the
			// width multiplies by one and the two cards hold the tensor pair.
			name: "parallel_width_declared_local_share_of_one_multiplies_by_one",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{
						"--tensor-parallel-size=2", "--data-parallel-size=2", "--data-parallel-size-local=1",
					}
					r.Resources = cards(2)
				})),
		},
		{
			// With no wiring flag, a declared local share is exactly the DP ranks this member
			// runs: eight of them on a two-card member cannot start.
			name: "parallel_width_declared_local_share_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{
						"--data-parallel-size=8", "--data-parallel-size-local=8",
					}
					r.Resources = cards(2)
				})),
			wantMessage: "2 cards cannot hold the width the role declares (data-parallel-local 8): the engine needs 8 per member",
		},
		{
			name: "parallel_width_declared_local_share_admitted_when_it_fits",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{
						"--tensor-parallel-size=2", "--data-parallel-size=2", "--data-parallel-size-local=2",
					}
					r.Resources = cards(4)
				})),
		},
		{
			// Degrees each inside the host int still overflow the product: the width pins one
			// past the cards rather than wrap, and the refusal names no wrapped figure.
			name: "parallel_width_overflowing_product_still_refuses",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{
						"--tensor-parallel-size=4000000000", "--pipeline-parallel-size=3000000000",
					}
					r.Resources = cards(8)
				})),
			wantMessage: "the engine needs more than 8 per member",
		},
		{
			name: "parallel_width_wiring_flag_silences",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2", "--nnodes=2"}
					r.Resources = cards(1)
				})),
		},
		{
			name: "parallel_width_prefill_context_parallel_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2", "--prefill-context-parallel-size=2"}
					r.Resources = cards(3)
				})),
			wantMessage: "the engine needs 4 per member",
		},
		{
			// Decode-context parallelism reuses the tensor-parallel ranks, so it adds no cards.
			name: "parallel_width_decode_context_parallel_reuses_tp_ranks",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--decode-context-parallel-size=4"}
					r.Resources = cards(1)
				})),
		},
		{
			// An explicit zero request is a value the user wrote; the check stays silent on it.
			name: "parallel_width_zero_cards_stays_silent",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = cards(0)
				})),
		},
		{
			// A fractional card count is not a readable count of cards.
			name: "parallel_width_fractional_cards_stay_silent",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = &workercore.ModelDeploymentRoleResources{
						Accelerator: resource.NewMilliQuantity(1500, resource.DecimalSI),
					}
				})),
		},
		{
			// The env spelling of the degree is on the same books, so it enters the width too.
			name: "parallel_width_dp_env_is_on_the_books",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "2"}}
					r.Resources = cards(3)
				})),
			wantMessage: "the engine needs 4 per member",
		},
		{
			name: "parallel_width_sglang_data_parallel_multiplies",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2"}
					r.Resources = cards(3)
				})),
			wantMessage: "the engine needs 4 per member",
		},
		{
			// DP attention moves the data-parallel width inside the tensor world.
			name: "parallel_width_sglang_dp_attention_narrows",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--enable-dp-attention"}
					r.Resources = cards(2)
				})),
		},
		{
			name: "parallel_width_sglang_dwdp_narrows",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--dwdp-size=2"}
					r.Resources = cards(2)
				})),
		},
		{
			name: "parallel_width_sglang_moe_dp_narrows",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--moe-dp-size=2"}
					r.Resources = cards(2)
				})),
		},
		{
			name: "parallel_width_sglang_attention_cp_narrows",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--attn-cp-size=2"}
					r.Resources = cards(2)
				})),
		},
		{
			name: "parallel_width_sglang_wiring_silences",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tp-size=2", "--dp-size=2", "--nnodes=2"}
					r.Resources = cards(1)
				})),
		},
		{
			// A take-over role's books are its command, so a degree on that line enters the
			// width exactly as a managed role's does.
			name: "parallel_width_takeover_command_counts",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Command = []string{"/bin/my-server", "--tensor-parallel-size=2"}
					r.Resources = cards(1)
				})),
			wantMessage: "the engine needs 2 per member",
		},
		{
			// The inert extra arguments beside a take-over command enter no width.
			name: "parallel_width_inert_args_beside_takeover_stay_silent",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.Command = []string{"/bin/my-server"}
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = cards(1)
				})),
		},
		{
			// A resources block that asks for no accelerator carries no card count to compare.
			name: "parallel_width_nil_accelerator_stays_silent",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--tensor-parallel-size=2"}
					r.Resources = &workercore.ModelDeploymentRoleResources{
						AcceleratorSlicedMemoryPercentage: 50,
					}
				})),
		},
		{
			// Expert parallelism is a mode in vLLM: it adds no ranks and never enters the product.
			name: "parallel_width_vllm_expert_parallel_mode_adds_no_cards",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--enable-expert-parallel"}
					r.Resources = cards(1)
				})),
		},
		{
			// SGLang's expert-parallel DEGREE is stored by the parse but is not part of the width.
			name: "parallel_width_sglang_expert_parallel_adds_no_cards",
			md: modelDeployment(workercore.ModelDeploymentEngineSGLang,
				role(func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--ep-size=4"}
					r.Resources = cards(1)
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

// TestValidateModelDeploymentRoleExtraArgs_AnEngineWithNoTableAdmitsEverything pins the parse's
// open world: an engine the table does not know parses to all ones without error, so its flags --
// known or not -- stay the engine's own business.
func TestValidateModelDeploymentRoleExtraArgs_AnEngineWithNoTableAdmitsEverything(t *testing.T) {
	r := role(func(r *workercore.ModelDeploymentRole) {
		r.ExtraArgs = []string{"--tensor-parallel-size", "2", "--such-a-flag"}
	})

	assert.Empty(t, validateModelDeploymentRoleExtraArgs(
		"test-engine", &r, field.NewPath("spec", "roles").Index(0)))
}

// TestValidateModelDeploymentRoleParallelWidth_DoesNotRefuseTwice pins the silence rule: a
// declaration the parse cannot read is the parse's refusal and only the parse's refusal -- the
// width check has no books to multiply and adds no second error of its own.
func TestValidateModelDeploymentRoleParallelWidth_DoesNotRefuseTwice(t *testing.T) {
	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) {
			r.ExtraArgs = []string{"--tensor-parallel-size=two"}
			r.Resources = cards(1)
		}))

	errs := validateModelDeployment(md, nil)
	require.Len(t, errs, 1, "the unreadable declaration is the parse's refusal alone")
	assert.Contains(t, errs[0].Error(), "is not an integer")
}

// TestValidateModelDeploymentRoleAdditionalVolumes covers the three rules the field documentation
// states and the schema cannot carry: a mount path used twice, a subPath that climbs out of its
// volume, and an entry naming no source. The first two used to reach the API server as a rejected
// Pod on every reconcile pass; the third reached nothing at all, because the renderer skips such an
// entry and the container simply comes up without the mount.
//
// The ".." cases are the pair that matters: a segment equal to ".." is the traversal, while a NAME
// merely containing those characters is an ordinary directory and must pass.
//
// THE SOURCELESS CASE IS PAIRED WITH ONE PER SOURCE, because the rule is a three-way disjunction: an
// implementation testing only configMap accepts the same table, and each accepting case is what says
// the other two sources still satisfy it.
func TestValidateModelDeploymentRoleAdditionalVolumes(t *testing.T) {
	volumes := func(avs ...workercore.ModelDeploymentAdditionalVolume) *workercore.ModelDeployment {
		return modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) { r.AdditionalVolumes = avs }))
	}
	cm := &core.LocalObjectReference{Name: "weights"}

	testCases := []struct {
		name        string
		md          *workercore.ModelDeployment
		wantMessage string
	}{
		{
			name: "duplicate_mount_path",
			md: volumes(
				workercore.ModelDeploymentAdditionalVolume{MountPath: "/data", ConfigMap: cm},
				workercore.ModelDeploymentAdditionalVolume{MountPath: "/data", ConfigMap: cm},
			),
			wantMessage: "is already mounted by",
		},
		{
			name: "sub_path_climbs_out",
			md: volumes(workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/data", SubPath: "a/../../etc", ConfigMap: cm,
			}),
			wantMessage: `must not contain a ".." element`,
		},
		{
			name: "sub_path_is_only_dot_dot",
			md: volumes(workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/data", SubPath: "..", ConfigMap: cm,
			}),
			wantMessage: `must not contain a ".." element`,
		},
		{
			name: "no_source_named",
			md:   volumes(workercore.ModelDeploymentAdditionalVolume{MountPath: "/data"}),
			// Named by mount path, because that is the only thing the entry carries: an error
			// quoting the index alone leaves the writer counting list entries.
			wantMessage: `"/data" names something to mount`,
		},
		{
			name: "secret_source_passes",
			md: volumes(workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/creds", Secret: &core.LocalObjectReference{Name: "hf-token"},
			}),
			wantMessage: "",
		},
		{
			name: "host_path_source_passes",
			md: volumes(workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/dev/shm", HostPath: &core.HostPathVolumeSource{Path: "/dev/shm"},
			}),
			wantMessage: "",
		},
		{
			name: "distinct_paths_and_an_ordinary_sub_path_pass",
			md: volumes(
				workercore.ModelDeploymentAdditionalVolume{
					MountPath: "/data", SubPath: "a..b/c", ConfigMap: cm,
				},
				workercore.ModelDeploymentAdditionalVolume{MountPath: "/cache", ConfigMap: cm},
			),
			wantMessage: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelDeploymentRoles(tc.md)
			if tc.wantMessage == "" {
				assert.Empty(t, errs, "a legal set of volumes is not refused")

				return
			}
			require.NotEmpty(t, errs, "the rule must refuse this shape")
			assert.True(t, errsContain(errs.ToAggregate().Error(), tc.wantMessage),
				"the refusal names what is wrong; got %q", errs.ToAggregate().Error())
		})
	}
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

// servingInstanceType is an acceleratable InstanceType whose status the reconciler has computed: a
// non-empty Manufacturer is what marks the detail ready, and the ceiling is what the pool hands out
// at once.
//
// It offers NEITHER slicing NOR partitioning unless an option adds one, because those are the two
// capabilities the mode rule reads and a fixture that quietly offered both would let a refusal case
// pass for the wrong reason. The not-yet-synced state is a separate fixture rather than an option
// here, so that "ready" is never something a case forgets to ask for.
func servingInstanceType(
	name string, ceiling int64, opts ...func(*worker.InstanceType),
) *worker.InstanceType {
	instType := acceleratableInstanceType(name, true)
	instType.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
	instType.Status.Accelerator.OnceMaxRequest = *resource.NewQuantity(ceiling, resource.DecimalSI)

	for _, opt := range opts {
		opt(instType)
	}

	return instType
}

// offeringLogicalSlices makes the pool report a logically sliceable card. The count is what the
// predicate reads; the value beyond "non-zero" carries no meaning here.
func offeringLogicalSlices(instType *worker.InstanceType) {
	instType.Status.Detail.SlicedDetail.Logical.Count = 1
}

// offeringPartitionProfiles makes the pool report hardware partitioning AND the inventory it serves.
// The capability count and the profile list are independent fields, and a fixture setting only the
// list would model a pool that cannot partition while naming profiles -- a state the reconciler does
// not produce, and one that would let the capability branch go untested.
func offeringPartitionProfiles(profiles ...string) func(*worker.InstanceType) {
	return func(instType *worker.InstanceType) {
		instType.Status.Detail.SlicedDetail.Physical.Count = 1
		for _, name := range profiles {
			instType.Status.Detail.SlicedDetail.Physical.Profiles = append(
				instType.Status.Detail.SlicedDetail.Physical.Profiles,
				workercore.AcceleratorSlicedPhysicalDetailProfile{Name: name, Count: 1, MemoryMib: 10240})
		}
	}
}

// notSyncedInstanceType is an acceleratable type whose reconciler has not run: an empty Detail, which
// a reader must treat as "not known yet" and never as "offers no modes".
func notSyncedInstanceType(name string) *worker.InstanceType {
	return acceleratableInstanceType(name, true)
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
			// AN EXPLICIT ZERO IS A VALUE THE USER WROTE. It is kept because replacing it would
			// make the request stop meaning what it says; validation owns unsafe combinations.
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

// TestModelDeploymentWebhook_ValidateRefusesZeroAcceleratorInAMultiRoleGroup covers the explicit
// value that defaulting deliberately leaves alone. A role asking for no accelerator on an
// acceleratable type requests nothing its queue accounts for, so its replicas are admitted and run
// while that queue charges them nothing -- and the siblings sharing the type are charged for every
// card they hold, competing for a pool this role spends from uncounted.
func TestModelDeploymentWebhook_ValidateRefusesZeroAcceleratorInAMultiRoleGroup(t *testing.T) {
	withDerivedFromNode(t, true)

	zero := resource.NewQuantity(0, resource.DecimalSI)
	one := resource.NewQuantity(1, resource.DecimalSI)
	zeroOnA100 := roleWithAccelerator("decode", zero)
	zeroOnA100.InstanceType = "a100-8x"

	testCases := []struct {
		name      string
		roles     []workercore.ModelDeploymentRole
		types     []ctrlcli.Object
		refusedAt []string
	}{
		{
			name: "two zero-card roles sharing an accelerated type",
			roles: []workercore.ModelDeploymentRole{
				roleWithAccelerator("prefill", zero),
				roleWithAccelerator("decode", zero),
			},
			types: []ctrlcli.Object{servingInstanceType("h20-8x", 8)},
			refusedAt: []string{
				"spec.roles[0].resources.accelerator",
				"spec.roles[1].resources.accelerator",
			},
		},
		{
			name: "one zero-card role beside a covered role",
			roles: []workercore.ModelDeploymentRole{
				roleWithAccelerator("prefill", zero),
				roleWithAccelerator("decode", one),
			},
			types:     []ctrlcli.Object{servingInstanceType("h20-8x", 8)},
			refusedAt: []string{"spec.roles[0].resources.accelerator"},
		},
		{
			name:  "one zero-card role on an accelerated type",
			roles: []workercore.ModelDeploymentRole{roleWithAccelerator("server", zero)},
			types: []ctrlcli.Object{servingInstanceType("h20-8x", 8)},
		},
		{
			name: "zero-card roles in separate scheduling groups",
			roles: []workercore.ModelDeploymentRole{
				roleWithAccelerator("prefill", zero),
				zeroOnA100,
			},
			types: []ctrlcli.Object{
				servingInstanceType("h20-8x", 8),
				servingInstanceType("a100-8x", 8),
			},
		},
		{
			name: "zero-card roles sharing a CPU-only type",
			roles: []workercore.ModelDeploymentRole{
				roleWithAccelerator("prefill", zero),
				roleWithAccelerator("decode", zero),
			},
			types: []ctrlcli.Object{acceleratableInstanceType("h20-8x", false)},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			w := newModelDeploymentWebhookWith(tc.types)
			md := modelDeployment(workercore.ModelDeploymentEngineVLLM, tc.roles...)
			_, err := w.ValidateCreate(context.Background(), md)

			if len(tc.refusedAt) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			for _, path := range tc.refusedAt {
				assert.Contains(t, err.Error(), path)
			}
			assert.Contains(t, err.Error(), "request at least one accelerator")
			assert.Contains(t, err.Error(), "non-acceleratable instance type")
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
			r.Env = []workercore.ModelDeploymentEnvVar{{Name: "HF_HOME", Value: "/cache"}}
			r.Image = "vllm/vllm-openai:v0.25.1"
			r.ImagePullPolicy = core.PullIfNotPresent
			r.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry"}}
			r.Command = []string{"/bin/serve"}
			r.Ports = []workercore.ModelDeploymentPort{{Port: 8000}}
			r.AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{
				{MountPath: "/data", ConfigMap: &core.LocalObjectReference{Name: "weights"}},
			}
			r.Env = append(r.Env, []workercore.ModelDeploymentEnvVar{{Name: "VLLM_LOG", Value: "info"}}...)
		}),
	)
	// The connector's one legal value is a schema default and a schema enum, with no Go constant to
	// name it; the literal is what a stored object carries.
	md.Spec.KVCache.Connector = "mooncake"

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
			md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		}, "spec.engine.name"},
		{"kvcache_pool_ref", func(md *workercore.ModelDeployment) {
			md.Spec.KVCache.PoolRef.Name = "another-kv"
		}, "spec.kvCache"},
		{"kvcache_connector", func(md *workercore.ModelDeployment) {
			md.Spec.KVCache.Connector = "auto"
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
		{"role_command", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Command = []string{"/bin/other"}
		}, "spec.roles[0].command"},
		{"role_added", func(md *workercore.ModelDeployment) {
			md.Spec.Roles = append(md.Spec.Roles, role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "decode"
			}))
		}, "spec.roles[1].name"},
		{"role_removed", func(md *workercore.ModelDeployment) {
			md.Spec.Roles = nil
		}, "spec.roles"},

		// Editable: how the deployment is being run right now.
		{"engine_version", func(md *workercore.ModelDeployment) { md.Spec.Engine.Version = "0.26.0" }, ""},
		{"role_replicas", func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 8 }, ""},
		{"role_extra_args", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=16384"}
		}, ""},
		{"role_env", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{{Name: "HF_HOME", Value: "/other"}}
		}, ""},
		{"role_image", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
		}, ""},
		{"role_image_pull_policy", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ImagePullPolicy = core.PullAlways
		}, ""},
		{"role_image_pull_secrets", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ImagePullSecrets = []core.LocalObjectReference{{Name: "other-registry"}}
		}, ""},
		{"role_privileged", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Privileged = true
		}, ""},
		{"role_ports", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 9000}}
		}, ""},
		{"role_additional_volumes", func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{
				{MountPath: "/other", ConfigMap: &core.LocalObjectReference{Name: "weights"}},
			}
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

func TestValidateModelDeploymentRouterName(t *testing.T) {
	cases := []struct {
		name    string
		old     *workercore.ModelDeploymentRouter
		current *workercore.ModelDeploymentRouter
		refuse  bool
	}{
		{
			name: "router name changed outside the schema",
			old:  &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
			// This becomes reachable through the API server when the router-name enum widens.
			current: &workercore.ModelDeploymentRouter{Name: "another-router"},
			refuse:  true,
		},
		{
			name:    "router name unchanged",
			old:     &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
			current: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
		},
		{
			name:    "router added",
			current: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
		},
		{
			name: "router removed",
			old:  &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
			old.Spec.Router = tc.old
			md := old.DeepCopy()
			md.Spec.Router = tc.current

			errs := validateModelDeploymentRouterName(md, old)
			if !tc.refuse {
				assert.Empty(t, errs)
				return
			}

			require.Len(t, errs, 1)
			assert.Equal(t, "spec.router.name", errs[0].Field)
			assert.Equal(t, field.ErrorTypeInvalid, errs[0].Type)
			assert.Equal(t, "field is immutable", errs[0].Detail)
			assert.NotEqual(t, modelDeploymentIdentityMessage, errs[0].Detail)
		})
	}
}

// TestModelDeploymentWebhook_ValidateUpdateStatesTheRuleNotTheMechanism pins the wording a user
// meets. The freeze was first argued as "shrink the surface two writers can disagree on" and that
// reason is no longer the one: a message giving it would send an operator hunting for a locking
// problem that does not exist.
func TestModelDeploymentWebhook_ValidateUpdateStatesTheRuleNotTheMechanism(t *testing.T) {
	// The type has to be live now that two of the rules read it; without it the handler answers
	// about the name instead, and this case would assert its wording against the wrong refusal.
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})
	old := modelDeploymentWithEveryField()
	md := old.DeepCopy()
	md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang

	_, err := r.ValidateUpdate(context.Background(), old, md)
	require.Error(t, err)

	got := err.Error()
	assert.True(t, errsContain(got, "describes a different deployment"), got)
	for _, wrongReason := range []string{"conflict", "concurrent", "lock", "race"} {
		assert.False(t, errsContain(got, wrongReason),
			"the message must not send the reader looking for %q", wrongReason)
	}
}

// TestModelDeploymentWebhook_RefusesMemberNamesThatCannotBeHostnames covers the budget a
// multi-Member role spends that a single-Member one does not.
//
// THE FIRST CASE IS THE ONE THAT MATTERS: its <deployment>-<role> is legal, so the Service-name rule
// accepts it, and only the member name it implies is too long. A test whose refused input was
// already refused by another rule would pass against a build where this rule does not exist.
func TestModelDeploymentWebhook_RefusesMemberNamesThatCannotBeHostnames(t *testing.T) {
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})

	// 40 + 1 + 20 = 61 characters, which is a legal Service name; the member name it implies is 67.
	// The names end in an alphanumeric on purpose: a trailing hyphen is refused by the Service-name
	// rule for a reason that has nothing to do with length, and an input refused twice would let
	// this case pass against a build where the rule under test is missing.
	longName := "deploymentaaaaaaaaaaaaaaaaaaaaaaaaaaaaax"
	longRole := "roleaaaaaaaaaaaaaaay"

	build := func(name, role string, replicas, size int32) *workercore.ModelDeployment {
		md := modelDeploymentWithEveryField()
		md.Name = name
		md.Spec.Roles = md.Spec.Roles[:1]
		md.Spec.Roles[0].Name = role
		md.Spec.Roles[0].Replicas, md.Spec.Roles[0].ReplicaSize = replicas, size

		return md
	}

	t.Run("a_name_legal_at_size_one_is_refused_above_it", func(t *testing.T) {
		// The baseline: the same deployment at size one is ACCEPTED, which is what proves the
		// refusal below is about the member name and not about the role or deployment name.
		_, err := r.ValidateCreate(context.Background(), build(longName, longRole, 1, 1))
		require.NoError(t, err, "at one member per replica the render names nothing, so there is no budget")

		_, err = r.ValidateCreate(context.Background(), build(longName, longRole, 1, 2))
		require.Error(t, err)

		got := err.Error()
		assert.True(t, errsContain(got, "spec.roles[0]: Invalid value"),
			"the error belongs to the role, since four inputs spell the name it measured: %s", got)
		assert.True(t, errsContain(got, "cannot be a hostname"), got)
		assert.True(t, errsContain(got, longName+"-"+longRole+"-r0-m1"),
			"the message has to quote the name it measured, or nobody can tell what to shorten: %s", got)
	})

	t.Run("a_scale_that_lengthens_the_name_is_refused", func(t *testing.T) {
		// 57 characters of prefix, which is exact: -r9-m1 reaches 63 and fits, -r10-m1 reaches 64
		// and does not. This is the case the Service-name rule structurally cannot catch, since it
		// skips roles that already exist and the pair it measures never changes.
		// Ten replicas reach ordinal 9, which is one digit; eleven reach ordinal 10, which is two.
		name, role := "deploymentaaaaaaaaaaaaaaaaaaaaaaaaax", "roleaaaaaaaaaaaaaaay"
		old := build(name, role, 10, 2)
		_, err := r.ValidateCreate(context.Background(), old)
		require.NoError(t, err, "ten replicas reach ordinal 9, and -r9-m1 is exactly 63")

		_, err = r.ValidateUpdate(context.Background(), old, build(name, role, 11, 2))
		require.Error(t, err, "the eleventh reaches ordinal 10, adding a digit to the longest name")

		got := err.Error()
		assert.True(t, errsContain(got, "declare fewer replicas"), got)
		// THE FIELD THE REFUSAL NAMES IS THE ONE THE USER CAN ACT ON. Nothing about `size` moved on
		// this edit and nothing could -- it is immutable -- so an error attached to it would send an
		// operator to change the one input this deployment has already frozen.
		assert.False(t, errsContain(got, "spec.roles[0].size"),
			"a replicas-only scale must not be reported against size, which did not change: %s", got)
		assert.True(t, errsContain(got, "spec.roles[0]: Invalid value"), got)
	})
}

// TestModelDeploymentWebhook_RefusesTwoServicesNamedTheSame covers the collision distinct role names
// do not prevent, because the two names come out of two different formulas: a role is fronted by
// <deployment>-<role>, and a role of several members publishes each instance behind
// <deployment>-<role>-r<ordinal>.
//
// THE TWO BASELINES ARE WHAT MAKE THE REFUSAL MEAN ANYTHING. The same pair of role names at one
// member per instance is ACCEPTED -- no instance Service is rendered there, so no name is claimed
// twice -- which is what proves the rule is about the derived Service and not about role names that
// look alike. And a sibling named something else is accepted at either size, which is what proves it
// is not simply refusing multi-member roles.
func TestModelDeploymentWebhook_RefusesTwoServicesNamedTheSame(t *testing.T) {
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})

	// Role "x" runs two instances, so it derives <deployment>-x-r0 and <deployment>-x-r1. A sibling
	// named "x-r0" derives <deployment>-x-r0 for itself.
	build := func(sibling string, size int32) *workercore.ModelDeployment {
		md := modelDeploymentWithEveryField()
		md.Spec.Roles = md.Spec.Roles[:1]
		md.Spec.Roles[0].Name = "x"
		md.Spec.Roles[0].Replicas, md.Spec.Roles[0].ReplicaSize = 2, size

		second := *md.Spec.Roles[0].DeepCopy()
		second.Name, second.Replicas, second.ReplicaSize = sibling, 1, 1

		md.Spec.Roles = append(md.Spec.Roles, second)

		return md
	}

	t.Run("a_sibling_named_like_a_derived_instance_service_is_refused", func(t *testing.T) {
		_, err := r.ValidateCreate(context.Background(), build("x-r0", 1))
		require.NoError(t, err,
			"at one member per instance no headless Service is rendered, so the name is claimed once")

		_, err = r.ValidateCreate(context.Background(), build("x-r0", 2))
		require.Error(t, err)

		got := err.Error()
		assert.True(t, errsContain(got, "qwen-72b-x-r0"),
			"the message quotes the name both would carry, or nobody can tell what collides: %s", got)
		assert.True(t, errsContain(got, "spec.roles[1].name"),
			"the refusal belongs to the role that can be renamed, which is the one declared second: %s", got)
	})

	t.Run("an_unrelated_sibling_is_accepted_at_either_size", func(t *testing.T) {
		for _, size := range []int32{1, 2} {
			_, err := r.ValidateCreate(context.Background(), build("y", size))
			require.NoErrorf(t, err, "a sibling named y collides with nothing at size %d", size)
		}
	})
}

// TestModelDeploymentWebhook_ValidateUpdateFreezesSizeButNotReplicas holds the two halves of the
// scaling story against each other, on one object, in one test.
//
// THE PAIR IS THE POINT, NOT EITHER HALF. A rule refusing a size change would also be satisfied by
// a rule refusing every numeric change, and that implementation takes away the only elasticity this
// role has. Asserting the refusal beside the acceptance is what distinguishes "size is frozen" from
// "numbers are frozen", and the two subtests start from the same stored object so nothing but the
// field under test differs.
//
// THE REFUSAL MUST NAME size AND POINT AT replicas. An operator raising size almost always wants
// capacity, which replicas gives without disturbing anything already serving; a refusal that only
// says no leaves them with a deployment they believe cannot grow.
func TestModelDeploymentWebhook_ValidateUpdateFreezesSizeButNotReplicas(t *testing.T) {
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})

	stored := func() *workercore.ModelDeployment {
		md := modelDeploymentWithEveryField()
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].Replicas, md.Spec.Roles[i].ReplicaSize = 1, 2
		}

		return md
	}

	t.Run("size_change_is_refused", func(t *testing.T) {
		old := stored()
		md := old.DeepCopy()
		md.Spec.Roles[0].ReplicaSize = 3

		_, err := r.ValidateUpdate(context.Background(), old, md)
		require.Error(t, err)

		got := err.Error()
		assert.True(t, errsContain(got, "spec.roles[0].size"), got)
		assert.True(t, errsContain(got, "fixed when the deployment is created"), got)
		assert.True(t, errsContain(got, "replicas"),
			"the refusal has to name the field that does move: %s", got)
		// The other frozen fields answer "this is a different deployment". Size is not that: the
		// deployment is the same one, and what cannot happen is this edit to it.
		assert.False(t, errsContain(got, "describes a different deployment"), got)
	})

	t.Run("replicas_change_is_accepted", func(t *testing.T) {
		old := stored()
		md := old.DeepCopy()
		md.Spec.Roles[0].Replicas = 4

		_, err := r.ValidateUpdate(context.Background(), old, md)
		assert.NoError(t, err, "scaling a role is the one edit this whole shape exists to allow")
	})
}

// TestModelDeploymentWebhook_ValidateUpdateAllowsMetadataAndTheDeletionWindow covers the two edits
// that must keep working, including the one that releases the object.
func TestModelDeploymentWebhook_ValidateUpdateAllowsMetadataAndTheDeletionWindow(t *testing.T) {
	// The deletion subtest deliberately keeps NO live InstanceType: an absent type is exactly the
	// state that would strand the object if the rules reading it ran in that window, so the fixture
	// has to be able to express it. The metadata subtest gets its own handler with the type present.
	r := newModelDeploymentWebhookWith(nil)

	t.Run("metadata_only_edit", func(t *testing.T) {
		r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})

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

// TestValidateRoleResourcesAgainstInstanceType covers the two rules that need the InstanceType.
//
// EACH CASE ASSERTS THE SET OF FIELD PATHS REFUSED, not that something was refused. The two rules
// answer about different fields, so an implementation running only one of them still refuses the
// case where both are violated — and a test asserting "an error came back" would call that a pass.
//
// THE INDEPENDENCE IS WHAT THE LAST THREE CASES ARE FOR. A request can satisfy the mode rule and
// break the ceiling, or the reverse, so each rule gets a case where the OTHER one is violated. A
// table whose acceptance cases all satisfied both rules at once would pass against an implementation
// that had only ever run one.
func TestValidateRoleResourcesAgainstInstanceType(t *testing.T) {
	plain := servingInstanceType("h20-8x", 8)
	sliceable := servingInstanceType("h20-8x", 8, offeringLogicalSlices)
	partitionable := servingInstanceType("h20-8x", 8, offeringPartitionProfiles("1g.10gb", "2g.20gb"))
	// AN ALL-PARTITIONED POOL, WHOSE WHOLE-CARD CEILING IS ZERO. status.accelerator.onceMaxRequest
	// counts FREE UNPARTITIONED cards, so a pool that has carved all of its own reports none there
	// while its partitioned view serves requests all day. It is the only fixture on which the
	// whole-card ceiling and a valid partitioned request disagree, and without it an acceptance case
	// for one partitioned card passes whether or not the ceiling is applied to it.
	allPartitioned := servingInstanceType("h20-mig", 0, offeringPartitionProfiles("1g.10gb"))

	cards := func(n int64) *resource.Quantity { return resource.NewQuantity(n, resource.DecimalSI) }
	const (
		accelPath   = "spec.roles[0].resources.accelerator"
		slicePath   = "spec.roles[0].resources.acceleratorSlicedMemoryPercentage"
		profilePath = "spec.roles[0].resources.acceleratorPartitionedProfile"
	)

	cases := []struct {
		name     string
		instType *worker.InstanceType
		ress     *workercore.ModelDeploymentRoleResources
		// refuse is every field path the refusals must name, and nothing else.
		refuse []string
		// says is a phrase the refusal must carry, for the pairs of rules that answer on ONE field
		// path and are told apart only by what they say. A missing capability and a mistyped profile
		// both land on acceleratorPartitionedProfile, and a case asserting the path alone passes
		// whichever of the two the code happened to run.
		says string
		// aboutTheRequest lists the paths whose refusal is DELIBERATELY type-independent, and it is
		// an exemption from the type-naming assertion below rather than a relaxation of it.
		//
		// That assertion's reason is "the same request is correct against another type", and for the
		// single-card rule the premise is false: two cards at fifty percent each is wrong against
		// EVERY type, because a slice is a fraction of one card. Naming a type in such a message
		// sends the operator to inspect a pool that has nothing to do with the problem.
		aboutTheRequest []string
	}{
		{
			name: "whole_card_at_the_ceiling", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{Accelerator: cards(8)},
		},
		{
			name: "whole_card_over_the_ceiling", instType: plain,
			ress:   &workercore.ModelDeploymentRoleResources{Accelerator: cards(9)},
			refuse: []string{accelPath},
		},
		{
			name: "sliced_on_a_type_that_offers_slicing", instType: sliceable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorSlicedMemoryPercentage: 50,
			},
		},
		{
			name: "sliced_on_a_type_that_offers_none", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorSlicedMemoryPercentage: 50,
			},
			refuse: []string{slicePath},
		},
		{
			// The compute percentage alone is a slice request too; a rule reading only the memory
			// one would let this through.
			name: "sliced_by_cores_only_on_a_type_that_offers_none", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorSlicedCoresPercentage: 50,
			},
			refuse: []string{slicePath},
		},
		{
			name: "partitioned_on_a_type_that_offers_none", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorPartitionedProfile: "1g.10gb",
			},
			refuse: []string{profilePath},
			says:   "does not offer hardware partitioning",
		},
		{
			name: "partition_profile_in_the_inventory", instType: partitionable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorPartitionedProfile: "2g.20gb",
			},
		},
		{
			name: "partition_profile_outside_the_inventory", instType: partitionable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorPartitionedProfile: "7g.80gb",
			},
			refuse: []string{profilePath},
			says:   "does not offer this partition profile",
		},

		// The three that hold the two rules apart.
		{
			// A SLICED REQUEST IS BOUNDED BY BEING ONE CARD, NOT BY THE WHOLE-CARD CEILING. The
			// whole-card view counts free unpartitioned cards and reads zero on an all-partitioned
			// pool, so applying it here refused correct one-card requests while describing the type
			// rather than the request. What is wrong with nine cards at fifty percent is that a
			// slice is a fraction of ONE card, and that holds against every pool.
			name: "sliced_over_one_card", instType: sliceable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(9), AcceleratorSlicedMemoryPercentage: 50,
			},
			refuse:          []string{accelPath},
			says:            "must be exactly 1 for a sliced request",
			aboutTheRequest: []string{accelPath},
		},
		{
			// THE CASE THE OLD RULE REFUSED AND SHOULD NOT HAVE, and it needs the zero-ceiling pool to
			// say anything: on a pool with free whole cards the request clears the old ceiling too, so
			// an acceptance case there passes either way. One card against a profile this pool offers
			// is the ordinary partitioned request, and the count is the one this webhook's OWN
			// defaulting writes when the operator names none -- so the refusal it used to produce was
			// one nobody could avoid and nobody could act on.
			name: "partitioned_one_card_on_an_all_partitioned_pool", instType: allPartitioned,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorPartitionedProfile: "1g.10gb",
			},
		},
		{
			// The same shape for a slice, so neither mode's acceptance rests on the other's.
			name: "sliced_one_card_is_accepted", instType: sliceable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(1), AcceleratorSlicedMemoryPercentage: 50,
			},
		},
		{
			// The same for a partition, which is one instance on one card.
			name: "partitioned_over_one_card", instType: partitionable,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(2), AcceleratorPartitionedProfile: "2g.20gb",
			},
			refuse:          []string{accelPath},
			says:            "must be exactly 1 for a partitioned request",
			aboutTheRequest: []string{accelPath},
		},
		{
			// TWO INDEPENDENT FAULTS AND BOTH ARE REPORTED: the pool offers no slicing, and four
			// cards at fifty percent each describes nothing a scheduler can place. An operator who
			// saw only the first would fix the type and be refused again.
			name: "unoffered_mode_and_over_one_card", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(4), AcceleratorSlicedMemoryPercentage: 50,
			},
			refuse:          []string{slicePath, accelPath},
			aboutTheRequest: []string{accelPath},
		},
		{
			name: "unoffered_mode_far_over_one_card", instType: plain,
			ress: &workercore.ModelDeploymentRoleResources{
				Accelerator: cards(9), AcceleratorSlicedMemoryPercentage: 50,
			},
			refuse:          []string{slicePath, accelPath},
			aboutTheRequest: []string{accelPath},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateRoleResourcesAgainstInstanceType(
				tc.instType, tc.ress, field.NewPath("spec", "roles").Index(0).Child("resources"))

			got := make([]string, 0, len(errs))
			joined := ""
			for _, e := range errs {
				got = append(got, e.Field)
				joined += e.Error() + "\n"
				if slices.Contains(tc.aboutTheRequest, e.Field) {
					assert.NotContains(t, e.Error(), tc.instType.Name,
						"this refusal is about the request and holds against every type; naming one "+
							"sends the operator to inspect a pool that is not the problem")

					continue
				}
				assert.Contains(t, e.Error(), tc.instType.Name,
					"the field alone does not locate the problem: the same request is correct against another type")
			}
			assert.ElementsMatch(t, tc.refuse, got)
			if tc.says != "" {
				assert.Contains(t, joined, tc.says,
					"two rules answer on this path and only the wording tells the reader which one fired")
			}
		})
	}
}

// TestValidateRoleResourcesAgainstInstanceType_TheCeilingIsInTheMessage pins the number, not only
// the refusal. "Exceeds the maximum" says the request was wrong; the ceiling says what would be
// right, and without it the next attempt is a guess.
func TestValidateRoleResourcesAgainstInstanceType_TheCeilingIsInTheMessage(t *testing.T) {
	errs := validateRoleResourcesAgainstInstanceType(
		servingInstanceType("h20-8x", 8),
		&workercore.ModelDeploymentRoleResources{Accelerator: resource.NewQuantity(9, resource.DecimalSI)},
		field.NewPath("spec", "roles").Index(0).Child("resources"))

	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "8", "the refusal carries the ceiling the request has to fit")
}

// TestModelDeploymentWebhook_ValidateRefusesAModeTheTypeDoesNotOffer drives the rule through the
// handler, so the read of the type and the wiring into the response are covered and not only the
// pure function.
func TestModelDeploymentWebhook_ValidateRefusesAModeTheTypeDoesNotOffer(t *testing.T) {
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})

	md := modelDeploymentWithEveryField()
	md.Spec.Roles[0].Resources.AcceleratorSlicedMemoryPercentage = 50

	_, err := r.ValidateCreate(context.Background(), md)
	require.Error(t, err)
	assert.True(t, errsContain(err.Error(), "does not offer logical slicing"), err.Error())
	assert.True(t, errsContain(err.Error(), "h20-8x"), err.Error())
}

// TestModelDeploymentWebhook_ValidateTreatsAnUncomputedDetailAsTransient is the case that fails an
// implementation reading an empty detail as "this type offers no modes".
//
// The distinction is the whole point: a permanent field error tells the user their mode is wrong,
// and the fix is to wait. The error type is therefore the assertion — a test checking only that the
// request was refused passes against the bug this case exists for.
func TestModelDeploymentWebhook_ValidateTreatsAnUncomputedDetailAsTransient(t *testing.T) {
	r := newModelDeploymentWebhookWith([]ctrlcli.Object{notSyncedInstanceType("h20-8x")})

	md := modelDeploymentWithEveryField()
	md.Spec.Roles[0].Resources.AcceleratorSlicedMemoryPercentage = 50

	_, err := r.ValidateCreate(context.Background(), md)
	require.Error(t, err)
	assert.True(t, kerrors.IsInternalError(err),
		"an empty detail is not-yet-synced, and the user's fix is to wait: %v", err)
	assert.False(t, kerrors.IsInvalid(err), "a permanent refusal here names a mode the type does offer")
}

// TestModelDeploymentWebhook_ValidateSkipsTheTypeRulesDuringDeletion pins the window that would
// otherwise deadlock: the rules reading the type refuse when the type is absent, so an object whose
// type went first could never clear its own finalizer.
func TestModelDeploymentWebhook_ValidateSkipsTheTypeRulesDuringDeletion(t *testing.T) {
	r := newModelDeploymentWebhookWith(nil)

	old := modelDeploymentWithEveryField()
	old.DeletionTimestamp = ptr.To(meta.Now())
	old.Finalizers = []string{"worker.gpustack.ai/model-deployment"}
	md := old.DeepCopy()
	md.Finalizers = nil

	_, err := r.ValidateUpdate(context.Background(), old, md)
	assert.NoError(t, err, "the type is gone, and refusing here is the deadlock this skip exists for")

	// The same absent type on an object that is NOT being deleted still refuses, so the skip is
	// bounded by the deletion timestamp rather than by the type being missing.
	live := modelDeploymentWithEveryField()
	_, err = r.ValidateCreate(context.Background(), live)
	assert.Error(t, err, "outside the deletion window an absent type is still refused")
}

// inAcceleratorGroup puts an InstanceType over a named accelerator population. Two types sharing one
// draw from the same cards however differently they are named.
func inAcceleratorGroup(group string) func(*worker.InstanceType) {
	return func(instType *worker.InstanceType) {
		instType.Spec.AcceleratorGroup = group
	}
}

// pdRole builds one role of a prefill/decode pair.
func pdRole(name string, kind workercore.ModelDeploymentRoleKind, instanceType string,
	ress *workercore.ModelDeploymentRoleResources,
) workercore.ModelDeploymentRole {
	return workercore.ModelDeploymentRole{
		Name: name, Kind: kind, Replicas: 1, InstanceType: instanceType, Resources: ress,
	}
}

// TestModelDeploymentWebhook_APairMayNotShareOneAccelerator covers when a shared accelerator is
// refused and when it is not.
//
// THE QUALIFIER IS WHAT THE TABLE IS FOR. A rule with none refuses every sliced pair and blocks the
// heterogeneous shape two instanceTypes exist to enable; a rule keyed on the type NAMES being
// different accepts two names over one accelerator group, which is precisely the case the rule was
// written to close. Only a table carrying both wrong predicates' counterexamples tells the three
// implementations apart.
func TestModelDeploymentWebhook_APairMayNotShareOneAccelerator(t *testing.T) {
	const (
		sharedGroup = "nvidia-h20"
		otherGroup  = "nvidia-a100"
	)

	sliced := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator:                       resource.NewQuantity(1, resource.DecimalSI),
			AcceleratorSlicedMemoryPercentage: 50,
		}
	}
	wholeCard := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator: resource.NewQuantity(1, resource.DecimalSI),
		}
	}
	partitioned := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator:                   resource.NewQuantity(1, resource.DecimalSI),
			AcceleratorPartitionedProfile: "1g.10gb",
		}
	}

	// Three types: two over ONE accelerator group under different names, and one over another.
	// The sibling is what makes "the names differ" an insufficient answer; the disjoint one is what
	// makes "refuse every sliced pair" an over-refusal.
	live := []ctrlcli.Object{
		servingInstanceType("h20-8x", 8, offeringLogicalSlices, inAcceleratorGroup(sharedGroup),
			offeringPartitionProfiles("1g.10gb")),
		servingInstanceType("h20-8x-mig", 8, offeringLogicalSlices, inAcceleratorGroup(sharedGroup),
			offeringPartitionProfiles("1g.10gb")),
		servingInstanceType("a100-8x", 8, offeringLogicalSlices, inAcceleratorGroup(otherGroup),
			offeringPartitionProfiles("1g.10gb")),
	}

	cases := []struct {
		name   string
		roles  []workercore.ModelDeploymentRole
		refuse bool
	}{
		{
			name: "pd_both_sliced", refuse: true,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x", sliced()),
			},
		},
		{
			// EITHER PERCENTAGE ALONE IS A SLICE REQUEST. The defaulting half copies one into the
			// other, so a compute-only pair holds two fractions of a card exactly as a memory-only
			// pair does -- and a rule reading only the memory percentage lets it through.
			name: "pd_both_sliced_by_cores_only", refuse: true,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x",
					&workercore.ModelDeploymentRoleResources{
						Accelerator:                      resource.NewQuantity(1, resource.DecimalSI),
						AcceleratorSlicedCoresPercentage: 50,
					}),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x",
					&workercore.ModelDeploymentRoleResources{
						Accelerator:                      resource.NewQuantity(1, resource.DecimalSI),
						AcceleratorSlicedCoresPercentage: 50,
					}),
			},
		},
		{
			// TWO NAMES, ONE POPULATION. A predicate comparing the instanceType names accepts this,
			// and the pair then lands as two views of one card -- the exact state the rule exists
			// to refuse.
			name: "pd_sliced_sibling_types", refuse: true,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x-mig", sliced()),
			},
		},
		{
			// The shape the split exists to enable. A blanket refusal of every sliced pair fails
			// here, and this is the only case that catches that.
			name: "pd_sliced_disjoint_types",
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "a100-8x", sliced()),
			},
		},
		{
			name: "pd_whole_cards",
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", wholeCard()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x", wholeCard()),
			},
		},
		{
			// Accepted EVEN ON ONE CARD: hardware partitions are isolated by the device, which is
			// the property this rule is about. A slice is not, and that is the whole difference.
			name: "pd_partitioned",
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", partitioned()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x", partitioned()),
			},
		},
		{
			// The rule is about a pair; a lone role sharing a card with itself is what a slice is for.
			name: "single_role_sliced",
			roles: []workercore.ModelDeploymentRole{
				pdRole("server", workercore.ModelDeploymentRoleKindServer, "h20-8x", sliced()),
			},
		},
		{
			// One half of the pair sliced and the other not: the two cannot contend for one card
			// through a slice, so the rule does not fire.
			name: "pd_only_prefill_sliced",
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x", wholeCard()),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The disjoint case puts the pair on two instance types, which the barrier rule refuses
			// unless it can be installed. That rule has its own test; here it must not be what
			// answers, or this table would be asserting it by accident.
			withDerivedFromNode(t, true)

			r := newModelDeploymentWebhookWith(live)

			md := modelDeployment(workercore.ModelDeploymentEngineVLLM, tc.roles...)
			md.Spec.KVCache.Connector = "mooncake"

			_, err := r.ValidateCreate(context.Background(), md)
			if !tc.refuse {
				assert.NoError(t, err)

				return
			}

			require.Error(t, err)
			for _, role := range tc.roles {
				assert.True(t, errsContain(err.Error(), role.Name),
					"the refusal names both roles, and %q is missing: %v", role.Name, err)
			}
			assert.True(t, errsContain(err.Error(), "logical slice"),
				"and names the field that makes them shareable: %v", err)
		})
	}
}

// TestModelDeploymentWebhook_ThePairRuleReadsEveryPair covers a deployment declaring more than one
// role of one kind.
//
// TWO PREFILL ROLES ARE NOW REFUSED BY THE KIND RULE, so this fixture trips two rules rather than
// one, and the pair rule is the one under test here. It is kept because the pair rule iterates every
// prefill against every decode and that loop is the defense that survives if the kind rule is ever
// relaxed -- a rule proven only on shapes another rule already excludes is a rule nobody has tested.
//
// THE ASSERTIONS THEREFORE NAME WHICH RULE REFUSED, rather than searching the whole aggregate. Both
// rules mention "prefill-shared", so an aggregate-wide substring check passes on the kind rule's
// message alone and would keep passing if the pair rule were deleted.
//
// THE FIXTURE IS BUILT SO THE LAST PAIR IS THE INNOCENT ONE. A rule keeping one role per kind ends up
// holding the prefiller declared last, which here sits on a disjoint accelerator group, and it then
// admits the violating pair declared before it -- two slices of one card handed to the two roles the
// split exists to separate. Ordering the roles the other way round would let that rule pass.
func TestModelDeploymentWebhook_ThePairRuleReadsEveryPair(t *testing.T) {
	const (
		sharedGroup = "nvidia-h20"
		otherGroup  = "nvidia-a100"
	)

	sliced := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator:                       resource.NewQuantity(1, resource.DecimalSI),
			AcceleratorSlicedMemoryPercentage: 50,
		}
	}

	live := []ctrlcli.Object{
		servingInstanceType("h20-8x", 8, offeringLogicalSlices, inAcceleratorGroup(sharedGroup)),
		servingInstanceType("a100-8x", 8, offeringLogicalSlices, inAcceleratorGroup(otherGroup)),
	}

	withDerivedFromNode(t, true)

	r := newModelDeploymentWebhookWith(live)

	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		pdRole("prefill-shared", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
		pdRole("prefill-disjoint", workercore.ModelDeploymentRoleKindPrefill, "a100-8x", sliced()),
		pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "h20-8x", sliced()),
	)
	md.Spec.KVCache.Connector = "mooncake"

	_, err := r.ValidateCreate(context.Background(), md)
	require.Error(t, err, "the prefiller declared first shares one accelerator group with the decoder")

	// pairRefusal is the one refusal carrying the pair rule's own wording. Requiring it to exist is
	// what makes the assertions below about THIS rule: without it they would read the kind rule's
	// message, which names two of the same roles.
	var pairRefusal string
	for _, line := range strings.Split(err.Error(), "\n") {
		if errsContain(line, "both request a logical slice") {
			pairRefusal = line

			break
		}
	}
	require.NotEmpty(t, pairRefusal, "the pair rule refused as well as the kind rule: %v", err)

	assert.True(t, errsContain(pairRefusal, "prefill-shared"),
		"the refusal names the prefiller that can land on the decoder's card: %s", pairRefusal)
	assert.True(t, errsContain(pairRefusal, "decode"),
		"and the decoder it would share it with: %s", pairRefusal)
	assert.False(t, errsContain(pairRefusal, "prefill-disjoint"),
		"and not the prefiller on a disjoint group, which is a shape this rule exists to allow: %s",
		pairRefusal)
}

// TestModelDeploymentWebhook_TheBarrierRuleCannotStrandAnObject covers the rule's own escape hatch.
//
// THIS RULE READS CLUSTER STATE, SO IT CAN START REFUSING AN OBJECT IT ONCE ACCEPTED. A deployment
// spanning two instance types is admitted while the derived-from-node setting is on, and refused the
// moment it goes off. Validation still runs while an object is being deleted, so the update that
// clears the finalizer is refused too -- and instanceType is frozen by the identity rule, so the
// operator cannot edit their way out either. The object then has no reachable state from which it
// can be removed, which is a worse outcome than the shape this rule exists to prevent.
//
// BOTH SIDES ARE REQUIRED. A rule that never refused anything would pass the deletion case, and the
// refusal is what the whole rule is for.
func TestModelDeploymentWebhook_TheBarrierRuleCannotStrandAnObject(t *testing.T) {
	withDerivedFromNode(t, false)

	spanning := func() *workercore.ModelDeployment {
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name, r.InstanceType = "decode", "a100-8x"
			}),
		)

		return md
	}

	live := spanning()
	assert.NotEmpty(t, validateModelDeploymentBarrierIsInstallable(context.Background(), live),
		"with the setting off nothing installs the barrier, and this shape needs it")

	deleting := spanning()
	deleting.DeletionTimestamp = ptr.To(meta.Now())
	assert.Empty(t, validateModelDeploymentBarrierIsInstallable(context.Background(), deleting),
		"a deployment being deleted is not asking to be admitted, and refusing it strands the object "+
			"forever: the finalizer-clearing update is refused too, and instanceType cannot be edited")
}

// TestModelDeploymentWebhook_AMissingTypeDoesNotHideTheRest covers how a named-but-absent
// InstanceType comes back.
//
// IT IS A FACT ABOUT THIS OBJECT'S FIELD, so it belongs with the other field errors rather than
// ending the pass. Returned bare it did two things: the response was a plain denial instead of an
// Invalid naming the path, and it SHORT-CIRCUITED -- one mistyped instanceType hid every other
// validation error on the object, so the operator fixed one thing per apply.
func TestModelDeploymentWebhook_AMissingTypeDoesNotHideTheRest(t *testing.T) {
	withDerivedFromNode(t, true)

	r := newModelDeploymentWebhookWith(nil)

	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(role *workercore.ModelDeploymentRole) {
			role.Name, role.InstanceType = "server", "no-such-type"
			role.Resources = &workercore.ModelDeploymentRoleResources{
				Accelerator: resource.NewQuantity(1, resource.DecimalSI),
			}
		}),
	)
	// A SECOND, UNRELATED FAULT that is answerable from the object alone. Without it this case cannot
	// tell "the missing type was reported" from "the missing type was the only thing reported".
	md.Spec.Engine.Name = ""

	_, err := r.ValidateCreate(context.Background(), md)
	require.Error(t, err)

	assert.True(t, kerrors.IsInvalid(err),
		"a field that names a missing object is Invalid, not a bare denial: %v", err)
	assert.True(t, errsContain(err.Error(), "instanceType"),
		"the refusal names the field that names the missing type: %v", err)
	assert.True(t, errsContain(err.Error(), "engine"),
		"and it does not short-circuit: the object's other faults are reported in the same pass: %v", err)
}

// TestModelDeploymentWebhook_AnUnsetAcceleratorGroupIsUnknown covers what two blanks mean.
//
// AN UNSET GROUP IS UNKNOWN, NOT A GROUP TWO TYPES SHARE. The InstanceType webhook requires the
// field on an acceleratable type, so only objects that passed that rule carry it; two empty strings
// comparing equal refuses a pair on the strength of what NEITHER type says. That is the wrong way for
// this rule to be wrong -- it blocks the heterogeneous shape the split exists to enable, and the
// operator has nothing to edit, because instanceType is frozen.
func TestModelDeploymentWebhook_AnUnsetAcceleratorGroupIsUnknown(t *testing.T) {
	sliced := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator:                       resource.NewQuantity(1, resource.DecimalSI),
			AcceleratorSlicedMemoryPercentage: 50,
		}
	}

	// Neither type names a group, which is what an object stored before that rule existed looks like.
	live := []ctrlcli.Object{
		servingInstanceType("h20-8x", 8, offeringLogicalSlices),
		servingInstanceType("a100-8x", 8, offeringLogicalSlices),
	}

	withDerivedFromNode(t, true)

	r := newModelDeploymentWebhookWith(live)

	md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", sliced()),
		pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "a100-8x", sliced()),
	)
	md.Spec.KVCache.Connector = "mooncake"

	_, err := r.ValidateCreate(context.Background(), md)
	assert.NoError(t, err,
		"two types that say nothing about their accelerator population do not thereby say they share one")
}

// withDerivedFromNode makes the derived-from-node setting read the given value for one case.
//
// IT WRITES THE SECRET INTO THE LOOPBACK CLIENT RATHER THAN REPLACING THE CLIENT. That holder is a
// varx.Once: TestMain configures it and every later Configure is silently a no-op, so a helper that
// tried to swap in a seeded fake would leave the setting reading exactly what it read before -- and
// the case asserting the "on" behavior would fail for a reason that has nothing to do with the rule.
// The key-scoped merge/remove mechanics live in settingtest.MergeDelegatedSettings.
func withDerivedFromNode(t *testing.T, on bool) {
	t.Helper()
	settingtest.MergeDelegatedSettings(t, map[string]string{"instance-type-derived-from-node": strconv.FormatBool(on)})
}

// TestModelDeploymentWebhook_SeveralInstanceTypesNeedTheBarrier covers the barrier refusal in both
// directions.
//
// THE POSITIVE CASE IS WHAT MAKES THE REFUSAL MEAN ANYTHING. Without it, a rule refusing every
// multi-instanceType deployment passes the negative case exactly as the correct rule does -- and the
// shape it would be refusing is the one this whole design exists to enable.
func TestModelDeploymentWebhook_SeveralInstanceTypesNeedTheBarrier(t *testing.T) {
	twoTypes := func() *workercore.ModelDeployment {
		return modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name, r.InstanceType = "decode", "a100-8x"
			}),
		)
	}

	t.Run("setting_off_refuses", func(t *testing.T) {
		withDerivedFromNode(t, false)

		errs := validateModelDeploymentBarrierIsInstallable(context.Background(), twoTypes())
		require.Len(t, errs, 1)
		assert.Equal(t, "spec.roles", errs[0].Field)
		assert.Contains(t, errs[0].Detail, "instance-type-derived-from-node",
			"the message names the setting, which is the one thing the operator can act on")
	})

	t.Run("setting_on_accepts", func(t *testing.T) {
		withDerivedFromNode(t, true)

		assert.Empty(t, validateModelDeploymentBarrierIsInstallable(context.Background(), twoTypes()),
			"this is the shape roles on two instance types exist to make possible")
	})

	t.Run("one_type_is_unaffected_with_the_setting_off", func(t *testing.T) {
		withDerivedFromNode(t, false)

		one := modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) { r.Name = "prefill" }),
			role(func(r *workercore.ModelDeploymentRole) { r.Name = "decode" }),
		)
		assert.Empty(t, validateModelDeploymentBarrierIsInstallable(context.Background(), one),
			"one group is admitted as a unit by Kueue without help from this barrier")
	})
}

// TestValidateModelDeploymentRouter_EngineMatched pins the pairing table, both directions.
//
// THE REFUSAL AND THE ACCEPTANCE ARE ONE CASE because either alone is consistent with a rule that
// answers the wrong question: a refusal on its own passes for a handler that refuses the router
// value outright, and an acceptance on its own passes for one with no rule at all.
func TestValidateModelDeploymentRouter_EngineMatched(t *testing.T) {
	cases := []struct {
		name        string
		engine      string
		router      string
		wantMessage string
	}{
		{
			name:   "the picker takes vllm",
			engine: workercore.ModelDeploymentEngineVLLM,
			router: workercore.ModelDeploymentRouterLLMD,
		},
		{
			// Upstream carries a handshake connector and a metrics configuration for each engine,
			// which is what makes this one value admitted with both.
			name:   "the picker takes sglang too",
			engine: workercore.ModelDeploymentEngineSGLang,
			router: workercore.ModelDeploymentRouterLLMD,
		},
		{
			name:   "the vllm router takes its own engine",
			engine: workercore.ModelDeploymentEngineVLLM,
			router: workercore.ModelDeploymentRouterVLLM,
		},
		{
			name:        "the vllm router does not front another project's engine",
			engine:      workercore.ModelDeploymentEngineSGLang,
			router:      workercore.ModelDeploymentRouterVLLM,
			wantMessage: `router "vllm-router" does not front engine "sglang"`,
		},
		{
			name:   "the gateway takes its own engine",
			engine: workercore.ModelDeploymentEngineSGLang,
			router: workercore.ModelDeploymentRouterSGLang,
		},
		{
			// The mirror of the row above it, which is what says the table is a match rather than a
			// list of routers that happen to be refused with one engine.
			name:        "the gateway does not front another project's engine",
			engine:      workercore.ModelDeploymentEngineVLLM,
			router:      workercore.ModelDeploymentRouterSGLang,
			wantMessage: `router "sglang-gateway" does not front engine "vllm"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := modelDeployment(c.engine)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: c.router}

			errs := validateModelDeploymentRouter(md)
			if c.wantMessage == "" {
				for _, err := range errs {
					assert.NotContains(t, err.Error(), "does not front engine",
						"this pair is admitted, so the pairing rule must not be what answers")
				}

				return
			}
			require.NotEmpty(t, errs)
			assert.True(t, errsContain(errs.ToAggregate().Error(), c.wantMessage),
				"the refusal names both sides; got %q", errs.ToAggregate().Error())
		})
	}
}

// TestValidateModelDeploymentRouter_OwnedArgumentsArePerRouter is what keeps the catalog from being
// read as one list shared by every value.
//
// The two routers derive different flags, so the same argument is refused in front of one and
// accepted in front of the other. A case that only asserted the refusal would pass for a handler
// that refused the flag for every router, which would reject a name the other router has no
// opinion about.
func TestValidateModelDeploymentRouter_OwnedArgumentsArePerRouter(t *testing.T) {
	cases := []struct {
		name    string
		router  string
		arg     string
		refused bool
		engine  string
	}{
		{
			name: "a bind address the vllm router derives", router: workercore.ModelDeploymentRouterVLLM,
			arg: "--host=127.0.0.1", refused: true, engine: workercore.ModelDeploymentEngineVLLM,
		},
		{
			name: "the connector the vllm router derives", router: workercore.ModelDeploymentRouterVLLM,
			arg: "--kv-connector=nixl", refused: true, engine: workercore.ModelDeploymentEngineVLLM,
		},
		{
			name:   "the same bind address in front of the picker, which has no such flag",
			router: workercore.ModelDeploymentRouterLLMD, arg: "--host=127.0.0.1", refused: false,
			engine: workercore.ModelDeploymentEngineVLLM,
		},
		{
			name: "a flag neither derives", router: workercore.ModelDeploymentRouterVLLM,
			arg: "--log-level=debug", refused: false, engine: workercore.ModelDeploymentEngineVLLM,
		},
		{
			// The disaggregation switch is spelled per project, so each catalog owns its own
			// spelling and neither owns the other's. A shared list would refuse a flag the router
			// in front has no concept of, and let through the one it does.
			name: "the gateway's own disaggregation switch", router: workercore.ModelDeploymentRouterSGLang,
			arg: "--pd-disaggregation", refused: true, engine: workercore.ModelDeploymentEngineSGLang,
		},
		{
			name:   "the vllm router's spelling of it, in front of the gateway",
			router: workercore.ModelDeploymentRouterSGLang, arg: "--vllm-pd-disaggregation",
			refused: false, engine: workercore.ModelDeploymentEngineSGLang,
		},
		{
			// The gateway has no transfer-connector flag at all, so this names nothing it reads.
			name:   "the connector flag the gateway does not have",
			router: workercore.ModelDeploymentRouterSGLang, arg: "--kv-connector=mooncake",
			refused: false, engine: workercore.ModelDeploymentEngineSGLang,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := modelDeployment(c.engine)
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name: c.router, ExtraArgs: []string{c.arg},
			}

			errs := validateModelDeploymentRouter(md)
			owned := false
			for _, err := range errs {
				if errsContain(err.Error(), "is set by the operator for router") {
					owned = true
				}
			}
			assert.Equal(t, c.refused, owned,
				"the catalog is keyed by router; got %v", errs)
		})
	}
}

// TestValidateModelDeploymentServiceNamesAreDistinct_RouterClaimsItsOwnName covers the collision the
// set did not enumerate: every router value renders its objects under `<deployment>-router`, which
// is exactly the name a role called "router" derives for itself.
func TestValidateModelDeploymentServiceNamesAreDistinct_RouterClaimsItsOwnName(t *testing.T) {
	withRouter := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "router" }))
	withRouter.Spec.Router = &workercore.ModelDeploymentRouter{
		Name: workercore.ModelDeploymentRouterLLMD,
	}

	errs := validateModelDeploymentServiceNamesAreDistinct(withRouter)
	require.NotEmpty(t, errs, "the role and the router derive one name")
	assert.True(t, errsContain(errs.ToAggregate().Error(), "the Service fronting the managed router"),
		"the refusal names what it collides with; got %q", errs.ToAggregate().Error())

	// The same role without a router is legal, which is what says the claim follows the router
	// rather than the role's name being reserved outright.
	withoutRouter := modelDeployment(workercore.ModelDeploymentEngineVLLM,
		role(func(r *workercore.ModelDeploymentRole) { r.Name = "router" }))
	assert.Empty(t, validateModelDeploymentServiceNamesAreDistinct(withoutRouter),
		"nothing claims that name when no router is asked for")
}

// TestValidateModelDeploymentRouter_MetricsTableIsThePickersAlone pins the gate on both halves of
// the reason it exists.
//
// The table feeds one router's metrics extractor; the other two score on their own observations.
// The refusal it produces names the router from the RENDERER PACKAGE'S OWN CONSTANT rather than
// from the object, so a call left unconditional would tell a user who asked for the gateway that
// `llm-d-router` requires a metric — a router they never named. Asserting the message's absence is
// therefore the assertion, not merely that admission passed.
func TestValidateModelDeploymentRouter_MetricsTableIsThePickersAlone(t *testing.T) {
	// An engine the table has no row for, so the gate is the only thing that can decide the answer.
	const unknownEngine = "an-engine-with-no-metrics-row"

	t.Run("the picker is refused, naming itself", func(t *testing.T) {
		md := modelDeployment(unknownEngine)
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name: workercore.ModelDeploymentRouterLLMD,
		}

		errs := validateModelDeploymentRouter(md)
		require.NotEmpty(t, errs)
		assert.True(t, errsContain(errs.ToAggregate().Error(), "required by router"),
			"the picker's precondition is what answers; got %q", errs.ToAggregate().Error())
	})

	for _, name := range []string{
		workercore.ModelDeploymentRouterVLLM,
		workercore.ModelDeploymentRouterSGLang,
	} {
		t.Run("the "+name+" never reaches it", func(t *testing.T) {
			md := modelDeployment(unknownEngine)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: name}

			for _, err := range validateModelDeploymentRouter(md) {
				assert.NotContains(t, err.Error(), "required by router",
					"this router reads no engine metrics, so its user must not be told another "+
						"router's requirement")
				assert.NotContains(t, err.Error(), workercore.ModelDeploymentRouterLLMD,
					"nothing may name a router the object did not")
			}
		})
	}
}

// TestValidateModelDeploymentRouter_ThresholdIsThePickersAlone pins the refusal and, in the same
// case, the acceptance it is a refusal relative to.
//
// The field is meaningful where a scheduler decides per request whether to split one; the other two
// routers have no such decision, so a value there would be legal to write and render nothing. The
// accepted rows are what keep this from passing for a handler that refuses the field outright, and
// the explicit zero is a row of its own because zero is a value upstream defines rather than an
// absent field spelled differently.
func TestValidateModelDeploymentRouter_ThresholdIsThePickersAlone(t *testing.T) {
	cases := []struct {
		name      string
		router    string
		engine    string
		threshold *int32
		refused   bool
	}{
		{
			name: "the picker takes it", router: workercore.ModelDeploymentRouterLLMD,
			engine: workercore.ModelDeploymentEngineVLLM, threshold: ptr.To(int32(8)),
		},
		{
			name:   "and takes an explicit zero, which disables splitting",
			router: workercore.ModelDeploymentRouterLLMD,
			engine: workercore.ModelDeploymentEngineVLLM, threshold: ptr.To(int32(0)),
		},
		{
			name: "the vllm router has no such decision", router: workercore.ModelDeploymentRouterVLLM,
			engine: workercore.ModelDeploymentEngineVLLM, threshold: ptr.To(int32(8)),
			refused: true,
		},
		{
			name: "nor does the gateway", router: workercore.ModelDeploymentRouterSGLang,
			engine: workercore.ModelDeploymentEngineSGLang, threshold: ptr.To(int32(8)),
			refused: true,
		},
		{
			name: "an absent field is refused nowhere", router: workercore.ModelDeploymentRouterVLLM,
			engine: workercore.ModelDeploymentEngineVLLM,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := modelDeployment(c.engine)
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name: c.router, DisaggregationThresholdTokens: c.threshold,
			}

			refused := false
			for _, err := range validateModelDeploymentRouter(md) {
				if errsContain(err.Error(), "has no prompt-token threshold to set") {
					refused = true
				}
			}
			assert.Equal(t, c.refused, refused)
		})
	}
}
