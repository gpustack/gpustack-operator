package worker

import (
	"os"
	"testing"

	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/system"
)

// TestMain configures empty loopback clients for the package's reconciler tests: reconcilers read
// settings via ShouldValueBool, which reaches the loopback controller-runtime client, and the
// delivery mode via ShouldValueFromRemote, which reaches the loopback Kubernetes client, so without
// these a setting read would nil-panic. Empty fakes (no delegated Secret) make settings resolve to
// their defaults.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(
		ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
	system.LoopbackKubeClient.Configure(kubefake.NewSimpleClientset())
	// Reconciler-level render assertions are written against the native sidecar shape, which a
	// version at the floor unlocks; the classic shape is asserted at render level instead.
	system.LoopbackKubeVersion.Configure(kubediscovery.Version{
		Major: "1", Minor: "31", GitVersion: "v1.31.0",
	})
	os.Exit(m.Run())
}
