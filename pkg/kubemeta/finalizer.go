package kubemeta

import "slices"

// AddFinalizer adds a finalizer to the given resource.
func AddFinalizer(obj MetaObject, finalizer string) (added bool) {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	if slices.Contains(fs, finalizer) {
		return false
	}

	fs = append(fs, finalizer)
	obj.SetFinalizers(fs)
	return true
}

// RemoveFinalizer removes a finalizer from the given resource.
func RemoveFinalizer(obj MetaObject, finalizer string) (removed bool) {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	fs2 := slices.DeleteFunc(fs, func(s string) bool {
		return s == finalizer
	})
	if len(fs) == len(fs2) {
		return false
	}
	obj.SetFinalizers(fs2)
	return true
}

// HasFinalizer returns true if the given resource has the finalizer.
func HasFinalizer(obj MetaObject, finalizer string) bool {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	return slices.Contains(fs, finalizer)
}
