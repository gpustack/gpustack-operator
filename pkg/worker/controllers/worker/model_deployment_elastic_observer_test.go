package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// THE ANCHOR NEGATIVE: a document whose tables parse but whose rank proof is not
// complete must be Unknown with the reason named, never a zero-width world. Every
// other case is calibrated against it, because Unknown is the answer that holds a Pod
// and a false zero is the answer that deletes one.

const (
	observerTestDeployUID  = types.UID("elastic-deploy-uid")
	observerTestDeployName = "qwen"
	observerTestGeneration = 7
	observerTestContainer  = modelDeploymentMainContainerName
	observerTestLabelKey   = modelDeploymentElasticPodUIDLabelKey
)

func intPtr(i int) *int { return &i }

func strPtr(s string) *string { return &s }

// Document construction: strict parser tests go through raw JSON; join tests go
// through the wire structs the parser produces.

func observerTestDocumentJSON(t *testing.T, nodes []elasticRayNodeWire, actors []elasticRayActorWire, pgs []elasticRayPGWire) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema_version":   1,
		"ray_version":      "2.56.1",
		"gcs_address":      "127.0.0.1:6379",
		"collected_at_ms":  1790847987000,
		"nodes":            nodes,
		"actors":           actors,
		"placement_groups": pgs,
		"errors":           []elasticRayErrorWire{},
	})
	require.NoError(t, err)
	return raw
}

func observerTestNode(nodeHex string, alive bool, labels map[string]string) elasticRayNodeWire {
	return elasticRayNodeWire{
		NodeIDHex:          strPtr(nodeHex),
		Alive:              boolPtr(alive),
		NodeManagerAddress: "10.0.0.1",
		Labels:             labels,
		Resources:          map[string]string{},
	}
}

func observerTestActor(id string, state int, class, nodeHex, pgHex string) elasticRayActorWire {
	return elasticRayActorWire{
		ActorIDHex:          strPtr(id),
		State:               intPtr(state),
		StateName:           "STATE",
		ClassName:           class,
		NodeIDHex:           nodeHex,
		PlacementGroupIDHex: pgHex,
	}
}

func observerTestPG(id, name string, state int) elasticRayPGWire {
	return elasticRayPGWire{
		PlacementGroupIDHex: strPtr(id),
		Name:                strPtr(name),
		State:               intPtr(state),
		StateName:           "STATE",
		Bundles:             []elasticRayBundleWire{},
	}
}

func observerEmptyDocument(t *testing.T) []byte {
	t.Helper()
	return observerTestDocumentJSON(t, []elasticRayNodeWire{}, []elasticRayActorWire{}, []elasticRayPGWire{})
}

func TestParseElasticRayDocument(t *testing.T) {
	t.Run("valid document parses", func(t *testing.T) {
		doc, err := parseElasticRayDocument(observerEmptyDocument(t))
		require.NoError(t, err)
		assert.Equal(t, 1, *doc.SchemaVersion)
		assert.Equal(t, "2.56.1", *doc.RayVersion)
		assert.NotNil(t, doc.Nodes)
		assert.NotNil(t, doc.Actors)
		assert.NotNil(t, doc.PlacementGroups)
	})

	negatives := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name:   "missing nodes table",
			mutate: func(s string) string { return strings.Replace(s, `"nodes":[]`, "", 1) },
		},
		{
			name:   "null actors table",
			mutate: func(s string) string { return strings.Replace(s, `"actors":[]`, `"actors":null`, 1) },
		},
		{
			name:   "unknown field",
			mutate: func(s string) string { return strings.Replace(s, `"errors":[]`, `"errors":[],"extra":1`, 1) },
		},
		{
			name:   "trailing json value",
			mutate: func(s string) string { return s + ` {"schema_version":1}` },
		},
		{
			name: "duplicate json field keeps no silent last value",
			mutate: func(s string) string {
				return strings.Replace(s, `"ray_version":"2.56.1"`, `"ray_version":"2.56.1","ray_version":"2.56.0"`, 1)
			},
		},
		{
			name:   "wrong schema version",
			mutate: func(s string) string { return strings.Replace(s, `"schema_version":1`, `"schema_version":2`, 1) },
		},
		{
			name: "collector recorded a refused table",
			mutate: func(s string) string {
				return strings.Replace(s, `"errors":[]`, `"errors":[{"table":"actors","error":"boom"}]`, 1)
			},
		},
		{
			name: "unknown actor state enum",
			mutate: func(s string) string {
				return strings.Replace(s, `"actors":[]`, `"actors":[{"actor_id_hex":"ab","state":99,"state_name":"X","class_name":"EngineCoreActor","node_id_hex":"11","placement_group_id_hex":"aa"}]`, 1)
			},
		},
		{
			name: "unknown placement group state enum",
			mutate: func(s string) string {
				return strings.Replace(s, `"placement_groups":[]`, `"placement_groups":[{"placement_group_id_hex":"aa","name":"dp_rank_0","state":9,"state_name":"X","bundles":[]}]`, 1)
			},
		},
		{
			name: "actor row without state",
			mutate: func(s string) string {
				return strings.Replace(s, `"actors":[]`, `"actors":[{"actor_id_hex":"ab","class_name":"EngineCoreActor","node_id_hex":"11","placement_group_id_hex":"aa"}]`, 1)
			},
		},
	}
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseElasticRayDocument([]byte(tc.mutate(string(observerEmptyDocument(t)))))
			assert.Error(t, err)
		})
	}
}

// observerHappyWorld is the known-good Ray world: head + master + two GPU workers, one
// dp_rank PG per worker rank, each CREATED with one ALIVE native actor at an alive
// labeled GPU node.
func observerHappyWorld() (string, []elasticRayNodeWire, []elasticRayActorWire, []elasticRayPGWire) {
	const (
		nodeHead   = "11"
		nodeMaster = "22"
		nodeZero   = "33"
		nodeOne    = "44"
		pgZero     = "aa"
		pgOne      = "bb"
	)
	nodes := []elasticRayNodeWire{
		observerTestNode(nodeHead, true, map[string]string{observerTestLabelKey: "head-uid"}),
		observerTestNode(nodeMaster, true, map[string]string{observerTestLabelKey: "master-uid"}),
		observerTestNode(nodeZero, true, map[string]string{observerTestLabelKey: "worker-uid-0"}),
		observerTestNode(nodeOne, true, map[string]string{observerTestLabelKey: "worker-uid-1"}),
	}
	actors := []elasticRayActorWire{
		observerTestActor("a0", elasticRayActorAlive, modelDeploymentElasticActorClassDPMoEEngine, nodeZero, pgZero),
		observerTestActor("a1", elasticRayActorAlive, modelDeploymentElasticActorClassEngineCore, nodeOne, pgOne),
	}
	pgs := []elasticRayPGWire{
		observerTestPG(pgZero, "dp_rank_0", elasticRayPGCreated),
		observerTestPG(pgOne, "dp_rank_1", elasticRayPGCreated),
	}
	return nodeZero, nodes, actors, pgs
}

func observerMembers() []modelDeploymentElasticCapturedMember {
	return []modelDeploymentElasticCapturedMember{
		{PodUID: "head-uid", PodName: "elastic-head", Container: observerTestContainer, Role: modelDeploymentElasticRoleHead},
		{PodUID: "master-uid", PodName: "gpu-master", Container: observerTestContainer, Role: modelDeploymentElasticRoleMaster},
		{PodUID: "worker-uid-0", PodName: "gpu-worker-0", Container: observerTestContainer, Role: modelDeploymentElasticRoleWorker},
		{PodUID: "worker-uid-1", PodName: "gpu-worker-1", Container: observerTestContainer, Role: modelDeploymentElasticRoleWorker},
	}
}

func observerHappyDocument(t *testing.T) *elasticRayDocumentWire {
	t.Helper()
	_, nodes, actors, pgs := observerHappyWorld()
	doc, err := parseElasticRayDocument(observerTestDocumentJSON(t, nodes, actors, pgs))
	require.NoError(t, err)
	return doc
}

// observerScaleDownDocument is the world after a drained width-2→1 resize: the rank-1
// placement group is REMOVED, its actor DEAD on the drained member's node, and rank 0
// still proves width 1. Actor-free is only ever asked in this shape.
func observerScaleDownDocument(t *testing.T) *elasticRayDocumentWire {
	t.Helper()
	doc := observerHappyDocument(t)
	(*doc.PlacementGroups)[1].State = intPtr(elasticRayPGRemoved)
	(*doc.PlacementGroups)[1].StateName = "REMOVED"
	(*doc.Actors)[1].State = intPtr(elasticRayActorDead)
	(*doc.Actors)[1].StateName = "DEAD"
	return doc
}

func TestJoinModelDeploymentElasticRayIdentity(t *testing.T) {
	members := observerMembers()

	t.Run("happy width two with rank map and actor facts", func(t *testing.T) {
		doc := observerHappyDocument(t)
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)

		assert.True(t, obs.WidthKnown, "reason: %s", obs.WidthUnknownReason)
		assert.Equal(t, 2, obs.RayWidth)
		assert.Equal(t, map[int]types.UID{0: "worker-uid-0", 1: "worker-uid-1"}, obs.RankToPodUID)
		assert.True(t, obs.ActorsComplete)
		assert.False(t, obs.ActorFree["worker-uid-0"].ActorFree)
		assert.False(t, obs.ActorFree["worker-uid-1"].ActorFree)
		// A node with no actors at all is vacuously actor-free; the master is never a
		// retirement candidate, but its fact is still a fact.
		assert.True(t, obs.ActorFree["master-uid"].ActorFree)
		// The CPU head is never a width member and never an actor-free candidate.
		assert.NotContains(t, obs.RankToPodUID, 2)
		assert.NotContains(t, obs.ActorFree, types.UID("head-uid"))
		assert.Empty(t, obs.UnknownReasons)
	})

	t.Run("head label alone never counts toward GPU width", func(t *testing.T) {
		doc := observerHappyDocument(t)
		doc.Nodes = &[]elasticRayNodeWire{
			observerTestNode("11", true, map[string]string{observerTestLabelKey: "head-uid"}),
		}
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.False(t, obs.WidthKnown)
		assert.Equal(t, 0, obs.RayWidth)
	})

	t.Run("duplicate captured member uid refuses the whole join", func(t *testing.T) {
		dup := append(observerMembers(), observerMembers()[2])
		obs := joinModelDeploymentElasticRayIdentity(observerHappyDocument(t), dup, 2, 1)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "repeat Pod UID")
	})

	cases := []struct {
		name       string
		mutateDoc  func(*elasticRayDocumentWire)
		wantReason string
	}{
		{
			name: "pending actor on rank zero holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.Actors)[0].State = intPtr(elasticRayActorPendingCreation)
				(*d.Actors)[0].StateName = "PENDING_CREATION"
			},
			wantReason: "rank 0",
		},
		{
			name: "restarting actor on rank one holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.Actors)[1].State = intPtr(elasticRayActorRestarting)
				(*d.Actors)[1].StateName = "RESTARTING"
			},
			wantReason: "rank 1",
		},
		{
			name: "dependencies-unready actor on rank zero holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.Actors)[0].State = intPtr(elasticRayActorDependenciesUnready)
				(*d.Actors)[0].StateName = "DEPENDENCIES_UNREADY"
			},
			wantReason: "rank 0",
		},
		{
			name: "removed placement group holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.PlacementGroups)[0].State = intPtr(elasticRayPGRemoved)
				(*d.PlacementGroups)[0].StateName = "REMOVED"
			},
			wantReason: "rank 0",
		},
		{
			name: "rescheduling placement group holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.PlacementGroups)[1].State = intPtr(elasticRayPGRescheduling)
				(*d.PlacementGroups)[1].StateName = "RESCHEDULING"
			},
			wantReason: "rank 1",
		},
		{
			name: "missing rank placement group holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.PlacementGroups = (*d.PlacementGroups)[1:]
			},
			wantReason: "missing native rank 0",
		},
		{
			name: "actor at dead node holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				for i := range *d.Nodes {
					if *(*d.Nodes)[i].NodeIDHex == "33" {
						(*d.Nodes)[i].Alive = boolPtr(false)
					}
				}
			},
			wantReason: "rank 0",
		},
		{
			name: "foreign node label holds",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.Nodes = append(*d.Nodes, observerTestNode("99", true, map[string]string{observerTestLabelKey: "not-captured-uid"}))
			},
			wantReason: "foreign Pod UID",
		},
		{
			name: "replacement node claim holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.Nodes = append(*d.Nodes, observerTestNode("55", true, map[string]string{observerTestLabelKey: "worker-uid-0"}))
			},
			wantReason: "claimed by two ALIVE Ray nodes",
		},
		{
			name: "unexpected native rank holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.PlacementGroups = append(*d.PlacementGroups, observerTestPG("cc", "dp_rank_4", elasticRayPGCreated))
			},
			wantReason: "unexpected native rank 4",
		},
		{
			name: "non-native alive actor does not satisfy a rank proof",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.Actors)[0].ClassName = "SomeOtherActor"
			},
			wantReason: "rank 0",
		},
		{
			name: "actor on unknown node breaks complete actor identities",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.Actors = append(*d.Actors, observerTestActor("a9", elasticRayActorAlive, modelDeploymentElasticActorClassEngineCore, "ee", "aa"))
			},
			wantReason: "unknown node ee",
		},
		{
			name: "duplicate node id holds",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.Nodes = append(*d.Nodes, (*d.Nodes)[2])
			},
			wantReason: "repeat node id",
		},
		{
			name: "duplicate rank placement group holds width",
			mutateDoc: func(d *elasticRayDocumentWire) {
				duplicate := observerTestPG("cc", "dp_rank_0", elasticRayPGCreated)
				*d.PlacementGroups = append(*d.PlacementGroups, duplicate)
			},
			wantReason: "repeat rank 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := joinModelDeploymentElasticRayIdentity(observerHappyDocument(t), members, 2, 1)
			require.True(t, baseline.WidthKnown)
			doc := observerHappyDocument(t)
			tc.mutateDoc(doc)
			obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
			assert.False(t, obs.WidthKnown, "expected Unknown, got width %d (%s)", obs.RayWidth, obs.WidthUnknownReason)
			assert.Contains(t, obs.WidthUnknownReason, tc.wantReason)
			assert.NotEmpty(t, obs.UnknownReasons)
		})
	}

	actorFreeCases := []struct {
		name      string
		mutateDoc func(*elasticRayDocumentWire)
		uid       types.UID
		wantFree  bool
	}{
		{
			name:      "dead actors alone are actor-free after the resize",
			mutateDoc: func(d *elasticRayDocumentWire) {},
			uid:       "worker-uid-1",
			wantFree:  true,
		},
		{
			name: "still-alive native actor blocks",
			mutateDoc: func(d *elasticRayDocumentWire) {
				(*d.Actors)[1].State = intPtr(elasticRayActorAlive)
				(*d.Actors)[1].StateName = "ALIVE"
			},
			uid:      "worker-uid-1",
			wantFree: false,
		},
		{
			name: "alive NON-native actor also blocks",
			mutateDoc: func(d *elasticRayDocumentWire) {
				*d.Actors = append(*d.Actors, observerTestActor("a7", elasticRayActorAlive, "Leftover", "44", "cc"))
			},
			uid:      "worker-uid-1",
			wantFree: false,
		},
		{
			name: "dangling captured member is not actor-free",
			mutateDoc: func(d *elasticRayDocumentWire) {
				kept := []elasticRayNodeWire{}
				for _, node := range *d.Nodes {
					if node.Labels[observerTestLabelKey] != "worker-uid-1" {
						kept = append(kept, node)
					}
				}
				*d.Nodes = kept
			},
			uid:      "worker-uid-1",
			wantFree: false,
		},
	}
	for _, tc := range actorFreeCases {
		t.Run(tc.name, func(t *testing.T) {
			doc := observerScaleDownDocument(t)
			tc.mutateDoc(doc)
			obs := joinModelDeploymentElasticRayIdentity(doc, members, 1, 1)
			fact, known := obs.ActorFree[tc.uid]
			require.True(t, known)
			assert.Equal(t, tc.wantFree, fact.ActorFree, "reason: %s", fact.Reason)
			if !tc.wantFree {
				assert.NotEmpty(t, fact.Reason)
			}
		})
	}

	t.Run("scale-down world still proves width one", func(t *testing.T) {
		obs := joinModelDeploymentElasticRayIdentity(observerScaleDownDocument(t), members, 1, 1)
		assert.True(t, obs.WidthKnown, "reason: %s", obs.WidthUnknownReason)
		assert.Equal(t, 1, obs.RayWidth)
		assert.Empty(t, obs.UnknownReasons)
	})
}

func TestElasticRayRankOfName(t *testing.T) {
	rank, ok := elasticRayRankOfName("dp_rank_0")
	assert.True(t, ok)
	assert.Equal(t, 0, rank)
	rank, ok = elasticRayRankOfName("dp_rank_17")
	assert.True(t, ok)
	assert.Equal(t, 17, rank)
	for _, bad := range []string{"dp_rank_", "dp_rank_1a", "dp_rank_1x", "dp_rank_-1", "other", "dp_rank_0 "} {
		_, ok = elasticRayRankOfName(bad)
		assert.False(t, ok, bad)
	}
}

// Collector tests bind uncached identities around the exec transport.

type observerDriftReader struct {
	inner     ctrlcli.Reader
	deployGet *int
}

func (r *observerDriftReader) Get(
	ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption,
) error {
	if _, isDeployment := obj.(*workercore.ModelDeployment); isDeployment {
		*r.deployGet++
		if *r.deployGet > 1 {
			// The second (after) read sees a moved generation: drift.
			deployment := &workercore.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:  key.Namespace,
					Name:       key.Name,
					UID:        observerTestDeployUID,
					Generation: observerTestGeneration + 1,
				},
			}
			deployment.DeepCopyInto(obj.(*workercore.ModelDeployment))
			return nil
		}
	}
	return r.inner.Get(ctx, key, obj, opts...)
}

func (r *observerDriftReader) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	return r.inner.List(ctx, list, opts...)
}

func observerPodsAsObjects() []ctrlcli.Object {
	members := observerMembers()
	objects := make([]ctrlcli.Object, 0, len(members))
	for _, member := range members {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      member.PodName,
				Namespace: "default",
				UID:       member.PodUID,
				Labels:    map[string]string{modelDeploymentLabelKeyComponent: string(member.Role)},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: workercore.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       observerTestDeployName,
					UID:        observerTestDeployUID,
					Controller: boolPtr(true),
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: member.Container}}},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  member.Container,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		}
		objects = append(objects, &pod)
	}
	return objects
}

func observerTestObserver(t *testing.T, exec observerFakeExec, reader ctrlcli.Reader) *modelDeploymentElasticObserver {
	t.Helper()
	objects := observerPodsAsObjects()
	deployment := &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       observerTestDeployName,
		UID:        observerTestDeployUID,
		Generation: observerTestGeneration,
	}}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithRuntimeObjects([]runtime.Object{deployment}...).
		WithObjects(objects...).
		Build()
	if reader == nil {
		reader = client
	}
	observer := newModelDeploymentElasticObserver(client, reader, nil, nil)
	observer.exec = modelDeploymentDrainExec(exec)
	return observer
}

type observerFakeExec func(ctx context.Context, pod *corev1.Pod, container string, argv []string) (string, error)

func observerHappyExec(t *testing.T) observerFakeExec {
	t.Helper()
	return func(_ context.Context, pod *corev1.Pod, container string, argv []string) (string, error) {
		require.Equal(t, "elastic-head", pod.Name)
		require.Equal(t, observerTestContainer, container)
		_, nodes, actors, pgs := observerHappyWorld()
		return string(observerTestDocumentJSON(t, nodes, actors, pgs)), nil
	}
}

func observerTestMD() *workercore.ModelDeployment {
	return &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      observerTestDeployName,
	}}
}

func TestElasticObserverObserve(t *testing.T) {
	ctx := context.Background()
	members := observerMembers()

	t.Run("happy observation binds width and rank map", func(t *testing.T) {
		observer := observerTestObserver(t, observerHappyExec(t), nil)
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.True(t, obs.WidthKnown, "reason: %s", obs.WidthUnknownReason)
		assert.Equal(t, 2, obs.RayWidth)
		assert.Equal(t, types.UID("worker-uid-0"), obs.RankToPodUID[0])
		assert.Equal(t, types.UID("worker-uid-1"), obs.RankToPodUID[1])
		assert.False(t, obs.ActorFree["worker-uid-1"].ActorFree)
		assert.True(t, obs.ActorFree[types.UID("master-uid")].ActorFree)
	})

	t.Run("exec argv carries the frozen seam and the script bounds", func(t *testing.T) {
		var gotArgv []string
		exec := func(_ context.Context, _ *corev1.Pod, _ string, argv []string) (string, error) {
			gotArgv = argv
			_, nodes, actors, pgs := observerHappyWorld()
			return string(observerTestDocumentJSON(t, nodes, actors, pgs)), nil
		}
		observer := observerTestObserver(t, exec, nil)
		_, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		joined := strings.Join(gotArgv, " ")
		assert.Contains(t, joined, "--gcs 127.0.0.1:6379")
		assert.Contains(t, joined, "--deadline-seconds 20")
		assert.Contains(t, joined, "--max-output-bytes 6291456")
		assert.Equal(t, "python3", gotArgv[0])
	})

	t.Run("same-name replacement Pod is Unknown", func(t *testing.T) {
		objects := observerPodsAsObjects()
		for _, object := range objects {
			pod := object.(*corev1.Pod)
			if pod.Name == "gpu-worker-0" {
				pod.UID = "replacement-uid"
			}
		}
		deployment := &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       observerTestDeployName,
			UID:        observerTestDeployUID,
			Generation: observerTestGeneration,
		}}
		client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(deployment).WithObjects(objects...).Build()
		observer := newModelDeploymentElasticObserver(client, client, nil, nil)
		observer.exec = modelDeploymentDrainExec(observerHappyExec(t))
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "same-name replacement")
	})

	t.Run("non-running container is Unknown", func(t *testing.T) {
		objects := observerPodsAsObjects()
		for _, object := range objects {
			pod := object.(*corev1.Pod)
			if pod.Name == "gpu-worker-1" {
				pod.Status = corev1.PodStatus{}
			}
		}
		deployment := &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       observerTestDeployName,
			UID:        observerTestDeployUID,
			Generation: observerTestGeneration,
		}}
		client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(deployment).WithObjects(objects...).Build()
		observer := newModelDeploymentElasticObserver(client, client, nil, nil)
		observer.exec = modelDeploymentDrainExec(observerHappyExec(t))
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "not running")
	})

	t.Run("pod owned by a different deployment is Unknown", func(t *testing.T) {
		objects := observerPodsAsObjects()
		for _, object := range objects {
			pod := object.(*corev1.Pod)
			if pod.Name == "gpu-master" {
				pod.OwnerReferences[0].UID = "another-deploy-uid"
			}
		}
		deployment := &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       observerTestDeployName,
			UID:        observerTestDeployUID,
			Generation: observerTestGeneration,
		}}
		client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(deployment).WithObjects(objects...).Build()
		observer := newModelDeploymentElasticObserver(client, client, nil, nil)
		observer.exec = modelDeploymentDrainExec(observerHappyExec(t))
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "not owned by deployment")
	})

	t.Run("transport failure is Unknown", func(t *testing.T) {
		exec := func(_ context.Context, _ *corev1.Pod, _ string, _ []string) (string, error) {
			return "", errors.New("spdy stream cut")
		}
		observer := observerTestObserver(t, exec, nil)
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "ray identity exec failed")
	})

	t.Run("empty body is Unknown, never an empty cluster", func(t *testing.T) {
		exec := func(_ context.Context, _ *corev1.Pod, _ string, _ []string) (string, error) {
			return "", nil
		}
		observer := observerTestObserver(t, exec, nil)
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "refused")
	})

	t.Run("generation drift between the bounding reads is Unknown", func(t *testing.T) {
		client := ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithRuntimeObjects(&workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
				Namespace:  "default",
				Name:       observerTestDeployName,
				UID:        observerTestDeployUID,
				Generation: observerTestGeneration,
			}}).
			WithObjects(observerPodsAsObjects()...).
			Build()
		count := 0
		reader := &observerDriftReader{inner: client, deployGet: &count}
		observer := newModelDeploymentElasticObserver(client, reader, nil, nil)
		observer.exec = modelDeploymentDrainExec(observerHappyExec(t))
		obs, err := observer.Observe(ctx, observerTestMD(), members, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "drifted")
	})

	t.Run("no captured head is Unknown", func(t *testing.T) {
		gpuOnly := []modelDeploymentElasticCapturedMember{members[1], members[2], members[3]}
		observer := observerTestObserver(t, observerHappyExec(t), nil)
		obs, err := observer.Observe(ctx, observerTestMD(), gpuOnly, 2, "")
		require.NoError(t, err)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "no CPU head")
	})
}

// Registered GPU capacity is independent of native rank width,
// and DEAD historical nodes make no claims.

func observerGPUNode(nodeHex string, labels map[string]string, gpu string) elasticRayNodeWire {
	node := observerTestNode(nodeHex, true, labels)
	node.Resources = map[string]string{"GPU": gpu}
	return node
}

func observerDeadNode(nodeHex string, labels map[string]string) elasticRayNodeWire {
	return observerTestNode(nodeHex, false, labels)
}

// observerScaleUpWorld: four captured GPU members (master + three workers) on four ALIVE
// GPU nodes, but only ranks 0 and 1 exist natively — the shape mid-scale-up where the
// kernel needs registered capacity 4 BEFORE the command creates ranks 2 and 3.
func observerScaleUpWorld(t *testing.T) (*elasticRayDocumentWire, []modelDeploymentElasticCapturedMember) {
	t.Helper()
	pgs := []elasticRayPGWire{
		observerTestPG("aa", "dp_rank_0", elasticRayPGCreated),
		observerTestPG("bb", "dp_rank_1", elasticRayPGCreated),
	}
	actors := []elasticRayActorWire{
		observerTestActor("a0", elasticRayActorAlive, modelDeploymentElasticActorClassDPMoEEngine, "33", "aa"),
		observerTestActor("a1", elasticRayActorAlive, modelDeploymentElasticActorClassEngineCore, "44", "bb"),
	}
	nodes := []elasticRayNodeWire{
		observerTestNode("11", true, map[string]string{observerTestLabelKey: "head-uid"}),
		observerGPUNode("22", map[string]string{observerTestLabelKey: "master-uid"}, "1"),
		observerGPUNode("33", map[string]string{observerTestLabelKey: "worker-uid-0"}, "1"),
		observerGPUNode("44", map[string]string{observerTestLabelKey: "worker-uid-1"}, "1"),
		observerGPUNode("55", map[string]string{observerTestLabelKey: "worker-uid-2"}, "1"),
	}
	doc, err := parseElasticRayDocument(observerTestDocumentJSON(t, nodes, actors, pgs))
	require.NoError(t, err)
	members := []modelDeploymentElasticCapturedMember{
		{PodUID: "head-uid", PodName: "elastic-head", Container: observerTestContainer, Role: modelDeploymentElasticRoleHead},
		{PodUID: "master-uid", PodName: "gpu-master", Container: observerTestContainer, Role: modelDeploymentElasticRoleMaster},
		{PodUID: "worker-uid-0", PodName: "gpu-worker-0", Container: observerTestContainer, Role: modelDeploymentElasticRoleWorker},
		{PodUID: "worker-uid-1", PodName: "gpu-worker-1", Container: observerTestContainer, Role: modelDeploymentElasticRoleWorker},
		{PodUID: "worker-uid-2", PodName: "gpu-worker-2", Container: observerTestContainer, Role: modelDeploymentElasticRoleWorker},
	}
	return doc, members
}

func TestJoinRegisteredGPUAndHistory(t *testing.T) {
	t.Run("scale-up observes registered 4 and native width 2 independently", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.True(t, obs.WidthKnown, "width reason: %s", obs.WidthUnknownReason)
		assert.Equal(t, 2, obs.RayWidth)
		assert.True(t, obs.RegisteredGPUKnown, "registered reason: %s", obs.RegisteredGPUUnknownReason)
		assert.Equal(t, 4, obs.RegisteredGPU)
		assert.Empty(t, obs.UnknownReasons)
	})

	t.Run("no captured GPU members is a known registered zero", func(t *testing.T) {
		doc := observerHappyDocument(t)
		headOnly := []modelDeploymentElasticCapturedMember{
			{PodUID: "head-uid", PodName: "elastic-head", Container: observerTestContainer, Role: modelDeploymentElasticRoleHead},
		}
		obs := joinModelDeploymentElasticRayIdentity(doc, headOnly, 0, 1)
		assert.True(t, obs.RegisteredGPUKnown)
		assert.Equal(t, 0, obs.RegisteredGPU)
	})

	t.Run("missing current captured GPU node is Unknown not zero", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		kept := []elasticRayNodeWire{}
		for _, node := range *doc.Nodes {
			if node.Labels[observerTestLabelKey] != "worker-uid-2" {
				kept = append(kept, node)
			}
		}
		*doc.Nodes = kept
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.False(t, obs.RegisteredGPUKnown)
		assert.Contains(t, obs.RegisteredGPUUnknownReason, "gpu-worker-2")
	})

	gpuParseCases := []struct {
		name string
		gpu  string
	}{
		{name: "absent GPU resource", gpu: ""},
		{name: "malformed GPU resource", gpu: "many"},
		{name: "nonfinite GPU resource", gpu: "Inf"},
		{name: "fractional GPU resource", gpu: "1.5"},
		{name: "multiple GPU capacity", gpu: "8"},
	}
	for _, tc := range gpuParseCases {
		t.Run(tc.name+" fails registered capacity closed", func(t *testing.T) {
			doc, members := observerScaleUpWorld(t)
			for i := range *doc.Nodes {
				if (*doc.Nodes)[i].Labels[observerTestLabelKey] == "worker-uid-2" {
					if tc.gpu == "" {
						(*doc.Nodes)[i].Resources = map[string]string{}
					} else {
						(*doc.Nodes)[i].Resources = map[string]string{"GPU": tc.gpu}
					}
				}
			}
			obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
			assert.False(t, obs.RegisteredGPUKnown, "reason: %s", obs.RegisteredGPUUnknownReason)
		})
	}

	t.Run("historical DEAD same-UID node does not duplicate the restarted Pod", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		*doc.Nodes = append(*doc.Nodes, observerDeadNode("66", map[string]string{observerTestLabelKey: "worker-uid-2"}))
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.True(t, obs.WidthKnown, "reason: %s", obs.WidthUnknownReason)
		assert.True(t, obs.RegisteredGPUKnown, "reason: %s", obs.RegisteredGPUUnknownReason)
		assert.Equal(t, 4, obs.RegisteredGPU)
		assert.Empty(t, obs.UnknownReasons)
	})

	t.Run("retired DEAD foreign node with only DEAD actors permits the current world", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		*doc.Nodes = append(*doc.Nodes, observerDeadNode("77", map[string]string{observerTestLabelKey: "retired-foreign-uid"}))
		*doc.Actors = append(*doc.Actors,
			observerTestActor("old0", elasticRayActorDead, modelDeploymentElasticActorClassEngineCore, "77", "aa"),
			observerTestActor("old1", elasticRayActorDead, "Leftover", "77", "bb"))
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.True(t, obs.WidthKnown, "reason: %s", obs.WidthUnknownReason)
		assert.True(t, obs.RegisteredGPUKnown, "reason: %s", obs.RegisteredGPUUnknownReason)
		assert.Empty(t, obs.UnknownReasons)
	})

	t.Run("ALIVE foreign node still holds", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		*doc.Nodes = append(*doc.Nodes, observerGPUNode("77", map[string]string{observerTestLabelKey: "not-captured"}, "1"))
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "foreign Pod UID")
	})

	t.Run("nonterminal actor on a DEAD node holds", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		*doc.Nodes = append(*doc.Nodes, observerDeadNode("77", map[string]string{observerTestLabelKey: "retired-foreign-uid"}))
		*doc.Actors = append(*doc.Actors, observerTestActor("old0", elasticRayActorAlive, "Leftover", "77", "aa"))
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.False(t, obs.WidthKnown)
		assert.Contains(t, obs.WidthUnknownReason, "nonterminal actor old0")
	})

	t.Run("duplicate ALIVE claim blocks actor-free and registered capacity for that uid", func(t *testing.T) {
		doc, members := observerScaleUpWorld(t)
		*doc.Nodes = append(*doc.Nodes, observerGPUNode("66", map[string]string{observerTestLabelKey: "worker-uid-2"}, "1"))
		obs := joinModelDeploymentElasticRayIdentity(doc, members, 2, 1)
		assert.False(t, obs.RegisteredGPUKnown)
		assert.Contains(t, obs.RegisteredGPUUnknownReason, "gpu-worker-2")
		fact := obs.ActorFree["worker-uid-2"]
		assert.False(t, fact.ActorFree)
	})
}

func TestElasticPlacementGroupHistory(t *testing.T) {
	for _, state := range []int{elasticRayPGRemoved, elasticRayPGCreated} {
		t.Run(fmt.Sprintf("old-state-%d", state), func(t *testing.T) {
			doc := observerHappyDocument(t)
			old := observerTestPG("cc", "dp_rank_0", state)
			*doc.PlacementGroups = append(*doc.PlacementGroups, old)
			obs := joinModelDeploymentElasticRayIdentity(doc, observerMembers(), 2, 1)
			require.Equal(t, state == elasticRayPGRemoved, obs.WidthKnown, "removed history must not conflict with a current rank; duplicate current ranks must hold: %s", obs.WidthUnknownReason)
		})
	}
}

func TestElasticNodeTableIncludesHeadAndHistory(t *testing.T) {
	_, nodes, actors, pgs := observerHappyWorld()
	for i := len(nodes); i < 65; i++ {
		nodes = append(nodes, observerTestNode(fmt.Sprintf("%04x", i+128), false, map[string]string{}))
	}
	_, err := parseElasticRayDocument(observerTestDocumentJSON(t, nodes, actors, pgs))
	require.NoError(t, err, "supported width64 needs a CPU head in the bounded node table")
}
