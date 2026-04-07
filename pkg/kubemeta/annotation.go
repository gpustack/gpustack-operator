package kubemeta

import (
	kvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// GetAnnotation returns the value of the annotation with the given key.
func GetAnnotation(obj MetaObject, key string) (string, bool) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		return "", false
	}

	v, ok := as[key]
	return v, ok
}

// SetAnnotation sets the value of the annotation with the given key.
//
// If the annotation exists, its value will be updated.
func SetAnnotation(obj MetaObject, key, value string) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		as = map[string]string{}
	}

	as[key] = value
	obj.SetAnnotations(as)
}

// AddAnnotation adds the annotation with the given key and value.
//
// If the annotation already exists, it will not be added.
func AddAnnotation(obj MetaObject, key, value string) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		as = map[string]string{}
	}

	if _, ok := as[key]; ok {
		return
	}

	as[key] = value
	obj.SetAnnotations(as)
}

// IsAnnotated returns true if the annotation with the given key and value exists.
func IsAnnotated(obj MetaObject, key, value string) bool {
	v, ok := GetAnnotation(obj, key)
	return ok && v == value
}

// HasAnnotation returns true if the annotation with the given key exists.
func HasAnnotation(obj MetaObject, key string) bool {
	_, ok := GetAnnotation(obj, key)
	return ok
}

// DeleteAnnotation deletes the annotation with the given key.
func DeleteAnnotation(obj MetaObject, key string) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		return
	} else if _, ok := as[key]; !ok {
		return
	}

	delete(as, key)
	obj.SetAnnotations(as)
}

const _LastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// SanitizeLastAppliedAnnotation erases the last-applied-configuration annotation with a blank string if found.
//
// It usually to prevent the sensitive information from being stored in
// the last-applied-configuration annotation.
func SanitizeLastAppliedAnnotation(obj MetaObject) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		return
	} else if _, ok := as[_LastAppliedConfigAnnotation]; !ok {
		return
	}

	// Erase.
	as[_LastAppliedConfigAnnotation] = "{}"
	obj.SetAnnotations(as)
}

// OverwriteLastAppliedAnnotation overwrites the last-applied-configuration annotation if it exists.
//
// It usually to satisfy the requirement of the `kubectl apply ...`.
func OverwriteLastAppliedAnnotation(obj MetaObject) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		return
	} else if _, ok := as[_LastAppliedConfigAnnotation]; !ok {
		return
	}

	// Temporarily remove managed fields.
	mf := obj.GetManagedFields()
	defer func() {
		obj.SetManagedFields(mf)
	}()
	obj.SetManagedFields(nil)

	// Erase old last-applied annotation.
	delete(as, _LastAppliedConfigAnnotation)
	if len(as) == 0 {
		obj.SetAnnotations(nil)
	} else {
		obj.SetAnnotations(as)
	}

	// Set new last-applied annotation.
	lastApplied, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		// Failed to encode, ignore.
		return
	}
	as[_LastAppliedConfigAnnotation] = string(lastApplied)
	if err = kvalidation.ValidateAnnotationsSize(as); err != nil {
		// Too large, ignore.
		return
	}
	obj.SetAnnotations(as)
}

// ConfigureLastAppliedAnnotation is similar to OverwriteLastAppliedAnnotation,
// but it always sets the last-applied-configuration annotation.
//
// It usually to satisfy the requirement of the `kubectl apply ...`.
func ConfigureLastAppliedAnnotation(obj MetaObject) {
	if obj == nil {
		panic("object is nil")
	}

	as := obj.GetAnnotations()
	if as == nil {
		as = map[string]string{}
	}

	// Imagine the last-applied-configuration annotation is existed.
	as[_LastAppliedConfigAnnotation] = "{}"
	obj.SetAnnotations(as)
	// Then overwrite it.
	OverwriteLastAppliedAnnotation(obj)
}
