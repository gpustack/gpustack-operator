package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

const (
	// ModelDeploymentConditionRuntimeControlSynced reports whether live runtime-control parameters
	// and role pause transitions have converged across all answering worker Pods.
	ModelDeploymentConditionRuntimeControlSynced kubeapistatus.ConditionType = "RuntimeControlSynced"

	ModelDeploymentReasonRuntimeControlSynced                    = "Synced"
	ModelDeploymentReasonRuntimeControlNotConfigured             = "NotConfigured"
	ModelDeploymentReasonRuntimeControlReconciling               = "Reconciling"
	ModelDeploymentReasonRuntimeControlDraining                  = "Draining"
	ModelDeploymentReasonRuntimeControlEndpointUnreachable       = "EndpointUnreachable"
	ModelDeploymentReasonRuntimeControlWaitingForRouterIsolation = "WaitingForRouterIsolation"
	ModelDeploymentReasonRuntimeControlReconcileFailed           = "ReconcileFailed"
	ModelDeploymentReasonRuntimeControlUnsupportedEngineVersion  = "UnsupportedEngineVersion"
	ModelDeploymentReasonRuntimeControlDrainFailed               = "DrainFailed"
	ModelDeploymentReasonRuntimeControlAmbiguousOperation        = "AmbiguousOperation"

	modelDeploymentRuntimeControlMaxConcurrency = 8
	modelDeploymentRuntimeControlPassBudget     = 15 * time.Second
)

var (
	runtimeControlOperationSeq atomic.Uint64
	runtimeControlProcessNonce = newRuntimeControlProcessNonce()
)

func newRuntimeControlProcessNonce() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ModelDeploymentRuntimeControlClient defines the HTTP client contract used by
// ModelDeploymentReconciler to inspect and reconcile sentinel runtime controls.
type ModelDeploymentRuntimeControlClient interface {
	GetReady(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ReadyResponse, error)
	GetLive(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.LiveResponse, error)
	Ack(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.AckRequest) (*runtimecontrol.ReadyResponse, error)
	GetPause(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.PauseResponse, error)
	SetPause(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.PauseRequest) (*runtimecontrol.PauseResponse, error)
	GetRuntimeConfig(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.RuntimeConfigState, error)
	ApplyRuntimeConfig(
		ctx context.Context,
		target runtimecontrol.Target,
		req runtimecontrol.ApplyRuntimeConfigRequest,
	) (*runtimecontrol.RuntimeConfigState, error)
}

var _ ModelDeploymentRuntimeControlClient = (*runtimecontrol.Client)(nil)

var defaultModelDeploymentRuntimeControlClient = runtimecontrol.NewClient()

type modelDeploymentRuntimeControlGroupKey struct {
	Role    string
	Ordinal int
}

type modelDeploymentRuntimeControlObservation struct {
	ConditionStatus  meta.ConditionStatus
	ConditionReason  string
	ConditionMessage string
	RoleStatuses     map[string]*workercore.ModelDeploymentRoleRuntimeControlStatus
	HeldRoles        map[string]bool
	HeldGroups       map[modelDeploymentRuntimeControlGroupKey]bool
	HeldPods         map[types.UID]bool
	Requeue          bool
}

func (o *modelDeploymentRuntimeControlObservation) HoldsGroup(
	role *workercore.ModelDeploymentRole,
	view modelDeploymentReplicaView,
	pod *core.Pod,
) bool {
	if o == nil || role == nil {
		return false
	}
	if o.HeldRoles[role.Name] {
		return true
	}
	if pod != nil && o.HeldPods[pod.UID] {
		return true
	}
	if view.Seated {
		return o.HeldGroups[modelDeploymentRuntimeControlGroupKey{Role: role.Name, Ordinal: view.Ordinal}]
	}
	return false
}

func (r *ModelDeploymentReconciler) apiReaderOrClient() ctrlcli.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ModelDeploymentReconciler) runtimeControlClient() ModelDeploymentRuntimeControlClient {
	if r.RuntimeControlClient != nil {
		return r.RuntimeControlClient
	}
	return defaultModelDeploymentRuntimeControlClient
}

func (r *ModelDeploymentReconciler) runtimeControlLedgerStore() *runtimecontrol.LedgerStore {
	if r.LedgerStore != nil {
		return r.LedgerStore
	}
	return runtimecontrol.NewLedgerStore(
		r.apiReaderOrClient(),
		r.Client,
		runtimecontrol.WithLedgerClock(r.modelDeploymentNow),
	)
}

func (r *ModelDeploymentReconciler) runtimeControlDrainReader() *runtimecontrol.DrainReader {
	if r.DrainReader != nil {
		return r.DrainReader
	}
	return runtimecontrol.NewDrainReader(runtimecontrol.WithDrainClock(r.modelDeploymentNow))
}

func modelDeploymentRoleHasObservedRuntimeControl(md *workercore.ModelDeployment, roleName string) bool {
	if md == nil {
		return false
	}
	for i := range md.Status.Roles {
		if md.Status.Roles[i].Name == roleName && md.Status.Roles[i].RuntimeControl != nil {
			return true
		}
	}
	return false
}

func modelDeploymentAnyRoleHasObservedRuntimeControl(md *workercore.ModelDeployment) bool {
	if md == nil {
		return false
	}
	for i := range md.Status.Roles {
		if md.Status.Roles[i].RuntimeControl != nil {
			return true
		}
	}
	return false
}

func modelDeploymentRoleIsObservedPaused(md *workercore.ModelDeployment, roleName string) bool {
	if md == nil {
		return false
	}
	for i := range md.Status.Roles {
		if md.Status.Roles[i].Name == roleName && md.Status.Roles[i].RuntimeControl != nil {
			return md.Status.Roles[i].RuntimeControl.Paused
		}
	}
	return false
}

func modelDeploymentRolePaused(md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole) bool {
	if role == nil {
		return false
	}
	return role.Pause || modelDeploymentRoleIsObservedPaused(md, role.Name)
}

func modelDeploymentAnyRolePaused(md *workercore.ModelDeployment) bool {
	if md == nil {
		return false
	}
	for i := range md.Spec.Roles {
		if modelDeploymentRolePaused(md, &md.Spec.Roles[i]) {
			return true
		}
	}
	return false
}

func modelDeploymentRoleHasSentinelParameters(role *workercore.ModelDeploymentRole) bool {
	if role == nil || role.RuntimeControl == nil {
		return false
	}
	return len(runtimecontrol.SentinelParameterKeys(role.RuntimeControl.Parameters)) > 0
}

func modelDeploymentRoleDeclaresPauseOrRuntimeControl(role *workercore.ModelDeploymentRole) bool {
	if role == nil {
		return false
	}
	return role.Pause || modelDeploymentRoleHasSentinelParameters(role)
}

func modelDeploymentAnyRoleDeclaresPauseOrRuntimeControl(md *workercore.ModelDeployment) bool {
	if md == nil {
		return false
	}
	for i := range md.Spec.Roles {
		if modelDeploymentRoleDeclaresPauseOrRuntimeControl(&md.Spec.Roles[i]) {
			return true
		}
	}
	return false
}

// modelDeploymentControllerPodMatchesCurrency evaluates pod currency with legacy full-router hash
// acceptance and ensures sentinel container activation matches when required by desired.
func modelDeploymentControllerPodMatchesCurrency(
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	ordinal, member int,
	actual, desired *core.Pod,
	minRenderVersion int,
) bool {
	legacyHash := modelDeploymentDesiredPodLegacyRouterHash(md, role, ordinal, member, desired)
	var matches bool
	if legacyHash == "" {
		matches = modelDeploymentPodMatchesCurrency(actual, desired, minRenderVersion)
	} else {
		matches = modelDeploymentPodMatchesCurrencyWithLegacy(actual, desired, legacyHash, minRenderVersion)
	}
	if !matches {
		return false
	}
	return modelDeploymentPodMatchesRuntimeControlActivation(actual, desired)
}

func modelDeploymentPodMatchesRuntimeControlActivation(actual, desired *core.Pod) bool {
	if actual == nil || desired == nil {
		return false
	}
	wantCtr, ok := modelDeploymentDrainContainerOf(desired, modelDeploymentMainContainerName)
	if !ok {
		return true
	}
	gotCtr, ok := modelDeploymentDrainContainerOf(actual, modelDeploymentMainContainerName)
	if !ok {
		return true
	}
	for _, key := range []string{
		modelDeploymentEngineSentinelEnableEnv,
		modelDeploymentEngineSentinelEngineEnv,
	} {
		if containerHasEnvDefined(wantCtr, key) && !containerHasEnvDefined(gotCtr, key) {
			return false
		}
	}
	return true
}

func containerHasEnvDefined(ctr *core.Container, name string) bool {
	if ctr == nil {
		return false
	}
	for i := range ctr.Env {
		if ctr.Env[i].Name == name {
			return ctr.Env[i].Value != "" || ctr.Env[i].ValueFrom != nil
		}
	}
	return false
}

func modelDeploymentPodHasRuntimeControlEnable(pod *core.Pod) bool {
	ctr, ok := modelDeploymentDrainContainerOf(pod, modelDeploymentMainContainerName)
	if !ok {
		return false
	}
	return containerHasEnvDefined(ctr, modelDeploymentEngineSentinelEnableEnv)
}

func (r *ModelDeploymentReconciler) resolveModelDeploymentRouterIsolation(
	ctx context.Context,
	md *workercore.ModelDeployment,
) (bool, string, error) {
	if md == nil || md.Spec.Router == nil || !modelDeploymentRouterRequiresAllowlistProxy(md) {
		return true, "", nil
	}
	retained, err := r.modelDeploymentRetainedRouterEligibility(ctx, md)
	if err != nil {
		return false, "", err
	}
	routerObjects, err := renderModelDeploymentRouterObjects(
		ctx, md, r.modelDeploymentRoleManufacturers(ctx, md), retained,
	)
	if err != nil {
		return false, "", err
	}
	return modelDeploymentRouterIsolationReady(ctx, r.apiReaderOrClient(), md, routerObjects)
}

func (r *ModelDeploymentReconciler) modelDeploymentRetainedRouterEligibility(
	ctx context.Context,
	md *workercore.ModelDeployment,
) (bool, error) {
	if md == nil || md.Spec.Router == nil {
		return false, nil
	}
	dep := new(core.ConfigMap)
	err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{
		Namespace: md.Namespace,
		Name:      md.Name + "-router",
	}, dep)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(dep.Data[modelDeploymentRouterSelectorKey], modelDeploymentLabelKeyEndpointEligible+"="), nil
}

func (r *ModelDeploymentReconciler) modelDeploymentElasticResizeInFlight(
	ctx context.Context,
	md *workercore.ModelDeployment,
) (bool, string) {
	role := ModelDeploymentElasticRole(md)
	if role == nil || !runtimecontrol.RoleHasElasticEP(role) {
		return false, ""
	}
	if r.Client != nil && md.UID != "" {
		store := newElasticOperationStore(r.Client)
		op, err := store.Read(ctx, md.Namespace, md.Name, md.UID)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return true, fmt.Sprintf("unable to read elastic EP resize operation: %v", err)
			}
		} else if op != nil {
			if op.State != elasticStateCompleted && op.State != elasticStateAbandoned {
				return true, "waiting for in-flight elastic EP resize operation to settle"
			}
		}
	}
	if r.APIReader != nil && md.UID != "" {
		cm, err := r.elasticObservationDocument(ctx, md)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return true, fmt.Sprintf("unable to read elastic EP observation: %v", err)
			}
		} else if cm != nil {
			var obs modelDeploymentElasticStatus
			if uErr := json.Unmarshal([]byte(cm.Data["observation.json"]), &obs); uErr != nil {
				return true, fmt.Sprintf("unable to decode elastic EP observation: %v", uErr)
			}
			switch obs.State {
			case elasticStateRecorded,
				elasticStateCommandSent,
				elasticStateAwaitingWidth,
				elasticStateNativeDone,
				elasticStateWithdrawn,
				elasticStateReleased:
				return true, "waiting for active elastic EP resize state to complete"
			}
		}
	}
	return false, ""
}

// ensureModelDeploymentElasticMasterRuntimeControl replays desired runtimeControl parameters and
// acknowledges the elastic master across all active ranks before releasing the elastic withdrawal gate.
func (r *ModelDeploymentReconciler) ensureModelDeploymentElasticMasterRuntimeControl(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	master *core.Pod,
) (bool, string, error) {
	expectedRanks := 0
	if width, ok, err := runtimecontrol.ParseElasticWidth(role); err == nil && ok && width > 0 {
		expectedRanks = int(width)
	}
	return r.ensureModelDeploymentElasticMasterRuntimeControlForWidth(ctx, md, role, master, expectedRanks)
}

func (r *ModelDeploymentReconciler) ensureModelDeploymentElasticMasterRuntimeControlForWidth(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	master *core.Pod,
	expectedRanks int,
) (bool, string, error) {
	if md == nil || role == nil || master == nil {
		return true, "", nil
	}
	if !modelDeploymentRoleDeclaresPauseOrRuntimeControl(role) &&
		!modelDeploymentRoleHasObservedRuntimeControl(md, role.Name) {
		return true, "", nil
	}
	// Settle post-resize replay with parameter-only semantics (Pause=false) so a pause arriving
	// during an in-flight resize does not deadlock waiting for router withdrawal on an empty
	// ServingAnswer; the ordinary pass executes role pause immediately after the resize retires.
	replayRole := *role
	replayRole.Pause = false
	if !modelDeploymentRoleDeclaresPauseOrRuntimeControl(&replayRole) &&
		!modelDeploymentRoleHasObservedRuntimeControl(md, role.Name) {
		return true, "", nil
	}

	owner := runtimecontrol.LedgerOwnerFromModelDeployment(md)
	ledgerStore := r.runtimeControlLedgerStore()
	ledger, err := ledgerStore.Read(ctx, owner, role.Name)
	if err != nil {
		return false, "", err
	}
	// Clear prior digest confirmation so newly joined elastic DP ranks receive a fresh replay and ACK.
	if entry, ok := ledger.GetEntry(master.Name); ok && entry.ConfirmedAppliedDigest != "" {
		entry.ConfirmedAppliedDigest = ""
		ledger.SetEntry(master.Name, entry)
		if saveErr := ledgerStore.Save(ctx, owner, ledger); saveErr != nil {
			if runtimecontrol.IsLedgerConflict(saveErr) {
				return false, "retrying runtime-control ledger update after conflict", nil
			}
			return false, "", saveErr
		}
	}

	budgetCtx, cancel := context.WithTimeout(ctx, modelDeploymentRuntimeControlPassBudget)
	defer cancel()
	outcome, err := r.reconcileRoleRuntimeControlWithOptions(
		budgetCtx,
		md,
		&replayRole,
		[]core.Pod{*master},
		nil,
		false,
		"",
		ServingAnswer{},
		expectedRanks,
		true,
	)
	if err != nil {
		return false, "", err
	}
	if !outcome.allConverged {
		msg := outcome.message
		if msg == "" {
			msg = "waiting for post-resize runtime-control replay to converge on elastic master"
		}
		return false, msg, nil
	}
	return true, "", nil
}

type roleRuntimeControlOutcome struct {
	configured    bool
	publishStatus bool
	roleStatus    *workercore.ModelDeploymentRoleRuntimeControlStatus
	condStatus    meta.ConditionStatus
	condReason    string
	message       string
	allConverged  bool
	holdRole      bool
	heldGroups    map[int]bool
	heldPods      map[types.UID]bool
	requeue       bool
}

// reconcileModelDeploymentRuntimeControl reconciles pause and runtimeControl across all roles and
// records per-role status, condition verdicts, and endpoint-eligibility holds onto observation.
func (r *ModelDeploymentReconciler) reconcileModelDeploymentRuntimeControl(
	ctx context.Context,
	md *workercore.ModelDeployment,
	pods []core.Pod,
	replacedByRole map[string]map[int]bool,
	routerIsolationReady bool,
	routerIsolationMessage string,
	observation *modelDeploymentPassObservation,
) error {
	obs := &modelDeploymentRuntimeControlObservation{
		ConditionStatus:  meta.ConditionTrue,
		ConditionReason:  ModelDeploymentReasonRuntimeControlNotConfigured,
		ConditionMessage: "runtimeControl is not configured on any role",
		RoleStatuses:     make(map[string]*workercore.ModelDeploymentRoleRuntimeControlStatus, len(md.Spec.Roles)),
		HeldRoles:        make(map[string]bool),
		HeldGroups:       make(map[modelDeploymentRuntimeControlGroupKey]bool),
		HeldPods:         make(map[types.UID]bool),
	}
	if observation != nil {
		observation.RuntimeControl = obs
	}

	if !routerIsolationReady {
		anyActiveOrObserved := modelDeploymentAnyRoleDeclaresPauseOrRuntimeControl(md) ||
			modelDeploymentAnyRoleHasObservedRuntimeControl(md)
		if !anyActiveOrObserved {
			return nil
		}
		msg := routerIsolationMessage
		if msg == "" {
			msg = "waiting for managed router rollout to enforce the inference allowlist"
		}
		obs.ConditionStatus = meta.ConditionUnknown
		obs.ConditionReason = ModelDeploymentReasonRuntimeControlWaitingForRouterIsolation
		obs.ConditionMessage = msg
		obs.Requeue = true
		for i := range md.Spec.Roles {
			role := &md.Spec.Roles[i]
			if !modelDeploymentRoleDeclaresPauseOrRuntimeControl(role) &&
				!modelDeploymentRoleHasObservedRuntimeControl(md, role.Name) {
				continue
			}
			if role.Pause || !roleHasReadyEligiblePods(role.Name, pods) {
				obs.HeldRoles[role.Name] = true
			} else {
				// Preserve already-ready eligible Pods individually so previously withheld or
				// unready sibling Pods stay fenced while router isolation is unknown.
				for j := range pods {
					p := &pods[j]
					if p.DeletionTimestamp != nil || modelDeploymentPodRole(p) != role.Name {
						continue
					}
					if !podIsReady(p) || p.Labels[modelDeploymentLabelKeyEndpointEligible] != modelDeploymentEndpointEligibleValue {
						obs.HeldPods[p.UID] = true
					}
				}
			}
			obs.RoleStatuses[role.Name] = &workercore.ModelDeploymentRoleRuntimeControlStatus{
				ObservedGeneration: md.Generation,
				Synced:             false,
				Reason:             ModelDeploymentReasonRuntimeControlWaitingForRouterIsolation,
				Message:            msg,
				Paused:             previousPausedState(md, role.Name),
			}
		}
		return nil
	}

	elasticInFlight, elasticInFlightMsg := r.modelDeploymentElasticResizeInFlight(ctx, md)
	var serving ServingAnswer
	if observation != nil {
		serving = observation.Serving
	}

	roleCount := max(len(md.Spec.Roles), 1)
	roleBudget := max(modelDeploymentRuntimeControlPassBudget/time.Duration(roleCount), 5*time.Second)

	outcomes := make([]roleRuntimeControlOutcome, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		roleCtx, roleCancel := context.WithTimeout(ctx, roleBudget)
		outcome, roleErr := r.reconcileRoleRuntimeControl(
			roleCtx,
			md,
			role,
			pods,
			replacedByRole[role.Name],
			elasticInFlight,
			elasticInFlightMsg,
			serving,
		)
		roleCancel()
		if roleErr != nil {
			if errors.Is(roleErr, context.DeadlineExceeded) && ctx.Err() == nil {
				msg := fmt.Sprintf("role %q runtime-control pass budget exceeded; will retry", role.Name)
				obs.Requeue = true
				if role.Pause || previousPausedState(md, role.Name) {
					obs.HeldRoles[role.Name] = true
				}
				if modelDeploymentRoleDeclaresPauseOrRuntimeControl(role) ||
					modelDeploymentRoleHasObservedRuntimeControl(md, role.Name) {
					obs.RoleStatuses[role.Name] = &workercore.ModelDeploymentRoleRuntimeControlStatus{
						ObservedGeneration: md.Generation,
						Synced:             false,
						Reason:             ModelDeploymentReasonRuntimeControlReconciling,
						Message:            msg,
						Paused:             previousPausedState(md, role.Name),
					}
				}
				outcomes = append(outcomes, roleRuntimeControlOutcome{
					configured: true,
					condStatus: meta.ConditionUnknown,
					condReason: ModelDeploymentReasonRuntimeControlReconciling,
					message:    msg,
					requeue:    true,
				})
				continue
			}
			return roleErr
		}
		if outcome.publishStatus && outcome.roleStatus != nil {
			obs.RoleStatuses[role.Name] = outcome.roleStatus
		}
		if outcome.holdRole {
			obs.HeldRoles[role.Name] = true
		}
		for ord := range outcome.heldGroups {
			obs.HeldGroups[modelDeploymentRuntimeControlGroupKey{Role: role.Name, Ordinal: ord}] = true
		}
		for uid := range outcome.heldPods {
			obs.HeldPods[uid] = true
		}
		if outcome.requeue {
			obs.Requeue = true
		}
		outcomes = append(outcomes, outcome)
	}

	obs.ConditionStatus, obs.ConditionReason, obs.ConditionMessage = aggregateRuntimeControlCondition(outcomes)
	return nil
}

func aggregateRuntimeControlCondition(
	outcomes []roleRuntimeControlOutcome,
) (meta.ConditionStatus, string, string) {
	for _, out := range outcomes {
		if out.condStatus == meta.ConditionFalse {
			return meta.ConditionFalse, out.condReason, out.message
		}
	}
	for _, out := range outcomes {
		if out.condStatus == meta.ConditionUnknown {
			return meta.ConditionUnknown, out.condReason, out.message
		}
	}
	for _, out := range outcomes {
		if out.configured {
			return meta.ConditionTrue, ModelDeploymentReasonRuntimeControlSynced, "all answering Pods converged on desired runtimeControl"
		}
	}
	return meta.ConditionTrue, ModelDeploymentReasonRuntimeControlNotConfigured, "runtimeControl is not configured on any role"
}

func applyModelDeploymentRuntimeControlStatus(
	desired *workercore.ModelDeploymentStatus,
	md *workercore.ModelDeployment,
	obs *modelDeploymentRuntimeControlObservation,
) {
	if desired == nil || obs == nil {
		return
	}
	for i := range desired.Roles {
		roleName := desired.Roles[i].Name
		if st, ok := obs.RoleStatuses[roleName]; ok && st != nil {
			desired.Roles[i].RuntimeControl = st.DeepCopy()
		} else {
			desired.Roles[i].RuntimeControl = nil
		}
	}
	holder := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Generation: md.Generation},
		Status:     *desired,
	}
	switch obs.ConditionStatus {
	case meta.ConditionTrue:
		ModelDeploymentConditionRuntimeControlSynced.True(holder, obs.ConditionReason, obs.ConditionMessage)
	case meta.ConditionFalse:
		ModelDeploymentConditionRuntimeControlSynced.False(holder, obs.ConditionReason, obs.ConditionMessage)
	default:
		ModelDeploymentConditionRuntimeControlSynced.Unknown(holder, obs.ConditionReason, obs.ConditionMessage)
	}
	desired.Conditions = holder.Status.Conditions
}

type roleAnsweringGroup struct {
	ordinal       int
	seated        bool
	requiredCount int
	runningPods   []*core.Pod
	allMembers    []*core.Pod
}

func resolveRoleAnsweringGroups(
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pods []core.Pod,
	replacingOrdinals map[int]bool,
) []roleAnsweringGroup {
	if md == nil || role == nil {
		return nil
	}

	if runtimecontrol.RoleHasElasticEP(role) {
		masters := modelDeploymentElasticMasters(md, pods)
		if len(masters) == 0 {
			return nil
		}
		var running []*core.Pod
		var all []*core.Pod
		for i := range masters {
			p := &masters[i]
			if p.DeletionTimestamp != nil || p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
				continue
			}
			all = append(all, p)
			if p.Status.Phase == core.PodRunning && p.Status.PodIP != "" {
				running = append(running, p)
			}
		}
		if len(all) == 0 {
			return nil
		}
		return []roleAnsweringGroup{{
			ordinal:       0,
			seated:        true,
			requiredCount: 1,
			runningPods:   running,
			allMembers:    all,
		}}
	}

	rolePods := make([]core.Pod, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
			continue
		}
		if modelDeploymentPodRole(p) != role.Name {
			continue
		}
		rolePods = append(rolePods, *p)
	}
	if len(rolePods) == 0 {
		return nil
	}

	views := modelDeploymentGroupPodsByReplica(rolePods)
	groups := make([]roleAnsweringGroup, 0, len(views))
	for _, view := range views {
		if view.Seated && replacingOrdinals[view.Ordinal] {
			continue
		}
		repShape := modelDeploymentDeployedReplicaShape(view)
		answeringShape := modelDeploymentDeployedAnsweringShape(view, md.Spec.Engine.Name)

		var required int
		var candidates []*core.Pod
		switch answeringShape {
		case modelDeploymentAnsweringAll:
			if repShape.Total > 0 {
				required = repShape.Total
			} else {
				required = max(1, modelDeploymentRoleSize(role))
			}
			candidates = view.Members
		case modelDeploymentAnsweringSole:
			required = 1
			candidates = view.Members[:1]
		case modelDeploymentAnsweringLeader:
			required = 1
			for _, m := range view.Members {
				if modelDeploymentAnsweringMemberLeader(m) {
					candidates = append(candidates, m)
				}
			}
		default:
			if modelDeploymentRoleExternalDP(md, role) {
				required = max(1, modelDeploymentRoleSize(role))
				candidates = view.Members
			} else {
				required = 1
				for _, m := range view.Members {
					if modelDeploymentAnsweringMemberLeader(m) {
						candidates = append(candidates, m)
					}
				}
			}
		}

		running := make([]*core.Pod, 0, len(candidates))
		for _, m := range candidates {
			if m.Status.Phase == core.PodRunning &&
				(m.Status.PodIP != "" || (!modelDeploymentPodHasRuntimeControlEnable(m) && len(role.Command) > 0)) {
				running = append(running, m)
			}
		}
		groups = append(groups, roleAnsweringGroup{
			ordinal:       view.Ordinal,
			seated:        view.Seated,
			requiredCount: required,
			runningPods:   running,
			allMembers:    view.Members,
		})
	}
	return groups
}

func (r *ModelDeploymentReconciler) reconcileRoleRuntimeControl(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pods []core.Pod,
	replacingOrdinals map[int]bool,
	elasticInFlight bool,
	elasticInFlightMsg string,
	serving ServingAnswer,
) (roleRuntimeControlOutcome, error) {
	return r.reconcileRoleRuntimeControlWithOptions(
		ctx,
		md,
		role,
		pods,
		replacingOrdinals,
		elasticInFlight,
		elasticInFlightMsg,
		serving,
		0,
		false,
	)
}

func (r *ModelDeploymentReconciler) reconcileRoleRuntimeControlWithOptions(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pods []core.Pod,
	replacingOrdinals map[int]bool,
	elasticInFlight bool,
	elasticInFlightMsg string,
	serving ServingAnswer,
	expectedRanks int,
	parameterOnlyReplay bool,
) (roleRuntimeControlOutcome, error) {
	out := roleRuntimeControlOutcome{
		condStatus: meta.ConditionTrue,
		condReason: ModelDeploymentReasonRuntimeControlNotConfigured,
		heldGroups: make(map[int]bool),
		heldPods:   make(map[types.UID]bool),
	}

	hasSentinelParams := modelDeploymentRoleHasSentinelParameters(role)
	declaresActive := role.Pause || hasSentinelParams
	hadObserved := modelDeploymentRoleHasObservedRuntimeControl(md, role.Name)

	owner := runtimecontrol.LedgerOwnerFromModelDeployment(md)
	ledgerStore := r.runtimeControlLedgerStore()
	var ledger *runtimecontrol.RoleLedger
	if md.UID != "" && (declaresActive || hadObserved || roleHasRuntimeControlPod(role.Name, pods)) {
		var err error
		ledger, err = ledgerStore.Read(ctx, owner, role.Name)
		if err != nil {
			return out, err
		}
	}

	emptyDigest, err := runtimecontrol.ComputeAppliedConfigDigest(nil, false)
	if err != nil {
		return out, err
	}

	needsRestoration := !declaresActive && (hadObserved || ledgerHasNonDefaultState(ledger, emptyDigest))
	if !declaresActive && !needsRestoration {
		if ledger != nil && len(ledger.Entries) > 0 && md.UID != "" {
			if delErr := ledgerStore.DeleteIfMatch(ctx, owner, role.Name, ledger.ObjectUID, ledger.ResourceVersion); delErr != nil {
				return out, delErr
			}
		}
		r.reconcileDefaultSentinelBootstrap(ctx, md, role, pods, replacingOrdinals, emptyDigest, &out)
		return out, nil
	}

	out.configured = declaresActive
	out.publishStatus = declaresActive || needsRestoration

	desiredRC := role.RuntimeControl
	desiredPaused := role.Pause
	desiredDigest, digestProblem := computeRoleAppliedDigest(desiredRC, desiredPaused)
	if digestProblem != "" {
		out.condStatus = meta.ConditionFalse
		out.condReason = ModelDeploymentReasonRuntimeControlReconcileFailed
		out.message = digestProblem
		out.holdRole = true
		out.roleStatus = &workercore.ModelDeploymentRoleRuntimeControlStatus{
			ObservedGeneration: md.Generation,
			Synced:             false,
			Reason:             out.condReason,
			Message:            out.message,
			Paused:             previousPausedState(md, role.Name),
		}
		return out, nil
	}

	groups := resolveRoleAnsweringGroups(md, role, pods, replacingOrdinals)
	if ledger != nil && md.UID != "" {
		liveMap := make(map[string]types.UID)
		for i := range pods {
			p := &pods[i]
			if p.DeletionTimestamp == nil && modelDeploymentPodRole(p) == role.Name {
				liveMap[p.Name] = p.UID
			}
		}
		if ledger.Compact(liveMap) > 0 {
			if saveErr := ledgerStore.Save(ctx, owner, ledger); saveErr != nil && !runtimecontrol.IsLedgerConflict(saveErr) {
				return out, saveErr
			}
		}
	}

	totalAnswering := 0
	for _, g := range groups {
		totalAnswering += g.requiredCount
	}

	if role.Replicas > 0 && len(groups) == 0 {
		out.condStatus = meta.ConditionUnknown
		out.condReason = ModelDeploymentReasonRuntimeControlReconciling
		out.message = fmt.Sprintf("waiting for answering Pods of role %q to start", role.Name)
		out.holdRole = true
		out.requeue = true
		if out.publishStatus {
			out.roleStatus = &workercore.ModelDeploymentRoleRuntimeControlStatus{
				ObservedGeneration:  md.Generation,
				Synced:              false,
				Reason:              out.condReason,
				Message:             out.message,
				Paused:              previousPausedState(md, role.Name),
				AppliedConfigDigest: desiredDigest,
			}
		}
		return out, nil
	}

	if role.Replicas == 0 && len(groups) == 0 {
		if needsRestoration {
			if ledger != nil && ledger.ResourceVersion != "" && md.UID != "" {
				if delErr := ledgerStore.DeleteIfMatch(ctx, owner, role.Name, ledger.ObjectUID, ledger.ResourceVersion); delErr != nil {
					return out, delErr
				}
			}
			out.publishStatus = false
			out.configured = false
			return out, nil
		}
		out.allConverged = true
		out.holdRole = desiredPaused
		out.condStatus = meta.ConditionTrue
		out.condReason = ModelDeploymentReasonRuntimeControlSynced
		out.message = fmt.Sprintf("role %q has zero desired replicas", role.Name)
		if out.publishStatus {
			out.roleStatus = &workercore.ModelDeploymentRoleRuntimeControlStatus{
				ObservedGeneration:  md.Generation,
				Synced:              true,
				Reason:              ModelDeploymentReasonRuntimeControlSynced,
				Paused:              desiredPaused,
				AppliedConfigDigest: desiredDigest,
			}
		}
		return out, nil
	}

	var ledgerMu sync.Mutex
	var ledgerConflict atomic.Bool
	saveLedger := func(mutator func(*runtimecontrol.RoleLedger)) error {
		if ledger == nil || md.UID == "" {
			return nil
		}
		ledgerMu.Lock()
		defer ledgerMu.Unlock()
		candidate := ledger.DeepCopy()
		mutator(candidate)
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer saveCancel()
		if saveErr := ledgerStore.Save(saveCtx, owner, candidate); saveErr != nil {
			if runtimecontrol.IsLedgerConflict(saveErr) {
				ledgerConflict.Store(true)
				return saveErr
			}
			return saveErr
		}
		*ledger = *candidate
		return nil
	}
	readEntry := func(podName string) (runtimecontrol.PodLedgerEntry, bool) {
		if ledger == nil {
			return runtimecontrol.PodLedgerEntry{}, false
		}
		ledgerMu.Lock()
		defer ledgerMu.Unlock()
		return ledger.GetEntry(podName)
	}

	type podTask struct {
		groupIdx int
		pod      *core.Pod
		peers    []*core.Pod
	}
	var tasks []podTask
	for gi, g := range groups {
		for _, p := range g.runningPods {
			tasks = append(tasks, podTask{groupIdx: gi, pod: p, peers: g.allMembers})
		}
	}

	results := make([]podRuntimeControlResult, len(tasks))
	sem := make(chan struct{}, modelDeploymentRuntimeControlMaxConcurrency)
	var wg sync.WaitGroup
	for idx, task := range tasks {
		wg.Add(1)
		go func(i int, tk podTask) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = podRuntimeControlResult{
					condStatus: meta.ConditionUnknown,
					reason:     ModelDeploymentReasonRuntimeControlReconciling,
					message:    "runtime-control reconcile time budget exhausted",
					requeue:    true,
				}
				return
			}
			defer func() { <-sem }()

			results[i] = r.reconcilePodRuntimeControl(
				ctx,
				md,
				role,
				tk.pod,
				tk.peers,
				desiredRC,
				desiredPaused,
				hasSentinelParams,
				desiredDigest,
				needsRestoration,
				elasticInFlight,
				elasticInFlightMsg,
				serving,
				expectedRanks,
				parameterOnlyReplay,
				readEntry,
				saveLedger,
			)
		}(idx, task)
	}
	wg.Wait()

	if ledgerConflict.Load() {
		out.condStatus = meta.ConditionUnknown
		out.condReason = ModelDeploymentReasonRuntimeControlReconciling
		out.message = "retrying after runtime-control ledger optimistic lock conflict"
		out.requeue = true
		out.holdRole = desiredPaused
	}

	groupConvergedCounts := make([]int, len(groups))
	groupReadyCounts := make([]int, len(groups))
	unsupportedSet := make(map[string]struct{})
	anyPodPaused := false
	allConfirmedUnpaused := len(tasks) > 0
	var firstFalse *podRuntimeControlResult
	var firstUnknown *podRuntimeControlResult

	for idx, res := range results {
		gi := tasks[idx].groupIdx
		if res.observedPaused {
			anyPodPaused = true
		}
		if !res.confirmedUnpaused {
			allConfirmedUnpaused = false
		}
		for _, f := range res.unsupportedFields {
			unsupportedSet[f] = struct{}{}
		}
		if res.converged {
			groupConvergedCounts[gi]++
			if res.readyToServe {
				groupReadyCounts[gi]++
			}
		} else if !res.preserveEligibility {
			out.heldPods[tasks[idx].pod.UID] = true
		}
		if res.requeue {
			out.requeue = true
		}
		switch res.condStatus {
		case meta.ConditionFalse:
			if firstFalse == nil {
				cp := res
				firstFalse = &cp
			}
		case meta.ConditionUnknown:
			if firstUnknown == nil {
				cp := res
				firstUnknown = &cp
			}
		}
	}

	totalConverged := 0
	allGroupsConverged := len(groups) > 0
	allGroupsServeReady := len(groups) > 0
	wantsActiveServing := !desiredPaused && !parameterOnlyReplay

	if desiredPaused {
		allPreserve := len(tasks) > 0
		for _, res := range results {
			if !res.preserveEligibility {
				allPreserve = false
				break
			}
		}
		if !allPreserve {
			out.holdRole = true
		}
	}

	for gi, g := range groups {
		totalConverged += groupConvergedCounts[gi]
		groupFullyConverged := len(g.runningPods) == g.requiredCount && groupConvergedCounts[gi] == g.requiredCount
		if !groupFullyConverged {
			allGroupsConverged = false
		}
		groupServeReady := wantsActiveServing && groupFullyConverged && groupReadyCounts[gi] == g.requiredCount
		if !groupServeReady {
			allGroupsServeReady = false
			preserveGroup := len(g.runningPods) == g.requiredCount && len(g.runningPods) > 0
			for idx, tk := range tasks {
				if tk.groupIdx == gi && !results[idx].preserveEligibility {
					preserveGroup = false
					break
				}
			}
			if !preserveGroup {
				if g.seated {
					out.heldGroups[g.ordinal] = true
				} else {
					for _, m := range g.allMembers {
						out.heldPods[m.UID] = true
					}
				}
			}
		}
		if len(g.runningPods) < g.requiredCount {
			out.requeue = true
			if firstUnknown == nil {
				firstUnknown = &podRuntimeControlResult{
					condStatus: meta.ConditionUnknown,
					reason:     ModelDeploymentReasonRuntimeControlReconciling,
					message: fmt.Sprintf(
						"waiting for all %d answering Pods in role %q replica %d to run (%d running)",
						g.requiredCount, role.Name, g.ordinal, len(g.runningPods),
					),
				}
			}
		}
	}

	if wantsActiveServing && allGroupsConverged && !allGroupsServeReady {
		out.requeue = true
		if firstUnknown == nil {
			firstUnknown = &podRuntimeControlResult{
				condStatus: meta.ConditionUnknown,
				reason:     ModelDeploymentReasonRuntimeControlReconciling,
				message:    fmt.Sprintf("waiting for unpaused or acknowledged Pods of role %q to pass readiness probes", role.Name),
			}
		}
	}

	sortedUnsupported := sortedMapKeys(unsupportedSet)

	switch {
	case firstFalse != nil:
		out.condStatus = meta.ConditionFalse
		out.condReason = firstFalse.reason
		out.message = firstFalse.message
		out.publishStatus = true
	case firstUnknown != nil:
		out.condStatus = meta.ConditionUnknown
		out.condReason = firstUnknown.reason
		out.message = firstUnknown.message
		if !out.configured && !needsRestoration {
			out.publishStatus = false
		}
	case needsRestoration && allGroupsConverged && allGroupsServeReady:
		if ledger != nil && ledger.ResourceVersion != "" && md.UID != "" {
			if delErr := ledgerStore.DeleteIfMatch(ctx, owner, role.Name, ledger.ObjectUID, ledger.ResourceVersion); delErr != nil {
				return out, delErr
			}
		}
		out.allConverged = true
		out.configured = false
		out.publishStatus = false
		out.condStatus = meta.ConditionTrue
		out.condReason = ModelDeploymentReasonRuntimeControlNotConfigured
		out.message = ""
		return out, nil
	default:
		out.allConverged = allGroupsConverged && (!wantsActiveServing || allGroupsServeReady)
		if out.configured {
			out.condStatus = meta.ConditionTrue
			out.condReason = ModelDeploymentReasonRuntimeControlSynced
			out.message = ""
		} else {
			out.condStatus = meta.ConditionTrue
			out.condReason = ModelDeploymentReasonRuntimeControlNotConfigured
			out.message = ""
		}
	}

	if out.publishStatus {
		prevPaused := previousPausedState(md, role.Name)
		activePaused := prevPaused
		appliedDigest := previousAppliedDigest(md, role.Name)
		switch {
		case out.allConverged && out.condStatus == meta.ConditionTrue:
			activePaused = desiredPaused
			appliedDigest = desiredDigest
		case !desiredPaused:
			if allGroupsConverged && allConfirmedUnpaused && !anyPodPaused {
				activePaused = false
				appliedDigest = desiredDigest
			} else if prevPaused || anyPodPaused {
				// Keep observed pause fail-closed until every answering Pod positively confirms resume.
				activePaused = true
			}
		case anyPodPaused:
			// A partial pause that already paused at least one Pod must hold scaler writes immediately.
			activePaused = true
		}
		out.roleStatus = &workercore.ModelDeploymentRoleRuntimeControlStatus{
			ObservedGeneration:  md.Generation,
			Synced:              out.allConverged && out.condStatus == meta.ConditionTrue,
			Reason:              out.condReason,
			Message:             out.message,
			Paused:              activePaused,
			AppliedConfigDigest: appliedDigest,
			ConvergedPods:       int32(totalConverged),
			TotalAnsweringPods:  int32(totalAnswering),
			UnsupportedFields:   sortedUnsupported,
		}
	}
	return out, nil
}

// reconcileDefaultSentinelBootstrap acknowledges newly booted or restarted sentinel Pods on roles
// that do not configure pause or runtimeControl parameters, keeping RuntimeControlSynced True
// (NotConfigured) so unconfigured deployments never degrade status.phase.
func (r *ModelDeploymentReconciler) reconcileDefaultSentinelBootstrap(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pods []core.Pod,
	replacingOrdinals map[int]bool,
	emptyDigest string,
	out *roleRuntimeControlOutcome,
) {
	if !ModelDeploymentRoleSupportsEngineSentinel(md, role) && !roleHasRuntimeControlPod(role.Name, pods) {
		return
	}
	groups := resolveRoleAnsweringGroups(md, role, pods, replacingOrdinals)
	if len(groups) == 0 {
		return
	}

	hasInjectedTransport := r.RuntimeControlClient != nil || r.runtimeControlTarget != nil
	client := r.runtimeControlClient()

	for _, g := range groups {
		groupAllReady := len(g.allMembers) > 0
		for _, m := range g.allMembers {
			if !podIsReady(m) {
				groupAllReady = false
				break
			}
		}
		for _, pod := range g.runningPods {
			if !modelDeploymentPodHasRuntimeControlEnable(pod) {
				continue
			}
			// Once kubelet's readiness probe against /gpustack/v1/ready succeeds in production,
			// the Pod's boot latch is already acknowledged and unpaused.
			if podIsReady(pod) && !hasInjectedTransport {
				continue
			}
			target, err := r.resolveRuntimeControlTarget(ctx, md, role, pod)
			if err != nil {
				out.heldPods[pod.UID] = true
				out.requeue = true
				groupAllReady = false
				continue
			}
			readyResp, err := client.GetReady(ctx, target)
			if err != nil {
				out.heldPods[pod.UID] = true
				out.requeue = true
				groupAllReady = false
				continue
			}
			bootID := strings.TrimSpace(readyResp.BootID)
			if bootID == "" {
				out.heldPods[pod.UID] = true
				out.requeue = true
				groupAllReady = false
				continue
			}
			if readyResp.Reason == "role_paused" {
				if _, pauseErr := client.SetPause(ctx, target, runtimecontrol.PauseRequest{
					Paused:         false,
					ExpectedBootID: bootID,
				}); pauseErr != nil {
					out.heldPods[pod.UID] = true
					out.requeue = true
					groupAllReady = false
					continue
				}
			}
			needsAck := !readyResp.Ready || readyResp.ObservedGeneration == nil
			if needsAck {
				acked, ackErr := client.Ack(ctx, target, runtimecontrol.AckRequest{
					BootID:              bootID,
					ObservedGeneration:  md.Generation,
					AppliedConfigDigest: emptyDigest,
				})
				if ackErr != nil {
					out.heldPods[pod.UID] = true
					out.requeue = true
					groupAllReady = false
					continue
				}
				readyResp = acked
			}
			if !readyResp.Ready || !podIsReady(pod) {
				out.heldPods[pod.UID] = true
				out.requeue = true
				groupAllReady = false
			}
		}
		if !groupAllReady {
			if g.seated {
				out.heldGroups[g.ordinal] = true
			} else {
				for _, m := range g.allMembers {
					out.heldPods[m.UID] = true
				}
			}
		}
	}
}

type podRuntimeControlResult struct {
	converged           bool
	readyToServe        bool
	preserveEligibility bool
	condStatus          meta.ConditionStatus
	reason              string
	message             string
	unsupportedFields   []string
	requeue             bool
	observedPaused      bool
	confirmedUnpaused   bool
}

func (r *ModelDeploymentReconciler) reconcilePodRuntimeControl(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
	peers []*core.Pod,
	desiredRC *workercore.ModelDeploymentRoleRuntimeControl,
	desiredPaused bool,
	hasSentinelParams bool,
	desiredDigest string,
	needsRestoration bool,
	elasticInFlight bool,
	elasticInFlightMsg string,
	serving ServingAnswer,
	expectedRanks int,
	parameterOnlyReplay bool,
	readEntry func(string) (runtimecontrol.PodLedgerEntry, bool),
	saveLedger func(func(*runtimecontrol.RoleLedger)) error,
) podRuntimeControlResult {
	if !modelDeploymentPodHasRuntimeControlEnable(pod) {
		return r.reconcileNonSentinelPodRuntimeControl(
			ctx,
			md,
			role,
			pod,
			peers,
			desiredRC,
			desiredPaused,
			hasSentinelParams,
			desiredDigest,
			needsRestoration,
			elasticInFlight,
			elasticInFlightMsg,
			serving,
			readEntry,
			saveLedger,
		)
	}

	target, err := r.resolveRuntimeControlTarget(ctx, md, role, pod)
	if err != nil {
		return podRuntimeControlResult{
			condStatus: meta.ConditionFalse,
			reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
			message:    err.Error(),
			requeue:    true,
		}
	}

	client := r.runtimeControlClient()
	rcState, err := client.GetRuntimeConfig(ctx, target)
	if err != nil {
		res := classifyRuntimeControlReadError(pod.Name, err)
		if entry, ok := readEntry(pod.Name); ok && entry.PodUID == pod.UID &&
			(entry.Paused || pendingTargetImpliesPaused(entry.PendingTargetState)) {
			res.observedPaused = true
		}
		return res
	}

	bootID := strings.TrimSpace(rcState.BootID)
	if bootID == "" {
		return podRuntimeControlResult{
			condStatus: meta.ConditionUnknown,
			reason:     ModelDeploymentReasonRuntimeControlEndpointUnreachable,
			message:    fmt.Sprintf("Pod %s returned an empty runtime-control boot_id", pod.Name),
			requeue:    true,
		}
	}
	restartCount := int32(modelDeploymentMemberRestartCount(pod))

	entry, hasEntry := readEntry(pod.Name)
	if hasEntry && (entry.PodUID != pod.UID || (entry.BootID != "" && entry.BootID != bootID)) {
		entry = runtimecontrol.PodLedgerEntry{}
		hasEntry = false
	}

	effectiveObservedPaused := rcState.Paused ||
		(hasEntry && (entry.Paused || pendingTargetImpliesPaused(entry.PendingTargetState)))

	if hasEntry && (entry.AmbiguousOperationID != "" || entry.PendingOperationID != "") {
		cleared, fenceMsg := r.reconcileAmbiguousPodOperation(
			ctx, client, target, pod, bootID, restartCount, expectedRanks, entry, rcState, saveLedger,
		)
		if !cleared {
			return podRuntimeControlResult{
				condStatus:        meta.ConditionFalse,
				reason:            ModelDeploymentReasonRuntimeControlAmbiguousOperation,
				message:           fenceMsg,
				observedPaused:    effectiveObservedPaused,
				confirmedUnpaused: !effectiveObservedPaused,
			}
		}
		entry, hasEntry = readEntry(pod.Name)
		effectiveObservedPaused = rcState.Paused ||
			(hasEntry && (entry.Paused || pendingTargetImpliesPaused(entry.PendingTargetState)))
	}

	if unsupported := detectUnsupportedRuntimeFields(desiredRC, rcState); len(unsupported) > 0 {
		return podRuntimeControlResult{
			condStatus:        meta.ConditionFalse,
			reason:            ModelDeploymentReasonRuntimeControlUnsupportedEngineVersion,
			message:           fmt.Sprintf("Pod %s does not support runtimeControl fields: %s", pod.Name, strings.Join(unsupported, ", ")),
			unsupportedFields: unsupported,
			observedPaused:    effectiveObservedPaused,
			confirmedUnpaused: !effectiveObservedPaused,
			preserveEligibility: !desiredPaused && podIsReady(pod) &&
				pod.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue,
		}
	}

	stateAcked := rcState.Ready || rcState.ObservedGeneration != nil

	var desiredKeys []string
	if desiredRC != nil {
		desiredKeys = runtimecontrol.SentinelParameterKeys(desiredRC.Parameters)
	}
	livePaused := rcState.Paused
	if parameterOnlyReplay {
		// Parameter-only post-resize replay must never resume an already-paused master or engage
		// a new pause before router withdrawal is observed.
		desiredPaused = livePaused
		if replayDigest, digestErrMsg := computeRoleAppliedDigest(desiredRC, desiredPaused); digestErrMsg == "" {
			desiredDigest = replayDigest
		}
	}
	alreadyAtDesiredPause := livePaused == desiredPaused && (!desiredPaused || (hasEntry && entry.Paused))

	recordOp := func(opID, opKind string) error {
		stateStr := formatPendingState(opKind, desiredDigest, desiredPaused, md.Generation)
		return r.recordPendingPodOp(pod, bootID, restartCount, opID, stateStr, saveLedger)
	}

	if hasEntry &&
		entry.ConfirmedAppliedDigest == desiredDigest &&
		entry.Paused == desiredPaused &&
		entry.PendingOperationID == "" &&
		entry.AmbiguousOperationID == "" &&
		stateAcked &&
		rcState.AppliedConfigDigest == desiredDigest &&
		livePaused == desiredPaused &&
		(!hasSentinelParams || allRanksMatchDigest(rcState, desiredDigest, expectedRanks)) {
		if entry.ObservedGeneration != md.Generation {
			ackOpID := nextOperationID(pod.Name, "ack")
			if err := recordOp(ackOpID, "ack"); err != nil {
				res := podConflictOrErrorResult(err)
				res.observedPaused = livePaused
				return res
			}
			if _, ackErr := client.Ack(ctx, target, runtimecontrol.AckRequest{
				BootID:              bootID,
				ObservedGeneration:  md.Generation,
				AppliedConfigDigest: desiredDigest,
			}); ackErr != nil {
				res := r.handleMutatingError(pod, bootID, restartCount, ackOpID, ackErr, saveLedger)
				res.observedPaused = livePaused
				return res
			}
			if err := saveLedger(func(l *runtimecontrol.RoleLedger) {
				l.RecordConfirmedApply(
					pod.Name, pod.UID, bootID, restartCount, desiredDigest, md.Generation,
					desiredKeys, desiredPaused, r.modelDeploymentNow(),
				)
			}); err != nil {
				res := podConflictOrErrorResult(err)
				res.observedPaused = livePaused
				return res
			}
		}
		return podRuntimeControlResult{
			converged:         true,
			readyToServe:      !desiredPaused && podIsReady(pod),
			condStatus:        meta.ConditionTrue,
			reason:            ModelDeploymentReasonRuntimeControlSynced,
			observedPaused:    desiredPaused,
			confirmedUnpaused: !desiredPaused,
		}
	}

	if !parameterOnlyReplay && desiredPaused && !alreadyAtDesiredPause {
		if holdRes, held := r.gateRolePauseWithdrawalAndDrain(
			ctx, md, role, pod, peers, target, bootID, restartCount, elasticInFlight, elasticInFlightMsg, serving, true,
		); held {
			holdRes.observedPaused = livePaused
			return holdRes
		}
	}

	// Step 4: Clear sentinel pause first when returning to pause=false so parameter replay runs cleanly.
	if !desiredPaused && livePaused {
		opID := nextOperationID(pod.Name, "resume")
		if err := recordOp(opID, "resume"); err != nil {
			res := podConflictOrErrorResult(err)
			res.observedPaused = true
			return res
		}
		pauseResp, resumeErr := client.SetPause(ctx, target, runtimecontrol.PauseRequest{
			Paused:         false,
			ExpectedBootID: bootID,
		})
		if resumeErr != nil {
			res := r.handleMutatingError(pod, bootID, restartCount, opID, resumeErr, saveLedger)
			res.observedPaused = true
			return res
		}
		if pauseResp != nil && pauseResp.Paused {
			return podRuntimeControlResult{
				condStatus:     meta.ConditionFalse,
				reason:         ModelDeploymentReasonRuntimeControlReconcileFailed,
				message:        fmt.Sprintf("Pod %s remained paused after resume", pod.Name),
				requeue:        true,
				observedPaused: true,
			}
		}
		livePaused = false
	}

	// Step 5: Replay or reset runtimeControl.parameters across all engine ranks.
	var desiredParamMap map[string]string
	if desiredRC != nil {
		desiredParamMap = desiredRC.Parameters
	}
	desiredParams, buildErr := runtimecontrol.BuildSentinelParameters(desiredParamMap)
	if buildErr != nil {
		return podRuntimeControlResult{
			condStatus:     meta.ConditionFalse,
			reason:         ModelDeploymentReasonRuntimeControlReconcileFailed,
			message:        buildErr.Error(),
			observedPaused: livePaused,
		}
	}
	resetFields := computeSentinelResetFields(entry, rcState, desiredRC)
	needsConfigApply := len(resetFields) > 0 ||
		(len(desiredParams) > 0 && (!allRanksMatchDigest(rcState, desiredDigest, expectedRanks) || rcState.AppliedConfigDigest != desiredDigest))
	if needsConfigApply {
		opID := nextOperationID(pod.Name, "config")
		if err := recordOp(opID, "config"); err != nil {
			res := podConflictOrErrorResult(err)
			res.observedPaused = livePaused
			return res
		}
		var outParams map[string]any
		if len(desiredParams) > 0 {
			outParams = desiredParams
		}
		applyReq := runtimecontrol.ApplyRuntimeConfigRequest{
			OperationID:         opID,
			ExpectedBootID:      bootID,
			Generation:          md.Generation,
			AppliedConfigDigest: desiredDigest,
			Parameters:          outParams,
			ResetFields:         resetFields,
		}
		updatedState, applyErr := client.ApplyRuntimeConfig(ctx, target, applyReq)
		if applyErr != nil {
			res := r.handleMutatingError(pod, bootID, restartCount, opID, applyErr, saveLedger)
			res.observedPaused = livePaused
			return res
		}
		if !allRanksMatchDigest(updatedState, desiredDigest, expectedRanks) {
			return podRuntimeControlResult{
				condStatus:     meta.ConditionFalse,
				reason:         ModelDeploymentReasonRuntimeControlReconcileFailed,
				message:        fmt.Sprintf("Pod %s rank receipts did not converge on digest %s", pod.Name, desiredDigest),
				requeue:        true,
				observedPaused: livePaused,
			}
		}
	}

	// Step 6: Engage sentinel ASGI pause after withdrawal and drain complete.
	if desiredPaused && !livePaused {
		opID := nextOperationID(pod.Name, "pause")
		if err := recordOp(opID, "pause"); err != nil {
			res := podConflictOrErrorResult(err)
			res.observedPaused = livePaused
			return res
		}
		if _, pauseErr := client.SetPause(ctx, target, runtimecontrol.PauseRequest{
			Paused:         true,
			ExpectedBootID: bootID,
		}); pauseErr != nil {
			res := r.handleMutatingError(pod, bootID, restartCount, opID, pauseErr, saveLedger)
			res.observedPaused = true
			return res
		}
		livePaused = true
	}

	// Step 7: Acknowledge the boot-local readiness latch for (boot_id, generation, desiredDigest)
	// and record confirmed convergence in the sharded ledger.
	ackOpID := nextOperationID(pod.Name, "ack")
	if err := recordOp(ackOpID, "ack"); err != nil {
		res := podConflictOrErrorResult(err)
		res.observedPaused = livePaused
		return res
	}
	if _, ackErr := client.Ack(ctx, target, runtimecontrol.AckRequest{
		BootID:              bootID,
		ObservedGeneration:  md.Generation,
		AppliedConfigDigest: desiredDigest,
	}); ackErr != nil {
		res := r.handleMutatingError(pod, bootID, restartCount, ackOpID, ackErr, saveLedger)
		res.observedPaused = livePaused
		return res
	}

	if err := saveLedger(func(l *runtimecontrol.RoleLedger) {
		l.RecordConfirmedApply(
			pod.Name, pod.UID, bootID, restartCount, desiredDigest, md.Generation,
			desiredKeys, desiredPaused, r.modelDeploymentNow(),
		)
	}); err != nil {
		res := podConflictOrErrorResult(err)
		res.observedPaused = livePaused
		return res
	}

	return podRuntimeControlResult{
		converged:         true,
		readyToServe:      !desiredPaused && podIsReady(pod),
		condStatus:        meta.ConditionTrue,
		reason:            ModelDeploymentReasonRuntimeControlSynced,
		observedPaused:    desiredPaused,
		confirmedUnpaused: !desiredPaused,
	}
}

func (r *ModelDeploymentReconciler) reconcileNonSentinelPodRuntimeControl(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
	peers []*core.Pod,
	desiredRC *workercore.ModelDeploymentRoleRuntimeControl,
	desiredPaused bool,
	hasSentinelParams bool,
	desiredDigest string,
	needsRestoration bool,
	elasticInFlight bool,
	elasticInFlightMsg string,
	serving ServingAnswer,
	readEntry func(string) (runtimecontrol.PodLedgerEntry, bool),
	saveLedger func(func(*runtimecontrol.RoleLedger)) error,
) podRuntimeControlResult {
	if ModelDeploymentRoleSupportsEngineSentinel(md, role) {
		if !desiredPaused && !hasSentinelParams && !needsRestoration {
			return podRuntimeControlResult{
				converged:         true,
				readyToServe:      podIsReady(pod),
				condStatus:        meta.ConditionTrue,
				reason:            ModelDeploymentReasonRuntimeControlNotConfigured,
				confirmedUnpaused: true,
			}
		}
		return podRuntimeControlResult{
			condStatus: meta.ConditionUnknown,
			reason:     ModelDeploymentReasonRuntimeControlReconciling,
			message:    fmt.Sprintf("waiting for Pod %s to roll onto sentinel-enabled spec", pod.Name),
			requeue:    true,
		}
	}

	if hasSentinelParams {
		var keys []string
		if desiredRC != nil {
			keys = runtimecontrol.SentinelParameterKeys(desiredRC.Parameters)
		}
		return podRuntimeControlResult{
			condStatus:        meta.ConditionFalse,
			reason:            ModelDeploymentReasonRuntimeControlUnsupportedEngineVersion,
			message:           fmt.Sprintf("role %q does not support sentinel runtimeControl parameters on Pod %s", role.Name, pod.Name),
			unsupportedFields: keys,
			preserveEligibility: !desiredPaused && podIsReady(pod) &&
				pod.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue,
		}
	}

	if !desiredPaused && !needsRestoration {
		return podRuntimeControlResult{
			converged:         true,
			readyToServe:      podIsReady(pod),
			condStatus:        meta.ConditionTrue,
			reason:            ModelDeploymentReasonRuntimeControlNotConfigured,
			confirmedUnpaused: true,
		}
	}

	restartCount := int32(modelDeploymentMemberRestartCount(pod))
	bootID := fmt.Sprintf("pod-%s-%d", pod.UID, restartCount)
	entry, hasEntry := readEntry(pod.Name)
	if hasEntry && (entry.PodUID != pod.UID || entry.RestartCount != restartCount) {
		entry = runtimecontrol.PodLedgerEntry{}
		hasEntry = false
	}

	if desiredPaused {
		if hasEntry && entry.Paused && entry.ConfirmedAppliedDigest == desiredDigest {
			if entry.ObservedGeneration != md.Generation {
				if err := saveLedger(func(l *runtimecontrol.RoleLedger) {
					l.RecordConfirmedApply(
						pod.Name, pod.UID, bootID, restartCount, desiredDigest, md.Generation,
						nil, true, r.modelDeploymentNow(),
					)
				}); err != nil {
					res := podConflictOrErrorResult(err)
					res.observedPaused = true
					return res
				}
			}
			return podRuntimeControlResult{
				converged:      true,
				readyToServe:   false,
				condStatus:     meta.ConditionTrue,
				reason:         ModelDeploymentReasonRuntimeControlSynced,
				observedPaused: true,
			}
		}

		canDrain := (len(role.Command) == 0 || r.DrainReader != nil) && pod.Status.PodIP != ""
		var target runtimecontrol.Target
		if canDrain {
			var err error
			target, err = r.resolveRuntimeControlTarget(ctx, md, role, pod)
			if err != nil {
				return podRuntimeControlResult{
					condStatus: meta.ConditionFalse,
					reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
					message:    err.Error(),
					requeue:    true,
				}
			}
		}
		if holdRes, held := r.gateRolePauseWithdrawalAndDrain(
			ctx, md, role, pod, peers, target, bootID, restartCount, elasticInFlight, elasticInFlightMsg, serving, canDrain,
		); held {
			return holdRes
		}

		if err := saveLedger(func(l *runtimecontrol.RoleLedger) {
			l.RecordConfirmedApply(
				pod.Name, pod.UID, bootID, restartCount, desiredDigest, md.Generation,
				nil, true, r.modelDeploymentNow(),
			)
		}); err != nil {
			return podConflictOrErrorResult(err)
		}
		return podRuntimeControlResult{
			converged:      true,
			readyToServe:   false,
			condStatus:     meta.ConditionTrue,
			reason:         ModelDeploymentReasonRuntimeControlSynced,
			observedPaused: true,
		}
	}

	if err := saveLedger(func(l *runtimecontrol.RoleLedger) {
		l.RecordConfirmedApply(
			pod.Name, pod.UID, bootID, restartCount, desiredDigest, md.Generation,
			nil, false, r.modelDeploymentNow(),
		)
	}); err != nil {
		return podConflictOrErrorResult(err)
	}
	return podRuntimeControlResult{
		converged:         true,
		readyToServe:      podIsReady(pod),
		condStatus:        meta.ConditionTrue,
		reason:            ModelDeploymentReasonRuntimeControlSynced,
		confirmedUnpaused: true,
	}
}

func (r *ModelDeploymentReconciler) gateRolePauseWithdrawalAndDrain(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
	peers []*core.Pod,
	target runtimecontrol.Target,
	bootID string,
	restartCount int32,
	elasticInFlight bool,
	elasticInFlightMsg string,
	serving ServingAnswer,
	runDrain bool,
) (podRuntimeControlResult, bool) {
	if elasticInFlight {
		msg := elasticInFlightMsg
		if msg == "" {
			msg = "holding role pause transition while elastic EP resize is in flight"
		}
		return podRuntimeControlResult{
			condStatus:          meta.ConditionUnknown,
			reason:              ModelDeploymentReasonRuntimeControlReconciling,
			message:             msg,
			preserveEligibility: true,
			requeue:             true,
		}, true
	}

	// Step 1: Withdraw endpoint eligibility across the Pod and its replica peers before draining.
	if anyPeerCarriesEligibility(pod, peers) {
		return podRuntimeControlResult{
			condStatus: meta.ConditionUnknown,
			reason:     ModelDeploymentReasonRuntimeControlDraining,
			message:    fmt.Sprintf("withdrawing endpoint eligibility on Pod %s before role pause", pod.Name),
			requeue:    true,
		}, true
	}

	// Step 2: When a managed router is configured, wait until router observation confirms withdrawal.
	if md.Spec.Router != nil && !r.routerConfirmsRoleWithdrawn(role.Name, serving) {
		return podRuntimeControlResult{
			condStatus: meta.ConditionUnknown,
			reason:     ModelDeploymentReasonRuntimeControlDraining,
			message:    fmt.Sprintf("waiting for router observation to confirm withdrawal of role %q", role.Name),
			requeue:    true,
		}, true
	}

	// Step 3: Verify incarnation-relative in-flight request drain on (podUID, bootID).
	if runDrain {
		reading, drainErr := r.runtimeControlDrainReader().Read(ctx, runtimecontrol.DrainTarget{
			Target:       target,
			Engine:       md.Spec.Engine.Name,
			PodUID:       pod.UID,
			BootID:       bootID,
			RestartCount: restartCount,
		})
		if drainErr != nil {
			return podRuntimeControlResult{
				condStatus: meta.ConditionFalse,
				reason:     ModelDeploymentReasonRuntimeControlDrainFailed,
				message:    fmt.Sprintf("drain check failed on Pod %s: %v", pod.Name, drainErr),
				requeue:    true,
			}, true
		}
		if !reading.Drained {
			return podRuntimeControlResult{
				condStatus: meta.ConditionUnknown,
				reason:     ModelDeploymentReasonRuntimeControlDraining,
				message: fmt.Sprintf(
					"draining Pod %s before role pause (running=%.0f, waiting=%.0f)",
					pod.Name, reading.Running, reading.Waiting,
				),
				requeue: true,
			}, true
		}
	}

	return podRuntimeControlResult{}, false
}

func (r *ModelDeploymentReconciler) reconcileAmbiguousPodOperation(
	ctx context.Context,
	client ModelDeploymentRuntimeControlClient,
	target runtimecontrol.Target,
	pod *core.Pod,
	bootID string,
	restartCount int32,
	expectedRanks int,
	entry runtimecontrol.PodLedgerEntry,
	rcState *runtimecontrol.RuntimeConfigState,
	saveLedger func(func(*runtimecontrol.RoleLedger)) error,
) (bool, string) {
	paused := rcState.Paused
	if pauseResp, err := client.GetPause(ctx, target); err == nil && pauseResp != nil {
		paused = pauseResp.Paused
	}

	opID := entry.AmbiguousOperationID
	if opID == "" {
		opID = entry.PendingOperationID
	}

	opKind, targetDigest, targetGen := parsePendingState(entry.PendingTargetState)
	settled := false
	switch opKind {
	case "resume":
		settled = !paused
	case "pause":
		settled = paused
	case "config":
		opMatches := opID != "" && rcState.LastOperationID == opID
		settled = targetDigest != "" &&
			opMatches &&
			rcState.AppliedConfigDigest == targetDigest &&
			allRanksMatchDigest(rcState, targetDigest, expectedRanks)
	case "ack":
		genMatches := targetGen <= 0 || (rcState.ObservedGeneration != nil && *rcState.ObservedGeneration == targetGen)
		digestMatches := targetDigest == "" || rcState.AppliedConfigDigest == targetDigest
		settled = (rcState.Ready || rcState.ObservedGeneration != nil) && digestMatches && genMatches
	}

	if settled {
		if saveErr := saveLedger(func(l *runtimecontrol.RoleLedger) {
			cur, ok := l.GetEntry(pod.Name)
			if !ok || cur.PodUID != pod.UID || cur.BootID != bootID {
				return
			}
			cur.PendingOperationID = ""
			cur.PendingTargetState = ""
			cur.AmbiguousOperationID = ""
			cur.AmbiguousReason = ""
			cur.RestartCount = restartCount
			if opKind == "ack" {
				if targetGen > cur.ObservedGeneration {
					cur.ObservedGeneration = targetGen
				}
				if targetDigest != "" {
					cur.ConfirmedAppliedDigest = targetDigest
				}
			}
			l.SetEntry(pod.Name, cur)
		}); saveErr != nil {
			return false, fmt.Sprintf("failed to persist settled operation %s on Pod %s: %v", opID, pod.Name, saveErr)
		}
		return true, ""
	}

	reason := entry.AmbiguousReason
	if reason == "" {
		reason = "unfinished pending operation recovered after controller interruption"
		now := r.modelDeploymentNow()
		if saveErr := saveLedger(func(l *runtimecontrol.RoleLedger) {
			l.RecordAmbiguousOperation(pod.Name, pod.UID, bootID, restartCount, opID, reason, now)
		}); saveErr != nil {
			return false, fmt.Sprintf("fenced unfinished operation %s on Pod %s (ledger persist error: %v)", opID, pod.Name, saveErr)
		}
	}
	msg := fmt.Sprintf("fenced ambiguous operation %s on Pod %s (%s)", opID, pod.Name, reason)
	return false, msg
}

func (r *ModelDeploymentReconciler) recordPendingPodOp(
	pod *core.Pod,
	bootID string,
	restartCount int32,
	opID string,
	targetState string,
	saveLedger func(func(*runtimecontrol.RoleLedger)) error,
) error {
	now := r.modelDeploymentNow()
	return saveLedger(func(l *runtimecontrol.RoleLedger) {
		l.RecordPendingOperation(pod.Name, pod.UID, bootID, restartCount, opID, targetState, now)
	})
}

func (r *ModelDeploymentReconciler) handleMutatingError(
	pod *core.Pod,
	bootID string,
	restartCount int32,
	opID string,
	err error,
	saveLedger func(func(*runtimecontrol.RoleLedger)) error,
) podRuntimeControlResult {
	if runtimecontrol.IsAmbiguousOperation(err) {
		now := r.modelDeploymentNow()
		saveErr := saveLedger(func(l *runtimecontrol.RoleLedger) {
			l.RecordAmbiguousOperation(pod.Name, pod.UID, bootID, restartCount, opID, err.Error(), now)
		})
		msg := fmt.Sprintf("ambiguous POST timeout on Pod %s (%s): %v", pod.Name, opID, err)
		if saveErr != nil {
			msg = fmt.Sprintf("%s (ledger fence save failed: %v)", msg, saveErr)
		}
		return podRuntimeControlResult{
			condStatus: meta.ConditionFalse,
			reason:     ModelDeploymentReasonRuntimeControlAmbiguousOperation,
			message:    msg,
			requeue:    saveErr != nil,
		}
	}

	clearDefinitivePending := func() error {
		return saveLedger(func(l *runtimecontrol.RoleLedger) {
			cur, ok := l.GetEntry(pod.Name)
			if !ok || cur.PodUID != pod.UID || cur.BootID != bootID {
				return
			}
			if cur.PendingOperationID == opID {
				cur.PendingOperationID = ""
				cur.PendingTargetState = ""
				l.SetEntry(pod.Name, cur)
			}
		})
	}

	var unsup *runtimecontrol.UnsupportedFieldsError
	if errors.As(err, &unsup) {
		clearErr := clearDefinitivePending()
		msg := fmt.Sprintf("Pod %s rejected unsupported fields: %v", pod.Name, err)
		if clearErr != nil {
			msg = fmt.Sprintf("%s (failed to clear pending ledger entry: %v)", msg, clearErr)
		}
		return podRuntimeControlResult{
			condStatus:        meta.ConditionFalse,
			reason:            ModelDeploymentReasonRuntimeControlUnsupportedEngineVersion,
			message:           msg,
			unsupportedFields: slices.Clone(unsup.UnsupportedFields),
			requeue:           clearErr != nil,
			preserveEligibility: clearErr == nil && podIsReady(pod) &&
				pod.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue,
		}
	}

	// Definitive 4xx refusals (e.g., value above actual engine boot ceiling or boot ID mismatch)
	// did not apply; clear the pending intent durably so a corrected spec is not fenced.
	var statusErr *runtimecontrol.HTTPStatusError
	if errors.Is(err, runtimecontrol.ErrInvalidArgument) ||
		errors.Is(err, runtimecontrol.ErrBootIDMismatch) ||
		(errors.As(err, &statusErr) &&
			(statusErr.StatusCode == http.StatusBadRequest ||
				statusErr.StatusCode == http.StatusUnprocessableEntity)) {
		if clearErr := clearDefinitivePending(); clearErr != nil {
			return podRuntimeControlResult{
				condStatus: meta.ConditionFalse,
				reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
				message:    fmt.Sprintf("runtime-control apply rejected on Pod %s (%v) and pending clear failed: %v", pod.Name, err, clearErr),
				requeue:    true,
			}
		}
	}

	return podRuntimeControlResult{
		condStatus: meta.ConditionFalse,
		reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
		message:    fmt.Sprintf("runtime-control apply failed on Pod %s: %v", pod.Name, err),
		requeue:    true,
	}
}

func podConflictOrErrorResult(err error) podRuntimeControlResult {
	if runtimecontrol.IsLedgerConflict(err) {
		return podRuntimeControlResult{
			condStatus: meta.ConditionUnknown,
			reason:     ModelDeploymentReasonRuntimeControlReconciling,
			message:    "retrying after ledger conflict",
			requeue:    true,
		}
	}
	return podRuntimeControlResult{
		condStatus: meta.ConditionFalse,
		reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
		message:    err.Error(),
		requeue:    true,
	}
}

func classifyRuntimeControlReadError(podName string, err error) podRuntimeControlResult {
	var statusErr *runtimecontrol.HTTPStatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden) {
		return podRuntimeControlResult{
			condStatus: meta.ConditionFalse,
			reason:     ModelDeploymentReasonRuntimeControlReconcileFailed,
			message:    fmt.Sprintf("sentinel request rejected on Pod %s: %v", podName, err),
			requeue:    true,
		}
	}
	return podRuntimeControlResult{
		condStatus: meta.ConditionUnknown,
		reason:     ModelDeploymentReasonRuntimeControlEndpointUnreachable,
		message:    fmt.Sprintf("Pod %s runtime-control endpoint is unreachable: %v", podName, err),
		requeue:    true,
	}
}

func (r *ModelDeploymentReconciler) resolveRuntimeControlTarget(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
) (runtimecontrol.Target, error) {
	if r.runtimeControlTarget != nil {
		return r.runtimeControlTarget(ctx, md, role, pod)
	}
	port := ModelDeploymentRoleServingPort(md, role)
	if rawPort := strings.TrimSpace(pod.Annotations["prometheus.io/port"]); rawPort != "" {
		if parsed, err := strconv.ParseInt(rawPort, 10, 32); err == nil && parsed > 0 {
			port = int32(parsed)
		}
	}
	scheme := strings.ToLower(string(ModelDeploymentRoleServingScheme(md.Spec.Engine.Name, role)))
	if rawScheme := strings.ToLower(strings.TrimSpace(pod.Annotations["prometheus.io/scheme"])); rawScheme == "http" || rawScheme == "https" {
		scheme = rawScheme
	}
	if scheme == "" {
		scheme = "http"
	}
	return runtimecontrol.Target{
		Scheme:             scheme,
		Host:               pod.Status.PodIP,
		Port:               int(port),
		InsecureSkipVerify: scheme == "https",
	}, nil
}

func (r *ModelDeploymentReconciler) routerConfirmsRoleWithdrawn(roleName string, serving ServingAnswer) bool {
	if serving.State != workercore.ModelDeploymentServingStateConfirmed {
		return false
	}
	if !r.modelDeploymentServingFreshNow(serving) {
		return false
	}
	return serving.ByRole[roleName] == 0
}

func anyPeerCarriesEligibility(pod *core.Pod, peers []*core.Pod) bool {
	if podCarriesRoutingLabels(pod) {
		return true
	}
	for _, peer := range peers {
		if podCarriesRoutingLabels(peer) {
			return true
		}
	}
	return false
}

func podCarriesRoutingLabels(pod *core.Pod) bool {
	if pod == nil {
		return false
	}
	return pod.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue ||
		pod.Labels[modelDeploymentRouterCandidateLabel] == modelDeploymentEndpointEligibleValue ||
		pod.Labels[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue
}

func previousPausedState(md *workercore.ModelDeployment, roleName string) bool {
	if md != nil {
		for i := range md.Status.Roles {
			if md.Status.Roles[i].Name == roleName && md.Status.Roles[i].RuntimeControl != nil {
				return md.Status.Roles[i].RuntimeControl.Paused
			}
		}
	}
	return false
}

func previousAppliedDigest(md *workercore.ModelDeployment, roleName string) string {
	if md != nil {
		for i := range md.Status.Roles {
			if md.Status.Roles[i].Name == roleName && md.Status.Roles[i].RuntimeControl != nil {
				return md.Status.Roles[i].RuntimeControl.AppliedConfigDigest
			}
		}
	}
	return ""
}

func allRanksMatchDigest(state *runtimecontrol.RuntimeConfigState, wantDigest string, expectedRanks int) bool {
	if state == nil || len(state.Ranks) == 0 {
		return false
	}
	if expectedRanks > 0 && len(state.Ranks) != expectedRanks {
		return false
	}
	seen := make(map[int]struct{}, len(state.Ranks))
	for i := range state.Ranks {
		r := state.Ranks[i]
		if expectedRanks > 0 && (r.RankID < 0 || r.RankID >= expectedRanks) {
			return false
		}
		if _, dup := seen[r.RankID]; dup {
			return false
		}
		seen[r.RankID] = struct{}{}
		if strings.TrimSpace(r.BootID) == "" || r.AppliedDigest != wantDigest {
			return false
		}
	}
	return true
}

func detectUnsupportedRuntimeFields(
	rc *workercore.ModelDeploymentRoleRuntimeControl,
	state *runtimecontrol.RuntimeConfigState,
) []string {
	if rc == nil || state == nil {
		return nil
	}
	requested := runtimecontrol.SentinelParameterKeys(rc.Parameters)
	if len(requested) == 0 {
		return nil
	}
	unsupportedSet := make(map[string]struct{}, len(state.UnsupportedFields))
	for _, f := range state.UnsupportedFields {
		unsupportedSet[f] = struct{}{}
	}
	supportedSet := make(map[string]struct{}, len(state.SupportedFields))
	for _, f := range state.SupportedFields {
		supportedSet[f] = struct{}{}
	}

	found := make(map[string]struct{})
	for _, reqField := range requested {
		if _, bad := unsupportedSet[reqField]; bad {
			found[reqField] = struct{}{}
			continue
		}
		if len(supportedSet) > 0 {
			if _, ok := supportedSet[reqField]; !ok {
				found[reqField] = struct{}{}
			}
		}
	}
	return sortedMapKeys(found)
}

func computeSentinelResetFields(
	entry runtimecontrol.PodLedgerEntry,
	liveState *runtimecontrol.RuntimeConfigState,
	desiredRC *workercore.ModelDeploymentRoleRuntimeControl,
) []string {
	prevSet := make(map[string]struct{}, len(entry.AppliedKeys))
	for _, k := range entry.AppliedKeys {
		prevSet[k] = struct{}{}
	}
	if liveState != nil {
		for k := range liveState.Values {
			if _, ok := runtimecontrol.LookupParameter(k); ok {
				prevSet[k] = struct{}{}
			}
		}
	}
	if len(prevSet) == 0 {
		return nil
	}
	prevKeys := sortedMapKeys(prevSet)
	var desiredMap map[string]string
	if desiredRC != nil {
		desiredMap = desiredRC.Parameters
	}
	rawResets := runtimecontrol.ComputeResetFields(desiredMap, prevKeys)
	if len(rawResets) == 0 || liveState == nil || len(liveState.SupportedFields) == 0 {
		return rawResets
	}
	supportedSet := make(map[string]struct{}, len(liveState.SupportedFields))
	for _, f := range liveState.SupportedFields {
		supportedSet[f] = struct{}{}
	}
	filtered := make([]string, 0, len(rawResets))
	for _, f := range rawResets {
		if _, ok := supportedSet[f]; ok {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

func ledgerHasNonDefaultState(ledger *runtimecontrol.RoleLedger, emptyDigest string) bool {
	if ledger == nil || len(ledger.Entries) == 0 {
		return false
	}
	for _, entry := range ledger.Entries {
		if entry.Paused || len(entry.AppliedKeys) > 0 {
			return true
		}
		if entry.ConfirmedAppliedDigest != "" && entry.ConfirmedAppliedDigest != emptyDigest {
			return true
		}
		if entry.PendingOperationID != "" || entry.AmbiguousOperationID != "" {
			return true
		}
	}
	return false
}

func roleHasRuntimeControlPod(roleName string, pods []core.Pod) bool {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil && modelDeploymentPodRole(p) == roleName && modelDeploymentPodHasRuntimeControlEnable(p) {
			return true
		}
	}
	return false
}

func roleHasReadyEligiblePods(roleName string, pods []core.Pod) bool {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil &&
			modelDeploymentPodRole(p) == roleName &&
			podIsReady(p) &&
			p.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue {
			return true
		}
	}
	return false
}

func nextOperationID(podName, action string) string {
	seq := runtimeControlOperationSeq.Add(1)
	return fmt.Sprintf("%s-%s-%s-%d", podName, action, runtimeControlProcessNonce, seq)
}

func formatPendingState(opKind, digest string, paused bool, generation int64) string {
	return fmt.Sprintf("op=%s;digest=%s;paused=%t;gen=%d", opKind, digest, paused, generation)
}

func pendingTargetImpliesPaused(raw string) bool {
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if ok && k == "paused" {
			return v == "true"
		}
	}
	return false
}

func parsePendingState(raw string) (opKind, digest string, generation int64) {
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "op":
			opKind = v
		case "digest":
			digest = v
		case "gen":
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				generation = parsed
			}
		}
	}
	return opKind, digest, generation
}

func computeRoleAppliedDigest(rc *workercore.ModelDeploymentRoleRuntimeControl, paused bool) (string, string) {
	digest, err := runtimecontrol.ComputeAppliedConfigDigest(rc, paused)
	if err != nil {
		return "", err.Error()
	}
	return digest, ""
}

func sortedMapKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
