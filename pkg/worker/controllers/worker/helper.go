package worker

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	"gpustack.ai/gpustack/pkg/worker/settings"
)

// redirectedImage applies the cluster's registry and namespace redirection to an
// image this operator chose, so an air-gapped installation reaches its own mirror instead of the
// public one. The settings that drive it take an image reference of the form `namespace/name:tag`,
// which is why every default they redirect is written that way rather than with a registry host.
//
// The namespace replacement is skipped when the first segment is a REGISTRY HOST -- one carrying a
// dot or a port, or `localhost` -- because the settings are user-controlled and a value already
// carrying a registry would otherwise have its host replaced by the namespace, silently resolving
// to a different image than the one configured.
//
// An image named on the object itself is the user's own reference and is NEVER passed through here:
// redirecting it would silently resolve their reference to a different one.
func redirectedImage(ctx context.Context, image string) string {
	if cn := settings.ContainerNamespace.ShouldValue(ctx); cn != "" {
		if first, suffix, found := strings.Cut(image, "/"); found &&
			!strings.ContainsAny(first, ".:") && first != "localhost" {
			image = cn + "/" + suffix
		}
	}
	if rn := settings.ContainerRegistry.ShouldValue(ctx); rn != "" {
		image = rn + "/" + image
	}
	return image
}

// _requeueAfterConflict is the onConflict result of objectWriteResult for a reconciler whose own
// predicate filters out some newer versions of the object it writes, typically a status-only change
// under a generation predicate. No event may follow such a conflict, so the reconciler asks for the
// retry itself. The delay only has to outlast the informer catching up with the newer version.
var _requeueAfterConflict = ctrl.Result{RequeueAfter: time.Second}

// objectWriteResult turns a failed write of the reconciled object into the reconcile result, whether
// the write is to its status or to its spec and metadata, as long as it carries the resource version
// the object was read at. It serves a write to another object only when the reconciler also watches
// that object, deletions included: without that watch, nothing would reconcile again after a not
// found. Two failures are expected and heal on their own, so they are logged at V(1) and not
// returned: not found means the object is gone and nothing is left to write; a conflict means the
// object changed after it was read, and it is reconciled again from its newer version as onConflict
// says. A reconciler whose predicate passes every newer version that still needs the write passes
// an empty result, because the change that caused the conflict is itself an event; one whose
// predicate may filter that change passes _requeueAfterConflict. Returning either error would only
// add an error log and a backoff retry of a write that is already moot.
//
// The conflict is kept rather than avoided. A write judged from the object as read must not land on
// a newer one, and the resource version it carries is what stops it: Kueue resets the checks of an
// evicted Workload to Pending, and a verdict judged before that reset must not overwrite it.
func objectWriteResult(logger logr.Logger, err error, msg string, onConflict ctrl.Result) (ctrl.Result, error) {
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		logger.V(1).Info(msg+" skipped, the object changed or was deleted since it was read", "reason", err.Error())
		if apierrors.IsConflict(err) {
			return onConflict, nil
		}
		return ctrl.Result{}, nil
	}
	logger.Error(err, msg)
	return ctrl.Result{}, err
}
