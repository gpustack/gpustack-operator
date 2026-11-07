variable "name_prefix" {
  description = "Prefix for the explicitly named cluster nodes."
  type        = string
  default     = "gpustack-spheron"
}

variable "team_id" {
  description = "Optional Spheron team ID."
  type        = string
  default     = null
}

variable "ssh_public_key" {
  description = "Path to the public key installed on every instance."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "ssh_private_key" {
  description = "Local private-key path passed to the second-stage k3s module, never sent to Spheron."
  type        = string
  default     = "~/.ssh/id_ed25519"
}

variable "node_internal_ip" {
  description = "Explicit SSH-IP to cluster-IP mapping for the k3s stage. No private network or RDMA connectivity is inferred."
  type        = map(string)
  default     = {}
}

variable "nodes" {
  description = "Stable node names mapped to server/agent roles and exact catalog selections."
  type = map(object({
    role            = string
    request_key     = string
    volume_ids      = optional(list(string), [])
    max_hourly_rate = optional(number)
    instance_type = object({
      provider         = string
      offer_id         = string
      gpu_type         = string
      gpu_count        = number
      region           = string
      operating_system = string
      preemptible      = optional(bool, false)
    })
  }))
  validation {
    condition     = length([for node in var.nodes : node if node.role == "server"]) >= 1 && alltrue([for node in var.nodes : contains(["server", "agent"], node.role)])
    error_message = "At least one server is required and each role must be server or agent."
  }
  validation {
    condition     = length(distinct([for node in var.nodes : node.request_key])) == length(var.nodes)
    error_message = "Each node must have a distinct allocation request_key."
  }
}
