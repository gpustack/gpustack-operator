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
  # (e.g. "1.31"); 1.31+ is required for the cuda12.8/ubuntu24.04 GPU driver preset.
  description = "Kubernetes version for the cluster control plane and node groups, e.g. '1.31'."
  type        = string
  default     = "1.31"
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

# Each node group becomes a nebius_mk8s_v1_node_group named after its map key (a "cpu" group and
# any number of GPU groups, mirroring clusters/eks's gpu-<name> convention -- prefix a GPU key
# with "gpu-" yourself, e.g. "gpu-h100"). instance_type has no image_family: unlike a standalone
# compute VM (computes/nebius), the mk8s node template picks its image from `os` + (for GPU groups)
# `gpu.drivers_preset`, per the platform/Kubernetes-version/os/driver matrix:
#
#   gpu-l40s-a, gpu-l40s-d, gpu-h100-sxm, gpu-h200-sxm, cpu-e1, cpu-e2, cpu-d3:
#     drivers_preset = ""       : 1.30 -> os "ubuntu22.04" | 1.31 -> "ubuntu22.04" (default), "ubuntu24.04"
#   gpu-l40s-a, gpu-l40s-d, gpu-h100-sxm, gpu-h200-sxm:
#     drivers_preset = "cuda12"   (CUDA 12.4) : 1.30, 1.31 -> os "ubuntu22.04"
#     drivers_preset = "cuda12.4"             : 1.31       -> os "ubuntu22.04"
#     drivers_preset = "cuda12.8"             : 1.31       -> os "ubuntu24.04"
#   gpu-b200-sxm:
#     drivers_preset = ""         : 1.30, 1.31 -> os "ubuntu24.04"
#     drivers_preset = "cuda12"   (CUDA 12.8) : 1.30, 1.31 -> os "ubuntu24.04"
#     drivers_preset = "cuda12.8"             : 1.31       -> os "ubuntu24.04"
#   gpu-b200-sxm-a:
#     drivers_preset = ""                     : 1.31 -> os "ubuntu24.04"
#     drivers_preset = "cuda12.8"              : 1.31 -> os "ubuntu24.04"
variable "node_groups" {
  description = "Node groups keyed by name; instance_type/os per the platform/Kubernetes-version/os/driver matrix above, gpu.drivers_preset required for GPU platforms."
  type = map(object({
    instance_type = object({
      platform = string
      preset   = string
    })
    os               = string
    fixed_node_count = number
    gpu = optional(object({
      drivers_preset = string
    }))
  }))
  default = {
    cpu = {
      instance_type    = { platform = "cpu-e2", preset = "4vcpu-16gb" }
      os               = "ubuntu24.04"
      fixed_node_count = 1
    }
    gpu-h100 = {
      instance_type    = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }
      os               = "ubuntu24.04"
      fixed_node_count = 1
      gpu              = { drivers_preset = "cuda12.8" }
    }
  }
}
