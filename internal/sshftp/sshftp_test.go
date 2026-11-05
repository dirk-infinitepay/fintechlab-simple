package sshftp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func TestEnsureLayoutRefusesEscape(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root, []string{"foo", "../etc"}); err == nil {
		t.Fatal("expected escape error")
	}
}

func TestEnsureLayoutCreatesNested(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root, []string{"foo", "bar/baz"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"foo", "bar/baz"} {
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); err != nil || !st.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
	}
}

func TestPGPRoundTrip(t *testing.T) {
	dir := t.TempDir()
	e, err := LoadOrGenerateKeypair(filepath.Join(dir, "pub.asc"), filepath.Join(dir, "priv.asc"), "lab", "test", "lab@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("settlement-bytes")
	enc, err := (&Keys{Own: e}).EncryptAndSign(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(enc, openpgp.EntityList{e})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q", got)
	}
}

func TestSFTPRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := EnsureLayout(root, []string{"in", "out"}); err != nil {
		t.Fatal(err)
	}
	want := []byte("hello-sftp")
	if err := os.WriteFile(filepath.Join(root, "out", "a.csv"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	signer, err := LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := ListenAndServe(ctx, "127.0.0.1:0", root, signer, Auth{Password: "sim-sftp-dev-only"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := &ssh.ClientConfig{
		User:            "lab",
		Auth:            []ssh.AuthMethod{ssh.Password("sim-sftp-dev-only")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	conn, err := ssh.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := c.Open("out/a.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("got %q", raw)
	}
}

func TestSFTPJail(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "jail")
	if err := EnsureLayout(root, []string{"download"}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside_secret.txt")
	if err := os.WriteFile(outside, []byte("PRIVATE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "download", "ok.csv"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the jail pointing at the host secret.
	if err := os.Symlink(outside, filepath.Join(root, "download", "link")); err != nil {
		t.Fatal(err)
	}

	signer, err := LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := ListenAndServe(ctx, "127.0.0.1:0", root, signer, Auth{Password: "sim-sftp-dev-only"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := &ssh.ClientConfig{
		User:            "lab",
		Auth:            []ssh.AuthMethod{ssh.Password("sim-sftp-dev-only")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	conn, err := ssh.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got, err := c.Open("download/ok.csv")
	if err != nil {
		t.Fatalf("legit open: %v", err)
	}
	body, _ := io.ReadAll(got)
	got.Close()
	if string(body) != "inside" {
		t.Fatalf("legit read %q", body)
	}

	mustNotRead := func(name, p string) {
		t.Helper()
		f, err := c.Open(p)
		if err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if bytes.Contains(b, []byte("PRIVATE")) {
				t.Fatalf("%s: read host secret via %q", name, p)
			}
		}
	}
	mustNotRead("absolute", outside)
	mustNotRead("relative ..", "../outside_secret.txt")
	mustNotRead("posix climb", "/../outside_secret.txt")
	mustNotRead("symlink", "download/link")

	escapeWrite := filepath.Join(base, "escaped.txt")
	wf, err := c.OpenFile(outside, os.O_CREATE|os.O_WRONLY)
	if err == nil {
		_, _ = wf.Write([]byte("pwned"))
		wf.Close()
	}
	if _, err := os.Stat(escapeWrite); err == nil {
		t.Fatal("write created a file next to the jail")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "PRIVATE" {
		t.Fatalf("host secret mutated: %s %v", data, err)
	}
}

func TestJailResolveNeverLeavesRoot(t *testing.T) {
	root := t.TempDir()
	j, err := newJail(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/etc/passwd", "../secret", "/../../etc/passwd", outsidePath(root)} {
		full, err := j.resolve(p)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(j.root, full)
		if err != nil || rel == ".." || stringsHasDotDot(rel) {
			t.Fatalf("resolve(%q) escaped to %q", p, full)
		}
	}
}

func outsidePath(root string) string {
	return filepath.Join(filepath.Dir(root), "outside")
}

func stringsHasDotDot(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}
