variable "region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "ssh_public_key" {
  # Registered as the nodes' EC2 key pair for SSH access (the node security group
  # already opens port 22). pathexpand() handles the leading "~".
  description = "Path to the SSH public key registered as the EC2 key pair for node access."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "172.31.0.0/16"
}

variable "name_prefix" {
  description = "Prefix for the EKS cluster name"
  type        = string
  default     = "gpustack-eks"
}

variable "release" {
  description = "EKS version"
  type        = string
  default     = "1.34"
}

variable "cpu_instance_types" {
  description = "Instance types for EKS CPU node group list, check with https://aws.amazon.com/ec2/pricing/on-demand/"
  type        = list(string)
  default     = ["c6a.4xlarge", "c7a.4xlarge"]
}

variable "cpu_node_count" {
  description = "Number of nodes in the CPU node group (min = max = this)."
  type        = number
  default     = 1

  validation {
    condition     = var.cpu_node_count > 0 && var.cpu_node_count == floor(var.cpu_node_count)
    error_message = "cpu_node_count must be a positive whole number."
  }
}

variable "gpu_instance_types" {
  # Keyed by group name so each GPU node group has a stable key (gpu-<name>).
  # Adding a key is a +create only; editing a key's instance-type list replaces
  # just that group, never rotating the others.
  description = "Instance types per EKS GPU node group, keyed by group name; check with https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html and https://aws.amazon.com/ec2/pricing/on-demand/"
  type        = map(list(string))
  default     = { g4dn = ["g4dn.xlarge", "g4dn.12xlarge"] }
  # default     = { xlarge = ["g4dn.xlarge", "g5.xlarge"], xlarge-alt = ["g5.xlarge", "g6.xlarge"], large = ["g4dn.12xlarge", "g5.12xlarge"] }
  # default     = { small = ["g4dn.xlarge", "g4dn.12xlarge", "g6.xlarge"], large = ["g4dn.12xlarge", "g5.12xlarge", "g6.12xlarge"] }
  # default     = { g4dn = ["g4dn.xlarge", "g4dn.12xlarge"], g5 = ["g5.xlarge", "g5.12xlarge"], g6 = ["g6.xlarge", "g6.12xlarge"] }
}

variable "node_boot_disk_type" {
  # iops/throughput are optional so overriding volume_type to a non-gp3/io* type
  # doesn't force incompatible values onto the block_device_mappings.xvda.ebs block.
  description = "Node root (boot) volume EBS type/performance, driving block_device_mappings.xvda.ebs for both node groups."
  type = object({
    volume_type = string
    iops        = optional(number)
    throughput  = optional(number)
  })
  default = { volume_type = "gp3", iops = 3000, throughput = 125 }

  validation {
    condition     = var.node_boot_disk_type.iops == null || (var.node_boot_disk_type.iops == floor(var.node_boot_disk_type.iops) && var.node_boot_disk_type.iops > 0)
    error_message = "node_boot_disk_type.iops, when set, must be a positive whole number."
  }

  validation {
    condition     = var.node_boot_disk_type.throughput == null || (var.node_boot_disk_type.throughput == floor(var.node_boot_disk_type.throughput) && var.node_boot_disk_type.throughput > 0)
    error_message = "node_boot_disk_type.throughput, when set, must be a positive whole number."
  }
}

variable "node_boot_disk_size_gb" {
  description = "Node root (boot) volume size, in GiB. Under this module's custom launch template, block_device_mappings.xvda IS the boot disk (disk_size is ignored)."
  type        = number
  default     = 100

  validation {
    condition     = var.node_boot_disk_size_gb > 0 && var.node_boot_disk_size_gb == floor(var.node_boot_disk_size_gb)
    error_message = "node_boot_disk_size_gb must be a positive whole number."
  }
}

variable "node_instance_store_count" {
  # This module's launch template maps xvda explicitly, which drops the AMI's
  # default ephemeral mappings, so instance-store devices must be re-declared.
  # The count applies to every node group and must not exceed the instance
  # type's disk count, e.g. i7ie.xlarge has 1 (check with:
  #   aws ec2 describe-instance-types --instance-types <type> \
  #     --query 'InstanceTypes[0].InstanceStorageInfo.Disks').
  # On Nitro instances the device_name in the mapping is ignored; the devices
  # show up as /dev/nvme1n1 and onward.
  #
  # WARNING (observed 2026-09-08, i7ie.xlarge, EKS 1.34, AL2023): the mapping
  # itself is fine — a standalone instance with the same explicit ephemeral
  # mapping gets /dev/nvme1n1 at boot. But on EKS nodes the
  # aws-ec2-local-instance-store-csi-driver addon (enabled by default in this
  # module, see enable_instance_store_csi_driver) claims every instance-store
  # controller and DELETES the pre-existing namespace (nvme1n1) to reclaim it
  # into its own NVMeDevice pool, re-creating namespaces only when a PVC is
  # provisioned through its StorageClass. With the addon installed there is no
  # raw /dev/nvme1n1 for hostPath/local-device use. Disable the addon (and
  # reboot the nodes so the namespace is re-created) when you need the raw
  # device.
  description = "Number of instance-store (ephemeral NVMe) devices to map on every node group; 0 maps none."
  type        = number
  default     = 0

  validation {
    condition     = var.node_instance_store_count >= 0 && var.node_instance_store_count == floor(var.node_instance_store_count) && var.node_instance_store_count <= 24
    error_message = "node_instance_store_count must be a whole number between 0 and 24."
  }
}

variable "enable_instance_store_csi_driver" {
  # The aws-ec2-local-instance-store-csi-driver EKS addon takes ownership of
  # every instance-store NVMe controller on each node: on startup it deletes
  # any namespace it does not own (e.g. the default nvme1n1 that Nitro
  # surfaces) and hands capacity out only through PVCs bound to its
  # StorageClass. That conflicts with node_instance_store_count, whose point
  # is exposing the raw /dev/nvmeNn1 devices for hostPath-style use, so this
  # defaults to false. Enable it only when workloads consume instance store
  # via the driver's CSI volumes.
  description = "Install the aws-ec2-local-instance-store-csi-driver EKS addon (CSI-managed instance store; deletes unmanaged NVMe namespaces such as the raw nvme1n1)."
  type        = bool
  default     = false
}

variable "switch_kube_context" {
  # The cluster is merged into ~/.kube/config either way; this only decides whether a
  # bare kubectl points at it afterwards. Set it to false while another cluster is
  # mid-verification. Nothing is restored when there was no current context to begin
  # with (a kubeconfig that did not exist yet), so the merged one stays current there.
  description = "Whether `aws eks update-kubeconfig` may leave this cluster as the current context. When false, the context that was current before the update is restored."
  type        = bool
  default     = true
}
