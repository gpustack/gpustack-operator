package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func TestSessionManifestPagesAuthenticateMetadataOwner(t *testing.T) {
	b, request, pod, manifest := manifestPageFixture(t, 1, 0)
	targetInstance := request.TargetInstanceID
	pages := manifest.TensorPages
	for _, tc := range []struct {
		name     string
		owner    string
		instance string
		page     string
		want     bool
	}{
		{name: "metadata before target registration", owner: "pod-uid-tenant-a", instance: targetInstance, page: pages[0].SHA256, want: true},
		{name: "foreign owner", owner: "foreign", instance: targetInstance, page: pages[0].SHA256},
		{name: "foreign target instance", owner: "pod-uid-tenant-a", instance: strings.Repeat("d", 32), page: pages[0].SHA256},
		{name: "page outside root", owner: "pod-uid-tenant-a", instance: targetInstance, page: strings.Repeat("f", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, identity := request, pod
			req.PodUID, identity.UID = tc.owner, tc.owner
			req.TargetInstanceID = tc.instance
			req.PageSHA256 = tc.page
			raw, err := b.sessionManifestPage(context.Background(), req, identity)
			if !tc.want {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			hash := sha256.Sum256(raw)
			require.Equal(t, pages[0].SHA256, hex.EncodeToString(hash[:]))
		})
	}
	for _, tc := range []struct {
		name        string
		phase       core.PodPhase
		terminating bool
		wantStatus  int
	}{
		{name: "live owner", phase: core.PodRunning, wantStatus: http.StatusOK},
		{name: "terminating owner", phase: core.PodRunning, terminating: true, wantStatus: http.StatusForbidden},
		{name: "finished owner", phase: core.PodSucceeded, wantStatus: http.StatusForbidden},
		{name: "failed owner", phase: core.PodFailed, wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: types.UID(pod.UID)}, Spec: core.PodSpec{ServiceAccountName: "reader"}, Status: core.PodStatus{Phase: tc.phase}}
			if tc.terminating {
				now := meta.Now()
				p.DeletionTimestamp = &now
				p.Finalizers = []string{"test.gpustack.ai/hold"}
			}
			auth := &authorization.Authorizer{Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(p).Build(), Reviewer: sessionReviewer{}}
			body, err := json.Marshal(request)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/v1/manifest-pages", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer projected-token")
			response := httptest.NewRecorder()
			NewSessionHandler(b, auth).ServeHTTP(response, req)
			require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			_, authErr := auth.AuthenticatePod(context.Background(), authorization.Request{Token: "projected-token", PodNamespace: pod.Namespace, PodName: pod.Name, PodUID: pod.UID})
			require.Nil(t, authErr, "cleanup remains available")
		})
	}
}

func manifestPageFixture(t *testing.T, count, padding int) (*Broker, manifestPageRequest, authorization.PodIdentity, protocol.Manifest) {
	t.Helper()
	limits := defaultLimits()
	limits.PhysicalBytes = 1 << 24
	b, _, digest := newBroker(t, limits)
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	private := &operationTestSource{}
	b.SetPrivateSource(private)
	manifest := protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
		Files: []protocol.FileDescriptor{{Path: "weight.safetensors", Size: int64(count + 2)}}, Tensors: []protocol.TensorDescriptor{},
	}
	pagesDir := t.TempDir()
	pages, err := protocol.WriteManifestPages(context.Background(), pagesDir, manifest.Files, func(yield func(protocol.TensorDescriptor, error) bool) {
		for i := range count {
			tensor := protocol.TensorDescriptor{Name: fmt.Sprintf("%06d", i) + strings.Repeat("x", padding), File: "weight.safetensors", Dtype: "U8", Shape: []int64{1}, DataBegin: int64(i + 1), DataEnd: int64(i + 2)}
			if !yield(tensor, nil) {
				return
			}
		}
	})
	require.NoError(t, err)
	manifest.TensorPages, manifest.Tensors = pages, []protocol.TensorDescriptor{}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	intent := Intent{
		Key: "operation-intent", Namespace: "tenant-a", Digest: digest, TotalBytes: int64(count + 2),
		Grant: grant("tenant-a", 1<<20), WorkerID: "source", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "prefetch", ArtifactName: "model", ArtifactUID: "artifact",
		SourceGeneration: 1, BindingUID: "binding-uid-1", BindingName: "binding", ReservationID: "operation-reservation",
		Registrations: 1, Backend: "nixl", ManifestJSON: string(raw), ManifestSHA256: hex.EncodeToString(sum[:]), ManifestPagesPath: pagesDir,
	}
	assignment, perr := b.AdmitIntentFill(intent)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	lease, perr := b.RegisterWorker(assignment.Generation, "source", assignment.WorkerSecret, instance)
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, "source", lease.ID, assignment.WorkerSecret, instance))
	require.Nil(t, b.FillVerified(assignment.Generation, "source", lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{Name: "weight.safetensors", Addr: 1, Length: int64(count + 2), MemoryType: "HOST", Backend: "nixl", WorkerUID: "source"}}, instance))

	targetInstance := strings.Repeat("b", 32)
	owner := SessionOwner{
		Namespace: "tenant-a", PodUID: "pod-uid-tenant-a", ArtifactUID: "artifact", Digest: digest,
		RequestID: strings.Repeat("c", 32), TargetInstanceID: targetInstance,
	}
	session, perr := b.AdmitSessionContext(context.Background(), owner, "tenant-a", assignment.Generation)
	require.Nil(t, perr)

	pod := authorization.PodIdentity{Namespace: "tenant-a", Name: "engine-1", UID: "pod-uid-tenant-a", IP: "127.0.0.1"}
	request := manifestPageRequest{
		PodNamespace: pod.Namespace, PodName: pod.Name, PodUID: pod.UID,
		SessionID: session.ID, TargetInstanceID: targetInstance, SourcePodUID: "source", WorkerInstanceID: instance,
		Generation: assignment.Generation, ManifestSHA256: hex.EncodeToString(sum[:]), PageSHA256: pages[0].SHA256,
	}
	return b, request, pod, manifest
}

func TestPagedManifestPythonHTTPBeyondLegacyLimit(t *testing.T) {
	const count = 70000
	b, request, pod, manifest := manifestPageFixture(t, count, 1024)
	total := 0
	for _, page := range manifest.TensorPages {
		total += page.Size
	}
	require.Greater(t, total, protocol.MaxManifestBytes)
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: types.UID(pod.UID)}, Spec: core.PodSpec{ServiceAccountName: "reader"}, Status: core.PodStatus{Phase: core.PodRunning}}
	auth := &authorization.Authorizer{Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(p).Build(), Reviewer: sessionReviewer{}}
	server := httptest.NewTLSServer(NewSessionHandler(b, auth))
	defer server.Close()
	directory := t.TempDir()
	root, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "root.json"), root, 0o600))
	body, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "request.json"), body, 0o600))
	pythonRoot, err := filepath.Abs("../../../model-transfer/python")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", `
import hashlib,json,pathlib,sys,ssl,urllib.request
from gpustack_model_transfer.core.manifest import parse_manifest
root=pathlib.Path(sys.argv[1]);request=json.loads((root/'request.json').read_text());sizes=[]
def page_reader(page):
 payload=dict(request,pageSHA256=page['sha256'])
 r=urllib.request.urlopen(urllib.request.Request(sys.argv[2]+'/v1/manifest-pages',data=json.dumps(payload).encode(),headers={'Authorization':'Bearer projected-token','Content-Type':'application/json'}),context=ssl._create_unverified_context(),timeout=20)
 value=json.load(r);assert value['manifestSHA256']==request['manifestSHA256'] and value['pageSHA256']==page['sha256']
 raw=value['pageJSON'].encode();sizes.append(len(raw));return raw
raw=(root/'root.json').read_bytes();manifest=parse_manifest(raw,hashlib.sha256(raw).hexdigest(),page_reader=page_reader)
assert len(manifest.tensors)==70000 and not isinstance(manifest.tensors,(list,tuple))
assert manifest.tensor('069999'+'x'*1024).data_end==70001
assert sum(sizes)>64<<20 and max(sizes)<=4<<20
print(json.dumps({'tensors':len(manifest.tensors),'pageBytes':sum(sizes),'largestPage':max(sizes),'requests':len(sizes)}))
`, directory, server.URL)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonRoot)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Log(string(output))
}
