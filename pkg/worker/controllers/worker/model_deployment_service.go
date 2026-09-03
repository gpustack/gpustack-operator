package worker

import (
	"maps"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// renderModelDeploymentService renders the one Service a deployment is reached through.
//
// It is deliberately NOT what instance.go does. convertServiceFromPod renders one NodePort Service
// PER POD, which is right for an Instance — a single addressable development box a user connects
// to. N interchangeable replicas behind one address is a different shape: a per-Pod NodePort would
// publish N addresses with no load balancing across them, and burn one node port per replica.
//
// It fronts the FIRST role. With one role that is the only reading; the spec that introduces P/D
// roles decides which of several is the front door, and does so knowing what a router in front of
// them means — a question a single-role deployment cannot answer and must not pre-empt.
func renderModelDeploymentService(md *workercore.ModelDeployment) *core.Service {
	role := &md.Spec.Roles[0]
	port := modelDeploymentServicePort(role)

	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      md.Name,
			Namespace: md.Namespace,
			Labels:    modelDeploymentSelectorLabels(md, role),
		},
		Spec: core.ServiceSpec{
			Type: core.ServiceTypeClusterIP,
			// The selector carries identity only. A Service's selector is mutable, unlike a
			// Deployment's, but moving it would orphan every replica it used to front — so nothing
			// a spec update can change is in it, and the entrance label in particular is not.
			Selector: modelDeploymentSelectorLabels(md, role),
			Ports: []core.ServicePort{{
				Name:       port.Name,
				Protocol:   port.Protocol,
				Port:       port.ContainerPort,
				TargetPort: intstr.FromInt32(port.ContainerPort),
			}},
		},
	}

	systemmeta.NoteResource(svc, ModelDeploymentResourceType, map[string]string{
		ModelDeploymentResourceNoteRole: role.Name,
	})
	kubemeta.ControlOnWithoutBlock(svc, md, workercore.SchemeGroupVersionKind("ModelDeployment"))

	return svc
}

// modelDeploymentServicePort is the port the deployment is served on: the role template's first
// entry, or the default every supported engine's OpenAI-compatible server listens on.
//
// It reads the same render the replicas get rather than the template directly, so the Service and
// the containers behind it cannot name different ports.
func modelDeploymentServicePort(role *workercore.ModelDeploymentRole) core.ContainerPort {
	if role.Template == nil {
		return core.ContainerPort{
			Name:          modelDeploymentDefaultPortName,
			Protocol:      core.ProtocolTCP,
			ContainerPort: modelDeploymentDefaultPort,
		}
	}

	return modelDeploymentContainerPorts(role.Template)[0]
}

// modelDeploymentEndpoint is the one address every replica serves behind.
//
// It is derived rather than read back off the Service, because the in-cluster DNS name is decided by
// the Service's name and namespace — both of which this operator chose — and a ClusterIP is neither
// the address callers use nor stable across a recreate.
func modelDeploymentEndpoint(md *workercore.ModelDeployment) string {
	port := modelDeploymentServicePort(&md.Spec.Roles[0])

	return "http://" + md.Name + "." + md.Namespace + ".svc:" + strconvx.Itoa(int(port.ContainerPort))
}

// alignModelDeploymentService folds the rendered Service onto the observed one and reports whether
// anything changed.
//
// It aligns rather than replaces because a Service carries state the operator did not write and
// must not discard — the allocated ClusterIP above all, which every client that resolved it is
// still using.
func alignModelDeploymentService(actual, expected *core.Service) (changed bool) {
	if !maps.Equal(actual.Spec.Selector, expected.Spec.Selector) {
		actual.Spec.Selector = expected.Spec.Selector
		changed = true
	}

	if !equalModelDeploymentServicePorts(actual.Spec.Ports, expected.Spec.Ports) {
		actual.Spec.Ports = expected.Spec.Ports
		changed = true
	}

	return changed
}

// equalModelDeploymentServicePorts compares the fields this operator renders, and only those. A
// Service's ports carry defaults the API server fills in, so comparing the whole slice would report
// a difference on every pass and rewrite the object forever.
func equalModelDeploymentServicePorts(actual, expected []core.ServicePort) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range expected {
		if actual[i].Name != expected[i].Name ||
			actual[i].Port != expected[i].Port ||
			actual[i].Protocol != expected[i].Protocol ||
			actual[i].TargetPort != expected[i].TargetPort {
			return false
		}
	}

	return true
}
