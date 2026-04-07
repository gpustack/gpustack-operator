package kubemeta

import meta "k8s.io/apimachinery/pkg/apis/meta/v1"

// SanitizeObjectMeta creates a sanitized copy of the given ObjectMeta.
func SanitizeObjectMeta(src meta.ObjectMeta) meta.ObjectMeta {
	dst := src.DeepCopy()

	// Sanitize the copied ObjectMeta by clearing fields that are not expected to be set by users.
	dst.UID = ""
	dst.ResourceVersion = ""
	dst.Generation = 0
	dst.CreationTimestamp = meta.Time{}
	dst.DeletionTimestamp = nil
	dst.DeletionGracePeriodSeconds = nil
	dst.OwnerReferences = nil
	dst.Finalizers = nil
	dst.ManagedFields = nil
	return *dst
}
