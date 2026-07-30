package kuberess

import (
	"context"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeapp"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// applicationWildcard disables every application at once. It is consumed by
// kubeapp.ExecuteInstall, which skips the install entirely when it is present.
const applicationWildcard = "*"

// applicationValuesKeys maps each name --disable-applications accepts to the chart
// values key that switches that component off. It is the authoritative name set: the
// worker validates the flag against it, and the overlay renders one switch per entry.
//
// The gpustack-cpu-info NodeFeatureRule has no entry because it has no switch: the
// scheduling chain starts at that rule, so a release without it classifies no node.
var applicationValuesKeys = map[string]string{
	"kueue":                  "kueue",
	"node-feature-discovery": "node-feature-discovery",
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

// applicationInstallLockName is the Lease every worker replica serializes its application
// install on.
//
// The install runs in Prepare, before leader election gates anything, so every replica
// reaches it — and a rolling update overlaps two of them even at one replica. Helm's own
// release storage is a single compare-and-create, not a mutex: the loser lands on whichever
// of several errors matches where it got to, and two Helm actions applying the same objects
// at once can leave the release wedged in a state no later attempt can get past.
const applicationInstallLockName = "applications.worker.gpustack.ai"

// InstallApplications installs applications, one replica at a time.
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

	disable := system.DisableApplications.Get()
	install := func(ctx context.Context) error {
		return kubeapp.ExecuteInstall(
			ctx,
			system.LoopbackKubeRestConfig.Get(),
			disable,
			SystemNamespaceName,
			installs,
			gvc,
		)
	}

	// There is nothing to serialize where a chart deploys the worker: the install returns
	// on the wildcard without touching anything, and taking the lock would leave a Lease
	// behind for it.
	if disable.Has(applicationWildcard) {
		return install(ctx)
	}

	lock := kubeapp.Lock{
		Leases: system.LoopbackKubeClient.Get().CoordinationV1().Leases(SystemNamespaceName),
		Name:   applicationInstallLockName,
		Holder: applicationInstallLockHolder(),
	}

	return lock.Do(ctx, install)
}

// applicationInstallLockHolder identifies this replica to its peers, preferring the pod
// name the chart injects. A Pod's hostname is that same name, so it stands in wherever the
// variable was dropped.
func applicationInstallLockHolder() string {
	if name := osx.Getenv("KUBERNETES_POD_NAME"); name != "" {
		return name
	}
	name, _ := os.Hostname()

	return name
}
