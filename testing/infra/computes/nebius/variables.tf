# The platform/preset/image_family truth is the API's, not this file's. These two commands print it:
#
#   region=$(nebius iam project get --id <project_id> --format json | jq -r .spec.region)
#   nebius compute platform list --parent-id <project_id> --format json \
#     | jq -r '.items[] | "\(.metadata.name): \([.spec.presets[].name] | join(", "))"'
#   nebius compute image list-public --region "$region" --format json \
#     | jq -r '.items[] | select((.spec.recommended_platforms // []) | length > 0)
#             | "\(.spec.image_family) <- \(.spec.recommended_platforms | join(", "))"'
#
# The module runs the same three calls itself when instance_type.image_family is null, so a platform
# Nebius adds tomorrow works with no change here. A SNAPSHOT of eu-north1 as of 2026-08-19, for
# orientation only -- do not treat it as current:
#
#   cpu-d3, cpu-e2                                  -> ubuntu24.04-driverless   (AMD64, min 10 GiB)
#   gpu-h100-sxm, gpu-h200-sxm, gpu-l40s-a/-d,
#   gpu-b200-sxm/-a, gpu-b300-sxm, gpu-rtx6000/-a   -> ubuntu24.04-cuda13.0     (AMD64, min 40 GiB)
#   gpu-gb200, gpu-gb300                            -> ubuntu24.04-cuda13.0-arm64 (ARM64, min 40 GiB)
#
variable "project_id" {
  description = "Nebius project ID; its region fixes VM placement and which platforms are available."
  type        = string

  validation {
    condition     = can(regex("^project-", var.project_id))
    error_message = "project_id must be a Nebius project ID, e.g. 'project-...'."
  }
}

variable "name_prefix" {
  description = "Prefix for the VM and its network/subnet/security-group names (a random suffix is appended)."
  type        = string
  default     = "gpustack-nebius"
}

variable "ssh_public_key" {
  # pathexpand() handles the leading "~".
  description = "Path to the SSH public key injected into the VM via cloud-init."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"

  validation {
    condition     = fileexists(pathexpand(var.ssh_public_key))
    error_message = "ssh_public_key must point to a file that exists on disk."
  }
}

variable "instance_type" {
  # The platform/preset pair defines the instance shape. image_family is optional: left null, the
  # module resolves it from the platform against Nebius' live public-image catalogue, which is the only
  # place that mapping is actually true. Pinning it skips the lookup entirely -- see the README for
  # what that opts out of.
  description = "Nebius instance type. platform/preset are required; image_family is optional and resolved from the live image catalogue when null; gpu_cluster_id is optional and joins the VM to an already-existing GPU cluster's InfiniBand fabric."
  type = object({
    platform     = string
    preset       = string
    image_family = optional(string)
    # ID of an already-existing GPU cluster in the project (a "computegpucluster-..." resource)
    # whose InfiniBand fabric the VM joins at creation -- the thing that puts RDMA devices on it.
    # The API accepts this only when the VM is created (never afterwards), and only for a preset
    # whose allow_gpu_clustering is true; preparation checks the preset even for a pinned image.
    gpu_cluster_id            = optional(string)
    infiniband_fabric         = optional(string)
    reuse_gpu_cluster         = optional(bool, true)
    fabric_selection_revision = optional(string, "")
    preemptible               = optional(bool, false)
    spot_price_from_history   = optional(bool, false)
    spot_price_ceiling        = optional(string)
    spot_max_price            = optional(string)
    spot_pricing_policy_id    = optional(string)
  })
  default = {
    platform = "gpu-h100-sxm"
    preset   = "1gpu-16vcpu-200gb"
  }

  validation {
    condition     = var.instance_type.gpu_cluster_id == null || var.instance_type.infiniband_fabric == null
    error_message = "gpu_cluster_id and infiniband_fabric are mutually exclusive."
  }

  validation {
    # Null is "resolve it"; empty is neither a family nor a request to look one up, and it would skip
    # the lookup and reach the provider as a blank image reference.
    condition     = try(var.instance_type.image_family, null) != ""
    error_message = "instance_type.image_family must be an image family name, or left out so the module resolves it from the platform. An empty string is neither."
  }

  validation {
    condition     = var.instance_type.platform != "" && var.instance_type.preset != ""
    error_message = "instance_type.platform and instance_type.preset must both be set; they are what the image family and the machine shape are resolved from."
  }

  validation {
    condition     = var.instance_type.gpu_cluster_id == null || can(regex("^computegpucluster-", var.instance_type.gpu_cluster_id))
    error_message = "instance_type.gpu_cluster_id must be a Nebius GPU cluster ID, e.g. 'computegpucluster-...'."
  }
  validation {
    condition = (
      (var.instance_type.spot_max_price == null || (can(regex("^[0-9]+(\\.[0-9]{1,3})?$", var.instance_type.spot_max_price)) && try(tonumber(var.instance_type.spot_max_price) > 0, false))) &&
      (var.instance_type.spot_max_price == null || var.instance_type.spot_pricing_policy_id == null) &&
      (var.instance_type.preemptible || (var.instance_type.spot_max_price == null && var.instance_type.spot_pricing_policy_id == null && !var.instance_type.spot_price_from_history)) &&
      (!var.instance_type.spot_price_from_history || (var.instance_type.spot_max_price == null && var.instance_type.spot_pricing_policy_id == null)) &&
      (var.instance_type.spot_price_ceiling == null || (var.instance_type.spot_price_from_history && (can(regex("^[0-9]+(\\.[0-9]{1,3})?$", var.instance_type.spot_price_ceiling)) && try(tonumber(var.instance_type.spot_price_ceiling) > 0, false))))
    )
    error_message = "Select at most one of spot_max_price, spot_pricing_policy_id, and spot_price_from_history on a preemptible instance. Prices must be positive decimals with at most three fractional digits; the ceiling requires history pricing."
  }

}

variable "boot_disk_type" {
  description = "Boot disk type, one of 'NETWORK_SSD', 'NETWORK_HDD', 'NETWORK_SSD_NON_REPLICATED', 'NETWORK_SSD_IO_M3'."
  type        = string
  default     = "NETWORK_SSD"

  validation {
    condition     = contains(["NETWORK_SSD", "NETWORK_HDD", "NETWORK_SSD_NON_REPLICATED", "NETWORK_SSD_IO_M3"], var.boot_disk_type)
    error_message = "boot_disk_type must be one of: NETWORK_SSD, NETWORK_HDD, NETWORK_SSD_NON_REPLICATED, NETWORK_SSD_IO_M3."
  }
}

variable "boot_disk_size_gb" {
  description = "Boot disk size, in GiB."
  type        = number
  default     = 100

  validation {
    condition     = var.boot_disk_size_gb > 0 && var.boot_disk_size_gb == floor(var.boot_disk_size_gb)
    error_message = "boot_disk_size_gb must be a positive whole number."
  }

  validation {
    # Nebius' non-replicated and IO_M3 disks are allocated in 93 GiB units, and NETWORK_SSD tops out at
    # 8192 GiB. Enforced here so a size the API will reject fails at plan; the image's own minimum is a
    # separate check, on the instance, because it takes a live lookup.
    condition = (
      contains(["NETWORK_SSD_NON_REPLICATED", "NETWORK_SSD_IO_M3"], var.boot_disk_type)
      ? var.boot_disk_size_gb >= 93 && var.boot_disk_size_gb % 93 == 0
      : var.boot_disk_type != "NETWORK_SSD" || (var.boot_disk_size_gb >= 1 && var.boot_disk_size_gb <= 8192)
    )
    error_message = "NETWORK_SSD_NON_REPLICATED and NETWORK_SSD_IO_M3 are allocated in whole 93 GiB units (93, 186, 279, ...); NETWORK_SSD must be 1-8192 GiB."
  }
}

variable "public_ip" {
  description = "Allocate a public IPv4 address for the VM."
  type        = bool
  default     = true
}

variable "nebius_resolved" {
  description = "Internal snapshot written by null_resource.prepare: fingerprint, project_id, region, prices by group, and bindings by group."
  type        = any
  default     = null
}
