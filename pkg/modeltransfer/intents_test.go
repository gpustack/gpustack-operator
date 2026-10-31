package modeltransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type failingIntentClient struct{ ctrlcli.Client }

type countingIntentClient struct {
	ctrlcli.Client
	reads int
}

func (c *countingIntentClient) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	c.reads++
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingIntentClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	c.reads++
	return c.Client.List(ctx, list, opts...)
}

type sessionTestAuthorizer struct{ digest string }

type testClosedSource struct {
	posts   int
	request map[string]any
}

func (s *testClosedSource) Call(_ context.Context, path string, payload any) (broker.PrivateReply, error) {
	if path == "/v1/sessions/close" {
		return broker.PrivateReply{State: "closed"}, nil
	}
	body := payload.(map[string]any)
	id := body["requestID"].(string)
	if path == "/v1/operations" {
		s.posts++
		s.request = body
		return broker.PrivateReply{RequestID: id, State: "submitted"}, nil
	}
	if path == "/v1/operations/status" {
		return broker.PrivateReply{RequestID: id, State: "terminal", Verdict: "succeeded"}, nil
	}
	if path == "/v1/operations/ack" {
		return broker.PrivateReply{RequestID: id, State: "acknowledged", Verdict: "succeeded"}, nil
	}
	return broker.PrivateReply{}, errors.New("unexpected private call")
}

func (a sessionTestAuthorizer) AuthorizeSession(_ context.Context, req authorization.Request) (authorization.Result, *protocol.Error) {
	if req.Token != "token" || req.ManifestDigest != a.digest || req.ArtifactUID != "artifact-uid" {
		return authorization.Result{}, protocol.New(protocol.KindAuthentication, "session", "invalid test request")
	}
	return authorization.Result{
		Pod:       authorization.PodIdentity{Namespace: "tenant", Name: "engine", UID: "engine-uid"},
		DigestHex: store.HexOf(a.digest), SourceGeneration: 1,
	}, nil
}

func (a sessionTestAuthorizer) AuthenticatePod(_ context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	if req.Token != "token" {
		return authorization.PodIdentity{}, protocol.New(protocol.KindAuthentication, "session", "invalid test request")
	}
	return authorization.PodIdentity{Namespace: "tenant", Name: "engine", UID: "engine-uid"}, nil
}

func (c failingIntentClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	if _, ok := list.(*workercore.ModelPrefetchList); ok {
		return errors.New("API unavailable")
	}
	return c.Client.List(ctx, list, opts...)
}

func TestKubernetesIntentsAdmitRealPublishedArtifactAndHoldOnReadFailure(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	file, err := os.ReadFile(filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	sum := sha256.Sum256(file)
	fileDigest := "sha256:" + hex.EncodeToString(sum[:])
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}})
	require.NoError(t, err)
	attempt, err := st.NewAttempt(store.HexOf(manifest.Digest))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(attempt.Dir(), "tree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "tree", "checkpoint.safetensors"), file, 0o644))
	listing, err := json.Marshal(peer.Listing{Digest: manifest.Digest, Entries: []peer.ManifestFile{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"), listing, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{Digest: manifest.Digest, SizeBytes: int64(len(file)), ManifestFormat: modelartifact.ManifestHeader, Source: store.SourceHub}))
	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st, broker.Limits{PhysicalBytes: 1 << 30, MaxGenerations: 4, MaxQuarantined: 2, QuarantineBytes: 1 << 28, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.EnableOperationJournal())
	private := &testClosedSource{}
	b.SetPrivateSource(private)
	require.NoError(t, b.RequireAuthorityInventory("node-1"))
	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source-1", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: node.Name, Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}
	kube := kubefake.NewSimpleClientset(node, ds, pod)
	kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*core.ConfigMap)
		cm.UID = "authority-uid"
		cm.ResourceVersion = "1"
		return false, nil, nil
	})
	pf := &workercore.ModelPrefetch{ObjectMeta: meta.ObjectMeta{Name: "prefetch", Namespace: "tenant", UID: "prefetch-uid", Generation: 1}, Spec: workercore.ModelPrefetchSpec{ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"}, BindingRef: workercore.ModelPrefetchBindingReference{Name: "grant"}, Source: &workercore.ModelPrefetchSource{}, Placement: &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}}
	model := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "model", Namespace: "tenant", UID: "artifact-uid", Generation: 1}, Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"}}}, Status: workercore.ModelArtifactStatus{ObservedGeneration: 1, Resolved: &workercore.ModelArtifactResolved{ManifestDigest: manifest.Digest, SizeBytes: int64(len(file))}, Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}}}}
	binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid"}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store"}}, SourceMemory: &workercore.ModelStoreBindingSourceMemory{Bytes: resource.MustParse("1Gi"), Registrations: 4, Sessions: 4}}}
	modelStore := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, pf, model, binding, modelStore).Build()
	cfg := &Config{Store: st, Broker: b, Client: kube, WorkerNamespace: "operator", WorkerID: "pod-uid", NodeName: "node-1", VendorScope: "nvidia"}
	src := &kubernetesIntentSource{config: cfg, client: client}
	cfg.Intents = src
	b.SetIntentValidator(cfg.validateLiveIntent)
	require.NoError(t, broker.ReconcileIntents(context.Background(), b, src))
	offers := b.OfferedAssignments()
	require.Len(t, offers, 1)
	updatedBinding := &workercore.ModelStoreBinding{}
	require.NoError(t, client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(binding), updatedBinding))
	cm, err := kube.CoreV1().ConfigMaps("operator").Get(context.Background(), broker.AuthorityName(binding.UID), meta.GetOptions{})
	require.NoError(t, err)
	authorityReader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(updatedBinding, cm).Build()
	reservations, err := broker.ReadPinnedAuthority(context.Background(), authorityReader, "operator", updatedBinding)
	require.NoError(t, err)
	require.Len(t, reservations, 1)
	assert.Equal(t, "node-1", reservations[0].NodeName)
	assert.Equal(t, "artifact-uid", reservations[0].ArtifactUID)
	assert.Equal(t, "prefetch-uid", reservations[0].IntentUID)
	assert.Equal(t, "pod-uid", reservations[0].SourcePodUID)
	assert.Equal(t, broker.ReservationStateAdmitted, reservations[0].State)
	// Repeated checks reuse both Kubernetes clients until invalidation or expiry.
	counting := &countingIntentClient{Client: client}
	src.client = counting
	liveIntent := broker.LiveIntent{
		Namespace: "tenant", IntentName: pf.Name, IntentUID: string(pf.UID), IntentGeneration: pf.Generation,
		BindingName: binding.Name, BindingUID: string(binding.UID), ArtifactName: model.Name,
		ArtifactUID: string(model.UID), SourceGeneration: model.Generation, Digest: manifest.Digest,
		NodeName: cfg.NodeName, WorkerID: cfg.WorkerID,
	}
	ctx := context.Background()
	require.NoError(t, cfg.validateLiveIntent(ctx, liveIntent))
	reads, actions := counting.reads, len(kube.Actions())
	require.Positive(t, reads)
	require.Positive(t, actions)
	require.NoError(t, cfg.validateLiveIntent(ctx, liveIntent))
	assert.Equal(t, reads, counting.reads)
	assert.Len(t, kube.Actions(), actions)
	cfg.invalidateLiveIntentCache()
	require.NoError(t, cfg.validateLiveIntent(ctx, liveIntent))
	assert.Greater(t, counting.reads, reads)
	cfg.liveIntentCache[liveIntent] = time.Now().Add(-time.Second)
	reads = counting.reads
	require.NoError(t, cfg.validateLiveIntent(ctx, liveIntent))
	assert.Greater(t, counting.reads, reads)
	cfg.invalidateLiveIntentCache()
	src.client = client
	for _, offer := range offers {
		assert.Equal(t, "artifact-uid", offer.ArtifactUID)
		assert.Equal(t, manifest.Digest, offer.Digest)
		assert.NotEqual(t, store.HexOf(manifest.Digest), offer.ManifestSHA256)
		assert.Equal(t, int64(len(file)), offer.TotalBytes)
		assert.EqualValues(t, 1, offer.Registrations)
		assert.NotEmpty(t, offer.ManifestJSON)
		instance := strings.Repeat("a", 32)
		lease, perr := b.RegisterWorker(offer.Generation, "pod-uid", offer.WorkerSecret, instance)
		require.Nil(t, perr)
		require.Nil(t, b.NativeStart(offer.Generation, "pod-uid", lease.ID, offer.WorkerSecret, instance))
		require.Nil(t, b.FillVerified(offer.Generation, "pod-uid", lease.ID, offer.WorkerSecret, 1,
			[]broker.RegistrationDescriptor{{Name: "checkpoint.safetensors", Addr: 4096, Length: int64(len(file)), MemoryType: "HOST", Backend: "nixl", WorkerUID: "pod-uid"}}, instance))
		derived, resolveErr := b.ResolveManifest(context.Background(), "tenant", "artifact-uid", manifest.Digest, 1)
		require.NoError(t, resolveErr)
		assert.Equal(t, offer.ManifestSHA256, derived.ManifestSHA256)
		_, resolveErr = b.ResolveManifest(context.Background(), "tenant", "artifact-uid", manifest.Digest, 2)
		require.Error(t, resolveErr)
		handler := broker.NewSessionHandler(b, sessionTestAuthorizer{digest: manifest.Digest})
		requestID, targetID := strings.Repeat("b", 32), strings.Repeat("c", 32)
		openBody, marshalErr := json.Marshal(map[string]any{
			"requestID": requestID, "targetInstanceID": targetID,
			"podNamespace": "tenant", "podName": "engine", "podUID": "engine-uid", "artifactName": "model",
			"artifactUID": "artifact-uid", "manifestDigest": manifest.Digest, "sourceGeneration": 0,
			"tensor": "model.norm.weight", "offset": 0, "length": 1,
		})
		require.NoError(t, marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(openBody))
		req.Header.Set("Authorization", "Bearer token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		// The source gate is open by default, so the admitted session answers
		// with its manifest and never with the worker secret.
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var answer map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &answer))
		assert.Equal(t, requestID, answer["requestID"])
		assert.NotEmpty(t, answer["sessionID"])
		assert.NotEmpty(t, answer["manifestJSON"])
		assert.NotContains(t, recorder.Body.String(), offer.WorkerSecret)
		// The admitted session closes and releases before the withdrawal
		// checks below, so no live session holds the generation open.
		closeOwner := broker.SessionOwner{
			PodUID: "engine-uid", Namespace: "tenant", ArtifactUID: "artifact-uid",
			Digest: manifest.Digest, TargetInstanceID: targetID,
			CloseRequestID: strings.Repeat("d", 32), TargetReleased: true,
		}
		sessionID := answer["sessionID"].(string)
		require.Nil(t, b.ClosePrivateSession(context.Background(), closeOwner, sessionID))
		require.Nil(t, b.ReleaseSessionContext(context.Background(), closeOwner, sessionID))
		currentPrefetch := &workercore.ModelPrefetch{}
		require.NoError(t, client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pf), currentPrefetch))
		cfg.invalidateLiveIntentCache()
		currentPrefetch.Spec.Source = nil
		require.NoError(t, client.Update(context.Background(), currentPrefetch))
		newOpen := bytes.Replace(openBody, []byte(requestID), []byte(strings.Repeat("e", 32)), 1)
		req = httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(newOpen))
		req.Header.Set("Authorization", "Bearer token")
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusConflict, recorder.Code)
		// The withdrawn intent refuses the session before the gate does, so
		// the two refusals stay apart.
		assert.NotContains(t, recorder.Body.String(), "acceptance record")
		currentPrefetch.Spec.Source = &workercore.ModelPrefetchSource{}
		require.NoError(t, client.Update(context.Background(), currentPrefetch))
	}
	src.client = failingIntentClient{Client: client}
	require.Error(t, broker.ReconcileIntents(context.Background(), b, src))
	assert.Len(t, b.OfferedAssignments(), 1)
	src.client = client
	podLive, err := kube.CoreV1().Pods("operator").Get(context.Background(), pod.Name, meta.GetOptions{})
	require.NoError(t, err)
	podLive.Spec.Containers[0].Image = "worker:other"
	_, err = kube.CoreV1().Pods("operator").Update(context.Background(), podLive, meta.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, broker.ReconcileIntents(context.Background(), b, src))
	assert.Empty(t, b.OfferedAssignments())
	cm, err = kube.CoreV1().ConfigMaps("operator").Get(context.Background(), broker.AuthorityName(binding.UID), meta.GetOptions{})
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(cm.Data["reservations.json"]), &document))
	entries := document["entries"].([]any)
	entries[0].(map[string]any)["ledgerIdentity"] = "lost-previous-ledger"
	changed, marshalErr := json.Marshal(document)
	require.NoError(t, marshalErr)
	cm.Data["reservations.json"] = string(changed)
	_, err = kube.CoreV1().ConfigMaps("operator").Update(context.Background(), cm, meta.UpdateOptions{})
	require.NoError(t, err)
	_, err = src.ListIntents(context.Background())
	require.Error(t, err, "a reservation from another ledger on this node must be held")
}

func TestReconcileIntentsRecoversRecreatedBindingAndReclaimsDeletedTargetPodSession(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	file, err := os.ReadFile(filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	sum := sha256.Sum256(file)
	fileDigest := "sha256:" + hex.EncodeToString(sum[:])
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}})
	require.NoError(t, err)
	attempt, err := st.NewAttempt(store.HexOf(manifest.Digest))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(attempt.Dir(), "tree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "tree", "checkpoint.safetensors"), file, 0o644))
	listing, err := json.Marshal(peer.Listing{Digest: manifest.Digest, Entries: []peer.ManifestFile{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"), listing, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{Digest: manifest.Digest, SizeBytes: int64(len(file)), ManifestFormat: modelartifact.ManifestHeader, Source: store.SourceHub}))

	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st, broker.Limits{PhysicalBytes: 1 << 30, MaxGenerations: 4, MaxQuarantined: 2, QuarantineBytes: 1 << 28, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.EnableOperationJournal())
	private := &testClosedSource{}
	b.SetPrivateSource(private)
	require.NoError(t, b.RequireAuthorityInventory("node-1"))

	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source-1", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: node.Name, Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}
	kube := kubefake.NewSimpleClientset(node, ds, pod)
	cmCounter := 0
	kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cmCounter++
		cm := action.(ktesting.CreateAction).GetObject().(*core.ConfigMap)
		cm.UID = meta.ObjectMeta{UID: pod.UID}.UID + "-" + meta.ObjectMeta{UID: ds.UID}.UID
		if cmCounter == 1 {
			cm.UID = "authority-uid-1"
		} else {
			cm.UID = "authority-uid-2"
		}
		cm.ResourceVersion = "1"
		return false, nil, nil
	})

	pf := &workercore.ModelPrefetch{ObjectMeta: meta.ObjectMeta{Name: "prefetch", Namespace: "tenant", UID: "prefetch-uid", Generation: 1}, Spec: workercore.ModelPrefetchSpec{ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"}, BindingRef: workercore.ModelPrefetchBindingReference{Name: "grant"}, Source: &workercore.ModelPrefetchSource{}, Placement: &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}}
	model := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "model", Namespace: "tenant", UID: "artifact-uid", Generation: 1}, Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"}}}, Status: workercore.ModelArtifactStatus{ObservedGeneration: 1, Resolved: &workercore.ModelArtifactResolved{ManifestDigest: manifest.Digest, SizeBytes: int64(len(file))}, Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}}}}
	binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid-1"}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store"}}, SourceMemory: &workercore.ModelStoreBindingSourceMemory{Bytes: resource.MustParse("1Gi"), Registrations: 4, Sessions: 4}}}
	modelStore := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}
	targetPod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "engine", Namespace: "tenant", UID: "engine-uid"}, Status: core.PodStatus{Phase: core.PodRunning}}

	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, pf, model, binding, modelStore, targetPod).Build()
	cfg := &Config{Store: st, Broker: b, Client: kube, WorkerNamespace: "operator", WorkerID: "pod-uid", NodeName: "node-1", VendorScope: "nvidia"}
	src := &kubernetesIntentSource{config: cfg, client: client}
	cfg.Intents = src
	b.SetIntentValidator(cfg.validateLiveIntent)
	b.SetSessionPodChecker(cfg.isSessionPodLive)

	require.NoError(t, broker.ReconcileIntents(context.Background(), b, src))
	offers := b.OfferedAssignments()
	require.Len(t, offers, 1)
	var firstOffer broker.Assignment
	for _, offer := range offers {
		firstOffer = offer
	}
	instance := strings.Repeat("a", 32)
	lease, perr := b.RegisterWorker(firstOffer.Generation, "pod-uid", firstOffer.WorkerSecret, instance)
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(firstOffer.Generation, "pod-uid", lease.ID, firstOffer.WorkerSecret, instance))
	require.Nil(t, b.FillVerified(firstOffer.Generation, "pod-uid", lease.ID, firstOffer.WorkerSecret, 1,
		[]broker.RegistrationDescriptor{{Name: "checkpoint.safetensors", Addr: 4096, Length: int64(len(file)), MemoryType: "HOST", Backend: "nixl", WorkerUID: "pod-uid"}}, instance))

	handler := broker.NewSessionHandler(b, sessionTestAuthorizer{digest: manifest.Digest})
	requestID, targetID := strings.Repeat("b", 32), strings.Repeat("c", 32)
	openBody, err := json.Marshal(map[string]any{
		"requestID": requestID, "targetInstanceID": targetID,
		"podNamespace": "tenant", "podName": "engine", "podUID": "engine-uid", "artifactName": "model",
		"artifactUID": "artifact-uid", "manifestDigest": manifest.Digest, "sourceGeneration": 0,
		"tensor": "model.norm.weight", "offset": 0, "length": 1,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(openBody))
	req.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// Delete the target Pod without closing its session, and recreate ModelStoreBinding with a new UID.
	require.NoError(t, client.Delete(context.Background(), targetPod))
	require.NoError(t, client.Delete(context.Background(), binding))
	recreatedBinding := &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid-2"},
		Spec:       binding.Spec,
	}
	require.NoError(t, client.Create(context.Background(), recreatedBinding))
	cfg.invalidateLiveIntentCache()

	// ReconcileIntents reclaims the terminated target Pod's idle session, retires the old intent, and admits the new Binding's intent.
	require.NoError(t, broker.ReconcileIntents(context.Background(), b, src))
	offers = b.OfferedAssignments()
	require.Len(t, offers, 1)
	var secondOffer broker.Assignment
	for _, offer := range offers {
		secondOffer = offer
	}
	assert.NotEqual(t, firstOffer.Generation, secondOffer.Generation)
	require.Nil(t, b.DrainAckContext(context.Background(), firstOffer.Generation, "pod-uid", lease.ID, firstOffer.WorkerSecret, instance))
}

func (a sessionTestAuthorizer) AuthenticateActivePod(ctx context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return a.AuthenticatePod(ctx, req)
}
