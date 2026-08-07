package detector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/kuberess"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/datax"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

var logger = klog.Background().WithName("detector")

type Detector struct {
	noPCICheck              bool
	manufacturers           sets.Set[string]
	noFastFailed            bool
	detectors               []device.Detector
	monitorPeriod           time.Duration
	monitorSnapshot         datax.Snapshot[MonitorSnapshot]
	detectedManufacturersCh chan<- sets.Set[string]
}

// MonitorSnapshot is the latest accelerator metrics sample stored by the monitor loop.
//
// The envelope carries its own Timestamp (the time the device manager stored the sample) because
// device.MetricsGroupList has no top-level timestamp — each MetricsGroup timestamps itself — and a
// single store time makes staleness checks (one monitor period) a one-field comparison.
type MonitorSnapshot struct {
	// Timestamp is the time when the sample was stored,
	// zero before the first monitor tick.
	Timestamp time.Time `json:"timestamp"`
	// Groups is the latest accelerator metrics sample,
	// empty before the first monitor tick.
	Groups device.MetricsGroupList `json:"groups"`
}

// New creates a Detector with the given configuration.
func New(c *Config) (*Detector, error) {
	isWsl := stringx.ContainSubstrings(osx.GetRelease(), "microsoft", "wsl")
	noPCICheck := c.NoPCICheck || isWsl

	manufacturers := getAllowedManufacturers(c.Manufacturers)

	creators := getAllowedDetectorCreators(manufacturers)
	createOpts := device.DetectorOptions{
		Logger: logger.V(3),
	}
	detectors := make([]device.Detector, 0, len(creators))
	for i := range creators {
		detectors = append(detectors, creators[i](createOpts))
	}

	return &Detector{
		noPCICheck:              noPCICheck,
		manufacturers:           manufacturers,
		noFastFailed:            c.NoFastFailed,
		detectors:               detectors,
		monitorPeriod:           c.MonitorPeriod,
		detectedManufacturersCh: c.DetectedManufacturersCh,
	}, nil
}

// DetectAccelerator detects the accelerators on the node and returns a list of device groups once.
func (d *Detector) DetectAccelerator(_ context.Context) (grpListMerged device.DevicesGroupList) {
	for i := range d.detectors {
		logger := logger.V(2).WithValues("manufacturer", d.detectors[i].Name())

		grpList, err := d.detectors[i].DetectAccelerator(d.noPCICheck)
		if err != nil {
			logger.Error(err, "detect accelerators")
			continue
		}
		if len(grpList) == 0 {
			continue
		}

		grpListMerged = append(grpListMerged, grpList...)
		logger.Info("detected accelerators")
	}

	return grpListMerged
}

// MonitorAccelerator monitors the accelerators on the node and returns a list of metrics groups once.
func (d *Detector) MonitorAccelerator(_ context.Context) (grpListMerged device.MetricsGroupList) {
	for i := range d.detectors {
		logger := logger.V(2).WithValues("manufacturer", d.detectors[i].Name())

		grpList, err := d.detectors[i].MonitorAccelerator(d.noPCICheck)
		if err != nil {
			logger.Error(err, "monitor accelerators")
			continue
		}
		if len(grpList) == 0 {
			continue
		}

		grpListMerged = append(grpListMerged, grpList...)
		logger.Info("monitored accelerators")
	}

	return grpListMerged
}

// MonitorSnapshot returns the latest accelerator metrics sample stored by the monitor loop,
// or nil if the monitor has not completed a tick yet.
func (d *Detector) MonitorSnapshot() *MonitorSnapshot {
	return d.monitorSnapshot.Load()
}

// _DeviceKey represents the comparable unique identifier of a device.
//
// This is used to compare the devices detected in different loops,
// so that we can decide whether to continue monitoring or re-detecting.
type _DeviceKey struct {
	Manufacturer string
	ID           string
	Unhealthy    bool
}

// Start starts the detector to detect and monitor the devices periodically until the context is canceled.
func (d *Detector) Start(ctx context.Context) error {
	failedOnFirstNotDetected := !d.noFastFailed && d.manufacturers.Len() == 1

	return waitx.UntilContextCancel(ctx, d.monitorPeriod, true, func(ctx context.Context) error {
		logger.V(2).Info("detecting")

		devicesGrpList := d.DetectAccelerator(ctx)

		// Get detected device keys and manufacturers from the detect result.
		deviceKeys := sets.New[_DeviceKey]()
		manufacturers := sets.New[string]()
		if len(devicesGrpList) != 0 {
			for i := range devicesGrpList {
				manufacturers.Insert(devicesGrpList[i].Manufacturer)
				for j := range devicesGrpList[i].Accelerators {
					deviceKeys.Insert(_DeviceKey{
						devicesGrpList[i].Manufacturer,
						devicesGrpList[i].Accelerators[j].ID,
						devicesGrpList[i].Accelerators[j].Status.Unhealthy,
					})
				}
			}
		}

		// If there is no device detected,
		// but there is one manufacturer expected,
		// return error directly without monitoring,
		// which means the detector is failed.
		if failedOnFirstNotDetected {
			if manufacturers.Len() == 0 {
				err := fmt.Errorf("manufacturer %s is expected but not detected", d.manufacturers.UnsortedList()[0])
				logger.Error(err, "failed to detect, quitting...")
				return err
			}
			failedOnFirstNotDetected = false
		}

		// Publish detected devices groups.
		if d.detectedManufacturersCh != nil {
			select {
			case d.detectedManufacturersCh <- manufacturers:
			default:
				logger.Info("skipping publishing detected manufacturers, channel is full")
			}
		}

		// Report devices.
		logger.V(2).Info("reporting")
		if err := d.reportDevices(ctx, devicesGrpList); err != nil {
			logger.Error(err, "reporting")
			deviceKeys.Clear() // Clear device keys to trigger re-detection in the next loop.
		}

		// Monitor devices and update the monitor snapshot.
		_ = waitx.UntilContextCancel(ctx, d.monitorPeriod, true, func(ctx context.Context) error {
			logger.V(3).Info("monitoring")

			metricsGrpList := d.MonitorAccelerator(ctx)
			if len(metricsGrpList) != 0 {
				d.monitorSnapshot.Store(&MonitorSnapshot{
					Timestamp: time.Now(),
					Groups:    metricsGrpList,
				})
			}

			// Get current devices ID from the monitor result.
			curDeviceKeys := sets.New[_DeviceKey]()
			if len(metricsGrpList) != 0 {
				for i := range metricsGrpList {
					for j := range metricsGrpList[i].Accelerators {
						curDeviceKeys.Insert(_DeviceKey{
							metricsGrpList[i].Manufacturer,
							metricsGrpList[i].Accelerators[j].ID,
							metricsGrpList[i].Accelerators[j].Unhealthy,
						})
					}
				}
			}

			// Compare the current devices with the previous devices,
			// if they are the same, continue to monitor,
			// otherwise, detect again.
			if !deviceKeys.Equal(curDeviceKeys) {
				logger.Info("changed, going to detect again")
				return waitx.ErrCanceled
			}
			return nil
		})

		return nil
	})
}

type (
	// _DeviceGroupKey represents the comparable unique identifier of a device group.
	//
	// This is used to index the existing device groups when reporting devices,
	// so that we can update or remove the existing groups based on the expected groups.
	_DeviceGroupKey struct {
		Manufacturer string
		ID           string
	}
	// _DeviceGroupValue represents the value of a device group in the indexer.
	//
	// This is used to mark whether the existing group should be removed if it is not in the expected groups.
	_DeviceGroupValue struct {
		Group  device.DevicesGroup
		Remove bool
	}
)

func (d *Detector) reportDevices(ctx context.Context, eGroups device.DevicesGroupList) error {
	ndName := osx.Getenv("KUBERNETES_NODE_NAME")
	if ndName == "" {
		return errors.New("environment variable KUBERNETES_NODE_NAME is not set")
	}

	lpCli := system.LoopbackKubeClient.Get()
	nd, err := lpCli.CoreV1().Nodes().Get(ctx, ndName,
		meta.GetOptions{
			ResourceVersion: "0",
		})
	if err != nil {
		return err
	}

	// Skip if deleted.
	if nd.DeletionTimestamp != nil {
		return errors.New("skip deleted node")
	}

	// NodeFeature.

	nfCli := lpCli.NfdV1alpha1().NodeFeatures(kuberess.SystemNamespaceName)
	eNf := &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{
			Name:      ndName + "-gpustack-device-manager",
			Namespace: kuberess.SystemNamespaceName,
			Labels: map[string]string{
				nfd.NodeFeatureObjNodeNameLabel: ndName,
				"app.kubernetes.io/part-of":     "gpustack-operator-device-manager",
			},
		},
		Spec: func() nfd.NodeFeatureSpec {
			nfs := nfd.NewNodeFeatureSpec()
			nfs.Labels = nodefeature.ConstructAcceleratableNodeLabels(eGroups)
			return *nfs
		}(),
	}
	kubemeta.ControlOnWithoutBlock(eNf, nd, core.SchemeGroupVersion.WithKind("Node"))
	nfAlignFn := func(aNf *nfd.NodeFeature) (_ *nfd.NodeFeature, skip bool, err error) {
		skip = true
		if aNf.Labels == nil {
			aNf.Labels = make(map[string]string)
		}
		if aNf.Spec.Labels == nil {
			aNf.Spec.Labels = make(map[string]string)
		}
		// Update labels if not contained.
		for k, v := range eNf.Labels {
			if aNf.Labels[k] != v {
				aNf.Labels[k] = v
				skip = false
			}
		}
		// Update spec labels if not contained.
		for k, v := range eNf.Spec.Labels {
			if aNf.Spec.Labels[k] != v {
				aNf.Spec.Labels[k] = v
				skip = false
			}
		}
		// Update owner reference.
		if !kubemeta.IsControlledBy(aNf, nd) {
			controlOnNodeWithoutBlock(aNf, nd)
			skip = false
		}
		return aNf, skip, err
	}

	aNf, err := kubeclientset.Create(ctx, nfCli, eNf,
		kubeclientset.WithUpdateIfExisted(nfAlignFn))
	if err != nil {
		return fmt.Errorf("failed to sync NodeFeature object for node %s: %w", ndName, err)
	}

	// Devices.

	devsCli := lpCli.WorkerV1alpha1().Devices()
	// Stamp the accelerator flavors' selector labels (os/arch + feature key) so the worker
	// locates this node's Devices by one List. Take the feature labels from the NodeFeature
	// just applied, not read back off the node, which NFD merges only later — leaving a
	// freshly onboarded node's Devices unstamped. gpustack.ai/managed is synced separately
	// by NodeDevicesReconciler.
	devsLabels := acceleratableDevicesSelectorLabels(nd, aNf.Spec.Labels)
	eDevs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{
			Name:   ndName,
			Labels: devsLabels,
		},
		Spec: workercore.DevicesSpec{
			Groups: eGroups,
		},
	}
	// Owned by the Node, not by the NodeFeature above: Devices is cluster-scoped, and the garbage
	// collector resolves a cluster-scoped dependent's owner in the EMPTY namespace. A namespaced
	// owner is therefore unresolvable — it yields an OwnerRefInvalidNamespace warning on every
	// sweep and the object is never collected, so a Devices outlives the node it describes and
	// keeps reporting that node's cards to every consumer that lists them.
	kubemeta.ControlOnWithoutBlock(eDevs, nd, core.SchemeGroupVersion.WithKind("Node"))
	devsAlginFn := func(aDevs *workercore.Devices) (_ *workercore.Devices, skip bool, err error) {
		skip = true
		// Update groups.
		if !kubemeta.DeepEqual(aDevs.Spec.Groups, eDevs.Spec.Groups) {
			groups, groupsChanged := alignDeviceGroups(aDevs.Spec.Groups, eGroups, d.manufacturers)
			if groupsChanged {
				skip = false
			}
			aDevs.Spec.Groups = groups
		}
		// Update selector labels (they appear once NFD has applied the feature labels).
		if len(devsLabels) > 0 && aDevs.Labels == nil {
			aDevs.Labels = make(map[string]string)
		}
		for k, v := range devsLabels {
			if aDevs.Labels[k] != v {
				aDevs.Labels[k] = v
				skip = false
			}
		}
		// Update owner reference.
		if !kubemeta.IsControlledBy(aDevs, nd) {
			controlOnNodeWithoutBlock(aDevs, nd)
			skip = false
		}
		return aDevs, skip, err
	}

	_, err = kubeclientset.Create(ctx, devsCli, eDevs,
		kubeclientset.WithUpdateIfExisted(devsAlginFn))
	if err != nil {
		return fmt.Errorf("failed to sync Devices object for node %s: %w", ndName, err)
	}

	return nil
}

// controlOnNodeWithoutBlock sets the node as the object's sole controller owner, retiring any
// existing controller reference of another kind first. ControlOnWithoutBlock only replaces a
// reference matched by api-version and kind, so without this an object carried over from a release
// that owned it by a different kind (e.g. Devices owned by the NodeFeature before v0.6) would gain
// a SECOND controller reference — which the API server rejects, freezing the object at its
// pre-upgrade content on every sync pass.
func controlOnNodeWithoutBlock(obj kubemeta.MetaObject, nd *core.Node) {
	if ctrlRef := kubemeta.GetControllerOfNoCopy(obj); ctrlRef != nil && ctrlRef.UID != nd.UID {
		kubemeta.ControlOff(obj, nil, schema.FromAPIVersionAndKind(ctrlRef.APIVersion, ctrlRef.Kind))
	}
	kubemeta.ControlOnWithoutBlock(obj, nd, core.SchemeGroupVersion.WithKind("Node"))
}

// alignDeviceGroups reconciles the existing device groups (aGroups) against the freshly detected
// ones (eGroups) for one manufacturer pass: a group present in both is kept but refreshed with the
// newly detected content (its accelerators' slicing capability included), a group only in eGroups is
// added, and a group only in aGroups is dropped unless its manufacturer is outside the allowed set
// (in which case it is left untouched, since this pass has no fresh data for it). changed reports
// whether the returned list differs from aGroups.
//
// Note: the caller's re-detect trigger only watches the {manufacturer, id, unhealthy} device-key set
// (see Start), so toggling a card's partitioning mode without changing that set never fires a
// re-detect; the stale capability then only clears on the next re-detect (e.g. a DaemonSet restart).
func alignDeviceGroups(
	aGroups, eGroups device.DevicesGroupList, allowedManufacturers sets.Set[string],
) (groups device.DevicesGroupList, changed bool) {
	// Index the existing groups by multi-keys: manufacturer and id.
	devGrpIndex := make(map[_DeviceGroupKey]*_DeviceGroupValue)
	for i := range aGroups {
		k := _DeviceGroupKey{aGroups[i].Manufacturer, aGroups[i].ID}
		v := &_DeviceGroupValue{
			Group: aGroups[i],
			// If the manufacturer is not in the allowed list, keep it.
			Remove: allowedManufacturers.Has(aGroups[i].Manufacturer),
		}
		devGrpIndex[k] = v
	}
	// Iterate the expected groups, if the group is in the index, update it, otherwise, add it.
	groups = make(device.DevicesGroupList, 0, len(eGroups))
	for i := range eGroups {
		k := _DeviceGroupKey{eGroups[i].Manufacturer, eGroups[i].ID}
		if v, ok := devGrpIndex[k]; ok {
			// The group is in the index, keep it and update it if needed.
			v.Remove = false
			if !kubemeta.DeepEqual(v.Group, eGroups[i]) {
				v.Group = eGroups[i]
				changed = true
			}
			continue
		}
		// The group is not in the index, add it.
		groups = append(groups, eGroups[i])
		changed = true
	}
	// Iterate the index, if the group is marked to remove, remove it.
	for i := range aGroups {
		k := _DeviceGroupKey{aGroups[i].Manufacturer, aGroups[i].ID}
		v := devGrpIndex[k]
		if v.Remove {
			changed = true
			continue
		}
		groups = append(groups, v.Group)
	}
	return groups, changed
}

// acceleratableDevicesSelectorLabels builds the selector labels stamped on a node's Devices object
// (os/arch plus each acceleratable feature key, minus the managed mark, the .count sizing pin, and the
// general(CPU) key) that let the worker locate the node's Devices by a single List. The acceleratable
// feature keys are taken from the feature labels being published this pass (publishedFeatureLabels,
// i.e. the NodeFeature's spec labels), NOT read back off the node: NFD merges those labels onto the
// node only afterwards, so a freshly onboarded node would otherwise yield no keys until an unrelated
// resync. os/arch are stable node labels present from registration. gpustack.ai/managed and the real
// general(CPU) key are synced separately by NodeDevicesReconciler (this NodeFeature carries no CPU
// labels, so ExtractGeneralNodeKey here can only yield the "generic" sentinel — the worker, which sees
// the node's CPU labels, owns the CPU key on the Devices). The per-node .count pin sizes the
// ResourceFlavor's node batch; it is not a Devices selector key, so it is dropped here.
func acceleratableDevicesSelectorLabels(node *core.Node, publishedFeatureLabels map[string]string) map[string]string {
	src := &core.Node{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{
		core.LabelOSStable:   node.Labels[core.LabelOSStable],
		core.LabelArchStable: node.Labels[core.LabelArchStable],
	}}}
	maps.Copy(src.Labels, publishedFeatureLabels)

	// The accelerator selector labels are the union, over the node's accelerated flavors, of each
	// flavor's node labels minus the worker-owned keys (the managed mark and the general(CPU) key —
	// the "generic" one derivable here is a wrong guess the worker later replaces) and the .count
	// sizing pin (a batch hint, not a selector).
	accelerated := slicex.Filter(nodefeature.ExtractNodeFlavors(src), func(nf nodefeature.NodeFlavor) bool {
		return nf.Acceleratable
	})
	return mapx.Merge(slicex.Transform(accelerated, func(nf nodefeature.NodeFlavor) map[string]string {
		return mapx.Filter(nf.NodeLabels, func(k, _ string) bool {
			return k != systemname.ManagedLabelKey &&
				!strings.HasSuffix(k, ".count") &&
				!strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix)
		})
	})...)
}
