package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authn "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/webhook"
)

const (
	testPluginNamespace = "gpustack-system"
	testPluginSA        = "gpustack-operator-model-manager"
	testPluginUser      = "system:serviceaccount:" + testPluginNamespace + ":" + testPluginSA
)

func testPluginPod(name, uid, node string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Namespace: testPluginNamespace, Name: name, UID: types.UID(uid)},
		Spec:       core.PodSpec{NodeName: node},
	}
}

// statusContext is ctx carrying a status UPDATE from username with the given user extra.
func statusContext(username, sub string, extra map[string]authn.ExtraValue) context.Context {
	return ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation:   admissionv1.Update,
			SubResource: sub,
			UserInfo:    authn.UserInfo{Username: username, Extra: extra},
		},
	})
}

func podExtra(name, uid string) map[string]authn.ExtraValue {
	return map[string]authn.ExtraValue{
		"authentication.kubernetes.io/pod-name": {name},
		"authentication.kubernetes.io/pod-uid":  {uid},
	}
}

func TestNodeModelStoreStatusGuard(t *testing.T) {
	withNode := func(extra map[string]authn.ExtraValue, node string) map[string]authn.ExtraValue {
		extra["authentication.kubernetes.io/node-name"] = authn.ExtraValue{node}
		return extra
	}
	cases := []struct {
		name     string
		username string
		sub      string
		extra    map[string]authn.ExtraValue
		deleting bool
		wantRule string
	}{
		{
			name: "the plugin Pod on the node, found by its Pod (Kubernetes 1.29)", username: testPluginUser, sub: "status",
			extra: podExtra("mm-node-1", "uid-1"),
		},
		{
			name: "the plugin Pod on the node, by the token's node (Kubernetes 1.30)", username: testPluginUser, sub: "status",
			extra: withNode(podExtra("mm-gone", "uid-x"), "node-1"),
		},
		{
			name: "the plugin Pod on another node", username: testPluginUser, sub: "status",
			extra: podExtra("mm-node-2", "uid-2"), wantRule: nodeModelStoreRuleNode,
		},
		{
			name: "the plugin with a token naming another node", username: testPluginUser, sub: "status",
			extra: withNode(podExtra("mm-node-1", "uid-1"), "node-2"), wantRule: nodeModelStoreRuleNode,
		},
		{
			name: "a Pod name with another Pod's UID", username: testPluginUser, sub: "status",
			extra: podExtra("mm-node-1", "uid-2"), wantRule: nodeModelStoreRuleBinding,
		},
		{
			name: "a Pod that no longer exists", username: testPluginUser, sub: "status",
			extra: podExtra("mm-gone", "uid-x"), wantRule: nodeModelStoreRuleBinding,
		},
		{
			name: "a legacy token without Pod extras", username: testPluginUser, sub: "status",
			wantRule: nodeModelStoreRuleBinding,
		},
		{
			name: "another identity", username: "system:serviceaccount:team-a:default", sub: "status",
			extra: podExtra("mm-node-1", "uid-1"), wantRule: nodeModelStoreRuleIdentity,
		},
		{
			name: "the worker", username: "system:serviceaccount:gpustack-system:gpustack-operator-worker", sub: "status",
			wantRule: nodeModelStoreRuleIdentity,
		},
		{
			name: "another identity on a Terminating object", username: "kubernetes-admin", sub: "status",
			deleting: true, wantRule: nodeModelStoreRuleIdentity,
		},
		{name: "the worker writing spec is not this rule's", username: "system:serviceaccount:gpustack-system:gpustack-operator-worker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
				testPluginPod("mm-node-1", "uid-1", "node-1"),
				testPluginPod("mm-node-2", "uid-2", "node-2"),
			).Build()
			wh := &NodeModelStoreWebhook{Reader: reader, Namespace: testPluginNamespace, ServiceAccount: testPluginSA}

			nms := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
			if c.deleting {
				now := meta.Now()
				nms.DeletionTimestamp = &now
			}
			_, err := wh.ValidateUpdate(statusContext(c.username, c.sub, c.extra), nms.DeepCopy(), nms)
			if c.wantRule == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantRule)
		})
	}
}

func TestNodeModelStoreStatusGuardHoldsDuringDeletion(t *testing.T) {
	_, ok := any(new(NodeModelStoreWebhook)).(webhook.ReceiveDeletionUpdate)
	assert.True(t, ok)
}
