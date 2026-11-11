package wlsftp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func TestLoadOrGenerateKeypairIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "worldline_public.asc")
	privPath := filepath.Join(dir, "worldline_private.asc")

	first, err := LoadOrGenerateKeypair(pubPath, privPath, "mock-Worldline", "test", "worldline@test.local")
	if err != nil {
		t.Fatal(err)
	}
	firstPub, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	firstPriv, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}

	second, err := LoadOrGenerateKeypair(pubPath, privPath, "mock-Worldline", "test", "worldline@test.local")
	if err != nil {
		t.Fatal(err)
	}

	if first.PrimaryKey.KeyId != second.PrimaryKey.KeyId {
		t.Fatalf("second call read back a different key: first=%x second=%x", first.PrimaryKey.KeyId, second.PrimaryKey.KeyId)
	}

	secondPub, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	secondPriv, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstPub) != string(secondPub) {
		t.Fatal("public key file was rewritten on the second call instead of being read back")
	}
	if string(firstPriv) != string(secondPriv) {
		t.Fatal("private key file was rewritten on the second call instead of being read back")
	}
}

func TestLoadOrGenerateKeypairGeneratesFreshWhenMissing(t *testing.T) {
	dir := t.TempDir()
	e, err := LoadOrGenerateKeypair(filepath.Join(dir, "pub.asc"), filepath.Join(dir, "priv.asc"), "mock-Worldline", "test", "worldline@test.local")
	if err != nil {
		t.Fatal(err)
	}
	if e.PrivateKey == nil || e.PrivateKey.Encrypted {
		t.Fatal("expected a usable, unencrypted private key")
	}
	if _, ok := e.EncryptionKey(time.Now()); !ok {
		t.Fatal("generated entity has no usable encryption subkey")
	}
}

func TestEncryptAndSignSelfEncryptRoundTrips(t *testing.T) {
	own, err := GenerateEntity("mock-Worldline", "test", "worldline@test.local")
	if err != nil {
		t.Fatal(err)
	}
	keys := &PGPKeys{Own: own} // Recipient unset: Addendum §E self-encrypt default

	plaintext := []byte("VERSION_NUMBER,RECORD_TYPE\n\"1\",\"Settlement\"\n")
	ciphertext, err := keys.EncryptAndSign(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("expected non-empty ciphertext")
	}

	got, err := Decrypt(ciphertext, openpgp.EntityList{own})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("decrypted = %q, want %q", got, plaintext)
	}
}

func TestEncryptAndSignConfiguredRecipientRoundTrips(t *testing.T) {
	own, err := GenerateEntity("mock-Worldline", "test", "worldline@test.local")
	if err != nil {
		t.Fatal(err)
	}
	infinite, err := GenerateEntity("mock-Infinite", "test", "infinite@test.local")
	if err != nil {
		t.Fatal(err)
	}
	keys := &PGPKeys{Own: own, Recipient: infinite} // Addendum §E: recipient configured

	plaintext := []byte("settlement file content")
	ciphertext, err := keys.EncryptAndSign(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	// The configured recipient (holding its own private key, as the real
	// Infinite side would) can decrypt and verify mock-Worldline's
	// signature.
	got, err := Decrypt(ciphertext, openpgp.EntityList{infinite, own})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("decrypted = %q, want %q", got, plaintext)
	}

	// mock-Worldline's own key must NOT be able to decrypt a message that
	// was encrypted to the configured recipient instead of self-encrypted
	// — this is the whole point of Addendum §E's direction switch.
	if _, err := Decrypt(ciphertext, openpgp.EntityList{own}); err == nil {
		t.Fatal("expected mock-Worldline's own key to be unable to decrypt a message encrypted to the configured recipient")
	}
}
