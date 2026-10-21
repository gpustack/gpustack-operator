package worker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// instanceIsolationPolicyName is the fixed name of the NetworkPolicy every non-reserved
// namespace gets. It is one object per namespace, shared by every Instance that will ever land
// there: the selector is the stable resource-type label, so a stop/start or a new Instance
// changes nothing about it.
const instanceIsolationPolicyName = "gpustack-instance-isolation"

// convertIsolationPolicy renders the isolation contract for one namespace: Instance Pods
// may be reached from their own namespace and from anywhere outside the pod network — which is
// how external clients arrive through the NodePort Service, their traffic SNAT'd to a node
// address — but never from another namespace's Pods. Egress mirrors the rule, so a shell inside
// one Instance cannot scan or reach a neighbor tenant's Pods either; DNS stays open port-scoped,
// because a CNI may hand the policy engine the translated pod address of a DNS server.
//
// The ipBlock exceptions carry the pod CIDRs the administrator's Setting names: every address
// inside them is a Pod of some namespace, and the same-namespace rule already covers the friendly
// ones. Only IPv4 CIDRs may ride a 0.0.0.0/0 rule's except list, and the Setting's admission
// already refuses anything else.
func convertIsolationPolicy(podCIDRs []string) *networking.NetworkPolicy {
	external := networking.IPBlock{CIDR: "0.0.0.0/0", Except: podCIDRs}
	sameNamespace := networking.NetworkPolicyPeer{PodSelector: &meta.LabelSelector{}}
	return &networking.NetworkPolicy{
		ObjectMeta: meta.ObjectMeta{
			Name: instanceIsolationPolicyName,
		},
		Spec: networking.NetworkPolicySpec{
			PodSelector: meta.LabelSelector{
				MatchLabels: map[string]string{
					systemmeta.ResourceTypeLabel: "instances",
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

// InstanceIsolationReconciler reconciles every non-reserved namespace's
// namespace-isolation policy against the administrator's pod-CIDR Setting. It watches Namespaces,
// so a namespace is covered from the moment it exists — before its first Instance — and it
// watches the delegated settings Secret, so a Setting edit converges every namespace without
// waiting for an unrelated object event.
//
// The Setting value travels through currentPodCIDRs, never through a live read on the reconcile
// path: a read that fails mid-flight would otherwise look exactly like an emptied Setting, and an
// emptied Setting collects the policies. The cache is primed once at setup, refreshed only when a
// Secret event's freshly parsed value differs from the cached one, and a reconcile that finds the
// cache empty re-probes and errors for another try instead of acting on an unknown.
type InstanceIsolationReconciler struct {
	Client ctrlcli.Client

	currentPodCIDRs atomic.Value // holds []string; absent until the first successful read
}

var _ ctrlreconcile.Reconciler = (*InstanceIsolationReconciler)(nil)

// Reconcile brings one namespace's namespace-isolation policy in line with the last-known pod
// CIDRs. Reserved namespaces hold no Instance Pods and are never isolated, the same rule the
// LocalQueue entrance follows; a namespace that has vanished meanwhile needs nothing, because its
// policy died with it.
func (r *InstanceIsolationReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ns := new(core.Namespace)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, ns, ctrlclix.WithoutQuorum)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetch namespace: %w", err)
	}
	if ns.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted namespace")
		return ctrl.Result{}, nil
	}
	if kuberess.IsReservedNamespace(ns.Name) {
		logger.V(3).Info("skip reserved namespace")
		return ctrl.Result{}, nil
	}

	cidrs, err := r.lastKnownPodCIDRs(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := convertIsolationPolicy(cidrs)
	desired.Namespace = ns.Name

	np := new(networking.NetworkPolicy)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: ns.Name, Name: instanceIsolationPolicyName,
	}, np, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetch namespace isolation policy: %w", err)
		}
		if len(cidrs) == 0 {
			// An emptied Setting and no policy of ours is already the desired state.
			return ctrl.Result{}, nil
		}
		systemmeta.NoteResource(desired, "instances", nil)
		if err = r.Client.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("create namespace isolation policy: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if len(cidrs) == 0 {
		// The opt-out collects the policy this operator noted as its own. A same-named policy
		// nobody here rendered belongs to whoever wrote it, and deleting their networking over a
		// Setting edit is not this controller's call.
		if systemmeta.DescribeResourceType(np) != "instances" {
			logger.V(2).Info("namespace isolation policy was not rendered by this operator; leaving it alone",
				"namespace", ns.Name)
			return ctrl.Result{}, nil
		}
		if err = r.Client.Delete(ctx, np); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete namespace isolation policy: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// The same courtesy the opt-out extends: a same-named policy nobody here rendered belongs
	// to whoever wrote it, and rewriting their networking over a Setting change is not this
	// controller's call either.
	if systemmeta.DescribeResourceType(np) != "instances" {
		logger.V(2).Info("namespace isolation policy was not rendered by this operator; leaving it alone",
			"namespace", ns.Name)
		return ctrl.Result{}, nil
	}

	if kubemeta.DeepEqual(np.Spec, desired.Spec) {
		return ctrl.Result{}, nil
	}
	np.Spec = desired.Spec
	if err = r.Client.Update(ctx, np); err != nil {
		return ctrl.Result{}, fmt.Errorf("align namespace isolation policy: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupController watches Namespaces — the create, the edit and the delete all carry a state the
// policy has to match — and the delegated settings Secret, whose content IS the Setting value the
// policy renders from. The Setting is an extension-API view over that Secret, so the Secret is
// the thing a watch can actually see. The cache is primed here from one probe, so the first
// reconcile never has to read the Setting live.
func (r *InstanceIsolationReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	if err := r.refreshPodCIDRsFromSecret(ctx); err != nil {
		// Unknown at start-up is not empty: the first reconciles re-probe and retry until a real
		// value lands in the cache, and nothing is collected on the way.
		ctrllog.FromContext(ctx).Error(err, "prime the pod CIDRs cache from the delegated settings secret")
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("instanceisolation").
		For(&core.Namespace{}).
		Watches(
			&core.Secret{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueNamespacesForSettingsSecret),
		).
		Complete(r)
}

// enqueueNamespacesForSettingsSecret fans one delegated settings Secret change out to every
// namespace — but only when the fresh content actually differs from what the policies were last
// rendered from: a touched-but-identical Secret is the common shape of a re-list, and fanning it
// out would reconcile the whole cluster for nothing. A successfully parsed change refreshes the
// cache first, so the reconciles that follow read the value that triggered them.
func (r *InstanceIsolationReconciler) enqueueNamespacesForSettingsSecret(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	if obj.GetNamespace() != setting.DelegatedSecretNamespace || obj.GetName() != setting.DelegatedSecretName {
		return nil
	}
	sec, ok := obj.(*core.Secret)
	if !ok {
		return nil
	}
	fresh := parseIsolationPodCIDRs(string(sec.Data[settings.InstanceIsolationPodCIDRs.Name()]))
	if current, ok := r.currentPodCIDRs.Load().([]string); ok && slices.Equal(current, fresh) {
		return nil
	}
	r.currentPodCIDRs.Store(fresh)

	namespaces := new(core.NamespaceList)
	if err := r.Client.List(ctx, namespaces, ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list namespaces for a settings change")
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0, len(namespaces.Items))
	for i := range namespaces.Items {
		if kuberess.IsReservedNamespace(namespaces.Items[i].Name) {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: namespaces.Items[i].Name}})
	}
	return reqs
}

// lastKnownPodCIDRs returns the pod CIDRs the policies are rendered from. The cached value wins:
// it is what the last observed Secret content said, and a live read that failed mid-flight must
// never be mistaken for an emptied Setting, which collects the policies. An absent cache means
// nothing has been observed yet — one live probe settles it, and a failed probe is an error for
// another try, never an answer.
func (r *InstanceIsolationReconciler) lastKnownPodCIDRs(ctx context.Context) ([]string, error) {
	if current, ok := r.currentPodCIDRs.Load().([]string); ok {
		return current, nil
	}
	if err := r.refreshPodCIDRsFromSecret(ctx); err != nil {
		return nil, err
	}
	return r.currentPodCIDRs.Load().([]string), nil
}

// refreshPodCIDRsFromSecret reads the delegated settings Secret and refreshes the cache from its
// content for this one Setting. A missing key reads as the blank default — the Setting has never
// been written, and blank is the opt-out.
func (r *InstanceIsolationReconciler) refreshPodCIDRsFromSecret(ctx context.Context) error {
	sec := new(core.Secret)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName,
	}, sec, ctrlclix.WithoutQuorum)
	if err != nil {
		return fmt.Errorf("read the delegated settings secret: %w", err)
	}
	r.currentPodCIDRs.Store(parseIsolationPodCIDRs(string(sec.Data[settings.InstanceIsolationPodCIDRs.Name()])))
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
