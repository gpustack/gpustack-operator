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
  eks_name = "${var.eks_name_prefix}-${random_string.suffix.result}"

  azs = slice(data.aws_availability_zones.available.names, 0, 3)
  public_subnets = [
    for i in range(length(local.azs)) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnets = [
    for i in range(3) : cidrsubnet(var.vpc_cidr, 8, i + length(local.azs))
  ]
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
  public_key = file("~/.ssh/id_rsa.pub")
}

locals {
  node_groups = merge(
    {
      cpu = {
        # https://docs.aws.amazon.com/eks/latest/APIReference/API_Nodegroup.html#AmazonEKS-Type-Nodegroup-amiType
        ami_type       = "AL2023_x86_64_STANDARD"
        max_size       = 1
        min_size       = 1
        instance_types = var.eks_cpu_instance_types
        disk_size      = 100
        key_name       = aws_key_pair.accessor.key_name
        network_interfaces = [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = {
          xvda = {
            device_name = "/dev/xvda"
            ebs = {
              volume_size           = 100
              volume_type           = "gp3"
              iops                  = 3000
              throughput            = 125
              delete_on_termination = true
            }
          }
        }
      }
    },
    {
      for idx, it in var.eks_gpu_instance_types :
      "gpu-${idx}" => {
        ami_type       = "AL2023_x86_64_NVIDIA"
        max_size       = 1
        min_size       = 0
        instance_types = it
        disk_size      = 100
        key_name       = aws_key_pair.accessor.key_name
        network_interfaces = [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = {
          xvda = {
            device_name = "/dev/xvda"
            ebs = {
              volume_size           = 100
              volume_type           = "gp3"
              iops                  = 3000
              throughput            = 125
              delete_on_termination = true
            }
          }
        }
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
  kubernetes_version = var.eks_version

  addons = {
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
    aws-ec2-local-instance-store-csi-driver = {}
    eks-pod-identity-agent = {
      before_compute = true
    }
    vpc-cni = {
      before_compute = true
    }
  }

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

  provisioner "local-exec" {
    command = "aws eks --region ${var.region} update-kubeconfig --name ${module.eks.cluster_name} --alias ${module.eks.cluster_name} --user-alias ${module.eks.cluster_name}"
  }
}

provider "helm" {
  kubernetes = {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
    exec = {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args        = ["eks", "get-token", "--region", var.region, "--cluster-name", module.eks.cluster_name]
    }
  }
}

locals {
  # Split the test image reference "<repository>:<tag>" into the separate fields
  # expected by the chart's "image" values. Falls back to empty fields when no
  # image is provided.
  gpustack_operator_image = length(var.image) > 0 ? try(regex("^(?P<repository>.+):(?P<tag>[^:/]+)$", var.image), { repository = var.image, tag = "" }) : { repository = "", tag = "" }
}

resource "helm_release" "gpustack_operator" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  name             = "gpustack-operator"
  namespace        = "gpustack-system"
  create_namespace = true

  chart             = "${path.module}/../../../../deploy/gpustack-operator/chart"
  dependency_update = true

  # Match the previous behaviour: do not block apply on the worker rollout, as
  # the operator installs the in-cluster applications after it starts.
  wait    = false
  timeout = 600

  set = [
    {
      name  = "image.repository"
      value = local.gpustack_operator_image.repository
    },
    {
      name  = "image.tag"
      value = local.gpustack_operator_image.tag
    },
    {
      name  = "image.pullPolicy"
      value = "Always"
    },
    # The EKS module installs the cert-manager addon, so let the chart issue the
    # worker webhook certificate through cert-manager.
    {
      name  = "worker.certmanager.enabled"
      value = "true"
    }
  ]
}
