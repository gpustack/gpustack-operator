package worker

import (
	"os"
	"testing"

	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
)

// TestMain configures an empty loopback client for the package's webhook tests: validation reads
// settings via ShouldValueBool, which reaches the loopback client, so without this a setting read
// would nil-panic. An empty fake (no delegated Secret) makes settings resolve to their defaults.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(
		ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
	os.Exit(m.Run())
}
