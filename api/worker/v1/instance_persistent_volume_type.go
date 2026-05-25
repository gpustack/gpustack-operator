package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstancePersistentVolumeType is the schema for worker.gpustack.ai.
//
// Underhood, an InstancePersistentVolumeType is mapping to a Kubernetes StorageClass,
// and the InstancePersistentVolumeType's name is the same as the Kubernetes StorageClass's name.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"]
type InstancePersistentVolumeType struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec InstancePersistentVolumeTypeSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

var _ runtime.Object = (*InstancePersistentVolumeType)(nil)

// InstancePersistentVolumeTypeSpec defines the desired state of InstancePersistentVolumeType.
type InstancePersistentVolumeTypeSpec struct {
	// DisplayName is the display name of the InstancePersistentVolumeType.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is the description of the InstancePersistentVolumeType.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`

	// InstancePersistentVolumeSource is the source of the InstancePersistentVolumeType.
	InstancePersistentVolumeSource `json:",inline" protobuf:"bytes,3,opt,name=instancePersistentVolumeSource"`
}

// InstancePersistentVolumeSource defines the source of the InstancePersistentVolumeType.
type InstancePersistentVolumeSource struct {
	// NFS represents a volume source that is managed by an NFS server.
	NFS *NFSInstancePersistentVolumeSource `json:"nfs,omitempty" protobuf:"bytes,1,opt,name=nfs"`

	// S3 represents a volume source that is managed by an S3-compatible object storage service.
	S3 *S3InstancePersistentVolumeSource `json:"s3,omitempty" protobuf:"bytes,2,opt,name=s3"`
}

// NFSInstancePersistentVolumeSource defines the source of NFS.
type NFSInstancePersistentVolumeSource struct {
	// Server is the hostname or IP address of the NFS server.
	//
	// Immutable after creation.
	//
	// +required
	Server string `json:"server" protobuf:"bytes,1,name=server"`

	// Share is the path of NFS share from the server.
	// For each InstancePersistentVolume,
	// a corresponding subpath will be created in the NFS share.
	//
	// Immutable after creation.
	//
	// +default="/"
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	Share string `json:"share,omitempty" protobuf:"bytes,2,opt,name=share"`

	// SubDirectory is the subdirectory in the NFS share for each InstancePersistentVolume.
	// If it is blank, the subdirectory will be the same as the volume ID of the InstancePersistentVolume.
	//
	// It supports a specific string or the following template variables:
	// - `${pvc.metadata.name}`: the name the corresponding PersistentVolumeClaim.
	// - `${pvc.metadata.namespace}`: the namespace of the corresponding PersistentVolumeClaim.
	// - `${pv.metadata.name}`: the name of the corresponding Kubernetes PersistentVolume.
	//
	// For example, specify `${pvc.metadata.namespace}/${pvc.metadata.name}` to create a subdirectory
	// with the namespaced name of the corresponding PersistentVolumeClaim in the NFS share
	// for each InstancePersistentVolume.
	//
	// Immutable after creation.
	SubDirectory string `json:"subDirectory,omitempty" protobuf:"bytes,3,opt,name=subDirectory"`

	// MountPermissions is the mounted directory permissions.
	// If it is non-zero, perform a chmod with the specified permissions after mounted.
	MountPermissions string `json:"mountPermissions,omitempty" protobuf:"bytes,4,opt,name=mountPermissions"`

	// MountOptions is the mount options for the NFS share.
	//
	// +default=["hard","vers=4","rsize=1048576","wsize=1048576","noatime","nodiratime"]
	MountOptions []string `json:"mountOptions,omitempty" protobuf:"bytes,5,opt,name=mountOptions"`
}

// S3InstancePersistentVolumeSource defines the source of S3.
type S3InstancePersistentVolumeSource struct {
	// Endpoint is the endpoint of the S3-compatible object storage service.
	//
	// +required
	Endpoint string `json:"endpoint" protobuf:"bytes,1,name=endpoint"`

	// Region is the region of the S3-compatible object storage service.
	// For AWS, the region is required to generate the presigned URL for the S3-compatible object storage service.
	Region string `json:"region,omitempty" protobuf:"bytes,2,opt,name=region"`

	// Insecure indicates whether to use insecure connection to the S3-compatible object storage service.
	// If it is true, the connection will not verify the TLS certificate of the S3-compatible object storage service.
	Insecure bool `json:"insecure,omitempty" protobuf:"varint,3,opt,name=insecure"`

	// AccessKey is the access key of the S3-compatible object storage service.
	//
	// Write-only input, it is required in create or update operations.
	AccessKey string `json:"accessKey,omitempty" protobuf:"bytes,4,opt,name=accessKey"`

	// SecretKey is the secret key of the S3-compatible object storage service.
	//
	// Write-only input, it is required in create or update operations.
	SecretKey string `json:"secretKey,omitempty" protobuf:"bytes,5,opt,name=secretKey"`

	// Bucket is the bucket name in the S3-compatible object storage service.
	// If it is blank, for each InstancePersistentVolume,
	// a corresponding bucket with the same volume ID as the InstancePersistentVolume will be created in the S3-compatible object storage service.
	//
	// Immutable after creation.
	Bucket string `json:"bucket,omitempty" protobuf:"bytes,6,opt,name=bucket"`

	// Prefix is the prefix in the bucket for each InstancePersistentVolume.
	// If it is blank, the prefix will be the same as the volume ID of the InstancePersistentVolume.
	//
	// It supports a specific string or the following template variables:
	// - `${pvc.metadata.name}`: the name the corresponding PersistentVolumeClaim.
	// - `${pvc.metadata.namespace}`: the namespace of the corresponding PersistentVolumeClaim.
	// - `${pv.metadata.name}`: the name of the corresponding Kubernetes PersistentVolume.
	//
	// For example, specify `${pvc.metadata.namespace}/${pvc.metadata.name}` to create a prefix
	// with the namespaced name of the corresponding PersistentVolumeClaim in the bucket
	// for each InstancePersistentVolume.
	//
	// Immutable after creation.
	Prefix string `json:"prefix,omitempty" protobuf:"bytes,7,opt,name=prefix"`

	// Mounter is the mounter for the S3-compatible object storage service.
	// It is used to specify the mounter for [GeeseFS](https://github.com/yandex-cloud/geesefs).
	//
	// Immutable after creation.
	//
	// +default="geesefs"
	//
	Mounter string `json:"mounter,omitempty" protobuf:"bytes,8,opt,name=mounter"`

	// MountOptions is the mount options for [GeeseFS](https://github.com/yandex-cloud/geesefs).
	//
	// Immutable after creation.
	//
	// Intensive writing for large files:
	//   disable CPU overhead, reduce freshening frequency, maximize parallelism,
	//   and reduce part sizes to improve writing performance for large files.
	//   ["--no-checksum","--memory-limit=4000","--max-flushers=32","--max-parallel-parts=32","--part-sizes=25"]
	//
	// Sequential reading for large files:
	//   increase read-ahead size and parallelism,
	//   and increase the memory cache limit to improve reading performance for large files.
	//   ["--read-ahead-large=200000"," --large-read-cutoff=10240","--read-ahead-parallel=40000","--memory-limit=8000"]
	//
	// Random reading for small files:
	//   decrease read-ahead size, extend metadata cache TTL,
	//   and increase the entry limit to improve reading performance for small files.
	//   ["--read-ahead-small=64","--small-read-cutoff=64","--read-ahead=1024","--stat-cache-ttl=300s","--entry-limit=200000"]
	//
	// High availability for writing:
	//   increase the number of retries and enable fsync on close to improve data durability for writing.
	//   ["--sdk-max-retries=10","--read-retry-attempts=5","--fsync-on-close","--cache=/mnt/disk-cache]
	//
	// For non-Yandex S3-compatible object storage service:
	//   ["--list-type=2","--no-specials"]
	//
	// +default=["--no-checksum","--memory-limit=4000","--max-flushers=32","--max-parallel-parts=32","--part-sizes=25","--list-type=2","--no-specials"]
	MountOptions []string `json:"mountOptions,omitempty" protobuf:"bytes,9,name=mountOptions"`
}

// InstancePersistentVolumeTypeList holds the list of InstancePersistentVolumeType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstancePersistentVolumeTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,name=metadata"`

	Items []InstancePersistentVolumeType `json:"items" protobuf:"bytes,2,name=items"`
}

var _ runtime.Object = (*InstancePersistentVolumeTypeList)(nil)
