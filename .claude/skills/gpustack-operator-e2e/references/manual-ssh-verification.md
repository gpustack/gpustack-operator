# Manual SSH-Instance verification (VS Code Remote-SSH + sshfs)

`case-21.sh` covers the exec channel, the interactive login, an `sftp` round-trip, and a loopback TCP
port-forward automatically. Two things it cannot drive live in CI: a real **VS Code Remote-SSH** session
(a GUI client that opens a workspace and lands an integrated terminal in `main`) and a real **`sshfs`**
mount. This runbook is the manual pass for those two, on any cluster with an SSH-enabled Instance whose
`sshd` sidecar runs the fixed image.

The SSH server the operator injects overrides every client request with `ForceCommand /chroot.sh`, which
`nsenter`s into `main` and drops all capabilities. VS Code Remote-SSH needs the **exec channel** (its
bootstrap command) and **TCP/socket forwarding**; `sshfs` needs the **SFTP subsystem**. All three must
work through that ForceCommand for these steps to pass.

## 1. Prerequisites

- The operator is deployed and the scheduling chain is materialized (`case-1.sh` green).
- The `instance-ssh-server-image` setting points at the image under test:

  ```
  kubectl -n gpustack-system get settings.gpustack.ai instance-ssh-server-image
  ```

- An `ssh` client, VS Code with the **Remote - SSH** extension, and `sshfs` on the local machine.

## 2. Render an SSH-enabled Instance

```bash
NS=default
ssh-keygen -t ed25519 -f /tmp/inst-key -N "" -q
kubectl -n "$NS" create secret generic inst-ssh-key --from-file=authorized_keys=/tmp/inst-key.pub

IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' | tr ' ' '\n' | grep -m1 'gpustack-')

cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: inst-ssh, namespace: ${NS} }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  sshPublicKey: { name: inst-ssh-key }
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
EOF

kubectl -n "$NS" wait --for=condition=Ready pod/inst-ssh --timeout=300s
kubectl -n "$NS" get pod inst-ssh \
  -o jsonpath='{range .spec.containers[*]}{.name}={.image}{"\n"}{end}'   # sshd must be the image under test
```

## 3. Reach the sshd

Forward a local port to the sidecar (works from anywhere with cluster access):

```bash
kubectl -n "$NS" port-forward pod/inst-ssh 22022:22
```

Leave it running. Everything below connects to `127.0.0.1:22022`.

## 4. VS Code Remote-SSH

Add a host entry (`~/.ssh/config`):

```
Host inst-ssh
  HostName 127.0.0.1
  Port 22022
  User root
  IdentityFile /tmp/inst-key
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
```

In VS Code: **Remote-SSH: Connect to Host… → inst-ssh**, then **Open Folder → `/workspace`**.

Pass when:

- the connection completes (VS Code's server bootstrap, which rides the exec channel, runs — no hang on
  "Setting up SSH host");
- the folder `/workspace` opens in the Explorer;
- an integrated terminal (`` Ctrl+` ``) lands in `main`: `hostname` shows the Instance, `ls /workspace`
  matches the volume, and `grep Cap /proc/self/status` shows `CapEff` / `CapBnd` all-zero (confined);
- port forwarding works — start any listener in the terminal (e.g. `python3 -m http.server 8000`), add
  it under the **Ports** panel, and open it from the local browser.

If VS Code hangs at the bootstrap step, the exec channel is discarding the command (the regression
`case-21.sh` guards); if the Ports panel cannot forward, TCP forwarding is disabled in `sshd_config`.

## 5. sshfs mount

`sshfs` rides the SFTP subsystem, which reaches the bundled sftp-server staged into `main`:

```bash
mkdir -p /tmp/inst-mnt
sshfs -p 22022 root@127.0.0.1:/workspace /tmp/inst-mnt \
  -o IdentityFile=/tmp/inst-key,IdentitiesOnly=yes,StrictHostKeyChecking=no,UserKnownHostsFile=/dev/null

echo hello > /tmp/inst-mnt/from-laptop.txt                                  # write through the mount
ssh -p 22022 -i /tmp/inst-key -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null root@127.0.0.1 'cat /workspace/from-laptop.txt'   # reads back in main
```

Pass when the mount succeeds, the written file is listed under `/tmp/inst-mnt`, and the same bytes read
back through an exec into `main` — proving the SFTP subsystem serves `main`'s filesystem, not the sidecar.

## 6. Cleanup

```bash
umount /tmp/inst-mnt 2>/dev/null || fusermount -u /tmp/inst-mnt 2>/dev/null
kubectl -n "$NS" delete instance inst-ssh --ignore-not-found
kubectl -n "$NS" delete secret inst-ssh-key --ignore-not-found
rm -f /tmp/inst-key /tmp/inst-key.pub; rmdir /tmp/inst-mnt 2>/dev/null
```
