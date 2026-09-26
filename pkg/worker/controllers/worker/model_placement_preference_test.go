package worker

import (
	"context"
	"fmt"
	rand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/report"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// placementStore builds a node's NodeModelStore holding testArtifactDigest in state, with its Ready
// condition at ready.
func placementStore(node string, state workercore.NodeModelStoreModelState, ready meta.ConditionStatus) *workercore.NodeModelStore {
	nms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{Digest: testArtifactDigest, State: state}},
		},
	}
	if ready != "" {
		nms.Status.Conditions = []gpustack.Condition{{Type: "Ready", Status: ready}}
	}

	return nms
}

func placementCSINode(node string, registered bool) *storage.CSINode {
	csiNode := &storage.CSINode{ObjectMeta: meta.ObjectMeta{Name: node}}
	if registered {
		csiNode.Spec.Drivers = []storage.CSINodeDriver{{Name: modelstore.DriverName, NodeID: node}}
	}

	return csiNode
}

func placementNode(node, hostname string) *core.Node {
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: node}}
	if hostname != "" {
		nd.Labels = map[string]string{core.LabelHostname: hostname}
	}

	return nd
}

// hotNode is every object that makes node a candidate for testArtifactDigest.
func hotNode(node string) []ctrlcli.Object {
	return []ctrlcli.Object{
		placementStore(node, workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
		placementCSINode(node, true),
		placementNode(node, node),
	}
}

func TestModelPlacementPreferenceCandidates(t *testing.T) {
	cases := []struct {
		name string
		objs []ctrlcli.Object
		want []string
	}{
		{name: "every condition holds", objs: hotNode("n1"), want: []string{"n1"}},
		{
			name: "the digest is still downloading",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateDownloading, meta.ConditionTrue),
				placementCSINode("n1", true), placementNode("n1", "n1"),
			},
		},
		{
			name: "the digest failed",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateFailed, meta.ConditionTrue),
				placementCSINode("n1", true), placementNode("n1", "n1"),
			},
		},
		{
			name: "the store is not Ready",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionFalse),
				placementCSINode("n1", true), placementNode("n1", "n1"),
			},
		},
		{
			name: "the store has no Ready condition",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, ""),
				placementCSINode("n1", true), placementNode("n1", "n1"),
			},
		},
		{
			name: "a stale store: Ready, and the driver is no longer in CSINode",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
				placementCSINode("n1", false), placementNode("n1", "n1"),
			},
		},
		{
			name: "the node has no CSINode",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
				placementNode("n1", "n1"),
			},
		},
		{
			name: "the Node is gone",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
				placementCSINode("n1", true),
			},
		},
		{
			name: "the Node has no hostname label",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
				placementCSINode("n1", true), placementNode("n1", ""),
			},
		},
		{
			name: "the hostname label differs from the Node name",
			objs: []ctrlcli.Object{
				placementStore("n1", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue),
				placementCSINode("n1", true), placementNode("n1", "host-1.example"),
			},
			want: []string{"host-1.example"},
		},
		{
			name: "another digest on the node",
			objs: []ctrlcli.Object{
				&workercore.NodeModelStore{
					ObjectMeta: meta.ObjectMeta{Name: "n1"},
					Status: workercore.NodeModelStoreStatus{
						Models: []workercore.NodeModelStoreModel{
							{Digest: "sha256:" + strings.Repeat("2", 64), State: workercore.NodeModelStoreModelStateReady},
						},
						Conditions: []gpustack.Condition{{Type: "Ready", Status: meta.ConditionTrue}},
					},
				},
				placementCSINode("n1", true), placementNode("n1", "n1"),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := newModelDeploymentClient(c.objs...)

			term := modelPlacementPreference(context.Background(), cli, testArtifactDigest)
			if c.want == nil {
				assert.Nil(t, term, "no candidate adds no term")
				return
			}
			require.NotNil(t, term)
			assert.Equal(t, int32(modelPlacementPreferenceWeight), term.Weight)
			assert.Equal(t, []core.NodeSelectorRequirement{
				{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: c.want},
			}, term.Preference.MatchExpressions)
		})
	}
}

func TestModelPlacementPreferenceOrderAndCap(t *testing.T) {
	hour := func(h int) *meta.Time {
		return &meta.Time{Time: time.Date(2026, 9, 26, h, 0, 0, 0, time.UTC)}
	}
	// Twenty candidates: n00 and n01 are mounted now, n02..n19 were last used at hour 19-i, and
	// n05 and n06 share an hour so the name decides between them.
	stores := make([]workercore.NodeModelStore, 0, 20)
	for i := range 20 {
		m := workercore.NodeModelStoreModel{Digest: testArtifactDigest, State: workercore.NodeModelStoreModelStateReady}
		switch {
		case i < 2:
			m.Referenced = true
		case i == 6:
			m.LastUsedTime = hour(19 - 5)
		default:
			m.LastUsedTime = hour(19 - i)
		}
		stores = append(stores, workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: fmt.Sprintf("n%02d", i)},
			Status:     workercore.NodeModelStoreStatus{Models: []workercore.NodeModelStoreModel{m}},
		})
	}
	want := []string{"n00", "n01", "n02", "n03", "n04", "n05", "n06", "n07", "n08", "n09", "n10", "n11", "n12", "n13", "n14", "n15"}

	for seed := range uint64(5) {
		shuffled := append([]workercore.NodeModelStore(nil), stores...)
		rand.New(rand.NewPCG(seed, seed)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		candidates := make([]modelPlacementCandidate, 0, len(shuffled))
		for i := range shuffled {
			candidates = append(candidates, modelPlacementCandidate{
				hostname: shuffled[i].Name, model: shuffled[i].Status.Models[0], node: shuffled[i].Name,
			})
		}

		assert.Equal(t, want, modelPlacementHostnames(candidates), "seed %d", seed)
	}
}

func TestModelPlacementPreferenceInjection(t *testing.T) {
	term := &core.PreferredSchedulingTerm{Weight: modelPlacementPreferenceWeight, Preference: core.NodeSelectorTerm{
		MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: []string{"n1"}}},
	}}
	existing := core.PreferredSchedulingTerm{Weight: 10, Preference: core.NodeSelectorTerm{
		MatchExpressions: []core.NodeSelectorRequirement{{Key: "zone", Operator: core.NodeSelectorOpIn, Values: []string{"z1"}}},
	}}
	cases := []struct {
		name string
		pod  func() *core.Pod
		term *core.PreferredSchedulingTerm
		want []core.PreferredSchedulingTerm
	}{
		{
			name: "a Pod without affinity takes the term", pod: func() *core.Pod { return &core.Pod{} }, term: term,
			want: []core.PreferredSchedulingTerm{*term},
		},
		{
			name: "an existing term is kept",
			pod: func() *core.Pod {
				return &core.Pod{Spec: core.PodSpec{Affinity: &core.Affinity{NodeAffinity: &core.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{existing},
				}}}}
			},
			term: term, want: []core.PreferredSchedulingTerm{existing, *term},
		},
		{name: "no term adds nothing", pod: func() *core.Pod { return &core.Pod{} }},
		{
			name: "a Pod asking for a preferred topology level is left alone",
			pod: func() *core.Pod {
				return &core.Pod{ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{
					kueue.PodSetPreferredTopologyAnnotation: core.LabelHostname,
				}}}
			},
			term: term,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := c.pod()
			injectModelPlacementPreference(pod, c.term)
			var got []core.PreferredSchedulingTerm
			if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
				got = pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestModelPlacementPreferenceResolve(t *testing.T) {
	driver := &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}
	cases := []struct {
		name     string
		delivery string
		nodeOnly bool
		objs     []ctrlcli.Object
		want     bool
	}{
		{name: "Node delivery with a hot node", delivery: "Node", objs: append(hotNode("n1"), artifactFixture("", true, true), driver), want: true},
		{
			name: "an Instance's hub artifact is Node delivered", delivery: "Engine", nodeOnly: true,
			objs: append(hotNode("n1"), artifactFixture("", true, true), driver), want: true,
		},
		{name: "Node delivery without a hot node", delivery: "Node", objs: []ctrlcli.Object{artifactFixture("", true, true), driver}},
		{name: "Engine delivery", delivery: "Engine", objs: append(hotNode("n1"), artifactFixture("", true, true), driver)},
		{name: "Node delivery without the plugin is blocked", delivery: "Node", objs: append(hotNode("n1"), artifactFixture("", true, true))},
		{name: "an artifact that lost access is blocked", delivery: "Node", objs: append(hotNode("n1"), artifactFixture("", true, false), driver)},
		{
			name: "a claim", delivery: "Node",
			objs: append(hotNode("n1"), artifactFixture("models", true, true), claimFixture(core.ClaimBound, "local", core.ReadOnlyMany),
				volumeFixture("")),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := modelArtifactDeliveryMode
			t.Cleanup(func() { modelArtifactDeliveryMode = orig })
			modelArtifactDeliveryMode = func(context.Context) string { return c.delivery }
			cli := newModelDeploymentClient(c.objs...)

			w, err := resolveModelArtifactWeights(context.Background(), cli, "team-a", "qwen", 1, c.nodeOnly)
			require.NoError(t, err)
			term := w.placementPreference(context.Background(), cli)
			if !c.want {
				assert.Nil(t, term)
				return
			}
			require.NotNil(t, term)
			assert.Equal(t, []string{"n1"}, term.Preference.MatchExpressions[0].Values)
		})
	}
}

// TestModelPlacementPreferenceReadsThePluginsReadyCondition pins the condition type the candidate
// filter reads to the one the plugin writes. A rename on the writer's side would otherwise make every
// node silently lose its preference, with nothing failing.
func TestModelPlacementPreferenceReadsThePluginsReadyCondition(t *testing.T) {
	assert.Equal(t, report.ConditionReady, nodeModelStoreConditionReady)
}
