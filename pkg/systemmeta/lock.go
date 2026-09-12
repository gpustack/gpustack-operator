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
// THE RESULT IS NAMED FOR THE STATE IT FOUND, not for the operation, and that is what the name has to
// carry: a true means the finalizer was already there and nothing changed, which is the opposite of
// what "locked" reads as. A signature is what an editor puts at the call site while this comment is
// not, so a result named after the operation invites the caller to write back on the one branch that
// has nothing to write. It has already cost one object a finalizer nothing would ever remove.
//
//	if !systemmeta.Lock(obj) {
//	    _, _ = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, obj)
//	}
func Lock(obj MetaObject) (alreadyLocked bool) {
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
// If the resource is not controlled, returns true,
// otherwise, returns false.
//
// THE RESULT IS NAMED FOR THE STATE IT FOUND, on the same terms as Lock above and for the same
// reason: a true means the finalizer was already absent and this call removed nothing. Read as the
// operation, it is exactly backwards -- the branch that needs the write back is the false one.
//
//	if systemmeta.Unlock(obj) {
//	    return ctrl.Result{}, nil
//	}
//	// Clean
//	_, _ = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, obj)
func Unlock(obj MetaObject) (alreadyUnlocked bool) {
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
