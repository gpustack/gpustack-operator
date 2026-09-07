package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// refusingHandler refuses every call and records that it was reached. Refusing rather than passing is
// what makes the guard visible: a delegate that admitted everything would report the same outcome
// whether it was called or skipped.
type refusingHandler struct {
	defaulted bool
	created   bool
	updated   bool
	deleted   bool
}

var (
	_ ctrladmission.Defaulter[runtime.Object] = (*refusingHandler)(nil)
	_ ctrladmission.Validator[runtime.Object] = (*refusingHandler)(nil)
)

var errRefused = errors.New("the delegate was reached")

func (h *refusingHandler) Default(_ context.Context, _ runtime.Object) error {
	h.defaulted = true
	return errRefused
}

func (h *refusingHandler) ValidateCreate(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	h.created = true
	return nil, errRefused
}

func (h *refusingHandler) ValidateUpdate(_ context.Context, _, _ runtime.Object) (ctrladmission.Warnings, error) {
	h.updated = true
	return nil, errRefused
}

func (h *refusingHandler) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	h.deleted = true
	return nil, errRefused
}

// guardTestObject builds an object carrying the given deletion timestamp. Any type with an
// ObjectMeta serves, since the guard reads nothing but that timestamp.
func guardTestObject(deletionTimestamp *meta.Time) runtime.Object {
	return &admreg.ValidatingWebhookConfiguration{
		ObjectMeta: meta.ObjectMeta{
			Name:              testVwcName,
			DeletionTimestamp: deletionTimestamp,
		},
	}
}

// Test_deletionGuardedValidator pins which calls the guard withholds from the delegate.
//
// The withheld one is update validation on an object being deleted, and that is the whole point:
// update validation depending on state which may already be gone must not be able to reject an
// update that only clears a finalizer. Create and delete validation are delegated unchanged, in both
// states, because neither can be the update that releases an object.
func Test_deletionGuardedValidator(t *testing.T) {
	deletedAt := meta.Now()

	testCases := []struct {
		name        string
		deletion    *meta.Time
		wantCreated bool
		wantUpdated bool
		wantDeleted bool
	}{
		{
			name:        "a live object reaches every validation",
			wantCreated: true,
			wantUpdated: true,
			wantDeleted: true,
		},
		{
			name:        "a terminating object reaches every validation except update",
			deletion:    &deletedAt,
			wantCreated: true,
			wantUpdated: false,
			wantDeleted: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := guardTestObject(tc.deletion)
			delegate := &refusingHandler{}
			guarded := deletionGuardedValidator{delegate: delegate}

			_, createErr := guarded.ValidateCreate(t.Context(), obj)
			_, updateErr := guarded.ValidateUpdate(t.Context(), guardTestObject(nil), obj)
			_, deleteErr := guarded.ValidateDelete(t.Context(), obj)

			assert.Equal(t, tc.wantCreated, delegate.created, "create validation")
			assert.Equal(t, tc.wantUpdated, delegate.updated, "update validation")
			assert.Equal(t, tc.wantDeleted, delegate.deleted, "delete validation")

			// The refusal is what the caller sees, so assert it rather than only the flag: a guard
			// that called the delegate and swallowed its error would set the flag and admit anyway.
			assert.Equal(t, tc.wantCreated, errors.Is(createErr, errRefused), "create refusal")
			assert.Equal(t, tc.wantUpdated, errors.Is(updateErr, errRefused), "update refusal")
			assert.Equal(t, tc.wantDeleted, errors.Is(deleteErr, errRefused), "delete refusal")
		})
	}
}

// Test_deletionGuardedDefaulter pins the same rule for the mutating half. It is a separate assertion
// because a single marker releases both halves, so a handler whose defaulting depends on another
// object keeps the guard however pure its update validation is.
func Test_deletionGuardedDefaulter(t *testing.T) {
	deletedAt := meta.Now()

	testCases := []struct {
		name          string
		deletion      *meta.Time
		wantDefaulted bool
	}{
		{name: "a live object is defaulted", wantDefaulted: true},
		{name: "a terminating object is not", deletion: &deletedAt, wantDefaulted: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			delegate := &refusingHandler{}
			err := deletionGuardedDefaulter{delegate: delegate}.
				Default(t.Context(), guardTestObject(tc.deletion))

			assert.Equal(t, tc.wantDefaulted, delegate.defaulted, "defaulting")
			assert.Equal(t, tc.wantDefaulted, errors.Is(err, errRefused), "refusal")
		})
	}
}

// Test_ReceiveDeletionUpdate_isWhatChangesTheOutcome is the control the two tests above cannot be:
// they measure the wrapper, which only matters if something decides whether it is applied. That
// decision is one type assertion in ExecuteSetup, and this is the same expression.
//
// Both handlers below behave identically when called directly. The marker is the only difference
// between them, so it is the only thing that says whether either one's rules describe production.
func Test_ReceiveDeletionUpdate_isWhatChangesTheOutcome(t *testing.T) {
	deletedAt := meta.Now()
	terminating := guardTestObject(&deletedAt)

	plain := &refusingHandler{}
	_, plainErr := plain.ValidateUpdate(t.Context(), guardTestObject(nil), terminating)
	require.ErrorIs(t, plainErr, errRefused,
		"a direct call refuses whatever the marker says, which is why it cannot be the assertion")

	_, plainKeeps := any(plain).(ReceiveDeletionUpdate)
	assert.False(t, plainKeeps, "the handler without the marker is wrapped, so it is skipped")

	opted := &optedInHandler{}
	_, optedKeeps := any(opted).(ReceiveDeletionUpdate)
	assert.True(t, optedKeeps, "the handler with the marker is left unwrapped, so it still runs")
}

// optedInHandler is refusingHandler plus the marker, which is the only difference between the two.
type optedInHandler struct {
	refusingHandler
}

var _ ReceiveDeletionUpdate = (*optedInHandler)(nil)

func (h *optedInHandler) ReceiveDeletionUpdate() {}
