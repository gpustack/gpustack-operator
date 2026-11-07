package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The Binding-wide reservation authority. SourceMemory Bytes, Registrations,
// and Sessions are Binding-wide grants, so no node may spend the whole grant
// on its own: every node/vendor reserves against ONE authority document — a
// ConfigMap in the operator's own namespace named for the full Binding UID —
// with a resourceVersion CAS per change. The document carries no credentials,
// native keys, or pointers, and its absence or corruption is an error that
// HOLDS local charges, never an empty set that would free them.
const (
	authorityAPIVersion    = 1
	authorityMaxEntries    = 256
	authorityMaxDataBytes  = 256 << 10
	authorityCASAttempts   = 6
	authorityCASBaseDelay  = 20 * time.Millisecond
	authorityDocumentKey   = "reservations.json"
	AuthorityAnnotation    = "model-transfer.gpustack.ai/authority"
	authorityStateReserved = "reserved"
	authorityStateAdmitted = "admitted"
	authorityStateDrained  = "drained"
	// ReservationStateAdmitted marks a global reservation exposed locally.
	ReservationStateAdmitted = authorityStateAdmitted
)

// ReservationEntry is one node/vendor's claimed slice of a Binding-wide
// grant. It binds the immutable identities — Binding UID, node scope, ledger
// identity, generation, intent UID and spec generation — never a
// resourceVersion, so a replay after a crash re-adopts exactly this entry.
type ReservationEntry struct {
	ReservationID    string               `json:"reservationID"`
	NodeScope        string               `json:"nodeScope"`
	LedgerIdentity   string               `json:"ledgerIdentity"`
	Generation       string               `json:"generation"`
	IntentUID        string               `json:"intentUID"`
	IntentGeneration int64                `json:"intentGeneration"`
	Digest           string               `json:"digest"`
	Namespace        string               `json:"namespace"`
	NodeName         string               `json:"nodeName,omitempty"`
	ArtifactUID      string               `json:"artifactUID,omitempty"`
	SourceGeneration int64                `json:"sourceGeneration,omitempty"`
	SourcePodUID     string               `json:"sourcePodUID,omitempty"`
	Bytes            int64                `json:"bytes"`
	Registrations    int32                `json:"registrations"`
	Sessions         []SessionReservation `json:"sessions,omitempty"`
	State            string               `json:"state"`
	UpdatedAt        time.Time            `json:"updatedAt"`
}

// SessionReservation is one admitted read session's owner record: the
// authority counts owners, never bare counters, so an interrupted write
// recovers the same owner and a deletion never credits capacity.
type SessionReservation struct {
	SessionID        string `json:"sessionID"`
	PodUID           string `json:"podUID"`
	Namespace        string `json:"namespace"`
	ArtifactUID      string `json:"artifactUID"`
	Digest           string `json:"digest"`
	Generation       string `json:"generation"`
	RequestID        string `json:"requestID,omitempty"`
	TargetInstanceID string `json:"targetInstanceID,omitempty"`
	SourcePodUID     string `json:"sourcePodUID,omitempty"`
	SourceInstanceID string `json:"sourceInstanceID,omitempty"`
	SourceLeaseID    string `json:"sourceLeaseID,omitempty"`
	OpenFingerprint  string `json:"openFingerprint,omitempty"`
}

// authorityDocument is the whole reservation state for one Binding.
type authorityDocument struct {
	APIVersion int                `json:"apiVersion"`
	BindingUID string             `json:"bindingUID"`
	Epoch      string             `json:"epoch"`
	Entries    []ReservationEntry `json:"entries"`
}

// ReservationGrant is the live Binding-wide grant a reservation is checked
// against, fetched fresh by the caller at CAS time.
type ReservationGrant struct {
	// BindingUID is the live grant's own Binding identity: a grant read
	// for another Binding UID never passes the pinned authority.
	BindingUID    types.UID
	Bytes         int64
	Registrations int32
	Sessions      int32
}

// ErrReservationUnavailable marks authority state the caller must hold on:
// a corrupt, mismatched, over-cap, or API-unavailable document is never an
// empty set and never a release.
var ErrReservationUnavailable = errors.New(
	"the reservation authority is unavailable; local charges are held",
)

// ErrReservationReadFailed marks a failed API observation without evidence of
// authority loss. Admission still fails closed through ErrReservationUnavailable.
var ErrReservationReadFailed = errors.New("the reservation authority could not be read")

func reservationReadError(operation string, err error) error {
	if errors.Is(err, ErrReservationUnavailable) || apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s: %w", ErrReservationUnavailable, operation, err)
	}
	return fmt.Errorf("%w: %w: %s: %w", ErrReservationUnavailable, ErrReservationReadFailed, operation, err)
}

// authorityPin is the pinned authority identity verified on every
// mutation: the ConfigMap UID at pin time and the epoch recorded on the
// Binding. There is no zero-value production pin; only an explicit test
// fixture builds an authority without going through the real pin path.
type authorityPin struct {
	UID   string
	Epoch string
}

// AuthorityPin binds a Binding's reservations to one immutable authority
// object and epoch. The source service restores it from the Binding annotation.
type AuthorityPin = authorityPin

// AuthorityConfigMaps is the ConfigMap API used for authority CAS.
type AuthorityConfigMaps interface {
	Get(context.Context, string, meta.GetOptions) (*core.ConfigMap, error)
	List(context.Context, meta.ListOptions) (*core.ConfigMapList, error)
	Create(context.Context, *core.ConfigMap, meta.CreateOptions) (*core.ConfigMap, error)
	Update(context.Context, *core.ConfigMap, meta.UpdateOptions) (*core.ConfigMap, error)
}

// ReservationAuthority owns one Binding's authority ConfigMap.
type ReservationAuthority struct {
	maps        AuthorityConfigMaps
	namespace   string
	bindingUID  types.UID
	pin         authorityPin
	maxEntries  int
	maxBytes    int
	casAttempts int
	now         func() time.Time
	liveGrant   func(ctx context.Context) (ReservationGrant, error)
}

// SetClock replaces the clock, for tests.
func (a *ReservationAuthority) SetClock(now func() time.Time) { a.now = now }

// NewReservationAuthority builds the authority for one pinned Binding. The
// grant reader runs before every CAS pass so growth admits and shrink holds;
// production supplies it from live Binding reads, tests from fixtures. The
// pin is required: there is no zero-value production authority.
func NewReservationAuthority(maps AuthorityConfigMaps, namespace string,
	bindingUID types.UID, pin authorityPin,
	liveGrant func(ctx context.Context) (ReservationGrant, error),
) *ReservationAuthority {
	return &ReservationAuthority{
		maps:        maps,
		namespace:   namespace,
		bindingUID:  bindingUID,
		pin:         pin,
		maxEntries:  authorityMaxEntries,
		maxBytes:    authorityMaxDataBytes,
		casAttempts: authorityCASAttempts,
		now:         time.Now,
		liveGrant:   liveGrant,
	}
}

// AuthorityName is the deterministic ConfigMap name for a Binding UID.
func AuthorityName(bindingUID types.UID) string {
	return "model-transfer-resv-" + string(bindingUID)
}

// ReservationRequest is one node/vendor's claim before its native fill.
type ReservationRequest struct {
	// ReservationID is minted by the caller and persisted in its own
	// prepared record BEFORE this call, so a crash between the two writes
	// replays the same identity instead of forking a second claim.
	ReservationID    string
	NodeScope        string
	LedgerIdentity   string
	Generation       string
	IntentUID        string
	IntentGeneration int64
	Digest           string
	Namespace        string
	NodeName         string
	ArtifactUID      string
	SourceGeneration int64
	SourcePodUID     string
	Bytes            int64
	// Registrations is this generation's actual registration plan — the
	// number of windows the verified manifest and backend plan will
	// register — never a namespace-wide total and never an assumption.
	Registrations int32
}

// Reserve CAS-claims one entry against the live grant. The reservation is
// taken BEFORE the native fill starts; state stays reserved until the
// caller's local record is durably admitted.
func (a *ReservationAuthority) Reserve(ctx context.Context, req ReservationRequest) (
	ReservationEntry, error,
) {
	if err := ctx.Err(); err != nil {
		return ReservationEntry{}, fmt.Errorf("%w: %w", ErrReservationUnavailable, err)
	}
	if req.ReservationID == "" || req.NodeScope == "" || req.LedgerIdentity == "" || req.Generation == "" ||
		req.IntentUID == "" || req.IntentGeneration < 1 || req.Digest == "" || req.Namespace == "" {
		return ReservationEntry{}, fmt.Errorf("%w: the reservation names no complete identity",
			ErrReservationUnavailable)
	}
	if req.Bytes <= 0 || req.Registrations <= 0 {
		return ReservationEntry{}, fmt.Errorf("%w: the reservation's bytes and registrations must be positive",
			ErrReservationUnavailable)
	}
	var last error
	for attempt := 0; attempt < a.casAttempts; attempt++ {
		entry, err := a.tryReserve(ctx, req)
		if err == nil {
			return entry, nil
		}
		if !errors.Is(err, errReservationConflict) {
			return ReservationEntry{}, err
		}
		last = err
		if !a.pause(ctx, attempt) {
			return ReservationEntry{}, fmt.Errorf("%w: %w", ErrReservationUnavailable, ctx.Err())
		}
	}
	return ReservationEntry{}, fmt.Errorf("%w: the authority stayed contested: %w",
		ErrReservationUnavailable, last)
}

var errReservationConflict = errors.New("the reservation document changed under us")

// errReservationCapacity marks a DEFINITIVE refusal: a CAS pass completed
// against a well-formed, full document. Only this outcome may cancel a
// prepared record; every other error retains the charge for replay.
var errReservationCapacity = errors.New("the Binding-wide capacity is exhausted")

// tryReserve is one read-modify-write pass.
func (a *ReservationAuthority) tryReserve(ctx context.Context, req ReservationRequest) (
	ReservationEntry, error,
) {
	// The grant is re-read before EVERY pass: growth admits, shrink holds,
	// and no stale snapshot captured before the retries ever passes.
	grant, err := a.readGrant(ctx)
	if err != nil {
		return ReservationEntry{}, fmt.Errorf("%w: %w", ErrReservationUnavailable, err)
	}
	existing, err := a.maps.Get(ctx, AuthorityName(a.bindingUID), getOneOptions)
	if apierrors.IsNotFound(err) {
		// The pinned authority is missing after pinning: fail closed and
		// hold charges; recreating it here could fork the grant.
		return ReservationEntry{}, fmt.Errorf("%w: the pinned authority object is missing",
			ErrReservationUnavailable)
	}
	if err != nil {
		return ReservationEntry{}, reservationReadError("read authority", err)
	}
	doc, err := a.decode(existing)
	if err != nil {
		return ReservationEntry{}, err
	}
	if doc.BindingUID != string(a.bindingUID) {
		return ReservationEntry{}, fmt.Errorf("%w: the authority belongs to Binding UID %q",
			ErrReservationUnavailable, doc.BindingUID)
	}
	// Idempotence: the same scope, ledger identity, and generation is the
	// same reservation, whatever called.
	for i := range doc.Entries {
		entry := &doc.Entries[i]
		if entry.ReservationID == req.ReservationID || (entry.NodeScope == req.NodeScope &&
			entry.LedgerIdentity == req.LedgerIdentity && entry.Generation == req.Generation) {
			if entry.ReservationID != req.ReservationID || entry.NodeScope != req.NodeScope ||
				entry.LedgerIdentity != req.LedgerIdentity || entry.Generation != req.Generation ||
				entry.IntentUID != req.IntentUID || entry.IntentGeneration != req.IntentGeneration ||
				entry.Digest != req.Digest || entry.Namespace != req.Namespace || entry.Bytes != req.Bytes ||
				entry.Registrations != req.Registrations || entry.NodeName != req.NodeName ||
				entry.ArtifactUID != req.ArtifactUID || entry.SourceGeneration != req.SourceGeneration || entry.SourcePodUID != req.SourcePodUID {
				return ReservationEntry{}, fmt.Errorf("%w: the reservation replay changes immutable fields",
					ErrReservationUnavailable)
			}
			return *entry, nil
		}
	}
	if len(doc.Entries) >= a.maxEntries {
		return ReservationEntry{}, capacityDenied("the Binding's reservation entries are exhausted")
	}
	// The grant is checked against the live totals of every entry of every
	// node and vendor, plus this request — overflow-safe by subtraction.
	bytes, registrations, _, err := liveTotals(doc)
	if err != nil {
		return ReservationEntry{}, err
	}
	if bytes > grant.Bytes || req.Bytes > grant.Bytes-bytes {
		return ReservationEntry{}, capacityDenied("the Binding's reserved-bytes grant is exhausted across nodes")
	}
	if registrations > int64(grant.Registrations) || int64(req.Registrations) > int64(grant.Registrations)-registrations {
		return ReservationEntry{}, capacityDenied("the Binding's registration grant is exhausted across nodes")
	}
	entry := ReservationEntry{
		ReservationID:    req.ReservationID,
		NodeScope:        req.NodeScope,
		LedgerIdentity:   req.LedgerIdentity,
		Generation:       req.Generation,
		IntentUID:        req.IntentUID,
		IntentGeneration: req.IntentGeneration,
		Digest:           req.Digest,
		Namespace:        req.Namespace,
		NodeName:         req.NodeName,
		ArtifactUID:      req.ArtifactUID,
		SourceGeneration: req.SourceGeneration,
		SourcePodUID:     req.SourcePodUID,
		Bytes:            req.Bytes,
		Registrations:    req.Registrations,
		Sessions:         []SessionReservation{},
		State:            authorityStateReserved,
		UpdatedAt:        a.now(),
	}
	doc.Entries = append(doc.Entries, entry)
	if err := a.update(ctx, existing, doc); err != nil {
		return ReservationEntry{}, err
	}
	return entry, nil
}

// MarkReserved confirms a prepared record's global reservation exists; it
// is a no-op on an already-reserved or admitted entry.
func (a *ReservationAuthority) MarkReserved(ctx context.Context, nodeScope,
	ledgerIdentity, generation string,
) error {
	return a.markAdmissible(ctx, nodeScope, ledgerIdentity, generation, false)
}

// MarkAdmitted marks the entry bound to this scope, ledger identity, and
// generation admitted once the node's own record is durably present; only
// then may the assignment reach a worker. The lookup is by immutable
// identity, so whichever CAS copy of the reservation the caller holds, the
// marked entry is the one the authority actually keeps.
func (a *ReservationAuthority) MarkAdmitted(ctx context.Context, nodeScope,
	ledgerIdentity, generation string,
) error {
	return a.markAdmissible(ctx, nodeScope, ledgerIdentity, generation, true)
}

func (a *ReservationAuthority) markAdmissible(ctx context.Context, nodeScope, ledgerIdentity, generation string, admit bool) error {
	return a.mutate(ctx, func(doc *authorityDocument) (bool, error) {
		grant, err := a.readGrant(ctx)
		if err != nil {
			return false, err
		}
		bytes, registrations, _, err := liveTotals(doc)
		if err != nil {
			return false, err
		}
		// Sessions is deliberately absent from this check: a grant whose
		// session cap was lowered online keeps its already admitted sessions
		// while they drain, so a live total above the lowered cap must not
		// block the generation-level marks.
		if bytes > grant.Bytes || registrations > int64(grant.Registrations) {
			return false, capacityDenied("the live Binding grant no longer covers its reservations")
		}
		for i := range doc.Entries {
			entry := &doc.Entries[i]
			if entry.NodeScope != nodeScope || entry.LedgerIdentity != ledgerIdentity || entry.Generation != generation {
				continue
			}
			if entry.State != authorityStateReserved && entry.State != authorityStateAdmitted {
				return false, fmt.Errorf("%w: the entry is not admissible", ErrReservationUnavailable)
			}
			if !admit || entry.State == authorityStateAdmitted {
				return false, nil
			}
			entry.State = authorityStateAdmitted
			entry.UpdatedAt = a.now()
			return true, nil
		}
		return false, fmt.Errorf("%w: the reservation is unavailable", ErrReservationUnavailable)
	})
}

// ReserveSessionSlot CAS-claims one named session owner, summing every
// live entry's sessions across nodes against the fresh grant. The full
// owner record and the immutable session attempt ID are the identity: a
// retry is idempotent, a different owner is a refusal. A grant whose
// session cap was lowered online keeps its already held owners idempotent
// and caps only new claims until the live total drains below the new cap.
func (a *ReservationAuthority) ReserveSessionSlot(ctx context.Context, reservationID string,
	owner SessionReservation,
) error {
	return a.mutate(ctx, func(doc *authorityDocument) (bool, error) {
		var entry *ReservationEntry
		for i := range doc.Entries {
			if doc.Entries[i].ReservationID == reservationID {
				entry = &doc.Entries[i]
				break
			}
		}
		if entry == nil || entry.State != authorityStateAdmitted {
			return false, fmt.Errorf("%w: the session reservation is not admitted", ErrReservationUnavailable)
		}
		if owner.SessionID == "" || owner.PodUID == "" || owner.ArtifactUID == "" ||
			owner.Namespace != entry.Namespace || owner.Generation != entry.Generation || owner.Digest != entry.Digest {
			return false, fmt.Errorf("%w: the session owner does not match its generation", ErrReservationUnavailable)
		}
		grant, err := a.readGrant(ctx)
		if err != nil {
			return false, err
		}
		bytes, registrations, liveSessions, serr := liveTotals(doc)
		if serr != nil {
			return false, serr
		}
		// Bytes and registrations must still cover the live totals, but the
		// session cap is enforced for NEW claims only, after the idempotent
		// replay below: a cap lowered online must not strand the sessions it
		// already admitted.
		if bytes > grant.Bytes || registrations > int64(grant.Registrations) {
			return false, capacityDenied("the Binding's grant no longer covers its reservations")
		}
		for _, existing := range entry.Sessions {
			if existing.SessionID == owner.SessionID {
				if existing != owner {
					return false, fmt.Errorf("session %q is already held by a different owner", owner.SessionID)
				}
				return false, nil
			}
		}
		if liveSessions >= int64(grant.Sessions) {
			return false, capacityDenied("the Binding's session grant is exhausted across nodes")
		}
		entry.Sessions = append(entry.Sessions, owner)
		entry.UpdatedAt = a.now()
		return true, nil
	})
}

// ReleaseSessionSlot removes exactly one named session owner, and only for
// the entry's own scope and ledger identity with the complete owner record.
// Deleting the authority object never credits anything: only this named
// removal does.
func (a *ReservationAuthority) ReleaseSessionSlot(ctx context.Context, reservationID,
	nodeScope, ledgerIdentity string, owner SessionReservation,
) error {
	return a.mutateByIDAndScope(ctx, reservationID, nodeScope, ledgerIdentity,
		func(entry *ReservationEntry) error {
			kept := entry.Sessions[:0]
			removed := false
			for _, existing := range entry.Sessions {
				if existing == owner {
					removed = true
					continue
				}
				kept = append(kept, existing)
			}
			if !removed {
				return fmt.Errorf("session %q is not held by this exact owner record",
					owner.SessionID)
			}
			entry.Sessions = kept
			return nil
		}, false)
}

// ReleaseSessionSlotIfPresent replays an exact owner's durable release.
// A different owner with the same session ID remains an error.
func (a *ReservationAuthority) ReleaseSessionSlotIfPresent(ctx context.Context, reservationID,
	nodeScope, ledgerIdentity string, owner SessionReservation,
) error {
	return a.mutate(ctx, func(doc *authorityDocument) (bool, error) {
		for i := range doc.Entries {
			entry := &doc.Entries[i]
			if entry.ReservationID != reservationID {
				continue
			}
			if entry.NodeScope != nodeScope || entry.LedgerIdentity != ledgerIdentity {
				return false, fmt.Errorf("%w: reservation owner mismatch", ErrReservationUnavailable)
			}
			for j, existing := range entry.Sessions {
				if existing.SessionID != owner.SessionID {
					continue
				}
				if existing != owner {
					return false, fmt.Errorf("%w: session owner mismatch", ErrReservationUnavailable)
				}
				entry.Sessions = append(entry.Sessions[:j], entry.Sessions[j+1:]...)
				entry.UpdatedAt = a.now()
				return true, nil
			}
			return false, nil
		}
		return false, nil
	})
}

// Release compacts one fully drained entry out of the document, returning
// its quota exactly once: the owning scope and ledger identity must match,
// the entry must carry no session owners, and its state must be reserved
// (never exposed) or explicitly drained by its owning broker. An admitted
// generation cannot be credited on the basis of time or object absence.
func (a *ReservationAuthority) Release(ctx context.Context, reservationID, nodeScope,
	ledgerIdentity string,
) error {
	return a.mutateByIDAndScope(ctx, reservationID, nodeScope, ledgerIdentity,
		func(entry *ReservationEntry) error {
			if len(entry.Sessions) > 0 {
				return fmt.Errorf("the entry holds %d session owners; release is refused",
					len(entry.Sessions))
			}
			if entry.State != authorityStateReserved && entry.State != authorityStateDrained {
				return fmt.Errorf("generation %q has no confirmed drain", entry.Generation)
			}
			return nil
		}, true)
}

// MarkDrained records the owning broker's durable confirmation of native
// teardown. Call it only after successful disconnect/deregistration or an
// independently qualified fence; lease expiry and Pod absence are insufficient.
// The charge remains until Release completes its separate CAS.
func (a *ReservationAuthority) MarkDrained(ctx context.Context, reservationID, nodeScope, ledgerIdentity string) error {
	return a.mutateByIDAndScope(ctx, reservationID, nodeScope, ledgerIdentity, func(entry *ReservationEntry) error {
		if len(entry.Sessions) != 0 {
			return fmt.Errorf("the generation still holds session owners")
		}
		entry.State = authorityStateDrained
		return nil
	}, false)
}

// ReleaseDrainedIfPresent retries a locally recorded global release after a
// confirmed MarkDrained. An absent entry is an idempotent replay only after
// that durable local confirmation.
func (a *ReservationAuthority) ReleaseDrainedIfPresent(ctx context.Context, reservationID, nodeScope, ledgerIdentity string) error {
	return a.mutate(ctx, func(doc *authorityDocument) (bool, error) {
		for i := range doc.Entries {
			entry := &doc.Entries[i]
			if entry.ReservationID != reservationID {
				continue
			}
			if entry.NodeScope != nodeScope || entry.LedgerIdentity != ledgerIdentity || entry.State != authorityStateDrained || len(entry.Sessions) != 0 {
				return false, fmt.Errorf("%w: reservation is not fully drained by this owner", ErrReservationUnavailable)
			}
			doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
			return true, nil
		}
		return false, nil
	})
}

// Adopt returns this scope and ledger identity's live entries after a
// restart, so prepared work re-adopts exactly its own reservation.
func (a *ReservationAuthority) Adopt(ctx context.Context, nodeScope,
	ledgerIdentity string,
) ([]ReservationEntry, error) {
	existing, err := a.maps.Get(ctx, AuthorityName(a.bindingUID), getOneOptions)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: the pinned authority object is missing", ErrReservationUnavailable)
	}
	if err != nil {
		return nil, reservationReadError("read authority", err)
	}
	doc, err := a.decode(existing)
	if err != nil {
		return nil, err
	}
	var out []ReservationEntry
	for i := range doc.Entries {
		if doc.Entries[i].NodeScope == nodeScope && doc.Entries[i].LedgerIdentity == ledgerIdentity {
			out = append(out, doc.Entries[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Generation < out[j].Generation })
	return out, nil
}

// mutateByID runs one CAS pass of mutate over the entry with the named
// reservation ID, verified against its owning scope and ledger identity.
func (a *ReservationAuthority) mutateByIDAndScope(ctx context.Context, reservationID,
	nodeScope, ledgerIdentity string, mutate func(*ReservationEntry) error,
	remove bool,
) error {
	return a.mutate(ctx, func(doc *authorityDocument) (bool, error) {
		for i := range doc.Entries {
			entry := &doc.Entries[i]
			if entry.ReservationID != reservationID {
				continue
			}
			if entry.NodeScope != nodeScope || entry.LedgerIdentity != ledgerIdentity {
				return false, fmt.Errorf(
					"reservation %q belongs to scope %q, not %q",
					reservationID, entry.NodeScope, nodeScope,
				)
			}
			if err := mutate(entry); err != nil {
				return false, err
			}
			entry.UpdatedAt = a.now()
			if remove {
				doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
			}
			return true, nil
		}
		return false, fmt.Errorf("%w: the reservation %q is unknown to the authority",
			ErrReservationUnavailable, reservationID)
	})
}

// mutate drives one read-validate-mutate-CAS cycle with bounded retries. The
// authority object's UID and the document's pinned epoch are verified on
// every pass; a missing, recreated, or corrupt authority is an error that
// holds charges, and a document write whose response is lost surfaces as a
// conflict so the replay re-reads what actually landed. The mutator reports
// whether it changed the document; an unchanged replay writes nothing.
func (a *ReservationAuthority) mutate(ctx context.Context,
	mutate func(*authorityDocument) (bool, error),
) error {
	var last error
	for attempt := 0; attempt < a.casAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrReservationUnavailable, err)
		}
		existing, err := a.maps.Get(ctx, a.name(), getOneOptions)
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: the pinned authority object is missing", ErrReservationUnavailable)
		}
		if err != nil {
			return reservationReadError("read authority", err)
		}
		if string(existing.UID) != a.pin.UID {
			return fmt.Errorf("%w: the authority object's UID changed since the pin",
				ErrReservationUnavailable)
		}
		doc, err := a.decode(existing)
		if err != nil {
			return err
		}
		changed, err := mutate(doc)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if err := a.write(ctx, existing, doc); err == nil {
			return nil
		} else if !errors.Is(err, errReservationConflict) {
			return err
		}
		last = err
		if !a.pause(ctx, attempt) {
			return fmt.Errorf("%w: %w", ErrReservationUnavailable, ctx.Err())
		}
	}
	return fmt.Errorf("%w: the authority stayed contested: %w", ErrReservationUnavailable, last)
}

// capacityDenied marks a DEFINITIVE refusal: a CAS pass completed against a
// well-formed document whose live totals refused the request.
func capacityDenied(detail string) error {
	return fmt.Errorf("%w: %s", errReservationCapacity, detail)
}

// BindingPinner records the authority ConfigMap's UID and epoch on the
// Binding through a metadata patch; the Binding update webhook protects the
// annotation once written. Until the pin exists, no admission runs.
type BindingPinner interface {
	PatchBindingAuthority(ctx context.Context, bindingNamespace, bindingName,
		authorityUID, epoch string) error
}

// Pin reads the authority object and pins its UID and immutable epoch on
// the Binding, so two pinners converge on the same values.
func Pin(ctx context.Context, pinner BindingPinner, bindingNamespace, bindingName string,
	a *ReservationAuthority,
) error {
	cm, err := a.maps.Get(ctx, a.name(), getOneOptions)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: the authority object must be created by its owner first",
			ErrReservationUnavailable)
	}
	if err != nil {
		return reservationReadError("read authority", err)
	}
	doc, err := a.decode(cm)
	if err != nil {
		return err
	}
	return pinner.PatchBindingAuthority(ctx, bindingNamespace, bindingName,
		string(cm.UID), doc.Epoch)
}

func (a *ReservationAuthority) name() string {
	return AuthorityName(a.bindingUID)
}

func (a *ReservationAuthority) readGrant(ctx context.Context) (ReservationGrant, error) {
	if a.liveGrant == nil {
		return ReservationGrant{}, fmt.Errorf("%w: no live Binding grant reader", ErrReservationUnavailable)
	}
	grant, err := a.liveGrant(ctx)
	if err != nil {
		return ReservationGrant{}, reservationReadError("read live Binding grant", err)
	}
	if grant.BindingUID != a.bindingUID || grant.Bytes <= 0 || grant.Registrations <= 0 || grant.Sessions <= 0 {
		return ReservationGrant{}, fmt.Errorf("%w: the live Binding grant is absent or has changed identity", ErrReservationUnavailable)
	}
	return grant, nil
}

func (a *ReservationAuthority) decode(cm *core.ConfigMap) (*authorityDocument, error) {
	doc, err := decodeAuthority(cm, a.pin, a.maxEntries)
	if err != nil {
		return nil, err
	}
	if doc.BindingUID != string(a.bindingUID) || cm.Namespace != a.namespace {
		return nil, fmt.Errorf("%w: authority Binding or namespace mismatch", ErrReservationUnavailable)
	}
	return doc, nil
}

// pause waits between CAS passes under a small bounded cap, honoring the
// caller's context: the whole retry window stays well under a second.
func (a *ReservationAuthority) pause(ctx context.Context, attempt int) bool {
	delay := authorityCASBaseDelay * time.Duration(1<<attempt)
	if delay > 160*time.Millisecond {
		delay = 160 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// liveTotals sums every live entry's charges with checked arithmetic: a
// corrupt or overflowing document is unavailable, never a smaller total.
func liveTotals(doc *authorityDocument) (bytes, registrations, sessions int64, err error) {
	const ceiling = int64(1) << 62
	for i := range doc.Entries {
		entry := &doc.Entries[i]
		if entry.Bytes < 0 || entry.Registrations < 0 {
			return 0, 0, 0, fmt.Errorf("%w: entry %q carries a negative charge",
				ErrReservationUnavailable, entry.ReservationID)
		}
		if bytes > ceiling-entry.Bytes {
			return 0, 0, 0, fmt.Errorf("%w: the entries' bytes overflow the checked range",
				ErrReservationUnavailable)
		}
		bytes += entry.Bytes
		registrations += int64(entry.Registrations)
		sessions += int64(len(entry.Sessions))
	}
	return bytes, registrations, sessions, nil
}

// decodeAuthority parses and fully validates the inbound document: strict
// JSON (no unknown fields, no duplicate keys, no trailing data), bounded
// size, pinned UID and epoch, positive version, complete identities, known
// states, and no duplicate reservations.
func decodeAuthority(cm *core.ConfigMap, a_pin authorityPin, maxEntries int) (
	*authorityDocument, error,
) {
	if a_pin.UID == "" || a_pin.Epoch == "" || string(cm.UID) != a_pin.UID {
		return nil, fmt.Errorf("%w: the authority object does not match its immutable pin",
			ErrReservationUnavailable)
	}
	raw, ok := cm.Data[authorityDocumentKey]
	if !ok {
		return nil, fmt.Errorf("%w: the authority document is missing its data",
			ErrReservationUnavailable)
	}
	if len(raw) > authorityMaxDataBytes {
		return nil, fmt.Errorf("%w: the authority document exceeds its size bound",
			ErrReservationUnavailable)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReservationUnavailable, err)
	}
	doc := &authorityDocument{}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(doc); err != nil {
		return nil, fmt.Errorf("%w: the authority document is corrupt: %w",
			ErrReservationUnavailable, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: the authority document carries trailing data",
			ErrReservationUnavailable)
	}
	if doc.APIVersion != authorityAPIVersion || doc.BindingUID == "" || doc.Epoch == "" {
		return nil, fmt.Errorf("%w: the authority document's version, Binding UID, or epoch is wrong",
			ErrReservationUnavailable)
	}
	if doc.Epoch != a_pin.Epoch {
		return nil, fmt.Errorf("%w: the authority document's epoch does not match the pin",
			ErrReservationUnavailable)
	}
	seen := map[string]bool{}
	seenGenerations := map[string]bool{}
	seenSessions := map[string]bool{}
	for i := range doc.Entries {
		entry := &doc.Entries[i]
		if entry.ReservationID == "" || entry.NodeScope == "" || entry.LedgerIdentity == "" ||
			entry.Generation == "" || entry.IntentUID == "" || entry.Digest == "" ||
			entry.Namespace == "" {
			return nil, fmt.Errorf("%w: entry %q is missing an immutable identity field",
				ErrReservationUnavailable, entry.ReservationID)
		}
		if entry.Bytes < 0 || entry.Registrations <= 0 {
			return nil, fmt.Errorf("%w: entry %q carries non-positive or negative charges",
				ErrReservationUnavailable, entry.ReservationID)
		}
		switch entry.State {
		case authorityStateReserved, authorityStateAdmitted, authorityStateDrained:
		default:
			return nil, fmt.Errorf("%w: entry %q is in unknown state %q",
				ErrReservationUnavailable, entry.ReservationID, entry.State)
		}
		if seen[entry.ReservationID] {
			return nil, fmt.Errorf("%w: entry %q appears twice in the authority document",
				ErrReservationUnavailable, entry.ReservationID)
		}
		seen[entry.ReservationID] = true
		identity := entry.NodeScope + "\x00" + entry.LedgerIdentity + "\x00" + entry.Generation
		if seenGenerations[identity] {
			return nil, fmt.Errorf("%w: duplicate generation identity", ErrReservationUnavailable)
		}
		seenGenerations[identity] = true
		for _, owner := range entry.Sessions {
			if owner.SessionID == "" || owner.PodUID == "" || owner.ArtifactUID == "" ||
				owner.Namespace != entry.Namespace || owner.Digest != entry.Digest || owner.Generation != entry.Generation || seenSessions[owner.SessionID] {
				return nil, fmt.Errorf("%w: invalid or duplicate session owner", ErrReservationUnavailable)
			}
			seenSessions[owner.SessionID] = true
		}
	}
	if len(doc.Entries) > maxEntries {
		return nil, fmt.Errorf("%w: the authority document holds %d entries over its cap",
			ErrReservationUnavailable, len(doc.Entries))
	}
	return doc, nil
}

// rejectDuplicateJSONKeys walks the raw document and refuses any object
// whose object-level keys repeat: encoding/json alone would silently keep
// the last value.
func rejectDuplicateJSONKeys(raw string) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	if err := walkDuplicateKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func walkDuplicateKeys(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	return walkObjectKeys(dec, 0)
}

func walkObjectKeys(dec *json.Decoder, depth int) error {
	seen := map[string]bool{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("non-string object key")
		}
		if seen[key] {
			return fmt.Errorf("duplicate JSON key %q", key)
		}
		seen[key] = true
		if err := skipOrWalkValue(dec, depth+1); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

func skipOrWalkValue(dec *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("authority JSON nesting exceeds its bound")
	}
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); ok {
		switch d {
		case '{':
			return walkObjectKeys(dec, depth)
		case '[':
			for dec.More() {
				if err := skipOrWalkValue(dec, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
	}
	return nil
}

// write replaces the document under the read resourceVersion; a conflict is
// the caller's retry signal, and an applied-but-unreported write surfaces as
// the same conflict so the replay re-reads what actually landed.
func (a *ReservationAuthority) write(ctx context.Context, existing *core.ConfigMap,
	doc *authorityDocument,
) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if len(raw) > a.maxBytes {
		return fmt.Errorf("%w: the reservation document exceeds its size bound",
			ErrReservationUnavailable)
	}
	existing.Data = map[string]string{authorityDocumentKey: string(raw)}
	_, err = a.maps.Update(ctx, existing, updateOneOptions)
	if apierrors.IsConflict(err) {
		return errReservationConflict
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReservationUnavailable, err)
	}
	return nil
}

// Option sets for the authority's reads and writes.
var (
	getOneOptions    = meta.GetOptions{}
	updateOneOptions = meta.UpdateOptions{}
)

// update is the pre-compaction name for the CAS document writer; the
// mutate helpers call it.
func (a *ReservationAuthority) update(ctx context.Context, existing *core.ConfigMap,
	doc *authorityDocument,
) error {
	return a.write(ctx, existing, doc)
}
