package protocol

// OpState is the state of one submitted operation. Unknown is a first-class
// retained state: its resources are held until a qualified fence resolves
// it, and it is not terminal.
type OpState string

// The operation states.
const (
	OpPending   OpState = "pending"
	OpSucceeded OpState = "succeeded"
	OpFailed    OpState = "failed"
	OpCancelled OpState = "cancelled"
	OpUnknown   OpState = "unknown"
)

// Terminal reports whether the state is final: succeeded, failed, or
// canceled. Unknown is never terminal.
func (s OpState) Terminal() bool {
	return s == OpSucceeded || s == OpFailed || s == OpCancelled
}

// Operation is one submitted read with its own state machine. Every
// submitted operation keeps its state until released; no other operation's
// outcome changes it.
type Operation struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Tensor       string  `json:"tensor"`
	Offset       int64   `json:"offset"`
	Length       int64   `json:"length"`
	State        OpState `json:"state"`
	ErrorCode    string  `json:"error_code,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Retained     bool    `json:"retained,omitempty"`
}

// MarkSucceeded completes the operation. A pending or UNKNOWN operation may
// succeed; UNKNOWN resolves through this mark. Marking success twice is a
// no-op; marking a differently decided operation is an error.
func (o *Operation) MarkSucceeded() error {
	return o.markTerminal(OpSucceeded, "", "")
}

// MarkFailed fails the operation with a typed code and message. A pending
// or UNKNOWN operation may fail; failing twice is a no-op.
func (o *Operation) MarkFailed(code, message string) error {
	return o.markTerminal(OpFailed, code, message)
}

// MarkCancelled cancels the operation. Idempotent and never an error:
// canceling a decided operation is a no-op, and an UNKNOWN operation stays
// retained because its outcome is unknown.
func (o *Operation) MarkCancelled() {
	if o.State == OpPending {
		o.State = OpCancelled
	}
}

// MarkUnknown records that the backend state was not terminal at the
// deadline. Only a pending operation can become UNKNOWN, and its resources
// stay retained until ResolveUnknown.
func (o *Operation) MarkUnknown(reason string) error {
	if o.State != OpPending {
		return New(KindOperationState, o.ID,
			"cannot mark unknown from %s", o.State)
	}
	o.State = OpUnknown
	o.ErrorMessage = reason
	o.Retained = true
	return nil
}

// ResolveUnknown resolves a retained UNKNOWN operation through a qualified
// fence. Only succeeded or failed are valid targets.
func (o *Operation) ResolveUnknown(target OpState, code, message string) error {
	if o.State != OpUnknown {
		return New(KindOperationState, o.ID,
			"only UNKNOWN operations resolve, state is %s", o.State)
	}
	if target != OpSucceeded && target != OpFailed {
		return New(KindOperationState, o.ID,
			"UNKNOWN resolves to succeeded or failed, not %s", target)
	}
	o.State = target
	o.ErrorCode = code
	o.ErrorMessage = message
	o.Retained = false
	return nil
}

// CanRelease reports whether the operation is terminal and not retained, so
// its window and output may be released.
func (o Operation) CanRelease() bool {
	return o.State.Terminal() && !o.Retained
}

func (o *Operation) markTerminal(target OpState, code, message string) error {
	if o.State == target {
		return nil
	}
	if o.State != OpPending && o.State != OpUnknown {
		return New(KindOperationState, o.ID,
			"cannot mark %s from %s", target, o.State)
	}
	o.State = target
	o.ErrorCode = code
	o.ErrorMessage = message
	o.Retained = false
	return nil
}
