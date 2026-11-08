package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// TransferEntitlement is the private source identity of an existing session.
type TransferEntitlement struct {
	Namespace, PodName, PodUID, TargetInstanceID string
	ArtifactName, ArtifactUID, Digest            string
	SourceGeneration                             int64
}

// TransferSession returns the owner and artifact needed for live authorization.
func (b *Broker) TransferSession(id, podUID, targetInstance string) (TransferEntitlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[id]
	if session == nil || session.OwnerUID != podUID || session.TargetInstanceID != targetInstance || session.Phase != "admitted" {
		return TransferEntitlement{}, errors.New("the session is unavailable")
	}
	rec := b.records[session.Generation]
	if rec == nil || !b.serving(rec) || rec.ArtifactName == "" {
		return TransferEntitlement{}, errors.New("the source is unavailable")
	}
	return TransferEntitlement{
		Namespace: session.Namespace, PodUID: session.OwnerUID, TargetInstanceID: session.TargetInstanceID,
		ArtifactName: rec.ArtifactName, ArtifactUID: session.ArtifactUID, Digest: session.Digest, SourceGeneration: rec.SourceGeneration,
	}, nil
}

func validTarget(target TransferTarget, length int64, podIP string) bool {
	if target.Address == 0 || target.Length <= 0 || target.Length > protocol.MaxWindowBytes || length <= 0 || length > target.Length ||
		target.Address > math.MaxUint64-uint64(target.Length) || len(target.Metadata) > maxControlBody/2 || len(target.Endpoint) > 512 {
		return false
	}
	switch target.Backend {
	case "nixl":
		if target.Metadata == "" {
			return false
		}
		_, err := base64.StdEncoding.Strict().DecodeString(target.Metadata)
		return err == nil
	case "hixl":
		host, port, err := net.SplitHostPort(target.Endpoint)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(port)
		ip, expected := net.ParseIP(host), net.ParseIP(podIP)
		return err == nil && n > 0 && n <= 65535 && ip != nil && expected != nil && ip.Equal(expected)
	default:
		return false
	}
}

func validTransferIdentifiers(req TransferRequest) bool {
	return sessionNoncePattern.MatchString(req.RequestID) && sessionNoncePattern.MatchString(req.TargetInstanceID) &&
		sessionNoncePattern.MatchString(req.RegistrationID) && sessionNoncePattern.MatchString(req.WindowID) &&
		req.SessionID != "" && req.PodNamespace != "" && req.PodName != "" && req.PodUID != "" &&
		req.SourcePodUID != "" && req.WorkerInstanceID != "" && req.Generation != "" && req.Tensor != ""
}

func sameWindow(a, b TransferRequest) bool {
	return a.TargetInstanceID == b.TargetInstanceID &&
		a.RegistrationID == b.RegistrationID && a.WindowID == b.WindowID
}

func privateCommon(op *OperationRecord, requestID string) map[string]any {
	return map[string]any{
		"sourcePodUID": op.Request.SourcePodUID, "workerInstanceID": op.Request.WorkerInstanceID,
		"generation": op.Request.Generation, "leaseID": op.SourceLeaseID, "sessionID": op.Request.SessionID, "requestID": requestID,
	}
}

func privateSubmit(op *OperationRecord) map[string]any {
	body := privateCommon(op, op.Request.RequestID)
	body["targetPodUID"], body["targetInstanceID"] = op.Request.PodUID, op.Request.TargetInstanceID
	body["registrationID"], body["windowID"] = op.Request.RegistrationID, op.Request.WindowID
	body["tensor"], body["file"], body["fileOffset"] = op.Request.Tensor, op.File, op.FileOffset
	body["tensorOffset"], body["length"], body["target"] = op.Request.TensorOffset, op.Request.Length, op.Request.Target
	return body
}

func (b *Broker) operationOwnerLocked(req TransferRequest, pod authorization.PodIdentity) (*SessionRecord, *GenerationRecord, error) {
	if !validTransferIdentifiers(req) {
		return nil, nil, errors.New("operation identity is unavailable")
	}
	return b.sessionOwnerLocked(req, pod)
}

func (b *Broker) sessionOwnerLocked(req TransferRequest, pod authorization.PodIdentity) (*SessionRecord, *GenerationRecord, error) {
	if !b.operationsReady || b.privateSource == nil || !sessionNoncePattern.MatchString(req.TargetInstanceID) ||
		req.SessionID == "" || req.PodNamespace == "" || req.PodName == "" || req.PodUID == "" ||
		pod.Namespace != req.PodNamespace || pod.Name != req.PodName || pod.UID != req.PodUID {
		return nil, nil, errors.New("operation identity is unavailable")
	}
	session := b.sessions[req.SessionID]
	if session == nil || session.Phase != "admitted" || session.OwnerUID != pod.UID || session.Namespace != pod.Namespace ||
		session.TargetInstanceID != req.TargetInstanceID || session.Generation != req.Generation ||
		session.SourcePodUID != req.SourcePodUID || session.SourceInstanceID != req.WorkerInstanceID || session.SourceLeaseID == "" {
		return nil, nil, errors.New("session owner or source incarnation changed")
	}
	rec := b.records[session.Generation]
	if rec == nil || !b.serving(rec) || rec.LeaseID != session.SourceLeaseID || rec.NativeInstance != session.SourceInstanceID ||
		rec.WorkerID != session.SourcePodUID || rec.ArtifactUID != session.ArtifactUID || rec.Digest != session.Digest {
		return nil, nil, errors.New("source generation is unavailable")
	}
	if b.assignmentsFrozen || b.intentValidator == nil {
		return nil, nil, errors.New("source entitlement is unavailable")
	}
	return session, rec, nil
}

// SubmitTransfer durably prepares and marks dispatch before the single private POST.
// The manifest reads and the live entitlement check run without the broker
// mutex, bracketed by owner and identity revalidation before anything is
// durably prepared; a source that moved while the caller waited is refused
// without publishing an operation.
func (b *Broker) SubmitTransfer(ctx context.Context, req TransferRequest, pod authorization.PodIdentity) (OperationRecord, error) {
	b.mu.Lock()
	_, rec, err := b.operationOwnerLocked(req, pod)
	if err != nil {
		b.mu.Unlock()
		return OperationRecord{}, err
	}
	if !validTarget(req.Target, req.Length, pod.IP) || req.Target.Backend != rec.Backend {
		b.mu.Unlock()
		return OperationRecord{}, errors.New("target window is invalid")
	}
	validator := b.intentValidator
	src := manifestSourceOf(rec)
	intent := liveIntentOf(rec)
	b.mu.Unlock()

	cm, err := b.derivedManifest(ctx, src)
	if err != nil {
		return OperationRecord{}, err
	}
	tensor, tensorErr := cm.manifest.Tensor(ctx, req.Tensor)
	if tensorErr != nil {
		return OperationRecord{}, errors.New("tensor is unavailable")
	}
	if req.Length <= 0 {
		return OperationRecord{}, errors.New("tensor range is invalid")
	}
	valid, spanErr := cm.manifest.ValidTensorSpan(ctx, tensor, req.TensorOffset, req.Length)
	if spanErr != nil {
		return OperationRecord{}, fmt.Errorf("tensor span cannot be verified: %w", spanErr)
	}
	if !valid {
		return OperationRecord{}, errors.New("tensor range is invalid")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = validator(checkCtx, intent)
	cancel()
	if err != nil {
		return OperationRecord{}, errors.New("live source entitlement is unavailable")
	}

	b.mu.Lock()
	var session *SessionRecord
	session, rec, err = b.operationOwnerLocked(req, pod)
	if err != nil {
		b.mu.Unlock()
		return OperationRecord{}, err
	}
	if !manifestSourceMatches(rec, src) || liveIntentOf(rec) != intent {
		b.mu.Unlock()
		return OperationRecord{}, errors.New("source generation is unavailable")
	}
	if prior := b.operations[req.RequestID]; prior != nil {
		if prior.Request != req || prior.SourceLeaseID != session.SourceLeaseID || prior.ArtifactUID != session.ArtifactUID ||
			prior.Digest != session.Digest || prior.SourceGeneration != rec.SourceGeneration {
			b.mu.Unlock()
			return OperationRecord{}, errors.New("operation replay conflicts")
		}
		if prior.State != "prepared" {
			// A dispatched or terminal request stays an observation of that
			// same operation: it is polled, never reposted.
			snapshot := *prior
			b.mu.Unlock()
			return b.refreshOperation(ctx, snapshot)
		}
		// A prepared request would post to the source now. The gate stops
		// that; the record stays prepared until the recovery path releases it.
		if err := b.sourceGate("transfer", rec.Backend); err != nil {
			b.mu.Unlock()
			return OperationRecord{}, err
		}
	} else {
		if err := b.sourceGate("transfer", rec.Backend); err != nil {
			b.mu.Unlock()
			return OperationRecord{}, err
		}
		if len(b.operations) >= operationLimit {
			b.mu.Unlock()
			return OperationRecord{}, errors.New("operation journal is full")
		}
		for _, prior := range b.operations {
			if prior.State != "acknowledged" && sameWindow(prior.Request, req) {
				b.mu.Unlock()
				return OperationRecord{}, errors.New("target window remains owned")
			}
		}
		prepared := &OperationRecord{
			Request: req, SourceLeaseID: session.SourceLeaseID, ArtifactUID: session.ArtifactUID,
			Digest: session.Digest, SourceGeneration: rec.SourceGeneration, File: tensor.File,
			FileOffset: tensor.DataBegin + req.TensorOffset, State: "prepared",
		}
		if err := b.persistOperationLocked(prepared); err != nil {
			b.operationsReady = false
			b.mu.Unlock()
			return OperationRecord{}, err
		}
	}
	dispatch := *b.operations[req.RequestID]
	dispatch.State = "dispatching"
	if err := b.persistOperationLocked(&dispatch); err != nil {
		b.operationsReady = false
		b.mu.Unlock()
		return OperationRecord{}, err
	}
	private := b.privateSource
	b.mu.Unlock()
	reply, callErr := private.Call(ctx, "/v1/operations", privateSubmit(&dispatch))
	return b.applyOperationReply(dispatch.Request.RequestID, reply, callErr)
}

func operationOwnerMatches(op *OperationRecord, sessionID, uid, targetInstance string) bool {
	return op.Request.SessionID == sessionID && op.Request.PodUID == uid && op.Request.TargetInstanceID == targetInstance
}

// OperationIdentity returns the private Pod name and namespace for owner authentication.
func (b *Broker) OperationIdentity(id, sessionID, uid, targetInstance string) (authorization.PodIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	op := b.operations[id]
	if !b.operationsReady || op == nil || !operationOwnerMatches(op, sessionID, uid, targetInstance) {
		return authorization.PodIdentity{}, errors.New("operation owner is unknown")
	}
	return authorization.PodIdentity{Namespace: op.Request.PodNamespace, Name: op.Request.PodName, UID: op.Request.PodUID}, nil
}

// OperationStatus authenticates the owner and never reposts an operation.
func (b *Broker) OperationStatus(ctx context.Context, id, sessionID, uid, targetInstance string) (OperationRecord, error) {
	b.mu.Lock()
	op := b.operations[id]
	if !b.operationsReady || op == nil || !operationOwnerMatches(op, sessionID, uid, targetInstance) {
		b.mu.Unlock()
		return OperationRecord{}, errors.New("operation owner is unknown")
	}
	snapshot := *op
	b.mu.Unlock()
	return b.refreshOperation(ctx, snapshot)
}

func (b *Broker) refreshOperation(ctx context.Context, op OperationRecord) (OperationRecord, error) {
	if op.State == "prepared" || op.State == "terminal" || op.State == "acknowledged" {
		return op, nil
	}
	b.mu.Lock()
	private := b.privateSource
	rec := b.records[op.Request.Generation]
	valid := rec != nil && rec.WorkerID == op.Request.SourcePodUID &&
		rec.NativeInstance == op.Request.WorkerInstanceID && rec.LeaseID == op.SourceLeaseID
	b.mu.Unlock()
	if private == nil || !valid {
		return b.applyOperationReply(op.Request.RequestID, PrivateReply{}, errors.New("source incarnation unavailable"))
	}
	reply, err := private.Call(ctx, "/v1/operations/status", privateCommon(&op, op.Request.RequestID))
	return b.applyOperationReply(op.Request.RequestID, reply, err)
}

func (b *Broker) applyOperationReply(id string, reply PrivateReply, callErr error) (OperationRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.operations[id]
	if !b.operationsReady || current == nil {
		return OperationRecord{}, errors.New("operation receipt unavailable")
	}
	next := *current
	if callErr != nil || reply.RequestID != id || !validPrivateOperationReply(reply) ||
		(next.TerminalVerdict != "" && reply.Verdict != "" && next.TerminalVerdict != reply.Verdict) {
		if next.State != "terminal" && next.State != "acknowledged" {
			next.State, next.Verdict = "unknown", ""
		}
	} else {
		switch reply.State {
		case "dispatching", "submitted":
			if next.State != "terminal" && next.State != "consumerReleased" && next.State != "acknowledged" {
				next.State, next.Verdict = "submitted", ""
			}
		case "unknown":
			if next.State != "terminal" && next.State != "acknowledged" {
				next.State, next.Verdict = "unknown", ""
			}
		case "terminal":
			if next.State != "acknowledged" {
				next.TerminalVerdict = reply.Verdict
				next.State, next.Verdict = "terminal", reply.Verdict
				if next.ConsumerReleased {
					next.State = "consumerReleased"
				}
			}
		case "acknowledged":
			if next.ConsumerReleased {
				next.TerminalVerdict = reply.Verdict
				next.State, next.Verdict = "acknowledged", reply.Verdict
			} else {
				next.State, next.Verdict = "unknown", ""
			}
		}
	}
	if next != *current {
		if err := b.persistOperationLocked(&next); err != nil {
			b.operationsReady = false
			return OperationRecord{}, err
		}
	}
	var observationErr *privateObservationError
	if next.State == "unknown" && errors.As(callErr, &observationErr) {
		klog.InfoS("Model transfer operation observation unavailable", "requestID", id, "reason", observationErr.reason)
		return next, observationErr
	}
	return next, nil
}

func validPrivateOperationReply(reply PrivateReply) bool {
	if reply.RequestID == "" || strings.TrimSpace(reply.RequestID) != reply.RequestID {
		return false
	}
	switch reply.State {
	case "dispatching", "submitted", "unknown":
		return reply.Verdict == ""
	case "terminal", "acknowledged":
		return reply.Verdict == "succeeded" || reply.Verdict == "failed" || reply.Verdict == "not-posted"
	default:
		return false
	}
}

// AcknowledgeTransfer records consumer release before retrying private revocation.
func (b *Broker) AcknowledgeTransfer(ctx context.Context, id string, req TransferRequest, pod authorization.PodIdentity) (OperationRecord, error) {
	b.mu.Lock()
	op := b.operations[id]
	if !b.operationsReady || op == nil || pod.Namespace != op.Request.PodNamespace || pod.Name != op.Request.PodName ||
		pod.UID != op.Request.PodUID || !operationOwnerMatches(op, req.SessionID, req.PodUID, req.TargetInstanceID) ||
		req.RegistrationID != op.Request.RegistrationID || req.WindowID != op.Request.WindowID {
		b.mu.Unlock()
		return OperationRecord{}, errors.New("operation owner does not match")
	}
	if op.State == "acknowledged" {
		snapshot := *op
		b.mu.Unlock()
		return snapshot, nil
	}
	if op.State != "terminal" && op.State != "consumerReleased" && (op.State != "unknown" || !op.ConsumerReleased) {
		b.mu.Unlock()
		return OperationRecord{}, errors.New("operation is not terminal")
	}
	if !op.ConsumerReleased {
		next := *op
		next.ConsumerReleased, next.State = true, "consumerReleased"
		if err := b.persistOperationLocked(&next); err != nil {
			b.operationsReady = false
			b.mu.Unlock()
			return OperationRecord{}, err
		}
		op = b.operations[id]
	}
	snapshot := *op
	private := b.privateSource
	rec := b.records[op.Request.Generation]
	valid := rec != nil && rec.WorkerID == op.Request.SourcePodUID &&
		rec.NativeInstance == op.Request.WorkerInstanceID && rec.LeaseID == op.SourceLeaseID
	b.mu.Unlock()
	if private == nil || !valid {
		return b.applyOperationReply(id, PrivateReply{}, errors.New("private source unavailable"))
	}
	body := privateCommon(&snapshot, id)
	body["targetPodUID"], body["targetInstanceID"] = snapshot.Request.PodUID, snapshot.Request.TargetInstanceID
	body["registrationID"], body["windowID"] = snapshot.Request.RegistrationID, snapshot.Request.WindowID
	reply, err := private.Call(ctx, "/v1/operations/ack", body)
	return b.applyOperationReply(id, reply, err)
}

func operationView(op OperationRecord) map[string]any {
	return map[string]any{"requestID": op.Request.RequestID, "sessionID": op.Request.SessionID, "state": op.State, "verdict": op.Verdict}
}
