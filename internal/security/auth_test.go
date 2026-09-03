package security

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

const (
	testSecret   = "an-hs256-secret-of-at-least-32-bytes"
	testIssuer   = "https://issuer.test.invalid"
	testAudience = "ttl-aware-bff"
)

// securityCfg is the configuration every auth test starts from: HS256, an
// issuer and audience to pin, and the default RBAC role table.
func securityCfg() config.SecurityConfig {
	def := config.Default().Security
	def.JWT.Issuer = testIssuer
	def.JWT.Audience = testAudience
	def.JWT.Leeway = config.Duration(30 * time.Second)
	def.JWT.TenantClaim = "tenant_id"
	def.JWT.RolesClaim = "roles"
	def.JWT.RequiredClaims = []string{"sub", "tenant_id"}
	return def
}

func newTestAuthenticator(t *testing.T, mutate func(*config.SecurityConfig)) *Authenticator {
	t.Helper()
	cfg := securityCfg()
	if mutate != nil {
		mutate(&cfg)
	}
	keys, err := NewHMACKeyProvider(testSecret)
	testutil.NoError(t, err, "building the HMAC key provider")
	return NewAuthenticator(cfg, keys)
}

// claimSet is the shape of a well-formed token, before a test breaks one field.
func claimSet() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":       testIssuer,
		"aud":       testAudience,
		"sub":       "user-1",
		"tenant_id": "tenant-a",
		"roles":     []string{"bff.viewer"},
		"jti":       "token-1",
		"iat":       now.Add(-time.Minute).Unix(),
		"exp":       now.Add(time.Hour).Unix(),
	}
}

// signHS256 mints a token with the shared secret.
func signHS256(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	testutil.NoError(t, err, "signing an HS256 token")
	return raw
}

// bearer builds a header carrying the token.
func bearer(raw string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+raw)
	return h
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestAuthenticate_ValidToken verifies REQ-SEC-001 and REQ-SEC-002: a
// well-formed token yields a principal carrying the subject, the tenant, the
// roles and the permissions those roles flatten to.
func TestAuthenticate_ValidToken(t *testing.T) {
	t.Parallel()

	a := newTestAuthenticator(t, nil)
	claims := claimSet()
	claims["roles"] = []string{"bff.operator"}

	p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
	testutil.NoError(t, err, "a valid token must authenticate")

	testutil.Equal(t, p.Subject, "user-1", "sub -> Subject")
	testutil.Equal(t, p.TenantID, "tenant-a", "the tenant comes from the verified token")
	testutil.Equal(t, p.Roles, []string{"bff.operator"}, "roles")
	testutil.Equal(t, p.TokenID, "token-1", "jti -> TokenID, for audit logging")
	testutil.False(t, p.ExpiresAt.IsZero(), "the expiry is carried")

	// bff.operator grants read plus audit, but not config.read.
	testutil.True(t, p.Can(PermResourceRead), "resource.read is granted")
	testutil.True(t, p.Can(PermExecutionRead), "execution.read is granted")
	testutil.True(t, p.Can(PermExecutionAudit), "execution.audit.read is granted")
	testutil.False(t, p.Can(PermConfigRead), "config.read is not granted by this role")
	testutil.Equal(t, len(p.Permissions), 3, "exactly the role's permissions")
}

// TestAuthenticate_PermissionsFlattenAcrossRoles verifies REQ-SEC-005: several
// roles union their permissions, and an unknown role contributes nothing rather
// than failing the request.
func TestAuthenticate_PermissionsFlattenAcrossRoles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		roles []string
		want  []string
		deny  []string
	}{
		{
			name:  "single role",
			roles: []string{"bff.viewer"},
			want:  []string{PermResourceRead, PermExecutionRead},
			deny:  []string{PermExecutionAudit, PermConfigRead},
		},
		{
			name:  "overlapping roles union",
			roles: []string{"bff.viewer", "bff.operator"},
			want:  []string{PermResourceRead, PermExecutionRead, PermExecutionAudit},
			deny:  []string{PermConfigRead},
		},
		{
			name:  "admin",
			roles: []string{"bff.admin"},
			want:  []string{PermResourceRead, PermExecutionRead, PermExecutionAudit, PermConfigRead},
			deny:  []string{PermAdminInvalidate},
		},
		{
			name:  "unknown role grants nothing",
			roles: []string{"bff.wizard"},
			deny:  []string{PermResourceRead, PermExecutionRead, PermExecutionAudit, PermConfigRead},
		},
		{
			name:  "no roles at all",
			roles: []string{},
			deny:  []string{PermResourceRead},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAuthenticator(t, nil)
			claims := claimSet()
			claims["roles"] = tc.roles

			p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
			testutil.NoError(t, err, "authenticate")
			for _, perm := range tc.want {
				testutil.True(t, p.Can(perm), "%v should grant %s", tc.roles, perm)
			}
			for _, perm := range tc.deny {
				testutil.False(t, p.Can(perm), "%v must not grant %s", tc.roles, perm)
			}
		})
	}
}

// TestPrincipal_Can verifies that a missing principal is never permitted
// anything (REQ-SEC-005).
func TestPrincipal_Can(t *testing.T) {
	t.Parallel()

	var nilPrincipal *Principal
	testutil.False(t, nilPrincipal.Can(PermResourceRead), "a nil principal holds no permission")

	empty := &Principal{}
	testutil.False(t, empty.Can(PermResourceRead), "a principal with no permission map holds nothing")

	p := &Principal{Permissions: map[string]struct{}{PermResourceRead: {}}}
	testutil.True(t, p.Can(PermResourceRead), "a granted permission")
	testutil.False(t, p.Can(PermConfigRead), "an ungranted permission")
}

// ---------------------------------------------------------------------------
// Header handling
// ---------------------------------------------------------------------------

// TestAuthenticate_AuthorizationHeader verifies REQ-SEC-001 and REQ-SEC-013:
// every malformed Authorization header is refused as UNAUTHENTICATED, and the
// message never tells a prober which part of the credential was wrong beyond
// the header's own shape.
func TestAuthenticate_AuthorizationHeader(t *testing.T) {
	t.Parallel()

	valid := signHS256(t, claimSet())

	cases := []struct {
		name       string
		header     string
		setHeader  bool
		wantOp     string
		wantDetail string
	}{
		{name: "absent", setHeader: false, wantOp: "security.header", wantDetail: "an Authorization header is required"},
		{name: "empty", header: "", setHeader: true, wantOp: "security.header", wantDetail: "an Authorization header is required"},
		{name: "bare token, no scheme", header: valid, wantOp: "security.header", wantDetail: "Bearer scheme"},
		{name: "wrong scheme", header: "Basic " + valid, wantOp: "security.header", wantDetail: "Bearer scheme"},
		{name: "wrong scheme, no space", header: "Token" + valid, wantOp: "security.header", wantDetail: "Bearer scheme"},
		{name: "scheme only", header: "Bearer", wantOp: "security.header", wantDetail: "Bearer scheme"},
		{name: "scheme and a space", header: "Bearer ", wantOp: "security.header", wantDetail: "Bearer scheme"},
		{name: "scheme and whitespace", header: "Bearer     ", wantOp: "security.header", wantDetail: "carries no token"},
		{name: "malformed token", header: "Bearer not.a.jwt", wantOp: "security.parse"},
		{name: "truncated token", header: "Bearer " + valid[:len(valid)/2], wantOp: "security.parse"},
		{name: "empty segments", header: "Bearer ..", wantOp: "security.parse"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAuthenticator(t, nil)

			h := http.Header{}
			if tc.setHeader || tc.header != "" {
				h.Set("Authorization", tc.header)
			}

			p, err := a.Authenticate(context.Background(), h)
			testutil.Error(t, err, "%q must be refused", tc.header)
			testutil.ErrCode(t, err, errs.CodeUnauthenticated, "every credential problem is UNAUTHENTICATED")
			testutil.True(t, p == nil, "no principal is produced")

			e, ok := errs.As(err)
			testutil.True(t, ok, "the error carries the taxonomy type")
			testutil.Equal(t, e.Op, tc.wantOp, "internal operation name")
			if tc.wantDetail != "" {
				testutil.True(t, strings.Contains(e.Message, tc.wantDetail),
					"message %q should mention %q", e.Message, tc.wantDetail)
			}
		})
	}

	t.Run("the scheme is matched case-insensitively", func(t *testing.T) {
		t.Parallel()
		a := newTestAuthenticator(t, nil)
		for _, scheme := range []string{"Bearer ", "bearer ", "BEARER ", "BeArEr "} {
			h := http.Header{}
			h.Set("Authorization", scheme+valid)
			_, err := a.Authenticate(context.Background(), h)
			testutil.NoError(t, err, "scheme %q must be accepted", scheme)
		}
	})
}

// ---------------------------------------------------------------------------
// Claim validation
// ---------------------------------------------------------------------------

// TestAuthenticate_ClaimValidation verifies REQ-SEC-002: expiry, issuer and
// audience are all verified, and the client is told nothing about which one
// failed (REQ-SEC-013).
func TestAuthenticate_ClaimValidation(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name    string
		mutate  func(jwt.MapClaims)
		wantErr bool
		wantOp  string
	}{
		{name: "well formed", mutate: func(jwt.MapClaims) {}},
		{
			name:    "expired well beyond the leeway",
			mutate:  func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Hour).Unix() },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "expired just past the leeway",
			mutate:  func(c jwt.MapClaims) { c["exp"] = now.Add(-45 * time.Second).Unix() },
			wantErr: true, wantOp: "security.parse",
		},
		{
			// Small clock differences between the issuer and this service must
			// not reject an otherwise valid token, which is what leeway is for.
			name:   "expired inside the leeway is admitted",
			mutate: func(c jwt.MapClaims) { c["exp"] = now.Add(-10 * time.Second).Unix() },
		},
		{
			name:    "no expiry at all",
			mutate:  func(c jwt.MapClaims) { delete(c, "exp") },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "wrong issuer",
			mutate:  func(c jwt.MapClaims) { c["iss"] = "https://evil.invalid" },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "no issuer",
			mutate:  func(c jwt.MapClaims) { delete(c, "iss") },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "wrong audience",
			mutate:  func(c jwt.MapClaims) { c["aud"] = "some-other-service" },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "no audience",
			mutate:  func(c jwt.MapClaims) { delete(c, "aud") },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:   "audience list containing ours",
			mutate: func(c jwt.MapClaims) { c["aud"] = []string{"other", testAudience} },
		},
		{
			name:    "not yet valid",
			mutate:  func(c jwt.MapClaims) { c["nbf"] = now.Add(time.Hour).Unix() },
			wantErr: true, wantOp: "security.parse",
		},
		{
			name:    "missing a required claim",
			mutate:  func(c jwt.MapClaims) { delete(c, "sub") },
			wantErr: true, wantOp: "security.claims",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAuthenticator(t, nil)
			claims := claimSet()
			tc.mutate(claims)

			p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
			if !tc.wantErr {
				testutil.NoError(t, err, "the token should be accepted")
				testutil.Equal(t, p.TenantID, "tenant-a", "tenant")
				return
			}
			testutil.ErrCode(t, err, errs.CodeUnauthenticated, "the token must be refused")
			e, _ := errs.As(err)
			testutil.Equal(t, e.Op, tc.wantOp, "internal operation name")
			testutil.Equal(t, e.Message, "the supplied credentials are not valid",
				"the client is told nothing about which check failed")
		})
	}
}

// TestAuthenticate_MissingTenantClaim verifies REQ-MT-001: a token with no
// usable tenant establishes no tenant, and the request is refused rather than
// defaulted.
func TestAuthenticate_MissingTenantClaim(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value any
	}{
		{name: "empty string", value: ""},
		{name: "a number", value: 42},
		{name: "a list", value: []string{"tenant-a"}},
		{name: "an object", value: map[string]any{"id": "tenant-a"}},
		{name: "null", value: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The required-claims check only asserts presence, so the tenant
			// check is what has to catch an unusable value.
			a := newTestAuthenticator(t, func(c *config.SecurityConfig) {
				c.JWT.RequiredClaims = []string{"sub"}
			})
			claims := claimSet()
			claims["tenant_id"] = tc.value

			p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
			testutil.ErrCode(t, err, errs.CodeUnauthenticated, "a token with no usable tenant is refused")
			testutil.True(t, p == nil, "no principal")
			e, _ := errs.As(err)
			testutil.Equal(t, e.Op, "security.tenant", "the tenant check refused it")
		})
	}

	t.Run("a custom tenant claim name is honoured", func(t *testing.T) {
		t.Parallel()
		a := newTestAuthenticator(t, func(c *config.SecurityConfig) {
			c.JWT.TenantClaim = "https://example.invalid/tenant"
			c.JWT.RequiredClaims = []string{"sub"}
		})
		claims := claimSet()
		delete(claims, "tenant_id")
		claims["https://example.invalid/tenant"] = "tenant-z"

		p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
		testutil.NoError(t, err, "authenticate")
		testutil.Equal(t, p.TenantID, "tenant-z", "the configured claim supplies the tenant")
	})
}

// TestAuthenticate_RolesClaimShapes verifies REQ-SEC-002: the roles claim is
// accepted in every shape real identity providers emit -- a JSON array, a
// space-separated string, and a comma-separated string.
func TestAuthenticate_RolesClaimShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{name: "json array", value: []string{"bff.viewer", "bff.operator"}, want: []string{"bff.viewer", "bff.operator"}},
		{name: "array of any", value: []any{"bff.viewer", "bff.operator"}, want: []string{"bff.viewer", "bff.operator"}},
		{name: "array with non-strings", value: []any{"bff.viewer", 7, true}, want: []string{"bff.viewer"}},
		{name: "single string", value: "bff.viewer", want: []string{"bff.viewer"}},
		{name: "space separated", value: "bff.viewer bff.operator", want: []string{"bff.viewer", "bff.operator"}},
		{name: "comma separated", value: "bff.viewer,bff.operator", want: []string{"bff.viewer", "bff.operator"}},
		{name: "comma and space separated", value: "bff.viewer, bff.operator", want: []string{"bff.viewer", "bff.operator"}},
		{name: "empty string", value: "", want: []string{}},
		{name: "empty array", value: []string{}, want: []string{}},
		{name: "absent", value: nil, want: nil},
		{name: "a number", value: 7, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAuthenticator(t, nil)
			claims := claimSet()
			if tc.value == nil {
				delete(claims, "roles")
			} else {
				claims["roles"] = tc.value
			}

			p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
			testutil.NoError(t, err, "authenticate")
			testutil.Equal(t, p.Roles, tc.want, "roles parsed from %#v", tc.value)
		})
	}

	t.Run("a custom roles claim name is honoured", func(t *testing.T) {
		t.Parallel()
		a := newTestAuthenticator(t, func(c *config.SecurityConfig) { c.JWT.RolesClaim = "scope" })
		claims := claimSet()
		delete(claims, "roles")
		claims["scope"] = "bff.admin"

		p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claims)))
		testutil.NoError(t, err, "authenticate")
		testutil.Equal(t, p.Roles, []string{"bff.admin"}, "the configured claim supplies the roles")
		testutil.True(t, p.Can(PermConfigRead), "and the permissions follow")
	})

	t.Run("an empty roles claim name yields no roles", func(t *testing.T) {
		t.Parallel()
		a := newTestAuthenticator(t, func(c *config.SecurityConfig) { c.JWT.RolesClaim = "" })
		p, err := a.Authenticate(context.Background(), bearer(signHS256(t, claimSet())))
		testutil.NoError(t, err, "authenticate")
		testutil.True(t, p.Roles == nil, "no roles claim is configured, so no roles are read")
		testutil.Equal(t, len(p.Permissions), 0, "and no permissions are granted")
	})
}

// ---------------------------------------------------------------------------
// Algorithm pinning
// ---------------------------------------------------------------------------

// TestAuthenticate_RejectsAlgNone verifies REQ-SEC-002: an unsigned token is
// refused. Pinning the accepted algorithms is what makes "alg: none" a
// non-event rather than a total authentication bypass.
func TestAuthenticate_RejectsAlgNone(t *testing.T) {
	t.Parallel()

	raw, err := jwt.NewWithClaims(jwt.SigningMethodNone, claimSet()).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	testutil.NoError(t, err, "minting an unsigned token")
	testutil.True(t, strings.HasSuffix(raw, "."), "the token really does carry an empty signature")

	a := newTestAuthenticator(t, nil)
	p, authErr := a.Authenticate(context.Background(), bearer(raw))
	testutil.ErrCode(t, authErr, errs.CodeUnauthenticated, "an unsigned token must be refused")
	testutil.True(t, p == nil, "no principal is produced from an unsigned token")
}

// TestAuthenticate_RejectsAlgorithmConfusion verifies REQ-SEC-002: a token
// signed with an asymmetric algorithm is refused by the HMAC provider. Without
// algorithm pinning this is the classic RS256/HS256 confusion attack.
func TestAuthenticate_RejectsAlgorithmConfusion(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	testutil.NoError(t, err, "generating an RSA key")

	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claimSet()).SignedString(key)
	testutil.NoError(t, err, "signing an RS256 token")

	a := newTestAuthenticator(t, nil)
	p, authErr := a.Authenticate(context.Background(), bearer(raw))
	testutil.ErrCode(t, authErr, errs.CodeUnauthenticated, "an RS256 token must be refused by the HMAC provider")
	testutil.True(t, p == nil, "no principal")

	t.Run("a token signed with the wrong secret is refused", func(t *testing.T) {
		t.Parallel()
		other, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claimSet()).
			SignedString([]byte("a-different-secret-of-at-least-32-bytes"))
		testutil.NoError(t, err, "signing with a foreign secret")
		_, authErr := a.Authenticate(context.Background(), bearer(other))
		testutil.ErrCode(t, authErr, errs.CodeUnauthenticated, "a bad signature is refused")
	})

	t.Run("HS384 is refused even though it is symmetric", func(t *testing.T) {
		t.Parallel()
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claimSet()).SignedString([]byte(testSecret))
		testutil.NoError(t, err, "signing an HS384 token")
		_, authErr := a.Authenticate(context.Background(), bearer(raw))
		testutil.ErrCode(t, authErr, errs.CodeUnauthenticated, "only the pinned algorithm is accepted")
	})
}

// TestHMACKeyProvider verifies REQ-SEC-002: the provider pins exactly one
// algorithm, and refuses a secret too short to be worth anything.
func TestHMACKeyProvider(t *testing.T) {
	t.Parallel()

	p, err := NewHMACKeyProvider(testSecret)
	testutil.NoError(t, err, "a long enough secret is accepted")
	testutil.Equal(t, p.Algorithms(), []string{"HS256"}, "exactly one algorithm is pinned")

	key, err := p.Key(nil)
	testutil.NoError(t, err, "the key is returned for any token header")
	testutil.Equal(t, key.([]byte), []byte(testSecret), "the secret is the verification key")

	for _, short := range []string{"", "short", strings.Repeat("x", 31)} {
		got, err := NewHMACKeyProvider(short)
		testutil.Error(t, err, "a %d-byte secret must be refused", len(short))
		testutil.True(t, got == nil, "no provider is returned")
		testutil.True(t, strings.Contains(err.Error(), "at least 32 bytes"), "the reason is explained: %v", err)
	}
}

// TestJWKSKeyProvider_Algorithms verifies REQ-SEC-002/REQ-SEC-003: the JWKS
// provider accepts only asymmetric algorithms, so a symmetric key arriving over
// a public key set can never be used.
func TestJWKSKeyProvider_Algorithms(t *testing.T) {
	t.Parallel()

	p := NewJWKSKeyProvider("https://issuer.test.invalid/jwks", time.Minute, nil)
	algs := p.Algorithms()
	testutil.Equal(t, algs, []string{"RS256", "RS384", "RS512", "ES256"}, "pinned algorithms")
	for _, a := range algs {
		testutil.False(t, strings.HasPrefix(a, "HS"), "no symmetric algorithm may be accepted over JWKS")
		testutil.NotEqual(t, a, "none", "the none algorithm is never accepted")
	}

	_, err := p.Key(&jwt.Token{Header: map[string]any{}})
	testutil.Error(t, err, "a token with no key id cannot be verified")
	testutil.True(t, strings.Contains(err.Error(), "no key id"), "the reason is explained: %v", err)
}

// ---------------------------------------------------------------------------
// Insecure development mode
// ---------------------------------------------------------------------------

// TestAuthenticate_InsecureMode verifies REQ-SEC-001: the no-auth mode exists so
// the local compose stack is usable, grants every configured role, and takes its
// tenant from a header because there is no token to take it from. Configuration
// validation refuses this mode outside local and test environments.
func TestAuthenticate_InsecureMode(t *testing.T) {
	t.Parallel()

	a := newTestAuthenticator(t, func(c *config.SecurityConfig) { c.AllowInsecureNoAuth = true })

	t.Run("no header at all", func(t *testing.T) {
		t.Parallel()
		p, err := a.Authenticate(context.Background(), http.Header{})
		testutil.NoError(t, err, "insecure mode never refuses")
		testutil.Equal(t, p.Subject, "local-dev", "a fixed development subject")
		testutil.Equal(t, p.TenantID, "local", "a fixed development tenant")
		testutil.Equal(t, len(p.Roles), 3, "every configured role is granted")
		testutil.True(t, p.Can(PermConfigRead), "including the admin permissions")
	})

	t.Run("tenant from a header", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		h.Set("X-Tenant-ID", "tenant-dev")
		p, err := a.Authenticate(context.Background(), h)
		testutil.NoError(t, err, "insecure mode never refuses")
		testutil.Equal(t, p.TenantID, "tenant-dev", "the header establishes the tenant in this mode only")
	})

	t.Run("a garbage token is ignored", func(t *testing.T) {
		t.Parallel()
		p, err := a.Authenticate(context.Background(), bearer("not.a.jwt"))
		testutil.NoError(t, err, "the token is not even looked at")
		testutil.Equal(t, p.Subject, "local-dev", "the development principal is used regardless")
	})
}

// ---------------------------------------------------------------------------
// ResolveTenant
// ---------------------------------------------------------------------------

// TestResolveTenant verifies REQ-MT-001 and REQ-SEC-004: a header may assert a
// tenant and will be checked against the token, but it can never establish one.
// A mismatch is refused rather than resolved in either direction.
func TestResolveTenant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		principal  *Principal
		header     string
		wantTenant string
		wantCode   errs.Code
	}{
		{
			name:       "no header asserts nothing",
			principal:  &Principal{TenantID: "tenant-a"},
			header:     "",
			wantTenant: "tenant-a",
		},
		{
			name:       "a matching header is accepted",
			principal:  &Principal{TenantID: "tenant-a"},
			header:     "tenant-a",
			wantTenant: "tenant-a",
		},
		{
			name:       "a padded matching header is accepted",
			principal:  &Principal{TenantID: "tenant-a"},
			header:     "  tenant-a  ",
			wantTenant: "tenant-a",
		},
		{
			name:       "a whitespace-only header asserts nothing",
			principal:  &Principal{TenantID: "tenant-a"},
			header:     "   ",
			wantTenant: "tenant-a",
		},
		{
			name:      "a mismatched header is refused",
			principal: &Principal{TenantID: "tenant-a"},
			header:    "tenant-b",
			wantCode:  errs.CodeTenantMismatch,
		},
		{
			name:      "case matters: tenant ids are opaque",
			principal: &Principal{TenantID: "tenant-a"},
			header:    "TENANT-A",
			wantCode:  errs.CodeTenantMismatch,
		},
		{
			name:      "a nil principal establishes nothing",
			principal: nil,
			header:    "tenant-a",
			wantCode:  errs.CodeUnauthenticated,
		},
		{
			name:      "a principal with no tenant establishes nothing",
			principal: &Principal{Subject: "user-1"},
			header:    "tenant-a",
			wantCode:  errs.CodeUnauthenticated,
		},
		{
			name:      "a header cannot establish a tenant on its own",
			principal: &Principal{Subject: "user-1"},
			header:    "tenant-b",
			wantCode:  errs.CodeUnauthenticated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTenant(tc.principal, tc.header)
			if tc.wantCode == "" {
				testutil.NoError(t, err, "resolution should succeed")
				testutil.Equal(t, got, tc.wantTenant, "resolved tenant")
				return
			}
			testutil.ErrCode(t, err, tc.wantCode, "resolution must fail")
			testutil.Equal(t, got, "", "no tenant is returned on failure")
			e, _ := errs.As(err)
			testutil.Equal(t, e.Op, "security.resolve_tenant", "internal operation name")
		})
	}
}

// ---------------------------------------------------------------------------
// Authorize
// ---------------------------------------------------------------------------

// TestAuthorize verifies REQ-SEC-005: with default_deny on, a permission that
// was not granted is a 403; with it off, the check is advisory. Production runs
// with default_deny on, which configuration validation documents.
func TestAuthorize(t *testing.T) {
	t.Parallel()

	granted := &Principal{Permissions: map[string]struct{}{PermResourceRead: {}}}

	cases := []struct {
		name        string
		principal   *Principal
		perm        string
		defaultDeny bool
		wantErr     bool
	}{
		{name: "granted, deny by default", principal: granted, perm: PermResourceRead, defaultDeny: true},
		{name: "granted, allow by default", principal: granted, perm: PermResourceRead, defaultDeny: false},
		{name: "not granted, deny by default", principal: granted, perm: PermExecutionAudit, defaultDeny: true, wantErr: true},
		{name: "not granted, allow by default", principal: granted, perm: PermExecutionAudit, defaultDeny: false},
		{name: "nil principal, deny by default", principal: nil, perm: PermResourceRead, defaultDeny: true, wantErr: true},
		{name: "nil principal, allow by default", principal: nil, perm: PermResourceRead, defaultDeny: false},
		{name: "no permissions, deny by default", principal: &Principal{}, perm: PermResourceRead, defaultDeny: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Authorize(tc.principal, tc.perm, tc.defaultDeny)
			if !tc.wantErr {
				testutil.NoError(t, err, "the call should be permitted")
				return
			}
			testutil.ErrCode(t, err, errs.CodeForbidden, "the call must be refused")
			e, _ := errs.As(err)
			testutil.Equal(t, e.Op, "security.authorize", "internal operation name")
			testutil.Equal(t, e.Details["permission"], any(tc.perm),
				"the required permission is recorded for the log, not for the client")
			testutil.Equal(t, e.Message, "the caller is not permitted to perform this operation",
				"the client message names no permission")
		})
	}
}

// ---------------------------------------------------------------------------
// Context plumbing
// ---------------------------------------------------------------------------

// TestPrincipalContext verifies REQ-MT-001: the principal travels on the
// context, and a context without one yields nil rather than a zero principal
// that would look authenticated.
func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	testutil.True(t, PrincipalFrom(context.Background()) == nil, "an unauthenticated context carries no principal")

	p := &Principal{Subject: "user-1", TenantID: "tenant-a"}
	ctx := WithPrincipal(context.Background(), p)
	testutil.True(t, PrincipalFrom(ctx) == p, "the principal round-trips")

	// A value stored under a colliding string key must not be mistaken for one.
	type otherKey struct{}
	polluted := context.WithValue(context.Background(), otherKey{}, p)
	testutil.True(t, PrincipalFrom(polluted) == nil, "only the package's own key is read")
}
