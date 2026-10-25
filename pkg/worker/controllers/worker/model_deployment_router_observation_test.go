package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// The JSON contract the two observer patches serve (DESIGN §1.2/§1.3). One healthy
// payload every other test derives from.
const observerHealthyPayload = `{
  "router": {"boot_generation": 1700000000000, "now_ms": 1700000001000},
  "registry_revision": 7,
  "workers": [
    {
      "url": "http://10.0.0.5:8000",
      "port": "8000",
      "role": "regular",
      "pod_hint": "10.0.0.5",
      "worker_id": "wid-1",
      "selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true}
    },
    {
      "url": "http://10.0.0.6:8000",
      "port": "8000",
      "role": "prefill",
      "pod_hint": "10.0.0.6",
      "worker_id": "wid-2",
      "selection": {"registered": true, "healthy": false, "circuit_open": true, "in_selection": false}
    }
  ]
}`

// observationMemberPort is the engine port a bound worker URL names. It is a fixture constant
// because the payload and the Pod's own declared ports have to agree: the binder refuses a URL
// whose port the bound Pod never declared, which is the check that stops a row being counted
// against a listener that member is not serving.
const observationMemberPort int32 = 8000

// observationLivePod builds a member Pod the binder can place: the workload identity labels the
// render stamps on every member, and the declared listener its metrics URL names.
//
// THE IDENTITY IS NOT OPTIONAL IN A FIXTURE. The binder derives the role, the member rank and the
// listener from the bound Pod alone, and refuses an endpoint that carries none of them, so a Pod
// without the rendered labels exercises the refusal rather than the count.
func observationLivePod(name, ip string, uid types.UID, running bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			UID:       uid,
			Labels: map[string]string{
				modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:  "qwen",
				modelDeploymentLabelKeyComponent: "regular",
				modelDeploymentMemberIndexLabel:  "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "engine",
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: observationMemberPort}},
		}}},
		Status: corev1.PodStatus{PodIP: ip},
	}
	// The controlling owner is what makes this Pod one of the deployment's members, and the
	// collection's own fresh listing filters on exactly this reference. A member with no owner is
	// not counted by the cache either, so a fixture without it would measure an empty pool.
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: workercore.SchemeGroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "qwen",
		UID:        "md-uid",
		Controller: ptr.To(true),
	}}
	if running {
		pod.Status.Phase = corev1.PodRunning
	} else {
		pod.Status.Phase = corev1.PodFailed
	}

	return pod
}

// seedRouterProcess gives a deployment a Router process whose live controlling chain the
// collection can verify.
//
// THE ROUTER DEPLOYMENT IS THE ONE THE RECONCILE ALREADY RENDERED, because the chain is built
// around that object rather than around a second Deployment of the same name: the collection
// verifies every hop live, and a Deployment of the same name under another identity is a process
// it cannot place.
func seedRouterProcess(
	t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment, name, ip string,
) {
	t.Helper()

	routerDeployment := new(appsv1.Deployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, routerDeployment))
	controller := true
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-rs", Namespace: md.Namespace, UID: types.UID("rs-uid-" + name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment",
				Name: routerDeployment.Name, UID: routerDeployment.UID, Controller: &controller,
			}},
		},
	}
	routerPod := retirementRouterPod(name, ip)
	routerPod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "ReplicaSet",
		Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controller,
	}}
	require.NoError(t, cli.Create(context.Background(), replicaSet))
	require.NoError(t, cli.Create(context.Background(), routerPod))
}

// TestTheRealCollectorDrivesTheRestoreAcrossAnEmptyAndAPopulatedRegistry drives the real collector
// and the real label writer over one group.
//
// A COHERENT EMPTY REGISTRY IS NOT A FAULT. The Router says it is not serving this group, which is
// a fact about the Router and not a verdict on the group, and it must not block a restore: a
// deployment whose Router has not registered its worker yet would otherwise never come up.
//
// THE POPULATED REGISTRY IS THE SAME GROUP SEEN AGAIN, and it restores what the carried label alone
// could not prove: the member's eligibility generation annotation is removed by hand before that
// pass, so the group is no longer retained, and the populated registry is what puts the annotation
// back.
//
// BOTH FACTS ARE READ BY THE PRODUCTION COLLECTOR, so a case that built the answer by hand would be
// proving the shape of an answer rather than the collection of it.
func TestTheRealCollectorDrivesTheRestoreAcrossAnEmptyAndAPopulatedRegistry(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	// THE MEMBERS ARRIVE OVER PASSES, because a group is composed and its members admitted one at a
	// time. A case that wanted one member would not notice, and the retention rules differ between
	// a leader-only group and a collective one.
	endpointEligibilityTestSeed(t, cli, md)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	setEndpointEligibilityPodsReady(t, cli, true)
	require.Len(t, replicaPods(t, cli), 2)

	// THE ROUTER IS DECLARED AFTER THE GROUP IS STANDING, so the Router's own objects are rendered
	// by a real pass and the process below has a rendered Deployment to be owned by. The stored
	// object is read first, because the passes above moved it.
	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name}, stored))
	stored.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), stored))
	md.Spec.Router = stored.Spec.Router
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// THE KNOWN STARTING STATE: THE GROUP CARRIES NO ELIGIBILITY, so a label written below was
	// written by the pass under test.
	setEndpointEligibilityLabelOnPods(t, cli, false)

	// THE ROUTER PROCESS AND ITS CHAIN, because the collection verifies every hop of it through the
	// uncached reader before it reads a byte, and holds a process it cannot place.
	seedRouterProcess(t, cli, md, "router-zero", "10.0.9.1")

	// THE DEPLOYMENT'S OWN PODS. The Router process is discovered by the collection but is not one
	// of them: it is owned by the workload Deployment, not by the deployment, so it never enters a
	// group's membership.
	members := func() []*corev1.Pod {
		live := replicaPods(t, cli)
		owned := make([]*corev1.Pod, 0, len(live))
		for i := range live {
			owned = append(owned, &live[i])
		}

		return owned
	}

	// THE MEMBERS' ADDRESSES, because the collection binds an observed worker to the Pod its address
	// resolves to, and refuses a view it cannot bind.
	for i, pod := range members() {
		pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		pod.Status.Phase = corev1.PodRunning
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}

	now := time.Now()
	serving := []string{}
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock: func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			live := members()
			values := make([]corev1.Pod, 0, len(live))
			for _, pod := range live {
				values = append(values, *pod)
			}

			return retirementRouterView(serving, values), nil
		},
	}

	pods := members()
	require.Len(t, pods, 2)
	uids := make([]string, 0, len(pods))
	for _, pod := range pods {
		uids = append(uids, string(pod.UID))
	}
	slices.Sort(uids)
	generation := modelDeploymentMembershipGeneration(strings.Join(uids, "/"))
	key := modelDeploymentGroupKey{Role: "server", Ordinal: 0, Generation: generation}

	write := func(observation modelDeploymentPassObservation) {
		t.Helper()

		live := members()
		qualification := modelDeploymentTestQualification(live, generation, false)
		byMember := make(map[types.UID]modelDeploymentInstanceQualification, len(live))
		actual := make([]corev1.Pod, 0, len(live))
		for _, pod := range live {
			byMember[pod.UID] = qualification
			actual = append(actual, *pod)
		}
		require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
			context.Background(), md, actual, byMember, nil, observation))
	}
	leaderOf := func() *corev1.Pod {
		t.Helper()

		for _, pod := range members() {
			if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
				return pod
			}
		}

		return nil
	}

	// THE FIRST COLLECTION: THE REGISTRY IS EMPTY, AND THAT IS A FACT ABOUT THE ROUTER.
	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), md),
	}
	fact, observed := observation.Serving.Groups[key]
	require.True(t, observed, "the collector read the group")
	assert.True(t, fact.SnapshotValid, "a coherent zero is not a fault")
	assert.Equal(t, workercore.ModelDeploymentServingStateConfirmed, fact.State, fact.InvalidReason)
	assert.EqualValues(t, 0, fact.Endpoints, "the router holds none of the group's endpoints")

	write(observation)

	// THE COHERENT ZERO RESTORED THE GROUP.
	leader := leaderOf()
	require.NotNil(t, leader)
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		leader.Labels[modelDeploymentLabelKeyEndpointEligible],
		"a coherent empty registry does not block a restore")
	assert.Equal(t, string(generation),
		leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
		"the restored member names the generation the label belongs to")

	// THE SECOND COLLECTION: THE SAME GROUP, POPULATED. The member's annotation is removed first, so
	// the group is no longer retained and the populated registry is what restores it.
	delete(leader.Annotations, modelDeploymentAnnotationKeyEndpointEligibleGeneration)
	require.NoError(t, cli.Update(context.Background(), leader))
	serving = []string{string(leader.UID)}
	observation = modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), md),
	}
	fact, observed = observation.Serving.Groups[key]
	require.True(t, observed)
	assert.True(t, fact.SnapshotValid, "the router holds the group's whole answering set")
	assert.Equal(t, workercore.ModelDeploymentServingStateConfirmed, fact.State, fact.InvalidReason)
	assert.EqualValues(t, 1, fact.Endpoints, "the router holds the group's one expected endpoint")
	assert.False(t, fact.Retained, "the annotation is gone, so the group is no longer retained")

	write(observation)

	leader = leaderOf()
	require.NotNil(t, leader)
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		leader.Labels[modelDeploymentLabelKeyEndpointEligible],
		"a populated registry keeps the group restored")
	assert.Equal(t, string(generation),
		leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
		"the populated registry restores the annotation the carried label could not prove")
}

// routerListFailureClient refuses to list the Router processes, so the collection loses the
// Router's side of the answer while the deployment's own membership is still read fresh.
//
// IT FAILS ONLY THE ROUTER LISTING. The membership listing is left alone, so the case has the
// fresh members the retention rests on.
type routerListFailureClient struct {
	ctrlcli.Reader
}

func (c *routerListFailureClient) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	if modelDeploymentListSelectsRouters(opts) {
		return fmt.Errorf("the router's pods could not be listed: the apiserver refused")
	}

	return c.Reader.List(ctx, list, opts...)
}

// memberListFailureClient refuses to list the deployment's own members, so the collection has no
// fresh membership to judge a group on.
//
// IT FAILS ONLY THE MEMBERSHIP LISTING. The Router listing is left alone, so the case fails
// exactly where the evidence the retention rests on is missing.
type memberListFailureClient struct {
	ctrlcli.Reader
}

func (c *memberListFailureClient) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	if !modelDeploymentListSelectsRouters(opts) {
		return fmt.Errorf("the deployment's pods could not be listed uncached: the apiserver refused")
	}

	return c.Reader.List(ctx, list, opts...)
}

// modelDeploymentListSelectsRouters reports whether a listing is the collection's Router listing.
//
// THE ROUTER LISTING IS THE ONE THAT REQUIRES THE ROUTER LABEL, and every other listing in the
// collection asks for the deployment's own members by their identity labels alone.
func modelDeploymentListSelectsRouters(opts []ctrlcli.ListOption) bool {
	options := &ctrlcli.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(options)
	}

	_, requiresRouter := options.LabelSelector.RequiresExactMatch(modelDeploymentRouterLabelKey)

	return requiresRouter
}

// nthListFailureClient refuses the Nth Pod listing of one kind, so a case can arrange that an
// earlier read succeeded and a later one failed.
//
// THE COUNT IS OVER THE POD LISTINGS OF ONE KIND, because the collection lists the deployment's
// members twice and the Router processes three times. The first member listing is the collection's
// own fresh read and the second is the closing bookend's; the first Router listing is the opening
// bookend's, the second is the collection's own, and the third is the closing bookend's.
//
// A CLIENT THAT REFUSED EVERY LISTING OF ITS KIND COULD NOT EXPRESS THE CASES THAT MATTER HERE,
// because the closing bookend is the one that has to fail after the evidence was already read.
type nthListFailureClient struct {
	ctrlcli.Reader
	routers bool
	failAt  int
	seen    int
}

func (c *nthListFailureClient) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	if _, isPod := list.(*corev1.PodList); !isPod {
		return c.Reader.List(ctx, list, opts...)
	}
	if modelDeploymentListSelectsRouters(opts) != c.routers {
		return c.Reader.List(ctx, list, opts...)
	}
	c.seen++
	if c.seen == c.failAt {
		return fmt.Errorf("the apiserver refused the listing")
	}

	return c.Reader.List(ctx, list, opts...)
}

// unreadyMemberClient reports one member as not ready on the uncached read, while the caller's
// cached copy of the same Pod still says ready.
//
// THE TWO READS DISAGREE ON PURPOSE. The readiness the qualification reads is the caller's, and
// the readiness the retention is proved by is the fresh one, so a case that wants the fresh word to
// dominate has to make the two disagree.
type unreadyMemberClient struct {
	ctrlcli.Reader
	uid types.UID
}

func (c *unreadyMemberClient) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	if err := c.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	pods, isPod := list.(*corev1.PodList)
	if !isPod {
		return nil
	}
	for i := range pods.Items {
		if pods.Items[i].UID != c.uid {
			continue
		}
		pods.Items[i].Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
	}

	return nil
}

// qualifiedGroupFixture builds a deployment whose single group is already qualified at its current
// membership generation, with the Router it would be served through.
//
// THE GROUP IS QUALIFIED BY HAND, because the subject of every case here is what a collection that
// failed does to a group that was already qualified, and a group the operator qualified on its own
// is the only honest starting point for that.
type qualifiedGroupFixture struct {
	md         *workercore.ModelDeployment
	cli        ctrlcli.Client
	generation modelDeploymentMembershipGeneration
	key        modelDeploymentGroupKey
	members    []*corev1.Pod
}

func newQualifiedGroupFixture(t *testing.T) qualifiedGroupFixture {
	t.Helper()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	setEndpointEligibilityPodsReady(t, cli, true)
	require.Len(t, replicaPods(t, cli), 2)

	// THE ROUTER IS DECLARED AND RENDERED, so the collection has a process it would read.
	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name}, stored))
	stored.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), stored))
	md.Spec.Router = stored.Spec.Router
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	seedRouterProcess(t, cli, md, "router-zero", "10.0.9.1")

	members := func() []*corev1.Pod {
		live := replicaPods(t, cli)
		owned := make([]*corev1.Pod, 0, len(live))
		for i := range live {
			owned = append(owned, &live[i])
		}

		return owned
	}
	for i, pod := range members() {
		pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		pod.Status.Phase = corev1.PodRunning
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}

	live := members()
	require.Len(t, live, 2)
	uids := make([]string, 0, len(live))
	for _, pod := range live {
		uids = append(uids, string(pod.UID))
	}
	slices.Sort(uids)
	generation := modelDeploymentMembershipGeneration(strings.Join(uids, "/"))

	var leader *corev1.Pod
	for _, pod := range live {
		if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			leader = pod
		}
	}
	require.NotNil(t, leader, "the group has a leader")
	leader.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
	leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = string(generation)
	require.NoError(t, cli.Update(context.Background(), leader))

	return qualifiedGroupFixture{
		md:         md,
		cli:        cli,
		generation: generation,
		key:        modelDeploymentGroupKey{Role: "server", Ordinal: 0, Generation: generation},
		members:    live,
	}
}

// leaderName reads the name of the group's leader, which is the member a case has to make unready
// when it wants the fresh readiness to contradict the cached one.
func (f qualifiedGroupFixture) leaderName(t *testing.T) string {
	t.Helper()

	for _, pod := range f.members {
		if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			return pod.Name
		}
	}

	t.Fatal("the group has no leader")

	return ""
}

// write runs the eligibility write the way the reconciler runs it, with the group's members
// qualified and the observation the collection produced.
func (f qualifiedGroupFixture) write(
	t *testing.T, r *ModelDeploymentReconciler, observation modelDeploymentPassObservation,
) {
	t.Helper()

	qualification := modelDeploymentTestQualification(f.members, f.generation, false)
	byMember := make(map[types.UID]modelDeploymentInstanceQualification, len(f.members))
	actual := make([]corev1.Pod, 0, len(f.members))
	for _, pod := range f.members {
		byMember[pod.UID] = qualification
		actual = append(actual, *pod)
	}
	require.NoError(t, r.convergeModelDeploymentEndpointEligibility(
		context.Background(), f.md, actual, byMember, nil, observation))
}

// leaderLabel reads the eligibility key off the group's leader.
func (f qualifiedGroupFixture) leaderLabel(t *testing.T) string {
	t.Helper()

	for _, pod := range replicaPods(t, f.cli) {
		if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			return pod.Labels[modelDeploymentLabelKeyEndpointEligible]
		}
	}

	return ""
}

// leaderUID reads the UID of the group's leader, which is the member a case has to make unready
// when it wants the fresh readiness to contradict the cached one.
func (f qualifiedGroupFixture) leaderUID(t *testing.T) types.UID {
	t.Helper()

	for _, pod := range replicaPods(t, f.cli) {
		if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			return pod.UID
		}
	}

	t.Fatal("the group has no leader")

	return ""
}

// TestACollectionThatCannotListTheRoutersKeepsTheFreshlyQualifiedGroup pins the retention the
// spec promises, at the collector rather than at the decision that reads the collector's answer.
//
// A COLLECTION THAT LOSES THE ROUTER HAS NOT LOST THE GROUP. The group's members were read fresh
// before the collection failed, every one of them carries the eligibility label, and the
// eligibility annotation on them names the group's current complete membership generation: this
// is the group that was qualified, and the loss is the Router's.
//
// THE ANSWER STILL CARRIES THE GROUP'S FACT, and the fact is what holds the group open. An answer
// that carries no fact at all knows nothing, and nothing holds a group.
func TestACollectionThatCannotListTheRoutersKeepsTheFreshlyQualifiedGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &routerListFailureClient{Reader: fixture.cli},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	require.True(t, observed,
		"the collection lost the Router, not the group: the members were read fresh")
	assert.False(t, fact.SnapshotValid, "no view was read, so the snapshot is not valid")
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, fact.State, fact.InvalidReason)
	assert.True(t, fact.Retained,
		"every member carries the label and the annotation names the current generation")
	assert.EqualValues(t, 0, fact.Endpoints, "a lost view confirms no endpoint count")

	// THE WRITE KEEPS THE KEY. The group was qualified before the loss and nothing has been found
	// wrong with it since, so the key is left exactly as it was.
	fixture.write(t, r, observation)
	assert.Equal(t, modelDeploymentEndpointEligibleValue, fixture.leaderLabel(t),
		"a group that was already qualified keeps its eligibility while the collection is lost")
}

// TestACollectionThatCannotListTheMembersClosesTheGroup pins the other half of the rule: a
// collection that cannot read the deployment's own members has no evidence about the group at all.
//
// IT IS NOT AN OBSERVATION LOSS. Nothing was observed and nothing is known to have changed, and
// the difference matters: a group that is merely unobserved keeps what it had, while a group this
// operator cannot place is closed until a collection can place it.
func TestACollectionThatCannotListTheMembersClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &memberListFailureClient{Reader: fixture.cli},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	_, observed := observation.Serving.Groups[fixture.key]
	assert.False(t, observed,
		"a collection with no fresh membership knows nothing about the group, and a fact is what "+
			"holds a group open")

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a group this operator cannot place is closed until a collection can place it")
}

// TestACollectionThatProvesTheMembershipMovedClosesTheGroup is the control for the retention: a
// collection that PROVES the group moved closes it, because a group that is no longer the group
// that was qualified must not keep its eligibility.
//
// THE PROOF IS A CHANGE UNDER THE COLLECTION, made from inside the fetch, because that is the
// window the guard exists to close. The members are read fresh, so the freshness alone proves
// nothing: what closes the group is that the membership the guard bookends is not the membership
// the group was qualified under.
func TestACollectionThatProvesTheMembershipMovedClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: fixture.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock: func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			// THE MEMBERSHIP MOVES UNDER THE COLLECTION: a member leaves while the views are read.
			// The follower goes, so the group keeps its leader and loses a member.
			for _, pod := range fixture.members {
				if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
					continue
				}
				err := fixture.cli.Delete(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
				})
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
			}

			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	if observed {
		assert.False(t, fact.Retained,
			"a group whose membership moved is not the group that was qualified")
	}

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a group whose membership moved is withdrawn rather than kept on the old generation")
}

// TestACollectionThatProvesTheDeploymentMovedClosesTheGroup is the second control: a collection
// that proves the deployment itself moved closes the group, because the eligibility rule derives
// from the deployment's own spec.
func TestACollectionThatProvesTheDeploymentMovedClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: fixture.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock: func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			// THE DEPLOYMENT MOVES UNDER THE COLLECTION: its generation changes while the views
			// are read.
			live := new(workercore.ModelDeployment)
			require.NoError(t, fixture.cli.Get(context.Background(),
				ctrlcli.ObjectKeyFromObject(fixture.md), live))
			live.Generation++
			require.NoError(t, fixture.cli.Update(context.Background(), live))

			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	if observed {
		assert.False(t, fact.Retained,
			"a group whose deployment moved is not the group that was qualified")
	}

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a group whose deployment moved is withdrawn rather than kept on the old generation")
}

// TestACollectionThatLostTheClosingReadWithoutAChangeKeepsTheFreshlyQualifiedGroup is the
// positive control for the closing bookend.
//
// THE CLOSING BOOKEND EXISTS TO PROVE A CHANGE, and a read that failed proves nothing. When
// nothing moved and the closing read could not be read, the group keeps what it had: the fresh
// membership is in hand and it says the group is still the group that was qualified.
//
// IT IS THE CONTROL FOR THE TWO CASES BESIDE IT. A rule that closed a group because a later read
// failed would close this one too, and this one must not close.
func TestACollectionThatLostTheClosingReadWithoutAChangeKeepsTheFreshlyQualifiedGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		// THE THIRD ROUTER LISTING FAILS: the first is the opening bookend's, the second is the
		// collection's own, and the third is the closing bookend's.
		Client: fixture.cli, APIReader: &nthListFailureClient{
			Reader: fixture.cli, routers: true, failAt: 3,
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	require.True(t, observed, "the collection lost the closing read, not the group")
	assert.True(t, fact.Retained,
		"nothing moved, so the group is still the group that was qualified")

	fixture.write(t, r, observation)
	assert.Equal(t, modelDeploymentEndpointEligibleValue, fixture.leaderLabel(t),
		"a closing read that proved nothing leaves the group as it was")
}

// TestACollectionThatProvesTheDeploymentChangedAndThenLostTheMembershipClosesTheGroup is the
// first mixed case: the closing bookend learned that the deployment moved, and a later read then
// failed.
//
// A KNOWN CHANGE DOMINATES A LATER UNCERTAINTY. The deployment's own generation is the thing the
// eligibility rule derives from, the closing bookend read it, and a membership listing that failed
// afterwards does not unmake it. Closing the group is what the change requires, and the failure
// afterwards is only a failure.
func TestACollectionThatProvesTheDeploymentChangedAndThenLostTheMembershipClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		// THE SECOND MEMBER LISTING FAILS: the first is the collection's own fresh read and the
		// second is the closing bookend's.
		Client: fixture.cli, APIReader: &nthListFailureClient{
			Reader: fixture.cli, routers: false, failAt: 2,
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			// THE DEPLOYMENT MOVES UNDER THE COLLECTION, so the closing bookend reads a
			// generation the opening one did not see.
			live := new(workercore.ModelDeployment)
			require.NoError(t, fixture.cli.Get(context.Background(),
				ctrlcli.ObjectKeyFromObject(fixture.md), live))
			live.Generation++
			require.NoError(t, fixture.cli.Update(context.Background(), live))

			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	if observed {
		assert.False(t, fact.Retained,
			"a group whose deployment moved is not the group that was qualified")
	}

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"the change the closing bookend proved survives the read that failed after it")
}

// TestACollectionThatProvesTheMembershipChangedAndThenLostTheRoutersClosesTheGroup is the
// second mixed case: the closing bookend learned that the membership moved, and the Router
// listing then failed.
//
// THE MEMBERSHIP IS THE GROUP. A member that left under the collection means the group is not the
// group that was qualified, and the Router processes it would have been served through are a
// different question that was never answered.
func TestACollectionThatProvesTheMembershipChangedAndThenLostTheRoutersClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		// THE THIRD ROUTER LISTING FAILS, so the closing bookend reads the membership and then
		// cannot read the routers.
		Client: fixture.cli, APIReader: &nthListFailureClient{
			Reader: fixture.cli, routers: true, failAt: 3,
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			// THE MEMBERSHIP MOVES UNDER THE COLLECTION, so the closing bookend reads a
			// membership the opening one did not see.
			for _, pod := range fixture.members {
				if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
					continue
				}
				err := fixture.cli.Delete(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
				})
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
			}

			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	if observed {
		assert.False(t, fact.Retained,
			"a group whose membership moved is not the group that was qualified")
	}

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"the change the closing bookend proved survives the read that failed after it")
}

// TestACollectionThatReadAFreshlyUnreadyMemberClosesTheGroup is the third case: the fresh
// membership says a member is not ready, while the caller's cached copy of the same Pod still says
// ready.
//
// THE FRESH READ IS THE AUTHORITY. The qualification's own readiness legs read the caller's list,
// and a list that is behind the API server cannot say that a member is ready. A group with an
// unready member is not a group that may be served, and no acceptance path may open for it: not
// the retention, and not the fresh restore either.
func TestACollectionThatReadAFreshlyUnreadyMemberClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &routerListFailureClient{
			Reader: &unreadyMemberClient{Reader: fixture.cli, uid: fixture.leaderUID(t)},
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	require.True(t, observed, "the collection lost the Router, not the group")
	assert.False(t, fact.Retained,
		"a group with a member the fresh read says is not ready is not a group that may be served")
	assert.False(t, fact.SnapshotValid, "an unsafe group has no valid observation to restore from")
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, fact.State, fact.InvalidReason)
	assert.EqualValues(t, 0, fact.Endpoints, "unsafe evidence carries no endpoint count")
	assert.True(t, fact.MemberNotReady,
		"the fresh read found a member that is not ready, and a known fact is not a missing one")
	assert.Contains(t, fact.InvalidReason, fixture.leaderName(t),
		"the fact names the member the fresh read found not ready")

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a group with an unready member leaves the pool even while the collection is lost")
}

// TestACollectionThatConfirmedTheGroupButReadAFreshlyUnreadyMemberClosesTheGroup is the same
// unsafe evidence on the path where the collection SUCCEEDED.
//
// THE FRESH RESTORE BRANCH IS A SEPARATE ACCEPTANCE PATH. A collection that read the whole
// answering set confirms the Router's view of the group, and the restore that follows would open
// the group on that confirmation alone. The fresh membership's word about readiness still has to
// close it, because the cached list the qualification read cannot say that a member is ready.
func TestACollectionThatConfirmedTheGroupButReadAFreshlyUnreadyMemberClosesTheGroup(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &unreadyMemberClient{
			Reader: fixture.cli, uid: fixture.leaderUID(t),
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	require.True(t, observed, "the collection read the group")
	assert.False(t, fact.SnapshotValid,
		"a group with a member the fresh read says is not ready is not confirmed")
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, fact.State, fact.InvalidReason)
	assert.EqualValues(t, 0, fact.Endpoints, "unsafe evidence carries no endpoint count")
	assert.True(t, fact.MemberNotReady,
		"the fresh read found a member that is not ready, and a known fact is not a missing one")
	assert.Contains(t, fact.InvalidReason, fixture.leaderName(t),
		"the fact names the member the fresh read found not ready")

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a confirmed view of an unsafe group does not restore it")
}

// TestACollectionThatLostTheRouterDoesNotKeepAGroupThatWasNeverQualified is the other half of
// the retention rule: a group that was never qualified is not a group the retention can hold open.
//
// THE RETENTION IS ABOUT A GROUP THAT WAS QUALIFIED. A group whose members carry no eligibility
// label was never in the pool, and a collection that lost the Router does not put it there.
func TestACollectionThatLostTheRouterDoesNotKeepAGroupThatWasNeverQualified(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	// THE GROUP WAS NEVER QUALIFIED, so its members carry no eligibility label.
	for _, pod := range fixture.members {
		pod.Labels[modelDeploymentLabelKeyEndpointEligible] = ""
		require.NoError(t, fixture.cli.Update(context.Background(), pod))
	}

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &routerListFailureClient{Reader: fixture.cli},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observation := modelDeploymentPassObservation{
		Serving: r.observeModelDeploymentServing(context.Background(), fixture.md),
	}
	fact, observed := observation.Serving.Groups[fixture.key]
	require.True(t, observed, "the collection lost the Router, not the group")
	assert.False(t, fact.Retained, "a group that was never qualified is not retained")

	fixture.write(t, r, observation)
	assert.Empty(t, fixture.leaderLabel(t),
		"a group that was never qualified stays out of the pool")
}

// TestTheRealCollectorReadsTheMembershipItBindsAgainst pins that the group facts are computed
// against the membership the collection re-read, and that the Router process is not part of it.
//
// THE ROUTER PROCESS IS DISCOVERED BUT NEVER A MEMBER. It is owned by the workload Deployment rather
// than by the deployment, so it enters no group's membership and its UID enters no group's
// generation. A generation that named it would never be written on any Pod, and the retention rule
// that compares a member's annotation against the group's generation could then never hold.
func TestTheRealCollectorReadsTheMembershipItBindsAgainst(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)

	seedRouterProcess(t, cli, md, "router-zero", "10.0.9.1")

	live := replicaPods(t, cli)
	require.Len(t, live, 2)
	for i := range live {
		live[i].Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		live[i].Status.Phase = corev1.PodRunning
		require.NoError(t, cli.Status().Update(context.Background(), &live[i]))
	}

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock: func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return retirementRouterView(nil, live), nil
		},
	}

	answer := r.observeModelDeploymentServing(context.Background(), md)
	require.Len(t, answer.Groups, 1)
	for key, fact := range answer.Groups {
		assert.Equal(t, "server", key.Role)
		assert.Equal(t, 0, key.Ordinal)
		assert.True(t, fact.SnapshotValid, "the empty registry is coherent")
		assert.NotContains(t, string(key.Generation), "router-uid-router-zero",
			"the router process is not a member of any group")
		assert.EqualValues(t, 2, fact.Members, "the group holds its own two members")
	}
}

// patches serve parses fully, and anything else — an unknown field, a missing router
// block, an unsupported per-plugin dump — is an error the caller reports as an unusable
// view, never as an empty one.
func TestParseRouterObservationView(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		payload    string
		wantErr    string
		assertView func(t *testing.T, view RouterServingView)
	}{
		{
			name:    "the observer payload parses fully",
			payload: observerHealthyPayload,
			assertView: func(t *testing.T, view RouterServingView) {
				assert.Equal(t, uint64(1700000000000), view.RouterBootGeneration)
				assert.Equal(t, uint64(7), view.RegistryRevision)
				require.Len(t, view.Workers, 2)
				assert.Equal(t, "http://10.0.0.5:8000", view.Workers[0].URL)
				assert.Equal(t, "8000", view.Workers[0].Port)
				assert.True(t, view.Workers[0].Selection.InSelection)
				assert.False(t, view.Workers[1].Selection.Healthy)
				assert.True(t, view.Workers[1].Selection.CircuitOpen)
				assert.False(t, view.Workers[1].Selection.InSelection)
			},
		},
		{
			name: "an unknown field rejects the payload instead of ignoring it",
			payload: `{
				"router": {"boot_generation": 1, "now_ms": 2},
				"registry_revision": 1,
				"workers": [],
				"timestamp": "2026-10-02T00:00:00Z"
			}`,
			wantErr: "unknown field",
		},
		{
			name: "a per-plugin debug dump shape is not a membership view",
			payload: `{
				"timestamp": "2026-10-02T00:00:00Z",
				"plugins": {"picker": {"name": "picker", "message": "plugin does not support state collection"}}
			}`,
			wantErr: "unknown field",
		},
		{
			name: "a payload with no router block is rejected, never read as generation zero",
			payload: `{
				"registry_revision": 1,
				"workers": []
			}`,
			wantErr: "boot_generation",
		},
		{
			name: "the gateway view with its operational fields parses as served",
			payload: `{
				"router": {"boot_generation": 9, "now_ms": 10},
				"registry_revision": 3,
				"workers": [
					{
						"url": "http://10.0.0.7:8000",
						"port": "8000",
						"role": "regular",
						"pod_hint": "10.0.0.7",
						"worker_id": "wid-9",
						"selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true},
						"lane": {"busy": false, "waiting": 0},
						"last_job": {"job_type": "add", "worker_url": "http://10.0.0.7:8000", "status": "success", "message": null, "timestamp": 1700000000}
					}
				]
			}`,
			assertView: func(t *testing.T, view RouterServingView) {
				require.Len(t, view.Workers, 1)
				require.NotNil(t, view.Workers[0].Lane)
				assert.False(t, view.Workers[0].Lane.Busy)
				assert.Equal(t, 0, view.Workers[0].Lane.Waiting)
				assert.Contains(t, string(view.Workers[0].LastJob), `"status": "success"`,
					"the raw job status is carried as the gateway wrote it")
			},
		},
		{
			name: "an explicit empty worker array is a valid view of an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": []
			}`,
			assertView: func(t *testing.T, view RouterServingView) {
				assert.NotNil(t, view.Workers,
					"an explicit empty array is present membership, not absent membership")
				assert.Empty(t, view.Workers)
			},
		},
		{
			// The routers serialize a non-Option Vec, so the array is always on the wire.
			// Its absence is therefore a payload this operator cannot read, and an empty
			// membership claimed by a payload that never stated one is exactly the
			// fabricated zero the answer must never be.
			name: "an omitted workers array is rejected, never read as an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2
			}`,
			wantErr: "workers",
		},
		{
			name: "a null workers array is rejected, never read as an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": null
			}`,
			wantErr: "workers",
		},
		{
			name: "a workers value that is not an array is rejected as malformed",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": {"url": "http://10.0.0.5:8000"}
			}`,
			wantErr: "membership view",
		},
		{
			name: "a workers entry that is not an object is rejected as malformed",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": ["http://10.0.0.5:8000"]
			}`,
			wantErr: "membership view",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			view, err := parseRouterObservationView([]byte(tc.payload))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, strings.ToLower(err.Error()), tc.wantErr)

				return
			}
			require.NoError(t, err)
			tc.assertView(t, view)
		})
	}
}

// TestBindRouterObservationView pins collection-time identity binding: a serving worker is
// bound to the live Pod its address resolves to, the binding follows the live Pod object
// rather than the URL's history, and an address that resolves to nothing live makes the
// whole view indeterminate.
func TestBindRouterObservationView(t *testing.T) {
	t.Parallel()

	livePod := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)

	testCases := []struct {
		name      string
		view      RouterServingView
		pods      []*corev1.Pod
		wantBound []ServingEndpoint
		wantErr   string
	}{
		{
			name: "a serving worker binds to the live pod at its address",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true, Registered: true, Healthy: true},
				},
			}},
			pods: []*corev1.Pod{livePod},
			wantBound: []ServingEndpoint{{
				PodUID: "pod-uid-1", Member: "server-0", Role: "regular", Rank: "0",
				Listener: observationMemberPort,
				URL:      "http://10.0.0.5:8000", Port: "8000", Serving: true,
			}},
		},
		{
			name: "the binding follows the live pod, so a same-name replacement binds to the new UID",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods: []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-new", true)},
			wantBound: []ServingEndpoint{{
				PodUID: "pod-uid-new", Member: "server-0", Role: "regular", Rank: "0",
				Listener: observationMemberPort,
				URL:      "http://10.0.0.5:8000", Port: "8000", Serving: true,
			}},
		},
		{
			name: "an address no live pod holds makes the view indeterminate",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.9:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods:    []*corev1.Pod{livePod},
			wantErr: "indeterminate",
		},
		{
			name: "an address whose only pod is not running makes the view indeterminate",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods:    []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-1", false)},
			wantErr: "indeterminate",
		},
		{
			name: "a worker outside the selection is never bound and never fails the view",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.9:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: false},
				},
			}},
			pods:      []*corev1.Pod{livePod},
			wantBound: []ServingEndpoint{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bound, _, err := bindRouterObservationView(tc.view, tc.pods, false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBound, bound)
		})
	}
}

// TestAggregateModelDeploymentServing pins the Feature 2 state machine: the count is the
// deduplicated union across complete views, replicas that disagree are NotConverged, any
// missing/stale/indeterminate view is Unknown, an unconfigured router is NotConfigured, and
// a confirmed zero is a real explicit zero.
func TestAggregateModelDeploymentServing(t *testing.T) {
	t.Parallel()

	fresh := time.Now()
	freshness := 30 * time.Second

	// viewOf builds one Router process's observation. The router UID is explicit in every
	// case, because boot generation is a per-process property: two replicas started at
	// different times are expected to report different generations, and a case that means
	// "two independent routers" has to say so rather than leave it to a bare generation.
	viewOf := func(routerUID types.UID, gen uint64, at time.Time, serving bool, uids ...string) RouterServingObservation {
		view := RouterServingView{RouterPodUID: routerUID, RouterBootGeneration: gen}
		bound := make([]ServingEndpoint, 0, len(uids))
		for _, uid := range uids {
			bound = append(bound, ServingEndpoint{PodUID: types.UID(uid), Serving: serving})
		}

		return RouterServingObservation{View: view, CollectedAt: at, Bound: bound}
	}

	testCases := []struct {
		name         string
		configured   bool
		observations []RouterServingObservation
		freshness    time.Duration
		wantState    workercore.ModelDeploymentServingState
		wantValue    *int32
	}{
		{
			// The two views are two replicas, so they carry distinct identities; their
			// generations are held equal here so this case asserts the union rule alone.
			// The generation rule is asserted separately below.
			name:       "the union deduplicates endpoints two views both hold",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a", "pod-b"),
				viewOf("router-b", 1, fresh, true, "pod-b", "pod-c"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(3)),
		},
		{
			name:       "views that disagree about one endpoint are NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a"),
				viewOf("router-b", 1, fresh, false, "pod-a"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name:         "a configured router with no collected view is Unknown, never zero",
			configured:   true,
			observations: []RouterServingObservation{},
			freshness:    freshness,
			wantState:    workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "an unconfigured router is NotConfigured with no value",
			configured: false,
			wantState:  workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name:       "one unusable view makes the answer Unknown",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a"),
				{Err: errors.New("dial failed")},
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "a stale view makes the answer Unknown",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh.Add(-2*freshness), true, "pod-a"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			// Boot generation is the Router process's start time, so two replicas started
			// at different times are expected to differ. Comparing one scalar across
			// distinct processes made every multi-replica deployment permanently
			// NotConverged, so withdrawal could never confirm.
			name:       "independent router processes with their own boot generations converge",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-b", 1700000009000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(2)),
		},
		{
			name:       "two reads of one router process agreeing on its generation converge",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-a", 1700000000000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(2)),
		},
		{
			// A Router that rolled mid-observation still means the views describe two
			// different processes, so the same process identity reporting two generations
			// is the case that has to stay NotConverged.
			name:       "one router identity reporting two generations is NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-a", 1700000009000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			// Identity-less views cannot be placed in any process group, so they are
			// held to one another: a disagreement among them still means two processes,
			// and dropping the check would read a roll as convergence.
			name:       "views whose router identity is unknown must still agree with each other",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("", 1, fresh, true, "pod-a"),
				viewOf("", 2, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name:       "a gateway-shaped view parses, binds and aggregates like any other",
			configured: true,
			observations: func() []RouterServingObservation {
				view, err := parseRouterObservationView([]byte(`{
					"router": {"boot_generation": 9, "now_ms": 10},
					"registry_revision": 3,
					"workers": [
						{
							"url": "http://10.0.0.5:8000",
							"port": "8000",
							"role": "regular",
							"pod_hint": "10.0.0.5",
							"worker_id": "wid-9",
							"selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true},
							"lane": {"busy": false, "waiting": 0},
							"last_job": {"job_type": "add", "worker_url": "http://10.0.0.5:8000", "status": "success", "message": null, "timestamp": 1700000000}
						}
					]
				}`))
				if err != nil {
					panic(err)
				}

				return []RouterServingObservation{{
					View:        view,
					CollectedAt: fresh,
					Bound: []ServingEndpoint{{
						PodUID: "pod-a", Member: "server-0", Role: "regular",
						URL: "http://10.0.0.5:8000", Port: "8000", Serving: true,
					}},
				}}
			}(),
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(1)),
		},
		{
			name:       "a confirmed zero is the explicit empty set",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(0)),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := aggregateModelDeploymentServing(tc.configured, tc.observations, tc.freshness)
			assert.Equal(t, tc.wantState, answer.State)
			assert.Equal(t, tc.wantValue, answer.Value)
		})
	}
}

// TestCollectModelDeploymentServingView pins the collection boundary: the view is read
// directly from one router pod's observer endpoint over the passed fetch, a transport or
// schema failure is that view's error (never an empty success), and the collected view
// carries the router pod's UID as its identity.
func TestCollectModelDeploymentServingView(t *testing.T) {
	t.Parallel()

	t.Run("a served payload binds to the target pod", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(observerHealthyPayload))
		}))
		defer server.Close()

		// The test server stands in for the router pod's observer port; the fetch function
		// the production code passes resolves the pod IP to the server's address.
		view, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return []byte(observerHealthyPayload), nil
			},
			"router-pod-uid",
		)
		require.NoError(t, err)
		assert.Equal(t, types.UID("router-pod-uid"), view.View.RouterPodUID)
		assert.Len(t, view.View.Workers, 2)
		_ = server.URL
	})

	t.Run("a transport failure is the view's error, never an empty success", func(t *testing.T) {
		t.Parallel()

		view, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return nil, errors.New("connection refused")
			},
			"router-pod-uid",
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connection refused")
		assert.True(t, view.Empty())
	})

	t.Run("a served non-JSON payload is the view's error", func(t *testing.T) {
		t.Parallel()

		_, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return []byte("<html>not the observer</html>"), nil
			},
			"router-pod-uid",
		)
		require.Error(t, err)
	})
}

// TestOneCollectionBoundsEveryRouterUnderOneDeadline covers the budget itself, which is the part
// that cannot be checked by looking at a single call.
//
// EACH READ CARRIES THE SAME CONTEXT. The collection derives one deadline and every Router's fetch
// is made under it, so a second Router cannot start fresh and a slow first one cannot be paid for
// twice. A caller's own deadline is kept when it has one, because the collection is part of a
// larger pass and must not outlive it.
//
// THE BOUND A READ GETS IS ITS OWN. An unlimited caller is given the per-fetch deadline and not the
// collection's budget, and a caller's deadline that is earlier still wins over that one.
func TestOneCollectionBoundsEveryRouterUnderOneDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		caller     func() (context.Context, context.CancelFunc)
		wantBound  bool
		wantWithin time.Duration
	}{
		{
			name:       "an unlimited caller is still given a finite deadline",
			caller:     func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			wantBound:  true,
			wantWithin: modelDeploymentServingFetchTimeout,
		},
		{
			name: "a caller's own earlier deadline is kept",
			caller: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Second)
			},
			wantBound:  true,
			wantWithin: time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.caller()
			defer cancel()

			before := time.Now()
			readCtx, stop := modelDeploymentObservationContext(ctx)
			defer stop()

			_, bounded := readCtx.Deadline()
			require.Equal(t, tc.wantBound, bounded, "an observation read must never be unbounded")
			deadline, ok := readCtx.Deadline()
			require.True(t, ok)
			assert.True(t, deadline.After(before), "the deadline is in the future")
			assert.True(t, deadline.Before(before.Add(modelDeploymentServingFreshness)),
				"and inside the window the aggregator will still call fresh")
			assert.True(t, deadline.Before(before.Add(tc.wantWithin+time.Second)),
				"and inside the bound this case names")
		})
	}
}

// TestAnUnlimitedCallerCannotOutliveItsReadBudget is the same rule from the other side: canceling
// what the caller passed has to stop the read, which is what makes a controller's own cancellation
// reach a collection that derived its own deadline.
func TestAnUnlimitedCallerCannotOutliveItsReadBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readCtx, stop := modelDeploymentObservationContext(ctx)
	defer stop()
	cancel()

	<-readCtx.Done()
	assert.ErrorIs(t, readCtx.Err(), context.Canceled)
}

// TestTheObserverTransportRefusesABodyItShouldNot carries covers the body bound with the real
// transport rather than a stubbed one.
//
// A Router that streams forever, or that answers with something enormous, is bounded here. The
// positive beside them is the ordinary case: a membership view is a few hundred workers and reads
// back unchanged.
func TestTheObserverTransportRefusesABodyItShouldNot(t *testing.T) {
	const view = `{"router":{"boot_generation":1700000000000,"now_ms":1700000001000},` +
		`"registry_revision":7,"workers":[]}`

	for _, tc := range []struct {
		name    string
		body    func(w http.ResponseWriter)
		wantErr string
	}{
		{
			name: "an ordinary membership view reads back unchanged",
			body: func(w http.ResponseWriter) { _, _ = w.Write([]byte(view)) },
		},
		{
			name: "a body past the cap is refused",
			body: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(view))
				chunk := make([]byte, 1<<20)
				for range 5 {
					_, _ = w.Write(chunk)
				}
			},
			wantErr: "byte bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.body(w)
			}))
			defer server.Close()

			ctx, cancel := modelDeploymentObservationContext(context.Background())
			defer cancel()

			raw, err := defaultServingViewFetch(ctx, server.URL)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, view, string(raw))
		})
	}
}

// TestTheObserverTransportBoundsOneFetch measures the deadline the transport puts on a single read.
//
// A COLLECTION IS A SUM OF ITS READS, so a read that inherits only the collection's budget can spend
// all of it on one Router that never answers. The transport bounds one read on its own, and the
// caller's own context still governs it: an earlier deadline wins over the per-fetch bound, and a
// canceled caller ends the read at once.
//
// THE READER ANSWERS LATE ON PURPOSE, so the deadline is the only thing that can end the request.
// Under a transport that waits for the collection's budget the first case reads the answer back
// instead of failing, and the case fails on the missing error.
func TestTheObserverTransportBoundsOneFetch(t *testing.T) {
	const lateAnswer = 3 * time.Second

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(lateAnswer)
		_, _ = w.Write([]byte(`{"router":{"boot_generation":1700000000000,"now_ms":1700000001000},` +
			`"registry_revision":7,"workers":[]}`))
	}))
	defer server.Close()

	for _, tc := range []struct {
		name      string
		caller    func() (context.Context, context.CancelFunc)
		wantErr   error
		wantShort bool
	}{
		{
			name:    "an unlimited caller is bounded by the per-fetch deadline",
			caller:  func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			wantErr: context.DeadlineExceeded,
		},
		{
			name: "a caller's earlier deadline wins over the per-fetch bound",
			caller: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 200*time.Millisecond)
			},
			wantErr:   context.DeadlineExceeded,
			wantShort: true,
		},
		{
			name: "a cancelled caller ends the read at once",
			caller: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, func() {}
			},
			wantErr:   context.Canceled,
			wantShort: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.caller()
			defer cancel()

			before := time.Now()
			_, err := defaultServingViewFetch(ctx, server.URL)
			elapsed := time.Since(before)

			require.Error(t, err, "a read that outlives its deadline must fail")
			assert.ErrorIs(t, err, tc.wantErr)
			if tc.wantShort {
				assert.Less(t, elapsed, modelDeploymentServingFetchTimeout,
					"the caller's own deadline ended the read")

				return
			}
			assert.GreaterOrEqual(t, elapsed, modelDeploymentServingFetchTimeout-time.Second/10,
				"the read waited for the per-fetch deadline")
			assert.Less(t, elapsed, 2*modelDeploymentServingFetchTimeout,
				"and not for the collection's budget, which is far longer")
		})
	}
}

// observationDeadlineTransport answers like a Router and records the deadline of every request, so
// the collection's shared budget can be observed rather than inferred.
type observationDeadlineTransport struct {
	mu        sync.Mutex
	deadlines []time.Time
	view      string
}

func (transport *observationDeadlineTransport) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	deadline, bounded := request.Context().Deadline()
	transport.mu.Lock()
	if bounded {
		transport.deadlines = append(transport.deadlines, deadline)
	} else {
		transport.deadlines = append(transport.deadlines, time.Time{})
	}
	transport.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(transport.view)),
		Header:     make(http.Header),
		Request:    request,
	}, nil
}

// TestEveryRouterOfOneCollectionSharesOneDeadline is the shared budget measured where it matters:
// on the requests the Routers actually received.
//
// A HELPER that derives a deadline proves nothing about the collection, because the collection could
// still call the transport with the caller's own context. Here two Router Pods are read through the
// production transport, and both requests must carry the same bounded deadline. A second Router that
// started fresh would be a pass whose length is the sum of the Routers' choices.
//
// THE CALLER'S DEADLINE IS EARLIER THAN THE PER-FETCH BOUND, so the deadline the requests carry is
// the collection's one. A read bounds itself as well, and a read that bounds itself carries its own
// deadline rather than the collection's, so the shared deadline is observed where it is still
// visible: under a caller whose own deadline is the earlier of the two. That bound is measured by
// TestTheObserverTransportBoundsOneFetch.
func TestEveryRouterOfOneCollectionSharesOneDeadline(t *testing.T) {
	md, _ := retirementRouterFixture(newRenderDeployment())
	md.Generation = 1
	routers := []*corev1.Pod{
		retirementRouterPod("router-zero", "10.0.9.1"),
		retirementRouterPod("router-one", "10.0.9.2"),
	}
	for _, router := range routers {
		router.Status.PodIP = routerIP(t, router)
	}

	transport := &observationDeadlineTransport{view: observerHealthyPayload}
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = original })

	// The Router processes carry their full controlling chain, because the collection verifies it
	// before it dials anything: a fixture that seeded only the Pods would measure the hold an
	// unplaceable process produces and read as a pass that never reached the transport.
	chain := ownedRouterObjectsFor(md, routers[0], routers[1])
	seeded := append([]ctrlcli.Object{md}, chain...)
	seeded = append(seeded, observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true))
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(seeded...).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(ctrlcli.Object) []string { return nil }).
		Build()
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	// The caller's own deadline is earlier than the per-fetch bound, so the deadline the requests
	// carry is the collection's and not a read's own.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	callerDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	observations, failure := r.collectModelDeploymentRouterObservations(
		ctx, md, []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)})
	require.Empty(t, failure, "both routers answer the same view: %s", failure)
	require.Len(t, observations, 2, "both routers are read")

	require.Len(t, transport.deadlines, 2, "each router is read once")
	for _, deadline := range transport.deadlines {
		require.False(t, deadline.IsZero(), "every outbound request carries a deadline")
		assert.WithinDuration(t, callerDeadline, deadline, time.Millisecond,
			"and the deadline is the collection's, so a caller that must not outlive its own deadline is obeyed")
	}
	assert.Equal(t, transport.deadlines[0], transport.deadlines[1],
		"both routers run under one derived deadline, not one each")
}

// routerIP reads a Router Pod's own address out of its fixture.
func routerIP(t *testing.T, pod *corev1.Pod) string {
	t.Helper()

	live := pod.DeepCopy()
	live.Status.PodIP = "10.0.9.1"
	if pod.Name == "router-one" {
		live.Status.PodIP = "10.0.9.2"
	}

	return live.Status.PodIP
}

// TestTheParserRefusesAPayloadThatDoesNotFinish covers the two malformed shapes a decoder would
// otherwise accept silently: a null worker row, and a second JSON value after the payload.
func TestTheParserRefusesAPayloadThatDoesNotFinish(t *testing.T) {
	const complete = `{"registered":true,"healthy":true,"circuit_open":false,"in_selection":true}`

	for _, tc := range []struct {
		name   string
		view   string
		wantOK bool
	}{
		{
			name:   "a complete view parses",
			view:   `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[{"url":"http://10.0.0.5:8000","selection":` + complete + `}]}`,
			wantOK: true,
		},
		{
			name: "a null worker row is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[null]}`,
		},
		{
			name: "a trailing JSON value is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}{"router":{"boot_generation":9}}`,
		},
		{
			name: "trailing content after the payload is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]} and then some`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := parseRouterObservationView([]byte(tc.view))
			if tc.wantOK {
				require.NoError(t, err)
				require.Len(t, view.Workers, 1)

				return
			}
			require.Error(t, err, "a payload that does not fully state the view is not a view")
		})
	}
}

type discoveryDeadlineClient struct {
	ctrlcli.Client
	deadlines []time.Time
}

func (c *discoveryDeadlineClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	deadline, _ := ctx.Deadline()
	c.deadlines = append(c.deadlines, deadline)
	return c.Client.List(ctx, list, opts...)
}

func TestObservationBudgetCoversDiscoveryAndRemainingRetirement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		residual bool
		caller   time.Duration
		phaseAge time.Duration
		overall  time.Duration
		want     time.Duration
	}{
		{name: "serving discovery with unlimited caller", overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "residual discovery with unlimited caller", residual: true, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "serving generous caller is capped", caller: time.Hour, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "residual generous caller is capped", residual: true, caller: time.Hour, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "serving keeps shorter caller", caller: 3 * time.Second, overall: time.Minute, want: 3 * time.Second},
		{name: "residual keeps shorter caller", residual: true, caller: 3 * time.Second, overall: time.Minute, want: 3 * time.Second},
		{name: "serving respects remaining withdrawal phase", phaseAge: 29 * time.Second, overall: time.Minute, want: time.Second},
		{name: "residual respects remaining withdrawal phase", residual: true, phaseAge: 29 * time.Second, overall: time.Minute, want: time.Second},
		{name: "serving respects remaining overall operation", overall: time.Second, want: time.Second},
		{name: "residual respects remaining overall operation", residual: true, overall: time.Second, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, _ := retirementRouterFixture(newRenderDeployment())
			now := time.Now()
			md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
				State:          workercore.ModelDeploymentRetirementStateWithdrawing,
				PhaseStartedAt: metav1.NewTime(now.Add(-tc.phaseAge)),
				Deadline:       metav1.NewTime(now.Add(tc.overall)),
			}
			router := retirementRouterPod("router-zero", "10.0.9.1")
			routerRS, routerDeployment := ownedRouterChain(router, md)
			base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(md, router, &routerRS, &routerDeployment).Build()
			recorder := &discoveryDeadlineClient{Client: base}
			reads := 0
			r := &ModelDeploymentReconciler{Client: recorder, APIReader: recorder, clock: func() time.Time { return now }, servingViewFetch: func(ctx context.Context, _ string) ([]byte, error) {
				reads++
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, deadline.Sub(now), tc.want+500*time.Millisecond)
				return []byte(`{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}`), nil
			}}
			ctx := context.Background()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.caller)
				defer cancel()
			}
			if tc.residual {
				reason, held := r.observeModelDeploymentRetirementResidual(ctx, md, sets.New[types.UID]("target"))
				require.False(t, held, reason)
			} else {
				answer := r.observeModelDeploymentServing(ctx, md)
				require.Equal(t, workercore.ModelDeploymentServingStateConfirmed, answer.State, answer.Reason)
				require.NotNil(t, answer.Value)
				require.EqualValues(t, 0, *answer.Value)
			}
			require.Equal(t, 1, reads, "actual Router fetch reached")
			require.GreaterOrEqual(t, len(recorder.deadlines), 2, "worker discovery and Router discovery both reached")
			for _, deadline := range recorder.deadlines {
				require.False(t, deadline.IsZero(), "every discovery is bounded")
				require.LessOrEqual(t, deadline.Sub(now), tc.want+500*time.Millisecond)
			}
		})
	}
}

// journalWorker is one observed worker, stated as facts rather than as a handler. The maps are the
// literal JSON the shipped observer serves, so a row cannot drift into a shape the producer does
// not emit.
type journalWorker map[string]any

// journalWorkerFact builds one observed worker at a live Pod's own address. The address is the Pod
// list's, never a literal here, because the collection binds a worker's address to the Pod it
// resolves to at read time and refuses a view it cannot bind.
func journalWorkerFact(ip, uid string, inSelection bool, lastJob map[string]any) journalWorker {
	worker := journalWorker{
		"url":  "http://" + ip + ":8000",
		"port": "8000", "role": "server",
		"pod_hint": uid, "worker_id": uid,
		"selection": map[string]any{
			"registered": inSelection, "healthy": inSelection,
			"circuit_open": false, "in_selection": inSelection,
		},
	}
	if lastJob != nil {
		worker["last_job"] = lastJob
	}

	return worker
}

// journalView renders the view the shipped llm-d observer serves for a set of workers. A failure to
// encode is a broken fixture rather than a case under test, so it stops the test where it is
// reported instead of carrying a panic into the collection.
func journalView(t *testing.T, workers []journalWorker) []byte {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"router":            map[string]any{"boot_generation": 1, "now_ms": 1},
		"registry_revision": 1,
		"workers":           workers,
	})
	require.NoError(t, err)

	return payload
}

// ownedRouterChain gives a Router process the live controlling chain the render produces —
// Pod, ReplicaSet, workload Deployment, ModelDeployment — and returns the two apps objects the
// fixture must seed so the collection's ownership verification finds the real rendered shape.
//
// THE DEPLOYMENT IS ONE OBJECT PER MODELDEPLOYMENT, NOT ONE PER POD. Every Router process of a
// deployment is scaled by the same workload Deployment, so that Deployment has one name and one
// UID whatever else varies. Deriving its UID from the Pod made two Router fixtures build the same
// name with different identities, which the API cannot hold: the second write is rejected and a
// fixture that only means to place two routers cannot be built at all.
func ownedRouterChain(pod *corev1.Pod, md *workercore.ModelDeployment) (appsv1.ReplicaSet, appsv1.Deployment) {
	rsUID := types.UID("rs-uid-" + pod.Name)
	deploymentUID := types.UID("deployment-uid-router-" + md.Namespace + "-" + md.Name)
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: appsv1.SchemeGroupVersion.String(),
		Kind:       "ReplicaSet",
		Name:       pod.Name + "-rs",
		UID:        rsUID,
		Controller: &controller,
	}}
	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name + "-rs",
			Namespace: pod.Namespace,
			UID:       rsUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(),
				Kind:       "Deployment",
				Name:       md.Name + "-router",
				UID:        deploymentUID,
				Controller: &controller,
			}},
		},
	}
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      md.Name + "-router",
			Namespace: pod.Namespace,
			UID:       deploymentUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       md.Name,
				UID:        md.UID,
				Controller: &controller,
			}},
		},
	}

	return replicaSet, deployment
}

// ownedRouterObjects returns the one Router process a fixture needs together with every live object
// its controlling chain names, ready to spread into an object list.
//
// A fixture that seeds only the Pod is not a faithful positive path: the collection verifies every
// hop of the chain through the uncached reader and holds a process it cannot place, so a fixture
// that omits the ReplicaSet and Deployment measures the hold rather than the answer.
func ownedRouterObjects(md *workercore.ModelDeployment) []ctrlcli.Object {
	pod := retirementRouterPod("router-zero", "10.0.9.1")
	replicaSet, deployment := ownedRouterChain(pod, md)

	return []ctrlcli.Object{pod, &replicaSet, &deployment}
}

// ownedRouterObjectsFor is ownedRouterObjects for a fixture that places more than one Router process.
// Each process gets its own ReplicaSet, because a ReplicaSet's name is derived from its Deployment
// and a hash and two processes of one Deployment are two different sets; the Deployment they share
// is seeded once, because it is one object.
func ownedRouterObjectsFor(
	md *workercore.ModelDeployment, pods ...*corev1.Pod,
) []ctrlcli.Object {
	objects := make([]ctrlcli.Object, 0, 2*len(pods)+1)
	var shared *appsv1.Deployment
	for _, pod := range pods {
		replicaSet, deployment := ownedRouterChain(pod, md)
		objects = append(objects, pod, &replicaSet)
		if shared == nil {
			shared = &deployment
		}
	}

	return append(objects, shared)
}

// TestRetirementResidualReadsTheDispatchJournal asks the real residual function, with the real
// collector, about a target the router removed from its selection.
//
// The shipped observer keeps a removed worker in the view with in_selection false and marks the
// dispatches it recently sent to it under last_job.recent_dispatch. The binder refuses to count a
// worker outside the selection as serving, which is right, so the journal is the only remaining
// evidence that a dispatch reached the target. Before it was read, a target with a recent dispatch
// read as a target with nothing left serving it, and the operation released it.
func TestRetirementResidualReadsTheDispatchJournal(t *testing.T) {
	recent := map[string]any{"recent_dispatch": true}
	notRecent := map[string]any{"recent_dispatch": false}

	for _, tc := range []struct {
		name     string
		workers  []journalWorker
		status   int
		wantHeld bool
	}{
		{
			name:     "a removed captured target with a recent dispatch holds the release",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, recent)},
			wantHeld: true,
		},
		{
			name:     "a complete view with no target selection and no dispatch clears this check",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, nil)},
			wantHeld: false,
		},
		{
			name:     "a journal that says the dispatch was not recent does not hold",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, notRecent)},
			wantHeld: false,
		},
		{
			name: "recent dispatch for an unrelated survivor does not hold the captured target",
			workers: []journalWorker{
				journalWorkerFact("10.0.1.1", "member-1", false, nil),
				journalWorkerFact("10.0.1.2", "other-1", false, recent),
			},
			wantHeld: false,
		},
		{
			// A journal block the producer does not emit is a block this operator cannot read.
			// Reading it as false would be a decoder default turning unreadable evidence into
			// confirmed absence of a dispatch, which is the one thing the journal must never be.
			name:     "a journal without the boolean holds rather than confirming absence",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, map[string]any{"job_type": "add"})},
			wantHeld: true,
		},
		{
			name:     "a selectable captured target is still held by the serving count",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", true, nil)},
			wantHeld: true,
		},
		{
			// The observer answers 503 while a selection or send is pending, which is exactly the
			// window in which a dispatch may be in flight and unrecorded. No supported shape, no
			// fresh timestamp and no absent journal can stand in for that answer.
			name:     "an actual native 503 remains unknown and holds",
			workers:  nil,
			status:   http.StatusServiceUnavailable,
			wantHeld: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, _ := retirementRouterFixture(newRenderDeployment())
			pods := []corev1.Pod{
				*surplusReplicaAt(t, md, "qwen-server-one", 1, ""),
				*surplusReplicaAt(t, md, "qwen-server-zero", 0, ""),
			}
			pods[0].UID = "member-1"
			pods[1].UID = "other-1"
			ips := []string{"10.0.1.1", "10.0.1.2"}
			objects := make([]ctrlcli.Object, 0, 3+len(pods))
			routerProcess := retirementRouterPod("router-zero", "10.0.9.1")
			routerRS, routerDeployment := ownedRouterChain(routerProcess, md)
			objects = append(objects, md, routerProcess, &routerRS, &routerDeployment)
			for i := range pods {
				pods[i].Status.Phase = corev1.PodRunning
				pods[i].Status.PodIP = ips[i]
				objects = append(objects, pods[i].DeepCopy())
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			r := &ModelDeploymentReconciler{
				Client: client, APIReader: client, clock: time.Now,
				servingViewFetch: func(context.Context, string) ([]byte, error) {
					if status != http.StatusOK {
						return nil, fmt.Errorf("observer answered %d", status)
					}

					return journalView(t, tc.workers), nil
				},
			}

			reason, held := r.observeModelDeploymentRetirementResidual(
				context.Background(), md, sets.New[types.UID]("member-1"))

			assert.Equal(t, tc.wantHeld, held, reason)
		})
	}
}

// TestTheObserverIsReadFromTheOwningProfileListener pins the listener each profile serves the
// observer on. The llm-d patches attach it to the management listener the router already runs;
// the other two serve it on the request port their Service already targets, and their contract is
// unchanged.
func TestTheObserverIsReadFromTheOwningProfileListener(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    int32
	}{
		{profile: "llm-d-router", want: modelDeploymentRouterMetricsPort},
		{profile: "vllm-router", want: modelDeploymentRouterHTTPPort},
		{profile: "sglang-gateway", want: modelDeploymentRouterHTTPPort},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentRouterObserverPort(tc.profile))
		})
	}
}

func TestObserverTransportCancelsStalledHeadersAndBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flush bool
	}{{"headers never arrive", false}, {"body never finishes", true}} {
		t.Run(tc.name, func(t *testing.T) {
			reached := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.flush {
					_, _ = w.Write([]byte("{"))
					w.(http.Flusher).Flush()
				}
				close(reached)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := defaultServingViewFetch(ctx, server.URL)
			require.True(t, errors.Is(err, context.DeadlineExceeded), "actual transport must stop on caller deadline: %v", err)
			select {
			case <-reached:
			default:
				t.Fatal("server stall was never reached")
			}
		})
	}
}

// TestRouterViewCollectedAtIsStampedAfterItsOwnRead pins the per-view stamp (I10) without any
// timing guess: the reconciler's injected clock advances once per read, so a slow first read is
// visible as a strictly earlier stamp on the first view than on the second. A stamp taken once
// before the loop would make the two views equal here, and a slow first fetch would pre-age the
// last view toward the freshness edge in production.
func TestRouterViewCollectedAtIsStampedAfterItsOwnRead(t *testing.T) {
	md, router := retirementRouterFixture(newRenderDeployment())
	routerTwo := retirementRouterPod("router-one", "10.0.9.2")
	chain := ownedRouterObjectsFor(md, router, routerTwo)
	base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(append([]ctrlcli.Object{md}, chain...)...).Build()

	tick := 0
	baseNow := time.Now()
	r := &ModelDeploymentReconciler{
		Client: base, APIReader: base,
		clock: func() time.Time {
			tick++
			return baseNow.Add(time.Duration(tick) * time.Minute)
		},
		servingViewFetch: func(_ context.Context, _ string) ([]byte, error) {
			return []byte(`{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}`), nil
		},
	}

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md,
		[]*corev1.Pod{retirementRouterPod("router-zero", "10.0.9.1"), retirementRouterPod("router-one", "10.0.9.2")},
	)
	require.Empty(t, failure)
	require.Len(t, observations, 2)
	assert.True(t, observations[0].CollectedAt.Before(observations[1].CollectedAt),
		"each view is stamped after its own read, not from one pre-loop timestamp: %v vs %v",
		observations[0].CollectedAt, observations[1].CollectedAt)
}

// seedRouterOwnership gives every Router process of a deployment its live controlling chain — Pod,
// ReplicaSet, workload Deployment, ModelDeployment — and seeds the apps objects that chain names,
// so a collection that verifies ownership finds the real rendered shape.
func seedRouterOwnership(client ctrlcli.Client, md *workercore.ModelDeployment) error {
	routers := new(corev1.PodList)
	if err := client.List(context.Background(), routers,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{modelDeploymentRouterLabelKey: md.Spec.Router.Name}); err != nil {
		return err
	}
	for i := range routers.Items {
		routerPod := &routers.Items[i]
		// A PROCESS THE RECONCILER HAS ALREADY RENDERED IS LEFT ALONE. The chain objects carry the
		// names the real render gives them, so seeding a second chain here would put a foreign
		// object under the reconciler's own names; the sync that maintains those names would then
		// find it different from its own render on every pass and rewrite it forever. A Pod that
		// already carries a controlling owner is one whose chain the reconciler owns.
		if metav1.GetControllerOf(routerPod) != nil {
			continue
		}
		replicaSet, deployment := ownedRouterChain(routerPod, md)
		if err := client.Update(context.Background(), routerPod); err != nil {
			return err
		}
		if err := client.Create(context.Background(), &replicaSet); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := client.Create(context.Background(), &deployment); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}

// seedHealthyRouterOwnership is seedRouterOwnership for the convergence-only positives whose
// fixture path seeds no Router objects itself.
func seedHealthyRouterOwnership(t *testing.T, client ctrlcli.Client, md *workercore.ModelDeployment) {
	t.Helper()
	// A deployment that declares no router has declared its absence, and there is nothing to seed:
	// writing a chain here would invent a Router the spec does not have.
	if md.Spec.Router == nil {
		return
	}
	require.NoError(t, seedRouterOwnership(client, md))
}

// liveModelDeploymentFor reads the deployment a fixture seeded, without a *testing.T, so a fixture
// builder can complete its own seeding before it hands a reconciler back to the test.
func liveModelDeploymentFor(cli ctrlcli.Client) *workercore.ModelDeployment {
	list := new(workercore.ModelDeploymentList)
	if err := cli.List(context.Background(), list); err != nil || len(list.Items) == 0 {
		return nil
	}

	return &list.Items[0]
}

// ownedRouterFixture returns a Router process carrying its live controlling chain together with
// the ReplicaSet and Deployment objects that chain names, ready for a fixture's objects list.
func ownedRouterFixture(md *workercore.ModelDeployment) (*corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment) {
	router := retirementRouterPod("router-zero", "10.0.9.1")
	replicaSet, deployment := ownedRouterChain(router, md)

	return router, &replicaSet, &deployment
}

// TestABoundEndpointNeedsTheIdentityTheWorkloadStates pins the binding acceptance: a bound
// endpoint is counted under the role, rank and listener the bound Pod itself declares, and a row
// whose Pod cannot state all three is refused rather than counted with a gap.
//
// The observer's own worker_id is never a source. It is a native registry identity that enumerates
// virtual ranks on a data-parallel member, and reading it as a member rank would report the
// router's registry size rather than the pool's serving members.
func TestABoundEndpointNeedsTheIdentityTheWorkloadStates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		port    string
		mutate  func(*corev1.Pod)
		wantErr string
	}{
		{
			name: "a member with its rendered role, rank and declared listener binds",
			port: "8000",
		},
		{
			name: "a pod that declares no workload role is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, modelDeploymentLabelKeyComponent)
			},
			wantErr: "carries no workload role",
		},
		{
			name: "a pod that declares no member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, modelDeploymentMemberIndexLabel)
			},
			wantErr: "carries no member rank",
		},
		{
			// The render writes a decimal index counting from zero. A label a user edited into
			// something else is not an ordinal, and reading it as one would name a member that
			// does not exist.
			name: "a non-decimal member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				pod.Labels[modelDeploymentMemberIndexLabel] = "leader"
			},
			wantErr: "rather than a decimal index",
		},
		{
			name: "a signed member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				pod.Labels[modelDeploymentMemberIndexLabel] = "-1"
			},
			wantErr: "rather than a decimal index",
		},
		{
			// A port the Pod never declared is not a listener it can be serving on, so a row
			// naming one is a row about nothing this member owns.
			name: "a port the member never declared is refused",
			port: "9999",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Ports = nil
			},
			wantErr: "declares no container port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			view := RouterServingView{Workers: []RouterObservedWorker{{
				URL:       "http://10.0.0.5:" + tc.port,
				Port:      tc.port,
				Selection: RouterObservedSelection{InSelection: true, Registered: true, Healthy: true},
			}}}

			bound, _, err := bindRouterObservationView(view, []*corev1.Pod{pod}, false)
			if tc.wantErr != "" {
				require.Error(t, err, "an endpoint the workload cannot identify is not an endpoint")
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			require.Len(t, bound, 1)
			assert.Equal(t, "regular", bound[0].Role)
			assert.Equal(t, "0", bound[0].Rank)
			assert.Equal(t, observationMemberPort, bound[0].Listener)
		})
	}
}

// TestTheServingCountFoldsVirtualRowsOnOnePhysicalListener pins the union rule: several rows a
// router registers for one data-parallel member are ONE physical listener, and the count reports
// the members serving rather than the rows received. It also pins that a contradiction refuses
// convergence while agreeing views and an explicit empty registry stay accepted.
func TestTheServingCountFoldsVirtualRowsOnOnePhysicalListener(t *testing.T) {
	t.Parallel()

	// One member, four virtual ranks: the same Pod and the same declared listener, which is the
	// shape a data-parallel engine registers.
	virtuals := func() []RouterObservedWorker {
		workers := make([]RouterObservedWorker, 0, 4)
		for i := 0; i < 4; i++ {
			workers = append(workers, RouterObservedWorker{
				URL: fmt.Sprintf("http://10.0.0.5:8000@%d", i), Port: "8000", WorkerID: string(rune('a' + i)),
				Selection: RouterObservedSelection{InSelection: true},
			})
		}

		return workers
	}
	selected := func(workers []RouterObservedWorker, inSelection bool) RouterServingObservation {
		return RouterServingObservation{
			View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1, Workers: workers},
			CollectedAt: time.Now(),
			Bound: []ServingEndpoint{{
				PodUID: "pod-uid-1", Role: "regular", Rank: "0",
				Listener: observationMemberPort, Serving: inSelection,
			}},
		}
	}
	bound := func(workers []RouterObservedWorker) RouterServingObservation {
		observation := selected(workers, true)
		bound, _, err := bindRouterObservationView(observation.View, []*corev1.Pod{
			observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true),
		}, false)
		require.NoError(t, err)
		observation.Bound = bound

		return observation
	}

	for _, tc := range []struct {
		name         string
		observations []RouterServingObservation
		wantState    workercore.ModelDeploymentServingState
		wantValue    int32
		wantReason   string
	}{
		{
			name:         "four virtual rows on one listener are one serving member",
			observations: []RouterServingObservation{bound(virtuals())},
			wantState:    workercore.ModelDeploymentServingStateConfirmed,
			wantValue:    1,
		},
		{
			name:         "two routers that agree on the same physical member count it once",
			observations: []RouterServingObservation{bound(virtuals()), bound(virtuals())},
			wantState:    workercore.ModelDeploymentServingStateConfirmed,
			wantValue:    1,
		},
		{
			// A router that selects a listener another refuses is not a lagging view; it is two
			// answers about the same member, and a count built on either one is a guess.
			name: "routers that disagree about one physical member refuse convergence",
			observations: []RouterServingObservation{
				bound(virtuals()), selected(virtuals(), false),
			},
			wantState:  workercore.ModelDeploymentServingStateNotConverged,
			wantReason: "disagree",
		},
		{
			// The same contradiction inside one view is caught by the same fold, because the union
			// is walked row by row and the refused rows are carried rather than dropped.
			name: "one view that both selects and refuses a listener refuses convergence",
			observations: []RouterServingObservation{
				func() RouterServingObservation {
					observation := bound(virtuals())
					observation.Bound = append(observation.Bound, ServingEndpoint{
						PodUID: "pod-uid-1", Role: "regular", Rank: "0",
						Listener: observationMemberPort, Serving: false,
					})

					return observation
				}(),
			},
			wantState:  workercore.ModelDeploymentServingStateNotConverged,
			wantReason: "disagree",
		},
		{
			name: "an explicit empty registry is a confirmed zero",
			observations: []RouterServingObservation{{
				View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1},
				CollectedAt: time.Now(),
			}},
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := aggregateModelDeploymentServing(
				true, tc.observations, modelDeploymentServingFreshness)
			assert.Equal(t, tc.wantState, answer.State, answer.Reason)
			if tc.wantValue == 0 && tc.wantState != workercore.ModelDeploymentServingStateConfirmed {
				return
			}
			if answer.Value != nil {
				assert.Equal(t, tc.wantValue, *answer.Value, answer.Reason)
			}
			if tc.wantReason != "" {
				assert.Contains(t, answer.Reason, tc.wantReason)
			}
		})
	}
}

// TestARouterProcessIsHeldUnlessEveryChainHopIsTheRenderedOne pins the ownership chain: each
// live hop of Pod, ReplicaSet, Deployment and ModelDeployment is checked by kind, API version,
// name and identity, and a hop that is missing, unreadable or different holds the process rather
// than narrowing the answer.
func TestARouterProcessIsHeldUnlessEveryChainHopIsTheRenderedOne(t *testing.T) {
	t.Parallel()

	md := newRenderDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	faithful := func(t *testing.T) *ModelDeploymentReconciler {
		pod, replicaSet, deployment := ownedRouterFixture(md)
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(md.DeepCopy(), pod, replicaSet, deployment).Build()

		return &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, cli ctrlcli.Client)
		replace *chainReplacement
		wantErr string
	}{
		{
			name: "the rendered chain verifies",
		},
		{
			name: "a pod with no controlling owner is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				dropOwner(t, cli, "router-zero")
			},
			wantErr: "no controlling owner",
		},
		{
			name: "a pod controlled by something other than a ReplicaSet is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				rewriteOwner(t, cli, "router-zero", "StatefulSet", appsv1.SchemeGroupVersion.String(),
					"router-zero-rs", "rs-uid-router-zero")
			},
			wantErr: "rather than a ReplicaSet",
		},
		{
			// A ReplicaSet of a different API version is not the chain this operator verifies,
			// and reading it as one would verify a shape the reconciler never renders.
			name: "a ReplicaSet of another API version is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				rewriteOwner(t, cli, "router-zero", "ReplicaSet", "apps/v1beta2",
					"router-zero-rs", "rs-uid-router-zero")
			},
			wantErr: "rather than a ReplicaSet",
		},
		{
			name: "a replica set that no longer exists is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				require.NoError(t, cli.Delete(context.Background(),
					&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
						Name: "router-zero-rs", Namespace: md.Namespace,
					}}))
			},
			wantErr: "could not be read uncached",
		},
		{
			// A ReplicaSet deleted and recreated under its own name is what a rollout or a
			// retirement leaves behind: the read still succeeds, and the identity the Pod's
			// controlling reference carries is the only thing left to tell the two apart.
			name: "a replica set that changed identity is held",
			replace: &chainReplacement{
				object:   func() ctrlcli.Object { return new(appsv1.ReplicaSet) },
				key:      ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-zero-rs"},
				freshUID: types.UID("rs-uid-router-zero-replacement"),
			},
			wantErr: "ReplicaSet changed identity during observation",
		},
		{
			// The workload Deployment is the far end of the apps chain, and it is replaced the
			// same way: the same name, a read that still succeeds, and an identity comparison
			// that is the only thing left holding the process.
			name: "a Deployment that changed identity is held",
			replace: &chainReplacement{
				object:   func() ctrlcli.Object { return new(appsv1.Deployment) },
				key:      ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"},
				freshUID: types.UID("deployment-uid-router-replacement"),
			},
			wantErr: "Deployment changed identity during observation",
		},
		{
			name: "a replica set with no controlling Deployment is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				replicaSet := new(appsv1.ReplicaSet)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-zero-rs"}, replicaSet))
				replicaSet.OwnerReferences = nil
				require.NoError(t, cli.Update(context.Background(), replicaSet))
			},
			wantErr: "no controlling Deployment",
		},
		{
			name: "a Deployment owned by another deployment is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				deployment := new(appsv1.Deployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, deployment))
				deployment.OwnerReferences[0].UID = "someone-else"
				require.NoError(t, cli.Update(context.Background(), deployment))
			},
			wantErr: "rather than this deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := faithful(t)
			if tc.mutate != nil {
				tc.mutate(t, r.Client)
			}
			if tc.replace != nil {
				replaceChainHop(t, r.Client, *tc.replace)
			}
			// The pod is read back from the client, because what the row mutated is the object the
			// collection would find, not the fixture's own copy of it.
			pod := new(corev1.Pod)
			require.NoError(t, r.Client.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: "team-a", Name: "router-zero"}, pod))

			err := r.verifyRouterPodOwnership(context.Background(), md, pod,
				newRouterOwnerChain(r.APIReader))
			if tc.wantErr == "" {
				require.NoError(t, err, "the rendered chain is the one the render produces")

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// dropOwner removes a Pod's controlling owner reference, leaving the Pod otherwise untouched.
func dropOwner(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = nil
	require.NoError(t, cli.Update(context.Background(), pod))
}

// rewriteOwner replaces a Pod's controlling owner reference with a named one.
func rewriteOwner(t *testing.T, cli ctrlcli.Client, name, kind, apiVersion, ownerName string, uid types.UID) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: apiVersion, Kind: kind, Name: ownerName, UID: uid,
		Controller: ptr.To(true),
	}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// chainReplacement states that one object of the rendered chain is replaced in place: the object
// keeps its namespace and name and is given another UID, the shape a rollout or a retirement
// leaves behind.
type chainReplacement struct {
	object func() ctrlcli.Object
	key    ctrlcli.ObjectKey
	// freshUID is the identity the replacement carries. It has to differ from the identity the
	// chain's controlling reference still names, or the row would verify nothing.
	freshUID types.UID
}

// replaceChainHop deletes the named object of the rendered chain and recreates it under the same
// namespace and name with another UID, changing nothing else about it, and then asserts that the
// object the reconciler will read is that replacement. The read therefore succeeds, so the row is
// only ever held by the identity comparison, and the assertion is what proves the row reached it
// rather than a missing-object path.
func replaceChainHop(t *testing.T, cli ctrlcli.Client, hop chainReplacement) {
	t.Helper()

	live := hop.object()
	require.NoError(t, cli.Get(context.Background(), hop.key, live))
	assert.NotEqual(t, live.GetUID(), hop.freshUID,
		"a replacement reusing the rendered identity would verify nothing")

	removed := hop.object()
	removed.SetNamespace(hop.key.Namespace)
	removed.SetName(hop.key.Name)
	require.NoError(t, cli.Delete(context.Background(), removed))

	live.SetUID(hop.freshUID)
	// A READ OBJECT CARRIES THE VERSION OF THE ONE THAT WAS DELETED. A create names no version,
	// so the replacement is written without one, exactly as the API server would store it.
	live.SetResourceVersion("")
	require.NoError(t, cli.Create(context.Background(), live))

	// THE REPLACEMENT IS THE OBJECT THE CHAIN READS. This reads it back rather than trusting the
	// write, so a row that failed to establish a fresh identity is reported as a broken fixture
	// instead of passing against the read failure it did not mean to exercise.
	replaced := hop.object()
	require.NoError(t, cli.Get(context.Background(), hop.key, replaced))
	assert.Equal(t, hop.freshUID, replaced.GetUID(),
		"the chain must read the replacement, so the identity comparison is what holds it")
}

// TestATerminatingRouterProcessIsStillAProcess pins the coverage rule: a Router Pod that is
// terminating still holds an address and still answers, so it is read like any other, and only a
// process that has lost its address ends the coverage.
func TestATerminatingRouterProcessIsStillAProcess(t *testing.T) {
	t.Parallel()

	md, router := retirementRouterFixture(newRenderDeployment())
	// A deleting object the API accepts still carries a finalizer, and a fake client that refuses
	// one without is modeling the API rather than the operator, so the fixture is a real one.
	router.DeletionTimestamp = ptr.To(metav1.Now())
	router.Finalizers = []string{"gpustack.ai/test"}
	// The chain belongs to THIS Pod, the one the collection discovers by label.
	replicaSet, deployment := ownedRouterChain(router, md)
	member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
	member.Labels[modelDeploymentLabelKeyInstance] = md.Name
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(md, router, member, &replicaSet, &deployment).Build()
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, clock: time.Now,
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, []*corev1.Pod{member})
	require.Empty(t, failure)
	require.Len(t, observations, 1,
		"a terminating process is a running process: it holds an address and answers")
	assert.NoError(t, observations[0].Err,
		"a terminating process is read, not held: dropping it would read a shutdown as a converged empty registry")
}

// TestTheCollectionIsBoundedByTheDeploymentItStartedFrom pins the guard: the deployment and
// its membership are re-read uncached after the views, and a deployment that changed underneath
// the collection holds the whole answer rather than confirming absence from an earlier snapshot.
//
// THE CHANGE IS MADE FROM INSIDE THE FETCH, between the two reads, because that is the window the
// guard exists to close. A change made before the collection is simply what both snapshots see, and
// a guard that cannot tell that apart from a change underneath it is not the guard.
func TestTheCollectionIsBoundedByTheDeploymentItStartedFrom(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment)
		wantMsg string
	}{
		{
			name: "an unchanged deployment and membership confirm",
		},
		{
			name: "a deployment whose generation moved holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				live := new(workercore.ModelDeployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKeyFromObject(md), live))
				live.Generation++
				require.NoError(t, cli.Update(context.Background(), live))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a deployment that was replaced by another object holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				live := new(workercore.ModelDeployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKeyFromObject(md), live))
				live.UID = "a-different-deployment"
				require.NoError(t, cli.Update(context.Background(), live))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a membership that lost a member holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				require.NoError(t, cli.Delete(context.Background(),
					&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: "server-0", Namespace: "team-a",
					}}))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			// The member keeps its UID and changes its controlling owner. The two bookends still
			// agree on the membership, so a UID-only comparison would read this member as the same
			// one and confirm absence about a Pod this pass never bound.
			name: "a member that changed owner while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				writeStatus(t, cli, "server-0", func(pod *corev1.Pod) {
					_ = pod
				})
				adopt(t, cli, "server-0", mdForMember(t, cli))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a member that was relabelled into another role while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				relabel(t, cli, "server-0", modelDeploymentLabelKeyComponent, "prefill")
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a member whose declared listener moved while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				moveListener(t, cli, "server-0", 9000)
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			// A Pod that wears this deployment's identity labels but is owned by another object is
			// not a fact any binding can act on, so the guard must not bookend it. Refusing the
			// collection over it would make the guard describe a wider set than the bindings read,
			// and one foreign Pod's churn would hold a member this deployment does have.
			name: "a labelled pod this deployment does not own does not hold",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				impostor := observationLivePod("server-9", "10.0.0.9", "pod-uid-9", true)
				impostor.Labels[modelDeploymentLabelKeyInstance] = md.Name
				impostor.OwnerReferences[0].UID = "another-deployment"
				require.NoError(t, cli.Create(context.Background(), impostor))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md, router := retirementRouterFixture(newRenderDeployment())
			member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
			member.Labels[modelDeploymentLabelKeyInstance] = md.Name
			replicaSet, deployment := ownedRouterChain(router, md)
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(md, router, member, &replicaSet, &deployment).Build()
			r := &ModelDeploymentReconciler{
				Client: cli, APIReader: cli, clock: time.Now,
				servingViewFetch: func(context.Context, string) ([]byte, error) {
					if tc.mutate != nil {
						tc.mutate(t, cli, md)
					}

					return []byte(observerHealthyPayload), nil
				},
			}

			observations, failure := r.collectModelDeploymentRouterObservations(
				context.Background(), md, []*corev1.Pod{member})
			if tc.wantMsg == "" {
				require.Empty(t, failure)
				require.Len(t, observations, 1)

				return
			}
			require.NotEmpty(t, failure,
				"confirmed absence from an earlier snapshot would be a lie about the current one")
			assert.Contains(t, failure, tc.wantMsg)
		})
	}
}

// TestACallerHoldingAReplacedDeploymentIsRefused pins the caller half of the bookend: the
// first uncached read is compared with the object the caller passed in, not only with the second
// uncached read.
//
// TWO EQUAL FRESH SNAPSHOTS SAY NOTHING ABOUT THE CALLER. A reconcile that started before a
// replacement carries the old object for its whole pass, and both of its fresh reads agree on the
// replacement, so a guard comparing only the two reads accepts the caller's stale object as
// current. This is a control for exactly that: the deployment is replaced before the collection
// starts, so the two bookends agree and only the caller disagrees.
func TestACallerHoldingAReplacedDeploymentIsRefused(t *testing.T) {
	t.Parallel()

	md, router := retirementRouterFixture(newRenderDeployment())
	member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
	member.Labels[modelDeploymentLabelKeyInstance] = md.Name
	replicaSet, deployment := ownedRouterChain(router, md)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(md, router, member, &replicaSet, &deployment).Build()
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, clock: time.Now,
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	// The replacement happens FIRST, so both uncached bookends see the same new object and the
	// only disagreeing party is the caller still holding the old one.
	live := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), live))
	live.UID = "a-replacement-deployment"
	require.NoError(t, cli.Update(context.Background(), live))

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, []*corev1.Pod{member})
	require.NotEmpty(t, failure,
		"two agreeing fresh reads are a fact about the two reads, not about the caller")
	assert.Contains(t, failure, "the caller read deployment")
	assert.Empty(t, observations)
}

// adopt re-points a Pod's controlling owner at another object, as an adoption would.
func adopt(t *testing.T, cli ctrlcli.Client, name string, md *workercore.ModelDeployment) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: workercore.SchemeGroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        "someone-else",
		Controller: ptr.To(true),
	}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// relabel changes one label on a Pod in place, keeping its UID and everything else.
func relabel(t *testing.T, cli ctrlcli.Client, name, key, value string) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.Labels[key] = value
	require.NoError(t, cli.Update(context.Background(), pod))
}

// moveListener re-declares the member's container ports, keeping its UID and every label.
func moveListener(t *testing.T, cli ctrlcli.Client, name string, port int32) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// writeStatus changes a Pod's status in place, the way the kubelet writes it.
func writeStatus(t *testing.T, cli ctrlcli.Client, name string, mutate func(*corev1.Pod)) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	mutate(pod)
	require.NoError(t, cli.Status().Update(context.Background(), pod))
}

// mdForMember reads the seeded deployment, for a row that mutates ownership mid-collection.
func mdForMember(t *testing.T, cli ctrlcli.Client) *workercore.ModelDeployment {
	t.Helper()
	live := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, live))

	return live
}

// TestTheCountIsPerRoleOverTheSameUnion pins the per-role derivation: the union of three physical
// endpoints is counted once globally and per role, and the global total is never copied onto a
// role that does not hold it.
//
// THE FIXTURE IS THE SHAPE THE REVIEW NAMED: two endpoints in custom-a, one in custom-b and a
// fourth that every router has already refused. A role with no serving endpoint is an explicit
// zero, which is the only reading under which a role's own field is not reporting its neighbors.
func TestTheCountIsPerRoleOverTheSameUnion(t *testing.T) {
	t.Parallel()

	endpoints := []ServingEndpoint{
		{PodUID: "pod-a-0", Role: "custom-a", Rank: "0", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-a-1", Role: "custom-a", Rank: "1", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-b-0", Role: "custom-b", Rank: "0", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-z-0", Role: "unselected", Rank: "0", Listener: observationMemberPort, Serving: false},
	}
	answer := aggregateModelDeploymentServing(true, []RouterServingObservation{{
		View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1},
		CollectedAt: time.Now(),
		Bound:       endpoints,
	}}, modelDeploymentServingFreshness)

	require.Equal(t, workercore.ModelDeploymentServingStateConfirmed, answer.State, answer.Reason)
	require.NotNil(t, answer.Value)
	assert.Equal(t, int32(3), *answer.Value, "three physical endpoints serve")
	assert.Equal(t, map[string]int32{"custom-a": 2, "custom-b": 1}, answer.ByRole,
		"each role is counted from the same union, and a refused endpoint counts for nobody")

	status := &workercore.ModelDeploymentStatus{Roles: []workercore.ModelDeploymentRoleStatus{
		{Name: "custom-a"}, {Name: "custom-b"}, {Name: "unselected"},
	}}
	applyModelDeploymentServing(status, answer)
	for i, want := range []int32{2, 1, 0} {
		require.NotNil(t, status.Roles[i].Endpoints.Serving.Value)
		assert.Equal(t, want, *status.Roles[i].Endpoints.Serving.Value,
			"role %q reports its own count, not the deployment's", status.Roles[i].Name)
	}
}

// TestOnlyAConfirmedAnswerCarriesACount pins the nil contract: Unknown, NotConverged and
// NotConfigured all leave every role's serving value absent, and only Confirmed allocates one.
//
// THE ABSENCE IS THE CONTRACT, not a missing field. A zero written into a state that never
// established a count is a measurement of zero, which is the false zero this whole path exists to
// refuse, and a reader cannot tell it from a role that genuinely serves nothing.
func TestOnlyAConfirmedAnswerCarriesACount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		answer    ServingAnswer
		wantState workercore.ModelDeploymentServingState
		// wantValue is per role, in the order the status declares them.
		wantValue []int32
	}{
		{
			name:      "not configured carries no count",
			answer:    ServingAnswer{State: workercore.ModelDeploymentServingStateNotConfigured},
			wantState: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name: "unknown carries no count even when a per-role map was built",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateUnknown,
				ByRole: map[string]int32{"custom-a": 2},
			},
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name: "not converged carries no count",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateNotConverged,
				ByRole: map[string]int32{"custom-a": 2},
			},
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name: "confirmed carries the role's own count",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateConfirmed,
				Value:  ptr.To(int32(3)),
				ByRole: map[string]int32{"custom-a": 2, "custom-b": 1},
			},
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: []int32{2, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status := &workercore.ModelDeploymentStatus{Roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "custom-a"}, {Name: "custom-b"},
			}}
			applyModelDeploymentServing(status, tc.answer)

			for i := range status.Roles {
				got := status.Roles[i].Endpoints.Serving
				assert.Equal(t, tc.wantState, got.State)
				if tc.wantValue == nil {
					assert.Nil(t, got.Value,
						"a state that established no count must carry no number")

					continue
				}
				require.NotNil(t, got.Value)
				assert.Equal(t, tc.wantValue[i], *got.Value,
					"role %q reports its own count", status.Roles[i].Name)
			}
			// Each role owns its value; two roles must never alias one count.
			if tc.wantValue != nil {
				require.NotNil(t, status.Roles[0].Endpoints.Serving.Value)
				require.NotNil(t, status.Roles[1].Endpoints.Serving.Value)
				assert.NotSame(t, status.Roles[0].Endpoints.Serving.Value,
					status.Roles[1].Endpoints.Serving.Value)
			}
		})
	}
}

// routerChainReconciler builds a reconciler over a seeded cluster whose uncached reads are
// counted, so a case can assert what one collection cost and move the world while it runs.
func routerChainReconciler(
	objs []ctrlcli.Object, beforeRead func(served int, obj any),
) (*ModelDeploymentReconciler, *uncachedReadCounter) {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(ctrlcli.Object) []string { return nil }).
		Build()
	reader := &uncachedReadCounter{Reader: cli, beforeRead: beforeRead}

	return &ModelDeploymentReconciler{
		Client: cli, APIReader: reader,
		clock:            time.Now,
		servingViewFetch: healthyServingViewFetch,
	}, reader
}

// healthyServingViewFetch answers every view read with the healthy observer payload, so a case
// about the cluster's reads is never also a case about an unreachable router.
func healthyServingViewFetch(context.Context, string) ([]byte, error) {
	return []byte(observerHealthyPayload), nil
}

// sharedChainRouterObjects gives every Router process the ONE ReplicaSet and Deployment a real
// render produces, rather than the per-process ReplicaSet the single-hop ownership cases mutate
// one at a time. A scaled Deployment's replicas share one ReplicaSet, so this is the shape a
// collection actually meets, and it is the shape in which the chain behind the processes is one
// object rather than one per process.
func sharedChainRouterObjects(
	md *workercore.ModelDeployment, pods ...*corev1.Pod,
) []ctrlcli.Object {
	controller := true
	replicaSet, deployment := ownedRouterChain(pods[0], md)
	objects := make([]ctrlcli.Object, 0, 2+len(pods))
	objects = append(objects, &replicaSet, &deployment)
	for _, pod := range pods {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "ReplicaSet",
			Name:       replicaSet.Name,
			UID:        replicaSet.UID,
			Controller: &controller,
		}}
		objects = append(objects, pod)
	}

	return objects
}

// routerChainFixture seeds the deployment, one shared chain behind every Router process, and the
// one member pod the collected views bind to.
func routerChainFixture(t *testing.T, count int) (*workercore.ModelDeployment, []ctrlcli.Object) {
	t.Helper()

	md, _ := retirementRouterFixture(newRenderDeployment())
	md.Generation = 1
	addresses := []string{"10.0.9.1", "10.0.9.2", "10.0.9.3", "10.0.9.4"}
	routers := make([]*corev1.Pod, 0, count)
	for i := range count {
		router := retirementRouterPod("router-"+strconv.Itoa(i), addresses[i])
		router.Status.PodIP = addresses[i]
		routers = append(routers, router)
	}
	seeded := sharedChainRouterObjects(md, routers...)
	seeded = append(seeded, observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true))

	return md, append([]ctrlcli.Object{md}, seeded...)
}

// TestTheRouterOwningChainIsReadOncePerCollection is the read-amplification contract for the
// other half of this issue: every Router process shares one reading of the chain behind them,
// and the collection pays two more reads to prove that chain did not move underneath it.
//
// THE COUNTS ARE THE CONTRACT. Three processes sharing one ReplicaSet and one Deployment spent
// SIX reads on the chain, two per process, and now spend FOUR whatever the process count: two to
// read the chain once, two to read it again. The Pod reads stay one per process on purpose,
// because the Pod is the object the collection is about, and they are counted separately here so
// the saving is not credited to a check that was never shared.
func TestTheRouterOwningChainIsReadOncePerCollection(t *testing.T) {
	md, seeded := routerChainFixture(t, 3)

	// THE READS ARE COUNTED BY KIND, so the assertions below are about the chain's objects and
	// not about the collection's other traffic.
	ownerReads, podReads := 0, 0
	r, _ := routerChainReconciler(seeded, func(_ int, obj any) {
		switch obj.(type) {
		case *appsv1.ReplicaSet, *appsv1.Deployment:
			ownerReads++
		case *corev1.Pod:
			podReads++
		}
	})

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, nil)
	require.Empty(t, failure, "three processes answer the same view: %s", failure)
	require.Len(t, observations, 3)

	assert.Equal(t, 4, ownerReads,
		"one ReplicaSet and one Deployment read, then one re-read of each to prove neither moved")
	assert.Equal(t, 3, podReads, "every Router process is still read live for itself")
}

// TestTheRouterOwningChainIsHeldWhenItMovesMidCollection is the case a shared read could have
// broken. The chain is verified for the first process, the Deployment is reparented to another
// ModelDeployment while the collection is still running, and the collection must refuse. Read
// once per process, the second process would have seen the reparent itself; read once per
// collection, nothing but the closing bookend stands between it and a confirmed answer.
func TestTheRouterOwningChainIsHeldWhenItMovesMidCollection(t *testing.T) {
	md, seeded := routerChainFixture(t, 3)

	// THE REPARENT LANDS AFTER THE CHAIN HAS BEEN READ AND THE COLLECTION HAS MOVED ON. The
	// next Router process is the one whose read would have covered for the reparent, so the
	// mutation waits for exactly that read.
	reparented, chainRead := false, false
	var cli ctrlcli.Client
	r, _ := routerChainReconciler(seeded, func(_ int, obj any) {
		switch obj.(type) {
		case *appsv1.Deployment:
			chainRead = true
		case *corev1.Pod:
			if !chainRead || reparented || cli == nil {
				return
			}
			reparented = true
			live := new(appsv1.Deployment)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, live))
			live.OwnerReferences[0].UID = "another-model-deployment"
			require.NoError(t, cli.Update(context.Background(), live))
		}
	})
	cli = r.Client

	_, failure := r.collectModelDeploymentRouterObservations(context.Background(), md, nil)

	require.True(t, reparented, "the case did not reparent the owner, so it proves nothing")
	require.NotEmpty(t, failure, "a chain that moved under the collection must refuse it")
	assert.Contains(t, failure, "owning chain changed")
}

// TestTheRouterOwningChainIsHeldWhenAnOwnerIsReplacedMidCollection is the same refusal for the
// other way an owner moves: the same name under a new identity. A same-name replacement is the
// shape that a name-keyed read cannot notice, so it is the one the bookend has to catch.
func TestTheRouterOwningChainIsHeldWhenAnOwnerIsReplacedMidCollection(t *testing.T) {
	md, seeded := routerChainFixture(t, 3)

	replaced, chainRead := false, false
	var cli ctrlcli.Client
	r, _ := routerChainReconciler(seeded, func(_ int, obj any) {
		switch obj.(type) {
		case *appsv1.Deployment:
			chainRead = true
		case *corev1.Pod:
			if !chainRead || replaced || cli == nil {
				return
			}
			replaced = true
			key := ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}
			live := new(appsv1.Deployment)
			require.NoError(t, cli.Get(context.Background(), key, live))
			require.NoError(t, cli.Delete(context.Background(), live.DeepCopy()))
			live.SetUID("a-fresh-router-deployment")
			live.SetResourceVersion("")
			require.NoError(t, cli.Create(context.Background(), live))
		}
	})
	cli = r.Client

	_, failure := r.collectModelDeploymentRouterObservations(context.Background(), md, nil)

	require.True(t, replaced, "the case did not replace the owner, so it proves nothing")
	require.NotEmpty(t, failure, "a replaced chain must refuse the collection")
	assert.Contains(t, failure, "owning chain changed")
}

// TestTheRouterOwningChainIsHeldWhenItsReplicaSetMovesMidCollection is the same refusal one hop
// down: the shared ReplicaSet is reparented to another ModelDeployment while the collection runs.
// A reparent keeps the ReplicaSet's own UID, so identity alone still verifies; the owner is the
// fact the bookend has to compare.
func TestTheRouterOwningChainIsHeldWhenItsReplicaSetMovesMidCollection(t *testing.T) {
	md, seeded := routerChainFixture(t, 3)

	reparented, chainRead := false, false
	var cli ctrlcli.Client
	r, _ := routerChainReconciler(seeded, func(_ int, obj any) {
		switch obj.(type) {
		case *appsv1.ReplicaSet:
			chainRead = true
		case *corev1.Pod:
			if !chainRead || reparented || cli == nil {
				return
			}
			reparented = true
			live := new(appsv1.ReplicaSet)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-0-rs"}, live))
			live.OwnerReferences[0].UID = "another-model-deployment"
			require.NoError(t, cli.Update(context.Background(), live))
		}
	})
	cli = r.Client

	_, failure := r.collectModelDeploymentRouterObservations(context.Background(), md, nil)

	require.True(t, reparented, "the case did not reparent the replica set, so it proves nothing")
	require.NotEmpty(t, failure, "a chain that moved under the collection must refuse it")
	assert.Contains(t, failure, "owning chain changed")
	assert.Contains(t, failure, "changed owner",
		"the refusal names the owner change, not the replacement identity")
}

// TestTheRouterOwningChainSurvivesASameOwnerReplicaSetWrite is the control for the refusal above:
// a ReplicaSet that is written without changing hands is not a moved chain, and the collection
// still returns its views.
func TestTheRouterOwningChainSurvivesASameOwnerReplicaSetWrite(t *testing.T) {
	md, seeded := routerChainFixture(t, 3)

	touched, chainRead := false, false
	var cli ctrlcli.Client
	r, _ := routerChainReconciler(seeded, func(_ int, obj any) {
		switch obj.(type) {
		case *appsv1.ReplicaSet:
			chainRead = true
		case *corev1.Pod:
			if !chainRead || touched || cli == nil {
				return
			}
			touched = true
			live := new(appsv1.ReplicaSet)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-0-rs"}, live))
			live.Labels = map[string]string{"touched-mid-collection": "true"}
			require.NoError(t, cli.Update(context.Background(), live))
		}
	})
	cli = r.Client

	observations, failure := r.collectModelDeploymentRouterObservations(context.Background(), md, nil)

	require.True(t, touched, "the case did not touch the replica set, so it controls nothing")
	require.Empty(t, failure, "an owner that did not change hands is not a moved chain")
	require.Len(t, observations, 3)
}

// servingGroupMember builds one member of a group the per-group facts can read.
//
// THE IDENTITY IS WHAT THE READERS NEED: the workload labels name the role and the seat, the group
// total annotation declares the group's size, and the serving-port annotation marks a command line
// this operator built, which is what makes the member's shape readable at all.
//
// THE MEMBER IS READY, because a group with an unready member is not a group that may be served,
// and the facts read the readiness off the fresh membership. A case that wants an unsafe group
// says so by making the member unready.
func servingGroupMember(
	role string, ordinal, seat, total int, uid types.UID, externalDP bool,
) *corev1.Pod {
	name := fmt.Sprintf("%s-%d-%d", role, ordinal, seat)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			UID:       uid,
			Labels: map[string]string{
				modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:    "qwen",
				modelDeploymentLabelKeyComponent:   role,
				modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
				modelDeploymentMemberIndexLabel:    strconv.Itoa(seat),
			},
			Annotations: map[string]string{
				kueuepodconst.GroupTotalCountAnnotation: strconv.Itoa(total),
				"prometheus.io/port":                    "8000",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    modelDeploymentMainContainerName,
			Command: []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
			Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: observationMemberPort}},
		}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.0.0.5",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if externalDP {
		pod.Spec.Containers[0].Args = []string{
			"--data-parallel-size", strconv.Itoa(total),
			"--data-parallel-rank", strconv.Itoa(seat),
		}
	}

	return pod
}

// servingObservation builds one Router process's observation over the members it holds.
func servingObservation(serving ...types.UID) RouterServingObservation {
	bound := make([]ServingEndpoint, 0, len(serving))
	for _, uid := range serving {
		bound = append(bound, ServingEndpoint{
			PodUID:   uid,
			Listener: observationMemberPort,
			Serving:  true,
		})
	}

	return RouterServingObservation{
		View:        RouterServingView{RouterPodUID: "router-a", RouterBootGeneration: 1},
		CollectedAt: time.Now(),
		Bound:       bound,
	}
}

// TestModelDeploymentServingGroups pins the per-group facts: the group identity is the role, the
// ordinal and the CURRENT membership generation; the counts are members and Router endpoints and
// never the same thing; a partial view is not a confirmation; a coherent zero is; and the retention
// fact is about the answering set and not about the membership.
// servingGroupFacts computes the group facts from a collection, with the collection's own verdict
// gating them, exactly as the reconcile does: a collection that was incomplete confirms no group.
func servingGroupFacts(
	md *workercore.ModelDeployment, members []*corev1.Pod, observations []RouterServingObservation,
) (map[modelDeploymentGroupKey]modelDeploymentGroupServing, map[types.UID]modelDeploymentMemberServing) {
	return modelDeploymentServingGroups(md, members, observations,
		aggregateModelDeploymentServing(true, observations, modelDeploymentServingFreshness))
}

func TestModelDeploymentServingGroups(t *testing.T) {
	t.Parallel()

	md := &workercore.ModelDeployment{}
	md.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	md.Spec.Roles = []workercore.ModelDeploymentRole{{Name: "regular", Replicas: 1, ReplicaSize: 3}}

	// THE GENERATION OF A THREE-MEMBER GROUP, spelled out rather than derived, because the key is
	// the fact under test and a fixture that computed it with the code under test would prove
	// nothing about it.
	const leaderGroupGeneration = "uid-a/uid-b/uid-c"

	leader := servingGroupMember("regular", 0, 0, 3, "uid-a", false)
	followerOne := servingGroupMember("regular", 0, 1, 3, "uid-b", false)
	followerTwo := servingGroupMember("regular", 0, 2, 3, "uid-c", false)

	t.Run("a leader-served group is complete when the router holds its leader alone", func(t *testing.T) {
		t.Parallel()

		members := []*corev1.Pod{leader, followerOne, followerTwo}
		groups, _ := servingGroupFacts(md, members, []RouterServingObservation{
			servingObservation("uid-a"),
		})

		fact, found := groups[modelDeploymentGroupKey{
			Role:       "regular",
			Ordinal:    0,
			Generation: modelDeploymentMembershipGeneration(leaderGroupGeneration),
		}]
		require.True(t, found, "the group is keyed by its current membership generation")
		assert.True(t, fact.SnapshotValid, "the group's view is complete")
		assert.Equal(t, workercore.ModelDeploymentServingStateConfirmed, fact.State)
		assert.Equal(t, int32(1), fact.Endpoints, "one router endpoint, not three members")
		assert.Equal(t, int32(3), fact.Members)
		assert.Equal(t, int32(3), fact.Expected)
		assert.False(t, fact.Retained, "no member carries the eligibility label yet")
	})

	t.Run("an external dp group the router holds one rank of is not a confirmation", func(t *testing.T) {
		t.Parallel()

		rankZero := servingGroupMember("regular", 0, 0, 2, "uid-a", true)
		rankOne := servingGroupMember("regular", 0, 1, 2, "uid-b", true)
		groups, _ := servingGroupFacts(md, []*corev1.Pod{rankZero, rankOne},
			[]RouterServingObservation{servingObservation("uid-a")})

		fact, found := groups[modelDeploymentGroupKey{
			Role:       "regular",
			Ordinal:    0,
			Generation: modelDeploymentMembershipGeneration("uid-a/uid-b"),
		}]
		require.True(t, found, "a partial view still yields an entry")
		assert.False(t, fact.SnapshotValid, "a group seen half is not a group seen")
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, fact.State)
		assert.Contains(t, fact.InvalidReason, "partial")
		assert.False(t, fact.Retained, "no member carries the eligibility label yet")
	})

	// THE RECORDED WEDGE: a gateway that registers one rank of a two-rank group leaves the
	// survivor's view permanently partial, and the partial branch once returned before the
	// retention was computed, so an eligible group of an unchanged generation was held
	// AwaitingObservation forever. The retention is proved by the fresh membership, so it is
	// computed before the view is judged and carries such a group through the half view.
	t.Run("a partial view of a labeled group retains its eligibility", func(t *testing.T) {
		t.Parallel()

		generation := modelDeploymentMembershipGeneration("uid-a/uid-b")
		labeled := func(uid types.UID, seat int) *corev1.Pod {
			pod := servingGroupMember("regular", 0, seat, 2, uid, true)
			pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
			pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = string(generation)

			return pod
		}

		groups, _ := servingGroupFacts(md, []*corev1.Pod{labeled("uid-a", 0), labeled("uid-b", 1)},
			[]RouterServingObservation{servingObservation("uid-a")})

		fact, found := groups[modelDeploymentGroupKey{
			Role: "regular", Ordinal: 0, Generation: generation,
		}]
		require.True(t, found, "a partial view still yields an entry")
		assert.False(t, fact.SnapshotValid, "the view is still half, and that fact is published")
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, fact.State)
		assert.Contains(t, fact.InvalidReason, "partial")
		assert.True(t, fact.Retained,
			"every answering member carries the label at the current generation, so the retention holds")
		assert.True(t, modelDeploymentGroupRestores(true, true, fact),
			"a retained group restores through a partial view instead of waiting on one forever")
	})

	t.Run("a partial view of a group whose generation moved does not retain", func(t *testing.T) {
		t.Parallel()

		pod := servingGroupMember("regular", 0, 0, 2, "uid-a", true)
		pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = "uid-a/uid-old"
		peer := servingGroupMember("regular", 0, 1, 2, "uid-b", true)
		peer.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		peer.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = "uid-a/uid-old"

		groups, _ := servingGroupFacts(md, []*corev1.Pod{pod, peer},
			[]RouterServingObservation{servingObservation("uid-a")})

		fact, found := groups[modelDeploymentGroupKey{
			Role:       "regular",
			Ordinal:    0,
			Generation: modelDeploymentMembershipGeneration("uid-a/uid-b"),
		}]
		require.True(t, found)
		assert.False(t, fact.Retained,
			"the members' annotation names an older generation, so the retention does not apply")
		assert.False(t, modelDeploymentGroupRestores(true, true, fact),
			"a group whose membership moved waits for a complete observation")
	})

	t.Run("a group the router holds none of is valid and observed as empty", func(t *testing.T) {
		t.Parallel()

		members := []*corev1.Pod{leader, followerOne, followerTwo}
		groups, _ := servingGroupFacts(md, members, []RouterServingObservation{
			servingObservation(),
		})

		fact, found := groups[modelDeploymentGroupKey{
			Role:       "regular",
			Ordinal:    0,
			Generation: modelDeploymentMembershipGeneration(leaderGroupGeneration),
		}]
		require.True(t, found)
		assert.True(t, fact.SnapshotValid, "a coherent zero is not a fault")
		assert.Equal(t, workercore.ModelDeploymentServingStateConfirmed, fact.State)
		assert.Equal(t, int32(0), fact.Endpoints)
	})

	t.Run("the retention fact is about the answering set and the current generation", func(t *testing.T) {
		t.Parallel()

		eligible := servingGroupMember("regular", 0, 0, 3, "uid-a", false)
		eligible.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		eligible.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = leaderGroupGeneration
		others := []*corev1.Pod{
			servingGroupMember("regular", 0, 1, 3, "uid-b", false),
			servingGroupMember("regular", 0, 2, 3, "uid-c", false),
		}

		groups, carried := servingGroupFacts(md, append([]*corev1.Pod{eligible}, others...),
			[]RouterServingObservation{servingObservation("uid-a")})
		key := modelDeploymentGroupKey{
			Role:       "regular",
			Ordinal:    0,
			Generation: modelDeploymentMembershipGeneration(leaderGroupGeneration),
		}
		fact, found := groups[key]
		require.True(t, found)
		assert.True(t, fact.Retained,
			"the answering member carries the label and the annotation names the current generation")
		assert.True(t, carried[eligible.UID].Eligible)
		assert.Equal(t, modelDeploymentMembershipGeneration(leaderGroupGeneration),
			carried[eligible.UID].Generation)

		// THE SAME GROUP UNDER AN OLDER GENERATION IS NOT RETAINED, and the followers that carry
		// no label at all do not withdraw it either.
		older := servingGroupMember("regular", 0, 0, 3, "uid-a", false)
		older.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		older.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration] = "uid-a/uid-b"
		groups, _ = servingGroupFacts(md, append([]*corev1.Pod{older}, others...),
			[]RouterServingObservation{servingObservation("uid-a")})
		fact, found = groups[key]
		require.True(t, found)
		assert.False(t, fact.Retained, "an annotation naming an earlier generation proves nothing")
	})
}

// modelDeploymentGroupFixture is a Router-bearing deployment whose group is standing, with the
// pieces a boundary case needs to drive the real collector and the real label writer.
//
// THE COLLECTOR AND THE WRITER ARE THE PRODUCTION ONES. A case that built the answer or the label
// by hand would be proving the shape of the rule rather than the rule.
type modelDeploymentGroupFixture struct {
	md      *workercore.ModelDeployment
	cli     ctrlcli.Client
	members func() []*corev1.Pod
	serving *[]string
	fail    *bool
	failIP  string
	now     *time.Time
	router  *ModelDeploymentReconciler
}

// newModelDeploymentGroupFixture stands up a two-member group and a Router process the collection
// can place. The group answers as an External DP group, where every rank answers, or as an ordinary
// one, where only the leader does.
func newModelDeploymentGroupFixture(t *testing.T, externalDP bool) *modelDeploymentGroupFixture {
	t.Helper()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
		if externalDP {
			md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
		}
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	setEndpointEligibilityPodsReady(t, cli, true)

	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name}, stored))
	stored.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), stored))
	md.Spec.Router = stored.Spec.Router
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	seedRouterProcess(t, cli, md, "router-zero", "10.0.9.1")
	seedRouterProcess(t, cli, md, "router-one", "10.0.9.2")

	fail := false
	fixture := &modelDeploymentGroupFixture{md: md, cli: cli, serving: &[]string{}, fail: &fail}
	fixture.members = func() []*corev1.Pod {
		live := replicaPods(t, cli)
		owned := make([]*corev1.Pod, 0, len(live))
		for i := range live {
			owned = append(owned, &live[i])
		}

		return owned
	}
	for i, pod := range fixture.members() {
		pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		pod.Status.Phase = corev1.PodRunning
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
	now := time.Now()
	fixture.now = &now
	fixture.router = &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock: func() time.Time { return *fixture.now },
		servingViewFetch: func(_ context.Context, url string) ([]byte, error) {
			if *fixture.fail {
				return nil, errors.New("the router did not answer")
			}
			if fixture.failIP != "" && strings.Contains(url, fixture.failIP) {
				return nil, errors.New("the router did not answer")
			}
			live := fixture.members()
			values := make([]corev1.Pod, 0, len(live))
			for _, pod := range live {
				values = append(values, *pod)
			}

			return retirementRouterView(*fixture.serving, values), nil
		},
	}

	return fixture
}

// generation is the membership generation of the group's current members.
func (f *modelDeploymentGroupFixture) generation() modelDeploymentMembershipGeneration {
	uids := make([]string, 0, len(f.members()))
	for _, pod := range f.members() {
		uids = append(uids, string(pod.UID))
	}
	slices.Sort(uids)

	return modelDeploymentMembershipGeneration(strings.Join(uids, "/"))
}

// answering is the set of members the Router reports as serving.
func (f *modelDeploymentGroupFixture) answering(externalDP bool) []string {
	ids := []string{}
	for _, pod := range f.members() {
		if externalDP || pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			ids = append(ids, string(pod.UID))
		}
	}

	return ids
}

// collect reads the group with the real collector.
func (f *modelDeploymentGroupFixture) collect() modelDeploymentPassObservation {
	return modelDeploymentPassObservation{
		Serving: f.router.observeModelDeploymentServing(context.Background(), f.md),
	}
}

// write applies the answer with the real label writer.
func (f *modelDeploymentGroupFixture) write(t *testing.T, observation modelDeploymentPassObservation, refuted bool) {
	t.Helper()

	live := f.members()
	generation := f.generation()
	qualification := modelDeploymentTestQualification(live, generation, refuted)
	byMember := make(map[types.UID]modelDeploymentInstanceQualification, len(live))
	actual := make([]corev1.Pod, 0, len(live))
	for _, pod := range live {
		byMember[pod.UID] = qualification
		actual = append(actual, *pod)
	}
	require.NoError(t, f.router.convergeModelDeploymentEndpointEligibility(
		context.Background(), f.md, actual, byMember, nil, observation))
}

// replaceMember puts a new member in the group's place, so the membership generation moves.
func (f *modelDeploymentGroupFixture) replaceMember(t *testing.T, index string) {
	t.Helper()

	old := f.member(index)
	require.NotNil(t, old)
	replacement := old.DeepCopy()
	replacement.UID = types.UID("replaced-" + index + "-" + old.Name)
	replacement.Name = old.Name + "-replaced"
	replacement.ResourceVersion = ""
	require.NoError(t, f.cli.Delete(context.Background(), old))
	require.NoError(t, f.cli.Create(context.Background(), replacement))
}

// member is the group's member carrying the given member index.
func (f *modelDeploymentGroupFixture) member(index string) *corev1.Pod {
	for _, pod := range f.members() {
		if pod.Labels[modelDeploymentMemberIndexLabel] == index {
			return pod
		}
	}

	return nil
}

// TestTheRestoreDecisionOverTheMembershipAndFreshnessBoundaries pins the boundaries the closure
// names, against the real collector and the real label writer.
//
// A CHANGED MEMBERSHIP GENERATION IS HELD ACROSS TWO CONSECUTIVE UNKNOWN PASSES. The group is not
// restored on a pass that could not see it, and no label or annotation acquires the new generation
// while it is held; a later valid observation restores it and names the generation it belongs to.
//
// AN EXTERNAL DP GROUP RETAINS ONLY WHEN EVERY ANSWERING RANK CARRIES THE LABEL. A follower
// replaced inside such a group is an answering rank that carries nothing, so the group is no longer
// retained; a follower that is merely a follower of an ordinary group never answers, so its silence
// is not a reason to withhold the group.
//
// A NEAR-EXPIRED SNAPSHOT CANNOT GAIN A FRESH LEASE FROM A LATER COLLECTION TIMESTAMP. The answer
// carries the OLDEST view's stamp, so the snapshot is judged on that stamp, and a later collection is
// what makes a new answer rather than a lease on the old one.
func TestTheRestoreDecisionOverTheMembershipAndFreshnessBoundaries(t *testing.T) {
	t.Run("a changed membership is held across two unknown passes and restored by a valid one", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, false)
		*fixture.serving = fixture.answering(false)

		// THE GROUP IS RESTORED AT ITS CURRENT GENERATION.
		observation := fixture.collect()
		fixture.write(t, observation, false)
		first := fixture.generation()
		leader := fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible])
		assert.Equal(t, string(first),
			leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration])

		// A MEMBER IS REPLACED, SO THE GROUP'S GENERATION MOVES.
		fixture.replaceMember(t, "1")
		second := fixture.generation()
		require.NotEqual(t, first, second, "the membership generation moved")

		// TWO CONSECUTIVE PASSES THAT COULD NOT SEE THE GROUP HOLD IT.
		*fixture.fail = true
		for pass := range 2 {
			observation = fixture.collect()
			assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, observation.Serving.State,
				"pass %d could not see the group", pass)
			fixture.write(t, observation, false)
			leader = fixture.member("0")
			require.NotNil(t, leader)
			assert.NotContains(t, leader.Labels, modelDeploymentLabelKeyEndpointEligible,
				"pass %d restored a group it could not see", pass)
			assert.NotContains(t, leader.Annotations, modelDeploymentAnnotationKeyEndpointEligibleGeneration,
				"pass %d stamped the new generation while the group was held", pass)
		}

		// A LATER VALID OBSERVATION RESTORES THE NEW GENERATION.
		*fixture.fail = false
		*fixture.serving = fixture.answering(false)
		observation = fixture.collect()
		fixture.write(t, observation, false)
		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible])
		assert.Equal(t, string(second),
			leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
			"the restored member names the generation it belongs to")
	})

	t.Run("an external DP group retains only when every answering rank carries the label", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, true)
		*fixture.serving = fixture.answering(true)

		// EVERY RANK ANSWERS, AND EVERY RANK IS RESTORED.
		observation := fixture.collect()
		fixture.write(t, observation, false)
		generation := fixture.generation()
		for _, index := range []string{"0", "1"} {
			rank := fixture.member(index)
			require.NotNil(t, rank, "rank %s", index)
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				rank.Labels[modelDeploymentLabelKeyEndpointEligible], "rank %s", index)
			assert.Equal(t, string(generation),
				rank.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration], "rank %s", index)
		}

		// THE OBSERVATION IS LOST, AND EVERY RANK STILL CARRIES THE LABEL AND THE GENERATION, SO THE
		// GROUP IS RETAINED.
		*fixture.fail = true
		observation = fixture.collect()
		fixture.write(t, observation, false)
		for _, index := range []string{"0", "1"} {
			rank := fixture.member(index)
			require.NotNil(t, rank, "rank %s", index)
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				rank.Labels[modelDeploymentLabelKeyEndpointEligible],
				"rank %s is retained while the answer is lost", index)
		}

		// A FOLLOWER LOSES THE LABEL WITHOUT THE MEMBERSHIP MOVING, SO THE GENERATION IS UNCHANGED
		// AND THE LEADER IS STILL FINE. THE GROUP IS NOT RETAINED, BECAUSE THE RETENTION FACT CHECKS
		// EVERY ANSWERING RANK AND NOT THE LEADER ALONE.
		//
		// THE LOSS HAPPENS BEFORE THE COLLECTION, because the retention fact is read from the
		// membership the collection re-read: a member relabelled after that read is not one the
		// answer saw, and the fact says so.
		follower := fixture.member("1")
		require.NotNil(t, follower)
		delete(follower.Labels, modelDeploymentLabelKeyEndpointEligible)
		require.NoError(t, fixture.cli.Update(context.Background(), follower))
		observation = fixture.collect()
		fixture.write(t, observation, false)
		for _, index := range []string{"0", "1"} {
			rank := fixture.member(index)
			require.NotNil(t, rank, "rank %s", index)
			assert.NotContains(t, rank.Labels, modelDeploymentLabelKeyEndpointEligible,
				"rank %s: an answering rank that carries nothing is not a retained group", index)
		}

		// A VALID ANSWER RESTORES EVERY RANK, AND A FOLLOWER REPLACED AFTERWARDS MOVES THE
		// GENERATION, WHICH IS ANOTHER WAY FOR THE GROUP TO STOP BEING RETAINED.
		*fixture.fail = false
		*fixture.serving = fixture.answering(true)
		observation = fixture.collect()
		fixture.write(t, observation, false)
		for _, index := range []string{"0", "1"} {
			rank := fixture.member(index)
			require.NotNil(t, rank, "rank %s", index)
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				rank.Labels[modelDeploymentLabelKeyEndpointEligible], "rank %s", index)
		}

		fixture.replaceMember(t, "1")
		*fixture.fail = true
		observation = fixture.collect()
		fixture.write(t, observation, false)
		for _, index := range []string{"0", "1"} {
			rank := fixture.member(index)
			require.NotNil(t, rank, "rank %s", index)
			assert.NotContains(t, rank.Labels, modelDeploymentLabelKeyEndpointEligible,
				"rank %s: a replaced rank moves the generation and the group is not retained", index)
		}
	})

	t.Run("a leader-only group retains with unlabelled collective followers", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, false)
		*fixture.serving = fixture.answering(false)

		// THE GROUP IS RESTORED, AND ONLY THE LEADER CARRIES THE LABEL: THE FOLLOWER NEVER ANSWERS.
		observation := fixture.collect()
		fixture.write(t, observation, false)
		leader := fixture.member("0")
		follower := fixture.member("1")
		require.NotNil(t, leader)
		require.NotNil(t, follower)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible])
		assert.NotContains(t, follower.Labels, modelDeploymentLabelKeyEndpointEligible,
			"a follower never answers and never carries the key")

		// THE OBSERVATION IS LOST, AND THE UNLABELLED FOLLOWER IS NOT A REASON TO WITHHOLD THE
		// GROUP: ONLY THE ANSWERING MEMBER HAS TO CARRY IT.
		*fixture.fail = true
		observation = fixture.collect()
		fixture.write(t, observation, false)
		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"the group is retained by its answering member alone")
	})

	t.Run("one unusable router does not let the other's view restore the group", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, false)
		*fixture.serving = fixture.answering(false)
		// THE SECOND ROUTER DOES NOT ANSWER, so the collection is INCOMPLETE while the first one's
		// view is a coherent one. The answer's stamp is the first view's, so the freshness gate is
		// satisfied, and the group's own fact is coherent: what stops the restore is the
		// COLLECTION'S VERDICT, not the age of its evidence.
		fixture.failIP = "10.0.9.2"

		observation := fixture.collect()
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, observation.Serving.State,
			"a collection that lost a read is not a confirmed answer")
		require.False(t, observation.Serving.CollectedAt.IsZero(),
			"the answer still carries the oldest view's stamp")

		fixture.write(t, observation, false)
		for _, pod := range fixture.members() {
			if pod.Labels[modelDeploymentMemberIndexLabel] != "0" {
				continue
			}
			assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
				"a group whose observation is unusable is not restored")
			assert.NotContains(t, pod.Annotations, modelDeploymentAnnotationKeyEndpointEligibleGeneration,
				"a group whose observation is unusable acquires no generation")
		}
	})

	t.Run("a near-expired snapshot cannot gain a fresh lease from a later collection", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, false)
		*fixture.serving = fixture.answering(false)

		observation := fixture.collect()
		collected := observation.Serving.CollectedAt
		require.False(t, collected.IsZero(), "the answer carries the oldest view's stamp")

		// NEARLY EXPIRED AND STILL HONORED.
		*fixture.now = collected.Add(modelDeploymentServingFreshness - time.Second)
		fixture.write(t, observation, false)
		leader := fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"a snapshot inside the freshness window is honored")

		// THE GROUP IS NO LONGER RETAINED, SO AN EXPIRED ANSWER HAS NOTHING TO RESTORE.
		leader = fixture.member("0")
		delete(leader.Labels, modelDeploymentLabelKeyEndpointEligible)
		delete(leader.Annotations, modelDeploymentAnnotationKeyEndpointEligibleGeneration)
		require.NoError(t, fixture.cli.Update(context.Background(), leader))

		*fixture.now = collected.Add(modelDeploymentServingFreshness + time.Second)
		fixture.write(t, observation, false)
		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.NotContains(t, leader.Labels, modelDeploymentLabelKeyEndpointEligible,
			"an expired answer restores nothing")

		// A LATER COLLECTION IS WHAT MAKES A FRESH ANSWER, NOT A LEASE ON THE OLD ONE.
		*fixture.now = collected.Add(modelDeploymentServingFreshness + 2*time.Second)
		fresh := fixture.collect()
		assert.True(t, fresh.Serving.CollectedAt.After(collected),
			"a new collection carries a new stamp")
		fixture.write(t, fresh, false)
		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"the new answer restores the group")
	})

	t.Run("a view that was already old when the answer was assembled cannot restore the group", func(t *testing.T) {
		fixture := newModelDeploymentGroupFixture(t, false)
		*fixture.serving = fixture.answering(false)

		// THE VIEW'S STAMP COMES FROM THE INJECTABLE CLOCK AND THE AGGREGATE JUDGES ITS AGE ON
		// THE WALL CLOCK. In production both are the same clock, so a view is old when the answer
		// is assembled when the collection itself took longer than the freshness window. Placing
		// the stamp in the past states the same thing in the fixture's terms, and it is the only
		// way to reach the aggregate's own staleness verdict from here.
		*fixture.now = time.Now().Add(-modelDeploymentServingFreshness - time.Second)
		observation := fixture.collect()
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, observation.Serving.State,
			"a view that was already old makes the answer Unknown")
		assert.Contains(t, observation.Serving.Reason, "stale",
			"the answer says which view was stale")

		// THE GROUP CARRIES NOTHING TO RETAIN, so only a restore could give it the label.
		leader := fixture.member("0")
		require.NotNil(t, leader)
		delete(leader.Labels, modelDeploymentLabelKeyEndpointEligible)
		delete(leader.Annotations, modelDeploymentAnnotationKeyEndpointEligibleGeneration)
		require.NoError(t, fixture.cli.Update(context.Background(), leader))

		// THE WRITE'S OWN FRESHNESS CHECK PASSES HERE, because the injected clock has not moved
		// since the collection. Only the aggregate's verdict can be the reason nothing is restored,
		// which is what this case discriminates.
		fixture.write(t, observation, false)
		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.NotContains(t, leader.Labels, modelDeploymentLabelKeyEndpointEligible,
			"an answer assembled from an old view restores nothing")
	})
}
