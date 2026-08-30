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
- Reaches nodes through an SSH jump host when one is named (`ssh_jumper`), and
  advertises a cluster address that differs from the SSH address when one is
  given (`node_internal_ip`). See [Addressing the nodes](#addressing-the-nodes).
- Fetches the cluster kubeconfig, renames its context/cluster/user to
  `k3s-<first-server>`, merges it into `~/.kube/config`, and makes it the
  current context (unless `switch_kube_context=false`); on destroy it removes
  that context/cluster/user.
- For every node, fetches `/etc/docker/daemon.json` to the local machine and
  parses it locally with `jq` (the remote host is never required to have
  `jq`). Any custom runtime found there (e.g. `ascend`, `amd`) gets a matching
  k3s containerd runtime handler, written to both `config.toml.tmpl` and
  `config-v3.toml.tmpl` under `/var/lib/rancher/k3s/agent/etc/containerd/`
  (containerd 2, the default on k3s v1.34+, reads the v3 name). The two files
  get **different** content: containerd's CRI plugin is
  `io.containerd.grpc.v1.cri` in config version 2 and
  `io.containerd.cri.v1.runtime` in version 3, and containerd silently ignores a
  runtime declared under the other version's path. `nvidia` is always dropped --
  k3s auto-detects and wires it itself. A node with no `daemon.json` or no
  non-`nvidia` runtimes is left alone; if a previously written template
  disappears from `daemon.json`, it is removed and the service restarted.
- Keeps a per-release artifact cache on each node (`image_archives_dir`, on by
  default) holding the image archives, the k3s binary and the installer itself.
  The archives are staged into k3s' images directory between the reclaim and the
  install and the binary is put in place, so the install runs with
  `INSTALL_K3S_SKIP_DOWNLOAD=true` and a warm node needs no network at all. See
  [Artifact cache](#artifact-cache).
- Asserts after the install that `k3s --version` matches `release`, so Terraform
  state cannot claim a Kubernetes version the cluster does not run.
- Advertises the node's own cluster address as `--node-ip` and the address you
  SSH to as `--node-external-ip` when the two differ, pinning
  `--advertise-address` to the former so the apiserver keeps advertising an
  address its node actually holds.
- Registers a `RuntimeClass` named after each of those runtimes, since a
  containerd handler alone is not selectable by a Pod. Vendors whose accelerator
  device nodes are injected by their runtime's OCI hook (AMD's
  `amd-container-runtime` injects `/dev/kfd` and `/dev/dri`) get no devices at
  all without one.

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
4. Install `terraform`, `kubectl`, `ssh` and **`jq`** locally. `jq` is a *workstation*
   requirement, not a node one: each node's `daemon.json` is fetched here and the containerd
   runtimes are rendered locally. Without it the render is empty, which is indistinguishable
   from "this node has no custom runtimes" -- so the module refuses to run rather than guess.

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
- `switch_kube_context`: defaults to `true`, which leaves the merged context
  current. Pass `-var='switch_kube_context=false'` to keep the context you are
  already on -- the cluster is still merged in, it just is not what a bare
  `kubectl` talks to.
- `image_archives_dir`: absolute path **on each node** for the airgap image
  cache, on by default at `/var/lib/k3s-image-archives`; pass `''` to disable it.
  See below.
- `mirror` / `system_default_registry`: for hosts that cannot reach github.com or
  get.k3s.io -- where the cache is filled from, and where runtime system-image
  pulls resolve. See [China networks](#china-networks-mirror-and-system-registry).
- `node_internal_ip` / `ssh_jumper` / `ssh_jumper_port`: for nodes whose cluster address
  is not the address you SSH to, or that are only reachable through another
  machine. Three shapes, see [Addressing the nodes](#addressing-the-nodes).

### Addressing the nodes

`server` / `agent` are always the addresses **this workstation SSHes to**.
`node_internal_ip` is an **exception list**: it names the address a node uses
*inside* the cluster, and only for the nodes where that is not the address you
SSH to. Those two rules produce three shapes.

**1 -- every node reachable from here.** Address them outwardly, map each to its
internal address:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@203.0.113.11"]' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2","203.0.113.11"="10.0.0.3"}'
```

The map is optional in this shape -- leave it out and each node detects its own
address on its default route. What it buys is that the nodes join over the
internal network instead of hairpinning through a public address, and that the
address is pinned rather than detected.

**2 -- a separate jump host in front of the nodes.** The jump host need not be
one of the nodes:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@10.0.0.3"]' \
  -var='ssh_jumper=root@203.0.113.99' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2"}'
```

The agent is reached through the jump host, and so is the server -- SSH to
*every* node whose host differs from the jumper's goes through it. The server
still carries an address this workstation can reach on
`server_https_listen_port`, which is the constraint below: addressing every node
internally and reaching all of them through the jumper installs a cluster and
then **fails the apply**, because the last step checks the apiserver from here.
[Known limits](#known-limits) has the workaround.

**3 -- the jump host is one of the servers.** The special case of 2, and the
usual shape for lab hardware: one machine with an outward address, the rest
internal-only behind it:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@10.0.0.3"]' \
  -var='ssh_jumper=203.0.113.10' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2"}'
```

Only the server needs an entry -- it is addressed publicly and holds `10.0.0.2`
internally. Mapping the agent to itself would say nothing.

One line of the module covers all three: every node whose SSH host differs from
the jumper's is reached through it, and the jumper's own host is reached
directly. So 3 is a special case in what you type, not in the code.

**Where each half lands.** The value is the node's own address inside the
cluster: its `--node-ip`, the first server's join address, and `--tls-san`
alongside the SSH host -- a certificate covering only one of those two addresses
would fail whichever client used the other. The key becomes the node's
`--node-external-ip`, which is what `kubectl get nodes -o wide` shows under
`EXTERNAL-IP`. A node with no entry, or one mapped to itself, reports `<none>`
there: it has no separate outward address to declare. Both halves are addresses,
so a mapped host is given as an IP literal -- `server` / `agent` otherwise accept
a DNS name, and k3s defines both flags as addresses rather than names.

Declaring an external address drags one more setting with it, which this module
pins rather than leaves to default. Both distributions derive
`--advertise-address` from `--node-external-ip` and only then from `--node-ip`,
so left alone the apiserver advertises the outward address to cluster members and
the `kubernetes` Service endpoint becomes an address no node holds. Every
in-cluster API call then leaves the cluster and comes back -- and comes back at
all only on a network that hairpins its NAT. So `--advertise-address` is set to
the cluster address, on the servers only: it is a listener flag the agent does
not accept, and an unknown flag is fatal at parsing rather than ignored.

**A host you leave out of the map is one whose SSH address is already its cluster
address.** That is the convention, and it is what makes the join address fall back
to the SSH host by contract rather than by guesswork. The module does not, however,
copy that address into `--node-ip`: with no entry it passes nothing and lets k3s
detect the address on the node's default route. Same answer when the convention
holds, and a working node when it does not -- a kubelet handed an address its host
does not own refuses to start.

**A separate jump host also goes into the servers' `--tls-san`** (shape 2 only;
in shape 3 its address is already there as the server's own). That is not what
makes the fetched kubeconfig work -- nothing serves the apiserver on the jump
host. It is what a client needs when something *does* front the API there: a
port forward, a DNAT rule, or that address later becoming the cluster's public
name.

**The first server must be reachable from this workstation on
`server_https_listen_port` as well as on SSH.** The apiserver endpoint is not
proxied: the kubeconfig is rewritten to the first server's SSH host and `kubectl`
runs against it from here. A first server reachable only through a bastion would
install a cluster and then fail the apply with an unusable kubeconfig. A bastion
in front of the *other* nodes is fully supported.

## Artifact cache

k3s imports every archive it finds under `/var/lib/rancher/k3s/agent/images/`
when it starts. `image_archives_dir` names a directory on the nodes where the
module keeps that archive, the release's own `k3s` binary, and a pinned copy of
the installer. **It is on by default** at `/var/lib/k3s-image-archives`, since a
throttled registry is the normal case for this hardware; point it elsewhere, or
disable it entirely:

```bash
terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='image_archives_dir='      # no cache: k3s pulls every image as before
```

On each node, between reclaiming the host and running the installer, the module:

1. creates `<image_archives_dir>/<release>/` -- one directory per release tag,
2. fetches `sha256sum-<arch>.txt` first (it is the trust anchor for the rest),
3. downloads the airgap archive that file lists -- `.tar.zst` for preference --
   and the release's `k3s` binary, each only if the cache does not already have
   it, to a `partial.` name, verified there and renamed into place only once it
   checks out,
4. fetches `install.sh` from `get.k3s.io` unless the cache already has it, which
   pins the installer per release: an upstream edit cannot change what a
   re-apply installs,
5. puts the binary in `/usr/local/bin/k3s`, executable,
6. copies every archive in that directory into k3s' images directory.

The install then runs the cached `install.sh` with
`INSTALL_K3S_SKIP_DOWNLOAD=true`, so a warm cache reaches the network for
nothing at all -- and a post-install `k3s --version` assertion is what makes the
cached binary's version answerable for.

So the first apply fills the cache and every later one is a local copy: no
download, and no registry pull when the cluster starts.

Worth knowing:

- **The cache is keyed by `release`.** Changing `release` uses (and fills) a
  different directory, so one version's archives can never be staged for
  another version's binary.
- **`INSTALL_K3S_SKIP_DOWNLOAD=true` also skips the SELinux policy RPM**, which
  is the same variable. That is deliberate and matches the rke2 module, which
  forces the tar method so the installer cannot add a vendor repository to the
  host either. A host that needs `k3s-selinux` has to carry it already.
- **Each node keeps its own cache**, and it is assumed node-local. A directory
  on shared network storage would have every node writing the same partial
  filenames with no locking; that is not supported.
- **Roughly 1 GB per release directory**, plus the staged copy under
  `/var/lib`. Nothing is ever pruned -- an old release directory is yours to
  remove, and the module never deletes what it did not create.
- **Restricted network?** Pre-place `sha256sum-<arch>.txt`, the archive, the
  `k3s` binary (`k3s-arm64` / `k3s-armhf` on those architectures) and
  `install.sh` in the version directory yourself; whatever is already there is
  used as found and nothing is fetched. That is a full airgap install.
- **Your own bundles are welcome.** Any other `*.tar`, `*.tar.gz` or `*.tar.zst`
  in the release directory is staged too, and logged as unverified since the
  release publishes no digest for it. `*.txt` files are never staged: there they
  are metadata, but in the images directory a `.txt` means "pull every image
  named in me".
- **Keep it out of `/var/lib/rancher/k3s`** (rejected by validation): the
  uninstall that runs before every install removes that tree.
- **`terraform destroy` leaves it in place**, deliberately: the uninstall removes
  the *staged copies* under `/var/lib/rancher/k3s`, never the cache, so the next
  apply is as fast as the last one. It is yours to delete when you are done with
  the host.
- **Changing the value re-provisions every node.** It is tracked in each install
  resource's `triggers`, so a change takes effect now rather than at whatever
  later reinstall happens to come along -- including the change from no cache to
  the default one, on a directory that already holds state.

After apply:

```bash
kubectl config use-context "$(terraform output -raw context_name)"
kubectl get nodes

# Or, without switching what a bare kubectl points at:
kubectl --context "$(terraform output -raw context_name)" get nodes
```

Tear down (no `-var` needed -- `server` is carried across from the last apply):

```bash
terraform destroy
```

That carry-across is a `.last-apply.auto.tfvars.json` the module writes before it touches the first
node and removes once the last one is gone. So it is there for a destroy, and for a **retry** of a
destroy that failed part-way -- and after a destroy that succeeded it is gone, which means the next
apply needs `-var server=...` again. That is deliberate: a snapshot left lying around is a value that
silently overrides a hand-written `terraform.tfvars` on every later apply in this directory.

**It carries `server` and nothing else**, which is all a destroy needs -- everything else either has a
default or does not bear on tearing down what state already holds. A `plan` or a re-apply still needs
every non-default variable on the command line. In a directory that has agents, leaving `agent` out
makes the plan propose **destroying them**: with the variable empty they are no longer keys in the
`for_each` map, which reads as "these nodes were removed" rather than "these nodes were not mentioned".

## China networks: mirror and system registry

For hosts that cannot reach github.com or get.k3s.io, two orthogonal variables.
`mirror` decides where **install-time artifacts** come from;
`system_default_registry` decides where **runtime system-image pulls**
(`docker.io/rancher/mirrored-*`) resolve. An airgap-staged node needs no registry;
an online node may want the registry without the mirror.

`mirror=cn` points the module's own download script at rancher-mirror.rancher.cn:

- release assets come from `https://rancher-mirror.rancher.cn/k3s/<tag with '+' spelled '-'>/`
  (e.g. `.../k3s/v1.34.9-k3s1/sha256sum-amd64.txt`) -- the same asset names and
  byte-identical checksum files as GitHub, so a cache warmed in one mode verifies
  cleanly in the other and the cache layout is unchanged;
- the installer comes from `https://rancher-mirror.rancher.cn/k3s/k3s-install.sh`.

```bash
terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='mirror=cn'
```

Worth knowing:

- **`mirror=cn` requires the artifact cache** and is rejected together with
  `image_archives_dir=''`: without the cache the only CN-reachable install path
  would be the installer's own `INSTALL_K3S_MIRROR` parameter, which this module
  deliberately never sets -- the upstream and CN-hosted `install.sh` variants
  differ, and a node may already hold a cached copy of either. Mirror downloads
  are done by the module's script instead, so whichever variant a node has cached
  is irrelevant.
- **The mirror can lag a freshly-cut release.** A release absent from it fails at
  download with the cache's usual error; pick an older `release` rather than
  working around it.

`system_default_registry` is passed as `--system-default-registry` to the
**server** installs only -- the k3s agent CLI has no such flag, and agents get
their system images from the staged archives:

```bash
terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='system_default_registry=registry.rancher.cn'
```

`registry.rancher.cn` is the CN-reachable example; it is never a default.
`rancher-mirror.rancher.cn` is **not** an OCI registry and does not work as a
value here.

## Known limits

Things the module does not do:

- **A first server reachable only through the jump host is not supported, and the failure comes last.**
  The final step rewrites the fetched kubeconfig to the first server's SSH host and then polls
  `/readyz` **from this workstation**, 60 times. With that host on an internal network the cluster
  installs, every node goes Ready, and the apply then fails on the poll. Two ways round it: give the
  first server an address reachable here on `server_https_listen_port` (shape 2 under
  [Addressing the nodes](#addressing-the-nodes)), or tunnel and drive the cluster by hand --
  `ssh -L 6443:10.0.0.2:6443 root@<jump host>`, then point `kubectl` at `https://127.0.0.1:6443`. The
  tunnel needs no module change because k3s already puts `127.0.0.1` and `localhost` in the serving
  certificate, alongside the addresses this module adds.
- **Reinstalling the first server reinstalls every other node.** It owns the datastore and the
  cluster CA, so a member that outlived it would hold credentials for a cluster that no longer
  exists. Every other install therefore tracks the first server's resource -- including a retry after
  a failed provisioner, where nothing else about the other nodes changed. Re-provisioning ONE node
  without disturbing the others holds for every node except the first server.
- **A jump host that is one of the *agents* is not signed into the servers' certificate.** `tls-san`
  is a server setting, and a jumper that is itself a managed node is taken to have its address
  covered already -- which is true for a server and not for an agent. A client fronting the apiserver
  at an agent's address fails the handshake; put that address on a server, or front the API somewhere
  the certificate covers.
- **Control planes are restarted, and joined, concurrently.** The containerd step runs `for_each` over
  every node with no ordering, so every control plane restarts its service at the same time; additional
  servers likewise join in parallel. With one or two servers that is nothing. With **three or more**,
  restarting every etcd member at once can lose quorum -- run those applies with
  `terraform apply -parallelism=1`.
- **A cache holding another release's artifacts is only caught after the install.** The
  release-keyed layout makes version skew unreachable for anything the module downloaded itself, but
  an operator who hand-places a different release's files in a version-named directory gets a cache
  that is internally consistent -- the checksum file matches the artifacts -- and nothing in the
  download protocol can fault it. The post-install `k3s --version` assertion is what catches it,
  after the install rather than before. The images imported at startup are a weaker case still:
  another release's images with a matching digest are indistinguishable from the right ones.
- **`RuntimeClass` pruning is per node, not per cluster.** Each node's step prunes the classes this
  module owns against the runtimes *that node* renders. On a cluster whose nodes carry different
  `daemon.json` runtimes they therefore collide **within a single apply** as well as between applies:
  the nodes run concurrently, so one deletes what another has just created, and the surviving set
  depends on which finished last. A mixed-accelerator cluster wants a cluster-scoped step that
  unions every node's rendered set.
- **`scripts/render-containerd-runtimes.sh` exists twice, once per cluster module, and nothing checks
  that the copies agree.** That is deliberate -- a module directory complete on its own is worth more
  here than a de-duplicated one -- and each copy carries the same test suite, so an unintended
  *behaviour* change fails in the copy that was edited. An accidental divergence in a comment is
  invisible; compare the two files by hand if it matters.

## Variables

| Variable | Description | Default |
|---|---|---|
| `server` | Server (control-plane) hosts, `host` or `user@host`; at least one | (required) |
| `agent` | Agent (worker) hosts; when empty the servers run workloads themselves | `[]` |
| `ssh_user` | Fallback SSH user when an address has no `user@` prefix | `root` |
| `ssh_private_key` | Path to the SSH private key | `~/.ssh/id_ed25519` |
| `server_ssh_port` / `agent_ssh_port` | SSH ports | `22` |
| `node_internal_ip` | Exception list: the in-cluster address of a node whose SSH address is not already it. The value becomes `node-ip` and, for the first server, the join address; the key becomes `node-external-ip`. Both are IP literals | `{}` |
| `ssh_jumper` | SSH jump host (`host` or `user@host`) for nodes not directly reachable | `""` |
| `ssh_jumper_port` | SSH port of the jump host | `22` |
| `release` | K3s version (`INSTALL_K3S_VERSION`) | `v1.34.9+k3s1` |
| `flannel_backend` | `vxlan` / `host-gw` / `wireguard-native` / `none` | `vxlan` |
| `cluster_cidr` | Pod network (`--cluster-cidr`, comma-separated for dual-stack) | `10.42.0.0/16` |
| `service_cidr` | Service network (`--service-cidr`, comma-separated for dual-stack) | `10.43.0.0/16` |
| `server_https_listen_port` | Kubernetes apiserver port (`--https-listen-port`) | `6443` |
| `service_node_port_range` | NodePort Service port range (`--service-node-port-range`) | `30000-32767` |
| `image_archives_dir` | Absolute path on each node for the per-release airgap image cache; `""` disables it | `/var/lib/k3s-image-archives` |
| `mirror` | Where the cache is filled from: `""` (github.com / get.k3s.io) or `cn` (rancher-mirror.rancher.cn); requires `image_archives_dir` | `""` |
| `system_default_registry` | `--system-default-registry` on the server installs, e.g. `registry.rancher.cn`; the k3s agent has no such flag | `""` |
| `switch_kube_context` | Let the merged context become the current one; `false` restores the previous one | `true` |

## Outputs

| Output | Description |
|---|---|
| `context_name` | The kubectl context merged into `~/.kube/config` |
| `server_hosts` / `agent_hosts` | Server / agent host addresses |
| `kubeconfig_path` | Path to the standalone kubeconfig in the module directory |
