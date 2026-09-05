package detector

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/kuberess"
	"gpustack.ai/gpustack/pkg/devicemanager/procattr"
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

	// lastDetected is the newest groups each manufacturer's detect pass reported, kept so a pass that
	// FAILS can report what was last detected instead of nothing. Only the detect loop reads or
	// writes it, so it carries no lock of its own.
	lastDetected map[string]device.DevicesGroupList

	// reportedInterfaces is what the last successful report enumerated, so the monitor loop can
	// tell whether the machine's interfaces have moved since. The comparison strips first-seen
	// times from both sides itself, so this field may hold a merged record or an unmerged one.
	//
	// Only the detect loop and its monitor closure touch it, and they run in the same goroutine
	// one after the other, so it carries no lock of its own for the same reason lastDetected does
	// not.
	reportedInterfaces []workercore.DeviceInterface

	// reportedInterfacesKnown says whether reportedInterfaces is a baseline, which the slice itself
	// cannot express: a host that enumerated nothing gate-relevant and a host that has never
	// enumerated at all both hold the empty list. Only the second must take another detect round,
	// and it stays in that state after a first pass whose enumeration failed.
	reportedInterfacesKnown bool

	// podLister and procResolver are the two node-local reads the slice section needs: which Pods
	// this node runs, and which of them a host process belongs to. They are fields rather than
	// direct calls so the pass tests against a fake Pod list and a fake /proc tree.
	podLister    func(ctx context.Context) ([]core.Pod, error)
	procResolver *procattr.Resolver
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
	// PeriodSeconds is the monitor period in effect when the sample was stored,
	// so consumers can scale their staleness bound to the configured cadence.
	PeriodSeconds int64 `json:"periodSeconds"`
	// Groups is the latest accelerator metrics sample,
	// empty before the first monitor tick.
	Groups device.MetricsGroupList `json:"groups"`
	// Slices is what each Instance holds of the accelerators the groups above describe as wholes,
	// absent when the pass could not measure — which is not the same claim as an empty section,
	// and is why the field is a pointer. Its SchemaVersion is what lets a consumer tell a
	// producer that predates slice reporting from hardware that cannot be measured.
	Slices *MonitorSliceSection `json:"slices,omitempty"`
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

	d := &Detector{
		noPCICheck:              noPCICheck,
		manufacturers:           manufacturers,
		noFastFailed:            c.NoFastFailed,
		detectors:               detectors,
		monitorPeriod:           c.MonitorPeriod,
		detectedManufacturersCh: c.DetectedManufacturersCh,
	}
	d.podLister = d.listNodePods
	// The device manager runs with hostPID, so its own /proc lists the host's processes — the
	// namespace the vendor libraries report their pids in.
	d.procResolver = procattr.New(os.DirFS("/proc"))
	return d, nil
}

// DetectAccelerator detects the accelerators on the node and returns a list of device groups once,
// naming the manufacturers whose pass could not measure them.
//
// A manufacturer whose pass FAILED is reported as it was last detected rather than left out. An error
// from a detector says that pass could not measure and nothing about the hardware — a driver that will
// not load or a bus holding no card is answered with an empty list and no error — so leaving the
// manufacturer out would tell every consumer downstream that its accelerators are gone: the allocator
// stops, its device-plugin sockets retire, and the node loses that family's capacity keys. There is
// nothing to report on a first pass, so a manufacturer that has never answered is still absent.
//
// Reporting the last result is a substitution, which is why the manufacturers it was made for are
// named: a caller that keeps detecting is meant to come back rather than settle on it. What it reports
// is a copy, so the held result is never the one a consumer holds.
//
// This is the detect loop's own pass and holds the state that loop carries between rounds, so it is not
// safe to call while Start is running.
//
// A pass that ran and found nothing is the opposite claim, and the only one that undetects a
// manufacturer. Whatever was held for it is dropped there, so a later failure cannot resurrect a card
// that was pulled.
func (d *Detector) DetectAccelerator(
	_ context.Context,
) (grpListMerged device.DevicesGroupList, unmeasured sets.Set[string]) {
	unmeasured = sets.New[string]()

	for i := range d.detectors {
		manufacturer := d.detectors[i].Name()
		logger := logger.V(2).WithValues("manufacturer", manufacturer)

		grpList, err := d.detectors[i].DetectAccelerator(d.noPCICheck)
		if err != nil {
			logger.Error(err, "detect accelerators")
			unmeasured.Insert(manufacturer)
			if lastGrpList := d.lastDetected[manufacturer]; len(lastGrpList) != 0 {
				logger.Info("reporting the accelerators last detected")
				grpListMerged = append(grpListMerged, cloneDeviceGroups(lastGrpList)...)
			}
			continue
		}
		if len(grpList) == 0 {
			delete(d.lastDetected, manufacturer)
			continue
		}

		if d.lastDetected == nil {
			d.lastDetected = make(map[string]device.DevicesGroupList, len(d.detectors))
		}
		d.lastDetected[manufacturer] = grpList

		grpListMerged = append(grpListMerged, grpList...)
		logger.Info("detected accelerators")
	}

	return grpListMerged, unmeasured
}

// MonitorAccelerator monitors the accelerators on the node and returns a list of metrics groups once,
// naming the manufacturers whose pass could not measure them.
//
// Those are named rather than reported as they were last measured: a sample is only worth what its
// timestamp claims, so a manufacturer that could not be measured is absent from the result. Naming it
// is what keeps its absence from reading as a device set that shrank.
func (d *Detector) MonitorAccelerator(
	_ context.Context,
) (grpListMerged device.MetricsGroupList, unmeasured sets.Set[string]) {
	unmeasured = sets.New[string]()

	for i := range d.detectors {
		manufacturer := d.detectors[i].Name()
		logger := logger.V(2).WithValues("manufacturer", manufacturer)

		grpList, err := d.detectors[i].MonitorAccelerator(d.noPCICheck)
		if err != nil {
			logger.Error(err, "monitor accelerators")
			unmeasured.Insert(manufacturer)
			continue
		}
		if len(grpList) == 0 {
			continue
		}

		grpListMerged = append(grpListMerged, grpList...)
		logger.Info("monitored accelerators")
	}

	return grpListMerged, unmeasured
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

// detectEveryNMonitorRounds is how many monitor rounds pass before a detect is forced regardless of
// what the monitor loop observed.
//
// It is a floor under an edge-triggered loop, not a poll: the two edges that loop watches cannot see
// a fact only detection reads, and the fabric domain is one such fact. Counted in rounds rather than
// wall time so the floor tracks whatever monitorPeriod the operator chose.
//
// Forty rounds is ten minutes at the default fifteen-second period. The facts it refreshes change
// only when someone re-cables a machine or upgrades a driver, so minutes of staleness cost a
// scheduler nothing, while a detect pass on every round would re-enumerate every accelerator's
// endpoints forty times over for an answer that almost never differs.
const detectEveryNMonitorRounds = 40

// Start starts the detector to detect and monitor the devices periodically until the context is canceled.
func (d *Detector) Start(ctx context.Context) error {
	holdUntilFirstDetected := !d.noFastFailed && d.manufacturers.Len() == 1

	return waitx.UntilContextCancel(ctx, d.monitorPeriod, true, func(ctx context.Context) error {
		logger.V(2).Info("detecting")

		devicesGrpList, unmeasuredByDetect := d.DetectAccelerator(ctx)

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

		// A device manager asked for exactly one manufacturer runs on a node its DaemonSet was
		// scheduled to by that manufacturer's PCI vendor label, so a first round that detected
		// nothing is a node whose hardware is there and whose software has not answered yet. Hold
		// the round back rather than publish and report a node with no accelerators, and say so
		// again on every round for as long as it lasts: the driver appearing is what ends it.
		//
		// Returning an error ends this round and no more. The poll this runs under keeps polling on
		// one, by contract and by the other three callers' design, and remembers it as the reason
		// should the process go down still waiting. --no-fast-failed is what publishes and reports
		// an empty first result instead of holding it.
		if holdUntilFirstDetected {
			if manufacturers.Len() == 0 {
				err := fmt.Errorf("manufacturer %s is expected but not detected", d.manufacturers.UnsortedList()[0])
				logger.Error(err, "not reporting a node with no accelerators, detecting again")
				return err
			}
			holdUntilFirstDetected = false
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
		//
		// rounds counts this monitor stretch, and is reset by construction: leaving the loop means
		// detecting again, so the floor below measures time since the last detect rather than since
		// the process started.
		rounds := 0
		_ = waitx.UntilContextCancel(ctx, d.monitorPeriod, true, func(ctx context.Context) error {
			logger.V(3).Info("monitoring")

			metricsGrpList, unmeasuredByMonitor := d.MonitorAccelerator(ctx)
			if len(metricsGrpList) != 0 {
				// The per-process pass runs beside the card pass and is stamped with the same
				// store time. It is a second enumeration of the same devices, so the card and
				// process readings are milliseconds apart rather than interleaved — which costs
				// a consumer nothing, since neither figure is derived from the other, and buys
				// that the raw rows cannot reach the snapshot by construction.
				d.monitorSnapshot.Store(&MonitorSnapshot{
					Timestamp: time.Now(),
					// Round up: truncating a sub-second remainder would understate the period
					// and make consumers drop healthy samples as stale.
					PeriodSeconds: int64((d.monitorPeriod + time.Second - 1) / time.Second),
					Groups:        metricsGrpList,
					Slices:        d.collectSlices(ctx, metricsGrpList),
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

			// What this round reported for a manufacturer whose detect pass failed — the accelerators
			// it last detected, or nothing when it has never detected any — stands in for an answer
			// rather than being one, so ask again instead of settling on it. Monitoring on it would
			// log the failure once and then look healthy for as long as it lasts.
			if unmeasuredByDetect.Len() != 0 {
				logger.Info("a detect pass could not measure, going to detect again",
					"manufacturers", unmeasuredByDetect.UnsortedList())
				return waitx.ErrCanceled
			}

			// Compare the current devices with the previous devices,
			// if they are the same, continue to monitor,
			// otherwise, detect again.
			//
			// A manufacturer this pass could not measure is left out of the comparison entirely: it
			// has no devices here, and reading that as devices that went away would take the loop
			// round again on no evidence. A manufacturer that answered and reported nothing is a
			// device set that DID shrink, and still takes it round.
			if !measuredDeviceKeys(deviceKeys, unmeasuredByMonitor).Equal(curDeviceKeys) {
				logger.Info("changed, going to detect again")
				return waitx.ErrCanceled
			}

			// The interface record is compared here too, on the MONITOR cadence, because
			// _DeviceKey carries nothing about the network. A link going down changes no
			// accelerator key, so this loop would keep spinning on unchanged keys while the
			// recorded inventory went stale — and the label gate that withholds `rdma.capable` on
			// a broken link could then only ever fire when an accelerator happened to change at
			// the same moment. A gate that needs an unrelated event to fire is not a gate.
			//
			// What is compared is the subset that can affect that gate, not the whole record: see
			// interfacesChanged. Taking the round on any change at all made every Pod start rerun
			// the driver detection below, because a Pod brings a `veth` with it.
			detected, detectedErr := DetectInterfaces()
			if interfacesChanged(d.reportedInterfaces, d.reportedInterfacesKnown, detected, detectedErr) {
				logger.Info("network interfaces changed, going to detect again")
				return waitx.ErrCanceled
			}

			// Everything above is edge-triggered, and the two edges it watches are the only ones a
			// monitor pass can see: an accelerator key, and the network record. Neither carries the
			// scale-up fabric -- a node joining or leaving a super pod changes no accelerator id and
			// no link -- and the monitor result has no fabric in it to compare, so a purely
			// event-driven loop publishes `fabric.domain` on the round that detected it and then
			// never revisits it. The withholding rule that takes the label back when a node leaves
			// its domain would fire only if an accelerator happened to change at the same moment,
			// which is the same defect the interface comparison above exists to fix.
			//
			// So the loop is given a floor: detect again on a period regardless of what was seen.
			// This is the level-based half of an otherwise edge-triggered loop, and it covers every
			// fact only a detect pass reads -- driver version and product shape as much as fabric --
			// rather than just the one that prompted it. Counted in monitor rounds rather than held
			// as a deadline so that it scales with monitorPeriod instead of drifting from it.
			rounds++
			if rounds >= detectEveryNMonitorRounds {
				logger.V(1).Info("detect period elapsed, going to detect again")
				return waitx.ErrCanceled
			}
			return nil
		})

		return nil
	})
}

// nodeWideDeviceGroups returns the accelerators of the WHOLE NODE: the ones this pass detected, plus
// the ones the stored record holds for manufacturers this pass does not own.
//
// mine is this detect pass's manufacturer set, and it is what decides which stored groups to take:
// a stored group for a manufacturer this pass DOES own is this pass's own business and its fresh
// reading wins, while a group for any other manufacturer belongs to a different DaemonSet and is the
// only account of it available here. Taking a stored group for an owned manufacturer would resurrect
// hardware this pass just found to be gone.
//
// The result is a fresh slice whenever it differs from own, so the extra entries cannot be written
// into own's spare capacity — the caller publishes own as this node's detected inventory.
func nodeWideDeviceGroups(
	own, stored device.DevicesGroupList, mine sets.Set[string],
) device.DevicesGroupList {
	var others device.DevicesGroupList
	for i := range stored {
		if !mine.Has(stored[i].Manufacturer) {
			others = append(others, stored[i])
		}
	}
	if len(others) == 0 {
		return own
	}
	return append(cloneDeviceGroups(own), others...)
}

// cloneDeviceGroups returns groups that share nothing with the ones given. The held result of a
// manufacturer whose pass failed is reported again on every round that keeps failing, so handing it out
// as it is would let a consumer that reaches into what it was given change what the next round reports.
func cloneDeviceGroups(groups device.DevicesGroupList) device.DevicesGroupList {
	out := make(device.DevicesGroupList, len(groups))
	for i := range groups {
		groups[i].DeepCopyInto(&out[i])
	}
	return out
}

// measuredDeviceKeys returns the detected device keys of every manufacturer but those a monitor pass
// could not measure, which is what that pass's result can be compared against.
func measuredDeviceKeys(keys sets.Set[_DeviceKey], unmeasured sets.Set[string]) sets.Set[_DeviceKey] {
	if unmeasured.Len() == 0 {
		return keys
	}

	measured := sets.New[_DeviceKey]()
	for k := range keys {
		if !unmeasured.Has(k.Manufacturer) {
			measured.Insert(k)
		}
	}
	return measured
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

	// The network interfaces are read here rather than inside the per-manufacturer loop, because a
	// NIC belongs to the machine and not to a manufacturer's accelerators. It happens before the
	// NodeFeature below because the RDMA feature labels are derived from it.
	//
	// A failure to enumerate is NOT an empty inventory. It is reported here and then leaves both
	// the previously recorded list and the previously published labels alone, because an empty
	// list reads as "this worker has no interfaces" — a claim a failed read cannot support. On a
	// first pass there is nothing to preserve, so the object is created without interfaces and
	// this log line is the only account of why; it is logged at Error precisely because that is
	// the one level no verbosity setting can hide.
	eInterfaces, eInterfacesErr := DetectInterfaces()
	if eInterfacesErr != nil {
		logger.Error(eInterfacesErr, "could not enumerate network interfaces",
			"node", ndName,
			"consequence", "keeping whatever inventory and labels were recorded before this pass")
	}
	// Kept for the monitor loop to compare against. The comparison strips first-seen times itself,
	// so taking the snapshot before the merge below is not what makes it correct — but only a pass
	// that actually enumerated may replace it: overwriting on a failed read would hand the monitor
	// loop an empty baseline and make the next successful pass look like a change.
	if eInterfacesErr == nil {
		d.reportedInterfaces = cloneInterfaces(eInterfaces)
		d.reportedInterfacesKnown = true
	}

	// One instant for the whole pass, and stamped here as well as in the align function because
	// the two write paths need it separately: an object created by a first pass would otherwise
	// store a link failure with no first-seen time, and every later pass would carry that absence
	// forward rather than fill it in.
	now := meta.Now()
	carryLinkFirstSeen(nil, eInterfaces, now)

	// The distance label is a claim about the WHOLE NODE, so it is reduced over every accelerator the
	// node has rather than over this pass's alone.
	//
	// There is one device-manager DaemonSet per manufacturer and one NodeFeature per node, so a
	// mixed-vendor node has several writers pointing at the same object. Computing the distance from
	// this pass's groups only made the key last-writer-wins between values that are each correct for
	// their own writer and neither of which is the closest distance the node has — and since the
	// stale-key removal below deletes an RDMA key this pass did not report, the two writers also
	// overwrote each other on every pass, forever. The other manufacturers' groups are taken from the
	// Devices object all of them already share.
	//
	// A read that fails degrades to this pass's own groups instead of failing the pass, because the
	// reduction is a MINIMUM: over a subset it can only come out equal or FURTHER, so the published
	// value stays conservative and the next pass converges. Underclaiming proximity is the safe
	// direction, the same asymmetry device.BusDistance is built on — an overclaim is what nothing
	// downstream can catch.
	devsCli := lpCli.WorkerV1alpha1().Devices()
	nodeGroups := eGroups
	aDevs, derr := devsCli.Get(ctx, ndName, meta.GetOptions{ResourceVersion: "0"})
	syncNodeWide := nodeWideReadSynced(derr)
	switch {
	case derr == nil:
		nodeGroups = nodeWideDeviceGroups(eGroups, aDevs.Spec.Groups, d.manufacturers)
	case !syncNodeWide:
		logger.V(1).Info("could not read the node's Devices for the node-wide reductions",
			"node", ndName, "error", derr.Error(),
			"consequence", "the distance is reduced over this pass's accelerators only, "+
				"and the fabric labels are left as previously published")
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
			nfs.Labels = nodeFeatureLabels(eGroups, nodeGroups, eInterfaces,
				eInterfacesErr == nil, syncNodeWide)
			return *nfs
		}(),
	}
	kubemeta.ControlOnWithoutBlock(eNf, nd, core.SchemeGroupVersion.WithKind("Node"))
	nfAlignment := nodeFeatureAlignment{
		expected:       eNf,
		node:           nd,
		syncInterfaces: eInterfacesErr == nil,
		syncFabric:     syncNodeWide,
	}

	aNf, err := kubeclientset.Create(ctx, nfCli, eNf,
		kubeclientset.WithUpdateIfExisted(nfAlignment.apply))
	if err != nil {
		return fmt.Errorf("failed to sync NodeFeature object for node %s: %w", ndName, err)
	}

	// Devices.

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
			Groups:     eGroups,
			Interfaces: eInterfaces,
		},
	}
	// Owned by the Node, not by the NodeFeature above: Devices is cluster-scoped, and the garbage
	// collector resolves a cluster-scoped dependent's owner in the EMPTY namespace. A namespaced
	// owner is therefore unresolvable — it yields an OwnerRefInvalidNamespace warning on every
	// sweep and the object is never collected, so a Devices outlives the node it describes and
	// keeps reporting that node's accelerators to every consumer that lists them.
	kubemeta.ControlOnWithoutBlock(eDevs, nd, core.SchemeGroupVersion.WithKind("Node"))
	devsAlignment := devicesAlignment{
		expected:       eDevs,
		node:           nd,
		labels:         devsLabels,
		manufacturers:  d.manufacturers,
		syncInterfaces: eInterfacesErr == nil,
		now:            now,
	}

	_, err = kubeclientset.Create(ctx, devsCli, eDevs,
		kubeclientset.WithUpdateIfExisted(devsAlignment.apply))
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

// nodeWideReadSynced says whether a Devices read can support WITHHOLDING a node-wide label.
//
// NotFound is not a failure but the node's first pass: nothing is stored yet, so this pass's own
// groups already are everything there is to see — the same reading nodeWideDeviceGroups gives an
// empty stored record, and the label converges once the other DaemonSet has written. Any other error
// means this pass cannot see the rest of the node, and a label withheld on that basis would be
// REMOVED from the object on the strength of a read that never happened.
//
// It is a function so that reading can be checked without a cluster: the caller needs a client, and
// the difference between the two error kinds is invisible in the object either produces.
func nodeWideReadSynced(err error) bool {
	return err == nil || kerrors.IsNotFound(err)
}

// nodeFeatureLabels reduces one detect pass into the feature labels it publishes.
//
// It is a function rather than the inline closure it replaced because WHICH GROUP SET each label is
// reduced over is an acceptance criterion, and a criterion needs a seam to be checked at — the same
// reason nodeFeatureAlignment below is a type. Reduced over the wrong set every label here still
// computes, the object still looks healthy, and only a mixed-vendor node ever shows the difference.
//
// ownGroups are this pass's own accelerators; nodeGroups are those plus the other manufacturers'.
// syncInterfaces and syncNodeWide report whether the read behind each node-wide set actually
// happened, and a label withheld here is REMOVED from the object by nodeFeatureAlignment.
func nodeFeatureLabels(
	ownGroups, nodeGroups device.DevicesGroupList, interfaces []device.Interface,
	syncInterfaces, syncNodeWide bool,
) map[string]string {
	// Accelerator labels describe what this pass detected, and each manufacturer's pass contributes
	// its own keys, so they are reduced over this pass's groups alone.
	labels := nodefeature.ConstructAcceleratableNodeLabels(ownGroups)

	// The fabric domain is reduced over the node-wide set for the same reason the RDMA distance is:
	// the key claims something about EVERY accelerator the node has, and one device-manager
	// DaemonSet per manufacturer writes this one object. Over this pass's groups alone a
	// mixed-vendor node publishes the Ascend pass's domain while its NVIDIA accelerators are in no
	// such domain at all, so the key promises co-location the node does not offer — and the
	// stale-key removal would then have the two writers deleting each other's key on every pass,
	// forever.
	//
	// Only when that read succeeded. Withholding here REMOVES, and a read that did not happen
	// cannot support removing a domain that is still true. Degrading to this pass's groups the way
	// the distance does would be unsafe in the other direction: the distance is a MINIMUM, so a
	// subset can only underclaim, while a domain reduced over a subset can find one where the whole
	// node has none, which OVERCLAIMS.
	if syncNodeWide {
		maps.Copy(labels, nodefeature.ConstructFabricNodeLabels(nodeGroups))
	}

	// Only when this pass enumerated. A failed read must leave whatever was published before it
	// standing: emitting nothing here, combined with the stale-key removal, would withhold the RDMA
	// labels on the strength of a read that never happened — and withholding one of them stops a
	// flavor selecting this node.
	if syncInterfaces {
		maps.Copy(labels, nodefeature.ConstructRDMANodeLabels(nodeGroups, interfaces))
	}

	return labels
}

// nodeFeatureAlignment reconciles an existing NodeFeature towards what this detect pass produced.
//
// It is a type with a method rather than the closure it replaced for the same reason
// devicesAlignment is: the stale-key removal below is what makes a withheld label take effect, so
// it is an acceptance criterion, and a criterion needs a seam to be checked at.
type nodeFeatureAlignment struct {
	// expected is the object this pass would have created had none existed.
	expected *nfd.NodeFeature
	// node owns the object.
	node *core.Node
	// syncInterfaces is false when this pass could not enumerate the network interfaces. The
	// previously published RDMA labels are then left untouched — neither refreshed nor removed —
	// because withholding one of them stops a flavor selecting this node, and a read that did not
	// happen cannot support that.
	syncInterfaces bool
	// syncFabric is false when this pass could not read the node's other manufacturers' groups. The
	// previously published fabric labels are then left untouched, for the same reason: the domain
	// they name may still be true, and this pass cannot see enough of the node to say otherwise.
	syncFabric bool
}

func (a nodeFeatureAlignment) apply(aNf *nfd.NodeFeature) (_ *nfd.NodeFeature, skip bool, err error) {
	skip = true
	if aNf.Labels == nil {
		aNf.Labels = make(map[string]string)
	}
	if aNf.Spec.Labels == nil {
		aNf.Spec.Labels = make(map[string]string)
	}
	// Update labels if not contained.
	for k, v := range a.expected.Labels {
		if aNf.Labels[k] != v {
			aNf.Labels[k] = v
			skip = false
		}
	}
	// Update spec labels if not contained.
	for k, v := range a.expected.Spec.Labels {
		if aNf.Spec.Labels[k] != v {
			aNf.Spec.Labels[k] = v
			skip = false
		}
	}
	// Remove the keys this pass did NOT report, under the prefixes it speaks for and only those.
	//
	// Everything above adds and overwrites; nothing deletes. For these two sets that is not a
	// tolerable gap but the difference between having a gate and not: withholding a key is HOW the
	// node stops being selected, and a key that is never removed is never withheld. It would stay
	// true for as long as the object lives, with the object's own inventory contradicting it the
	// whole time — an unusable RDMA link still advertised as capable, a super pod still named after
	// the node left it.
	//
	// Each prefix is gated on ITS OWN read, because they fail independently: enumerating the
	// network interfaces and reading the node's other manufacturers' groups are different calls,
	// and one failing says nothing about the other. Scoped to these prefixes so neither can touch
	// a key this pass does not own — the accelerator keys beside them keep their existing add-only
	// behavior, which is not this change to fix.
	if a.syncInterfaces &&
		removeUnreported(aNf.Spec.Labels, a.expected.Spec.Labels, nodefeature.RDMAFeatureLabelPrefix) {
		skip = false
	}
	if a.syncFabric &&
		removeUnreported(aNf.Spec.Labels, a.expected.Spec.Labels, nodefeature.FabricFeatureLabelPrefix) {
		skip = false
	}
	// Update owner reference.
	if !kubemeta.IsControlledBy(aNf, a.node) {
		controlOnNodeWithoutBlock(aNf, a.node)
		skip = false
	}
	return aNf, skip, err
}

// removeUnreported deletes from stored every key under prefix that reported does not carry, and says
// whether it deleted anything. Keys outside the prefix are never touched, so one caller's withheld
// key cannot take another's label with it.
func removeUnreported(stored, reported map[string]string, prefix string) (removed bool) {
	for k := range stored {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, ok := reported[k]; !ok {
			delete(stored, k)
			removed = true
		}
	}
	return removed
}

// devicesAlignment reconciles an existing Devices object towards what this detect pass produced,
// reporting whether anything actually changed so an unchanged pass issues no API write.
//
// It is a type with a method rather than the closure it replaced so that a test can drive the
// decision directly. That is not a stylistic preference: the branch comparing the interface
// inventory is the single most likely way this feature ships broken while looking healthy —
// without it the field is computed on every pass and written never, and nothing about the stored
// object looks wrong — so it is an acceptance criterion, and a criterion needs a seam to be
// checked at.
type devicesAlignment struct {
	// expected is the object this pass would have created had none existed.
	expected *workercore.Devices
	// node owns the object. A cluster-scoped dependent of a cluster-scoped owner; see where
	// expected is built for why it cannot be the NodeFeature.
	node *core.Node
	// labels are the accelerator flavors' selector labels to stamp.
	labels map[string]string
	// manufacturers bounds which device groups this pass is allowed to speak for.
	manufacturers sets.Set[string]
	// syncInterfaces is false when this pass could not enumerate the network interfaces at all.
	// The previously recorded inventory is then left untouched rather than replaced with an empty
	// one: an empty list reads as "this worker has no interfaces", which a failed read cannot
	// claim. A pass that enumerated successfully and found none is a different case, and does
	// write the empty list.
	syncInterfaces bool
	// now is the instant this detect pass observed, used as the first-seen time of a link failure
	// this pass is the first to see. It is one value for the whole pass rather than a clock read
	// per interface, so two interfaces that failed together record the same instant.
	now meta.Time
}

func (a devicesAlignment) apply(actual *workercore.Devices) (*workercore.Devices, bool, error) {
	skip := true

	// Update groups.
	if !kubemeta.DeepEqual(actual.Spec.Groups, a.expected.Spec.Groups) {
		groups, groupsChanged := alignDeviceGroups(actual.Spec.Groups, a.expected.Spec.Groups, a.manufacturers)
		if groupsChanged {
			skip = false
		}
		actual.Spec.Groups = groups
	}

	// Update network interfaces, compared independently of the groups above. The two inventories
	// change for unrelated reasons, so folding this into the groups branch would write a changed
	// interface list only when an accelerator happened to change too.
	if a.syncInterfaces {
		// Merge the failing links' first-seen times from the stored inventory BEFORE comparing.
		// This pass observed the failures but not when they started, and taking the current
		// instant here would make the comparison below never match — an API write on every pass,
		// forever, with correct data in the object throughout.
		carryLinkFirstSeen(actual.Spec.Interfaces, a.expected.Spec.Interfaces, a.now)
		if !kubemeta.DeepEqual(actual.Spec.Interfaces, a.expected.Spec.Interfaces) {
			actual.Spec.Interfaces = a.expected.Spec.Interfaces
			skip = false
		}
	}

	// Update selector labels (they appear once NFD has applied the feature labels).
	if len(a.labels) > 0 && actual.Labels == nil {
		actual.Labels = make(map[string]string)
	}
	for k, v := range a.labels {
		if actual.Labels[k] != v {
			actual.Labels[k] = v
			skip = false
		}
	}

	// Update owner reference.
	if !kubemeta.IsControlledBy(actual, a.node) {
		controlOnNodeWithoutBlock(actual, a.node)
		skip = false
	}

	return actual, skip, nil
}

// alignDeviceGroups reconciles the existing device groups (aGroups) against the freshly detected
// ones (eGroups) for one manufacturer pass: a group present in both is kept but refreshed with the
// newly detected content (its accelerators' slicing capability included), a group only in eGroups is
// added, and a group only in aGroups is dropped unless its manufacturer is outside the allowed set
// (in which case it is left untouched, since this pass has no fresh data for it). changed reports
// whether the returned list differs from aGroups.
//
// Note: the caller's re-detect trigger only watches the {manufacturer, id, unhealthy} device-key set
// (see Start), so toggling an accelerator's partitioning mode without changing that set never
// fires a re-detect; the stale capability then only clears on the next re-detect (e.g. a DaemonSet
// restart).
func alignDeviceGroups(
	aGroups, eGroups device.DevicesGroupList, allowedManufacturers sets.Set[string],
) (groups device.DevicesGroupList, changed bool) {
	// Merge canonical forms of both sides, so the order a detector happened to report its
	// accelerators in is never mistaken for a content change — which would rewrite the ledger with
	// identical content on every detect pass. The stored list is kept as it was read, because it is
	// what the result has to be judged against: an off-canonical stored order IS a change, and the
	// only one that heals it.
	stored := aGroups
	aGroups = canonicalDeviceGroups(aGroups)
	eGroups = canonicalDeviceGroups(eGroups)

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

	// The passes above append newly detected groups ahead of the ones the ledger already carried,
	// so the stored order would otherwise record which detection pass first saw each group rather
	// than anything about the hardware — and, since the passes preserve that stored order, a ledger
	// written in one order would keep it forever.
	groups = canonicalDeviceGroups(groups)

	return groups, changed || !kubemeta.DeepEqual(stored, groups)
}

// canonicalDeviceGroups returns the groups ordered by the hardware they describe: each group's
// accelerators by the enumeration index their detector recorded, and the groups themselves by
// manufacturer and then by the first accelerator each holds. Each manufacturer numbers its own
// accelerators from zero, which is why the manufacturer leads — ordering on the index alone would
// interleave them. A group holding no accelerator sorts last among its manufacturer's, and the group
// ID breaks any remaining tie, so the result is a total order over any input.
//
// The input is left untouched: the accelerators of a group reached through a copied group value
// still share one backing array with it, so sorting in place would reorder the caller's list as
// well — and a caller comparing the two would then find them equal.
//
// A consumer must not read a positional meaning into the result regardless. Both lists are declared
// as maps keyed by identity, so their order carries no API meaning and a server-side apply may
// reorder them; a consumer that needs a position orders what it reads. What this buys is a stored
// list that is a function of the hardware alone, so it neither drifts on unchanged content nor
// records the pass that first saw a group.
func canonicalDeviceGroups(groups device.DevicesGroupList) device.DevicesGroupList {
	byIndex := func(a, b device.Accelerator) int { return cmp.Compare(a.Index, b.Index) }

	out := slices.Clone(groups)
	for i := range out {
		out[i].Accelerators = slices.Clone(out[i].Accelerators)
		slices.SortStableFunc(out[i].Accelerators, byIndex)
	}
	slices.SortStableFunc(out, func(a, b device.DevicesGroup) int {
		if c := cmp.Compare(a.Manufacturer, b.Manufacturer); c != 0 {
			return c
		}
		if c := cmp.Compare(firstAcceleratorIndex(a), firstAcceleratorIndex(b)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	return out
}

// firstAcceleratorIndex returns the lowest enumeration index a group holds, or the maximum index
// when it holds no accelerator at all — which sorts such a group last rather than ahead of every
// populated one.
func firstAcceleratorIndex(grp device.DevicesGroup) uint32 {
	if len(grp.Accelerators) == 0 {
		return math.MaxUint32
	}
	return slices.MinFunc(grp.Accelerators, func(a, b device.Accelerator) int {
		return cmp.Compare(a.Index, b.Index)
	}).Index
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
