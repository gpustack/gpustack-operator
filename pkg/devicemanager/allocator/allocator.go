package allocator

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/device"
	allocatormock "gpustack.ai/gpustack/pkg/devicemanager/allocator/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

var logger = klog.Background().WithName("allocator")

type Allocator struct {
	kubeSocket              string
	hostRoot                string
	noShared                bool
	noSliced                bool
	noPartitioned           bool
	detectedManufacturersCh <-chan sets.Set[string]
	allocatorCreators       map[string]func(device.AllocatorOptions) device.Allocator
	rdmaCreator             func(device.AllocatorOptions) device.Allocator
}

func New(c *Config) (*Allocator, error) {
	allocatorCreators := supportedAllocatorCreators
	rdmaCreator := rdmaAllocatorCreator

	specStore := c.MockSpecStore
	if specStore == nil && c.MockDevicesFile != "" {
		var err error
		specStore, err = mockspec.NewStore(c.MockDevicesFile, mockspec.WithLogger(logger.WithName("mockspec")))
		if err != nil {
			return nil, err
		}
		klog.Warningf("!!! mock device mode is active (mock-devices-file=%q); real hardware detection and device injection are bypassed !!!",
			specStore.Path())
	}
	if specStore != nil {
		partStore := c.MockPartitionStore
		if partStore == nil {
			partStore = allocatormock.NewPartitionStore()
		}
		nodeName := osx.Getenv("KUBERNETES_NODE_NAME")
		mockOpts := make([]allocatormock.Option, 0, 2+len(c.MockOptions))
		mockOpts = append(mockOpts,
			allocatormock.WithPartitionStore(partStore),
			allocatormock.WithSpecStore(specStore, nodeName),
		)
		mockOpts = append(mockOpts, c.MockOptions...)

		allocatorCreators = allocatormock.SupportedAllocatorCreators(mockOpts...)
		rdmaCreator = allocatormock.RDMAAllocatorCreator(mockOpts...)
	}

	return &Allocator{
		kubeSocket:              c.KubeSocket,
		hostRoot:                c.HostRoot,
		noShared:                c.NoShared,
		noSliced:                c.NoSliced,
		noPartitioned:           c.NoPartitioned,
		detectedManufacturersCh: c.DetectedManufacturersCh,
		allocatorCreators:       allocatorCreators,
		rdmaCreator:             rdmaCreator,
	}, nil
}

func (a *Allocator) Start(ctx context.Context) error {
	allocators := make(map[string]device.Allocator)
	defer func() {
		for m := range allocators {
			allocators[m].Stop()
		}
	}()

	errCh := make(chan error)
	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	createOpts := device.AllocatorOptions{
		Logger:        logger.V(3),
		KubeSocket:    a.kubeSocket,
		HostRoot:      a.hostRoot,
		NoShared:      a.noShared,
		NoSliced:      a.noSliced,
		NoPartitioned: a.noPartitioned,
	}

	// The RDMA family is served on every platform that serves the vendor families, so it is
	// started here, once, rather than by the loop below: that loop is keyed on a detected
	// manufacturer, and a network interface belongs to the node rather than to a vendor. A
	// platform that registers no creator starts none, which keeps a device manager that today
	// starts no allocator at all from failing on a socket directory it does not have.
	if a.rdmaCreator != nil {
		allocator := a.rdmaCreator(createOpts)
		gox.Go(func() {
			if err := allocator.Start(sCtx); err != nil {
				logger.Error(err, "failed to start allocator", "family", allocator.Name())
				errCh <- err
			}
		})
		defer allocator.Stop()
	}

	detectedCh := a.detectedManufacturersCh
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case manus, ok := <-detectedCh:
			if !ok {
				detectedCh = nil
				continue
			}
			logger.V(2).Info("received detected manufacturers")
			pending := manus.Clone()

			// Stop the existing allocators if their manufacturers are not detected.
			for m := range allocators {
				if !pending.Has(m) {
					allocators[m].Stop()
					delete(allocators, m)
					continue
				}
				pending.Delete(m)
			}

			// Start new allocators if their manufacturers are detected but not exist.
			for _, m := range pending.UnsortedList() {
				creator := a.allocatorCreators[m]
				if creator == nil {
					logger.V(2).Info("unsupported manufacturer, skipping")
					continue
				}
				allocators[m] = creator(createOpts)
				allocator := allocators[m]
				gox.Go(func() {
					if err := allocator.Start(sCtx); err != nil {
						logger.Error(err, "failed to start allocator", "manufacturer", m)
						errCh <- err
					}
				})
			}
		}
	}
}
