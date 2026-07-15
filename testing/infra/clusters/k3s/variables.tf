variable "server" {
  description = "K3s server (control-plane) hosts, each given as 'user@host' or 'host'."
  type        = list(string)

  validation {
    condition     = length(var.server) >= 1
    error_message = "At least one server address must be provided in var.server."
  }

  validation {
    condition     = length(var.server) == length(distinct([for s in var.server : strcontains(s, "@") ? split("@", s)[1] : s]))
    error_message = "Duplicate server hosts are not allowed; each host keys one node, and addresses differing only by SSH user still collide."
  }
}

variable "agent" {
  description = "K3s agent (worker) hosts, each given as 'user@host' or 'host'. When empty, the servers run workloads themselves."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.agent) == length(distinct([for a in var.agent : strcontains(a, "@") ? split("@", a)[1] : a]))
    error_message = "Duplicate agent hosts are not allowed; each host keys one node, and addresses differing only by SSH user still collide."
  }
}

variable "server_ssh_port" {
  description = "SSH port used to reach the server hosts."
  type        = number
  default     = 22
}

variable "agent_ssh_port" {
  description = "SSH port used to reach the agent hosts."
  type        = number
  default     = 22
}

variable "ssh_user" {
  description = "Fallback SSH user for host addresses given without a 'user@' prefix."
  type        = string
  default     = "root"
}

variable "ssh_private_key" {
  # Defaults to an ed25519 key: Terraform's SSH provisioner signs RSA keys with
  # ssh-rsa (SHA-1), which modern OpenSSH servers (8.8+) reject.
  description = "Path to the private key used for SSH connections to the hosts."
  type        = string
  default     = "~/.ssh/id_ed25519"
}

variable "release" {
  # Named "release" because Terraform reserves the variable name "version".
  description = "K3s release tag as published at https://github.com/k3s-io/k3s/releases, e.g. 'v1.34.9+k3s1'. Passed directly to the installer as INSTALL_K3S_VERSION."
  type        = string
  default     = "v1.34.9+k3s1"
}

variable "flannel_backend" {
  # host-gw routes pod traffic directly (no VXLAN encapsulation) for faster
  # node-to-node networking, but requires every node on one L2 segment; keep the
  # "vxlan" default when nodes may be routed across subnets.
  description = "K3s flannel backend, one of 'vxlan' (default, works across subnets), 'host-gw' (faster, requires all nodes on one L2 segment), 'wireguard-native', or 'none'."
  type        = string
  default     = "vxlan"

  validation {
    condition     = contains(["vxlan", "host-gw", "wireguard-native", "none"], var.flannel_backend)
    error_message = "flannel_backend must be one of: vxlan, host-gw, wireguard-native, none."
  }
}
