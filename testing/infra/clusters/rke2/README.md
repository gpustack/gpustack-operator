# RKE2 cluster

Install RKE2 (embedded etcd) over SSH onto a set of existing servers via Terraform, and merge the
cluster's kubeconfig into your local `~/.kube/config`. The parameter panel mirrors the `k3s` module
next door, so switching a verification run between the two distributions is a directory change.

## What it does

- Installs an RKE2 server with an embedded etcd datastore on the first server host, and starts it.
  RKE2's installer installs but does not start the service, and RKE2 takes its configuration from
  `/etc/rancher/rke2/config.yaml` rather than from command-line arguments, so this module writes
  that file and then starts the unit.
- Before each install, uninstalls any pre-existing RKE2 and cleans up the residue its own uninstall
  leaves behind, including Calico's pod-CIDR blackhole route.
- Forces `INSTALL_RKE2_METHOD=tar`. On a host that has `yum` the installer otherwise defaults to
  the `rpm` method, which adds a Rancher repository to the machine.
- Asserts after the install that `rke2 --version` matches `release`, so Terraform state cannot
  claim a Kubernetes version the cluster does not run.
- Keeps a per-release artifact cache on each node (`image_archives_dir`, on by default) holding the
  binary, the image archives and the installer itself, so a warm node needs no network at all
  (with `mirror=cn` the cache deliberately holds no image archives, and the node pulls the system
  images from `system_default_registry` at runtime -- see
  [China networks](#china-networks-mirror-and-system-registry)). See [Artifact cache](#artifact-cache).
- Advertises the node's own cluster address as `node-ip` and the address you SSH to as
  `node-external-ip` when the two differ, pinning `advertise-address` to the former so the apiserver
  keeps advertising an address its node actually holds.
- With Calico, applies the multi-NIC fix that keeps CoreDNS and cross-node pod traffic working on a
  host with several subnets. See [The Calico multi-NIC fix](#the-calico-multi-nic-fix).
- For every node, fetches `/etc/docker/daemon.json` to the local machine and parses it locally with
  `jq` (the remote host is never required to have `jq`). Any custom runtime found there (e.g.
  `ascend`, `amd`) gets a matching RKE2 containerd runtime handler, written to both
  `config.toml.tmpl` and `config-v3.toml.tmpl` under
  `/var/lib/rancher/rke2/agent/etc/containerd/`. The two files get **different** content: containerd's
  CRI plugin is `io.containerd.grpc.v1.cri` in config version 2 and `io.containerd.cri.v1.runtime` in
  version 3, and containerd silently ignores a runtime declared under the other version's path.
  `nvidia` is always dropped -- RKE2 auto-detects and wires it itself.
- Registers a `RuntimeClass` named after each of those runtimes, labelled
  `gpustack.ai/managed-by=testing-infra-rke2`, since a containerd handler alone is not selectable by
  a Pod. A class this module owns whose handler is no longer rendered is pruned; a class belonging to
  a vendor's own operator is left untouched. On a cluster whose nodes carry different `daemon.json`
  runtimes the pruning collides -- see [Known limits](#known-limits).
- Fetches the cluster kubeconfig, renames its context/cluster/user to `rke2-<first-server>`,
  merges it into `~/.kube/config`, and makes it the current context (unless
  `switch_kube_context=false`); on destroy it removes that context/cluster/user and clears
  `current-context` if it named the removed context.

## Prerequisites

1. A server reachable on the network. **The first server host must be reachable from
   this workstation**: the kubeconfig is rewritten to that address and `kubectl` runs against it
   from here, so a first server reachable only through a bastion would install a cluster and then
   fail the apply with an unusable kubeconfig.
2. Key-based (passwordless) SSH login configured. The default key is `~/.ssh/id_ed25519` --
   Terraform's SSH provisioner signs RSA keys with `ssh-rsa` (SHA-1), which OpenSSH 8.8+ rejects,
   so prefer ed25519.
3. The SSH user must have **passwordless sudo** (otherwise the non-interactive provisioner hangs on
   the sudo password prompt), and RKE2's own installer refuses to run as anything but root.
4. Install `terraform`, `kubectl`, `ssh` and **`jq`** locally. `jq` is a *workstation*
   requirement, not a node one: each node's `daemon.json` is fetched here and the containerd
   runtimes are rendered locally. Without it the render is empty, which is indistinguishable
   from "this node has no custom runtimes" -- so the module refuses to run rather than guess.
5. **SELinux must not be enforcing.** The forced `tar` method never installs the `rke2-selinux`
   policy (only the rpm method pulls it from the Rancher repository), and the installer relabels
   files only when that RPM is already present. On an enforcing RPM-based host `rke2-server` would
   fail to start with nothing here explaining why; such a host is out of scope for this module.

## Usage

```bash
cd testing/infra/clusters/rke2
terraform init

terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='ssh_user=root'
```

- `server` / `agent`: accept `host` or `user@host`. A bare host uses `ssh_user`; a `user@host` entry
  carries its own user.
- `release`: the RKE2 version, e.g. `v1.34.9+rke2r1` -- the same Kubernetes patch as the k3s
  module's default. Pick another from
  <https://update.rke2.io/v1-release/channels>. It also names the cache directory and the
  download URLs, so it is authoritative for both the binary and the images.
- `cni`: `calico` (default), `canal` (RKE2's own default), `cilium`, `flannel`, or `none`.
- `cluster_cidr` / `service_cidr`: pod / service networks (default `10.42.0.0/16` /
  `10.43.0.0/16`; comma-separate two CIDRs for dual-stack).
- `service_node_port_range`: NodePort Service port range (default `30000-32767`).
- `image_archives_dir`: absolute path **on each node** for the artifact cache, on by default at
  `/var/lib/rke2-image-archives`; pass `''` to disable it. See below.
- `mirror` / `system_default_registry`: for hosts that cannot reach github.com or get.rke2.io --
  where the cache is filled from, and where runtime system-image pulls resolve. `mirror=cn` alone
  is enough; it derives the registry. See
  [China networks](#china-networks-mirror-and-system-registry).
- `calico_multi_nic_fix`: on whenever `cni` is `calico`, and an off switch only. See below.
- `node_internal_ip` / `ssh_jumper` / `ssh_jumper_port`: for nodes whose cluster address is not the address
  you SSH to, or that are only reachable through another machine. Three shapes, see
  [Addressing the nodes](#addressing-the-nodes).
- `switch_kube_context`: defaults to `true`, which leaves the merged context current. Pass
  `-var='switch_kube_context=false'` to keep the context you are already on.

### Addressing the nodes

`server` / `agent` are always the addresses **this workstation SSHes to**. `node_internal_ip` is an
**exception list**: it names the address a node uses *inside* the cluster, and only for the nodes where
that is not the address you SSH to. Those two rules produce three shapes.

**1 -- every node reachable from here.** Address them outwardly, map each to its internal address:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@203.0.113.11"]' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2","203.0.113.11"="10.0.0.3"}'
```

The map is optional in this shape -- leave it out and each node detects its own address on its default
route. What it buys is that the nodes join over the internal network instead of hairpinning through a
public address, and that the address is pinned rather than detected.

**2 -- a separate jump host in front of the nodes.** The jump host need not be one of the nodes:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@10.0.0.3"]' \
  -var='ssh_jumper=root@203.0.113.99' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2"}'
```

The agent is reached through the jump host, and so is the server -- SSH to *every* node whose host
differs from the jumper's goes through it. The server still carries an address this workstation can
reach on **6443**, which is the constraint below: addressing every node internally and reaching all of
them through the jumper installs a cluster and then **fails the apply**, because the last step checks
the apiserver from here. [Known limits](#known-limits) has the workaround.

**3 -- the jump host is one of the servers.** The special case of 2, and the usual shape for lab
hardware: one machine with an outward address, the rest internal-only behind it:

```bash
terraform apply \
  -var='server=["root@203.0.113.10"]' \
  -var='agent=["root@10.0.0.3"]' \
  -var='ssh_jumper=203.0.113.10' \
  -var='node_internal_ip={"203.0.113.10"="10.0.0.2"}'
```

Only the server needs an entry -- it is addressed publicly and holds `10.0.0.2` internally. Mapping
the agent to itself would say nothing.

One line of the module covers all three: every node whose SSH host differs from the jumper's is
reached through it, and the jumper's own host is reached directly. So 3 is a special case in what you
type, not in the code, and a standalone bastion in front of every node is the general case.

**Where each half lands.** The value is the node's own address inside the cluster: its `node-ip`, the
first server's join address, and `tls-san` alongside the SSH host -- a certificate covering only one of
those two addresses would fail whichever client used the other. The key becomes the node's
`node-external-ip`, which is what `kubectl get nodes -o wide` shows under `EXTERNAL-IP`. A node with no
entry, or one mapped to itself, reports `<none>` there: it has no separate outward address to declare.
Both halves are addresses, so a mapped host is given as an IP literal -- `server` / `agent` otherwise
accept a DNS name, and RKE2 defines both settings as addresses rather than names.

Declaring an external address drags one more setting with it, which this module pins rather than
leaves to default. Both distributions derive `advertise-address` from `node-external-ip` and only then
from `node-ip`, so left alone the apiserver advertises the outward address to cluster members and the
`kubernetes` Service endpoint becomes an address no node holds. Every in-cluster API call then leaves
the cluster and comes back -- and comes back at all only on a network that hairpins its NAT. So
`advertise-address` is set to the cluster address, on the servers only: it is a listener setting, and
an unknown key in an agent's config is fatal at flag parsing rather than ignored.

**A host you leave out of the map is one whose SSH address is already its cluster address.** That is
the convention, and it is what makes the join address fall back to the SSH host by contract rather
than by guesswork. The module does not, however, copy that address into `node-ip`: with no entry it
writes nothing and lets RKE2 detect the address on the node's default route. Same answer when the
convention holds, and a working node when it does not -- a kubelet handed an address its host does not
own refuses to start.

**A separate jump host also goes into the servers' `tls-san`** (shape 2 only; in shape 3 its address is
already there as the server's own). That is not what makes the fetched kubeconfig work -- nothing
serves the apiserver on the jump host. It is what a client needs when something *does* front the API
there: a port forward, a DNAT rule, or that address later becoming the cluster's public name.

**The first server must be reachable from this workstation on 6443 as well as on SSH.** The
apiserver endpoint is not proxied: the kubeconfig is rewritten to the first server's SSH host and
`kubectl` runs against it from here. A first server reachable only through a bastion would install a
cluster and then fail the apply with an unusable kubeconfig. A bastion in front of the *other* nodes
is fully supported.

There is deliberately **no port variable**. RKE2 fixes the apiserver on 6443 and the endpoint other
nodes join through on 9345, and exposes no flag for either, so the k3s module's
`server_https_listen_port` has no counterpart here.

After apply:

```bash
kubectl config use-context "$(terraform output -raw context_name)"
kubectl get nodes
kubectl get --raw=/readyz
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

## Artifact cache

RKE2 imports every archive it finds under `/var/lib/rancher/rke2/agent/images/` when it starts, and
its installer can take every artifact from a local directory instead of the network.
`image_archives_dir` names a directory on the nodes where the module keeps that release's artifacts.
**It is on by default** at `/var/lib/rke2-image-archives`, since a throttled registry is the normal
case for this hardware; point it elsewhere, or disable it with `-var 'image_archives_dir='`.

```
/var/lib/rke2-image-archives/v1.34.9+rke2r1/
  sha256sum-arm64.txt
  rke2.linux-arm64.tar.gz
  rke2-images.linux-arm64.tar.zst
  rke2-images-calico.linux-arm64.tar.zst
  install.sh
```

On each node, between reclaiming the host and running the installer, the module:

1. creates `<image_archives_dir>/<release>/` -- one directory per release tag,
2. fetches `sha256sum-<arch>.txt` first (it is the trust anchor for everything else, and is itself
   checked for shape so an empty or error response cannot become one),
3. downloads whatever is missing -- the binary tarball, the core image set, the CNI extra, and the
   installer script -- to a `partial.` name, verifies it there, and renames it into place only once
   it checks out,
4. fills a **separate, module-owned** directory with an allowlisted copy of exactly what the
   installer needs, hands that to the installer, and removes it afterwards,
5. after the install, copies any remaining archives from the cache into RKE2's images directory.

So the first apply fills the cache and every later one needs no network for RKE2 at all -- with
`mirror=cn` that covers the install only: the cache holds no image archives by design, and the
system images are pulled from `system_default_registry` at runtime (see
[China networks](#china-networks-mirror-and-system-registry)).

Worth knowing:

- **The cache is keyed by `release`.** Changing `release` uses (and fills) a different directory, so
  one version's images can never be paired with another version's binary. The post-install version
  assertion catches the one case left: an artifact an operator hand-placed under the wrong version.
- **The CNI extra is not optional.** RKE2's default image set carries `hardened-calico` and
  `hardened-flannel`, which is what `canal` needs; `calico`, `cilium` and `flannel` each need a
  whole stack of their own images that live only in `rke2-images-<cni>.linux-<arch>.tar.zst`.
  Because the wanted-file list is derived from `cni`, changing that variable fetches the right
  extra by construction rather than by remembering.
- **The installer is never pointed at your directory.** It copies every regular file matching
  `rke2-images-*.linux-<arch>*` into the images directory without looking at the extension, so a
  stray `.txt` there would land where it means "pull every image named in me". That is why step 4
  above exists.
- **Each node keeps its own cache**, and it is assumed node-local. A directory on shared network
  storage would have every node writing the same partial filenames with no locking; that is not
  supported.
- **Roughly 1--2 GB per release directory**, plus the staged image copies under `/var/lib`. Nothing
  is ever pruned -- an old release directory is yours to remove, and the module never deletes what
  it did not create.
- **Restricted network?** Pre-place the checksum file and the artifacts in the version directory
  yourself; whatever is already there is used as found. The installer script is the only file with
  no published digest, so it is trusted no further than TLS -- caching it per release at least pins
  it, so an upstream edit cannot change what a re-apply installs.
- **Your own bundles are welcome.** Any other `*.tar`, `*.tar.gz` or `*.tar.zst` in the release
  directory is staged too, and logged as unverified since the release publishes no digest for it.
  `*.txt` files are never staged, and the binary tarball is never staged as an image archive.
- **`terraform destroy` does not touch it.** `rke2-uninstall.sh` removes `/var/lib/rancher/rke2`,
  which takes the *staged copies* of the archives with it -- the cache itself is only ever read
  from, so the next apply is as fast as the last one. Keeping the cache under
  `/var/lib/rancher/rke2` is rejected by validation for exactly that reason. It is yours to delete
  when you are done with the host.
- **Changing the value re-provisions every node.** It is tracked in each install resource's
  `triggers`, so a change takes effect now rather than at whatever later reinstall happens to come
  along -- including the change from no cache to the default one, on a directory that already holds
  state.

## China networks: mirror and system registry

For hosts that cannot reach github.com or get.rke2.io, two orthogonal variables.
`mirror` decides where **install-time artifacts** come from;
`system_default_registry` decides where **runtime system-image pulls**
(`docker.io/rancher/mirrored-*`) resolve. They are separable -- an online node may
want the registry without the mirror -- but not independent: `mirror=cn` on its own
derives `system_default_registry=registry.rancher.cn`, because a node pointed away
from github.com is a node that cannot reach docker.io either.

`mirror=cn` points the module's own download script at rancher-mirror.rancher.cn:

- the checksum anchor and the binary tarball come from
  `https://rancher-mirror.rancher.cn/rke2/releases/download/<tag with '+' percent-encoded as '%2D'>/`
  (e.g. `.../v1.34.9%2Drke2r1/sha256sum-amd64.txt`) -- the same asset names and
  byte-identical checksum files as GitHub, so a cache warmed in one mode verifies
  cleanly in the other and the cache layout is unchanged;
- the installer comes from `https://rancher-mirror.rancher.cn/rke2/install.sh`.

The mirror carries **no `rke2-images-*` archives at all**, so cn mode downloads
none: the system images are pulled at runtime from `system_default_registry`
instead. That is why the registry is derived rather than merely suggested here --
without it a cn-mirrored node has nowhere to get a single system image. An
**explicitly** empty registry under `mirror=cn` is still rejected by a
precondition on every install resource, so that combination stays something a
caller has to mean.

```bash
terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='mirror=cn'
```

A node whose cache already holds a full release's archives needs no mirror at
all: leave `mirror` empty, and the find-or-fetch cache verifies the pre-placed
files against the checksum anchor and downloads nothing.

Worth knowing:

- **`mirror=cn` requires the artifact cache** and is rejected together with
  `image_archives_dir=''`: without the cache the only CN-reachable install path
  would be the installer's own `INSTALL_RKE2_MIRROR` parameter, which this module
  deliberately never sets -- the upstream and CN-hosted `install.sh` variants
  differ, and a node may already hold a cached copy of either. Mirror downloads
  are done by the module's script instead, so whichever variant a node has cached
  is irrelevant.
- **The mirror can lag a freshly-cut release.** A release absent from it fails at
  download with the cache's usual error; pick an older `release` rather than
  working around it.

`system_default_registry` is an Agent/Runtime setting in RKE2, valid on servers
and agents alike, so it is written as `system-default-registry` into **every**
node's `config.yaml`:

```bash
terraform apply \
  -var='server=["192.168.1.10"]' \
  -var='system_default_registry=registry.rancher.cn'
```

Left unset it **follows the mirror**: `mirror=cn` derives `registry.rancher.cn`
and no mirror derives nothing, so the `mirror=cn` command above already carries
it. `rancher-mirror.rancher.cn` is **not** an OCI registry and does not work as a
value here.

## The Calico multi-NIC fix

On a host with several subnets, Calico gets two things wrong, and both fail silently -- every node
stays `Ready` while the cluster does not work. `calico_multi_nic_fix` is on whenever `cni` is
`calico`, and is a switch that only turns it off: forcing it on for another CNI is rejected, since
both halves reconcile Calico's own objects.

The fix is two halves, applied at different times because they need different things:

1. **Which address Calico advertises as its VXLAN tunnel endpoint.** A `HelmChartConfig` written into
   `server/manifests/` *before* the first start pins `nodeAddressAutodetectionV4` to the kubelet's
   `NodeInternalIP`, which is the address the rest of the cluster routes to. Calico's own "first
   found interface" autodetection picks whatever comes first -- a container bridge, a VLAN address --
   so each node can end up encapsulating to an address the others cannot reach. Because this half is
   written before the server starts, flipping the variable from `false` to `true` reinstalls the
   servers rather than reconciling them.
2. **Which source address the node uses to reach a local pod.** A `FelixConfiguration` per node,
   applied *after* the control plane is up, sets `deviceRouteSourceAddress` to that node's
   `InternalIP`, so a reply from a Pod has a source it can return to. This half needs the cluster's
   real node names, so it cannot be a pre-start manifest.

Plus a small `hostNetwork` DaemonSet that relaxes `rp_filter` and keeps a pod-egress SNAT rule at the
**top** of `nat POSTROUTING` -- position, not mere presence, because kube-proxy re-prepends
`KUBE-POSTROUTING` on every restart and the `MASQUERADE` inside it ends nat processing. It runs on
the image `calico-node` is already running, read from the cluster: the one ref guaranteed to be on
every node, so the Pod that repairs the network never needs the network to start.

**On a single-homed node the first two halves are no-ops** -- `firstFound` and `NodeInternalIP`
resolve to the same address, and so do the kernel's route source and `deviceRouteSourceAddress`. The
DaemonSet is not free there: it relaxes reverse-path filtering and leaves a privileged Pod polling
every 60s. That is what the off switch is for.

Everything the fix creates is labelled `gpustack.ai/managed-by=testing-infra-rke2`, so an object
whose node has left the cluster -- or every object, when the fix is turned off -- is pruned without
guessing at ownership. Nothing is cleaned on destroy: the uninstall takes the cluster with it.

## Recovering a host by hand

A creation provisioner that fails taints its resource, and Terraform does not run destroy-time
provisioners for a tainted resource. So an apply that dies after the installer finished, followed
immediately by `terraform destroy`, drops the resource from state with RKE2 still installed. The
reclaim step converges that on the next apply; to clean the host without re-applying, run the
uninstaller there yourself:

```bash
# on the node -- the tar method may have put it in /opt/rke2/bin instead
sudo /usr/local/bin/rke2-uninstall.sh
sudo ip route flush root 10.42.0.0/16   # Calico's blackhole route survives the uninstall
```

An **rpm-method** RKE2 install (`rpm -q rke2-common` succeeds) is refused by the reclaim step rather
than installed over: the forced `tar` method cannot replace it, and the installer's own error for
that case names no remedy. Run `rke2-uninstall.sh` on the host and re-apply.

## Known limits

Things the module does not do:

- **A node's own password is what lets it rejoin, and the module carries it across a re-provision.**
  RKE2 gives every node a password (`/etc/rancher/node/password`, hashed into a
  `<node>.node-password.rke2` Secret) and `rke2-uninstall.sh` deletes it, so a node replaced while the
  cluster survives would present a fresh one and be refused for good. The module saves the file before
  each uninstall, to `/etc/rancher/node-password.saved` outside the directory the uninstall removes,
  and restores it before the service starts. If it is already gone -- a host cleaned by other means --
  delete that node's Secret and let it re-register:
  ```bash
  kubectl -n kube-system delete secret <node name>.node-password.rke2
  ```
  The `k3s` module needs none of this: its uninstall leaves `/etc/rancher/node` alone.
- **A `terraform destroy` leaves `/etc/rancher/node-password.saved` on each host, and it is inert
  there.** The save is in the destroy-time provisioner as well, which cannot know whether a create
  follows, and only a create restores the file and removes it. After a full destroy there is nothing
  left to match it against: the datastore goes with the cluster, and with it every
  `node-password.rke2` Secret, so the nodes of the next cluster register with whatever password they
  present. It is load-bearing in the other case only -- a node replaced while the cluster survives.
  Delete it whenever you are done with the host; a later apply neither needs it nor minds it.
- **A first server reachable only through the jump host is not supported, and the failure comes last.**
  The final step rewrites the fetched kubeconfig to the first server's SSH host and then polls
  `/readyz` **from this workstation**, 60 times. With that host on an internal network the cluster
  installs, every node goes Ready, and the apply then fails on the poll. Two ways round it: give the
  first server an address reachable here on 6443 (shape 2 above), or tunnel and drive the cluster by
  hand -- `ssh -L 6443:10.0.0.2:6443 root@<jump host>`, then point `kubectl` at
  `https://127.0.0.1:6443`. The tunnel works without any module change because RKE2 already puts
  `127.0.0.1` and `localhost` in the serving certificate, alongside the addresses this module adds.
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
- **Turning the Calico fix off removes its objects, not what its DaemonSet did to the hosts.** The
  FelixConfigurations and the DaemonSet go, which stops the pod that re-asserts them; the SNAT rule
  already at `nat POSTROUTING` position 1 and the relaxed `rp_filter` stay until the node reboots.
  The distributions' own `killall` scripts keep them too -- they drop only `KUBE-`/`CNI-` rules. Undo
  them by hand on a host that has to stay up:
  ```bash
  # <node address> is that node's InternalIP
  sudo iptables -t nat -D POSTROUTING -o cali+ -m mark --mark 0x4000/0x4000 -j SNAT --to-source <node address>
  sudo sysctl -w net.ipv4.conf.all.rp_filter=1 net.ipv4.conf.default.rp_filter=1
  ```
- **Control planes are restarted, and joined, concurrently.** The containerd step runs `for_each` over
  every node with no ordering, so every control plane restarts its service at the same time; additional
  servers likewise join in parallel. With one or two servers that is nothing. With **three or more**,
  restarting every etcd member at once can lose quorum -- run those applies with
  `terraform apply -parallelism=1`.
- **A cache holding another release's artifacts is only caught after the install.** The
  release-keyed layout makes version skew unreachable for anything the module downloaded itself, but
  an operator who hand-places a different release's files in a version-named directory gets a cache
  that is internally consistent -- the checksum file matches the archives -- and nothing in the
  download protocol can fault it. The post-install `rke2 --version` assertion is what catches it -- after the install, not before.
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
| `server` | Server (control-plane) hosts, `host` or `user@host`; at least one, the first reachable from here | (required) |
| `ssh_user` | Fallback SSH user when an address has no `user@` prefix | `root` |
| `ssh_private_key` | Path to the SSH private key | `~/.ssh/id_ed25519` |
| `agent` | Agent (worker) hosts; when empty the servers run workloads themselves | `[]` |
| `node_internal_ip` | Exception list: the in-cluster address of a node whose SSH address is not already it. The value becomes `node-ip` and, for the first server, the join address; the key becomes `node-external-ip`. Both are IP literals | `{}` |
| `ssh_jumper` | SSH jump host (`host` or `user@host`) for nodes not directly reachable | `""` |
| `ssh_jumper_port` | SSH port of the jump host | `22` |
| `calico_multi_nic_fix` | Apply the Calico multi-NIC fix; unset means "when cni is calico", and `true` requires it | `null` |
| `server_ssh_port` / `agent_ssh_port` | SSH ports | `22` |
| `release` | RKE2 version; names the cache directory and the release assets | `v1.34.9+rke2r1` |
| `cni` | `none` / `calico` / `canal` / `cilium` / `flannel` | `calico` |
| `cluster_cidr` | Pod network (`cluster-cidr`, comma-separated for dual-stack) | `10.42.0.0/16` |
| `service_cidr` | Service network (`service-cidr`, comma-separated for dual-stack) | `10.43.0.0/16` |
| `service_node_port_range` | NodePort Service port range (`service-node-port-range`) | `30000-32767` |
| `image_archives_dir` | Absolute path on each node for the per-release artifact cache; `""` disables it | `/var/lib/rke2-image-archives` |
| `mirror` | Where the cache is filled from: `""` (github.com / get.rke2.io) or `cn` (rancher-mirror.rancher.cn); requires `image_archives_dir`, and `cn` derives `system_default_registry` (the mirror carries no `rke2-images-*` archives) | `""` |
| `system_default_registry` | `system-default-registry` in every node's `config.yaml`, e.g. `registry.rancher.cn`; a host[:port], no path. Unset follows `mirror`; `""` writes nothing and is refused under `cn` | unset (`cn` derives `registry.rancher.cn`) |
| `switch_kube_context` | Let the merged context become the current one; `false` restores the previous one | `true` |

## Outputs

| Output | Description |
|---|---|
| `context_name` | The kubectl context merged into `~/.kube/config` |
| `server_hosts` / `agent_hosts` | Server / agent host addresses |
| `kubeconfig_path` | Path to the standalone kubeconfig in the module directory |
