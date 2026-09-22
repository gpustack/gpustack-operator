provider "aws" {
  region = var.region
}

data "aws_availability_zones" "available" {
  exclude_zone_ids = ["us-east-1e"]
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

resource "random_string" "suffix" {
  length  = 8
  special = false
}

locals {
  eks_name = "${var.name_prefix}-${random_string.suffix.result}"

  azs = slice(data.aws_availability_zones.available.names, 0, 3)
  public_subnets = [
    for i in range(length(local.azs)) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnets = [
    for i in range(3) : cidrsubnet(var.vpc_cidr, 8, i + length(local.azs))
  ]

  # iops/throughput are optional on node_boot_disk_type, so only merge them in when set.
  node_boot_disk_ebs = merge(
    {
      volume_size           = var.node_boot_disk_size_gb
      volume_type           = var.node_boot_disk_type.volume_type
      delete_on_termination = true
    },
    var.node_boot_disk_type.iops != null ? { iops = var.node_boot_disk_type.iops } : {},
    var.node_boot_disk_type.throughput != null ? { throughput = var.node_boot_disk_type.throughput } : {},
  )

  # Instance-store devices take letters b..y (xvda is the boot volume).
  node_instance_store_letters = ["b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y"]
  node_instance_store_mappings = {
    for i in range(var.node_instance_store_count) :
    "ephemeral${i}" => {
      device_name  = "/dev/xvd${local.node_instance_store_letters[i]}"
      virtual_name = "ephemeral${i}"
    }
  }

  node_block_device_mappings = merge(
    {
      xvda = {
        device_name = "/dev/xvda"
        ebs         = local.node_boot_disk_ebs
      }
    },
    local.node_instance_store_mappings,
  )
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.6.1"

  name = local.eks_name
  cidr = var.vpc_cidr

  azs             = local.azs
  private_subnets = local.private_subnets
  public_subnets  = local.public_subnets

  enable_nat_gateway     = true
  single_nat_gateway     = true
  one_nat_gateway_per_az = false

  enable_dns_hostnames = true

  public_subnet_tags = {
    "kubernetes.io/role/elb" = 1
    Environment              = "testing"
    Terraform                = "true"
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = 1
    Environment                       = "testing"
    Terraform                         = "true"
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

resource "aws_key_pair" "accessor" {
  key_name   = "${local.eks_name}-accessor"
  public_key = file(pathexpand(var.ssh_public_key))
}

locals {
  # Scheduled sweeps terminate untagged instances; DO_NOT_DELETE keeps the
  # nodes of a live verification cluster alive. Merged into every node group,
  # which propagates to the launch template's instance/volume/ENI tag specs.
  node_group_tags = { DO_NOT_DELETE = "true" }

  node_groups = merge(
    # A CPU group that should not exist is an absent key in cpu_instance_types, the
    # same rule clusters/nebius uses. On an accelerator-only cluster the plain node is
    # not a small extra cost but a node nothing schedules onto, and an empty group is
    # not a way to express that here: desired_size is ignored on an existing group, so
    # a group asked for and then emptied is a group that stays.
    {
      for name, cfg in var.cpu_instance_types : name => {
        # https://docs.aws.amazon.com/eks/latest/APIReference/API_Nodegroup.html#AmazonEKS-Type-Nodegroup-amiType
        ami_type           = "AL2023_x86_64_STANDARD"
        desired_size       = cfg.node_count
        max_size           = cfg.node_count
        min_size           = cfg.node_count
        instance_types     = cfg.instance_types
        key_name           = aws_key_pair.accessor.key_name
        tags               = local.node_group_tags
        labels             = var.efa_enabled ? { "gpustack.ai/efa" = "true" } : {}
        enable_efa_support = var.efa_enabled
        enable_efa_only    = false
        # A cluster placement group is scoped to one availability zone, so the group takes a
        # single subnet, and under EFA it has to be a private one: an EFA interface cannot
        # carry a public address, so in a public subnet the node would have no route out and
        # EKS refuses the group with Ec2SubnetInvalidConfiguration. Those nodes are not
        # reachable over SSH either way.
        subnet_ids = var.efa_enabled ? [module.vpc.private_subnets[var.efa_availability_zone_index]] : null
        # Under EFA the module substitutes its own interface set, sized and indexed for the
        # instance's network cards, so this group declares none of its own.
        network_interfaces = var.efa_enabled ? [] : [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = local.node_block_device_mappings
      }
    },
    {
      for name, cfg in var.gpu_instance_types : name => {
        ami_type = "AL2023_x86_64_NVIDIA"
        # desired_size REACHES AWS ONLY ON CREATE. The module this delegates to declares
        # ignore_changes on scaling_config[0].desired_size, so terraform will not move a
        # group that already exists: lowering this on a running group plans only the
        # max_size line below, which leaves max under desired. Park an idle group with
        # `aws eks update-nodegroup-config --scaling-config desiredSize=0` instead, which
        # is drift-free precisely because the attribute is ignored here.
        desired_size = cfg.node_count
        # EKS refuses a maximum below one; the variable's validation keeps node_count at
        # one or more, so max follows the count with no floor needed.
        max_size           = cfg.node_count
        min_size           = 0
        instance_types     = cfg.instance_types
        key_name           = aws_key_pair.accessor.key_name
        tags               = local.node_group_tags
        labels             = var.efa_enabled ? { "gpustack.ai/efa" = "true" } : {}
        enable_efa_support = var.efa_enabled
        enable_efa_only    = false
        # Same rule as the cpu group: a cluster placement group is scoped to one
        # availability zone, and an EFA interface cannot carry a public address, so
        # under EFA the group takes a private subnet. It is the same private subnet
        # the cpu group takes, on purpose: one availability zone is REQUIRED for
        # cross-node RDMA, which does not reach across zones. Not a copy-paste slip.
        #
        # ONE ZONE IS NOT ENOUGH, AND NOTHING HERE MAKES IT ENOUGH. The upstream module
        # builds one cluster placement group per node group, and EFA needs both ends of
        # a transfer inside the SAME group, so a transfer between this group and the cpu
        # group cannot complete however the zones line up. It fails silently: the
        # handshake succeeds, the endpoint pair reports established, then work requests
        # hang with the receiving adapter's counters at zero. Measured, and left as is --
        # sharing one group across node groups is the upstream module's shape to change,
        # not this file's. Keep both ends of a cross-node fabric test in one node group;
        # the README says the same thing where someone running it will read it.
        subnet_ids = var.efa_enabled ? [module.vpc.private_subnets[var.efa_availability_zone_index]] : null
        # Under EFA the module substitutes its own interface set, sized and indexed for
        # the instance's network cards, so this group declares none of its own.
        network_interfaces = var.efa_enabled ? [] : [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = local.node_block_device_mappings
      }
    }
  )
}

# module "ebs_csi_driver_irsa" {
#   source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts"
#   version = "6.6.1"
#
#   name                  = "ebs-csi"
#   attach_ebs_csi_policy = true
#
#   oidc_providers = {
#     this = {
#       provider_arn               = module.eks.oidc_provider_arn
#       namespace_service_accounts = ["kube-system:ebs-csi-controller-sa"]
#     }
#   }
#
#   tags = {
#     Environment = "testing"
#     Terraform   = "true"
#   }
# }
#
# module "efs_csi_driver_irsa" {
#   source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts"
#   version = "6.6.1"
#
#   name                  = "efs-csi"
#   attach_efs_csi_policy = true
#
#   oidc_providers = {
#     this = {
#       provider_arn               = module.eks.oidc_provider_arn
#       namespace_service_accounts = ["kube-system:efs-csi-controller-sa"]
#     }
#   }
#
#   tags = {
#     Environment = "testing"
#     Terraform   = "true"
#   }
# }

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "21.24.0"

  name               = local.eks_name
  kubernetes_version = var.release

  addons = merge(
    {
      cert-manager              = {}
      coredns                   = {}
      eks-node-monitoring-agent = {}
      external-dns              = {}
      kube-proxy                = {}
      metrics-server            = {}
      # aws-ebs-csi-driver = {
      #   service_account_role_arn = module.ebs_csi_driver_irsa.arn
      # }
      # aws-efs-csi-driver = {
      #   service_account_role_arn = module.efs_csi_driver_irsa.arn
      # }
      eks-pod-identity-agent = {
        before_compute = true
      }
      vpc-cni = {
        before_compute = true
      }
    },
    var.enable_instance_store_csi_driver ? {
      aws-ec2-local-instance-store-csi-driver = {}
    } : {},
  )

  endpoint_public_access                   = true
  enable_cluster_creator_admin_permissions = true

  compute_config = {
    enabled = false
  }

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.public_subnets

  eks_managed_node_groups = {
    for ng_name, ng_config in local.node_groups :
    ng_name => ng_config
  }

  node_security_group_additional_rules = {
    cluster-to-node-tcp = {
      description                   = "Cluster to node ingress on ephemeral tcp ports"
      protocol                      = "tcp"
      from_port                     = 1025
      to_port                       = 65535
      source_cluster_security_group = true
    }
    cluster-to-node-udp = {
      description                   = "Cluster to node ingress on ephemeral udp ports"
      protocol                      = "udp"
      from_port                     = 1025
      to_port                       = 65535
      source_cluster_security_group = true
    }
    allow-ssh-tcp = {
      description = "Allow ingress access to the nodes on SSH port"
      protocol    = "tcp"
      from_port   = 22
      to_port     = 22
      cidr_blocks = ["0.0.0.0/0"]
    }
    allow-ephemeral-tcp = {
      description = "Allow ingress access to the nodes on ephemeral tcp ports"
      protocol    = "tcp"
      from_port   = 30000
      to_port     = 32767
      cidr_blocks = ["0.0.0.0/0"]
    }
    allow-ephemeral-udp = {
      description = "Allow ingress access to the nodes on ephemeral udp ports"
      protocol    = "udp"
      from_port   = 30000
      to_port     = 32767
      cidr_blocks = ["0.0.0.0/0"]
    }
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

resource "null_resource" "update_kubeconfig" {
  depends_on = [module.eks]

  # Stash the connection details in triggers so the destroy provisioner (which
  # may only reference self) can strip the same context/cluster/user it added.
  triggers = {
    region  = var.region
    name    = module.eks.cluster_name
    arn     = module.eks.cluster_arn
    context = module.eks.cluster_name
    # Tracked so flipping the flag re-runs the update, instead of taking effect only
    # the next time the cluster itself changes.
    switch_kube_context = var.switch_kube_context
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      # Read before the update: update-kubeconfig always makes the cluster it writes the
      # current context, so keeping the current context means putting this one back
      # afterwards. Empty when there is no kubeconfig yet.
      previous="$(kubectl config current-context 2>/dev/null || true)"
      aws eks --region ${self.triggers.region} update-kubeconfig --name ${self.triggers.name} --alias ${self.triggers.context} --user-alias ${self.triggers.context}
      if [ '${var.switch_kube_context}' = 'false' ] && [ -n "$previous" ]; then
        kubectl config use-context "$previous" >/dev/null
        echo "current context left at $previous"
      fi
    EOT
  }

  # On destroy, remove the context (named by --alias), the cluster entry (named
  # by its ARN), and the user (named by --user-alias) from ~/.kube/config.
  # on_failure=continue keeps re-destroys idempotent when the entries are gone.
  provisioner "local-exec" {
    when       = destroy
    on_failure = continue
    command    = "kubectl config delete-context '${self.triggers.context}' || true; kubectl config delete-cluster '${self.triggers.arn}' || true; kubectl config unset 'users.${self.triggers.context}' || true"
  }
}
