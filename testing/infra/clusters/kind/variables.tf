variable "host" {
  description = "Remote Docker host where the Kind cluster runs, given as 'user@host' or 'host'."
  type        = string

  validation {
    # Spliced into an ssh command line and, for the jumper case, into a ProxyCommand a shell
    # executes with no escaping. The colon is allowed for IPv6 literals.
    condition     = can(regex("^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$", var.host))
    error_message = "host must be 'host' or 'user@host' using [A-Za-z0-9._:-] (it is spliced into an ssh command line)."
  }
}

variable "ssh_port" {
  description = "SSH port used to reach the remote Docker host."
  type        = number
  default     = 22

  validation {
    condition     = var.ssh_port == floor(var.ssh_port) && var.ssh_port >= 1 && var.ssh_port <= 65535
    error_message = "ssh_port must be a whole number between 1 and 65535."
  }
}

variable "ssh_user" {
  description = "Fallback SSH user when host is given without a 'user@' prefix."
  type        = string
  default     = "root"

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]+$", var.ssh_user))
    error_message = "ssh_user must be made of [A-Za-z0-9._-] (it is spliced into an ssh command line)."
  }
}

variable "ssh_private_key" {
  # Defaults to an ed25519 key: Terraform's SSH provisioner signs RSA keys with
  # ssh-rsa (SHA-1), which modern OpenSSH servers (8.8+) reject.
  description = "Path to the private key used for SSH connections to the host (and to the jump host)."
  type        = string
  default     = "~/.ssh/id_ed25519"

  validation {
    condition     = can(regex("^[A-Za-z0-9._@+:/~-]+$", var.ssh_private_key))
    error_message = "ssh_private_key must be a path made of [A-Za-z0-9._@+:/~-] (it is spliced into an ssh ProxyCommand)."
  }
}

variable "ssh_jumper" {
  description = "Optional SSH jump host for reaching var.host, as 'user@host' or 'host'. Empty means direct connection."
  type        = string
  default     = ""

  validation {
    condition     = var.ssh_jumper == "" || can(regex("^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$", var.ssh_jumper))
    error_message = "ssh_jumper must be 'host' or 'user@host' using [A-Za-z0-9._:-] (it is spliced into an ssh ProxyCommand)."
  }
}

variable "ssh_jumper_port" {
  description = "SSH port used to reach the jump host."
  type        = number
  default     = 22

  validation {
    condition     = var.ssh_jumper_port == floor(var.ssh_jumper_port) && var.ssh_jumper_port >= 1 && var.ssh_jumper_port <= 65535
    error_message = "ssh_jumper_port must be a whole number between 1 and 65535."
  }
}

variable "cluster_name" {
  description = "Name of the Kind cluster created on the remote Docker host."
  type        = string
  default     = "gpustack-kind"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", var.cluster_name))
    error_message = "cluster_name must be a valid DNS label matching ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$."
  }
}

variable "release" {
  # Named "release" because Terraform reserves the variable name "version".
  # Accepts any Kubernetes semver tag published as kindest/node:<tag> (with optional @sha256: digest),
  # e.g. 'v1.31.14', 'v1.34.11', 'v1.35.8', 'v1.37.0@sha256:...'.
  description = "Kubernetes release tag for kindest/node, e.g. 'v1.34.11', 'v1.35.8', or 'v1.34.11@sha256:...'. Ignored when node_image is explicitly set."
  type        = string
  default     = "v1.34.11"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+(-[A-Za-z0-9.-]+)?(@sha256:[0-9a-f]{64})?$", var.release))
    error_message = "release must be a Kubernetes semver tag like 'v1.34.11' (optionally pinned with '@sha256:<64-hex>')."
  }
}

variable "node_image_repository" {
  description = "OCI repository used with var.release to build the Kind node image reference (default 'kindest/node')."
  type        = string
  default     = "kindest/node"

  validation {
    condition     = can(regex("^[A-Za-z0-9._:/-]+$", var.node_image_repository))
    error_message = "node_image_repository must contain only [A-Za-z0-9._:/-]."
  }
}

variable "node_image" {
  description = "Optional full Kind node image override (e.g. 'kindest/node:v1.34.11@sha256:...'). When non-empty, supersedes node_image_repository and release."
  type        = string
  default     = ""

  validation {
    condition     = var.node_image == "" || can(regex("^[A-Za-z0-9._:@/-]+$", var.node_image))
    error_message = "node_image must be empty or a valid OCI image reference using [A-Za-z0-9._:@/-]."
  }
}

variable "kind_version" {
  description = "Kind CLI release version installed on the remote host when missing or different, e.g. 'v0.33.0'."
  type        = string
  default     = "v0.33.0"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+(-[A-Za-z0-9.-]+)?$", var.kind_version))
    error_message = "kind_version must be a release tag of the form 'vX.Y.Z'."
  }
}

variable "control_planes" {
  description = "Number of Kind control-plane nodes."
  type        = number
  default     = 1

  validation {
    condition     = var.control_planes == floor(var.control_planes) && var.control_planes >= 1
    error_message = "control_planes must be a whole number >= 1."
  }
}

variable "workers" {
  description = "Number of Kind worker nodes (0 runs workloads on the control-plane; 2..32+ builds multi-node clusters)."
  type        = number
  default     = 3

  validation {
    condition     = var.workers == floor(var.workers) && var.workers >= 0
    error_message = "workers must be a whole number >= 0."
  }
}

variable "topology_region" {
  description = "Value for topology.kubernetes.io/region stamped on worker nodes. Empty disables automatic topology labels."
  type        = string
  default     = "kind-region"

  validation {
    condition     = var.topology_region == "" || can(regex("^[A-Za-z0-9._-]+$", var.topology_region))
    error_message = "topology_region must be empty or made of [A-Za-z0-9._-]."
  }
}

variable "topology_zones" {
  description = "Number of availability zones (zone-1..zone-Z) distributed round-robin across worker nodes when topology_region is non-empty."
  type        = number
  default     = 2

  validation {
    condition     = var.topology_zones == floor(var.topology_zones) && var.topology_zones >= 1
    error_message = "topology_zones must be a whole number >= 1."
  }
}

variable "topology_racks_per_zone" {
  description = "Number of racks per zone (strictly nested region -> zone -> rack for Kueue TAS) when topology_region is non-empty."
  type        = number
  default     = 2

  validation {
    condition     = var.topology_racks_per_zone == floor(var.topology_racks_per_zone) && var.topology_racks_per_zone >= 1
    error_message = "topology_racks_per_zone must be a whole number >= 1."
  }
}

variable "api_server_address" {
  description = "Bind address for Kind's exposed Kubernetes API server port on the remote host (default '0.0.0.0' so this workstation can reach it directly)."
  type        = string
  default     = "0.0.0.0"

  validation {
    condition     = can(cidrhost("${var.api_server_address}/32", 0)) || can(cidrhost("${var.api_server_address}/128", 0))
    error_message = "api_server_address must be an IPv4 or IPv6 literal (e.g. '0.0.0.0' or '127.0.0.1')."
  }
}

variable "api_server_port" {
  description = "Host TCP port exposed on the remote machine for the Kubernetes API server."
  type        = number
  default     = 6443

  validation {
    condition     = var.api_server_port == floor(var.api_server_port) && var.api_server_port >= 1 && var.api_server_port <= 65535
    error_message = "api_server_port must be a whole number between 1 and 65535."
  }
}

variable "extra_cert_sans" {
  description = "Additional TLS SAN hostnames or IPs added to the Kubernetes API server certificate (the remote SSH host, 127.0.0.1, localhost, and any jump host are included automatically)."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for s in var.extra_cert_sans : can(regex("^[A-Za-z0-9._:-]+$", s))])
    error_message = "Every entry in extra_cert_sans must be a hostname or IP literal using [A-Za-z0-9._:-]."
  }
}

variable "cluster_cidr" {
  description = "Pod network CIDR passed to Kind networking.podSubnet."
  type        = string
  default     = "10.244.0.0/16"

  validation {
    condition     = alltrue([for c in split(",", var.cluster_cidr) : can(cidrhost(c, 0))])
    error_message = "cluster_cidr must be a CIDR, or comma-separated CIDRs for dual-stack."
  }
}

variable "service_cidr" {
  description = "Service network CIDR passed to Kind networking.serviceSubnet."
  type        = string
  default     = "10.96.0.0/12"

  validation {
    condition     = alltrue([for c in split(",", var.service_cidr) : can(cidrhost(c, 0))])
    error_message = "service_cidr must be a CIDR, or comma-separated CIDRs for dual-stack."
  }
}

variable "disable_default_cni" {
  description = "Set true to disable Kind's built-in kindnetd CNI when testing a custom CNI."
  type        = bool
  default     = false
}

variable "mirror" {
  description = "Optional mirror preset: '' (upstream GitHub/Docker Hub) or 'cn' (derives community docker.io containerd registry mirrors inside Kind nodes)."
  type        = string
  default     = ""

  validation {
    condition     = contains(["", "cn"], var.mirror)
    error_message = "mirror must be '' or 'cn'."
  }
}

variable "registry_mirrors" {
  description = "Per-registry mirror endpoints configured under /etc/containerd/certs.d inside every Kind node, e.g. {\"docker.io\" = [\"https://mirror.example.com\"]}. Unset follows mirror; {} writes none."
  type        = map(list(string))
  default     = null

  validation {
    condition = var.registry_mirrors == null || alltrue(flatten([
      for reg, eps in var.registry_mirrors : concat(
        [can(regex("^[A-Za-z0-9._-]+(:[0-9]+)?$", reg))],
        [for e in eps : can(regex("^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?(/[A-Za-z0-9._/-]*)?$", e))],
      )
    ]))
    error_message = "registry_mirrors keys must be registry host[:port], and every endpoint must be an http(s) URL naming a host."
  }

  validation {
    condition     = var.registry_mirrors == null || alltrue([for eps in values(var.registry_mirrors) : length(eps) > 0])
    error_message = "Every registry in registry_mirrors must name at least one endpoint; drop the key instead of giving an empty list."
  }
}

variable "preload_images" {
  description = "Docker image references on the remote host to load into every Kind node via 'kind load docker-image' after cluster creation."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for img in var.preload_images : can(regex("^[A-Za-z0-9._:@/-]+$", img))])
    error_message = "Every entry in preload_images must be a valid OCI image reference using [A-Za-z0-9._:@/-]."
  }
}

variable "extra_mounts" {
  description = "Optional host bind mounts attached to every Kind worker node (or control-plane when workers = 0)."
  type = list(object({
    host_path      = string
    container_path = string
    read_only      = optional(bool, false)
  }))
  default = []

  validation {
    condition = alltrue([
      for m in var.extra_mounts :
      can(regex("^/[A-Za-z0-9._@+:/-]*$", m.host_path)) && can(regex("^/[A-Za-z0-9._@+:/-]*$", m.container_path))
    ])
    error_message = "Every extra_mounts host_path and container_path must be an absolute path using [A-Za-z0-9._@+:/-]."
  }
}

variable "kubelet_extra_args" {
  description = "Extra kubelet arguments merged into kubeadm nodeRegistration.kubeletExtraArgs on every node."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k in keys(var.kubelet_extra_args) : can(regex("^[A-Za-z0-9._-]+$", k))])
    error_message = "Every key in kubelet_extra_args must be a kubelet flag name made of [A-Za-z0-9._-]."
  }
}

variable "binary_cache_dir" {
  description = "Directory on the remote host holding pinned kind CLI binaries (<dir>/<kind_version>/kind). Empty downloads directly to /usr/local/bin/kind."
  type        = string
  default     = "/var/lib/kind-cache"

  validation {
    condition     = var.binary_cache_dir == "" || can(regex("^/[A-Za-z0-9._@+:/-]*$", var.binary_cache_dir))
    error_message = "binary_cache_dir must be an absolute path made of [A-Za-z0-9._@+:/-], or empty."
  }
}

variable "inotify_max_user_instances" {
  description = "Minimum fs.inotify.max_user_instances raised on the remote host before creating multi-node Kind clusters (never lowers an existing higher limit)."
  type        = number
  default     = 8192

  validation {
    condition     = var.inotify_max_user_instances == floor(var.inotify_max_user_instances) && var.inotify_max_user_instances >= 128
    error_message = "inotify_max_user_instances must be a whole number >= 128."
  }
}

variable "inotify_max_user_watches" {
  description = "Minimum fs.inotify.max_user_watches raised on the remote host before creating multi-node Kind clusters (never lowers an existing higher limit)."
  type        = number
  default     = 524288

  validation {
    condition     = var.inotify_max_user_watches == floor(var.inotify_max_user_watches) && var.inotify_max_user_watches >= 8192
    error_message = "inotify_max_user_watches must be a whole number >= 8192."
  }
}

variable "switch_kube_context" {
  description = "Whether merging this cluster into ~/.kube/config may leave it as the current context. When false, the previous current context is restored."
  type        = bool
  default     = true
}
