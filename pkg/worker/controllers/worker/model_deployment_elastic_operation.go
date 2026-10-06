package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The elastic resize operation is a durable record plus a pure decision function over it. The record
// exists because a resize cannot be recovered by asking the engine what it thinks is happening: a
// request that timed out, a gateway that answered 408 or 500, and a request that was never sent at
// all are the same three states to every observer, and answering them differently is how a resize
// gets replayed against an engine that has already applied it.
//
// So the command intent is written BEFORE the request leaves, and a record that says a command was
// sent is never evidence that it may be sent again. Reconstruction after a restart reads that
// record and the fresh facts, and derives the same next action it would have derived before.

// elasticOperationState is where the operation is, in the order the states can be reached.
//
// A record is written before anything is asked of the engine, and the intent to ask is part of the
// state rather than a side effect of doing it. The states after the one that carries the intent are
// therefore reached by observation, never by assuming the request landed.
type elasticOperationState string

const (
	elasticStateRecorded      elasticOperationState = "Recorded"
	elasticStateCommandSent   elasticOperationState = "CommandSent"
	elasticStateAwaitingWidth elasticOperationState = "AwaitingWidth"
	elasticStateNativeDone    elasticOperationState = "NativeComplete"
	elasticStateWithdrawn     elasticOperationState = "Withdrawn"
	elasticStateReleased      elasticOperationState = "Released"
	elasticStateCompleted     elasticOperationState = "Completed"
	elasticStateAbandoned     elasticOperationState = "Abandoned"
)

// elasticWidth is the expert-parallel width the operation is moving between. Both ends are positive
// because a width of zero is a different operation, not a resize.
type elasticWidth struct {
	Old    int `json:"old"`
	Target int `json:"target"`
}

// Upward reports whether this operation adds workers.
func (w elasticWidth) Upward() bool { return w.Target > w.Old }

// CapturedIdentity is one worker the operation captured, by name and by UID.
//
// THE UID IS CAPTIRED BECAUSE THE NAME IS NOT AN IDENTITY. A worker deleted and recreated under the
// same name is a different worker, and a retirement that looked it up by name would remove the
// replacement in the captured worker's place.
type elasticCapturedIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// elasticOperation is the durable record of one resize.
type elasticOperation struct {
	// Name is the record's own object name, which is derived from the deployment so a second
	// operation for the same deployment cannot be created beside it unnoticed.
	Name string `json:"name"`
	// DeploymentUID is the ModelDeployment this operation belongs to, and it is the owner the
	// stored object is required to carry. A record whose owner does not match is not this
	// deployment's record however it is named.
	DeploymentUID types.UID `json:"deploymentUID"`
	// Namespace is where the record is stored, which is the deployment's namespace.
	Namespace string `json:"namespace"`
	// Generation is the spec generation the operation was decided against. A deployment whose spec
	// has moved on is a different intent, and an operation still unresolved when it does is not
	// carried forward against the new one.
	Generation int64 `json:"generation"`
	// Width is where the operation came from and where it is going.
	Width elasticWidth `json:"width"`
	// Workers is the captured set this operation may act on. The master is named separately
	// because it is a worker that is never a retirement candidate.
	Workers []elasticCapturedIdentity         `json:"workers"`
	Master  elasticCapturedIdentity           `json:"master"`
	Head    elasticCapturedIdentity           `json:"head,omitempty"`
	Members []elasticCapturedIdentity         `json:"members,omitempty"`
	Release *modelDeploymentRetirementRelease `json:"release,omitempty"`
	State   elasticOperationState             `json:"state"`
	// CommandIntent is what this operation asked the engine to do, written before the ask. A
	// resumed or reconstructed record carries it, which is what stops the same command being
	// issued twice after an ambiguous answer.
	CommandIntent string `json:"commandIntent"`
	// CommandSent records that the intent left. It is set with the intent, so its presence means
	// the engine may already have it.
	CommandSent bool `json:"commandSent"`
	// ScaleAttempts counts the definite refusals the engine has answered with, which is the retry
	// bound's counter: the kernel abandons the operation once it names the bound. An attempt whose
	// answer never arrived is not counted — whether it landed is not known, so it consumes
	// nothing.
	ScaleAttempts int `json:"scaleAttempts,omitempty"`
	// ScaleLastError is the engine's own refusal text from the last definite answer. It is kept
	// until the command is accepted or the retry bound is spent, so the refusal survives the
	// passes that would otherwise overwrite it with a wait.
	ScaleLastError string `json:"scaleLastError,omitempty"`
	// ScaleLastFailedAt anchors the backoff: the next attempt is owed only after the schedule has
	// run from this moment. A nil means the last attempt is not owed a wait.
	ScaleLastFailedAt *time.Time `json:"scaleLastFailedAt,omitempty"`
	// ResourceVersion is the stored object's version, carried so an update is against the object
	// this record was read from rather than against whatever is current.
	ResourceVersion string `json:"resourceVersion"`
}

// SentState reports whether the record's own state says a command has left.
//
// THE STATE IS THE AUTHORITY AND THE BOOLEAN A COPY OF IT. A record reconstructed from its document
// carries both, and a writer that trusts the copy can be handed a record whose state says a command
// left while its boolean says it did not. The state is what was written before the request went, so
// the state is what is read.
//
// Abandoned is here because its command also left: the state ends the operation on the spent retry
// bound instead of re-deriving what a sent record awaits.
func (eo *elasticOperation) SentState() bool {
	switch eo.State {
	case elasticStateCommandSent, elasticStateAwaitingWidth, elasticStateNativeDone,
		elasticStateWithdrawn, elasticStateReleased, elasticStateCompleted, elasticStateAbandoned:
		return true
	default:
		return false
	}
}

// envelopeDrift names the part of a record an update is not allowed to move, or the empty string.
//
// THE WIDTHS, THE GENERATION AND THE CAPTURED IDENTITIES ARE WHAT THE OPERATION WAS DECIDED AGAINST.
// Changing any of them through an update would be a second operation wearing the first one's name, and
// a caller that could retarget a stored operation could aim a resize the cluster never admitted.
func (eo *elasticOperation) envelopeDrift(next *elasticOperation) string {
	if eo.Head != next.Head || !reflect.DeepEqual(eo.Members, next.Members) || !reflect.DeepEqual(eo.Release, next.Release) {
		return "the captured runtime and allocation identities cannot change"
	}
	if eo.DeploymentUID != next.DeploymentUID {
		return "the record cannot be moved to another deployment"
	}
	if eo.Generation != next.Generation {
		return "the record cannot be rebound to another spec generation"
	}
	if eo.Width != next.Width {
		return "the record cannot be retargeted to another width"
	}
	if len(eo.Workers) != len(next.Workers) {
		return "the record cannot capture a different number of workers"
	}
	for i := range eo.Workers {
		if eo.Workers[i] != next.Workers[i] {
			return "the record cannot capture a different worker identity"
		}
	}
	if eo.Master != next.Master {
		return "the record cannot capture a different master identity"
	}
	// THE INTENT IS MONOTONIC. A record that has recorded an intent may only keep it; clearing it is
	// how a replay would be authorized after the fact.
	if eo.CommandIntent != "" && next.CommandIntent != eo.CommandIntent {
		return "the record cannot change the command it already recorded"
	}
	if eo.CommandSent && !next.CommandSent {
		return "the record cannot forget that a command was sent"
	}

	return ""
}

// elasticOperationFromData decodes one stored document, refusing anything malformed.
func elasticOperationFromData(body string) (*elasticOperation, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the stored record is empty", errElasticOperation)
	}

	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	op := new(elasticOperation)
	if err := decoder.Decode(op); err != nil {
		return nil, fmt.Errorf("%w: %w", errElasticOperation, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: the stored record carries a trailing JSON value", errElasticOperation)
	}

	return op, nil
}

// elasticOperationLabel marks the stored object so it is findable and cannot be mistaken for an
// ordinary ConfigMap.
const elasticOperationLabel = "gpustack.ai/elastic-operation"

// elasticOperationDataKey is the single key the record is serialized under. One key means one
// document, so a second value in the same object is malformed rather than a second record.
const elasticOperationDataKey = "operation.json"

// elasticControllerRef is the single true this package hands an owner reference. It is a function
// rather than a shared variable so the value cannot be mutated by anything else in the package.
func elasticControllerRef() *bool {
	yes := true

	return &yes
}

// errElasticOperation is returned for every refusal of a record, so a caller can tell a record
// problem from a storage problem without matching on prose.
var errElasticOperation = errors.New("elastic resize operation is not usable")

// validate refuses a record that cannot describe an operation.
//
// IDENTITY AND WIDTHS ARE BOTH CHECKED because a record missing either one would decide something
// about a deployment it cannot name. An operation with no deployment is not a record anyone may act
// on, and a width that is not positive is not a resize.
func (eo *elasticOperation) validate() error {
	if eo.DeploymentUID == "" {
		return fmt.Errorf("%w: it names no deployment", errElasticOperation)
	}
	if eo.Generation <= 0 {
		return fmt.Errorf("%w: it is bound to no spec generation", errElasticOperation)
	}
	if eo.Width.Old <= 0 || eo.Width.Target <= 0 {
		return fmt.Errorf(
			"%w: widths %d to %d are not a resize", errElasticOperation, eo.Width.Old, eo.Width.Target)
	}
	if eo.Width.Old == eo.Width.Target {
		return fmt.Errorf("%w: the target width equals the old one", errElasticOperation)
	}
	if eo.State == "" {
		return fmt.Errorf("%w: it carries no state", errElasticOperation)
	}
	// A STATE THAT SAYS A COMMAND LEFT MAY NOT CARRY A BOOLEAN SAYING IT DID NOT. The two are the
	// same fact recorded twice, and a record that disagrees with itself is refused rather than
	// resolved in favor of the part that would authorize a replay.
	if eo.CommandSent && !eo.SentState() {
		return fmt.Errorf("%w: it says a command was sent in a state that has not", errElasticOperation)
	}
	if !eo.CommandSent && eo.SentState() {
		return fmt.Errorf("%w: it says no command was sent in a state that has", errElasticOperation)
	}
	if eo.Master.UID == "" || eo.Master.Name == "" {
		return fmt.Errorf("%w: it captured no master identity", errElasticOperation)
	}
	if eo.Name == "" {
		return fmt.Errorf("%w: it names no deployment", errElasticOperation)
	}
	if eo.Namespace == "" {
		return fmt.Errorf("%w: it names no namespace to store the record in", errElasticOperation)
	}
	for i, worker := range eo.Workers {
		if worker.UID == "" {
			return fmt.Errorf("%w: captured worker %d has no identity", errElasticOperation, i)
		}
		if worker.Name == "" {
			return fmt.Errorf("%w: captured worker %d has no name", errElasticOperation, i)
		}
		if worker.UID == eo.Master.UID {
			return fmt.Errorf("%w: the master is also captured as retirable", errElasticOperation)
		}
	}

	return nil
}

// retractableWorkers is the captured set this operation may ever remove, which is the captured set
// minus the master.
func (eo *elasticOperation) retractableWorkers() []elasticCapturedIdentity {
	retractable := make([]elasticCapturedIdentity, 0, len(eo.Workers))
	for _, worker := range eo.Workers {
		if worker.UID != eo.Master.UID {
			retractable = append(retractable, worker)
		}
	}

	return retractable
}

// elasticOperationStore keeps the record in a ConfigMap owned by the deployment, so the record's
// lifetime is the deployment's and a record cannot outlive the thing it describes.
type elasticOperationStore struct {
	client ctrlcli.Client
	// now is the record's creation stamp, supplied rather than read so a test can say what it
	// expects without depending on the wall clock.
	now func() time.Time
}

// newElasticOperationStore builds the store. A nil client is refused by every call rather than
// panicking, because a store with no client is a bug in its wiring and a panic would take the
// controller with it.
func newElasticOperationStore(client ctrlcli.Client) *elasticOperationStore {
	return &elasticOperationStore{client: client, now: time.Now}
}

// recordName is the object name for a deployment's single unresolved record. It is derived from the
// deployment so that two operations for one deployment collide rather than running beside each
// other, which is the failure the single-writer rule exists to prevent.
func elasticOperationRecordName(deploymentUID types.UID) string {
	return "elastic-resize-" + string(deploymentUID)
}

// Create writes the record, and refuses one that is not usable.
//
// THE RECORD IS WRITTEN BEFORE ANY ENGINE REQUEST IS PERMITTED, which is the whole reason this
// store exists: the caller gets back a stored object carrying the command intent, and only a
// record that was stored may cause a command to leave.
func (s *elasticOperationStore) Create(
	ctx context.Context, op *elasticOperation,
) (*elasticOperation, error) {
	if err := op.validate(); err != nil {
		return nil, err
	}
	if s.client == nil {
		return nil, fmt.Errorf("%w: no client is configured", errElasticOperation)
	}

	document, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("encode the elastic resize record: %w", err)
	}

	stored := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elasticOperationRecordName(op.DeploymentUID),
			Namespace: op.Namespace,
			Labels: map[string]string{
				elasticOperationLabel: "true",
			},
			// THE OWNER IS THE DEPLOYMENT'S OWN UID, not its name. A record that outlived a
			// recreated deployment would otherwise be read as this one's operation.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.GroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       op.Name,
				UID:        op.DeploymentUID,
				Controller: elasticControllerRef(),
			}},
		},
		Data: map[string]string{elasticOperationDataKey: string(document)},
	}
	if s.now != nil {
		stored.CreationTimestamp = metav1.NewTime(s.now())
	}

	if err := s.client.Create(ctx, stored); err != nil {
		return nil, fmt.Errorf("store the elastic resize record: %w", err)
	}
	op.ResourceVersion = stored.ResourceVersion

	return op, nil
}

// Read returns the deployment's record, and refuses anything that is not this deployment's record.
//
// A wrong owner, a missing controlling owner, an empty body, malformed JSON, a body that decodes to
// nothing, and a second JSON value after the record are all refusals. None of them is an empty
// record, because an empty record is what a completed or absent operation looks like from the
// outside, and reading either as "no operation" would drop a decision.
func (s *elasticOperationStore) Read(
	ctx context.Context, namespace, name string, deploymentUID types.UID,
) (*elasticOperation, error) {
	if s.client == nil {
		return nil, fmt.Errorf("%w: no client is configured", errElasticOperation)
	}

	stored := new(corev1.ConfigMap)
	if err := s.client.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: elasticOperationRecordName(deploymentUID)}, stored); err != nil {
		return nil, err
	}

	// THE OWNER IS CHECKED IN FULL, AGAINST THE IDENTITY THE CALLER EXPECTED. A UID alone is not
	// enough: an object of another kind may carry the same UID by accident, and reading a Pod's
	// record as a deployment's would be a decision about an object the deployment never owned. The
	// name is checked too, because a caller asking for one deployment and receiving another's record
	// is a mismatch even when every field of the record agrees with itself.
	if err := elasticControllingOwnerIs(stored, name, deploymentUID); err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(stored)

	body, found := stored.Data[elasticOperationDataKey]
	if !found || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the stored record is empty", errElasticOperation)
	}

	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	op := new(elasticOperation)
	if err := decoder.Decode(op); err != nil {
		return nil, fmt.Errorf("%w: %w", errElasticOperation, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: the stored record carries a trailing JSON value", errElasticOperation)
	}

	// THE DOCUMENT'S IDENTITY IS COMPARED, NOT REPLACED. Writing the owner's name into a record that
	// named another deployment would make a mismatch read as agreement.
	if op.DeploymentUID != "" && op.DeploymentUID != owner.UID {
		return nil, fmt.Errorf(
			"%w: the record inside names deployment %s while its owner is %s",
			errElasticOperation, op.DeploymentUID, owner.UID)
	}
	if op.Name != "" && op.Name != owner.Name {
		return nil, fmt.Errorf(
			"%w: the record inside names %s while its owner is %s",
			errElasticOperation, op.Name, owner.Name)
	}
	op.Namespace = stored.Namespace
	op.ResourceVersion = stored.ResourceVersion
	op.Name = owner.Name
	if err := op.validate(); err != nil {
		return nil, err
	}
	if op.DeploymentUID != deploymentUID {
		return nil, fmt.Errorf(
			"%w: the record inside is bound to %s rather than %s",
			errElasticOperation, op.DeploymentUID, deploymentUID)
	}

	return op, nil
}

// elasticControllingOwnerIs refuses a stored record whose controlling owner is not the deployment
// the caller named, in full.
//
// BOTH READ AND UPDATE USE IT, because a record that was not this deployment's must not be read as
// this deployment's, and a write against it would replace the wrong object's bytes. An expected
// identity that is itself missing is refused rather than treated as a wildcard: a caller that names
// no deployment is not asking about every deployment.
func elasticControllingOwnerIs(stored *corev1.ConfigMap, name string, deploymentUID types.UID) error {
	if name == "" || deploymentUID == "" {
		return fmt.Errorf(
			"%w: no expected deployment identity was given to match the stored owner", errElasticOperation)
	}

	owner := metav1.GetControllerOf(stored)
	if owner == nil {
		return fmt.Errorf("%w: the stored record has no controlling owner", errElasticOperation)
	}
	if owner.Kind != "ModelDeployment" {
		return fmt.Errorf(
			"%w: the stored record is owned by a %s rather than a ModelDeployment",
			errElasticOperation, owner.Kind)
	}
	if owner.APIVersion != workercore.GroupVersion.String() {
		return fmt.Errorf(
			"%w: the stored record's owner is %s rather than %s",
			errElasticOperation, owner.APIVersion, workercore.GroupVersion.String())
	}
	if owner.Name != name {
		return fmt.Errorf(
			"%w: the stored record is owned by %s rather than the expected %s",
			errElasticOperation, owner.Name, name)
	}
	if owner.UID != deploymentUID {
		return fmt.Errorf(
			"%w: the stored record is owned by %s rather than %s",
			errElasticOperation, owner.UID, deploymentUID)
	}

	return nil
}

// Update writes the record against the exact object it was read from.
//
// THE CALLER'S OPERATION IS NOT MUTATED WHEN THE WRITE FAILS. The version it carries is the one it
// was read with, and a write that loses the race has changed nothing, so leaving the version on the
// caller would invite a second attempt against a version that is now someone else's.
func (s *elasticOperationStore) Update(ctx context.Context, op *elasticOperation) error {
	if err := op.validate(); err != nil {
		return err
	}
	if s.client == nil {
		return fmt.Errorf("%w: no client is configured", errElasticOperation)
	}

	stored := new(corev1.ConfigMap)
	if err := s.client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: op.Namespace, Name: elasticOperationRecordName(op.DeploymentUID),
	}, stored); err != nil {
		return err
	}
	// AN UNOBSERVED VERSION IS A REFUSAL, NOT A BLANKET. Writing with no version would replace
	// whatever the object holds now with a decision made against facts that have since moved, which
	// is the overwrite the optimistic write exists to prevent.
	if op.ResourceVersion == "" {
		return fmt.Errorf(
			"%w: the record was not read, so it carries no version to write against", errElasticOperation)
	}
	if stored.ResourceVersion != op.ResourceVersion {
		return apierrors.NewConflict(
			corev1.Resource("configmaps"), elasticOperationRecordName(op.DeploymentUID),
			fmt.Errorf("the record moved from %s to %s", op.ResourceVersion, stored.ResourceVersion))
	}

	// THE OWNER IS RE-READ AND RE-CHECKED, NOT ASSUMED FROM THE READ THAT PRODUCED THIS OPERATION.
	// A fresh version means the object was written since, and whoever wrote it may have taken
	// ownership of it. Writing now would replace another deployment's bytes, so the identity is
	// confirmed against the object as it stands before a single byte of it is changed.
	if err := elasticControllingOwnerIs(stored, op.Name, op.DeploymentUID); err != nil {
		return err
	}

	// THE ENVELOPE IS FIXED WHEN THE RECORD IS WRITTEN. Its deployment, generation, widths and
	// captured identities are what the operation was decided against, and a later write that moved
	// any of them would be a different operation wearing this one's name. Forward state is what
	// advances, and the command intent may only ever be set, never cleared.
	current, err := elasticOperationFromData(stored.Data[elasticOperationDataKey])
	if err != nil {
		return err
	}
	if reason := current.envelopeDrift(op); reason != "" {
		return fmt.Errorf("%w: %s", errElasticOperation, reason)
	}

	document, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("encode the elastic resize record: %w", err)
	}
	stored.Data = map[string]string{elasticOperationDataKey: string(document)}

	if err := s.client.Update(ctx, stored); err != nil {
		return err
	}
	op.ResourceVersion = stored.ResourceVersion

	return nil
}

// RecoverInterrupted replaces one interrupted record with its recovery record in a single write.
//
// THE ENVELOPE MOVES ON THIS ONE TRANSITION, BY DESIGN, which is why it does not go through
// envelopeDrift like every other write. The record being replaced is a generation-stale upward
// command that never advanced the native world: recovery re-proves that command's old width and
// retires the stale intent into a release-only record at the proven width, so the widths, the
// captured workers and the release identities all differ between the two records. The recovery
// record removes nothing by itself — every worker it captured still owes the ordinary withdrawal,
// actor-free and allocation-return proofs before any deletion — so the envelope move never
// shortcuts a proof. Everything else about Update's optimism is kept: the stored object is read
// fresh, the version and the owner are re-checked, and the stored envelope is compared against
// the record the caller decided against, so a record that moved since that decision is a
// refusal rather than an overwrite.
func (s *elasticOperationStore) RecoverInterrupted(
	ctx context.Context, current, recovered *elasticOperation,
) error {
	if err := current.validate(); err != nil {
		return err
	}
	if err := recovered.validate(); err != nil {
		return err
	}
	if recovered.Name != current.Name || recovered.Namespace != current.Namespace ||
		recovered.DeploymentUID != current.DeploymentUID {
		return fmt.Errorf("%w: the recovery record is not this deployment's", errElasticOperation)
	}
	if s.client == nil {
		return fmt.Errorf("%w: no client is configured", errElasticOperation)
	}

	stored := new(corev1.ConfigMap)
	if err := s.client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: current.Namespace, Name: elasticOperationRecordName(current.DeploymentUID),
	}, stored); err != nil {
		return err
	}
	if current.ResourceVersion == "" {
		return fmt.Errorf(
			"%w: the record was not read, so it carries no version to write against", errElasticOperation)
	}
	if stored.ResourceVersion != current.ResourceVersion {
		return apierrors.NewConflict(
			corev1.Resource("configmaps"), elasticOperationRecordName(current.DeploymentUID),
			fmt.Errorf("the record moved from %s to %s", current.ResourceVersion, stored.ResourceVersion))
	}
	if err := elasticControllingOwnerIs(stored, current.Name, current.DeploymentUID); err != nil {
		return err
	}
	storedRecord, err := elasticOperationFromData(stored.Data[elasticOperationDataKey])
	if err != nil {
		return err
	}
	if reason := storedRecord.envelopeDrift(current); reason != "" {
		return fmt.Errorf("%w: %s", errElasticOperation, reason)
	}

	document, err := json.Marshal(recovered)
	if err != nil {
		return fmt.Errorf("encode the elastic resize record: %w", err)
	}
	stored.Data = map[string]string{elasticOperationDataKey: string(document)}

	if err := s.client.Update(ctx, stored); err != nil {
		return err
	}
	recovered.ResourceVersion = stored.ResourceVersion

	return nil
}

// Delete removes the record, which is the only way a deployment's operation stops being unresolved.
func (s *elasticOperationStore) Delete(ctx context.Context, op *elasticOperation) error {
	if s.client == nil || op == nil || op.ResourceVersion == "" {
		return fmt.Errorf("%w: deletion requires a read record", errElasticOperation)
	}
	stored := new(corev1.ConfigMap)
	if err := s.client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: op.Namespace, Name: elasticOperationRecordName(op.DeploymentUID),
	}, stored); err != nil {
		return err
	}
	if err := elasticControllingOwnerIs(stored, op.Name, op.DeploymentUID); err != nil {
		return err
	}
	if stored.ResourceVersion != op.ResourceVersion {
		return apierrors.NewConflict(corev1.Resource("configmaps"), stored.Name,
			fmt.Errorf("the operation record changed before deletion"))
	}
	return s.client.Delete(ctx, stored, ctrlcli.Preconditions{
		UID: &stored.UID, ResourceVersion: &op.ResourceVersion,
	})
}
