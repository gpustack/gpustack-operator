package cache

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	k8stesting "k8s.io/client-go/testing"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/utils/certs"
)

const (
	testGroup     = "worker"
	testNamespace = "gpustack-system"
	testSecrets   = "secrets"

	// testKey is shaped like the real cache key, which is a colon-joined host, alternative
	// IPs and alternative DNS names, and is therefore not a valid object name.
	testKey      = "gpustack-worker::gpustack-worker.gpustack-system.svc,localhost"
	testOtherKey = "gpustack-worker::localhost:rsa"

	// secretNamePrefix is the prefix the e2e asserts on when counting cached certificates.
	secretNamePrefix = "gpustack-cert-"
)

// newTestCache builds a cache backed by the given fake client.
func newTestCache(t *testing.T, cli *kubefake.Clientset) certs.Cache {
	t.Helper()

	// A fake watch does not implement the initial-events bookmark the watch list protocol
	// requires, so the informer would never report itself synced.
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)

	c, err := NewK8sCache(t.Context(), testGroup, cli.CoreV1().Secrets(testNamespace))
	require.NoError(t, err)
	return c
}

// newLegacySecret builds a cached certificate as an older release wrote it, under a generated
// name instead of a name derived from the key.
func newLegacySecret(key string, value []byte) *core.Secret {
	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      secretNamePrefix + "l3g4cy",
			Annotations: map[string]string{
				k8sManagedNameAnno:     key,
				k8sManagedNameSumAnno:  sumName(key),
				k8sManagedValueSumAnno: sumValue(value),
			},
			Labels: map[string]string{
				k8sManagedLabel:      "true",
				k8sManagedGroupLabel: testGroup,
			},
		},
		Type: core.SecretTypeOpaque,
		Data: map[string][]byte{
			k8sManagedValueKey: value,
		},
	}
}

// listTestSecrets returns every secret living in the tested namespace.
func listTestSecrets(t *testing.T, cli *kubefake.Clientset) []core.Secret {
	t.Helper()
	list, err := cli.CoreV1().Secrets(testNamespace).List(t.Context(), meta.ListOptions{})
	require.NoError(t, err)
	return list.Items
}

// countSecretActions counts the actions of the given verb the cache issued on secrets.
func countSecretActions(cli *kubefake.Clientset, verb string) int {
	var n int
	for _, a := range cli.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == testSecrets {
			n++
		}
	}
	return n
}

// requireCachedValue waits for the given value to be readable, since the read path is served
// by an informer that trails the write by a moment.
func requireCachedValue(t *testing.T, c certs.Cache, key string, want []byte) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := c.Get(t.Context(), key)
		return err == nil && string(got) == string(want)
	}, 3*time.Second, 20*time.Millisecond, "cached value of %q never became %q", key, want)
}

// Test_k8sCache_Put asserts a cached certificate lives in exactly one secret, whatever the
// number of writes, and that an unchanged value is not rewritten.
func Test_k8sCache_Put(t *testing.T) {
	testCases := []struct {
		name            string
		puts            []string
		wantValue       string
		wantUpdateCalls int
	}{
		{
			name:            "creates one secret",
			puts:            []string{"first"},
			wantValue:       "first",
			wantUpdateCalls: 0,
		},
		{
			name:            "updates the same secret in place",
			puts:            []string{"first", "second"},
			wantValue:       "second",
			wantUpdateCalls: 1,
		},
		{
			name:            "skips an unchanged value",
			puts:            []string{"first", "first"},
			wantValue:       "first",
			wantUpdateCalls: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := kubefake.NewSimpleClientset()
			c := newTestCache(t, cli)

			for _, put := range tc.puts {
				require.NoError(t, c.Put(t.Context(), testKey, []byte(put)))
			}

			secs := listTestSecrets(t, cli)
			require.Len(t, secs, 1, "one key must occupy one secret")
			assert.True(t, strings.HasPrefix(secs[0].Name, secretNamePrefix),
				"secret name %q must keep the cached certificate prefix", secs[0].Name)
			assert.Equal(t, core.SecretTypeOpaque, secs[0].Type)
			assert.Equal(t, testGroup, secs[0].Labels[k8sManagedGroupLabel])
			assert.Equal(t, testKey, secs[0].Annotations[k8sManagedNameAnno])
			assert.Equal(t, tc.wantValue, string(secs[0].Data[k8sManagedValueKey]))
			assert.Equal(t, tc.wantUpdateCalls, countSecretActions(cli, "update"), "update calls")

			requireCachedValue(t, c, testKey, []byte(tc.wantValue))
		})
	}
}

// Test_k8sCache_ConcurrentPutsConvergeOnOneSecret drives the concurrent-boot path: several
// replicas cache the same certificate at once and must converge on one secret instead of each
// creating its own and then deleting the others.
func Test_k8sCache_ConcurrentPutsConvergeOnOneSecret(t *testing.T) {
	const writers = 4

	cli := kubefake.NewSimpleClientset()
	c := newTestCache(t, cli)

	var (
		wg   sync.WaitGroup
		errs = make([]error, writers)
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Put(t.Context(), testKey, []byte{byte('a' + i)})
		}()
	}
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i], "writer %d", i)
	}

	secs := listTestSecrets(t, cli)
	require.Len(t, secs, 1, "concurrent writers must converge on one secret")
	assert.True(t, strings.HasPrefix(secs[0].Name, secretNamePrefix))
	assert.Len(t, secs[0].Data[k8sManagedValueKey], 1, "the surviving value must be one writer's")
}

// Test_k8sCache_SeparateCachesShareOneSecret is the same convergence across replicas, each of
// which runs its own cache instance.
func Test_k8sCache_SeparateCachesShareOneSecret(t *testing.T) {
	cli := kubefake.NewSimpleClientset()

	first := newTestCache(t, cli)
	require.NoError(t, first.Put(t.Context(), testKey, []byte("first")))

	second := newTestCache(t, cli)
	require.NoError(t, second.Put(t.Context(), testKey, []byte("second")))

	secs := listTestSecrets(t, cli)
	require.Len(t, secs, 1, "both replicas must write the same secret")
	assert.Equal(t, "second", string(secs[0].Data[k8sManagedValueKey]))

	requireCachedValue(t, first, testKey, []byte("second"))
}

// Test_k8sCache_DistinctKeysGetDistinctSecrets asserts the secret is keyed by the cache key,
// not shared by every certificate of the group.
func Test_k8sCache_DistinctKeysGetDistinctSecrets(t *testing.T) {
	cli := kubefake.NewSimpleClientset()
	c := newTestCache(t, cli)

	require.NoError(t, c.Put(t.Context(), testKey, []byte("first")))
	require.NoError(t, c.Put(t.Context(), testOtherKey, []byte("second")))

	assert.Len(t, listTestSecrets(t, cli), 2)
	requireCachedValue(t, c, testKey, []byte("first"))
	requireCachedValue(t, c, testOtherKey, []byte("second"))
}

// Test_k8sCache_PutLosingCreateFallsBackToUpdate covers the replica that loses the create to a
// peer: it must adopt the peer's secret rather than fail.
func Test_k8sCache_PutLosingCreateFallsBackToUpdate(t *testing.T) {
	cli := kubefake.NewSimpleClientset()

	var once sync.Once
	cli.PrependReactor("create", testSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		var (
			handled bool
			err     error
		)
		once.Do(func() {
			handled = true
			sec, ok := action.(k8stesting.CreateAction).GetObject().(*core.Secret)
			if !ok {
				return
			}
			// The peer created the very same secret first, holding its own value.
			peer := sec.DeepCopy()
			peer.Data = map[string][]byte{k8sManagedValueKey: []byte("peer")}
			if addErr := cli.Tracker().Create(action.GetResource(), peer, action.GetNamespace()); addErr != nil {
				err = addErr
				return
			}
			err = kerrors.NewAlreadyExists(core.Resource(testSecrets), sec.Name)
		})
		return handled, nil, err
	})

	c := newTestCache(t, cli)
	require.NoError(t, c.Put(t.Context(), testKey, []byte("mine")))

	secs := listTestSecrets(t, cli)
	require.Len(t, secs, 1)
	assert.Equal(t, "mine", string(secs[0].Data[k8sManagedValueKey]))
}

// Test_k8sCache_LeavesLegacyDuplicateAlone pins that no secret is deleted on the read or write
// path: a cached certificate an older release wrote under a generated name is ignored, never
// reused and never cleaned up, because deleting look-alikes is what made replicas delete each
// other's certificates.
func Test_k8sCache_LeavesLegacyDuplicateAlone(t *testing.T) {
	legacy := newLegacySecret(testKey, []byte("legacy"))
	cli := kubefake.NewSimpleClientset(legacy)
	c := newTestCache(t, cli)

	require.NoError(t, c.Put(t.Context(), testKey, []byte("current")))
	requireCachedValue(t, c, testKey, []byte("current"))

	secs := listTestSecrets(t, cli)
	require.Len(t, secs, 2, "the legacy secret must be left in place")
	for i := range secs {
		if secs[i].Name != legacy.Name {
			continue
		}
		assert.Equal(t, "legacy", string(secs[i].Data[k8sManagedValueKey]),
			"the legacy secret must not be written to")
	}
	assert.Zero(t, countSecretActions(cli, "delete"), "no secret may be deleted")
}

// Test_k8sCache_Delete asserts a deleted key reports a cache miss.
func Test_k8sCache_Delete(t *testing.T) {
	cli := kubefake.NewSimpleClientset()
	c := newTestCache(t, cli)

	require.NoError(t, c.Put(t.Context(), testKey, []byte("first")))
	requireCachedValue(t, c, testKey, []byte("first"))

	require.NoError(t, c.Delete(t.Context(), testKey))
	assert.Empty(t, listTestSecrets(t, cli))

	require.Eventually(t, func() bool {
		_, err := c.Get(t.Context(), testKey)
		return errors.Is(err, certs.ErrCacheMiss)
	}, 3*time.Second, 20*time.Millisecond, "deleted key never became a cache miss")
}
