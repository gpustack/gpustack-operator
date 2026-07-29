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

variable "switch_kube_context" {
  # The cluster is merged into ~/.kube/config either way; this only decides whether a
  # bare kubectl points at it afterwards. Set it to false while another cluster is
  # mid-verification. Nothing is restored when there was no current context to begin
  # with (a kubeconfig that did not exist yet), so the merged one stays current there.
  description = "Whether `aws eks update-kubeconfig` may leave this cluster as the current context. When false, the context that was current before the update is restored."
  type        = bool
  default     = true
}
