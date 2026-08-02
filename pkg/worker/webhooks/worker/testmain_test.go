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
// would nil-panic.
//
// The fake holds no delegated Secret, so every read fails and ShouldValueBool yields false — not the
// setting's declared default, which ValueBool drops along with the error. That happens to coincide
// with the default for the two host-access gates, which is why they read correctly here. A test
// covering a setting whose default is true cannot rely on this client and must seed the Secret.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(
		ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
	os.Exit(m.Run())
}
