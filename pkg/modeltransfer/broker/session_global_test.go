package broker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestBindingSessionAndDrainRecoverLostResponses(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "session-intent", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
		IntentUID: "prefetch-uid", IntentGeneration: 1, BindingUID: "binding-uid-1",
		ReservationID: "session-reservation", Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	}
	assignment, err := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, err)
	lease, err := b.RegisterWorker(assignment.Generation, intent.WorkerID, assignment.WorkerSecret, "instance-a")
	require.Nil(t, err)
	require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{Name: "file", Addr: 1024, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID}}, "instance-a"))
	entries := b.Snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, intent.WorkerID, entries[0].SourcePodUID)
	assert.Equal(t, "instance-a", entries[0].SourceInstanceID)
	assert.EqualValues(t, intent.SourceGeneration, entries[0].SourceGeneration)
	require.NotNil(t, entries[0].LeaseExpiresAt)
	assert.True(t, entries[0].LeaseExpiresAt.Time.Equal(lease.Expires))
	owner := SessionOwner{PodUID: "pod", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: digest}
	session, err := b.AdmitSession(context.Background(), owner, intent.Namespace, assignment.Generation)
	require.Nil(t, err)
	authority := b.reservationAuthorities[types.UID(intent.BindingUID)]
	wrapper := &failingMaps{ConfigMapInterface: authority.maps.(coreclient.ConfigMapInterface)}
	authority.maps = wrapper
	wrapper.failOnUpdate.Store(true)
	require.NotNil(t, b.ReleaseSession(context.Background(), owner, session.ID))
	assert.Equal(t, "releasing", b.sessions[session.ID].Phase)
	wrapper.failOnUpdate.Store(false)
	require.NoError(t, b.RetrySessionTransactions(context.Background()))
	assert.Equal(t, "released", b.sessions[session.ID].Phase)
	require.Nil(t, b.ReleaseSession(context.Background(), owner, session.ID))
	require.Nil(t, b.Drain(assignment.Generation))
	require.Nil(t, b.DrainAck(context.Background(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
	require.Nil(t, b.DrainAck(context.Background(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
	assert.Equal(t, StateReleased, b.records[assignment.Generation].State)
	reserved, readErr := authority.Adopt(context.Background(), b.scope, b.ledger.identity)
	require.NoError(t, readErr)
	assert.Empty(t, reserved)
}

func TestBindingSessionOpenReplayAndTargetRelease(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetPrivateSource(closedTestSource{})
	intent := Intent{
		Key: "replay", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
		IntentUID: "prefetch", IntentGeneration: 1, BindingUID: "binding-uid-1",
		ReservationID: "replay-reservation", Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	lease, perr := b.RegisterWorker(assignment.Generation, intent.WorkerID, assignment.WorkerSecret, "instance")
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance"))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{Name: "file", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID}}, "instance"))
	owner := SessionOwner{
		PodUID: "pod", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: digest,
		RequestID: strings.Repeat("a", 32), TargetInstanceID: strings.Repeat("b", 32),
	}
	first, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, assignment.Generation)
	require.Nil(t, perr)
	second, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, assignment.Generation)
	require.Nil(t, perr)
	assert.Equal(t, first.ID, second.ID)
	closeOwner := owner
	closeOwner.CloseRequestID = strings.Repeat("c", 32)
	require.Nil(t, b.ClosePrivateSession(context.Background(), closeOwner, first.ID))
	require.Nil(t, b.ClosePrivateSession(context.Background(), closeOwner, first.ID))
	require.NotNil(t, b.ReleaseSession(context.Background(), closeOwner, first.ID))
	closeOwner.TargetReleased = true
	require.Nil(t, b.ReleaseSession(context.Background(), closeOwner, first.ID))
	require.Nil(t, b.ReleaseSession(context.Background(), closeOwner, first.ID))
	assert.EqualValues(t, 0, b.records[assignment.Generation].ActiveSessions)
}

type closedTestSource struct{}

func (closedTestSource) Call(_ context.Context, path string, _ any) (PrivateReply, error) {
	if path != "/v1/sessions/close" {
		return PrivateReply{}, fmt.Errorf("unexpected private request")
	}
	return PrivateReply{State: "closed"}, nil
}

func TestRetrySessionTransactionsReclaimsTerminatedPodOnlyWithoutUnacknowledgedOperations(t *testing.T) {
	for _, finalState := range []string{"acknowledged", "prepared"} {
		t.Run(finalState, func(t *testing.T) {
			b, _, digest := newBroker(t, defaultLimits())
			require.NoError(t, b.EnableOperationJournal())
			b.SetPrivateSource(closedTestSource{})
			intent := Intent{
				Key: "terminated-pod-intent", Namespace: "tenant-a", Digest: digest,
				TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
				IntentUID: "prefetch-uid", IntentGeneration: 1, BindingUID: "binding-uid-1",
				ReservationID: "terminated-pod-reservation", Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
			}
			assignment, perr := b.AdmitIntentFill(t.Context(), intent)
			require.Nil(t, perr)
			lease, perr := b.RegisterWorker(assignment.Generation, intent.WorkerID, assignment.WorkerSecret, "instance-a")
			require.Nil(t, perr)
			require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
			require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, 1,
				[]RegistrationDescriptor{{Name: "file", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID}}, "instance-a"))

			owner := SessionOwner{
				PodUID: "target-pod-uid", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: digest,
				RequestID: strings.Repeat("a", 32), TargetInstanceID: strings.Repeat("b", 32),
			}
			session, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, assignment.Generation)
			require.Nil(t, perr)

			opID := strings.Repeat("c", 32)
			b.mu.Lock()
			b.operations[opID] = &OperationRecord{
				Request: TransferRequest{
					RequestID:        opID,
					SessionID:        session.ID,
					PodNamespace:     intent.Namespace,
					PodName:          "target-pod",
					PodUID:           owner.PodUID,
					TargetInstanceID: owner.TargetInstanceID,
					Generation:       assignment.Generation,
				},
				State:         "submitted",
				SourceLeaseID: lease.ID,
			}
			b.mu.Unlock()

			podLive := false
			b.SetSessionPodChecker(func(_ context.Context, namespace, podUID string) (bool, error) {
				assert.Equal(t, intent.Namespace, namespace)
				assert.Equal(t, owner.PodUID, podUID)
				return podLive, nil
			})

			cases := []struct {
				name          string
				opState       string
				podLive       bool
				wantPhase     string
				wantActive    int32
				wantDrainable bool
			}{
				{
					name:          "terminated pod with submitted operation stays fail-closed",
					opState:       "submitted",
					podLive:       false,
					wantPhase:     "admitted",
					wantActive:    1,
					wantDrainable: false,
				},
				{
					name:          "terminated pod with unknown operation stays fail-closed",
					opState:       "unknown",
					podLive:       false,
					wantPhase:     "admitted",
					wantActive:    1,
					wantDrainable: false,
				},
				{
					name:    "live pod with prepared operation stays admitted",
					opState: "prepared", podLive: true, wantPhase: "admitted", wantActive: 1,
				},
				{
					name:          "live pod with acknowledged operation stays admitted",
					opState:       "acknowledged",
					podLive:       true,
					wantPhase:     "admitted",
					wantActive:    1,
					wantDrainable: false,
				},
				{
					name:          "terminated pod with safe operation closes and releases",
					opState:       finalState,
					podLive:       false,
					wantPhase:     "released",
					wantActive:    0,
					wantDrainable: true,
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					b.mu.Lock()
					b.operations[opID].State = tc.opState
					b.mu.Unlock()
					podLive = tc.podLive

					require.NoError(t, b.RetrySessionTransactions(context.Background()))
					assert.Equal(t, tc.wantPhase, b.sessions[session.ID].Phase)
					assert.Equal(t, tc.wantActive, b.records[assignment.Generation].ActiveSessions)
					if tc.wantDrainable {
						require.Nil(t, b.Drain(assignment.Generation))
						require.Nil(t, b.DrainAck(context.Background(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
						assert.Equal(t, StateReleased, b.records[assignment.Generation].State)
					}
				})
			}
		})
	}
}
