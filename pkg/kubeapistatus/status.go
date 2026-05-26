package kubeapistatus

import (
	"reflect"
	"strings"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type (
	// StatusSummarizer holds the steps and makes a summary of the status's conditions.
	StatusSummarizer interface {
		// GetSummary walks all conditions of the status,
		// then returns the summary phase and phase message.
		GetSummary(status any) (phase, phaseMessage string)
		// Summarize walks all conditions of the status,
		// and applies the summary phase and phase message to the status.
		Summarize(status any)
	}

	// StatusScore is the evaluated score for condition.
	StatusScore int

	// StatusDecide returns readable and sensible status by the given condition status and reason,
	// and moves to next path step if both returning `isError` and `isTransitioning` are false.
	StatusDecide func(st meta.ConditionStatus, reason string) (phase, message string, score StatusScore)
)

const (
	_ StatusScore = iota
	// StatusDone marks the step as done, and moves to next step.
	StatusDone
	// StatusTransitioning marks the step as being transitioning, and quits from the walk.
	StatusTransitioning
	// StatusInterrupted marks the step as interrupted, and quits from the walk.
	StatusInterrupted
	_
)

// NewSummarizer creates a stacking status summarizer by the given steps group,
// and applies the customized decision to the steps group.
//   - `stepsGroup` specifies the path steps in line,
//     logically, move to next step if the current step is done.
//     By default, StatusSummarizer decides to move to the next step on whether the corresponding condition is True status.
//   - `arrange` applies the customized decision,
//     for example, moving to next step on a dedicated step if its status is False,
//     or changing step's display content by its status.
func NewSummarizer[T ~string](stepsGroup [][]T, arranges ...func(Decision[T])) StatusSummarizer {
	if len(stepsGroup) == 0 {
		panic("empty steps group")
	}

	fs := make(paths[T], 0, len(stepsGroup))
	for i := range stepsGroup {
		fs = append(fs, newPath(stepsGroup[i], arranges...))
	}

	return fs
}

// paths stacks a collection of path,
// and picks the highest StatusScore result.
type paths[T ~string] []path[T]

func (ps paths[T]) GetSummary(st any) (phase, phaseMessage string) {
	stValue := reflect.ValueOf(st)
	for stValue.Kind() == reflect.Interface || stValue.Kind() == reflect.Ptr {
		stValue = stValue.Elem()
	}

	phaseField := stValue.FieldByName("Phase")
	if phaseField.IsValid() && phaseField.Kind() == reflect.String {
		phase = phaseField.String()
	}
	phaseMessageField := stValue.FieldByName("PhaseMessage")
	if phaseMessageField.IsValid() && phaseMessageField.Kind() == reflect.String {
		phaseMessage = phaseMessageField.String()
	}

	conditionsField := stValue.FieldByName("Conditions")
	if !conditionsField.IsValid() || conditionsField.Kind() != reflect.Slice {
		return phase, formatMessage(phaseMessage)
	}

	var r *_ScoredStatusDescriptor

	for i := range ps {
		l := ps[i].Walk(conditionsField, phase, phaseMessage)
		if r == nil {
			r = l
			continue
		}

		// Accept the result that has a higher StatusScore.
		ls, rs := l.Score, r.Score
		if ls <= rs {
			continue
		}
		r, rs = l, ls

		// Quit soon if found one highest result.
		if rs > StatusInterrupted {
			break
		}
	}

	if r != nil {
		phase, phaseMessage = r.Phase, r.PhaseMessage
	}

	return phase, formatMessage(phaseMessage)
}

func (ps paths[T]) Summarize(st any) {
	phase, phaseMessage := ps.GetSummary(st)

	stValue := reflect.ValueOf(st)
	for stValue.Kind() == reflect.Interface || stValue.Kind() == reflect.Ptr {
		stValue = stValue.Elem()
	}

	phaseField := stValue.FieldByName("Phase")
	if phaseField.IsValid() && phaseField.CanSet() && phaseField.Kind() == reflect.String {
		phaseField.SetString(phase)
	}
	phaseMessageField := stValue.FieldByName("PhaseMessage")
	if phaseMessageField.IsValid() && phaseMessageField.CanSet() && phaseMessageField.Kind() == reflect.String {
		phaseMessageField.SetString(phaseMessage)
	}
}

// newPath creates a path and initializes it.
func newPath[T ~string](steps []T, arranges ...func(Decision[T])) path[T] {
	if len(steps) == 0 {
		panic("empty steps")
	}

	p := path[T]{
		steps:       steps,
		stepsIndex:  make(map[T]int, len(steps)),
		stepsDecide: make([]StatusDecide, len(steps)),
	}
	for i := range steps {
		// Loop check, panic if found.
		if _, exist := p.stepsIndex[steps[i]]; exist {
			panic("found loop")
		}
		p.stepsIndex[steps[i]] = i
		p.stepsDecide[i] = getGeneralDecide(steps[i])
	}

	// Change the default decide logic after arranging.
	for i := range arranges {
		arranges[i](Decision[T](p))
	}

	return p
}

// path holds the steps and makes a summary of the status's conditions.
type path[T ~string] struct {
	steps       []T
	stepsIndex  map[T]int
	stepsDecide []StatusDecide
}

type (
	_ScoredStatusDescriptor struct {
		Phase        string
		PhaseMessage string
		Score        StatusScore
	}
)

func (f path[T]) Walk(conditionsField reflect.Value, phase, phaseMessage string) *_ScoredStatusDescriptor {
	s := &_ScoredStatusDescriptor{
		Phase:        phase,
		PhaseMessage: phaseMessage,
	}

	// GetSummary the status if condition list is not empty.
	if conditionsField.Len() != 0 {
		// Map conditions with the specified steps for quick indexing.
		stepsConditionIndex := make([]int, len(f.steps))

		for i := 0; i < conditionsField.Len(); i++ {
			cValue := conditionsField.Index(i)
			for cValue.Kind() == reflect.Interface || cValue.Kind() == reflect.Ptr {
				cValue = cValue.Elem()
			}

			var cType T
			{
				typeField := cValue.FieldByName("Type")
				if typeField.IsValid() && typeField.Kind() == reflect.String {
					cType = T(typeField.String())
				}
			}

			// Plus 1 to avoid aligning not found item.
			if idx, exist := f.stepsIndex[cType]; exist {
				stepsConditionIndex[idx] = i + 1
			}
		}

		// GetSummary the path to configure the summary.
		for i := range f.steps {
			if stepsConditionIndex[i] == 0 {
				if i == 0 {
					// Give up the walk if the first step is not found.
					return s
				}
				// Not found step in the given status's condition list.
				continue
			}
			cValue := conditionsField.Index(stepsConditionIndex[i] - 1)

			// Get summary from display result.
			var (
				cStatus meta.ConditionStatus
				cReason string
			)
			{
				statusField := cValue.FieldByName("Status")
				if statusField.IsValid() && statusField.Kind() == reflect.String {
					cStatus = meta.ConditionStatus(statusField.String())
				} else {
					cStatus = meta.ConditionUnknown
				}
				reasonField := cValue.FieldByName("Reason")
				if reasonField.IsValid() && reasonField.Kind() == reflect.String {
					cReason = reasonField.String()
				}
			}

			s.Phase, s.PhaseMessage, s.Score = f.stepsDecide[i](cStatus, cReason)
			if s.PhaseMessage == "" {
				messageField := cValue.FieldByName("Message")
				if messageField.IsValid() && messageField.Kind() == reflect.String {
					s.PhaseMessage = messageField.String()
				}
			}

			// Quit from the walk if still error or being transitioning.
			if s.Score == StatusInterrupted || s.Score == StatusTransitioning {
				break
			}
		}
	}

	// Default summary if it hasn't been configured.
	if s.Phase == "" {
		s.Phase, s.PhaseMessage, s.Score = f.stepsDecide[len(f.steps)-1]("", "")
	}

	return s
}

// Decision exposes ability to customize how to make a decision on one specified step.
type Decision[T ~string] path[T]

// Make makes a decision on the given specified step with dedicated decide logic.
func (d Decision[T]) Make(step T, with StatusDecide) Decision[T] {
	if with != nil {
		if idx, exist := d.stepsIndex[step]; exist {
			d.stepsDecide[idx] = with
		}
	}

	return d
}

// getGeneralDecide returns a decision that adapts general scene, including,
//   - displays step pretty,
//   - marks step as interrupted if status is False,
//   - marks step as transitioning if status is Unknown,
//   - and moves to next step if status is True.
func getGeneralDecide[T ~string](step T) StatusDecide {
	s := string(step)

	// Pretty the display with some rules,
	// most rules are for not present tense word.
	displays := [3]string{s, s, s} // Transitioning, Error, Done.

	for m, r := range replacements {
		if !strings.HasSuffix(s, m) {
			continue
		}
		p := s[:len(s)-len(m)]
		displays[0], displays[1], displays[2] = p+r.T, p+r.E, p+r.D
	}

	return func(st meta.ConditionStatus, _ string) (string, string, StatusScore) {
		switch st {
		case meta.ConditionUnknown:
			return displays[0], "", StatusTransitioning
		case meta.ConditionFalse:
			return displays[1], "", StatusInterrupted
		}

		return displays[2], "", StatusDone
	}
}

// replacements collects the rules for replacing phased descriptor of the key,
// includes transitioning(T), error(E) and done(D).
var replacements = map[string]struct {
	T, E, D string
}{
	"Running":     {"Running", "Failed", "Completed"},
	"Pending":     {"Pending", "Failed", "Pending"},
	"Progressing": {"Progressing", "Progressing", "Progressed"},
	"Imported":    {"Importing", "ImportedFailed", "Imported"},
	"Connected":   {"Connecting", "Disconnected", "Connected"},
	"Initialized": {"Initializing", "InitializeFailed", "Initialized"},
	"Scheduled":   {"Scheduling", "ScheduleFailed", "Scheduled"},
	"Accepted":    {"Accepting", "NotAccepted", "Accepted"},
	"Deployed":    {"Deploying", "DeployFailed", "Deployed"},
	"Stopped":     {"Stopping", "StopFailed", "Stopped"},
	"Synced":      {"Syncing", "SyncFailed", "Synced"},
	"Available":   {"Preparing", "Unavailable", "Available"},
	"Ready":       {"Preparing", "NotReady", "Ready"},
	"Active":      {"Preparing", "Inactive", "Active"},
	"Canceled":    {"Canceling", "CancelFailed", "Canceled"},
	"Planned":     {"Planning", "Failed", "Planned"},
	"Applied":     {"Running", "Failed", "Succeeded"},
}

// formatMessage formats the message with some rules,
// if the first character of the message is lower case, it will be capitalized,
// if the message doesn't end with a punctuation, a dot will be added at the end.
func formatMessage(str string) string {
	if str == "" {
		return str
	}

	runes := []rune(str)
	if runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] = runes[0] - 'a' + 'A'
	}
	if runes[len(runes)-1] != '.' && runes[len(runes)-1] != '!' && runes[len(runes)-1] != '?' {
		runes = append(runes, '.')
	}

	return string(runes)
}
