package worker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// instanceTenantNetworkPolicyName is the fixed name of the NetworkPolicy every namespace holding
// Instances gets. It is one object per namespace, shared by every Instance there: the selector is
// the stable resource-type label, so a stop/start or a new Instance changes nothing about it.
const instanceTenantNetworkPolicyName = "gpustack-instance-tenant-isolation"

// convertTenantNetworkPolicy renders the isolation contract for one namespace: Instance Pods may
// be reached from their own namespace and from anywhere outside the pod network — which is how
// external clients arrive through the NodePort Service, their traffic SNAT'd to a node address —
// but never from another namespace's Pods. Egress mirrors the rule, so a shell inside one
// Instance cannot scan or reach a neighbor tenant's Pods either; DNS stays open port-scoped,
// because a CNI may hand the policy engine the translated pod address of a DNS server.
//
// The ipBlock exceptions carry the pod CIDRs the administrator's Setting names: every address
// inside them is a Pod of some namespace, and the same-namespace rule already covers the friendly
// ones. Only IPv4 CIDRs may ride a 0.0.0.0/0 rule's except list, and the Setting's admission
// already refuses anything else.
func convertTenantNetworkPolicy(podCIDRs []string) *networking.NetworkPolicy {
	external := networking.IPBlock{CIDR: "0.0.0.0/0", Except: podCIDRs}
	sameNamespace := networking.NetworkPolicyPeer{PodSelector: &meta.LabelSelector{}}
	return &networking.NetworkPolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: instanceTenantNetworkPolicyName,
		},
		Spec: networking.NetworkPolicySpec{
			PodSelector: meta.LabelSelector{
				MatchLabels: map[string]string{
					systemmeta.ResourceTypeLabel: deviceplugin.InstanceResourceType,
				},
			},
			PolicyTypes: []networking.PolicyType{
				networking.PolicyTypeIngress,
				networking.PolicyTypeEgress,
			},
			Ingress: []networking.NetworkPolicyIngressRule{{
				From: []networking.NetworkPolicyPeer{
					sameNamespace,
					{IPBlock: &external},
				},
			}},
			Egress: []networking.NetworkPolicyEgressRule{
				{To: []networking.NetworkPolicyPeer{sameNamespace}},
				{To: []networking.NetworkPolicyPeer{{IPBlock: &external}}},
				{Ports: []networking.NetworkPolicyPort{
					{Protocol: ptr.To(core.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))},
					{Protocol: ptr.To(core.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))},
				}},
			},
		},
	}
}

// ensureTenantNetworkPolicy brings the namespace's tenant-isolation policy in line with the
// administrator's pod-CIDR Setting, the one source of the pod address space. It is read through
// the cache on every pass and rendered into the desired spec without touching any other object,
// so a reconcile costs one cached read of the policy itself and nothing else — no node lists, no
// CNI CRD reads, nothing to learn wrong.
//
// A non-empty Setting renders and aligns: the policy is created on first sight and rewritten only
// when the render drifted, the drift comparison made with the API machinery's semantic equality so
// a hand-touched object converges instead of fighting. An empty Setting is the opt-out: the policy
// this operator noted as its own is collected again, and an object of the same name that nobody
// here rendered is left exactly as found.
func (r *InstanceReconciler) ensureTenantNetworkPolicy(ctx context.Context, inst *workercore.Instance) error {
	cidrs := parseIsolationPodCIDRs(settings.InstanceTenantIsolationPodCIDRs.ShouldValue(ctx))
	if len(cidrs) == 0 {
		return r.deleteTenantNetworkPolicy(ctx, inst.Namespace)
	}
	desired := convertTenantNetworkPolicy(cidrs)
	desired.Namespace = inst.Namespace

	np := new(networking.NetworkPolicy)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: inst.Namespace, Name: instanceTenantNetworkPolicyName,
	}, np)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("fetch tenant network policy: %w", err)
		}
		systemmeta.NoteResource(desired, deviceplugin.InstanceResourceType, nil)
		if err = r.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create tenant network policy: %w", err)
		}
		return nil
	}

	if apiequality.Semantic.DeepEqual(np.Spec, desired.Spec) {
		return nil
	}
	np.Spec = desired.Spec
	if err = r.Client.Update(ctx, np); err != nil {
		return fmt.Errorf("align tenant network policy: %w", err)
	}
	return nil
}

// deleteTenantNetworkPolicy collects the tenant-isolation policy of one namespace when the
// pod-CIDR Setting empties. Only an object this operator noted as its own is removed: the fixed
// name is this policy's public contract, so an object of the same name that nobody here rendered
// belongs to whoever wrote it, and deleting their networking over a Setting edit is not this
// controller's call. It is idempotent the way every level-based step must be: a missing policy is
// already the desired state.
func (r *InstanceReconciler) deleteTenantNetworkPolicy(ctx context.Context, namespace string) error {
	np := new(networking.NetworkPolicy)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: namespace, Name: instanceTenantNetworkPolicyName,
	}, np)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("fetch tenant network policy: %w", err)
		}
		return nil
	}
	if systemmeta.DescribeResourceType(np) != deviceplugin.InstanceResourceType {
		ctrllog.FromContext(ctx).V(2).Info(
			"tenant network policy was not rendered by this operator; leaving it alone",
			"namespace", namespace, "name", instanceTenantNetworkPolicyName,
		)
		return nil
	}
	if err := r.Client.Delete(ctx, np); err != nil {
		return fmt.Errorf("delete tenant network policy: %w", err)
	}
	return nil
}

// parseIsolationPodCIDRs splits the Setting's comma-separated value into the CIDRs the ipBlock
// exceptions carry, deduplicated and sorted so the render is byte-stable across reconciles and
// across worker restarts. Blank segments from a trailing comma or padded spacing are dropped; the
// Setting's admission has already refused anything that is not an IPv4 CIDR.
func parseIsolationPodCIDRs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	set := make(map[string]struct{})
	for _, segment := range strings.Split(raw, ",") {
		cidr := strings.TrimSpace(segment)
		if cidr == "" {
			continue
		}
		set[cidr] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}
