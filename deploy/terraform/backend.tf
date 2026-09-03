# Remote state backend.
#
# Left COMMENTED so that `terraform init` works out of the box for someone
# reading this repository, and so that nobody accidentally writes state into
# another team's bucket. Uncomment and fill in before the first real apply —
# local state for a stack that owns a VPC and an EKS cluster is a single
# laptop failure away from an unrecoverable mess.
#
# The bucket and lock table are chicken-and-egg: they must exist before this
# backend can be used. Create them once, out of band:
#
#   aws s3api create-bucket \
#     --bucket ttl-aware-bff-tfstate \
#     --region us-east-1
#   aws s3api put-bucket-versioning \
#     --bucket ttl-aware-bff-tfstate \
#     --versioning-configuration Status=Enabled
#   aws s3api put-bucket-encryption \
#     --bucket ttl-aware-bff-tfstate \
#     --server-side-encryption-configuration \
#       '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms"}}]}'
#   aws s3api put-public-access-block \
#     --bucket ttl-aware-bff-tfstate \
#     --public-access-block-configuration \
#       'BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true'
#
# Versioning is not optional: it is the only way back from a corrupted state
# file or a bad `terraform state rm`.
#
# terraform {
#   backend "s3" {
#     bucket = "ttl-aware-bff-tfstate"
#     # One key per environment. Never share a key between environments —
#     # that is how a dev apply destroys production.
#     key    = "bff/prod/terraform.tfstate"
#     region = "us-east-1"
#
#     encrypt    = true
#     kms_key_id = "alias/terraform-state"
#
#     # S3-native locking (Terraform >= 1.9). It replaces the DynamoDB table
#     # that older stacks used; set `dynamodb_table` instead if you are on an
#     # older Terraform.
#     use_lockfile = true
#
#     # Assume a dedicated state role rather than using ambient credentials, so
#     # state access is auditable independently of the apply role.
#     # role_arn = "arn:aws:iam::222222222222:role/terraform-state"
#   }
# }
#
# Initialise per environment with a partial backend config:
#
#   terraform init -backend-config=envs/prod.s3.tfbackend
#
# where envs/prod.s3.tfbackend contains just the `key` (and any per-account
# `role_arn`), keeping the shared settings here.
