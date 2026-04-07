package kubemeta

// GetLabel returns the value of the label with the given key.
func GetLabel(obj MetaObject, key string) (string, bool) {
	if obj == nil {
		panic("object is nil")
	}

	ls := obj.GetLabels()
	if ls == nil {
		return "", false
	}

	v, ok := ls[key]
	return v, ok
}

// SetLabel sets the value of the label with the given key.
//
// If the label exists, its value will be updated.
func SetLabel(obj MetaObject, key, value string) {
	if obj == nil {
		panic("object is nil")
	}

	ls := obj.GetLabels()
	if ls == nil {
		ls = map[string]string{}
	}

	ls[key] = value
	obj.SetLabels(ls)
}

// AddLabel adds the label with the given key and value.
//
// If the label already exists, it will not be added.
func AddLabel(obj MetaObject, key, value string) {
	if obj == nil {
		panic("object is nil")
	}

	ls := obj.GetLabels()
	if ls == nil {
		ls = map[string]string{}
	}

	if _, ok := ls[key]; ok {
		return
	}

	ls[key] = value
	obj.SetLabels(ls)
}

// IsLabeled returns true if the label with the given key and value exists.
func IsLabeled(obj MetaObject, key, value string) bool {
	v, ok := GetLabel(obj, key)
	return ok && v == value
}

// HasLabel returns true if the label with the given key exists.
func HasLabel(obj MetaObject, key string) bool {
	_, ok := GetLabel(obj, key)
	return ok
}

// DeleteLabel deletes the label with the given key.
func DeleteLabel(obj MetaObject, key string) {
	if obj == nil {
		panic("object is nil")
	}

	ls := obj.GetLabels()
	if ls == nil {
		return
	} else if _, ok := ls[key]; !ok {
		return
	}

	delete(ls, key)
	obj.SetLabels(ls)
}
