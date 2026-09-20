package allocator

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

var logger = klog.Background().WithName("allocator")

type Allocator struct {
	kubeSocket              string
	noShared                bool
	noSliced                bool
	noPartitioned           bool
	detectedManufacturersCh <-chan sets.Set[string]
}

func New(c *Config) (*Allocator, error) {
	return &Allocator{
		kubeSocket:              c.KubeSocket,
		noShared:                c.NoShared,
		noSliced:                c.NoSliced,
		noPartitioned:           c.NoPartitioned,
		detectedManufacturersCh: c.DetectedManufacturersCh,
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
		NoShared:      a.noShared,
		NoSliced:      a.noSliced,
		NoPartitioned: a.noPartitioned,
	}

	// The RDMA family is served on every platform that serves the vendor families, so it is
	// started here, once, rather than by the loop below: that loop is keyed on a detected
	// manufacturer, and a network interface belongs to the node rather than to a vendor. A
	// platform that registers no creator starts none, which keeps a device manager that today
	// starts no allocator at all from failing on a socket directory it does not have.
	if rdmaAllocatorCreator != nil {
		allocator := rdmaAllocatorCreator(createOpts)
		gox.Go(func() {
			if err := allocator.Start(sCtx); err != nil {
				logger.Error(err, "failed to start allocator", "family", allocator.Name())
				errCh <- err
			}
		})
		defer allocator.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case manus := <-a.detectedManufacturersCh:
			logger.V(2).Info("received detected manufacturers")

			// Stop the existing allocators if their manufacturers are not detected.
			for m := range allocators {
				if !manus.Has(m) {
					allocators[m].Stop()
					delete(allocators, m)
					continue
				}
				manus.Delete(m)
			}

			// Start new allocators if their manufacturers are detected but not exist.
			for _, m := range manus.UnsortedList() {
				creator := supportedAllocatorCreators[m]
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
