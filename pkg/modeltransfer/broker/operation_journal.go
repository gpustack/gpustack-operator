package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	operationLimit        = 1 << 20
	operationJournalBytes = 32 << 20
	operationDir          = "operations"
	operationLayoutFile   = "operation-layout.json"
)

// TransferTarget is an authenticated target-owned registered host window.
type TransferTarget struct {
	Backend  string `json:"backend"`
	Address  uint64 `json:"address"`
	Length   int64  `json:"length"`
	Metadata string `json:"metadata"`
	Endpoint string `json:"endpoint"`
}

// TransferRequest is the immutable public operation identity and target window.
type TransferRequest struct {
	RequestID        string         `json:"requestID"`
	SessionID        string         `json:"sessionID"`
	PodNamespace     string         `json:"podNamespace"`
	PodName          string         `json:"podName"`
	PodUID           string         `json:"podUID"`
	TargetInstanceID string         `json:"targetInstanceID"`
	SourcePodUID     string         `json:"sourcePodUID"`
	WorkerInstanceID string         `json:"workerInstanceID"`
	Generation       string         `json:"generation"`
	RegistrationID   string         `json:"registrationID"`
	WindowID         string         `json:"windowID"`
	Tensor           string         `json:"tensor"`
	TensorOffset     int64          `json:"tensorOffset"`
	Length           int64          `json:"length"`
	Target           TransferTarget `json:"target"`
}

// OperationRecord is a private, durable transfer receipt.
type OperationRecord struct {
	Request          TransferRequest `json:"request"`
	SourceLeaseID    string          `json:"sourceLeaseID"`
	ArtifactUID      string          `json:"artifactUID"`
	Digest           string          `json:"digest"`
	SourceGeneration int64           `json:"sourceGeneration"`
	File             string          `json:"file"`
	FileOffset       int64           `json:"fileOffset"`
	State            string          `json:"state"`
	Verdict          string          `json:"verdict,omitempty"`
	TerminalVerdict  string          `json:"terminalVerdict,omitempty"`
	ConsumerReleased bool            `json:"consumerReleased,omitempty"`
}

type operationLayout struct {
	Version        int    `json:"version"`
	Scope          string `json:"scope"`
	LedgerIdentity string `json:"ledgerIdentity"`
}

// EnableOperationJournal verifies or initializes the durable operation journal.
// An established ledger with a missing journal is unknown and refuses startup.
func (b *Broker) EnableOperationJournal() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.operationsReady = false
	sessionPath := filepath.Join(b.root, "sessions.json")
	if _, err := os.Stat(sessionPath); errors.Is(err, os.ErrNotExist) {
		if !b.fresh {
			return errors.New("established ledger has no session journal")
		}
		if err := b.persistSessionsLocked(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	path := filepath.Join(b.root, operationLayoutFile)
	raw, err := readOperationDocument(path, operationJournalBytes)
	if errors.Is(err, os.ErrNotExist) {
		if !b.fresh {
			return errors.New("established ledger has no operation journal")
		}
		if err := os.MkdirAll(filepath.Join(b.root, operationDir), 0o700); err != nil {
			return err
		}
		raw, err = json.Marshal(operationLayout{Version: 1, Scope: b.scope, LedgerIdentity: b.ledger.identity})
		if err != nil {
			return err
		}
		if err := writeSynced(path, raw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var layout operationLayout
	if err := decodeStrictDocument(raw, &layout); err != nil || layout.Version != 1 ||
		layout.Scope != b.scope || layout.LedgerIdentity != b.ledger.identity {
		return errors.New("operation journal identity is corrupt")
	}
	entries, err := os.ReadDir(filepath.Join(b.root, operationDir))
	if err != nil {
		return err
	}
	var staleTemps []string
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json.tmp")
		if entry.Type().IsRegular() && entry.Name() == id+".json.tmp" && sessionNoncePattern.MatchString(id) {
			staleTemps = append(staleTemps, entry.Name())
		}
	}
	if len(entries)-len(staleTemps) > operationLimit*2 {
		return errors.New("operation journal exceeds its bound")
	}
	ops := map[string]*OperationRecord{}
	var total int64
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !entry.Type().IsRegular() || entry.Name() != id+".json" || !sessionNoncePattern.MatchString(id) {
			return errors.New("operation journal contains an invalid entry")
		}
		if len(ops) >= operationLimit {
			return errors.New("operation journal exceeds its bound")
		}
		data, err := readOperationDocument(filepath.Join(b.root, operationDir, entry.Name()), operationJournalBytes-total)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > operationJournalBytes {
			return errors.New("operation journal exceeds its byte bound")
		}
		var op OperationRecord
		if err := decodeStrictDocument(data, &op); err != nil || op.Request.RequestID != id ||
			!validOperationState(op.State, op.Verdict) || op.SourceLeaseID == "" || op.Request.SessionID == "" {
			return errors.New("operation journal entry is corrupt")
		}
		ops[id] = &op
	}
	for _, name := range staleTemps {
		if err := os.Remove(filepath.Join(b.root, operationDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove operation staging: %w", err)
		}
	}
	b.operations, b.operationBytes, b.operationsReady = ops, total, true
	return nil
}

func readOperationDocument(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("operation journal exceeds its byte bound")
	}
	return data, nil
}

func decodeStrictDocument(raw []byte, value any) error {
	if len(raw) == 0 || len(raw) > operationJournalBytes || rejectDuplicateJSONKeys(string(raw)) != nil {
		return errors.New("invalid document")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing document")
	}
	return nil
}

func validOperationState(state, verdict string) bool {
	switch state {
	case "prepared", "dispatching", "submitted", "unknown":
		return verdict == ""
	case "terminal", "consumerReleased", "acknowledged":
		return verdict == "succeeded" || verdict == "failed" || verdict == "not-posted"
	default:
		return false
	}
}

func (b *Broker) persistOperationLocked(op *OperationRecord) error {
	if _, exists := b.operations[op.Request.RequestID]; !exists && len(b.operations) >= operationLimit {
		return errors.New("operation journal is full")
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	oldSize := int64(0)
	if old := b.operations[op.Request.RequestID]; old != nil {
		oldRaw, err := json.Marshal(old)
		if err != nil {
			return err
		}
		oldSize = int64(len(oldRaw))
	}
	if b.operationBytes-oldSize+int64(len(raw)) > operationJournalBytes {
		return errors.New("operation journal is full")
	}
	path := filepath.Join(b.root, operationDir, op.Request.RequestID+".json")
	if err := writeSynced(path, raw); err != nil {
		return fmt.Errorf("persist operation receipt: %w", err)
	}
	b.operationBytes += int64(len(raw)) - oldSize
	b.operations[op.Request.RequestID] = op
	return nil
}

// compactOperationsLocked reclaims journal space from receipts whose
// session or generation is finished. Only acknowledged receipts are ever
// removed: every other state records a transfer whose outcome is not yet
// proven, and losing it could repost a window or free an unproven charge.
// A receipt is reclaimed when its session is gone or terminal, or its
// generation is released. When the journal is past half of either bound,
// each still-active session's acknowledged history is also trimmed to a
// single receipt, so a long-lived session's window history cannot grow
// into the caps. The receipts carry no ordering, so the trimmed survivor
// is one deterministic representative per session.
func (b *Broker) compactOperationsLocked() {
	if !b.operationsReady {
		return
	}
	for id, op := range b.operations {
		if op.State != "acknowledged" {
			continue
		}
		session := b.sessions[op.Request.SessionID]
		sessionDone := session == nil || session.Phase == "released" || session.Phase == "refused"
		rec := b.records[op.Request.Generation]
		generationDone := rec == nil || rec.State == StateReleased
		if sessionDone || generationDone {
			b.removeOperationLocked(id, op)
		}
	}
	if b.operationBytes <= operationJournalBytes/2 && len(b.operations) <= operationLimit/2 {
		return
	}
	keep := map[string]string{}
	for id, op := range b.operations {
		if op.State != "acknowledged" {
			continue
		}
		session := b.sessions[op.Request.SessionID]
		if session == nil || session.Phase == "released" || session.Phase == "refused" {
			continue
		}
		if current, ok := keep[op.Request.SessionID]; ok && current > id {
			continue
		}
		keep[op.Request.SessionID] = id
	}
	for id, op := range b.operations {
		if op.State != "acknowledged" || keep[op.Request.SessionID] == id {
			continue
		}
		b.removeOperationLocked(id, op)
	}
}

// removeOperationLocked deletes one receipt from the journal and its map,
// charging the freed bytes back to the journal's total.
func (b *Broker) removeOperationLocked(id string, op *OperationRecord) {
	if err := os.Remove(filepath.Join(b.root, operationDir, id+".json")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return
	}
	raw, err := json.Marshal(op)
	if err != nil {
		raw = nil
	}
	b.operationBytes -= int64(len(raw))
	if b.operationBytes < 0 {
		b.operationBytes = 0
	}
	delete(b.operations, id)
}

// SetPrivateSource attaches the source-process channel after bootstrap is minted.
func (b *Broker) SetPrivateSource(source PrivateSource) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.privateSource = source
}
