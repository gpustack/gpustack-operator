# K3s cluster

Install K3s (embedded etcd) over SSH onto a set of existing servers via
Terraform, and merge the cluster's kubeconfig into your local `~/.kube/config`.

## What it does

- Installs a K3s server with `--cluster-init` (embedded etcd) on the first
  server; any additional servers join as control-plane members and agents join
  as workers.
- Before each install, uninstalls any pre-existing K3s and cleans up residue
  (including leftover flannel host-gw routes) so the install lands on a clean
  node.
- Fetches the cluster kubeconfig, renames its context/cluster/user to
  `k3s-<first-server>`, merges it into `~/.kube/config`, and makes it the
  current context; on destroy it removes that context/cluster/user.
- For every node, fetches `/etc/docker/daemon.json` to the local machine and
  parses it locally with `jq` (the remote host is never required to have
  `jq`). Any custom runtime found there (e.g. `ascend`) gets a matching k3s
  containerd runtime class, written to both `config.toml.tmpl` and
  `config-v3.toml.tmpl` under `/var/lib/rancher/k3s/agent/etc/containerd/`
  (containerd 2, the default on k3s v1.34+, prefers the v3 name). `nvidia` is
  always dropped -- k3s auto-detects and wires it itself. A node with no
  `daemon.json` or no non-`nvidia` runtimes is left alone; if a previously
  written template disappears from `daemon.json`, it is removed and the
  service restarted.

## Prerequisites

1. A set of servers (with Docker installed) reachable on the same network.
2. Key-based (passwordless) SSH login configured. The default key is
   `~/.ssh/id_ed25519` — Terraform's SSH provisioner signs RSA keys with
   `ssh-rsa` (SHA-1), which OpenSSH 8.8+ rejects, so prefer ed25519.
3. The SSH user must have **passwordless sudo** (otherwise the non-interactive
   provisioner hangs on the sudo password prompt). On Ubuntu, for example:
   ```bash
   sudo usermod -aG sudo <user>
   echo '<user> ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/<user>
   ```
4. Install `terraform`, `kubectl`, and `ssh` locally.

## Usage

```bash
cd testing/infra/clusters/k3s
terraform init

terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='agent=["192.168.1.11"]' \
  -var='ssh_user=ubuntu' \
  -var='flannel_backend=host-gw'
```

- `server` / `agent`: accept `host` or `user@host`. Bare hosts use `ssh_user`
  (as above); a `user@host` entry carries its own user, so you only need
  `ssh_user` (or a per-entry `user@`) when the login differs from the default
  `root`.
- `flannel_backend`: defaults to `vxlan` (works across subnets); `host-gw` is
  faster but requires all nodes on one L2 segment.
- `release`: the K3s version, e.g. `v1.34.9+k3s1`.
- `cluster_cidr` / `service_cidr`: pod / service networks (default
  `10.42.0.0/16` / `10.43.0.0/16`; comma-separate two CIDRs for dual-stack).
- `server_https_listen_port`: Kubernetes apiserver port (default `6443`).
- `service_node_port_range`: NodePort Service port range (default
  `30000-32767`).

After apply:

```bash
kubectl config use-context "$(terraform output -raw context_name)"
kubectl get nodes
```

Tear down (pass the same host variables):

```bash
terraform destroy \
  -var='server=["192.168.1.10"]' \
  -var='agent=["192.168.1.11"]' \
  -var='ssh_user=ubuntu'
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `server` | Server (control-plane) hosts, `host` or `user@host`; at least one | (required) |
| `agent` | Agent (worker) hosts; when empty the servers run workloads themselves | `[]` |
| `ssh_user` | Fallback SSH user when an address has no `user@` prefix | `root` |
| `ssh_private_key` | Path to the SSH private key | `~/.ssh/id_ed25519` |
| `server_ssh_port` / `agent_ssh_port` | SSH ports | `22` |
| `release` | K3s version (`INSTALL_K3S_VERSION`) | `v1.34.9+k3s1` |
| `flannel_backend` | `vxlan` / `host-gw` / `wireguard-native` / `none` | `vxlan` |
| `cluster_cidr` | Pod network (`--cluster-cidr`, comma-separated for dual-stack) | `10.42.0.0/16` |
| `service_cidr` | Service network (`--service-cidr`, comma-separated for dual-stack) | `10.43.0.0/16` |
| `server_https_listen_port` | Kubernetes apiserver port (`--https-listen-port`) | `6443` |
| `service_node_port_range` | NodePort Service port range (`--service-node-port-range`) | `30000-32767` |

## Outputs

| Output | Description |
|---|---|
| `context_name` | The kubectl context merged into `~/.kube/config` |
| `server_hosts` / `agent_hosts` | Server / agent host addresses |
| `kubeconfig_path` | Path to the standalone kubeconfig in the module directory |
