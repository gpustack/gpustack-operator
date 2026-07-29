package kubeclientset

import (
	"context"
	"errors"
	"reflect"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	// MetaObject is the interface for the object with metadata.
	MetaObject = ctrlcli.Object

	// AlignWithFn is a function to compare the actual object with the expected object,
	// and returns aligned object if the actual object is no the same with the expected object.
	AlignWithFn[T MetaObject] func(actualCopied T) (aligned T, skip bool, err error)

	// CompareWithFn is a function to compare the actual object with the expected object,
	// and returns true if the actual object is the same with the expected object.
	CompareWithFn[T MetaObject] func(actualCopied T) (skip bool)

	// PatchAlignWithFn is a function to compare the actual object with the expected object,
	// and returns the patch data if the actual object is no the same with the expected object.
	PatchAlignWithFn[T MetaObject] func(actualCopied T) (patched []byte, skip bool, err error)
)

type (
	GetClient[T MetaObject] interface {
		Get(ctx context.Context, name string, opts meta.GetOptions) (T, error)
	}

	CreateClient[T MetaObject] interface {
		Create(ctx context.Context, obj T, opts meta.CreateOptions) (T, error)
	}

	_CreateOptions[T MetaObject] struct {
		meta.CreateOptions
		UpdateAlignFunc     AlignWithFn[T]
		RecreateCompareFunc CompareWithFn[T]
	}

	CreateOption[T MetaObject] func(*_CreateOptions[T])
)

// WithCreateMetaOptions sets the create options.
func WithCreateMetaOptions[T MetaObject](opts meta.CreateOptions) CreateOption[T] {
	return func(co *_CreateOptions[T]) {
		co.CreateOptions = opts
	}
}

// WithUpdateIfExisted with the align function to update the resource if existed.
//
// WithUpdateIfExisted is conflict to WithRecreateIfDuplicated, if both provided,
// WithUpdateIfExisted will be used.
func WithUpdateIfExisted[T MetaObject](fn AlignWithFn[T]) CreateOption[T] {
	return func(co *_CreateOptions[T]) {
		co.UpdateAlignFunc = fn
	}
}

// WithRecreateIfDuplicated with the compare function to recreate the resource if different.
//
// WithRecreateIfDuplicated is conflict to WithUpdateIfExisted, if both provided,
// WithUpdateIfExisted will be used.
func WithRecreateIfDuplicated[T MetaObject](fn CompareWithFn[T]) CreateOption[T] {
	return func(co *_CreateOptions[T]) {
		co.RecreateCompareFunc = fn
	}
}

// Create is similar to Apply, will create the resource if it does not exist.
//
// Create updates the resource if WithUpdateIfExisted provided,
// or recreate the resource if WithRecreateIfDuplicated provided.
// Select one from WithUpdateIfExisted and WithRecreateIfDuplicated, if both provided,
// WithUpdateIfExisted will be used.
func Create[T MetaObject](ctx context.Context, cli CreateClient[T], expected T, opts ...CreateOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var co _CreateOptions[T]
	for i := range opts {
		opts[i](&co)
	}

	var (
		name   = expected.GetName()
		err    = errors.New("resource name may not be empty")
		actual T
	)

	if name != "" {
		if getter, ok := cli.(GetClient[T]); ok {
			actual, err = getter.Get(ctx, name,
				readOptions(ctx))
			if err != nil && !kerrors.IsNotFound(err) {
				if rctx, ok := retryTransient(ctx, err); ok {
					return Create(rctx, cli, expected, opts...)
				}
				return actual, err
			}
		}
	}

	// Create if not found or deleting.
	if err != nil || actual.GetDeletionTimestamp() != nil {
		deleting := err == nil && actual.GetDeletionTimestamp() != nil && len(actual.GetFinalizers()) == 0
		if deleting {
			// NB(thxCode): sleep a while to avoid server flipping.
			time.Sleep(10 * time.Millisecond)
		}
		actual, err = cli.Create(ctx, expected, meta.CreateOptions{
			DryRun: co.DryRun,
		})
		if err != nil {
			switch {
			case isRetryError(err):
				if rctx, ok := retryTransient(ctx, err); ok {
					return Create(rctx, cli, expected, opts...)
				}
			case kerrors.IsAlreadyExists(err):
				// Retry on already existed if:
				// - configure align function.
				// - configure compare function.
				// - the resource is deleting without finalizers.
				if co.UpdateAlignFunc != nil || co.RecreateCompareFunc != nil || deleting {
					if rctx, ok := nextAttempt(ctx, err); ok {
						return Create(rctx, cli, expected, opts...)
					}
				} else {
					err = nil
				}
			}
		}
		return actual, err
	}

	switch {
	case co.UpdateAlignFunc != nil:
		var (
			copied T
			skip   bool
		)
		copied, skip, err = co.UpdateAlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}

		updater, ok := cli.(UpdateClient[T])
		if !ok {
			return actual, errors.New("client does not support update")
		}

		// Copy resource version for update.
		//
		// And keep the original labels, annotations, finalizers, and owner references if they are not set.
		// If you want to clean the above fields, please set them to empty in the expected object.
		copiedOm, actualOm := copied, actual
		copiedOm.SetResourceVersion(actualOm.GetResourceVersion())
		if copiedOm.GetLabels() == nil {
			copiedOm.SetLabels(actualOm.GetLabels())
		}
		if copiedOm.GetAnnotations() == nil {
			copiedOm.SetAnnotations(actualOm.GetAnnotations())
		}
		if copiedOm.GetFinalizers() == nil {
			copiedOm.SetFinalizers(actualOm.GetFinalizers())
		}
		if copiedOm.GetOwnerReferences() == nil {
			copiedOm.SetOwnerReferences(actualOm.GetOwnerReferences())
		}

		updated, err := updater.Update(ctx, copied, meta.UpdateOptions{
			DryRun: co.DryRun,
		})
		if err == nil {
			return updated, nil
		}

		// Retry if the server asked to back off, or if another writer got there first:
		// the next pass reads the actual resource again and aligns on top of it, so a
		// writer that lost a concurrent update converges instead of failing.
		//
		// Any one of these reasons is enough to retry, hence the disjunction: they are
		// mutually exclusive, so requiring them together could never hold.
		if rctx, ok := retryToConverge(ctx, err); ok {
			return Create(rctx, cli, expected, opts...)
		}

		return updated, err
	case co.RecreateCompareFunc != nil:
		skip := co.RecreateCompareFunc(actual.DeepCopyObject().(T))
		if skip {
			return actual, nil
		}

		deleter, ok := cli.(DeleteClient)
		if !ok {
			return actual, errors.New("client does not support delete")
		}

		err = deleter.Delete(ctx, name, meta.DeleteOptions{
			DryRun:            co.DryRun,
			PropagationPolicy: ptr.To(meta.DeletePropagationForeground),
		})
		if err != nil && !kerrors.IsNotFound(err) && !isRetryError(err) {
			return actual, err
		}

		// Recreate, bounded like every other re-entry here: a delete that keeps being
		// followed by a live object would otherwise recurse forever. No server error was
		// returned on this path, so a spent budget has to be its own.
		if rctx, ok := nextAttempt(ctx, nil); ok {
			return Create(rctx, cli, expected, opts...)
		}

		return actual, errTooManyAttempts
	}

	return actual, nil
}

// CreateWithCtrlClient is similar to Create, but uses the ctrl client.
func CreateWithCtrlClient[T MetaObject](ctx context.Context, cli ctrlcli.Client, expected T, opts ...CreateOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var co _CreateOptions[T]
	for i := range opts {
		opts[i](&co)
	}

	var (
		name   = expected.GetName()
		err    = errors.New("resource name may not be empty")
		actual = expected.DeepCopyObject().(T)
	)

	if name != "" {
		err = cli.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual)
		if err != nil && !kerrors.IsNotFound(err) {
			if rctx, ok := retryTransient(ctx, err); ok {
				return CreateWithCtrlClient[T](rctx, cli, expected, opts...)
			}
			return actual, err
		}
	}

	// Create if not found or deleting.
	if err != nil || actual.GetDeletionTimestamp() != nil {
		deleting := err == nil && actual.GetDeletionTimestamp() != nil && len(actual.GetFinalizers()) == 0
		if deleting {
			// NB(thxCode): sleep a while to avoid server flipping.
			time.Sleep(10 * time.Millisecond)
		}
		err = cli.Create(ctx, expected, &ctrlcli.CreateOptions{
			DryRun:       co.DryRun,
			FieldManager: co.FieldManager,
			Raw:          ptr.To(co.CreateOptions),
		})
		if err == nil {
			return expected, nil
		}
		switch {
		case isRetryError(err):
			if rctx, ok := retryTransient(ctx, err); ok {
				return CreateWithCtrlClient[T](rctx, cli, expected, opts...)
			}
		case kerrors.IsAlreadyExists(err):
			// Retry on already existed if:
			// - configure align function.
			// - configure compare function.
			// - the resource is deleting without finalizers.
			if co.UpdateAlignFunc != nil || co.RecreateCompareFunc != nil || deleting {
				if rctx, ok := nextAttempt(ctx, err); ok {
					return CreateWithCtrlClient[T](rctx, cli, expected, opts...)
				}
			} else {
				err = nil
			}
		}
		return actual, err
	}

	switch {
	case co.UpdateAlignFunc != nil:
		var (
			copied T
			skip   bool
		)
		copied, skip, err = co.UpdateAlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}

		// Copy resource version for update.
		//
		// And keep the original labels, annotations, finalizers, and owner references if they are not set.
		// If you want to clean the above fields, please set them to empty in the expected object.
		copiedOm, actualOm := copied, actual
		copiedOm.SetResourceVersion(actualOm.GetResourceVersion())
		if copiedOm.GetLabels() == nil {
			copiedOm.SetLabels(actualOm.GetLabels())
		}
		if copiedOm.GetAnnotations() == nil {
			copiedOm.SetAnnotations(actualOm.GetAnnotations())
		}
		if copiedOm.GetFinalizers() == nil {
			copiedOm.SetFinalizers(actualOm.GetFinalizers())
		}
		if copiedOm.GetOwnerReferences() == nil {
			copiedOm.SetOwnerReferences(actualOm.GetOwnerReferences())
		}

		err = cli.Update(ctx, copied, &ctrlcli.UpdateOptions{
			DryRun: co.DryRun,
			Raw:    &meta.UpdateOptions{DryRun: co.DryRun},
		})
		if err == nil {
			return copied, nil
		}

		// Retry if the server asked to back off, or if another writer got there first,
		// as in Create.
		if rctx, ok := retryToConverge(ctx, err); ok {
			return CreateWithCtrlClient[T](rctx, cli, expected, opts...)
		}

		return copied, err
	case co.RecreateCompareFunc != nil:
		skip := co.RecreateCompareFunc(actual.DeepCopyObject().(T))
		if skip {
			return actual, nil
		}

		err = cli.Delete(ctx, expected, &ctrlcli.DeleteOptions{
			DryRun:            co.DryRun,
			PropagationPolicy: ptr.To(meta.DeletePropagationForeground),
		})
		if err != nil && !kerrors.IsNotFound(err) && !isRetryError(err) {
			return actual, err
		}

		// Recreate, bounded like every other re-entry here: a delete that keeps being
		// followed by a live object would otherwise recurse forever. No server error was
		// returned on this path, so a spent budget has to be its own.
		if rctx, ok := nextAttempt(ctx, nil); ok {
			return CreateWithCtrlClient[T](rctx, cli, expected, opts...)
		}

		return actual, errTooManyAttempts
	}

	return actual, nil
}

type (
	UpdateClient[T MetaObject] interface {
		Update(ctx context.Context, obj T, opts meta.UpdateOptions) (T, error)
		Get(ctx context.Context, name string, opts meta.GetOptions) (T, error)
	}

	_UpdateOptions[T MetaObject] struct {
		meta.UpdateOptions
		AlignFunc          AlignWithFn[T]
		CreateIfNotExisted bool
	}

	UpdateOption[T MetaObject] func(*_UpdateOptions[T])
)

// WithUpdateMetaOptions sets the update options.
func WithUpdateMetaOptions[T MetaObject](opts meta.UpdateOptions) UpdateOption[T] {
	return func(uo *_UpdateOptions[T]) {
		uo.UpdateOptions = opts
	}
}

// WithUpdateAlign with the align function to update the resource.
func WithUpdateAlign[T MetaObject](fn AlignWithFn[T]) UpdateOption[T] {
	return func(uo *_UpdateOptions[T]) {
		uo.AlignFunc = fn
	}
}

// WithCreateIfNotExisted will create the resource if it does not exist.
func WithCreateIfNotExisted[T MetaObject]() UpdateOption[T] {
	return func(uo *_UpdateOptions[T]) {
		uo.CreateIfNotExisted = true
	}
}

// Update will update the resource if it exists,
// and returns the updated resource.
//
// Update returns error if the resource is not found or updating failed.
//
// Update will retry if the resource is updating conflicted when AlignWithFn is provided.
func Update[T MetaObject](ctx context.Context, cli UpdateClient[T], expected T, opts ...UpdateOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var uo _UpdateOptions[T]
	for i := range opts {
		opts[i](&uo)
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	actual, err := cli.Get(ctx, name,
		readOptions(ctx))
	if err != nil {
		if kerrors.IsNotFound(err) && uo.CreateIfNotExisted {
			creator, ok := cli.(CreateClient[T])
			if !ok {
				return actual, errors.New("client does not support create")
			}
			actual, err = creator.Create(ctx, expected, meta.CreateOptions{
				DryRun: uo.DryRun,
			})
			if err != nil && kerrors.IsAlreadyExists(err) {
				// Retry if already existed.
				if rctx, ok := nextAttempt(ctx, err); ok {
					return Update(rctx, cli, expected, opts...)
				}
			}
		}
		if rctx, ok := retryTransient(ctx, err); ok {
			return Update(rctx, cli, expected, opts...)
		}
		return actual, err
	}

	var copied T
	if uo.AlignFunc != nil {
		var skip bool
		copied, skip, err = uo.AlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}
	} else {
		copied = expected.DeepCopyObject().(T)
		// Copy resource version for update.
		//
		// And keep the original labels, annotations, finalizers, and owner references if they are not set.
		// If you want to clean the above fields, please set them to empty in the expected object.
		copiedOm, actualOm := copied, actual
		copiedOm.SetResourceVersion(actualOm.GetResourceVersion())
		if copiedOm.GetLabels() == nil {
			copiedOm.SetLabels(actualOm.GetLabels())
		}
		if copiedOm.GetAnnotations() == nil {
			copiedOm.SetAnnotations(actualOm.GetAnnotations())
		}
		if copiedOm.GetFinalizers() == nil {
			copiedOm.SetFinalizers(actualOm.GetFinalizers())
		}
		if copiedOm.GetOwnerReferences() == nil {
			copiedOm.SetOwnerReferences(actualOm.GetOwnerReferences())
		}
	}

	updated, err := cli.Update(ctx, copied, meta.UpdateOptions{
		DryRun: uo.DryRun,
	})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return Update(rctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			if rctx, ok := nextAttempt(ctx, err); ok {
				return Update(rctx, cli, expected, opts...)
			}
		}
	}

	return updated, err
}

// UpdateWithCtrlClient is similar to Update, but uses the ctrl client.
func UpdateWithCtrlClient[T MetaObject](ctx context.Context, cli ctrlcli.Client, expected T, opts ...UpdateOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var uo _UpdateOptions[T]
	for i := range opts {
		opts[i](&uo)
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	actual := expected.DeepCopyObject().(T)
	err := cli.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual)
	if err != nil {
		if kerrors.IsNotFound(err) && uo.CreateIfNotExisted {
			actual = expected.DeepCopyObject().(T)
			err = cli.Create(ctx, actual, &ctrlcli.CreateOptions{
				DryRun:       uo.DryRun,
				FieldManager: uo.FieldManager,
			})
			if err != nil && kerrors.IsAlreadyExists(err) {
				// Retry if already existed.
				if rctx, ok := nextAttempt(ctx, err); ok {
					return UpdateWithCtrlClient[T](rctx, cli, expected, opts...)
				}
			}
		}
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateWithCtrlClient[T](rctx, cli, expected, opts...)
		}
		return actual, err
	}

	var copied T
	if uo.AlignFunc != nil {
		var skip bool
		copied, skip, err = uo.AlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}
	} else {
		copied = expected.DeepCopyObject().(T)
		// Copy resource version for update.
		//
		// And keep the original labels, annotations, finalizers, and owner references if they are not set.
		// If you want to clean the above fields, please set them to empty in the expected object.
		copiedOm, actualOm := copied, actual
		copiedOm.SetResourceVersion(actualOm.GetResourceVersion())
		if copiedOm.GetLabels() == nil {
			copiedOm.SetLabels(actualOm.GetLabels())
		}
		if copiedOm.GetAnnotations() == nil {
			copiedOm.SetAnnotations(actualOm.GetAnnotations())
		}
		if copiedOm.GetFinalizers() == nil {
			copiedOm.SetFinalizers(actualOm.GetFinalizers())
		}
		if copiedOm.GetOwnerReferences() == nil {
			copiedOm.SetOwnerReferences(actualOm.GetOwnerReferences())
		}
	}

	updated := copied
	err = cli.Update(ctx, updated, &ctrlcli.UpdateOptions{
		DryRun:       uo.DryRun,
		FieldManager: uo.FieldManager,
		Raw:          ptr.To(uo.UpdateOptions),
	})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateWithCtrlClient[T](rctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			if rctx, ok := nextAttempt(ctx, err); ok {
				return UpdateWithCtrlClient[T](rctx, cli, expected, opts...)
			}
		}
	}

	return updated, err
}

type (
	UpdateStatusClient[T MetaObject] interface {
		UpdateStatus(ctx context.Context, obj T, opts meta.UpdateOptions) (T, error)
		Get(ctx context.Context, name string, opts meta.GetOptions) (T, error)
	}

	_UpdateStatusOptions[T MetaObject] struct {
		meta.UpdateOptions
		AlignFunc AlignWithFn[T]
	}

	UpdateStatusOption[T MetaObject] func(*_UpdateStatusOptions[T])
)

// WithUpdateStatusMetaOptions sets the update status options.
func WithUpdateStatusMetaOptions[T MetaObject](opts meta.UpdateOptions) UpdateStatusOption[T] {
	return func(uso *_UpdateStatusOptions[T]) {
		uso.UpdateOptions = opts
	}
}

// WithUpdateStatusAlign with the align function to update the resource status.
func WithUpdateStatusAlign[T MetaObject](fn AlignWithFn[T]) UpdateStatusOption[T] {
	return func(uso *_UpdateStatusOptions[T]) {
		uso.AlignFunc = fn
	}
}

// UpdateStatus will update the resource status if it exists,
// and returns the updated resource.
//
// UpdateStatus returns error if the resource is not found or updating failed.
//
// UpdateStatus will retry if the resource is updating conflicted when AlignWithFn is provided.
func UpdateStatus[T MetaObject](ctx context.Context, cli UpdateStatusClient[T], expected T, opts ...UpdateStatusOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var uo _UpdateStatusOptions[T]
	for i := range opts {
		opts[i](&uo)
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	actual, err := cli.Get(ctx, name,
		readOptions(ctx))
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateStatus(rctx, cli, expected, opts...)
		}
		return actual, err
	}

	var copied T
	if uo.AlignFunc != nil {
		var skip bool
		copied, skip, err = uo.AlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}
	} else {
		copied = expected.DeepCopyObject().(T)
	}

	updated, err := cli.UpdateStatus(ctx, copied, meta.UpdateOptions{
		DryRun:          uo.DryRun,
		FieldManager:    uo.FieldManager,
		FieldValidation: uo.FieldValidation,
	})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateStatus(rctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			if rctx, ok := nextAttempt(ctx, err); ok {
				return UpdateStatus(rctx, cli, expected, opts...)
			}
		}
	}

	return updated, err
}

// UpdateStatusWithCtrlClient is similar to UpdateStatus, but uses the ctrl client.
func UpdateStatusWithCtrlClient[T MetaObject](ctx context.Context, cli ctrlcli.Client, expected T, opts ...UpdateStatusOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	var uo _UpdateStatusOptions[T]
	for i := range opts {
		opts[i](&uo)
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	actual := expected.DeepCopyObject().(T)
	err := cli.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual,
		&ctrlcli.GetOptions{
			Raw: ptr.To(readOptions(ctx)),
		})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateStatusWithCtrlClient[T](rctx, cli, expected, opts...)
		}
		return actual, err
	}

	var copied T
	if uo.AlignFunc != nil {
		var skip bool
		copied, skip, err = uo.AlignFunc(actual.DeepCopyObject().(T))
		if err != nil {
			return actual, err
		}
		if skip {
			return actual, nil
		}
	} else {
		copied = expected.DeepCopyObject().(T)
	}

	updated := copied
	err = cli.Status().Update(ctx, updated, &ctrlcli.SubResourceUpdateOptions{
		UpdateOptions: ctrlcli.UpdateOptions{
			DryRun:          uo.DryRun,
			FieldManager:    uo.FieldManager,
			FieldValidation: uo.FieldValidation,
			Raw:             ptr.To(uo.UpdateOptions),
		},
	})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return UpdateStatusWithCtrlClient[T](rctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			if rctx, ok := nextAttempt(ctx, err); ok {
				return UpdateStatusWithCtrlClient[T](rctx, cli, expected, opts...)
			}
		}
	}

	return updated, err
}

type (
	PatchClient[T MetaObject] interface {
		Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts meta.PatchOptions, subresources ...string) (T, error)
		Get(ctx context.Context, name string, opts meta.GetOptions) (T, error)
	}

	_PatchOptions[T MetaObject] struct {
		meta.PatchOptions
		AlignFunc PatchAlignWithFn[T]
	}

	PatchOption[T MetaObject] func(*_PatchOptions[T])
)

// WithPatchMetaOptions sets the patch options.
func WithPatchMetaOptions[T MetaObject](opts meta.PatchOptions) PatchOption[T] {
	return func(po *_PatchOptions[T]) {
		po.PatchOptions = opts
	}
}

// WithPatchAlign with the align function to patch the resource.
func WithPatchAlign[T MetaObject](fn PatchAlignWithFn[T]) PatchOption[T] {
	return func(po *_PatchOptions[T]) {
		po.AlignFunc = fn
	}
}

// Patch will patch the resource if it exists,
// and returns the patched resource.
//
// Patch returns error if the resource is not found or patching failed.
//
// Patch will retry if the resource is updating conflicted when PatchAlignWithFn is provided.
func Patch[T MetaObject](ctx context.Context, cli PatchClient[T], expected T, pt types.PatchType, data []byte, opts ...PatchOption[T]) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	var po _PatchOptions[T]
	for i := range opts {
		opts[i](&po)
	}

	patched, err := cli.Patch(ctx, name, pt, data, po.PatchOptions)
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return Patch(rctx, cli, expected, pt, data, opts...)
		}
		if kerrors.IsConflict(err) && po.AlignFunc != nil {
			actual, err := cli.Get(ctx, name,
				readOptions(ctx))
			if err != nil {
				if rctx, ok := retryTransient(ctx, err); ok {
					return Patch(rctx, cli, expected, pt, data, opts...)
				}
				return actual, err
			}
			var skip bool
			data, skip, err = po.AlignFunc(actual.DeepCopyObject().(T))
			if err != nil {
				return actual, err
			}
			if skip {
				return actual, nil
			}
			if rctx, ok := nextAttempt(ctx, err); ok {
				return Patch(rctx, cli, expected, pt, data, opts...)
			}
		}
	}
	return patched, err
}

// PatchWithCtrlClient is similar to Patch, but uses the ctrl client.
func PatchWithCtrlClient[T MetaObject](
	ctx context.Context,
	cli ctrlcli.Client,
	expected T,
	pt types.PatchType,
	data []byte,
	opts ...PatchOption[T],
) (T, error) {
	if reflect.ValueOf(expected).IsZero() {
		return expected, errors.New("expected is nil")
	}

	name := expected.GetName()
	if name == "" {
		return expected, errors.New("resource name may not be empty")
	}

	var po _PatchOptions[T]
	for i := range opts {
		opts[i](&po)
	}

	patched := expected.DeepCopyObject().(T)
	err := cli.Patch(ctx, patched, ctrlcli.RawPatch(pt, data), &ctrlcli.PatchOptions{
		DryRun:       po.DryRun,
		FieldManager: po.FieldManager,
		Raw:          ptr.To(po.PatchOptions),
	})
	if err != nil {
		if rctx, ok := retryTransient(ctx, err); ok {
			return PatchWithCtrlClient[T](rctx, cli, expected, pt, data, opts...)
		}
		if kerrors.IsConflict(err) && po.AlignFunc != nil {
			actual := expected.DeepCopyObject().(T)
			err = cli.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual,
				&ctrlcli.GetOptions{
					Raw: ptr.To(readOptions(ctx)),
				})
			if err != nil {
				if rctx, ok := retryTransient(ctx, err); ok {
					return PatchWithCtrlClient[T](rctx, cli, expected, pt, data, opts...)
				}
				return actual, err
			}
			var skip bool
			data, skip, err = po.AlignFunc(actual.DeepCopyObject().(T))
			if err != nil {
				return actual, err
			}
			if skip {
				return actual, nil
			}
			if rctx, ok := nextAttempt(ctx, err); ok {
				return PatchWithCtrlClient[T](rctx, cli, expected, pt, data, opts...)
			}
		}
	}
	return patched, err
}

type (
	DeleteClient interface {
		Delete(ctx context.Context, name string, opts meta.DeleteOptions) error
	}

	_DeleteOptions struct {
		meta.DeleteOptions
	}

	DeleteOption func(*_DeleteOptions)
)

// WithDeleteMetaOptions sets the delete options.
func WithDeleteMetaOptions(opts meta.DeleteOptions) DeleteOption {
	return func(do *_DeleteOptions) {
		do.DeleteOptions = opts
	}
}

// Delete will delete the resource if it exists.
//
// Delete doesn't return error if the resource is not found.
func Delete(ctx context.Context, cli DeleteClient, expected MetaObject, opts ...DeleteOption) error {
	if reflect.ValueOf(expected).IsZero() {
		return errors.New("expected is nil")
	}

	name := expected.GetName()
	if name == "" {
		return errors.New("resource name may not be empty")
	}

	var do _DeleteOptions
	for i := range opts {
		opts[i](&do)
	}

	err := cli.Delete(ctx, name, do.DeleteOptions)
	if err != nil && !kerrors.IsNotFound(err) {
		if rctx, ok := retryTransient(ctx, err); ok {
			return Delete(rctx, cli, expected, opts...)
		}
		return err
	}

	return nil
}

// DeleteWithCtrlClient is similar to Delete, but uses the ctrl client.
func DeleteWithCtrlClient(ctx context.Context, cli ctrlcli.Client, expected MetaObject, opts ...DeleteOption) error {
	if reflect.ValueOf(expected).IsZero() {
		return errors.New("expected is nil")
	}

	name := expected.GetName()
	if name == "" {
		return errors.New("resource name may not be empty")
	}

	var do _DeleteOptions
	for i := range opts {
		opts[i](&do)
	}

	err := cli.Delete(ctx, expected, &ctrlcli.DeleteOptions{
		GracePeriodSeconds: do.GracePeriodSeconds,
		Preconditions:      do.Preconditions,
		PropagationPolicy:  do.PropagationPolicy,
		DryRun:             do.DryRun,
		Raw:                ptr.To(do.DeleteOptions),
	})
	if err != nil && !kerrors.IsNotFound(err) {
		if rctx, ok := retryTransient(ctx, err); ok {
			return DeleteWithCtrlClient(rctx, cli, expected, opts...)
		}
		return err
	}

	return nil
}

// isRetryError reports whether the server itself asked for another attempt. It is a pure
// predicate: waiting belongs to nextAttempt, which is also reached from the paths that retry for
// reasons the server never states.
func isRetryError(err error) bool {
	if kerrors.IsTooManyRequests(err) || kerrors.IsGone(err) || kerrors.IsTimeout(err) || kerrors.IsServerTimeout(err) {
		return true
	}
	_, ok := kerrors.SuggestsClientDelay(err)

	return ok
}

// errTooManyAttempts is returned where re-entering is the operation's only way forward, so a
// spent budget cannot be reported as the last server error — that path produced none.
var errTooManyAttempts = errors.New("too many attempts")

// retryAttemptKey carries an operation's re-entry count in its context.
type retryAttemptKey struct{}

const (
	// maxAttempts bounds how many times one operation may re-enter itself. Every operation
	// here converges by reading the live resource again and aligning on top of it, which is a
	// retry loop written as recursion. Unbounded, a peer writer that keeps winning makes it
	// recurse forever: the stack grows until the process aborts, and the caller never receives
	// an error it could report or set a condition from.
	maxAttempts = 8
	// attemptBackoff is the wait before the first re-entry, doubled for each one after. A
	// Conflict carries no server-suggested delay, so without this the next attempt is
	// immediate and a contended object becomes a hot loop against the API server.
	attemptBackoff = 10 * time.Millisecond
)

// nextAttempt returns the context to re-enter an operation with, after waiting out this attempt's
// backoff, and reports whether the budget allowed one at all. A server-suggested delay wins over
// the backoff, since the server named a figure and this only guesses one.
//
// The count rides the context because that is the only channel every operation here already
// threads through its own recursion; the options are typed per operation, so no shared option
// could carry it without changing all of their signatures.
func nextAttempt(ctx context.Context, err error) (context.Context, bool) {
	attempt, _ := ctx.Value(retryAttemptKey{}).(int)
	if attempt >= maxAttempts {
		return ctx, false
	}

	wait := attemptBackoff << attempt
	if s, ok := kerrors.SuggestsClientDelay(err); ok {
		wait = time.Duration(s) * time.Second
	}

	select {
	case <-ctx.Done():
		return ctx, false
	case <-time.After(wait):
	}

	return context.WithValue(ctx, retryAttemptKey{}, attempt+1), true
}

// retryTransient answers the sites that retry only because the server asked them to.
func retryTransient(ctx context.Context, err error) (context.Context, bool) {
	if !isRetryError(err) {
		return ctx, false
	}

	return nextAttempt(ctx, err)
}

// retryToConverge answers the sites that also retry a lost write: the next pass reads the actual
// resource again and aligns on top of it, so a writer that lost a concurrent update converges
// instead of failing.
//
// Any one of these reasons is enough, hence the disjunction: they are mutually exclusive, so
// requiring them together could never hold.
func retryToConverge(ctx context.Context, err error) (context.Context, bool) {
	if !isRetryError(err) && !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
		return ctx, false
	}

	return nextAttempt(ctx, err)
}

// readOptions are the options for the read an operation does before it writes. The first read may
// be served from the API server's watch cache, which is only an optimization; a re-entry may not,
// because aligning onto the same stale object that just lost a conflict would lose again, and the
// cache is exactly what would serve it.
func readOptions(ctx context.Context) meta.GetOptions {
	if attempt, _ := ctx.Value(retryAttemptKey{}).(int); attempt > 0 {
		return meta.GetOptions{}
	}

	return meta.GetOptions{ResourceVersion: "0"}
}
