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
  # (e.g. "1.33"). Nebius refuses a version within a month of its end of life -- the create call
  # fails with "k8s version <x> is deprecated and cannot be used" -- so this default tracks a
  # version that is still current, not the oldest one that works.
  description = "Kubernetes version for the cluster control plane and node groups, e.g. '1.33'."
  type        = string
  default     = "1.33"
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

# The (singular) CPU node group's shape, mirroring clusters/eks's cpu_instance_types. No
# image_family: unlike a standalone compute VM (computes/nebius), the mk8s node template picks
# its image from `os` alone for a driverless (CPU) platform.
variable "cpu_instance_types" {
  description = "Instance type for the CPU node group: platform/preset (see the region table above) plus os."
  type = object({
    platform = string
    preset   = string
    os       = string
  })
  default = { platform = "cpu-e2", preset = "4vcpu-16gb", os = "ubuntu24.04" }
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
variable "gpu_instance_types" {
  description = "GPU node groups keyed by group name (each becomes gpu-<name>). platform+preset are required; os and drivers_preset default to the newest match from `nebius mk8s node-group get-compatibility-matrix` for var.release; preemptible defaults to false; mig defaults to whether the platform supports NVIDIA MIG."
  type = map(object({
    platform       = string
    preset         = string
    os             = optional(string)
    drivers_preset = optional(string)
    preemptible    = optional(bool, false)
    mig            = optional(bool)
  }))
  default = {
    h100 = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }
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
