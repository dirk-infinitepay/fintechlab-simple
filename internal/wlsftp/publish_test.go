package wlsftp

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/ssh"
)

// testKeys builds a PGP keypair that both encrypts (as the acquirer) and
// decrypts (as the platform), which is what this lab does when no separate
// platform key is configured.
func testKeys(t *testing.T) (*PGPKeys, openpgp.EntityList) {
	t.Helper()
	e, err := GenerateEntity("test", "fintechlab-simple", "test@fintechlab-simple.local")
	if err != nil {
		t.Fatalf("GenerateEntity: %v", err)
	}
	return &PGPKeys{Own: e, Recipient: e}, openpgp.EntityList{e}
}

func TestPublishWritesPlainToBothPathConventions(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	body := []byte(`"VERSION_NUMBER","RECORD_TYPE"` + "\n" + `"1","ST"` + "\n")
	const want = "20260904084200_Worldline_Settlement_ER_EUR.csv"

	// Worldline leaves its settlement files on the channel unencrypted:
	// the name is the file's own and the bytes are the file.
	name, err := Publish(root, want, body)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if name != want {
		t.Fatalf("published name = %q, want %q", name, want)
	}
	for _, dir := range []string{DirDownload, DirToWLNordic} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name))
		if err != nil {
			t.Fatalf("read %s/%s: %v", dir, name, err)
		}
		if string(raw) != string(body) {
			t.Fatalf("%s/%s = %q, want %q", dir, name, raw, body)
		}
	}
}

// serveTestSFTP starts a server on an ephemeral port and returns its
// address plus the host key, so a test can exercise host-key pinning.
func serveTestSFTP(t *testing.T, root, password, authorizedKey string) (addr string, hostSigner ssh.Signer) {
	t.Helper()
	hostSigner, err := LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatalf("LoadOrGenerateHostKey: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, ln, root, hostSigner, password, authorizedKey, t.Logf) }()
	return ln.Addr().String(), hostSigner
}

func splitAddr(t *testing.T, addr string) (host, port string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return host, port
}

func TestClientListsAndDownloadsWhatPublishWrote(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	plaintext := []byte("settlement file body\n")
	name, err := Publish(root, "20260904084200_Worldline_Settlement_ER_EUR.csv", plaintext)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Beside it, a file someone left encrypted: the client still decrypts
	// a .pgp name, as it does the enrolment receipts.
	keys, keyring := testKeys(t)
	sealed, err := keys.EncryptAndSign(plaintext)
	if err != nil {
		t.Fatalf("EncryptAndSign: %v", err)
	}
	const sealedName = "20260904084300_Worldline_Settlement_ER_GBP.csv.pgp"
	if err := os.WriteFile(filepath.Join(root, DirDownload, sealedName), sealed, 0o644); err != nil {
		t.Fatalf("write %s: %v", sealedName, err)
	}

	addr, hostSigner := serveTestSFTP(t, root, "sim-sftp-dev-only", "")
	host, port := splitAddr(t, addr)

	// Pin the host key: the pinned path is the one a real deployment must
	// use, so it is the one the test exercises.
	hostKeyPath := filepath.Join(t.TempDir(), "known_host.pub")
	if err := os.WriteFile(hostKeyPath, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()), 0o644); err != nil {
		t.Fatalf("write host key: %v", err)
	}

	c, err := Dial(ClientConfig{
		Host: host, Port: port, User: "platform",
		Password: "sim-sftp-dev-only", HostKeyPath: hostKeyPath,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	files, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 2 || files[0].Name != name || files[1].Name != sealedName {
		t.Fatalf("List = %v, want %q and %q", files, name, sealedName)
	}

	for _, n := range []string{name, sealedName} {
		got, err := c.Download(n, keyring)
		if err != nil {
			t.Fatalf("Download %s: %v", n, err)
		}
		if string(got) != string(plaintext) {
			t.Fatalf("Download %s returned %q, want %q", n, got, plaintext)
		}
	}
}

func TestClientAuthenticatesWithAKeyWhileAPasswordIsAlsoConfigured(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	// A client keypair, as a real Worldline SFT account uses.
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, encodePEM(pemBlock), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	authorized := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	// Both methods configured on the server at once: the point of the fix
	// is that a password no longer switches key auth off.
	addr, _ := serveTestSFTP(t, root, "sim-sftp-dev-only", authorized)
	host, port := splitAddr(t, addr)

	c, err := Dial(ClientConfig{
		Host: host, Port: port, User: "platform",
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("Dial with key auth: %v", err)
	}
	defer c.Close()
	if _, err := c.List(); err != nil {
		t.Fatalf("List over a key-authenticated session: %v", err)
	}

	c2, err := Dial(ClientConfig{
		Host: host, Port: port, User: "platform",
		Password: "sim-sftp-dev-only",
	})
	if err != nil {
		t.Fatalf("Dial with password auth: %v", err)
	}
	defer c2.Close()
	if _, err := c2.List(); err != nil {
		t.Fatalf("List over a password-authenticated session: %v", err)
	}
}

func TestClientRejectsAnUnpinnedHostKey(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	addr, _ := serveTestSFTP(t, root, "pw", "")
	host, port := splitAddr(t, addr)

	// Pin somebody else's key: the dial must fail rather than quietly
	// trusting whatever answered.
	other, err := LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "other_host_key"))
	if err != nil {
		t.Fatalf("LoadOrGenerateHostKey: %v", err)
	}
	pinned := filepath.Join(t.TempDir(), "wrong.pub")
	if err := os.WriteFile(pinned, ssh.MarshalAuthorizedKey(other.PublicKey()), 0o644); err != nil {
		t.Fatalf("write pinned key: %v", err)
	}

	if _, err := Dial(ClientConfig{Host: host, Port: port, User: "u", Password: "pw", HostKeyPath: pinned}); err == nil {
		t.Fatal("Dial succeeded against a server whose host key was not the pinned one")
	}
}

func TestClientNeedsSomeAuthentication(t *testing.T) {
	if _, err := Dial(ClientConfig{Host: "127.0.0.1", Port: "1", User: "u"}); err == nil {
		t.Fatal("Dial with no password and no key succeeded, want an error")
	}
}

// encodePEM renders a *pem.Block, kept here so the test above reads as one
// flow rather than importing encoding/pem at the top for a single call.
func encodePEM(b *pem.Block) []byte { return pem.EncodeToMemory(b) }
