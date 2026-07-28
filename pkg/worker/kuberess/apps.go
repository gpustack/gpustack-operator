package kuberess

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeapp"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// applicationWildcard disables every application at once. It is consumed by
// kubeapp.ExecuteInstall, which skips the install entirely when it is present.
const applicationWildcard = "*"

// applicationValuesKeys maps each name --disable-applications accepts to the chart
// values key that switches that component off. It is the authoritative name set: the
// worker validates the flag against it, and the overlay renders one switch per entry.
var applicationValuesKeys = map[string]string{
	"kueue":                  "kueue",
	"node-feature-discovery": "node-feature-discovery",
	"node-feature-rule":      "nodeFeatureRule",
	"csi-driver-nfs":         "csi-driver-nfs",
	"csi-driver-s3":          "csi-driver-s3",
	"device-manager":         "deviceManager",
}

// ApplicationNames returns every name --disable-applications accepts, sorted: the
// wildcard plus one name per installable component.
func ApplicationNames() []string {
	names := make([]string, 0, len(applicationValuesKeys)+1)
	names = append(names, applicationWildcard)
	for name := range applicationValuesKeys {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// componentSwitches maps each component's chart values key to whether it stays enabled
// under the given disable set.
func componentSwitches(disable sets.Set[string]) map[string]bool {
	switches := make(map[string]bool, len(applicationValuesKeys))
	for name, key := range applicationValuesKeys {
		switches[key] = !disable.Has(name)
	}

	return switches
}

// installs is the list of application installers.
var installs = []kubeapp.Install{
	installGPUStackOperator,
}

// InstallApplications installs applications.
func InstallApplications(ctx context.Context, manufacturers []string) error {
	gvc := map[string]any{
		"ContainerRegistry":  settings.ContainerRegistry.ShouldValueFromRemote(ctx),
		"ContainerNamespace": settings.ContainerNamespace.ShouldValueFromRemote(ctx),
		"ImagePullSecrets": func() []string {
			v := settings.ImagePullSecrets.ShouldValueFromRemote(ctx)
			if v == "" {
				return nil
			}
			return strings.Split(v, ",")
		}(),
		"ImagePullPolicy": settings.ImagePullPolicy.ShouldValueFromRemote(ctx),
		"Manufacturers":   manufacturers,
	}

	return kubeapp.ExecuteInstall(
		ctx,
		system.LoopbackKubeRestConfig.Get(),
		system.DisableApplications.Get(),
		SystemNamespaceName,
		installs,
		gvc,
	)
}
