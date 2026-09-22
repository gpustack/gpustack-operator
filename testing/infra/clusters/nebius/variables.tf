# region -> available platforms (region is fixed by project_id's project, NOT a TF variable):
#   eu-north1   : cpu-d3, cpu-e2, gpu-h100-sxm, gpu-h200-sxm, gpu-l40s-a, gpu-l40s-d
#   eu-west1    : cpu-d3, gpu-h200-sxm
#   me-west1    : cpu-d3, gpu-b200-sxm-a
#   uk-south1   : cpu-d3, gpu-b300-sxm
#   us-central1 : cpu-d3, gpu-b200-sxm, gpu-h200-sxm, gpu-rtx6000
variable "project_id" {
  description = "Nebius project ID; its region fixes node placement and platform availability (see the region table above)."
  type        = string

  validation {
    condition     = can(regex("^project-", var.project_id))
    error_message = "project_id must be a Nebius project ID, e.g. 'project-...'."
  }
}

variable "name_prefix" {
  description = "Prefix for the cluster and its network/subnet/security-group names (a random suffix is appended)."
  type        = string
  default     = "gpustack-nebius"
}

variable "release" {
  # Named "release" to match clusters/k3s and clusters/eks. Only "<major>.<minor>" is accepted
  # (e.g. "1.35"). Nebius refuses a version within a month of its end of life -- the create call
  # fails with "k8s version <x> is deprecated and cannot be used" -- so this default tracks a
  # version that is still current, not the oldest one that works.
  #
  # To find the current upper bound, ask the compatibility matrix for a candidate version:
  #
  #   nebius mk8s v1 node-group get-compatibility-matrix \
  #     --cluster-kubernetes-version 1.36 --format json
  #
  # A version Nebius knows returns a populated "versions" array; one it does not returns the
  # empty object {}. Read the BODY: the exit code is 0 either way, so a check that tests only
  # the status will happily pick a version that cannot be created. That query does not say
  # whether a known version is still creatable, though -- it answers for deprecated versions
  # just as happily -- so it gives the ceiling, not the floor.
  description = "Kubernetes version for the cluster control plane and node groups, e.g. '1.35'."
  type        = string
  default     = "1.35"
}

variable "ssh_public_key" {
  # pathexpand() handles the leading "~".
  description = "Path to the SSH public key injected into every node via cloud-init."
  type        = string
  default     = "~/.ssh/id_rsa.pub"

  validation {
    condition     = fileexists(pathexpand(var.ssh_public_key))
    error_message = "ssh_public_key must point to a file that exists on disk."
  }
}

variable "node_boot_disk_size_gb" {
  description = "Node boot disk size, in GiB, for every node group."
  type        = number
  default     = 100

  validation {
    condition     = var.node_boot_disk_size_gb > 0 && var.node_boot_disk_size_gb == floor(var.node_boot_disk_size_gb)
    error_message = "node_boot_disk_size_gb must be a positive whole number."
  }
}

variable "node_boot_disk_type" {
  description = "Node boot disk type, one of 'NETWORK_SSD', 'NETWORK_HDD', 'NETWORK_SSD_NON_REPLICATED', 'NETWORK_SSD_IO_M3'."
  type        = string
  default     = "NETWORK_SSD"

  validation {
    condition     = contains(["NETWORK_SSD", "NETWORK_HDD", "NETWORK_SSD_NON_REPLICATED", "NETWORK_SSD_IO_M3"], var.node_boot_disk_type)
    error_message = "node_boot_disk_type must be one of: NETWORK_SSD, NETWORK_HDD, NETWORK_SSD_NON_REPLICATED, NETWORK_SSD_IO_M3."
  }
}

# The CPU node group's shape, mirroring clusters/eks's cpu_instance_types. There is exactly one CPU
# group -- unlike gpu_instance_types, which is keyed by group name -- but it is not therefore one
# node: cpu_node_count sizes it. No image_family: unlike a standalone compute VM (computes/nebius),
# the mk8s node template picks its image from `os` alone for a driverless (CPU) platform.
#
# public_ip gives the node a public IPv4, which is what makes it reachable over SSH. It defaults
# to false because the accelerator tests drive GPU nodes, not this one, and every address is
# charged against the project's vpc.ipv4-address.public.count quota. Set it to true for the one
# workflow that does need inbound reach: building images on the node itself. Dropping the address
# does not cost the node its outbound internet (see README), so pulls work either way.
variable "cpu_instance_types" {
  description = "Instance type for the CPU node group: platform/preset (see the region table above), os, and whether the node takes a public IPv4 (public_ip, default false; true makes it SSH-reachable at one public-address quota unit)."
  type = object({
    platform  = string
    preset    = string
    os        = string
    public_ip = optional(bool, false)
  })
  default = { platform = "cpu-e2", preset = "4vcpu-16gb", os = "ubuntu24.04" }
}

# How many nodes the CPU group runs. Every GPU group is one node, so this is the module's only
# multi-node knob: a test that needs several plain nodes -- one that moves data between them, or
# that adds a member to a running set -- gets them here rather than by buying accelerator capacity
# it will not use. Combined with cpu_instance_types.public_ip, the quota cost is one address per
# node rather than one for the group (see README).
#
# Zero drops the group instead of sizing it to nothing. Some regions sell accelerator capacity
# alone and hold compute.instance.non-gpu.vcpu at zero, where a CPU node cannot be created at all
# and asking for one fails the apply rather than costing a little extra.
variable "cpu_node_count" {
  # SCHEDULED TO GO AWAY together with gpu_node_count below, which carries the reason:
  # cpu_instance_types becomes a map of groups with the count inside each one, and "no
  # group" becomes an absent key rather than a zero. Plan in issue #502.
  description = "Number of nodes in the CPU node group. Zero drops the group altogether, which is what a region holding compute.instance.non-gpu.vcpu at zero requires. Same name, type, default and zero-drops rule as clusters/eks."
  type        = number
  default     = 1

  validation {
    condition     = var.cpu_node_count >= 0 && var.cpu_node_count == floor(var.cpu_node_count)
    error_message = "cpu_node_count must be a whole number, zero or greater."
  }
}

variable "gpu_node_count" {
  # THIS VARIABLE IS SCHEDULED TO GO AWAY, and so is cpu_node_count above: the count
  # belongs inside gpu_instance_types beside the group shape it counts, and
  # cpu_instance_types becomes a map of groups at the same time, so a group is described
  # one way in both modules. It exists at all because clusters/eks grew one and this
  # module did not, and matching first makes the removal one change instead of two. The
  # written-out plan, including the key-naming rule that keeps an existing cluster's
  # resource addresses stable, is issue #502.
  #
  # Nodes per GPU group, which until this variable existed was the literal 1 in main.tf's
  # group builder. It is named, typed and defaulted to match clusters/eks so that moving
  # between the two modules does not mean relearning the surface.
  #
  # ADDING A KEY TO gpu_instance_types AND RAISING THIS ARE DIFFERENT PURCHASES, and the
  # distinction survives the unification: a key buys a group with its own platform and
  # preset, this buys more nodes of the shape a group already names. Accelerator quota is
  # granted per platform and preset, so a count above one can be refused where a second
  # key would not be.
  #
  # Unlike clusters/eks, this value DOES move a group that already exists: the node group
  # resource here declares no ignore_changes on its size, so an edit is applied rather
  # than silently dropped. That difference is upstream, not a choice made here.
  description = "Number of nodes in EACH GPU node group. Zero drops the GPU groups altogether, mirroring cpu_node_count. Unlike clusters/eks -- where the value reaches the cloud only at create time -- an edit here does resize a group that already exists."
  type        = number
  default     = 1

  validation {
    condition     = var.gpu_node_count >= 0 && var.gpu_node_count == floor(var.gpu_node_count)
    error_message = "gpu_node_count must be a whole number, zero or greater."
  }
}

# Keyed by group name so each GPU node group has a stable key (gpu-<name>), mirroring
# clusters/eks's gpu_instance_types map(list(string)) convention. Only platform + preset are
# required per group: os and drivers_preset are auto-resolved from Nebius' live compatibility
# matrix (`nebius mk8s node-group get-compatibility-matrix`) for the group's platform and
# var.release, picking the newest available driver preset. Set os/drivers_preset explicitly only
# to override that choice (e.g. pin an older CUDA preset); run the matrix query yourself to see
# the valid combinations (see README).
#
# preemptible buys the group's nodes from preemptible capacity: cheaper, and reclaimable by the
# platform at any time. mig declares whether the group's cards can be partitioned in hardware; it
# defaults to whether the platform appears in main.tf's mig_platforms list, and gates the
# MIG-specific node preparation (see README).
#
# public_ip gives the group's nodes a public IPv4, which is what makes them reachable over SSH —
# how the hardware-partition tests toggle MIG on the card — and so defaults to true for a GPU
# group. Each address is charged against the project's vpc.ipv4-address.public.count quota, so set
# it to false on a GPU group nobody logs in to; the CPU group has its own flag, off by default
# (see README).
variable "gpu_instance_types" {
  description = "GPU node groups keyed by group name (each becomes gpu-<name>). platform+preset are required; os and drivers_preset default to the newest match from `nebius mk8s node-group get-compatibility-matrix` for var.release; preemptible defaults to false; mig defaults to whether the platform supports NVIDIA MIG; public_ip defaults to true, giving the nodes an SSH-reachable public IPv4 at the cost of one public-address quota unit each; boot_disk_size_gb overrides var.node_boot_disk_size_gb for the group -- set it (e.g. 400) on groups that pull inference-engine images, which overflow the 100 GiB module default into kubelet disk pressure; infiniband_fabric attaches the group to an InfiniBand fabric, which is what gives its nodes RDMA devices, and requires a preset whose allow_gpu_clustering is true."
  type = map(object({
    platform       = string
    preset         = string
    os             = optional(string)
    drivers_preset = optional(string)
    preemptible    = optional(bool, false)
    mig            = optional(bool)
    public_ip      = optional(bool, true)
    # Name of the physical InfiniBand fabric to attach the group's nodes to, which is what puts
    # RDMA devices on the node. Unset (the default) leaves the group without them.
    #
    # Only a preset whose `allow_gpu_clustering` is true can be attached, and that is a property of
    # the preset rather than of the platform: on the same accelerator, the whole-node preset allows
    # it and the single-card preset does not. Ask the API which is which rather than guessing from
    # the name:
    #
    #   nebius compute platform list --parent-id <project> --format json \
    #     | jq -r '.items[] | .metadata.name as $p
    #              | .spec.presets[] | "\($p) \(.name) \(.allow_gpu_clustering)"'
    #
    # Fabric names are region-scoped, and a node can only join a fabric when it is created -- there
    # is no adding one afterwards -- so a group that needs RDMA must be created with it set.
    infiniband_fabric = optional(string)
    # Per-group override of var.node_boot_disk_size_gb. GPU nodes that pull inference-engine
    # images need far more than the 100 GiB module default -- one such image is tens of GiB,
    # and it shares the boot disk with the container runtime's layers, so a disk that is big
    # enough for the OS alone pushes the kubelet into disk pressure once the pulls start.
    boot_disk_size_gb = optional(number)
  }))
  default = {
    h100 = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }
  }

  validation {
    condition = alltrue([
      for cfg in values(var.gpu_instance_types) :
      cfg.boot_disk_size_gb == null || (cfg.boot_disk_size_gb > 0 && cfg.boot_disk_size_gb == floor(cfg.boot_disk_size_gb))
    ])
    error_message = "boot_disk_size_gb must be a positive whole number when set."
  }
}

variable "switch_kube_context" {
  # The cluster is merged into ~/.kube/config either way; this only decides whether a
  # bare kubectl points at it afterwards. Set it to false while another cluster is
  # mid-verification. Nothing is restored when there was no current context to begin
  # with (a kubeconfig that did not exist yet), so the merged one stays current there.
  description = "Whether `nebius mk8s cluster get-credentials` may leave this cluster as the current context. When false, the context that was current before it ran is restored."
  type        = bool
  default     = true
}
