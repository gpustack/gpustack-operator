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

// renderModelDeploymentServices renders every Service a deployment owns: the one it is reached
// through, and one per role.
//
// The deployment-wide one is FIRST, and the order is part of the contract: the caller garbage
// collects by name, and status reads its endpoint.
func renderModelDeploymentServices(md *workercore.ModelDeployment) []*core.Service {
	svcs := make([]*core.Service, 0, len(md.Spec.Roles)+1)
	svcs = append(svcs, renderModelDeploymentService(md))
	for i := range md.Spec.Roles {
		svcs = append(svcs, renderModelDeploymentRoleService(md, &md.Spec.Roles[i]))
	}

	return svcs
}

// renderModelDeploymentService renders the one Service a deployment is reached through.
//
// It is deliberately NOT what instance.go does. convertServiceFromPod renders one NodePort Service
// PER POD, which is right for an Instance — a single addressable development box a user connects
// to. N interchangeable replicas behind one address is a different shape: a per-Pod NodePort would
// publish N addresses with no load balancing across them, and burn one node port per replica.
//
// IT STILL FRONTS THE FIRST ROLE, and with P/D roles that stays true rather than being decided
// otherwise. The single-role spec left the choice to this one; this one's Non-Goals answer it by
// declining to route: nothing here steers a request between a prefiller and a decoder, so no role is
// a front door in the sense that would make choosing between them meaningful. A Service selecting
// EVERY role would be worse than arbitrary — it would round-robin a request onto a process
// configured as a producer and one configured as a consumer, which is the silent wrong answer this
// spec's whole kind field exists to prevent. Per-role addressability is what this task adds; a real
// front door needs the router, and that is a later spec.
func renderModelDeploymentService(md *workercore.ModelDeployment) *core.Service {
	return renderModelDeploymentServiceFor(md, &md.Spec.Roles[0], md.Name)
}

// renderModelDeploymentRoleService renders the Service that fronts ONE role.
//
// A P/D deployment needs it because a decoder has to be reachable AS a decoder: the two roles are
// deliberately different configurations, so an address that resolves to "whichever replica" is not
// an address for either of them. Nothing in this spec calls it — no router exists yet — and that is
// exactly why it is rendered now: the pairing spec that will call it should find the addresses
// already there rather than have to change this object's shape to get them.
//
// The name is <deployment>-<role>, and a Service name is a DNS-1035 label that stops at 63
// characters while an object name runs to 253.
//
// THIS IS A NEW CLASS OF FAILURE, NOT AN INHERITED ONE — an earlier version of this comment claimed
// otherwise and was wrong. The deployment-wide Service fails only for a deployment whose own name is
// too long; this one fails for a 40-character deployment with a 30-character role, both of which are
// legal on their own. The reachable input set changed, which is what decides it, rather than whether
// the failure has a familiar shape.
//
// So the webhook refuses the combination at admission — validateModelDeploymentRoleServiceNames —
// where the message can name both halves. Without that the Pod-side of the deployment works, the
// per-role Service is rejected on every create, and the reconciler retries forever with the cause
// two objects away from the field that caused it.
func renderModelDeploymentRoleService(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) *core.Service {
	return renderModelDeploymentServiceFor(md, role, md.Name+"-"+role.Name)
}

// renderModelDeploymentServiceFor is the shape both Services share: a ClusterIP fronting one role's
// replicas, on that role's port, owned by the deployment.
func renderModelDeploymentServiceFor(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, name string,
) *core.Service {
	port := modelDeploymentServicePort(role)

	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: md.Namespace,
			Labels:    modelDeploymentSelectorLabels(md, role),
		},
		Spec: core.ServiceSpec{
			Type: core.ServiceTypeClusterIP,
			// The selector carries identity only. A Service's selector is mutable, unlike a
			// Deployment's, but moving it would orphan every replica it used to front — so nothing
			// a spec update can change is in it, and the entrance label in particular is not.
			//
			// It names the DEPLOYMENT as well as the role. A selector on the role alone would make
			// two deployments in one namespace, each running a role called "decode", share endpoints.
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

	// THE TYPE IS CONVERGED TOO, and it became reachable when this controller began watching the
	// Service: an out-of-band edit to NodePort or LoadBalancer now wakes this reconciler, and
	// leaving the type alone would mean the wake-up observed the drift and corrected everything
	// except it. A deployment's front door is a ClusterIP by design -- exposing every replica set on
	// a node port is a decision this operator does not make on a user's behalf.
	//
	// The ports are replaced along with it rather than compared: moving away from NodePort has to
	// drop the `nodePort` the API server assigned, and the port comparison above deliberately
	// ignores that field, so it would not notice.
	if actual.Spec.Type != expected.Spec.Type {
		actual.Spec.Type = expected.Spec.Type
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
