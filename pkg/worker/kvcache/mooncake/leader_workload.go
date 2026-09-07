package mooncake

import (
	"fmt"
	"math"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
)

const (
	// leaderResourceNoteRole distinguishes the leader's objects from a member group's, which are
	// noted the same way and land in the same namespace.
	leaderResourceNoteRole = "leader"

	// The identity labels the leader's objects and Pods carry. They are constants because a watch
	// on the leader's Pods has to read back exactly what the renderer wrote, and a Pod is the one
	// object here that carries no resource note: it is made by the Deployment's own controller from
	// a template, and the note sits on the Deployment rather than inside that template.
	labelKeyName      = "app.kubernetes.io/name"
	labelKeyInstance  = "app.kubernetes.io/instance"
	labelKeyComponent = "app.kubernetes.io/component"
	labelValueName    = "kv-cache-backend"
	labelValueLeader  = "leader"

	// leaderRPCPortName and leaderAdminPortName name the two published ports. They are names
	// rather than numbers on the Service so a consumer can ask for a role, and because the two
	// serve entirely different audiences: one is what an inference engine dials, the other is what
	// this operator scrapes.
	leaderRPCPortName   = "rpc"
	leaderAdminPortName = "admin"

	// leaderReadinessPath is a GATED route: the artifact wraps it in a check on the service plane
	// and answers 503 until that plane is up. That is exactly the readiness question — may traffic
	// go here — so the probe asks it directly instead of inferring it.
	leaderReadinessPath = "/get_all_segments"
	// leaderLivenessPath is an UNGATED route: it answers 200 whenever the process answers at all,
	// whatever state the service plane is in. That is the liveness question, and it is the reason
	// liveness may NOT reuse the readiness path — a leader that is merely slow to activate would be
	// killed for being slow.
	leaderLivenessPath = "/health"

	// The two volumes the quota policy needs, and the container that bridges them. They are separate
	// volumes because they answer opposite requirements: one has to be writable and cannot be a
	// ConfigMap, the other has to carry the operator's desired state and can only be a ConfigMap.
	quotaPolicyVolumeName     = "tenant-quota-policy"
	quotaPolicySeedVolumeName = "tenant-quota-policy-seed"
	quotaPolicyInitName       = "seed-tenant-quota-policy"

	// The environment variables the seed command reads. The command itself is a fixed string and
	// every value it touches arrives this way — a rendered YAML document interpolated into a shell
	// command would be a policy passing through quoting rules.
	quotaPolicySeedEnv  = "POLICY_SEED"
	quotaPolicyFileEnv  = "POLICY_FILE"
	quotaPolicyEmptyEnv = "POLICY_EMPTY"

	// quotaPolicySeedCommand copies the operator's desired policy onto the writable volume, and
	// writes an empty one when there is nothing to copy. The variables it reads are the three
	// constants above; a test pins both sides to the same literal names.
	//
	// The fallback is not defensive: the ConfigMap is rendered by the POOL reconciler, so a backend
	// with multi-tenancy on and no pool bound to it yet has nobody to write one. Its volume is
	// optional and mounts as an empty directory, the copy finds no source, and without a document
	// here the master would refuse to start — an empty file does not satisfy its parser either.
	//
	// Which is why the fallback is gated on the source EXISTING, and not on the copy failing.
	// `cp || fallback` cannot tell the intended case apart from a copy that failed for any other
	// reason — a permission, a truncated mount, a transient IO error — and `2>/dev/null` removed
	// the last trace of it. Laundered into the no-pool case, a real failure seeds an EMPTY ledger,
	// the init container exits 0, the master starts with no tenant quotas, and the pool goes on
	// reporting Ready over grants that are gone. Gated on the test, that same failure fails the
	// init container with cp's own message, which is the only place it can still be seen.
	//
	// This runs on every Pod start, so it OVERWRITES whatever the master last wrote through the
	// admin API. That is the intended direction: the ConfigMap is the desired state and the file is
	// the master's copy of it, so a restart resets the copy to what the operator wants and the next
	// reconcile pass re-converges the ledger to match.
	quotaPolicySeedCommand = `if [ -f "$POLICY_SEED" ]; then ` +
		`cp "$POLICY_SEED" "$POLICY_FILE"; else ` +
		`printf '%s' "$POLICY_EMPTY" > "$POLICY_FILE"; fi`
)

// LeaderObjectName is the name of every object rendered for a backend's leader.
//
// The backend is cluster-scoped and its objects are not, so the name has to survive the move into
// one shared namespace: it is the backend's own name with a role suffix, which keeps two backends
// apart and keeps a backend's own objects together.
func LeaderObjectName(kvcb *workercore.KVCacheBackend) string {
	return kvcb.Name + "-leader"
}

// LeaderServiceHost is the in-cluster DNS name the leader answers on.
func LeaderServiceHost(kvcb *workercore.KVCacheBackend) string {
	return fmt.Sprintf("%s.%s.svc", LeaderObjectName(kvcb), kuberess.SystemNamespaceName)
}

// LeaderEndpoints is what status.endpoints publishes for a managed backend: the same host on two
// ports, each named for who reads it.
func LeaderEndpoints(kvcb *workercore.KVCacheBackend) []workercore.KVCacheBackendEndpoint {
	host := LeaderServiceHost(kvcb)
	return []workercore.KVCacheBackendEndpoint{
		{
			Name:    workercore.KVCacheBackendEndpointNameClient,
			Address: fmt.Sprintf("%s:%d", host, LeaderRPCPort),
		},
		{
			Name:    workercore.KVCacheBackendEndpointNameAdmin,
			Address: fmt.Sprintf("%s:%d", host, LeaderMetricsPort),
		},
	}
}

// LeaderReplicas is how many leader processes the object asks for, defaulting to one.
func LeaderReplicas(leader workercore.KVCacheBackendLeader) int32 {
	if leader.Replicas != nil {
		return *leader.Replicas
	}

	return 1
}

// leaderUpdateStrategy picks how the leader's Deployment is updated, and the two answers are
// opposites rather than variations.
//
// SINGLE REPLICA: Recreate. A RollingUpdate's maxSurge defaults to 25%, which ROUNDS UP -- to one,
// against one desired replica -- so the new master starts before the old one stops and the two run
// at once. Without an election that is a split brain, on every image or flag change rather than
// never. The cost is a gap with no master, which is the right trade: a member that loses its master
// keeps its segment and re-registers, while two masters allocating against one pool cannot be
// reconciled after the fact.
//
// SEVERAL REPLICAS: RollingUpdate, and both parameters invert.
//   - maxSurge may exceed zero, because the lease admits one leader however many processes run.
//     The surge that is a split brain above is safe here.
//   - maxUnavailable is replicas-1, because exactly one replica is ever ready: the leader serves and
//     the standbys deliberately do not. Anything smaller demands more available replicas than this
//     workload ever has, and the rollout does not move at all.
//
// LIMITED: this reads the replica count rather than an HA field, which holds only because admission
// refuses more than one replica without the leadership backend configured. The two are equivalent
// there and nowhere else.
func leaderUpdateStrategy(replicas int32) apps.DeploymentStrategy {
	if replicas <= 1 {
		return apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
	}

	return apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
			MaxUnavailable: ptr.To(intstr.FromInt32(replicas - 1)),
		},
	}
}

// leaderProgressDeadlineSeconds disables the rollout timeout once there are standbys, and leaves it
// to the API server's default otherwise.
//
// The timeout cannot discriminate on this workload. DeploymentComplete requires availableReplicas to
// equal spec.replicas, which is permanently false when only the leader is ready, so the Progressing
// condition stops advancing as soon as the rollout finishes -- exactly as it would if the image
// could not be pulled. Both outcomes present as a stale timestamp, and a check whose two answers are
// identical is not a check.
//
// REQUIRED: whoever removes this has to bring the replacement with it. The predicate that does
// discriminate is DeploymentComplete without its availableReplicas clause: updatedReplicas and
// replicas both reach spec.replicas normally here.
func leaderProgressDeadlineSeconds(replicas int32) *int32 {
	if replicas <= 1 {
		return nil
	}

	return ptr.To(int32(math.MaxInt32))
}

// leaderSelectorLabels is what the Deployment selects its Pods by and what the Service fronts.
//
// A Deployment's spec.selector is IMMUTABLE. An update carrying a different one is rejected, and
// the object then needs deleting by hand. So this carries only identity — which backend, which role
// — and never anything a spec update can move: not the image, not the transport, not a flag.
func leaderSelectorLabels(kvcb *workercore.KVCacheBackend) map[string]string {
	return map[string]string{
		labelKeyName:      labelValueName,
		labelKeyInstance:  kvcb.Name,
		labelKeyComponent: labelValueLeader,
	}
}

// LeaderPodBackendName reports which backend a Pod belongs to, and "" for any Pod that is not a
// leader's.
//
// It reads the identity labels rather than a resource note because the leader's Pods carry no note
// — see the label constants. Those labels are the Deployment's own selector, which is immutable, so
// a watch matching on them cannot drift away from what the renderer writes.
func LeaderPodBackendName(labels map[string]string) string {
	if labels[labelKeyName] != labelValueName || labels[labelKeyComponent] != labelValueLeader {
		return ""
	}
	return labels[labelKeyInstance]
}

// leaderPodLabels is what the Pods carry: the selector, plus the part-of label every object this
// operator renders carries. The extra label is outside the selector on purpose — a selector is
// forever, and this one is presentation.
func leaderPodLabels(kvcb *workercore.KVCacheBackend) map[string]string {
	labels := leaderSelectorLabels(kvcb)
	labels["app.kubernetes.io/part-of"] = "gpustack-operator-worker"
	return labels
}

// RenderLeaderDeployment renders the leader's Deployment.
//
// The image is a parameter rather than something read here: whether it came from spec.image or from
// the cluster-wide Setting is the reconciler's decision, and resolving it inside the renderer would
// put a Setting read behind a pure function and make the fallback untestable from the outside.
//
// It is deterministic — the reconciler converges this object on every pass, so a render whose output
// wandered would rewrite the Deployment forever and restart the leader with it.
func RenderLeaderDeployment(kvcb *workercore.KVCacheBackend, image string) *apps.Deployment {
	leader := kvcb.Spec.Connection.Managed.Leader

	deploy := &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      LeaderObjectName(kvcb),
			Namespace: kuberess.SystemNamespaceName,
			Labels:    leaderPodLabels(kvcb),
		},
		Spec: apps.DeploymentSpec{
			Replicas:                ptr.To(LeaderReplicas(leader)),
			Strategy:                leaderUpdateStrategy(LeaderReplicas(leader)),
			ProgressDeadlineSeconds: leaderProgressDeadlineSeconds(LeaderReplicas(leader)),
			Selector:                &meta.LabelSelector{MatchLabels: leaderSelectorLabels(kvcb)},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: leaderPodLabels(kvcb)},
				Spec: core.PodSpec{
					// Nothing in this workload talks to the API server — it is a third-party store
					// binary — and the default is to mount a service-account token it never asked
					// for into it. Rendered rather than left to the server, so the aligner converges
					// it: a value the renderer omits is one the API server fills in as true.
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             kvcb.Spec.ImagePullSecrets,
					Containers: []core.Container{
						leaderContainerSpec(leader, image, kvcache.EffectivePullPolicy(kvcb, image)),
					},
				},
			},
		},
	}

	if leader.MultiTenancy {
		podSpec := &deploy.Spec.Template.Spec
		podSpec.Volumes = quotaPolicyVolumes(kvcb)
		podSpec.InitContainers = []core.Container{
			quotaPolicySeedContainer(image, kvcache.EffectivePullPolicy(kvcb, image)),
		}
	}

	systemmeta.NoteResource(deploy, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
		"role":                      leaderResourceNoteRole,
	})
	kubemeta.ControlOnWithoutBlock(deploy, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return deploy
}

// leaderContainerSpec is the container the Deployment runs.
//
// What it does NOT ask for is load-bearing: no hostNetwork, no device, no privilege, and no volume
// beyond the one multi-tenancy cannot start without. The leader is a metadata service that holds no
// cache bytes — the members are the side that needs the host — so anything here would be a privilege
// nobody asked for.
func leaderContainerSpec(
	leader workercore.KVCacheBackendLeader, image string, pullPolicy core.PullPolicy,
) core.Container {
	var volumeMounts []core.VolumeMount
	if leader.MultiTenancy {
		// Not read-only, and that is the point: the master writes a temp file into this directory
		// and renames it over the policy on every admin-API change.
		//
		// The seed volume is deliberately absent here. The leader has no reason to read the
		// ConfigMap, and a mount it could see would invite pointing the connector URI straight at
		// it — which fails on the first quota write rather than at startup.
		volumeMounts = []core.VolumeMount{
			{Name: quotaPolicyVolumeName, MountPath: QuotaPolicyDir},
		}
	}

	return core.Container{
		Name:            "leader",
		Image:           image,
		ImagePullPolicy: pullPolicy,
		// This artifact never writes /dev/termination-log, so the default policy would report every
		// death as a bare "Error".
		TerminationMessagePolicy: core.TerminationMessageFallbackToLogsOnError,
		// The entrypoint is named rather than inherited from the image, so what runs is visible in
		// `kubectl get deploy -o yaml` without entering the container — the same reason the flags
		// are argv and not environment variables.
		Command:      []string{"mooncake_master"},
		Args:         RenderLeaderFlags(leader),
		VolumeMounts: volumeMounts,
		Ports: []core.ContainerPort{
			{Name: leaderRPCPortName, ContainerPort: LeaderRPCPort, Protocol: core.ProtocolTCP},
			{Name: leaderAdminPortName, ContainerPort: LeaderMetricsPort, Protocol: core.ProtocolTCP},
		},
		// The rendered argv refers to both of these. Without them the flag reaches the process as
		// the literal "$(KUBERNETES_POD_NAME)" and the leader takes that for its own name.
		Env: []core.EnvVar{
			{
				Name: LeaderPodNameEnv,
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{
				Name: LeaderPodNamespaceEnv,
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			},
		},
		ReadinessProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				HTTPGet: &core.HTTPGetAction{
					Path: leaderReadinessPath,
					Port: intstr.FromInt32(LeaderMetricsPort),
				},
			},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		LivenessProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				HTTPGet: &core.HTTPGetAction{
					Path: leaderLivenessPath,
					Port: intstr.FromInt32(LeaderMetricsPort),
				},
			},
			PeriodSeconds: 10,
			// Higher than readiness on purpose. Withholding traffic from a leader that is not
			// serving costs nothing; killing one that is only slow to activate costs a restart and
			// every segment mounted against it.
			FailureThreshold: 6,
		},
	}
}

// quotaPolicyVolumes is the pair the tenant quota policy needs: the writable one the master runs
// from, and the read-only one it is seeded from.
//
// The ConfigMap is optional because the two objects have different authors — the POOL reconciler
// renders it, and a backend with multi-tenancy on that no pool has bound to yet has nobody to write
// one. A required volume would leave that backend's Pod waiting on a ConfigMap nothing is going to
// create; an optional one mounts as an empty directory and the seed command falls back.
func quotaPolicyVolumes(kvcb *workercore.KVCacheBackend) []core.Volume {
	return []core.Volume{
		{
			Name:         quotaPolicyVolumeName,
			VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
		},
		{
			Name: quotaPolicySeedVolumeName,
			VolumeSource: core.VolumeSource{
				ConfigMap: &core.ConfigMapVolumeSource{
					LocalObjectReference: core.LocalObjectReference{
						Name: QuotaPolicyObjectName(kvcb),
					},
					Optional: ptr.To(true),
					// Spelled out although it is exactly what the API server would default it to,
					// because the aligner compares this list as a WHOLE. A field left for the server
					// to fill comes back differing from what was rendered, and the comparison would
					// then move it on every pass — a write per resync on a Deployment nothing asked
					// to change.
					DefaultMode: ptr.To[int32](0o644),
				},
			},
		},
	}
}

// quotaPolicySeedContainer puts a policy document on the writable volume before the master looks for
// one.
//
// It runs the leader's own image rather than a copy utility, so an air-gapped install pulls nothing
// extra and the pull policy question has one answer instead of two. What that image can run is a
// property of the image, not of this render, and is asserted against a real one by the e2e case.
func quotaPolicySeedContainer(image string, pullPolicy core.PullPolicy) core.Container {
	// The error is dropped rather than plumbed out: RenderQuotaPolicy fails only on a tenant it
	// refuses, and an empty set has none. A renderer that returned an error here would have to be
	// fallible all the way up to a reconciler for a case that cannot happen.
	emptyPolicy, _ := RenderQuotaPolicy(nil)

	return core.Container{
		Name:                     quotaPolicyInitName,
		Image:                    image,
		ImagePullPolicy:          pullPolicy,
		TerminationMessagePolicy: core.TerminationMessageFallbackToLogsOnError,
		Command:                  []string{"sh", "-c", quotaPolicySeedCommand},
		Env: []core.EnvVar{
			{Name: quotaPolicySeedEnv, Value: QuotaPolicySeedFilePath},
			{Name: quotaPolicyFileEnv, Value: QuotaPolicyFilePath},
			{Name: quotaPolicyEmptyEnv, Value: string(emptyPolicy)},
		},
		VolumeMounts: []core.VolumeMount{
			{Name: quotaPolicyVolumeName, MountPath: QuotaPolicyDir},
			{Name: quotaPolicySeedVolumeName, MountPath: QuotaPolicySeedDir, ReadOnly: true},
		},
	}
}

// RenderLeaderService renders the ClusterIP Service in front of the leader.
//
// Both ports are published because both are read, by different audiences: the RPC port is what an
// inference engine dials, the admin port is what this operator scrapes for health, capacity and the
// segment listing. A Service naming only one leaves the other side with nothing to point at.
func RenderLeaderService(kvcb *workercore.KVCacheBackend) *core.Service {
	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      LeaderObjectName(kvcb),
			Namespace: kuberess.SystemNamespaceName,
			Labels:    leaderPodLabels(kvcb),
		},
		Spec: core.ServiceSpec{
			// Reached from inside the cluster only. Nothing in this scope publishes a KV cache
			// outward, and a backend is a privileged physical resource.
			Type:     core.ServiceTypeClusterIP,
			Selector: leaderSelectorLabels(kvcb),
			Ports: []core.ServicePort{
				{
					Name:       leaderRPCPortName,
					Port:       LeaderRPCPort,
					TargetPort: intstr.FromString(leaderRPCPortName),
					Protocol:   core.ProtocolTCP,
				},
				{
					Name:       leaderAdminPortName,
					Port:       LeaderMetricsPort,
					TargetPort: intstr.FromString(leaderAdminPortName),
					Protocol:   core.ProtocolTCP,
				},
			},
		},
	}

	systemmeta.NoteResource(svc, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
		"role":                      leaderResourceNoteRole,
	})
	kubemeta.ControlOnWithoutBlock(svc, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return svc
}
