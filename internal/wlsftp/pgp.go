package wlsftp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// pgpKeyBits is the RSA modulus size used for every keypair this package
// generates, matching real Worldline/Infinite SFTP+PGP deployments.
const pgpKeyBits = 3072

// GenerateEntity creates a fresh RSA-3072 OpenPGP entity (a signing
// primary key plus an encryption subkey) with a single user id.
func GenerateEntity(name, comment, email string) (*openpgp.Entity, error) {
	e, err := openpgp.NewEntity(name, comment, email, &packet.Config{RSABits: pgpKeyBits})
	if err != nil {
		return nil, fmt.Errorf("wlsftp: generate pgp entity: %w", err)
	}
	return e, nil
}

// LoadOrGenerateKeypair loads an OpenPGP keypair from pubPath/privPath,
// generating and persisting (armored ASCII) a fresh RSA-3072 keypair the
// first time either file is missing. A second call against the same paths
// reads the persisted key back rather than generating a new one.
func LoadOrGenerateKeypair(pubPath, privPath, name, comment, email string) (*openpgp.Entity, error) {
	_, pubErr := os.Stat(pubPath)
	_, privErr := os.Stat(privPath)
	switch {
	case pubErr == nil && privErr == nil:
		return readPrivateKeypair(privPath)
	case pubErr != nil && !os.IsNotExist(pubErr):
		return nil, fmt.Errorf("wlsftp: stat %s: %w", pubPath, pubErr)
	case privErr != nil && !os.IsNotExist(privErr):
		return nil, fmt.Errorf("wlsftp: stat %s: %w", privPath, privErr)
	}

	e, err := GenerateEntity(name, comment, email)
	if err != nil {
		return nil, err
	}
	if err := persistKeypair(e, pubPath, privPath); err != nil {
		return nil, err
	}
	return e, nil
}

// LoadPublicKey reads an armored OpenPGP public key from path — used for a
// configured recipient key this package never holds the private half of
// (e.g. INFINITE_PGP_PUBLIC_KEY_PATH).
func LoadPublicKey(path string) (*openpgp.Entity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: open public key %s: %w", path, err)
	}
	defer f.Close()
	el, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: parse public key %s: %w", path, err)
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("wlsftp: public key file %s contains no entities", path)
	}
	return el[0], nil
}

// ArmoredPublicKey serializes e's public key as armored ASCII, suitable
// for printing at startup or for a real buddy deployment to be pointed at
// this lab with (per docs/ARCHITECTURE-vendor-corrections.md §2).
func ArmoredPublicKey(e *openpgp.Entity) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", fmt.Errorf("wlsftp: armor public key: %w", err)
	}
	if err := e.Serialize(w); err != nil {
		return "", fmt.Errorf("wlsftp: serialize public key: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("wlsftp: close public key armor: %w", err)
	}
	return buf.String(), nil
}

func readPrivateKeypair(privPath string) (*openpgp.Entity, error) {
	f, err := os.Open(privPath)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: open private key %s: %w", privPath, err)
	}
	defer f.Close()
	el, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: parse private key %s: %w", privPath, err)
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("wlsftp: private key file %s contains no entities", privPath)
	}
	return el[0], nil
}

func persistKeypair(e *openpgp.Entity, pubPath, privPath string) error {
	if err := os.MkdirAll(filepath.Dir(pubPath), 0o755); err != nil {
		return fmt.Errorf("wlsftp: create pgp key dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(privPath), 0o755); err != nil {
		return fmt.Errorf("wlsftp: create pgp key dir: %w", err)
	}

	armoredPub, err := ArmoredPublicKey(e)
	if err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(armoredPub), 0o644); err != nil {
		return fmt.Errorf("wlsftp: write public key %s: %w", pubPath, err)
	}

	var priv bytes.Buffer
	privW, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return fmt.Errorf("wlsftp: armor private key: %w", err)
	}
	if err := e.SerializePrivate(privW, nil); err != nil {
		return fmt.Errorf("wlsftp: serialize private key: %w", err)
	}
	if err := privW.Close(); err != nil {
		return fmt.Errorf("wlsftp: close private key armor: %w", err)
	}
	// 644, not 600: this is lab-only fake key material (gitignored, never
	// a real Worldline key) shared across a UID boundary -- worldline
	// writes it as one (possibly rootless-remapped) UID, but settlement,
	// the e2e container and a host-side `make e2e` all need to read it back to
	// decrypt what they download. Same tradeoff as ca/generate.sh's cert
	// permissions (see docs/security/ca-and-tls.md).
	if err := os.WriteFile(privPath, priv.Bytes(), 0o644); err != nil {
		return fmt.Errorf("wlsftp: write private key %s: %w", privPath, err)
	}
	return nil
}

// PGPKeys bundles mock-Worldline's own keypair with an optional configured
// recipient key, implementing the encryption direction fixed by
// docs/ARCHITECTURE-vendor-corrections.md Addendum §E: encrypt to the
// configured "Infinite" recipient key if one is present, otherwise
// self-encrypt to mock-Worldline's own public key; always sign with
// mock-Worldline's own private key, regardless.
type PGPKeys struct {
	Own       *openpgp.Entity // mock-Worldline's own keypair (public + private)
	Recipient *openpgp.Entity // optional Infinite public key; nil => self-encrypt
}

// Keyring is the set of entities that can open a message addressed to this
// side. Own carries the private half; Recipient is included because a
// self-encrypted message — what EncryptAndSign produces when no recipient
// is configured — is addressed to Own anyway, and a caller decrypting
// should not have to know which of the two paths produced the file.
func (k *PGPKeys) Keyring() openpgp.EntityList {
	var ring openpgp.EntityList
	if k.Own != nil {
		ring = append(ring, k.Own)
	}
	if k.Recipient != nil {
		ring = append(ring, k.Recipient)
	}
	return ring
}

// EncryptAndSign implements the Addendum §E encryption direction described
// on PGPKeys.
func (k *PGPKeys) EncryptAndSign(plaintext []byte) ([]byte, error) {
	to := k.Own
	if k.Recipient != nil {
		to = k.Recipient
	}
	return Encrypt(plaintext, to, k.Own)
}

// Encrypt PGP-encrypts plaintext to recipient's public key and signs it
// with signer's private key, returning the binary (non-armored) OpenPGP
// message — matching the real Worldline/Infinite wire format (OpenPGP.js
// binary format, not ASCII-armored).
func Encrypt(plaintext []byte, recipient, signer *openpgp.Entity) ([]byte, error) {
	var buf bytes.Buffer
	w, err := openpgp.Encrypt(&buf, []*openpgp.Entity{recipient}, signer, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("wlsftp: encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("wlsftp: encrypt close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt reads a binary OpenPGP message produced by Encrypt, decrypting it
// with whichever private key in keyring matches the message and verifying
// its signature against a public key also present in keyring. It returns
// an error if the message carries a signature that fails verification.
func Decrypt(ciphertext []byte, keyring openpgp.EntityList) ([]byte, error) {
	md, err := openpgp.ReadMessage(bytes.NewReader(ciphertext), keyring, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: read pgp message: %w", err)
	}
	plaintext, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: decrypt: %w", err)
	}
	if md.IsSigned && md.SignatureError != nil {
		return nil, fmt.Errorf("wlsftp: signature verification failed: %w", md.SignatureError)
	}
	if md.IsSigned && md.SignedBy == nil {
		return nil, errors.New("wlsftp: message signed by an unknown key")
	}
	return plaintext, nil
}
