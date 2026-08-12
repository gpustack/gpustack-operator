package v1alpha1

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Instance is the schema for worker.gpustack.ai.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",subResources=["status"]
type Instance struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   InstanceSpec   `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	Status InstanceStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*Instance)(nil)

// InstanceSpec defines the desired state of Instance.
type InstanceSpec struct {
	// DisplayName is the display name of the Instance.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is the description of the Instance.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`

	// Type is the name of InstanceType that provisions corresponding resources.
	//
	// Immutable after creation.
	//
	// +required
	Type string `json:"type" protobuf:"bytes,3,name=type"`

	// InstanceTemplate is the template for the Instance to run.
	//
	// Immutable after creation.
	InstanceTemplate `json:",inline" protobuf:"bytes,4,name=instanceTemplate"`

	// Volume is the volume to mount in the Instance.
	//
	// Immutable after creation.
	//
	// +required
	Volume InstanceVolume `json:"volume,omitempty" protobuf:"bytes,5,opt,name=volume"`

	// SSHPublicKey is the reference to the InstanceSSHPublicKey that contains the SSH public key to access the Instance.
	//
	// Immutable after creation.
	SSHPublicKey *core.LocalObjectReference `json:"sshPublicKey,omitempty" protobuf:"bytes,6,opt,name=sshPublicKey"`

	// Stop indicates whether to stop the Instance after it is created.
	Stop bool `json:"stop,omitempty" protobuf:"varint,7,opt,name=stop"`

	// NodeName pins the Instance to one Kubernetes Node, which must exist when the Instance is
	// created. It is rendered as the backing Pod's nodeSelector on kubernetes.io/hostname, never
	// as the Pod's own nodeName, so the scheduler and Kueue admission still mediate placement.
	// Nothing beyond the node's existence is validated, so a node that cannot serve the referenced
	// InstanceType leaves the Pod Pending rather than being refused. Empty means no pinning.
	//
	// Immutable unless the Instance is stopped.
	//
	// +k8s:validation:maxLength=253
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,8,opt,name=nodeName"`
}

// InstanceTemplate defines the template for the Instance to run.
type InstanceTemplate struct {
	// Image is the container image to run.
	//
	// +required
	Image string `json:"image" protobuf:"bytes,1,name=image"`

	// ImagePullPolicy is the image pull policy to use.
	//
	// +default="IfNotPresent"
	// +k8s:validation:enum=["Always","IfNotPresent","Never"]
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,2,name=imagePullPolicy"`

	// Command is the command to run in the Instance,
	// which should overwrite the default command in the container image CMD.
	//
	// +listType=atomic
	Command []string `json:"command,omitempty" protobuf:"bytes,3,rep,name=command"`

	// Privileged indicates whether the container should run in privileged mode.
	//
	// +k8s:validation:default=false
	Privileged bool `json:"privileged,omitempty" protobuf:"bytes,4,opt,name=privileged"`

	// Ports is the list of ports to expose from the Instance.
	//
	// +patchMergeKey=port
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=port
	// +listMapKey=protocol
	Ports []InstancePort `json:"ports,omitempty" patchStrategy:"merge" patchMergeKey:"port" protobuf:"bytes,5,rep,name=ports"`

	// Env is the list of environment variables to set in the Instance.
	//
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Env []InstanceEnvVar `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,6,rep,name=env"`

	// Resources is the resource requirements for the Instance.
	Resources *InstanceResources `json:"resources,omitempty" protobuf:"bytes,7,opt,name=resources"`

	// VolumeMount is a path to mount the volume in the Instance.
	//
	// +default="/workspace"
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	VolumeMount string `json:"volumeMount,omitempty" protobuf:"bytes,8,opt,name=volumeMount"`

	// ImagePullSecret is the reference to the InstanceImagePullSecret that contains the credentials to pull the container image.
	ImagePullSecret *core.LocalObjectReference `json:"imagePullSecret,omitempty" protobuf:"bytes,9,opt,name=imagePullSecret"`

	// AdditionalVolumes is the list of volumes to mount in the Instance besides its workspace,
	// each at a path of its own. They are mounted into the Instance's main container only, which
	// the SSH server also observes.
	//
	// Immutable unless the Instance is stopped.
	//
	// +listType=atomic
	AdditionalVolumes []InstanceAdditionalVolume `json:"additionalVolumes,omitempty" protobuf:"bytes,10,rep,name=additionalVolumes"` // nolint: lll
}

// InstanceAdditionalVolume defines one volume to mount in the Instance besides its workspace.
// Exactly one source must be specified.
type InstanceAdditionalVolume struct {
	// MountPath is the absolute in-container path to mount the volume at. It must not duplicate
	// another entry's path, nor the workspace's VolumeMount.
	//
	// +required
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	MountPath string `json:"mountPath" protobuf:"bytes,1,name=mountPath"`

	// ReadOnly mounts the volume read-only.
	ReadOnly bool `json:"readOnly,omitempty" protobuf:"varint,2,opt,name=readOnly"`

	// SubPath mounts a relative path inside the volume rather than its root.
	// It must not be absolute nor contain a ".." element.
	//
	// +k8s:validation:maxLength=1024
	SubPath string `json:"subPath,omitempty" protobuf:"bytes,3,opt,name=subPath"`

	// Persistent is the reference to the InstancePersistentVolume to mount, in the same namespace.
	Persistent *core.LocalObjectReference `json:"persistent,omitempty" protobuf:"bytes,4,opt,name=persistent"`

	// ConfigMap is the reference to the ConfigMap to mount, in the same namespace.
	ConfigMap *core.LocalObjectReference `json:"configMap,omitempty" protobuf:"bytes,5,opt,name=configMap"`

	// Secret is the reference to the Secret to mount, in the same namespace.
	Secret *core.LocalObjectReference `json:"secret,omitempty" protobuf:"bytes,6,opt,name=secret"`

	// HostPath is the path on the Kubernetes Node to mount. It crosses the node boundary, so
	// taking it requires the instance-host-path-volume-allowed Setting — at creation, and on any
	// later change that adds or widens such a mount. One the Instance already holds is never
	// re-judged, so the Setting going off does not strand it.
	HostPath *core.HostPathVolumeSource `json:"hostPath,omitempty" protobuf:"bytes,7,opt,name=hostPath"`
}

// InstancePort defines the port to expose from the Instance.
type InstancePort struct {
	// Port is the port number to expose on the Instance.
	Port int32 `json:"port" protobuf:"varint,1,name=port"`

	// Protocol is the protocol to use for the port.
	//
	// +k8s:validation:enum=["TCP","UDP","SCTP"]
	// +k8s:validation:default="TCP"
	Protocol core.Protocol `json:"protocol,omitempty" protobuf:"bytes,2,opt,name=protocol"`

	// Name is the name of the port.
	//
	// +k8s:validation:maxLength=16
	Name string `json:"name,omitempty" protobuf:"bytes,3,opt,name=name"`
}

// InstanceEnvVar defines the environment variable to set in the Instance.
type InstanceEnvVar struct {
	// Name is the name of the environment variable,
	// each name in one Instance must be unique.
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Value is the value of the environment variable.
	Value string `json:"value" protobuf:"bytes,2,name=value"`
}

// InstanceResources defines the resource requirements for the Instance.
type InstanceResources struct {
	// CPU is the CPU resource requirement for the Instance, e.g. "4", "8".
	//
	// +required
	CPU resource.Quantity `json:"cpu" protobuf:"bytes,1,name=cpu"`

	// RAM is the RAM resource requirement for the Instance, e.g. "40G", "16G".
	//
	// +required
	RAM resource.Quantity `json:"ram" protobuf:"bytes,2,name=ram"`

	// LocalStorage is the local storage resource requirement for the Instance, e.g. "100G", "500G".
	//
	// +required
	LocalStorage resource.Quantity `json:"localStorage" protobuf:"bytes,3,name=localStorage"`

	// Accelerator is the accelerator resource requirement for the Instance, e.g. "1", "2".
	Accelerator *resource.Quantity `json:"accelerator,omitempty" protobuf:"bytes,4,opt,name=accelerator"`

	// AcceleratorSlicedMemoryPercentage is the per-accelerator VRAM budget requested on a
	// sliced InstanceType, as a percentage in [0,100]. 0 disables slicing (the request
	// becomes an exclusive whole-accelerator request). The Pod webhook folds it into the
	// normalized .sliced.units; it is ignored by non-sliced requests.
	//
	// +optional
	AcceleratorSlicedMemoryPercentage int32 `json:"acceleratorSlicedMemoryPercentage,omitempty" protobuf:"varint,5,opt,name=acceleratorSlicedMemoryPercentage"` // nolint: lll

	// AcceleratorSlicedCoresPercentage is the per-accelerator compute (SM) budget requested on
	// a sliced InstanceType, as a percentage in [0,100]. It is independent of
	// AcceleratorSlicedMemoryPercentage; when only one of the two is set the webhook
	// copies it to the other. It is ignored by non-sliced requests.
	//
	// +optional
	AcceleratorSlicedCoresPercentage int32 `json:"acceleratorSlicedCoresPercentage,omitempty" protobuf:"varint,6,opt,name=acceleratorSlicedCoresPercentage"` // nolint: lll

	// AcceleratorPartitionedProfile is the hardware partition profile requested on a
	// partition-offering InstanceType, e.g. "3g.40gb". A non-empty value makes this a
	// request for one hardware partition of that shape, which is mutually exclusive with
	// the two slice percentages above: hardware partitioning and software slicing cannot
	// both apply to one accelerator. It is ignored by InstanceTypes offering no partition.
	//
	// +optional
	AcceleratorPartitionedProfile string `json:"acceleratorPartitionedProfile,omitempty" protobuf:"bytes,7,opt,name=acceleratorPartitionedProfile"` // nolint: lll
}

// InstanceVolume defines the volume to mount in the Instance,
// which can be either ephemeral or persistent.
type InstanceVolume struct {
	// Ephemeral is the ephemeral volume to mount in the Instance.
	Ephemeral *InstanceEphemeralVolume `json:"ephemeral,omitempty" protobuf:"bytes,1,opt,name=ephemeral"`

	// Persistent is the reference to the InstancePersistentVolume to mount in the Instance.
	Persistent *core.LocalObjectReference `json:"persistent,omitempty" protobuf:"bytes,2,opt,name=persistent"`
}

// InstanceEphemeralVolume defines the ephemeral volume to mount in the Instance.
type InstanceEphemeralVolume struct {
	// Capacity is the size limit of the volume.
	//
	// +required
	Capacity resource.Quantity `json:"capacity" protobuf:"bytes,1,name=capacity"`
}

// InstanceStatus defines the observed state of Instance.
type InstanceStatus struct {
	// Phase is the current phase of the Instance.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,name=phase"`

	// PhaseMessage is the message to describe the current phase of the Instance.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// NodeName is the name of the Kubernetes Node that the Instance is running on.
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,3,opt,name=nodeName"`

	// AccessAddresses holds the accessible addresses allocated to the Instance.
	AccessAddresses []string `json:"accessAddresses,omitempty" protobuf:"bytes,4,rep,name=accessAddresses"`

	// HostIPs holds the IP addresses allocated to the host.
	HostIPs []core.HostIP `json:"hostIPs,omitempty" protobuf:"bytes,5,rep,name=hostIPs"`

	// PodIPs holds the IP addresses allocated to the Kubernetes Pod that related to the Instance.
	PodIPs []core.PodIP `json:"podIPs,omitempty" protobuf:"bytes,6,rep,name=podIPs"`

	// Ports is the list of ports to expose from the Instance.
	Ports []InstanceServicePort `json:"ports,omitempty" protobuf:"bytes,7,rep,name=ports"`

	// Allocations is the list of devices allocated to the Instance.
	Allocations []DevicesAllocationGroup `json:"allocations,omitempty" protobuf:"bytes,8,rep,name=allocations"`
}

// InstanceServicePort defines the port to expose from the container and the port to expose on the node.
type InstanceServicePort struct {
	InstancePort `json:",inline" protobuf:"bytes,1,name=instancePort"`

	// NodePort is the port number to expose on the Kubernetes Node.
	NodePort int32 `json:"nodePort,omitempty" protobuf:"varint,2,opt,name=nodePort"`
}

// InstanceList holds the list of Instance.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Instance `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstanceList)(nil)
