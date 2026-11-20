package runtimecontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	// DefaultMaxLedgerBytes reserves headroom below the Kubernetes 1 MiB ConfigMap limit.
	DefaultMaxLedgerBytes = 960 * 1024
	// LedgerDataKey is the ConfigMap data key storing the serialized role ledger.
	LedgerDataKey = "ledger.json"
	// LedgerLabelKey identifies role-sharded runtime-control ledger ConfigMaps.
	LedgerLabelKey = "modeldeployment.gpustack.ai/runtime-control-ledger"

	ledgerNamePrefix = "runtime-control-"
	dns1123MaxLen    = 63
	roleHashHexLen   = 8
)

var (
	// ErrLedgerOwnerMismatch reports that a ledger ConfigMap is not owned by the expected ModelDeployment.
	ErrLedgerOwnerMismatch = errors.New("runtime-control ledger owner mismatch")
	// ErrLedgerConflict reports an optimistic-concurrency conflict while saving a ledger.
	ErrLedgerConflict = errors.New("runtime-control ledger conflict")
	// ErrLedgerStaleGeneration reports an attempted write from an older deployment generation.
	ErrLedgerStaleGeneration = errors.New("runtime-control ledger stale generation")
	// ErrLedgerSizeExceeded reports that the serialized ledger exceeds the ConfigMap size bound.
	ErrLedgerSizeExceeded = errors.New("runtime-control ledger size exceeded")
	// ErrLedgerCorrupt reports unreadable or structurally invalid ledger JSON.
	ErrLedgerCorrupt = errors.New("runtime-control ledger corrupt")
)

// IsLedgerConflict reports whether err is a ledger CAS or create conflict.
func IsLedgerConflict(err error) bool {
	return errors.Is(err, ErrLedgerConflict) || apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

// LedgerConfigMapName returns a deterministic DNS-1123 ConfigMap name (at most 63 chars)
// sharded by ModelDeployment UID and role name.
func LedgerConfigMapName(deploymentUID types.UID, roleName string) string {
	roleSum := sha256.Sum256([]byte(roleName))
	roleHash := hex.EncodeToString(roleSum[:])[:roleHashHexLen]

	sanitizedUID := sanitizeDNS1123Segment(string(deploymentUID))
	candidate := ledgerNamePrefix + sanitizedUID + "-" + roleHash
	if sanitizedUID != "" && len(candidate) <= dns1123MaxLen {
		return candidate
	}

	// Fall back to a deterministic combined hash when UID exceeds standard UUID length.
	combinedSum := sha256.Sum256([]byte(string(deploymentUID) + "\x00" + roleName))
	combinedHash := hex.EncodeToString(combinedSum[:])[:16]
	maxUIDLen := dns1123MaxLen - len(ledgerNamePrefix) - 1 - len(combinedHash)
	if sanitizedUID == "" {
		return ledgerNamePrefix + combinedHash
	}
	trimmed := strings.TrimRight(sanitizedUID[:min(len(sanitizedUID), maxUIDLen)], "-")
	if trimmed == "" {
		return ledgerNamePrefix + combinedHash
	}
	return ledgerNamePrefix + trimmed + "-" + combinedHash
}

func sanitizeDNS1123Segment(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(lower))
	for i := 0; i < len(lower); i++ {
		ch := lower[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			b.WriteByte(ch)
		}
	}
	return strings.Trim(b.String(), "-")
}

// LedgerOwner identifies the ModelDeployment that owns a role-sharded ledger ConfigMap.
type LedgerOwner struct {
	Namespace  string
	Name       string
	UID        types.UID
	Generation int64
}

// LedgerOwnerFromModelDeployment extracts a LedgerOwner from a ModelDeployment object.
func LedgerOwnerFromModelDeployment(md *workercore.ModelDeployment) LedgerOwner {
	if md == nil {
		return LedgerOwner{}
	}
	return LedgerOwner{
		Namespace:  md.Namespace,
		Name:       md.Name,
		UID:        md.UID,
		Generation: md.Generation,
	}
}

func (o LedgerOwner) validate() error {
	if strings.TrimSpace(o.Namespace) == "" || strings.TrimSpace(o.Name) == "" || o.UID == "" {
		return fmt.Errorf("%w: namespace, name, and uid are required", ErrInvalidArgument)
	}
	return nil
}

// PodLedgerEntry records durable runtime-control convergence and in-flight intent for one Pod.
type PodLedgerEntry struct {
	PodUID                 types.UID   `json:"podUID"`
	BootID                 string      `json:"bootID,omitempty"`
	RestartCount           int32       `json:"restartCount"`
	ConfirmedAppliedDigest string      `json:"confirmedAppliedDigest,omitempty"`
	ObservedGeneration     int64       `json:"observedGeneration,omitempty"`
	PendingOperationID     string      `json:"pendingOperationID,omitempty"`
	PendingTargetState     string      `json:"pendingTargetState,omitempty"`
	AmbiguousOperationID   string      `json:"ambiguousOperationID,omitempty"`
	AmbiguousReason        string      `json:"ambiguousReason,omitempty"`
	AppliedKeys            []string    `json:"appliedKeys,omitempty"`
	Paused                 bool        `json:"paused,omitempty"`
	UpdatedAt              metav1.Time `json:"updatedAt,omitempty"`
}

// RoleLedger is the in-memory representation of one role-sharded ledger ConfigMap.
type RoleLedger struct {
	Namespace       string                    `json:"namespace"`
	DeploymentName  string                    `json:"deploymentName"`
	DeploymentUID   types.UID                 `json:"deploymentUID"`
	RoleName        string                    `json:"roleName"`
	Generation      int64                     `json:"generation,omitempty"`
	ObjectUID       types.UID                 `json:"-"`
	ResourceVersion string                    `json:"-"`
	Entries         map[string]PodLedgerEntry `json:"entries"`
}

// DeepCopy returns an isolated copy of the RoleLedger so failed writes never taint in-memory state.
func (l *RoleLedger) DeepCopy() *RoleLedger {
	if l == nil {
		return nil
	}
	cp := *l
	if l.Entries != nil {
		cp.Entries = make(map[string]PodLedgerEntry, len(l.Entries))
		for k, v := range l.Entries {
			if len(v.AppliedKeys) > 0 {
				v.AppliedKeys = append([]string(nil), v.AppliedKeys...)
			}
			cp.Entries[k] = v
		}
	}
	return &cp
}

func (l *RoleLedger) ensureEntries() {
	if l.Entries == nil {
		l.Entries = make(map[string]PodLedgerEntry)
	}
}

// GetEntry returns the stored entry for podName if present.
func (l *RoleLedger) GetEntry(podName string) (PodLedgerEntry, bool) {
	if l == nil || l.Entries == nil {
		return PodLedgerEntry{}, false
	}
	entry, ok := l.Entries[podName]
	if ok && len(entry.AppliedKeys) > 0 {
		entry.AppliedKeys = append([]string(nil), entry.AppliedKeys...)
	}
	return entry, ok
}

// SetEntry stores entry under podName with canonicalized applied-key ordering.
func (l *RoleLedger) SetEntry(podName string, entry PodLedgerEntry) {
	l.ensureEntries()
	entry.AppliedKeys = normalizeStringList(entry.AppliedKeys)
	l.Entries[podName] = entry
}

// DeleteEntry removes podName from the ledger.
func (l *RoleLedger) DeleteEntry(podName string) {
	if l == nil || l.Entries == nil {
		return
	}
	delete(l.Entries, podName)
}

// RecordPendingOperation persists mutating intent for podName without altering ConfirmedAppliedDigest.
func (l *RoleLedger) RecordPendingOperation(
	podName string,
	podUID types.UID,
	bootID string,
	restartCount int32,
	opID string,
	targetState string,
	now time.Time,
) {
	l.ensureEntries()
	entry := l.Entries[podName]
	if entry.PodUID != "" && entry.PodUID != podUID {
		entry = PodLedgerEntry{}
	}
	entry.PodUID = podUID
	entry.BootID = bootID
	entry.RestartCount = restartCount
	entry.PendingOperationID = opID
	entry.PendingTargetState = targetState
	entry.UpdatedAt = metav1.NewTime(now.UTC())
	l.Entries[podName] = entry
}

// RecordConfirmedApply records confirmed convergence for podName and clears pending/ambiguous markers.
func (l *RoleLedger) RecordConfirmedApply(
	podName string,
	podUID types.UID,
	bootID string,
	restartCount int32,
	digest string,
	generation int64,
	appliedKeys []string,
	paused bool,
	now time.Time,
) {
	l.ensureEntries()
	entry := l.Entries[podName]
	if entry.PodUID != "" && entry.PodUID != podUID {
		entry = PodLedgerEntry{}
	}
	entry.PodUID = podUID
	entry.BootID = bootID
	entry.RestartCount = restartCount
	entry.ConfirmedAppliedDigest = digest
	entry.ObservedGeneration = generation
	entry.PendingOperationID = ""
	entry.PendingTargetState = ""
	entry.AmbiguousOperationID = ""
	entry.AmbiguousReason = ""
	entry.AppliedKeys = normalizeStringList(appliedKeys)
	entry.Paused = paused
	entry.UpdatedAt = metav1.NewTime(now.UTC())
	l.Entries[podName] = entry
}

// RecordAmbiguousOperation fences an unresolved operation without altering ConfirmedAppliedDigest.
func (l *RoleLedger) RecordAmbiguousOperation(
	podName string,
	podUID types.UID,
	bootID string,
	restartCount int32,
	opID string,
	reason string,
	now time.Time,
) {
	l.ensureEntries()
	entry := l.Entries[podName]
	if entry.PodUID != "" && entry.PodUID != podUID {
		entry = PodLedgerEntry{}
	}
	entry.PodUID = podUID
	entry.BootID = bootID
	entry.RestartCount = restartCount
	entry.AmbiguousOperationID = opID
	entry.AmbiguousReason = reason
	entry.UpdatedAt = metav1.NewTime(now.UTC())
	l.Entries[podName] = entry
}

// Compact deletes entries whose Pod name is absent from livePods or whose PodUID has changed.
func (l *RoleLedger) Compact(livePods map[string]types.UID) int {
	if l == nil || len(l.Entries) == 0 {
		return 0
	}
	removed := 0
	for podName, entry := range l.Entries {
		liveUID, exists := livePods[podName]
		if !exists || liveUID != entry.PodUID {
			delete(l.Entries, podName)
			removed++
		}
	}
	return removed
}

func normalizeStringList(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// LedgerStoreOption configures a LedgerStore.
type LedgerStoreOption func(*LedgerStore)

// WithMaxLedgerBytes overrides the serialized ConfigMap payload limit when positive.
func WithMaxLedgerBytes(limit int) LedgerStoreOption {
	return func(s *LedgerStore) {
		if limit > 0 {
			s.maxBytes = limit
		}
	}
}

// WithLedgerClock overrides the clock used for ledger timestamps in tests.
func WithLedgerClock(now func() time.Time) LedgerStoreOption {
	return func(s *LedgerStore) {
		if now != nil {
			s.now = now
		}
	}
}

// LedgerStore manages role-sharded ConfigMap ledgers using an uncached APIReader for reads
// and optimistic-concurrency writes through a controller-runtime Client.
type LedgerStore struct {
	reader   ctrlcli.Reader
	writer   ctrlcli.Client
	maxBytes int
	now      func() time.Time
}

// NewLedgerStore builds a LedgerStore backed by an uncached reader and a writer client.
func NewLedgerStore(reader ctrlcli.Reader, writer ctrlcli.Client, opts ...LedgerStoreOption) *LedgerStore {
	s := &LedgerStore{
		reader:   reader,
		writer:   writer,
		maxBytes: DefaultMaxLedgerBytes,
		now:      time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Read loads a role ledger directly from the uncached reader and verifies ownership.
// When the ConfigMap does not exist, it returns an empty initialized RoleLedger.
func (s *LedgerStore) Read(ctx context.Context, owner LedgerOwner, roleName string) (*RoleLedger, error) {
	if s.reader == nil {
		return nil, fmt.Errorf("%w: uncached reader is nil", ErrInvalidArgument)
	}
	if err := owner.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(roleName) == "" {
		return nil, fmt.Errorf("%w: roleName is required", ErrInvalidArgument)
	}

	cmName := LedgerConfigMapName(owner.UID, roleName)
	stored := new(corev1.ConfigMap)
	err := s.reader.Get(ctx, ctrlcli.ObjectKey{Namespace: owner.Namespace, Name: cmName}, stored)
	if apierrors.IsNotFound(err) {
		return &RoleLedger{
			Namespace:      owner.Namespace,
			DeploymentName: owner.Name,
			DeploymentUID:  owner.UID,
			RoleName:       roleName,
			Generation:     owner.Generation,
			Entries:        make(map[string]PodLedgerEntry),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime-control ledger %s/%s: %w", owner.Namespace, cmName, err)
	}

	return decodeVerifiedLedger(stored, owner, roleName)
}

// Save creates or CAS-updates the role-sharded ConfigMap ledger with owner, generation, and size guards.
func (s *LedgerStore) Save(ctx context.Context, owner LedgerOwner, ledger *RoleLedger) error {
	if s.reader == nil || s.writer == nil {
		return fmt.Errorf("%w: reader and writer clients are required", ErrInvalidArgument)
	}
	if err := owner.validate(); err != nil {
		return err
	}
	if ledger == nil || strings.TrimSpace(ledger.RoleName) == "" {
		return fmt.Errorf("%w: ledger and roleName are required", ErrInvalidArgument)
	}
	if ledger.DeploymentUID != "" && ledger.DeploymentUID != owner.UID {
		return fmt.Errorf("%w: ledger uid %s does not match owner %s", ErrLedgerOwnerMismatch, ledger.DeploymentUID, owner.UID)
	}

	toStore := RoleLedger{
		Namespace:      owner.Namespace,
		DeploymentName: owner.Name,
		DeploymentUID:  owner.UID,
		RoleName:       ledger.RoleName,
		Generation:     max(owner.Generation, ledger.Generation),
		Entries:        make(map[string]PodLedgerEntry, len(ledger.Entries)),
	}
	for podName, entry := range ledger.Entries {
		entry.AppliedKeys = normalizeStringList(entry.AppliedKeys)
		toStore.Entries[podName] = entry
	}

	encoded, err := json.Marshal(toStore)
	if err != nil {
		return fmt.Errorf("marshal runtime-control ledger: %w", err)
	}
	if len(encoded) > s.maxBytes {
		return fmt.Errorf("%w: encoded ledger %d bytes exceeds limit %d", ErrLedgerSizeExceeded, len(encoded), s.maxBytes)
	}

	cmName := LedgerConfigMapName(owner.UID, ledger.RoleName)
	if ledger.ResourceVersion == "" {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: owner.Namespace,
				Labels: map[string]string{
					LedgerLabelKey: "true",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         workercore.SchemeGroupVersion.String(),
					Kind:               "ModelDeployment",
					Name:               owner.Name,
					UID:                owner.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				}},
			},
			Data: map[string]string{LedgerDataKey: string(encoded)},
		}
		if createErr := s.writer.Create(ctx, cm); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) || apierrors.IsConflict(createErr) {
				return fmt.Errorf("%w: %w", ErrLedgerConflict, createErr)
			}
			return fmt.Errorf("create runtime-control ledger %s/%s: %w", owner.Namespace, cmName, createErr)
		}
		ledger.Namespace = owner.Namespace
		ledger.DeploymentName = owner.Name
		ledger.DeploymentUID = owner.UID
		ledger.Generation = toStore.Generation
		ledger.ObjectUID = cm.UID
		ledger.ResourceVersion = cm.ResourceVersion
		return nil
	}

	stored := new(corev1.ConfigMap)
	if err := s.reader.Get(ctx, ctrlcli.ObjectKey{Namespace: owner.Namespace, Name: cmName}, stored); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: ledger %s/%s was deleted concurrently", ErrLedgerConflict, owner.Namespace, cmName)
		}
		return fmt.Errorf("verify runtime-control ledger %s/%s before update: %w", owner.Namespace, cmName, err)
	}

	existing, err := decodeVerifiedLedger(stored, owner, ledger.RoleName)
	if err != nil {
		return err
	}
	if stored.ResourceVersion != ledger.ResourceVersion {
		return fmt.Errorf(
			"%w: resourceVersion moved from %s to %s",
			ErrLedgerConflict, ledger.ResourceVersion, stored.ResourceVersion,
		)
	}
	if owner.Generation > 0 && existing.Generation > owner.Generation {
		return fmt.Errorf(
			"%w: owner generation %d is behind stored generation %d",
			ErrLedgerStaleGeneration, owner.Generation, existing.Generation,
		)
	}

	updated := stored.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = make(map[string]string, 1)
	}
	updated.Labels[LedgerLabelKey] = "true"
	if updated.Data == nil {
		updated.Data = make(map[string]string, 1)
	}
	updated.Data[LedgerDataKey] = string(encoded)

	if err := s.writer.Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: %w", ErrLedgerConflict, err)
		}
		return fmt.Errorf("update runtime-control ledger %s/%s: %w", owner.Namespace, cmName, err)
	}
	ledger.Generation = toStore.Generation
	ledger.ResourceVersion = updated.ResourceVersion
	return nil
}

// Delete removes a role-sharded ledger ConfigMap after verifying ownership.
// Delete removes the role-sharded ledger ConfigMap if owned by owner.
func (s *LedgerStore) Delete(ctx context.Context, owner LedgerOwner, roleName string) error {
	return s.DeleteIfMatch(ctx, owner, roleName, "", "")
}

// DeleteIfMatch removes the role-sharded ledger ConfigMap only if it is owned by owner and still
// matches expectedUID/expectedResourceVersion when supplied.
func (s *LedgerStore) DeleteIfMatch(
	ctx context.Context,
	owner LedgerOwner,
	roleName string,
	expectedUID types.UID,
	expectedResourceVersion string,
) error {
	if s.reader == nil || s.writer == nil {
		return fmt.Errorf("%w: reader and writer clients are required", ErrInvalidArgument)
	}
	if err := owner.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(roleName) == "" {
		return fmt.Errorf("%w: roleName is required", ErrInvalidArgument)
	}

	cmName := LedgerConfigMapName(owner.UID, roleName)
	stored := new(corev1.ConfigMap)
	err := s.reader.Get(ctx, ctrlcli.ObjectKey{Namespace: owner.Namespace, Name: cmName}, stored)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runtime-control ledger %s/%s before delete: %w", owner.Namespace, cmName, err)
	}
	if err := verifyLedgerOwner(stored, owner); err != nil {
		return err
	}
	if expectedUID != "" && stored.UID != "" && stored.UID != expectedUID {
		return fmt.Errorf(
			"%w: runtime-control ledger %s/%s UID changed (%q != %q)",
			ErrLedgerConflict, owner.Namespace, cmName, stored.UID, expectedUID,
		)
	}
	if expectedResourceVersion != "" && stored.ResourceVersion != expectedResourceVersion {
		return fmt.Errorf(
			"%w: runtime-control ledger %s/%s resourceVersion changed (%q != %q)",
			ErrLedgerConflict, owner.Namespace, cmName, stored.ResourceVersion, expectedResourceVersion,
		)
	}
	var deleteOpts []ctrlcli.DeleteOption
	precond := &metav1.Preconditions{}
	if stored.UID != "" {
		uid := stored.UID
		precond.UID = &uid
	}
	if stored.ResourceVersion != "" {
		rv := stored.ResourceVersion
		precond.ResourceVersion = &rv
	}
	if precond.UID != nil || precond.ResourceVersion != nil {
		deleteOpts = append(deleteOpts, &ctrlcli.DeleteOptions{Preconditions: precond})
	}
	if err := s.writer.Delete(ctx, stored, deleteOpts...); err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: delete runtime-control ledger %s/%s: %w", ErrLedgerConflict, owner.Namespace, cmName, err)
		}
		return fmt.Errorf("delete runtime-control ledger %s/%s: %w", owner.Namespace, cmName, err)
	}
	return nil
}

func decodeVerifiedLedger(stored *corev1.ConfigMap, owner LedgerOwner, roleName string) (*RoleLedger, error) {
	if err := verifyLedgerOwner(stored, owner); err != nil {
		return nil, err
	}
	raw, ok := stored.Data[LedgerDataKey]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: missing %s data key", ErrLedgerCorrupt, LedgerDataKey)
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc RoleLedger
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLedgerCorrupt, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON payload", ErrLedgerCorrupt)
	}

	if doc.DeploymentUID != owner.UID || doc.DeploymentName != owner.Name || doc.RoleName != roleName {
		return nil, fmt.Errorf(
			"%w: document (%s/%s, role=%s) does not match expected (%s/%s, role=%s)",
			ErrLedgerOwnerMismatch,
			doc.DeploymentName, doc.DeploymentUID, doc.RoleName,
			owner.Name, owner.UID, roleName,
		)
	}
	doc.Namespace = stored.Namespace
	doc.ObjectUID = stored.UID
	doc.ResourceVersion = stored.ResourceVersion
	doc.ensureEntries()
	return &doc, nil
}

func verifyLedgerOwner(stored *corev1.ConfigMap, owner LedgerOwner) error {
	ctrl := metav1.GetControllerOf(stored)
	if ctrl == nil {
		return fmt.Errorf("%w: ConfigMap %s/%s has no controlling owner", ErrLedgerOwnerMismatch, stored.Namespace, stored.Name)
	}
	if ctrl.Kind != "ModelDeployment" || ctrl.APIVersion != workercore.SchemeGroupVersion.String() {
		return fmt.Errorf(
			"%w: ConfigMap %s/%s controlled by %s/%s",
			ErrLedgerOwnerMismatch, stored.Namespace, stored.Name, ctrl.APIVersion, ctrl.Kind,
		)
	}
	if ctrl.UID != owner.UID || ctrl.Name != owner.Name {
		return fmt.Errorf(
			"%w: ConfigMap %s/%s owned by %s (%s) rather than %s (%s)",
			ErrLedgerOwnerMismatch, stored.Namespace, stored.Name, ctrl.Name, ctrl.UID, owner.Name, owner.UID,
		)
	}
	return nil
}
