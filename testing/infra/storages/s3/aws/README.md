# AWS S3 bucket

Provision an AWS S3 bucket via Terraform, used to test S3 storage services
across clusters.

## What it does

Creates an S3 bucket named `<s3_name_prefix>-<random-suffix>` with
`force_destroy = true` (destroy removes the bucket together with its objects).

## Prerequisites

1. Install the AWS CLI and run `aws configure` to set your access key / secret
   key (the identity needs S3 permissions).
2. Install `terraform`.

## Usage

```bash
cd testing/infra/storages/s3/aws
terraform init
terraform apply                       # or: -var='region=ap-east-1'

terraform output                      # bucket domain names, region, ...
terraform destroy
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `region` | AWS region | `ap-east-1` |
| `s3_name_prefix` | Bucket name prefix (a random suffix is appended) | `gpustack-s3` |

## Outputs

`region`, `bucket_domain_name`, `bucket_regional_domain_name`, `bucket_region`,
`bucket_website_domain`, `bucket_website_endpoint`.
