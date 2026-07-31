package worker

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// NodeFlavorReconciler reconciles kueue.ResourceFlavor objects keyed by their own
// name, driven by both ResourceFlavor and Kubernetes Node changes:
//   - When one or more Nodes contribute to the flavor's name, (re)build the
//     ResourceFlavor — capacity = pooled nodes × per-node count — and then, when
//     instance-type-derived-from-node is enabled, author the pool's InstanceType.
//     The authoring is create-only: it never updates or deletes an existing type, so
//     an admin's edits are preserved and the InstanceTypeReconciler stays the sole
//     owner of an InstanceType's lifecycle.
//   - When no Node contributes, delete the flavor; an unused flavor advertises
//     stale capacity and a tombstone buys nothing once the ClusterQueue is rebuilt
//     from ResourceFlavor labels.
//
// The flavor is identified by (key, os, arch, count) and its notes carry only device
// information — never a unit spec. The unit spec is no longer node-derived; it is a
// fixed default stamped on the InstanceType.
//
// Watching ResourceFlavor with For(...) means a full resync on start-up
// re-evaluates every flavor, so orphans left behind by a key/count switch are
// deleted even though no Node event would ever enqueue them.
type NodeFlavorReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeFlavorReconciler)(nil)

const (
	// The schedule labels are stamped on both the ResourceFlavor and its backing
	// ClusterQueue in the node-feature vocabulary, so each is reverse-looked-up from
	// the other and from the matching Nodes/Devices by a label selector: the flavor's
	// feature key ("general."/"acceleratable." prefixed, value "true"), the well-known
	// kubernetes.io/os and kubernetes.io/arch, and — on the ResourceFlavor only — the
	// per-key ".count"/".capacity" siblings the InstanceTypeReconciler reads to build the
	// queue without listing or watching Nodes (capacity = pooled nodes × count).
	_ResourceFlavorCountLabelSuffix    = ".count"
	_ResourceFlavorCapacityLabelSuffix = ".capacity"
)

// _ResourceFlavorResType is the systemmeta resource type carried by the
// ResourceFlavors this reconciler owns.
const _ResourceFlavorResType = "nodes"

func (r *NodeFlavorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	rf := new(kueue.ResourceFlavor)
	err := r.Client.Get(ctx, req.NamespacedName, rf)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch resource flavor")
			return ctrl.Result{}, err
		}
		// Going to create a new flavor if needed.
		rf = nil
	}

	// Skip if deleted.
	if rf != nil && rf.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted resource flavor")
		return ctrl.Result{}, nil
	}

	// List the Nodes whose feature labels contribute to this flavor's name.
	ndList := new(core.NodeList)
	err = r.Client.List(ctx, ndList,
		ctrlcli.MatchingFields{IndexingNodeByScheduleFlavor: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list nodes by schedule flavor")
		return ctrl.Result{}, err
	}

	// Read instance-type-mixed-on-node per-reconcile so a runtime flip applies
	// without restart: when mixing is disabled an accelerated node does not
	// contribute to a CPU flavor.
	mixingAllowed := settings.InstanceTypeMixedOnNode.ShouldValueBool(ctx)

	// Resolve each node's flavor matching this name and collect the contributors;
	// the flavor identity is read back from the first one below.
	var contributors []*core.Node
	for i := range ndList.Items {
		nd := &ndList.Items[i]
		matched := matchNodeFlavor(nd, req.Name)
		if matched == nil {
			continue
		}
		if !mixingAllowed && !matched.Acceleratable && nodeIsAccelerated(nd) {
			continue
		}
		contributors = append(contributors, nd)
	}

	if len(contributors) == 0 {
		// No Node contributes: a flavor that does not exist yet is a no-op; an
		// existing flavor is deleted so it stops advertising stale capacity.
		if rf == nil {
			logger.V(3).Info("resource flavor not found and unused, skip")
			return ctrl.Result{}, nil
		}
		err = r.Client.Delete(ctx, rf)
		if err != nil {
			if kerrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			logger.Error(err, "delete unused resource flavor")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("deleted unused resource flavor")
		return ctrl.Result{}, nil
	}

	// Active: capacity = pooled nodes × count. Every contributor to this name yields
	// the same flavor identity (key/os/arch/count + metadata), so read it back from the
	// first — the unit spec is no longer node-derived, so there is no min to pick.
	node := contributors[0]
	flavor := matchNodeFlavor(node, req.Name)
	if flavor == nil {
		logger.V(3).Info("matched node no longer carries the flavor, skip")
		return ctrl.Result{}, nil
	}
	capacity := int64(len(contributors)) * flavor.Count

	keyLabel := featureKeyLabel(flavor.Acceleratable, flavor.OwnKey())
	labels := map[string]string{
		keyLabel:             "true",
		core.LabelOSStable:   flavor.OS,
		core.LabelArchStable: flavor.Arch,
		// The generic-vs-accelerated discriminator every pool selector matches on, so a
		// collapsed generic pool selects "not accelerated" and an aware generic pool never
		// matches an accelerated flavor of the same CPU.
		nodefeature.NodeAcceleratableLabelKey:         strconv.FormatBool(flavor.Acceleratable),
		keyLabel + _ResourceFlavorCountLabelSuffix:    strconvx.Itoa(flavor.Count),
		keyLabel + _ResourceFlavorCapacityLabelSuffix: strconvx.Itoa(capacity),
	}
	// An accelerated flavor also carries the paired CPU key's presence, so an aware
	// (CPU-split) pool can select it while a collapsed pool ignores it.
	if flavor.Acceleratable {
		labels[nodefeature.GeneralFeatureLabelPrefix+flavor.GeneralKey] = "true"
	}
	// When disallow mixed CPU/accelerator nodes,
	// mark the flavor as not acceleratable so a collapsed generic pool does not select it.
	nodeLabels := flavor.NodeLabels
	if !flavor.Acceleratable && !mixingAllowed {
		nodeLabels[nodefeature.NodeAcceleratableLabelKey] = "false"
	}
	eRf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name:   req.Name,
			Labels: labels,
		},
		Spec: kueue.ResourceFlavorSpec{
			// Tolerate every taint so a workload routed here by quota is never held
			// back by a node taint; node eligibility is governed by nodeLabels (the
			// managed mark + feature key + os/arch), not by taints.
			Tolerations: []core.Toleration{{Operator: core.TolerationOpExists}},
			NodeLabels:  nodeLabels,
		},
	}

	// Read instance-type-aware-cpu-manufacturer per-reconcile: it gates only the accelerated
	// flavor's cpuDetail note (the CPU is otherwise not a scheduling axis for an accelerated
	// flavor); it never changes the flavor's name or labels.
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)

	// generalGroup is always the CPU key; acceleratorGroup is the accelerator key on an
	// accelerated flavor, empty otherwise. Together they let the InstanceTypeFlavor and the
	// aggregation layer group by the awareness setting.
	eNotes := map[string]string{
		"acceleratable":    strconv.FormatBool(flavor.Acceleratable),
		"generalGroup":     flavor.GeneralKey,
		"acceleratorGroup": flavor.AcceleratorKey,
		"manufacturer":     flavor.Manufacturer,
		"product":          flavor.Product,
		"family":           flavor.Family,
		"memory":           flavor.Memory,
		"cores":            flavor.Cores,
	}
	// Record the raw CPU detail: always for a CPU flavor; for an accelerated flavor only when
	// CPU-manufacturer awareness is on.
	if !flavor.Acceleratable || cpuAware {
		eNotes["cpuDetail"] = cpuDetailNote(nodefeature.ExtractGeneralDetail(node), flavor.Acceleratable)
	}

	systemmeta.NoteResource(eRf, _ResourceFlavorResType, eNotes)
	rfAlignFn := func(aRf *kueue.ResourceFlavor) (_ *kueue.ResourceFlavor, skip bool, err error) {
		skip = true
		// Update schedule labels (capacity changes as nodes join or leave).
		if !mapx.Contain(aRf.Labels, eRf.Labels) {
			if aRf.Labels == nil {
				aRf.Labels = make(map[string]string)
			}
			for k, v := range eRf.Labels {
				aRf.Labels[k] = v
			}
			skip = false
		}
		// Update spec.
		if !kubemeta.DeepEqual(aRf.Spec, eRf.Spec) {
			aRf.Spec = eRf.Spec
			skip = false
		}
		// Update notes — replace the operator note set wholesale so a note that is no longer
		// desired (e.g. the awareness-gated cpuDetail after the setting flips off, or a stale
		// transitional note) is removed, not just left behind: systemmeta.NoteResource only
		// adds/overwrites. This also fixes the mirror case — a note that must now appear (cpuDetail
		// after the setting flips on) is written even though the existing notes are a subset.
		if syncNodeFlavorNotes(aRf, eNotes) {
			skip = false
		}
		return aRf, skip, nil
	}
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eRf,
		kubeclientset.WithUpdateIfExisted(rfAlignFn))
	if err != nil {
		logger.Error(err, "sync resource flavor")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("synced resource flavor")

	// Author the pool's InstanceType from the just-synced flavor when
	// instance-type-derived-from-node is enabled — create-only, never delete/update, so an
	// admin's edits to an existing type are preserved. The InstanceTypeReconciler no longer
	// derives it.
	if settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx) {
		err = r.authorDerivedInstanceType(ctx, flavor)
		if err != nil {
			logger.Error(err, "author derived instance type")
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// syncNodeFlavorNotes makes the ResourceFlavor's operator notes exactly equal to want, returning
// whether it changed anything. It replaces the note set wholesale rather than only overwriting:
// systemmeta.NoteResource cannot delete, so a note that is no longer desired (the awareness-gated
// cpuDetail once the setting flips off, or a retired transitional note) would otherwise linger, and
// a subset comparison would also miss a note that must now appear (cpuDetail once the setting flips
// on). Non-operator annotations (any key without the note prefix) are left untouched.
func syncNodeFlavorNotes(rf *kueue.ResourceFlavor, want map[string]string) bool {
	rt, have := systemmeta.DescribeResource(rf)
	if rt == _ResourceFlavorResType && maps.Equal(have, want) {
		return false
	}
	for k := range rf.Annotations {
		if strings.HasPrefix(k, systemmeta.ResourceNoteAnnoPrefix) {
			delete(rf.Annotations, k)
		}
	}
	systemmeta.NoteResource(rf, _ResourceFlavorResType, want)
	return true
}

// matchNodeFlavor returns the node's flavor whose name equals flavorName, or nil
// when the node contributes no such flavor.
func matchNodeFlavor(nd *core.Node, flavorName string) *nodefeature.NodeFlavor {
	for _, f := range nodefeature.ExtractNodeFlavors(nd) {
		if f.Name == flavorName {
			matched := f
			return &matched
		}
	}
	return nil
}

// featureKeyLabel returns the "<general.|acceleratable.>feature.gpustack.ai/<key>"
// label key (value "true") identifying a flavor's pool: general for a CPU flavor,
// acceleratable for a device flavor.
func featureKeyLabel(acceleratable bool, key string) string {
	if acceleratable {
		return nodefeature.AcceleratableFeatureLabelPrefix + key
	}
	return nodefeature.GeneralFeatureLabelPrefix + key
}

// nodeIsAccelerated reports whether the node carries any accelerator, via the
// umbrella "feature.gpustack.ai/acceleratable=true" label.
func nodeIsAccelerated(nd *core.Node) bool {
	return kubemeta.IsLabeled(nd, nodefeature.NodeAcceleratableLabelKey, "true")
}

const (
	// _InstanceTypeDerivedFromNodeLabel marks an InstanceType the operator authored by deriving it
	// from the node-fed ResourceFlavors (instance-type-derived-from-node); it is a provenance marker
	// only — a derived type is never auto-removed.
	_InstanceTypeDerivedFromNodeLabel = "schedule.gpustack.ai/derived-from-node"

	// cpuOnlyDisplayName is the DisplayName a derived CPU-manufacturer-agnostic collapsed pool
	// carries, since no single pooled node's product represents it.
	cpuOnlyDisplayName = "CPU-only"

	// derivedLocalStorage is the per-unit local storage every derived InstanceType is stamped
	// with. Unlike the unit CPU/RAM it is not preset per product, because the storage a workload
	// wants does not track the accelerator it runs on. Admins override it per InstanceType.
	derivedLocalStorage = "100Gi"
)

// authorDerivedInstanceType creates the pool's operator-owned InstanceType from a synced flavor.
// It stamps the setting-correct pool identity (general/accelerator group + acceleratable/os/arch)
// + the creation-time unit spec + the derived marker + the human-friendly DisplayName (the
// flavor's product, or the "CPU-only" sentinel for the CPU-manufacturer-agnostic collapsed pool).
// DisplayName is admin-editable, so this creation-time default never fights a later admin rename;
// an admin-created type is not auto-named. It only ever creates — an existing type (admin- or
// operator-owned) is left untouched, so an AlreadyExists is a no-op.
func (r *NodeFlavorReconciler) authorDerivedInstanceType(ctx context.Context, flavor *nodefeature.NodeFlavor) error {
	logger := ctrllog.FromContext(ctx)

	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	name, generalGroup, acceleratorGroup := flavor.DerivedInstanceTypeIdentity(cpuAware)
	unitCpu, unitRam := derivedUnitResources(flavor)

	// The collapsed generic pool folds many CPUs into one type, so no single node's product
	// represents it (the flavor carries an empty product there anyway); label it "CPU-only".
	displayName := flavor.Product
	if !flavor.Acceleratable && !cpuAware {
		displayName = cpuOnlyDisplayName
	}
	if runes := []rune(displayName); len(runes) > 64 {
		displayName = string(runes[:64])
	}

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:   name,
			Labels: map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
		},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:     generalGroup,
			AcceleratorGroup: acceleratorGroup,
			Acceleratable:    flavor.Acceleratable,
			OS:               flavor.OS,
			Arch:             flavor.Arch,
			DisplayName:      displayName,
			UnitResources:    workercore.InstanceTypeUnitResources{CPU: unitCpu, RAM: unitRam},
			LocalStorage:     derivedLocalStorage,
		},
	}
	err := r.Client.Create(ctx, it)
	if err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}

	logger.V(2).Info("authored derived instance type",
		"instance type", it.Name)
	return nil
}

// cpuDetailNote marshals a node's CPU detail into the JSON carried by the ResourceFlavor's
// cpuDetail note, in the shape the InstanceType webhook folds straight back into the spec (the
// workercore CPU structs are the single typed source shared by both sides): a non-accelerated
// flavor stores a plain InstanceTypeCPU — its CPU manufacturer/product/family are the
// InstanceType's top-level descriptors — while an accelerated flavor stores an
// InstanceTypeAcceleratorCPU that also carries the CPU's own manufacturer/product/family (distinct
// from the device's). The note is a nice-to-have, so a marshal error is ignored (ShouldMarshal).
func cpuDetailNote(d nodefeature.CPUDetail, acceleratable bool) string {
	cpu := workercore.InstanceTypeCPU{
		PhysicalCores:          d.PhysicalCores,
		ThreadsPerPhysicalCore: d.ThreadsPerPhysicalCore,
		LogicalCores:           d.LogicalCores,
		Stepping:               d.Stepping,
		ClockSpeed:             d.ClockSpeed,
		MaxClockSpeed:          d.MaxClockSpeed,
		CacheLine:              d.CacheLine,
		Cache: workercore.InstanceTypeCPUCache{
			L1I: d.CacheL1I,
			L1D: d.CacheL1D,
			L2:  d.CacheL2,
			L3:  d.CacheL3,
		},
	}
	if acceleratable {
		return string(json.ShouldMarshal(workercore.InstanceTypeAcceleratorCPU{
			Manufacturer:    d.Manufacturer,
			Product:         d.Product,
			Family:          d.Family,
			InstanceTypeCPU: cpu,
		}))
	}
	return string(json.ShouldMarshal(cpu))
}

// derivedUnitResources returns the per-unit CPU/RAM stamped on a derived InstanceType. A
// non-accelerated unit is a fixed 1 CPU / 2Gi — the webhook pins a CPU-only type's unit CPU to
// exactly 1 anyway. An accelerated unit is one whole card, sized from the per-product preset
// table; a card the table does not recognize keeps the historical 4 CPU / 16Gi. Admins override
// both per InstanceType, and the spec is immutable afterwards, so this only ever decides a
// pool's first value.
func derivedUnitResources(flavor *nodefeature.NodeFlavor) (unitCpu, unitRam string) {
	if !flavor.Acceleratable {
		return "1", "2Gi"
	}
	return nodefeature.PresetUnitResources(flavor.Manufacturer, flavor.Product)
}

const (
	// IndexingNodeByScheduleFlavor indexes managed Nodes by the ResourceFlavor names
	// they contribute to (one CPU flavor plus one per device key). A node that is not
	// managed drops out of the index, which is how the reconciler detects a flavor has
	// no Node left (→ delete).
	IndexingNodeByScheduleFlavor = "nodes.schedule.gpustack.ai/flavor"
)

// indexNodeByScheduleFlavor is the field-index extractor for
// IndexingNodeByScheduleFlavor. A deleting node still counts as present (a brief
// deletion must not churn the pool), but an unmanaged node is excluded so its
// flavors are deleted.
func indexNodeByScheduleFlavor(obj ctrlcli.Object) []string {
	nd, ok := obj.(*core.Node)
	if !ok || nd == nil {
		return nil
	}
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	return slicex.Transform(nodefeature.ExtractNodeFlavors(nd),
		func(f nodefeature.NodeFlavor) string {
			return f.Name
		})
}

func (r *NodeFlavorReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Node{}, IndexingNodeByScheduleFlavor, indexNodeByScheduleFlavor)
	if err != nil {
		return fmt.Errorf("index node '%s': %w", IndexingNodeByScheduleFlavor, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodeflavor").
		For(
			// Reconcile relevant ResourceFlavor objects keyed by their own name.
			// A full resync on start-up re-evaluates every flavor, so orphans left
			// behind by a key/count switch get deleted even though no Node event
			// would ever enqueue them.
			&kueue.ResourceFlavor{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant ResourceFlavor objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ResourceFlavorResType)
				}),
				// Trigger reconciliation when a ResourceFlavor is:
				// - created (incl. the start-up resync).
				// - updated if its spec, schedule labels (incl. capacity) or notes
				//   have changed.
				// Never react to deletion: a Node event re-creates the flavor when a
				// Node still contributes to it.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldRf, newRf := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newRf.DeletionTimestamp == nil {
							// Fire when spec has changed.
							if !kubemeta.DeepEqual(oldRf.Spec, newRf.Spec) {
								return true
							}
							// Fire when schedule labels have changed.
							if !mapx.EqualWithStringPrefix(oldRf.Labels, newRf.Labels,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix,
								core.LabelOSStable,
								core.LabelArchStable) {
								return true
							}
							// Fire when notes have changed.
							if !systemmeta.EqualResourceTypeAndNotes(oldRf, newRf) {
								return true
							}
						}
						return false
					},
				},
			),
		).
		Watches(
			// Watch Nodes and enqueue the corresponding ResourceFlavor by name.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueResourceFlavorWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - deleted (so a flavor losing its last Node gets deleted).
				// - updated if its managed mark or feature labels have changed (a
				//   node leaving management deletes its orphaned flavors).
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Fire when the managed mark or feature labels have changed.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.ManagedLabelKey,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix) {
								return true
							}
						}
						return false
					},
				},
			),
		).
		Watches(
			// Watch the InstanceTypes this reconciler authors, and enqueue the flavors when one
			// is deleted. Without it the reconciler hears about its inputs only, so an
			// InstanceType destroyed on its own is invisible: a definition lost at runtime takes
			// every one of them at once, and the flavors that derived them never change, so
			// nothing would ever author them again. The periodic informer resync is no fallback
			// either — it re-delivers the flavor unchanged, which the update filter above drops.
			&workercore.InstanceType{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueResourceFlavorsWhenDerivedInstanceTypeDeleted,
			),
			ctrlbuilder.WithPredicates(
				// Interested in the derived InstanceTypes only; an admin-owned one is nothing
				// this reconciler authors.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return obj.GetLabels()[_InstanceTypeDerivedFromNodeLabel] == "true"
				}),
				// Trigger reconciliation when a derived InstanceType is:
				// - deleted.
				// Nothing else says anything the inputs do not: authoring is create-only, so a
				// creation is this reconciler's own write, and the status churn an InstanceType
				// carries never changes whether it exists.
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
				}),
			),
		).
		Complete(r)
}

// enqueueResourceFlavorsWhenDerivedInstanceTypeDeleted enqueues every managed ResourceFlavor when
// a derived InstanceType is deleted, so the one that derived it authors it again.
//
// Which flavor derived which type cannot be read back from the type, and a cluster carries a
// handful of flavors, so all of them are enqueued; a reconcile with nothing to author is a no-op
// and authoring is create-only, so the ones that still have their type are untouched.
func (r *NodeFlavorReconciler) enqueueResourceFlavorsWhenDerivedInstanceTypeDeleted(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("instance type", ctrlcli.ObjectKeyFromObject(obj))

	rfList := new(kueue.ResourceFlavorList)
	err := r.Client.List(ctx, rfList,
		systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels](_ResourceFlavorResType),
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list resource flavors for deleted derived instance type")
		return nil
	}
	if len(rfList.Items) == 0 {
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(rfList.Items))
	for i := range rfList.Items {
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: rfList.Items[i].Name,
			},
		})
	}

	logger.V(2).Info("enqueue resource flavors from deleted derived instance type",
		"requests", reqs)
	return reqs
}

func (r *NodeFlavorReconciler) enqueueResourceFlavorWhenNodeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("node", ctrlcli.ObjectKeyFromObject(obj))

	nd := obj.(*core.Node)

	flavors := nodefeature.ExtractNodeFlavors(nd)
	if len(flavors) == 0 {
		logger.V(2).Info("node has no flavor")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(flavors))
	for i := range flavors {
		if flavors[i].Name == "" {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: flavors[i].Name,
			},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue resource flavor from node", "requests", reqs)
	return reqs
}
