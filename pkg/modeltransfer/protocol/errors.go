package protocol

import (
	"errors"
	"fmt"
)

// Kind classifies a protocol error. The kinds a caller must distinguish
// (authentication failure, denied content, stale generation, capacity
// exhaustion, unavailable source) never share a value.
type Kind string

// The error kinds of the reader contract.
const (
	KindAuthentication      Kind = "authentication"
	KindDeniedContent       Kind = "denied-content"
	KindStaleGeneration     Kind = "stale-generation"
	KindCapacityExhausted   Kind = "capacity-exhausted"
	KindSourceUnavailable   Kind = "source-unavailable"
	KindContractVersion     Kind = "contract-version"
	KindIdentity            Kind = "identity"
	KindInvalidDescriptor   Kind = "invalid-descriptor"
	KindRangeOverflow       Kind = "range-overflow"
	KindInvalidCapabilities Kind = "invalid-capabilities"
	KindOperationState      Kind = "operation-state"
	KindBudget              Kind = "budget"
)

// KindInternal marks an error that is not a protocol verdict.
const KindInternal Kind = "internal"

// Error is a typed protocol failure. Where names the operation or field the
// failure belongs to; Detail carries the human-readable reason.
type Error struct {
	Kind   Kind
	Where  string
	Detail string
}

// Error implements the error interface with kind, location, and reason.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Kind, e.Where, e.Detail)
}

// New builds a typed protocol error with a formatted detail.
func New(kind Kind, where, format string, args ...any) *Error {
	return &Error{Kind: kind, Where: where, Detail: fmt.Sprintf(format, args...)}
}

// KindOf returns the protocol kind of err, unwrapping wrapped errors. An
// error that is not a protocol verdict is KindInternal.
func KindOf(err error) Kind {
	var protocolErr *Error
	if errors.As(err, &protocolErr) {
		return protocolErr.Kind
	}
	return KindInternal
}
