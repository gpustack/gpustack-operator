package worker

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/yaml"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

func routedModelDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
	})
	for _, m := range mutate {
		m(md)
	}

	return md
}

func routerObjectsAsClients(objects ModelDeploymentRouterObjects) []ctrlcli.Object {
	return []ctrlcli.Object{
		objects.Deployment,
		objects.ConfigMap,
		objects.Service,
		objects.ServiceAccount,
		objects.Role,
		objects.RoleBinding,
	}
}

func TestRenderModelDeploymentRouterObjects_OwnedAndDiscoverable(t *testing.T) {
	md := routedModelDeployment()
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	for _, object := range routerObjectsAsClients(objects) {
		assert.True(t, modelDeploymentOwns(object, md), "%T must be owned", object)
		assert.True(t, systemmeta.MatchResource(object, ModelDeploymentResourceType),
			"%T must carry the resource note used by the watch predicate", object)
	}

	assert.IsType(t, &app.Deployment{}, objects.Deployment)
	assert.IsType(t, &core.ConfigMap{}, objects.ConfigMap)
	assert.IsType(t, &core.Service{}, objects.Service)
	assert.IsType(t, &core.ServiceAccount{}, objects.ServiceAccount)
	assert.IsType(t, &rbac.Role{}, objects.Role)
	assert.IsType(t, &rbac.RoleBinding{}, objects.RoleBinding)
	assert.Equal(t, []rbac.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"},
	}}, objects.Role.Rules)
	assert.Equal(t, objects.ServiceAccount.Name, objects.Deployment.Spec.Template.Spec.ServiceAccountName)
	assert.NotContains(t, objects.Service.Spec.Selector, systemmeta.ResourceTypeLabel,
		"the ownership label belongs to the Service, not its backend selector")
	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
		modelDeploymentLabelKeyInstance+"=qwen")
	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
		modelDeploymentLabelKeyRoleKind)
}

// TestRenderModelDeploymentRouterObjects_BothSelectorsSelectOnlyThePodsThatAnswer states the set
// every discovery path names: the members that answer the API, one per replica, at every role size.
//
// THE CASES ARE THE TWO SHAPES A CONJUNCTION CANNOT TELL APART, which is why the table holds both:
// server roles of different sizes share one discovery filter because the pickers filter by kind,
// and a prefill/decode pair mixes a multi-Member role with a single-Member one. Each case renders
// every member of every role and matches BOTH selectors with the machinery a consumer would use --
// the discovery selector is parsed, and the published one is turned into a selector from its own
// entries -- so what is asserted is the set a router would actually reach, not a restatement of the
// renderer's own strings.
//
// THE SINGLE-MEMBER ROWS FAIL OPEN BY DESIGN. An implementation that writes the member index only
// where a role has several members passes every other assertion in this suite: the multi-Member
// leaders still match, and only the single-Member Pods -- which carry no index at all -- fall out
// of the set. Naming member zero of a single-Member role here is what turns that silence into a
// failure. The published selector carries the mirror property: without its leader term it names
// every member of a multi-Member role, and the per-role set below is what goes red.
func TestRenderModelDeploymentRouterObjects_BothSelectorsSelectOnlyThePodsThatAnswer(t *testing.T) {
	type roleSpec struct {
		name string
		kind workercore.ModelDeploymentRoleKind
		size int32
	}

	testCases := []struct {
		name  string
		roles []roleSpec
	}{
		{
			name: "two server roles of different sizes",
			roles: []roleSpec{
				{name: "small", kind: workercore.ModelDeploymentRoleKindServer, size: 1},
				{name: "large", kind: workercore.ModelDeploymentRoleKindServer, size: 4},
			},
		},
		{
			name: "a multi-member prefiller beside a single-member decoder",
			roles: []roleSpec{
				{name: "prefill", kind: workercore.ModelDeploymentRoleKindPrefill, size: 2},
				{name: "decode", kind: workercore.ModelDeploymentRoleKindDecode, size: 1},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Name = tc.roles[0].name
				md.Spec.Roles[0].Kind = tc.roles[0].kind
				md.Spec.Roles[0].ReplicaSize = tc.roles[0].size
				for _, spec := range tc.roles[1:] {
					role := md.Spec.Roles[0]
					role.Name = spec.name
					role.Kind = spec.kind
					role.ReplicaSize = spec.size
					md.Spec.Roles = append(md.Spec.Roles, role)
				}
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
			require.NoError(t, err)

			discovery, err := labels.Parse(objects.ConfigMap.Data[modelDeploymentRouterSelectorKey])
			require.NoError(t, err, "the discovery selector must parse with the machinery a router uses")

			published := make(map[string]labels.Selector, len(objects.Contract.Roles))
			for i := range objects.Contract.Roles {
				published[objects.Contract.Roles[i].Name] = labels.SelectorFromSet(objects.Contract.Roles[i].Selector)
			}

			// One member of ordinal zero per declared size, rendered and stamped the way the
			// converger renders them, so the selectors are matched against what would run rather
			// than against labels a test wrote by hand. The answering member also carries the
			// eligibility label the reconciler writes at runtime — a member the selectors may
			// reach is by definition one it has marked eligible.
			renderMember := func(roleIndex, member int) *core.Pod {
				t.Helper()

				pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
					Deployment: md, Role: &md.Spec.Roles[roleIndex], InstanceType: newRenderInstanceType(),
				})
				require.NoError(t, err)
				stampModelDeploymentPod(pod, md, &md.Spec.Roles[roleIndex], 0, member)
				if member == 0 {
					pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
					// THE CANDIDATE MARKER IS WRITTEN IN THE SAME PATCH AS THE ELIGIBILITY LABEL,
					// so a member the reconciler found eligible carries both.
					pod.Labels[modelDeploymentRouterCandidateLabel] = modelDeploymentEndpointEligibleValue
				}

				return pod
			}

			var wantDiscovered, gotDiscovered []string
			for i := range tc.roles {
				spec := tc.roles[i]
				wantDiscovered = append(wantDiscovered, spec.name+"/member-0")

				var gotPublished []string
				for member := 0; member < int(spec.size); member++ {
					pod := renderMember(i, member)
					id := spec.name + "/member-" + pod.Labels[modelDeploymentMemberIndexLabel]

					if discovery.Matches(labels.Set(pod.Labels)) {
						gotDiscovered = append(gotDiscovered, id)
					}
					if published[spec.name].Matches(labels.Set(pod.Labels)) {
						gotPublished = append(gotPublished, id)
					}
				}

				assert.Equal(t, []string{spec.name + "/member-0"}, gotPublished,
					"the published selector of %s must name exactly the members that answer, "+
						"neither the ones that serve nothing nor fewer than its leader", spec.name)
			}

			assert.ElementsMatch(t, wantDiscovered, gotDiscovered,
				"discovery must reach exactly one member per replica at every size: a set missing "+
					"a single-Member role is the narrowed selector, and a set holding indexes above "+
					"zero is the widened one")
		})
	}
}

func TestRenderModelDeploymentRouterObjects_PodStaysOutsideAdmission(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	pod := objects.Deployment.Spec.Template
	assert.NotContains(t, pod.Labels, kueuectrlconst.QueueLabel)
	for _, container := range pod.Spec.Containers {
		assert.Empty(t, container.Resources.Requests, container.Name)
		assert.Empty(t, container.Resources.Limits, container.Name)
	}
}

// TestRenderModelDeploymentRouterObjects_SelectsEligibleEndpoints pins the eligibility term in
// both surfaces a router is configured from: the published per-role selector and the discovery
// equalities the router's own configuration is derived from. While a member carries no eligibility
// label no selector here may match it, and a restored label puts every one of them back.
func TestRenderModelDeploymentRouterObjects_SelectsEligibleEndpoints(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	require.NotEmpty(t, objects.Contract.Roles)
	// A MANAGED PROTECTED GROUP CARRIES THE TERM FROM THE FIRST ENABLE. A prefill/decode pair is
	// such a group: both halves are managed, so both name the eligible endpoints. A group this
	// operator cannot verify then restores nothing, which is a different matter from a selector
	// that never asked the question at all.
	for _, role := range objects.Contract.Roles {
		require.NotNil(t, role.Selector, "%s", role.Name)
		_, has := role.Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.True(t, has,
			"a managed protected role's published selector names the eligible endpoints (%s)", role.Name)
	}

	config := objects.ConfigMap.Data[modelDeploymentRouterConfigKey]
	assert.Contains(t, config, modelDeploymentLabelKeyEndpointEligible+"="+modelDeploymentEndpointEligibleValue,
		"the discovery equalities name the eligible endpoints with the published selectors")
}

// mixedEligibilityDeployment is the shape whose roles disagree about activatability: an encoder of
// one member, a prefiller of two and a decoder of one, on llm-d with vLLM.
//
// It is the only shape that separates the two questions the gate asks. The encoder has nothing to
// verify and answers NotApplicable, so it activates; the pair beside it took its command lines
// over, so the operator knows no route to ask about it and it can never activate. One deployment
// therefore carries a role the gate decides beside roles it may not.
func mixedEligibilityDeployment() *workercore.ModelDeployment {
	role := func(
		name string, kind workercore.ModelDeploymentRoleKind, replicaSize int32,
	) workercore.ModelDeploymentRole {
		return workercore.ModelDeploymentRole{
			Name: name, Kind: kind, Replicas: 1, ReplicaSize: replicaSize,
			InstanceType: "h20-8x", Image: "vllm/vllm-openai:v0.30.0",
			// One serving port for every role, because the router refuses a deployment whose roles
			// disagree on it. No command, no Elastic EP and no parallelism arguments: the shape a
			// managed role has, so the group shape below is the one the operator itself renders.
			Ports: []workercore.ModelDeploymentPort{{Port: 8000, Protocol: core.ProtocolTCP}},
		}
	}

	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Model = workercore.ModelDeploymentModel{Name: "Qwen/Qwen3-VL-8B-Instruct"}
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.KVCache = nil
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			role("encode", workercore.ModelDeploymentRoleKindEncode, 1),
			role("prefill", workercore.ModelDeploymentRoleKindPrefill, 2),
			role("decode", workercore.ModelDeploymentRoleKindDecode, 1),
		}
		// THE PREFILLER AND THE DECODER TOOK THEIR COMMAND LINES OVER, so the operator knows no
		// route to ask the pair about and neither of them can ever activate. That is the premise
		// the lifecycle below is about: a deployment whose roles disagree about activatability,
		// with the encoder able to qualify and the pair beside it unable to.
		md.Spec.Roles[1].Command = []string{"custom-server"}
		md.Spec.Roles[2].Command = []string{"custom-server"}
	})
}

// TestRenderModelDeploymentRouterObjects_EPDKeepsUnsupportedPrefillDiscoverable states the candidate
// set the picker's global discovery selector names for a mixed encoder, prefill and decode
// deployment, across the one lifecycle that moves it: healthy, a definite fault, then recovery.
//
// THE ASYMMETRY UNDER TEST IS BETWEEN TWO SELECTORS RENDERED BY THE SAME PASS. A published per-role
// selector gains the eligibility equality only when THAT role can ever activate, and a multi-member
// prefiller never can: the group-forward leg is Unsupported for it, so no pass would ever satisfy
// the term. The global discovery selector gains the same equality when ANY role can activate, and
// the encoder beside it always can. One activation therefore narrows a selector the prefiller's own
// selector never narrowed, and a prefiller the reconciler is not permitted to qualify keeps serving
// through its own Service while global discovery stops naming it.
//
// EVERY LABEL BELOW IS DERIVED. The members are rendered and stamped the way the converger renders
// them, qualified through the real evaluator, and labeled through the production writers, so the
// sets compared here are the ones the reconciler would produce rather than ones a fixture decided
// to write by hand. A hand-stamped eligibility label would make this an assertion about the fixture.
func TestRenderModelDeploymentRouterObjects_EPDKeepsUnsupportedPrefillDiscoverable(t *testing.T) {
	md := mixedEligibilityDeployment()
	instanceType := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
	})
	manufacturers := map[string]string{
		"encode":  nodefeature.ManufacturerNVIDIA,
		"prefill": nodefeature.ManufacturerNVIDIA,
		"decode":  nodefeature.ManufacturerNVIDIA,
	}
	roles := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roles[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	// The four members this deployment runs, named the way the assertions below read them. Only the
	// prefiller has more than one: a second member of any other role would change nothing, since a
	// one-member replica is answered before the engine is even read.
	members := []struct {
		id     string
		role   string
		member int
	}{
		{id: "E0", role: "encode", member: 0},
		{id: "P0", role: "prefill", member: 0},
		{id: "P1", role: "prefill", member: 1},
		{id: "D0", role: "decode", member: 0},
	}
	pods := make([]core.Pod, 0, len(members))
	ids := make(map[types.UID]string, len(members))
	for _, m := range members {
		pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: roles[m.role], InstanceType: instanceType,
		})
		require.NoErrorf(t, err, "render the %s member of %s", m.id, m.role)
		stampModelDeploymentPod(pod, md, roles[m.role], 0, m.member)
		pod.UID = types.UID("uid-" + m.id)
		pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		pods = append(pods, *pod)
		ids[pod.UID] = m.id
	}
	byID := make(map[string]*core.Pod, len(members))
	for i := range pods {
		byID[ids[pods[i].UID]] = &pods[i]
	}
	setReady := func(t *testing.T, id string, ready bool) {
		t.Helper()
		status := core.ConditionFalse
		if ready {
			status = core.ConditionTrue
		}
		require.NotNil(t, byID[id], "no member named %s in this fixture", id)
		byID[id].Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: status}}
	}

	// NO MEMBER OF THIS SHAPE MAY REACH A PROBE. A one-member replica and an encoder are both
	// answered NotApplicable before the engine is read, and a multi-member prefiller or decoder is
	// answered Unsupported before any address is dialed. A transport that runs would mean one of
	// those three answers had stopped being made, which is a different defect than the one under
	// test and would silently change the labels derived below.
	unexpectedProbe := func(_ context.Context, url string, _ []byte) ([]byte, error) {
		t.Fatalf("no group-forward request was expected, got one to %s", url)

		return nil, nil
	}

	// One pass of the reconciler's eligibility write, in the order the converger runs it: evaluate
	// the whole-group predicate once per replica, read each member's own replica answer, and write
	// the key through the production label writer.
	deriveLabels := func(t *testing.T) []modelDeploymentInstanceQualification {
		t.Helper()
		qualifications := qualifyModelDeploymentInstances(
			context.Background(), md, pods, modelDeploymentPendingReplacement{}, unexpectedProbe)
		byMember := modelDeploymentQualificationsByMember(qualifications)
		replicaOf := make(map[types.UID]modelDeploymentReplicaView, len(pods))
		for _, view := range modelDeploymentGroupPods(pods, true) {
			for _, member := range view.Members {
				replicaOf[member.UID] = view
			}
		}
		for i := range pods {
			pod := &pods[i]
			role := roles[modelDeploymentPodRole(pod)]
			require.NotNil(t, role, "no role names the pod %s", pod.Name)
			view, seated := replicaOf[pod.UID]
			if !seated {
				view = modelDeploymentReplicaView{Members: []*core.Pod{pod}, Seated: false}
			}
			qualification, evaluated := byMember[pod.UID]
			eligible := modelDeploymentPodEligible(md, role, pod, view, qualification, evaluated,
				modelDeploymentGroupDecision{})
			modelDeploymentApplyLabel(pod, modelDeploymentLabelKeyEndpointEligible, eligible)
			// THE CANDIDATE IS WRITTEN WITH THE LABEL, as the real converger writes it: a role the
			// operator judges follows that judgement, and a role it cannot judge keeps the
			// membership the shape gives it.
			modelDeploymentApplyLabel(pod, modelDeploymentRouterCandidateLabel,
				modelDeploymentRouterCandidateOf(md, modelDeploymentRoleJudged(md, role), eligible,
					modelDeploymentShapeOfMember(pod, view, md.Spec.Engine.Name),
					pod.Labels[modelDeploymentRouterCandidateLabel] == modelDeploymentEndpointEligibleValue))
		}

		return qualifications
	}
	groupForwardOf := func(t *testing.T, role string,
		qualifications []modelDeploymentInstanceQualification,
	) modelDeploymentGroupForward {
		t.Helper()
		for _, qualification := range qualifications {
			if qualification.View.Role == role {
				return qualification.GroupForward
			}
		}
		t.Fatalf("no qualification was produced for the %s replica", role)

		return modelDeploymentGroupForward{}
	}

	render := func(t *testing.T, retainedEligibility bool) ModelDeploymentRouterObjects {
		t.Helper()
		objects, err := renderModelDeploymentRouterObjects(
			context.Background(), md, manufacturers, retainedEligibility)
		require.NoError(t, err)

		return objects
	}
	// The discovery selector is parsed with the machinery a router uses rather than read as text,
	// and the sets below are produced by matching Pod labels against the parsed form.
	discovery := func(t *testing.T, objects ModelDeploymentRouterObjects) labels.Selector {
		t.Helper()
		selector, err := labels.Parse(objects.ConfigMap.Data[modelDeploymentRouterSelectorKey])
		require.NoError(t, err, "the discovery selector must parse with the machinery a router uses")

		return selector
	}
	publishedSelector := func(objects ModelDeploymentRouterObjects, role string) map[string]string {
		for i := range objects.Contract.Roles {
			if objects.Contract.Roles[i].Name == role {
				return objects.Contract.Roles[i].Selector
			}
		}

		return nil
	}
	published := func(objects ModelDeploymentRouterObjects, role string) labels.Selector {
		return labels.SelectorFromSet(publishedSelector(objects, role))
	}
	reached := func(selector labels.Selector) []string {
		var got []string
		for i := range pods {
			if selector.Matches(labels.Set(pods[i].Labels)) {
				got = append(got, ids[pods[i].UID])
			}
		}
		slices.Sort(got)

		return got
	}
	// The picker document is parsed as the structure the router loads, so a term can be asked of a
	// stage filter rather than read out of the text that renders it.
	stageFilters := func(t *testing.T, objects ModelDeploymentRouterObjects) map[string]map[string]string {
		t.Helper()
		var document struct {
			Plugins []struct {
				Name       string `json:"name"`
				Type       string `json:"type"`
				Parameters struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"parameters"`
			} `json:"plugins"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(objects.ConfigMap.Data[modelDeploymentRouterConfigKey]), &document),
			"the picker document must parse with the machinery the router uses")
		filters := make(map[string]map[string]string, len(document.Plugins))
		for _, plugin := range document.Plugins {
			if plugin.Type == "label-selector-filter" {
				filters[plugin.Name] = plugin.Parameters.MatchLabels
			}
		}
		// EVERY KIND THIS DEPLOYMENT DECLARES MUST HAVE RENDERED A FILTER. A missing one is a nil
		// map here, which matches every Pod and so would read as a stage demanding nothing -- the
		// absence would pass every assertion below while the stage did not exist at all.
		for _, name := range []string{"encode-pods", "prefill-pods", "decode-pods"} {
			require.NotNil(t, filters[name], "the document rendered no %s stage filter", name)
		}

		return filters
	}
	// The members one stage filter would actually select, matched against the labels the real
	// derivation wrote. A stage filter names a role and not a leader, so it is expected to reach
	// every member of that role -- which is the whole difference between the filter and discovery.
	stageCandidates := func(matchLabels map[string]string) []string {
		selector := labels.SelectorFromSet(matchLabels)
		var got []string
		for i := range pods {
			if selector.Matches(labels.Set(pods[i].Labels)) {
				got = append(got, ids[pods[i].UID])
			}
		}
		slices.Sort(got)

		return got
	}
	filters := func(t *testing.T, objects ModelDeploymentRouterObjects, name string) map[string]string {
		t.Helper()

		return stageFilters(t, objects)[name]
	}
	// The retention flag the reconciler would read back, read off the document the way production
	// reads it: a stage filter demanding the candidates of some role is a requirement in force.
	//
	// IT IS THE CANDIDATE NEEDLE AND NOT THE ELIGIBILITY ONE, because in a marker-backed shape the
	// requirement the filter carries is the candidate term.
	retainedEligibilityOf := func(t *testing.T, objects ModelDeploymentRouterObjects) bool {
		t.Helper()
		for _, matchLabels := range stageFilters(t, objects) {
			if matchLabels[modelDeploymentRouterCandidateLabel] == modelDeploymentEndpointEligibleValue {
				return true
			}
		}

		return false
	}
	// The bytes a router Pod is rolled by: the whole ConfigMap, and the hash stamped on the Pod
	// template that is taken over it.
	publishedConfig := func(objects ModelDeploymentRouterObjects) map[string]string {
		return maps.Clone(objects.ConfigMap.Data)
	}
	publishedHash := func(objects ModelDeploymentRouterObjects) string {
		return objects.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash]
	}

	// THE DISCOVERY SELECTOR IS CONSTANT FOR THE WHOLE LIFE OF THIS DEPLOYMENT, and the router Pod
	// IS NOT. The first activation changes the picker's document -- that is where the encoder's
	// requirement moves to, and nothing else changes with it -- so it rolls the router once, and
	// once is the price of withdrawing an encoder at all. Every fault and recovery after that
	// re-renders the same document through retention, so none of them rolls it again. The phases
	// below assert exactly that split: the first two differ, the second and third are identical.
	var publishedDiscoverySelector string
	var coldHash, faultHash string
	var coldConfig, faultConfig map[string]string

	t.Run("a healthy deployment is discovered through the leader of every role", func(t *testing.T) {
		qualifications := deriveLabels(t)
		// THE FIXTURE'S OWN PREMISE. The deployment is an encoder of one member, a prefiller of
		// two and a decoder of one. A Prefill or a Decode group is unverifiable whatever its
		// member count, because its serving path is a pair, so the prefiller of two and the
		// decoder of one report the same Unsupported the third phase states.
		assert.Equal(t, modelDeploymentGroupForwardNotApplicable,
			groupForwardOf(t, "encode", qualifications).State, "the encoder serves no tokens")
		assert.Equal(t, modelDeploymentGroupForwardUnsupported,
			groupForwardOf(t, "prefill", qualifications).State, "a multi-member prefiller is unverifiable")
		assert.Equal(t, modelDeploymentGroupForwardUnsupported,
			groupForwardOf(t, "decode", qualifications).State, "a decoder is unverifiable whatever its member count")

		observeModelDeploymentEndpointEligibility(md, qualifications)
		assert.Equal(t, string(meta.ConditionUnknown),
			ModelDeploymentConditionEndpointEligibility.GetStatus(md), "Unsupported decides nothing")
		assert.Equal(t, modelDeploymentReasonEndpointsUnsupported,
			ModelDeploymentConditionEndpointEligibility.GetReason(md))

		objects := render(t, false)
		selector := discovery(t, objects)
		assert.ElementsMatch(t, []string{"D0", "E0", "P0"}, reached(selector),
			"before anything activates, discovery is leader-only and reaches the leader of every role")
		assert.NotContains(t, reached(selector), "P1", "a follower answers no request of its own")
		assert.False(t, selector.Matches(labels.Set(objects.Deployment.Spec.Template.Labels)),
			"the router's own Pod is not one of its endpoints")

		publishedDiscoverySelector = objects.ConfigMap.Data[modelDeploymentRouterSelectorKey]
		coldHash = publishedHash(objects)
		coldConfig = publishedConfig(objects)
		// Nothing has been decided, so no stage may demand the label -- and each stage still reaches
		// every member of its own role, the follower included.
		for filter, matchLabels := range stageFilters(t, objects) {
			_, demanded := matchLabels[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, demanded,
				"nothing has decided the gate yet, so no stage filter may demand the label (%s)", filter)
		}
		assert.ElementsMatch(t, []string{"E0"}, stageCandidates(filters(t, objects, "encode-pods")),
			"with nothing decided the encoder is a candidate of its own stage")
		assert.ElementsMatch(t, []string{"P0"}, stageCandidates(filters(t, objects, "prefill-pods")),
			"the prefiller stage reaches the candidates of its role: a follower does not answer")
		assert.ElementsMatch(t, []string{"D0"}, stageCandidates(filters(t, objects, "decode-pods")),
			"the decoder stage reaches the decoder")
	})

	// The requirement the second phase published, carried into the third as the reconciler carries
	// it -- out of the document, which is the only place it lives in this shape.
	var retainedEligibility bool

	t.Run("a definite fault withdraws the encoder and nothing else", func(t *testing.T) {
		setReady(t, "E0", false)
		qualifications := deriveLabels(t)
		observeModelDeploymentEndpointEligibility(md, qualifications)
		assert.Equal(t, string(meta.ConditionFalse),
			ModelDeploymentConditionEndpointEligibility.GetStatus(md), "the unready member is a definite fault")
		assert.Equal(t, modelDeploymentReasonEndpointsRevoked,
			ModelDeploymentConditionEndpointEligibility.GetReason(md))

		objects := render(t, false)
		selector := discovery(t, objects)
		assert.Equal(t, publishedDiscoverySelector,
			objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
			"a decided fault must not move the discovery selector: it is hashed onto the router Pod")
		// AN ENCODER OUTAGE MUST NOT TAKE TEXT-ONLY SERVING WITH IT. The prefiller beside the
		// encoder is healthy, is serving, and is a role the gate may not judge, so discovery names
		// it exactly as it did before the fault.
		assert.ElementsMatch(t, []string{"D0", "P0"}, reached(selector),
			"the roles the gate cannot judge stay discoverable through a fault in a role it can, "+
				"and the encoder the fault was in does not")

		// The withdrawal now lives where a per-role requirement can live: in the stage filter of
		// the role the fault was in, and nowhere else.
		filters := stageFilters(t, objects)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			filters["encode-pods"][modelDeploymentRouterCandidateLabel],
			"the encoder is withdrawn by its own stage filter")
		for _, filter := range []string{"prefill-pods", "decode-pods"} {
			_, demanded := filters[filter][modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, demanded,
				"a role that can never activate must not be asked for a label it never carries (%s)", filter)
		}
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			publishedSelector(objects, "encode")[modelDeploymentLabelKeyEndpointEligible],
			"the encoder's own published selector narrows too: it is the role whose activation decided this")
		for _, role := range []string{"prefill", "decode"} {
			_, narrowed := publishedSelector(objects, role)[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, narrowed,
				"a role that can never activate must keep a selector no pass would satisfy (%s)", role)
		}
		// THE COMPLETE CANDIDATE SET OF EACH STAGE, matched against labels the real derivation
		// wrote. The encoder's is empty because its only member lost its label; the other two are
		// untouched, follower included.
		assert.Empty(t, stageCandidates(filters["encode-pods"]),
			"the fault emptied the encoder stage and nothing else")
		assert.ElementsMatch(t, []string{"P0"}, stageCandidates(filters["prefill-pods"]),
			"the prefiller stage is untouched by a fault in the encoder")
		assert.ElementsMatch(t, []string{"D0"}, stageCandidates(filters["decode-pods"]),
			"the decoder stage is untouched by a fault in the encoder")

		// THE DOCUMENT DOES NOT MOVE AT ALL. The mixed scope is the stable effective policy and not
		// the pass's verdict, so a decided fault changes neither the selector nor the picker
		// document, and the router is not rolled by the fault that withdraws an endpoint.
		assert.Equal(t, coldHash, publishedHash(objects),
			"a decided fault must not roll the router: the document is hashed onto the Pod template")
		assert.Equal(t, coldConfig, publishedConfig(objects),
			"the whole ConfigMap, not only the selector inside it")

		retainedEligibility = retainedEligibilityOf(t, objects)
		assert.True(t, retainedEligibility,
			"this is the document the next pass reads its retention back from")
		faultConfig = publishedConfig(objects)
		faultHash = publishedHash(objects)
	})

	t.Run("recovery must not lose a prefiller its own selector still names", func(t *testing.T) {
		setReady(t, "E0", true)
		qualifications := deriveLabels(t)
		observeModelDeploymentEndpointEligibility(md, qualifications)
		assert.Equal(t, string(meta.ConditionUnknown),
			ModelDeploymentConditionEndpointEligibility.GetStatus(md),
			"a healthy mixed pass reports the same Unsupported the first phase reported")
		assert.Equal(t, modelDeploymentReasonEndpointsUnsupported,
			ModelDeploymentConditionEndpointEligibility.GetReason(md))

		objects := render(t, retainedEligibility)
		selector := discovery(t, objects)
		assert.Equal(t, publishedDiscoverySelector,
			objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
			"a recovery must not move the discovery selector either: it is hashed onto the router Pod")
		// THE PRECONDITION OF THE CLAIM. The prefiller is reachable, and its own published selector
		// says so, so a discovery set missing it is a disagreement between two surfaces of one pass
		// rather than a fixture that never built a reachable prefiller.
		assert.Equal(t, []string{"P0"}, reached(published(objects, "prefill")),
			"the published prefiller selector names the leader and nothing else")
		assert.ElementsMatch(t, []string{"D0", "E0", "P0"}, reached(selector),
			"global discovery must name every endpoint the deployment still serves, including a "+
				"prefiller the reconciler is not permitted to qualify: an activation beside it must "+
				"not narrow a selector its own published selector never narrowed")
		assert.NotContains(t, reached(selector), "P1", "a follower answers no request of its own")
		assert.False(t, selector.Matches(labels.Set(objects.Deployment.Spec.Template.Labels)),
			"the router's own Pod is not one of its endpoints")

		// E0 RECOVERED, SO IT IS A CANDIDATE AGAIN. Its real NotApplicable qualification restored
		// the label, and the requirement stays in force precisely so that a label means something:
		// the stage admits the encoder again on the strength of that label, not merely because the
		// filter is there.
		filters := stageFilters(t, objects)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			filters["encode-pods"][modelDeploymentRouterCandidateLabel],
			"the requirement survives the recovery, which is what retention is for")
		assert.ElementsMatch(t, []string{"E0"}, stageCandidates(filters["encode-pods"]),
			"a recovered encoder whose label the reconciler restored is a candidate of its stage again")
		assert.ElementsMatch(t, []string{"P0"}, stageCandidates(filters["prefill-pods"]),
			"and the stages the gate cannot judge never moved")
		assert.ElementsMatch(t, []string{"D0"}, stageCandidates(filters["decode-pods"]),
			"nor the decoder's")

		// AND THE RECOVERY ROLLS NOTHING. Every byte is what the fault published, because retention
		// holds the requirement rather than the reconciler deciding it again.
		assert.Equal(t, faultHash, publishedHash(objects),
			"a recovery re-renders the document the fault published, so the router does not restart")
		assert.Equal(t, faultConfig, publishedConfig(objects),
			"the whole ConfigMap, not only the hash of it")
	})

	t.Run("an encoder ready but not yet labelled stays out of its stage", func(t *testing.T) {
		// READY, BUT NOT YET QUALIFIED. The label is removed by the real derivation while the
		// member is unready, and readiness is then restored WITHOUT re-deriving -- which is the
		// window every rollout creates, between a Pod passing its probe and the reconciler's next
		// pass writing what it read. Nothing here stamps a label by hand.
		setReady(t, "E0", false)
		deriveLabels(t)
		setReady(t, "E0", true)
		assert.NotEmpty(t, byID["E0"].Status.Conditions, "the member must be Ready for this phase")
		_, labelled := byID["E0"].Labels[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, labelled, "a ready member the reconciler has not yet read carries no label")

		objects := render(t, true)
		filters := stageFilters(t, objects)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			filters["encode-pods"][modelDeploymentRouterCandidateLabel],
			"the requirement is still in force, so an unlabelled encoder is still refused")
		assert.Empty(t, stageCandidates(filters["encode-pods"]),
			"an encoder that is Ready but carries no label is not a candidate of its own stage")
		assert.ElementsMatch(t, []string{"P0"}, stageCandidates(filters["prefill-pods"]),
			"while the stages the gate cannot judge keep the candidates they had")
		assert.ElementsMatch(t, []string{"D0"}, stageCandidates(filters["decode-pods"]),
			"including the decoder's")
	})
}

// TestRenderModelDeploymentRouterObjects_MixedRolesRenderTheirOwnRequirements measures the mixed
// configuration the discovery-scoping question is about.
//
// TWO SERVER ROLES ARE ADMISSIBLE, so one of them can be a managed protected group while the other
// took its command line over. The managed role's own selector names the eligible endpoints; the
// custom role's does not, because a route this operator cannot judge carries no label for a term to
// demand. THE DEPLOYMENT-WIDE SELECTOR CAN HOLD NEITHER REQUIREMENT, and this case states what it
// renders in that shape and what it therefore admits.
func TestRenderModelDeploymentRouterObjects_MixedRolesRenderTheirOwnRequirements(t *testing.T) {
	mixed := func() *workercore.ModelDeployment {
		return newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles[0].Name = "managed"
			md.Spec.Roles[0].ReplicaSize = 2
			md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
				Name:         "custom",
				Replicas:     1,
				InstanceType: "h20-8x",
				Image:        "vllm/vllm-openai:v0.25.1",
				Command:      []string{"custom-server"},
			})
		})
	}

	objects, err := renderModelDeploymentRouterObjects(context.Background(), mixed(), nil, false)
	require.NoError(t, err)
	require.Len(t, objects.Contract.Roles, 2)

	// THE MANAGED ROLE'S OWN SELECTOR NAMES THE ELIGIBLE ENDPOINTS.
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		objects.Contract.Roles[0].Selector[modelDeploymentLabelKeyEndpointEligible],
		"the managed protected role names the eligible endpoints")
	// THE CUSTOM ROLE'S DOES NOT, because a route this operator cannot judge carries no label.
	assert.NotContains(t, objects.Contract.Roles[1].Selector, modelDeploymentLabelKeyEndpointEligible,
		"the custom route keeps the selection it had")

	// THE DEPLOYMENT-WIDE SELECTOR NAMES NEITHER, because one selector cannot hold two roles'
	// requirements apart.
	discovery := objects.ConfigMap.Data[modelDeploymentRouterSelectorKey]
	assert.NotContains(t, discovery, modelDeploymentLabelKeyEndpointEligible,
		"the deployment-wide selector renders as it always did when the roles disagree")

	// AND WHAT IT ADMITS IS THE CONSEQUENCE. A WITHDRAWN MANAGED MEMBER carries no eligibility label,
	// and it is still matched by that selector, so a router discovering by it can reach an endpoint
	// the operator withdrew.
	withdrawn := map[string]string{
		modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance:    "qwen",
		modelDeploymentLabelKeyComponent:   "managed",
		modelDeploymentMemberIndexLabel:    "0",
		modelDeploymentReplicaOrdinalLabel: "0",
	}
	assert.True(t, labels.SelectorFromSet(map[string]string{
		modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance: "qwen",
		modelDeploymentMemberIndexLabel: "0",
	}).Matches(labels.Set(withdrawn)),
		"the deployment-wide selector still matches a withdrawn managed member")

	// THE TWO RUNTIME CONSUMERS OF THAT SELECTOR. The llm-d router discovers its endpoints by the
	// deployment-wide selector alone, and its stage filters narrow per role.
	assert.False(t, router.DocumentStageFilterDemands(
		objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
		modelDeploymentLabelKeyEndpointEligible, modelDeploymentEndpointEligibleValue),
		"the llm-d stage filter demands no eligibility term outside the encoder shape")

	argvMD := mixed()
	argvMD.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
	argv, err := renderModelDeploymentRouterObjects(context.Background(), argvMD, nil, false)
	require.NoError(t, err)
	// THE ARGV PROFILES DISCOVER BY THE DEPLOYMENT-WIDE SELECTOR IN THE UNDIVIDED SHAPE, so the same
	// admission follows there.
	argvSelector := []string{}
	for _, container := range argv.Deployment.Spec.Template.Spec.Containers {
		argvSelector = append(argvSelector, container.Args...)
	}
	assert.NotContains(t, strings.Join(argvSelector, " "), modelDeploymentLabelKeyEndpointEligible,
		"the argv profile discovers by the deployment-wide selector, which names no eligibility term")
}

// TestRenderModelDeploymentRouterObjects_PrefillDecodeRendersUnchanged pins the profile that
// carries no activatable role against every byte a router Pod is rolled by.
//
// A pair whose roles took their command lines over serves routes this operator does not know, so
// no pass can ever qualify it and no pass ever writes an eligibility label on it. The gate has
// therefore nothing to say about this shape, and any term it gained would select zero endpoints.
// The hash below is taken over the whole ConfigMap, so it pins the picker document and every
// selector in it as well as the selector string beside it -- which is the point: a change here that
// reads as a comment edit is a restart of every prefill/decode router in the cluster.
//
// THE CARVE-OUT IS A PROPERTY OF THE ROLES, not of the pair: a pair whose halves are still
// managed is a managed protected group, and its selectors narrow from the first enable. The
// second half of this case is that pair, and it says so.
func TestRenderModelDeploymentRouterObjects_PrefillDecodeRendersUnchanged(t *testing.T) {
	const (
		wantSelector = "app.kubernetes.io/name=model-deployment," +
			"app.kubernetes.io/instance=qwen,modeldeployment.gpustack.ai/member-index=0," +
			"!modeldeployment.gpustack.ai/router"
		wantHash = "b3009d0233730081bcfeb308d7e3900e371ff5b151787cf076d8fe9641f76258"
	)
	// Decided as well as dormant, because a decided condition is the only thing that could move
	// this shape and the assertion that it does not is the whole claim.
	for _, decided := range []bool{false, true} {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			// THE PROFILE THE HASH BELOW PINS IS A SHAPE THE GATE MAY NEVER JUDGE, so both roles
			// take their command lines over: the operator knows no route to ask the pair about,
			// and a role it cannot ask about can never qualify.
			md.Spec.Roles[0].Command = []string{"custom-server"}
			md.Spec.Roles[1].Command = []string{"custom-server"}
		})
		if decided {
			ModelDeploymentConditionEndpointEligibility.False(
				md, modelDeploymentReasonEndpointsRevoked, "a member was withdrawn")
		}
		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		require.NotNil(t, objects.ConfigMap)

		assert.Equal(t, wantSelector, objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
			"the discovery selector of a shape with nothing to qualify")
		assert.Equal(t, wantHash,
			objects.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash],
			"every byte of the ConfigMap a router Pod is rolled by")
		assert.NotContains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
			modelDeploymentLabelKeyEndpointEligible,
			"no stage filter may demand a label no pass writes for this shape")
	}

	// The two argv profiles render no ConfigMap, so their whole configuration is the argument list
	// a Pod is rolled by. Each discovers its halves by per-role selectors, and a MANAGED pair
	// narrows those selectors from the first enable, one label per argument.
	for _, routerName := range []string{
		workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentRouterSGLang,
	} {
		objects, err := renderModelDeploymentRouterObjects(context.Background(),
			routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = routerName
			}), nil, false)
		require.NoError(t, err)
		assert.Nil(t, objects.ConfigMap, "%s publishes no ConfigMap", routerName)
		narrowed := 0
		for _, container := range objects.Deployment.Spec.Template.Spec.Containers {
			for _, arg := range container.Args {
				if arg == modelDeploymentLabelKeyEndpointEligible+"="+modelDeploymentEndpointEligibleValue {
					narrowed++
				}
			}
		}
		assert.Equal(t, 2, narrowed,
			"%s discovers by per-role selectors, and both halves of a managed protected pair narrow them (%s)",
			routerName, routerName)
	}
}

// TestSyncModelDeploymentRouter_RetentionReadsTheDocument covers the quiet passes of a mixed
// deployment, where the requirement lives only in the picker's document.
//
// The discovery selector of this shape is constant for the life of the deployment, so the term is
// never in it and a reconciler reading only the selector finds nothing. It would conclude the gate
// had never fired, stop demanding eligibility of an encoder that is ready again but not yet
// qualified, and offer it back to every media request -- on a pass that looks quiet, because
// nothing about the members changed.
func TestSyncModelDeploymentRouter_RetentionReadsTheDocument(t *testing.T) {
	md := mixedEligibilityDeployment()
	// The pass a fault leaves behind: a decided condition, and a document that now demands
	// eligibility of the encoder.
	ModelDeploymentConditionEndpointEligibility.False(
		md, modelDeploymentReasonEndpointsRevoked, "the encoder was withdrawn")
	faulted, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	require.NotContains(t, faulted.ConfigMap.Data[modelDeploymentRouterSelectorKey],
		modelDeploymentLabelKeyEndpointEligible,
		"the mixed discovery selector never carries the term, decided or not")
	require.Contains(t, faulted.ConfigMap.Data[modelDeploymentRouterConfigKey],
		modelDeploymentRouterCandidateLabel,
		"the requirement is in the document, as the candidate term of the encoder's stage")

	cli := newModelDeploymentClient(faulted.ConfigMap.DeepCopy())
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	published := new(core.ConfigMap)
	read := func(t *testing.T) *core.ConfigMap {
		t.Helper()
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, published))

		return published
	}

	// Two quiet passes over an otherwise unchanged deployment. The second is the one that needs
	// the document read: after the first, the term survives nowhere else.
	ModelDeploymentConditionEndpointEligibility.Unknown(
		md, modelDeploymentReasonEndpointsUnsupported, "a prefiller cannot be verified")
	for _, pass := range []string{"first", "second"} {
		require.NoError(t, r.syncModelDeploymentRouter(context.Background(), md, false))
		quiet := read(t)
		assert.Contains(t, quiet.Data[modelDeploymentRouterConfigKey],
			modelDeploymentRouterCandidateLabel,
			"a quiet pass must keep demanding the candidates of the encoder's stage, or it puts back "+
				"what the fault withdrew (the %s quiet pass)", pass)
		assert.NotContains(t, quiet.Data[modelDeploymentRouterSelectorKey],
			modelDeploymentLabelKeyEndpointEligible,
			"and the discovery selector must stay broad, or the prefiller is lost (the %s quiet pass)", pass)
	}
}

// TestRenderModelDeploymentRouterObjects_MixedServerRolesRenderTheCandidateUnion pins the shape the
// candidate marker exists for: several Server roles whose commands disagree.
//
// A managed Server role and a role the user gave its own command are mixed by exactly the test the
// marker is about -- one role can be qualified, the other never will be. Every Server role maps to
// one upstream kind, so they share one stage filter and one selector, and one requirement cannot name
// them both: the eligibility term would withhold the compatibility candidates of the role beside it,
// and no term at all would admit the managed members the gate withdrew.
//
// The selector below names the candidates of the deployment, and the hash covers the whole ConfigMap
// and the Pod template's hash of it, so a change to any byte a router reads, in either role order,
// fails here.
const (
	// THE DISCOVERY SELECTOR OF A MIXED SHAPE, and the bytes a router Pod is rolled by. The
	// selector names the candidates of the deployment, and the member index is gone: a replica
	// whose members all answer must be discovered whole.
	mixedServerCandidateSelector = "app.kubernetes.io/name=model-deployment," +
		"app.kubernetes.io/instance=qwen,modeldeployment.gpustack.ai/router-candidate=true," +
		"!modeldeployment.gpustack.ai/router"
	mixedServerCandidateHash = "9023938cb8e616a0317354e4465b8a9ca4e7e9b3f90da50f365d5eb009b4bc72"
)

func TestRenderModelDeploymentRouterObjects_MixedServerRolesRenderTheCandidateUnion(t *testing.T) {
	mixedServerDeployment := func(t *testing.T, externalFirst, decided bool) ModelDeploymentRouterObjects {
		t.Helper()
		managed := newRenderDeployment().Spec.Roles[0]
		managed.Name = "managed"
		managed.Replicas = 1
		managed.ReplicaSize = 2
		managed.ExtraArgs = []string{"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
		custom := *managed.DeepCopy()
		custom.Name = "custom"
		custom.ReplicaSize = 1
		custom.Command = []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}

		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		})
		if externalFirst {
			md.Spec.Roles = []workercore.ModelDeploymentRole{managed, custom}
		} else {
			md.Spec.Roles = []workercore.ModelDeploymentRole{custom, managed}
		}
		if decided {
			ModelDeploymentConditionEndpointEligibility.True(
				md, modelDeploymentReasonEndpointsQualified, "the managed group qualified")
		}
		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		require.NotNil(t, objects.ConfigMap)

		return objects
	}

	// Both orders, because a per-kind rule takes whichever role came last, and both states, because
	// only a decided gate is what could narrow anything.
	//
	// THE MIXED SCOPE IS THE STABLE EFFECTIVE POLICY, so the selector is the same in every case
	// here: a decided gate does not move it, and neither does the order of the roles.
	for _, tc := range []struct {
		externalFirst, decided bool
	}{
		{externalFirst: true, decided: false},
		{externalFirst: true, decided: true},
		{externalFirst: false, decided: false},
		{externalFirst: false, decided: true},
	} {
		order := "custom-role first"
		if tc.externalFirst {
			order = "external role first"
		}
		objects := mixedServerDeployment(t, tc.externalFirst, tc.decided)
		// THE DISCOVERY SELECTOR NAMES THE CANDIDATES, and the member index is gone: the marker
		// says which members may be discovered, and the index has nothing left to say.
		assert.Equal(t, mixedServerCandidateSelector,
			objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
			"the discovery selector of a mixed shape names the candidates (%s, decided=%t)", order, tc.decided)
		assert.Equal(t, mixedServerCandidateHash,
			objects.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash],
			"every byte a router Pod is rolled by (%s, decided=%t)", order, tc.decided)
		// THE STAGE FILTER OF THE KIND CARRIES THE CANDIDATE TERM, and not the eligibility term:
		// the filter is per kind and the requirement is per role.
		assert.True(t, router.DocumentStageFilterDemands(
			objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
			modelDeploymentRouterCandidateLabel, modelDeploymentEndpointEligibleValue),
			"the stage filter of the kind admits the candidate union (%s, decided=%t)", order, tc.decided)
		assert.False(t, router.DocumentStageFilterDemands(
			objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
			modelDeploymentLabelKeyEndpointEligible, modelDeploymentEndpointEligibleValue),
			"and it does not demand one role's eligibility requirement (%s, decided=%t)", order, tc.decided)
	}
}

// TestSyncModelDeploymentRouter_ModelNameDoesNotRetain pins the false positive a text search for the
// eligibility label would let through.
//
// A model name is user text the operator renders into the picker document, and nothing forbids it
// from being the label's own key. Read as text, such a document says the gate is in force when no
// pass ever decided it, and the next render narrows the discovery selector of a deployment that
// never asked to be narrowed -- selecting members that carry no label, of which there are none.
func TestSyncModelDeploymentRouter_ModelNameDoesNotRetain(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.KVCache = nil
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		// A non-mixed deployment: one managed Server role, so nothing here is the encoder shape
		// and the gate has no role it may judge differently from another.
		md.Spec.Model = workercore.ModelDeploymentModel{
			Name: modelDeploymentLabelKeyEndpointEligible,
		}
	})
	// The document such a deployment renders on a cold pass.
	live, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	require.Contains(t, live.ConfigMap.Data[modelDeploymentRouterConfigKey],
		modelDeploymentLabelKeyEndpointEligible,
		"the model name must be rendered into the document, or this case proves nothing")

	cli := newModelDeploymentClient(live.ConfigMap.DeepCopy())
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	require.NoError(t, r.syncModelDeploymentRouter(context.Background(), md, false))

	published := new(core.ConfigMap)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, published))
	assert.NotContains(t, published.Data[modelDeploymentRouterSelectorKey],
		modelDeploymentLabelKeyEndpointEligible,
		"a model name that reads as the label must not activate a gate no pass decided")
}

func TestRenderModelDeploymentRouterObjects_ConfigHashIsDeterministic(t *testing.T) {
	md := routedModelDeployment()
	first, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	second, err := renderModelDeploymentRouterObjects(context.Background(), md.DeepCopy(), nil, false)
	require.NoError(t, err)

	assert.Equal(t, first.ConfigMap.Data, second.ConfigMap.Data)
	assert.Equal(t, first.Deployment.Spec.Template.Annotations, second.Deployment.Spec.Template.Annotations)
}

func TestRenderModelDeploymentRouterObjects_RoleOrderDoesNotChangeTokenizer(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	md.Spec.Roles[0], md.Spec.Roles[1] = md.Spec.Roles[1], md.Spec.Roles[0]
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Equal(t, before.ConfigMap.Data, after.ConfigMap.Data)
	assert.Contains(t, after.ConfigMap.Data[modelDeploymentRouterConfigKey],
		"url: http://qwen-prefill.team-a.svc:8000")
}

func TestHashRouterConfig_IgnoresMapInsertionOrder(t *testing.T) {
	left := map[string]string{"a": "one", "b": "two"}
	right := make(map[string]string, 2)
	right["b"] = "two"
	right["a"] = "one"

	assert.Equal(t, hashRouterConfig(left), hashRouterConfig(right))
}

func TestRenderModelDeploymentRouterObjects_ScalingDoesNotRoll(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	md.Spec.Roles[0].Replicas++
	md.Spec.Roles[1].Replicas += 2
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Equal(t, before.ConfigMap.Data, after.ConfigMap.Data)
	assert.Equal(t, before.Deployment.Spec.Template, after.Deployment.Spec.Template)
	assert.NotContains(t, objectsConfig(before), "replicas")
	assert.NotContains(t, objectsConfig(before), "qwen-prefill-0")
}

func TestRenderModelDeploymentRouterObjects_ConfigChangeMovesTheHash(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	md.Spec.Router.ExtraArgs = []string{"--zap-log-level=debug"}
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.NotEqual(t,
		before.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash],
		after.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash])
	assert.NotEqual(t, before.ConfigMap.Data, after.ConfigMap.Data)
}

func TestRenderModelDeploymentRouterObjects_CarriesEngineContracts(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	config := objects.ConfigMap.Data[modelDeploymentRouterConfigKey]
	for _, value := range []string{
		"vllm:num_requests_waiting",
		"vllm:num_requests_running",
		"vllm:kv_cache_usage_perc",
		"port: 8000",
		"topicFilter: kv@",
		"socketPort: 5557",
		"replaySocketPort: 5558",
	} {
		assert.Contains(t, config, value)
	}
}

func TestRenderModelDeploymentRouterObjects_MetricsUseServingPort(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Port: 8100}}
		}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey], "port: 8100")
	assert.Equal(t, int32(8100), objects.Contract.Metrics.Port)
}

func TestRenderModelDeploymentRouterObjects_MetricsScrape(t *testing.T) {
	for _, name := range []string{
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentRouterVLLM,
		workercore.ModelDeploymentRouterSGLang,
	} {
		t.Run(name, func(t *testing.T) {
			md := routedModelDeployment()
			md.Spec.Router.Name = name
			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
			require.NoError(t, err)
			annotations := objects.Deployment.Spec.Template.Annotations
			assert.Equal(t, "true", annotations["prometheus.io/scrape"])
			assert.Equal(t, "/metrics", annotations["prometheus.io/path"])
			assert.Equal(t, "9090", annotations["prometheus.io/port"])
			assert.Equal(t, "http", annotations["prometheus.io/scheme"])
			found := false
			for _, container := range objects.Deployment.Spec.Template.Spec.Containers {
				for _, port := range container.Ports {
					if port.Name == "metrics" && port.ContainerPort == 9090 {
						found = true
					}
				}
			}
			assert.True(t, found, "the advertised port belongs to a router container")
		})
	}
}

func TestRenderModelDeploymentRouterObjects_RefusesDifferentServingPorts(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[1].Ports = []workercore.ModelDeploymentPort{{Port: 8100}}
	})

	_, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.EqualError(t, err, "router requires every role to use the same serving port; got [8000 8100]")
}

// TestRenderModelDeploymentRouterObjects_TargetsThePortTheEngineOpens covers roles that move their
// engine's port with their own --port and declare no ports: the router dials a Pod directly, so it
// has to dial where the engine listens rather than the port the Services publish.
func TestRenderModelDeploymentRouterObjects_TargetsThePortTheEngineOpens(t *testing.T) {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].ExtraArgs = []string{"--por", "9100"}
		}
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "9100", objects.ConfigMap.Data[modelDeploymentRouterTargetPortsKey])

	md.Spec.Roles[1].ExtraArgs = nil
	_, err = renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.EqualError(t, err, "router requires every role to use the same serving port; got [8000 9100]")
}

func TestRenderModelDeploymentRouterObjects_ProbesBothRouterContainers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	envoy := objects.Deployment.Spec.Template.Spec.Containers[0]
	require.NotNil(t, envoy.ReadinessProbe)
	assert.Equal(t, intstr.FromInt32(modelDeploymentRouterHTTPPort), envoy.ReadinessProbe.TCPSocket.Port)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	require.NotNil(t, epp.ReadinessProbe)
	require.NotNil(t, epp.ReadinessProbe.GRPC)
	assert.Equal(t, int32(9003), epp.ReadinessProbe.GRPC.Port)
}

// TestRenderModelDeploymentRouterObjects_PullPolicyReachesBothContainers pins the policy onto BOTH
// router containers rather than one.
//
// Both images are resolved through the same redirect, so both are as replaceable as each other, and
// a mutable tag left on the default policy is served from whatever the node already has. The field
// is the deployment's one answer for its router; a container it did not reach was a gap rather than
// an exemption.
func TestRenderModelDeploymentRouterObjects_PullPolicyReachesBothContainers(t *testing.T) {
	md := routedModelDeployment()
	md.Spec.Router.ImagePullPolicy = core.PullAlways

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	containers := objects.Deployment.Spec.Template.Spec.Containers
	require.Len(t, containers, 2)
	for i := range containers {
		assert.Equal(t, core.PullAlways, containers[i].ImagePullPolicy,
			"%s carries the policy the router declared", containers[i].Name)
	}
}

// TestRenderModelDeploymentRouterObjects_PullPolicyIsResolvedWhenUndeclared pins the case the test
// above cannot see: the field is optional and nothing defaults it.
//
// An empty policy rendered here is one the API server fills in on write, and alignRenderedContainer
// compares the field unconditionally — so the next pass reads the server's default as a difference,
// writes the empty value back, and the Deployment updates forever. Asserting a non-empty value is
// what makes that loop impossible rather than merely unobserved.
func TestRenderModelDeploymentRouterObjects_PullPolicyIsResolvedWhenUndeclared(t *testing.T) {
	md := routedModelDeployment()
	md.Spec.Router.ImagePullPolicy = ""

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	containers := objects.Deployment.Spec.Template.Spec.Containers
	require.Len(t, containers, 2)
	for i := range containers {
		assert.NotEmpty(t, containers[i].ImagePullPolicy,
			"%s renders a policy the API server has no reason to default", containers[i].Name)
	}
}

func TestRenderModelDeploymentRouterObjects_CarriesRuntimeIdentityAndStreamingTrailers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	for _, env := range []core.EnvVar{
		{Name: "NAMESPACE", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "POD_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	} {
		assert.Contains(t, epp.Env, env)
	}

	envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
	assert.Contains(t, envoy, "socket_address: {address: 0.0.0.0, port_value: 8000}")
	assert.Contains(t, envoy, "request_trailer_mode: SEND")
	assert.Contains(t, envoy, "response_trailer_mode: SEND")
	assert.Contains(t, envoy, "typed_extension_protocol_options:")
	assert.Contains(t, envoy, "explicit_http_config:")
	// The original-destination cluster follows x-gateway-destination-endpoint, so a client-supplied
	// copy of that header is what would pick the upstream. Only the EPP may set it.
	//
	// Three assertions rather than one, because the removal has to happen in a place Envoy accepts
	// AND at a moment that leaves the EPP's own copy standing, and a single "the text is present"
	// check reports neither. An earlier version of this file asserted the removal as a
	// connection-manager field, which Envoy has never declared, so the assertion passed against a
	// configuration that stopped the proxy from starting.
	// The trailing colon matters: it matches the YAML key and not the prose in the comment beside
	// it, which names the same field in order to say why it is not used.
	assert.NotContains(t, envoy, "request_headers_to_remove:",
		"HttpConnectionManager declares no such field, and a route-level removal would run after ext_proc")
	assert.Contains(t, envoy, "- remove: x-gateway-destination-endpoint")
	assert.Less(t,
		strings.Index(envoy, "envoy.filters.http.header_mutation"),
		strings.Index(envoy, "envoy.filters.http.ext_proc"),
		"the client's copy must be dropped before the EPP is consulted, and filters run in order")
}

// TestRenderModelDeploymentRouterObjects_KeepsTrafficManagementEastWest pins the two flags this
// operator renders OFF while upstream defaults them ON.
//
// IT GUARDS A BOUNDARY, NOT A VALUE. A router picks which replica serves a request that is already
// inside the cluster; TLS and caller authentication are the gateway's, at the edge. The assertion
// exists because the inverted default is exactly what a reader comparing against upstream would
// "fix", and because neither flag is reachable through extraArgs -- so flipping one here would be
// the only way it could happen, and it would happen silently.
func TestRenderModelDeploymentRouterObjects_KeepsTrafficManagementEastWest(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil, false)
	require.NoError(t, err)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	assert.Contains(t, epp.Args, "--secure-serving=false")
	assert.Contains(t, epp.Args, "--metrics-endpoint-auth=false")
}

func TestRenderModelDeploymentRouterObjects_UnmanagedProducerPublishesNoKVEvents(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "custom-model"}
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Nil(t, objects.Contract.Roles[0].KVEvents)
}

func TestRenderModelDeploymentRouterObjects_EndpointUsesRouterTransport(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--ssl-keyfile=/tls/key.pem"}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Equal(t, "http://qwen-router.team-a.svc:8000", objects.Contract.Endpoint)
	assert.Equal(t, "https://qwen-prefill.team-a.svc:8000", objects.Contract.Roles[0].Endpoint)
}

func objectsConfig(objects ModelDeploymentRouterObjects) string {
	return objects.ConfigMap.Data[modelDeploymentRouterConfigKey] +
		objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
}

func TestModelDeploymentRouter_ConvergesAndRepairsConfig(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	config := new(core.ConfigMap)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	require.NoError(t, cli.Get(context.Background(), key, config))
	want := config.Data[modelDeploymentRouterConfigKey]
	config.Data[modelDeploymentRouterConfigKey] = "edited out of band"
	require.NoError(t, cli.Update(context.Background(), config))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), key, config))
	assert.Equal(t, want, config.Data[modelDeploymentRouterConfigKey])
}

func TestModelDeploymentRouter_RemovalPrunesEveryObject(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	objects := []ctrlcli.Object{
		new(app.Deployment), new(core.ConfigMap), new(core.Service),
		new(core.ServiceAccount), new(rbac.Role), new(rbac.RoleBinding),
	}
	for _, object := range objects {
		require.NoError(t, cli.Get(context.Background(), key, object), "%T must exist before removal", object)
	}

	md := getModelDeployment(t, cli)
	md.Spec.Router = nil
	require.NoError(t, cli.Update(context.Background(), md))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for _, object := range objects {
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), key, object)), "%T", object)
	}
	md = getModelDeployment(t, cli)
	assert.Nil(t, md.Status.Router)
	assert.Equal(t, "http://qwen.team-a.svc:8000", md.Status.Endpoint)

	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), md))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	for _, object := range objects {
		require.NoError(t, cli.Get(context.Background(), key, object), "%T must be recreated", object)
	}
}

func TestModelDeploymentRouter_ReadyReplicaPublishesRouterEndpoint(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	md := getModelDeployment(t, cli)
	assert.Empty(t, md.Status.Endpoint)

	deployment := new(app.Deployment)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	require.NoError(t, cli.Get(context.Background(), key, deployment))
	deployment.Status.ReadyReplicas = 1
	require.NoError(t, cli.Status().Update(context.Background(), deployment))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md = getModelDeployment(t, cli)
	assert.Equal(t, "http://qwen-router.team-a.svc:8000", md.Status.Endpoint)
}

func TestModelDeploymentRouter_RefusesToAdopt(t *testing.T) {
	foreign := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "qwen-router", Namespace: "team-a"}}
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType(), foreign)

	_, err := reconcileModelDeployment(t, cli)
	require.EqualError(t, err, "configmap team-a/qwen-router is not owned by this deployment")
}

func TestModelDeploymentRouter_SecondPassWritesNothing(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, 13, writes.creates,
		"the pass creates four replicas, three role Services and six router objects")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWrites{}, *writes)
}

func TestModelDeploymentRouter_ConfigChangeRollsOnce(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md := getModelDeployment(t, cli)
	md.Spec.Router.ExtraArgs = []string{"--zap-log-level=debug"}
	require.NoError(t, cli.Update(context.Background(), md))
	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, 2, writes.updates, "only the ConfigMap and Deployment move")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWrites{}, *writes)
}

// TestRenderModelDeploymentRouterObjects_NamesTheProgramToRun pins the command on the container
// that runs out of the shared router image, for every router that renders one.
//
// IT IS ASSERTED ON THE CONTAINER rather than on the render, because the defect it exists for lived
// in the gap between them: the render named the binary and the container did not carry it. The
// image declares no entrypoint on purpose -- it holds three programs -- so a container given
// arguments and no command puts the first flag where the program name goes and dies as "executable
// file not found", naming the flag. Found by running it on a cluster, not by this suite, which is
// why the assertion is on the field rather than on a message.
func TestRenderModelDeploymentRouterObjects_NamesTheProgramToRun(t *testing.T) {
	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		container string
		want      []string
	}{
		{
			name:      "the picker runs the endpoint picker",
			md:        routedModelDeployment(),
			container: "epp",
			want:      []string{"epp"},
		},
		{
			name: "the vllm router runs its own binary",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			}),
			container: "router",
			want:      []string{"vllm-router"},
		},
		{
			name: "the sglang gateway runs its own binary",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			}),
			container: "router",
			want:      []string{"sgl-model-gateway"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(context.Background(), c.md, nil, false)
			require.NoError(t, err)

			commands := map[string][]string{}
			for _, ctr := range objects.Deployment.Spec.Template.Spec.Containers {
				commands[ctr.Name] = ctr.Command
			}
			require.Contains(t, commands, c.container, "the router must render this container")
			assert.Equal(t, c.want, commands[c.container],
				"the container must name which of the image's three programs it runs")
		})
	}
}

// TestRenderModelDeploymentRouterObjects_ImageSources pins where each of a managed router's two
// container images comes from. Both defaults are settings rather than constants, so a change to
// either is one every cluster inherits at once, and this is the only place that shows up.
//
// The picker's row also pins the precedence: an image named on the object wins, and it is taken
// verbatim rather than redirected, because it is the user's own reference.
func TestRenderModelDeploymentRouterObjects_ImageSources(t *testing.T) {
	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		container string
		want      string
		why       string
	}{
		{
			name:      "proxy runs the mirrored image, pinned to a release",
			md:        routedModelDeployment(),
			container: "envoy",
			want:      "gpustack/mirrored-envoy:distroless-v1.33.2",
			why:       "the proxy has no field to override it; only the setting redirects it",
		},
		{
			// One image carries every router, and which binary runs is the rendered command's
			// decision rather than the image's. It is this project's own build because the three
			// routers come from three sources and one of them carries a patch shipped here, so no
			// upstream image holds them together.
			name:      "picker defaults to the image that carries every router",
			md:        routedModelDeployment(),
			container: "epp",
			want:      "gpustack/llm-router:v0.3.1",
			why:       "the default names a release of this project's own build, not a moving upstream tag",
		},
		{
			name: "and so does a router configured by argv, from the same setting",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			}),
			container: "router",
			want:      "gpustack/llm-router:v0.3.1",
			why:       "one setting says where the binaries come from; the command picks which one runs",
		},
		{
			name: "an image named on the object is used verbatim",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Image = "registry.example/team/picker:v9"
			}),
			container: "epp",
			want:      "registry.example/team/picker:v9",
			why:       "the user's own reference must not be rewritten to the cluster mirror",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(context.Background(), c.md, nil, false)
			require.NoError(t, err)

			images := map[string]string{}
			for _, ctr := range objects.Deployment.Spec.Template.Spec.Containers {
				images[ctr.Name] = ctr.Image
			}
			require.Contains(t, images, c.container, "the router must render this container")
			assert.Equal(t, c.want, images[c.container], c.why)
		})
	}
}

// TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender pins the WHOLE
// object set -- serialized and digested, not field-picked -- for one input off every branch of the
// renderer. The other cases in this file assert the fields they exist for, so a refactor of the
// render path could drop a field no case reads and every one of them would stay green; this is the
// case that cannot, because it compares the bytes of everything at once.
//
// A DIGEST MOVES ONLY WHEN THE RENDERED OUTPUT OR THE DEFAULT ROUTER IMAGE CHANGES. Every case
// running the default image moves together when that image advances, and the one case naming its
// own image does not, which pins such a move to the setting rather than to the renderer. A case
// whose engine renders differently from the rest moves alone, so an SGLang-only picker change shows
// up in its digest while the vLLM cases pin theirs as unchanged.
//
// To re-baseline after an intended rendering change: empty the table, run this case, and pin the
// digests the failures print.
func TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender(t *testing.T) {
	pinned := map[string]string{
		// The digests moved when the router HTTP port unified on 8000; they are pinned again here.
		// They moved again when a managed protected group began to name the eligible endpoints from
		// the first enable: every case whose roles are managed carries the term, and the sole
		// single-member Server, which keeps its existing behavior, does not move at all.
		"a prefill and decode pair": "9a449fcaaf867826dea0367a483e54e5388ddb395bd4afc8294553ba02b5bcb5",
		"a sole server role":        "093e7e521452bbc07a00de54ad477f1a58dfd0d155fb16bdac70d94fad06edb5",
		"a router declaring its own image, policy, replicas and extra arguments": "79a13fe3afffc0896e22d1b01cff9ad87f7c9be22aa04356838731d18d41645f",
		"a role whose command the user took over, so it publishes no events":     "c9e93f6badf7d4ec334934ad2b5807bfe92ab16af110ac47b921ee67be001f17",
		"an sglang engine, whose metrics contract differs":                       "475e475ecf7d41aa7eee26de36516cd582d70c955a3bd03c975acb525d58bc6d",
	}

	soleServerRole := func() *workercore.ModelDeployment {
		md := newRenderDeployment()
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

		return md
	}

	testCases := []struct {
		name          string
		md            *workercore.ModelDeployment
		manufacturers map[string]string
	}{
		{
			name: "a prefill and decode pair",
			md:   routedModelDeployment(),
		},
		{
			name: "a sole server role",
			md:   soleServerRole(),
		},
		{
			name: "a router declaring its own image, policy, replicas and extra arguments",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Image = "registry.example/team/picker:v9"
				md.Spec.Router.ImagePullPolicy = core.PullAlways
				md.Spec.Router.Replicas = ptr.To(int32(3))
				md.Spec.Router.ExtraArgs = []string{"--v=4"}
			}),
		},
		{
			name: "a role whose command the user took over, so it publishes no events",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "custom-model"}
			}),
		},
		{
			name: "an sglang engine, whose metrics contract differs",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(
				context.Background(), tc.md, tc.manufacturers, false)
			require.NoError(t, err)
			for i := range objects.Contract.Roles {
				kind := ModelDeploymentEffectiveRoleKind(&tc.md.Spec.Roles[i])
				assert.Equal(t, kind, objects.Contract.Roles[i].Kind)
				upstream, ok := ModelDeploymentRoleUpstreamKind(kind)
				require.True(t, ok)
				objects.Contract.Roles[i].Kind = workercore.ModelDeploymentRoleKind(upstream)
			}

			encoded, err := json.Marshal(objects)
			require.NoError(t, err)
			digest := stringx.SumBytesBySHA256(encoded)

			want, ok := pinned[tc.name]
			if !ok {
				t.Fatalf("no pinned digest for %q; the digest this build produces is %s -- pin it in the table", tc.name, digest)
			}
			assert.Equal(t, want, digest)
		})
	}
}

// TestRenderModelDeploymentRouterWorkload_RefusesARouterWithNoWorkload pins the refusal on the shape
// a newly added router arrives in.
//
// The caller renders the router's configuration first and is refused there for a name nothing
// registered, so this branch is not reachable through it today. It is asserted anyway because this
// is the function a fourth router is added to, and the failure it prevents is silent: a branch that
// returns the zero value renders a Deployment carrying no containers at all.
func TestRenderModelDeploymentRouterWorkload_RefusesARouterWithNoWorkload(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router.Name = "a-router-with-no-branch"
	})

	_, err := renderModelDeploymentRouterWorkload(context.Background(), md,
		modelDeploymentRouterDerived{Name: "qwen-router", Rendered: router.Output{Document: "a: b"}})
	require.EqualError(t, err, `router "a-router-with-no-branch" renders no workload`)
}

// TestRenderModelDeploymentRouterWorkload_ConfigurationBelongsToTheBranchThatReadsIt states the
// boundary the split draws, from the side that would silently stop being true.
//
// EVERY ConfigMap key belongs to a branch, including the three that look router-independent. The
// selector, the target ports and the user's arguments reach the picker as a mounted file and as
// environment entries, and they reach a router configured by argv as arguments -- so writing them
// in the shared half would put a second copy of one derivation where nothing reads it, and leave a
// reader unable to tell which copy the process is using. The digests cannot state this: both halves
// write into one object, so a key rendered from the wrong side produces identical bytes.
func TestRenderModelDeploymentRouterWorkload_ConfigurationBelongsToTheBranchThatReadsIt(t *testing.T) {
	derived := modelDeploymentRouterDerived{
		Name:             "qwen-router",
		Rendered:         router.Output{Document: "a: b"},
		EndpointSelector: "app.kubernetes.io/name=model-deployment",
		TargetPorts:      "8000",
		ExtraArgs:        "[]",
	}

	picker, err := renderModelDeploymentRouterWorkload(
		context.Background(), routedModelDeployment(), derived)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		modelDeploymentRouterConfigKey,
		modelDeploymentRouterEnvoyConfigKey,
		modelDeploymentRouterExtraArgsKey,
		modelDeploymentRouterSelectorKey,
		modelDeploymentRouterTargetPortsKey,
	}, slices.Collect(maps.Keys(picker.Config)),
		"the picker reads all five, so its branch writes all five")
	assert.Len(t, picker.Containers, 2, "the picker and the proxy that fronts it")
	assert.Len(t, picker.Volumes, 1, "the mounted document, and nothing else")

	argv, err := renderModelDeploymentRouterWorkload(context.Background(),
		routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
		}), derived)
	require.NoError(t, err)
	assert.Empty(t, argv.Config, "a router configured by argv reads no configuration object")
	assert.Len(t, argv.Containers, 1, "one process, no proxy")
	assert.Empty(t, argv.Volumes, "nothing to mount")
}

// TestRenderModelDeploymentRouterObjects_ArgvRouterRendersNoConfigMap states what the case above
// implies about the object set: no configuration means no object, rather than an empty one.
//
// An empty ConfigMap would be indistinguishable from one whose content was dropped, and the Pod
// would carry a configuration hash taken over nothing -- a value that never moves, on an annotation
// whose whole purpose is to move.
func TestRenderModelDeploymentRouterObjects_ArgvRouterRendersNoConfigMap(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	assert.Nil(t, objects.ConfigMap)
	assert.NotContains(t, objects.Deployment.Spec.Template.Annotations, modelDeploymentRouterConfigHash)
	require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, objects.Deployment.Spec.Template.Spec.Volumes)
	assert.Empty(t, objects.Deployment.Spec.Template.Spec.Containers[0].VolumeMounts)
	assert.NotNil(t, objects.Service, "the shared half still fronts it")
	assert.NotNil(t, objects.Role, "and still grants it the discovery it needs")
}

// TestRenderModelDeploymentRouterObjects_TheGatewayAndTheEngineReadOneField pins the property the
// SGLang gateway's routing rests on.
//
// That gateway registers a discovered worker under the id the WORKER reports for itself, not one
// discovery assigns, and it refuses a request naming a model no registered worker answers to. So
// the name a caller asks for and the name the engine registers under have to be one field, and this
// asserts they are -- by comparing the two derivations WITH EACH OTHER rather than each with a
// literal, which is what the cases already in this repository do and what would keep passing if one
// side started reading somewhere else.
//
// The cluster half of the same claim -- that the gateway does register the worker under this name
// -- is an end-to-end case; nothing rendered here can observe a registry.
func TestRenderModelDeploymentRouterObjects_TheGatewayAndTheEngineReadOneField(t *testing.T) {
	// Deliberately not the fixture's own model, so a renderer that hardcoded that string fails.
	const model = "org/a-model-the-fixture-does-not-name"

	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
		md.Spec.Model.Name = model
	})
	require.NotEqual(t, model, newRenderDeployment().Spec.Model.Name,
		"the fixture must not already carry this name, or the comparison proves nothing")

	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
	})
	require.NoError(t, err)

	argv := pod.Spec.Containers[0].Command
	at := slices.Index(argv, "--model-path")
	require.GreaterOrEqual(t, at, 0, "the engine is launched with the model named; got %v", argv)
	require.Less(t, at+1, len(argv))

	assert.Equal(t, md.Spec.Model.Name, argv[at+1],
		"the id the worker registers under is the deployment's own model name")

	// And the other end names nothing, which is what makes the engine's value the only one: the
	// gateway routes by what workers registered, so a name rendered here would be a second source
	// for one answer.
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
	for _, arg := range objects.Deployment.Spec.Template.Spec.Containers[0].Args {
		assert.NotContains(t, arg, model,
			"the gateway is told no model name; it routes by the id each worker reports")
	}
}

// TestRenderModelDeploymentRouterObjects_MetricsContractIsThePickersAlone is the renderer's half of
// the same gate the admission handler carries.
//
// The engine metrics table exists to feed one router's metrics extractor. The other two score on
// their own observations and scrape none of it, so a published contract naming engine metrics under
// them would describe a router that is not running — and the call itself would refuse, in a third
// router's name, on an engine whose row happened to be missing.
func TestRenderModelDeploymentRouterObjects_MetricsContractIsThePickersAlone(t *testing.T) {
	cases := []struct {
		router, engine string
		want           bool
	}{
		{router: workercore.ModelDeploymentRouterLLMD, engine: workercore.ModelDeploymentEngineVLLM, want: true},
		{router: workercore.ModelDeploymentRouterVLLM, engine: workercore.ModelDeploymentEngineVLLM},
		{router: workercore.ModelDeploymentRouterSGLang, engine: workercore.ModelDeploymentEngineSGLang},
	}
	for _, c := range cases {
		t.Run(c.router, func(t *testing.T) {
			md := routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = c.router
				md.Spec.Engine.Name = c.engine
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
			require.NoError(t, err)

			if c.want {
				require.NotNil(t, objects.Contract.Metrics)
				assert.NotEmpty(t, objects.Contract.Metrics.QueuedRequests,
					"the picker is configured from these strings, so they are published")

				return
			}
			assert.Nil(t, objects.Contract.Metrics,
				"this router scrapes no engine metrics, so it publishes none")
		})
	}
}

// TestRenderModelDeploymentRouterObjects_ProxyTemplateIsAssertedOnItsRenderedText reads the proxy
// configuration as the proxy would, rather than as the constant it came from.
//
// NO PLACEHOLDER MAY SURVIVE, and that is the assertion this case exists for. The template is
// substituted rather than formatted, because it is full of Envoy's own percent-delimited operators
// and a format verb would either mangle them or need every one escaped. Substitution fails quietly:
// a renamed placeholder leaves its own text in the rendered file, Envoy reads it as a duration it
// cannot parse, and the proxy does not start -- with nothing here to say why.
func TestRenderModelDeploymentRouterObjects_ProxyTemplateIsAssertedOnItsRenderedText(t *testing.T) {
	t.Run("the field renders as the route timeout", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.RequestTimeoutSeconds = ptr.To(int32(90))
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "timeout: 90s")
		assert.NotContains(t, envoy, modelDeploymentRouterEnvoyTimeoutPlaceholder,
			"a surviving placeholder is a duration Envoy cannot parse")
		assert.NotContains(t, envoy, "<", "no placeholder of any spelling survives")
	})

	t.Run("unset keeps the proxy's own day", func(t *testing.T) {
		objects, err := renderModelDeploymentRouterObjects(
			context.Background(), routedModelDeployment(), nil, false)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "timeout: 86400s",
			"absence renders the value this proxy has always carried, not a second field's default")
		assert.NotContains(t, envoy, modelDeploymentRouterEnvoyTimeoutPlaceholder)
	})

	t.Run("the access log names the endpoint beside the outcome", func(t *testing.T) {
		objects, err := renderModelDeploymentRouterObjects(
			context.Background(), routedModelDeployment(), nil, false)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "envoy.access_loggers.stdout")
		for _, operator := range []string{
			"%RESPONSE_CODE%", "%RESPONSE_FLAGS%", "%RESPONSE_CODE_DETAILS%", "%DURATION%",
			"%UPSTREAM_HOST%", "%REQ(:METHOD)%", "%REQ(X-REQUEST-ID)%",
			"%BYTES_RECEIVED%", "%BYTES_SENT%",
		} {
			assert.Contains(t, envoy, operator,
				"the log carries the pair no other log on this Pod does: the endpoint and the outcome")
		}
		assert.NotContains(t, envoy, "admin:",
			"an admin listener is an unauthenticated control surface, not an access log")
	})
}

// TestRenderModelDeploymentRouterObjects_TimeoutAndThresholdReachTheirOwnRouter pins where each of
// the two fields lands, which is not the same place for both.
//
// The timeout reaches all three: the proxy's route under the picker, a flag under the other two.
// The threshold reaches one, because only the picker has a per-request decision to threshold —
// admission refuses it on the others, so this asserts what the accepted value does rather than
// restating that refusal.
func TestRenderModelDeploymentRouterObjects_TimeoutAndThresholdReachTheirOwnRouter(t *testing.T) {
	t.Run("the timeout becomes a flag on a router configured by argv", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			md.Spec.Router.RequestTimeoutSeconds = ptr.To(int32(90))
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
		assert.Contains(t, objects.Deployment.Spec.Template.Spec.Containers[0].Args,
			"--request-timeout-secs=90")
	})

	t.Run("unset renders no flag at all", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		for _, arg := range objects.Deployment.Spec.Template.Spec.Containers[0].Args {
			assert.NotContains(t, arg, "--request-timeout-secs",
				"absence is what leaves this router on its own default, which differs from the proxy's")
		}
	})

	for _, tc := range []struct {
		name      string
		threshold *int32
		want      string
	}{
		{name: "a declared threshold", threshold: ptr.To(int32(512)), want: "nonCachedTokens: 512"},
		{name: "an explicit zero, which disables splitting", threshold: ptr.To(int32(0)), want: "nonCachedTokens: 0"},
		{name: "unset, which renders the value it always did", want: "nonCachedTokens: 8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.DisaggregationThresholdTokens = tc.threshold
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
			require.NoError(t, err)
			assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey], tc.want)
		})
	}
}

// TestModelDeploymentRouterCandidate_RidesTheGuardedEligibilityWrite pins the candidate marker
// against the guarded write it rides.
//
// THE MARKER IS NOT A QUALIFICATION. It names the members a router may discover, and it is written
// in the same patch as the eligibility label and under the same decision, so a member that loses its
// eligibility loses its candidacy in the same write. No pass can read one without the other.
//
// A MEMBER THAT DOES NOT ANSWER CARRIES NEITHER. The follower of a leader-served replica is not an
// endpoint, so it is neither eligible nor a candidate, and the two facts cannot come apart.
//
// THE REPLACED MEMBER IS NOT INHERITED. A name is not an identity: a different incarnation standing
// under the name the pass judged is skipped, and the candidate membership of the member that is gone
// does not land on it.
//
// THE FAILED WRITE IS NOT A HALF WRITE. The patch carries the version the read returned, so a member
// that moved under the pass fails the write instead of landing on facts the pass no longer holds.
func TestModelDeploymentRouterCandidate_RidesTheGuardedEligibilityWrite(t *testing.T) {
	type fixture struct {
		md          *workercore.ModelDeployment
		members     []*core.Pod
		pods        []core.Pod
		byMember    map[types.UID]modelDeploymentInstanceQualification
		observation modelDeploymentPassObservation
		generation  modelDeploymentMembershipGeneration
		leader      string
	}
	build := func(t *testing.T) fixture {
		t.Helper()

		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Replicas = 1
			md.Spec.Roles[0].ReplicaSize = 2
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		})
		standing := replicaMembersAt(t, md, "server", 0)
		require.Len(t, standing, 2)

		members := make([]*core.Pod, 0, len(standing))
		for _, pod := range standing {
			live := pod.DeepCopy()
			live.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
			members = append(members, live)
		}
		pods := make([]core.Pod, 0, len(members))
		for _, member := range members {
			pods = append(pods, *member)
		}
		views := modelDeploymentGroupPods(pods, true)
		require.Len(t, views, 1, "one replica of one role")
		generation := modelDeploymentGenerationOf(views[0])
		key := modelDeploymentGroupKey{
			Role: views[0].Role, Ordinal: views[0].Ordinal, Generation: generation,
		}

		// THE OBSERVATION OF A GROUP THAT IS SERVING, which is what restores a protected group.
		observation := modelDeploymentPassObservation{
			Serving: ServingAnswer{
				State:       workercore.ModelDeploymentServingStateConfirmed,
				CollectedAt: time.Now(),
				Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{
					key: {
						SnapshotValid: true,
						State:         workercore.ModelDeploymentServingStateConfirmed,
						Endpoints:     1,
						Members:       2,
						Expected:      2,
					},
				},
			},
		}
		byMember := map[types.UID]modelDeploymentInstanceQualification{}
		for _, member := range members {
			byMember[member.UID] = modelDeploymentTestQualification(members, generation, false)
		}
		leader := ""
		for _, member := range members {
			if modelDeploymentAnsweringMemberLeader(member) {
				leader = member.Name
			}
		}
		require.NotEmpty(t, leader, "the replica has a leader")

		return fixture{
			md: md, members: members, pods: pods, byMember: byMember,
			observation: observation, generation: generation, leader: leader,
		}
	}
	labelsOf := func(t *testing.T, cli ctrlcli.Client) map[string]map[string]string {
		t.Helper()

		carried := map[string]map[string]string{}
		for _, pod := range replicaPods(t, cli) {
			carried[pod.Name] = pod.Labels
		}

		return carried
	}

	t.Run("the candidacy is written with the eligibility label", func(t *testing.T) {
		f := build(t)
		cli := newCompositionClient(f.md, &f.pods[0], &f.pods[1])
		r := &ModelDeploymentReconciler{
			Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		}
		require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
			context.Background(), f.md, f.pods, f.byMember, nil, f.observation))

		carried := labelsOf(t, cli)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			carried[f.leader][modelDeploymentLabelKeyEndpointEligible],
			"the answering member of a qualified group is eligible")
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			carried[f.leader][modelDeploymentRouterCandidateLabel],
			"and it is a routing candidate in the same write")
		follower := ""
		for _, member := range f.members {
			if member.Name != f.leader {
				follower = member.Name
			}
		}
		assert.NotContains(t, carried[follower], modelDeploymentLabelKeyEndpointEligible,
			"a member that does not answer is not eligible")
		assert.NotContains(t, carried[follower], modelDeploymentRouterCandidateLabel,
			"and it is not a candidate either")
	})

	t.Run("a definite fault withdraws the candidacy with the label", func(t *testing.T) {
		f := build(t)
		cli := newCompositionClient(f.md, &f.pods[0], &f.pods[1])
		r := &ModelDeploymentReconciler{
			Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		}
		require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
			context.Background(), f.md, f.pods, f.byMember, nil, f.observation))
		require.Equal(t, modelDeploymentEndpointEligibleValue,
			labelsOf(t, cli)[f.leader][modelDeploymentLabelKeyEndpointEligible],
			"the qualified member carries its eligibility")

		// THE PODS ARE READ BACK, because the write landed on the server and the next pass reads it
		// there: a pass that judged the copy it already held would find nothing to withdraw.
		live := replicaPods(t, cli)
		refuted := map[types.UID]modelDeploymentInstanceQualification{}
		for i := range live {
			refuted[live[i].UID] = modelDeploymentTestQualification(
				[]*core.Pod{&live[i]}, f.generation, true)
		}
		require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
			context.Background(), f.md, live, refuted, nil, f.observation))

		carried := labelsOf(t, cli)
		assert.NotContains(t, carried[f.leader], modelDeploymentLabelKeyEndpointEligible,
			"a definite fault withdraws the member")
		assert.NotContains(t, carried[f.leader], modelDeploymentRouterCandidateLabel,
			"and the candidacy goes with it in the same write")
	})

	t.Run("a replaced member does not inherit the candidacy", func(t *testing.T) {
		f := build(t)
		replaced := newCompositionClient(f.md, &f.pods[0], &f.pods[1])
		intercepted := ctrlinterceptor.NewClient(replaced, ctrlinterceptor.Funcs{
			Get: func(ctx context.Context, next ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if err := next.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if pod, ok := obj.(*core.Pod); ok && pod.Name == f.leader {
					pod.UID = types.UID("a-different-incarnation")
				}

				return nil
			},
		})
		r := &ModelDeploymentReconciler{
			Client: intercepted, APIReader: intercepted, Recorder: ctrlrecord.NewFakeRecorder(64),
		}
		require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
			context.Background(), f.md, f.pods, f.byMember, nil, f.observation))

		assert.NotContains(t, labelsOf(t, replaced)[f.leader], modelDeploymentRouterCandidateLabel,
			"the candidate membership of the member that is gone does not land on its replacement")
	})

	t.Run("a member that moved under the pass fails the write", func(t *testing.T) {
		f := build(t)
		moved := newCompositionClient(f.md, &f.pods[0], &f.pods[1])
		intercepted := ctrlinterceptor.NewClient(moved, ctrlinterceptor.Funcs{
			Get: func(ctx context.Context, next ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if err := next.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if pod, ok := obj.(*core.Pod); ok && pod.Name == f.leader {
					pod.ResourceVersion = "a-version-this-pass-did-not-read"
				}

				return nil
			},
		})
		r := &ModelDeploymentReconciler{
			Client: intercepted, APIReader: intercepted, Recorder: ctrlrecord.NewFakeRecorder(64),
		}
		err := r.convergeModelDeploymentEndpointEligibility(
			context.Background(), f.md, f.pods, f.byMember, nil, f.observation)
		require.Error(t, err, "the optimistic lock refuses the write")
		assert.NotContains(t, labelsOf(t, moved)[f.leader], modelDeploymentRouterCandidateLabel,
			"and no candidate membership lands on facts the pass no longer holds")
	})
}

// TestModelDeploymentRoleJudged_KeepsTheProtectedClassNarrow pins the judgment the mixed scope is
// read from.
//
// A MANAGED PAIR WHOSE GROUP CANNOT BE OBSERVED IS STILL JUDGED. The protected class is in force
// from the first enable, and a group the operator cannot observe is a group it can still withdraw.
//
// A CUSTOM PAIR IS NOT. Its route is one this operator does not know, so there is no group of it to
// qualify and no requirement to carry.
//
// THE TWO ARE DISTINCT, and the distinction is the whole of the mixed scope: a deployment that holds
// one of each is the shape the candidate marker exists for.
func TestModelDeploymentRoleJudged_KeepsTheProtectedClassNarrow(t *testing.T) {
	managed := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
	})
	assert.True(t, modelDeploymentRoleJudged(managed, &managed.Spec.Roles[0]),
		"a managed pair whose group cannot be observed is judged: the protected class is in force")
	assert.True(t, modelDeploymentRoleJudged(managed, &managed.Spec.Roles[1]),
		"and so is its partner")

	custom := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Command = []string{"custom-server"}
	})
	assert.False(t, modelDeploymentRoleJudged(custom, &custom.Spec.Roles[0]),
		"a role whose route this operator does not know is not judged")
	assert.True(t, modelDeploymentRoleJudged(custom, &custom.Spec.Roles[1]),
		"and its managed partner beside it still is")
}

// TestModelDeploymentStampRouterCandidate_BirthAndReplacement pins the marker at birth.
//
// A ROLE THE OPERATOR JUDGES IS NOT A CANDIDATE AT BIRTH: it has not been judged, and the marker
// comes with the eligibility label.
//
// A COMPATIBILITY ROLE IS ONE FROM BIRTH, and only the member that answers carries it: the
// follower of a leader-served replica does not answer, so it is not a candidate.
//
// A REPLACEMENT MEMBER IS STAMPED THE SAME WAY. It is a new object under a new name, and the fact
// that its predecessor was withdrawn does not reach it.
func TestModelDeploymentStampRouterCandidate_BirthAndReplacement(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Command = []string{"custom-server"}
	})
	role := &md.Spec.Roles[0]

	newborn := func(name string) *core.Pod {
		pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace}}
		pod.Spec.Containers = []core.Container{{
			Name: "model", Image: "vllm/vllm-openai:v0.25.1", Command: role.Command,
		}}

		return pod
	}

	// THE COMPATIBILITY ROLE'S LEADER IS A CANDIDATE FROM BIRTH.
	leader := newborn("custom-0")
	modelDeploymentStampRouterCandidate(leader, md, role, modelDeploymentLeaderMemberIndex)
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		leader.Labels[modelDeploymentRouterCandidateLabel],
		"a compatibility candidate is a candidate from birth")

	// ITS FOLLOWER DOES NOT ANSWER, so it is not one.
	follower := newborn("custom-1")
	modelDeploymentStampRouterCandidate(follower, md, role, 1)
	assert.NotContains(t, follower.Labels, modelDeploymentRouterCandidateLabel,
		"a follower of a leader-served replica does not answer")

	// THE REPLACEMENT OF THE LEADER IS STAMPED THE SAME WAY: it is a new object under a new name.
	replacement := newborn("custom-0-replacement")
	modelDeploymentStampRouterCandidate(replacement, md, role, modelDeploymentLeaderMemberIndex)
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		replacement.Labels[modelDeploymentRouterCandidateLabel],
		"the replacement of a candidate is a candidate from birth")

	// A ROLE THE OPERATOR JUDGES STARTS UNMARKED, and the converger marks it with the label.
	judged := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
	})
	managed := newborn("managed-0")
	modelDeploymentStampRouterCandidate(managed, judged, &judged.Spec.Roles[0],
		modelDeploymentLeaderMemberIndex)
	assert.NotContains(t, managed.Labels, modelDeploymentRouterCandidateLabel,
		"a managed newborn has not been judged, so it starts unmarked")
}

// TestModelDeploymentRouterCandidate_ThePublishedDiscoveryAndTheWrittenLabelsAgree compares the
// published discovery with the labels the pass writes.
//
// WHAT IT COMPARES: the discovery the rendered configuration publishes, parsed, against the labels
// the members carry. Every member the pass marked is admitted by that discovery and every member it
// did not mark is refused, so the two agree about which members may be discovered.
//
// WHAT IT DOES NOT SHOW: the order the two writes happen in. An agreement is the same whether the
// configuration was published before the labels were written or after it, so this case cannot tell a
// pass that publishes first from a pass that publishes last. The order is measured from inside a
// pass, by TestModelDeploymentRouterCandidate_TheLabelsAreCommittedBeforeTheConfigurationIsPublished.
func TestModelDeploymentRouterCandidate_ThePublishedDiscoveryAndTheWrittenLabelsAgree(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.KVCache = nil
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles[0].Name = "managed"
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
			Name:         "custom",
			Replicas:     1,
			ReplicaSize:  2,
			InstanceType: "h20-8x",
			Image:        "vllm/vllm-openai:v0.25.1",
			Command:      []string{"python3", "-m", "vllm.entrypoints.openai.api_server"},
		})
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	// THE DISCOVERY AS PUBLISHED, read out of the rendered configuration.
	discovery, err := labels.Parse(objects.ConfigMap.Data[modelDeploymentRouterSelectorKey])
	require.NoError(t, err)

	// THE MEMBERS AS THE PASS LEFT THEM. The managed leader qualified and is a candidate; the
	// managed rank beside it did not; the custom leader answers and is one; the custom follower
	// does not answer and is not.
	member := func(name string, index int, eligible, candidate bool) map[string]string {
		labels := map[string]string{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: "qwen",
			modelDeploymentLabelKeyRoleKind: "server",
			modelDeploymentMemberIndexLabel: string(rune('0' + index)),
		}
		if eligible {
			labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		}
		if candidate {
			labels[modelDeploymentRouterCandidateLabel] = modelDeploymentEndpointEligibleValue
		}

		return labels
	}
	members := map[string]map[string]string{
		"M0": member("M0", 0, true, true),
		"M1": member("M1", 1, false, false),
		"P0": member("P0", 0, false, true),
		"P1": member("P1", 1, false, false),
	}

	var admitted, refused []string
	for name, memberLabels := range members {
		if discovery.Matches(labels.Set(memberLabels)) {
			admitted = append(admitted, name)
		} else {
			refused = append(refused, name)
		}
	}
	slices.Sort(admitted)
	slices.Sort(refused)

	assert.Equal(t, []string{"M0", "P0"}, admitted,
		"the published discovery admits exactly the members the pass marked")
	assert.Equal(t, []string{"M1", "P1"}, refused,
		"and refuses the members it did not")
}
