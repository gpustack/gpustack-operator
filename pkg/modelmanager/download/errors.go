// Package download fetches the files of a manifest from a model hub and verifies every byte against
// the manifest while it arrives.
package download

import (
	"errors"
	"fmt"
)

// The reasons a download fails with. They are the node's reason table: a node model store entry,
// a metric and a mount's error message carry them, and nothing parses the message beside one.
const (
	// ReasonInvalidRequest is a configuration that cannot be executed.
	ReasonInvalidRequest = "InvalidRequest"
	// ReasonAccessDenied is the hub refusing the credential.
	ReasonAccessDenied = "AccessDenied"
	// ReasonSourceUnavailable is the hub unreachable or failing, or a file's ranges failing again
	// and again.
	ReasonSourceUnavailable = "SourceUnavailable"
	// ReasonIntegrityMismatch is content, or a manifest, that does not match its digest.
	ReasonIntegrityMismatch = "IntegrityMismatch"
	// ReasonInsufficientCapacity is a disk with no room left.
	ReasonInsufficientCapacity = "InsufficientCapacity"
	// ReasonCanceled is an attempt nobody waits for any more.
	ReasonCanceled = "Canceled"
)

// Error is a classified failure. Its message names files by their manifest path and hubs by the
// URL the caller gave, never by a redirect target, which may be a signed URL, and never carries a
// credential.
//
// Detail is the part of the failure that names no tenant: an HTTP status, a timeout, a TLS failure,
// or a condition of the node. A digest is shared by every tenant whose artifact resolves to it, so
// what a node publishes about a failure carries the reason and Detail only; the message, whose URL
// names the repository, is for the plugin's log.
type Error struct {
	Reason  string
	Message string
	Detail  string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func errorf(reason, format string, args ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func detailedf(reason, detail, format string, args ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...), Detail: detail}
}

// DetailOf returns the Detail of an Error anywhere in err's chain, or "".
func DetailOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Detail
	}

	return ""
}

// ReasonOf returns the reason of the classified failure in err's chain, SourceUnavailable for an
// unclassified one, and "" for nil.
func ReasonOf(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Reason
	}

	return ReasonSourceUnavailable
}
