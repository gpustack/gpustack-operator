# region -> available platforms (region is fixed by parent_id's project, NOT a TF variable):
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
variable "parent_id" {
  description = "Nebius project ID; its region fixes VM placement and platform availability (see the region table above)."
  type        = string

  validation {
    condition     = can(regex("^project-", var.parent_id))
    error_message = "parent_id must be a Nebius project ID, e.g. 'project-...'."
  }
}

variable "vm_name_prefix" {
  description = "Prefix for the VM and its network/subnet/security-group names (a random suffix is appended)."
  type        = string
  default     = "gpustack-nebius"
}

variable "ssh_source_cidrs" {
  description = "CIDR blocks allowed to SSH (TCP/22) into the VM."
  type        = list(string)
  default     = ["0.0.0.0/0"]

  validation {
    condition     = alltrue([for c in var.ssh_source_cidrs : can(cidrhost(c, 0))])
    error_message = "ssh_source_cidrs must be valid CIDR blocks (e.g. '0.0.0.0/0')."
  }
}

variable "ssh_public_key" {
  # pathexpand() handles the leading "~".
  description = "Path to the SSH public key injected into the VM via cloud-init."
  type        = string
  default     = "~/.ssh/id_rsa.pub"

  validation {
    condition     = fileexists(pathexpand(var.ssh_public_key))
    error_message = "ssh_public_key must point to a file that exists on disk."
  }
}

variable "ssh_username" {
  # Interpolated unquoted into the cloud-init YAML and the ssh_command output, so it's
  # restricted to safe Linux username characters to avoid breaking either.
  description = "Username created on the VM with the SSH public key as an authorized key."
  type        = string
  default     = "ubuntu"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_-]*$", var.ssh_username))
    error_message = "ssh_username must be a valid Linux username (lowercase letters, digits, underscores, hyphens; starting with a letter or underscore)."
  }
}

# platform -> preset.name  (pick a matching pair; an invalid platform/preset combo fails at apply).
# Default: cpu-e2 / 2vcpu-8gb (smallest CPU box; eu-north1 only -- for other regions use cpu-d3).
#
# CPU:
#   cpu-d3  [AMD Epyc Genoa, all regions] : 4vcpu-16gb, 8vcpu-32gb, 16vcpu-64gb, 32vcpu-128gb,
#                                           48vcpu-192gb, 64vcpu-256gb, 96vcpu-384gb, 128vcpu-512gb
#   cpu-e2  [Intel Ice Lake, eu-north1]   : 2vcpu-8gb, 4vcpu-16gb, 8vcpu-32gb, 16vcpu-64gb,
#                                           32vcpu-128gb, 48vcpu-192gb, 64vcpu-256gb, 80vcpu-320gb
# GPU:
#   gpu-h100-sxm   [H100 NVLink]  : 1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb
#   gpu-h200-sxm   [H200 NVLink]  : 1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb
#   gpu-l40s-a     [L40S/IceLake] : 1gpu-8vcpu-32gb, 1gpu-16vcpu-64gb, 1gpu-24vcpu-96gb,
#                                   1gpu-32vcpu-128gb, 1gpu-40vcpu-160gb
#   gpu-l40s-d     [L40S/Genoa]   : 1gpu-16vcpu-96gb, 1gpu-32vcpu-192gb, 1gpu-48vcpu-288gb,
#                                   2gpu-64vcpu-384gb, 2gpu-96vcpu-576gb, 4gpu-128vcpu-768gb,
#                                   4gpu-192vcpu-1152gb
#   gpu-b200-sxm   [B200 NVLink]  : 1gpu-20vcpu-224gb, 8gpu-160vcpu-1792gb
#   gpu-b200-sxm-a [B200 NVLink]  : 1gpu-20vcpu-224gb, 8gpu-160vcpu-1792gb
#   gpu-b300-sxm   [B300 NVLink]  : 1gpu-24vcpu-346gb, 8gpu-192vcpu-2768gb
#   gpu-rtx6000    [RTX PRO 6000] : 1gpu-24vcpu-218gb, 8gpu-192vcpu-1744gb
variable "platform" {
  description = "Nebius compute platform (see the platform -> preset table above); availability depends on parent_id's region."
  type        = string
  default     = "cpu-e2"
}

variable "preset" {
  description = "Nebius compute preset (vCPU/RAM shape) matching var.platform (see the platform -> preset table above)."
  type        = string
  default     = "2vcpu-8gb"
}

variable "image_family" {
  description = "Boot disk source image family."
  type        = string
  default     = "ubuntu24.04"
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

variable "boot_disk_size_gibibytes" {
  description = "Boot disk size, in GiB."
  type        = number
  default     = 64

  validation {
    condition     = var.boot_disk_size_gibibytes > 0 && var.boot_disk_size_gibibytes == floor(var.boot_disk_size_gibibytes)
    error_message = "boot_disk_size_gibibytes must be a positive whole number."
  }
}
