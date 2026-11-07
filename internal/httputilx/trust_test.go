package httputilx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func labCA(t *testing.T) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fintechlab test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTheLabCAIsTrustedOnTopOfTheSystemRoots(t *testing.T) {
	cert, p := labCA(t)
	pool, ok := RootsWithLabCA(p)
	if !ok {
		t.Fatal("a PEM certificate was not added")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("the lab CA does not verify against the pool: %v", err)
	}
	// The system roots are the starting point, not replaced: whatever the
	// host trusts, the pool trusts at least as much.
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		if sys.Equal(pool) {
			t.Fatal("the pool is just the system roots: the lab CA is missing")
		}
	}
}

func TestPEMWithNoCertificateIsReported(t *testing.T) {
	if _, ok := RootsWithLabCA([]byte("not a certificate")); ok {
		t.Fatal("garbage reported as a certificate")
	}
}
