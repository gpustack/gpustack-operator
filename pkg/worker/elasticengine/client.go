// Package elasticengine is the operator's client for the engine's own elastic width endpoints.
//
// It talks to a pinned vLLM's two native routes and nothing else. The native contract, read from
// v0.29.0's serve/elastic_ep router, is:
//
//	POST /scale_elastic_ep      {"new_data_parallel_size": positive int, "drain_timeout": positive int}
//	                            200 {"message": ...} | 400 | 408 | 500
//	POST /is_scaling_elastic_ep  200 {"is_scaling_elastic_ep": bool}
//
// A success here means exactly one thing: the engine accepted the request. It is not the effective
// width, and nothing in this package can tell you the effective width. The engine reports the width
// it believes it is at, and that is a claim about itself; only a request actually served afterwards
// is evidence that it serves. The same is true of the scaling flag, which is a statement that a
// resize is in progress and never a statement that a worker is free to remove.
//
// Nothing here retries. A resize is a mutation of a collective, and a mutation that is replayed
// against a request that may already have been applied is how a collective ends up at an unintended
// width. An ambiguous answer here is reported as ambiguous and the caller's state machine decides
// what to do about it; that state machine, not this client, owns reconstruction.
package elasticengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// scalePath is the native route that changes the data parallel width.
	scalePath = "/scale_elastic_ep"
	// isScalingPath is the native route that reports whether a resize is in progress.
	isScalingPath = "/is_scaling_elastic_ep"
	// isScalingField is the single field the native answer carries. It is required to be present
	// and boolean, because an answer that does not carry it did not say anything.
	isScalingField = "is_scaling_elastic_ep"
	// ackField is the single field the native scale answer carries. It is required to be present
	// and a string, because an answer without it did not acknowledge the resize.
	ackField = "message"
)

// maxResponseBytes bounds what one answer may be. A member that streams forever is cut off here
// rather than by the size of whatever it chooses to serve.
const maxResponseBytes = 1 << 20

// Kind is why a call failed, in the terms a caller has to act on.
//
// The ambiguous kind is the one that matters most. A transport that dropped a request, a caller
// that canceled one, and a request the engine applied and then failed to acknowledge are the same
// three states to anything that did not record the intent first. They are reported as one kind
// precisely because no answer to them may be inferred from the failure itself.
type Kind int

const (
	// KindInvalidInput is a request this client refused to send, or one the engine rejected as
	// malformed. Nothing was changed.
	KindInvalidInput Kind = iota
	// KindReconfiguring is the engine's own 503: it is not accepting the request right now because
	// it is already reconfiguring. The request was not applied.
	KindReconfiguring
	// KindNativeFailure is the engine's 408 or 500. The request reached it and it reported that it
	// could not complete, which is a different fact from a request that never arrived.
	KindNativeFailure
	// KindAmbiguous is a transport failure, a cancellation, or any answer this client could not
	// finish reading. Whether the request was applied is not known and must not be assumed.
	KindAmbiguous
	// KindMalformed is an answer that arrived and did not carry what the route promises.
	KindMalformed
)

// String names the kind, for a reason string and for logs.
func (k Kind) String() string {
	switch k {
	case KindInvalidInput:
		return "invalid input"
	case KindReconfiguring:
		return "engine is reconfiguring"
	case KindNativeFailure:
		return "engine reported failure"
	case KindAmbiguous:
		return "ambiguous"
	case KindMalformed:
		return "malformed response"
	default:
		return "unknown"
	}
}

// Error is every failure this package returns, classified.
//
// It carries the status and the route but not the body. A native error body is whatever the engine
// chose to put there, and it may carry a model name, a URL, or a fragment of a request. None of
// that is needed to decide what to do next, and all of it is the kind of thing that ends up in a
// log.
type Error struct {
	Kind Kind
	// Op is the route that was called.
	Op string
	// StatusCode is the engine's status, or zero when the call never produced one.
	StatusCode int
	// Detail is a short, non-sensitive description of what was wrong.
	Detail string
	// Err is the underlying cause, for an error chain. It is nil for a refusal this client made
	// before sending anything.
	Err error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("elastic engine %s: %s (status %d): %s", e.Op, e.Kind, e.StatusCode, e.Detail)
	}

	return fmt.Sprintf("elastic engine %s: %s: %s", e.Op, e.Kind, e.Detail)
}

func (e *Error) Unwrap() error { return e.Err }

// invalid builds a refusal made before anything was sent.
func invalid(op, format string, args ...any) *Error {
	return &Error{Kind: KindInvalidInput, Op: op, Detail: fmt.Sprintf(format, args...)}
}

// Client is a configured, immutable client for one engine's elastic endpoints.
//
// It is immutable after construction and safe for concurrent use. Every field is read-only, the
// underlying http.Client is itself safe for concurrent use, and no call mutates the client, so
// there is no lock here and nothing to get wrong.
type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

// New builds a client for one engine.
//
// The timeout is mandatory and positive, and it is the operator's own bound rather than something
// derived from a drain timeout. A drain timeout describes how long the engine may take to retire a
// worker; it says nothing about how long this call may hold a reconcile open, and letting the first
// govern the second is how one slow member holds the controller for a drain budget.
//
// There is no way to supply a transport. A supplied client would replace the one that refuses
// redirects, and a redirect refusal is the only thing standing between a mutation and a second
// application of it. The client owns its own transport and stays immutable.
func New(baseURL string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, invalid("", "the call timeout must be positive, got %s", timeout)
	}

	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return nil, invalid("", "the engine base URL is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// The parse error is not reported. It repeats the base URL, and a base URL may carry a
		// userinfo credential. A generic reason loses detail and leaks nothing.
		return nil, invalid("", "the engine base URL is not a URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, invalid("", "the engine base URL must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, invalid("", "the engine base URL has no host")
	}

	client := &Client{
		baseURL: strings.TrimRight(trimmed, "/"),
		timeout: timeout,
	}
	// A redirect is not followed. Both routes are POSTs and a POST that is redirected may be
	// replayed to whatever answered the redirect, which is exactly the second application of a
	// mutation this package exists to prevent.
	client.http = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client, nil
}

// callContext bounds one call by the operator's timeout.
//
// A timeout already respects an earlier deadline on the parent, so a caller that already bounded
// itself keeps its own tighter bound and the sooner of the two always wins.
func (c *Client) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// post sends one request and returns the bounded body, classifying every failure along the way.
func (c *Client) post(ctx context.Context, op, path string, payload any) ([]byte, *Error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{
				Kind: KindInvalidInput, Op: op,
				Detail: "the request body could not be encoded", Err: err,
			}
		}
		body = bytes.NewReader(encoded)
	}

	callCtx, cancel := c.callContext(ctx)
	defer cancel()

	request, err := http.NewRequestWithContext(
		callCtx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, &Error{Kind: KindInvalidInput, Op: op, Detail: "the request could not be built", Err: err}
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// A canceled or timed-out call is ambiguous, not failed. The request may have been
		// applied before the client stopped hearing about it, and reporting that as a failure
		// would invite a retry of a mutation that may already have happened.
		kind := KindAmbiguous
		detail := "the request could not be completed"
		if errors.Is(err, context.Canceled) {
			detail = "the caller cancelled the request"
		} else if errors.Is(err, context.DeadlineExceeded) {
			detail = "the call exceeded its bound"
		}

		return nil, &Error{Kind: kind, Op: op, Detail: detail, Err: err}
	}
	defer response.Body.Close()

	// The body is bounded while it is read, so a member that never stops writing is cut off
	// instead of being allowed to grow the answer.
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &Error{
			Kind: KindAmbiguous, Op: op,
			Detail: "the answer could not be read to its end", Err: err,
		}
	}
	if len(raw) > maxResponseBytes {
		return nil, &Error{
			Kind: KindMalformed, Op: op,
			Detail: fmt.Sprintf("the answer exceeded the %d byte bound", maxResponseBytes),
		}
	}

	switch {
	case response.StatusCode == http.StatusOK:
		return raw, nil
	case response.StatusCode == http.StatusServiceUnavailable:
		return nil, &Error{
			Kind: KindReconfiguring, Op: op, StatusCode: response.StatusCode,
			Detail: "the engine is not accepting requests while it reconfigures",
		}
	case response.StatusCode == http.StatusRequestTimeout,
		response.StatusCode >= http.StatusInternalServerError:
		return nil, &Error{
			Kind: KindNativeFailure, Op: op, StatusCode: response.StatusCode,
			Detail: "the engine received the request and reported that it could not complete it",
		}
	case response.StatusCode == http.StatusBadRequest:
		return nil, &Error{
			Kind: KindInvalidInput, Op: op, StatusCode: response.StatusCode,
			Detail: "the engine rejected the request as malformed",
		}
	default:
		return nil, &Error{
			Kind: KindMalformed, Op: op, StatusCode: response.StatusCode,
			Detail: "the engine answered with a status this client does not model",
		}
	}
}

// scaleRequest is the native body, named for the native field.
type scaleRequest struct {
	NewDataParallelSize int `json:"new_data_parallel_size"`
	DrainTimeout        int `json:"drain_timeout"`
}

// Scale asks the engine to move to a new data parallel width.
//
// The request is sent exactly once. There is no retry here and no path that turns a failure into
// another attempt, because whether a failed call was applied is not something this client can answer.
//
// A nil error means the engine accepted and completed the request as its route defines it. It does
// not mean the engine is serving at the new width, and a caller that treats it as that is reading
// an acknowledgement as a fact.
func (c *Client) Scale(ctx context.Context, newDataParallelSize int, drainTimeout time.Duration) error {
	const op = scalePath

	// The inputs are checked before any request is built, because a refused request must leave the
	// engine untouched. The native route answers 400 for exactly these cases, and a client that
	// relied on that would turn a local mistake into a round trip.
	if newDataParallelSize <= 0 {
		return invalid(op, "the new data parallel size must be positive, got %d", newDataParallelSize)
	}
	if drainTimeout <= 0 {
		return invalid(op, "the drain timeout must be positive, got %s", drainTimeout)
	}
	seconds := int(drainTimeout / time.Second)
	if seconds <= 0 {
		return invalid(op, "the drain timeout of %s is under a second", drainTimeout)
	}

	raw, failure := c.post(ctx, op, scalePath, scaleRequest{
		NewDataParallelSize: newDataParallelSize,
		DrainTimeout:        seconds,
	})
	if failure != nil {
		return failure
	}

	// The nil is returned explicitly. Returning the *Error directly would put a nil *Error into a
	// non-nil error interface, and the caller would see a failure where the engine acknowledged.
	if failure := acknowledge(op, raw); failure != nil {
		return failure
	}

	return nil
}

// acknowledge checks the native 200 body against what the route promises.
//
// The route answers with one object holding a message string. A body that is valid JSON but not
// that object did not acknowledge anything, and a nil error here would tell the caller the
// mutation was accepted on the strength of a document the route never sends.
func acknowledge(op string, raw []byte) *Error {
	answer, failure := decodeObject(op, raw)
	if failure != nil {
		return failure
	}

	message, present := answer[ackField]
	if !present {
		return &Error{
			Kind: KindMalformed, Op: op, Detail: "the acknowledgement does not carry " + ackField,
		}
	}
	// A null is refused before the unmarshal, because unmarshalling a null into a string leaves the
	// string empty and reports no error at all.
	if string(message) == "null" {
		return &Error{
			Kind: KindMalformed, Op: op, Detail: ackField + " is null rather than a string",
		}
	}
	var text string
	if err := json.Unmarshal(message, &text); err != nil {
		return &Error{
			Kind: KindMalformed, Op: op,
			Detail: ackField + " is not a string", Err: err,
		}
	}

	return nil
}

// decodeObject reads exactly one JSON object out of an answer.
//
// A null is refused on purpose. Decoding it into a map succeeds and leaves the map nil, so a
// caller that only checked the error would read a silent engine as an object with no fields.
func decodeObject(op string, raw []byte) (map[string]json.RawMessage, *Error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	var answer map[string]json.RawMessage
	if err := decoder.Decode(&answer); err != nil {
		return nil, &Error{
			Kind: KindMalformed, Op: op, Detail: "the answer is not a JSON object", Err: err,
		}
	}
	if answer == nil {
		return nil, &Error{
			Kind: KindMalformed, Op: op, Detail: "the answer is a JSON null rather than an object",
		}
	}

	// A second value after the answer is refused. The decoder reads the first and would otherwise
	// ignore whatever followed it.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, &Error{
			Kind: KindMalformed, Op: op, Detail: "the answer carries a trailing JSON value",
		}
	}

	return answer, nil
}

// IsScaling asks the engine whether a resize is in progress.
//
// The answer must carry the field explicitly. The route answers with a JSON object holding one
// boolean; an answer that omits it, nulls it, or carries something else did not say whether a
// resize is in progress, and reading any of those as false would report a quiet engine for one that
// never answered.
//
// A false here is a flag and nothing more. It is not evidence that a worker is free to remove.
func (c *Client) IsScaling(ctx context.Context) (bool, error) {
	const op = isScalingPath

	raw, failure := c.post(ctx, op, isScalingPath, struct{}{})
	if failure != nil {
		return false, failure
	}

	answer, failure := decodeObject(op, raw)
	if failure != nil {
		return false, failure
	}

	field, present := answer[isScalingField]
	if !present {
		return false, &Error{
			Kind: KindMalformed, Op: op, Detail: "the answer does not carry " + isScalingField,
		}
	}
	if string(field) == "null" {
		return false, &Error{
			Kind: KindMalformed, Op: op, Detail: isScalingField + " is null rather than a boolean",
		}
	}
	var scaling bool
	if err := json.Unmarshal(field, &scaling); err != nil {
		return false, &Error{
			Kind: KindMalformed, Op: op,
			Detail: isScalingField + " is not a boolean", Err: err,
		}
	}

	return scaling, nil
}
