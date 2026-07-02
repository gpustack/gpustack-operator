package kubemeta

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetCondition returns the corresponding conditionType from conditions if present, or nil if not present.
func GetCondition(conditions []meta.Condition, conditionType string) *meta.Condition {
	return apimeta.FindStatusCondition(conditions, conditionType)
}

// SetCondition sets the corresponding condition in conditions to newCondition and returns true
// if the conditions are changed by this call.
// conditions must be non-nil.
//  1. if the condition of the specified type already exists (all fields of the existing condition are updated to
//     newCondition, LastTransitionTime is set to now if the new status differs from the old status)
//  2. if a condition of the specified type does not exist (LastTransitionTime is set to now() if unset, and newCondition is appended)
func SetCondition(conditions *[]meta.Condition, newCondition meta.Condition) (changed bool) {
	return apimeta.SetStatusCondition(conditions, newCondition)
}

// DeleteCondition removes the corresponding conditionType from conditions if present. Returns
// true if it was present and got removed.
// conditions must be non-nil.
func DeleteCondition(conditions *[]meta.Condition, conditionType string) (removed bool) {
	return apimeta.RemoveStatusCondition(conditions, conditionType)
}

// HasCondition returns true if the corresponding conditionType is present in conditions.
func HasCondition(conditions []meta.Condition, conditionType string) bool {
	return apimeta.FindStatusCondition(conditions, conditionType) != nil
}

// IsConditionTrue returns true if the corresponding conditionType is present in conditions and its status is True.
func IsConditionTrue(conditions []meta.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionTrue(conditions, conditionType)
}

// IsConditionFalse returns true if the corresponding conditionType is present in conditions and its status is False.
func IsConditionFalse(conditions []meta.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionFalse(conditions, conditionType)
}

// IsConditionUnknown returns true if the corresponding conditionType is present in conditions and its status is Unknown.
func IsConditionUnknown(conditions []meta.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionPresentAndEqual(conditions, conditionType, meta.ConditionUnknown)
}
