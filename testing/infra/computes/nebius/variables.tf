# region -> available platforms (region is fixed by project_id's project, NOT a TF variable):
#   eu-north1   : cpu-d3, cpu-e2, gpu-h100-sxm, gpu-h200-sxm, gpu-l40s-a, gpu-l40s-d
#   eu-west1    : cpu-d3, gpu-h200-sxm
#   me-west1    : cpu-d3, gpu-b200-sxm-a
#   uk-south1   : cpu-d3, gpu-b300-sxm
#   us-central1 : cpu-d3, gpu-b200-sxm, gpu-h200-sxm, gpu-rtx6000
#
# Example -- eu-north1 (default region, richest availability), platform -> presets:
#   cpu-e2       : 2vcpu-8gb (default), 4vcpu-16gb, ... 80vcpu-320gb
#   cpu-d3       : 4vcpu-16gb, ... 128vcpu-512gb
#   gpu-h100-sxm : 1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb
#   gpu-h200-sxm : 1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb
#   gpu-l40s-a   : 1gpu-8vcpu-32gb, ... 1gpu-40vcpu-160gb
#   gpu-l40s-d   : 1gpu-16vcpu-96gb, ... 4gpu-192vcpu-1152gb
variable "project_id" {
  description = "Nebius project ID; its region fixes VM placement and platform availability (see the region table above)."
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

# platform / preset / image_family combos (pick one matching triple; an invalid combo fails at apply).
# project_id's region gates platform availability (see the region table above).
# Default: gpu-h100-sxm / 1gpu-16vcpu-200gb / ubuntu24.04-cuda13.0.
#
# | Platform         | Notes                    | Presets                                                | Image family          |
# |------------------|--------------------------|---------------------------------------------------------|-----------------------|
# | cpu-d3           | AMD Epyc Genoa, all regions | 4vcpu-16gb, 8vcpu-32gb, 16vcpu-64gb, 32vcpu-128gb, 48vcpu-192gb, 64vcpu-256gb, 96vcpu-384gb, 128vcpu-512gb | ubuntu24.04-driverless |
# | cpu-e2           | Intel Ice Lake, eu-north1 only | 2vcpu-8gb, 4vcpu-16gb, 8vcpu-32gb, 16vcpu-64gb, 32vcpu-128gb, 48vcpu-192gb, 64vcpu-256gb, 80vcpu-320gb | ubuntu24.04-driverless |
# | gpu-h100-sxm     | H100 NVLink              | 1gpu-16vcpu-200gb (default), 8gpu-128vcpu-1600gb        | ubuntu24.04-cuda13.0 (default) |
# | gpu-h200-sxm     | H200 NVLink              | 1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb                  | ubuntu24.04-cuda13.0  |
# | gpu-l40s-a       | L40S / Ice Lake          | 1gpu-8vcpu-32gb, 1gpu-16vcpu-64gb, 1gpu-24vcpu-96gb, 1gpu-32vcpu-128gb, 1gpu-40vcpu-160gb | ubuntu24.04-cuda13.0  |
# | gpu-l40s-d       | L40S / Genoa             | 1gpu-16vcpu-96gb, 1gpu-32vcpu-192gb, 1gpu-48vcpu-288gb, 2gpu-64vcpu-384gb, 2gpu-96vcpu-576gb, 4gpu-128vcpu-768gb, 4gpu-192vcpu-1152gb | ubuntu24.04-cuda13.0  |
# | gpu-b200-sxm     | B200 NVLink              | 1gpu-20vcpu-224gb, 8gpu-160vcpu-1792gb                  | ubuntu24.04-cuda13.0  |
# | gpu-b200-sxm-a   | B200 NVLink              | 1gpu-20vcpu-224gb, 8gpu-160vcpu-1792gb                  | ubuntu24.04-cuda13.0  |
# | gpu-b300-sxm     | B300 NVLink              | 1gpu-24vcpu-346gb, 8gpu-192vcpu-2768gb                  | ubuntu24.04-cuda13.0 (cuda12 families unsupported here, need nvidia_gpu_drivers 580.x) |
# | gpu-rtx6000      | RTX PRO 6000             | 1gpu-24vcpu-218gb, 8gpu-192vcpu-1744gb                  | ubuntu24.04-cuda13.0 (cuda12 families unsupported here, need nvidia_gpu_drivers 580.x) |
variable "instance_type" {
  # The Nebius platform/preset/image_family combination that defines the instance shape.
  description = "Nebius instance type: the platform/preset/image_family combination (see the table above); pick a matching combo."
  type        = object({ platform = string, preset = string, image_family = string })
  default = {
    platform     = "gpu-h100-sxm"
    preset       = "1gpu-16vcpu-200gb"
    image_family = "ubuntu24.04-cuda13.0"
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
}
