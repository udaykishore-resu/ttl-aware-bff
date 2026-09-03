# TTL-Aware BFF — Security

JWT validation, tenant resolution, the RBAC matrix, tenant-isolation enforcement
points, transport security to both sources, secrets handling, input validation
and limits, rate limiting, output redaction, and audit logging.

Implementation: `internal/security` (JWT/JWKS, RBAC, tenant resolution,
redaction, audit) and `internal/api/middleware` (the enforcement chain).
Requirements from `spec/requirements.md`.

---

## 1. Threat model and posture

| Threat | Control | Req |
|---|---|---|
| Unauthenticated access to tenant data | bearer JWT required on every `/api/v1/*` route | REQ-SEC-001 |
| Forged or replayed token | signature + `iss`/`aud`/`exp`/`nbf` with bounded leeway; `alg` allow-list | REQ-SEC-002 |
| Algorithm confusion (`none`, HS/RS swap) | `alg` allow-list checked before key selection | REQ-SEC-002 |
| JWKS-refresh DoS via forged `kid` | refresh rate-limited + negative `kid` cache | REQ-SEC-003 |
| Cross-tenant read via a spoofed header | tenant comes from the **claim**; header must match | REQ-SEC-004 |
| Cross-tenant read via cache-key collision | `tenantId` is a structural key segment | REQ-CACHE-002, REQ-MT-005 |
| Cross-tenant read via a misbehaving source | every mapped record's `tenantId` is asserted | REQ-SEC-006 |
| Privilege escalation across endpoints | per-endpoint permission requirement | REQ-SEC-005 |
| Interception between BFF and sources | TLS 1.3 required; mTLS to the ODS | REQ-SEC-007 |
| Secrets in images, config or logs | env-var indirection + redaction pass | REQ-SEC-008, REQ-OBS-006 |
| Injection / traversal via path params | strict regex validation before any upstream call | REQ-SEC-009 |
| Resource exhaustion | body/header limits, rate limit, bulkhead | REQ-SEC-010, REQ-RES-007/008 |
| Sensitive data leakage to the UI | field-level output filtering by permission | REQ-SEC-011 |
| Internal detail leakage via errors | `Public` vs `Message` split on every error | REQ-SEC-013 |
| Undetected abuse | audit events for every authz-relevant decision | REQ-SEC-012 |

**Posture: fail closed.** Every ambiguous condition — unresolvable tenant,
unknown health, missing claim, unparseable token, unknown source record tenant —
resolves to denial, not to a default. The routing chain's first rule
(`guard.tenant_missing`) exists to make that structural rather than incidental.

---

## 2. JWT validation

### 2.1 Configuration

```yaml
security:
  jwt:
    issuer: https://idp.internal/
    audience: ttl-aware-bff
    jwks_url: https://idp.internal/.well-known/jwks.json
    hs256_secret_env: BFF_JWT_HS256_SECRET      # development only
    leeway: 30s
    required_claims: [sub, tenant_id, roles]
```

### 2.2 Validation sequence (REQ-SEC-002)

Order matters; each step is cheap relative to the next, and none may be skipped.

| # | Check | Failure |
|---|---|---|
| 1 | `Authorization: Bearer <token>` present and well-formed | `401 UNAUTHENTICATED` |
| 2 | Token parses as a compact JWS with three segments; size ≤ 8 KiB | `401` |
| 3 | Header `alg` ∈ allow-list (`RS256`, `RS384`, `RS512`, `ES256`, `ES384`; `HS256` **only** when `hs256_secret_env` is set and the build is not production) | `401` |
| 4 | `alg` is checked **before** key selection, so a token cannot select a key type its `alg` does not match | `401` |
| 5 | `kid` resolves to a key in the cached JWKS (see §3) | `401` |
| 6 | Signature verifies over the exact received `header.payload` bytes | `401` |
| 7 | `iss` equals `security.jwt.issuer` (exact string, no prefix matching) | `401` |
| 8 | `aud` contains `security.jwt.audience` (array or string form) | `401` |
| 9 | `exp` > `now − leeway` | `401`, detail `token expired` |
| 10 | `nbf` ≤ `now + leeway` when present | `401` |
| 11 | `iat` ≤ `now + leeway` when present; `iat` further than 24 h in the past is rejected | `401` |
| 12 | Every `required_claims` entry is present and non-empty | `401` |

**`alg: none` is rejected at step 3**, before any key lookup, and the allow-list
is consulted as a set membership test rather than by attempting each verifier in
turn. Attempting verifiers in turn is how algorithm-confusion bugs arise: an
HS256 token signed with the RSA *public key* as the HMAC secret verifies if the
implementation lets the token choose the algorithm after choosing the key.

**Leeway is symmetric** (`±leeway`) and applies to `exp`, `nbf` and `iat`
identically. An asymmetric leeway effectively extends token lifetime in one
direction only and is a common source of accidental long-lived acceptance.

**Verified by** `TestJWT_ValidationMatrix`, `TestJWT_RejectsAlgNone`,
`TestJWT_LeewayApplied` (`internal/security/jwt_test.go`).

### 2.3 HS256 in development

`HS256` is accepted only when `security.jwt.hs256_secret_env` names a set
environment variable **and** the binary is not built for production. A production
build with an HS256 configuration fails startup validation (REQ-CFG-003) rather
than silently accepting symmetric tokens.

---

## 3. JWKS handling and rotation

| Aspect | Behaviour | Req |
|---|---|---|
| Fetch | `GET security.jwt.jwks_url` over TLS at startup and on refresh | REQ-SEC-003 |
| Cache | in-memory key set with a soft TTL (default 15 min) and a hard expiry (default 24 h) | |
| Background refresh | at soft TTL, jittered, off the request path | |
| Unknown `kid` | triggers **one** refresh, then re-attempts verification | |
| Refresh rate limit | at most one refresh per 60 s regardless of unknown-`kid` volume | |
| Negative `kid` cache | a `kid` that a fresh JWKS does not contain is remembered for 5 min and rejected without a refresh | |
| Fetch failure | keep the last good key set until hard expiry; log rate-limited; audit event | |
| Past hard expiry with no successful fetch | all tokens rejected `401`; `/readyz` stays `200` (auth is not a dependency-readiness concern) | |
| Rotation | overlapping `kid`s in the JWKS make rotation seamless — old and new keys are both present during the overlap window | |

**Why the rate limit and negative cache both exist.** An attacker sending tokens
with random `kid` values would otherwise force one JWKS fetch per request,
turning the BFF into a DoS amplifier against the identity provider. The rate
limit bounds outbound fetches; the negative cache bounds the work done per
request before rejection.

**Verified by** `TestJWKS_RotationOnUnknownKid`, `TestJWKS_RefreshRateLimited`.

---

## 4. Tenant resolution

### 4.1 Rules (REQ-SEC-004)

1. The tenant is the value of the configured claim (`tenant_id` by default). The
   claim is **authoritative**.
2. `X-Tenant-ID`, when present, must equal the claim. Mismatch → `403
   TENANT_MISMATCH` + audit event.
3. `X-Tenant-ID` alone never establishes a tenant.
4. An empty or absent claim means no tenant can be resolved. The request fails
   closed via routing rule `guard.tenant_missing`.
5. The resolved tenant is placed in the request context by `pkg/correlation` and
   is a **mandatory argument** at every enforcement point in §5.

### 4.2 Why the header exists at all

`X-Tenant-ID` is accepted purely as a client-side assertion for debugging and for
multi-tenant consoles that want an explicit confirmation that the session they
believe they are in matches the token they hold. It can only ever cause a
rejection, never a grant. A design where the header could select a tenant would
make every downstream control depend on the client's honesty.

**Verified by** `TestTenant_ClaimIsAuthoritative`,
`TestTenant_HeaderMismatchRejected`.

---

## 5. Tenant isolation enforcement points

Isolation is enforced at five independent points. Any single one failing must not
produce a cross-tenant read (REQ-MT-001, REQ-EDGE-016).

| # | Point | Mechanism | Failure mode it prevents | Req |
|---|---|---|---|---|
| 1 | **Request entry** | tenant resolved from the claim; no tenant ⇒ rule `guard.tenant_missing` ⇒ `400` | processing a request with no isolation context | REQ-MT-001 |
| 2 | **Cache key** | `{key_prefix}:v1:{tenantId}:{requestType}:{resourceId}[:{executionId}][:{paramHash}]` — `tenantId` is a **structural segment**, never inside `paramHash` | a hash collision or a shared resource id serving tenant A's entry to tenant B | REQ-CACHE-002, REQ-MT-005 |
| 3 | **Adapter call** | ODS `RequestContext.tenant_id`; EDS `tenantId` query parameter **and** `X-Tenant-ID` header | asking a source for data without scoping | REQ-DS-003, REQ-DS-004 |
| 4 | **Mapper assertion** | every mapped record's `tenantId` must equal the authenticated tenant, else `TENANT_MISMATCH` + audit | a compromised or buggy source returning foreign data | REQ-SEC-006 |
| 5 | **Telemetry** | `tenant_id` attribute on every metric, span and log line | inability to attribute or investigate an isolation failure | REQ-MT-004, REQ-OBS-005 |

Point 4 is the one most systems omit. The sources enforce isolation themselves
(stated in the proto's comment on `RequestContext`), but the BFF does not rely on
that — a source bug or a compromised source would otherwise flow straight to the
client with a `200`. Asserting on the way out converts a silent data breach into
a logged, alerted `403`.

### 5.1 Per-tenant resource isolation

| Resource | Partitioning | Req |
|---|---|---|
| Rate limit | independent token bucket per tenant when `rate_limit.per_tenant: true` | REQ-MT-003 |
| Cache | disjoint key space; L1 eviction is shared (memory pressure is not an isolation boundary) | REQ-CACHE-002 |
| Bulkhead | shared per source by default; may be partitioned per tenant class | REQ-RES-007 |
| Config | tenant overlay deep-merged over defaults, validated with the same schema | REQ-MT-002 |
| Metrics | `tenant_id` attribute, collapsing to `_other` beyond the cardinality cap | REQ-MT-004 |

**Verified by** `TestTenant_EnforcementPoints`, `TestCacheKey_TenantSeparation`,
`TestCache_NoCrossTenantRead`, `TestTenant_ResponseTenantAsserted`,
`TestEdge016_TenantIsolationCached`, `TestEdge016_TenantIsolationUncached`,
`TestEdge016_SourceReturnsForeignTenant`.

---

## 6. RBAC

### 6.1 Roles and permissions

```yaml
security:
  rbac:
    roles:
      resource.viewer:  [resources:read]
      resource.operator:[resources:read, resources:read_config, executions:read]
      resource.admin:   [resources:read, resources:read_config, executions:read,
                         resources:read_sensitive]
      platform.support: [resources:read, executions:read, resources:read_sensitive]
```

Roles come from the `roles` claim (array of strings). Unknown role names are
ignored, not rejected — an identity provider adding roles for other services must
not break this one. A principal with no recognised role has an empty permission
set and is denied everywhere.

### 6.2 Endpoint → required permission matrix (REQ-SEC-005)

| Endpoint | Permission | `resource.viewer` | `resource.operator` | `resource.admin` | `platform.support` |
|---|---|---|---|---|---|
| `GET /resources/{id}` | `resources:read` | allow | allow | allow | allow |
| `GET /resources/{id}/status` | `resources:read` | allow | allow | allow | allow |
| `GET /resources/{id}/configuration` | `resources:read_config` | **deny** | allow | allow | **deny** |
| `GET /resources/{id}/executions` | `executions:read` | **deny** | allow | allow | allow |
| `GET /resources/{id}/executions/{eid}` | `executions:read` | **deny** | allow | allow | allow |
| `GET /resources/{id}/details` | `resources:read` **and** `executions:read`¹ | partial² | allow | allow | allow |

¹ `/details` requires `resources:read`; `executions:read` is required only to
receive the execution field groups.
² Without `executions:read` the request succeeds with `200` and the execution
groups **omitted**, `partial: true`, and warning `PARTIAL_DATA`. It is
not a `403`: the caller is entitled to the resource, just not to the execution
context. Returning `403` for the whole endpoint would make an entitled read fail.

### 6.3 Field-level permissions

| Field | Permission | Without it |
|---|---|---|
| `owner.email` | `resources:read_sensitive` | omitted |
| `configuration[k]` where `k` matches a secret pattern | `resources:read_sensitive` | value replaced with `"[REDACTED]"`, key retained |
| `execution.audit[]` | `resources:read_sensitive` | omitted |
| `execution.error.message` | `executions:read` | omitted |

Secret-key patterns default to
`(?i)(password|secret|token|api[_-]?key|private[_-]?key|credential)` and are
per-tenant overridable.

**Redaction, not omission, for configuration values**: the key is retained so the
caller can see that a setting exists without seeing its value. Omitting the key
entirely would make a redacted config indistinguishable from an unset one.

**Verified by** `TestRBAC_EndpointPermissionMatrix`,
`TestRedaction_OutputFiltering`.

---

## 7. Transport security

### 7.1 Inbound

TLS is terminated at the ALB / ingress. The BFF assumes it is behind a trusted
terminator and does not itself serve TLS in the reference deployment. Required
inbound properties, enforced at the ingress and asserted in deployment tests:
TLS ≥ 1.2 (1.3 preferred), HSTS, no client-supplied `X-Forwarded-*` trusted
beyond the terminator's own.

### 7.2 Outbound to the sources (REQ-SEC-007)

| Property | ODS (gRPC) | EDS (REST) |
|---|---|---|
| TLS | required outside local dev | required outside local dev |
| Minimum version | 1.3 | 1.3 |
| Server verification | CA bundle + `server_name` pinned via config | CA bundle + `server_name` |
| Client authentication | **mTLS** — `cert_file` / `key_file` | service credential in `Authorization` |
| Certificate rotation | file watch on cert/key; reload without restart | same, plus credential from env |
| Insecure mode | dev only; a production build refuses to start with `tls.enabled: false` | same |

`InsecureSkipVerify` is not reachable from configuration in any build. There is no
environment variable that disables verification (REQ-SEC-007) — the one-off debug
convenience it provides is not worth the production accident it eventually
causes.

**Why mTLS to the ODS but not the EDS.** The ODS is a low-level internal service
that exposes per-tenant records with no per-caller authorization of its own
beyond the `RequestContext`; mutual authentication is the mechanism that
establishes *which service* is asking. The EDS has its own service-credential
model and authorization layer, so a bearer credential is the contract it offers.
Both are configurable; the defaults reflect the sources' own designs.

**End-user tokens are never forwarded** to either source. Forwarding would make
the sources' authorization surfaces depend on the BFF's token issuer and would
spread user credentials into systems that do not need them.

**Verified by** `TestTLS_ConfigEnforced`, `TestTLS_InsecureRejectedInProd`.

---

## 8. Secrets management

| Secret | Source | Never |
|---|---|---|
| Redis password | env var named by `cache.redis.password_env` | in `configs/bff.yaml`, in the image, in logs |
| JWT HS256 secret (dev) | env var named by `security.jwt.hs256_secret_env` | in config files |
| ODS client key | mounted file (`tls.key_file`), Kubernetes Secret backed by IRSA / external secret store | baked into the image |
| EDS service credential | env var | in config files |

Rules (REQ-SEC-008):

1. Config files hold **names of environment variables**, never values. This makes
   a leaked config file harmless and makes secret rotation a pod restart rather
   than a config change.
2. Secret values never appear in logs, metrics, traces, error bodies, or the
   `/admin/config` introspection endpoint, which returns the **redacted**
   snapshot (REQ-CFG-006).
3. Files mounted from Secrets are `0400`, owned by the non-root runtime user.
4. The container runs as non-root with a read-only root filesystem; secret mounts
   are the only writable-adjacent paths and they are read-only too.

**Verified by** `TestConfig_SecretsFromEnvOnly`,
`TestAdmin_ConfigEndpointRedacted`, `TestLogging_RedactsSecrets`.

---

## 9. Input validation and limits

### 9.1 Validation (REQ-SEC-009)

All validation happens **before** any upstream call, cache lookup or source
dispatch. A malformed request must cost nothing beyond parsing.

| Input | Rule | Failure |
|---|---|---|
| `resourceId` | `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` | `400 INVALID_REQUEST` |
| `executionId` | same | `400` |
| `limit` | integer, 1..200 — **never clamped** (REQ-API-014) | `400` |
| `cursor` | base64url, ≤ 512 B | `400` |
| `X-Correlation-ID` | `^[A-Za-z0-9._-]{1,128}$` | replaced with a generated UUIDv4, not rejected |
| `X-Tenant-ID` | `^[a-z0-9][a-z0-9-]{0,63}$` and equal to the claim | `403 TENANT_MISMATCH` |
| `traceparent` | W3C format | ignored if malformed, new trace started |
| `Cache-Control` | `no-cache` recognised; other values ignored | — |
| HTTP method | GET only on defined paths | `405` + `Allow` |
| Path | must match a registered route | `404` |

The `resourceId` pattern excludes `/`, `..`, `%`, whitespace and control
characters, which neutralises path traversal into the EDS's URL templates and
prevents header/query injection into either adapter. It is a positive allow-list,
not a denylist of dangerous characters.

### 9.2 Size limits (REQ-SEC-010)

| Limit | Value | Enforcement |
|---|---|---|
| Request body | `server.max_body_bytes` (default 64 KiB) | `http.MaxBytesReader` ⇒ `413 REQUEST_TOO_LARGE` |
| Request header total | 32 KiB | `http.Server.MaxHeaderBytes` |
| URL length | 8 KiB | server default |
| JWT | 8 KiB | rejected at parse |
| Upstream response body | 4 MiB per source | `io.LimitReader` / `MaxCallRecvMsgSize` ⇒ `UPSTREAM_INVALID_RESPONSE` (REQ-DS-009) |
| Response body | unbounded but bounded in practice by `limit` ≤ 200 | |

All six endpoints are `GET` and expect no body; the body limit exists so that a
request with an unexpected large body is rejected cheaply rather than read.

**Verified by** `TestValidation_PathParams`,
`TestValidation_RejectsBeforeUpstream`, `TestValidation_BodySizeLimit`,
`TestAdapters_ResponseSizeBounded`.

---

## 10. Rate limiting

```yaml
security:
  rate_limit:
    rps: 200
    burst: 400
    per_tenant: true
```

| Aspect | Behaviour | Req |
|---|---|---|
| Algorithm | token bucket (`golang.org/x/time/rate`) | REQ-RES-008 |
| Key | tenant when `per_tenant: true`, else global | REQ-MT-003 |
| Position in the chain | **after** authentication and tenant resolution, **before** validation and all I/O | |
| Rejection | `429 RATE_LIMITED` with `Retry-After` | |
| Bucket lifecycle | created lazily per tenant, evicted after 10 min idle to bound memory | |
| Unauthenticated floods | bounded by a separate global pre-auth limiter, so JWT verification cost is capped | |
| Admin listener | not rate limited | REQ-API-002 |

The limiter sits after tenant resolution so that limits are attributable and one
tenant cannot exhaust another's budget. It sits before validation and cache
lookup so that a rejected request costs only authentication.

Rate limiting is the **inbound** control; the bulkhead (REQ-RES-007) is the
**outbound** one. They protect different things: the limiter protects the BFF and
enforces fairness between tenants; the bulkhead protects each upstream and
enforces fairness between sources. Both are required — either alone leaves a gap.

**Verified by** `TestRateLimit_PerTenantBucket`,
`TestRateLimit_RetryAfterHeader`.

---

## 11. Output filtering and redaction

### 11.1 Response filtering (REQ-SEC-011)

Applied in `internal/api/response` after precedence resolution and before
encoding, so it operates on the final canonical values regardless of which source
supplied them.

| Rule | Applies to |
|---|---|
| Omit fields the caller lacks permission for (§6.3) | `owner.email`, `execution.audit`, `execution.error.message` |
| Replace secret-pattern configuration values with `"[REDACTED]"`, retaining the key | `resource.configuration` |
| Never echo source-native field names | enforced by `TestEnvelope_NoSourceNativeFields` (REQ-API-004) |
| Truncate free-text source fields | `error.message`, `audit.detail` at 2 KiB |
| Warnings carry no tenant-identifying data | `meta.warnings[].message` (REQ-API-015) |

### 11.2 Error body filtering (REQ-SEC-013)

`errs.Error` carries `Public` and `Message` as separate fields. The encoder reads
`Public`; the logger reads `Message`. Consequently the following never reach a
client: stack traces, upstream hostnames and addresses, raw upstream payloads,
gRPC status details, SQL or query fragments, JWT contents, configuration values,
internal file paths.

The client receives `code`, `title`, a sanitised `detail`, and `correlationId` —
which is the key an operator uses to retrieve everything in the logs.

### 11.3 Log and telemetry redaction (REQ-OBS-006)

Covered in `spec/observability.md` §5.3. Summary: `Authorization` values, token
contents, any config value whose key matches
`password|secret|token|key`, and `payload_excerpt` content all pass through the
redaction pass. `cache.key_hash` is used on spans rather than the raw key, since
the raw key contains `tenantId` and `resourceId` and traces are shared more
widely than logs.

---

## 12. Security headers

| Header | Value | Req |
|---|---|---|
| `X-Content-Type-Options` | `nosniff` | REQ-SEC-014 |
| `Cache-Control` | `no-store` on every authenticated response | REQ-SEC-014 |
| `Pragma` | `no-cache` | |
| `Referrer-Policy` | `no-referrer` | |
| `Server` | **omitted** | |
| `X-Frame-Options` | `DENY` | |
| `Strict-Transport-Security` | set at the ingress | |

`Cache-Control: no-store` matters specifically here: responses are tenant-scoped
and carry freshness metadata whose validity is time-bound. An intermediary cache
holding one would serve another tenant's data or would serve data whose
`meta.freshness` has become a lie.

**Verified by** `TestHeaders_SecurityDefaults`.

---

## 13. Audit logging

### 13.1 Events (REQ-SEC-012)

| Event | Trigger | Severity |
|---|---|---|
| `auth.failure` | JWT missing, malformed, expired, bad signature, bad `iss`/`aud`, missing required claim | warn |
| `authz.denial` | RBAC permission check failed | warn |
| `tenant.mismatch` | `X-Tenant-ID` ≠ claim | error |
| `tenant.cross_tenant_record_rejected` | a source returned a record whose `tenantId` ≠ authenticated tenant | **error — security incident** |
| `ratelimit.rejected` | token bucket exhausted | warn |
| `config.reloaded` | successful hot reload | info |
| `config.reload_rejected` | validation failure on reload | error |
| `jwks.refresh_failed` | JWKS fetch failed | warn |
| `jwks.rotated` | key set changed | info |
| `tls.cert_reloaded` | client certificate rotated | info |
| `security.field_redacted` | sensitive field omitted or redacted (sampled, not per-field) | debug |

### 13.2 Record shape

| Field | Content |
|---|---|
| `event` | the event name above |
| `who` | JWT `sub`; `unknown` when authentication itself failed |
| `what` | HTTP method + route **template** (never the filled path) |
| `when` | RFC 3339 UTC, ms |
| `tenant_id` | resolved tenant, or `unresolved` |
| `correlation_id` | joins to the request line and to both sources |
| `trace_id` | joins to the trace |
| `outcome` | `denied` \| `allowed` \| `failed` |
| `reason` | enumerated code, never free text derived from input |
| `source_ip` | client address from the trusted terminator's `X-Forwarded-For` |
| `details` | bounded, redacted, event-specific (e.g. `required_permission`, `claim_tenant`, `header_tenant`) |

Audit records go to the same structured log stream with `audit: true`, so they
inherit redaction and correlation. `reason` is an enumerated code rather than a
formatted message so that audit queries are stable and so that attacker-supplied
input cannot be injected into an audit record.

**Verified by** `TestAudit_EventsEmitted`.

---

## 14. Middleware order

Order is load-bearing. Each layer must be able to assume the previous one ran.

```
1.  recovery            panic → 500, never crash the process        REQ-RES-012
2.  request limits      body / header size → 413                    REQ-SEC-010
3.  correlation         accept/validate/generate id; open root span REQ-API-010
4.  tracing             otelhttp, W3C propagation                   REQ-API-011
5.  security headers    set response defaults                       REQ-SEC-014
6.  pre-auth limiter    global bucket, caps JWT verification cost   REQ-RES-008
7.  authentication      JWT validate → 401                          REQ-SEC-001/002
8.  tenant resolution   claim vs header → 403 TENANT_MISMATCH       REQ-SEC-004
9.  rate limit          per-tenant bucket → 429                     REQ-RES-008
10. authorization       RBAC permission → 403 FORBIDDEN             REQ-SEC-005
11. validation          path/query params → 400                     REQ-SEC-009
12. metrics/logging     record outcome on the way out               REQ-OBS-001/005
13. handler             the request lifecycle                       —
```

Justification for the contentious placements:

- **Recovery outermost** so a panic anywhere below, including in another
  middleware, becomes a `500` rather than a crashed process.
- **Correlation before authentication** so that a `401` still carries a
  correlation id and is traceable — otherwise auth failures are the least
  debuggable class of request.
- **Pre-auth limiter before authentication** so an unauthenticated flood cannot
  force unbounded signature verifications, which are the most expensive
  per-request cryptographic work the service does.
- **Tenant resolution before the per-tenant rate limit**, because the limit needs
  a key.
- **Authorization before validation**: an unauthorized caller should not learn
  which resource ids are well-formed. The information leak is small but free to
  avoid.
- **Validation before the handler**, so no invalid input reaches cache lookup or
  a source (REQ-SEC-009).

**Verified by** `TestMiddleware_Order`, `TestAuth_RequiredOnDataPlane`,
`TestAdmin_NotServedOnDataPlane`.

---

## 15. Test obligations

| Obligation | Test | Req |
|---|---|---|
| Auth required on every data-plane route | `TestAuth_RequiredOnDataPlane` | REQ-SEC-001 |
| Full JWT validation matrix | `TestJWT_ValidationMatrix` | REQ-SEC-002 |
| `alg: none` and algorithm confusion rejected | `TestJWT_RejectsAlgNone` | REQ-SEC-002 |
| Symmetric leeway | `TestJWT_LeewayApplied` | REQ-SEC-002 |
| JWKS rotation on unknown `kid` | `TestJWKS_RotationOnUnknownKid` | REQ-SEC-003 |
| JWKS refresh rate-limited | `TestJWKS_RefreshRateLimited` | REQ-SEC-003 |
| Claim is authoritative for tenant | `TestTenant_ClaimIsAuthoritative` | REQ-SEC-004 |
| Header mismatch rejected | `TestTenant_HeaderMismatchRejected` | REQ-SEC-004 |
| All five enforcement points exercised | `TestTenant_EnforcementPoints` | REQ-SEC-004 |
| Foreign-tenant source record rejected | `TestTenant_ResponseTenantAsserted`, `TestEdge016_SourceReturnsForeignTenant` | REQ-SEC-006 |
| Cache key tenant separation | `TestCacheKey_TenantSeparation`, `TestCache_NoCrossTenantRead` | REQ-MT-005 |
| RBAC matrix exact | `TestRBAC_EndpointPermissionMatrix` | REQ-SEC-005 |
| Field-level filtering | `TestRedaction_OutputFiltering` | REQ-SEC-011 |
| TLS enforced, insecure refused in prod | `TestTLS_ConfigEnforced`, `TestTLS_InsecureRejectedInProd` | REQ-SEC-007 |
| Secrets only from env | `TestConfig_SecretsFromEnvOnly` | REQ-SEC-008 |
| Admin config endpoint redacted | `TestAdmin_ConfigEndpointRedacted` | REQ-CFG-006 |
| Input validation before upstream | `TestValidation_RejectsBeforeUpstream` | REQ-SEC-009 |
| Body size limit | `TestValidation_BodySizeLimit` | REQ-SEC-010 |
| Per-tenant rate limiting | `TestRateLimit_PerTenantBucket` | REQ-RES-008 |
| No internal leakage in errors | `TestErrors_NoInternalLeakage` | REQ-SEC-013 |
| Secrets never logged | `TestLogging_RedactsSecrets` | REQ-OBS-006 |
| Security headers present | `TestHeaders_SecurityDefaults` | REQ-SEC-014 |
| Audit events emitted | `TestAudit_EventsEmitted` | REQ-SEC-012 |
| Middleware order | `TestMiddleware_Order` | §14 |
