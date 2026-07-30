package kuberess

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeapp"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
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
	install := func(ctx context.Context, exclusive bool) error {
		return kubeapp.ExecuteInstall(
			ctx,
			system.LoopbackKubeRestConfig.Get(),
			disable,
			SystemNamespaceName,
			installs,
			gvc,
			exclusive,
		)
	}

	// There is nothing to serialize where a chart deploys the worker: the install returns
	// on the wildcard without touching anything, and taking the lock would leave a Lease
	// behind for it.
	if disable.Has(applicationWildcard) {
		return install(ctx, false)
	}

	lock := kubeapp.Lock{
		Leases: system.LoopbackKubeClient.Get().CoordinationV1().Leases(SystemNamespaceName),
		Name:   applicationInstallLockName,
		Holder: applicationInstallLockHolder(),
	}

	return lock.Do(ctx, func(ctx context.Context, predecessor string) error {
		return install(ctx, predecessorHasStopped(ctx, system.LoopbackKubeClient.Get(), predecessor))
	})
}

// podHolderPrefix marks a lock identity as the name of a Pod in the system namespace, which
// is what lets a peer look the holder up. The other form names a process outside the
// cluster, which no peer can check.
const podHolderPrefix = "pod/"

// applicationInstallLockHolder identifies this replica to its peers.
//
// In a Pod that is the pod name, which is unique per replica and, being an object a peer
// can read, is also what makes a vanished holder recognizable. Outside one it is the host
// plus this process's id: the host alone is shared by every worker running on it, and peers
// that all read the claim as their own would each take the lock.
func applicationInstallLockHolder() string {
	if name := osx.Getenv("KUBERNETES_POD_NAME"); name != "" {
		return podHolderPrefix + name
	}
	host, _ := os.Hostname()

	return fmt.Sprintf("process/%s-%d", host, os.Getpid())
}

// predecessorHasStopped reports whether the process that last held the install lock is
// known to be gone, which is what makes a release it left mid-operation safe to repair.
//
// An empty predecessor is a lock that was found free rather than taken over, and a lock is
// released only where the call it guarded ended of its own accord — so its holder finished.
// A named predecessor went quiet instead, and quiet is not stopped: it may be a replica
// whose connection to the API stalled while Helm carried on applying. The Pod it named
// answers that: a Pod that is gone, or whose containers are no longer running, has no
// process left behind it. A holder this replica cannot look up is treated as still running,
// because guessing wrong here means two Helm actions on one release.
func predecessorHasStopped(ctx context.Context, cli kubernetes.Interface, predecessor string) bool {
	if predecessor == "" {
		return true
	}

	name, ok := strings.CutPrefix(predecessor, podHolderPrefix)
	if !ok {
		return false
	}

	// Read through to the API server rather than its watch cache: a cached Pod can be
	// several seconds behind, and "not running" is the answer that authorizes a repair.
	pod, err := cli.CoreV1().Pods(SystemNamespaceName).Get(ctx, name, meta.GetOptions{})
	if kerrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		return false
	}

	// A Pod whose containers have not reported yet answers neither way, and an empty slice
	// read as "stopped" would authorize a repair on no evidence at all.
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}

	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].State.Running != nil {
			return false
		}
	}

	return true
}
