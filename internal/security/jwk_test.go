package security

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwkJSON renders a JWK document from its fields, omitting the empty ones so a
// test can leave a parameter out entirely.
func jwkJSON(t *testing.T, fields map[string]string) json.RawMessage {
	t.Helper()
	m := map[string]string{}
	for k, v := range fields {
		if v != "" {
			m[k] = v
		}
	}
	raw, err := json.Marshal(m)
	testutil.NoError(t, err, "marshalling the JWK fixture")
	return raw
}

// TestParseJWK_RSA verifies REQ-SEC-003: an RSA signing key from a JWKS
// endpoint is reconstructed exactly, modulus and exponent alike.
func TestParseJWK_RSA(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	testutil.NoError(t, err, "generating an RSA key")
	pub := &key.PublicKey

	t.Run("valid key", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{
			"kty": "RSA",
			"kid": "rsa-1",
			"use": "sig",
			"alg": "RS256",
			"n":   b64(pub.N.Bytes()),
			"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
		})

		kid, got, err := parseJWK(raw)
		testutil.NoError(t, err, "a well-formed RSA JWK must parse")
		testutil.Equal(t, kid, "rsa-1", "the key id is returned so the cache can index it")

		parsed, ok := got.(*rsa.PublicKey)
		testutil.True(t, ok, "the parsed key is an *rsa.PublicKey")
		testutil.Equal(t, parsed.N.Cmp(pub.N), 0, "the modulus round-trips exactly")
		testutil.Equal(t, parsed.E, pub.E, "the exponent round-trips exactly")
	})

	t.Run("the standard AQAB exponent", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{"kty": "RSA", "kid": "rsa-2", "n": b64(pub.N.Bytes()), "e": "AQAB"})
		_, got, err := parseJWK(raw)
		testutil.NoError(t, err, "AQAB is the near-universal encoding of 65537")
		testutil.Equal(t, got.(*rsa.PublicKey).E, 65537, "AQAB decodes to 65537")
	})

	t.Run("no use field is accepted", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{"kty": "RSA", "kid": "rsa-3", "n": b64(pub.N.Bytes()), "e": "AQAB"})
		_, _, err := parseJWK(raw)
		testutil.NoError(t, err, "a key that does not declare its use is assumed to be a signing key")
	})

	t.Run("no kid", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{"kty": "RSA", "n": b64(pub.N.Bytes()), "e": "AQAB"})
		kid, got, err := parseJWK(raw)
		testutil.NoError(t, err, "a key without an id still parses")
		testutil.Equal(t, kid, "", "but it carries no id, and the caller skips it")
		testutil.True(t, got != nil, "the key itself is still returned")
	})
}

// TestParseJWK_EC verifies REQ-SEC-003: EC signing keys on the supported curves
// are reconstructed, and an unsupported curve is refused rather than guessed at.
func TestParseJWK_EC(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		crv   string
		curve elliptic.Curve
	}{
		{name: "P-256", crv: "P-256", curve: elliptic.P256()},
		{name: "P-384", crv: "P-384", curve: elliptic.P384()},
		{name: "P-521", crv: "P-521", curve: elliptic.P521()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			testutil.NoError(t, err, "generating an EC key")

			raw := jwkJSON(t, map[string]string{
				"kty": "EC",
				"kid": "ec-" + tc.crv,
				"use": "sig",
				"crv": tc.crv,
				"x":   b64(key.X.Bytes()),
				"y":   b64(key.Y.Bytes()),
			})

			kid, got, err := parseJWK(raw)
			testutil.NoError(t, err, "a well-formed EC JWK must parse")
			testutil.Equal(t, kid, "ec-"+tc.crv, "key id")

			parsed, ok := got.(*ecdsa.PublicKey)
			testutil.True(t, ok, "the parsed key is an *ecdsa.PublicKey")
			testutil.Equal(t, parsed.Curve, tc.curve, "the curve is the declared one")
			testutil.Equal(t, parsed.X.Cmp(key.X), 0, "X round-trips exactly")
			testutil.Equal(t, parsed.Y.Cmp(key.Y), 0, "Y round-trips exactly")
		})
	}

	t.Run("unsupported curve", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{"kty": "EC", "kid": "ec-x", "crv": "P-192", "x": "AQ", "y": "Ag"})
		_, got, err := parseJWK(raw)
		testutil.Error(t, err, "an unsupported curve must be refused")
		testutil.True(t, got == nil, "no key is returned")
		testutil.True(t, strings.Contains(err.Error(), "unsupported EC curve"), "the reason is explained: %v", err)
	})

	t.Run("missing curve", func(t *testing.T) {
		t.Parallel()
		raw := jwkJSON(t, map[string]string{"kty": "EC", "kid": "ec-x", "x": "AQ", "y": "Ag"})
		_, _, err := parseJWK(raw)
		testutil.Error(t, err, "a key with no declared curve must be refused")
	})
}

// TestParseJWK_Rejects verifies REQ-SEC-002/REQ-SEC-003: only asymmetric signing
// keys are accepted. A symmetric key arriving over a public JWKS endpoint is how
// HS/RS confusion attacks start, and an encryption key is not a verification
// key.
func TestParseJWK_Rejects(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	testutil.NoError(t, err, "generating an RSA key")
	n := b64(key.PublicKey.N.Bytes())

	cases := []struct {
		name   string
		raw    json.RawMessage
		detail string
	}{
		{
			name:   "symmetric key type",
			raw:    json.RawMessage(`{"kty":"oct","kid":"sym-1","k":"c2VjcmV0"}`),
			detail: `unsupported key type "oct"`,
		},
		{
			name:   "unknown key type",
			raw:    json.RawMessage(`{"kty":"OKP","kid":"ed-1","crv":"Ed25519","x":"AQ"}`),
			detail: `unsupported key type "OKP"`,
		},
		{
			name:   "no key type at all",
			raw:    json.RawMessage(`{"kid":"mystery-1"}`),
			detail: `unsupported key type ""`,
		},
		{
			name:   "encryption key",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-enc","use":"enc","n":"` + n + `","e":"AQAB"}`),
			detail: "not a signature key",
		},
		{
			name:   "unknown use",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-x","use":"tls","n":"` + n + `","e":"AQAB"}`),
			detail: "not a signature key",
		},
		{
			name:   "malformed modulus",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-bad","n":"!!!not-base64!!!","e":"AQAB"}`),
			detail: "decode JWK parameter",
		},
		{
			name:   "standard base64 padding is not raw url encoding",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-pad","n":"aGVsbG8=","e":"AQAB"}`),
			detail: "decode JWK parameter",
		},
		{
			name:   "malformed exponent",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-bad-e","n":"` + n + `","e":"?!?"}`),
			detail: "decode JWK parameter",
		},
		{
			name:   "empty modulus",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-empty-n","n":"","e":"AQAB"}`),
			detail: "empty JWK parameter",
		},
		{
			name:   "missing exponent",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-no-e","n":"` + n + `"}`),
			detail: "empty JWK parameter",
		},
		{
			name:   "zero exponent",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-zero-e","n":"` + n + `","e":"AA"}`),
			detail: "RSA exponent out of range",
		},
		{
			name:   "absurd exponent",
			raw:    json.RawMessage(`{"kty":"RSA","kid":"rsa-huge-e","n":"` + n + `","e":"` + b64(bytes.Repeat([]byte{0xff}, 32)) + `"}`),
			detail: "RSA exponent out of range",
		},
		{
			name:   "malformed EC coordinate",
			raw:    json.RawMessage(`{"kty":"EC","kid":"ec-bad","crv":"P-256","x":"###","y":"Ag"}`),
			detail: "decode JWK parameter",
		},
		{
			name:   "missing EC y coordinate",
			raw:    json.RawMessage(`{"kty":"EC","kid":"ec-no-y","crv":"P-256","x":"AQ"}`),
			detail: "empty JWK parameter",
		},
		{
			name:   "not an object",
			raw:    json.RawMessage(`["not","a","jwk"]`),
			detail: "decode JWK",
		},
		{
			name:   "malformed json",
			raw:    json.RawMessage(`{"kty":`),
			detail: "decode JWK",
		},
		{
			name:   "wrong field types",
			raw:    json.RawMessage(`{"kty":123}`),
			detail: "decode JWK",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kid, got, err := parseJWK(tc.raw)
			testutil.Error(t, err, "%s must be refused", tc.name)
			testutil.True(t, got == nil, "no key is returned")
			testutil.Equal(t, kid, "", "no key id is returned on failure")
			testutil.True(t, strings.Contains(err.Error(), tc.detail),
				"error %q should mention %q", err.Error(), tc.detail)
			testutil.True(t, strings.HasPrefix(err.Error(), "security:"),
				"the error is attributed to this package: %v", err)
		})
	}
}

// TestB64Uint verifies the base64url decoding the JWK parser relies on
// (REQ-SEC-003).
func TestB64Uint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		want    *big.Int
		wantErr string
	}{
		{name: "AQAB is 65537", in: "AQAB", want: big.NewInt(65537)},
		{name: "single byte", in: "AQ", want: big.NewInt(1)},
		{name: "zero byte", in: "AA", want: big.NewInt(0)},
		{name: "empty", in: "", wantErr: "empty JWK parameter"},
		{name: "not base64", in: "!!!", wantErr: "decode JWK parameter"},
		{name: "padded", in: "AQAB=", wantErr: "decode JWK parameter"},
		{name: "standard alphabet", in: "a+b/", wantErr: "decode JWK parameter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := b64uint(tc.in)
			if tc.wantErr != "" {
				testutil.Error(t, err, "%q must be refused", tc.in)
				testutil.True(t, strings.Contains(err.Error(), tc.wantErr),
					"error %q should mention %q", err.Error(), tc.wantErr)
				return
			}
			testutil.NoError(t, err, "decoding %q", tc.in)
			testutil.Equal(t, got.Cmp(tc.want), 0, "decoded value of %q", tc.in)
		})
	}
}
