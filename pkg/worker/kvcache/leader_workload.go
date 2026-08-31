package kvcache

import (
	"fmt"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// ResourceType is what every object rendered for a backend is noted as, and ResourceNoteBackend
	// is the note carrying which backend it belongs to.
	//
	// They are exported because the reconciler's watches read them: a Deployment changing anywhere
	// in the cluster is filtered down to this operator's own by the type, and then mapped back to
	// the backend to re-enqueue by the note. Without that pair, the watch would either wake on
	// every Deployment in the cluster or need a label selector that duplicates what the note
	// already says.
	ResourceType        = "kvcachebackends"
	ResourceNoteBackend = "backend"

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
			// One, and only one. Admission refuses anything else, and a second replica here would
			// be two masters with two views of the same segments rather than a spare.
			Replicas: ptr.To[int32](1),
			// Recreate, because the default would undo the line above during every update. A
			// RollingUpdate's maxSurge defaults to 25%, which ROUNDS UP — to one, against one
			// desired replica — so the new master is started before the old one is stopped and the
			// two run at once. That is the split brain the replica count exists to prevent, and it
			// would happen on every image or flag change rather than never.
			//
			// The cost is a gap with no master. It is the right trade: a member that loses its
			// master keeps its segment and re-registers, while two masters allocating against one
			// pool cannot be reconciled after the fact.
			Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
			Selector: &meta.LabelSelector{MatchLabels: leaderSelectorLabels(kvcb)},
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
						leaderContainerSpec(leader, image, EffectivePullPolicy(kvcb, image)),
					},
				},
			},
		},
	}

	systemmeta.NoteResource(deploy, ResourceType, map[string]string{
		ResourceNoteBackend: kvcb.Name,
		"role":              leaderResourceNoteRole,
	})
	kubemeta.ControlOnWithoutBlock(deploy, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return deploy
}

// leaderContainerSpec is the container the Deployment runs.
//
// What it does NOT ask for is load-bearing: no hostNetwork, no device, no volume, no privilege. The
// leader is a metadata service that holds no cache bytes — the members are the side that needs the
// host — so anything here would be a privilege nobody asked for.
func leaderContainerSpec(
	leader workercore.KVCacheBackendLeader, image string, pullPolicy core.PullPolicy,
) core.Container {
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
		Command: []string{"mooncake_master"},
		Args:    RenderLeaderFlags(leader),
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

	systemmeta.NoteResource(svc, ResourceType, map[string]string{
		ResourceNoteBackend: kvcb.Name,
		"role":              leaderResourceNoteRole,
	})
	kubemeta.ControlOnWithoutBlock(svc, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return svc
}
