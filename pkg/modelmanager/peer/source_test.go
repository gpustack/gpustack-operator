package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

const testNamespace = "gpustack-system"

func digestFor(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func pluginPod(name, node, ip string) *core.Pod {
	return pluginPodWithPorts(name, node, ip, true)
}

// pluginPodWithPorts leaves the peer port undeclared when asked, the shape of an older plugin
// that has nothing listening there.
func pluginPodWithPorts(name, node, ip string, withPeerPort bool) *core.Pod {
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Name: name, Namespace: testNamespace,
		Labels: map[string]string{deviceplugin.ComponentLabelKey: "model-manager"},
	}}
	p.Spec.NodeName = node
	p.Status.PodIP = ip
	if withPeerPort {
		p.Spec.Containers = []core.Container{{Ports: []core.ContainerPort{{
			Name: peerPortName, ContainerPort: 32446,
		}}}}
	}
	p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}

	return p
}

func entry(digest string, state workercore.NodeModelStoreModelState) workercore.NodeModelStoreModel {
	return workercore.NodeModelStoreModel{Digest: digest, State: state}
}

func storeFor(node string, models ...workercore.NodeModelStoreModel) *workercore.NodeModelStore {
	return &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Status:     workercore.NodeModelStoreStatus{Models: models},
	}
}

func TestDiscoverFindsReadyNodesButNotSelf(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer func() { ts.Close() }()

	// node-1 is self; node-2 holds the digest ready; node-3 is downloading it; node-4 holds
	// another digest; node-5's Pod has no address yet.
	objs := []ctrlcli.Object{
		storeFor("node-1", entry(digestFor('a'), workercore.NodeModelStoreModelStateReady)),
		storeFor("node-2",
			entry(digestFor('a'), workercore.NodeModelStoreModelStateReady),
			entry(digestFor('b'), workercore.NodeModelStoreModelStateReady)),
		storeFor("node-3", entry(digestFor('a'), workercore.NodeModelStoreModelStateDownloading)),
		storeFor("node-4", entry(digestFor('b'), workercore.NodeModelStoreModelStateReady)),
		storeFor("node-5", entry(digestFor('a'), workercore.NodeModelStoreModelStateReady)),
		pluginPod("pod-2", "node-2", "10.0.0.2"),
		pluginPod("pod-3", "node-3", "10.0.0.3"),
		pluginPod("pod-4", "node-4", "10.0.0.4"),
		pluginPod("pod-5", "node-5", ""),
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	opts := DiscoverOptions{
		Namespace: testNamespace, SelfNode: "node-1", Port: 32445, StreamsPerSource: 2, Client: ts.Client(),
	}

	sources, err := Discover(context.Background(), reader, opts, digestFor('a'))
	require.NoError(t, err)
	require.Len(t, sources, 1, "only node-2 holds the digest ready and answers at an address")
	assert.Equal(t, "node-2", sources[0].NodeName)
	assert.Equal(t, "https://10.0.0.2:32445", sources[0].BaseURL)

	sources, err = Discover(context.Background(), reader, opts, digestFor('c'))
	require.NoError(t, err)
	assert.Empty(t, sources, "a digest nobody holds comes back with no sources")
}

func TestDiscoverSkipsPodsWithoutThePeerPort(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer func() { ts.Close() }()

	// node-2 holds the digest ready and its Pod is ready at an address, but it is an older
	// plugin that declares no peer port: dialing it would only fail.
	objs := []ctrlcli.Object{
		storeFor("node-2", entry(digestFor('a'), workercore.NodeModelStoreModelStateReady)),
		pluginPodWithPorts("pod-2", "node-2", "10.0.0.2", false),
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	opts := DiscoverOptions{
		Namespace: testNamespace, SelfNode: "node-1", Port: 32445, StreamsPerSource: 2, Client: ts.Client(),
	}

	sources, err := Discover(context.Background(), reader, opts, digestFor('a'))
	require.NoError(t, err)
	assert.Empty(t, sources, "a plugin without the peer port is not a candidate")
}

func TestDiscoverRefusesADigestThatIsNotOne(t *testing.T) {
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	_, err := Discover(context.Background(), reader, DiscoverOptions{Namespace: testNamespace}, "not-a-digest")
	require.Error(t, err)
}
