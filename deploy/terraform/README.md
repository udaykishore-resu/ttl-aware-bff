# TTL-aware BFF — AWS reference infrastructure

Terraform for the AWS side of the TTL-aware BFF: a VPC, an EKS cluster,
ElastiCache for the L2 cache, the public entry point (ACM + WAF + access logs),
CloudWatch/ADOT plumbing, and the IRSA role the service assumes.

This stack provisions **infrastructure only**. The application is deployed by
CI (`.github/workflows/ci.yaml`) using the Helm chart in
`deploy/helm/ttl-aware-bff` or the manifests in `deploy/k8s`. That separation is
deliberate: shipping code should never require credentials that can delete a
VPC.

---

## Layout

```
deploy/terraform/
├── versions.tf              provider + Terraform version constraints
├── backend.tf               remote state (commented; read it before first apply)
├── providers.tf             AWS/Kubernetes/Helm providers, default_tags
├── variables.tf             every input, with validation
├── main.tf                  composes the six modules
├── outputs.tf               including ready-made helm_values
├── terraform.tfvars.example
└── modules/
    ├── vpc/                 VPC, subnets, NAT, flow logs, S3 endpoint
    ├── eks/                 control plane, node group, IRSA OIDC, add-ons
    ├── elasticache/         Redis replication group, KMS, AUTH token
    ├── alb/                 ACM cert, WAFv2 Web ACL, access-log bucket
    ├── observability/       log groups, ADOT IRSA role, X-Ray, alarms
    └── iam/                 the BFF's IRSA role and least-privilege policy
```

### What this stack does *not* create

The **Application Load Balancer itself**. The AWS Load Balancer Controller
creates it from the `Ingress` object, and Terraform creating one too would
leave two load balancers fighting over the same target group. The `alb` module
owns the things the Ingress *references* — the certificate, the Web ACL, the
log bucket — because those outlive any single deployment.

The **ADOT collector**, the **AWS Load Balancer Controller** and **External
Secrets** are cluster add-ons installed by the platform. Terraform owns their
IAM identities; it does not own their Helm releases.

---

## Apply order

### Greenfield: two phases

The `kubernetes` and `helm` providers are configured from the `eks` module's
outputs. On a brand-new account those outputs are unknown at plan time, so a
single `terraform apply` fails with *"Provider configuration not known until
apply"*. Create the cluster first:

```bash
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars   # then edit it

terraform init
# uncomment backend.tf and re-run with -backend-config first, for anything real

# Phase 1 — network and cluster
terraform apply -target=module.vpc -target=module.eks

# Phase 2 — everything else
terraform apply
```

`-target` is a sharp tool and normally a smell. This is the one case where it
is the documented answer rather than a workaround.

### Subsequent applies

```bash
terraform plan -out=tfplan
terraform apply tfplan
```

Always plan to a file and apply that file. A plan you did not read is a plan
you did not approve.

### Then: connect the application

```bash
aws eks update-kubeconfig --name "$(terraform output -raw cluster_name)" \
  --region "$(terraform output -raw region)"

# The values the chart needs, ready to merge into values-prod.yaml:
terraform output -json helm_values | yq -P
```

Key outputs and where each one goes:

| Output | Consumed by |
|---|---|
| `bff_irsa_role_arn` | `serviceAccount.roleArn`, or the `eks.amazonaws.com/role-arn` annotation |
| `elasticache_primary_endpoint` | `externalRedis.host` / `BFF_CACHE__REDIS__ADDR` |
| `elasticache_auth_token_secret_name` | the `ExternalSecret` `remoteRef.key` |
| `acm_certificate_arn` | `alb.ingress.kubernetes.io/certificate-arn` |
| `waf_web_acl_arn` | `alb.ingress.kubernetes.io/wafv2-acl-arn` |
| `alb_access_logs_bucket` | `access_logs.s3.bucket` in the ALB attributes |
| `otlp_endpoint` | `otel.endpoint` / `BFF_OBSERVABILITY__OTLP__ENDPOINT` |
| `vpc_cidr` | `networkPolicy.clusterCidr` |
| `adot_collector_role_arn` | the ADOT collector's ServiceAccount annotation |

One value is **not** created by Terraform: the HS256 JWT secret. The
`observability` module creates the Secrets Manager container; the value comes
from the identity provider, and a Terraform-invented one would verify nothing.

```bash
aws secretsmanager put-secret-value \
  --secret-id "$(terraform output -raw jwt_secret_name)" \
  --secret-string '{"secret":"<value from the IdP>"}'
```

---

## Destroy order

`terraform destroy` mostly works, with three known snags:

1. **The ALB.** Delete the `Ingress` first (`helm uninstall`, or
   `kubectl delete ingress -n bff ttl-aware-bff`) and wait for the controller
   to remove the load balancer. Otherwise the VPC destroy hangs on ENIs that
   Terraform does not know about.
2. **KMS keys** enter a 30-day pending-deletion window rather than disappearing.
   Their aliases are freed immediately, so a re-apply works, but the keys stay
   billable (1 USD/month each) until the window closes.
3. **The access-log bucket** has `force_destroy = false`, so a destroy fails
   while it still holds objects. That is intentional: it stops an accidental
   destroy from discarding an audit trail. Empty it deliberately when you mean
   to.

---

## Cost notes

Rough `us-east-1` on-demand figures for the **prod** defaults, per month. Treat
them as an order of magnitude, not a quote — use the AWS Pricing Calculator for
anything you have to commit to.

| Component | Config | ~USD/month |
|---|---|---|
| EKS control plane | one cluster | 73 |
| Node group | 3 × `m6i.large` on-demand | 210 |
| NAT gateways | 3 (one per AZ), hourly only | 100 |
| NAT data processing | 500 GB | 23 |
| ElastiCache | `cache.t4g.medium` × 2 (primary + replica) | 90 |
| ALB | one, low LCU usage | 25 |
| WAF | Web ACL + 4 rule groups + requests | 15 |
| CloudWatch logs | ~50 GB ingested, 30-day retention | 30 |
| S3 access logs | ~20 GB with lifecycle to IA | 2 |
| KMS | 3 customer-managed keys | 3 |
| Secrets Manager | 2 secrets | 1 |
| **Total** | | **≈ 570** |

### Where the money actually goes, and what to do about it

- **NAT gateways are the most common surprise.** 100 USD/month before a single
  byte moves, and 0.045 USD/GB after. The `vpc` module adds an **S3 gateway
  endpoint** (free) specifically because ECR image layers are S3 GETs, and on a
  cluster that pulls images all day that one endpoint is the largest single
  saving available. Adding interface endpoints for ECR API, STS and Secrets
  Manager saves more NAT data charges but costs ~7 USD/month each — worth it
  above roughly 150 GB/month of endpoint traffic.
- **`single_nat_gateway = true`** drops NAT cost to ~33 USD/month. The root
  module refuses it in prod: it makes one AZ's failure an egress outage for the
  entire VPC.
- **Spot nodes** cut the node-group line by 60–70%. The BFF is stateless with a
  PDB and a 45-second grace period, so it tolerates spot interruption well. Add
  a second node group with `capacity_type = "SPOT"` and let the topology spread
  constraints do the rest.
- **Log retention dominates CloudWatch cost**, not ingestion volume. Dropping
  `log_retention_days` from 30 to 14 roughly halves that line. VPC flow logs
  are the biggest single contributor — `enable_flow_logs = false` is defensible
  once the NetworkPolicy is stable, though they are exactly what you want the
  day a policy silently drops traffic.
- **ElastiCache scales with the working set.** A `cache.t4g.micro` is fine for
  dev at ~13 USD/month. Watch the `Evictions` alarm the module creates: rising
  evictions mean the working set no longer fits, and the cost shows up as
  upstream load rather than on the ElastiCache bill.

### Dev sizing

`environment = "dev"` with `single_nat_gateway = true`, a
`cache.t4g.micro`, `redis_replica_count = 0`, two AZs, two `t3.medium` nodes and
`enable_waf = false` lands around **150 USD/month**, most of it the EKS control
plane and the single NAT gateway.

---

## Security posture

Worth knowing before you review a plan:

- **IRSA, not node roles.** The BFF's role trusts exactly
  `system:serviceaccount:bff:ttl-aware-bff`, with `StringEquals` on both `:aud`
  and `:sub`. A wildcard in `:sub` would let any pod in the namespace assume
  the role, which is the most common IRSA mistake.
- **The BFF role carries an explicit deny** on `iam:*`, `sts:AssumeRole` and
  `organizations:*` that no attached policy can override.
- **ElastiCache is encrypted at rest and in transit**, with an AUTH token
  generated by Terraform and stored in Secrets Manager. `transit_encryption_enabled`
  means the BFF must connect with TLS — set `externalRedis.tls: true` in the
  Helm values (the `helm_values` output already does).
- **Kubernetes Secrets are envelope-encrypted** with a customer-managed KMS key.
- **The VPC's default security group is emptied**, so anything that lands in it
  by accident has no connectivity and the mistake surfaces immediately.
- **The VPC CNI add-on sets `ENABLE_NETWORK_POLICY=true`.** Without it, the
  NetworkPolicy objects in `deploy/k8s` are accepted and silently ignored —
  the classic false sense of security.
- **`cluster_endpoint_public_access_cidrs` defaults to `0.0.0.0/0`.** Narrow it.

---

## Conventions

- Every resource is named `${project}-${environment}-*`.
- Tags come from the provider's `default_tags`, so every taggable resource is
  tagged without each module remembering to.
- Modules take IDs and ARNs as inputs and never look each other's resources up
  by name. Composition is explicit in `main.tf`.
- `terraform fmt -recursive` is enforced in CI (`make tf-fmt`).
