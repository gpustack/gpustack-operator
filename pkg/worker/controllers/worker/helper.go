package worker

import (
	"context"
	"strings"

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
