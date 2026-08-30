package kubeapistatus

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// _AltCondition mirrors the shape of this project's own condition type: the same field names, in a
// different order, with Reason and Message optional. It exists so the cases below run against two
// structurally different condition types, which pins the property every caller depends on — these
// accessors are reflective and name no concrete condition type.
type _AltCondition struct {
	Type               string
	Status             meta.ConditionStatus
	LastTransitionTime meta.Time
	ObservedGeneration int64
	Reason             string
	Message            string
}

type _MetaConditioned struct {
	Status struct {
		Conditions []meta.Condition
	}
}

type _AltConditioned struct {
	Status struct {
		Conditions []_AltCondition
	}
}

// conditionSubject is one condition-carrying shape under test, plus the accessors that read a
// condition back out of it. Two subjects run the same table.
type conditionSubject struct {
	name   string
	newObj func() any
	read   func(obj any) (status meta.ConditionStatus, reason, message string, ts meta.Time)
	// rewind backdates the stamped condition. It is what lets the timestamp assertions be decided
	// by the code under test rather than by how fast two consecutive writes happen to run.
	rewind func(obj any, ts meta.Time)
}

func conditionSubjects() []conditionSubject {
	return []conditionSubject{
		{
			name:   "metav1.Condition",
			newObj: func() any { return new(_MetaConditioned) },
			read: func(obj any) (meta.ConditionStatus, string, string, meta.Time) {
				c := obj.(*_MetaConditioned).Status.Conditions[0]
				return c.Status, c.Reason, c.Message, c.LastTransitionTime
			},
			rewind: func(obj any, ts meta.Time) {
				obj.(*_MetaConditioned).Status.Conditions[0].LastTransitionTime = ts
			},
		},
		{
			name:   "a structurally different condition",
			newObj: func() any { return new(_AltConditioned) },
			read: func(obj any) (meta.ConditionStatus, string, string, meta.Time) {
				c := obj.(*_AltConditioned).Status.Conditions[0]
				return c.Status, c.Reason, c.Message, c.LastTransitionTime
			},
			rewind: func(obj any, ts meta.Time) {
				obj.(*_AltConditioned).Status.Conditions[0].LastTransitionTime = ts
			},
		},
	}
}

// TestConditionType_SetStatus pins when LastTransitionTime moves, which is the whole contract of a
// transition timestamp: it marks the moment the status became what it is, and nothing else.
//
// This matters beyond tidiness. A controller writes its conditions on every reconcile and guards the
// status write with an equality check; a timestamp that moved on every write would make every pass
// differ from the last, so every pass would write, and every write would wake the next pass.
func TestConditionType_SetStatus(t *testing.T) {
	const ct = ConditionType("Ready")

	cases := []struct {
		name string
		// second is the write performed after the initial True/Up/serving.
		second func(obj any)
		// wantMoved is whether that second write may move LastTransitionTime.
		wantMoved bool
		// wantStatus, wantReason and wantMessage are the settled values afterwards.
		wantStatus  meta.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "an identical write moves nothing",
			second:      func(obj any) { ct.True(obj, "Up", "serving") },
			wantMoved:   false,
			wantStatus:  meta.ConditionTrue,
			wantReason:  "Up",
			wantMessage: "serving",
		},
		{
			name:        "a reason and message change is not a transition",
			second:      func(obj any) { ct.True(obj, "StillUp", "serving 2 replicas") },
			wantMoved:   false,
			wantStatus:  meta.ConditionTrue,
			wantReason:  "StillUp",
			wantMessage: "serving 2 replicas",
		},
		{
			name:        "a flip to False is a transition",
			second:      func(obj any) { ct.False(obj, "Down", "stopped") },
			wantMoved:   true,
			wantStatus:  meta.ConditionFalse,
			wantReason:  "Down",
			wantMessage: "stopped",
		},
		{
			name:        "a flip to Unknown is a transition",
			second:      func(obj any) { ct.Unknown(obj, "Lost", "no answer") },
			wantMoved:   true,
			wantStatus:  meta.ConditionUnknown,
			wantReason:  "Lost",
			wantMessage: "no answer",
		},
	}

	for _, s := range conditionSubjects() {
		for _, c := range cases {
			t.Run(s.name+"/"+c.name, func(t *testing.T) {
				obj := s.newObj()

				ct.True(obj, "Up", "serving")
				_, _, _, stamped := s.read(obj)
				require.False(t, stamped.IsZero(), "a new condition must be stamped")

				// Backdate the stamp rather than sleeping between the two writes. A transition then
				// has to replace an hour-old timestamp and a non-transition has to keep it, so
				// neither verdict rests on the clock's resolution or on how fast this test runs.
				first := meta.NewTime(stamped.Add(-time.Hour))
				s.rewind(obj, first)

				c.second(obj)

				status, reason, message, second := s.read(obj)
				assert.Equal(t, c.wantStatus, status)
				assert.Equal(t, c.wantReason, reason)
				assert.Equal(t, c.wantMessage, message)

				if c.wantMoved {
					assert.True(t, second.After(first.Time),
						"a transition must advance LastTransitionTime")
					return
				}
				assert.Equal(t, first, second,
					"a write that does not change the status must leave LastTransitionTime alone")
			})
		}
	}
}

// TestConditionType_Reads pins the read accessors against both shapes, including the answer for a
// condition that is not there at all: absent is not False.
func TestConditionType_Reads(t *testing.T) {
	const ct = ConditionType("Ready")

	for _, s := range conditionSubjects() {
		t.Run(s.name, func(t *testing.T) {
			obj := s.newObj()

			assert.False(t, ct.Exists(obj))
			assert.False(t, ct.IsTrue(obj), "an absent condition is not True")
			assert.False(t, ct.IsFalse(obj), "an absent condition is not False either")

			ct.True(obj, "Up", "serving")
			assert.True(t, ct.Exists(obj))
			assert.True(t, ct.IsTrue(obj))
			assert.False(t, ct.IsFalse(obj))
			assert.Equal(t, "Up", ct.GetReason(obj))
			assert.Equal(t, "serving", ct.GetMessage(obj))

			ct.False(obj, "Down", "stopped")
			assert.False(t, ct.IsTrue(obj))
			assert.True(t, ct.IsFalse(obj))
			assert.True(t, ct.IsTrueOrFalse(obj))
			assert.False(t, ct.IsUnknown(obj))
		})
	}
}

// TestConditionType_SetsOneEntryPerType pins that repeated writes update the entry in place rather
// than appending a second one of the same type, which a list keyed by type could not represent.
func TestConditionType_SetsOneEntryPerType(t *testing.T) {
	const (
		ready   = ConditionType("Ready")
		healthy = ConditionType("Healthy")
	)

	obj := new(_MetaConditioned)

	ready.True(obj, "Up", "serving")
	ready.False(obj, "Down", "stopped")
	ready.True(obj, "Up", "serving again")
	require.Len(t, obj.Status.Conditions, 1)

	healthy.True(obj, "Ok", "probes pass")
	require.Len(t, obj.Status.Conditions, 2)
	assert.Equal(t, "Ready", obj.Status.Conditions[0].Type)
	assert.Equal(t, "Healthy", obj.Status.Conditions[1].Type)
}

// TestConditionType_LastTransitionTime pins the timestamp accessors, which are the two halves of a
// round trip: what the setter takes, the getter must give back.
//
// The field is a meta.Time — a struct, not a string — so both halves have to go through the value
// rather than through SetString/String, which is what makes this worth pinning.
func TestConditionType_LastTransitionTime(t *testing.T) {
	const (
		ct    = ConditionType("Ready")
		stamp = "2026-01-02T03:04:05Z"
	)

	for _, s := range conditionSubjects() {
		t.Run(s.name, func(t *testing.T) {
			obj := s.newObj()

			assert.Empty(t, ct.GetLastTransitionTime(obj),
				"an absent condition has no transition time")

			ct.True(obj, "Up", "serving")
			require.NotEmpty(t, ct.GetLastTransitionTime(obj),
				"a stamped condition must report its time, not a struct's String()")

			ct.LastTransitionTime(obj, stamp)
			assert.Equal(t, stamp, ct.GetLastTransitionTime(obj), "the round trip must be exact")

			_, _, _, ts := s.read(obj)
			assert.Equal(t, stamp, ts.UTC().Format(time.RFC3339),
				"the value written must reach the field itself, not a copy")
		})
	}
}

// TestConditionType_LastTransitionTimeRejectsGarbage pins that an unparseable stamp is refused
// loudly. It is programmer error — every caller passes a formatted value — and this file already
// panics on the other one it can detect, a non-pointer object.
func TestConditionType_LastTransitionTimeRejectsGarbage(t *testing.T) {
	const ct = ConditionType("Ready")

	obj := new(_MetaConditioned)
	ct.True(obj, "Up", "serving")

	assert.PanicsWithValue(t,
		`kubeapistatus: last transition time must be RFC3339, got "not a time"`,
		func() { ct.LastTransitionTime(obj, "not a time") })
}
