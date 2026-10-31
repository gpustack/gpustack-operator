package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// RegistrationDescriptor is one natively registered window a worker
// reports after a real backend registration succeeded. It binds the
// vendor registration to the worker instance, the backend, and the
// immutable generation and digest it was admitted for; the broker serves
// it to admitted sessions only while that exact generation is registered
// and Serving under a live lease.
type RegistrationDescriptor struct {
	Name       string `json:"name"`
	Addr       uint64 `json:"addr"`
	Length     int64  `json:"length"`
	MemoryType string `json:"memoryType"`
	Backend    string `json:"backend"`
	WorkerUID  string `json:"workerUID"`
}

// Assignment is one admitted fill the broker offers its node's worker
// over the loopback control wire. The worker learns its work here, never
// from cluster-visible state: no secret, address, or registration key
// travels through status, logs, or command argv. An assignment is a view
// over the generation's own durable record, so it exists exactly as long
// as that record does — a crash cannot separate an admission from its
// identity.
type Assignment struct {
	// IntentKey identifies the admitted source intent (Prefetch UID plus
	// revision) so re-offering after a restart is idempotent.
	IntentKey string `json:"-"`
	// Generation is the admitted generation's opaque token.
	Generation string `json:"generation"`
	// WorkerSecret authenticates this worker's control messages for the
	// generation. It exists in this answer and in the worker only.
	WorkerSecret string `json:"workerSecret"`
	Digest       string `json:"digest"`
	Namespace    string `json:"namespace"`
	// ManifestPath is the node-local verified manifest the worker fills
	// from; it is node-local work ordering, not shared status.
	ManifestPath        string `json:"-"`
	TotalBytes          int64  `json:"totalBytes"`
	ResidentBudgetBytes int64  `json:"residentBudgetBytes,omitempty"`
	ArtifactUID         string `json:"artifactUID"`
	SourceGeneration    int64  `json:"sourceGeneration"`
	ManifestSHA256      string `json:"manifestSHA256"`
	ManifestJSON        string `json:"manifestJSON"`
	TreePath            string `json:"treePath"`
	Registrations       int32  `json:"registrations"`
}

// SetBootstrapCredential installs the Pod-local bootstrap credential. The
// comparison later is constant-time: the credential authorizes the
// worker wire.
func (b *Broker) SetBootstrapCredential(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bootstrap = []byte(token)
}

func (b *Broker) bootstrapMatches(token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.bootstrap) == 0 || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), b.bootstrap) == 1
}

// liveAssignment reports whether a record still backs an offerable
// assignment: it carries an intent, its admission secret, and a state
// whose worker is still bound. A retired offer clears its intent while
// retaining the secret for the existing holder's drain.
func liveAssignment(rec *GenerationRecord) bool {
	switch rec.State {
	case StateFilling, StateServing:
	default:
		return false
	}
	return rec.IntentKey != "" && rec.WorkerSecret != ""
}

func recordAssignment(rec *GenerationRecord) Assignment {
	return Assignment{
		IntentKey:           rec.IntentKey,
		Generation:          rec.Generation,
		WorkerSecret:        rec.WorkerSecret,
		Digest:              rec.Digest,
		Namespace:           rec.Namespace,
		ManifestPath:        rec.ManifestPath,
		TotalBytes:          rec.TotalBytes,
		ResidentBudgetBytes: rec.ResidentBudgetBytes,
		ArtifactUID:         rec.ArtifactUID,
		SourceGeneration:    rec.SourceGeneration,
		ManifestSHA256:      rec.ManifestSHA256,
		ManifestJSON:        rec.ManifestJSON,
		TreePath:            rec.TreePath,
		Registrations:       rec.Registrations,
	}
}

// AssignmentForIntent answers the assignment one intent revision holds,
// if any: the reconciler's idempotence read, backed by the same durable
// record the admission wrote.
func (b *Broker) AssignmentForIntent(intentKey string) (Assignment, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.IntentKey == intentKey && liveAssignment(rec) {
			return recordAssignment(rec), true
		}
	}
	return Assignment{}, false
}

// OfferedAssignments snapshots every assignment currently offered to the
// node's worker; the reconciler drains and retires the ones whose intent
// no longer appears.
func (b *Broker) OfferedAssignments() map[string]Assignment {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]Assignment{}
	for _, rec := range b.records {
		if liveAssignment(rec) {
			out[rec.IntentKey] = recordAssignment(rec)
		}
	}
	return out
}

// PendingIntentKeys includes preparations that have not exposed an offer.
func (b *Broker) PendingIntentKeys() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := map[string]bool{}
	for _, rec := range b.records {
		if rec.IntentKey != "" && rec.State != StateReleased {
			keys[rec.IntentKey] = true
		}
	}
	return keys
}

// AssignmentsFor answers the node worker's authenticated assignment
// poll. The bearer is the Pod-local bootstrap credential the broker
// minted into the shared emptyDir file; it authorizes the worker
// container on this node only, and the loopback listener is the only
// place it is accepted.
func (b *Broker) AssignmentsFor(bootstrapToken string) ([]Assignment, *protocol.Error) {
	if !b.bootstrapMatches(bootstrapToken) {
		return nil, protocol.New(protocol.KindAuthentication, "worker",
			"the bootstrap credential does not match this broker")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Assignment, 0, len(b.records))
	if b.assignmentsFrozen {
		return out, nil
	}
	var records []*GenerationRecord
	for _, rec := range b.records {
		if liveAssignment(rec) {
			records = append(records, rec)
		}
	}
	if len(records) == 0 {
		return out, nil
	}
	sort.Slice(records, func(i, j int) bool { return compareGenerations(records[i].Generation, records[j].Generation) })
	if b.assignmentSizes == nil {
		b.assignmentSizes = map[string]int{}
	}
	const budget = protocol.MaxManifestResponseBytes
	used := 32
	start := b.assignmentCursor % len(records)
	for i := range records {
		if len(out) == 16 {
			break
		}
		index := (start + i) % len(records)
		rec := records[index]
		assignment := recordAssignment(rec)
		size := b.assignmentSizes[rec.Generation]
		if size == 0 {
			raw, err := json.Marshal(assignment)
			if err != nil {
				return nil, protocol.New(protocol.KindSourceUnavailable, "worker", "the assignment cannot be encoded")
			}
			size = len(raw) + 1
			b.assignmentSizes[rec.Generation] = size
		}
		if used+size > budget {
			b.assignmentCursor = index
			if len(out) == 0 {
				b.assignmentCursor = (index + 1) % len(records)
				return nil, protocol.New(protocol.KindSourceUnavailable, "worker", "the assignment exceeds the response byte bound")
			}
			break
		}
		out = append(out, assignment)
		used += size
		b.assignmentCursor = (index + 1) % len(records)
	}
	return out, nil
}

// RetireIntentAssignment stops offering an intent and drains its generation.
// The secret remains available to the existing holder until native teardown
// and global reservation release are confirmed.
func (b *Broker) RetireIntentAssignment(intentKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.IntentKey != intentKey {
			continue
		}
		switch rec.State {
		case StateServing, StateFilling:
			// Retain the reservation and its secret until the holder confirms
			// native release. A lease or a missing worker is not a fence.
			rec.State = StateDraining
			if rec.NativeStartEnforced && !rec.NativeStarted {
				rec.NativeReleased = true
			}
			rec.Updated = b.now()
			if err := b.ledger.write(rec); err != nil {
				return fmt.Errorf("hold the retired intent %s: %w", intentKey, err)
			}
		case StateDraining, StateQuarantined:
		default:
			return fmt.Errorf("the retired intent %s holds no retirement state", intentKey)
		}
		// Stop offering the assignment, but retain its credential for the
		// existing holder's heartbeats and drain acknowledgement.
		rec.IntentKey = ""
		rec.Updated = b.now()
		if err := b.ledger.write(rec); err != nil {
			return fmt.Errorf("retire the assignment: %w", err)
		}
		return nil
	}
	return nil
}

// validateDescriptors checks each registration against the admitted worker
// and the generation's manifest. Derived manifests require one complete HOST
// registration for each file referenced by a tensor.
func (b *Broker) validateDescriptors(rec *GenerationRecord, workerID string,
	descriptors []RegistrationDescriptor,
) *protocol.Error {
	ranges := map[string]int64{}
	if rec.ManifestJSON != "" {
		cm, err := b.cachedDerivedManifestLocked(rec)
		if err != nil {
			return protocol.New(protocol.KindInvalidDescriptor, "worker", "the generation's derived manifest is invalid")
		}
		for _, file := range cm.manifest.ReferencedFiles() {
			if !strings.HasSuffix(file.Path, ".safetensors") {
				return protocol.New(protocol.KindInvalidDescriptor, "worker", "a tensor references a non-safetensors file")
			}
			ranges[file.Path] = file.Size
		}
		if (rec.ResidentBudgetBytes == 0 && len(ranges) != len(descriptors)) || int32(len(ranges)) != rec.Registrations {
			return protocol.New(protocol.KindInvalidDescriptor, "worker", "the registration count does not match the manifest")
		}
	} else if rec.ManifestPath != "" {
		manifest, err := protocol.LoadManifest(rec.ManifestPath, rec.Digest)
		if err != nil {
			return protocol.New(protocol.KindInvalidDescriptor, "worker",
				"the generation's verified manifest could not be read: %v", err)
		}
		for _, tensor := range manifest.Tensors {
			ranges[tensor.Name] = tensor.DataEnd - tensor.DataBegin
		}
	}
	var resident int64
	for _, descriptor := range descriptors {
		if descriptor.Name == "" || descriptor.WorkerUID != workerID ||
			descriptor.Backend == "" || descriptor.Length <= 0 {
			return protocol.New(protocol.KindInvalidDescriptor, "worker",
				"descriptor %q is incomplete: it needs its window name, the reporting worker, a backend, and a positive length",
				descriptor.Name)
		}
		if descriptor.MemoryType != "HOST" && descriptor.MemoryType != "DEVICE" {
			return protocol.New(protocol.KindInvalidDescriptor, "worker",
				"descriptor %q names unknown memory class %q",
				descriptor.Name, descriptor.MemoryType)
		}
		if rec.ManifestJSON != "" {
			extent, ok := ranges[descriptor.Name]
			if !ok || descriptor.Addr == 0 || descriptor.Length != extent || descriptor.MemoryType != "HOST" || descriptor.Backend != rec.Backend {
				return protocol.New(protocol.KindInvalidDescriptor, "worker", "descriptor %q does not match the registered file", descriptor.Name)
			}
			allocation := sourceAllocationBytes(descriptor.Length, rec.Backend)
			if rec.ResidentBudgetBytes > 0 && allocation > rec.ResidentBudgetBytes-resident {
				return protocol.New(protocol.KindInvalidDescriptor, "worker", "registered bytes exceed the resident admission")
			}
			resident += allocation
			delete(ranges, descriptor.Name)
			continue
		}
		if rec.ManifestPath == "" {
			continue
		}
		extent, ok := ranges[descriptor.Name]
		if !ok {
			return protocol.New(protocol.KindInvalidDescriptor, "worker",
				"descriptor %q names no tensor of this generation's manifest",
				descriptor.Name)
		}
		if descriptor.Length > extent {
			return protocol.New(protocol.KindInvalidDescriptor, "worker",
				"descriptor %q registers %d bytes beyond tensor %q's %d",
				descriptor.Name, descriptor.Length, descriptor.Name, extent)
		}
	}
	return nil
}

type cachedManifest struct {
	manifest      *protocol.Manifest
	artifactUID   string
	generation    int64
	sha256        string
	rawJSON       string
	tensorsByName map[string]protocol.TensorDescriptor
	tensorsByFile map[string][]protocol.TensorDescriptor
}

func (b *Broker) cachedDerivedManifestLocked(rec *GenerationRecord) (*cachedManifest, error) {
	return b.cachedDerivedManifestContextLocked(context.Background(), rec)
}

func (b *Broker) cachedDerivedManifestContextLocked(ctx context.Context, rec *GenerationRecord) (*cachedManifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.manifestCache != nil {
		if cm := b.manifestCache[rec.Generation]; cm != nil &&
			cm.artifactUID == rec.ArtifactUID &&
			cm.generation == rec.SourceGeneration &&
			cm.sha256 == rec.ManifestSHA256 &&
			cm.rawJSON == rec.ManifestJSON {
			return cm, nil
		}
	}
	manifest, err := parseDerivedManifestContext(ctx, rec)
	if err != nil {
		return nil, err
	}
	var byName map[string]protocol.TensorDescriptor
	var byFile map[string][]protocol.TensorDescriptor
	if len(manifest.TensorPages) == 0 {
		byName = make(map[string]protocol.TensorDescriptor, len(manifest.Tensors))
		byFile = make(map[string][]protocol.TensorDescriptor, len(manifest.Files))
		for _, t := range manifest.Tensors {
			byName[t.Name] = t
			byFile[t.File] = append(byFile[t.File], t)
		}
		for _, items := range byFile {
			sort.Slice(items, func(i, j int) bool { return items[i].DataBegin < items[j].DataBegin })
		}
	}
	cm := &cachedManifest{
		manifest:      manifest,
		artifactUID:   rec.ArtifactUID,
		generation:    rec.SourceGeneration,
		sha256:        rec.ManifestSHA256,
		rawJSON:       rec.ManifestJSON,
		tensorsByName: byName,
		tensorsByFile: byFile,
	}
	if b.manifestCache == nil {
		b.manifestCache = map[string]*cachedManifest{}
	}
	if len(b.manifestCache) >= 2 {
		for generation := range b.manifestCache {
			delete(b.manifestCache, generation)
			break
		}
	}
	b.manifestCache[rec.Generation] = cm
	return cm, nil
}

func parseDerivedManifestContext(ctx context.Context, rec *GenerationRecord) (*protocol.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rec.ManifestJSON) == 0 || len(rec.ManifestJSON) > protocol.MaxManifestBytes || rejectDuplicateJSONKeys(rec.ManifestJSON) != nil {
		return nil, fmt.Errorf("invalid derived manifest")
	}
	sum := sha256.Sum256([]byte(rec.ManifestJSON))
	if hex.EncodeToString(sum[:]) != rec.ManifestSHA256 {
		return nil, fmt.Errorf("derived manifest digest mismatch")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(rec.ManifestJSON))
	decoder.DisallowUnknownFields()
	var manifest protocol.Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing derived manifest data")
	}
	if err := manifest.Check(); err != nil {
		return nil, err
	}
	if manifest.ArtifactUID != rec.ArtifactUID || manifest.SourceGeneration != rec.SourceGeneration {
		return nil, fmt.Errorf("derived manifest identity mismatch")
	}
	manifest.ManifestSHA256 = rec.ManifestSHA256
	if len(manifest.TensorPages) > 0 {
		if len(rec.ManifestJSON) > protocol.MaxPagedManifestBytes {
			return nil, fmt.Errorf("paged root exceeds bound")
		}
		if rec.ManifestPagesPath == "" {
			return nil, fmt.Errorf("manifest pages are unavailable")
		}
		if err := manifest.BindPages(ctx, protocol.LocalPageReader(rec.ManifestPagesPath)); err != nil {
			return nil, err
		}
	}
	return &manifest, nil
}

// ResolveManifest returns a live source's exact previously derived manifest.
func (b *Broker) ResolveManifest(ctx context.Context, namespace, artifactUID, digest string, generation int64) (*protocol.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.Namespace == namespace && rec.ArtifactUID == artifactUID && rec.Digest == digest &&
			rec.SourceGeneration == generation && b.serving(rec) {
			cm, err := b.cachedDerivedManifestContextLocked(ctx, rec)
			if err != nil {
				return nil, err
			}
			return cm.manifest, nil
		}
	}
	return nil, fmt.Errorf("no matching serving source")
}

// SessionDetails is what an admitted session may see: the generation
// binding and the registered descriptors of a Serving generation under a
// live lease. A filling, draining, quarantined, or lease-less generation
// serves no descriptors.
type SessionDetails struct {
	Generation       string                   `json:"generation"`
	Digest           string                   `json:"digest"`
	Descriptors      []RegistrationDescriptor `json:"descriptors"`
	ContractVer      int                      `json:"contractVersion"`
	ArtifactUID      string                   `json:"artifactUID"`
	SourceGeneration int64                    `json:"sourceGeneration"`
	ManifestSHA256   string                   `json:"manifestSHA256"`
	ManifestJSON     string                   `json:"manifestJSON"`
	SourcePodUID     string                   `json:"sourcePodUID"`
	WorkerInstanceID string                   `json:"workerInstanceID"`
}

// SessionView answers one admitted session's descriptor request. The
// caller must already hold a session admitted through AdmitSession under
// its own authenticated identity; a session ID alone never promotes a
// caller to see another generation's memory, and a generation whose
// lease has lapsed serves nothing.
func (b *Broker) SessionView(sessionID string) (*SessionDetails, *protocol.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.sessions[sessionID]
	if !ok {
		return nil, protocol.New(protocol.KindAuthentication, "session",
			"the session id is unknown")
	}
	if record.Phase != "" && record.Phase != "admitted" {
		return nil, protocol.New(protocol.KindOperationState, "session", "the session is not admitted")
	}
	generation := record.Generation
	rec, ok := b.records[generation]
	if !ok || rec.State != StateServing || len(rec.Descriptors) == 0 {
		return nil, protocol.New(protocol.KindOperationState, "session",
			"the generation is not serving registered memory under a live lease")
	}
	if rec.LeaseID == "" || !b.now().Before(rec.LeaseExpires) {
		return nil, protocol.New(protocol.KindOperationState, "session",
			"the generation's worker lease has lapsed")
	}
	return &SessionDetails{
		Generation:  generation,
		Digest:      rec.Digest,
		Descriptors: rec.Descriptors,
		ContractVer: protocol.ContractVersion,
		ArtifactUID: rec.ArtifactUID, SourceGeneration: rec.SourceGeneration,
		ManifestSHA256: rec.ManifestSHA256, ManifestJSON: rec.ManifestJSON,
		SourcePodUID: rec.WorkerID, WorkerInstanceID: rec.NativeInstance,
	}, nil
}

// GenerationFor resolves the opaque generation currently serving one
// namespace's digest. Only a Serving generation under a live worker
// lease answers; anything else is stale to a session.
func (b *Broker) GenerationFor(namespace, digest string) (string, *protocol.Error) {
	return b.GenerationForArtifact(namespace, digest, "", 0)
}

// GenerationForArtifact resolves the exact artifact identity held by a source.
func (b *Broker) GenerationForArtifact(namespace, digest, artifactUID string, sourceGeneration int64) (string, *protocol.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.Namespace != namespace || rec.Digest != digest {
			continue
		}
		if artifactUID != "" && (rec.ArtifactUID != artifactUID || rec.SourceGeneration != sourceGeneration) {
			continue
		}
		if rec.State != StateServing {
			continue
		}
		if rec.LeaseID != "" && b.now().Before(rec.LeaseExpires) {
			return rec.Generation, nil
		}
	}
	return "", protocol.New(protocol.KindOperationState, "session",
		"no serving generation holds this digest in this namespace")
}

// sessionsFile persists every admitted session's owner and generation, so
// a broker restart reconstructs the exact accounting instead of freeing
// capacity: a restart proves nothing about engines or DMA.
func (b *Broker) sessionsFile() string {
	return filepath.Join(b.root, "sessions.json")
}

// persistSessionsLocked writes the session map atomically and synced.
func (b *Broker) persistSessionsLocked() error {
	raw, err := json.Marshal(b.sessions)
	if err != nil {
		return err
	}
	return writeSynced(b.sessionsFile(), raw)
}

// loadSessions restores the persisted sessions. A corrupt or unreadable
// file is an error, never an empty set: an empty set would free every
// session's capacity on restart.
func (b *Broker) loadSessions() (map[string]*SessionRecord, error) {
	raw, err := os.ReadFile(b.sessionsFile())
	if os.IsNotExist(err) {
		return map[string]*SessionRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the session ledger: %w", err)
	}
	sessions := map[string]*SessionRecord{}
	if err := decodeStrictDocument(raw, &sessions); err != nil || sessions == nil || len(sessions) > 4096 {
		return nil, fmt.Errorf("the session ledger is corrupt")
	}
	for id, session := range sessions {
		if id == "" || session == nil || session.OwnerUID == "" || session.Namespace == "" {
			return nil, fmt.Errorf("the session ledger contains an invalid owner")
		}
	}
	return sessions, nil
}
