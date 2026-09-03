// Package security implements authentication, authorisation and tenant
// resolution.
//
// The invariant the whole multi-tenant design rests on: the tenant is derived
// from the verified token and from nowhere else. A header may *assert* a
// tenant, and the service will check that assertion against the token, but a
// header can never *establish* one.
//
// Traceability: REQ-SEC-001..REQ-SEC-012, REQ-MT-001, REQ-MT-002.
package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// Permissions granted by roles and required by endpoints.
const (
	PermResourceRead    = "resource.read"
	PermExecutionRead   = "execution.read"
	PermExecutionAudit  = "execution.audit.read"
	PermConfigRead      = "config.read"
	PermAdminInvalidate = "cache.invalidate"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject  string
	TenantID string
	Roles    []string
	// Permissions is the flattened set granted by the caller's roles.
	Permissions map[string]struct{}
	ExpiresAt   time.Time
	// TokenID is the jti claim, used for audit logging.
	TokenID string
}

// Can reports whether the principal holds a permission.
func (p *Principal) Can(perm string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Permissions[perm]
	return ok
}

// Authenticator verifies bearer tokens.
type Authenticator struct {
	cfg    config.JWTConfig
	rbac   config.RBACConfig
	keys   KeyProvider
	parser *jwt.Parser
	// allowInsecure disables verification entirely for local development. It
	// is refused by config validation outside local and test environments.
	allowInsecure bool
}

// KeyProvider supplies the verification key for a token.
type KeyProvider interface {
	// Key returns the verification key for a token's header.
	Key(t *jwt.Token) (any, error)
	// Algorithms lists the signing algorithms this provider accepts. Pinning
	// them is what prevents the "alg: none" and HS/RS confusion attacks.
	Algorithms() []string
}

// NewAuthenticator builds the authenticator.
func NewAuthenticator(cfg config.SecurityConfig, keys KeyProvider) *Authenticator {
	a := &Authenticator{
		cfg:           cfg.JWT,
		rbac:          cfg.RBAC,
		keys:          keys,
		allowInsecure: cfg.AllowInsecureNoAuth,
	}
	opts := []jwt.ParserOption{
		jwt.WithLeeway(cfg.JWT.Leeway.D()),
		jwt.WithExpirationRequired(),
	}
	if cfg.JWT.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.JWT.Issuer))
	}
	if cfg.JWT.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.JWT.Audience))
	}
	if keys != nil {
		opts = append(opts, jwt.WithValidMethods(keys.Algorithms()))
	}
	a.parser = jwt.NewParser(opts...)
	return a
}

// Authenticate verifies the Authorization header and returns the principal.
func (a *Authenticator) Authenticate(_ context.Context, header http.Header) (*Principal, error) {
	if a.allowInsecure {
		return a.insecurePrincipal(header), nil
	}

	raw, err := bearerToken(header)
	if err != nil {
		return nil, err
	}

	claims := jwt.MapClaims{}
	tok, err := a.parser.ParseWithClaims(raw, claims, a.keys.Key)
	if err != nil {
		// The reason is logged, never returned: telling a caller *why* a token
		// failed is a gift to anyone probing the service.
		return nil, errs.Wrap(errs.CodeUnauthenticated, "the supplied credentials are not valid", err).
			WithOp("security.parse")
	}
	if !tok.Valid {
		return nil, errs.ErrUnauthenticated.WithOp("security.invalid")
	}

	for _, required := range a.cfg.RequiredClaims {
		if _, ok := claims[required]; !ok {
			return nil, errs.New(errs.CodeUnauthenticated, "the supplied credentials are not valid").
				WithOp("security.claims").WithDetail("missing_claim", required)
		}
	}

	tenant := stringClaim(claims, a.cfg.TenantClaim)
	if tenant == "" {
		return nil, errs.New(errs.CodeUnauthenticated, "the supplied credentials carry no tenant").
			WithOp("security.tenant")
	}
	sub := stringClaim(claims, "sub")
	roles := stringsClaim(claims, a.cfg.RolesClaim)

	exp, _ := claims.GetExpirationTime()
	p := &Principal{
		Subject:     sub,
		TenantID:    tenant,
		Roles:       roles,
		Permissions: a.permissionsFor(roles),
		TokenID:     stringClaim(claims, "jti"),
	}
	if exp != nil {
		p.ExpiresAt = exp.Time
	}
	return p, nil
}

// insecurePrincipal builds a principal from headers, for local development
// only. It grants every configured role so that the compose stack is usable
// without an identity provider.
func (a *Authenticator) insecurePrincipal(header http.Header) *Principal {
	tenant := header.Get("X-Tenant-ID")
	if tenant == "" {
		tenant = "local"
	}
	roles := make([]string, 0, len(a.rbac.Roles))
	for r := range a.rbac.Roles {
		roles = append(roles, r)
	}
	return &Principal{
		Subject:     "local-dev",
		TenantID:    tenant,
		Roles:       roles,
		Permissions: a.permissionsFor(roles),
	}
}

func (a *Authenticator) permissionsFor(roles []string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range roles {
		for _, p := range a.rbac.Roles[r] {
			out[p] = struct{}{}
		}
	}
	return out
}

// ResolveTenant reconciles the token's tenant with an optional header
// assertion. A mismatch is refused rather than resolved in either direction:
// a client that believes it is acting for another tenant has either a bug or
// bad intent, and both deserve a 403 (REQ-MT-001).
func ResolveTenant(p *Principal, headerTenant string) (string, error) {
	if p == nil || p.TenantID == "" {
		return "", errs.ErrUnauthenticated.WithOp("security.resolve_tenant")
	}
	headerTenant = strings.TrimSpace(headerTenant)
	if headerTenant != "" && headerTenant != p.TenantID {
		return "", errs.ErrTenantMismatch.WithOp("security.resolve_tenant")
	}
	return p.TenantID, nil
}

// Authorize checks a permission and returns a taxonomy error if absent.
func Authorize(p *Principal, perm string, defaultDeny bool) error {
	if p.Can(perm) {
		return nil
	}
	if !defaultDeny {
		return nil
	}
	return errs.New(errs.CodeForbidden, "the caller is not permitted to perform this operation").
		WithOp("security.authorize").WithDetail("permission", perm)
}

func bearerToken(h http.Header) (string, error) {
	v := h.Get("Authorization")
	if v == "" {
		return "", errs.New(errs.CodeUnauthenticated, "an Authorization header is required").
			WithOp("security.header")
	}
	const prefix = "Bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", errs.New(errs.CodeUnauthenticated, "the Authorization header must use the Bearer scheme").
			WithOp("security.header")
	}
	tok := strings.TrimSpace(v[len(prefix):])
	if tok == "" {
		return "", errs.New(errs.CodeUnauthenticated, "the Authorization header carries no token").
			WithOp("security.header")
	}
	return tok, nil
}

func stringClaim(c jwt.MapClaims, key string) string {
	if key == "" {
		return ""
	}
	if v, ok := c[key].(string); ok {
		return v
	}
	return ""
}

func stringsClaim(c jwt.MapClaims, key string) []string {
	if key == "" {
		return nil
	}
	switch v := c[key].(type) {
	case []string:
		return v
	case string:
		// Space- or comma-separated is common in OAuth deployments.
		f := strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
		return f
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Key providers
// ---------------------------------------------------------------------------

// HMACKeyProvider verifies HS256 tokens from a shared secret. It exists for
// self-contained deployments and tests; production uses JWKS.
type HMACKeyProvider struct{ secret []byte }

// NewHMACKeyProvider builds an HS256 provider.
func NewHMACKeyProvider(secret string) (*HMACKeyProvider, error) {
	if len(secret) < 32 {
		return nil, errors.New("security: HS256 secret must be at least 32 bytes")
	}
	return &HMACKeyProvider{secret: []byte(secret)}, nil
}

// Key implements KeyProvider.
func (p *HMACKeyProvider) Key(*jwt.Token) (any, error) { return p.secret, nil }

// Algorithms implements KeyProvider.
func (p *HMACKeyProvider) Algorithms() []string { return []string{"HS256"} }

// JWKSKeyProvider fetches and caches an RFC 7517 key set.
//
// Rotation is handled by re-fetching when an unknown kid arrives, rate-limited
// so that a token with a bogus kid cannot be used to hammer the identity
// provider (REQ-SEC-003).
type JWKSKeyProvider struct {
	url    string
	client *http.Client
	ttl    time.Duration

	mu          sync.RWMutex
	keys        map[string]any
	fetchedAt   time.Time
	lastAttempt time.Time
}

// NewJWKSKeyProvider builds a JWKS-backed provider.
func NewJWKSKeyProvider(url string, ttl time.Duration, client *http.Client) *JWKSKeyProvider {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &JWKSKeyProvider{url: url, client: client, ttl: ttl, keys: map[string]any{}}
}

// Algorithms implements KeyProvider.
func (p *JWKSKeyProvider) Algorithms() []string { return []string{"RS256", "RS384", "RS512", "ES256"} }

// Key implements KeyProvider.
func (p *JWKSKeyProvider) Key(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("security: token has no key id")
	}

	p.mu.RLock()
	k, ok := p.keys[kid]
	stale := time.Since(p.fetchedAt) > p.ttl
	p.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}

	if err := p.refresh(); err != nil && !ok {
		return nil, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if k, ok := p.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("security: no key with id %q", kid)
}

// minRefreshInterval throttles refetches triggered by unknown key ids.
const minRefreshInterval = 30 * time.Second

func (p *JWKSKeyProvider) refresh() error {
	p.mu.Lock()
	if time.Since(p.lastAttempt) < minRefreshInterval {
		p.mu.Unlock()
		return errors.New("security: JWKS refresh throttled")
	}
	p.lastAttempt = time.Now()
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return fmt.Errorf("security: build JWKS request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("security: fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("security: JWKS endpoint returned %d", resp.StatusCode)
	}

	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("security: decode JWKS: %w", err)
	}

	parsed := make(map[string]any, len(set.Keys))
	for _, raw := range set.Keys {
		kid, key, err := parseJWK(raw)
		if err != nil || kid == "" {
			continue
		}
		parsed[kid] = key
	}
	if len(parsed) == 0 {
		return errors.New("security: JWKS contained no usable keys")
	}

	p.mu.Lock()
	p.keys = parsed
	p.fetchedAt = time.Now()
	p.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Context plumbing
// ---------------------------------------------------------------------------

type ctxKey int

const keyPrincipal ctxKey = 0

// WithPrincipal stores the authenticated principal in the context. Handlers
// read it for authorisation decisions that depend on more than the tenant.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, keyPrincipal, p)
}

// PrincipalFrom returns the authenticated principal, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	if p, ok := ctx.Value(keyPrincipal).(*Principal); ok {
		return p
	}
	return nil
}
