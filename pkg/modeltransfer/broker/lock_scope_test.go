package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"

	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// The regressions in this file hold the broker's mutual exclusion honest:
// slow external work — the live intent validator, the Binding authority's
// ConfigMap CAS, and manifest page reads — must run OUTSIDE b.mu with
// snapshot revalidation, so one blocked request can never head-of-line
// block an unrelated heartbeat, assignment poll, or status read. Every
// blocker is a channel gate, never a sleep.

// gatingMaps arms exactly one ConfigMap Update behind a channel: the first
// update signals entered and blocks until release is closed, so a test can
// hold the authority CAS open deterministically.
type gatingMaps struct {
	coreclient.ConfigMapInterface
	armed       atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (g *gatingMaps) Update(ctx context.Context, cm *core.ConfigMap,
	opts meta.UpdateOptions,
) (*core.ConfigMap, error) {
	if g.armed.CompareAndSwap(true, false) {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		<-g.release
	}
	return g.ConfigMapInterface.Update(ctx, cm, opts)
}

// releaseGate lets every current and future gated write through; safe to
// call from any number of sites.
func (g *gatingMaps) releaseGate() {
	g.releaseOnce.Do(func() { close(g.release) })
}

func gateAuthority(b *Broker, bindingUID string) *gatingMaps {
	authority := b.reservationAuthorities[types.UID(bindingUID)]
	gated := &gatingMaps{
		ConfigMapInterface: authority.maps.(coreclient.ConfigMapInterface),
		entered:            make(chan struct{}, 1),
		release:            make(chan struct{}),
	}
	gated.armed.Store(true)
	authority.maps = gated
	return gated
}

// awaitGateEntered fails the test if the gated external call is never
// reached; the bound only catches a broken fixture, never times a pass.
func awaitGateEntered(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the gated external call was never entered")
	}
}

// mustRunWhileBlocked runs one broker request and requires it to complete
// while the gate stays closed. On the serialized broker this cannot
// terminate, which is exactly the failure the regression reports.
func mustRunWhileBlocked(t *testing.T, name string, run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked behind the gated external call", name)
	}
}

// gatedIntentValidator blocks every live intent check behind a channel.
func gatedIntentValidator(entered, release chan struct{}) func(context.Context, LiveIntent) error {
	return func(context.Context, LiveIntent) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
}

// pagedSourceFixture admits one intent-bound generation whose derived
// manifest is paged, serves it, and admits one stable-id session. It wires
// the operation journal, the private source, and the worker bootstrap, and
// leaves the intent validator unset: every test installs its own.
type pagedSourceFixture struct {
	b            *Broker
	digest       string
	generation   string
	workerSecret string
	leaseID      string
	instance     string
	pagesDir     string
	manifest     protocol.Manifest
	pageRequest  manifestPageRequest
	pod          authorization.PodIdentity
	owner        SessionOwner
	sessionID    string
}

func newPagedSourceFixture(t *testing.T) *pagedSourceFixture {
	t.Helper()
	limits := defaultLimits()
	limits.PhysicalBytes = 1 << 24
	return newPagedSourceFixtureWithLimits(t, limits)
}

func newPagedSourceFixtureWithLimits(t *testing.T, limits Limits) *pagedSourceFixture {
	t.Helper()
	f := &pagedSourceFixture{}
	b, _, digest := newBroker(t, limits)
	require.NoError(t, b.EnableOperationJournal())
	b.SetPrivateSource(closedTestSource{})
	b.SetBootstrapCredential("worker-bootstrap")
	f.b, f.digest, f.instance = b, digest, strings.Repeat("a", 32)
	f.manifest = protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
		Files: []protocol.FileDescriptor{{Path: "weight.safetensors", Size: 3}}, Tensors: []protocol.TensorDescriptor{},
	}
	f.pagesDir = t.TempDir()
	pages, err := protocol.WriteManifestPages(context.Background(), f.pagesDir, f.manifest.Files,
		func(yield func(protocol.TensorDescriptor, error) bool) {
			tensor := protocol.TensorDescriptor{
				Name: "weight", File: "weight.safetensors", Dtype: "U8",
				Shape: []int64{1}, DataBegin: 1, DataEnd: 2,
			}
			yield(tensor, nil)
		})
	require.NoError(t, err)
	f.manifest.TensorPages, f.manifest.Tensors = pages, []protocol.TensorDescriptor{}
	raw, err := json.Marshal(f.manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	intent := Intent{
		Key: "lock-scope-intent", Namespace: "tenant-a", Digest: digest, TotalBytes: 3,
		Grant: grant("tenant-a", 1<<20), WorkerID: "source", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "prefetch", ArtifactName: "model", ArtifactUID: "artifact",
		SourceGeneration: 1, BindingUID: "binding-uid-1", BindingName: "binding", ReservationID: "lock-scope-reservation",
		Registrations: 1, Backend: "nixl", ManifestJSON: string(raw), ManifestSHA256: hex.EncodeToString(sum[:]),
		ManifestPagesPath: f.pagesDir,
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	f.generation, f.workerSecret = assignment.Generation, assignment.WorkerSecret
	lease, perr := b.RegisterWorker(f.generation, "source", f.workerSecret, f.instance)
	require.Nil(t, perr)
	f.leaseID = lease.ID
	require.Nil(t, b.NativeStart(f.generation, "source", f.leaseID, f.workerSecret, f.instance))
	require.Nil(t, b.FillVerified(t.Context(), f.generation, "source", f.leaseID, f.workerSecret, 1,
		[]RegistrationDescriptor{{
			Name: "weight.safetensors", Addr: 1, Length: 3, MemoryType: "HOST",
			Backend: "nixl", WorkerUID: "source",
		}}, f.instance))
	f.owner = SessionOwner{
		Namespace: "tenant-a", PodUID: "pod-uid-tenant-a", ArtifactUID: "artifact", Digest: digest,
		RequestID: strings.Repeat("c", 32), TargetInstanceID: strings.Repeat("b", 32),
	}
	session, perr := b.AdmitSession(context.Background(), f.owner, "tenant-a", f.generation)
	require.Nil(t, perr)
	f.sessionID = session.ID
	f.pod = authorization.PodIdentity{
		Namespace: "tenant-a", Name: "engine-1",
		UID: "pod-uid-tenant-a", IP: "127.0.0.1",
	}
	f.pageRequest = manifestPageRequest{
		PodNamespace: f.pod.Namespace, PodName: f.pod.Name, PodUID: f.pod.UID,
		SessionID: f.sessionID, TargetInstanceID: f.owner.TargetInstanceID, SourcePodUID: "source",
		WorkerInstanceID: f.instance, Generation: f.generation,
		ManifestSHA256: hex.EncodeToString(sum[:]), PageSHA256: pages[0].SHA256,
	}
	return f
}

// workerRequests answers the three worker/status surfaces an unrelated
// caller uses while another request holds slow external work.
func (f *pagedSourceFixture) workerRequests(t *testing.T) {
	t.Helper()
	result, herr := f.b.Heartbeat(f.generation, "source", f.leaseID, f.workerSecret, Health{}, f.instance)
	require.Nil(t, herr)
	require.Equal(t, ActionNone, result.Action)
	assignments, aerr := f.b.AssignmentsFor("worker-bootstrap")
	require.Nil(t, aerr)
	require.NotEmpty(t, assignments)
	view, verr := f.b.SessionView(f.sessionID)
	require.Nil(t, verr)
	require.Equal(t, f.generation, view.Generation)
	entitlement, terr := f.b.TransferSession(f.sessionID, f.owner.PodUID, f.owner.TargetInstanceID)
	require.NoError(t, terr)
	require.Equal(t, f.digest, entitlement.Digest)
}

// authoritySessionOwners counts the session owners this node holds in the
// Binding authority document.
func authoritySessionOwners(t *testing.T, b *Broker, bindingUID string) int {
	t.Helper()
	authority := b.reservationAuthorities[types.UID(bindingUID)]
	entries, err := authority.Adopt(context.Background(), b.scope, b.ledger.identity)
	require.NoError(t, err)
	total := 0
	for _, entry := range entries {
		total += len(entry.Sessions)
	}
	return total
}

func TestBlockedIntentValidatorDoesNotBlockWorkerAndStatusRequests(t *testing.T) {
	for _, route := range []string{"manifest-page", "transfer-submit"} {
		t.Run(route, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
			defer closeRelease()

			if route == "manifest-page" {
				f := newPagedSourceFixture(t)
				f.b.SetIntentValidator(gatedIntentValidator(entered, release))
				done := make(chan struct{})
				var pageErr error
				go func() {
					defer close(done)
					_, pageErr = f.b.sessionManifestPage(context.Background(), f.pageRequest, f.pod)
				}()
				awaitGateEntered(t, entered)
				f.workerRequests(t)
				closeRelease()
				<-done
				require.NoError(t, pageErr)
				return
			}

			// The transfer route gates the same validator mid-submit.
			transfer := newTransferFixture(t)
			transfer.b.SetIntentValidator(gatedIntentValidator(entered, release))
			done := make(chan struct{})
			var submitErr error
			go func() {
				defer close(done)
				_, submitErr = transfer.b.SubmitTransfer(context.Background(), transfer.request, transfer.pod)
			}()
			awaitGateEntered(t, entered)
			transfer.workerRequests(t)
			closeRelease()
			<-done
			require.NoError(t, submitErr)
			require.Len(t, transfer.private.posts, 1)
		})
	}
}

func TestBlockedAuthoritySessionCASDoesNotBlockWorkerAndStatusRequests(t *testing.T) {
	f := newPagedSourceFixture(t)
	gated := gateAuthority(f.b, "binding-uid-1")
	newOwner := f.owner
	newOwner.PodUID = "pod-uid-second"
	newOwner.RequestID = strings.Repeat("d", 32)
	done := make(chan struct{})
	var admission *Session
	var admissionErr *protocol.Error
	go func() {
		defer close(done)
		admission, admissionErr = f.b.AdmitSession(context.Background(), newOwner, "tenant-a", f.generation)
	}()
	awaitGateEntered(t, gated.entered)
	f.workerRequests(t)
	gated.releaseGate()
	<-done
	require.Nil(t, admissionErr)
	require.NotNil(t, admission)
	require.Equal(t, 2, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

func TestBlockedAuthorityReleaseAndDrainCASDoNotBlockWorkerAndStatusRequests(t *testing.T) {
	f := newPagedSourceFixture(t)
	// The release route: a closing session whose target release is
	// confirmed reaches ReleaseSessionSlotIfPresent under the lock today.
	closeOwner := f.owner
	closeOwner.CloseRequestID = strings.Repeat("e", 32)
	closeOwner.TargetReleased = true
	require.Nil(t, f.b.ClosePrivateSession(context.Background(), closeOwner, f.sessionID))
	gated := gateAuthority(f.b, "binding-uid-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.b.ReleaseSession(context.Background(), closeOwner, f.sessionID)
	}()
	awaitGateEntered(t, gated.entered)
	// During the release window the session reads as releasing, so the
	// worker surfaces stay: heartbeats and assignment polls must proceed.
	releaseResult, releaseErr := f.b.Heartbeat(f.generation, "source", f.leaseID, f.workerSecret, Health{}, f.instance)
	require.Nil(t, releaseErr)
	require.Equal(t, ActionNone, releaseResult.Action)
	_, releasePollErr := f.b.AssignmentsFor("worker-bootstrap")
	require.Nil(t, releasePollErr)
	gated.releaseGate()
	<-done
	require.Equal(t, "released", f.b.sessions[f.sessionID].Phase)
	require.Equal(t, 0, authoritySessionOwners(t, f.b, "binding-uid-1"))

	// The drain route: DrainAck reaches MarkDrained and then the release CAS.
	require.Nil(t, f.b.Drain(f.generation))
	gated.armed.Store(true)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_ = f.b.DrainAck(context.Background(), f.generation, "source", f.leaseID, f.workerSecret, f.instance)
	}()
	awaitGateEntered(t, gated.entered)
	result, herr := f.b.Heartbeat(f.generation, "source", f.leaseID, f.workerSecret, Health{}, f.instance)
	require.Nil(t, herr)
	require.Equal(t, ActionDrain, result.Action)
	_, aerr := f.b.AssignmentsFor("worker-bootstrap")
	require.Nil(t, aerr)
	gated.releaseGate()
	<-drainDone
	require.Equal(t, StateReleased, f.b.records[f.generation].State)
	require.Equal(t, 0, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

func TestStaleGlobalAdmissionRefusedAfterDrainDuringBlockedValidator(t *testing.T) {
	f := newPagedSourceFixture(t)
	// Production sets the live entitlement gate; the fixture mirrors it so
	// the admission runs the validator it must wait on.
	f.b.inventoryRequired, f.b.inventoryVerified = true, true
	entered := make(chan struct{}, 1)
	gatedRelease := make(chan struct{})
	var releaseOnce sync.Once
	closeGatedRelease := func() { releaseOnce.Do(func() { close(gatedRelease) }) }
	defer closeGatedRelease()
	f.b.SetIntentValidator(func(context.Context, LiveIntent) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gatedRelease
		return nil
	})
	newOwner := f.owner
	newOwner.PodUID = "pod-uid-stale"
	newOwner.RequestID = strings.Repeat("g", 32)

	done := make(chan struct{})
	var session *Session
	var perr *protocol.Error
	go func() {
		defer close(done)
		session, perr = f.b.AdmitSession(context.Background(), newOwner, "tenant-a", f.generation)
	}()
	awaitGateEntered(t, entered)
	// The generation drains while the admission waits on the live check.
	mustRunWhileBlocked(t, "Drain", func() {
		require.Nil(t, f.b.Drain(f.generation))
	})
	closeGatedRelease()
	<-done
	require.NotNil(t, perr, "the stale admission must be refused")
	require.Equal(t, protocol.KindStaleGeneration, perr.Kind)
	require.Nil(t, session)
	for _, record := range f.b.sessions {
		if record.OwnerUID == newOwner.PodUID {
			t.Fatalf("a stale admission published a session")
		}
	}
	require.EqualValues(t, 1, f.b.records[f.generation].ActiveSessions)
	require.Equal(t, 1, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

func TestRetryPassLeavesAnInFlightAdmissionAlone(t *testing.T) {
	f := newPagedSourceFixture(t)
	f.b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	gated := gateAuthority(f.b, "binding-uid-1")
	newOwner := f.owner
	newOwner.PodUID = "pod-uid-retry"
	newOwner.RequestID = strings.Repeat("f", 32)
	admissionDone := make(chan struct{})
	go func() {
		defer close(admissionDone)
		_, _ = f.b.AdmitSession(context.Background(), newOwner, "tenant-a", f.generation)
	}()
	awaitGateEntered(t, gated.entered)
	// The retry pass must complete while the admission waits, and must
	// neither release the prepared session nor touch its charge.
	mustRunWhileBlocked(t, "RetrySessionTransactions", func() {
		require.NoError(t, f.b.RetrySessionTransactions(context.Background()))
	})
	f.b.mu.Lock()
	prepared := ""
	for id, session := range f.b.sessions {
		if session.OwnerUID == newOwner.PodUID {
			prepared = id
			require.Equal(t, "prepared", session.Phase)
		}
	}
	f.b.mu.Unlock()
	require.NotEmpty(t, prepared, "the in-flight admission's prepared record must stand")
	gated.releaseGate()
	<-admissionDone
	require.Equal(t, "admitted", f.b.sessions[prepared].Phase)
	require.Equal(t, 2, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

// forceCrashCutPrepared rewinds the fixture's admitted session to the crash
// cut this file studies: the durable phase says prepared while the
// authority slot the pre-crash CAS reserved is still present.
func forceCrashCutPrepared(t *testing.T, f *pagedSourceFixture) {
	t.Helper()
	f.b.mu.Lock()
	session := f.b.sessions[f.sessionID]
	require.NotNil(t, session)
	session.Phase = "prepared"
	require.Nil(t, f.b.persistSessionsLocked())
	rec := f.b.records[f.generation]
	f.b.recountSessions(rec)
	f.b.mu.Unlock()
	require.Equal(t, 1, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

func TestRetryReleasesACrashCutPreparedSessionSlot(t *testing.T) {
	f := newPagedSourceFixture(t)
	forceCrashCutPrepared(t, f)
	require.Nil(t, f.b.RetrySessionTransactions(context.Background()))
	f.b.mu.Lock()
	require.Equal(t, "released", f.b.sessions[f.sessionID].Phase)
	f.b.mu.Unlock()
	require.Equal(t, 0, authoritySessionOwners(t, f.b, "binding-uid-1"))
	require.EqualValues(t, 0, f.b.records[f.generation].ActiveSessions)
}

func TestRetryClaimFencesConcurrentAdmissionReplay(t *testing.T) {
	f := newPagedSourceFixture(t)
	forceCrashCutPrepared(t, f)
	gated := gateAuthority(f.b, "binding-uid-1")
	retryDone := make(chan struct{})
	go func() {
		defer close(retryDone)
		_ = f.b.RetrySessionTransactions(context.Background())
	}()
	// The pass claims the crash-cut session prepared -> releasing under the
	// mutex, then blocks in its release CAS.
	awaitGateEntered(t, gated.entered)
	// A same-id admission replay that starts now must refuse the claimed
	// release: no admitted session may exist whose slot a release removes.
	_, perr := f.b.AdmitSession(context.Background(), f.owner, "tenant-a", f.generation)
	require.NotNil(t, perr, "an admission over a claimed release must be refused")
	require.Equal(t, protocol.KindStaleGeneration, perr.Kind)
	f.b.mu.Lock()
	require.Equal(t, "releasing", f.b.sessions[f.sessionID].Phase)
	f.b.mu.Unlock()
	gated.releaseGate()
	<-retryDone
	f.b.mu.Lock()
	require.Equal(t, "released", f.b.sessions[f.sessionID].Phase)
	f.b.mu.Unlock()
	require.Equal(t, 0, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

// TestFillPublishRerunsNativeStartProof proves the publication stage of
// FillVerified re-runs the entry checks exactly: native-start proof lost
// while a gated manifest read was in flight refuses Serving instead of
// publishing it.
func TestFillPublishRerunsNativeStartProof(t *testing.T) {
	f := newPagedSourceFixture(t)
	// Rewind the served generation to the crash cut: durable descriptors
	// exist, but the record is Filling again and the manifest cache is
	// cold, so the plan validation must read pages through the gate.
	f.b.mu.Lock()
	rec := f.b.records[f.generation]
	rec.State = StateFilling
	f.b.manifestCache = nil
	f.b.mu.Unlock()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.b.setPageReader(func(directory string) protocol.PageReader {
		inner := protocol.LocalPageReader(directory)
		return func(ctx context.Context, page protocol.TensorPage) ([]byte, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return inner(ctx, page)
		}
	})
	done := make(chan *protocol.Error, 1)
	go func() {
		defer close(done)
		done <- f.b.FillVerified(t.Context(), f.generation, "source", f.leaseID, f.workerSecret, 1,
			[]RegistrationDescriptor{{
				Name: "weight.safetensors", Addr: 1, Length: 3,
				MemoryType: "HOST", Backend: "nixl", WorkerUID: "source",
			}}, f.instance)
	}()
	awaitGateEntered(t, entered)
	// The native-start proof disappears while the manifest read waits.
	f.b.mu.Lock()
	f.b.records[f.generation].NativeStarted = false
	f.b.mu.Unlock()
	unblock()
	perr := <-done
	require.NotNil(t, perr, "lost native-start proof must refuse publication")
	require.Equal(t, protocol.KindAuthentication, perr.Kind)
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	require.Equal(t, StateFilling, f.b.records[f.generation].State)
}

func TestBlockedManifestPageReaderDoesNotBlockWorkerAndStatusRequests(t *testing.T) {
	f := newPagedSourceFixture(t)
	f.b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	f.b.setPageReader(func(directory string) protocol.PageReader {
		inner := protocol.LocalPageReader(directory)
		return func(ctx context.Context, page protocol.TensorPage) ([]byte, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return inner(ctx, page)
		}
	})
	done := make(chan struct{})
	var raw []byte
	var workerPageErr error
	go func() {
		defer close(done)
		raw, workerPageErr = f.b.workerManifestPage(context.Background(), "worker-bootstrap", f.pageRequest)
	}()
	awaitGateEntered(t, entered)
	f.workerRequests(t)
	closeRelease()
	<-done
	require.NoError(t, workerPageErr)
	require.NotEmpty(t, raw)
}

func TestConcurrentSessionAdmissionsRespectTheNodeCapAndGlobalSlots(t *testing.T) {
	limits := defaultLimits()
	limits.MaxSessions = 2
	f := newPagedSourceFixtureWithLimits(t, limits)
	f.b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	const callers = 6
	sessions := make([]*Session, callers)
	errs := make([]*protocol.Error, callers)
	var start sync.WaitGroup
	start.Add(callers)
	for i := 0; i < callers; i++ {
		owner := f.owner
		owner.PodUID = "pod-uid-cap-" + string(rune('a'+i))
		owner.RequestID = strings.Repeat(string(rune('a'+i)), 32)
		go func(slot int, owner SessionOwner) {
			defer start.Done()
			sessions[slot], errs[slot] = f.b.AdmitSession(context.Background(), owner, "tenant-a", f.generation)
		}(i, owner)
	}
	start.Wait()
	admitted := 0
	for i := range callers {
		if errs[i] == nil {
			admitted++
			require.NotNil(t, sessions[i])
			continue
		}
		require.Equal(t, protocol.KindCapacityExhausted, errs[i].Kind)
	}
	// The fixture's own session already holds one of the two slots.
	require.LessOrEqual(t, admitted, 1, "the node session cap must hold under concurrent admission")
	require.Equal(t, admitted+1, authoritySessionOwners(t, f.b, "binding-uid-1"))
}

func TestConcurrentSameOwnerAdmissionsShareOneSessionAndSlot(t *testing.T) {
	f := newPagedSourceFixture(t)
	f.b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	const callers = 4
	sessions := make([]*Session, callers)
	errs := make([]*protocol.Error, callers)
	var start sync.WaitGroup
	start.Add(callers)
	for i := 0; i < callers; i++ {
		go func(slot int) {
			defer start.Done()
			sessions[slot], errs[slot] = f.b.AdmitSession(context.Background(), f.owner, "tenant-a", f.generation)
		}(i)
	}
	start.Wait()
	first := sessions[0]
	for i := range callers {
		require.Nil(t, errs[i])
		require.Equal(t, first.ID, sessions[i].ID)
	}
	// Every caller replays the fixture's own stable session: one session,
	// one slot, one charge.
	require.Equal(t, 1, authoritySessionOwners(t, f.b, "binding-uid-1"))
	require.EqualValues(t, 1, f.b.records[f.generation].ActiveSessions)
}

func TestConcurrentSubmitTransferPostsOneOperation(t *testing.T) {
	transfer := newTransferFixture(t)
	transfer.b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	const callers = 4
	results := make([]OperationRecord, callers)
	errs := make([]error, callers)
	var start sync.WaitGroup
	start.Add(callers)
	for i := 0; i < callers; i++ {
		go func(slot int) {
			defer start.Done()
			results[slot], errs[slot] = transfer.b.SubmitTransfer(context.Background(), transfer.request, transfer.pod)
		}(i)
	}
	start.Wait()
	for i := range callers {
		require.NoError(t, errs[i])
		require.Equal(t, results[0].Request.RequestID, results[i].Request.RequestID)
	}
	require.Len(t, transfer.private.posts, 1, "one request must post once")
	transfer.b.mu.Lock()
	count := len(transfer.b.operations)
	transfer.b.mu.Unlock()
	require.Equal(t, 1, count)
}

func TestConcurrentDistinctIntentsAdmitDistinctGenerations(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	const callers = 4
	admissions := make([]*Assignment, callers)
	errs := make([]*protocol.Error, callers)
	var start sync.WaitGroup
	start.Add(callers)
	for i := 0; i < callers; i++ {
		intent := Intent{
			Key: "parallel-intent-" + string(rune('a'+i)), Namespace: "tenant-a", Digest: digest,
			TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
			IntentUID: "parallel-uid-" + string(rune('a'+i)), IntentGeneration: 1, BindingUID: "binding-uid-1",
			ReservationID: "parallel-reservation-" + string(rune('a'+i)), Registrations: 1,
			ArtifactUID: "artifact-uid-" + string(rune('a'+i)), SourceGeneration: 1,
		}
		go func(slot int, intent Intent) {
			defer start.Done()
			admissions[slot], errs[slot] = b.AdmitIntentFill(t.Context(), intent)
		}(i, intent)
	}
	start.Wait()
	generations := map[string]bool{}
	for i := range callers {
		require.Nil(t, errs[i])
		require.NotNil(t, admissions[i])
		generations[admissions[i].Generation] = true
	}
	require.Len(t, generations, callers)
	require.Len(t, b.records, callers)
}

// transferFixture mirrors the operations_test fixture: one intent-bound
// generation with an inline (non-paged) manifest, one admitted stable-id
// session, and a valid transfer window against it.
type transferFixture struct {
	b              *Broker
	private        *operationTestSource
	request        TransferRequest
	pod            authorization.PodIdentity
	generation     string
	leaseID        string
	workerSecret   string
	instance       string
	sessionID      string
	ownerPodUID    string
	targetInstance string
	digest         string
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	f := &transferFixture{}
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	private := &operationTestSource{}
	b.SetPrivateSource(private)
	f.b, f.private, f.digest = b, private, digest
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
		Key: "transfer-lock-intent", Namespace: "tenant", Digest: digest, TotalBytes: 12,
		Grant: grant("tenant", 100), WorkerID: "source", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "prefetch", ArtifactName: "model", ArtifactUID: "artifact",
		SourceGeneration: 1, BindingUID: "binding-uid-1", BindingName: "binding", ReservationID: "transfer-lock-reservation",
		Registrations: 1, Backend: "nixl", ManifestJSON: string(raw), ManifestSHA256: hex.EncodeToString(sum[:]),
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	f.instance = strings.Repeat("a", 32)
	lease, perr := b.RegisterWorker(assignment.Generation, "source", assignment.WorkerSecret, f.instance)
	require.Nil(t, perr)
	f.generation, f.leaseID, f.workerSecret = assignment.Generation, lease.ID, assignment.WorkerSecret
	require.Nil(t, b.NativeStart(f.generation, "source", f.leaseID, f.workerSecret, f.instance))
	require.Nil(t, b.FillVerified(t.Context(), f.generation, "source", f.leaseID, f.workerSecret, 1,
		[]RegistrationDescriptor{{
			Name: "weight.safetensors", Addr: 1, Length: 12, MemoryType: "HOST",
			Backend: "nixl", WorkerUID: "source",
		}}, f.instance))
	f.targetInstance = strings.Repeat("b", 32)
	f.ownerPodUID = "owner"
	owner := SessionOwner{
		Namespace: "tenant", PodUID: f.ownerPodUID, ArtifactUID: "artifact", Digest: digest,
		RequestID: strings.Repeat("c", 32), TargetInstanceID: f.targetInstance,
	}
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", f.generation)
	require.Nil(t, perr)
	f.sessionID = session.ID
	f.pod = authorization.PodIdentity{Namespace: "tenant", Name: "engine", UID: f.ownerPodUID, IP: "127.0.0.1"}
	f.request = TransferRequest{
		RequestID: strings.Repeat("e", 32), SessionID: f.sessionID, PodNamespace: "tenant", PodName: "engine",
		PodUID: f.ownerPodUID, TargetInstanceID: f.targetInstance, SourcePodUID: "source",
		WorkerInstanceID: f.instance, Generation: f.generation,
		RegistrationID: strings.Repeat("f", 32), WindowID: strings.Repeat("1", 32),
		Tensor: "weight", TensorOffset: 2, Length: 4,
		Target: TransferTarget{Backend: "nixl", Address: 100, Length: 4, Metadata: "YQ=="},
	}
	return f
}

func (f *transferFixture) workerRequests(t *testing.T) {
	t.Helper()
	result, herr := f.b.Heartbeat(f.generation, "source", f.leaseID, f.workerSecret, Health{}, f.instance)
	require.Nil(t, herr)
	require.Equal(t, ActionNone, result.Action)
	entitlement, terr := f.b.TransferSession(f.sessionID, f.ownerPodUID, f.targetInstance)
	require.NoError(t, terr)
	require.Equal(t, f.digest, entitlement.Digest)
}
