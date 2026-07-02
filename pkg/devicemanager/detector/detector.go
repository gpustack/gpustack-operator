package detector

import (
	"context"
	"errors"
	"fmt"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"gpustack.ai/gpustack/pkg/utils/osx"
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
	monitorHistory          *datax.RingBuffer[device.MetricsGroupList]
	detectedManufacturersCh chan<- sets.Set[string]
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
		monitorHistory:          datax.NewRingBuffer[device.MetricsGroupList](c.MonitorHistory),
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

		// Monitor devices and update the monitor history.
		_ = waitx.UntilContextCancel(ctx, d.monitorPeriod, true, func(ctx context.Context) error {
			logger.V(3).Info("monitoring")

			metricsGrpList := d.MonitorAccelerator(ctx)
			if len(metricsGrpList) != 0 {
				d.monitorHistory.Push(metricsGrpList)
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
			kubemeta.ControlOnWithoutBlock(aNf, nd, core.SchemeGroupVersion.WithKind("Node"))
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
	kubemeta.ControlOnWithoutBlock(eDevs, aNf, nfd.SchemeGroupVersion.WithKind("NodeFeature"))
	devsAlginFn := func(aDevs *workercore.Devices) (_ *workercore.Devices, skip bool, err error) {
		skip = true
		// Update groups.
		if !kubemeta.DeepEqual(aDevs.Spec.Groups, eDevs.Spec.Groups) {
			// Index the existing groups by multi-keys: manufacturer and id.
			aGroups := aDevs.Spec.Groups
			devGrpIndex := make(map[_DeviceGroupKey]*_DeviceGroupValue)
			for i := range aGroups {
				k := _DeviceGroupKey{aGroups[i].Manufacturer, aGroups[i].ID}
				v := &_DeviceGroupValue{
					Group: aGroups[i],
					// If the manufacturer is not in the allowed list, keep it.
					Remove: d.manufacturers.Has(aGroups[i].Manufacturer),
				}
				devGrpIndex[k] = v
			}
			// Iterate the expected groups, if the group is in the index, update it, otherwise, add it.
			groups := make([]device.DevicesGroup, 0, len(eGroups))
			for i := range eGroups {
				k := _DeviceGroupKey{eGroups[i].Manufacturer, eGroups[i].ID}
				if v, ok := devGrpIndex[k]; ok {
					// The group is in the index, keep it and update it if needed.
					v.Remove = false
					if !kubemeta.DeepEqual(v.Group, eGroups[i]) {
						v.Group = eGroups[i]
						skip = false
					}
					continue
				}
				// The group is not in the index, add it.
				groups = append(groups, eGroups[i])
				skip = false
			}
			// Iterate the index, if the group is marked to remove, remove it.
			for i := range aGroups {
				k := _DeviceGroupKey{aGroups[i].Manufacturer, aGroups[i].ID}
				if devGrpIndex[k].Remove {
					skip = false
					continue
				}
				groups = append(groups, aGroups[i])
			}
			// Update groups.
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
		if !kubemeta.IsControlledBy(aDevs, aNf) {
			kubemeta.ControlOnWithoutBlock(aDevs, aNf, nfd.SchemeGroupVersion.WithKind("NodeFeature"))
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

// acceleratableDevicesSelectorLabels builds the selector labels stamped on a node's Devices object
// (os/arch plus each acceleratable feature key, minus the managed mark) that let the worker locate
// the node's Devices by a single List. The acceleratable feature keys are taken from the feature
// labels being published this pass (publishedFeatureLabels, i.e. the NodeFeature's spec labels), NOT
// read back off the node: NFD merges those labels onto the node only afterwards, so a freshly
// onboarded node would otherwise yield no keys until an unrelated resync. os/arch are stable node
// labels present from registration. gpustack.ai/managed is synced separately by NodeDevicesReconciler.
func acceleratableDevicesSelectorLabels(node *core.Node, publishedFeatureLabels map[string]string) map[string]string {
	src := &core.Node{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{
		core.LabelOSStable:   node.Labels[core.LabelOSStable],
		core.LabelArchStable: node.Labels[core.LabelArchStable],
	}}}
	for k, v := range publishedFeatureLabels {
		src.Labels[k] = v
	}
	out := map[string]string{}
	for _, nf := range nodefeature.ExtractNodeFlavors(src) {
		if !nf.Acceleratable {
			continue
		}
		for k, v := range nf.NodeLabels {
			if k == systemname.ManagedLabelKey {
				continue
			}
			out[k] = v
		}
	}
	return out
}
