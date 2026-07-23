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

  server_url = "https://${local.first_server.host}:${var.server_https_listen_port}"

  # kubectl context/cluster/user name for the fetched kubeconfig. The raw
  # k3s.yaml names everything "default", so we namespace it to the first server
  # host (sanitized to the characters kubectl names accept).
  context_name = "k3s-${replace(local.first_server.host, "/[^a-zA-Z0-9]/", "-")}"

  # Standalone rewritten kubeconfig kept in the module dir; also merged into ~/.kube/config.
  kubeconfig_path = "${path.module}/kubeconfig"
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
  triggers = {
    host                     = local.first_server.host
    user                     = local.first_server.user
    port                     = var.server_ssh_port
    key_path                 = pathexpand(var.ssh_private_key)
    version                  = var.release
    flannel_backend          = var.flannel_backend
    cluster_cidr             = var.cluster_cidr
    service_cidr             = var.service_cidr
    server_https_listen_port = var.server_https_listen_port
    service_node_port_range  = var.service_node_port_range
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)
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
      "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${var.release}' K3S_TOKEN='${random_string.token.result}' sh -s - server --cluster-init --flannel-backend ${var.flannel_backend} --cluster-cidr ${var.cluster_cidr} --service-cidr ${var.service_cidr} --tls-san ${local.first_server.host} --https-listen-port ${var.server_https_listen_port} --service-node-port-range ${var.service_node_port_range}",
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
  depends_on = [null_resource.server_init]

  triggers = {
    host     = each.value.host
    user     = each.value.user
    port     = var.server_ssh_port
    key_path = pathexpand(var.ssh_private_key)
    # Re-run the install when the release, backend, or join target changes, so
    # every member reacts together instead of only the first server.
    version                  = var.release
    server                   = local.server_url
    flannel_backend          = var.flannel_backend
    cluster_cidr             = var.cluster_cidr
    service_cidr             = var.service_cidr
    server_https_listen_port = var.server_https_listen_port
    service_node_port_range  = var.service_node_port_range
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)
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
      "timeout 180 bash -c 'until (exec 3<>/dev/tcp/${local.first_server.host}/${var.server_https_listen_port}) 2>/dev/null; do sleep 3; done'",
      "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${var.release}' K3S_TOKEN='${random_string.token.result}' sh -s - server --server ${local.server_url} --flannel-backend ${var.flannel_backend} --cluster-cidr ${var.cluster_cidr} --service-cidr ${var.service_cidr} --tls-san ${each.value.host} --https-listen-port ${var.server_https_listen_port} --service-node-port-range ${var.service_node_port_range}",
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
  depends_on = [null_resource.server_init]

  triggers = {
    host     = each.value.host
    user     = each.value.user
    port     = var.agent_ssh_port
    key_path = pathexpand(var.ssh_private_key)
    # Re-run the install when the release or the server URL changes, so agents
    # track the servers instead of only the first server being reinstalled.
    version                  = var.release
    server                   = local.server_url
    server_https_listen_port = var.server_https_listen_port
    # Tracked so this agent re-provisions when the pod network changes, and so the
    # destroy-time route flush can read the CIDR off self.triggers.
    cluster_cidr = var.cluster_cidr
  }

  connection {
    type        = "ssh"
    host        = self.triggers.host
    user        = self.triggers.user
    port        = tonumber(self.triggers.port)
    private_key = file(self.triggers.key_path)
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
      "timeout 180 bash -c 'until (exec 3<>/dev/tcp/${local.first_server.host}/${var.server_https_listen_port}) 2>/dev/null; do sleep 3; done'",
      "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${var.release}' K3S_TOKEN='${random_string.token.result}' K3S_URL='${local.server_url}' sh -s - agent",
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

# Fetch the kubeconfig via sudo (k3s writes it root-only at mode 600), namespace
# its "default" identifiers to a per-cluster context, repoint the server URL from
# 127.0.0.1 to the reachable host, and flatten-merge it into ~/.kube/config. Retries because
# k3s.yaml appears a few seconds after the service starts. The sed rules are
# anchored to whole identity lines so the base64 certificate blobs stay intact.
resource "null_resource" "kubeconfig" {
  depends_on = [null_resource.server_init]

  triggers = {
    host                     = local.first_server.host
    context                  = local.context_name
    server_https_listen_port = var.server_https_listen_port
    # Re-fetch when the first server is reinstalled (new certificates), so
    # ~/.kube/config never keeps stale credentials.
    server_init = null_resource.server_init.id
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      raw="$(mktemp)"
      merged=""
      trap 'rm -f "$raw" "$merged"' EXIT
      for i in $(seq 1 30); do
        if ssh -i '${pathexpand(var.ssh_private_key)}' -p ${var.server_ssh_port} \
             -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
             '${local.first_server.user}@${local.first_server.host}' \
             'sudo test -s /etc/rancher/k3s/k3s.yaml && sudo cat /etc/rancher/k3s/k3s.yaml' 2>/dev/null \
             > "$raw" && test -s "$raw"; then
          sed -E \
            -e 's|https://127\.0\.0\.1:[0-9]+|https://${local.first_server.host}:${var.server_https_listen_port}|' \
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
          # this cluster and makes it the current context (as aws update-kubeconfig does).
          KUBECONFIG='${local.kubeconfig_path}':"$HOME/.kube/config" \
            kubectl config view --flatten > "$merged"
          mv "$merged" "$HOME/.kube/config"
          chmod 600 "$HOME/.kube/config"
          echo "merged context ${local.context_name} into ~/.kube/config"
          exit 0
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
  provisioner "local-exec" {
    when        = destroy
    on_failure  = continue
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      kubectl config delete-context '${self.triggers.context}' || true
      kubectl config delete-cluster '${self.triggers.context}' || true
      kubectl config unset 'users.${self.triggers.context}' || true
    EOT
  }
}
