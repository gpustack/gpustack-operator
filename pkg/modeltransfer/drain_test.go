package modeltransfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

func TestRuntimeAttestationRequiresExactEmptySnapshot(t *testing.T) {
	instance := strings.Repeat("a", 32)
	cases := []struct {
		name, body string
		valid      bool
	}{
		{name: "empty", body: fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":[]}`, instance), valid: true},
		{name: "other worker", body: fmt.Sprintf(`{"workerID":"other","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":[]}`, instance)},
		{name: "retained", body: fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":["g"]}`, instance)},
		{name: "unknown", body: fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":1,"retainedGenerations":[]}`, instance)},
		{name: "missing count", body: fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"retainedGenerations":[]}`, instance)},
		{name: "duplicate", body: fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":["g"],"generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":[]}`, instance)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			cfg := &Config{WorkerID: "pod", bootstrapToken: "secret", runtimeURL: server.URL}
			err := cfg.observeEmptyRuntime(context.Background(), instance)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestRuntimeTransportFailurePreservesCause(t *testing.T) {
	for _, freeze := range []bool{false, true} {
		t.Run(fmt.Sprintf("freeze=%t", freeze), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			cfg := &Config{runtimeURL: "http://127.0.0.1:1/v1/runtime", bootstrapToken: "secret"}
			var err error
			if freeze {
				_, err = cfg.freezeRuntime(ctx)
			} else {
				_, err = cfg.inspectEmptyRuntime(ctx, "instance")
			}
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestDrainAcknowledgementRequiresInventoryAndRuntimeProof(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st,
		broker.Limits{PhysicalBytes: 1024, MaxGenerations: 1, MaxQuarantined: 1, QuarantineBytes: 512, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.RequireAuthorityInventory("node"))
	b.SetAssignmentsFrozen(true)
	instance := strings.Repeat("a", 32)
	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "ds", Namespace: "operator", UID: "ds-uid", Labels: labels}}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "source", Namespace: "operator", UID: "pod", Labels: labels,
			Annotations:     map[string]string{drainForAnnotation: "worker:v2", workerInstanceAnnotation: instance},
			OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "ds-uid", Controller: ptr.To(true)}},
		},
		Spec: core.PodSpec{NodeName: "node"},
	}
	kube := kubefake.NewSimpleClientset(ds, pod, &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node"}})
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	response := fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":["g"]}`, instance)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = fmt.Fprintf(w, `{"workerID":"pod","workerInstanceID":"%s","draining":true}`, instance)
			return
		}
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	cfg := &Config{Broker: b, Client: kube, NodeName: "node", WorkerNamespace: "operator", WorkerID: "pod", VendorScope: "nvidia", bootstrapToken: "token", runtimeURL: server.URL}
	src := &kubernetesIntentSource{config: cfg, client: reader}
	require.Error(t, cfg.acknowledgeDrain(context.Background(), src))
	current, err := kube.CoreV1().Pods("operator").Get(context.Background(), "source", meta.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, current.Annotations[drainedForAnnotation])
	response = fmt.Sprintf(`{"workerID":"pod","workerInstanceID":"%s","generations":[],"nativeOperations":0,"unknownOperations":0,"retainedGenerations":[]}`, instance)
	require.NoError(t, cfg.acknowledgeDrain(context.Background(), src))
	current, err = kube.CoreV1().Pods("operator").Get(context.Background(), "source", meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "worker:v2", current.Annotations[drainedForAnnotation])
	assert.Equal(t, instance, current.Annotations[drainedInstanceAnnotation])
	delete(current.Annotations, workerInstanceAnnotation)
	delete(current.Annotations, drainedForAnnotation)
	delete(current.Annotations, drainedInstanceAnnotation)
	_, err = kube.CoreV1().Pods("operator").Update(context.Background(), current, meta.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, cfg.acknowledgeDrain(context.Background(), src))
	current, err = kube.CoreV1().Pods("operator").Get(context.Background(), "source", meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, instance, current.Annotations[workerInstanceAnnotation])
	assert.Equal(t, instance, current.Annotations[drainedInstanceAnnotation])
	current.Annotations[drainForAnnotation] = "worker:v3"
	delete(current.Annotations, drainedForAnnotation)
	delete(current.Annotations, drainedInstanceAnnotation)
	_, err = kube.CoreV1().Pods("operator").Update(context.Background(), current, meta.UpdateOptions{})
	require.NoError(t, err)
	fired := false
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	kube.PrependReactor("update", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		obj, getErr := kube.Tracker().Get(gvr, "operator", "source")
		require.NoError(t, getErr)
		changed := obj.(*core.Pod).DeepCopy()
		changed.Annotations[drainForAnnotation] = "worker:v4"
		require.NoError(t, kube.Tracker().Update(gvr, changed, "operator"))
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "source", errors.New("changed"))
	})
	require.Error(t, cfg.acknowledgeDrain(context.Background(), src))
	current, err = kube.CoreV1().Pods("operator").Get(context.Background(), "source", meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "worker:v4", current.Annotations[drainForAnnotation])
	assert.Empty(t, current.Annotations[drainedForAnnotation])
}

// failingListClient stands in for a cluster client whose list calls fail, so
// an intent pass fails the way a control-plane outage does.
type failingListClient struct {
	ctrlcli.Client
	listErr error
}

func (r failingListClient) List(context.Context, ctrlcli.ObjectList, ...ctrlcli.ListOption) error {
	return r.listErr
}

// hysteresisFixture builds a broker with one serving generation and a
// source environment whose Pod listing the test can break and repair. The
// fixture's store publishes read-only trees, so its root is made writable
// again for cleanup.
func hysteresisFixture(t *testing.T) (*broker.Broker, *store.Store, *Config, *bool, string) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st,
		broker.Limits{PhysicalBytes: 1024, MaxGenerations: 4, MaxQuarantined: 1, QuarantineBytes: 512, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	generation := serveFixtureGeneration(t, b, st, strings.Repeat("ab", 32))

	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "ds", Namespace: "operator", UID: "ds-uid", Labels: labels}}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "source", Namespace: "operator", UID: "pod", Labels: labels,
			OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "ds-uid", Controller: ptr.To(true)}},
		},
		Spec: core.PodSpec{NodeName: "node"},
	}
	kube := kubefake.NewSimpleClientset(ds, pod, &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node"}})
	failPods := false
	kube.PrependReactor("list", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if failPods {
			return true, nil, errors.New("the API server is unreachable")
		}
		return false, nil, nil
	})
	cfg := &Config{
		Broker: b, Client: kube, NodeName: "node", WorkerNamespace: "operator",
		WorkerID: "pod", VendorScope: "nvidia",
	}
	cfg.Intents = &kubernetesIntentSource{config: cfg, client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()}
	return b, st, cfg, &failPods, generation
}

// serveFixtureGeneration publishes a verified tree for the digest and walks
// one generation through admission to serving, through the public API.
func serveFixtureGeneration(t *testing.T, b *broker.Broker, st *store.Store, hexDigest string) string {
	t.Helper()
	digest := "sha256:" + hexDigest
	attempt, err := st.NewAttempt(hexDigest)
	require.NoError(t, err)
	tree := filepath.Join(attempt.Dir(), "tree")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"), []byte("weights"), 0o644))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: digest, SizeBytes: 7, ManifestFormat: "gpustack-model-transfer/v1", Source: store.SourceHub,
	}))
	admission, perr := b.AdmitFill(broker.FillRequest{
		Digest: digest, Namespace: "tenant", TotalBytes: 7,
		Grant:    broker.Grant{Namespace: "tenant", Bytes: 512, Registrations: 4, Sessions: 4},
		WorkerID: "pod",
	})
	require.Nil(t, perr)
	lease, perr := b.RegisterWorker(admission.Generation, "pod", admission.WorkerSecret)
	require.Nil(t, perr)
	require.Nil(t, b.FillVerified(admission.Generation, "pod", lease.ID, admission.WorkerSecret, 1,
		[]broker.RegistrationDescriptor{{
			Name: "checkpoint.safetensors", Addr: 1, Length: 7,
			MemoryType: "HOST", Backend: "nixl", WorkerUID: "pod",
		}}))
	return admission.Generation
}

func generationState(t *testing.T, b *broker.Broker, generation string) string {
	t.Helper()
	for _, entry := range b.Snapshot() {
		if entry.Generation == generation {
			return string(entry.State)
		}
	}
	return ""
}

func TestReconcileSourceDrainsServingOnlyOnSustainedFailure(t *testing.T) {
	b, st, cfg, failPods, first := hysteresisFixture(t)
	require.Equal(t, string(broker.StateServing), generationState(t, b, first))

	for strike := 1; strike <= 2; strike++ {
		*failPods = true
		require.Error(t, cfg.reconcileSource(context.Background()))
		assert.Equal(t, string(broker.StateServing), generationState(t, b, first),
			"strike %d never empties the node's serving generations", strike)
	}
	*failPods = true
	require.Error(t, cfg.reconcileSource(context.Background()))
	assert.Equal(t, string(broker.StateDraining), generationState(t, b, first),
		"the third sustained failure drains serving generations")

	// One failure after a clean pass starts the count over.
	second := serveFixtureGeneration(t, b, st, strings.Repeat("cd", 32))
	*failPods = false
	require.NoError(t, cfg.reconcileSource(context.Background()))
	*failPods = true
	require.Error(t, cfg.reconcileSource(context.Background()))
	assert.Equal(t, string(broker.StateServing), generationState(t, b, second),
		"a single failure after a clean pass holds the serving generations")
}

func TestReconcileSourceDrainsServingOnAuthorityOutageImmediately(t *testing.T) {
	b, _, cfg, _, generation := hysteresisFixture(t)
	require.NoError(t, b.RequireAuthorityInventory("node"))
	_, err := cfg.Client.CoreV1().ConfigMaps(cfg.WorkerNamespace).Create(context.Background(), &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{Name: "model-transfer-resv-corrupt", Namespace: cfg.WorkerNamespace},
		Data:       map[string]string{"reservations.json": "{"},
	}, meta.CreateOptions{})
	require.NoError(t, err)
	err = cfg.reconcileSource(context.Background())
	require.ErrorIs(t, err, broker.ErrReservationUnavailable)
	require.NotErrorIs(t, err, broker.ErrReservationReadFailed)
	require.Equal(t, string(broker.StateDraining), generationState(t, b, generation))
}

func TestReconcileSourceAuthorityReadFailureWaitsForThreshold(t *testing.T) {
	b, _, cfg, _, generation := hysteresisFixture(t)
	require.NoError(t, b.RequireAuthorityInventory("node"))
	cfg.Intents = &kubernetesIntentSource{config: cfg, client: failingListClient{
		Client:  ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		listErr: errors.New("the API server is unreachable"),
	}}
	for _, want := range []string{string(broker.StateServing), string(broker.StateServing), string(broker.StateDraining)} {
		err := cfg.reconcileSource(context.Background())
		require.ErrorIs(t, err, broker.ErrReservationUnavailable)
		require.ErrorIs(t, err, broker.ErrReservationReadFailed)
		require.Equal(t, want, generationState(t, b, generation))
	}
}
