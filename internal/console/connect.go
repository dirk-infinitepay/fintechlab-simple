package console

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
)

// Connecting a platform to the lab.
//
// Every vendor here is only useful once the platform under test is pointed
// at it, and pointing it means two things: a handful of environment
// variables (addresses, account ids, lab credentials) and, for the vendors
// that exchange keys, the key material itself. Both used to be copied by
// hand out of compose.yml and the key directories. A Kit is that copy, done
// once, per vendor, in the platform's own variable names -- the ones the
// Infinite apps read (docs/integration/).
//
// Two sources, kept apart and said so in the file itself. Keys and
// certificates are read from the running lab's keys directory, so they are
// the ones it is actually using. Everything else is what compose.yml ships
// -- the console cannot read another container's environment -- and a test
// holds these constants to compose.yml so the two cannot drift.

// Lab constants a platform needs, as compose.yml sets them.
const (
	kitSFTPUser       = "infinitepay"
	kitSFTPPassword   = "sim-sftp-dev-only"
	kitSettlementID   = "Worldline_Settlement"
	kitB4BKeyID       = "b4b-mock-1"
	kitBCUser         = "sim"
	kitBCPassword     = "sim-bc-dev-only"
	kitSGAEUR         = "00000000-0000-4000-8000-000000000978"
	kitSGAGBP         = "00000000-0000-4000-8000-000000000826"
	kitVerifyKey      = "sim-verify-key-dev-only"
	kitACISecret      = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	kitVerificationID = "sim"
	// The verification vendors accept any non-empty credential; this one
	// says where it came from.
	kitVerificationSecret = "sim-verification-dev-only"
)

// KitVar is one environment variable. Value may span lines (a PEM), which
// the .env writes double-quoted, the way dotenv and the platform's own
// .env already carry keys.
type KitVar struct {
	Key   string `json:"key"`
	Value string `json:"-"`
	Note  string `json:"note,omitempty"`
	// Optional variables are written commented out: they change how the
	// platform behaves beyond pointing it here, so they are a decision, not
	// a default.
	Optional bool `json:"optional,omitempty"`
	// Missing names the key file this variable needed and could not read,
	// so the .env says why a value is empty instead of shipping a blank.
	Missing string `json:"missing,omitempty"`
}

// KitFile is one downloadable key or certificate. Path is relative to the
// keys directory and comes only from this file -- never from a request --
// so no URL can reach a server's private key or the CA's.
type KitFile struct {
	Name    string `json:"name"` // what it downloads as
	Path    string `json:"-"`
	Purpose string `json:"purpose"`
	Present bool   `json:"present"`
}

// Kit is everything one vendor asks of a platform that wants to talk to it.
type Kit struct {
	Service string    `json:"service"`
	Title   string    `json:"title"`
	Note    string    `json:"note,omitempty"`
	Vars    []KitVar  `json:"vars"`
	Files   []KitFile `json:"files"`
}

// ErrNoSuchFile is a download name no kit hands out.
var ErrNoSuchFile = errors.New("no such file in this kit")

// Kits builds every vendor's kit for a platform that reaches the lab at
// host, reading key material from keysDir. A key that has not been
// generated yet is reported on its variable, not fatal: the rest of the
// file is still right.
func Kits(cat *Catalogue, host, keysDir string) []Kit {
	b := kitBuilder{cat: cat, host: host, dir: keysDir}
	var out []Kit
	for _, k := range []Kit{b.worldline(), b.b4b(), b.bankingCircle(), b.aci(), b.verification(), b.verify()} {
		if _, ok := cat.Get(k.Service); ok {
			out = append(out, k)
		}
	}
	return out
}

// KitFor is one service's kit.
func KitFor(cat *Catalogue, host, keysDir, service string) (Kit, bool) {
	for _, k := range Kits(cat, host, keysDir) {
		if k.Service == service {
			return k, true
		}
	}
	return Kit{}, false
}

// KitFilePath resolves a download name to a file on disk, for that kit
// only.
func KitFilePath(k Kit, keysDir, name string) (string, error) {
	for _, f := range k.Files {
		if f.Name == name {
			return filepath.Join(keysDir, f.Path), nil
		}
	}
	return "", fmt.Errorf("%w: %s has no %q", ErrNoSuchFile, k.Service, name)
}

type kitBuilder struct {
	cat  *Catalogue
	host string
	dir  string
}

// published is one of a service's ports as the catalogue lists it
// ("8085/https+mtls"), so a port moved there moves here too.
func (b kitBuilder) published(service, kind string) (scheme, port string) {
	s, _ := b.cat.Get(service)
	for _, p := range s.Ports {
		num, k, _ := strings.Cut(p, "/")
		if strings.HasPrefix(k, kind) {
			if strings.HasPrefix(k, "https") {
				return "https", num
			}
			return "http", num
		}
	}
	return "http", ""
}

func (b kitBuilder) url(service, kind, path string) string {
	scheme, port := b.published(service, kind)
	return scheme + "://" + net.JoinHostPort(b.host, port) + path
}

func (b kitBuilder) port(service, kind string) string {
	_, port := b.published(service, kind)
	return port
}

func (b kitBuilder) file(name, path, purpose string) KitFile {
	_, err := os.Stat(filepath.Join(b.dir, path))
	return KitFile{Name: name, Path: path, Purpose: purpose, Present: err == nil}
}

// inline is a variable whose value is a key file's contents, optionally
// transformed (base64 for the variables that want one line).
func (b kitBuilder) inline(key, path, note string, enc func([]byte) string) KitVar {
	v := KitVar{Key: key, Note: note}
	raw, err := os.ReadFile(filepath.Join(b.dir, path))
	if err != nil {
		v.Missing = "keys/" + path + " is not there yet — the service that generates it has not started"
		return v
	}
	if enc == nil {
		v.Value = strings.TrimSpace(string(raw))
	} else {
		v.Value = enc(raw)
	}
	return v
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func (b kitBuilder) worldline() Kit {
	fp := KitVar{Key: "WORLDLINE_SFTP_HOST_KEY_FINGERPRINT",
		Note: "Pins the SFTP server's host key, derived from keys/wlsftp-keys/host_key.pub."}
	if raw, err := os.ReadFile(filepath.Join(b.dir, "wlsftp-keys/host_key.pub")); err != nil {
		fp.Missing = "keys/wlsftp-keys/host_key.pub is not there yet — start worldline"
	} else if pub, _, _, _, err := ssh.ParseAuthorizedKey(raw); err != nil {
		fp.Missing = "keys/wlsftp-keys/host_key.pub is not a public key: " + err.Error()
	} else {
		fp.Value = ssh.FingerprintSHA256(pub)
	}
	return Kit{
		Service: "worldline", Title: "Worldline",
		Note: "Real SFTP with PGP-encrypted, signed settlement files. The lab has no separate platform keypair: it encrypts to its own key, so the private key that decrypts the files is Worldline's.",
		Vars: []KitVar{
			{Key: "WORLDLINE_SFTP_HOST", Value: b.host},
			{Key: "WORLDLINE_SFTP_PORT", Value: b.port("worldline", "ssh")},
			{Key: "WORLDLINE_SFTP_USERNAME", Value: kitSFTPUser, Note: "The lab checks only the password."},
			{Key: "WORLDLINE_SFTP_PASSWORD", Value: kitSFTPPassword},
			fp,
			{Key: "WORLDLINE_SFTP_DOWNLOAD_DIR", Value: wlsftp.DirDownload},
			{Key: "WORLDLINE_SFTP_RECONCILIATION_DIR", Value: wlsftp.DirDownload},
			{Key: "WORLDLINE_SFTP_DISPUTES_DIR", Value: wlsftp.DirDisputesDownload},
			{Key: "WORLDLINE_SFTP_UPLOAD_DIR", Value: wlsftp.DirToWLNordic},
			{Key: "WORLDLINE_SETTLEMENT_IDENTIFIER", Value: kitSettlementID},
			b.inline("WORLDLINE_PGP_PUBLIC_KEY", "wlsftp-keys/worldline_public.asc", "Verifies the files' signatures.", nil),
			b.inline("INFINITE_PGP_PRIVATE_KEY", "wlsftp-keys/worldline_private.asc", "Decrypts the settlement files.", nil),
			{Key: "WORLDLINE_PGP_VERIFY_DOWNLOAD_SIGNATURES", Value: "true"},
		},
		Files: []KitFile{
			b.file("worldline-pgp-public.asc", "wlsftp-keys/worldline_public.asc", "Worldline's PGP public key: verifies what it signs."),
			b.file("worldline-pgp-private.asc", "wlsftp-keys/worldline_private.asc", "The PGP private key that decrypts the settlement files."),
			b.file("worldline-sftp-host-key.pub", "wlsftp-keys/host_key.pub", "The SFTP server's host key, to pin (known_hosts)."),
		},
	}
}

func (b kitBuilder) b4b() Kit {
	return Kit{
		Service: "b4b", Title: "B4B Payments",
		Note: "Every call carries an RS512 JWT. The lab generates the keypair and verifies with its public half, so the platform signs with the private key below — inverted from a real vendor, where the client holds its own.",
		Vars: []KitVar{
			{Key: "B4B_API_BASE_URL", Value: b.url("b4b", "http", "")},
			{Key: "B4B_JWT_KEY_ID", Value: kitB4BKeyID},
			b.inline("B4B_JWT_PRIVATE_KEY", "b4b-keys/private.pem", "Signs the bearer token (aud b4b-payments).", nil),
		},
		Files: []KitFile{
			b.file("b4b-jwt-private.pem", "b4b-keys/private.pem", "Signs the RS512 bearer token."),
			b.file("b4b-jwt-public.pem", "b4b-keys/public.pem", "What B4B verifies the token with."),
		},
	}
}

func (b kitBuilder) bankingCircle() Kit {
	api := b.url("banking-circle", "https", "")
	return Kit{
		Service: "banking-circle", Title: "Banking Circle",
		Note: "HTTPS with a mutual-TLS client certificate, then Basic → Bearer. The server certificate is signed by the lab's own CA, which Node has to be told to trust: NODE_EXTRA_CA_CERTS is a path, so download the CA and point it there.",
		Vars: []KitVar{
			{Key: "BC_AUTH_BASE_URL", Value: api},
			{Key: "BC_API_BASE_URL", Value: api},
			{Key: "BC_API_M2M_USERNAME", Value: kitBCUser, Note: "Not checked by the lab; the token exchange is."},
			{Key: "BC_API_M2M_PASSWORD", Value: kitBCPassword},
			{Key: "BC_API_MTLS_ENABLED", Value: "true"},
			b.inline("BC_API_CLIENT_CERT_PEM_BASE64", "certs/client.pem", "", b64),
			b.inline("BC_API_CLIENT_KEY_PEM_BASE64", "certs/client-key.pem", "Not encrypted: leave BC_API_CLIENT_KEY_PASSPHRASE unset.", b64),
			{Key: "BC_SAFEGUARDING_ACCOUNT_ID_EUR", Value: kitSGAEUR},
			{Key: "BC_SAFEGUARDING_ACCOUNT_ID_GBP", Value: kitSGAGBP},
			{Key: "NODE_EXTRA_CA_CERTS", Value: "./fintechlab-ca.pem", Optional: true,
				Note: "A path, read by Node at start: save fintechlab-ca.pem and point this at it."},
		},
		Files: []KitFile{
			b.file("fintechlab-ca.pem", "certs/ca.pem", "The lab's CA: trust it to reach Banking Circle over TLS."),
			b.file("bc-client.pem", "certs/client.pem", "The mTLS client certificate."),
			b.file("bc-client-key.pem", "certs/client-key.pem", "Its private key."),
		},
	}
}

func (b kitBuilder) aci() Kit {
	return Kit{
		Service: "aci", Title: "ACI",
		Note: "ACI calls the platform, not the other way round: its webhooks go to ACI_WEBHOOK_TARGET_URL on the aci service (compose.yml). This is the key they are encrypted with.",
		Vars: []KitVar{
			{Key: "ACI_WEBHOOK_SECRET", Value: kitACISecret, Note: "Hex; 32 raw bytes of AES-256-GCM key."},
		},
		Files: []KitFile{},
	}
}

func (b kitBuilder) verification() Kit {
	base := b.url("verification", "http", "")
	return Kit{
		Service: "verification", Title: "Verification vendors",
		Note: "Four vendors behind one address, each under its own path. Credentials are required but not checked; tokens are.",
		Vars: []KitVar{
			{Key: "CREDITSAFE_API_URL", Value: base + "/creditsafe"},
			{Key: "CREDITSAFE_USERNAME", Value: kitVerificationID},
			{Key: "CREDITSAFE_PASSWORD", Value: kitVerificationSecret},
			{Key: "IBAN_COM_BASE_URL", Value: base + "/iban/verify", Note: "For iban.com the base URL is the endpoint."},
			{Key: "IBAN_COM_API_KEY", Value: kitVerificationSecret},
			{Key: "KYC6_API_BASE_URL", Value: base + "/kyc6"},
			{Key: "KYC6_MONITOR_API_BASE_URL", Value: base + "/kyc6"},
			{Key: "KYC6_API_KEY", Value: kitVerificationSecret},
			{Key: "LEXISNEXIS_AUTH_URL", Value: base + "/lexisnexis"},
			{Key: "LEXISNEXIS_IDU_BASE_URL", Value: base + "/lexisnexis/idu"},
			{Key: "LEXISNEXIS_IVI_BASE_URL", Value: base + "/lexisnexis/ivi"},
			{Key: "LEXISNEXIS_CLIENT_ID", Value: kitVerificationID},
			{Key: "LEXISNEXIS_CLIENT_SECRET", Value: kitVerificationSecret},
			{Key: "LEXISNEXIS_IVI_API_KEY", Value: kitVerificationSecret},
		},
		Files: []KitFile{},
	}
}

func (b kitBuilder) verify() Kit {
	return Kit{
		Service: "verify", Title: "AML decision",
		Note: "Replaces the platform's own verification-service with the lab's always-approve stub — which is why it is commented out.",
		Vars: []KitVar{
			{Key: "VERIFICATION_SERVICE_API_URL", Value: b.url("verify", "http", "/api/v1/verification"), Optional: true},
			{Key: "VERIFICATION_INTERNAL_API_KEY", Value: kitVerifyKey, Optional: true},
		},
		Files: []KitFile{},
	}
}

// EnvFile renders kits as one .env. The header says where the values came
// from, because a file that outlives the lab it was downloaded from is a
// file somebody will trust after the keys it carries were regenerated.
func EnvFile(kits []Kit, host string, at time.Time) string {
	var sb strings.Builder
	sb.WriteString("# fintech sim lab v1 — connection settings for a platform under test\n")
	fmt.Fprintf(&sb, "# Downloaded from the lab API %s, for a lab reached at %s.\n", at.UTC().Format(time.RFC3339), host)
	sb.WriteString("#\n" +
		"# Keys and certificates are the ones the lab was running with at that\n" +
		"# moment: regenerate the lab's keys and download this again. Addresses,\n" +
		"# account ids and credentials are what compose.yml ships. Everything\n" +
		"# here is fake-and-obvious lab material. Lines left commented out are\n" +
		"# choices, not defaults — read the note above each.\n")
	for _, k := range kits {
		fmt.Fprintf(&sb, "\n# ── %s %s\n", k.Title, strings.Repeat("─", max(3, 66-len([]rune(k.Title)))))
		if k.Note != "" {
			for _, line := range wrap(k.Note, 74) {
				sb.WriteString("# " + line + "\n")
			}
		}
		for _, v := range k.Vars {
			if v.Note != "" {
				sb.WriteString("# " + v.Note + "\n")
			}
			if v.Missing != "" {
				sb.WriteString("# " + v.Missing + "\n")
			}
			prefix := ""
			if v.Optional {
				prefix = "# "
			}
			sb.WriteString(prefix + v.Key + "=" + envQuote(v.Value) + "\n")
		}
	}
	return sb.String()
}

// envQuote writes a value the way dotenv reads it back: bare when it is one
// plain line, double-quoted when it spans lines (a PEM) or has a space or a
// '#' a parser would take for a comment. Nothing is escaped: no value a kit
// carries contains a quote or a backslash, and dotenv would not unescape
// one anyway.
func envQuote(v string) string {
	if !strings.ContainsAny(v, "\n \t#") {
		return v
	}
	return `"` + v + `"`
}

func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len([]rune(line))+1+len([]rune(w)) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
