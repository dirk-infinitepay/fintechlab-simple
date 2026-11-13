package console

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// keysFixture lays out a keys directory the way the lab does, with a real
// SSH host key so the fingerprint is the genuine article.
func keysFixture(t *testing.T) (dir, fingerprint string) {
	t.Helper()
	dir = t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	write("wlsftp-keys/host_key.pub", string(ssh.MarshalAuthorizedKey(sshPub)))
	write("wlsftp-keys/host_key", "SERVER PRIVATE KEY")
	write("wlsftp-keys/worldline_public.asc", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nabc\n-----END PGP PUBLIC KEY BLOCK-----\n")
	write("wlsftp-keys/worldline_private.asc", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\ndef\n-----END PGP PRIVATE KEY BLOCK-----\n")
	write("b4b-keys/private.pem", "-----BEGIN RSA PRIVATE KEY-----\nb4b\n-----END RSA PRIVATE KEY-----\n")
	write("b4b-keys/public.pem", "-----BEGIN PUBLIC KEY-----\nb4b\n-----END PUBLIC KEY-----\n")
	write("certs/ca.pem", "CA CERT")
	write("certs/ca-key.pem", "CA PRIVATE KEY")
	write("certs/client.pem", "CLIENT CERT")
	write("certs/client-key.pem", "CLIENT KEY")
	write("certs/banking-circle-key.pem", "SERVER PRIVATE KEY")
	return dir, ssh.FingerprintSHA256(sshPub)
}

func envOf(k Kit) map[string]KitVar {
	out := map[string]KitVar{}
	for _, v := range k.Vars {
		out[v.Key] = v
	}
	return out
}

func TestKitsCarryTheKeysTheLabIsRunningWith(t *testing.T) {
	dir, fp := keysFixture(t)
	cat := DefaultCatalogue()

	wl, _ := KitFor(cat, "127.0.0.1", dir, "worldline")
	env := envOf(wl)
	if got := env["WORLDLINE_SFTP_HOST_KEY_FINGERPRINT"].Value; got != fp {
		t.Errorf("fingerprint %q, want %q", got, fp)
	}
	if got := env["INFINITE_PGP_PRIVATE_KEY"].Value; !strings.Contains(got, "BEGIN PGP PRIVATE KEY BLOCK") {
		t.Errorf("PGP private key not inlined: %q", got)
	}
	if got := env["WORLDLINE_SFTP_PORT"].Value; got != "2222" {
		t.Errorf("SFTP port %q, want the catalogue's 2222", got)
	}

	bc, _ := KitFor(cat, "127.0.0.1", dir, "banking-circle")
	env = envOf(bc)
	if got := env["BC_API_BASE_URL"].Value; got != "https://127.0.0.1:8085" {
		t.Errorf("BC_API_BASE_URL %q", got)
	}
	if got := env["BC_API_CLIENT_CERT_PEM_BASE64"].Value; got != base64.StdEncoding.EncodeToString([]byte("CLIENT CERT")) {
		t.Errorf("client cert not base64 of the file: %q", got)
	}

	b4b, _ := KitFor(cat, "127.0.0.1", dir, "b4b")
	if got := envOf(b4b)["B4B_API_BASE_URL"].Value; got != "http://127.0.0.1:8086" {
		t.Errorf("B4B_API_BASE_URL %q", got)
	}
}

// A cloud lab's front serves some services at a name of their own: those
// are https://<service>.<host> on 443, and only those; SFTP stays on the
// host.
func TestANamedServiceIsReachedAtItsOwnName(t *testing.T) {
	dir, _ := keysFixture(t)
	cat := DefaultCatalogue()
	named := []string{"b4b", "banking-circle", "verify", "creditsafe"}

	b4b, _ := KitFor(cat, "lab.example.test", dir, "b4b", named...)
	if got := envOf(b4b)["B4B_API_BASE_URL"].Value; got != "https://b4b.lab.example.test" {
		t.Errorf("B4B_API_BASE_URL %q", got)
	}
	bc, _ := KitFor(cat, "lab.example.test", dir, "banking-circle", named...)
	env := envOf(bc)
	for _, k := range []string{"BC_API_BASE_URL", "BC_AUTH_BASE_URL"} {
		if got := env[k].Value; got != "https://banking-circle.lab.example.test" {
			t.Errorf("%s %q", k, got)
		}
	}
	if !strings.Contains(bc.Note, "public certificate") {
		t.Errorf("Banking Circle's note still asks to trust the lab's CA for a public name: %q", bc.Note)
	}
	vf, _ := KitFor(cat, "lab.example.test", dir, "verify", named...)
	if got := envOf(vf)["VERIFICATION_SERVICE_API_URL"].Value; got != "https://verify.lab.example.test/api/v1/verification" {
		t.Errorf("VERIFICATION_SERVICE_API_URL %q: a path must survive the name", got)
	}
	cs, _ := KitFor(cat, "lab.example.test", dir, "creditsafe", named...)
	if got := envOf(cs)["CREDITSAFE_API_URL"].Value; got != "https://creditsafe.lab.example.test/creditsafe" {
		t.Errorf("CREDITSAFE_API_URL %q", got)
	}
	// Not named: host:port as ever.
	kn, _ := KitFor(cat, "lab.example.test", dir, "kyc6", named...)
	if got := envOf(kn)["KYC6_API_BASE_URL"].Value; got != "http://lab.example.test:8092/kyc6" {
		t.Errorf("KYC6_API_BASE_URL %q", got)
	}
	wl, _ := KitFor(cat, "lab.example.test", dir, "worldline", named...)
	if got := envOf(wl)["WORLDLINE_SFTP_HOST"].Value; got != "lab.example.test" {
		t.Errorf("WORLDLINE_SFTP_HOST %q: SFTP is not behind the front", got)
	}
}

func TestAnIPv6HostIsBracketedInURLsButNotInTheSFTPHost(t *testing.T) {
	dir, _ := keysFixture(t)
	cat := DefaultCatalogue()
	b4b, _ := KitFor(cat, "::1", dir, "b4b")
	if got := envOf(b4b)["B4B_API_BASE_URL"].Value; got != "http://[::1]:8086" {
		t.Errorf("B4B_API_BASE_URL %q", got)
	}
	wl, _ := KitFor(cat, "::1", dir, "worldline")
	if got := envOf(wl)["WORLDLINE_SFTP_HOST"].Value; got != "::1" {
		t.Errorf("WORLDLINE_SFTP_HOST %q", got)
	}
}

// The console serves only what a client holds. A server's private key or
// the CA's would let whoever downloaded it impersonate the lab, and there
// is no URL shape that should be able to reach one.
func TestNoKitHandsOutAServerOrCAPrivateKey(t *testing.T) {
	dir, _ := keysFixture(t)
	forbidden := map[string]bool{
		"certs/ca-key.pem": true, "certs/banking-circle-key.pem": true,
		"certs/receiver-key.pem": true, "certs/pod-bank-rails-key.pem": true,
		"wlsftp-keys/host_key": true,
	}
	for _, k := range Kits(DefaultCatalogue(), "127.0.0.1", dir) {
		for _, f := range k.Files {
			if forbidden[f.Path] {
				t.Errorf("%s hands out %s", k.Service, f.Path)
			}
		}
		for _, name := range []string{"../tls/ca-key.pem", "ca-key.pem", "host_key", "", "."} {
			if _, err := KitFilePath(k, dir, name); !errors.Is(err, ErrNoSuchFile) {
				t.Errorf("%s: %q resolved (err %v)", k.Service, name, err)
			}
		}
	}
}

func TestAKeyNotYetGeneratedSaysSoInsteadOfShippingABlank(t *testing.T) {
	empty := t.TempDir()
	cat := DefaultCatalogue()
	b4b, _ := KitFor(cat, "127.0.0.1", empty, "b4b")
	v := envOf(b4b)["B4B_JWT_PRIVATE_KEY"]
	if v.Value != "" || !strings.Contains(v.Missing, "keys/b4b-keys/private.pem") {
		t.Errorf("want an empty value and the missing file named, got %+v", v)
	}
	if b4b.Files[0].Present {
		t.Error("a file that is not there is reported present")
	}
	out := EnvFile([]Kit{b4b}, "127.0.0.1", time.Unix(0, 0))
	if !strings.Contains(out, "# keys/b4b-keys/private.pem is not there yet") {
		t.Errorf("the .env does not say why the key is empty:\n%s", out)
	}
}

func TestTheEnvFileQuotesPEMsAndCommentsOutChoices(t *testing.T) {
	dir, _ := keysFixture(t)
	cat := DefaultCatalogue()
	out := EnvFile(Kits(cat, "127.0.0.1", dir), "127.0.0.1", time.Unix(0, 0))

	if !strings.Contains(out, "B4B_JWT_PRIVATE_KEY=\"-----BEGIN RSA PRIVATE KEY-----\nb4b\n-----END RSA PRIVATE KEY-----\"\n") {
		t.Errorf("multi-line key not written double-quoted:\n%s", out)
	}
	if !strings.Contains(out, "\n# VERIFICATION_SERVICE_API_URL=http://127.0.0.1:8088/api/v1/verification\n") {
		t.Error("the AML stub replaces the platform's own service, so it must be commented out")
	}
	if !strings.Contains(out, "\nB4B_JWT_KEY_ID=b4b-mock-1\n") {
		t.Error("a plain value should be written bare")
	}
	// Every uncommented line is KEY=value or inside a quoted value.
	inQuote := false
	for _, line := range strings.Split(out, "\n") {
		if inQuote {
			inQuote = !strings.HasSuffix(line, `"`)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ToUpper(k) != k {
			t.Errorf("not a variable: %q", line)
		}
		inQuote = strings.HasPrefix(v, `"`) && !(len(v) > 1 && strings.HasSuffix(v, `"`))
	}
}

// The constants a kit carries are compose.yml's values, repeated because
// the console cannot read another container's environment. This is what
// stops the two drifting apart: change one without the other and a
// downloaded .env stops working in a way that points nowhere near here.
func TestKitConstantsAreWhatComposeShips(t *testing.T) {
	b, err := os.ReadFile("../../compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ service, key, want string }{
		{"worldline", "WORLDLINE_SFTP_PASSWORD", kitSFTPPassword},
		{"settlement", "WORLDLINE_SFTP_USER", kitSFTPUser},
		{"worldline", "WORLDLINE_SETTLEMENT_IDENTIFIER", kitSettlementID},
		{"worldline", "WORLDLINE_SFTP_PORT", "2222"},
		{"b4b", "B4B_JWT_KEY_ID", kitB4BKeyID},
		{"worldline", "BC_SAFEGUARDING_ACCOUNT_ID_EUR", kitSGAEUR},
		{"worldline", "BC_SAFEGUARDING_ACCOUNT_ID_GBP", kitSGAGBP},
		{"verify", "VERIFICATION_INTERNAL_API_KEY", kitVerifyKey},
		{"aci", "ACI_WEBHOOK_SECRET", kitACISecret},
		{"banking-circle", "BC_MTLS", "require"},
	} {
		svc, ok := c.Services[tc.service]
		if !ok {
			t.Errorf("compose.yml has no service %q", tc.service)
			continue
		}
		if got := svc.Environment[tc.key]; got != tc.want {
			t.Errorf("%s %s: compose.yml ships %q, the kit says %q", tc.service, tc.key, got, tc.want)
		}
	}
}
