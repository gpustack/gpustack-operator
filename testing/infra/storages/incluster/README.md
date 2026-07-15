# In-cluster storage services

Deploy RustFS (S3-compatible) and an NFS server into the Kubernetes cluster your
current kubeconfig points at, used to test storage services.

## What it does

In the `gpustack-testing-system` namespace:

- Installs **RustFS** via Helm (chart `rustfs` 0.3.0, standalone mode) to
  provide an S3-compatible endpoint, with a 100Gi data volume by default.
- Deploys an **NFS server** (`gists/nfs-server`) with a 100Gi `ReadWriteMany`
  PVC and the `nfsserver-svc` service (2049/TCP, 111/UDP) to provide an NFS
  endpoint.

## Prerequisites

1. A reachable Kubernetes cluster with `~/.kube/config` pointing at it (all three
   providers use `~/.kube/config`).
   - The cluster needs a working default StorageClass — both the RustFS data
     volume and the NFS PVC rely on dynamic PV provisioning.
2. Install `terraform` locally (`kubectl` / `helm` are driven internally by the
   providers).

## Usage

```bash
cd testing/infra/storages/incluster
terraform init
terraform apply

kubectl -n gpustack-testing-system get pods,svc,pvc
terraform destroy
```

No input variables.
