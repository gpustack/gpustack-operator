package worker

import (
	"context"
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	_InstanceResource = "instances"
	_InstanceKind     = "Instance"
)

// InstanceHandler handles v1.Instance objects.
//
// InstanceHandler maps the v1.Instance to a Kubernetes Pod resource,
// which is named as the Instance's name.
type InstanceHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *InstanceHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &core.Pod{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, srs, err
	}
	err = fi.IndexField(ctx, &core.Node{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, srs, err
	}

	// Declare GVR.
	gvr = worker.SchemeGroupVersionResource(_InstanceResource)

	// Create table converter to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Type",
				Type: "string",
			},
			JSONPath: ".spec.type",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Access",
				Type: "string",
			},
			JSONPath: ".status.accessAddresses[0]",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "SSH-Port",
				Type: "string",
			},
			JSONPath: ".status.ports[?(@.port==22)].nodePort",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			JSONPath: ".status.phase",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.Instance{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"log":    newInstanceLogHandler(h.ObjectInfo, opts),
		"events": newInstanceEventsHandler(h.ObjectInfo, opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstanceHandler)(nil)
	_ rest.Creater           = (*InstanceHandler)(nil)
	_ rest.Lister            = (*InstanceHandler)(nil)
	_ rest.Watcher           = (*InstanceHandler)(nil)
	_ rest.Getter            = (*InstanceHandler)(nil)
	_ rest.Updater           = (*InstanceHandler)(nil)
	_ rest.Patcher           = (*InstanceHandler)(nil)
	_ rest.GracefulDeleter   = (*InstanceHandler)(nil)
	_ rest.CollectionDeleter = (*InstanceHandler)(nil)
)

func (h *InstanceHandler) New() runtime.Object {
	return &worker.Instance{}
}

func (h *InstanceHandler) Destroy() {}

func (h *InstanceHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	inst := obj.(*worker.Instance)

	// Validate.
	if inst.Namespace == kuberess.SystemNamespaceName {
		return nil, field.Invalid(
			field.NewPath("metadata.namespace"), inst.Namespace, "cannot create instance in system namespace")
	}
	if inst.Spec.Type == "" {
		return nil, field.Required(
			field.NewPath("spec.type"), "type must be specified")
	}
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name: inst.Spec.Type,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, field.InternalError(
				field.NewPath("spec.type"), fmt.Errorf("get instance type: %w", err))
		}
		return nil, field.NotFound(
			field.NewPath("spec.type"), inst.Spec.Type)
	}
	if inst.Spec.Resources != nil {
		var errs field.ErrorList
		if inst.Spec.Resources.CPU.Cmp(instType.Status.CPU.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.cpu"), inst.Spec.Resources.CPU.String(),
				fmt.Sprintf("exceeds the maximum CPU request of instance type %s", instType.Name)),
			)
		}
		if inst.Spec.Resources.RAM.Cmp(instType.Status.RAM.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.ram"), inst.Spec.Resources.RAM.String(),
				fmt.Sprintf("exceeds the maximum RAM request of instance type %s", instType.Name)),
			)
		}
		if inst.Spec.Resources.LocalStorage.Cmp(instType.Status.LocalStorage.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.localStorage"), inst.Spec.Resources.LocalStorage.String(),
				fmt.Sprintf("exceeds the maximum local storage request of instance type %s", instType.Name)),
			)
		}
		if inst.Spec.Resources.Accelerator != nil &&
			inst.Spec.Resources.Accelerator.Cmp(instType.Status.Accelerator.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.accelerator"), inst.Spec.Resources.Accelerator.String(),
				fmt.Sprintf("exceeds the maximum accelerator request of instance type %s", instType.Name)),
			)
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(worker.Kind(_InstanceKind), inst.Name, errs)
		}
	}
	if inst.Spec.Volume.Ephemeral != nil && inst.Spec.Volume.Persistent != nil {
		return nil, field.Required(
			field.NewPath("spec.volume"), "exactly one of ephemeral and persistent of volume should be specified")
	}

	// Default.
	if inst.Spec.VolumeMount == "" {
		inst.Spec.VolumeMount = "/workspace"
	}
	if inst.Spec.Resources == nil {
		inst.Spec.Resources = &worker.InstanceResources{}
	}
	if inst.Spec.Resources.CPU.IsZero() {
		inst.Spec.Resources.CPU = *resource.NewQuantity(1, resource.DecimalSI) // 1 core
	}
	if inst.Spec.Resources.RAM.IsZero() {
		inst.Spec.Resources.RAM = *resource.NewQuantity(2<<30, resource.BinarySI) // 2Gi
	}
	if inst.Spec.Resources.LocalStorage.IsZero() {
		inst.Spec.Resources.LocalStorage = *resource.NewQuantity(15<<30, resource.BinarySI) // 15Gi
	}
	if instType.Spec.Acceleratable {
		if inst.Spec.Resources.Accelerator == nil || inst.Spec.Resources.Accelerator.IsZero() {
			inst.Spec.Resources.Accelerator = resource.NewQuantity(1, resource.DecimalSI) // 1 accelerator
		}
	}

	// Create.
	pod := convertPodFromInstance(ctx, inst, instType)
	err = h.Client.Create(ctx, pod, &opts)
	if err != nil {
		return nil, err
	}

	staticAddress := settings.InstanceAccessStaticAddress.ShouldValue(ctx)
	wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValue(ctx)
	inst = convertInstanceFromPod(pod, staticAddress, wildcardDNS)
	return inst, nil
}

func (h *InstanceHandler) NewList() runtime.Object {
	return &worker.InstanceList{}
}

func (h *InstanceHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	podList := new(core.PodList)
	err := h.APIReader.List(ctx, podList,
		convertPodListOptsFromInstanceListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	instList := convertInstanceListFromPodList(ctx, podList, opts)
	return instList, nil
}

func (h *InstanceHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.PodList),
		convertPodListOptsFromInstanceListOpts(opts))
	if err != nil {
		return nil, err
	}

	staticAddress := settings.InstanceAccessStaticAddress.ShouldValue(ctx)
	wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValue(ctx)

	c := make(chan watch.Event)
	dw := watch.NewProxyWatcher(c)
	gox.Go(func() {
		defer close(c)
		defer uw.Stop()

		for {
			select {
			case <-ctx.Done():
				// Cancel by context.
				return
			case <-dw.StopChan():
				// Stop by downstream.
				return
			case e, ok := <-uw.ResultChan():
				if !ok {
					// Close by upstream.
					return
				}

				// Nothing to do.
				if e.Object == nil {
					c <- e
					continue
				}

				// Type assert.
				pod, ok := e.Object.(*core.Pod)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(pod)
					e.Object = &worker.Instance{ObjectMeta: pod.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				inst := convertInstanceFromPod(pod, staticAddress, wildcardDNS)
				if inst == nil {
					// Ignore if the pod doesn't match the requested namespace.
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !instanceMatchFieldSelector(opts, inst) {
					continue
				}

				// Dispatch.
				e.Object = inst
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *InstanceHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}
	err := h.APIReader.Get(ctx, key, pod, &opts)
	if err != nil {
		return nil, err
	}

	// Convert.
	staticAddress := settings.InstanceAccessStaticAddress.ShouldValue(ctx)
	wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValue(ctx)
	inst := convertInstanceFromPod(pod, staticAddress, wildcardDNS)
	if inst == nil {
		return nil, kerrors.NewNotFound(worker.Resource(_InstanceResource), key.Name)
	}

	return inst, nil
}

func (h *InstanceHandler) OnUpdate(
	ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions,
) (runtime.Object, error) {
	inst, instOld := obj.(*worker.Instance), oldObj.(*worker.Instance)

	// Validate.
	var errs field.ErrorList
	if inst.Spec.Type != instOld.Spec.Type {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.type"), "type is immutable"),
		)
	}
	if inst.Spec.Image != instOld.Spec.Image {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.image"), "image is immutable"),
		)
	}
	if inst.Spec.ImagePullPolicy != instOld.Spec.ImagePullPolicy {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.imagePullPolicy"), "imagePullPolicy is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.Command, instOld.Spec.Command) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.command"), "command is immutable"),
		)
	}
	if inst.Spec.Privileged != instOld.Spec.Privileged {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.privileged"), "privileged is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.Ports, instOld.Spec.Ports) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.ports"), "ports is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.Env, instOld.Spec.Env) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.env"), "env is immutable"),
		)
	}
	if inst.Spec.VolumeMount != instOld.Spec.VolumeMount {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.volumeMount"), "volumeMount is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.ImagePullSecret, instOld.Spec.ImagePullSecret) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.imagePullSecret"), "imagePullSecret is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.Resources, instOld.Spec.Resources) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.resources"), "resources is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.Volume, instOld.Spec.Volume) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.volume"), "volume is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.SSHPublicKey, instOld.Spec.SSHPublicKey) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.sshPublicKey"), "sshPublicKey is immutable"),
		)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(worker.Kind(_InstanceKind), inst.Name, errs)
	}

	// Update.
	oldPod := new(core.Pod)
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(inst), oldPod,
		kubeclientset.NonQuorum)
	if err != nil {
		return nil, err
	}

	pod := oldPod.DeepCopy()
	pod.ObjectMeta = inst.ObjectMeta
	systemmeta.NoteResource(pod, _InstanceResource, map[string]string{
		"displayName": inst.Spec.DisplayName,
		"description": inst.Spec.Description,
	})

	err = h.Client.Patch(ctx, pod, ctrlcli.MergeFrom(oldPod),
		kubeclientset.ToPatchOptions(opts))
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("update corresponding pod: %w", err))
	}

	staticAddress := settings.InstanceAccessStaticAddress.ShouldValue(ctx)
	wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValue(ctx)
	inst = convertInstanceFromPod(pod, staticAddress, wildcardDNS)
	return inst, nil
}

func (h *InstanceHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	inst := obj.(*worker.Instance)

	// Delete.
	pod := &core.Pod{ObjectMeta: inst.ObjectMeta}
	return h.Client.Delete(ctx, pod, &opts)
}

func convertPodListOptsFromInstanceListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_InstanceResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertPodFromInstance(ctx context.Context, inst *worker.Instance, instType *worker.InstanceType) *core.Pod {
	needSSHD := inst.Spec.SSHPublicKey != nil && inst.Spec.SSHPublicKey.Name != ""

	// Construct containers.
	var containers []core.Container
	if needSSHD {
		// Main container.
		mainC := core.Container{
			Name:            "main",
			Image:           inst.Spec.Image,
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Command:         inst.Spec.Command,
			SecurityContext: func() *core.SecurityContext {
				if !inst.Spec.Privileged {
					return nil
				}
				return &core.SecurityContext{
					Privileged: ptr.To(true),
				}
			}(),
			Resources: func() core.ResourceRequirements {
				requests := core.ResourceList{
					core.ResourceCPU:              inst.Spec.Resources.CPU,
					core.ResourceMemory:           inst.Spec.Resources.RAM,
					core.ResourceEphemeralStorage: inst.Spec.Resources.LocalStorage,
				}

				return core.ResourceRequirements{
					Requests: requests,
					Limits:   requests,
				}
			}(),
			Ports: slicex.Transform(inst.Spec.Ports, func(p worker.InstancePort) core.ContainerPort {
				return core.ContainerPort{
					Name:          strings.ToLower(fmt.Sprintf("%s%d", p.Protocol, p.Port)),
					Protocol:      p.Protocol,
					ContainerPort: p.Port,
				}
			}),
			Env: slicex.Transform(inst.Spec.Env, func(e worker.InstanceEnvVar) core.EnvVar {
				return core.EnvVar{
					Name:  e.Name,
					Value: e.Value,
				}
			}),
			VolumeMounts: []core.VolumeMount{{
				Name:      "workspace",
				MountPath: inst.Spec.VolumeMount,
			}},
		}

		// SSHD container.
		sshdC := core.Container{
			Name: "sshd",
			Image: func() string {
				img := settings.InstanceSSHServerImage.ShouldValue(ctx)
				if cn := settings.ContainerNamespace.ShouldValue(ctx); cn != "" {
					_, suffix, found := strings.Cut(img, "/")
					if found {
						img = cn + "/" + suffix
					}
				}
				if rn := settings.ContainerRegistry.ShouldValue(ctx); rn != "" {
					img = rn + "/" + img
				}
				return img
			}(),
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Stdin:           true,
			TTY:             true,
			SecurityContext: &core.SecurityContext{
				Capabilities: &core.Capabilities{
					Add: []core.Capability{
						"SYS_ADMIN",
						"SYS_PTRACE",
					},
				},
			},
			Resources: func() core.ResourceRequirements {
				requests := core.ResourceList{}
				if instType.Spec.Acceleratable && inst.Spec.Resources.Accelerator != nil {
					var resName core.ResourceName
					resQuantity := *inst.Spec.Resources.Accelerator
					if instType.Spec.Sliced > 0 {
						resQuantity = devicefeature.QuantityToAlignedValue(resQuantity, instType.Spec.Sliced)
						resName = devicefeature.GetResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeSliced)
					} else {
						resName = devicefeature.GetResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeExclusive)
					}
					requests[resName] = resQuantity
				}
				return core.ResourceRequirements{
					Requests: requests,
					Limits:   requests,
				}
			}(),
			Env: []core.EnvVar{
				{
					Name:  "VOLUME_MOUNT_PATH",
					Value: inst.Spec.VolumeMount,
				},
			},
			VolumeMounts: func() []core.VolumeMount {
				if inst.Spec.SSHPublicKey == nil {
					return nil
				}
				return []core.VolumeMount{{
					Name:      "sshd-authorized-keys",
					MountPath: "/var/run/sshd-authorized-keys",
					ReadOnly:  true,
				}}
			}(),
		}

		containers = []core.Container{mainC, sshdC}
	} else {
		// Main container.
		mainC := core.Container{
			Name:            "main",
			Image:           inst.Spec.Image,
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Command:         inst.Spec.Command,
			SecurityContext: func() *core.SecurityContext {
				if !inst.Spec.Privileged {
					return nil
				}
				return &core.SecurityContext{
					Privileged: ptr.To(true),
				}
			}(),
			Resources: func() core.ResourceRequirements {
				requests := core.ResourceList{
					core.ResourceCPU:              inst.Spec.Resources.CPU,
					core.ResourceMemory:           inst.Spec.Resources.RAM,
					core.ResourceEphemeralStorage: inst.Spec.Resources.LocalStorage,
				}
				if instType.Spec.Acceleratable && inst.Spec.Resources.Accelerator != nil {
					var resName core.ResourceName
					resQuantity := *inst.Spec.Resources.Accelerator
					if instType.Spec.Sliced > 0 {
						resQuantity = devicefeature.QuantityToAlignedValue(resQuantity, instType.Spec.Sliced)
						resName = devicefeature.GetResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeSliced)
					} else {
						resName = devicefeature.GetResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeExclusive)
					}
					requests[resName] = resQuantity
				}
				return core.ResourceRequirements{
					Requests: requests,
					Limits:   requests,
				}
			}(),
			Ports: slicex.Transform(inst.Spec.Ports, func(p worker.InstancePort) core.ContainerPort {
				return core.ContainerPort{
					Name:          strings.ToLower(fmt.Sprintf("%s-%d", p.Protocol, p.Port)),
					Protocol:      p.Protocol,
					ContainerPort: p.Port,
				}
			}),
			Env: slicex.Transform(inst.Spec.Env, func(e worker.InstanceEnvVar) core.EnvVar {
				return core.EnvVar{
					Name:  e.Name,
					Value: e.Value,
				}
			}),
			VolumeMounts: []core.VolumeMount{{
				Name:      "workspace",
				MountPath: inst.Spec.VolumeMount,
			}},
		}

		containers = []core.Container{mainC}
	}

	// Construct pod.
	pod := &core.Pod{
		ObjectMeta: inst.ObjectMeta,
		Spec: core.PodSpec{
			HostIPC:                      true,
			ShareProcessNamespace:        ptr.To(true),
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			ImagePullSecrets: func() []core.LocalObjectReference {
				if inst.Spec.ImagePullSecret == nil {
					return nil
				}
				return []core.LocalObjectReference{
					*inst.Spec.ImagePullSecret,
				}
			}(),
			Volumes: func() (vols []core.Volume) {
				if inst.Spec.SSHPublicKey != nil {
					vols = append(vols, core.Volume{
						Name: "sshd-authorized-keys",
						VolumeSource: core.VolumeSource{
							Secret: &core.SecretVolumeSource{
								SecretName:  inst.Spec.SSHPublicKey.Name,
								DefaultMode: ptr.To[int32](0o600),
							},
						},
					})
				}
				if inst.Spec.Volume.Ephemeral != nil {
					vols = append(vols, core.Volume{
						Name: "workspace",
						VolumeSource: core.VolumeSource{
							EmptyDir: &core.EmptyDirVolumeSource{
								SizeLimit: &inst.Spec.Volume.Ephemeral.Capacity,
							},
						},
					})
					return vols
				}
				vols = append(vols, core.Volume{
					Name: "workspace",
					VolumeSource: core.VolumeSource{
						PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
							ClaimName: inst.Spec.Volume.Persistent.Name,
						},
					},
				})
				return vols
			}(),
			Containers: containers,
		},
	}
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[kueuectrlconst.QueueLabel] = inst.Spec.Type // Scheduling.
	pod.Labels["app.kubernetes.io/part-of"] = inst.Name    // Accessing.

	notes := map[string]string{
		"displayName": inst.Spec.DisplayName,
		"description": inst.Spec.Description,
		"volumeEphemeralCapacity": func() string {
			if inst.Spec.Volume.Ephemeral == nil {
				return ""
			}
			return inst.Spec.Volume.Ephemeral.Capacity.String()
		}(),
		"volumePersistentName": func() string {
			if inst.Spec.Volume.Persistent == nil {
				return ""
			}
			return inst.Spec.Volume.Persistent.Name
		}(),
		"imagePullSecretName": func() string {
			if inst.Spec.ImagePullSecret == nil {
				return ""
			}
			return inst.Spec.ImagePullSecret.Name
		}(),
		"sshPublicKeyName": func() string {
			if inst.Spec.SSHPublicKey == nil {
				return ""
			}
			return inst.Spec.SSHPublicKey.Name
		}(),
	}
	for i := range containers[0].Ports {
		notes[containers[0].Ports[i].Name+"Alias"] = inst.Spec.Ports[i].Name
	}
	if instType.Spec.Acceleratable && instType.Spec.Sliced > 0 {
		notes["resourceAcceleratorSliced"] = strconvx.FormatInt(instType.Spec.Sliced, 10)
	}
	systemmeta.NoteResource(pod, _InstanceResource, notes)

	return pod
}

func convertInstanceFromPod(pod *core.Pod, staticAddress, wildcardDNS string) *worker.Instance {
	if pod == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(pod)
	if resType != _InstanceResource {
		return nil
	}

	// Retrieve type.
	var instType string
	if pod.Labels != nil {
		instType = pod.Labels[kueuectrlconst.QueueLabel]
		delete(pod.Labels, kueuectrlconst.QueueLabel)
	}
	if instType == "" {
		return nil
	}

	// Retrieve allocation info.
	var allocation workercore.DevicesStatus
	if pod.Annotations != nil && pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] != "" {
		v := pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey]
		json.ShouldUnmarshal(stringx.ToBytes(&v), &allocation)
		delete(pod.Annotations, deviceplugin.AllocatedAcceleratorAnnoKey)
	}

	// Reflect instance.
	var inst *worker.Instance
	{
		mainC := pod.Spec.Containers[0]
		inst = &worker.Instance{
			ObjectMeta: pod.ObjectMeta,
			Spec: worker.InstanceSpec{
				Type: instType,
				InstanceTemplate: worker.InstanceTemplate{
					Image:           mainC.Image,
					ImagePullPolicy: mainC.ImagePullPolicy,
					Command:         mainC.Command,
					Ports: slicex.Transform(mainC.Ports, func(p core.ContainerPort) worker.InstancePort {
						return worker.InstancePort{
							Port:     p.ContainerPort,
							Protocol: p.Protocol,
							Name:     notes[p.Name+"Alias"],
						}
					}),
					Env: slicex.Transform(mainC.Env, func(e core.EnvVar) worker.InstanceEnvVar {
						return worker.InstanceEnvVar{
							Name:  e.Name,
							Value: e.Value,
						}
					}),
					Resources: func() *worker.InstanceResources {
						ret := &worker.InstanceResources{}
						for n := range mainC.Resources.Limits {
							q := mainC.Resources.Limits[n]
							switch n {
							case core.ResourceCPU:
								ret.CPU = q
							case core.ResourceMemory:
								ret.RAM = q
							case core.ResourceEphemeralStorage:
								ret.LocalStorage = q
							default:
								if devicefeature.IsKnownResourceName(n) {
									ret.Accelerator = &q
								}
							}
						}
						return ret
					}(),
					VolumeMount: mainC.VolumeMounts[0].MountPath,
					ImagePullSecret: func() *core.LocalObjectReference {
						if notes["imagePullSecretName"] == "" {
							return nil
						}
						return &core.LocalObjectReference{
							Name: notes["imagePullSecretName"],
						}
					}(),
				},
				Volume: worker.InstanceVolume{
					Ephemeral: func() *worker.InstanceEphemeralVolume {
						if notes["volumeEphemeralCapacity"] == "" {
							return nil
						}
						return &worker.InstanceEphemeralVolume{
							Capacity: func() resource.Quantity {
								q, err := resource.ParseQuantity(notes["volumeEphemeralCapacity"])
								if err != nil {
									return resource.Quantity{}
								}
								return q
							}(),
						}
					}(),
					Persistent: func() *core.LocalObjectReference {
						if notes["volumePersistentName"] == "" {
							return nil
						}
						return &core.LocalObjectReference{
							Name: notes["volumePersistentName"],
						}
					}(),
				},
				DisplayName: notes["displayName"],
				Description: notes["description"],
				SSHPublicKey: func() *core.LocalObjectReference {
					if notes["sshPublicKeyName"] == "" {
						return nil
					}
					return &core.LocalObjectReference{
						Name: notes["sshPublicKeyName"],
					}
				}(),
			},
			Status: worker.InstanceStatus{
				NodeName: pod.Spec.NodeName,
				HostIPs: func() []core.HostIP {
					var ret []core.HostIP
					switch {
					case notes["externalHostIP"] != "":
						ret = append(ret, core.HostIP{IP: notes["externalHostIP"]})
					case notes["externalHostIPs"] != "":
						hostIPs := slicex.FilterTransform(strings.Split(notes["externalHostIPs"], ","),
							func(ip string) (core.HostIP, bool) {
								if ip == "" {
									return core.HostIP{}, false
								}
								return core.HostIP{IP: ip}, true
							})
						ret = hostIPs
					}
					if len(pod.Status.HostIPs) > 0 {
						ret = append(ret, pod.Status.HostIPs...)
					} else if pod.Status.HostIP != "" {
						ret = append(ret, core.HostIP{IP: pod.Status.HostIP})
					}
					return ret
				}(),
				PodIPs: func() []core.PodIP {
					if len(pod.Status.PodIPs) == 0 && pod.Status.PodIP != "" {
						return []core.PodIP{{IP: pod.Status.PodIP}}
					}
					return pod.Status.PodIPs
				}(),
				Ports: slicex.FilterTransform(mainC.Ports, func(p core.ContainerPort) (worker.InstanceServicePort, bool) {
					if notes[p.Name] == "" {
						return worker.InstanceServicePort{}, false
					}
					return worker.InstanceServicePort{
						InstancePort: worker.InstancePort{
							Port:     p.ContainerPort,
							Protocol: p.Protocol,
						},
						NodePort: funcx.NoError(strconvx.Atoi[int32](notes[p.Name])),
					}, true
				}),
			},
		}

		hasSSHD := len(pod.Spec.Containers) == 2
		if hasSSHD {
			sshdC := pod.Spec.Containers[1]
			for n := range sshdC.Resources.Limits {
				q := sshdC.Resources.Limits[n]
				if devicefeature.IsKnownResourceName(n) {
					inst.Spec.Resources.Accelerator = &q
					break
				}
			}
		}
	}
	inst.Status.Phase, inst.Status.PhaseMessage = apistatus.GetSummaryOfPod(&pod.Status)

	if inst.Spec.Resources.Accelerator != nil {
		if notes["resourceAcceleratorSliced"] != "" {
			sliced, err := strconvx.ParseInt[int64](notes["resourceAcceleratorSliced"], 10, 64)
			if err == nil && sliced > 0 {
				resQuantity := *inst.Spec.Resources.Accelerator
				resQuantity = devicefeature.QuantityToOriginalValue(resQuantity, sliced)
				inst.Spec.Resources.Accelerator = &resQuantity
			}
		}
	}

	switch {
	case staticAddress != "":
		inst.Status.AccessAddresses = []string{
			staticAddress,
		}
	case wildcardDNS != "" && len(inst.Status.HostIPs) > 0:
		ip := string(slicex.Transform([]rune(inst.Status.HostIPs[0].IP), func(r rune) rune {
			if r == '.' || r == ':' {
				return '-'
			}
			return r
		}))
		inst.Status.AccessAddresses = []string{
			fmt.Sprintf("%s.%s", ip, wildcardDNS),
		}
	}

	if len(allocation.Groups) > 0 {
		inst.Status.Allocations = allocation.Groups
	}

	return inst
}

func convertInstanceListFromPodList(ctx context.Context, podList *core.PodList, opts ctrlcli.ListOptions) *worker.InstanceList {
	if podList == nil {
		return &worker.InstanceList{}
	}

	staticAddress := settings.InstanceAccessStaticAddress.ShouldValue(ctx)
	wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValue(ctx)

	instList := &worker.InstanceList{
		ListMeta: podList.ListMeta,
		Items:    make([]worker.Instance, 0, len(podList.Items)),
	}

	for i := range podList.Items {
		inst := convertInstanceFromPod(&podList.Items[i], staticAddress, wildcardDNS)
		if inst == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !instanceMatchFieldSelector(opts, inst) {
			continue
		}
		instList.Items = append(instList.Items, *inst)
	}

	return instList
}

// instanceMatchFieldSelector checks if the Instance matches the field select in list options.
func instanceMatchFieldSelector(opts ctrlcli.ListOptions, inst *worker.Instance) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": inst.Namespace, "metadata.name": inst.Name})
}
