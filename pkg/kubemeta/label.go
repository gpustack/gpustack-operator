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

// SanitizeLabelValue converts a given string to a valid Kubernetes Label Value by following rules:
// - Must be 63 characters or less,
// - Unless empty, must begin and end with an alphanumeric character ([a-z0-9A-Z]),
// - Could contain dashes (-), underscores (_), dots (.), and alphanumerics between.
func SanitizeLabelValue(s string) string {
	if s == "" {
		return ""
	}

	const maxLength = 63

	buf := make([]rune, 0, min(len(s), maxLength))
	var pr rune
	for _, r := range s {
		switch {
		case isAlphanumericChar(r):
		case r == ' ':
			r = '-'
			fallthrough
		case isValidSignChar(r):
			if len(buf) == 0 {
				continue
			}
			if isValidSignChar(pr) {
				continue
			}
		default:
			continue
		}
		buf = append(buf, r)
		pr = r

		// Stop processing if the buffer has reached the maximum length.
		if len(buf) >= maxLength {
			break
		}
	}
	if len(buf) == 0 {
		return ""
	}

	// Trim trailing non-alphanumeric character.
	if isValidSignChar(buf[len(buf)-1]) {
		buf = buf[:len(buf)-1]
	}

	return string(buf)
}

func isAlphanumericChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isValidSignChar(r rune) bool {
	return r == '-' || r == '_' || r == '.'
}
