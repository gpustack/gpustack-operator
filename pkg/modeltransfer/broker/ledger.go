// Package broker is the node-local authority for source generations: it
// reserves physical and namespace memory atomically before any fill, binds
// every worker, lease, and session to one immutable generation, tracks drain
// and quarantine, and reconstructs its state from a durable on-disk ledger
// after a crash.
//
// The ledger is the node's own record, never an API object: it may name the
// namespace and source-worker Pod a reservation belongs to, and its 0600
// record files hold the admission secret a restart must re-offer to the
// same node's worker — credentials and pointers never enter public status,
// logs, or anything that leaves the node.
package broker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	recordsDir    = "generations"
	counterFile   = "next-generation"
	identityFile  = "identity"
	sessionsFile  = "sessions.json"
	layoutVersion = "gpustack-model-transfer-ledger v1"
	// maxGenerationLen is the API bound for a generation token; tokens are
	// opaque beyond their shape and never parsed for meaning.
	maxGenerationLen = 128
)

// The lifecycle states of one generation. Filling and Released are internal:
// a filling generation holds a reservation but nothing registered yet, and a
// released generation's record is gone. Serving, Draining, and Quarantined
// are the reported states.
const (
	StateFilling     = "Filling"
	StateServing     = "Serving"
	StateDraining    = "Draining"
	StateQuarantined = "Quarantined"
	StateReleased    = "Released"
)

var (
	// legacyGenerationPattern accepts the pre-scope numeric tokens so an
	// old ledger still loads; no new token is minted in that shape.
	legacyGenerationPattern = regexp.MustCompile(`^[1-9][0-9]{0,127}$`)
	// scopedGenerationPattern accepts g-<scope>-<identity>-<counter>: the
	// scope and the per-ledger identity make a token globally unique
	// across nodes, ledger recreations, and vendors. The token is opaque
	// to every caller; only the ledger reads its parts.
	scopedGenerationPattern = regexp.MustCompile(
		`^g-[a-z0-9][a-z0-9-]{0,62}-[0-9a-f]{32}-[1-9][0-9]{0,18}$`,
	)
	// scopePattern bounds the vendor scope a broker may name.
	scopePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// hexPattern validates a minted ledger identity.
	hexPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// validGeneration accepts the scoped tokens this ledger mints and the
// legacy numeric records it must keep loading, within the API bound.
func validGeneration(generation string) bool {
	if len(generation) == 0 || len(generation) > maxGenerationLen {
		return false
	}
	return legacyGenerationPattern.MatchString(generation) ||
		scopedGenerationPattern.MatchString(generation)
}

// GenerationRecord is one generation's durable ledger entry. It is the
// node's own on-disk record: it may name the namespace and worker a
// reservation belongs to, and — in its node-private 0600 file — the
// admission secret a restart must re-offer.
type GenerationRecord struct {
	Generation string `json:"generation"`
	Digest     string `json:"digest"`
	Namespace  string `json:"namespace"`
	WorkerID   string `json:"workerID"`
	NodeName   string `json:"nodeName,omitempty"`
	// WorkerSecretHash is the hex sha256 of the worker secret, used for
	// constant-time message checks.
	WorkerSecretHash string `json:"workerSecretHash"`
	// WorkerSecret is the admission secret itself, persisted so a broker
	// restart re-offers the same admission instead of minting a duplicate
	// generation for one intent. The record file is node-private (0600)
	// and never leaves the node; nothing derived from it reaches status
	// or logs.
	WorkerSecret string `json:"workerSecret,omitempty"`
	// IntentKey is the source intent revision this admission belongs to,
	// the crash-idempotence key: the record and its assignment are one
	// durable transaction.
	IntentKey        string `json:"intentKey,omitempty"`
	IntentUID        string `json:"intentUID,omitempty"`
	IntentName       string `json:"intentName,omitempty"`
	IntentGeneration int64  `json:"intentGeneration,omitempty"`
	ArtifactName     string `json:"artifactName,omitempty"`
	State            string `json:"state"`
	ReservedBytes    int64  `json:"reservedBytes"`
	ResidentBytes    int64  `json:"residentBytes"`
	Registrations    int32  `json:"registrations"`
	ActiveSessions   int32  `json:"activeSessions"`
	// AdmissionPhase walks prepared → reserved → admitted for intent
	// admissions; only an admitted record exposes an assignment. Legacy
	// (non-intent) records carry the empty phase.
	AdmissionPhase string `json:"admissionPhase,omitempty"`
	// ReservationID and the Binding identity are the prepared record's
	// durable link to its Binding-wide reservation, persisted before the
	// global CAS claim.
	ReservationID       string `json:"reservationID,omitempty"`
	BindingUID          string `json:"bindingUID,omitempty"`
	BindingNamespace    string `json:"bindingNamespace,omitempty"`
	BindingName         string `json:"bindingName,omitempty"`
	AuthorityUID        string `json:"authorityUID,omitempty"`
	AuthorityEpoch      string `json:"authorityEpoch,omitempty"`
	NativeReleased      bool   `json:"nativeReleased,omitempty"`
	NativeStartEnforced bool   `json:"nativeStartEnforced,omitempty"`
	AuthorityDrained    bool   `json:"authorityDrained,omitempty"`
	// DiagnosticStage is the first-observed bounded HIXL stage name
	// (connect, submit, poll, disconnect) carried in a quarantine report's
	// reason. It is a generation-level hint about where an attempt failed:
	// never evidence of submission, terminality, or DMA, and never a fence.
	// An older binary rewriting the ledger drops this field.
	DiagnosticStage string `json:"diagnosticStage,omitempty"`
	// UnknownSessions is the session charge whose owner the broker cannot
	// name — a restart is not evidence engines stopped, so only a
	// qualified fence clears it (ClearUnknownSessions).
	UnknownSessions    int32     `json:"unknownSessions,omitempty"`
	RejectedAdmissions int64     `json:"rejectedAdmissions"`
	Grant              Grant     `json:"grant"`
	LeaseID            string    `json:"leaseID,omitempty"`
	LeaseExpires       time.Time `json:"leaseExpires,omitempty"`
	// Descriptors are the worker's native registrations, reported only
	// after real backend registration succeeded; they persist so a broker
	// restart never loses what a live worker still holds.
	Descriptors []RegistrationDescriptor `json:"descriptors,omitempty"`
	// ManifestPath is the node-local verified manifest file the broker
	// wrote at admission, in the portable wire form the assignment carries;
	// registrations are validated against its files.
	ManifestPath string `json:"manifestPath,omitempty"`
	// The assignment's identity facts, resolved control-plane side at
	// admission: they re-offer exactly after a restart.
	ArtifactUID      string `json:"artifactUID,omitempty"`
	SourceGeneration int64  `json:"sourceGeneration,omitempty"`
	ManifestSHA256   string `json:"manifestSHA256,omitempty"`
	ManifestJSON     string `json:"manifestJSON,omitempty"`
	// ManifestPagesPath is a private directory of authenticated immutable pages.
	ManifestPagesPath string `json:"manifestPagesPath,omitempty"`
	ManifestStored    bool   `json:"manifestStored,omitempty"`
	// TreePath is the published tree the manifest verified against; a
	// node-local cache path.
	TreePath string `json:"treePath,omitempty"`
	// TotalBytes is the manifest's full file extent, and Registrations the
	// manifest's nonempty-file count; both are the admission the worker
	// was handed, distinct from ReservedBytes, which adds the node's own
	// overhead.
	TotalBytes int64 `json:"totalBytes,omitempty"`
	// ResidentBudgetBytes is the immutable admitted pinned-memory ceiling.
	// Zero preserves legacy full-residency records. ResidentBytes is observed.
	ResidentBudgetBytes int64  `json:"residentBudgetBytes,omitempty"`
	Backend             string `json:"backend,omitempty"`
	// WorkerInstanceID is the native worker boot this record is bound to: a
	// fresh random value per Python boot, carried by every control message.
	// A new instance reattaches only while provably pre-native.
	WorkerInstanceID string `json:"workerInstanceID,omitempty"`
	// NativeStarted and NativeInstance are the durable fence around native
	// work: set durably BEFORE the start answer is sent, so a lost answer
	// replays onto a record that already knows. Once true, a different
	// instance cannot reattach — the old native holder's charges stay
	// until a qualified fence.
	NativeStarted         bool      `json:"nativeStarted,omitempty"`
	NativeInstance        string    `json:"nativeInstance,omitempty"`
	Created               time.Time `json:"created"`
	Updated               time.Time `json:"updated"`
	quarantineUnpersisted bool
}

// refVolumeID is the durable source reference this generation holds in the
// cache store's ledger, so the collector never removes a digest a pool fill,
// admitted generation, or active reader still uses. The store's stable
// `model-transfer.` pool prefix is kept exactly — the store's rebuild
// classifies references by that prefix — and the uniqueness (vendor scope,
// per-ledger identity, counter) rides after it, so two vendor brokers on
// one mixed node can never collide in one cache reference.
func refVolumeID(scope, generation string) string {
	return "model-transfer." + scope + "." + generation
}

type ledger struct {
	root     string
	scope    string
	identity string
}

// ledgerLayout is the root's versioned metadata record. It is JSON so a
// two-word version string and the identity survive parsing intact, and
// unknown or corrupt metadata is an error — never silently rewritten.
type ledgerLayout struct {
	LayoutVersion string `json:"layoutVersion"`
	Scope         string `json:"scope"`
	Identity      string `json:"identity"`
}

// openLedger validates the scope, loads or mints the ledger's durable
// identity, and refuses a root whose recorded identity or scope differs:
// a ledger that lost its identity would mint tokens another ledger could
// collide with, and a root reused by another scope is a deployment bug.
// The second answer reports whether this root is genuinely fresh (its
// identity was minted now), which is the only state in which the counter
// may be initialized.
func openLedger(root, scope string) (*ledger, bool, error) {
	if !scopePattern.MatchString(scope) {
		return nil, false, fmt.Errorf("vendor scope %q is not a valid ledger scope", scope)
	}
	if err := os.MkdirAll(filepath.Join(root, recordsDir), 0o755); err != nil {
		return nil, false, err
	}
	fresh, err := initializeLedger(root, scope)
	if err != nil {
		return nil, false, err
	}
	identityRaw, err := os.ReadFile(filepath.Join(root, identityFile))
	if err != nil {
		return nil, false, fmt.Errorf("read the ledger identity: %w", err)
	}
	identity := strings.TrimSpace(string(identityRaw))
	if len(identity) != 32 || !hexPattern.MatchString(identity) {
		return nil, false, fmt.Errorf("the ledger identity %q is corrupt", identity)
	}
	layoutPath := filepath.Join(root, "layout")
	layoutRaw, err := os.ReadFile(layoutPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, fmt.Errorf(
			"the ledger layout is missing while the identity exists: " +
				"an established ledger never reinvents its metadata",
		)
	case err != nil:
		return nil, false, fmt.Errorf("read the ledger layout: %w", err)
	default:
		var layout ledgerLayout
		if jsonErr := json.Unmarshal(layoutRaw, &layout); jsonErr != nil {
			return nil, false, fmt.Errorf("the ledger layout record is corrupt: %w", jsonErr)
		}
		if layout.LayoutVersion != layoutVersion {
			return nil, false, fmt.Errorf(
				"the ledger layout version %q is not %q", layout.LayoutVersion, layoutVersion,
			)
		}
		if layout.Scope != scope || layout.Identity != identity {
			return nil, false, fmt.Errorf(
				"the ledger belongs to scope %q identity %q, not scope %q: "+
					"reopening it under another identity would collide",
				layout.Scope, layout.Identity, scope,
			)
		}
	}
	return &ledger{root: root, scope: scope, identity: identity}, fresh, nil
}

// initCounter durably writes a fresh ledger's zero counter. Only a
// genuinely fresh root may call this: an established ledger's counter is
// never reinitialized, because its past tokens must stay unreusable even
// after every record was released.
func (l *ledger) initCounter() error {
	return writeSynced(filepath.Join(l.root, counterFile), []byte("0\n"))
}

// nextGeneration returns the next monotonic generation token, durably, so
// a released token is never handed out again. The counter is validated,
// never reset: a corrupt, negative, or overflowing counter is an error,
// and a counter missing on an established ledger means the node lost
// state it cannot reinvent.
func (l *ledger) nextGeneration() (string, error) {
	number, err := l.readCounter()
	if err != nil {
		return "", err
	}
	number++
	if err := writeSynced(filepath.Join(l.root, counterFile),
		[]byte(strconv.FormatInt(number, 10)+"\n")); err != nil {
		return "", err
	}
	token := "g-" + l.scope + "-" + l.identity + "-" + strconv.FormatInt(number, 10)
	if len(token) > maxGenerationLen {
		return "", fmt.Errorf("the generation token exceeds its API bound")
	}
	return token, nil
}

// ensureCounter validates an established ledger's counter the same way
// minting does: existence, parseability, and bounds. A genuinely fresh
// root initializes its own instead; a non-fresh root whose counter is
// missing or corrupt has lost state and must refuse, whatever the
// records currently say.
func (l *ledger) ensureCounter(fresh bool) error {
	if fresh {
		return l.initCounter()
	}
	_, err := l.readCounter()
	return err
}

// readCounter reads and fully validates the durable counter: existence,
// parseability, positivity, and headroom.
func (l *ledger) readCounter() (int64, error) {
	path := filepath.Join(l.root, counterFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf(
			"the generation counter is missing on an established ledger",
		)
	}
	if err != nil {
		return 0, fmt.Errorf("read the generation counter: %w", err)
	}
	number, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("the generation counter %q is corrupt: %w",
			string(raw), parseErr)
	}
	if number < 0 {
		return 0, fmt.Errorf("the generation counter %d is negative", number)
	}
	if number >= 1<<62 {
		return 0, fmt.Errorf("the generation counter %d is exhausted", number)
	}
	return number, nil
}

func (l *ledger) write(rec *GenerationRecord) error {
	stored := *rec
	if stored.ManifestStored {
		stored.ManifestJSON = ""
		stored.ManifestPagesPath = ""
	}
	b, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	path := filepath.Join(l.root, recordsDir, rec.Generation+".json")
	return writeSynced(path, b)
}

// remove syncs the directory after unlink before the caller releases its charge.
// A retry syncs even if the record is already absent.
func (l *ledger) remove(generation string) error {
	if err := os.Remove(filepath.Join(l.root, recordsDir, generation+".json")); err != nil &&
		!errors.Is(err, fs.ErrNotExist) {
		return err
	}
	recordsPath := filepath.Join(l.root, recordsDir)
	d, err := os.Open(recordsPath)
	if err != nil {
		return fmt.Errorf("open %s to sync a record removal: %w", recordsPath, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s after removing the record of %s: %w", recordsPath, generation, err)
	}
	return nil
}

// records reads every ledger entry, by generation.
func (l *ledger) records() ([]*GenerationRecord, error) {
	entries, err := os.ReadDir(filepath.Join(l.root, recordsDir))
	if errors.Is(err, fs.ErrNotExist) {
		// A root no broker has written yet holds no generation: that is an
		// empty ledger, not a broken one.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []*GenerationRecord
	for _, e := range entries {
		if !validGeneration(strings.TrimSuffix(e.Name(), ".json")) {
			continue
		}
		rec, err := l.read(strings.TrimSuffix(e.Name(), ".json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ledger record %s: %w", e.Name(), err)
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		return compareGenerations(records[i].Generation, records[j].Generation)
	})
	return records, nil
}

func compareGenerations(a, b string) bool {
	// Scoped tokens order by their trailing counter so issue order is
	// stable past nine generations; legacy numeric records keep numeric
	// order; anything else falls back to the token itself.
	na, oka := scopedCounter(a)
	nb, okb := scopedCounter(b)
	if oka && okb {
		return na < nb
	}
	na, erra := strconv.ParseInt(a, 10, 64)
	nb, errb := strconv.ParseInt(b, 10, 64)
	if erra == nil && errb == nil {
		return na < nb
	}
	return a < b
}

// scopedCounter reads the monotonic counter from a scoped token's last
// dash-separated field.
func scopedCounter(generation string) (int64, bool) {
	index := strings.LastIndexByte(generation, '-')
	if index < 0 {
		return 0, false
	}
	number, err := strconv.ParseInt(generation[index+1:], 10, 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func (l *ledger) read(generation string) (*GenerationRecord, error) {
	if !validGeneration(generation) {
		return nil, fmt.Errorf("generation %q is not a token", generation)
	}
	b, err := os.ReadFile(filepath.Join(l.root, recordsDir, generation+".json"))
	if err != nil {
		return nil, err
	}
	var rec GenerationRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if err := l.restoreManifest(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func writeSynced(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
