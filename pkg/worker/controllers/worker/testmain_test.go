package worker

import (
	"os"
	"testing"

	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/system"
)

// TestMain configures an empty loopback client for the package's reconciler tests:
// reconcilers read settings via ShouldValueBool, which reaches the loopback client,
// so without this a setting read would nil-panic. An empty fake (no delegated Secret)
// makes settings resolve to their defaults.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(
		ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
	// Reconciler-level render assertions are written against the native sidecar shape, which a
	// version at the floor unlocks; the classic shape is asserted at render level instead.
	system.LoopbackKubeVersion.Configure(kubediscovery.Version{
		Major: "1", Minor: "31", GitVersion: "v1.31.0",
	})
	os.Exit(m.Run())
}
