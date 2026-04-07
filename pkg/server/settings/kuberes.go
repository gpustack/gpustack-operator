package settings

import (
	"context"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
)

// Initialize initializes Kubernetes resources for settings.
func Initialize(ctx context.Context, cli kubernetes.Interface) error {
	return settings.Initialize(ctx, cli)
}
