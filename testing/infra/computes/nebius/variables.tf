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
