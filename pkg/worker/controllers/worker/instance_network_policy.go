package worker

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// instanceTenantNetworkPolicyName is the fixed name of the NetworkPolicy every namespace holding
// Instances gets. It is one object per namespace, shared by every Instance there: the selector is
// the stable component label, so a stop/start or a new Instance changes nothing about it.
const instanceTenantNetworkPolicyName = "gpustack-instance-tenant-isolation"

// convertTenantNetworkPolicy renders the isolation contract for one namespace: Instance Pods may
// be reached from their own namespace and from anywhere outside the pod network — which is how
// external clients arrive through the NodePort Service, their traffic SNAT'd to a node address —
// but never from another namespace's Pods. Egress mirrors the rule, so a shell inside one
// Instance cannot scan or reach a neighbor tenant's Pods either; DNS stays open port-scoped,
// because a CNI may hand the policy engine the translated pod address of a DNS server.
//
// The ipBlock exceptions carry the cluster's pod CIDRs: every address inside them is a Pod of
// some namespace, and the same-namespace rule already covers the friendly ones. Only IPv4 CIDRs
// may ride a 0.0.0.0/0 rule's except list, so the caller filters the Nodes' dual-stack entries.
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
					deviceplugin.ComponentLabelKey: deviceplugin.InstanceComponent,
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

// clusterPodCIDRs learns the cluster's pod address space from the only place it is written down:
// the per-node podCIDRs. IPv6 entries are dropped because they cannot sit in the except list of
// an IPv4 cidr rule. An empty answer is NOT an error: it means no node has reported yet, and the
// caller skips the ensure rather than render a policy whose empty except list would allow the
// cross-namespace traffic this policy exists to block.
func (r *InstanceReconciler) clusterPodCIDRs(ctx context.Context) ([]string, error) {
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("list nodes for pod CIDRs: %w", err)
	}
	set := make(map[string]struct{})
	for i := range nodes.Items {
		for _, cidr := range nodes.Items[i].Spec.PodCIDRs {
			if strings.Contains(cidr, ":") {
				continue
			}
			set[cidr] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// ensureTenantNetworkPolicy brings the namespace's tenant-isolation policy to the rendered shape,
// creating it on first sight and rewriting it only when the render drifted. The object names no
// owner: it belongs to the namespace, not to whichever Instance reconciled first, so deleting an
// Instance never collects it. With no pod CIDRs reported by any node yet, the ensure is skipped:
// a render without exceptions would be an allow-everything policy wearing this policy's name.
func (r *InstanceReconciler) ensureTenantNetworkPolicy(ctx context.Context, inst *workercore.Instance) error {
	cidrs, err := r.clusterPodCIDRs(ctx)
	if err != nil {
		return err
	}
	if len(cidrs) == 0 {
		ctrllog.FromContext(ctx).V(2).Info("no node podCIDRs reported yet; skip tenant network policy")
		return nil
	}
	desired := convertTenantNetworkPolicy(cidrs)
	desired.Namespace = inst.Namespace

	np := new(networking.NetworkPolicy)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: inst.Namespace, Name: instanceTenantNetworkPolicyName,
	}, np)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("fetch tenant network policy: %w", err)
		}
		systemmeta.NoteResource(desired, "instances", nil)
		if err = r.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create tenant network policy: %w", err)
		}
		return nil
	}

	if reflect.DeepEqual(np.Spec, desired.Spec) {
		return nil
	}
	np.Spec = desired.Spec
	if err = r.Client.Update(ctx, np); err != nil {
		return fmt.Errorf("align tenant network policy: %w", err)
	}
	return nil
}
