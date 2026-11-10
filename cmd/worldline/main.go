// Worldline simulates the acquirer: it holds the card transactions it has
// acquired for each submerchant (MID), cuts the daily settlement file from
// them in two slots (a morning file and an afternoon confirmation),
// publishes them PGP-encrypted on a real SSH/SFTP server, and wires the
// matching lump sum to the platform's safeguarding account.
//
// It also serves the SFT file-exchange channel over HTTP: merchant
// onboarding documents and settlement correction files flow in, daily WX
// files and financial reports flow out, and a retrieved outbound file is
// archived by year/month. See docs/ARCHITECTURE-worldline-sftp-channel.md
// and docs/ARCHITECTURE-worldline-acquirer.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/ssh"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/sftpgateway"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// maxUploadBytes bounds a single inbound HTTP request body. Slightly above
// Channel's own MaxInboundFileSize so the channel's own error message (not
// a truncated body) is what rejects an oversized upload.
const maxUploadBytes = sftpgateway.MaxInboundFileSize + (1 << 10)

func main() {
	logring.Install()
	addr := env("LISTEN", ":8084")
	labclock.FollowEnv(context.Background(), "worldline")
	baseDir := env("SFTP_BASE_DIR", "/sftp")
	merchantID := env("MERCHANT_ID", "GB00SIM0000000000003")

	ch := &sftpgateway.Channel{BaseDir: baseDir}
	if err := ensureLayout(baseDir); err != nil {
		log.Fatalf("worldline: create directory layout: %v", err)
	}
	// The real deployment bind-mounts sftp/config/merchant-onboarding.json
	// (seeded at the repo root) into {SFTP_BASE_DIR}/config. A standalone
	// run (no compose, no mount) still needs a working /config endpoint, so
	// seed a matching default here if nothing is present yet.
	if err := seedDefaultConfig(ch, merchantID); err != nil {
		log.Fatalf("worldline: seed default config: %v", err)
	}

	lab := &sim{ch: ch, merchantID: merchantID}
	sshAddr := lab.startWorldlineSFTP()
	mux, ctl, err := lab.surfaces(addr, sshAddr)
	if err != nil {
		log.Fatalf("worldline: %v", err)
	}
	ctl.Start()

	log.Printf("worldline listening on %s base=%s merchant=%s", addr, baseDir, merchantID)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// surfaces builds worldline's two HTTP listeners: its own, on addr, and
// the control surface on addr's port plus 1000 (internal/simctl). The SFTP
// listener on sshAddr has no control port of its own; this one steers both.
// The old /sim paths still answer on addr, passed through to the control
// surface, until phase 4 of docs/lab-api/plan.md retires them.
func (s *sim) surfaces(addr, sshAddr string) (*http.ServeMux, *simctl.Control, error) {
	mux := vendorRoutes(s.ch, s.sftpLog)
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "worldline", Kind: "vendor", Simulates: "Worldline, the card acquirer",
		Listeners: []string{addr, sshAddr},
		Reset:     s.reset,
		Customers: s.tenants.customers,
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, nil, err
	}
	s.controlRoutes(ctl.Mux)
	mux.Handle("/sim/", ctl.Mux)
	return mux, ctl, nil
}

// vendorRoutes is worldline's own HTTP surface: the SFT file-exchange
// channel a platform calls. Nothing under /sim.
func vendorRoutes(ch *sftpgateway.Channel, sftpLog *activity.Log) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "worldline"})
	})
	mux.HandleFunc("POST /inbound/onboarding/{merchant_id}", uploadInbound(ch, sftpgateway.CategoryOnboarding))
	mux.HandleFunc("POST /inbound/corrections/{merchant_id}", uploadInbound(ch, sftpgateway.CategoryCorrections))
	mux.HandleFunc("GET /inbound/onboarding/{merchant_id}", listInbound(ch, sftpgateway.CategoryOnboarding))
	mux.HandleFunc("GET /inbound/corrections/{merchant_id}", listInbound(ch, sftpgateway.CategoryCorrections))
	mux.HandleFunc("GET /outbound/{merchant_id}/daily", listOutboundCategory(ch, sftpgateway.CategoryDaily))
	mux.HandleFunc("GET /outbound/{merchant_id}/financial", listOutboundCategory(ch, sftpgateway.CategoryFinancial))
	mux.HandleFunc("GET /outbound/{merchant_id}/{category}/{filename}",
		sftpLog.Watch("http.download", summarizeFileRead, readOutboundFile(ch)))
	mux.HandleFunc("GET /config", getConfig(ch))
	mux.HandleFunc("PUT /config", putConfig(ch))
	mux.HandleFunc("GET /archive/{year}/{month}", listArchive(ch))
	return mux
}

// controlRoutes registers the lab's hooks on the control surface, beside
// the contract simctl mounts there (and the customers, environments.go):
// seeding transactions, running the cycle, the enrolment desk, the
// activity log, and the channel config. Each acts in the environment the
// call names. GET and PUT /sim/config are GET and PUT /config, the same
// handlers answering the same, so the lab never has to call the vendor's
// own API to read or change a setting; the channel is the default's, and
// lab-wide.
func (s *sim) controlRoutes(mux *http.ServeMux) {
	s.cycleRoutes(mux)
	s.enrolmentRoutes(mux)
	// A read-only window onto a lab run, carrying no key material: only
	// who connected and which files they touched.
	mux.HandleFunc("GET /sim/activity", activity.Handler(s.sftpLog))
	mux.HandleFunc("GET /sim/config", getConfig(s.ch))
	mux.HandleFunc("PUT /sim/config", putConfig(s.ch))
}

// ensureLayout creates the top-level Worldline SFTP directory skeleton
// (docs/ARCHITECTURE-worldline-sftp-channel.md §1) so the service is
// browsable even before any merchant-specific subdirectory has been
// created on demand.
func ensureLayout(baseDir string) error {
	dirs := []string{
		filepath.Join(baseDir, "inbound", "onboarding"),
		filepath.Join(baseDir, "inbound", "corrections"),
		filepath.Join(baseDir, "outbound", "shared"),
		filepath.Join(baseDir, "config"),
		filepath.Join(baseDir, "archive"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o777); err != nil { // world-writable: host-mounted, see internal/sftp/staging.go
			return err
		}
	}
	return nil
}

// seedDefaultConfig writes a default channel configuration if none exists
// yet, mirroring the seeded sftp/config/merchant-onboarding.json shape.
func seedDefaultConfig(ch *sftpgateway.Channel, merchantID string) error {
	if _, err := ch.GetConfig(); err == nil {
		return nil
	}
	return ch.UpdateConfig(sftpgateway.DefaultConfig(ch.BaseDir, merchantID))
}

type fileEntry struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

func toEntries(infos []os.FileInfo) []fileEntry {
	out := make([]fileEntry, 0, len(infos))
	for _, fi := range infos {
		out = append(out, fileEntry{Name: fi.Name(), Size: fi.Size(), ModifiedAt: fi.ModTime()})
	}
	return out
}

// uploadInbound handles POST /inbound/{onboarding,corrections}/{merchant_id}.
// The upload filename is passed as a query parameter (?filename=), and the
// raw request body is the file content — this is a file drop simulation,
// not a real SFTP/SSH upload, so there is no multipart envelope to parse.
func uploadInbound(ch *sftpgateway.Channel, category string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := r.PathValue("merchant_id")
		filename := r.URL.Query().Get("filename")
		if filename == "" {
			httputilx.Error(w, 400, "filename query parameter required")
			return
		}
		content, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes))
		if err != nil {
			httputilx.Error(w, 400, "read body")
			return
		}
		path, err := ch.DropInbound(category, merchantID, filename, content)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		id := "up_" + shortID()
		log.Printf("stored inbound %s category=%s merchant=%s path=%s", id, category, merchantID, path)
		httputilx.WriteJSON(w, 201, map[string]string{"status": "stored", "id": id, "path": path})
	}
}

func listInbound(ch *sftpgateway.Channel, category string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := r.PathValue("merchant_id")
		infos, err := ch.ListInbound(category, merchantID)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{
			"merchant_id": merchantID,
			"category":    category,
			"files":       toEntries(infos),
		})
	}
}

func listOutboundCategory(ch *sftpgateway.Channel, category string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := r.PathValue("merchant_id")
		infos, err := ch.ListOutboundCategory(merchantID, category)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{
			"merchant_id": merchantID,
			"category":    category,
			"files":       toEntries(infos),
		})
	}
}

// readOutboundFile handles GET /outbound/{merchant_id}/{category}/{filename}.
// Per the documented file lifecycle, a successful retrieval archives the
// file into {SFTP_BASE_DIR}/archive/{year}/{month} so it is not served
// twice from the outbound directory.
func readOutboundFile(ch *sftpgateway.Channel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := r.PathValue("merchant_id")
		category := r.PathValue("category")
		filename := r.PathValue("filename")
		relativePath := category + "/" + filename

		content, err := ch.ReadOutbound(merchantID, relativePath)
		if err != nil {
			if os.IsNotExist(err) {
				httputilx.Error(w, 404, "file not found")
				return
			}
			httputilx.Error(w, 400, err.Error())
			return
		}
		if archived, err := ch.ArchiveOutbound(merchantID, relativePath); err != nil {
			log.Printf("worldline: archive %s %s: %v", merchantID, relativePath, err)
		} else {
			log.Printf("archived %s -> %s", relativePath, archived)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.WriteHeader(200)
		_, _ = w.Write(content)
	}
}

func getConfig(ch *sftpgateway.Channel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := ch.GetConfig()
		if err != nil {
			httputilx.Error(w, 404, "config not found")
			return
		}
		httputilx.WriteJSON(w, 200, cfg)
	}
}

func putConfig(ch *sftpgateway.Channel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg sftpgateway.Config
		if err := httputilx.ReadJSON(r, &cfg); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		if cfg.MerchantID == "" {
			httputilx.Error(w, 400, "merchant_id required")
			return
		}
		if err := ch.UpdateConfig(&cfg); err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, cfg)
	}
}

func listArchive(ch *sftpgateway.Channel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		year := r.PathValue("year")
		month := r.PathValue("month")
		infos, err := ch.ListArchive(year, month)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{
			"year":  year,
			"month": month,
			"files": toEntries(infos),
		})
	}
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// startWorldlineSFTP brings up mock-Worldline's acquiring side: the real
// SSH/SFTP+PGP server, the transaction store behind it, and the daily
// settlement cycle that publishes into it.
//
// The files it serves are written here, by the acquirer, straight into the
// server's own root. There used to be a background poller mirroring files
// the *platform* had written into a shared bind mount, which meant the
// platform produced the settlement file it was supposed to be receiving
// and no code ever exercised the download. Everything here runs in
// background goroutines so the HTTP listener is unaffected; a setup
// failure (bad host key, unparsable keys, bind failure) is fatal at
// startup rather than silently leaving the service half-configured. The
// default's cycle and enrolment desk share one guard, which a reset moves;
// every other environment's have one of their own (environments.go).
//
// It fills in s and returns the SFTP listener's address.
func (s *sim) startWorldlineSFTP() string {
	root := env("WORLDLINE_SFTP_ROOT", "/wlsftp")
	if err := wlsftp.EnsureLayout(root); err != nil {
		log.Fatalf("worldline: sftp layout: %v", err)
	}
	// Every other environment's jail is a folder of its own here, outside
	// the default's root.
	envRoot := env("WORLDLINE_SFTP_ENV_ROOT", defaultEnvRoot(root))
	if err := checkEnvRoot(root, envRoot); err != nil {
		log.Fatalf("worldline: %v", err)
	}
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		log.Printf("worldline: cannot create WORLDLINE_SFTP_ENV_ROOT %s: %v -- no environment can be added until it can", envRoot, err)
	}

	hostKeyPath := env("WORLDLINE_SFTP_HOST_KEY_PATH", "/keys/wlsftp-keys/host_key")
	hostSigner, err := wlsftp.LoadOrGenerateHostKey(hostKeyPath)
	if err != nil {
		log.Fatalf("worldline: sftp host key: %v", err)
	}
	// Publish the host key's public half next to it, so a client can pin
	// it (wlsftp.ClientConfig.HostKeyPath) instead of trusting whatever
	// answers -- the arrangement a real deployment must use.
	if pubPath := env("WORLDLINE_SFTP_HOST_KEY_PUB_PATH", hostKeyPath+".pub"); pubPath != "" {
		if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()), 0o644); err != nil {
			log.Printf("worldline: could not publish host public key to %s: %v (clients will have to skip pinning)", pubPath, err)
		}
	}

	pubPath := env("WORLDLINE_PGP_PUBLIC_KEY_PATH", "/keys/wlsftp-keys/worldline_public.asc")
	privPath := env("WORLDLINE_PGP_PRIVATE_KEY_PATH", "/keys/wlsftp-keys/worldline_private.asc")
	own, err := wlsftp.LoadOrGenerateKeypair(pubPath, privPath, "mock-Worldline", "fintechlab-simple", "worldline@fintechlab-simple.local")
	if err != nil {
		log.Fatalf("worldline: pgp keypair: %v", err)
	}
	armored, err := wlsftp.ArmoredPublicKey(own)
	if err != nil {
		log.Fatalf("worldline: armor pgp public key: %v", err)
	}
	log.Printf("worldline: mock-Worldline PGP public key (%s):\n%s", pubPath, armored)

	// A real acquirer encrypts to the *platform's* public key. When one is
	// configured that is what happens; otherwise mock-Worldline encrypts
	// to its own, and the platform side decrypts with the same generated
	// private key over the shared key volume.
	var recipient *openpgp.Entity
	if platformPubPath := env("INFINITE_PGP_PUBLIC_KEY_PATH", ""); platformPubPath != "" {
		recipient, err = wlsftp.LoadPublicKey(platformPubPath)
		if err != nil {
			log.Fatalf("worldline: load INFINITE_PGP_PUBLIC_KEY_PATH: %v", err)
		}
	}
	keys := &wlsftp.PGPKeys{Own: own, Recipient: recipient}

	source := worldline.FileSource(env("WORLDLINE_FILE_SOURCE", string(worldline.FileSourceGenerate)))
	switch source {
	case worldline.FileSourceGenerate, worldline.FileSourceFixture:
	default:
		log.Fatalf("worldline: WORLDLINE_FILE_SOURCE=%q, want %q or %q", source,
			worldline.FileSourceGenerate, worldline.FileSourceFixture)
	}

	// One guard for everything of the default's that outlives a request,
	// so a reset drops it all at once (reset.go).
	guard := &runGuard{}
	acq := &settlementApp{
		env:   tenant.Default,
		store: worldline.NewStore(),
		cfg: worldline.CycleConfig{
			Identifier:        env("WORLDLINE_SETTLEMENT_IDENTIFIER", "Worldline_Settlement"),
			SettlementAccount: env("WORLDLINE_SETTLEMENT_ACCOUNT", ""),
			MorningAt:         env("WORLDLINE_MORNING_FILE_AT", "08:00"),
			AfternoonAt:       env("WORLDLINE_AFTERNOON_FILE_AT", "15:30"),
			MorningJitter:     envDuration("WORLDLINE_MORNING_FILE_WINDOW", 2*time.Hour),
			SettlementDelay:   envDuration("WORLDLINE_SETTLEMENT_DELAY", 24*time.Hour),
			FixtureDir:        env("WORLDLINE_FIXTURE_DIR", "/wlsftp-fixtures"),
			Source:            source,
		},
		root:   root,
		keys:   keys,
		sgaURL: env("BC_INTERNAL_URL", ""),
		sgaAccount: map[string]string{
			"EUR": env("BC_SAFEGUARDING_ACCOUNT_ID_EUR", ""),
			"GBP": env("BC_SAFEGUARDING_ACCOUNT_ID_GBP", ""),
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
		guard:      guard,
	}

	sshAddr := ":" + env("WORLDLINE_SFTP_PORT", "2222")
	password := env("WORLDLINE_SFTP_PASSWORD", "")
	authorizedKey := env("WORLDLINE_SFTP_AUTHORIZED_KEY", "")
	// The acquirer's side of the file exchange is otherwise completely
	// silent to an operator: the platform connects, lists a directory and
	// takes a file, and unless you are tailing this container's stdout the
	// only evidence is a settlement that did or did not happen an hour
	// later. This is that evidence, next to the service it is about.
	sftpLog := activity.New("sftp", "File exchange",
		"Every SFTP session the platform opened against this acquirer, and what it did with it.")
	// The boarding desk answers enrolment batches uploaded to to_WLNORDIC.
	// It shares the acquirer's SFTP root and PGP keys: same channel, same
	// crypto, different conversation.
	desk := newEnrolmentDesk(root, keys,
		envDuration("WORLDLINE_ENROLMENT_DELAY", 30*time.Second),
		envDuration("WORLDLINE_ENROLMENT_SWEEP", 5*time.Second))
	desk.env, desk.guard = tenant.Default, guard

	s.guard, s.acq, s.desk, s.sftpLog = guard, acq, desk, sftpLog
	s.serveEnvironments(envRoot)

	// A customer's username and password open its own jail; any other
	// login is the default's, exactly as before environments.
	srv := wlsftp.Server{
		Root: root, HostSigner: hostSigner, Password: password, AuthorizedKey: authorizedKey,
		Accounts: s.sftpAccounts, Logf: log.Printf, Observe: recordSFTP(sftpLog, s.tenants.customers),
	}
	go func() {
		if err := srv.ListenAndServe(context.Background(), sshAddr); err != nil {
			log.Fatalf("worldline: sftp listener: %v", err)
		}
	}()
	go s.watchEnrolment()
	go s.schedule()
	log.Printf("worldline: sftp+pgp listening on %s root=%s environments=%s source=%s morning=%s afternoon=%s",
		sshAddr, root, envRoot, source, acq.cfg.MorningAt, acq.cfg.AfternoonAt)
	return sshAddr
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
