locals {
  target_user = strcontains(var.host, "@") ? split("@", var.host)[0] : var.ssh_user
  target_host = strcontains(var.host, "@") ? split("@", var.host)[1] : var.host

  jumper_host = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[1] : var.ssh_jumper)
  jumper_user = var.ssh_jumper == "" ? "" : (strcontains(var.ssh_jumper, "@") ? split("@", var.ssh_jumper)[0] : var.ssh_user)

  bastion_host = local.target_host == local.jumper_host ? "" : local.jumper_host
  bastion_user = local.bastion_host == "" ? "" : local.jumper_user
  bastion_port = local.bastion_host == "" ? "22" : tostring(var.ssh_jumper_port)

  ssh_proxy = local.bastion_host == "" ? ":" : "ssh_opts+=(-o \"ProxyCommand=ssh -i ${pathexpand(var.ssh_private_key)} -p ${var.ssh_jumper_port} -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -W %h:%p ${local.jumper_user}@${local.jumper_host}\")"

  jumper_san = local.jumper_host == "" || local.jumper_host == local.target_host ? "" : local.jumper_host

  api_server_is_loopback = can(regex("^127\\.", var.api_server_address)) || (
    can(cidrhost("${var.api_server_address}/128", 0)) && cidrhost("${var.api_server_address}/128", 0) == "::1"
  )
  target_is_loopback = local.target_host == "localhost" || can(regex("^127\\.", local.target_host)) || (
    can(cidrhost("${local.target_host}/128", 0)) && cidrhost("${local.target_host}/128", 0) == "::1"
  )
  api_server_san = contains(["0.0.0.0", "::"], var.api_server_address) || local.api_server_is_loopback ? "" : var.api_server_address

  # Include the SSH target host, loopback names, optional jump host, explicit bind IP, and caller
  # extras in the apiserver certificate so kubectl works directly from the workstation or via tunnel.
  cert_sans = distinct(compact(concat(
    [local.target_host, "127.0.0.1", "localhost", local.jumper_san, local.api_server_san],
    var.extra_cert_sans,
  )))

  # Resolve the node image from explicit override or <repository>:<release>.
  effective_node_image = var.node_image != "" ? var.node_image : "${var.node_image_repository}:${var.release}"

  # Expected Kubernetes server gitVersion (digest stripped) asserted after apply unless overridden.
  expected_k8s_version = var.node_image != "" ? "" : split("@", var.release)[0]

  node_count = var.control_planes + var.workers

  kubeconfig_host = strcontains(local.target_host, ":") ? "[${local.target_host}]" : local.target_host
  context_name    = "kind-${var.cluster_name}-${replace(local.target_host, "/[^a-zA-Z0-9]/", "-")}"
  kubeconfig_path = "${path.module}/kubeconfig"

  remote_config_path = "$HOME/.gpustack-kind-${var.cluster_name}.yaml"

  # Strictly nested region -> zone -> rack labels for every worker node so Kueue TAS builds a
  # valid topology tree out of the box. Use topology.gpustack.ai/rack rather than
  # topology.kubernetes.io/rack because kubelet's NodeRestriction allowlist rejects unknown
  # *.kubernetes.io/* keys at node join time.
  worker_labels = [
    for idx in range(var.workers) : var.topology_region == "" ? {} : {
      "topology.kubernetes.io/region" = var.topology_region
      "topology.kubernetes.io/zone"   = "zone-${(idx % var.topology_zones) + 1}"
      "topology.gpustack.ai/rack"     = "rack-${((idx % var.topology_zones) * var.topology_racks_per_zone) + (floor(idx / var.topology_zones) % var.topology_racks_per_zone) + 1}"
    }
  ]

  rendered_extra_mounts = [
    for m in var.extra_mounts : {
      hostPath      = m.host_path
      containerPath = m.container_path
      readOnly      = m.read_only
    }
  ]

  control_plane_nodes = [
    for i in range(var.control_planes) : merge(
      {
        role  = "control-plane"
        image = local.effective_node_image
        kubeadmConfigPatches = concat(
          [
            yamlencode({
              kind = "ClusterConfiguration"
              apiServer = {
                certSANs = local.cert_sans
                extraArgs = {
                  "enable-aggregator-routing" = "true"
                }
              }
            }),
          ],
          length(var.kubelet_extra_args) > 0 ? [
            yamlencode({
              kind = i == 0 ? "InitConfiguration" : "JoinConfiguration"
              nodeRegistration = {
                kubeletExtraArgs = var.kubelet_extra_args
              }
            }),
          ] : [],
        )
      },
      var.workers == 0 && length(local.rendered_extra_mounts) > 0 ? {
        extraMounts = local.rendered_extra_mounts
      } : {},
    )
  ]

  worker_nodes = [
    for i in range(var.workers) : merge(
      {
        role  = "worker"
        image = local.effective_node_image
      },
      length(local.worker_labels[i]) > 0 ? {
        labels = local.worker_labels[i]
      } : {},
      length(var.kubelet_extra_args) > 0 ? {
        kubeadmConfigPatches = [
          yamlencode({
            kind = "JoinConfiguration"
            nodeRegistration = {
              kubeletExtraArgs = var.kubelet_extra_args
            }
          }),
        ]
      } : {},
      length(local.rendered_extra_mounts) > 0 ? {
        extraMounts = local.rendered_extra_mounts
      } : {},
    )
  ]

  kind_cluster_config = yamlencode({
    kind       = "Cluster"
    apiVersion = "kind.x-k8s.io/v1alpha4"
    name       = var.cluster_name
    networking = {
      apiServerAddress  = var.api_server_address
      apiServerPort     = var.api_server_port
      podSubnet         = var.cluster_cidr
      serviceSubnet     = var.service_cidr
      disableDefaultCNI = var.disable_default_cni
    }
    nodes = concat(local.control_plane_nodes, local.worker_nodes)
  })

  # Same verified CN docker.io proxies as testing/infra/clusters/k3s when mirror = "cn".
  cn_docker_io_mirrors = [
    "https://docker.1panel.live",
    "https://image.jianmuhub.com",
    "https://dockerproxy.net",
    "https://proxy.vvvv.ee",
  ]

  registry_mirrors = var.registry_mirrors != null ? var.registry_mirrors : (
    var.mirror == "cn" ? { "docker.io" = local.cn_docker_io_mirrors } : {}
  )

  # Pre-render hosts.toml content per mirrored registry so containerd inside every Kind node
  # resolves workload pulls through the configured mirrors without restarting containerd.
  hosts_toml_by_registry = {
    for reg, eps in local.registry_mirrors : reg => join("\n", concat(
      ["server = \"https://${reg == "docker.io" ? "registry-1.docker.io" : reg}\""],
      [""],
      flatten([
        for ep in eps : [
          "[host.\"${ep}\"]",
          "  capabilities = [\"pull\", \"resolve\"]",
          "",
        ]
      ]),
    ))
  }
}

# Provision the multi-node Kind cluster on the remote Docker host over SSH.
# Connection parameters and cluster identity live in triggers so the destroy provisioner
# (which may only reference self) can delete the cluster cleanly.
resource "null_resource" "kind_cluster" {
  depends_on = [null_resource.vars_snapshot]

  lifecycle {
    precondition {
      condition     = !local.api_server_is_loopback || local.target_is_loopback
      error_message = "api_server_address '${var.api_server_address}' binds loopback only, which cannot be reached from this workstation against remote host '${local.target_host}' (this module opens no SSH tunnel); use '0.0.0.0' or a reachable interface IP."
    }
  }

  triggers = merge({
    host                       = local.target_host
    user                       = local.target_user
    port                       = var.ssh_port
    key_path                   = pathexpand(var.ssh_private_key)
    bastion_host               = local.bastion_host
    bastion_user               = local.bastion_user
    bastion_port               = local.bastion_port
    cluster_name               = var.cluster_name
    kind_version               = var.kind_version
    node_image                 = local.effective_node_image
    expected_k8s_version       = local.expected_k8s_version
    control_planes             = var.control_planes
    workers                    = var.workers
    api_server_address         = var.api_server_address
    api_server_port            = var.api_server_port
    config_sha256              = sha256(local.kind_cluster_config)
    remote_config_path         = local.remote_config_path
    binary_cache_dir           = var.binary_cache_dir
    inotify_max_user_instances = var.inotify_max_user_instances
    inotify_max_user_watches   = var.inotify_max_user_watches
    },
    length(local.registry_mirrors) == 0 ? {} : { registry_mirrors = jsonencode(local.registry_mirrors) },
    length(var.preload_images) == 0 ? {} : { preload_images = jsonencode(var.preload_images) },
  )

  connection {
    type                = "ssh"
    host                = self.triggers.host
    user                = self.triggers.user
    port                = tonumber(self.triggers.port)
    private_key         = file(self.triggers.key_path)
    bastion_host        = lookup(self.triggers, "bastion_host", "")
    bastion_user        = lookup(self.triggers, "bastion_user", "")
    bastion_port        = tonumber(lookup(self.triggers, "bastion_port", "22"))
    bastion_private_key = file(self.triggers.key_path)
  }

  provisioner "remote-exec" {
    inline = [
      <<-EOT
        set -eu

        # Determine whether docker requires sudo for this SSH user (POSIX sh for Ubuntu dash).
        if docker info >/dev/null 2>&1; then
          DOCKER_CMD="docker"
          KIND_PREFIX=""
        elif sudo -n docker info >/dev/null 2>&1; then
          DOCKER_CMD="sudo -n docker"
          KIND_PREFIX="sudo -n"
        elif ! sudo -n true >/dev/null 2>&1; then
          echo "docker on $(hostname) is not accessible without sudo and passwordless sudo is unavailable" >&2
          exit 1
        else
          echo "docker daemon is not reachable on $(hostname)" >&2
          exit 1
        fi

        # Raise host inotify sysctls if below target so multi-node Kind containers do not exhaust
        # file descriptors (never lowers an already-higher host setting).
        cur_inst="$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)"
        if [ "$${cur_inst:-0}" -lt ${var.inotify_max_user_instances} ]; then
          sudo sysctl -w fs.inotify.max_user_instances=${var.inotify_max_user_instances} >/dev/null
        fi
        cur_watch="$(sysctl -n fs.inotify.max_user_watches 2>/dev/null || echo 0)"
        if [ "$${cur_watch:-0}" -lt ${var.inotify_max_user_watches} ]; then
          sudo sysctl -w fs.inotify.max_user_watches=${var.inotify_max_user_watches} >/dev/null
        fi

        # Install or verify pinned kind CLI binary (cached under binary_cache_dir when set).
        want_kind='${var.kind_version}'
        need_kind=1
        if [ -x /usr/local/bin/kind ]; then
          got_kind="$(/usr/local/bin/kind version 2>/dev/null | awk '{print $2}' || true)"
          if [ "$got_kind" = "$want_kind" ]; then
            need_kind=0
          fi
        fi

        if [ "$need_kind" -eq 1 ]; then
          case "$(uname -m)" in
            x86_64|amd64) arch="amd64" ;;
            aarch64|arm64) arch="arm64" ;;
            *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
          esac
          cache_dir='${var.binary_cache_dir}'
          if [ -n "$cache_dir" ]; then
            ver_dir="$cache_dir/$want_kind"
            cached_bin="$ver_dir/kind"
            sudo mkdir -p "$ver_dir"
            if [ ! -x "$cached_bin" ]; then
              url="https://github.com/kubernetes-sigs/kind/releases/download/$want_kind/kind-linux-$arch"
              sha_url="$url.sha256sum"
              tmp_bin="$ver_dir/partial.kind.$$"
              tmp_sha="$ver_dir/partial.sha.$$"
              sudo curl -fsSL "$url" -o "$tmp_bin"
              sudo curl -fsSL "$sha_url" -o "$tmp_sha"
              want_sum="$(awk '{print $1}' "$tmp_sha")"
              got_sum="$(sha256sum "$tmp_bin" | awk '{print $1}')"
              if [ -z "$want_sum" ] || [ "$want_sum" != "$got_sum" ]; then
                sudo rm -f "$tmp_bin" "$tmp_sha"
                echo "sha256 mismatch downloading kind $want_kind ($got_sum != $want_sum)" >&2
                exit 1
              fi
              sudo chmod 0755 "$tmp_bin"
              sudo mv "$tmp_bin" "$cached_bin"
              sudo rm -f "$tmp_sha"
            fi
            sudo install -m 0755 "$cached_bin" /usr/local/bin/kind
          else
            url="https://github.com/kubernetes-sigs/kind/releases/download/$want_kind/kind-linux-$arch"
            tmp_bin="$(mktemp)"
            curl -fsSL "$url" -o "$tmp_bin"
            sudo install -m 0755 "$tmp_bin" /usr/local/bin/kind
            rm -f "$tmp_bin"
          fi
        fi

        got_kind="$(/usr/local/bin/kind version 2>/dev/null | awk '{print $2}' || true)"
        if [ "$got_kind" != "$want_kind" ]; then
          echo "installed kind is '$got_kind' but kind_version is '$want_kind'" >&2
          exit 1
        fi
        echo "using kind $got_kind"

        # Reclaim any existing Kind cluster of the same name so apply always starts clean.
        $KIND_PREFIX /usr/local/bin/kind delete cluster --name '${var.cluster_name}' >/dev/null 2>&1 || true

        # Write rendered Kind cluster config to the SSH user's HOME (never world-writable /tmp).
        echo '${base64encode(local.kind_cluster_config)}' | base64 -d > ${local.remote_config_path}
        chmod 600 ${local.remote_config_path}

        # Create the multi-node Kind cluster with the requested Kubernetes node image.
        $KIND_PREFIX /usr/local/bin/kind create cluster \
          --name '${var.cluster_name}' \
          --config ${local.remote_config_path} \
          --wait 5m

        # Configure containerd registry mirrors inside every Kind node container when requested.
        nodes="$($KIND_PREFIX /usr/local/bin/kind get nodes --name '${var.cluster_name}')"
        %{for reg, toml in local.hosts_toml_by_registry}
        for n in $nodes; do
          $DOCKER_CMD exec "$n" mkdir -p '/etc/containerd/certs.d/${reg}'
          echo '${base64encode(toml)}' | base64 -d | $DOCKER_CMD exec -i "$n" tee '/etc/containerd/certs.d/${reg}/hosts.toml' >/dev/null
        done
        %{endfor}

        # Preload any requested Docker images from the remote host daemon into every Kind node.
        %{for img in var.preload_images}
        $KIND_PREFIX /usr/local/bin/kind load docker-image '${img}' --name '${var.cluster_name}'
        %{endfor}

        # Ensure control-plane nodes remain schedulable when workers = 0 (Kind already untaints
        # single-node clusters; multi-control-plane with 0 workers needs explicit untaint).
        if [ '${var.workers}' = '0' ] && [ '${var.control_planes}' -gt 1 ]; then
          for n in $nodes; do
            $DOCKER_CMD exec "$n" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
              taint nodes "$n" node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true
          done
        fi

        echo "kind cluster '${var.cluster_name}' created with $(printf '%s\n' "$nodes" | grep -c .) node(s)"
      EOT
    ]
  }

  provisioner "remote-exec" {
    when       = destroy
    on_failure = continue

    inline = [
      <<-EOT
        if [ -x /usr/local/bin/kind ]; then
          if docker info >/dev/null 2>&1; then
            /usr/local/bin/kind delete cluster --name '${self.triggers.cluster_name}' || true
          else
            sudo /usr/local/bin/kind delete cluster --name '${self.triggers.cluster_name}' || true
          fi
        fi
        rm -f ${self.triggers.remote_config_path} || true
      EOT
    ]
  }
}

# Fetch the kubeconfig from the remote Kind cluster, rewrite its server URL to the remote host,
# rename its context/cluster/user to a unique per-cluster context, merge into ~/.kube/config,
# and verify all nodes register Ready and run the requested Kubernetes version.
resource "null_resource" "kubeconfig" {
  depends_on = [null_resource.kind_cluster]

  triggers = {
    host                 = local.target_host
    context              = local.context_name
    cluster_name         = var.cluster_name
    api_server_port      = var.api_server_port
    kind_cluster         = null_resource.kind_cluster.id
    switch_kube_context  = var.switch_kube_context
    expected_k8s_version = local.expected_k8s_version
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      raw="$(mktemp)"
      merged=""
      trap 'rm -f "$raw" "$merged"' EXIT

      previous="$(KUBECONFIG="$HOME/.kube/config" kubectl config current-context 2>/dev/null || true)"
      ssh_opts=(-i '${pathexpand(var.ssh_private_key)}' -p ${var.ssh_port} -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10)
      ${local.ssh_proxy}

      # Probe Docker/sudo access once so missing passwordless sudo fails fast instead of timing out.
      docker_mode="$(ssh "$${ssh_opts[@]}" '${local.target_user}@${local.target_host}' \
        "if docker info >/dev/null 2>&1; then echo direct; elif sudo -n docker info >/dev/null 2>&1; then echo sudo; elif ! sudo -n true >/dev/null 2>&1; then echo need-nopasswd-sudo; else echo no-docker; fi" \
        2>/dev/null || echo ssh-failed)"
      case "$docker_mode" in
        direct) remote_kind="/usr/local/bin/kind" ;;
        sudo) remote_kind="sudo -n /usr/local/bin/kind" ;;
        need-nopasswd-sudo)
          echo "docker on ${local.target_host} requires passwordless sudo (or docker group membership) to fetch Kind kubeconfig" >&2
          exit 1
          ;;
        no-docker)
          echo "docker daemon is not reachable on ${local.target_host}" >&2
          exit 1
          ;;
        *)
          echo "failed to probe Docker access over SSH on ${local.target_host}" >&2
          exit 1
          ;;
      esac

      kind_ctx="kind-${var.cluster_name}"
      for i in $(seq 1 20); do
        if ssh "$${ssh_opts[@]}" '${local.target_user}@${local.target_host}' \
             "$remote_kind get kubeconfig --name '${var.cluster_name}'" \
             2>/dev/null > "$raw" && test -s "$raw"; then
          sed -E \
            -e "s|https://[^:]+:[0-9]+|https://${local.kubeconfig_host}:${var.api_server_port}|" \
            -e "s|^  name: $${kind_ctx}$|  name: ${local.context_name}|" \
            -e "s|^    cluster: $${kind_ctx}$|    cluster: ${local.context_name}|" \
            -e "s|^    user: $${kind_ctx}$|    user: ${local.context_name}|" \
            -e "s|^- name: $${kind_ctx}$|- name: ${local.context_name}|" \
            -e "s|^current-context: $${kind_ctx}$|current-context: ${local.context_name}|" \
            "$raw" > '${local.kubeconfig_path}'
          chmod 600 '${local.kubeconfig_path}'

          mkdir -p "$HOME/.kube"
          merged="$(mktemp "$HOME/.kube/config.XXXXXX")"
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

          # Wait for all Kind nodes to be Ready from this workstation.
          for j in $(seq 1 60); do
            ready="$(KUBECONFIG='${local.kubeconfig_path}' kubectl get nodes -o json --request-timeout=10s 2>/dev/null \
              | jq -r '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length' 2>/dev/null || echo 0)"
            if [ "$${ready:-0}" -ge ${local.node_count} ]; then
              echo "all ${local.node_count} Kind node(s) are Ready"
              break
            fi
            echo "waiting for all ${local.node_count} Kind node(s) to be Ready ($ready so far, $j/60)"
            sleep 5
            if [ "$j" -eq 60 ]; then
              echo "timed out with $ready of ${local.node_count} node(s) Ready" >&2
              exit 1
            fi
          done

          # Assert the running Kubernetes server version matches var.release when not overridden.
          want_ver='${local.expected_k8s_version}'
          if [ -n "$want_ver" ]; then
            got_ver="$(KUBECONFIG='${local.kubeconfig_path}' kubectl version -o json --request-timeout=10s 2>/dev/null \
              | jq -r '.serverVersion.gitVersion // ""')"
            if [ "$got_ver" != "$want_ver" ]; then
              echo "cluster Kubernetes version is '$got_ver' but release is '$want_ver'" >&2
              exit 1
            fi
            echo "verified cluster Kubernetes version $got_ver"
          fi
          exit 0
        fi
        echo "waiting for Kind kubeconfig on ${local.target_host} ($i/20)"
        sleep 3
      done
      echo "timed out waiting for Kind kubeconfig on ${local.target_host}" >&2
      exit 1
    EOT
  }

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
      rm -f '${path.module}/kubeconfig' || true
    EOT
  }
}

# Carries host — the one variable with no default — across a destroy or retry without requiring
# -var on the command line (exact same lifetime inversion pattern as testing/infra/clusters/k3s).
resource "null_resource" "vars_snapshot" {
  triggers = {
    snapshot = jsonencode({
      host = var.host
    })
    path = "${path.module}/.last-apply.auto.tfvars.json"
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${self.triggers.path}' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["/bin/bash", "-c"]
    command     = "rm -f '${self.triggers.path}'"
  }
}
