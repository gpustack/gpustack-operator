package worker

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/webhook"
)

// NodeFeatureWebhook validates the "${nodeName}-gpustack-worker" nfd.NodeFeature
// objects — the place users are advised to set the slicing opt-in label
// "acceleratable.${prefix}${aKey}.sliced.partitions". It rejects partition counts
// that are not a power of two in [2, SlicedResourceMaxSize], and (best-effort,
// when the node's Devices CR is available) counts exceeding the card's hardware
// MaxPartitions. Other NodeFeature objects are not validated.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="nfd.k8s-sigs.io",version="v1alpha1",resource="nodefeatures",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type NodeFeatureWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *NodeFeatureWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &nfd.NodeFeature{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*NodeFeatureWebhook)(nil)

func (r *NodeFeatureWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, r.validate(ctx, obj.(*nfd.NodeFeature))
}

func (r *NodeFeatureWebhook) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, r.validate(ctx, newObj.(*nfd.NodeFeature))
}

func (r *NodeFeatureWebhook) ValidateDelete(context.Context, runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// validate checks every ".sliced.partitions" label carried by the worker
// NodeFeature. Non-worker NodeFeatures pass through untouched.
func (r *NodeFeatureWebhook) validate(ctx context.Context, nf *nfd.NodeFeature) error {
	nodeName := nf.Labels[nfd.NodeFeatureObjNodeNameLabel]
	if nodeName == "" || nf.Name != nodeName+"-gpustack-worker" {
		return nil
	}

	var (
		devs    *workercore.Devices
		fetched bool
	)
	for key, val := range nf.Spec.Labels {
		if !strings.HasPrefix(key, nodefeature.AcceleratableFeatureLabelPrefix) ||
			!strings.HasSuffix(key, nodefeature.SlicedPartitionsLabelSuffix) {
			continue
		}
		fldPath := field.NewPath("spec", "labels").Key(key)

		n, err := strconvx.Atoi[int64](val)
		if err != nil {
			return field.Invalid(fldPath, val, "sliced partitions must be an integer")
		}
		if !nodefeature.IsValidSlicedPartitions(n) {
			return field.Invalid(fldPath, val,
				fmt.Sprintf("sliced partitions must be a power of two in [2, %d]", nodefeature.SlicedResourceMaxSize))
		}

		// Best-effort hardware bound: reject counts exceeding the card's
		// MaxPartitions when the node's Devices CR is available. Any lookup miss
		// degrades to the power-of-two check above — never a false rejection.
		if !fetched {
			fetched = true
			d := new(workercore.Devices)
			if getErr := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, d); getErr == nil {
				devs = d
			}
		}
		aKey := strings.TrimSuffix(
			strings.TrimPrefix(key, nodefeature.AcceleratableFeatureLabelPrefix), nodefeature.SlicedPartitionsLabelSuffix)
		if maxParts := maxPartitionsForAKey(devs, aKey); maxParts > 0 && n > int64(maxParts) {
			return field.Invalid(fldPath, val,
				fmt.Sprintf("sliced partitions exceeds the device's maximum partitions (%d)", maxParts))
		}
	}
	return nil
}

// maxPartitionsForAKey returns the smallest positive MaxPartitions among the
// accelerators of the device group identified by aKey ("${manufacturer}-${id}"),
// or 0 when the group is unknown or reports no partition limit.
func maxPartitionsForAKey(devs *workercore.Devices, aKey string) int32 {
	if devs == nil {
		return 0
	}
	manufacturer, id, ok := strings.Cut(aKey, "-")
	if !ok {
		return 0
	}
	for gi := range devs.Spec.Groups {
		g := &devs.Spec.Groups[gi]
		if g.Manufacturer != manufacturer || g.ID != id {
			continue
		}
		var minMax int32
		for ai := range g.Accelerators {
			mp := g.Accelerators[ai].Features.MaxPartitions
			if mp <= 0 {
				continue
			}
			if minMax == 0 || mp < minMax {
				minMax = mp
			}
		}
		return minMax
	}
	return 0
}
