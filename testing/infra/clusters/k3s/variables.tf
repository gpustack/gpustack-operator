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

  validation {
    # Every entry is spliced into an ssh command line and, for the jumper case, into a ProxyCommand a
    # shell executes with no escaping. The colon is allowed for IPv6 literals.
    condition     = alltrue([for a in var.server : can(regex("^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$", a))])
    error_message = "Every server entry must be 'host' or 'user@host' using [A-Za-z0-9._:-] (each is spliced into an ssh command line)."
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

  validation {
    # Each list is checked against itself above, which leaves the case where one host is in
    # both: it would get two install resources racing to reclaim, install, and cache on the
    # same machine, each undoing the other.
    condition = length(setintersection(
      toset([for s in var.server : strcontains(s, "@") ? split("@", s)[1] : s]),
      toset([for a in var.agent : strcontains(a, "@") ? split("@", a)[1] : a]),
    )) == 0
    error_message = "A host may appear in either server or agent, not both; one node cannot be installed as a server and an agent at the same time."
  }

  validation {
    # Every entry is spliced into an ssh command line and, for the jumper case, into a ProxyCommand a
    # shell executes with no escaping. The colon is allowed for IPv6 literals.
    condition     = alltrue([for a in var.agent : can(regex("^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$", a))])
    error_message = "Every agent entry must be 'host' or 'user@host' using [A-Za-z0-9._:-] (each is spliced into an ssh command line)."
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

  validation {
    # Spliced into an ssh command line, and into a ProxyCommand a shell executes with no escaping.
    condition     = can(regex("^[A-Za-z0-9._-]+$", var.ssh_user))
    error_message = "ssh_user must be made of [A-Za-z0-9._-] (it is spliced into an ssh command line)."
  }
}

variable "ssh_private_key" {
  # Defaults to an ed25519 key: Terraform's SSH provisioner signs RSA keys with
  # ssh-rsa (SHA-1), which modern OpenSSH servers (8.8+) reject.
  description = "Path to the private key used for SSH connections to the hosts (and to the jump host)."
  type        = string
  default     = "~/.ssh/id_ed25519"

  validation {
    # This path is spliced into an ssh ProxyCommand that a shell executes with no escaping.
    condition     = can(regex("^[A-Za-z0-9._@+:/~-]+$", var.ssh_private_key))
    error_message = "ssh_private_key must be a path made of [A-Za-z0-9._@+:/~-] (it is spliced into an ssh ProxyCommand)."
  }
}

variable "ssh_jumper" {
  # For a topology with one externally reachable node and the rest on an internal network. The rule
  # needs no per-node flag -- every node whose SSH host differs from the jumper's is reached through
  # it -- and it covers both the expected shape (the reachable server doubling as the jump host) and
  # a standalone bastion in front of the agents.
  description = "SSH jump host for nodes not directly reachable, as 'user@host' or 'host'. Every node whose SSH host differs from this one is reached through it. Empty means direct connections."
  type        = string
  default     = ""

  validation {
    # Rejects only what is genuinely malformed. This string is interpolated into a ProxyCommand that a
    # shell executes with no escaping, so the character set is the boundary; the colon is allowed for
    # IPv6 literals. Deliberately NOT validated: that the jumper appears in the node lists -- requiring
    # that would forbid a standalone bastion, which is a topology this supports.
    condition     = var.ssh_jumper == "" || can(regex("^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$", var.ssh_jumper))
    error_message = "ssh_jumper must be 'host' or 'user@host' using [A-Za-z0-9._:-] (it is spliced into an ssh ProxyCommand), with a non-empty user if a '@' is given."
  }
}

variable "ssh_jumper_port" {
  description = "SSH port used to reach the jump host."
  type        = number
  default     = 22
}

variable "node_internal_ip" {
  # An EXCEPTION LIST, not a roster: only a node whose SSH address is not the address it uses inside
  # the cluster needs an entry. The key is the address typed into server/agent and is an index -- not
  # a claim that it is public -- and the value is the address the node holds inside the cluster. The
  # two differ when the outward one is public, floating, or NAT'd: such an address reaches the host
  # without appearing on any of its interfaces, so it cannot be advertised as a node address. The
  # first server's entry is also what the other nodes join through.
  #
  # An entry left out is the statement "this host's SSH address is already its cluster address". That is
  # the convention the join address relies on, and it is why falling back to the SSH host there is a
  # contract rather than a guess. What the module does NOT do is write that address out as --node-ip:
  # with no entry it passes nothing and lets k3s detect the address on the node's default route.
  # Same answer when the convention holds, and a caller who breaks it gets a working node instead of a
  # kubelet that refuses to start on an address its host does not hold.
  description = "An exception list: only a node whose SSH address is not its in-cluster address needs an entry. The key is the address given in server/agent (no 'user@' prefix), the value is the address the node holds inside the cluster -- passed to that node's install as --node-ip, and for the first server also the address the others join through. A host left out is taken to SSH on its cluster address already."
  type        = map(string)
  default     = {}

  validation {
    # A key that is not a node silently advertises nothing, which surfaces much later as a node
    # holding the wrong address.
    condition = length(setsubtract(
      toset(keys(var.node_internal_ip)),
      toset(concat(
        [for s in var.server : strcontains(s, "@") ? split("@", s)[1] : s],
        [for a in var.agent : strcontains(a, "@") ? split("@", a)[1] : a],
      )),
    )) == 0
    error_message = "Every node_internal_ip key must be one of the given server/agent SSH hosts, with any 'user@' prefix stripped: a key is the address you reach the node AT, and its value is the address the node uses inside the cluster."
  }

  validation {
    # k3s defines --node-ip as an IP address; a hostname there is rejected by the kubelet at startup.
    condition     = alltrue([for v in values(var.node_internal_ip) : can(cidrhost("${v}/32", 0)) || can(cidrhost("${v}/128", 0))])
    error_message = "Every node_internal_ip value must be an IPv4 or IPv6 literal -- the node's own in-cluster address (k3s defines --node-ip as an address, not a name)."
  }

  validation {
    # A mapped key is advertised as --node-external-ip, which k3s also defines as an address. server
    # and agent accept a DNS name, so a mapped host is the one place the name is not enough.
    condition     = alltrue([for k in keys(var.node_internal_ip) : can(cidrhost("${k}/32", 0)) || can(cidrhost("${k}/128", 0))])
    error_message = "Every node_internal_ip key must be an IPv4 or IPv6 literal -- the key is advertised as that node's --node-external-ip, which k3s defines as an address, not a name. A node reached by DNS name belongs outside the map."
  }
}

variable "release" {
  # Named "release" because Terraform reserves the variable name "version".
  description = "K3s release tag as published at https://github.com/k3s-io/k3s/releases, e.g. 'v1.34.9+k3s1'. Also names the cache directory and the download URLs."
  type        = string
  default     = "v1.34.9+k3s1"

  validation {
    # Checked because this now names a directory ON THE NODE as well as the installer's version:
    # 'v1.34.9+k3s1/../../root' is a path the cache layout would otherwise accept.
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+\\+k3s[0-9]+$", var.release))
    error_message = "release must be a k3s tag of the form 'vX.Y.Z+k3sN' (it names the cache directory and the release assets)."
  }
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

variable "image_archives_dir" {
  # A directory on each NODE, not on the workstation: the download is a node-to-release-assets
  # fetch with no WAN upload, and each node keeps its own copy. Partitioned by release
  # (<dir>/<release>/) so what gets staged for a release can only come from that release --
  # version skew is not something a check has to catch, it is something the layout cannot
  # express. Empty disables the feature entirely and k3s pulls its images as before.
  #
  # On by default, because a throttled or restricted network is the normal case for this lab
  # hardware and an off-by-default cache means every new host pays the pull. The two consequences
  # are documented rather than hidden: a cold node's first apply downloads, and the cache is left
  # behind on destroy -- which is the point of it. Under /var/lib to sit on the same filesystem as
  # the data directory it stages into.
  description = "Directory on each node holding a per-release airgap image cache (<dir>/<release>/), staged into k3s' images directory before install. Empty disables it."
  type        = string
  default     = "/var/lib/k3s-image-archives"

  validation {
    # Absolute, and made of characters a shell will not reinterpret: this path is spliced into
    # the install command that runs on the node, where a quote or a space would break the
    # command rather than the path.
    condition     = var.image_archives_dir == "" || can(regex("^/[A-Za-z0-9._@+:/-]*$", var.image_archives_dir))
    error_message = "image_archives_dir must be an absolute path made of [A-Za-z0-9._@+:/-] (it is passed to a shell on the node), or empty to disable the cache."
  }

  validation {
    # The reclaim step runs k3s-uninstall.sh, which removes this tree wholesale -- a cache kept
    # inside it is destroyed by design, on every apply, before it can ever be used.
    condition     = var.image_archives_dir != "/var/lib/rancher/k3s" && !startswith(var.image_archives_dir, "/var/lib/rancher/k3s/")
    error_message = "image_archives_dir must not be under /var/lib/rancher/k3s; the uninstall run before every install removes that tree."
  }
}

variable "mirror" {
  # Where the node's cache is filled FROM. 'cn' points the module's own download script at
  # rancher-mirror.rancher.cn -- the same asset names and byte-identical checksum files as
  # github.com, reachable from hosts that cannot reach github.com or get.k3s.io. The installer's
  # own INSTALL_K3S_MIRROR parameter is deliberately never set: the upstream and CN-hosted
  # install.sh variants differ, and a node may already hold a cached script of either variant, so
  # mirror downloads are done by the module's script instead. Requires image_archives_dir --
  # without the cache, avoiding github.com would need exactly that installer parameter.
  description = "Release-asset mirror the node's image cache is filled from: '' (github.com and get.k3s.io) or 'cn' (rancher-mirror.rancher.cn). Requires image_archives_dir."
  type        = string
  default     = ""

  validation {
    condition     = contains(["", "cn"], var.mirror)
    error_message = "mirror must be '' or 'cn' (rancher-mirror.rancher.cn)."
  }
}

variable "system_default_registry" {
  # Orthogonal to mirror: mirror decides where install-time artifacts come from, this decides
  # where runtime system-image pulls (docker.io/rancher/mirrored-*) resolve. k3s defines
  # --system-default-registry as a SERVER flag only -- the agent CLI has none, and agents get
  # their system images from the staged archives -- so it is passed to server installs only.
  # Empty means the flag is not passed at all, exactly as before. A CN-reachable value is
  # registry.rancher.cn; rancher-mirror.rancher.cn is NOT an OCI registry and does not work here.
  description = "Registry the k3s servers resolve system-image pulls through, passed as --system-default-registry to server installs (agents have no such flag). Empty passes nothing."
  type        = string
  default     = ""

  validation {
    # Spliced into the server install command that runs on the node, so the character set is the
    # boundary. k3s accepts only an RFC 3986 authority here -- host[:port], no path -- so the
    # colon is allowed for a port and the slash is not.
    condition     = var.system_default_registry == "" || can(regex("^[A-Za-z0-9._:-]+$", var.system_default_registry))
    error_message = "system_default_registry must be a registry host[:port] made of [A-Za-z0-9._:-] (it is spliced into the server install command), or empty."
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
