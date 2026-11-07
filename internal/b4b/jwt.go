package b4b

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RequiredAudience is the claim every inbound B4B payment-API call's JWT
// must carry, per docs/ARCHITECTURE-vendor-corrections.md section 4.
const RequiredAudience = "b4b-payments"

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type jwtClaims struct {
	Aud string `json:"aud"`
}

// VerifyBearerToken manually parses and verifies a compact JWS
// (base64url(header).base64url(payload).base64url(signature)): no JWT
// library, per the doc's explicit "manual 3-part JWT parse +
// rsa.VerifyPKCS1v15" instruction. It requires alg RS512, a kid matching
// wantKid, claim aud == RequiredAudience, and a signature verifying against
// pub. Any parse or verification failure returns a non-nil error describing
// exactly what failed, for the caller to log and answer 401.
func VerifyBearerToken(token string, pub *rsa.PublicKey, wantKid string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("b4b: token has %d parts, want 3", len(parts))
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("b4b: decode header: %w", err)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("b4b: decode payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("b4b: decode signature: %w", err)
	}

	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("b4b: parse header: %w", err)
	}
	if header.Alg != "RS512" {
		return fmt.Errorf("b4b: alg %q rejected, want RS512", header.Alg)
	}
	if header.Kid == "" || header.Kid != wantKid {
		return fmt.Errorf("b4b: kid %q does not match configured %q", header.Kid, wantKid)
	}

	var claims jwtClaims
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return fmt.Errorf("b4b: parse claims: %w", err)
	}
	if claims.Aud != RequiredAudience {
		return fmt.Errorf("b4b: aud %q rejected, want %q", claims.Aud, RequiredAudience)
	}

	if pub == nil {
		return errors.New("b4b: no public key configured")
	}
	sum := sha512.Sum512([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA512, sum[:], sig); err != nil {
		return fmt.Errorf("b4b: signature verification failed: %w", err)
	}
	return nil
}

// LoadOrGenerateKeyPair reads private.pem from dir and returns its RSA key.
// If dir has no private.pem yet, it generates a fresh RSA-2048 keypair and
// persists both private.pem and public.pem there (docs/
// ARCHITECTURE-vendor-corrections.md Addendum section D: this directory is
// a shared volume with settlement in the final compose wiring, read-write
// for B4B).
func LoadOrGenerateKeyPair(dir string) (*rsa.PrivateKey, error) {
	privPath := filepath.Join(dir, "private.pem")
	if data, err := os.ReadFile(privPath); err == nil {
		return parsePrivateKeyPEM(data)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("b4b: read %s: %w", privPath, err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("b4b: mkdir %s: %w", dir, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("b4b: generate RSA key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	// 644, not 600: lab-only fake key material (gitignored, never a real
	// B4B key), shared across a UID boundary the same way every other
	// generated credential in this lab is (see ca/generate.sh's cert
	// permissions comment) -- settlement's container reads this back to
	// sign its own JWTs, and a person or the e2e suite on the host may want to as
	// well (the whole point of persisting it here per Addendum section D).
	if err := os.WriteFile(privPath, privPEM, 0o644); err != nil {
		return nil, fmt.Errorf("b4b: write %s: %w", privPath, err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("b4b: marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	pubPath := filepath.Join(dir, "public.pem")
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return nil, fmt.Errorf("b4b: write %s: %w", pubPath, err)
	}
	return key, nil
}

// LoadPrivateKey reads one PEM-encoded RSA private key from path. Callers
// that only need to *sign* with B4B's keypair (settlement, the console)
// use this rather than LoadOrGenerateKeyPair, which would create a key --
// and a client that silently invents its own key would be rejected by B4B
// with a 401 that says nothing about why.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("b4b: read %s: %w", path, err)
	}
	key, err := parsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	return key, nil
}

func parsePrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("b4b: private.pem contains no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("b4b: parse private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("b4b: private.pem does not hold an RSA key")
	}
	return rsaKey, nil
}

// PublicKeyPEM renders pub as a PEM-encoded PKIX public key, for printing at
// startup so an operator or the e2e suite can see it.
func PublicKeyPEM(pub *rsa.PublicKey) (string, error) {
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("b4b: marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b})), nil
}

// SignBearerToken mints the RS512 bearer token every Oversight call
// carries: header {"alg":"RS512","kid":kid}, claims {"aud":"b4b-payments"}.
// It is the exact mirror of VerifyBearerToken, and lives here so a caller
// and the server cannot drift -- the signing was previously open-coded at
// each call site, which is one copy per client of a format that has to
// match byte for byte.
func SignBearerToken(key *rsa.PrivateKey, kid string) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS512", "kid": kid})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]string{"aud": RequiredAudience})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sum := sha512.Sum512([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA512, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// LoadPublicKey reads one PEM-encoded RSA public key from path. This is how
// a real vendor holds a client's credential -- the public half and nothing
// else -- and it is what B4B_JWT_PUBLIC_KEY_PATH points at when this mock
// is put in front of a client that owns its own signing key. Accepts both
// PKIX ("PUBLIC KEY") and PKCS#1 ("RSA PUBLIC KEY") encodings, because
// which one a client's tooling emits is not something it usually chooses.
func LoadPublicKey(path string) (*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("b4b: read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("b4b: %s contains no PEM block", path)
	}
	if parsed, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("b4b: %s does not hold an RSA public key", path)
		}
		return key, nil
	}
	key, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("b4b: parse public key %s: %w", path, err)
	}
	return key, nil
}
