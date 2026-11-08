package modeltransfer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

func validOptions() *Options {
	opts := NewOptions()
	opts.NodeName = "node-1"
	opts.PhysicalBytes = 1 << 30
	opts.WorkerID = "source-worker-node-1"
	opts.VendorScope = "ascend"
	opts.WorkerNamespace = "operator"
	opts.SessionTLSCert = "/tls/cert"
	opts.SessionTLSKey = "/tls/key"
	return opts
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Options)
		err    string
	}{
		{"valid", func(*Options) {}, ""},
		{"zero connection settings", func(o *Options) { o.KubeConnTimeout, o.KubeConnQPS, o.KubeConnBurst = 0, 0, 0 }, ""},
		{"short timeout", func(o *Options) { o.KubeConnTimeout = time.Second }, "kube-conn-timeout"},
		{"negative timeout", func(o *Options) { o.KubeConnTimeout = -time.Second }, "kube-conn-timeout"},
		{"NaN qps", func(o *Options) { o.KubeConnQPS = math.NaN() }, "kube-conn-qps"},
		{"infinite qps", func(o *Options) { o.KubeConnQPS = math.Inf(1) }, "kube-conn-qps"},
		{"small qps", func(o *Options) { o.KubeConnQPS = 9 }, "kube-conn-qps"},
		{"negative qps", func(o *Options) { o.KubeConnQPS = -1 }, "kube-conn-qps"},
		{"small burst", func(o *Options) { o.KubeConnBurst = 9 }, "kube-conn-burst"},
		{"negative burst", func(o *Options) { o.KubeConnBurst = -1 }, "kube-conn-burst"},
		{"burst equals qps", func(o *Options) { o.KubeConnBurst = 200 }, "kube-conn-burst"},
		{"no node name", func(o *Options) { o.NodeName = "" }, "node-name"},
		{"no cache root", func(o *Options) { o.CacheRoot = "" }, "cache-root"},
		{"no broker root", func(o *Options) { o.BrokerRoot = "" }, "broker-root"},
		{"no control address", func(o *Options) { o.ControlAddr = "" }, "control-addr"},
		{"zero physical bytes derives one at start", func(o *Options) { o.PhysicalBytes = 0 }, ""},
		{"negative physical bytes", func(o *Options) { o.PhysicalBytes = -1 }, "physical-bytes"},
		{"no generations", func(o *Options) { o.MaxGenerations = 0 }, "max-generations"},
		{"no lease ttl", func(o *Options) { o.LeaseTTL = 0 }, "lease-ttl"},
		{"no worker namespace", func(o *Options) { o.WorkerNamespace = "" }, "worker-namespace"},
		{"no TLS pair", func(o *Options) { o.SessionTLSCert, o.SessionTLSKey = "", "" }, "session-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			tc.mutate(opts)
			err := opts.Validate()
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.err)
		})
	}
}

func TestKubeConnectionFlags(t *testing.T) {
	opts := validOptions()
	fs := pflag.NewFlagSet("model-transfer", pflag.ContinueOnError)
	opts.AddFlags(fs)
	require.NoError(t, fs.Parse([]string{
		"--kube-conn-timeout=30s", "--kube-conn-qps=100",
		"--kube-conn-burst=300", "--kube-content-type=application/json",
	}))
	assert.Equal(t, 30*time.Second, opts.KubeConnTimeout)
	assert.Equal(t, float64(100), opts.KubeConnQPS)
	assert.Equal(t, 300, opts.KubeConnBurst)
	assert.Equal(t, "application/json", opts.KubeContentType)
}

func TestMemoryLimitFlags(t *testing.T) {
	opts := validOptions()
	opts.PhysicalBytes = 0
	fs := pflag.NewFlagSet("model-transfer", pflag.ContinueOnError)
	opts.AddFlags(fs)
	require.NoError(t, fs.Parse([]string{
		"--max-memory-bytes=8589934592", "--min-memory-bytes=2147483648", "--max-sessions=32",
	}))
	assert.Equal(t, int64(8<<30), opts.PhysicalBytes,
		"--max-memory-bytes drives the same ceiling as --physical-bytes")
	assert.Equal(t, int64(2<<30), opts.MinMemoryBytes)
	assert.Equal(t, int32(32), opts.MaxSessions)
}

// TestResolveMemoryLimits pins the silent corrections: a floor below 1 GiB
// resolves up to it, a zero ceiling derives one sixteenth of the node's
// total RAM, a small ceiling rises to the floor, and a session cap below
// one resolves to the default of 64.
func TestResolveMemoryLimits(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	require.NoError(t, os.WriteFile(meminfo, []byte(
		"MemTotal:       32768 kB\n"+
			"MemFree:         1024 kB\n"+
			"MemAvailable:    2048 kB\n"+
			"Buffers:          128 kB\n",
	), 0o644))

	t.Run("zero ceiling derives one sixteenth of total RAM", func(t *testing.T) {
		o := validOptions()
		o.PhysicalBytes = 0
		o.ResolveMemoryLimits(meminfo)
		assert.Equal(t, int64(1<<30), o.PhysicalBytes,
			"32768 kB / 16 = 2 MiB, under the floor, so the floor stands")
		assert.Equal(t, int64(1<<30), o.MinMemoryBytes)
		assert.Equal(t, int32(64), o.MaxSessions)
	})

	t.Run("a real total RAM derives one sixteenth of it", func(t *testing.T) {
		large := filepath.Join(t.TempDir(), "meminfo")
		require.NoError(t, os.WriteFile(large, []byte(
			"MemTotal:      536870912 kB\n"+
				"MemAvailable: 107374182 kB\n",
		), 0o644))
		o := validOptions()
		o.PhysicalBytes = 0
		o.ResolveMemoryLimits(large)
		assert.Equal(t, int64(32<<30), o.PhysicalBytes,
			"512 GiB / 16 = 32 GiB")
	})

	t.Run("unreadable meminfo falls back to the floor", func(t *testing.T) {
		o := validOptions()
		o.PhysicalBytes = 0
		o.ResolveMemoryLimits(filepath.Join(t.TempDir(), "absent"))
		assert.Equal(t, int64(1<<30), o.PhysicalBytes)
	})

	t.Run("a ceiling under the floor rises to it", func(t *testing.T) {
		o := validOptions()
		o.PhysicalBytes = 1 << 20
		o.ResolveMemoryLimits(meminfo)
		assert.Equal(t, int64(1<<30), o.PhysicalBytes)
	})

	t.Run("a floor under 1 GiB resolves up to it", func(t *testing.T) {
		o := validOptions()
		o.PhysicalBytes = 4 << 30
		o.MinMemoryBytes = 1 << 20
		o.ResolveMemoryLimits(meminfo)
		assert.Equal(t, int64(1<<30), o.MinMemoryBytes)
		assert.Equal(t, int64(4<<30), o.PhysicalBytes)
	})

	t.Run("a session cap below one resolves to the default", func(t *testing.T) {
		o := validOptions()
		o.MaxSessions = 0
		o.ResolveMemoryLimits(meminfo)
		assert.Equal(t, int32(64), o.MaxSessions)
	})

	t.Run("configured values pass through", func(t *testing.T) {
		o := validOptions()
		o.PhysicalBytes = 16 << 30
		o.MinMemoryBytes = 2 << 30
		o.MaxSessions = 32
		o.ResolveMemoryLimits(meminfo)
		assert.Equal(t, int64(16<<30), o.PhysicalBytes)
		assert.Equal(t, int64(2<<30), o.MinMemoryBytes)
		assert.Equal(t, int32(32), o.MaxSessions)
	})
}

func TestCompleteConnectionSettings(t *testing.T) {
	for _, tc := range []struct {
		name            string
		timeout         time.Duration
		qps             float64
		burst           int
		contentType     string
		input           rest.Config
		wantTimeout     time.Duration
		wantQPS         float32
		wantBurst       int
		wantContentType string
	}{
		{name: "defaults", wantQPS: 200, wantBurst: 400},
		{name: "preserve supplied config", input: rest.Config{QPS: 50, Burst: 100, Timeout: time.Minute, ContentType: "application/json"}, wantQPS: 50, wantBurst: 100, wantTimeout: time.Minute, wantContentType: "application/json"},
		{name: "explicit overrides", timeout: 30 * time.Second, qps: 100, burst: 300, contentType: "application/json", input: rest.Config{QPS: 50, Burst: 100, Timeout: time.Minute}, wantQPS: 100, wantBurst: 300, wantTimeout: 30 * time.Second, wantContentType: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			opts.CacheRoot, opts.BrokerRoot = t.TempDir(), t.TempDir()
			opts.KubeConnTimeout, opts.KubeConnQPS, opts.KubeConnBurst, opts.KubeContentType = tc.timeout, tc.qps, tc.burst, tc.contentType
			input := tc.input
			input.Host = "https://127.0.0.1"
			before := input
			cfg, err := opts.complete(&input)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cfg.Close()) })
			assert.Equal(t, before, input)
			assert.Equal(t, tc.wantTimeout, cfg.RestConfig.Timeout)
			assert.Equal(t, tc.wantQPS, cfg.RestConfig.QPS)
			assert.Equal(t, tc.wantBurst, cfg.RestConfig.Burst)
			assert.Equal(t, tc.wantContentType, cfg.RestConfig.ContentType)
		})
	}
}

func TestDefaultWorkerNamespaceRequiresConfiguration(t *testing.T) {
	assert.Empty(t, NewOptions().WorkerNamespace)
}

func TestCompleteOpensStoreAndBroker(t *testing.T) {
	opts := validOptions()
	opts.CacheRoot = t.TempDir()
	opts.BrokerRoot = t.TempDir()
	opts.PhysicalBytes = 1 << 30
	opts.LeaseTTL = time.Minute

	staging := filepath.Join(opts.BrokerRoot, "manifest-staging")
	require.NoError(t, os.MkdirAll(staging, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "orphan"), []byte("page"), 0o600))
	unowned := filepath.Join(opts.BrokerRoot, "unrelated")
	require.NoError(t, os.WriteFile(unowned, []byte("keep"), 0o600))
	restCfg := &rest.Config{Host: "https://127.0.0.1"}
	cfg, err := opts.complete(restCfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Store)
	require.NotNil(t, cfg.Broker)
	require.NotNil(t, cfg.Authorizer)
	require.NotNil(t, cfg.Client)
	assert.Equal(t, staging, cfg.manifestPageRoot)
	assert.NoDirExists(t, staging)
	assert.FileExists(t, unowned)
	assert.Equal(t, "node-1", cfg.NodeName)
	assert.Equal(t, "ascend", cfg.VendorScope)
	assert.Zero(t, restCfg.QPS, "caller rest.Config must not be mutated in place")
	assert.Equal(t, float32(defaultKubeConnQPS), cfg.RestConfig.QPS)
	assert.Equal(t, defaultKubeConnBurst, cfg.RestConfig.Burst)
	t.Cleanup(func() { _ = cfg.Close() })
}

// TestLiveWorkersReadsAcrossNamespaces exercises the listing helper; production supplies
// the operator namespace and records workers by Pod UID.
func TestLiveWorkersReadsAcrossNamespaces(t *testing.T) {
	pod := func(ns, name, node string, phase core.PodPhase, component string) *core.Pod {
		labels := map[string]string{}
		if component != "" {
			labels["app.kubernetes.io/component"] = component
		}

		return &core.Pod{
			ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name), Labels: labels},
			Spec:       core.PodSpec{NodeName: node},
			Status:     core.PodStatus{Phase: phase},
		}
	}
	cli := kubefake.NewSimpleClientset(
		pod("team-a", "warm-node1-source", "node-1", core.PodRunning, SourceWorkerComponent),
		pod("team-b", "warm-node2-source", "node-2", core.PodRunning, SourceWorkerComponent),
		pod("team-a", "warm-node3-source", "node-1", core.PodFailed, SourceWorkerComponent),
		pod("team-a", "someone-else", "node-1", core.PodRunning, "something-else"),
	)
	cfg := &Config{NodeName: "node-1", Client: cli, WorkerLabel: SourceWorkerLabel}

	live, err := cfg.LiveWorkers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"warm-node1-source": true,
		"warm-node3-source": false,
	}, live, "this node's workers read from both namespaces, a failed one reads dead, and another node's worker and another component's Pod are invisible")

	narrowed := &Config{NodeName: "", Client: cli, WorkerLabel: SourceWorkerLabel}
	live, err = narrowed.LiveWorkers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"warm-node1-source": true,
		"warm-node2-source": true,
		"warm-node3-source": false,
	}, live, "an unset node reads every node's workers")

	namespaced := &Config{NodeName: "", Client: cli, WorkerNamespace: "team-a", WorkerLabel: SourceWorkerLabel}
	live, err = namespaced.LiveWorkers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"warm-node1-source": true, "warm-node3-source": false}, live,
		"a non-empty namespace narrows the read to that namespace")
}
