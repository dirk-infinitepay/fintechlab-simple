package wlsftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// accountServer serves root with the server's own login (password,
// authorizedKey) and one account, "acme", jailed at its own folder. It
// returns the address and every session the observer saw.
func accountServer(t *testing.T, password, authorizedKey string) (addr, root, acmeRoot string, seen func() []Session) {
	t.Helper()
	base := t.TempDir()
	root, acmeRoot = filepath.Join(base, "root"), filepath.Join(base, "accounts", "acme")
	for _, r := range []string{root, acmeRoot} {
		if err := EnsureLayout(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, DirDownload, "server.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acmeRoot, DirDownload, "acme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, err := LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	var sessions []Session
	srv := Server{
		Root: root, HostSigner: host, Password: password, AuthorizedKey: authorizedKey, Logf: t.Logf,
		Accounts: func(user string) []Account {
			if user != "acme" {
				return nil
			}
			return []Account{{User: "acme", Password: "acme-pw", Root: acmeRoot, Tag: "acme-env"}}
		},
		Observe: func(s Session, op, path string, err error) {
			if op == "session" {
				mu.Lock()
				sessions = append(sessions, s)
				mu.Unlock()
			}
		},
	}
	go func() { _ = srv.Serve(ctx, ln) }()
	return ln.Addr().String(), root, acmeRoot, func() []Session {
		mu.Lock()
		defer mu.Unlock()
		return append([]Session(nil), sessions...)
	}
}

func dialAs(t *testing.T, addr, user string, auth ...ssh.AuthMethod) (*sftp.Client, error) {
	t.Helper()
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	t.Cleanup(func() { c.Close(); conn.Close() })
	return c, nil
}

func names(t *testing.T, c *sftp.Client) string {
	t.Helper()
	infos, err := c.ReadDir(DirDownload)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, fi := range infos {
		out = append(out, fi.Name())
	}
	return strings.Join(out, ",")
}

// An account's username and password open its own jail, tagged as its;
// its username with anything else is refused; every other login is the
// server's own, as on a server without accounts.
func TestAnAccountIsJailedInItsOwnRoot(t *testing.T) {
	addr, _, _, seen := accountServer(t, "server-pw", "")

	acme, err := dialAs(t, addr, "acme", ssh.Password("acme-pw"))
	if err != nil {
		t.Fatalf("acme's login: %v", err)
	}
	if got := names(t, acme); got != "acme.txt" {
		t.Errorf("acme sees %q, want its own acme.txt", got)
	}
	if f, err := acme.Open("../../root/download/server.txt"); err == nil {
		f.Close()
		t.Error("acme reached the server's root")
	}
	if _, err := dialAs(t, addr, "acme", ssh.Password("server-pw")); err == nil {
		t.Error("acme's username logged in with the server's password")
	}
	other, err := dialAs(t, addr, "anyone", ssh.Password("server-pw"))
	if err != nil {
		t.Fatalf("an unknown username with the server's password: %v", err)
	}
	if got := names(t, other); got != "server.txt" {
		t.Errorf("an unknown login sees %q, want the server's", got)
	}
	if _, err := dialAs(t, addr, "anyone", ssh.Password("acme-pw")); err == nil {
		t.Error("an account's password let another username in")
	}

	// The server records a refusal after the client has it.
	deadline := time.Now().Add(5 * time.Second)
	for len(seen()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var tags []string
	for _, s := range seen() {
		tags = append(tags, s.User+"/"+s.Tag)
	}
	// The refusal of acme's username is acme's to see; the others are the
	// server's.
	if got := strings.Join(tags, " "); got != "acme/acme-env /acme-env anyone/ /" {
		t.Errorf("sessions seen: %q", got)
	}
}

// An account takes only its password: not a key, even one the server's own
// login accepts, and not anything at all on a server that accepts anything.
func TestAnAccountTakesOnlyItsPassword(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	authorized := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	addr, _, _, _ := accountServer(t, "", authorized)
	if _, err := dialAs(t, addr, "acme", ssh.PublicKeys(signer)); err == nil {
		t.Error("acme's username logged in with the server's key")
	}
	if c, err := dialAs(t, addr, "acme", ssh.PublicKeys(signer), ssh.Password("acme-pw")); err != nil {
		t.Errorf("acme's key-then-password login: %v", err)
	} else if got := names(t, c); got != "acme.txt" {
		t.Errorf("acme sees %q", got)
	}
	if _, err := dialAs(t, addr, "anyone", ssh.Password("whatever")); err == nil {
		t.Error("a key-only server took a password for its own login")
	}
	if _, err := dialAs(t, addr, "anyone", ssh.PublicKeys(signer)); err != nil {
		t.Errorf("the server's own key login: %v", err)
	}

	open, _, _, _ := accountServer(t, "", "")
	if _, err := dialAs(t, open, "acme", ssh.Password("guess")); err == nil {
		t.Error("an open server let acme's username in without its password")
	}
	if _, err := dialAs(t, open, "anyone", ssh.Password("guess")); err != nil {
		t.Errorf("an open server refused its own login: %v", err)
	}
}
