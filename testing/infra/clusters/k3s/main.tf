locals {
  # Parse "user@host" addresses, falling back to var.ssh_user when no user is given.
  servers = [
    for addr in var.server : {
      user = strcontains(addr, "@") ? split("@", addr)[0] : var.ssh_user
      host = strcontains(addr, "@") ? split("@", addr)[1] : addr
    }
  ]
  agents = [
    for addr in var.agent : {
      user = strcontains(addr, "@") ? split("@", addr)[0] : var.ssh_user
      host = strcontains(addr, "@") ? split("@", addr)[1] : addr
    }
  ]
  first_server = local.servers[0]

  # Additional control-plane servers and agents join the first server. Keyed by
  # host so adding/removing one host never disturbs the others.
  join_servers = { for s in slice(local.servers, 1, length(local.servers)) : s.host => s }
  agent_hosts  = { for a in local.agents : a.host => a }

  # Every node this module installs, servers and agents together. The count the cluster has to
  # reach before an apply is allowed to report success.
  node_count = 1 + length(local.join_servers) + length(local.agent_hosts)

  # The jump host, parsed like any other address. Every node whose SSH host differs from this one is
  # reached through it; the jumper's own host is reached directly.
  jumper_host = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[1] : var.ssh_jumper)
  jumper_user = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[0] : var.ssh_user)

  # Every managed host, servers and agents together. The three per-node maps below are keyed by it.
  all_hosts = concat([for s in local.servers : s.host], [for a in local.agents : a.host])

  # Per node: the bastion Terraform's own provisioner connections use, empty when the node needs none.
  bastion_of = { for host in local.all_hosts :
    host => host == local.jumper_host ? "" : local.jumper_host
  }

  # The user and port that bastion is reached with -- neutral values for a node that has none, because
  # Terraform ignores both when bastion_host is empty and recording them regardless makes them a
  # trigger that fires without a reachability change. Measured on the rke2 pair: turning ON a jumper
  # that IS the first server left bastion_host untouched at "" but moved bastion_user to "root", which
  # replaced the server -- a reinstall of the control plane for no change in how it is reached.
  bastion_user_of = { for host, bastion in local.bastion_of : host => bastion == "" ? "" : local.jumper_user }
  # 22 rather than "" because the connection block runs this through tonumber(), which fails on one.
  bastion_port_of = { for host, bastion in local.bastion_of : host => bastion == "" ? "22" : tostring(var.ssh_jumper_port) }

  # Per node: the option a local-exec ssh/scp needs, empty when the node needs none.
  #
  # ProxyCommand rather than ProxyJump because OpenSSH does not propagate -i to a ProxyJump hop. The
  # inner options are spelled out because the OUTER ssh's BatchMode and StrictHostKeyChecking do not
  # reach the ProxyCommand child either: on a workstation whose known_hosts lacks the JUMPER's key, the
  # inner ssh would block on a host-key prompt with no TTY to answer it, and the apply would hang rather
  # than fail. Terraform's own bastion_* path is unaffected (it does no host-key validation), so this
  # defect lives only here.
  # Emitted as a bash ARRAY APPEND, not as a bare option string. The ProxyCommand value has to be
  # quoted as one word, and a quoted value spliced into a quoted shell assignment closes it early: the
  # assignment silently does not happen, `set -u` then kills the command substitution that reads the
  # node, and -- with no `set -e` -- the step carries on and reports "nothing to change".
  ssh_proxy_of = { for host, bastion in local.bastion_of :
    host => bastion == "" ? ":" : "ssh_opts+=(-o \"ProxyCommand=ssh -i ${pathexpand(var.ssh_private_key)} -p ${var.ssh_jumper_port} -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -W %h:%p ${local.jumper_user}@${local.jumper_host}\")"
  }

  # --node-ip comes ONLY from an explicit var.node_internal_ip entry, never from the SSH host, even
  # though the convention is that a host with no entry SSHes on its cluster address already. Copying
  # the SSH host out would turn that convention into an assertion about the machine: an SSH host that
  # happens to be an IP literal is not evidence that the node owns it -- a public, floating, or NAT
  # address reaches the host without appearing on any of its interfaces, and a kubelet given an
  # address its host does not hold refuses to start. With no entry k3s detects the address on the
  # default route instead, the same answer when the convention holds and a working node when it does
  # not.
  node_internal_ip_flag = { for host in local.all_hosts :
    host => lookup(var.node_internal_ip, host, "") == "" ? "" : "--node-ip ${var.node_internal_ip[host]}"
  }
  # The KEY side of node_internal_ip, and the only place it reaches the cluster: the address you SSH
  # to becomes the node's ExternalIP. Only where the two addresses actually differ -- a host mapped to
  # itself has no separate outward address to advertise, and a host with no entry has none either.
  # --node-external-ip is an agent/networking flag, so every node takes it.
  node_external_ip_flag = { for host in local.all_hosts :
    host => lookup(var.node_internal_ip, host, "") == "" || var.node_internal_ip[host] == host ? "" : "--node-external-ip ${host}"
  }

  # Pinned rather than left to default, and SERVERS ONLY -- --advertise-address is a listener flag
  # that the agent does not accept, and an unknown flag is fatal at parsing, which is a crash loop.
  #
  # k3s derives advertise-address from node-external-ip, then node-ip. So declaring an external
  # address above moves the address the apiserver advertises to cluster members -- and with it the
  # `kubernetes` Service endpoint -- onto the outward address, which no node holds. Measured on the
  # rke2 pair, whose expression is the same: removing this key moved the endpoint to the outward
  # address immediately. The cluster there kept working, because that network hairpins its NAT, so
  # what it really costs is a silent dependency on the hairpin plus a round trip out and back for
  # every in-cluster API call. On a network that does not hairpin it is a break.
  advertise_flag = { for host in [for s in local.servers : s.host] :
    host => lookup(var.node_internal_ip, host, "") == "" || var.node_internal_ip[host] == host ? "" : "--advertise-address ${var.node_internal_ip[host]}"
  }

  # The jump host's own address, but only when the jumper is a machine of its own: a jumper that is
  # also a node already has its address in that node's list. keys(bastion_of) is every managed node.
  #
  # What it buys: a client that reaches the apiserver THROUGH that address -- a port forward on the
  # jump host, a DNAT rule, or that address later fronting the cluster -- presents it as the server
  # name, and a certificate that does not cover it fails the handshake. What it does NOT buy: the
  # kubeconfig this module fetches is still rewritten to the first server's own SSH address, because
  # nothing serves the apiserver on the jump host. It costs nothing when nobody uses it.
  jumper_san = local.jumper_host == "" || contains(keys(local.bastion_of), local.jumper_host) ? "" : local.jumper_host

  # --tls-san carries the SSH host, because that is the address the fetched kubeconfig is rewritten to,
  # plus this node's cluster address when the two differ -- a certificate that covers only one of them
  # fails whichever client uses the other -- plus the jump host when there is a separate one.
  # compact() drops the addresses that do not apply and distinct() collapses a host mapped to itself.
  tls_san_flags = { for host in [for s in local.servers : s.host] :
    host => join(" ", [
      for san in distinct(compact([host, lookup(var.node_internal_ip, host, ""), local.jumper_san])) :
      "--tls-san ${san}"
    ])
  }

  # The address other nodes join through. It has to be one THEY can reach, which is the reason
  # node_internal_ip exists: on a host whose SSH address is public or NAT'd, the SSH address is not
  # it. With no entry it falls back to the SSH host -- which the convention says IS that host's
  # cluster address, so this is a contract rather than a guess. The pre-join probe below is what fails
  # fast, naming the address, for the caller who broke the convention.
  join_addr = lookup(var.node_internal_ip, local.first_server.host, local.first_server.host)
  # An IPv6 literal has to be bracketed in a URL, and must NOT be bracketed in bash's /dev/tcp path.
  server_url = "https://${strcontains(local.join_addr, ":") ? "[${local.join_addr}]" : local.join_addr}:${var.server_https_listen_port}"
  # The kubeconfig carries the SSH host rather than the join address, since kubectl runs from this
  # workstation. Bracketed by the same rule: an unbracketed IPv6 literal is an unparseable URL.
  kubeconfig_host = strcontains(local.first_server.host, ":") ? "[${local.first_server.host}]" : local.first_server.host
  join_wait       = "timeout 180 bash -c 'until (exec 3<>/dev/tcp/${local.join_addr}/${var.server_https_listen_port}) 2>/dev/null; do sleep 3; done' || { echo 'cannot reach ${local.join_addr}:${var.server_https_listen_port} from this node; set node_internal_ip for the first server to an address the other nodes can reach' >&2; exit 1; }"

  # kubectl context/cluster/user name for the fetched kubeconfig. The raw
  # k3s.yaml names everything "default", so we namespace it to the first server
  # host (sanitized to the characters kubectl names accept).
  context_name = "k3s-${replace(local.first_server.host, "/[^a-zA-Z0-9]/", "-")}"

  # Standalone rewritten kubeconfig kept in the module dir; also merged into ~/.kube/config.
  kubeconfig_path = "${path.module}/kubeconfig"

  # Uploaded to every node and run there, between the reclaim and the installer: the reclaim
  # removes the images directory, and k3s reads it at startup, so this is the only window that
  # works. One line spliced into each install provisioner, so an unset image_archives_dir
  # leaves the install exactly as it was.
  #
  # It lands in the SSH user's HOME rather than under a fixed name in /tmp, which is world-writable:
  # any local user there could pre-place or swap that name between the upload and the `sudo bash`
  # that runs it. A relative destination is what the file provisioner resolves against the remote home.
  image_archives_script      = ".gpustack-k3s-image-archives.sh"
  image_archives_script_path = "$HOME/.gpustack-k3s-image-archives.sh"
  # `|| exit 1` rather than a `set -e` in the first inline entry: Terraform concatenates the entries
  # into ONE shell script, so a `set -e` up there would also make the uninstall probes below it fatal
  # on a host that has no k3s to reclaim. Without either, every `die` in the script -- including its
  # assertion that an image archive actually landed -- is swallowed, because the step's exit code is
  # the last command's. The install commands carry the same guard: an installer that fails after
  # putting the binary in place is otherwise answered by the version assertion and reported as a
  # success.
  stage_image_archives = var.image_archives_dir == "" ? "echo 'image_archives_dir is unset; k3s will download its binary and pull its images from a registry'" : join(" ", compact([
    "sudo bash ${local.image_archives_script_path}",
    "--release '${var.release}'",
    "--cache-dir '${var.image_archives_dir}'",
    # Where the cache is filled FROM; empty leaves the step byte-identical to before.
    var.mirror == "" ? "" : "--mirror '${var.mirror}'",
    "|| exit 1",
  ]))

  cache_release_dir = "${var.image_archives_dir}/${var.release}"

  # Server-only flag: the k3s agent CLI has no --system-default-registry, and an agent's system
  # images come from the staged archives. Empty means the flag is not passed, exactly as before.
  # The leading space is part of the value, so an empty one splices byte-identically. The value is
  # single-quoted: a bracketed IPv6 literal is otherwise a glob pattern to the remote shell.
  system_default_registry_flag = var.system_default_registry == "" ? "" : " --system-default-registry '${var.system_default_registry}'"

  # How the installer is obtained, and what it is told about downloading. With a cache the step
  # above has already put that release's own binary in place and pinned a copy of the installer, so
  # INSTALL_K3S_SKIP_DOWNLOAD=true leaves the install with nothing to fetch -- and because the same
  # variable also governs the SELinux policy RPM, it stops the installer adding a vendor repository
  # to the host. That is the rke2 module's stance too, which is why it forces the tar method; a host
  # that needs the k3s SELinux policy has to carry it already. Without a cache, the upstream
  # one-liner, where INSTALL_K3S_VERSION is what decides the version.
  install_prefix = var.image_archives_dir == "" ? "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${var.release}' K3S_TOKEN='${random_string.token.result}'" : "INSTALL_K3S_SKIP_DOWNLOAD=true K3S_TOKEN='${random_string.token.result}'"
  install_sh     = var.image_archives_dir == "" ? "sh -s -" : "sh '${local.cache_release_dir}/install.sh'"

  # Run after every install, because with the cache on the installed version is whatever the cache
  # holds: INSTALL_K3S_VERSION is not passed at all, so nothing downstream would notice a release
  # directory an operator filled from another release. Cheap, and the only check that can catch it.
  version_assert = <<-EOT
    set -e
    k3s_bin=""
    for d in /usr/local/bin /usr/bin /opt/bin; do
      if [ -x "$d/k3s" ]; then k3s_bin="$d/k3s"; break; fi
    done
    [ -n "$k3s_bin" ] || { echo "no k3s binary after install" >&2; exit 1; }
    installed=$(sudo "$k3s_bin" --version | head -1 | awk '{print $3}')
    if [ "$installed" != '${var.release}' ]; then
      # The hint names the cache directory only when there IS one; with the cache off,
      # cache_release_dir is just "/<release>" and would send the reader to a path that does not exist.
      echo "installed k3s is $installed but release is ${var.release}${var.image_archives_dir == "" ? "" : "; a hand-placed artifact in ${local.cache_release_dir} is the usual cause"}" >&2
      exit 1
    fi
    echo "installed k3s $installed"
  EOT

  # Every managed node (servers and agents), keyed by host, with its connection
  # details, service name, and the install resource that must land first.
  containerd_nodes = merge(
    {
      (local.first_server.host) = {
        user       = local.first_server.user
        port       = var.server_ssh_port
        service    = "k3s"
        install_id = null_resource.server_init.id
      }
    },
    {
      for host, s in local.join_servers : host => {
        user       = s.user
        port       = var.server_ssh_port
        service    = "k3s"
        install_id = null_resource.server_join[host].id
      }
    },
    {
      for host, a in local.agent_hosts : host => {
        user       = a.user
        port       = var.agent_ssh_port
        service    = "k3s-agent"
        install_id = null_resource.agent[host].id
      }
    },
  )
}

# Shared join token so additional servers and agents authenticate against the
# cluster. random_string (not random_password) keeps install output visible in
# Terraform logs; the token still lands only in local, uncommitted state.
resource "random_string" "token" {
  length  = 48
  special = false
}

# Install K3s on the first server with an embedded etcd datastore (--cluster-init).
# Connection parameters live in triggers so the destroy provisioner (which may
# only reference self) can reuse them.
resource "null_resource" "server_init" {
  # Depends on the snapshot so that it is written before any node is touched, and -- because
  # Terraform destroys dependents first -- removed only after every node is gone.
  depends_on = [null_resource.vars_snapshot]

  triggers = {
    host                     = local.first_server.host
    user                     = local.first_server.user
    port                     = var.server_ssh_port
    key_path                 = pathexpand(var.ssh_private_key)
    bastion_host             = local.bastion_of[local.first_server.host]
    bastion_user             = local.bastion_user_of[local.first_server.host]
    bastion_port             = local.bastion_port_of[local.first_server.host]
    version                  = var.release
    flannel_backend          = var.flannel_backend
    cluster_cidr             = var.cluster_cidr
    service_cidr             = var.service_cidr
    server_https_listen_port = var.server_https_listen_port
    service_node_port_range  = var.service_node_port_range
    # This node's cluster address goes into its install flags and its certificate, and is also what
    # every other node joins through, so changing it has to reinstall this server rather than wait.
    node_internal_ip = lookup(var.node_internal_ip, local.first_server.host, "")
    # Tracked so setting or changing the cache re-provisions this node now, rather than taking
    # effect at whatever later reinstall happens to come along.
    image_archives_dir = var.image_archives_dir
    # Same reason: where the cache is filled from, and the registry the servers resolve
    # system-image pulls through, both feed this node's install command.
    mirror                  = var.mirror
    system_default_registry = var.system_default_registry
  }

  lifecycle {
    # Without the cache, the only CN-reachable install path would be the installer's own
    # INSTALL_K3S_MIRROR parameter -- which this module deliberately never sets (see the mirror
    # variable), so the combination is refused rather than silently reaching github.com.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.k3s.io."
    }
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)

    # Empty when this node is reachable directly, which is how Terraform is told to use no bastion at
    # all. Read off triggers so a destroy-time provisioner -- which may reference only self -- can
    # still reach a node that is only reachable through the jumper.
    #
    # lookup() with a default, not self.triggers.x: a resource already in state from before these keys
    # existed has no such element, and reading it directly fails its DESTROY -- leaving a node
    # installed with no way for this module to reclaim it.
    bastion_host        = lookup(self.triggers, "bastion_host", "")
    bastion_user        = lookup(self.triggers, "bastion_user", "")
    bastion_port        = tonumber(lookup(self.triggers, "bastion_port", "22"))
    bastion_private_key = file(self.triggers.key_path)
  }

  # Uploaded ahead of the install so the cache step below runs in the same session that
  # reclaimed the node. Sent unconditionally (a provisioner cannot be count-gated); it is the
  # install step that skips it when no cache is configured.
  provisioner "file" {
    source      = "${path.module}/scripts/image-archives.sh"
    destination = local.image_archives_script
  }

  # Install only, so the SSH session closes promptly instead of being held open
  # across the K3s/flannel bring-up. Readiness and fetch happen locally below.
  provisioner "remote-exec" {
    inline = [
      # Reclaim the host: uninstall any pre-existing k3s (server or agent, e.g. a
      # prior or foreign deployment) so the install always lands on a clean node.
      # Installing over stale etcd/token data otherwise fails to start the service.
      # Destructive by design — this module owns its target hosts.
      "if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; fi",
      "if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then sudo /usr/local/bin/k3s-agent-uninstall.sh; fi",
      # k3s-uninstall drops cni0/flannel.1 but not flannel's host-gw routes, which
      # live on the physical NIC and survive it. A re-apply that reassigns pod CIDRs
      # (e.g. swapped server/agent roles) would otherwise inherit stale routes that
      # send a node's own pod subnet to its peer and black-hole pod traffic. Flush
      # the routes under the configured cluster CIDR (var.cluster_cidr, split for
      # dual-stack) so fresh flannel rebuilds a clean table. No-op for vxlan.
      "for c in $(echo '${var.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
      # After the reclaim (which removed the images directory) and before the installer, which
      # is when k3s reads that directory. A warm cache makes this a local copy with no download.
      local.stage_image_archives,
      "${local.install_prefix} ${local.install_sh} server --cluster-init --flannel-backend ${var.flannel_backend} --cluster-cidr ${var.cluster_cidr} --service-cidr ${var.service_cidr} ${local.tls_san_flags[local.first_server.host]} --https-listen-port ${var.server_https_listen_port} --service-node-port-range ${var.service_node_port_range} ${local.node_internal_ip_flag[local.first_server.host]} ${local.node_external_ip_flag[local.first_server.host]} ${local.advertise_flag[local.first_server.host]}${local.system_default_registry_flag} || exit 1",
      local.version_assert,
    ]
  }

  # Uninstall on destroy. Only self is referenceable here; on_failure=continue
  # keeps re-destroys idempotent when the script is already gone.
  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      "sudo /usr/local/bin/k3s-uninstall.sh",
      # Also drop flannel's leftover host-gw pod-CIDR routes (see the reclaim step)
      # so destroy leaves the node's routing table clean for any later re-provision.
      # Destroy provisioners may reference only self, so read the CIDR off triggers.
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Join any additional servers as control-plane members. Empty when a single
# server is given.
resource "null_resource" "server_join" {
  for_each   = local.join_servers
  depends_on = [null_resource.server_init, null_resource.vars_snapshot]

  triggers = {
    host         = each.value.host
    user         = each.value.user
    port         = var.server_ssh_port
    key_path     = pathexpand(var.ssh_private_key)
    bastion_host = local.bastion_of[each.value.host]
    bastion_user = local.bastion_user_of[each.value.host]
    bastion_port = local.bastion_port_of[each.value.host]
    # Re-run the install when the release, backend, or join target changes, so
    # every member reacts together instead of only the first server.
    version                  = var.release
    server                   = local.server_url
    flannel_backend          = var.flannel_backend
    cluster_cidr             = var.cluster_cidr
    service_cidr             = var.service_cidr
    server_https_listen_port = var.server_https_listen_port
    service_node_port_range  = var.service_node_port_range
    node_internal_ip         = lookup(var.node_internal_ip, each.value.host, "")
    image_archives_dir       = var.image_archives_dir
    # See server_init: both feed this node's install command.
    mirror                  = var.mirror
    system_default_registry = var.system_default_registry
    # The first server owns the datastore and the cluster CA, so a member that outlives a
    # reinstall of it holds credentials for a cluster that no longer exists. Reinstalling it
    # therefore reinstalls every other member too -- including the case a taint causes, where
    # nothing else about this node changed.
    server_init = null_resource.server_init.id
  }

  lifecycle {
    # See server_init.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.k3s.io."
    }
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)

    # Empty when this node is reachable directly, which is how Terraform is told to use no bastion at
    # all. Read off triggers so a destroy-time provisioner -- which may reference only self -- can
    # still reach a node that is only reachable through the jumper.
    #
    # lookup() with a default, not self.triggers.x: a resource already in state from before these keys
    # existed has no such element, and reading it directly fails its DESTROY -- leaving a node
    # installed with no way for this module to reclaim it.
    bastion_host        = lookup(self.triggers, "bastion_host", "")
    bastion_user        = lookup(self.triggers, "bastion_user", "")
    bastion_port        = tonumber(lookup(self.triggers, "bastion_port", "22"))
    bastion_private_key = file(self.triggers.key_path)
  }

  provisioner "file" {
    source      = "${path.module}/scripts/image-archives.sh"
    destination = local.image_archives_script
  }

  provisioner "remote-exec" {
    inline = [
      # Reclaim the host before joining (see server_init).
      "if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; fi",
      "if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then sudo /usr/local/bin/k3s-agent-uninstall.sh; fi",
      # k3s-uninstall drops cni0/flannel.1 but not flannel's host-gw routes, which
      # live on the physical NIC and survive it. A re-apply that reassigns pod CIDRs
      # (e.g. swapped server/agent roles) would otherwise inherit stale routes that
      # send a node's own pod subnet to its peer and black-hole pod traffic. Flush
      # the routes under the configured cluster CIDR (var.cluster_cidr, split for
      # dual-stack) so fresh flannel rebuilds a clean table. No-op for vxlan.
      "for c in $(echo '${var.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
      local.join_wait,
      local.stage_image_archives,
      "${local.install_prefix} ${local.install_sh} server --server ${local.server_url} --flannel-backend ${var.flannel_backend} --cluster-cidr ${var.cluster_cidr} --service-cidr ${var.service_cidr} ${local.tls_san_flags[each.value.host]} --https-listen-port ${var.server_https_listen_port} --service-node-port-range ${var.service_node_port_range} ${local.node_internal_ip_flag[each.value.host]} ${local.node_external_ip_flag[each.value.host]} ${local.advertise_flag[each.value.host]}${local.system_default_registry_flag} || exit 1",
      local.version_assert,
    ]
  }

  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      "sudo /usr/local/bin/k3s-uninstall.sh",
      # Also drop flannel's leftover host-gw pod-CIDR routes (see the reclaim step)
      # so destroy leaves the node's routing table clean for any later re-provision.
      # Destroy provisioners may reference only self, so read the CIDR off triggers.
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Join agents as workers. Empty when no agents are given, in which case the
# servers run workloads themselves (K3s servers are schedulable when untainted).
resource "null_resource" "agent" {
  for_each   = local.agent_hosts
  depends_on = [null_resource.server_init, null_resource.vars_snapshot]

  triggers = {
    host         = each.value.host
    user         = each.value.user
    port         = var.agent_ssh_port
    key_path     = pathexpand(var.ssh_private_key)
    bastion_host = local.bastion_of[each.value.host]
    bastion_user = local.bastion_user_of[each.value.host]
    bastion_port = local.bastion_port_of[each.value.host]
    # Re-run the install when the release or the server URL changes, so agents
    # track the servers instead of only the first server being reinstalled.
    version                  = var.release
    server                   = local.server_url
    server_https_listen_port = var.server_https_listen_port
    node_internal_ip         = lookup(var.node_internal_ip, each.value.host, "")
    # Tracked so this agent re-provisions when the pod network changes, and so the
    # destroy-time route flush can read the CIDR off self.triggers.
    cluster_cidr       = var.cluster_cidr
    image_archives_dir = var.image_archives_dir
    # Where this node's cache is filled from feeds its staging command; system_default_registry
    # is server-only (the k3s agent CLI has no such flag), so it is not tracked here.
    mirror = var.mirror
    # See server_join: an agent that outlives a reinstall of the first server holds credentials
    # for a cluster that no longer exists.
    server_init = null_resource.server_init.id
  }

  lifecycle {
    # See server_init.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.k3s.io."
    }
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)

    # Empty when this node is reachable directly, which is how Terraform is told to use no bastion at
    # all. Read off triggers so a destroy-time provisioner -- which may reference only self -- can
    # still reach a node that is only reachable through the jumper.
    #
    # lookup() with a default, not self.triggers.x: a resource already in state from before these keys
    # existed has no such element, and reading it directly fails its DESTROY -- leaving a node
    # installed with no way for this module to reclaim it.
    bastion_host        = lookup(self.triggers, "bastion_host", "")
    bastion_user        = lookup(self.triggers, "bastion_user", "")
    bastion_port        = tonumber(lookup(self.triggers, "bastion_port", "22"))
    bastion_private_key = file(self.triggers.key_path)
  }

  provisioner "file" {
    source      = "${path.module}/scripts/image-archives.sh"
    destination = local.image_archives_script
  }

  provisioner "remote-exec" {
    inline = [
      # Reclaim the host before joining (see server_init).
      "if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; fi",
      "if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then sudo /usr/local/bin/k3s-agent-uninstall.sh; fi",
      # k3s-uninstall drops cni0/flannel.1 but not flannel's host-gw routes, which
      # live on the physical NIC and survive it. A re-apply that reassigns pod CIDRs
      # (e.g. swapped server/agent roles) would otherwise inherit stale routes that
      # send a node's own pod subnet to its peer and black-hole pod traffic. Flush
      # the routes under the configured cluster CIDR (var.cluster_cidr, split for
      # dual-stack) so fresh flannel rebuilds a clean table. No-op for vxlan.
      "for c in $(echo '${var.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
      local.join_wait,
      local.stage_image_archives,
      "${local.install_prefix} K3S_URL='${local.server_url}' ${local.install_sh} agent ${local.node_internal_ip_flag[each.value.host]} ${local.node_external_ip_flag[each.value.host]} || exit 1",
      local.version_assert,
    ]
  }

  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      "sudo /usr/local/bin/k3s-agent-uninstall.sh",
      # Also drop flannel's leftover host-gw pod-CIDR routes (see the reclaim step)
      # so destroy leaves the node's routing table clean for any later re-provision.
      # Destroy provisioners may reference only self, so read the CIDR off triggers.
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Give each node's k3s containerd the same custom runtimes (e.g. ascend) as its
# Docker daemon.json, dropped for nvidia (k3s auto-detects and wires that one
# itself), and register a RuntimeClass for each so a workload can select it.
# daemon.json is fetched over SSH to the local machine and parsed with local jq
# (scripts/render-containerd-runtimes.sh), since the remote host is not
# guaranteed to have jq. Re-derives whenever the node is (re)installed; an empty
# result removes any previously-written templates and restarts only on change.
resource "null_resource" "containerd_config" {
  for_each   = local.containerd_nodes
  depends_on = [null_resource.server_init, null_resource.server_join, null_resource.agent, null_resource.kubeconfig]

  triggers = {
    host       = each.key
    user       = each.value.user
    port       = each.value.port
    key_path   = pathexpand(var.ssh_private_key)
    service    = each.value.service
    install_id = each.value.install_id
    # Tracked so a change of jumper re-runs this step, which reaches the node itself.
    ssh_proxy = local.ssh_proxy_of[each.key]
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -uo pipefail
      die() {
        echo "$*" >&2
        exit 1
      }
      dir=/var/lib/rancher/k3s/agent/etc/containerd
      # Checked before it is used, because the renderer below is piped through `|| true`: without jq
      # its empty output reads as "this node has no custom runtimes", and that path DELETES the
      # templates and restarts containerd. A missing tool on this workstation must not look like a
      # fact about the node.
      command -v jq >/dev/null 2>&1 ||
        die "jq is required on this workstation to read a node's /etc/docker/daemon.json"
      # Every ssh below carries the same options, as an array so the ProxyCommand for a node reachable
      # only through the jumper stays one word.
      ssh_opts=(-i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10)
      ${local.ssh_proxy_of[each.key]}
      node='${self.triggers.user}@${self.triggers.host}'

      # The inner `|| true` makes a MISSING daemon.json an empty answer, while an ssh that cannot
      # reach the node still fails here. The difference matters: "no runtimes" removes the templates
      # and restarts the service, so taking an unreachable node for an empty one would tear down a
      # working node's runtimes on a transient network fault.
      daemon_json="$(ssh "$${ssh_opts[@]}" "$node" 'sudo cat /etc/docker/daemon.json 2>/dev/null || true')" ||
        die "cannot reach ${self.triggers.host} to read /etc/docker/daemon.json"
      # Two renderings of the same runtimes: containerd's CRI plugin is named
      # io.containerd.grpc.v1.cri in config version 2 but io.containerd.cri.v1.runtime in
      # version 3, and containerd ignores a runtime declared under the other version's path
      # without a word. One blob written to both templates therefore leaves whichever
      # containerd the node actually runs (2.x, config version 3, on k3s v1.34+) with no such
      # runtime at all, and every Pod selecting it fails to get a sandbox.
      rendered="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 2 || true)"
      rendered_v3="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 3 || true)"
      existing="$(ssh "$${ssh_opts[@]}" "$node" "sudo cat $dir/config.toml.tmpl 2>/dev/null" || true)"
      existing_v3="$(ssh "$${ssh_opts[@]}" "$node" "sudo cat $dir/config-v3.toml.tmpl 2>/dev/null" || true)"

      # A handler in containerd is only half of it: a Pod picks a runtime through a
      # RuntimeClass naming that handler, and a vendor whose device nodes reach the container
      # only through its runtime hook (amd-container-runtime injects /dev/kfd and /dev/dri)
      # gets none at all without one. The names are read back out of the rendered stanzas, so
      # the classes can never name a runtime containerd was not given. Applied on every run
      # rather than only on a change, so a deleted class comes back, and before the restart
      # below, so the apiserver is still up when it happens.
      # Every class this module creates is labelled, for two reasons. It is how a class belonging to a
      # vendor's own operator is left untouched rather than adopted, and it is how a class whose handler
      # this module has since stopped rendering can be pruned below without guessing at ownership.
      owner_label='gpustack.ai/managed-by=testing-infra-k3s'
      wanted=$(printf '%s\n' "$rendered_v3" | sed -n 's/^\[plugins\."[^"]*"\.containerd\.runtimes\.\([^.]*\)\]$/\1/p')

      for name in $wanted; do
        # A read that fails is not an answer. Treating a transient failure as "absent" would adopt — and
        # later prune — a class another component owns, so it is retried and only a real answer decides.
        for i in $(seq 1 12); do
          if foreign=$(KUBECONFIG='${local.kubeconfig_path}' kubectl get runtimeclass.node.k8s.io "$name" \
            -o 'jsonpath={.metadata.labels.gpustack\.ai/managed-by}' 2>/dev/null); then
            break
          fi
          if KUBECONFIG='${local.kubeconfig_path}' kubectl get runtimeclass.node.k8s.io >/dev/null 2>&1; then
            foreign=""   # the API answered and the class simply does not exist yet
            break
          fi
          if [ "$i" = 12 ]; then
            echo "cannot read RuntimeClass $name on ${self.triggers.host}" >&2
            exit 1
          fi
          sleep 5
        done
        if [ -n "$foreign" ] && [ "$foreign" != 'testing-infra-k3s' ]; then
          echo "RuntimeClass $name belongs to $foreign; left as found"
          continue
        fi
        for i in $(seq 1 12); do
          printf 'apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: %s\n  labels:\n    %s: %s\nhandler: %s\n' \
            "$name" 'gpustack.ai/managed-by' 'testing-infra-k3s' "$name" \
            | KUBECONFIG='${local.kubeconfig_path}' kubectl apply -f - && break
          if [ "$i" = 12 ]; then
            echo "failed to register RuntimeClass $name for ${self.triggers.host}" >&2
            exit 1
          fi
          sleep 5
        done
      done

      # Prune ours whose handler is no longer rendered. A class outliving its handler is worse than no
      # class: the kubelet rejects every Pod naming it, and nothing in the cluster says why.
      if ours=$(KUBECONFIG='${local.kubeconfig_path}' kubectl get runtimeclass.node.k8s.io \
        -l "$owner_label" -o name 2>/dev/null | sed 's|^.*/||'); then
        for name in $ours; do
          if ! printf '%s\n' "$wanted" | grep -qx "$name"; then
            # A failed delete fails the step. A class the kubelet rejects every Pod for, left behind
            # while the apply reports success, is the outcome this prune exists to prevent.
            if KUBECONFIG='${local.kubeconfig_path}' kubectl delete runtimeclass.node.k8s.io "$name" --ignore-not-found; then
              echo "pruned RuntimeClass $name; its containerd handler is no longer rendered"
            else
              echo "cannot delete RuntimeClass $name, whose containerd handler is no longer rendered" >&2
              exit 1
            fi
          fi
        done
      else
        echo "could not list this module's RuntimeClasses on ${self.triggers.host}; none pruned" >&2
      fi

      if [ "$rendered" = "$existing" ] && [ "$rendered_v3" = "$existing_v3" ]; then
        echo "containerd runtime templates on ${self.triggers.host} unchanged"
        exit 0
      fi

      # Every write below is checked. Each branch ends in an echo, so an unchecked failure would exit
      # 0 having reported a change that did not happen.
      if [ -z "$rendered" ]; then
        ssh "$${ssh_opts[@]}" "$node" \
          "sudo rm -f $dir/config.toml.tmpl $dir/config-v3.toml.tmpl && sudo systemctl restart ${self.triggers.service}" ||
          die "cannot remove the containerd runtime templates on ${self.triggers.host}"
        echo "removed stale containerd runtime template on ${self.triggers.host}"
        exit 0
      fi

      ssh "$${ssh_opts[@]}" "$node" "sudo mkdir -p $dir" || die "cannot create $dir on ${self.triggers.host}"
      printf '%s\n' "$rendered" | ssh "$${ssh_opts[@]}" "$node" "sudo tee $dir/config.toml.tmpl > /dev/null" ||
        die "cannot write $dir/config.toml.tmpl on ${self.triggers.host}"
      printf '%s\n' "$rendered_v3" | ssh "$${ssh_opts[@]}" "$node" "sudo tee $dir/config-v3.toml.tmpl > /dev/null" ||
        die "cannot write $dir/config-v3.toml.tmpl on ${self.triggers.host}"
      ssh "$${ssh_opts[@]}" "$node" "sudo systemctl restart ${self.triggers.service}" ||
        die "cannot restart ${self.triggers.service} on ${self.triggers.host}"
      echo "wrote containerd runtime templates on ${self.triggers.host} and restarted ${self.triggers.service}"
    EOT
  }
}

# Fetch the kubeconfig via sudo (k3s writes it root-only at mode 600), namespace
# its "default" identifiers to a per-cluster context, repoint the server URL from
# 127.0.0.1 to the reachable host, and flatten-merge it into ~/.kube/config. Retries because
# k3s.yaml appears a few seconds after the service starts. The sed rules are
# anchored to whole identity lines so the base64 certificate blobs stay intact.
resource "null_resource" "kubeconfig" {
  # Every install, not just the first server: the node-registration wait below needs them all to
  # have run before it can hold the apply to the count.
  depends_on = [null_resource.server_init, null_resource.server_join, null_resource.agent]

  triggers = {
    host                     = local.first_server.host
    context                  = local.context_name
    server_https_listen_port = var.server_https_listen_port
    # Re-fetch when the first server is reinstalled (new certificates), so
    # ~/.kube/config never keeps stale credentials.
    server_init = null_resource.server_init.id
    # Tracked so flipping the flag re-runs the merge, instead of taking effect only
    # the next time the server happens to be reinstalled.
    switch_kube_context = var.switch_kube_context
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      raw="$(mktemp)"
      merged=""
      trap 'rm -f "$raw" "$merged"' EXIT
      # Read before the merge: the merged view takes its current-context from the new
      # file (first in KUBECONFIG below), so keeping the current context means putting
      # this one back afterwards. Empty when there is no ~/.kube/config yet.
      previous="$(KUBECONFIG="$HOME/.kube/config" kubectl config current-context 2>/dev/null || true)"
      ssh_opts=(-i '${pathexpand(var.ssh_private_key)}' -p ${var.server_ssh_port} -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10)
      ${local.ssh_proxy_of[local.first_server.host]}
      for i in $(seq 1 30); do
        if ssh "$${ssh_opts[@]}" '${local.first_server.user}@${local.first_server.host}' \
             'sudo test -s /etc/rancher/k3s/k3s.yaml && sudo cat /etc/rancher/k3s/k3s.yaml' 2>/dev/null \
             > "$raw" && test -s "$raw"; then
          sed -E \
            -e 's|https://127\.0\.0\.1:[0-9]+|https://${local.kubeconfig_host}:${var.server_https_listen_port}|' \
            -e 's|^  name: default$|  name: ${local.context_name}|' \
            -e 's|^    cluster: default$|    cluster: ${local.context_name}|' \
            -e 's|^    user: default$|    user: ${local.context_name}|' \
            -e 's|^- name: default$|- name: ${local.context_name}|' \
            -e 's|^current-context: default$|current-context: ${local.context_name}|' \
            "$raw" > '${local.kubeconfig_path}'
          chmod 600 '${local.kubeconfig_path}'
          mkdir -p "$HOME/.kube"
          # Unique temp in ~/.kube so concurrent/interrupted applies can't clobber
          # a shared file, and the mv stays an atomic same-filesystem rename.
          merged="$(mktemp "$HOME/.kube/config.XXXXXX")"
          # New file first: its entries win on conflict, so a re-apply refreshes
          # this cluster and makes it the current context (as aws update-kubeconfig
          # does) unless var.switch_kube_context puts the previous one back below.
          KUBECONFIG='${local.kubeconfig_path}':"$HOME/.kube/config" \
            kubectl config view --flatten > "$merged"
          mv "$merged" "$HOME/.kube/config"
          chmod 600 "$HOME/.kube/config"
          if [ '${var.switch_kube_context}' = 'false' ] && [ -n "$previous" ]; then
            KUBECONFIG="$HOME/.kube/config" kubectl config use-context "$previous" >/dev/null
            echo "merged context ${local.context_name} into ~/.kube/config; current context left at $previous"
          else
            echo "merged context ${local.context_name} into ~/.kube/config; it is now the current context"
          fi
          # Every node this module installed has to be IN the cluster before the apply reports
          # success. A node whose service starts but never joins -- a supervisor endpoint it cannot
          # reach, a flag its config rejects -- leaves its own step green. This resource depends on
          # every install, so it is where the count is known. Registration, not Ready: a node is
          # registered as soon as its kubelet reaches the apiserver, whereas Ready also waits for the
          # CNI.
          for j in $(seq 1 60); do
            registered="$(KUBECONFIG='${local.kubeconfig_path}' kubectl get nodes -o name --request-timeout=10s 2>/dev/null | grep -c . || true)"
            if [ "$${registered:-0}" -ge ${local.node_count} ]; then
              echo "all ${local.node_count} node(s) have registered"
              exit 0
            fi
            echo "waiting for all ${local.node_count} node(s) to register ($registered so far, $j/60)"
            sleep 5
          done
          echo "timed out with $registered of ${local.node_count} node(s) registered; a node whose service started but never joined does not appear here" >&2
          exit 1
        fi
        echo "waiting for k3s.yaml on ${local.first_server.host} ($i/30)"
        sleep 5
      done
      echo "timed out waiting for k3s.yaml" >&2
      exit 1
    EOT
  }

  # Remove this cluster's context, cluster, and user from ~/.kube/config on
  # destroy. Only self is referenceable here; on_failure=continue keeps
  # re-destroys idempotent when the entries are already gone.
  #
  # Anchored to the same file the merge above writes, rather than to whatever
  # kubectl would resolve: with a KUBECONFIG set, cleanup would otherwise strip
  # the entries from that file and leave these behind in ~/.kube/config forever.
  #
  # Deleting the context does NOT clear current-context when it names that context — kubectl leaves
  # the field pointing at a context that no longer exists — and that dangling value is exactly what
  # makes the next apply fail under switch_kube_context = false, which reads the current context in
  # order to restore it. So clear it too.
  provisioner "local-exec" {
    when        = destroy
    on_failure  = continue
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      export KUBECONFIG="$HOME/.kube/config"
      kubectl config delete-context '${self.triggers.context}' || true
      kubectl config delete-cluster '${self.triggers.context}' || true
      kubectl config unset 'users.${self.triggers.context}' || true
      if [ "$(kubectl config current-context 2>/dev/null || true)" = '${self.triggers.context}' ]; then
        kubectl config unset current-context >/dev/null || true
      fi
    EOT
  }
}

# Carries server — the one variable with no default — across a failed or interrupted destroy retry.
# Terraform auto-loads *.auto.tfvars.json on every command (incl. destroy), and command-line -var
# still overrides it on apply. A managed hashicorp/local local_file is not used: it is deleted during
# destroy, which is exactly when the value is still needed.
#
# Everything else in this module depends on THIS resource, rather than the other way round, and that
# inversion is what makes the file's lifetime match its purpose:
#
#   - it is written before the first node is touched, so an apply that dies mid-install still leaves a
#     snapshot to destroy with. Written last, as it was, the one failure it exists for produced no file.
#   - Terraform destroys dependents first, so this resource is destroyed LAST -- after every node is
#     gone -- and its destroy-time provisioner can therefore remove the file without taking it away
#     while it is still needed. Verified: with a node's destroy failing, the file and the state both
#     survive for the retry; with the destroy succeeding, the file is gone.
#
# Snapshot NOTHING that has a default. Auto-loading is indiscriminate — it feeds `apply` just as
# readily as `destroy` — so a variable recorded here silently overrides its own default on every later
# apply in this directory, with nothing on the command line to hint at it. Nothing else is needed at
# destroy time either: every destroy provisioner reads its connection and its arguments off
# self.triggers, which live in state.
resource "null_resource" "vars_snapshot" {
  triggers = {
    snapshot = jsonencode({
      server = var.server
    })
    # In triggers, not spliced from path.module: a destroy-time provisioner may reference only self.
    path = "${path.module}/.last-apply.auto.tfvars.json"
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${self.triggers.path}' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }

  # Only reached once every node has been destroyed, so a completed destroy leaves the directory as it
  # was found. A destroy that failed part-way never gets here, which is the point.
  provisioner "local-exec" {
    when        = destroy
    interpreter = ["/bin/bash", "-c"]
    command     = "rm -f '${self.triggers.path}'"
  }
}
