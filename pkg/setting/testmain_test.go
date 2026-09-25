package setting

import (
	"context"
	"os"
	"testing"

	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"gpustack.ai/gpustack/pkg/system"
)

// errGet, when set, is what every Get through the loopback client returns, so a case can make a
// settings read fail. The loopback client can be configured only once per process, so the failure
// is switched here rather than by configuring another client.
var errGet error

// TestMain configures the loopback client the package's reads go through.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				if errGet != nil {
					return errGet
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build())
	os.Exit(m.Run())
}
