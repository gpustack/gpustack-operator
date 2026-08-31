package kubeapistatus

import (
	"fmt"
	"reflect"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
)

// ConditionType is the type of status.
type ConditionType string

// IsTrue check status value for object,
// object must be a pointer.
func (c ConditionType) IsTrue(obj any) bool {
	return getStatus(obj, string(c)) == string(meta.ConditionTrue)
}

// IsFalse check status value for object,
// object must be a pointer.
func (c ConditionType) IsFalse(obj any) bool {
	return getStatus(obj, string(c)) == string(meta.ConditionFalse)
}

// IsUnknown check status value for object,
// object must be a pointer.
func (c ConditionType) IsUnknown(obj any) bool {
	return getStatus(obj, string(c)) == string(meta.ConditionUnknown)
}

// IsTrueOrFalse check status value for object,
// which must be True or False,
// object must be a pointer.
func (c ConditionType) IsTrueOrFalse(obj any) bool {
	cond := findCond(obj, string(c))
	if cond == nil {
		return false
	}
	st := getFieldValue(*cond, "Status").String()
	return st == string(meta.ConditionTrue) || st == string(meta.ConditionFalse)
}

// IsTrueOrUnknown check status value for object,
// which must be Unknown or True,
// object must be a pointer.
func (c ConditionType) IsTrueOrUnknown(obj any) bool {
	cond := findCond(obj, string(c))
	if cond == nil {
		return false
	}
	st := getFieldValue(*cond, "Status").String()
	return st == string(meta.ConditionTrue) || st == string(meta.ConditionUnknown)
}

// IsFalseOrUnknown check status value for object,
// which must be Unknown or False,
// object must be a pointer.
func (c ConditionType) IsFalseOrUnknown(obj any) bool {
	cond := findCond(obj, string(c))
	if cond == nil {
		return false
	}
	st := getFieldValue(*cond, "Status").String()
	return st == string(meta.ConditionFalse) || st == string(meta.ConditionUnknown)
}

// Exists check conditionType exists in object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Exists(obj any) bool {
	return findCond(obj, string(c)) != nil
}

// GetStatus get status from conditionType for object field .Status.Conditions.
func (c ConditionType) GetStatus(obj any) string {
	return getStatus(obj, string(c))
}

// GetMessage get message from conditionType for object field .Status.Conditions.
func (c ConditionType) GetMessage(obj any) string {
	cond := findCond(obj, string(c))
	if cond == nil {
		return ""
	}
	return getFieldValue(*cond, "Message").String()
}

// GetReason get reason from conditionType for object field .Status.Conditions.
func (c ConditionType) GetReason(obj any) string {
	cond := findCond(obj, string(c))
	if cond == nil {
		return ""
	}
	return getFieldValue(*cond, "Reason").String()
}

// GetLastTransitionTime get last transition time for conditionType from object field .Status.Conditions.
func (c ConditionType) GetLastTransitionTime(obj any) string {
	return getTS(obj, string(c))
}

// True set status value to True for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) True(obj any, reason, message string) {
	setStatus(obj, string(c), string(meta.ConditionTrue), reason, message)
}

// False set status value to False for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) False(obj any, reason, message string) {
	setStatus(obj, string(c), string(meta.ConditionFalse), reason, message)
}

// Unknown set status value to Unknown for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Unknown(obj any, reason, message string) {
	setStatus(obj, string(c), string(meta.ConditionUnknown), reason, message)
}

// Status set status value to custom value for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Status(obj any, status, reason, message string) {
	setStatus(obj, string(c), status, reason, message)
}

// Message set message to conditionType for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Message(obj any, message string) {
	cond := upsertCond(obj, string(c))
	setValue(cond, "Message", message)
}

// Reason set reason to conditionType for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Reason(obj any, reason string) {
	cond := upsertCond(obj, string(c))
	getFieldValue(cond, "Reason").SetString(reason)
}

// LastTransitionTime set last transition time to conditionType for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) LastTransitionTime(obj any, ts string) {
	upsertTS(obj, string(c), ts)
}

// SetError set error to conditionType for object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) SetError(obj any, reason string, err error) {
	if reason == "" {
		reason = "Error"
	}
	c.False(obj, reason, err.Error())
}

// SetMessageIfBlank set message to conditionType for object field .Status.Conditions if message is blank,
// object must be a pointer.
func (c ConditionType) SetMessageIfBlank(obj any, message string) {
	if c.GetMessage(obj) == "" {
		c.Message(obj, message)
	}
}

// Reset clean the object field .Status.Conditions,
// and set the status as Unknown type into the object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) Reset(obj any, reason, message string) {
	resetCond(obj, string(c), string(meta.ConditionUnknown), reason, message)
}

// ResetTrue clean the object field .Status.Conditions,
// and set the status as True type into the object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) ResetTrue(obj any, reason, message string) {
	resetCond(obj, string(c), string(meta.ConditionTrue), reason, message)
}

// ResetFalse clean the object field .Status.Conditions,
// and set the status as False type into the object field .Status.Conditions,
// object must be a pointer.
func (c ConditionType) ResetFalse(obj any, reason, message string) {
	resetCond(obj, string(c), string(meta.ConditionFalse), reason, message)
}

// upsertTS create to update condition and set last transition time.
//
// The field is a meta.Time, so the value has to be built and Set rather than SetString — the same
// way setTS does it below. ts is RFC3339, which is how a meta.Time serializes, so what getTS gives
// back is what this takes.
//
// An unparseable ts panics: every caller passes a formatted value, so it is programmer error, and
// this file already panics on the other one it can detect — a non-pointer object.
func upsertTS(obj any, condName, ts string) {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(fmt.Sprintf("kubeapistatus: last transition time must be RFC3339, got %q", ts))
	}

	cond := upsertCond(obj, condName)
	getFieldValue(cond, "LastTransitionTime").Set(reflect.ValueOf(meta.Time{Time: parsed.UTC()}))
}

// setTS set last transition time to condition.
func setTS(value reflect.Value) {
	now := meta.Time{
		Time: time.Now().UTC(),
	}

	getFieldValue(value, "LastTransitionTime").Set(reflect.ValueOf(now))
}

// getTS get last transition time from condition.
//
// It reads the meta.Time out of the field and formats it. Calling String() on the field directly
// would render the struct — "<meta.Time Value>" — rather than a time, which is a value no caller
// can compare or display. A condition that carries no stamp reports nothing, the same as a
// condition that is not there: a zero time formatted would read as the year 1.
func getTS(obj any, condName string) string {
	cond := findCond(obj, condName)
	if cond == nil {
		return ""
	}

	ts, ok := getFieldValue(*cond, "LastTransitionTime").Interface().(meta.Time)
	if !ok || ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

// getStatus get status from condition.
func getStatus(obj any, condName string) string {
	cond := findCond(obj, condName)
	if cond == nil {
		return ""
	}
	return getFieldValue(*cond, "Status").String()
}

// setStatus set status and message to condition.
func setStatus(obj any, condName, status, reason, message string) {
	if reflect.TypeOf(obj).Kind() != reflect.Ptr {
		panic("obj passed must be a pointer")
	}

	cond := upsertCond(obj, condName)

	// Read the previous status BEFORE overwriting it. LastTransitionTime marks a TRANSITION, so it
	// may move only when Status actually changes — a controller that rewrites its conditions on
	// every pass would otherwise stamp a new time every pass, and a status write guarded by
	// equality would then fire forever, each write waking the next reconcile.
	//
	// getFieldValue, not getValue: cond is already a reflect.Value, while getValue takes an "any"
	// and reflects over whatever it is handed — given a reflect.Value it reflects over that struct,
	// whose "Status" field does not exist. The comparison therefore never matched and the timestamp
	// moved on every write, including a write that changed nothing.
	transitioned := getFieldValue(cond, "Status").String() != status

	setValue(cond, "Status", status)
	setValue(cond, "Reason", reason)
	setValue(cond, "Message", message)

	if transitioned {
		setTS(cond)
	}
}

// getValue get value from object with field names.
func getValue(obj any, name string, snames ...string) reflect.Value {
	if obj == nil {
		return reflect.Value{}
	}
	v := reflect.ValueOf(obj)
	t := v.Type()
	if t.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	field := v.FieldByName(name)
	if len(snames) == 0 {
		return field
	}
	return getFieldValue(field, snames[0], snames[1:]...)
}

// setValue set value to condition.
func setValue(cond reflect.Value, fieldName, newValue string) {
	value := getFieldValue(cond, fieldName)
	if value.String() != newValue {
		value.SetString(newValue)
	}
}

// findCond find condition from object.
func findCond(obj any, condName string) *reflect.Value {
	condSlice := getValue(obj, "Status", "Conditions")
	if !condSlice.IsValid() {
		condSlice = getValue(obj, "Conditions")
	}
	return queryCondsByName(obj, condSlice, condName)
}

// upsertCond create or update condition.
func upsertCond(obj any, condName string) reflect.Value {
	conds := getValue(obj, "Status", "Conditions")
	cond := queryCondsByName(obj, conds, condName)
	if cond != nil {
		return *cond
	}

	newCond := reflect.New(conds.Type().Elem()).Elem()
	newCond.FieldByName("Type").SetString(condName)
	newCond.FieldByName("Status").SetString("Unknown")
	setTS(newCond)

	conds.Set(reflect.Append(conds, newCond))
	return *queryCondsByName(obj, conds, condName)
}

// resetCond clean the object field .Status.Conditions, and set the status to Unknown.
func resetCond(obj any, condName, status, reason, message string) {
	conds := getValue(obj, "Status", "Conditions")

	newCond := reflect.New(conds.Type().Elem()).Elem()
	newCond.FieldByName("Type").SetString(condName)
	newCond.FieldByName("Status").SetString(status)
	newCond.FieldByName("Reason").SetString(reason)
	newCond.FieldByName("Message").SetString(message)
	setTS(newCond)

	slice := reflect.MakeSlice(reflect.SliceOf(newCond.Type()), 0, 0)
	conds.Set(reflect.Append(slice, newCond))
}

// queryCondsByName query condition by name.
func queryCondsByName(obj any, val reflect.Value, condName string) *reflect.Value {
	defer func() {
		if recover() != nil {
			klog.Fatalf("failed to find .Status.Conditions field on %v", reflect.TypeOf(obj))
		}
	}()

	for i := 0; i < val.Len(); i++ {
		cond := val.Index(i)
		typeVal := getFieldValue(cond, "Type")
		if typeVal.String() == condName {
			return &cond
		}
	}

	return nil
}

// getFieldValue get value from field names.
func getFieldValue(v reflect.Value, name string, snames ...string) reflect.Value {
	if !v.IsValid() {
		return v
	}
	field := v.FieldByName(name)
	if len(snames) == 0 {
		return field
	}
	return getFieldValue(field, snames[0], snames[1:]...)
}
