package report

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

var testNow = time.Date(2026, 9, 25, 6, 42, 17, 0, time.UTC)

func hexOf(c byte) string { return strings.Repeat(string(c), 64) }

type testEnv struct {
	r      *Reporter
	cli    ctrlcli.Client
	store  *store.Store
	writes *atomic.Int64
	usage  gc.Usage
	// usageFails is how many of the next usage reads fail.
	usageFails int
	running    map[string]bool
}

func validSpec() workercore.NodeModelStoreSpec {
	return workercore.NodeModelStoreSpec{
		Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
		Download:   workercore.NodeModelStoreDownload{Concurrency: 8},
		Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
	}
}

func newTestEnv(t *testing.T, spec workercore.NodeModelStoreSpec, objs ...ctrlcli.Object) *testEnv {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)

	env := &testEnv{store: st, writes: new(atomic.Int64), usage: gc.Usage{Total: 1000, Used: 420}, running: map[string]bool{}}
	nms := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1", Generation: 3}, Spec: spec}
	env.cli = ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.NodeModelStore{}).
		WithObjects(append(objs, nms)...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
				env.writes.Add(1)
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()

	collector := &gc.Collector{
		Store: st,
		Usage: func() (gc.Usage, error) {
			if env.usageFails > 0 {
				env.usageFails--
				return gc.Usage{}, errors.New("statfs: input/output error")
			}
			return env.usage, nil
		},
		Referenced:  func() (map[string]bool, error) { return map[string]bool{}, nil },
		Downloading: func() map[string]bool { return env.running },
		Now:         func() time.Time { return testNow },
	}
	env.r = &Reporter{
		Reader: env.cli, Client: env.cli, NodeName: "node-1", Namespace: "gpustack-system",
		Store: st, Collector: collector, Downloading: func() map[string]bool { return env.running },
		Downloader: download.New(nil, 1, 0), Now: func() time.Time { return testNow },
	}
	collector.Watermarks = env.r.Watermarks

	return env
}

func (e *testEnv) publish(t *testing.T, hex string, size int64) {
	t.Helper()
	a, err := e.store.NewAttempt(hex)
	require.NoError(t, err)
	p, err := a.FilePath("model.bin")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	require.NoError(t, a.Publish(store.Marker{Digest: "sha256:" + hex, SizeBytes: size, PublishedTime: testNow}))
}

func (e *testEnv) nms(t *testing.T) *workercore.NodeModelStore {
	t.Helper()
	nms := new(workercore.NodeModelStore)
	require.NoError(t, e.cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	return nms
}

func findCondition(conds []gpustack.Condition, typ string) gpustack.Condition {
	for _, c := range conds {
		if c.Type == typ {
			return c
		}
	}
	return gpustack.Condition{}
}

func TestReportWritesWhatTheNodeHolds(t *testing.T) {
	env := newTestEnv(t, validSpec())
	env.publish(t, hexOf('a'), 300)
	env.publish(t, hexOf('b'), 100)
	require.NoError(t, env.store.WriteRef(store.Ref{VolumeID: "csi-1", TargetPath: "/t", Hex: hexOf('a'), PodNamespace: "team-a", PodName: "p"}))
	env.r.Mounted(hexOf('a'))
	env.running[hexOf('c')] = true
	require.NoError(t, env.store.WriteDigest(hexOf('d'), store.DigestRecord{
		Failures: 1, Reason: download.ReasonIntegrityMismatch, Message: "config.json: hash mismatch", RetryTime: testNow.Add(time.Minute),
	}))
	require.NoError(t, env.store.WriteDigest(hexOf('e'), store.DigestRecord{Reason: download.ReasonCanceled, RetryTime: testNow}))

	require.NoError(t, env.r.Report(context.Background()))
	st := env.nms(t).Status

	assert.Equal(t, int64(3), st.ObservedGeneration)
	require.NotNil(t, st.Capacity)
	assert.Equal(t, workercore.NodeModelStoreCapacity{TotalBytes: 1000, StoredBytes: 400, UsedPercent: 40}, *st.Capacity,
		"42% used reports as 40, a multiple of 5")
	require.Len(t, st.Models, 4, "a cancellation is not listed as a failure")
	first := st.Models[0]
	assert.Equal(t, "sha256:"+hexOf('a'), first.Digest, "the referenced entry comes first")
	assert.Equal(t, workercore.NodeModelStoreModelStateReady, first.State)
	assert.Equal(t, int64(300), first.SizeBytes)
	assert.True(t, first.Referenced)
	require.NotNil(t, first.LastUsedTime)
	assert.True(t, first.LastUsedTime.Time.Equal(testNow.Truncate(time.Hour)), "its use is truncated to the hour")
	states := map[string]workercore.NodeModelStoreModelState{}
	for _, m := range st.Models {
		states[m.Digest] = m.State
		if m.State == workercore.NodeModelStoreModelStateFailed {
			assert.Equal(t, download.ReasonIntegrityMismatch, m.Reason)
			require.NotNil(t, m.RetryTime)
		}
	}
	assert.Equal(t, map[string]workercore.NodeModelStoreModelState{
		"sha256:" + hexOf('a'): workercore.NodeModelStoreModelStateReady,
		"sha256:" + hexOf('b'): workercore.NodeModelStoreModelStateReady,
		"sha256:" + hexOf('c'): workercore.NodeModelStoreModelStateDownloading,
		"sha256:" + hexOf('d'): workercore.NodeModelStoreModelStateFailed,
	}, states)
	assert.Equal(t, meta.ConditionTrue, findCondition(st.Conditions, ConditionReady).Status)
	assert.Equal(t, meta.ConditionFalse, findCondition(st.Conditions, ConditionCapacityLow).Status)
	for _, m := range st.Models {
		assert.NotContains(t, m.Message, "team-a", "no tenant name reaches the cluster-scoped object")
	}
}

func TestReportWritesNothingWhenNothingChanged(t *testing.T) {
	env := newTestEnv(t, validSpec())
	env.publish(t, hexOf('a'), 300)
	require.NoError(t, env.r.Report(context.Background()))
	require.Equal(t, int64(1), env.writes.Load())

	for range 5 {
		require.NoError(t, env.r.Report(context.Background()))
	}
	assert.Equal(t, int64(1), env.writes.Load(), "a quiet node writes nothing")

	env.usage.Used = 440
	require.NoError(t, env.r.Report(context.Background()))
	assert.Equal(t, int64(1), env.writes.Load(), "usage within the same 5% step writes nothing")
	env.usage.Used = 510
	require.NoError(t, env.r.Report(context.Background()))
	assert.Equal(t, int64(2), env.writes.Load(), "crossing a step writes once")
}

func TestReportRefusesAnInvalidSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    workercore.NodeModelStoreSpec
		objs    []ctrlcli.Object
		wantErr string
	}{
		{name: "a hand-edited concurrency", spec: func() workercore.NodeModelStoreSpec {
			s := validSpec()
			s.Download.Concurrency = 0
			return s
		}(), wantErr: "concurrency"},
		{name: "a missing CA ConfigMap", spec: func() workercore.NodeModelStoreSpec {
			s := validSpec()
			s.Hub.CABundleConfigMap = "hub-ca"
			return s
		}(), wantErr: "hub-ca"},
		{name: "a CA ConfigMap without ca.crt", spec: func() workercore.NodeModelStoreSpec {
			s := validSpec()
			s.Hub.CABundleConfigMap = "hub-ca"
			return s
		}(), objs: []ctrlcli.Object{&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "hub-ca"}}}, wantErr: "ca.crt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestEnv(t, c.spec, c.objs...)
			require.NoError(t, env.r.Report(context.Background()))

			ready := findCondition(env.nms(t).Status.Conditions, ConditionReady)
			assert.Equal(t, meta.ConditionFalse, ready.Status)
			assert.Equal(t, ReasonInvalidConfiguration, ready.Reason)
			assert.Contains(t, ready.Message, c.wantErr)
			_, _, err := env.r.Environment(context.Background())
			assert.Equal(t, download.ReasonInvalidRequest, download.ReasonOf(err), "no download starts")
		})
	}
}

func TestReportNeverCollectsBeforeTheSpecApplies(t *testing.T) {
	spec := validSpec()
	spec.Hub.CABundleConfigMap = "hub-ca" // missing, so this spec never applies
	env := newTestEnv(t, spec)
	env.usage.Used = 950
	env.publish(t, hexOf('a'), 100)
	require.NoError(t, env.store.WriteDigest(hexOf('a'), store.DigestRecord{LastUsedTime: testNow.Add(-2 * time.Hour)}))

	require.NoError(t, env.r.Report(context.Background()))
	assert.True(t, env.store.IsPublished(hexOf('a')), "an unreferenced tree past its grace stays while there are no watermarks")
	ready := findCondition(env.nms(t).Status.Conditions, ConditionReady)
	assert.Contains(t, ready.Message, "collection skipped")
}

func TestReportKeepsTheClientOfAnUnchangedSpec(t *testing.T) {
	env := newTestEnv(t, validSpec())
	require.NoError(t, env.r.Report(context.Background()))
	first := env.r.client
	require.NotNil(t, first)

	require.NoError(t, env.r.Report(context.Background()))
	assert.Same(t, first, env.r.client, "an unchanged spec keeps the client and its connections")

	nms := env.nms(t)
	nms.Spec.Download.Concurrency = 4
	require.NoError(t, env.cli.Update(context.Background(), nms))
	require.NoError(t, env.r.Report(context.Background()))
	assert.NotSame(t, first, env.r.client, "a changed spec is applied")
}

func TestEnvironmentBeforeTheSpecArrives(t *testing.T) {
	env := newTestEnv(t, validSpec())
	_, _, err := env.r.Environment(context.Background())
	assert.Equal(t, download.ReasonInvalidRequest, download.ReasonOf(err))

	require.NoError(t, env.r.Report(context.Background()))
	hub, dl, err := env.r.Environment(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, hub)
	assert.Same(t, env.r.Downloader, dl, "every attempt shares the node's limits")
}

func TestReportCapsTheWatermarkOnKubeletsFilesystem(t *testing.T) {
	spec := validSpec()
	spec.Watermarks = workercore.NodeModelStoreWatermarks{HighPercent: 90, LowPercent: 85}
	env := newTestEnv(t, spec)
	env.r.KubeletCap = func(uint64) *gc.Cap { return &gc.Cap{Percent: 80, Source: "kubelet's default thresholds"} }

	require.NoError(t, env.r.Report(context.Background()))
	high, low, ok := env.r.Watermarks()
	require.True(t, ok)
	assert.Equal(t, []int32{80, 79}, []int32{high, low})
	assert.Contains(t, findCondition(env.nms(t).Status.Conditions, ConditionReady).Message, "capped at 80%")
}

func TestReportKeepsTheKubeletCapWhenUsageCannotBeRead(t *testing.T) {
	spec := validSpec()
	spec.Watermarks = workercore.NodeModelStoreWatermarks{HighPercent: 90, LowPercent: 85}
	capped := func(env *testEnv) {
		env.r.KubeletCap = func(uint64) *gc.Cap { return &gc.Cap{Percent: 80, Source: "kubelet's default thresholds"} }
	}

	t.Run("without a reading yet the spec is not applied and nothing is collected", func(t *testing.T) {
		env := newTestEnv(t, spec)
		capped(env)
		env.usage.Used = 950
		env.publish(t, hexOf('a'), 100)
		require.NoError(t, env.store.WriteDigest(hexOf('a'), store.DigestRecord{LastUsedTime: testNow.Add(-2 * time.Hour)}))
		env.usageFails = 1

		require.NoError(t, env.r.Report(context.Background()))
		_, _, ok := env.r.Watermarks()
		assert.False(t, ok, "no uncapped watermarks")
		assert.True(t, env.store.IsPublished(hexOf('a')))
		ready := findCondition(env.nms(t).Status.Conditions, ConditionReady)
		assert.Contains(t, ready.Message, "kubelet cap")
		assert.Contains(t, ready.Message, "collection skipped")
	})
	t.Run("after a reading the cap and the client stay", func(t *testing.T) {
		env := newTestEnv(t, spec)
		capped(env)
		require.NoError(t, env.r.Report(context.Background()))
		first := env.r.client
		env.usageFails = 1

		require.NoError(t, env.r.Report(context.Background()))
		high, low, ok := env.r.Watermarks()
		require.True(t, ok)
		assert.Equal(t, []int32{80, 79}, []int32{high, low})
		assert.Same(t, first, env.r.client)
	})
}

func TestReportBoundsTheModels(t *testing.T) {
	env := newTestEnv(t, validSpec())
	for i := range MaxModels + 4 {
		hex := fmt.Sprintf("%064x", i+1)
		require.NoError(t, env.store.WriteDigest(hex, store.DigestRecord{
			Reason: download.ReasonSourceUnavailable, RetryTime: testNow, LastUsedTime: testNow.Add(-time.Duration(i) * time.Hour),
		}))
	}
	referenced := hexOf('f')
	env.publish(t, referenced, 1)
	require.NoError(t, env.store.WriteRef(store.Ref{VolumeID: "csi-1", TargetPath: "/t", Hex: referenced}))

	require.NoError(t, env.r.Report(context.Background()))
	st := env.nms(t).Status
	require.Len(t, st.Models, MaxModels)
	assert.Equal(t, "sha256:"+referenced, st.Models[0].Digest, "a referenced entry is kept first")
	assert.Equal(t, "sha256:"+fmt.Sprintf("%064x", 1), st.Models[1].Digest, "then the most recently used")
	assert.Contains(t, findCondition(st.Conditions, ConditionReady).Message, "5 more models")
}

func TestReportWithoutItsObject(t *testing.T) {
	env := newTestEnv(t, validSpec())
	require.NoError(t, env.cli.Delete(context.Background(), &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}))
	require.NoError(t, env.r.Report(context.Background()), "the worker creates it once kubelet lists the driver")
	assert.Zero(t, env.writes.Load())
}

func TestMountedRecordsTheUse(t *testing.T) {
	env := newTestEnv(t, validSpec())
	env.r.Unmounted(hexOf('a'))
	rec, err := env.store.ReadDigest(hexOf('a'))
	require.NoError(t, err)
	assert.Equal(t, testNow, rec.LastUsedTime)
}
