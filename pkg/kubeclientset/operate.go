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
				meta.GetOptions{
					ResourceVersion: "0",
				})
			if err != nil && !kerrors.IsNotFound(err) {
				if isRetryError(err) {
					return Create(ctx, cli, expected, opts...)
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
				return Create(ctx, cli, expected, opts...)
			case kerrors.IsAlreadyExists(err):
				// Retry on already existed if:
				// - configure align function.
				// - configure compare function.
				// - the resource is deleting without finalizers.
				if co.UpdateAlignFunc != nil || co.RecreateCompareFunc != nil || deleting {
					return Create(ctx, cli, expected, opts...)
				}
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
		if err == nil || !kerrors.IsConflict(err) || !kerrors.IsNotAcceptable(err) || !isRetryError(err) {
			return updated, err
		}

		// Retry if conflicted.
		return Create(ctx, cli, expected, opts...)
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

		// Recreate.
		return Create(ctx, cli, expected, opts...)
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
			if isRetryError(err) {
				return CreateWithCtrlClient[T](ctx, cli, expected, opts...)
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
			return CreateWithCtrlClient[T](ctx, cli, expected, opts...)
		case kerrors.IsAlreadyExists(err):
			// Retry on already existed if:
			// - configure align function.
			// - configure compare function.
			// - the resource is deleting without finalizers.
			if co.UpdateAlignFunc != nil || co.RecreateCompareFunc != nil || deleting {
				return CreateWithCtrlClient[T](ctx, cli, expected, opts...)
			}
			err = nil
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
		if err == nil || !kerrors.IsConflict(err) || !kerrors.IsNotAcceptable(err) || !isRetryError(err) {
			return copied, err
		}

		// Retry if conflicted.
		return CreateWithCtrlClient[T](ctx, cli, expected, opts...)
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

		// Recreate.
		return CreateWithCtrlClient[T](ctx, cli, expected, opts...)
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
		meta.GetOptions{
			ResourceVersion: "0",
		})
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
				return Update(ctx, cli, expected, opts...)
			}
		}
		if isRetryError(err) {
			return Update(ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return Update(ctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			return Update(ctx, cli, expected, opts...)
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
				return UpdateWithCtrlClient[T](ctx, cli, expected, opts...)
			}
		}
		if isRetryError(err) {
			return UpdateWithCtrlClient[T](ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return UpdateWithCtrlClient[T](ctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			return UpdateWithCtrlClient[T](ctx, cli, expected, opts...)
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
		meta.GetOptions{
			ResourceVersion: "0",
		})
	if err != nil {
		if isRetryError(err) {
			return UpdateStatus(ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return UpdateStatus(ctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			return UpdateStatus(ctx, cli, expected, opts...)
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
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	if err != nil {
		if isRetryError(err) {
			return UpdateStatusWithCtrlClient[T](ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return UpdateStatusWithCtrlClient[T](ctx, cli, expected, opts...)
		}

		if !kerrors.IsConflict(err) && !kerrors.IsNotAcceptable(err) {
			return actual, err
		}

		// Retry if conflicted when align function is provided.
		if uo.AlignFunc != nil {
			return UpdateStatusWithCtrlClient[T](ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return Patch(ctx, cli, expected, pt, data, opts...)
		}
		if kerrors.IsConflict(err) && po.AlignFunc != nil {
			actual, err := cli.Get(ctx, name,
				meta.GetOptions{
					ResourceVersion: "0",
				})
			if err != nil {
				if isRetryError(err) {
					return Patch(ctx, cli, expected, pt, data, opts...)
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
			return Patch(ctx, cli, expected, pt, data, opts...)
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
		if isRetryError(err) {
			return PatchWithCtrlClient[T](ctx, cli, expected, pt, data, opts...)
		}
		if kerrors.IsConflict(err) && po.AlignFunc != nil {
			actual := expected.DeepCopyObject().(T)
			err = cli.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual,
				&ctrlcli.GetOptions{
					Raw: &meta.GetOptions{
						ResourceVersion: "0",
					},
				})
			if err != nil {
				if isRetryError(err) {
					return PatchWithCtrlClient[T](ctx, cli, expected, pt, data, opts...)
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
			return PatchWithCtrlClient[T](ctx, cli, expected, pt, data, opts...)
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
		if isRetryError(err) {
			return Delete(ctx, cli, expected, opts...)
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
		if isRetryError(err) {
			return DeleteWithCtrlClient(ctx, cli, expected, opts...)
		}
		return err
	}

	return nil
}

func isRetryError(err error) bool {
	if kerrors.IsTooManyRequests(err) || kerrors.IsGone(err) || kerrors.IsTimeout(err) || kerrors.IsServerTimeout(err) {
		time.Sleep(10 * time.Millisecond)
		return true
	}
	if s, ok := kerrors.SuggestsClientDelay(err); ok {
		time.Sleep(time.Duration(s) * time.Second)
		return true
	}
	return false
}
