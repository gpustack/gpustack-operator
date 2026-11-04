variable "name_prefix" {
  description = "Name for this Spheron deployment. Use distinct names for each node."
  type        = string
  default     = "gpustack-spheron"
}

variable "request_key" {
  description = "Unique key for this allocation. Reuse after unknown outcomes; change for replacement, including after termination or preemption."
  type        = string
  validation {
    condition     = can(regex("^[A-Za-z0-9_.:-]{8,200}$", var.request_key))
    error_message = "request_key must contain 8-200 letters, digits, or _.:-."
  }
}

variable "team_id" {
  description = "Optional Spheron team ID. Authentication comes from SPHERON_API_KEY."
  type        = string
  default     = null
}

variable "instance_type" {
  description = "Exact catalog selection. The offer fixes CPU, RAM, disk and GPU shape; region is one value from its clusters field."
  type = object({
    provider         = string
    offer_id         = string
    gpu_type         = string
    gpu_count        = number
    region           = string
    operating_system = string
    preemptible      = optional(bool, false)
  })
  validation {
    condition     = var.instance_type.gpu_count >= 0 && floor(var.instance_type.gpu_count) == var.instance_type.gpu_count && ((var.instance_type.gpu_type == "CPU") == (var.instance_type.gpu_count == 0))
    error_message = "GPU instances need a positive integer gpu_count; CPU instances need gpu_type CPU and gpu_count zero."
  }
}

variable "ssh_public_key" {
  description = "Path to an existing SSH public key. Its content is sent at creation."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "cloud_init" {
  description = "Optional cloudInit object; only supported when the selected offer advertises supportsCloudInit. Do not include secrets."
  type = object({
    runcmd   = optional(list(string))
    packages = optional(list(string))
    writeFiles = optional(list(object({
      path        = string
      content     = string
      owner       = optional(string)
      permissions = optional(string)
    })))
  })
  default = null
}

variable "volume_ids" {
  description = "Existing volume IDs compatible with the offer provider and region."
  type        = list(string)
  default     = []
}

variable "max_hourly_rate" {
  description = "Whole-instance USD/hour cap, only for Spheron ES live-priced spot offers."
  type        = number
  default     = null
  validation {
    condition     = var.max_hourly_rate == null ? true : var.max_hourly_rate >= 0.01 && var.max_hourly_rate <= 1000 && var.instance_type.provider == "spheron-es" && var.instance_type.preemptible
    error_message = "A price cap requires Spheron ES spot and a value between 0.01 and 1000."
  }
}
