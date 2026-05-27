output "region" {
  description = "AWS region"
  value       = var.region
}

output "bucket_domain_name" {
  description = "Domain name of the S3 bucket"
  value       = module.s3_bucket.s3_bucket_bucket_domain_name
}

output "bucket_regional_domain_name" {
  description = "Regional domain name of the S3 bucket"
  value       = module.s3_bucket.s3_bucket_bucket_regional_domain_name
}

output "bucket_region" {
  description = "Region of the S3 bucket"
  value       = module.s3_bucket.s3_bucket_region
}

output "bucket_website_domain" {
  description = "Website domain name of the S3 bucket"
  value       = module.s3_bucket.s3_bucket_website_domain
}

output "bucket_website_endpoint" {
  description = "Website endpoint of the S3 bucket"
  value       = module.s3_bucket.s3_bucket_website_endpoint
}

