variable "server" {
  description = "RKE2 server (control-plane) hosts, each given as 'user@host' or 'host'. The first one must be reachable from this workstation."
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
  description = "RKE2 agent (worker) hosts, each given as 'user@host' or 'host'. When empty, the servers run workloads themselves."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.agent) == length(distinct([for a in var.agent : strcontains(a, "@") ? split("@", a)[1] : a]))
    error_message = "Duplicate agent hosts are not allowed; each host keys one node, and addresses differing only by SSH user still collide."
  }

  validation {
    # Each list is checked against itself above, which leaves the case where one host is in both:
    # it would get two install resources racing to reclaim, install, and cache on the same machine,
    # each undoing the other.
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
  # contract rather than a guess. What the module does NOT do is write that address out as node-ip:
  # with no entry it passes nothing and lets RKE2 detect the address on the node's default route.
  # Same answer when the convention holds, and a caller who breaks it gets a working node instead of a
  # kubelet that refuses to start on an address its host does not hold.
  description = "An exception list: only a node whose SSH address is not its in-cluster address needs an entry. The key is the address given in server/agent (no 'user@' prefix), the value is the address the node holds inside the cluster -- written to that node's config.yaml as `node-ip`, and for the first server also the address the others join through. A host left out is taken to SSH on its cluster address already."
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
    # RKE2 defines node-ip as an IP address; a hostname there is rejected by the kubelet at startup.
    condition     = alltrue([for v in values(var.node_internal_ip) : can(cidrhost("${v}/32", 0)) || can(cidrhost("${v}/128", 0))])
    error_message = "Every node_internal_ip value must be an IPv4 or IPv6 literal -- the node's own in-cluster address (RKE2 defines node-ip as an address, not a name)."
  }

  validation {
    # A mapped key is written out as node-external-ip, which RKE2 also defines as an address. server
    # and agent accept a DNS name, so a mapped host is the one place the name is not enough.
    condition     = alltrue([for k in keys(var.node_internal_ip) : can(cidrhost("${k}/32", 0)) || can(cidrhost("${k}/128", 0))])
    error_message = "Every node_internal_ip key must be an IPv4 or IPv6 literal -- the key is written out as that node's node-external-ip, which RKE2 defines as an address, not a name. A node reached by DNS name belongs outside the map."
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
  # For the topology this module exists for: one externally reachable node, the rest on an internal
  # network. The rule needs no per-node flag -- every node whose SSH host differs from the jumper's is
  # reached through it -- and it covers both the expected shape (the reachable server doubling as the
  # jump host) and a standalone bastion in front of the agents.
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

variable "release" {
  # Named "release" because Terraform reserves the variable name "version". The default carries the
  # same Kubernetes patch as the k3s module's v1.34.9+k3s1, so a verification run exercises the
  # same Kubernetes on either distribution. Channels: https://update.rke2.io/v1-release/channels
  description = "RKE2 release tag as published at https://github.com/rancher/rke2/releases, e.g. 'v1.34.9+rke2r1'. Also names the cache directory and the download URLs."
  type        = string
  default     = "v1.34.9+rke2r1"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+\\+rke2r[0-9]+$", var.release))
    error_message = "release must be an RKE2 tag of the form 'vX.Y.Z+rke2rN' (it names the cache directory and the release assets)."
  }
}

variable "cni" {
  # RKE2's own default is canal; this module defaults to calico, which is what the lab hardware is
  # verified against. Anything other than none/canal pulls a whole stack of its own images, which
  # is why the cache's wanted-file list is derived from this value rather than fixed.
  description = "RKE2 CNI plugin, one of 'none', 'calico' (default), 'canal', 'cilium', 'flannel'. Written to config.yaml as `cni`."
  type        = string
  default     = "calico"

  validation {
    condition     = contains(["none", "calico", "canal", "cilium", "flannel"], var.cni)
    error_message = "cni must be one of: none, calico, canal, cilium, flannel."
  }
}

variable "cluster_cidr" {
  # Defaults to RKE2's own default. Written to every server's config.yaml; agents inherit it.
  # Also scopes the post-uninstall route flush that clears Calico's leftover pod-CIDR blackhole
  # route, so a custom value stays consistent between the running pod network and the cleanup.
  description = "Pod (cluster) network CIDR, written to server config.yaml as `cluster-cidr`. Comma-separate two CIDRs for dual-stack."
  type        = string
  default     = "10.42.0.0/16"

  validation {
    condition     = alltrue([for c in split(",", var.cluster_cidr) : can(cidrhost(c, 0))])
    error_message = "cluster_cidr must be a CIDR, or comma-separated CIDRs for dual-stack (e.g. '10.42.0.0/16')."
  }
}

variable "service_cidr" {
  # Defaults to RKE2's own default. Written to every server's config.yaml; agents inherit it.
  description = "Service network CIDR, written to server config.yaml as `service-cidr`. Comma-separate two CIDRs for dual-stack."
  type        = string
  default     = "10.43.0.0/16"

  validation {
    condition     = alltrue([for c in split(",", var.service_cidr) : can(cidrhost(c, 0))])
    error_message = "service_cidr must be a CIDR, or comma-separated CIDRs for dual-stack (e.g. '10.43.0.0/16')."
  }
}

variable "service_node_port_range" {
  description = "NodePort Service port range, written to server config.yaml as `service-node-port-range` (format '<lo>-<hi>')."
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
  # (<dir>/<release>/) so what the installer is given for a release can only come from that release
  # -- version skew is not something a check has to catch, it is something the layout cannot
  # express. Unlike the k3s cache this one also holds the binary tarball and the installer script,
  # so a warm cache needs no network at all. Empty disables the feature entirely.
  #
  # On by default, because a throttled or restricted network is the normal case for this lab
  # hardware and an off-by-default cache means every new host pays the pull. The two consequences
  # are documented rather than hidden: a cold node's first apply downloads (measured 1.25 GB with
  # Calico), and the cache is left behind on destroy -- which is the point of it. Under /var/lib
  # because the installer's staging directory is filled from here by hardlink, which needs the same
  # filesystem.
  description = "Directory on each node holding a per-release artifact cache (<dir>/<release>/): the binary, the image archives and the installer. Empty disables it."
  type        = string
  default     = "/var/lib/rke2-image-archives"

  validation {
    # Absolute, and made of characters a shell will not reinterpret: this path is spliced into the
    # install command that runs on the node, where a quote or a space would break the command
    # rather than the path.
    condition     = var.image_archives_dir == "" || can(regex("^/[A-Za-z0-9._@+:/-]*$", var.image_archives_dir))
    error_message = "image_archives_dir must be an absolute path made of [A-Za-z0-9._@+:/-] (it is passed to a shell on the node), or empty to disable the cache."
  }

  validation {
    # The reclaim step runs rke2-uninstall.sh, which removes this tree wholesale -- a cache kept
    # inside it is destroyed by design, on every apply, before it can ever be used.
    condition     = var.image_archives_dir != "/var/lib/rancher/rke2" && !startswith(var.image_archives_dir, "/var/lib/rancher/rke2/")
    error_message = "image_archives_dir must not be under /var/lib/rancher/rke2; the uninstall run before every install removes that tree."
  }
}

variable "mirror" {
  # Where the node's cache is filled FROM. 'cn' points the module's own download script at
  # rancher-mirror.rancher.cn -- the same asset names and byte-identical checksum files as
  # github.com, reachable from hosts that cannot reach github.com or get.rke2.io. The installer's
  # own INSTALL_RKE2_MIRROR parameter is deliberately never set: the upstream and CN-hosted
  # install.sh variants differ, and a node may already hold a cached script of either variant, so
  # mirror downloads are done by the module's script instead. Requires image_archives_dir --
  # without the cache, avoiding github.com would need exactly that installer parameter.
  description = "Release-asset mirror the node's artifact cache is filled from: '' (github.com and get.rke2.io) or 'cn' (rancher-mirror.rancher.cn). Requires image_archives_dir."
  type        = string
  default     = ""

  validation {
    condition     = contains(["", "cn"], var.mirror)
    error_message = "mirror must be '' or 'cn' (rancher-mirror.rancher.cn)."
  }
}

variable "system_default_registry" {
  # A separate job from mirror, not a setting independent of it: mirror decides where
  # install-time artifacts come from, this decides where runtime system-image pulls
  # (docker.io/rancher/mirrored-*) resolve -- and mirror supplies this one's default,
  # below. RKE2 defines
  # system-default-registry as an Agent/Runtime setting, valid on servers and agents alike, so it
  # is written into EVERY node's config.yaml. A CN-reachable value is registry.rancher.cn;
  # rancher-mirror.rancher.cn is NOT an OCI registry and does not work here.
  #
  # Unset FOLLOWS THE MIRROR rather than writing nothing, so mirror = "cn" alone is a working
  # configuration. RKE2 needs the pairing harder than k3s does: the cn mirror carries no
  # rke2-images archives at all, so cn mode stages no system images and every one of them is
  # pulled at runtime from a host the node was mirrored precisely because it cannot reach. An
  # explicit "" still writes nothing and is still refused under the cn mirror, which is how that
  # combination stays an error the caller has to mean rather than one they fell into.
  description = "Registry every RKE2 node resolves system-image pulls through, written as system-default-registry into each node's config.yaml. Unset follows the mirror ('cn' derives registry.rancher.cn); empty writes nothing."
  type        = string
  default     = null

  validation {
    # Written double-quoted into the config.yaml each node gets through a single-quoted printf,
    # so both quote characters are excluded. RKE2 accepts only an RFC 3986 authority here: a
    # hostname/IPv4 or a bracketed IPv6 literal, with an optional numeric port; no path. A bracketed
    # value must also parse as a real IPv6 address (checked via cidrhost), so [::::] fails.
    condition = var.system_default_registry == null || var.system_default_registry == "" || (
      can(regex("^[A-Za-z0-9._-]+(:[0-9]+)?$", var.system_default_registry)) ||
      (can(regex("^\\[[0-9A-Fa-f:]+\\](:[0-9]+)?$", var.system_default_registry)) &&
      can(cidrhost("${regex("\\[([0-9A-Fa-f:]+)\\]", var.system_default_registry)[0]}/128", 0)))
    )
    error_message = "system_default_registry must be a registry host[:port] -- a hostname/IPv4 or bracketed IPv6 literal, optional numeric port (it is written into the node's config.yaml), or empty."
  }
}

variable "calico_multi_nic_fix" {
  # Defaults to on when cni is calico, off otherwise -- the fix is Calico-specific. `optional()` is
  # an object-attribute modifier and is not valid as a top-level variable type, so the "unset" state
  # is null and is resolved against var.cni.
  #
  # There is deliberately no per-node autodetection behind this: the tunnel-address half is
  # cluster-scoped and has to be written before any node exists to inspect, and "multi-homed" is not
  # a countable property -- what misled Calico on the lab pair was a container bridge address, which
  # a single-NIC node running Docker has too. On a node with one address the fix is a no-op by
  # construction; what is not free there is the DaemonSet, which relaxes rp_filter and leaves a
  # privileged Pod polling. That residue is what this switch is for.
  #
  # There is also deliberately no image variable. The DaemonSet uses the image calico-node is
  # already running on every node, read from the cluster: the only ref guaranteed to be present
  # locally, which matters because the Pod that repairs the network must not need a working network
  # to start. Any override -- a pinned tag most of all, since it moves with every RKE2 release --
  # can only make the fix unschedulable exactly where it is needed.
  description = "Whether to apply the Calico multi-NIC fix (per-node route source address plus a SNAT-position DaemonSet). Defaults to true when cni is 'calico'; it can only be turned off, not forced on for another CNI."
  type        = bool
  default     = null

  validation {
    # The switch turns the fix OFF, never on for a CNI that does not install Calico: both halves are
    # written into Calico's own objects, and the applier waits for calico-node to exist.
    condition     = var.calico_multi_nic_fix != true || var.cni == "calico"
    error_message = "calico_multi_nic_fix = true requires cni = \"calico\": the fix reconciles Calico's own FelixConfiguration and HelmChartConfig, and waits for calico-node, none of which another CNI installs."
  }
}

variable "switch_kube_context" {
  # The cluster is merged into ~/.kube/config either way; this only decides whether a bare kubectl
  # points at it afterwards. Set it to false while another cluster is mid-verification. Nothing is
  # restored when there was no current context to begin with (a kubeconfig that did not exist yet),
  # so the merged one stays current there.
  description = "Whether merging this cluster into ~/.kube/config may leave it as the current context. When false, the context that was current before the merge is restored."
  type        = bool
  default     = true
}
