package systemmeta

import (
	"slices"
)

// LockedResourceFinalizer is the finalizer to indicate the resource is locked by system.
const LockedResourceFinalizer = "gpustack.ai/controlled"

// Lock adds a finalizer to the given resource.
//
// If the resource has been controlled, returns true,
// otherwise, returns false.
//
//	if !systemmeta.Lock(obj) {
//	    _, _ = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, obj)
//	}
func Lock(obj MetaObject) (locked bool) {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	if slices.Contains(fs, LockedResourceFinalizer) {
		return true
	}

	fs = append(fs, LockedResourceFinalizer)
	obj.SetFinalizers(fs)
	return false
}

// Unlock removes a finalizer from the given resource.
//
// If the resource is not be controlled, returns true,
// otherwise, returns false.
//
//	if systemmeta.Unlock(obj) {
//	    return ctrl.Result{}, nil
//	}
//	// Clean
//	_, _ = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, obj)
func Unlock(obj MetaObject) (unlocked bool) {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	fs2 := slices.DeleteFunc(fs, func(s string) bool {
		return s == LockedResourceFinalizer
	})
	if len(fs) == len(fs2) {
		return true
	}
	obj.SetFinalizers(fs2)
	return false
}

// IsLocked returns true if the resource is locked by system.
func IsLocked(obj MetaObject) (locked bool) {
	if obj == nil {
		panic("object is nil")
	}

	fs := obj.GetFinalizers()
	return slices.IndexFunc(fs, func(s string) bool {
		return s == LockedResourceFinalizer
	}) != -1
}
