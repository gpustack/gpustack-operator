package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type operationTestSource struct {
	mu    sync.Mutex
	calls []string
	posts []map[string]any
}

func (s *operationTestSource) Call(_ context.Context, path string, payload any) (PrivateReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := payload.(map[string]any)
	s.calls = append(s.calls, path)
	id := body["requestID"].(string)
	switch path {
	case "/v1/operations":
		s.posts = append(s.posts, body)
		return PrivateReply{RequestID: id, State: "submitted"}, nil
	case "/v1/operations/status":
		return PrivateReply{RequestID: id, State: "terminal", Verdict: "succeeded"}, nil
	case "/v1/operations/ack":
		return PrivateReply{RequestID: id, State: "acknowledged", Verdict: "succeeded"}, nil
	default:
		return PrivateReply{}, nil
	}
}

func TestTransferOwnershipReplayAndWindowRelease(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	private := &operationTestSource{}
	b.SetPrivateSource(private)
	manifest := protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
		Files: []protocol.FileDescriptor{{Path: "weight.safetensors", Size: 12}},
		Tensors: []protocol.TensorDescriptor{{
			Name: "weight", File: "weight.safetensors", Dtype: "U8",
			Shape: []int64{8}, DataBegin: 2, DataEnd: 10,
		}},
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	intent := Intent{
		Key: "operation-intent", Namespace: "tenant", Digest: digest, TotalBytes: 12,
		Grant: grant("tenant", 100), WorkerID: "source", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "prefetch", ArtifactName: "model", ArtifactUID: "artifact",
		SourceGeneration: 1, BindingUID: "binding-uid-1", BindingName: "binding", ReservationID: "operation-reservation",
		Registrations: 1, Backend: "nixl", ManifestJSON: string(raw), ManifestSHA256: hex.EncodeToString(sum[:]),
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	lease, perr := b.RegisterWorker(assignment.Generation, "source", assignment.WorkerSecret, instance)
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, "source", lease.ID, assignment.WorkerSecret, instance))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, "source", lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{Name: "weight.safetensors", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: "source"}}, instance))

	targetInstance := strings.Repeat("b", 32)
	owner := SessionOwner{
		Namespace: "tenant", PodUID: "owner", ArtifactUID: "artifact", Digest: digest,
		RequestID: strings.Repeat("c", 32), TargetInstanceID: targetInstance,
	}
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", assignment.Generation)
	require.Nil(t, perr)
	otherOwner := owner
	otherOwner.PodUID, otherOwner.RequestID = "other", strings.Repeat("d", 32)
	otherSession, perr := b.AdmitSession(context.Background(), otherOwner, "tenant", assignment.Generation)
	require.Nil(t, perr)
	pod := authorization.PodIdentity{Namespace: "tenant", Name: "engine", UID: "owner", IP: "127.0.0.1"}
	otherPod := authorization.PodIdentity{Namespace: "tenant", Name: "engine-other", UID: "other", IP: "127.0.0.2"}
	request := TransferRequest{
		RequestID: strings.Repeat("e", 32), SessionID: session.ID, PodNamespace: "tenant", PodName: "engine", PodUID: "owner",
		TargetInstanceID: targetInstance, SourcePodUID: "source", WorkerInstanceID: instance, Generation: assignment.Generation,
		RegistrationID: strings.Repeat("f", 32), WindowID: strings.Repeat("1", 32), Tensor: "weight", TensorOffset: 2, Length: 4,
		Target: TransferTarget{Backend: "nixl", Address: 100, Length: 4, Metadata: "YQ=="},
	}
	foreign := request
	foreign.PodUID, foreign.PodName = otherPod.UID, otherPod.Name
	outOfRange := request
	outOfRange.TensorOffset = 5
	wrongGeneration := request
	wrongGeneration.WorkerInstanceID = strings.Repeat("2", 32)
	invalidTarget := request
	invalidTarget.Target.Metadata = "%"
	for _, tc := range []struct {
		name string
		req  TransferRequest
		pod  authorization.PodIdentity
	}{
		{name: "foreign owner", req: foreign, pod: otherPod},
		{name: "tensor range", req: outOfRange, pod: pod},
		{name: "source instance", req: wrongGeneration, pod: pod},
		{name: "target metadata", req: invalidTarget, pod: pod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.SubmitTransfer(context.Background(), tc.req, tc.pod)
			require.Error(t, err)
			require.Empty(t, private.calls)
		})
	}

	first, err := b.SubmitTransfer(context.Background(), request, pod)
	require.NoError(t, err)
	require.Equal(t, "submitted", first.State)
	require.Len(t, private.posts, 1)
	require.Equal(t, "weight.safetensors", private.posts[0]["file"])
	require.EqualValues(t, 4, private.posts[0]["fileOffset"])

	conflict := request
	conflict.TensorOffset = 1
	_, err = b.SubmitTransfer(context.Background(), conflict, pod)
	require.Error(t, err)
	other := request
	other.RequestID, other.SessionID = strings.Repeat("3", 32), otherSession.ID
	other.PodUID, other.PodName = otherPod.UID, otherPod.Name
	_, err = b.SubmitTransfer(context.Background(), other, otherPod)
	require.Error(t, err)
	require.Len(t, private.posts, 1)

	replay, err := b.SubmitTransfer(context.Background(), request, pod)
	require.NoError(t, err)
	require.Equal(t, "terminal", replay.State)
	require.Equal(t, "succeeded", replay.Verdict)
	_, err = b.AcknowledgeTransfer(context.Background(), request.RequestID, request, otherPod)
	require.Error(t, err)
	ack, err := b.AcknowledgeTransfer(context.Background(), request.RequestID, request, pod)
	require.NoError(t, err)
	require.Equal(t, "acknowledged", ack.State)
	_, err = b.SubmitTransfer(context.Background(), other, otherPod)
	require.NoError(t, err)
	require.Len(t, private.posts, 2)
}
