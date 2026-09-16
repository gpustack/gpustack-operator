// Package settingtest provides helpers for tests that seed the delegated settings Secret
// shared through the loopback client.
package settingtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
)

// MergeDelegatedSettings merges the given keys into the delegated settings Secret shared through
// the loopback client, creating the Secret when absent, and invalidates the settings cache so the
// next read observes the merge. The Secret is shared by every test of the importing package, so
// the merge only ever adds or overwrites the given keys, and the registered cleanup removes
// exactly those keys again (invalidating the cache once more) — never the whole Secret: creating
// the Secret outright collides with any test that seeded it first, and deleting it would drop
// keys other tests wrote.
func MergeDelegatedSettings(t *testing.T, kvs map[string]string) {
	t.Helper()

	ctx := context.Background()
	cli := system.LoopbackCtrlClient.Get()
	key := ctrlcli.ObjectKey{
		Name:      setting.DelegatedSecretName,
		Namespace: setting.DelegatedSecretNamespace,
	}

	sec := &core.Secret{ObjectMeta: meta.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if err := cli.Create(ctx, sec); err != nil {
		require.NoError(t, cli.Get(ctx, key, sec))
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range kvs {
		sec.Data[k] = []byte(v)
	}
	require.NoError(t, cli.Update(ctx, sec))
	setting.InvalidateCache()

	t.Cleanup(func() {
		sec := new(core.Secret)
		if err := cli.Get(ctx, key, sec); err == nil {
			for k := range kvs {
				delete(sec.Data, k)
			}
			_ = cli.Update(ctx, sec)
		}
		setting.InvalidateCache()
	})
}
