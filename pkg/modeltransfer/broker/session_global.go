package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

const (
	// sessionJournalLimit is the shared journal's hard total record bound.
	// Older binaries reject larger journals, so it never changes here.
	sessionJournalLimit = 4096
	// sessionJournalReserve is the part of the shared journal that new
	// refusal records must not consume. Existing session history and
	// already-full old journals can occupy it.
	sessionJournalReserve = 1024
	// refusalJournalCeiling is the largest total record count at which a
	// new refusal record may be created: only a pre-insertion total below
	// this value allows one.
	refusalJournalCeiling = sessionJournalLimit - sessionJournalReserve
)

// LiveIntent is the immutable source identity checked at session admission.
type LiveIntent struct {
	Namespace, IntentName, IntentUID, BindingName, BindingUID string
	ArtifactName, ArtifactUID, Digest, NodeName, WorkerID     string
	IntentGeneration, SourceGeneration                        int64
}

// SetIntentValidator installs the live Kubernetes entitlement check.
func (b *Broker) SetIntentValidator(validator func(context.Context, LiveIntent) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.intentValidator = validator
}

// SetSessionPodChecker installs the live Kubernetes check that reports
// whether a session's target Pod still exists and has not terminated.
func (b *Broker) SetSessionPodChecker(checker func(ctx context.Context, namespace, podUID string) (bool, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionPodChecker = checker
}

func (b *Broker) sessionHasUnacknowledgedOperationLocked(sessionID string) bool {
	for _, op := range b.operations {
		if op.Request.SessionID == sessionID && op.State != "acknowledged" {
			return true
		}
	}
	return false
}

// sessionHasDispatchedOperationLocked keeps every possibly posted operation fenced.
func (b *Broker) sessionHasDispatchedOperationLocked(sessionID string) bool {
	for _, op := range b.operations {
		if op.Request.SessionID == sessionID && op.State != "acknowledged" && op.State != "prepared" {
			return true
		}
	}
	return false
}

// hasRemainingOperationsLocked reports whether any receipt still names the
// session, in any state: a session its journal can still explain is never
// reclaimed.
func (b *Broker) hasRemainingOperationsLocked(sessionID string) bool {
	for _, op := range b.operations {
		if op.Request.SessionID == sessionID {
			return true
		}
	}
	return false
}

// sessionJournalCompactFloor is half the shared journal's hard bound: at
// or past it, terminal session records are reclaimed before the caps are
// checked, so completed history cannot wedge the node at the limit.
const sessionJournalCompactFloor = 2048

// compactSessionsLocked reclaims terminal session records and the journal
// space behind them. A released session whose generation is released is
// reclaimed whenever no receipt names it; released and refused records are
// also reclaimed under pressure, once the journal holds at least half its
// bound of records. The reclaimed set is persisted so a restart does not
// resurrect it. A persist failure leaves memory holding fewer sessions
// than disk, the safe direction: capacity stays charged, and the next
// successful persist of the whole map repairs the difference.
func (b *Broker) compactSessionsLocked() error {
	return b.compactSessionsScopeLocked(true)
}

// compactReleasedSessionsLocked reclaims only released session records,
// leaving refused history untouched: the refusal reserve is pinned to
// decline rather than reclaim refusal-only saturation, so the reserve
// checks never reclaim refused records themselves.
func (b *Broker) compactReleasedSessionsLocked() error {
	return b.compactSessionsScopeLocked(false)
}

// compactSessionsScopeLocked reclaims terminal session records and the
// journal space behind them. A released session whose generation is
// released is reclaimed whenever no receipt names it; released and,
// optionally, refused records are also reclaimed under pressure, once the
// journal holds at least half its bound of records. The reclaimed set is
// persisted so a restart does not resurrect it. A persist failure leaves
// memory holding fewer sessions than disk, the safe direction: capacity
// stays charged, and the next successful persist of the whole map repairs
// the difference.
func (b *Broker) compactSessionsScopeLocked(includeRefused bool) error {
	b.compactOperationsLocked()
	removed := false
	for id, session := range b.sessions {
		if b.hasRemainingOperationsLocked(id) {
			continue
		}
		rec := b.records[session.Generation]
		generationDone := rec == nil || rec.State == StateReleased
		if session.Phase == "released" && generationDone {
			delete(b.sessions, id)
			removed = true
			continue
		}
		if includeRefused && len(b.sessions) >= sessionJournalCompactFloor &&
			(session.Phase == "released" || session.Phase == "refused") {
			delete(b.sessions, id)
			removed = true
		}
	}
	if !removed {
		return nil
	}
	return b.persistSessionsLocked()
}

func sessionReservation(id string, session *SessionRecord) SessionReservation {
	return SessionReservation{
		SessionID: id, PodUID: session.OwnerUID, Namespace: session.Namespace,
		ArtifactUID: session.ArtifactUID, Digest: session.Digest, Generation: session.Generation,
		RequestID: session.RequestID, TargetInstanceID: session.TargetInstanceID,
		SourcePodUID: session.SourcePodUID, SourceInstanceID: session.SourceInstanceID,
		SourceLeaseID: session.SourceLeaseID, OpenFingerprint: session.OpenFingerprint,
	}
}

func stableSessionID(owner SessionOwner) string {
	hash := sha256.Sum256([]byte(owner.PodUID + "\x00" + owner.TargetInstanceID + "\x00" + owner.RequestID))
	return "s-" + hex.EncodeToString(hash[:])
}

// sameRefusal reports whether the record is the refusal of this exact
// request: the same owner, the same request identity and the same opened
// request bytes. A request that differs in any of them is another request.
func (s *SessionRecord) sameRefusal(owner SessionOwner) bool {
	return s.Phase == "refused" && s.OwnerUID == owner.PodUID && s.Namespace == owner.Namespace &&
		s.RequestID == owner.RequestID && s.TargetInstanceID == owner.TargetInstanceID &&
		s.OpenFingerprint == owner.OpenFingerprint
}

// RecordUnadmitted records an immutable refusal only when no session exists.
func (b *Broker) RecordUnadmitted(owner SessionOwner) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.operationsReady || !sessionNoncePattern.MatchString(owner.RequestID) || !sessionNoncePattern.MatchString(owner.TargetInstanceID) ||
		owner.PodUID == "" || owner.Namespace == "" || owner.OpenFingerprint == "" {
		return false
	}
	id := stableSessionID(owner)
	if existing := b.sessions[id]; existing != nil {
		return existing.sameRefusal(owner)
	}
	// Reclaim released history before the reserve ceiling is checked, so
	// finished generations cannot wedge refusal journaling at the limit.
	// Refused records are left alone: the reserve declines rather than
	// reclaims refusal-only saturation, so a journaled refusal receipt
	// stays immutable and the decline behavior stays stable. The reclaim
	// is best-effort: a failed persist still leaves the checks below to
	// answer honestly, and a refusal that cannot be journaled returns
	// false and carries no durable receipt while the caller still receives
	// the plain refusal from the live admission guards.
	if len(b.sessions) >= sessionJournalCompactFloor {
		_ = b.compactReleasedSessionsLocked()
	}
	if len(b.sessions) >= refusalJournalCeiling {
		return false
	}
	b.sessions[id] = &SessionRecord{
		Phase: "refused", OwnerUID: owner.PodUID, Namespace: owner.Namespace,
		RequestID: owner.RequestID, TargetInstanceID: owner.TargetInstanceID, OpenFingerprint: owner.OpenFingerprint,
	}
	if err := b.persistSessionsLocked(); err != nil {
		b.operationsReady = false
		return false
	}
	return true
}

// admitGlobalSessionLocked persists the local owner before the global CAS,
// and persists its admitted phase before exposing the session to a caller.
func (b *Broker) admitGlobalSessionLocked(ctx context.Context, rec *GenerationRecord, owner SessionOwner) (*Session, *protocol.Error) {
	if owner.PodUID == "" || owner.Namespace != rec.Namespace || owner.ArtifactUID == "" ||
		owner.ArtifactUID != rec.ArtifactUID || owner.Digest != rec.Digest {
		return nil, protocol.New(protocol.KindDeniedContent, "session", "the session owner does not match the generation")
	}
	if b.inventoryRequired {
		if b.assignmentsFrozen || b.intentValidator == nil {
			return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the live source entitlement is unavailable")
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := b.intentValidator(checkCtx, LiveIntent{
			Namespace: rec.Namespace, IntentName: rec.IntentName, IntentUID: rec.IntentUID,
			IntentGeneration: rec.IntentGeneration, BindingName: rec.BindingName, BindingUID: rec.BindingUID,
			ArtifactName: rec.ArtifactName, ArtifactUID: rec.ArtifactUID, SourceGeneration: rec.SourceGeneration,
			Digest: rec.Digest, NodeName: rec.NodeName, WorkerID: rec.WorkerID,
		})
		cancel()
		if err != nil {
			return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the live source entitlement is unavailable")
		}
	}
	authority := b.reservationAuthorities[types.UID(rec.BindingUID)]
	if authority == nil {
		return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the Binding authority is unavailable")
	}
	id := newSessionID()
	if owner.RequestID != "" && owner.TargetInstanceID != "" {
		id = stableSessionID(owner)
	}
	session := b.sessions[id]
	if session != nil && session.Phase == "refused" {
		// A refusal is immutable and terminal. The refused request identity
		// is answered with the same qualification refusal, so a client that
		// retries its own request is never told a false authentication
		// failure. A changed request is not the refused request, so the owner
		// and fingerprint binding refuses it. The refusal never becomes an
		// admission, whatever the gate does.
		if !session.sameRefusal(owner) {
			return nil, protocol.New(protocol.KindAuthentication, "session",
				"the request identity is bound to another request")
		}
		if err := b.sourceGate("session", rec.Backend); err != nil {
			return nil, err
		}
		return nil, protocol.New(protocol.KindStaleGeneration, "session", "the request is no longer open")
	}
	if session != nil {
		if session.Generation != rec.Generation || session.OwnerUID != owner.PodUID || session.Namespace != owner.Namespace ||
			session.ArtifactUID != owner.ArtifactUID || session.Digest != owner.Digest ||
			session.RequestID != owner.RequestID || session.TargetInstanceID != owner.TargetInstanceID ||
			session.OpenFingerprint != owner.OpenFingerprint || session.SourcePodUID != rec.WorkerID ||
			session.SourceInstanceID != rec.NativeInstance || session.SourceLeaseID != rec.LeaseID {
			return nil, protocol.New(protocol.KindAuthentication, "session", "the request is bound to another owner")
		}
		if session.Phase != "prepared" && session.Phase != "admitted" {
			return nil, protocol.New(protocol.KindStaleGeneration, "session", "the request is no longer open")
		}
	}
	// An admitted session is an observation of memory already exposed: its
	// owner may read it again and may close it. Any other request would newly
	// expose the source's memory, so the gate refuses it.
	if session == nil || session.Phase != "admitted" {
		if err := b.sourceGate("session", rec.Backend); err != nil {
			return nil, err
		}
	}
	if session == nil {
		// Reclaim released history before the hard caps are checked, so
		// finished generations cannot wedge admission at the journal
		// bound. Refused records stay put for the same reserve reason as
		// refusal journaling. Best-effort: the caps below still refuse a
		// genuinely full or genuinely over-active journal.
		if len(b.sessions) >= sessionJournalCompactFloor {
			_ = b.compactReleasedSessionsLocked()
		}
		if len(b.sessions) >= 4096 {
			return nil, protocol.New(protocol.KindCapacityExhausted, "session", "the session journal is full")
		}
		active := 0
		for _, existing := range b.sessions {
			if existing.Phase != "released" && existing.Phase != "refused" {
				active++
			}
		}
		if active >= 1024 {
			return nil, protocol.New(protocol.KindCapacityExhausted, "session", "the active session bound is reached")
		}
		session = &SessionRecord{
			Generation: rec.Generation, OwnerUID: owner.PodUID, Namespace: owner.Namespace,
			ArtifactUID: owner.ArtifactUID, Digest: owner.Digest, Phase: "prepared",
			RequestID: owner.RequestID, TargetInstanceID: owner.TargetInstanceID,
			SourcePodUID: rec.WorkerID, SourceInstanceID: rec.NativeInstance, SourceLeaseID: rec.LeaseID,
			OpenFingerprint: owner.OpenFingerprint,
		}
		b.sessions[id] = session
		if err := b.persistSessionsLocked(); err != nil {
			b.operationsReady = false
			delete(b.sessions, id)
			return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the prepared owner could not be persisted")
		}
		b.recountSessions(rec)
		if err := b.ledger.write(rec); err != nil {
			return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the prepared charge could not be persisted")
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := authority.ReserveSessionSlot(requestCtx, rec.ReservationID, sessionReservation(id, session)); err != nil {
		if errors.Is(err, errReservationCapacity) {
			return nil, protocol.New(protocol.KindCapacityExhausted, "session", "the Binding's session grant is exhausted")
		}
		return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the global session slot is unavailable")
	}
	session.Phase = "admitted"
	if err := b.persistSessionsLocked(); err != nil {
		b.operationsReady = false
		return nil, protocol.New(protocol.KindSourceUnavailable, "session", "the admitted owner could not be persisted")
	}
	return &Session{ID: id, Generation: rec.Generation}, nil
}

func (b *Broker) releaseGlobalSessionLocked(
	ctx context.Context, rec *GenerationRecord, id string, session *SessionRecord, owner SessionOwner,
) *protocol.Error {
	if session.Phase == "released" {
		if session.RequestID != "" && (session.CloseRequestID != owner.CloseRequestID || !owner.TargetReleased) {
			return protocol.New(protocol.KindAuthentication, "session", "the close request does not match")
		}
		return nil
	}
	if session.RequestID != "" && (session.CloseRequestID == "" || session.CloseRequestID != owner.CloseRequestID || !owner.TargetReleased) {
		return protocol.New(protocol.KindOperationState, "session", "the target release has not been confirmed")
	}
	if session.RequestID != "" && !session.SourceClosed {
		return protocol.New(protocol.KindOperationState, "session", "the source session has not closed")
	}
	for _, op := range b.operations {
		if op.Request.SessionID == id && op.State != "acknowledged" {
			return protocol.New(protocol.KindOperationState, "session", "an operation remains outstanding")
		}
	}
	if session.Phase != "admitted" && session.Phase != "closing" && session.Phase != "releasing" {
		return protocol.New(protocol.KindOperationState, "session", "the session was not exposed")
	}
	if session.Phase == "admitted" || session.Phase == "closing" {
		previous := session.Phase
		session.Phase = "releasing"
		if err := b.persistSessionsLocked(); err != nil {
			session.Phase = previous
			return protocol.New(protocol.KindSourceUnavailable, "session", "the release intent could not be persisted")
		}
	}
	if err := b.finishSessionReleaseLocked(ctx, rec, id, session); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "session", "the session release remains pending")
	}
	return nil
}

// BeginSessionClose records the authenticated target's request to stop the
// session. The global slot stays charged until target release is confirmed.
func (b *Broker) BeginSessionClose(owner SessionOwner, id string) *protocol.Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[id]
	if session == nil {
		return protocol.New(protocol.KindOperationState, "session", "the session is unknown")
	}
	if session.OwnerUID != owner.PodUID || session.Namespace != owner.Namespace || session.ArtifactUID != owner.ArtifactUID ||
		session.Digest != owner.Digest || session.TargetInstanceID != owner.TargetInstanceID || owner.CloseRequestID == "" {
		return protocol.New(protocol.KindAuthentication, "session", "the session owner does not match")
	}
	if session.Phase == "closing" || session.Phase == "releasing" || session.Phase == "released" {
		if session.CloseRequestID == owner.CloseRequestID {
			return nil
		}
		return protocol.New(protocol.KindAuthentication, "session", "the close request does not match")
	}
	if session.Phase != "admitted" || session.RequestID == "" {
		return protocol.New(protocol.KindOperationState, "session", "the session is not admitted")
	}
	session.Phase, session.CloseRequestID = "closing", owner.CloseRequestID
	if err := b.persistSessionsLocked(); err != nil {
		b.operationsReady = false
		session.Phase, session.CloseRequestID = "admitted", ""
		return protocol.New(protocol.KindSourceUnavailable, "session", "the close request could not be persisted")
	}
	return nil
}

// ClosePrivateSession fences source operations before the target releases the session.
func (b *Broker) ClosePrivateSession(ctx context.Context, owner SessionOwner, id string) *protocol.Error {
	if err := b.BeginSessionClose(owner, id); err != nil {
		return err
	}
	b.mu.Lock()
	session := b.sessions[id]
	if session == nil {
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "session", "the session is unknown")
	}
	if session.SourceClosed || session.Phase == "released" {
		b.mu.Unlock()
		return nil
	}
	rec := b.records[session.Generation]
	if !b.operationsReady || b.privateSource == nil || rec == nil || rec.WorkerID != session.SourcePodUID ||
		rec.NativeInstance != session.SourceInstanceID || rec.LeaseID != session.SourceLeaseID {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "session", "the source incarnation is unavailable")
	}
	if !session.SourceCloseStarted {
		session.SourceCloseStarted = true
		if err := b.persistSessionsLocked(); err != nil {
			b.operationsReady = false
			b.mu.Unlock()
			return protocol.New(protocol.KindSourceUnavailable, "session", "the source close could not be recorded")
		}
	}
	body := map[string]any{
		"sourcePodUID": session.SourcePodUID, "workerInstanceID": session.SourceInstanceID,
		"generation": session.Generation, "leaseID": session.SourceLeaseID, "sessionID": id, "requestID": owner.CloseRequestID,
	}
	private := b.privateSource
	b.mu.Unlock()
	reply, err := private.Call(ctx, "/v1/sessions/close", body)
	if err != nil || reply.State != "closed" {
		return protocol.New(protocol.KindSourceUnavailable, "session", "the source close remains pending")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	session = b.sessions[id]
	if !b.operationsReady || session == nil || session.CloseRequestID != owner.CloseRequestID {
		return protocol.New(protocol.KindSourceUnavailable, "session", "the source close is unknown")
	}
	for _, op := range b.operations {
		if op.Request.SessionID == id && op.State != "acknowledged" {
			return protocol.New(protocol.KindSourceUnavailable, "session", "an operation remains outstanding")
		}
	}
	session.SourceClosed = true
	if err := b.persistSessionsLocked(); err != nil {
		b.operationsReady = false
		return protocol.New(protocol.KindSourceUnavailable, "session", "the source close could not be recorded")
	}
	return nil
}

func (b *Broker) finishSessionReleaseLocked(ctx context.Context, rec *GenerationRecord, id string, session *SessionRecord) error {
	authority := b.reservationAuthorities[types.UID(rec.BindingUID)]
	if authority == nil {
		return fmt.Errorf("binding authority unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := authority.ReleaseSessionSlotIfPresent(
		requestCtx, rec.ReservationID, b.scope, b.ledger.identity, sessionReservation(id, session),
	); err != nil {
		return err
	}
	oldPhase := session.Phase
	session.Phase = "released"
	if err := b.persistSessionsLocked(); err != nil {
		session.Phase = oldPhase
		return err
	}
	b.recountSessions(rec)
	return b.ledger.write(rec)
}

// RetrySessionTransactions completes unexposed preparations, requested
// releases, and idle sessions whose target Pod has terminated, without
// granting access or releasing any session with an unacknowledged operation.
// A Pod-liveness read that fails is never treated as Pod disappearance: the
// session is left alone and the pass carries on, so one tenant namespace's
// API hiccup cannot stall the pass or drain a live session.
func (b *Broker) RetrySessionTransactions(ctx context.Context) error {
	b.mu.Lock()
	for id, session := range b.sessions {
		if session.Phase != "prepared" && session.Phase != "releasing" {
			continue
		}
		rec := b.records[session.Generation]
		if rec == nil || rec.BindingUID == "" {
			b.mu.Unlock()
			return fmt.Errorf("the session's generation is unavailable")
		}
		if err := b.finishSessionReleaseLocked(ctx, rec, id, session); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	checker := b.sessionPodChecker
	if checker == nil || (b.privateSource != nil && !b.operationsReady) {
		_ = b.compactSessionsLocked()
		b.mu.Unlock()
		return nil
	}
	type candidate struct {
		id        string
		namespace string
		ownerUID  string
	}
	var candidates []candidate
	for id, session := range b.sessions {
		if session.Phase != "admitted" && session.Phase != "closing" {
			continue
		}
		if session.Namespace == "" || session.OwnerUID == "" {
			continue
		}
		rec := b.records[session.Generation]
		if rec == nil || rec.BindingUID == "" {
			continue
		}
		if b.sessionHasDispatchedOperationLocked(id) {
			continue
		}
		candidates = append(candidates, candidate{
			id:        id,
			namespace: session.Namespace,
			ownerUID:  session.OwnerUID,
		})
	}
	b.mu.Unlock()

	// One Pod can own many sessions: the liveness answer is shared for the
	// pass, so a single owner's sessions never multiply into API reads.
	checked := map[string]bool{}
	for _, item := range candidates {
		key := item.namespace + "/" + item.ownerUID
		live, ok := checked[key]
		if !ok {
			var err error
			live, err = checker(ctx, item.namespace, item.ownerUID)
			if err != nil {
				// A failed read proves nothing about the Pod: treat it as
				// live so the pass carries on and the session is held.
				live = true
			}
			checked[key] = live
		}
		if live {
			continue
		}
		if err := b.reclaimTerminatedPodSession(ctx, item.id); err != nil {
			return err
		}
	}
	b.mu.Lock()
	_ = b.compactSessionsLocked()
	b.mu.Unlock()
	return nil
}

func (b *Broker) reclaimTerminatedPodSession(ctx context.Context, id string) error {
	b.mu.Lock()
	session := b.sessions[id]
	if session == nil || (session.Phase != "admitted" && session.Phase != "closing") {
		b.mu.Unlock()
		return nil
	}
	rec := b.records[session.Generation]
	if rec == nil || rec.BindingUID == "" {
		b.mu.Unlock()
		return nil
	}
	if (b.privateSource != nil && !b.operationsReady) || b.sessionHasDispatchedOperationLocked(id) {
		b.mu.Unlock()
		return nil
	}
	// A durable prepared record never reached the private POST. Once its
	// target Pod is gone, record that proof without contacting native code.
	for _, op := range b.operations {
		if op.Request.SessionID != id || op.State != "prepared" {
			continue
		}
		receipt := *op
		receipt.State, receipt.Verdict = "acknowledged", "not-posted"
		if err := b.persistOperationLocked(&receipt); err != nil {
			b.operationsReady = false
			b.mu.Unlock()
			return err
		}
	}
	if session.RequestID != "" && session.CloseRequestID == "" {
		session.CloseRequestID = session.RequestID
	}
	if session.Phase != "closing" {
		previous := session.Phase
		session.Phase = "closing"
		if err := b.persistSessionsLocked(); err != nil {
			b.operationsReady = false
			session.Phase = previous
			b.mu.Unlock()
			return err
		}
	}
	if session.RequestID != "" && !session.SourceClosed {
		if !b.operationsReady || b.privateSource == nil || rec.WorkerID != session.SourcePodUID ||
			rec.NativeInstance != session.SourceInstanceID || rec.LeaseID != session.SourceLeaseID {
			b.mu.Unlock()
			return nil
		}
		if !session.SourceCloseStarted {
			session.SourceCloseStarted = true
			if err := b.persistSessionsLocked(); err != nil {
				b.operationsReady = false
				b.mu.Unlock()
				return err
			}
		}
		closeRequestID := session.CloseRequestID
		body := map[string]any{
			"sourcePodUID":     session.SourcePodUID,
			"workerInstanceID": session.SourceInstanceID,
			"generation":       session.Generation,
			"leaseID":          session.SourceLeaseID,
			"sessionID":        id,
			"requestID":        closeRequestID,
		}
		private := b.privateSource
		b.mu.Unlock()
		reply, err := private.Call(ctx, "/v1/sessions/close", body)
		if err != nil || reply.State != "closed" {
			return nil
		}
		b.mu.Lock()
		session = b.sessions[id]
		if !b.operationsReady || session == nil || session.CloseRequestID != closeRequestID ||
			(session.Phase != "closing" && session.Phase != "releasing") ||
			b.sessionHasUnacknowledgedOperationLocked(id) {
			b.mu.Unlock()
			return nil
		}
		rec = b.records[session.Generation]
		if rec == nil || rec.BindingUID == "" {
			b.mu.Unlock()
			return nil
		}
		session.SourceClosed = true
		if err := b.persistSessionsLocked(); err != nil {
			b.operationsReady = false
			b.mu.Unlock()
			return err
		}
	}
	if session.Phase == "admitted" || session.Phase == "closing" {
		previous := session.Phase
		session.Phase = "releasing"
		if err := b.persistSessionsLocked(); err != nil {
			session.Phase = previous
			b.mu.Unlock()
			return err
		}
	}
	defer b.mu.Unlock()
	return b.finishSessionReleaseLocked(ctx, rec, id, session)
}
