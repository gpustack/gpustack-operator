package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// reserveOverhead is the fraction of the resolved byte total a reservation
// adds for fill and verification scratch, runtime context, and quarantine
// headroom. The resident weights are the artifact's resolved bytes; the
// overhead is the node's own cost of holding them.
const reserveOverhead = 10

// Limits are the node-level administrator limits within which the broker
// admits source generations. None of them is a namespace grant: the grant is
// the ModelStoreBinding's, frozen per reservation at admission.
type Limits struct {
	// PhysicalBytes is the node's registered-memory ceiling, charged per
	// distinct digest: two namespaces sharing one generation's content pay
	// the physical bytes once.
	PhysicalBytes int64
	// MaxGenerations bounds live generations on the node.
	MaxGenerations int
	// MaxQuarantined bounds quarantined generations, whose memory cannot be
	// reclaimed until a qualified fence applies.
	MaxQuarantined int
	// QuarantineBytes bounds the reserved bytes held by quarantined
	// generations.
	QuarantineBytes int64
	// LeaseTTL is how long a worker's health lease stands without a
	// heartbeat.
	LeaseTTL time.Duration
	// MinMemoryBytes is the floor under PhysicalBytes: the smallest source
	// buffer the node still serves. Zero means the protocol's own 1 GiB
	// floor.
	MinMemoryBytes int64
	// MaxSessions caps the node's concurrently admitted sessions across
	// every generation. Zero disables the cap, which is how a broker
	// opened without one behaves; production resolves a configured zero
	// to the default of 64 before the broker opens.
	MaxSessions int32
}

// Grant is one namespace's source-memory grant snapshot: the
// ModelStoreBinding's SourceMemory as the reservation was admitted. Bytes
// and Registrations never shrink under a live reservation; the session cap
// may move either way with the Binding, bounding new sessions while the
// already admitted ones drain naturally.
type Grant struct {
	Namespace     string `json:"namespace"`
	Bytes         int64  `json:"bytes"`
	Registrations int32  `json:"registrations"`
	Sessions      int32  `json:"sessions"`
}

// FillRequest is one namespace's ask to prepare a source generation from a
// verified published tree.
type FillRequest struct {
	// Digest is the artifact's manifest digest, "sha256:<hex>".
	Digest string
	// Namespace is the entitlement owner of the reservation.
	Namespace string
	// TotalBytes is the artifact's resolved byte total; it must be positive,
	// because an unknown size must not become zero-cost memory admission.
	TotalBytes int64
	// Grant is the namespace's administrator grant, required and positive.
	Grant Grant
	// WorkerID names the admitted source-worker Pod that will fill and serve.
	WorkerID string
	// BindingUID identifies the grant for intent admissions.
	BindingUID string
	// ManifestPath is the node-local verified manifest the worker fills
	// from; when present, reported registrations are validated against its
	// tensor ranges, not against the count alone.
	ManifestPath string
}

// Admission is a fill's authorization to proceed, bound to one new immutable
// generation.
type Admission struct {
	// Generation is the generation's opaque monotonic token.
	Generation string
	// WorkerSecret authenticates the admitted worker's control messages. It
	// is retained in the private ledger for crash-safe replay; authentication uses its hash.
	WorkerSecret string
}

// Lease is a worker's generation-scoped health lease.
type Lease struct {
	ID      string
	Expires time.Time
}

// Health is what a heartbeat reports about a generation's serving state.
type Health struct {
	// DrainRequested closes new session admission while existing owners drain.
	DrainRequested bool
	ResidentBytes  int64
	Registrations  int32
	ActiveSessions int32
}

// The actions a heartbeat answer can carry back to the worker.
const (
	ActionNone  = "none"
	ActionDrain = "drain"
)

// HeartbeatResult is a renewed lease plus the action the broker asks of the
// worker.
type HeartbeatResult struct {
	Action          string
	Lease           Lease
	ActiveSessions  int32
	UnknownSessions int32
}

// Session is one admitted reader.
type Session struct {
	ID         string
	Generation string
}

// Broker is the single authoritative node-local admission and lifecycle
// service for source generations. All mutating calls hold one mutex, so
// concurrent fills serialize and budgets are never oversold; every state
// change is written durably before it is reported.
type Broker struct {
	root               string
	scope              string
	store              *store.Store
	limits             Limits
	meminfoPath        string
	residentBudget     int64
	hixlResidentBudget int64
	now                func() time.Time
	lock               *os.File
	fresh              bool
	inventoryRequired  bool
	inventory          *authorityInventory
	inventoryVerified  bool
	assignmentsFrozen  bool
	intentValidator    func(context.Context, LiveIntent) error
	sessionPodChecker  func(ctx context.Context, namespace, podUID string) (bool, error)

	// maxSessions mirrors limits.MaxSessions outside the mutex. The
	// authority grant closures read the node limits while the broker's own
	// mutex is held mid-admission, so the session cap must be readable
	// without re-entering that mutex: a non-reentrant second Lock on the
	// same goroutine deadlocks the admission it serves.
	maxSessions atomic.Int32

	mu               sync.Mutex
	ledger           *ledger
	records          map[string]*GenerationRecord
	sessions         map[string]*SessionRecord
	bootstrap        []byte // the Pod-local worker bootstrap credential
	assignmentSizes  map[string]int
	assignmentCursor int
	privateSource    PrivateSource
	// pageReader replaces the manifest page reader in tests only; nil is
	// production's LocalPageReader.
	pageReader      func(directory string) protocol.PageReader
	operations      map[string]*OperationRecord
	operationBytes  int64
	operationsReady bool
	// sourceQualified controls supported-backend admission. Production opens
	// this gate; the cluster delivery policy requires explicit administrator
	// opt-in. This flag does not record hardware or safety acceptance.
	sourceQualified bool
	// One shared broker may serve MANY Bindings: each Binding resolves to
	// its own pinned authority and live grant reader, never a shared grant.
	reservationAuthorities map[types.UID]*ReservationAuthority
	reservationGrants      map[types.UID]func(ctx context.Context) (ReservationGrant, error)
	manifestCache          map[string]*cachedManifest
	// intentAdmissions holds one open channel per intent revision whose
	// prepare is running without the broker mutex; concurrent callers of
	// the same revision wait on it instead of preparing a duplicate.
	intentAdmissions map[string]chan struct{}
	// sessionAdmissionWaits counts admissions whose authority CAS is
	// running without the broker mutex; the session retry pass skips
	// those prepared records until the admission settles.
	sessionAdmissionWaits map[string]int
}

// SessionRecord is one admitted reader, bound to the authenticated owner
// that opened it: only that identity may read or release it, and a
// session id alone never promotes a caller.
type SessionRecord struct {
	Generation         string
	OwnerUID           string
	Namespace          string
	ArtifactUID        string
	Digest             string
	Phase              string `json:"phase,omitempty"`
	RequestID          string `json:"requestID,omitempty"`
	TargetInstanceID   string `json:"targetInstanceID,omitempty"`
	CloseRequestID     string `json:"closeRequestID,omitempty"`
	SourcePodUID       string `json:"sourcePodUID,omitempty"`
	SourceInstanceID   string `json:"sourceInstanceID,omitempty"`
	SourceLeaseID      string `json:"sourceLeaseID,omitempty"`
	OpenFingerprint    string `json:"openFingerprint,omitempty"`
	SourceClosed       bool   `json:"sourceClosed,omitempty"`
	SourceCloseStarted bool   `json:"sourceCloseStarted,omitempty"`
}

// Open prepares the broker's durable ledger under root and loads its
// records plus its persisted sessions. The scope is the broker's vendor
// identity: it prefixes every generation token and cache reference, and
// the ledger's own durable identity keeps tokens globally unique across
// nodes and ledger recreations. A restart does NOT prove engines or DMA
// stopped, so session accounting survives it: each persisted session
// keeps its owner and generation, and any legacy count a session record
// cannot explain is preserved rather than freed. No generation changes
// state because the broker restarted. Call Reconstruct before serving.
func Open(root, scope string, st *store.Store, limits Limits, now func() time.Time) (*Broker, error) {
	residentBudget, hixlResidentBudget, err := sourceResidentLimits()
	if err != nil {
		return nil, err
	}
	if scope == "" {
		return nil, fmt.Errorf("the broker's vendor scope is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("prepare the ledger root %s: %w", root, err)
	}
	// The ledger is single-writer across processes: two brokers sharing a
	// root would interleave record writes and counters. The lock is held
	// for the broker's lifetime and released by Close — on every error
	// path below, never only on success.
	lock, err := acquireLedgerLock(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Broker, error) {
		_ = lock.Close()
		return nil, err
	}
	l, fresh, err := openLedger(root, scope)
	if err != nil {
		return fail(err)
	}
	if err := l.ensureCounter(fresh); err != nil {
		return fail(err)
	}
	if now == nil {
		now = time.Now
	}
	b := &Broker{
		root: root, scope: scope, store: st, limits: limits, now: now, lock: lock,
		meminfoPath:    DefaultMemInfoPath,
		residentBudget: residentBudget, hixlResidentBudget: hixlResidentBudget,
		fresh:           fresh,
		sourceQualified: true,
		ledger:          l, records: map[string]*GenerationRecord{}, sessions: map[string]*SessionRecord{},
	}
	b.maxSessions.Store(limits.MaxSessions)
	records, err := l.records()
	if err != nil {
		return fail(err)
	}
	sessions, err := b.loadSessions()
	if err != nil {
		return fail(err)
	}
	for _, rec := range records {
		matched := 0
		for _, session := range sessions {
			if session.Generation == rec.Generation && session.Phase != "released" {
				matched++
			}
		}
		// A count the session file cannot explain becomes the generation's
		// unknown charge: the broker's restart proves nothing about those
		// engines, so it frees no capacity and invents no owner, and only
		// a qualified fence clears it.
		if legacy := rec.ActiveSessions - int32(matched); legacy > 0 {
			rec.UnknownSessions = legacy
		} else if int32(matched) > rec.ActiveSessions {
			// The session file is the finer-grained record: adopt it.
			rec.ActiveSessions = int32(matched)
		}
		b.records[rec.Generation] = rec
	}
	for id, session := range sessions {
		b.sessions[id] = session
	}
	// The sweep only removes references of records this broker already
	// reports as released; an unrelated unreadable reference must never
	// prevent the broker from starting.
	_ = b.sweepReleasedRefsLocked()
	return b, nil
}

// SetNow replaces the clock, for tests.
func (b *Broker) SetNow(now func() time.Time) { b.now = now }

// SetMemInfoPath names the meminfo-format file the dynamic source window
// reads its memory figures from. Production uses the default; tests inject
// a fixture.
func (b *Broker) SetMemInfoPath(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.meminfoPath = path
}

// setPageReader replaces the local manifest page reader, for tests. The
// nil default is production's LocalPageReader over the record's own
// authenticated pages directory; only a test file in this package writes
// the override, so page reads can be gated deterministically.
func (b *Broker) setPageReader(reader func(directory string) protocol.PageReader) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pageReader = reader
}

// pageReaderFor returns the page reader for one authenticated pages
// directory. Callers must not hold the broker mutex.
func (b *Broker) pageReaderFor(directory string) protocol.PageReader {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pageReader != nil {
		return b.pageReader(directory)
	}
	return protocol.LocalPageReader(directory)
}

// SetMaxSessions moves the node's concurrent session cap online. A raise
// applies to the next admission; a lower cap bounds only new admissions,
// and the sessions already admitted drain naturally.
func (b *Broker) SetMaxSessions(maxSessions int32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits.MaxSessions = maxSessions
	b.maxSessions.Store(maxSessions)
}

// LimitsSnapshot returns the node limits the broker currently admits under.
// Callers default missing grant fields to these, so a Binding without a
// sourceMemory section follows the node's own configured capacity.
//
// It never takes the broker mutex: the authority grant closures run while
// an admission holds that mutex, and the snapshot's only mutable figure —
// the session cap — travels in the atomic mirror instead. Everything else
// is fixed at Open and never rewritten.
func (b *Broker) LimitsSnapshot() Limits {
	return Limits{
		PhysicalBytes:   b.limits.PhysicalBytes,
		MaxGenerations:  b.limits.MaxGenerations,
		MaxQuarantined:  b.limits.MaxQuarantined,
		QuarantineBytes: b.limits.QuarantineBytes,
		LeaseTTL:        b.limits.LeaseTTL,
		MinMemoryBytes:  b.limits.MinMemoryBytes,
		MaxSessions:     b.maxSessions.Load(),
	}
}

// SourceWindowBytes resolves the transfer buffer ceiling the node offers a
// session right now: the dynamic buffer over its current available RAM
// against its configured ceiling and floor. The second answer is false
// when the node's memory cannot be read or cannot back even the floor. An
// unreadable or malformed meminfo is logged with its cause, so an operator
// can tell a broken memory source from genuine memory exhaustion; the
// refusal itself stays fail-closed either way.
func (b *Broker) SourceWindowBytes() (int64, bool) {
	b.mu.Lock()
	limits, path := b.limits, b.meminfoPath
	b.mu.Unlock()
	avail, err := memInfoAvailableBytes(path)
	if err != nil {
		klog.ErrorS(err, "The node's available-memory figure is unavailable; the dynamic source window is refused",
			"path", path)
		return 0, false
	}
	if avail <= 0 {
		return 0, false
	}
	return ComputeDynamicSourceBuffer(avail, limits.PhysicalBytes, limits.MinMemoryBytes)
}

// SetReservationAuthority registers one Binding's pinned authority and its
// live grant reader. Production wiring registers every Binding this node
// serves; an intent whose Binding has no registered authority is refused —
// production never admits a Binding-wide reservation on one node's word
// alone, and two Bindings in one namespace never share a grant.
func (b *Broker) SetReservationAuthority(bindingUID types.UID,
	authority *ReservationAuthority, grant func(ctx context.Context) (ReservationGrant, error),
) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reservationAuthorities == nil {
		b.reservationAuthorities = map[types.UID]*ReservationAuthority{}
		b.reservationGrants = map[types.UID]func(ctx context.Context) (ReservationGrant, error){}
	}
	b.reservationAuthorities[bindingUID] = authority
	b.reservationGrants[bindingUID] = grant
}

// CanBindWorkerInstance refuses a new supervisor boot while another boot may
// still own native state.
func (b *Broker) CanBindWorkerInstance(instance string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if instance == "" {
		return false
	}
	for _, rec := range b.records {
		if rec.State == StateReleased {
			continue
		}
		if !rec.NativeStartEnforced && rec.LeaseID != "" {
			return false
		}
		if rec.NativeStarted && !rec.NativeReleased && rec.NativeInstance != instance {
			return false
		}
	}
	return true
}

// SetAssignmentsFrozen blocks new worker assignment polls while an intent
// pass is unresolved or an image rollout is draining.
func (b *Broker) SetAssignmentsFrozen(frozen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assignmentsFrozen = frozen
}

// ReadyForDrain requires a verified inventory and durable release of every
// local generation and session; it never infers release from absence.
func (b *Broker) ReadyForDrain() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.assignmentsFrozen || !b.inventoryRequired || !b.inventoryVerified {
		return false
	}
	for _, rec := range b.records {
		if rec.State != StateReleased {
			return false
		}
	}
	for _, session := range b.sessions {
		if session.Phase != "released" && session.Phase != "refused" {
			return false
		}
	}
	if b.privateSource != nil {
		if !b.operationsReady {
			return false
		}
		for _, operation := range b.operations {
			if operation.State != "acknowledged" {
				return false
			}
		}
	}
	return true
}

// reserveBytes is the reservation charged for a resolved byte total.
func reserveBytes(total int64) int64 { return total + total/reserveOverhead }

// AdmitFill atomically reserves the physical node budget and the namespace's
// grant budget, claims a durable source reference on the verified published
// tree, and opens one new immutable generation in state Filling. Every
// refusal is typed: an unpublished or unverified source, a missing or empty
// grant, an unknown byte total, or an exhausted budget never masquerade as
// one another.
func (b *Broker) AdmitFill(ctx context.Context, req FillRequest) (*Admission, *protocol.Error) {
	return b.admit(ctx, req, "")
}

// AdmitIntentFill admits one resolved intent: the generation record and
// its assignment (intent key, recovery facts, grant snapshot, admission
// secret) are written as ONE durable node-private transaction, so a crash
// can leave the admission fully present or fully absent — never an
// identityless generation that a later pass would duplicate. The same
// intent revision always answers the same admission.
func (b *Broker) AdmitIntentFill(ctx context.Context, intent Intent) (*Assignment, *protocol.Error) {
	if err := b.syncIntentGrant(intent); err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the admitted generation's grant could not follow its Binding: %v", err)
	}
	if assignment, ok := b.AssignmentForIntent(intent.Key); ok {
		return &assignment, nil
	}
	if intent.BindingUID == "" || intent.ReservationID == "" || intent.Registrations <= 0 {
		return nil, protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the intent names no Binding UID, locally minted reservation ID, or registration plan")
	}
	b.mu.Lock()
	authority := b.reservationAuthorities[types.UID(intent.BindingUID)]
	grantReader := b.reservationGrants[types.UID(intent.BindingUID)]
	b.mu.Unlock()
	if authority == nil || grantReader == nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"no reservation authority is registered for this intent's Binding")
	}
	if err := b.recordAuthorityIdentity(intent, authority.pin); err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill", "the authority inventory is unavailable")
	}
	// One prepare per intent revision at a time: a concurrent caller of the
	// same revision waits for the in-flight prepare to settle and then
	// adopts its record, exactly as the serialized broker answered.
	releaseAdmission, err := b.claimIntentAdmission(ctx, intent.Key)
	if err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the intent's admission was interrupted: %v", err)
	}
	defer releaseAdmission()
	req := FillRequest{
		Digest:       intent.Digest,
		Namespace:    intent.Namespace,
		TotalBytes:   intent.TotalBytes,
		Grant:        intent.Grant,
		WorkerID:     intent.WorkerID,
		BindingUID:   intent.BindingUID,
		ManifestPath: intent.ManifestPath,
	}

	// Phase one, deduplication: adopt a live assignment, reuse a prepared
	// record, or refuse a revision whose prior generation is draining or
	// quarantined. The slow preparation that follows runs without the
	// broker mutex; publication rechecks the dedup facts under it.
	b.mu.Lock()
	prepared := ""
	var adopted *Assignment
	for _, rec := range b.records {
		if rec.IntentKey != intent.Key {
			continue
		}
		if rec.AdmissionPhase == "admitted" && rec.WorkerSecret != "" && liveAssignment(rec) {
			assignment := recordAssignment(rec)
			adopted = &assignment
			break
		}
		if rec.State == StateDraining || rec.State == StateQuarantined {
			b.mu.Unlock()
			return nil, protocol.New(protocol.KindStaleGeneration, "fill", "the prior generation is draining or quarantined")
		}
		if rec.AdmissionPhase == "prepared" || rec.AdmissionPhase == "reserved" {
			prepared = rec.Generation
		}
	}
	if adopted != nil {
		b.mu.Unlock()
		return adopted, nil
	}
	b.mu.Unlock()

	if prepared == "" {
		generation, perr := b.prepareIntentRecord(ctx, req, intent.Key, intent)
		if perr != nil {
			return nil, perr
		}
		prepared = generation
	}
	b.mu.Lock()
	rec := b.records[prepared]
	if rec == nil {
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the prepared generation vanished before its reservation")
	}
	reservationID := rec.ReservationID
	reservationBytes := rec.TotalBytes
	if rec.ResidentBudgetBytes > 0 {
		reservationBytes = rec.ReservedBytes
	}
	b.mu.Unlock()

	// Phase two — the GLOBAL reservation, with the locally persisted
	// reservation ID and the prepared generation's immutable identity. A
	// DEFINITIVE capacity refusal cancels the prepared record when it is
	// provably never exposed (no lease was ever registered); any
	// unavailable or contested outcome retains the prepared charge for
	// replay, because a lost response is not evidence of anything.
	_, rerr := authority.Reserve(ctx, ReservationRequest{
		ReservationID:    reservationID,
		NodeScope:        b.scope,
		LedgerIdentity:   b.ledger.identity,
		Generation:       prepared,
		IntentUID:        intent.IntentUID,
		IntentGeneration: intent.IntentGeneration,
		Digest:           intent.Digest,
		Namespace:        intent.Namespace,
		NodeName:         intent.NodeName,
		ArtifactUID:      intent.ArtifactUID,
		SourceGeneration: intent.SourceGeneration,
		SourcePodUID:     intent.WorkerID,
		Bytes:            reservationBytes,
		Registrations:    intent.Registrations,
	})
	if rerr != nil {
		definite := errors.Is(rerr, errReservationCapacity)
		if !definite {
			return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
				"the Binding-wide reservation is unavailable; the prepared charge is retained: %v", rerr)
		}
		b.mu.Lock()
		rec := b.records[prepared]
		if rec != nil && rec.State == StateFilling && rec.LeaseID == "" &&
			rec.AdmissionPhase == "prepared" {
			if perr := b.release(rec); perr != nil {
				b.mu.Unlock()
				return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
					"the refused reservation could not cancel the prepared fill: %v", perr)
			}
		}
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindCapacityExhausted, "fill",
			"the Binding-wide reservation was refused: %v", rerr)
	}

	// Phase three — the global entry is marked reserved-confirmed, and the
	// local record advances to reserved durably before anything is exposed.
	if merr := authority.MarkReserved(ctx, b.scope,
		b.ledger.identity, prepared); merr != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the reservation could not be confirmed: %v", merr)
	}
	b.mu.Lock()
	rec = b.records[prepared]
	if rec == nil {
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the prepared generation vanished before confirmation")
	}
	if rec.AdmissionPhase == "admitted" && liveAssignment(rec) {
		assignment := recordAssignment(rec)
		b.mu.Unlock()
		return &assignment, nil
	}
	if rec.IntentKey != intent.Key || rec.State != StateFilling {
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindStaleGeneration, "fill", "the prepared intent was retired")
	}
	reserved := *rec
	reserved.AdmissionPhase = "reserved"
	reserved.Updated = b.now()
	if err := b.ledger.write(&reserved); err != nil {
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the prepared admission could not be recorded: %v", err)
	}
	b.records[prepared] = &reserved
	b.mu.Unlock()

	// Confirm globally before publishing a local secret. A lost response
	// leaves a reserved local record; replay confirms the same global entry.
	if aerr := authority.MarkAdmitted(ctx, b.scope, b.ledger.identity, prepared); aerr != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the reservation could not be marked admitted: %v", aerr)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec = b.records[prepared]
	if rec == nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the reserved generation vanished before confirmation")
	}
	if rec.IntentKey != intent.Key || rec.State != StateFilling {
		return nil, protocol.New(protocol.KindStaleGeneration, "fill", "the reserved intent was retired")
	}
	if rec.WorkerSecret != "" {
		assignment := recordAssignment(rec)
		return &assignment, nil
	}
	secret, secretHash, serr := newWorkerSecret()
	if serr != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the worker secret could not be minted: %v", serr)
	}
	admitted := *rec
	admitted.WorkerSecret = secret
	admitted.WorkerSecretHash = secretHash
	admitted.AdmissionPhase = "admitted"
	admitted.Updated = b.now()
	if err := b.ledger.write(&admitted); err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the prepared admission could not be confirmed: %v", err)
	}
	b.records[prepared] = &admitted
	assignment := recordAssignment(b.records[prepared])
	return &assignment, nil
}

// syncIntentGrant follows an admitted generation's durable grant snapshot
// to the intent's current grant. The reconciler re-lists the same intent
// revision every pass, and an online Binding edit changes the grant without
// changing the revision, so the snapshot must move with it or the local
// fallback paths would govern new sessions and registrations by a
// superseded answer. Bytes and Registrations move forward only, and
// Sessions may move either way above zero — a lowered cap bounds only new
// sessions while the already admitted ones drain naturally; the global
// authority reads the Binding live at every CAS regardless. Any other
// transition is left unsynced, and a failed write is surfaced so the next
// pass retries it.
func (b *Broker) syncIntentGrant(intent Intent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.IntentKey != intent.Key || !liveAssignment(rec) {
			continue
		}
		next := intent.Grant
		if next.Namespace == "" {
			next.Namespace = intent.Namespace
		}
		if next == rec.Grant {
			return nil
		}
		if next.Bytes < rec.Grant.Bytes || next.Registrations < rec.Grant.Registrations ||
			next.Bytes <= 0 || next.Registrations < 1 || next.Sessions < 1 {
			return nil
		}
		adopted := *rec
		adopted.Grant = next
		adopted.Updated = b.now()
		if err := b.ledger.write(&adopted); err != nil {
			return err
		}
		b.records[rec.Generation] = &adopted
		return nil
	}
	return nil
}

// claimIntentAdmission serializes one intent revision's admission. A caller
// that finds another prepare in flight waits for it to settle and retries;
// the returned release function must run when the admission settles.
func (b *Broker) claimIntentAdmission(ctx context.Context, key string) (func(), error) {
	for {
		b.mu.Lock()
		if b.intentAdmissions == nil {
			b.intentAdmissions = map[string]chan struct{}{}
		}
		if done, busy := b.intentAdmissions[key]; busy {
			b.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		done := make(chan struct{})
		b.intentAdmissions[key] = done
		b.mu.Unlock()
		var releaseOnce sync.Once
		return func() {
			releaseOnce.Do(func() {
				b.mu.Lock()
				delete(b.intentAdmissions, key)
				b.mu.Unlock()
				close(done)
			})
		}, nil
	}
}

// prepareIntentRecord mints and durably writes one intent-bound generation
// record without its admission secret: the record is the crash-safe
// prepared identity, visible to deduplication and adoption, exposed to
// nothing. The store reads, the manifest parse, and the manifest
// publication block on disk, so they run without the broker mutex; the
// budget check and the record's durable publication share one critical
// section, so competing admissions can never oversell a budget.
func (b *Broker) prepareIntentRecord(ctx context.Context, req FillRequest, intentKey string,
	intent Intent,
) (string, *protocol.Error) {
	if req.Namespace == "" || req.WorkerID == "" {
		return "", protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the fill request names no namespace or worker")
	}
	if req.Grant.Namespace == "" {
		req.Grant.Namespace = req.Namespace
	}
	if req.Grant.Bytes <= 0 || req.Grant.Registrations <= 0 || req.Grant.Sessions <= 0 {
		return "", protocol.New(protocol.KindCapacityExhausted, "fill",
			"the namespace's source-memory grant is empty")
	}
	if req.TotalBytes <= 0 {
		return "", protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the artifact's resolved byte total is unknown")
	}
	hexDigest := store.HexOf(req.Digest)
	if hexDigest == "" {
		return "", protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the digest is not a sha256 content address")
	}
	marker, err := b.store.ReadMarker(hexDigest)
	if err != nil {
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"no verified published tree for the digest on this node")
	}
	if marker.Digest != req.Digest || marker.SizeBytes <= 0 {
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"the published tree's verified marker does not match the request")
	}
	resident, perr := b.sourceResidentBytes(req, nil, intent.Backend)
	if perr != nil && intent.ManifestJSON != "" {
		manifest, err := b.parseDerivedManifest(ctx, &GenerationRecord{
			ManifestJSON: intent.ManifestJSON, ManifestSHA256: intent.ManifestSHA256, ManifestPagesPath: intent.ManifestPagesPath,
			ArtifactUID: intent.ArtifactUID, SourceGeneration: intent.SourceGeneration,
		})
		if err != nil {
			return "", protocol.New(protocol.KindInvalidDescriptor, "fill", "the source manifest is invalid")
		}
		resident, perr = b.sourceResidentBytes(req, manifest, intent.Backend)
	}
	if perr != nil {
		return "", perr
	}
	reserve := reserveBytes(resident)
	b.mu.Lock()
	// An early budget read refuses a definitively exhausted budget before
	// the slow claim and publication; the authoritative check re-runs with
	// the publication below.
	if reason, denied := b.admissionDenied(req, reserve); denied {
		b.mu.Unlock()
		return "", reason
	}
	var pin AuthorityPin
	if authority := b.reservationAuthorities[types.UID(intent.BindingUID)]; authority != nil {
		pin = authority.pin
	}
	generation, err := b.ledger.nextGeneration()
	b.mu.Unlock()
	if err != nil {
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"the durable generation counter failed: %v", err)
	}
	claimed, err := b.store.Claim(ctx, store.Ref{
		VolumeID: refVolumeID(b.scope, generation), Hex: hexDigest,
		PodUID: req.WorkerID,
	})
	if err != nil {
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"the source reference could not be recorded: %v", err)
	}
	if !claimed {
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"the verified published tree vanished while admitting")
	}
	now := b.now()
	rec := &GenerationRecord{
		Generation: generation, Digest: req.Digest, Namespace: req.Namespace,
		WorkerID: req.WorkerID, NodeName: intent.NodeName, State: StateFilling,
		ReservedBytes: reserve, Grant: req.Grant, ManifestPath: req.ManifestPath,
		ResidentBudgetBytes: resident,
		IntentKey:           intentKey, AdmissionPhase: "prepared",
		IntentUID: intent.IntentUID, IntentName: intent.IntentName,
		IntentGeneration: intent.IntentGeneration, ArtifactName: intent.ArtifactName,
		ReservationID:       intent.ReservationID,
		BindingUID:          intent.BindingUID,
		BindingNamespace:    intent.BindingNamespace,
		BindingName:         intent.BindingName,
		AuthorityUID:        pin.UID,
		AuthorityEpoch:      pin.Epoch,
		NativeStartEnforced: true,
		Registrations:       intent.Registrations,
		ArtifactUID:         intent.ArtifactUID,
		SourceGeneration:    intent.SourceGeneration,
		ManifestSHA256:      intent.ManifestSHA256,
		ManifestJSON:        intent.ManifestJSON,
		ManifestPagesPath:   intent.ManifestPagesPath,
		TreePath:            intent.TreePath,
		TotalBytes:          intent.TotalBytes,
		Backend:             intent.Backend,
		Created:             now, Updated: now,
	}
	if err := b.storeManifest(ctx, rec); err != nil {
		_ = b.store.RemoveRef(refVolumeID(b.scope, generation))
		return "", protocol.New(protocol.KindSourceUnavailable, "fill", "immutable manifest publication failed: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if reason, denied := b.admissionDenied(req, reserve); denied {
		_ = b.store.RemoveRef(refVolumeID(b.scope, generation))
		return "", reason
	}
	if err := b.ledger.write(rec); err != nil {
		_ = b.store.RemoveRef(refVolumeID(b.scope, generation))
		return "", protocol.New(protocol.KindSourceUnavailable, "fill",
			"the reservation could not be recorded: %v", err)
	}
	b.records[generation] = rec
	return generation, nil
}

func (b *Broker) admit(ctx context.Context, req FillRequest, intentKey string) (*Admission, *protocol.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// The intent deduplication happens under the same lock the admission
	// takes: two callers of one revision meet here and both get the first
	// caller's admission, never two generations.
	if intentKey != "" {
		for _, rec := range b.records {
			if rec.IntentKey == intentKey && liveAssignment(rec) {
				return &Admission{
					Generation: rec.Generation, WorkerSecret: rec.WorkerSecret,
				}, nil
			}
		}
	}
	if req.Namespace == "" || req.WorkerID == "" {
		return nil, protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the fill request names no namespace or worker")
	}
	if req.Grant.Namespace == "" {
		req.Grant.Namespace = req.Namespace
	}
	if req.Grant.Bytes <= 0 && req.Grant.Registrations <= 0 && req.Grant.Sessions <= 0 {
		return nil, protocol.New(protocol.KindDeniedContent, "fill",
			"the namespace holds no source-memory grant; the disk quota never grants memory")
	}
	if req.Grant.Bytes <= 0 || req.Grant.Registrations <= 0 || req.Grant.Sessions <= 0 {
		return nil, protocol.New(protocol.KindCapacityExhausted, "fill",
			"the namespace's source-memory grant is empty")
	}
	if req.TotalBytes <= 0 {
		return nil, protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the artifact's resolved byte total is unknown")
	}
	hexDigest := store.HexOf(req.Digest)
	if hexDigest == "" {
		return nil, protocol.New(protocol.KindInvalidDescriptor, "fill",
			"the digest is not a sha256 content address")
	}

	marker, err := b.store.ReadMarker(hexDigest)
	if err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"no verified published tree for the digest on this node")
	}
	if marker.Digest != req.Digest || marker.SizeBytes <= 0 {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the published tree's verified marker does not match the request")
	}

	resident, perr := b.sourceResidentBytes(req, nil, "")
	if perr != nil && req.ManifestPath != "" {
		manifest, err := protocol.LoadManifest(ctx, req.ManifestPath, req.Digest)
		if err != nil {
			return nil, protocol.New(protocol.KindInvalidDescriptor, "fill", "the source manifest is invalid")
		}
		resident, perr = b.sourceResidentBytes(req, manifest, "")
	}
	if perr != nil {
		return nil, perr
	}
	reserve := reserveBytes(resident)
	if reason, denied := b.admissionDenied(req, reserve); denied {
		return nil, reason
	}

	// The generation token is the ledger's own scoped token: it already
	// carries the vendor scope and the ledger identity, so two vendor
	// brokers sharing one cache store on a mixed node can never collide
	// in the store's reference identity.
	generation, err := b.ledger.nextGeneration()
	if err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the durable generation counter failed: %v", err)
	}
	// The durable source reference is claimed before the reservation is
	// recorded, so a fill that started can never lose its tree to the
	// collector, and independently of any CSI mount reference.
	claimed, err := b.store.Claim(ctx, store.Ref{
		VolumeID: refVolumeID(b.scope, generation), Hex: hexDigest,
		PodUID: req.WorkerID,
	})
	if err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the source reference could not be recorded: %v", err)
	}
	if !claimed {
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the verified published tree vanished while admitting")
	}

	secret, secretHash, err := newWorkerSecret()
	if err != nil {
		_ = b.store.RemoveRef(refVolumeID(b.scope, generation))
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the worker secret could not be minted: %v", err)
	}
	now := b.now()
	rec := &GenerationRecord{
		Generation: generation, Digest: req.Digest, Namespace: req.Namespace,
		WorkerID: req.WorkerID, WorkerSecretHash: secretHash, State: StateFilling,
		ReservedBytes: reserve, Grant: req.Grant, ManifestPath: req.ManifestPath,
		ResidentBudgetBytes: resident,
		IntentKey:           intentKey, WorkerSecret: secret, TotalBytes: req.TotalBytes,
		Created: now, Updated: now,
	}
	if err := b.ledger.write(rec); err != nil {
		_ = b.store.RemoveRef(refVolumeID(b.scope, generation))
		return nil, protocol.New(protocol.KindSourceUnavailable, "fill",
			"the reservation could not be recorded: %v", err)
	}
	b.records[generation] = rec
	return &Admission{Generation: generation, WorkerSecret: secret}, nil
}

// admissionDenied reports whether the budgets refuse this fill, and why.
// Every live generation's actual reservation charges the node: two
// namespaces holding the same digest pay twice until shared backing is
// real, because a digest is not a promise that one buffer backs both.
// All comparisons subtract from the ceiling instead of adding up to it,
// so a near-ceiling reservation can never overflow into a false pass.
func (b *Broker) admissionDenied(req FillRequest, reserve int64) (*protocol.Error, bool) {
	live := 0
	quarantined := 0
	var quarantineReserved int64
	var namespaceReserved int64
	namespaceRegistrations := 0
	var physical int64
	for _, rec := range b.records {
		if rec.State == StateReleased {
			continue
		}
		live++
		switch rec.State {
		case StateQuarantined:
			quarantined++
			quarantineReserved += rec.ReservedBytes
		case StateFilling, StateServing, StateDraining:
		default:
			continue
		}
		physical += rec.ReservedBytes
		if rec.Namespace == req.Namespace && rec.BindingUID == req.BindingUID {
			namespaceReserved += rec.ReservedBytes
			namespaceRegistrations += int(max(rec.Registrations, 1))
		}
	}
	if quarantined >= b.limits.MaxQuarantined || quarantineReserved >= b.limits.QuarantineBytes {
		return protocol.New(protocol.KindCapacityExhausted, "fill",
			"the quarantine budget is exhausted; new fills wait for a fence"), true
	}
	if live >= b.limits.MaxGenerations {
		return protocol.New(protocol.KindCapacityExhausted, "fill",
			"the node holds its maximum generations"), true
	}
	if namespaceRegistrations >= int(req.Grant.Registrations) {
		return protocol.New(protocol.KindCapacityExhausted, "fill",
			"the namespace's registration count is exhausted"), true
	}
	if namespaceReserved > req.Grant.Bytes || reserve > req.Grant.Bytes-namespaceReserved {
		return protocol.New(protocol.KindCapacityExhausted, "fill",
			"the namespace's source-memory grant is exhausted"), true
	}
	if physical > b.limits.PhysicalBytes || reserve > b.limits.PhysicalBytes-physical {
		return protocol.New(protocol.KindCapacityExhausted, "fill",
			"the node's physical source-memory ceiling is reached"), true
	}
	return nil, false
}

// RegisterWorker binds a worker to its generation and issues its health
// lease. The worker identity and secret must be the admission's, and a
// quarantined or released generation accepts no worker.
func (b *Broker) RegisterWorker(generation, workerID, secret string, instances ...string) (*Lease, *protocol.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return nil, err
	}
	if !workerMatches(rec, workerID, secret) {
		return nil, protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	if rec.State == StateQuarantined {
		return nil, protocol.New(protocol.KindStaleGeneration, "worker",
			"the generation is quarantined and accepts no worker")
	}
	if rec.State == StateReleased {
		return nil, protocol.New(protocol.KindStaleGeneration, "worker", "the generation is released")
	}
	next := *rec
	if len(instances) > 0 {
		instance := instances[0]
		if instance == "" {
			return nil, protocol.New(protocol.KindAuthentication, "worker", "worker instance is required")
		}
		if rec.NativeStarted && rec.NativeInstance != instance {
			rec.State = StateQuarantined
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				rec.quarantineUnpersisted = true
				return nil, protocol.New(protocol.KindSourceUnavailable, "worker", "cannot persist quarantine: %v", err)
			}
			rec.quarantineUnpersisted = false
			return nil, protocol.New(protocol.KindStaleGeneration, "worker", "another native instance owns this generation")
		}
		next.WorkerInstanceID = instance
	}
	lease := b.issueLease(&next)
	if err := b.ledger.write(&next); err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "worker", "cannot persist worker lease: %v", err)
	}
	*rec = next
	return lease, nil
}

// NativeStart durably records the native owner before permitting vendor calls.
func (b *Broker) ValidateNativeStart(generation, workerID, leaseID, secret, instance string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return err
	}
	if !workerMatches(rec, workerID, secret) || instance == "" || rec.WorkerInstanceID != instance ||
		rec.LeaseID == "" || rec.LeaseID != leaseID || !b.now().Before(rec.LeaseExpires) {
		return protocol.New(protocol.KindAuthentication, "worker", "the worker instance or lease does not match")
	}
	if rec.State != StateFilling || (rec.NativeStarted && rec.NativeInstance != instance) {
		return protocol.New(protocol.KindOperationState, "worker", "generation cannot start native work")
	}
	return nil
}

// NativeStart durably records the native owner before permitting vendor calls.
func (b *Broker) NativeStart(generation, workerID, leaseID, secret, instance string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return err
	}
	if !workerMatches(rec, workerID, secret) || instance == "" || rec.WorkerInstanceID != instance ||
		rec.LeaseID == "" || rec.LeaseID != leaseID || !b.now().Before(rec.LeaseExpires) {
		return protocol.New(protocol.KindAuthentication, "worker", "the worker instance or lease does not match")
	}
	if rec.State != StateFilling {
		return protocol.New(protocol.KindOperationState, "worker", "generation is not filling")
	}
	if rec.NativeStarted {
		if rec.NativeInstance == instance {
			return nil
		}
		return protocol.New(protocol.KindStaleGeneration, "worker", "another instance owns native state")
	}
	next := *rec
	next.NativeStarted = true
	next.NativeInstance = instance
	next.Updated = b.now()
	if err := b.ledger.write(&next); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "worker", "cannot persist native start: %v", err)
	}
	*rec = next
	return nil
}

// Heartbeat renews a worker's lease and reports its serving figures. The
// answer carries the action the broker asks of the worker: drain once the
// generation is draining. A foreign worker, a wrong secret, or an unknown
// lease is refused; a generation that no longer exists is stale.
func (b *Broker) Heartbeat(generation, workerID, leaseID, secret string,
	health Health, instances ...string,
) (*HeartbeatResult, *protocol.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return nil, err
	}
	current := rec
	updated := *rec
	rec = &updated
	if !workerMatches(rec, workerID, secret) {
		return nil, protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	if len(instances) > 0 && (instances[0] == "" || rec.WorkerInstanceID != instances[0]) {
		return nil, protocol.New(protocol.KindAuthentication, "worker", "the worker instance does not match")
	}
	if rec.LeaseID == "" || rec.LeaseID != leaseID {
		return nil, protocol.New(protocol.KindAuthentication, "worker",
			"the lease is unknown; register again")
	}
	if rec.State == StateQuarantined {
		return nil, protocol.New(protocol.KindStaleGeneration, "worker", "the generation is quarantined")
	}
	if rec.State == StateReleased {
		return &HeartbeatResult{Action: ActionDrain, Lease: Lease{ID: rec.LeaseID, Expires: rec.LeaseExpires}}, nil
	}
	if !b.now().Before(rec.LeaseExpires) {
		if len(instances) == 0 {
			return nil, protocol.New(protocol.KindAuthentication, "worker", "the lease expired; register again")
		}
		// The same native instance may reconnect to finish a drain, but an
		// expired lease never restores Serving or new session admission.
		if rec.State == StateServing || rec.State == StateFilling {
			rec.State = StateDraining
		}
	}
	// A heartbeat renews the standing lease; only a registration mints a
	// new lease ID, so a worker tracks one ID per registration.
	if rec.NativeStartEnforced && (health.ResidentBytes < 0 || health.ResidentBytes > residentCeiling(rec) ||
		health.Registrations < 0 || health.Registrations > rec.Registrations) {
		return nil, protocol.New(protocol.KindInvalidDescriptor, "worker", "the health report exceeds the admitted file plan")
	}
	if health.DrainRequested && (rec.State == StateServing || rec.State == StateFilling) {
		rec.State = StateDraining
		rec.Updated = b.now()
	}
	rec.LeaseExpires = b.now().Add(b.limits.LeaseTTL)
	if health.ResidentBytes >= 0 {
		rec.ResidentBytes = health.ResidentBytes
	}
	if health.Registrations > 0 {
		if err := b.checkNamespaceRegistrations(rec, health.Registrations); err != nil {
			return nil, err
		}
		if !rec.NativeStartEnforced {
			rec.Registrations = health.Registrations
		}
	}
	// The session charge is the broker's own truth: named sessions plus
	// the persisted unknown charge. A worker's health word never lowers
	// it — a healthy-looking heartbeat proves no engine released
	// anything, and only a qualified fence clears unknowns.
	b.recountSessions(rec)
	lease := &Lease{ID: rec.LeaseID, Expires: rec.LeaseExpires}
	action := ActionNone
	if rec.State == StateDraining {
		action = ActionDrain
	}
	if err := b.ledger.write(rec); err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "worker",
			"the lease could not be recorded: %v", err)
	}
	*current = updated
	return &HeartbeatResult{Action: action, Lease: *lease, ActiveSessions: b.admittedSessions(generation), UnknownSessions: rec.UnknownSessions}, nil
}

// FillVerified moves a filling generation to Serving once the worker has
// verified the source bytes and actually registered them with the
// backend; until then the generation admits no reader. Every reported
// registration carries its native descriptor — bound to the reporting
// worker, a real backend, and the generation's own manifest ranges — and
// a generation registers nothing until at least one real registration
// exists: a count without descriptors claims memory nothing can address,
// so it is refused rather than served to sessions. The resident charge
// becomes the registered bytes actually held, never the reservation's
// overhead estimate.
//
// The manifest reads that bind each descriptor run without the broker
// mutex, bracketed by an authenticated snapshot and a revalidation that
// refuses to publish when the generation's admission identity moved
// while the plan was checked.
func (b *Broker) FillVerified(ctx context.Context, generation, workerID, leaseID, secret string,
	registrations int32, descriptors []RegistrationDescriptor, instances ...string,
) *protocol.Error {
	b.mu.Lock()
	current, err := b.mutable(generation)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	updated := *current
	rec := &updated
	if !workerMatches(rec, workerID, secret) || rec.LeaseID != leaseID {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	if len(instances) > 0 && (instances[0] == "" || rec.WorkerInstanceID != instances[0] || !rec.NativeStarted || rec.NativeInstance != instances[0]) {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker", "native start has not been recorded for this instance")
	}
	if len(instances) > 0 && !b.now().Before(rec.LeaseExpires) {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker", "the worker lease has expired")
	}
	switch rec.State {
	case StateServing:
		b.mu.Unlock()
		return nil
	case StateFilling:
	default:
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "worker",
			"only a filling generation starts serving")
	}
	if registrations <= 0 {
		b.mu.Unlock()
		return protocol.New(protocol.KindInvalidDescriptor, "worker",
			"becoming Serving requires at least one real backend registration")
	}
	if err := b.checkNamespaceRegistrations(rec, registrations); err != nil {
		b.mu.Unlock()
		return err
	}
	if int32(len(descriptors)) != registrations {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker",
			"each reported registration carries exactly one native descriptor")
	}
	plan := descriptorPlanOf(rec)
	b.mu.Unlock()

	if perr := b.validateDescriptorPlan(ctx, plan, workerID, descriptors); perr != nil {
		return perr
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	current, err = b.mutable(generation)
	if err != nil {
		return err
	}
	if !workerMatches(current, workerID, secret) || current.LeaseID != leaseID {
		return protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	// The manifest reads above can take arbitrarily long; the publication
	// re-runs the entry checks exactly, including native-start proof and
	// lease freshness, so a lease that expired while a page read was gated
	// cannot publish Serving.
	if len(instances) > 0 && (instances[0] == "" || current.WorkerInstanceID != instances[0] ||
		!current.NativeStarted || current.NativeInstance != instances[0]) {
		return protocol.New(protocol.KindAuthentication, "worker", "native start has not been recorded for this instance")
	}
	if len(instances) > 0 && !b.now().Before(current.LeaseExpires) {
		return protocol.New(protocol.KindAuthentication, "worker", "the worker lease has expired")
	}
	switch current.State {
	case StateServing:
		return nil
	case StateFilling:
	default:
		return protocol.New(protocol.KindOperationState, "worker",
			"only a filling generation starts serving")
	}
	if !descriptorPlanMatches(current, plan) {
		return protocol.New(protocol.KindInvalidDescriptor, "worker",
			"the generation's admission changed while its registration plan was verified")
	}
	if registrations <= 0 || int32(len(descriptors)) != registrations {
		return protocol.New(protocol.KindAuthentication, "worker",
			"each reported registration carries exactly one native descriptor")
	}
	if err := b.checkNamespaceRegistrations(current, registrations); err != nil {
		return err
	}
	next := *current
	var resident int64
	for _, descriptor := range descriptors {
		resident += sourceAllocationBytes(descriptor.Length, next.Backend)
	}
	next.State = StateServing
	next.ResidentBytes = resident
	if !next.NativeStartEnforced {
		next.Registrations = registrations
	}
	next.Descriptors = descriptors
	next.Updated = b.now()
	if err := b.ledger.write(&next); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "worker",
			"the state could not be recorded: %v", err)
	}
	*current = next
	return nil
}

// SessionOwner is the authenticated identity a session is bound to. The
// authorizer proves it; the broker only ever compares it.
type SessionOwner struct {
	PodUID           string
	Namespace        string
	ArtifactUID      string
	Digest           string
	RequestID        string
	TargetInstanceID string
	CloseRequestID   string
	TargetReleased   bool
	OpenFingerprint  string
}

// AdmitSession admits one reader to a Serving generation under its
// authenticated owner's identity. A draining, quarantined, or unknown
// generation is stale; the namespace must own the generation; the grant's
// session cap bounds the namespace's admitted readers, and a refusal is
// counted on the generation. The owner travels with the session: only
// that identity may later read or release it.
func (b *Broker) AdmitSession(ctx context.Context, owner SessionOwner, namespace, generation string) (*Session, *protocol.Error) {
	b.mu.Lock()
	rec, err := b.mutable(generation)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if rec.Namespace != namespace {
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindDeniedContent, "session",
			"the generation belongs to another namespace")
	}
	if !b.serving(rec) {
		b.countRefusal(rec)
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindStaleGeneration, "session",
			"the generation is not serving new readers")
	}
	// The node-level cap outranks every per-namespace grant: one node's
	// host RAM backs every generation it holds, so its total live session
	// charge, named and unknown alike, bounds the next admission.
	if b.limits.MaxSessions > 0 && b.nodeActiveSessionsLocked() >= b.limits.MaxSessions {
		b.countRefusal(rec)
		b.mu.Unlock()
		return nil, protocol.New(protocol.KindCapacityExhausted, "session",
			"the node session limit is reached")
	}
	if rec.BindingUID == "" {
		session, perr := b.admitLocalSessionLocked(rec, owner)
		b.mu.Unlock()
		return session, perr
	}
	validator := b.intentValidator
	authority := b.reservationAuthorities[types.UID(rec.BindingUID)]
	intent := liveIntentOf(rec)
	inventoryRequired := b.inventoryRequired
	frozen := b.assignmentsFrozen
	b.mu.Unlock()
	return b.admitGlobalSession(ctx, authority, validator, inventoryRequired, frozen,
		intent, namespace, generation, owner)
}

// admitLocalSessionLocked admits a session on a generation without a
// Binding-wide authority. The broker mutex is held by the caller; the
// path touches only local memory and local durable state.
func (b *Broker) admitLocalSessionLocked(rec *GenerationRecord, owner SessionOwner) (*Session, *protocol.Error) {
	var namespaceSessions int32
	for _, other := range b.records {
		if other.Namespace == rec.Namespace {
			namespaceSessions += other.ActiveSessions
		}
	}
	if namespaceSessions >= rec.Grant.Sessions {
		b.countRefusal(rec)
		return nil, protocol.New(protocol.KindCapacityExhausted, "session",
			"the namespace's admitted session limit is reached")
	}
	// A qualification refusal is a property of the source, not of this
	// generation, so it counts no admission refusal against the generation.
	if err := b.sourceGate("session", rec.Backend); err != nil {
		return nil, err
	}
	session := newSessionID()
	rec.Updated = b.now()
	b.sessions[session] = &SessionRecord{
		Generation: rec.Generation, OwnerUID: owner.PodUID,
		Namespace: owner.Namespace, ArtifactUID: owner.ArtifactUID,
		Digest: owner.Digest,
	}
	b.recountSessions(rec)
	if err := b.ledger.write(rec); err != nil {
		delete(b.sessions, session)
		b.recountSessions(rec)
		return nil, protocol.New(protocol.KindSourceUnavailable, "session",
			"the session could not be recorded: %v", err)
	}
	// The session map is durable: a restart must re-offer the same owner
	// and generation, never free the capacity.
	if err := b.persistSessionsLocked(); err != nil {
		delete(b.sessions, session)
		b.recountSessions(rec)
		if restoreErr := b.ledger.write(rec); restoreErr != nil {
			klog.ErrorS(restoreErr, "Restore generation session accounting", "generation", rec.Generation)
		}
		return nil, protocol.New(protocol.KindSourceUnavailable, "session",
			"the session could not be recorded: %v", err)
	}
	return &Session{ID: session, Generation: rec.Generation}, nil
}

// ReleaseSession releases one admitted reader under its authenticated
// owner's identity: a session id alone never releases anything, and a
// mismatched owner is an authentication refusal, not a release. Releasing
// a session twice is an error, not a credit: a released slot never eases
// a later admission.
func (b *Broker) ReleaseSession(ctx context.Context, owner SessionOwner, sessionID string) *protocol.Error {
	b.mu.Lock()
	record, ok := b.sessions[sessionID]
	if !ok {
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "session",
			"the session is not admitted")
	}
	if record.OwnerUID != owner.PodUID || record.Namespace != owner.Namespace ||
		record.ArtifactUID != owner.ArtifactUID || record.Digest != owner.Digest {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "session",
			"the caller is not the session's admitted owner")
	}
	if record.TargetInstanceID != "" && record.TargetInstanceID != owner.TargetInstanceID {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "session", "the caller is not the session's target instance")
	}
	rec := b.records[record.Generation]
	if record.RequestID != "" && rec == nil {
		if record.Phase == "released" && record.CloseRequestID == owner.CloseRequestID && owner.TargetReleased {
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "session", "the session generation is unavailable")
	}
	if rec != nil && rec.BindingUID != "" {
		authority := b.reservationAuthorities[types.UID(rec.BindingUID)]
		release, perr := b.prepareGlobalSessionReleaseLocked(rec, sessionID, record, owner, authority)
		if perr != nil {
			b.mu.Unlock()
			return perr
		}
		if release == nil {
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
		if err := b.finishGlobalSessionRelease(ctx, release); err != nil {
			return protocol.New(protocol.KindSourceUnavailable, "session", "the session release remains pending")
		}
		return nil
	}
	perr := b.releaseLocalSessionLocked(record, sessionID)
	b.mu.Unlock()
	return perr
}

// releaseLocalSessionLocked removes one non-Binding session and its
// generation charge. The broker mutex is held by the caller.
func (b *Broker) releaseLocalSessionLocked(record *SessionRecord, sessionID string) *protocol.Error {
	rec := b.records[record.Generation]
	delete(b.sessions, sessionID)
	if err := b.persistSessionsLocked(); err != nil {
		b.sessions[sessionID] = record
		return protocol.New(protocol.KindSourceUnavailable, "session",
			"the session release could not be recorded: %v", err)
	}
	if rec != nil {
		rec.Updated = b.now()
		b.recountSessions(rec)
		if err := b.ledger.write(rec); err != nil {
			// The generation record lost the release: put the session
			// back, both in memory and on disk, so the charge stays
			// truthful and a retry is safe.
			b.sessions[sessionID] = record
			b.recountSessions(rec)
			if persistErr := b.persistSessionsLocked(); persistErr != nil {
				return protocol.New(protocol.KindSourceUnavailable, "session",
					"the session release could not be recorded: %v", err)
			}
			if restoreErr := b.ledger.write(rec); restoreErr != nil {
				klog.ErrorS(restoreErr, "Restore generation session accounting", "generation", rec.Generation)
			}
			return protocol.New(protocol.KindSourceUnavailable, "session",
				"the session release could not be recorded: %v", err)
		}
	}
	return nil
}

// Drain stops a generation's admissions: it stops serving new readers while
// its in-flight sessions and every other generation carry on untouched.
func (b *Broker) Drain(generation string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return err
	}
	switch rec.State {
	case StateDraining:
		return nil
	case StateServing:
	default:
		return protocol.New(protocol.KindOperationState, "drain",
			"only a serving generation drains")
	}
	rec.State = StateDraining
	rec.Updated = b.now()
	if err := b.ledger.write(rec); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "drain",
			"the state could not be recorded: %v", err)
	}
	return nil
}

// DrainAck completes a drain: the worker has confirmed the required
// disconnect, and with no sessions left the generation is released together
// with its source reference and its budget charge. Sessions still in flight
// keep it held.
func (b *Broker) DrainAck(ctx context.Context, generation, workerID, leaseID, secret string, instances ...string) *protocol.Error {
	b.mu.Lock()
	rec, err := b.mutable(generation)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if !workerMatches(rec, workerID, secret) {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	if len(instances) > 0 && (instances[0] == "" || rec.WorkerInstanceID != instances[0]) {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker", "the worker instance does not match")
	}
	if rec.LeaseID == "" || rec.LeaseID != leaseID {
		b.mu.Unlock()
		return protocol.New(protocol.KindAuthentication, "worker",
			"the lease is unknown")
	}
	if rec.State == StateReleased {
		if err := b.store.RemoveRef(refVolumeID(b.scope, rec.Generation)); err != nil {
			b.mu.Unlock()
			return protocol.New(protocol.KindSourceUnavailable, "drain", "cache claim release remains pending")
		}
		b.mu.Unlock()
		return nil
	}
	if rec.State != StateDraining {
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "drain",
			"the generation is not draining")
	}
	b.recountSessions(rec)
	if rec.ActiveSessions > 0 || rec.UnknownSessions > 0 {
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "drain",
			"the generation still holds admitted sessions")
	}
	if rec.BindingUID != "" {
		b.mu.Unlock()
		if err := b.releaseGlobalGeneration(ctx, generation); err != nil {
			// The release path answers in protocol verdicts; a wrapped local
			// proof failure keeps its verdict through the unwrap.
			var perr *protocol.Error
			if errors.As(err, &perr) {
				return perr
			}
			return protocol.New(protocol.KindInternal, "drain", "%v", err)
		}
		return nil
	}
	perr := b.release(rec)
	b.mu.Unlock()
	return perr
}

// quarantineStageReasons are the only quarantine reasons that carry a
// diagnostic stage: four exact fixed strings, one per HIXL source stage.
// Any other reason — including one carrying arbitrary or hostile text —
// records no stage and quarantines normally.
var quarantineStageReasons = map[string]string{
	"hixl-connect":    "connect",
	"hixl-submit":     "submit",
	"hixl-poll":       "poll",
	"hixl-disconnect": "disconnect",
}

// quarantineStageFromReason returns the bounded stage an allowlisted
// quarantine reason carries, or the empty string for every other reason.
func quarantineStageFromReason(reason string) string {
	return quarantineStageReasons[reason]
}

// Quarantine holds a generation whose completion could not be proven: an
// operation with an unknown outcome, or a disconnect that failed. Its
// registration, memory, budget charge, and source reference are retained
// until a qualified fence applies; nothing recycles them while it stands.
func (b *Broker) Quarantine(generation, workerID, leaseID, secret, reason string, instances ...string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return err
	}
	if !workerMatches(rec, workerID, secret) {
		return protocol.New(protocol.KindAuthentication, "worker",
			"the control message does not match the admitted worker")
	}
	if len(instances) > 0 && (instances[0] == "" || rec.WorkerInstanceID != instances[0]) {
		return protocol.New(protocol.KindAuthentication, "worker", "the worker instance does not match")
	}
	if rec.LeaseID == "" || rec.LeaseID != leaseID {
		return protocol.New(protocol.KindAuthentication, "worker",
			"the lease is unknown")
	}
	// The stage rides the existing reason field as one of four exact fixed
	// strings. The first valid report fills the empty field and wins; no
	// report ever overwrites a recorded stage, and an unlisted or
	// secret-bearing reason is discarded while quarantine proceeds. The
	// stage is a generation-level hint about where an attempt failed —
	// never evidence of submission, terminality, or DMA. A failed ledger
	// write rolls the in-memory fields back so a retry can persist them;
	// the quarantine itself is retained either way.
	oldStage, oldUpdated := rec.DiagnosticStage, rec.Updated
	stage := quarantineStageFromReason(reason)
	stageFills := stage != "" && rec.DiagnosticStage == ""
	if stageFills {
		rec.DiagnosticStage = stage
	}
	if rec.State == StateQuarantined {
		if stageFills || rec.quarantineUnpersisted {
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				rec.DiagnosticStage, rec.Updated = oldStage, oldUpdated
				return protocol.New(protocol.KindSourceUnavailable, "quarantine",
					"the state could not be recorded: %v", err)
			}
			rec.quarantineUnpersisted = false
		}
		return nil
	}
	rec.State = StateQuarantined
	rec.Updated = b.now()
	// The quarantine reason stays in the worker's report except for its
	// allowlisted stage prefix, which the bounded field above persists; the
	// unbounded text itself never reaches the ledger.
	if err := b.ledger.write(rec); err != nil {
		rec.DiagnosticStage, rec.Updated = oldStage, oldUpdated
		rec.quarantineUnpersisted = true
		return protocol.New(protocol.KindSourceUnavailable, "quarantine",
			"the state could not be recorded: %v", err)
	}
	rec.quarantineUnpersisted = false
	return nil
}

// Tick sweeps leases: a serving generation whose lease expired has lost its
// worker, so its readiness is invalid — it drains, holding its memory, while
// its published tree stays disk-ready.
func (b *Broker) Tick() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.State != StateServing {
			continue
		}
		if rec.LeaseID == "" || !b.now().Before(rec.LeaseExpires) {
			rec.State = StateDraining
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reconstruct recovers durable records after reading the admitted workers.
// A restart invalidates readiness and holds filling and serving generations
// for drain: Pod absence does not prove a native release.
func (b *Broker) Reconstruct(liveWorkers func() (map[string]bool, error)) error {
	if liveWorkers == nil {
		return fmt.Errorf("no worker set was given")
	}
	live, err := liveWorkers()
	if err != nil {
		return fmt.Errorf("read the admitted workers: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.NativeStarted && !rec.NativeReleased && !live[rec.WorkerID] &&
			(rec.State == StateFilling || rec.State == StateServing || rec.State == StateDraining) {
			rec.State = StateQuarantined
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				rec.quarantineUnpersisted = true
				return err
			}
			rec.quarantineUnpersisted = false
			continue
		}
		switch rec.State {
		case StateFilling:
			// Absence of the Pod does not prove its native registrations
			// were released. Preserve the charge and stop new work.
			rec.State = StateDraining
			if rec.NativeStartEnforced && !rec.NativeStarted {
				rec.NativeReleased = true
			}
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				return err
			}
		case StateServing:
			rec.State = StateDraining
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// InvalidateServing transitions filling and serving generations to draining
// when their live authorization cannot be read, retaining all charges.
func (b *Broker) InvalidateServing() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.State != StateServing && rec.State != StateFilling {
			continue
		}
		rec.State = StateDraining
		rec.Updated = b.now()
		if err := b.ledger.write(rec); err != nil {
			return err
		}
	}
	return nil
}

// FenceReplacedWorkerPods invalidates readiness without proving native release.
// Pod absence never acknowledges an operation or releases a native charge.
//
// Deprecated: use Reconstruct; an API worker set cannot establish a fence.
func (b *Broker) FenceReplacedWorkerPods(liveWorkers map[string]bool, currentWorkerID string) error {
	if liveWorkers == nil || currentWorkerID == "" {
		return nil
	}
	live := make(map[string]bool, len(liveWorkers)+1)
	for id, present := range liveWorkers {
		live[id] = present
	}
	live[currentWorkerID] = true
	return b.Reconstruct(func() (map[string]bool, error) { return live, nil })
}

// Snapshot is the reported view of the node's generations. It names a digest
// and a generation and never a tenant, a worker, a lease, or a secret.
func (b *Broker) Snapshot() []workercore.NodeModelStoreModelTransferGeneration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return snapshotOf(b.records, b.now)
}

// ReadSnapshot reads a broker root's durable records and derives the reported
// view, without a broker process: the model-manager reporter uses this, so it
// stays the single writer of NodeModelStore status while the transport
// workers run in their own process. A serving record whose lease has expired
// reads as draining.
func ReadSnapshot(root string, now time.Time) ([]workercore.NodeModelStoreModelTransferGeneration, error) {
	l := &ledger{root: root}
	records, err := l.records()
	if err != nil {
		return nil, err
	}
	indexed := map[string]*GenerationRecord{}
	for _, rec := range records {
		indexed[rec.Generation] = rec
	}
	return snapshotOf(indexed, func() time.Time { return now }), nil
}

func snapshotOf(records map[string]*GenerationRecord, now func() time.Time) []workercore.NodeModelStoreModelTransferGeneration {
	var entries []workercore.NodeModelStoreModelTransferGeneration
	for _, rec := range records {
		if rec.State == StateFilling || rec.State == StateReleased {
			continue
		}
		state := workercore.NodeModelStoreModelTransferGenerationState(rec.State)
		if rec.State == StateServing &&
			(rec.LeaseID == "" || !now().Before(rec.LeaseExpires)) {
			state = workercore.NodeModelStoreModelTransferGenerationStateDraining
		}
		entry := workercore.NodeModelStoreModelTransferGeneration{
			Generation: rec.Generation, Digest: rec.Digest, State: state,
			ResidentBytes: rec.ResidentBytes, Registrations: rec.Registrations,
			ActiveSessions: rec.ActiveSessions, RejectedAdmissions: rec.RejectedAdmissions,
		}
		if rec.NativeStartEnforced && rec.NativeStarted && rec.WorkerID != "" && rec.NativeInstance != "" &&
			rec.NativeInstance == rec.WorkerInstanceID && rec.SourceGeneration > 0 && !rec.LeaseExpires.IsZero() {
			entry.SourcePodUID = rec.WorkerID
			entry.SourceInstanceID = rec.NativeInstance
			entry.SourceGeneration = rec.SourceGeneration
			lease := meta.NewTime(rec.LeaseExpires)
			entry.LeaseExpiresAt = &lease
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return compareGenerations(entries[i].Generation, entries[j].Generation)
	})
	return entries
}

func (b *Broker) admittedSessions(generation string) int32 {
	var count int32
	for _, record := range b.sessions {
		if record.Generation == generation && record.Phase != "released" {
			count++
		}
	}
	return count
}

// nodeActiveSessionsLocked counts every generation's live session charge:
// the sessions the broker can name plus the unknown ones it persists. A
// released generation charges nothing.
func (b *Broker) nodeActiveSessionsLocked() int32 {
	var count int32
	for _, rec := range b.records {
		if rec.State == StateReleased {
			continue
		}
		count += rec.ActiveSessions
	}
	return count
}

// recountSessions derives a generation's session charge from its own
// truth: the sessions the broker can name plus the unknown charge it
// persists. Unknown charges never decay on their own — a heartbeat, an
// admission, or a release moves only the named part.
func (b *Broker) recountSessions(rec *GenerationRecord) {
	rec.ActiveSessions = b.admittedSessions(rec.Generation) + rec.UnknownSessions
}

// ClearUnknownSessions is the qualified fence's hook: it is the only way
// an unknown session charge leaves a generation. Nothing in the broker's
// own lifecycle calls it, because a restart, a heartbeat, or a drain
// prove nothing about engines or DMA.
func (b *Broker) ClearUnknownSessions(generation, fenceReason string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.mutable(generation)
	if err != nil {
		return err
	}
	if fenceReason == "" {
		return protocol.New(protocol.KindInvalidDescriptor, "fence",
			"a qualified fence names its evidence")
	}
	rec.UnknownSessions = 0
	b.recountSessions(rec)
	rec.Updated = b.now()
	if err := b.ledger.write(rec); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "fence",
			"the fence could not be recorded: %v", err)
	}
	return nil
}

func (b *Broker) checkNamespaceRegistrations(rec *GenerationRecord, registrations int32) *protocol.Error {
	if rec.Grant.Registrations <= 0 {
		return nil
	}
	total := int(registrations)
	for _, other := range b.records {
		if other.Generation == rec.Generation || other.Namespace != rec.Namespace || other.BindingUID != rec.BindingUID {
			continue
		}
		switch other.State {
		case StateFilling, StateServing, StateDraining, StateQuarantined:
			total += int(max(other.Registrations, 1))
		}
	}
	if total > int(rec.Grant.Registrations) {
		return protocol.New(protocol.KindCapacityExhausted, "worker",
			"the namespace's registration count is exhausted")
	}
	return nil
}

// mutable returns the live record of a generation, refusing the ones no
// control message may touch.
func (b *Broker) mutable(generation string) (*GenerationRecord, *protocol.Error) {
	rec, ok := b.records[generation]
	if !ok {
		return nil, protocol.New(protocol.KindStaleGeneration, "generation",
			"the generation does not exist on this node")
	}
	return rec, nil
}

// serving says whether a generation admits new readers: it must be Serving
// with a live lease, so an expired lease is caught even before Tick.
func (b *Broker) serving(rec *GenerationRecord) bool {
	return rec.State == StateServing && rec.LeaseID != "" &&
		b.now().Before(rec.LeaseExpires)
}

func (b *Broker) countRefusal(rec *GenerationRecord) {
	rec.RejectedAdmissions++
	// Refusal reporting is best effort and never changes the admission outcome.
	_ = b.ledger.write(rec)
}

func (b *Broker) issueLease(rec *GenerationRecord) *Lease {
	rec.LeaseID = newSessionID()
	rec.LeaseExpires = b.now().Add(b.limits.LeaseTTL)
	rec.Updated = b.now()
	return &Lease{ID: rec.LeaseID, Expires: rec.LeaseExpires}
}

// release drops a generation: its record and its durable source reference go
// together, so the collector may take the tree once nothing else references
// it and the budgets stop charging it.
func (b *Broker) release(rec *GenerationRecord) *protocol.Error {
	if err := b.ledger.remove(rec.Generation); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "release",
			"the record could not be removed: %v", err)
	}
	if err := b.store.RemoveRef(refVolumeID(b.scope, rec.Generation)); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "release",
			"the source reference could not be removed: %v", err)
	}
	delete(b.records, rec.Generation)
	delete(b.assignmentSizes, rec.Generation)
	return nil
}

func workerMatches(rec *GenerationRecord, workerID, secret string) bool {
	if rec.WorkerID != workerID || workerID == "" || secret == "" {
		return false
	}
	hash := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hash[:])),
		[]byte(rec.WorkerSecretHash)) == 1
}

func newWorkerSecret() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	secret := hex.EncodeToString(buf)
	hash := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(hash[:]), nil
}

func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("s-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// Close releases the broker's single-writer ledger lock. Serving after
// Close is a programming error, not a recovered state: the lock's owner
// is gone.
func (b *Broker) Close() error {
	if b.lock == nil {
		return nil
	}
	err := b.lock.Close()
	b.lock = nil
	return err
}
