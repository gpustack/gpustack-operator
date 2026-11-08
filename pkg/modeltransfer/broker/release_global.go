package broker

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// localProofError marks a failure to read or persist the local record:
// the durable proof state of that record is uncertain, so work that would
// treat it as released must not continue. The protocol error keeps the
// verdict callers already map.
type localProofError struct{ perr *protocol.Error }

func (e *localProofError) Error() string { return e.perr.Error() }
func (e *localProofError) Unwrap() error { return e.perr }

// releaseGlobalGeneration retires one drained Binding-bound generation.
// Each authority CAS runs without the broker mutex between durable
// checkpoints — native release proof, the global drain mark, the global
// release, and the local released record — so a slow authority never
// blocks unrelated control traffic. A generation that stops draining
// between steps (quarantined, or charged again) holds the remaining
// release, and a lost CAS response replays idempotently through the
// generation release pass.
//
// A failure to read or persist the local record comes back wrapped in
// localProofError: the record's durable state is uncertain, unlike an
// authority or admission failure, which holds this generation alone.
func (b *Broker) releaseGlobalGeneration(ctx context.Context, generation string) error {
	// Checkpoint one: durable native-release proof and the release identity.
	b.mu.Lock()
	rec, err := b.mutable(generation)
	if err != nil {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain remains pending")
	}
	if rec.BindingUID == "" || rec.State != StateDraining {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain remains pending")
	}
	b.recountSessions(rec)
	if rec.ActiveSessions > 0 || rec.UnknownSessions > 0 {
		b.mu.Unlock()
		return protocol.New(protocol.KindOperationState, "drain", "the generation still holds admitted sessions")
	}
	if !rec.NativeReleased {
		next := *rec
		next.NativeReleased = true
		next.Updated = b.now()
		if err := b.ledger.write(&next); err != nil {
			b.mu.Unlock()
			return &localProofError{protocol.New(protocol.KindSourceUnavailable, "drain", "native release proof could not be persisted")}
		}
		*rec = next
	}
	authority := b.reservationAuthorities[types.UID(rec.BindingUID)]
	if authority == nil {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the Binding authority is unavailable")
	}
	reservationID, bindingUID := rec.ReservationID, rec.BindingUID
	b.mu.Unlock()

	// Checkpoint two: the global drain mark.
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := authority.MarkDrained(requestCtx, reservationID, b.scope, b.ledger.identity); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain remains pending")
	}
	b.mu.Lock()
	rec = b.records[generation]
	if rec == nil || rec.BindingUID != bindingUID || rec.ReservationID != reservationID ||
		rec.State != StateDraining || !rec.NativeReleased {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain remains pending")
	}
	b.recountSessions(rec)
	if rec.ActiveSessions > 0 || rec.UnknownSessions > 0 {
		b.mu.Unlock()
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain remains pending")
	}
	if !rec.AuthorityDrained {
		next := *rec
		next.AuthorityDrained = true
		next.Updated = b.now()
		if err := b.ledger.write(&next); err != nil {
			b.mu.Unlock()
			return &localProofError{protocol.New(protocol.KindSourceUnavailable, "drain", "the global drain confirmation could not be persisted")}
		}
		*rec = next
	}
	b.mu.Unlock()

	// Checkpoint three: the global release.
	if err := authority.ReleaseDrainedIfPresent(requestCtx, reservationID, b.scope, b.ledger.identity); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global release remains pending")
	}

	// Checkpoint four: the local released record and the cache reference.
	b.mu.Lock()
	defer b.mu.Unlock()
	rec = b.records[generation]
	if rec == nil || rec.BindingUID != bindingUID || rec.ReservationID != reservationID ||
		rec.State != StateDraining || !rec.NativeReleased || !rec.AuthorityDrained {
		return protocol.New(protocol.KindSourceUnavailable, "drain", "the global release remains pending")
	}
	next := *rec
	next.State = StateReleased
	next.IntentKey = ""
	next.WorkerSecret = ""
	next.ResidentBytes = 0
	next.Registrations = 0
	next.ManifestJSON = ""
	next.TreePath = ""
	next.Descriptors = nil
	next.Updated = b.now()
	if err := b.ledger.write(&next); err != nil {
		return &localProofError{protocol.New(protocol.KindSourceUnavailable, "drain", "the released generation could not be persisted")}
	}
	*rec = next
	delete(b.assignmentSizes, generation)
	if err := b.store.RemoveRef(refVolumeID(b.scope, rec.Generation)); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "drain", "cache claim release remains pending")
	}
	return nil
}

// sweepReleasedRefsLocked removes cache references for generations in this
// broker's vendor scope that are already released or absent from the ledger,
// recovering from a crash between writing StateReleased and removing the ref.
func (b *Broker) sweepReleasedRefsLocked() error {
	if b.store == nil || b.scope == "" {
		return nil
	}
	refs, err := b.store.Refs()
	if err != nil {
		return err
	}
	prefix := "model-transfer." + b.scope + "."
	for _, ref := range refs {
		if !strings.HasPrefix(ref.VolumeID, prefix) {
			continue
		}
		gen := strings.TrimPrefix(ref.VolumeID, prefix)
		if !validGeneration(gen) {
			continue
		}
		if rec := b.records[gen]; rec == nil || rec.State == StateReleased {
			if err := b.store.RemoveRef(ref.VolumeID); err != nil {
				return err
			}
		}
	}
	return nil
}

// releasedGenerationKeep is how many fully released generation records the
// ledger retains as recent history; older ones are reclaimed.
const releasedGenerationKeep = 64

// compactReleasedGenerationsLocked reclaims old released generation records
// once nothing names them anymore. A record any session or receipt still
// references is kept, whatever its age; beyond the retained history the
// oldest released records are removed, so a node that turns over many
// generations never grows its ledger without bound.
func (b *Broker) compactReleasedGenerationsLocked() error {
	if err := b.sweepManifestStoresLocked(); err != nil {
		return err
	}
	referenced := make(map[string]bool, len(b.sessions)+len(b.operations))
	for _, session := range b.sessions {
		referenced[session.Generation] = true
	}
	for _, op := range b.operations {
		referenced[op.Request.Generation] = true
	}
	var released []string
	for generation, rec := range b.records {
		if rec.State != StateReleased {
			continue
		}
		delete(b.assignmentSizes, generation)
		if referenced[generation] {
			continue
		}
		released = append(released, generation)
	}
	if len(released) <= releasedGenerationKeep {
		return nil
	}
	sort.Slice(released, func(i, j int) bool {
		return compareGenerations(released[i], released[j])
	})
	for _, generation := range released[:len(released)-releasedGenerationKeep] {
		if err := b.ledger.remove(generation); err != nil {
			return err
		}
		delete(b.records, generation)
		delete(b.assignmentSizes, generation)
	}
	return nil
}

// RetryGenerationReleases retries only generations with durable native release
// evidence, never one inferred from a lost Pod or expired lease. The
// authority CAS runs without the broker mutex per generation. An authority,
// admission-hold, or stale-state failure holds its generation alone: the
// pass keeps releasing the other candidates and still runs the terminal
// reclamation — released records and the terminal sessions and receipts
// behind them leave the ledger, and the released trees' cache references
// are swept — then reports the first failure it met. A failure to read or
// persist a record's local proof returns at once, with no terminal sweep:
// cleanup past an uncertain proof state could fabricate release evidence.
func (b *Broker) RetryGenerationReleases(ctx context.Context) error {
	b.mu.Lock()
	var candidates []string
	for generation, rec := range b.records {
		if rec.BindingUID == "" || rec.State != StateDraining || !rec.NativeReleased {
			continue
		}
		b.recountSessions(rec)
		if rec.ActiveSessions != 0 || rec.UnknownSessions != 0 {
			continue
		}
		candidates = append(candidates, generation)
	}
	b.mu.Unlock()
	var held error
	for _, generation := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.releaseGlobalGeneration(ctx, generation); err != nil {
			var proof *localProofError
			if errors.As(err, &proof) {
				return err
			}
			if held == nil {
				held = err
			}
			continue
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.compactSessionsLocked(); err != nil {
		return err
	}
	if err := b.compactReleasedGenerationsLocked(); err != nil {
		return err
	}
	if err := b.sweepReleasedRefsLocked(); err != nil {
		return err
	}
	return held
}
