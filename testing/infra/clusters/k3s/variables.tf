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

variable "cluster_cidr" {
  # Defaults to k3s's own default. Passed to every server as --cluster-cidr;
  # agents inherit it from the server. Also scopes the post-uninstall route
  # flush that clears stale flannel host-gw routes, so a custom value stays
  # consistent between the running pod network and the cleanup.
  description = "Pod (cluster) network CIDR, passed to k3s servers as --cluster-cidr. Comma-separate two CIDRs for dual-stack."
  type        = string
  default     = "10.42.0.0/16"

  validation {
    condition     = alltrue([for c in split(",", var.cluster_cidr) : can(cidrhost(c, 0))])
    error_message = "cluster_cidr must be a CIDR, or comma-separated CIDRs for dual-stack (e.g. '10.42.0.0/16')."
  }
}

variable "service_cidr" {
  # Defaults to k3s's own default. Passed to every server as --service-cidr;
  # agents inherit it from the server.
  description = "Service network CIDR, passed to k3s servers as --service-cidr. Comma-separate two CIDRs for dual-stack."
  type        = string
  default     = "10.43.0.0/16"

  validation {
    condition     = alltrue([for c in split(",", var.service_cidr) : can(cidrhost(c, 0))])
    error_message = "service_cidr must be a CIDR, or comma-separated CIDRs for dual-stack (e.g. '10.43.0.0/16')."
  }
}

variable "server_https_listen_port" {
  # Passed to every server install as --https-listen-port; threaded through the
  # server URL, join/agent readiness probes, and the kubeconfig server-URL rewrite.
  description = "Kubernetes apiserver (HTTPS) port, passed to k3s servers as --https-listen-port."
  type        = number
  default     = 6443

  validation {
    condition     = var.server_https_listen_port == floor(var.server_https_listen_port) && var.server_https_listen_port >= 1 && var.server_https_listen_port <= 65535
    error_message = "server_https_listen_port must be a whole number between 1 and 65535."
  }
}

variable "service_node_port_range" {
  # Passed to every server install as --service-node-port-range.
  description = "NodePort Service port range, passed to k3s servers as --service-node-port-range (format '<lo>-<hi>')."
  type        = string
  default     = "30000-32767"

  validation {
    condition     = can(regex("^[0-9]+-[0-9]+$", var.service_node_port_range))
    error_message = "service_node_port_range must be in the form '<lo>-<hi>' (e.g. '30000-32767')."
  }

  validation {
    # Short-circuits on the shape check first, so a malformed value (already caught by the
    # validation above) never reaches tonumber() and crashes instead of failing cleanly.
    condition = can(regex("^[0-9]+-[0-9]+$", var.service_node_port_range)) && (
      tonumber(split("-", var.service_node_port_range)[0]) >= 1 &&
      tonumber(split("-", var.service_node_port_range)[1]) <= 65535 &&
      tonumber(split("-", var.service_node_port_range)[0]) <= tonumber(split("-", var.service_node_port_range)[1])
    )
    error_message = "service_node_port_range bounds must satisfy 1 <= lo <= hi <= 65535."
  }
}

variable "switch_kube_context" {
  # The cluster is merged into ~/.kube/config either way; this only decides whether a
  # bare kubectl points at it afterwards. Set it to false while another cluster is
  # mid-verification. Nothing is restored when there was no current context to begin
  # with (a kubeconfig that did not exist yet), so the merged one stays current there.
  description = "Whether merging this cluster into ~/.kube/config may leave it as the current context. When false, the context that was current before the merge is restored."
  type        = bool
  default     = true
}
