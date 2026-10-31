package broker

import "fmt"

// adoptAuthoritySessions restores exact globally charged owners and refuses a
// missing or mismatched local generation rather than guessing that it ended.
func (b *Broker) adoptAuthoritySessions(bindingUID string, entries []ReservationEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	byGeneration := make(map[string]ReservationEntry, len(entries))
	owners := map[string]SessionReservation{}
	changed := false
	for _, entry := range entries {
		rec := b.records[entry.Generation]
		if rec == nil || rec.BindingUID != bindingUID || rec.ReservationID != entry.ReservationID || rec.State == StateReleased ||
			rec.NodeName != entry.NodeName || rec.WorkerID != entry.SourcePodUID || rec.ArtifactUID != entry.ArtifactUID ||
			rec.SourceGeneration != entry.SourceGeneration || rec.Namespace != entry.Namespace || rec.Digest != entry.Digest {
			return fmt.Errorf("%w: a global reservation has no matching local generation", ErrReservationUnavailable)
		}
		byGeneration[entry.Generation] = entry
		for _, owner := range entry.Sessions {
			if owner.Generation != entry.Generation || owner.Namespace != entry.Namespace || owner.Digest != entry.Digest ||
				owner.SessionID == "" || owner.PodUID == "" || owner.ArtifactUID == "" {
				return fmt.Errorf("%w: a global session owner is incomplete", ErrReservationUnavailable)
			}
			if _, duplicate := owners[owner.SessionID]; duplicate {
				return fmt.Errorf("%w: duplicate global session owner", ErrReservationUnavailable)
			}
			owners[owner.SessionID] = owner
			local := b.sessions[owner.SessionID]
			if local == nil {
				b.sessions[owner.SessionID] = &SessionRecord{
					Generation: owner.Generation, OwnerUID: owner.PodUID,
					Namespace: owner.Namespace, ArtifactUID: owner.ArtifactUID, Digest: owner.Digest, Phase: "admitted",
					RequestID: owner.RequestID, TargetInstanceID: owner.TargetInstanceID,
					SourcePodUID: owner.SourcePodUID, SourceInstanceID: owner.SourceInstanceID,
					SourceLeaseID: owner.SourceLeaseID, OpenFingerprint: owner.OpenFingerprint,
				}
				changed = true
			} else if local.Phase == "released" || sessionReservation(owner.SessionID, local) != owner {
				return fmt.Errorf("%w: a global session conflicts with its local owner", ErrReservationUnavailable)
			}
		}
	}
	for _, rec := range b.records {
		if rec.BindingUID != bindingUID || rec.State == StateReleased {
			continue
		}
		if _, exists := byGeneration[rec.Generation]; !exists && rec.AdmissionPhase != "prepared" && !rec.AuthorityDrained {
			return fmt.Errorf("%w: a local reservation is missing globally", ErrReservationUnavailable)
		}
	}
	for id, local := range b.sessions {
		rec := b.records[local.Generation]
		if rec == nil || rec.BindingUID != bindingUID || (local.Phase != "admitted" && local.Phase != "closing") {
			continue
		}
		if _, exists := owners[id]; !exists {
			return fmt.Errorf("%w: an admitted session is missing globally", ErrReservationUnavailable)
		}
	}
	if changed {
		if err := b.persistSessionsLocked(); err != nil {
			return err
		}
		for _, rec := range b.records {
			if rec.BindingUID != bindingUID {
				continue
			}
			b.recountSessions(rec)
			if err := b.ledger.write(rec); err != nil {
				return err
			}
		}
	}
	return nil
}
