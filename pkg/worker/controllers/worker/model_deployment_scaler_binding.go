package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

type modelDeploymentScalerDecisionAnnotation struct {
	ScalerUID        string           `json:"scalerUID"`
	DecisionID       string           `json:"decisionID"`
	Term             int64            `json:"term"`
	Vector           map[string]int32 `json:"vector"`
	BaselineVector   map[string]int32 `json:"baselineVector,omitempty"`
	Timestamp        *meta.Time       `json:"timestamp,omitempty"`
	TargetGeneration int64            `json:"targetGeneration,omitempty"`
}

func readScalerBinding(md *workercore.ModelDeployment) (workercore.ModelDeploymentScalerBinding, bool, error) {
	value := md.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]
	if value == "" {
		return workercore.ModelDeploymentScalerBinding{}, false, nil
	}
	var binding workercore.ModelDeploymentScalerBinding
	if err := json.Unmarshal([]byte(value), &binding); err != nil {
		return binding, false, fmt.Errorf("decode target scaler binding: %w", err)
	}
	return binding, true, nil
}

func (r *ModelDeploymentScalerReconciler) releaseOwnedBinding(
	ctx context.Context, scaler *workercore.ModelDeploymentScaler, md *workercore.ModelDeployment,
) error {
	binding, ok, err := readScalerBinding(md)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ignoring invalid scaler binding annotation", "target", md.Name)
		return nil
	}
	changed := false
	if ok && binding.ScalerUID == string(scaler.UID) {
		delete(md.Annotations, workercore.ModelDeploymentScalerBindingAnnotationKey)
		changed = true
	}
	if !scaler.DeletionTimestamp.IsZero() {
		if raw := md.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey]; raw != "" {
			var decision modelDeploymentScalerDecisionAnnotation
			if json.Unmarshal([]byte(raw), &decision) == nil && decision.ScalerUID == string(scaler.UID) {
				delete(md.Annotations, workercore.ModelDeploymentScalerDecisionAnnotationKey)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return r.Client.Update(ctx, md)
}

func (r *ModelDeploymentScalerReconciler) recoverDecision(
	ctx context.Context, scaler *workercore.ModelDeploymentScaler, md *workercore.ModelDeployment,
) {
	value := md.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey]
	if value == "" {
		return
	}
	var decision modelDeploymentScalerDecisionAnnotation
	if err := json.Unmarshal([]byte(value), &decision); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ignoring invalid scaler decision annotation", "target", md.Name)
		return
	}
	if decision.ScalerUID != string(scaler.UID) {
		return
	}
	binding, ok, err := readScalerBinding(md)
	if err != nil || (ok && (binding.ScalerUID != string(scaler.UID) || decision.Term != binding.Term)) {
		return
	}
	if scaler.Status.Binding != nil && (decision.Term != scaler.Status.Binding.Term ||
		(scaler.Status.Binding.TargetUID != "" && scaler.Status.Binding.TargetUID != string(md.UID))) {
		return
	}
	if !equalReplicaVectors(deploymentReplicaVector(md), decision.Vector) {
		return
	}
	if scaler.Status.LastCommit == nil || scaler.Status.LastCommit.Term != decision.Term ||
		scaler.Status.LastCommit.DecisionID != decision.DecisionID ||
		!equalReplicaVectors(scaler.Status.LastCommit.Vector, decision.Vector) {
		scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
			DecisionID: decision.DecisionID, Term: decision.Term, Vector: cloneReplicaVector(decision.Vector),
			BaselineVector: cloneReplicaVector(decision.BaselineVector), Timestamp: decision.Timestamp,
			TargetGeneration: decision.TargetGeneration,
		}
		if scaler.Status.LastCommit.TargetGeneration == 0 {
			scaler.Status.LastCommit.TargetGeneration = md.Generation
		}
	}
	if scaler.Status.Binding == nil || scaler.Status.Binding.TargetUID == "" {
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: decision.Term}
	}
}

func (r *ModelDeploymentScalerReconciler) writeVector(
	ctx context.Context,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	vector map[string]int32,
	baseline map[string]int32,
	now time.Time,
) (string, error) {
	term := scalerTerm(scaler)
	decisionID := fmt.Sprintf("%s-%d-%d", scaler.UID, term, now.UnixNano())
	for i := range md.Spec.Roles {
		if replicas, ok := vector[md.Spec.Roles[i].Name]; ok {
			md.Spec.Roles[i].Replicas = replicas
		}
	}
	decision, err := json.Marshal(modelDeploymentScalerDecisionAnnotation{
		ScalerUID: string(scaler.UID), DecisionID: decisionID, Term: term, Vector: cloneReplicaVector(vector),
		BaselineVector: cloneReplicaVector(baseline), Timestamp: timePointer(now), TargetGeneration: md.Generation + 1,
	})
	if err != nil {
		return "", err
	}
	if md.Annotations == nil {
		md.Annotations = make(map[string]string)
	}
	md.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey] = string(decision)
	if err := r.Client.Update(ctx, md); err != nil {
		return "", err
	}
	return decisionID, nil
}

func scalerTerm(scaler *workercore.ModelDeploymentScaler) int64 {
	if scaler.Status.Binding == nil {
		return 0
	}
	return scaler.Status.Binding.Term
}
