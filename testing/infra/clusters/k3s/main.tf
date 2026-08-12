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
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -uo pipefail
      dir=/var/lib/rancher/k3s/agent/etc/containerd

      daemon_json="$(ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' 'sudo cat /etc/docker/daemon.json 2>/dev/null' || true)"
      # Two renderings of the same runtimes: containerd's CRI plugin is named
      # io.containerd.grpc.v1.cri in config version 2 but io.containerd.cri.v1.runtime in
      # version 3, and containerd ignores a runtime declared under the other version's path
      # without a word. One blob written to both templates therefore leaves whichever
      # containerd the node actually runs (2.x, config version 3, on k3s v1.34+) with no such
      # runtime at all, and every Pod selecting it fails to get a sandbox.
      rendered="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 2 || true)"
      rendered_v3="$(printf '%s' "$daemon_json" | bash '${path.module}/scripts/render-containerd-runtimes.sh' 3 || true)"
      existing="$(ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo cat $dir/config.toml.tmpl 2>/dev/null" || true)"
      existing_v3="$(ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo cat $dir/config-v3.toml.tmpl 2>/dev/null" || true)"

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
            KUBECONFIG='${local.kubeconfig_path}' kubectl delete runtimeclass.node.k8s.io "$name" --ignore-not-found &&
              echo "pruned RuntimeClass $name; its containerd handler is no longer rendered"
          fi
        done
      else
        echo "could not list this module's RuntimeClasses on ${self.triggers.host}; none pruned" >&2
      fi

      if [ "$rendered" = "$existing" ] && [ "$rendered_v3" = "$existing_v3" ]; then
        echo "containerd runtime templates on ${self.triggers.host} unchanged"
        exit 0
      fi

      if [ -z "$rendered" ]; then
        ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
          -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
          '${self.triggers.user}@${self.triggers.host}' \
          "sudo rm -f $dir/config.toml.tmpl $dir/config-v3.toml.tmpl && sudo systemctl restart ${self.triggers.service}"
        echo "removed stale containerd runtime template on ${self.triggers.host}"
        exit 0
      fi

      ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo mkdir -p $dir"
      printf '%s\n' "$rendered" | ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo tee $dir/config.toml.tmpl > /dev/null"
      printf '%s\n' "$rendered_v3" | ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo tee $dir/config-v3.toml.tmpl > /dev/null"
      ssh -i '${pathexpand(var.ssh_private_key)}' -p ${self.triggers.port} \
        -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10 \
        '${self.triggers.user}@${self.triggers.host}' "sudo systemctl restart ${self.triggers.service}"
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
  depends_on = [null_resource.server_init]

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
  #
  # Anchored to the same file the merge above writes, rather than to whatever
  # kubectl would resolve: with a KUBECONFIG set, cleanup would otherwise strip
  # the entries from that file and leave these behind in ~/.kube/config forever.
  provisioner "local-exec" {
    when        = destroy
    on_failure  = continue
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      export KUBECONFIG="$HOME/.kube/config"
      kubectl config delete-context '${self.triggers.context}' || true
      kubectl config delete-cluster '${self.triggers.context}' || true
      kubectl config unset 'users.${self.triggers.context}' || true
    EOT
  }
}

# Carries server — the one variable with no default — across a failed or interrupted destroy
# retry. Terraform auto-loads *.auto.tfvars.json on every command (incl. destroy), and
# command-line -var still overrides it on apply. A managed hashicorp/local local_file is not used
# here: it is deleted during destroy, which is exactly when the value is still needed. A
# destroy-time provisioner on this resource has the same defect: last_apply depends on every real
# resource, so it is destroyed FIRST, and the file would be gone before the nodes were.
#
# Snapshot NOTHING that has a default. Auto-loading is indiscriminate — it feeds `apply` just as
# readily as `destroy` — so a variable recorded here silently overrides its own default on every
# later apply in this directory, with nothing on the command line to hint at it. Nothing else is
# needed at destroy time either: every destroy provisioner reads its connection and its arguments
# off self.triggers, which live in state.
resource "null_resource" "last_apply" {
  depends_on = [
    null_resource.server_init,
    null_resource.server_join,
    null_resource.agent,
    null_resource.containerd_config,
    null_resource.kubeconfig,
  ]

  triggers = {
    snapshot = jsonencode({
      server = var.server
    })
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${path.module}/.last-apply.auto.tfvars.json' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }
}
