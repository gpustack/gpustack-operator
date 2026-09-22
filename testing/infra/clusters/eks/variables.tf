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
  # Same create-time rule as gpu_node_count below: min and max do follow an edit, but
  # desired_size is ignored by the module, so an edit against a live group moves the
  # bounds and leaves the node count where it was.
  # SCHEDULED TO GO AWAY together with gpu_node_count below, which carries the reason:
  # cpu_instance_types becomes a map of groups with the count inside each one, and "no
  # group" becomes an absent key rather than a zero. Plan in issue #502.
  #
  # Zero drops the group rather than creating an empty one, which is the rule
  # clusters/nebius already carried. A group of zero would not express the same thing
  # here anyway: desired_size is ignored on an existing group, so a group asked for and
  # then emptied is a group that stays.
  description = "Number of nodes in the CPU node group at CREATE time (min = max = this). Zero drops the group altogether, the same as clusters/nebius. Terraform does not move desired_size on an existing group; see gpu_node_count."
  type        = number
  default     = 1

  validation {
    condition     = var.cpu_node_count >= 0 && var.cpu_node_count == floor(var.cpu_node_count)
    error_message = "cpu_node_count must be a whole number, zero or greater."
  }
}

variable "efa_enabled" {
  # cpu_instance_types must name an EFA-capable type. Most are not: neither
  # default above is, and nor is any small size of the c7i/m7i/r7i families.
  # c5n.9xlarge is one that is. Check a candidate before enabling this with
  # `aws ec2 describe-instance-types --instance-types <type> --query
  # 'InstanceTypes[].NetworkInfo.EfaSupported'`. With this on, every type in
  # gpu_instance_types must pass the same check.
  description = "Enable EFA on the CPU and GPU node groups. EFA nodes are placed in one availability zone with an EFA launch template and placement group."
  type        = bool
  default     = false
}

variable "efa_availability_zone_index" {
  # WHICH availability zone the EFA groups land in, as an index into the module's own three.
  # It exists because capacity is the thing that runs out, and it runs out PER ZONE: an
  # EFA-capable accelerator type inside a cluster placement group is one of the scarcer
  # things to ask an account for, and a refusal names the zone rather than the type.
  #
  # MOVING ZONES IS THE FIX THAT KEEPS THE MEASUREMENT; changing the instance type is not.
  # The type is chosen for EfaSupported and for the accelerator on it, so swapping it swaps
  # the hardware under test and quietly answers a different question. Measured 2026-09-22:
  # g6e.8xlarge was refused in the first zone with InsufficientInstanceCapacity, and the
  # refusal itself named the zones that had it.
  #
  # Both node groups follow this one index on purpose. A cluster placement group is scoped
  # to one zone and RDMA does not reach across zones, so the two groups splitting zones
  # would render a cluster that cannot do the thing it was built for.
  description = "Index into the module's three availability zones, deciding which one the EFA node groups and their placement groups land in. Only read when efa_enabled is true. Move it when a zone refuses capacity; do NOT change the instance types instead, because those are what is under test."
  type        = number
  default     = 0

  validation {
    condition     = var.efa_availability_zone_index >= 0 && var.efa_availability_zone_index <= 2 && var.efa_availability_zone_index == floor(var.efa_availability_zone_index)
    error_message = "efa_availability_zone_index must be 0, 1 or 2 -- the module creates three availability zones."
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

variable "gpu_node_count" {
  # THIS VARIABLE IS SCHEDULED TO GO AWAY, and anyone here to change it should read that
  # plan first: the count belongs inside gpu_instance_types, beside the group shape it
  # counts, because one number applied to every key is already wrong as soon as two keys
  # describe different hardware. cpu_instance_types becomes a map of groups at the same
  # time, so the two stop being spelled differently for no reason. clusters/nebius takes
  # the same change in the same edit. The written-out plan, including the key-naming rule
  # that keeps an existing cluster's resource addresses stable, is issue #502.
  #
  # This value REACHES AWS ONLY WHEN THE GROUP IS CREATED. The module this delegates to
  # declares ignore_changes on scaling_config[0].desired_size, so editing it against a
  # group that already exists moves nothing -- not up, and not down to park it. An
  # earlier version of this description promised both; main.tf carries the same
  # correction beside the attribute itself.
  description = "Number of nodes in each GPU node group at CREATE time (desired_size; max_size follows it but never drops below 1, which EKS refuses; min_size stays 0). Terraform does not move this on an existing group -- the module ignores changes to desired_size -- so resize or park a live group with `aws eks update-nodegroup-config --scaling-config desiredSize=N`, which is drift-free precisely because the attribute is ignored here."
  type        = number
  default     = 1

  validation {
    condition     = var.gpu_node_count >= 0 && var.gpu_node_count == floor(var.gpu_node_count)
    error_message = "gpu_node_count must be a whole number of at least 0."
  }
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
  # aws-ec2-local-instance-store-csi-driver addon (OFF by default in this
  # module, see enable_instance_store_csi_driver) claims every instance-store
  # controller and DELETES the pre-existing namespace (nvme1n1) to reclaim it
  # into its own NVMeDevice pool, re-creating namespaces only when a PVC is
  # provisioned through its StorageClass. With the addon installed there is no
  # raw /dev/nvme1n1 for hostPath/local-device use.
  #
  # ONCE THE DRIVER HAS RUN ON A NODE, TURNING IT OFF DOES NOT BRING THE
  # NAMESPACE BACK, AND NEITHER DOES A REBOOT. Recreate it with `nvme create-ns`
  # plus `nvme attach-ns`, or replace the node. Observed 2026-09-08 on
  # i7ie.xlarge with driver v1.0.5; the README's variable table states the same.
  # An earlier version of this comment said a reboot re-creates it, and said the
  # addon was enabled by default -- both were true of the module before the
  # addon became opt-in, and neither was true of the behaviour.
  description = "Number of instance-store (ephemeral NVMe) devices to map on every node group; 0 maps none."
  type        = number
  default     = 0

  validation {
    condition     = var.node_instance_store_count >= 0 && var.node_instance_store_count == floor(var.node_instance_store_count) && var.node_instance_store_count <= 24
    error_message = "node_instance_store_count must be a whole number between 0 and 24."
  }

  validation {
    condition     = !var.enable_instance_store_csi_driver || var.node_instance_store_count == 0
    error_message = "enable_instance_store_csi_driver conflicts with node_instance_store_count: the CSI driver deletes the raw NVMe namespaces that the mapping exposes."
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
  #
  # THE DEFAULT IS A BREAKING CHANGE FOR A CLUSTER THAT PREDATES THIS VARIABLE.
  # The addon used to be installed unconditionally, so a plain `terraform apply`
  # over such a cluster REMOVES it and any PVC bound to its StorageClass loses
  # its provisioner. Pass -var="enable_instance_store_csi_driver=true" to keep
  # it. It is not defaulted to true instead, because the default that preserved
  # the addon is the one that broke node_instance_store_count, and the whole
  # point of the variable is that the two cannot both hold.
  #
  # The node_instance_store_count validation rejects this setting with a raw-device mapping,
  # before the driver can delete the namespace the mapping would expose at node boot.
  description = "Install the aws-ec2-local-instance-store-csi-driver EKS addon (CSI-managed instance store; deletes unmanaged NVMe namespaces such as the raw nvme1n1). Removing it from a cluster that predates this variable also removes any PVC's provisioner bound to its StorageClass."
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
