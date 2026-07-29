package kuberess

import (
	"context"
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

const (
	// nodeDevicesAdmissionCheckTimeout bounds the wait for Kueue's AdmissionCheck CRD.
	// Kueue ships its CRDs as chart templates, so they land with the rest of the
	// release rather than ahead of it, and a worker booting alongside that rollout can
	// reach this step before the CRD is served.
	nodeDevicesAdmissionCheckTimeout = 5 * time.Minute
	// nodeDevicesAdmissionCheckInterval is the retry interval within that bound.
	nodeDevicesAdmissionCheckInterval = 5 * time.Second
)

// nodeDevicesAdmissionCheckYAML is the gate-3 AdmissionCheck object. Its name and
// controllerName are the contract shared with the worker's NodeDevicesAdmission
// controllers (_NodeDevicesAdmissionCheckName / _NodeDevicesControllerName).
const nodeDevicesAdmissionCheckYAML = `
apiVersion: kueue.x-k8s.io/v1beta2
kind: AdmissionCheck
metadata:
  name: gpustack-node-devices
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
spec:
  controllerName: worker.gpustack.ai/node-devices
`

// InstallNodeDevicesAdmissionCheck applies the node-devices AdmissionCheck, retrying
// until Kueue's CRD is established.
//
// The chart cannot ship this object: its CRD belongs to Kueue, and Kueue templates its
// CRDs, so nothing orders the CRD ahead of a custom resource in the same render. Apply
// only sets spec, so it never clobbers the controller-owned Active condition, and the
// step is safe to repeat. The worker's NodeDevicesAdmissionCheckReconciler marks the
// check Active and InstanceTypeReconciler references it from accelerated ClusterQueues,
// so the scheduling chain does not materialize without it.
func InstallNodeDevicesAdmissionCheck(ctx context.Context) error {
	restCfg := system.LoopbackKubeRestConfig.Get()

	err := waitx.PollUntilContextTimeout(ctx,
		nodeDevicesAdmissionCheckInterval, nodeDevicesAdmissionCheckTimeout, true,
		func(ctx context.Context) error {
			err := kubeappyaml.Apply(ctx, nodeDevicesAdmissionCheckYAML, restCfg)
			if err != nil {
				// Report every attempt. This wait runs before the server answers a probe, so
				// a container can be killed inside it, and the error below only prints if the
				// poll gives up — without this the wait is indistinguishable from a hang.
				klog.InfoS("waiting for kueue's AdmissionCheck CRD", "err", err)
			}

			return err
		})
	if err != nil {
		return fmt.Errorf("waiting for kueue's AdmissionCheck CRD: %w", err)
	}

	return nil
}
