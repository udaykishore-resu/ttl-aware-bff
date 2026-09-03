package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// jwk is the subset of RFC 7517 the BFF needs. Only RSA and EC keys are
// supported; symmetric keys must not arrive over a public JWKS endpoint, and
// accepting them there is how HS/RS confusion attacks start.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWK converts one JWK into a Go public key.
func parseJWK(raw json.RawMessage) (string, any, error) {
	var k jwk
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", nil, fmt.Errorf("security: decode JWK: %w", err)
	}
	if k.Use != "" && k.Use != "sig" {
		return "", nil, errors.New("security: JWK is not a signature key")
	}

	switch k.Kty {
	case "RSA":
		n, err := b64uint(k.N)
		if err != nil {
			return "", nil, err
		}
		e, err := b64uint(k.E)
		if err != nil {
			return "", nil, err
		}
		if !e.IsInt64() || e.Int64() <= 0 {
			return "", nil, errors.New("security: RSA exponent out of range")
		}
		return k.Kid, &rsa.PublicKey{N: n, E: int(e.Int64())}, nil

	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return "", nil, fmt.Errorf("security: unsupported EC curve %q", k.Crv)
		}
		x, err := b64uint(k.X)
		if err != nil {
			return "", nil, err
		}
		y, err := b64uint(k.Y)
		if err != nil {
			return "", nil, err
		}
		return k.Kid, &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil

	default:
		return "", nil, fmt.Errorf("security: unsupported key type %q", k.Kty)
	}
}

func b64uint(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("security: empty JWK parameter")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("security: decode JWK parameter: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}
