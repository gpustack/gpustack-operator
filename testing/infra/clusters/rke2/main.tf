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

  # Additional control-plane servers and agents join the first server. Keyed by host so
  # adding/removing one host never disturbs the others.
  join_servers = { for s in slice(local.servers, 1, length(local.servers)) : s.host => s }
  agent_hosts  = { for a in local.agents : a.host => a }

  # Every node this module installs, servers and agents together. The count the cluster has to
  # reach before an apply is allowed to report success.
  node_count = 1 + length(local.join_servers) + length(local.agent_hosts)

  # RKE2 fixes both of its ports and exposes no flag for either: 6443 for the apiserver, 9345 for
  # the supervisor endpoint other nodes join through. That is why this module has no port variable
  # to match the k3s module's server_https_listen_port.
  apiserver_port = 6443

  # The jump host, parsed like any other address. Every node whose SSH host differs from this one is
  # reached through it; the jumper's own host is reached directly.
  jumper_host = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[1] : var.ssh_jumper)
  jumper_user = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[0] : var.ssh_user)

  # Per node: the bastion Terraform's own provisioner connections use, empty when the node needs none.
  bastion_of = { for host in concat([for s in local.servers : s.host], [for a in local.agents : a.host]) :
    host => host == local.jumper_host ? "" : local.jumper_host
  }

  # The user and port that bastion is reached with -- neutral values for a node that has none, because
  # Terraform ignores both when bastion_host is empty and recording them regardless makes them a
  # trigger that fires without a reachability change. Measured on the pair: turning ON a jumper that IS
  # the first server left bastion_host untouched at "" but moved bastion_user to "root", which replaced
  # the server -- an rke2 reinstall on the control plane, and on a single-server cluster that is etcd.
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
  # node, and -- with no `set -e` -- the step carries on and reports "nothing to change". Observed
  # doing exactly that: a silent no-op that looked like success.
  ssh_proxy_of = { for host, bastion in local.bastion_of :
    host => bastion == "" ? ":" : "ssh_opts+=(-o \"ProxyCommand=ssh -i ${pathexpand(var.ssh_private_key)} -p ${var.ssh_jumper_port} -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -W %h:%p ${local.jumper_user}@${local.jumper_host}\")"
  }

  # kubectl context/cluster/user name for the fetched kubeconfig. The raw rke2.yaml names
  # everything "default", so we namespace it to the first server host (sanitized to the characters
  # kubectl names accept).
  context_name = "rke2-${replace(local.first_server.host, "/[^a-zA-Z0-9]/", "-")}"

  # Standalone rewritten kubeconfig kept in the module dir; also merged into ~/.kube/config.
  kubeconfig_path = "${path.module}/kubeconfig"

  # Uploaded to every node and run there. It lands in the SSH user's HOME rather than under a fixed
  # name in /tmp, which is world-writable: any local user there could pre-place or swap that name
  # between the upload and the `sudo bash` that runs it. A relative destination is what the file
  # provisioner resolves against the remote home.
  images_script      = ".gpustack-rke2-image-archives.sh"
  images_script_path = "$HOME/.gpustack-rke2-image-archives.sh"

  # The directory the installer is handed as INSTALL_RKE2_ARTIFACT_PATH. Module-owned and filled
  # from an allowlist, because the installer copies every file matching rke2-images-*.linux-<arch>*
  # into the images directory without looking at its extension -- pointing it at the operator's own
  # cache would let a stray .txt there become "pull every image named in me". Outside
  # /var/lib/rancher/rke2 so the uninstall cannot take it, and on the same filesystem as a cache
  # under /var/lib so the fill is a hardlink rather than a ~1 GB copy.
  artifact_staging_dir = "/var/lib/rke2-install-artifacts"

  cache_release_dir = "${var.image_archives_dir}/${var.release}"

  # Where the cache is filled FROM; empty leaves the cache steps byte-identical to before. fetch
  # and artifacts download (artifacts refreshes a missing anchor); stage only copies locally.
  mirror_arg = var.mirror == "" ? "" : " --mirror '${var.mirror}'"

  # Three spliced steps, all no-ops when no cache is configured: warm the cache and fill the
  # installer's directory before the install, then clean up and stage any extra bundles after it.
  cache_prepare = var.image_archives_dir == "" ? "echo 'image_archives_dir is unset; RKE2 will download its artifacts and pull its images from a registry'" : join("\n", [
    "sudo bash ${local.images_script_path} fetch --release '${var.release}' --cache-dir '${var.image_archives_dir}' --cni '${var.cni}'${local.mirror_arg}",
    "sudo bash ${local.images_script_path} artifacts --release '${var.release}' --cache-dir '${var.image_archives_dir}' --cni '${var.cni}' --staging-dir '${local.artifact_staging_dir}'${local.mirror_arg}",
  ])

  cache_finish = var.image_archives_dir == "" ? "echo 'no artifact cache to stage'" : join("\n", [
    "sudo rm -rf '${local.artifact_staging_dir}'",
    "sudo bash ${local.images_script_path} stage --release '${var.release}' --cache-dir '${var.image_archives_dir}'${local.mirror_arg}",
  ])

  # INSTALL_RKE2_METHOD=tar is always set: on a host that has yum the installer otherwise defaults
  # to the rpm method, which adds a Rancher repository. With a warm cache nothing is downloaded at
  # all -- the installer script itself comes from the cache too, so an upstream edit cannot change
  # what a re-apply installs.
  #
  # `|| exit 1` for the same reason as cache_prepare: Terraform concatenates a step's inline entries
  # into ONE script, whose exit code is the last command's. An installer that fails after putting the
  # binary in place would otherwise be answered by the version assertion below and reported as a
  # success.
  install_cmd = { for type in ["server", "agent"] :
    type => var.image_archives_dir == "" ? "curl -sfL https://get.rke2.io | sudo INSTALL_RKE2_METHOD=tar INSTALL_RKE2_TYPE=${type} INSTALL_RKE2_VERSION='${var.release}' sh - || exit 1" : "sudo INSTALL_RKE2_METHOD=tar INSTALL_RKE2_TYPE=${type} INSTALL_RKE2_ARTIFACT_PATH='${local.artifact_staging_dir}' sh '${local.cache_release_dir}/install.sh' || exit 1"
  }

  # The installer's own uninstall script, wherever it ended up: the tar method falls back to
  # /opt/rke2 when /usr/local is read-only or a dedicated mount point, and an earlier rpm-method
  # install puts it in /usr/bin. Probed rather than hardcoded, because a hardcoded miss under
  # on_failure = continue is swallowed silently and the NEXT apply then dies inside the installer's
  # own check_method_conflict.
  #
  # The uninstall's own failure is reported and stepped over, not propagated: this runs under the
  # `set -e` of local.reclaim, so an uninstaller that exits non-zero on a half-removed host would
  # fail the reclaim -- and that host could then never be reclaimed by any later apply either. The
  # installer's check_method_conflict is what catches a genuinely unusable host a step later.
  uninstall_probe = "for d in /usr/local/bin /opt/rke2/bin /usr/bin; do if [ -x \"$d/rke2-uninstall.sh\" ]; then sudo \"$d/rke2-uninstall.sh\" || echo \"$d/rke2-uninstall.sh exited non-zero; installing over it\" >&2; break; fi; done"

  # RKE2 gives every node a per-node password: the node keeps it in /etc/rancher/node/password and the
  # cluster keeps its hash in a <node name>.node-password.rke2 Secret. rke2-uninstall.sh removes
  # /etc/rancher/node, so a node reinstalled while the CLUSTER survives comes back with a fresh password
  # the server rejects -- "Node password rejected, duplicate hostname" -- and it can then never rejoin.
  # Measured on the lab pair: re-provisioning ONLY the agent (a changed cni or node_internal_ip is
  # enough) left it in that loop indefinitely, and the node stayed NotReady with the apply blocked on
  # the service start.
  #
  # Preserving the file is the intended semantics -- the password identifies the machine, and it is the
  # same machine -- and it is harmless when the whole cluster is being rebuilt, because a fresh server has
  # no entry to disagree with. Saved outside /etc/rancher/rke2 and /etc/rancher/node, the two trees the
  # uninstall removes. The k3s module needs none of this: its uninstall removes only /etc/rancher/k3s.
  node_password_save    = "if [ -f /etc/rancher/node/password ]; then sudo cp /etc/rancher/node/password /etc/rancher/node-password.saved && sudo chmod 600 /etc/rancher/node-password.saved; fi"
  node_password_restore = "if [ -f /etc/rancher/node-password.saved ]; then sudo mkdir -p /etc/rancher/node && sudo cp /etc/rancher/node-password.saved /etc/rancher/node/password && sudo chmod 600 /etc/rancher/node/password && sudo rm -f /etc/rancher/node-password.saved; fi"

  # Calico leaves a blackhole route for the pod CIDR behind after the uninstall. A re-provision that
  # reuses the CIDR would inherit it and black-hole its own pod traffic, so the routes under the
  # configured cluster CIDR (split for dual-stack) are flushed on both reclaim and destroy.
  route_flush = "for c in $(echo '${var.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done"

  # node-ip comes ONLY from an explicit var.node_internal_ip entry, never from the SSH host, even
  # though the convention is that a host with no entry SSHes on its cluster address already. Copying
  # the SSH host out would turn that convention into an assertion about the machine: an SSH host that
  # happens to be an IP literal is not evidence that the node owns it -- a public, floating, or NAT
  # address reaches the host without appearing on any of its interfaces, and a kubelet given an
  # address its host does not hold refuses to start. With no entry RKE2 detects the address on the
  # default route instead, the same answer when the convention holds and a working node when it does
  # not.
  node_internal_ip_lines = { for host in concat([for s in local.servers : s.host], [for a in local.agents : a.host]) :
    host => lookup(var.node_internal_ip, host, "") == "" ? [] : ["node-ip: ${var.node_internal_ip[host]}"]
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

  # The KEY side of node_internal_ip, and the only place it reaches the cluster: the address you SSH
  # to becomes the node's ExternalIP. Only where the two addresses actually differ -- a host mapped to
  # itself has no separate outward address to advertise, and a host with no entry has none either.
  # node-external-ip is an agent/networking setting, so every node takes it.
  external_ip_lines = { for host in concat([for s in local.servers : s.host], [for a in local.agents : a.host]) :
    host => lookup(var.node_internal_ip, host, "") == "" || var.node_internal_ip[host] == host ? [] : ["node-external-ip: ${host}"]
  }

  # Pinned rather than left to default, and SERVERS ONLY -- advertise-address is a listener setting,
  # and an unknown key in an agent's config.yaml is fatal at flag parsing, which is a crash loop.
  #
  # RKE2 derives advertise-address from node-external-ip, then node-ip. So declaring an external
  # address above moves the address the apiserver advertises to cluster members -- and with it the
  # `kubernetes` Service endpoint -- onto the outward address, which no node holds. Measured by
  # removing this key on the lab pair: the endpoint became the outward address immediately, and came
  # back on restoring it.
  #
  # What that costs is worth stating exactly, because it is not always a break. There the cluster kept
  # working: a pod still reached the apiserver, because that network hairpins its NAT. So the real
  # cost is a silent dependency on the hairpin plus a round trip out and back for every in-cluster API
  # call -- on a cluster that holds a perfectly good internal address. On a network that does not
  # hairpin, which is the usual shape of a cloud floating address, it is a break.
  advertise_lines = { for host in [for s in local.servers : s.host] :
    host => lookup(var.node_internal_ip, host, "") == "" || var.node_internal_ip[host] == host ? [] : ["advertise-address: ${var.node_internal_ip[host]}"]
  }

  # tls-san carries the SSH host, because that is the address the fetched kubeconfig is rewritten to,
  # plus this node's cluster address when the two differ -- a certificate that covers only one of them
  # fails whichever client uses the other -- plus the jump host when there is a separate one.
  # compact() drops the addresses that do not apply and distinct() collapses a host mapped to itself.
  tls_san_lines = { for host in [for s in local.servers : s.host] :
    host => concat(["tls-san:"], [
      for san in distinct(compact([host, lookup(var.node_internal_ip, host, ""), local.jumper_san])) :
      "  - ${san}"
    ])
  }

  # The address other nodes join through. It has to be one THEY can reach, which is the reason
  # node_internal_ip exists: on a host whose SSH address is public or NAT'd, the SSH address is not
  # it. With no entry it falls back to the SSH host -- which the convention says IS that host's
  # cluster address, so this is a contract rather than a guess. The pre-join probe below is what fails
  # fast, naming the address, for the caller who broke the convention.
  join_addr = lookup(var.node_internal_ip, local.first_server.host, local.first_server.host)
  # An IPv6 literal has to be bracketed in a URL, and must NOT be bracketed in bash's /dev/tcp path.
  join_url = "https://${strcontains(local.join_addr, ":") ? "[${local.join_addr}]" : local.join_addr}:9345"
  # The kubeconfig carries the SSH host rather than the join address, since kubectl runs from this
  # workstation. Bracketed by the same rule: an unbracketed IPv6 literal is an unparseable URL.
  kubeconfig_host = strcontains(local.first_server.host, ":") ? "[${local.first_server.host}]" : local.first_server.host

  # Every managed node (servers and agents), keyed by host, with its connection details, service name,
  # and the install resource that must land first.
  containerd_nodes = merge(
    {
      (local.first_server.host) = {
        user       = local.first_server.user
        port       = var.server_ssh_port
        service    = "rke2-server"
        install_id = null_resource.server_init.id
      }
    },
    {
      for host, s in local.join_servers : host => {
        user       = s.user
        port       = var.server_ssh_port
        service    = "rke2-server"
        install_id = null_resource.server_join[host].id
      }
    },
    {
      for host, a in local.agent_hosts : host => {
        user       = a.user
        port       = var.agent_ssh_port
        service    = "rke2-agent"
        install_id = null_resource.agent[host].id
      }
    },
  )

  # Half of the multi-NIC fix has to be in place before Calico is ever deployed, and it is the half
  # that needs no node identity: which address Calico advertises as its VXLAN tunnel endpoint.
  # Measured on the lab pair, Calico's default "first found" autodetection picked a local container
  # bridge on one node and a VLAN address on the other, so every node encapsulated to an
  # address the others cannot route -- cross-node pod traffic dies while the nodes look Ready. Pinning
  # it to the kubelet's InternalIP (which node_internal_ip already fixes) is what makes it correct.
  #
  # Written as a HelmChartConfig into server/manifests/ rather than patched afterwards: this is how
  # RKE2's bundled charts are meant to be configured, so it survives an rke2-server restart
  # re-running the helm job. The objections that rule out pre-start manifests for the per-node
  # FelixConfiguration do not apply here -- there is no node name to guess and no Calico CRD to race,
  # because this configures the chart rather than an object the chart owns.
  #
  # firstFound is set to false rather than removed. Helm merges maps, so leaving it out lets the
  # chart's own `firstFound: true` stay alongside and win; and `null` cannot remove it either -- the
  # chart renders the literal into the Installation, and the operator's CRD rejects "null" for a
  # boolean field, which fails the helm install outright and leaves the cluster with no CNI at all.
  calico_helm_config = <<-EOT
    apiVersion: helm.cattle.io/v1
    kind: HelmChartConfig
    metadata:
      name: rke2-calico
      namespace: kube-system
    spec:
      valuesContent: |-
        installation:
          calicoNetwork:
            nodeAddressAutodetectionV4:
              firstFound: false
              kubernetes: NodeInternalIP
  EOT

  # Written on servers only (agents have no server/manifests), and only for Calico.
  write_calico_helm_config = var.cni == "calico" && local.calico_fix_enabled ? join("\n", [
    "sudo mkdir -p /var/lib/rancher/rke2/server/manifests",
    "printf '%s' '${local.calico_helm_config}' | sudo tee /var/lib/rancher/rke2/server/manifests/rke2-calico-config.yaml > /dev/null",
  ]) : "sudo rm -f /var/lib/rancher/rke2/server/manifests/rke2-calico-config.yaml 2>/dev/null || true"

  # The fix is Calico-specific, so it follows var.cni unless the caller says otherwise.
  calico_fix_enabled = var.calico_multi_nic_fix == null ? var.cni == "calico" : var.calico_multi_nic_fix

  # An Agent/Runtime setting per the RKE2 reference, valid on servers and agents alike, so it
  # goes into every node's config.yaml. Empty means the key is not written, exactly as before.
  # The value is double-quoted: a bracketed IPv6 literal is otherwise a YAML flow sequence.
  system_default_registry_lines = var.system_default_registry == "" ? [] : ["system-default-registry: \"${var.system_default_registry}\""]

  server_common = concat([
    "cni: ${var.cni}",
    "cluster-cidr: ${var.cluster_cidr}",
    "service-cidr: ${var.service_cidr}",
    "service-node-port-range: ${var.service_node_port_range}",
  ], local.system_default_registry_lines)

  first_server_config = join("\n", concat(
    ["token: ${random_string.token.result}"],
    local.server_common,
    local.tls_san_lines[local.first_server.host],
    local.node_internal_ip_lines[local.first_server.host],
    local.external_ip_lines[local.first_server.host],
    local.advertise_lines[local.first_server.host],
  ))

  # A joining server is a full control-plane member: same settings, plus the address it joins through.
  server_join_config = { for host, s in local.join_servers : host => join("\n", concat(
    ["token: ${random_string.token.result}", "server: ${local.join_url}"],
    local.server_common,
    local.tls_san_lines[host],
    local.node_internal_ip_lines[host],
    local.external_ip_lines[host],
    local.advertise_lines[host],
  )) }

  # An agent inherits the cluster's networking from the server it joins, so it carries none of it --
  # but its own two addresses are its own, so both of those stay. No advertise-address: that is a
  # listener setting an agent does not know.
  agent_config = { for host, a in local.agent_hosts : host => join("\n", concat(
    ["token: ${random_string.token.result}", "server: ${local.join_url}"],
    local.system_default_registry_lines,
    local.node_internal_ip_lines[host],
    local.external_ip_lines[host],
  )) }

  # Run ON the joining node, before its install: RKE2's supervisor endpoint has to be answering or
  # the agent/server install comes up and then fails to register. The address is named on failure
  # because the usual cause is that it is not reachable from THIS node -- which is what
  # node_internal_ip is for on a host whose SSH address is a NAT address.
  # Reclaim the host: uninstall any pre-existing RKE2 so the install always lands on a clean node.
  # Installing over stale etcd/token data otherwise fails to start the service. Destructive by
  # design -- this module owns its target hosts.
  reclaim = <<-EOT
    set -e
    # The forced tar method cannot install over an rpm-method install, and the installer's own error
    # for that names no remedy. Refuse here instead, with the command to run.
    if command -v rpm >/dev/null 2>&1 && rpm -q rke2-common >/dev/null 2>&1; then
      echo "an rpm-method RKE2 install is present on this host; run rke2-uninstall.sh there first" >&2
      exit 1
    fi
    # Before the uninstall, which takes /etc/rancher/node with it. See local.node_password_save.
    ${local.node_password_save}
    ${local.uninstall_probe}
  EOT

  # The one case the release-keyed cache cannot rule out is an artifact an operator hand-placed in a
  # version-named directory. Without this, Terraform state would claim a Kubernetes version the
  # cluster does not run.
  version_assert = <<-EOT
    set -e
    rke2_bin=""
    for d in /usr/local/bin /opt/rke2/bin /usr/bin; do
      if [ -x "$d/rke2" ]; then rke2_bin="$d/rke2"; break; fi
    done
    [ -n "$rke2_bin" ] || { echo "no rke2 binary after install" >&2; exit 1; }
    installed=$(sudo "$rke2_bin" --version | head -1 | awk '{print $3}')
    if [ "$installed" != '${var.release}' ]; then
      # The hint names the cache directory only when there IS one. With the cache off,
      # cache_release_dir is just "/<release>" and would send the reader to a path that does not exist.
      echo "installed RKE2 is $installed but release is ${var.release}${var.image_archives_dir == "" ? "" : "; a hand-placed artifact in ${local.cache_release_dir} is the usual cause"}" >&2
      exit 1
    fi
    echo "installed RKE2 $installed"
  EOT

  join_wait = "timeout 300 bash -c 'until (exec 3<>/dev/tcp/${local.join_addr}/9345) 2>/dev/null; do sleep 3; done' || { echo 'cannot reach ${local.join_addr}:9345 from this node; set node_internal_ip for the first server to an address the other nodes can reach' >&2; exit 1; }"
}

# Shared join token so additional servers and agents authenticate against the cluster.
# random_string (not random_password) keeps install output visible in Terraform logs; the token
# still lands only in local, uncommitted state.
resource "random_string" "token" {
  length  = 48
  special = false
}

# Install RKE2 on the first server with an embedded etcd datastore (RKE2's default for a server
# started without a `server` URL). Connection parameters live in triggers so the destroy
# provisioner (which may only reference self) can reuse them.
resource "null_resource" "server_init" {
  # Depends on the snapshot so that it is written before any node is touched, and -- because
  # Terraform destroys dependents first -- removed only after every node is gone.
  depends_on = [null_resource.vars_snapshot]

  triggers = {
    host                    = local.first_server.host
    user                    = local.first_server.user
    port                    = var.server_ssh_port
    key_path                = pathexpand(var.ssh_private_key)
    bastion_host            = local.bastion_of[local.first_server.host]
    bastion_user            = local.bastion_user_of[local.first_server.host]
    bastion_port            = local.bastion_port_of[local.first_server.host]
    version                 = var.release
    cni                     = var.cni
    cluster_cidr            = var.cluster_cidr
    service_cidr            = var.service_cidr
    service_node_port_range = var.service_node_port_range
    # This node's cluster address goes into its config.yaml and its certificate, and is also what
    # every other node joins through, so changing it has to reinstall this server rather than wait.
    node_internal_ip = lookup(var.node_internal_ip, local.first_server.host, "")
    # The resolved flag, not the raw variable: half of the Calico fix is a manifest this provisioner
    # writes BEFORE the first start, so turning the fix on has to reinstall the server. Without this
    # a false -> true flip would apply only the per-node half and the switch would be one-way.
    calico_multi_nic_fix = local.calico_fix_enabled
    # Tracked so setting or changing the cache re-provisions this node now, rather than taking
    # effect at whatever later reinstall happens to come along.
    image_archives_dir = var.image_archives_dir
    # Same reason: where the cache is filled from feeds the cache steps, and the registry is
    # written into this node's config.yaml.
    mirror                  = var.mirror
    system_default_registry = var.system_default_registry
  }

  lifecycle {
    # Without the cache, the only CN-reachable install path would be the installer's own
    # INSTALL_RKE2_MIRROR parameter -- which this module deliberately never sets (see the mirror
    # variable), so the combination is refused rather than silently reaching github.com.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.rke2.io."
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

  # Uploaded ahead of the install so the cache steps below run in the same session that reclaimed
  # the node. Sent unconditionally (a provisioner cannot be count-gated); it is the install steps
  # that skip it when no cache is configured.
  provisioner "file" {
    source      = "${path.module}/scripts/image-archives.sh"
    destination = local.images_script
  }

  # Install and configure only, without waiting for the control plane to come up: the service is
  # started with --no-block and readiness is established by the kubeconfig fetch below, so the SSH
  # session is not held open across the RKE2 bring-up.
  provisioner "remote-exec" {
    inline = [
      # Reclaim the host: uninstall any pre-existing RKE2 so the install always lands on a clean
      # node. Installing over stale etcd/token data otherwise fails to start the service.
      # Destructive by design -- this module owns its target hosts.
      local.reclaim,
      local.route_flush,
      # After the reclaim (which removed the images directory) and before the installer, which is
      # what reads the artifact directory. A warm cache makes this entirely local.
      local.cache_prepare,
      local.install_cmd["server"],
      # The one case the release-keyed cache cannot rule out is an artifact an operator hand-placed
      # in a version-named directory. Without this, Terraform state would claim a Kubernetes version
      # the cluster does not run.
      local.version_assert,
      local.cache_finish,
      # RKE2 takes its configuration from a file rather than installer arguments, and the installer
      # does not start the service. Mode 600 because the file carries the join token.
      <<-EOT
        set -e
        sudo mkdir -p /etc/rancher/rke2
        # Created and restricted BEFORE the token is written into it, not chmod'ed afterwards: tee
        # keeps an existing file's mode, so there is no window at 0644 for anyone to read.
        sudo touch /etc/rancher/rke2/config.yaml
        sudo chmod 600 /etc/rancher/rke2/config.yaml
        printf '%s\n' '${local.first_server_config}' | sudo tee /etc/rancher/rke2/config.yaml > /dev/null
        # Put the node's own password back before the service starts; see local.node_password_save.
        ${local.node_password_restore}
      EOT
      ,
      # Before the first start, so Calico is deployed with the right tunnel address from the outset
      # rather than being repaired after a window in which cross-node traffic does not work.
      local.write_calico_helm_config,
      "sudo systemctl enable rke2-server",
      "sudo systemctl start --no-block rke2-server",
    ]
  }

  # Uninstall on destroy. Only self is referenceable here; on_failure=continue keeps re-destroys
  # idempotent when the script is already gone.
  #
  # Not a guarantee on its own: a creation provisioner that fails taints this resource, and
  # Terraform does not run destroy-time provisioners for a tainted resource -- so an apply that
  # dies mid-install followed by a destroy leaves RKE2 installed. The reclaim step above is what
  # converges that on the next apply; see the README for cleaning a host by hand.
  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      # Spelled out rather than taken from a local: a destroy-time provisioner may reference only
      # self, so the node-password save (see local.node_password_save), the uninstall probe (see
      # local.uninstall_probe) and the CIDR for the route flush all have to come from here.
      #
      # The save has to be HERE as well as in the reclaim, and this is the copy that matters for a
      # REPLACEMENT: Terraform destroys before it creates, so by the time the creation provisioner
      # runs its own save the uninstall below has already taken /etc/rancher/node with it. Measured:
      # without this line a replaced node comes back with a fresh password the surviving server
      # rejects, and it never rejoins.
      "if [ -f /etc/rancher/node/password ]; then sudo cp /etc/rancher/node/password /etc/rancher/node-password.saved && sudo chmod 600 /etc/rancher/node-password.saved; fi",
      "for d in /usr/local/bin /opt/rke2/bin /usr/bin; do if [ -x \"$d/rke2-uninstall.sh\" ]; then sudo \"$d/rke2-uninstall.sh\"; break; fi; done",
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Join any additional servers as control-plane members. Empty when a single server is given.
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
    # Re-run the install when the release, the CNI, or the join target changes, so every member
    # reacts together instead of only the first server.
    version                 = var.release
    server                  = local.join_url
    cni                     = var.cni
    cluster_cidr            = var.cluster_cidr
    service_cidr            = var.service_cidr
    service_node_port_range = var.service_node_port_range
    node_internal_ip        = lookup(var.node_internal_ip, each.value.host, "")
    # See server_init: the pre-start Calico manifest makes this a reinstall, not a re-reconcile.
    calico_multi_nic_fix = local.calico_fix_enabled
    image_archives_dir   = var.image_archives_dir
    # See server_init: mirror feeds the cache steps, and the registry lands in config.yaml.
    mirror                  = var.mirror
    system_default_registry = var.system_default_registry
    # The first server owns the datastore and the cluster CA, so a member that outlives a reinstall
    # of it holds credentials for a cluster that no longer exists. Reinstalling it therefore
    # reinstalls every other member too -- including the case a taint causes, where nothing else
    # about this node changed.
    server_init = null_resource.server_init.id
  }

  lifecycle {
    # See server_init.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.rke2.io."
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
    destination = local.images_script
  }

  provisioner "remote-exec" {
    inline = [
      local.reclaim,
      local.route_flush,
      local.cache_prepare,
      # Before the install, not after: a member that comes up without the supervisor answering
      # installs cleanly and then fails to register, which reads as a broken install rather than as
      # an unreachable join address.
      local.join_wait,
      local.install_cmd["server"],
      local.version_assert,
      local.cache_finish,
      <<-EOT
        set -e
        sudo mkdir -p /etc/rancher/rke2
        # See server_init: restricted before the token is written into it, not afterwards.
        sudo touch /etc/rancher/rke2/config.yaml
        sudo chmod 600 /etc/rancher/rke2/config.yaml
        printf '%s\n' '${local.server_join_config[each.value.host]}' | sudo tee /etc/rancher/rke2/config.yaml > /dev/null
        # Put the node's own password back before the service starts; see local.node_password_save.
        ${local.node_password_restore}
      EOT
      ,
      # Before the first start, so Calico is deployed with the right tunnel address from the outset
      # rather than being repaired after a window in which cross-node traffic does not work.
      local.write_calico_helm_config,
      "sudo systemctl enable rke2-server",
      "sudo systemctl start --no-block rke2-server",
    ]
  }

  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      # Spelled out rather than taken from a local: a destroy-time provisioner may reference only self.
      # The save must precede the uninstall here too; see server_init.
      "if [ -f /etc/rancher/node/password ]; then sudo cp /etc/rancher/node/password /etc/rancher/node-password.saved && sudo chmod 600 /etc/rancher/node-password.saved; fi",
      "for d in /usr/local/bin /opt/rke2/bin /usr/bin; do if [ -x \"$d/rke2-uninstall.sh\" ]; then sudo \"$d/rke2-uninstall.sh\"; break; fi; done",
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Join agents as workers. Empty when no agents are given, in which case the servers run workloads
# themselves (RKE2 servers are schedulable when untainted).
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
    # Re-run the install when the release or the join target changes, so agents track the servers
    # instead of only the first server being reinstalled.
    version = var.release
    server  = local.join_url
    # The agent takes its images from the CNI extra in the cache, so a change of CNI has to
    # re-provision it too; without this only the servers reacted and the agents kept the old set.
    cni              = var.cni
    node_internal_ip = lookup(var.node_internal_ip, each.value.host, "")
    # Tracked so this agent re-provisions when the pod network changes, and so the destroy-time
    # route flush can read the CIDR off self.triggers.
    cluster_cidr       = var.cluster_cidr
    image_archives_dir = var.image_archives_dir
    # See server_init: mirror feeds the cache steps, and the registry lands in this node's
    # config.yaml (an Agent/Runtime setting, valid on agents too).
    mirror                  = var.mirror
    system_default_registry = var.system_default_registry
    # See server_join: an agent that outlives a reinstall of the first server holds credentials for
    # a cluster that no longer exists.
    server_init = null_resource.server_init.id
  }

  lifecycle {
    # See server_init.
    precondition {
      condition     = var.mirror != "cn" || var.image_archives_dir != ""
      error_message = "mirror = \"cn\" requires image_archives_dir: without the artifact cache the install would still download from github.com and get.rke2.io."
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
    destination = local.images_script
  }

  provisioner "remote-exec" {
    inline = [
      local.reclaim,
      local.route_flush,
      local.cache_prepare,
      local.join_wait,
      local.install_cmd["agent"],
      local.version_assert,
      local.cache_finish,
      # An agent inherits the cluster's networking from the server, so its config carries only the
      # token, the join address, and its own cluster address.
      <<-EOT
        set -e
        sudo mkdir -p /etc/rancher/rke2
        # See server_init: restricted before the token is written into it, not afterwards.
        sudo touch /etc/rancher/rke2/config.yaml
        sudo chmod 600 /etc/rancher/rke2/config.yaml
        printf '%s\n' '${local.agent_config[each.value.host]}' | sudo tee /etc/rancher/rke2/config.yaml > /dev/null
        # Put the node's own password back before the service starts; see local.node_password_save.
        ${local.node_password_restore}
      EOT
      ,
      "sudo systemctl enable rke2-agent",
      "sudo systemctl start --no-block rke2-agent",
    ]
  }

  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      # The save must precede the uninstall here too; see server_init.
      "if [ -f /etc/rancher/node/password ]; then sudo cp /etc/rancher/node/password /etc/rancher/node-password.saved && sudo chmod 600 /etc/rancher/node-password.saved; fi",
      "for d in /usr/local/bin /opt/rke2/bin /usr/bin; do if [ -x \"$d/rke2-uninstall.sh\" ]; then sudo \"$d/rke2-uninstall.sh\"; break; fi; done",
      "for c in $(echo '${self.triggers.cluster_cidr}' | tr ',' ' '); do sudo ip route flush root \"$c\" || true; done",
    ]
  }
}

# Fetch the kubeconfig via sudo (RKE2 writes it root-only), namespace its "default" identifiers to
# a per-cluster context, repoint the server URL from 127.0.0.1 to the reachable host, and
# flatten-merge it into ~/.kube/config. Retries because rke2.yaml appears only once the control
# plane has started -- which, with --no-block above, is where the wait for it happens. The sed rules
# are anchored to whole identity lines so the base64 certificate blobs stay intact.
resource "null_resource" "kubeconfig" {
  depends_on = [null_resource.server_init, null_resource.server_join, null_resource.agent]

  triggers = {
    host    = local.first_server.host
    context = local.context_name
    # Re-fetch when the first server is reinstalled (new certificates), so ~/.kube/config never
    # keeps stale credentials.
    server_init = null_resource.server_init.id
    # Tracked so flipping the flag re-runs the merge, instead of taking effect only the next time
    # the server happens to be reinstalled.
    switch_kube_context = var.switch_kube_context
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      raw="$(mktemp)"
      merged=""
      trap 'rm -f "$raw" "$merged"' EXIT
      # Read before the merge: the merged view takes its current-context from the new file (first in
      # KUBECONFIG below), so keeping the current context means putting this one back afterwards.
      # Empty when there is no ~/.kube/config yet.
      previous="$(KUBECONFIG="$HOME/.kube/config" kubectl config current-context 2>/dev/null || true)"
      ssh_opts=(-i '${pathexpand(var.ssh_private_key)}' -p ${var.server_ssh_port} -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10)
      ${local.ssh_proxy_of[local.first_server.host]}
      for i in $(seq 1 60); do
        if ssh "$${ssh_opts[@]}" '${local.first_server.user}@${local.first_server.host}' \
             'sudo test -s /etc/rancher/rke2/rke2.yaml && sudo cat /etc/rancher/rke2/rke2.yaml' 2>/dev/null \
             > "$raw" && test -s "$raw"; then
          sed -E \
            -e 's|https://127\.0\.0\.1:[0-9]+|https://${local.kubeconfig_host}:${local.apiserver_port}|' \
            -e 's|^  name: default$|  name: ${local.context_name}|' \
            -e 's|^    cluster: default$|    cluster: ${local.context_name}|' \
            -e 's|^    user: default$|    user: ${local.context_name}|' \
            -e 's|^- name: default$|- name: ${local.context_name}|' \
            -e 's|^current-context: default$|current-context: ${local.context_name}|' \
            "$raw" > '${local.kubeconfig_path}'
          chmod 600 '${local.kubeconfig_path}'
          mkdir -p "$HOME/.kube"
          # Unique temp in ~/.kube so concurrent/interrupted applies can't clobber a shared file,
          # and the mv stays an atomic same-filesystem rename.
          merged="$(mktemp "$HOME/.kube/config.XXXXXX")"
          # New file first: its entries win on conflict, so a re-apply refreshes this cluster and
          # makes it the current context unless var.switch_kube_context puts the previous one back.
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
          # rke2.yaml is written before the apiserver starts serving, so having the file is not
          # having a cluster: for the first minute or so a request to it is refused mid-handshake.
          # Wait for it here, once, rather than leaving every caller to discover it -- an e2e script
          # that runs kubectl straight after a successful apply otherwise reads a transport failure
          # as a verdict about the cluster.
          for j in $(seq 1 60); do
            if [ "$(KUBECONFIG='${local.kubeconfig_path}' kubectl get --raw=/readyz --request-timeout=10s 2>/dev/null)" = ok ]; then
              echo "apiserver on ${local.first_server.host} is ready"
              break
            fi
            echo "waiting for the apiserver on ${local.first_server.host} ($j/60)"
            sleep 5
          done
          if [ "$(KUBECONFIG='${local.kubeconfig_path}' kubectl get --raw=/readyz --request-timeout=10s 2>/dev/null)" != ok ]; then
            echo "timed out waiting for the apiserver on ${local.first_server.host} to answer /readyz" >&2
            exit 1
          fi
          # Every node this module installed has to be IN the cluster before the apply reports
          # success. The install steps start the service with --no-block and return, so a node whose
          # config.yaml the service rejects, or that cannot reach the supervisor endpoint, leaves the
          # step green. This resource depends on every install, so it is where the count is known.
          # Registration, not Ready: a node is registered as soon as its kubelet reaches the
          # apiserver, whereas Ready also waits for the CNI, which a later step is what fixes.
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
        echo "waiting for rke2.yaml on ${local.first_server.host} ($i/60)"
        sleep 5
      done
      echo "timed out waiting for rke2.yaml" >&2
      exit 1
    EOT
  }

  # Remove this cluster's context, cluster, and user from ~/.kube/config on destroy. Only self is
  # referenceable here; on_failure=continue keeps re-destroys idempotent when the entries are gone.
  #
  # Anchored to the same file the merge above writes, rather than to whatever kubectl would resolve:
  # with a KUBECONFIG set, cleanup would otherwise strip the entries from that file and leave these
  # behind in ~/.kube/config forever.
  #
  # Deleting the context does NOT clear current-context when it names that context -- kubectl leaves
  # the field pointing at a context that no longer exists -- and that dangling value is exactly what
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

# Give each node's RKE2 containerd the same custom runtimes (e.g. ascend) as its Docker daemon.json,
# dropped for nvidia (RKE2 auto-detects and wires that one itself), and register a RuntimeClass for each
# so a workload can select it. daemon.json is fetched over SSH to the local machine and parsed with local
# jq (scripts/render-containerd-runtimes.sh), since the remote host is not guaranteed to have jq.
# Re-derives whenever the node is (re)installed; an empty result removes any previously-written templates
# and restarts only on change.
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
      dir=/var/lib/rancher/rke2/agent/etc/containerd
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
      node='${each.value.user}@${each.key}'

      # The inner `|| true` makes a MISSING daemon.json an empty answer, while an ssh that cannot reach
      # the node still fails here. The difference matters: "no runtimes" removes the templates and
      # restarts the service, so taking an unreachable node for an empty one would tear down a working
      # node's runtimes on a transient network fault.
      if ! daemon_json="$(ssh "$${ssh_opts[@]}" "$node" 'sudo cat /etc/docker/daemon.json 2>/dev/null || true')"; then
        echo "cannot reach ${each.key} to read /etc/docker/daemon.json" >&2
        exit 1
      fi
      # Two renderings of the same runtimes: containerd's CRI plugin is named
      # io.containerd.grpc.v1.cri in config version 2 but io.containerd.cri.v1.runtime in version 3, and
      # containerd ignores a runtime declared under the other version's path without a word. One blob
      # written to both templates therefore leaves whichever containerd the node actually runs with no
      # such runtime at all, and every Pod selecting it fails to get a sandbox.
      rendered="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 2 || true)"
      rendered_v3="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 3 || true)"
      existing="$(ssh "$${ssh_opts[@]}" "$node" "sudo cat $dir/config.toml.tmpl 2>/dev/null" || true)"
      existing_v3="$(ssh "$${ssh_opts[@]}" "$node" "sudo cat $dir/config-v3.toml.tmpl 2>/dev/null" || true)"

      # A handler in containerd is only half of it: a Pod picks a runtime through a RuntimeClass naming
      # that handler, and a vendor whose device nodes reach the container only through its runtime hook
      # gets none at all without one. The names are read back out of the rendered stanzas, so the classes
      # can never name a runtime containerd was not given. Applied on every run rather than only on a
      # change, so a deleted class comes back, and before the restart below, so the apiserver is still up.
      # Every class this module creates is labelled: it is how a class belonging to a vendor's own
      # operator is left untouched rather than adopted, and how a class whose handler this module has
      # since stopped rendering can be pruned without guessing at ownership.
      owner_label='gpustack.ai/managed-by=testing-infra-rke2'
      wanted=$(printf '%s\n' "$rendered_v3" | sed -n 's/^\[plugins\."[^"]*"\.containerd\.runtimes\.\([^.]*\)\]$/\1/p')

      for name in $wanted; do
        # A read that fails is not an answer. Treating a transient failure as "absent" would adopt --
        # and later prune -- a class another component owns, so it is retried and only a real answer
        # decides.
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
            echo "cannot read RuntimeClass $name on ${each.key}" >&2
            exit 1
          fi
          sleep 5
        done
        if [ -n "$foreign" ] && [ "$foreign" != 'testing-infra-rke2' ]; then
          echo "RuntimeClass $name belongs to $foreign; left as found"
          continue
        fi
        for i in $(seq 1 12); do
          printf 'apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: %s\n  labels:\n    %s: %s\nhandler: %s\n' \
            "$name" 'gpustack.ai/managed-by' 'testing-infra-rke2' "$name" \
            | KUBECONFIG='${local.kubeconfig_path}' kubectl apply -f - && break
          if [ "$i" = 12 ]; then
            echo "failed to register RuntimeClass $name for ${each.key}" >&2
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
        echo "could not list this module's RuntimeClasses on ${each.key}; none pruned" >&2
      fi

      if [ "$rendered" = "$existing" ] && [ "$rendered_v3" = "$existing_v3" ]; then
        echo "containerd runtime templates on ${each.key} unchanged"
        exit 0
      fi

      # Every write below is checked. Each branch ends in an echo, so an unchecked failure would exit
      # 0 having reported a change that did not happen.
      if [ -z "$rendered" ]; then
        ssh "$${ssh_opts[@]}" "$node" \
          "sudo rm -f $dir/config.toml.tmpl $dir/config-v3.toml.tmpl && sudo systemctl restart ${each.value.service}" ||
          die "cannot remove the containerd runtime templates on ${each.key}"
        echo "removed stale containerd runtime template on ${each.key}"
        exit 0
      fi

      ssh "$${ssh_opts[@]}" "$node" "sudo mkdir -p $dir" || die "cannot create $dir on ${each.key}"
      printf '%s\n' "$rendered" | ssh "$${ssh_opts[@]}" "$node" "sudo tee $dir/config.toml.tmpl > /dev/null" ||
        die "cannot write $dir/config.toml.tmpl on ${each.key}"
      printf '%s\n' "$rendered_v3" | ssh "$${ssh_opts[@]}" "$node" "sudo tee $dir/config-v3.toml.tmpl > /dev/null" ||
        die "cannot write $dir/config-v3.toml.tmpl on ${each.key}"
      ssh "$${ssh_opts[@]}" "$node" "sudo systemctl restart ${each.value.service}" ||
        die "cannot restart ${each.value.service} on ${each.key}"
      echo "wrote containerd runtime templates on ${each.key} and restarted ${each.value.service}"
    EOT
  }
}

# Apply the Calico multi-NIC fix once the control plane answers. A SEPARATE resource, not a step in
# the server's provisioner chain, for three reasons this hardware demonstrates:
#
#   - Node identity comes from the cluster, not from a guess. A FelixConfiguration is matched by
#     exact node name, and RKE2's node name is only by default the hostname.
#   - No CRD race. A manifest dropped into server/manifests/ before the first start is applied
#     before Calico's CRDs exist, so it depends on the deploy controller retrying.
#   - Adding a node works, and turning the fix off cleans up. A creation provisioner on the first
#     server does not re-run when an agent is added, and forcing it to would REPLACE the
#     control-plane install just to regenerate YAML. Removing a manifest file, meanwhile, does not
#     delete the objects it created -- so that shape has no disable path at all.
#
# The trade accepted: the objects land seconds after the control plane starts rather than before it,
# so CoreDNS may be briefly not-Ready on a first apply. The kubelet retries its probes, so the same
# apply converges -- and being late is worth being keyed on the truth rather than on a guess.
resource "null_resource" "calico_multi_nic" {
  depends_on = [null_resource.kubeconfig, null_resource.containerd_config]

  triggers = {
    enabled = local.calico_fix_enabled ? "yes" : "no"
    # A hash of the whole topology, so adding or re-addressing a node re-runs ONLY this resource.
    topology = sha256(jsonencode({
      servers          = [for s in local.servers : s.host]
      agents           = [for a in local.agents : a.host]
      node_internal_ip = var.node_internal_ip
    }))
    # And a reinstall of any node re-runs it too: a fresh Calico loses the objects with the cluster.
    installs = sha256(jsonencode(concat(
      [null_resource.server_init.id],
      [for r in null_resource.server_join : r.id],
      [for r in null_resource.agent : r.id],
    )))
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command = join(" ", [
      "bash '${path.module}/scripts/calico-multi-nic-fix.sh'",
      "--kubeconfig '${local.kubeconfig_path}'",
      "--enabled ${local.calico_fix_enabled ? "yes" : "no"}",
      # The module knows how many nodes it installed; the cluster is what has to catch up.
      "--expect-nodes ${local.node_count}",
    ])
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
