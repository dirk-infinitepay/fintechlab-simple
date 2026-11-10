package wlsftp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/sshftp"
	"golang.org/x/crypto/ssh"
)

// The four real Worldline SFT directories (see
// docs/ARCHITECTURE-vendor-corrections.md §2: two path conventions for the
// same content — /download + /disputes/download from one acquirer-fts
// config vintage, /to_WLNORDIC + /from_WLNORDIC from another — both must
// exist as real, listable directories under the server's root).
const (
	DirDownload         = "download"
	DirDisputesDownload = "disputes/download"
	DirToWLNordic       = "to_WLNORDIC"
	DirFromWLNordic     = "from_WLNORDIC"
)

// EnsureLayout creates the four real Worldline SFT directories under root
// if they do not already exist.
func EnsureLayout(root string) error {
	for _, d := range []string{DirDownload, DirDisputesDownload, DirToWLNordic, DirFromWLNordic} {
		dir := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("wlsftp: create %s: %w", dir, err)
		}
	}
	return nil
}

// Logf matches log.Printf's signature, letting callers pass their own
// per-binary logger without this package importing "log" directly.
type Logf func(format string, args ...any)

// Account is an SFTP login of its own, apart from the server's: a username
// and password that open a session jailed at Root instead of the server's
// root. It is how one server serves several customers — worldline gives
// each environment one (internal/tenant) — each seeing only its own
// folder, and none of them the server's.
type Account struct {
	User string
	// Password is the only way in: an account takes no key, and an empty
	// password opens nothing.
	Password string
	Root     string
	// Tag is the caller's name for whose session it is (worldline's: the
	// environment). It comes back on every Session the account opens.
	Tag string
}

// Accounts returns the accounts named user. None means the server's own
// login applies, exactly as on a server without accounts. One or more, and
// only a password one of them holds opens a session: a wrong one is
// refused, and so is a key, rather than falling back to the server's own
// login under someone else's name.
type Accounts func(user string) []Account

// Session is who an observed request came from.
type Session struct {
	User   string
	Remote string
	// Tag is the Account's Tag, or "" for the server's own login.
	Tag string
}

// Server is an SFTP server's whole configuration. The zero Accounts is a
// server with one login, its own (Password, AuthorizedKey), as it always
// was.
type Server struct {
	Root       string
	HostSigner ssh.Signer
	// Password and AuthorizedKey are the server's own login; see
	// newAuth for how they combine, and what neither means.
	Password      string
	AuthorizedKey string
	// Accounts, when set, are the logins of their own (Account).
	Accounts Accounts
	Logf     Logf
	// Observe, when set, is told about each session and each request.
	Observe SessionObserver
}

// SessionObserver is Observer with the whole session: which account, if
// any, it logged in as.
type SessionObserver func(s Session, op, path string, err error)

// auth is the server's logins, built once and handed to every connection.
//
// Password and public-key auth are enabled independently, and both may be
// on at once: real Worldline SFT accounts are key-authenticated, while a
// local run is far easier to drive with a password, and a client that
// offers a key and falls back to a password works against either without
// reconfiguration. Previously setting a password silently disabled key
// auth entirely, which meant the lab could not exercise the
// authentication method production actually uses.
//
// If neither is set, this lab accepts any authentication attempt at all --
// a deliberate, loudly-logged lab-only default, matching this repo's
// graceful-degradation philosophy rather than refusing to start. An
// Account's username is the exception: it needs its own password, always.
type auth struct {
	password string
	allowed  []ssh.PublicKey
	open     bool
	accounts Accounts
}

func newAuth(password, authorizedKey string, accounts Accounts, logf Logf) (*auth, error) {
	a := &auth{password: password, accounts: accounts}
	rest := []byte(authorizedKey)
	for len(bytes.TrimSpace(rest)) > 0 {
		key, _, _, remainder, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return nil, fmt.Errorf("wlsftp: parse WORLDLINE_SFTP_AUTHORIZED_KEY: %w", err)
		}
		a.allowed = append(a.allowed, key)
		rest = remainder
	}
	if password == "" && authorizedKey == "" {
		a.open = true
		if logf != nil {
			logf("wlsftp: WORLDLINE_SFTP_PASSWORD and WORLDLINE_SFTP_AUTHORIZED_KEY are both unset " +
				"-- accepting ANY SFTP authentication attempt. Lab-only default, do not use beyond local development.")
		}
	}
	return a, nil
}

// The permission extensions an accepted login carries to its session.
const (
	extRoot = "wlsftp-root"
	extTag  = "wlsftp-tag"
)

// attempt is what one connection's authentication learnt: the account its
// username named, for a refusal to say whose it was.
type attempt struct {
	mu  sync.Mutex
	tag string
}

func (t *attempt) named(tag string) {
	t.mu.Lock()
	t.tag = tag
	t.mu.Unlock()
}

func (t *attempt) whose() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tag
}

func (a *auth) lookup(user string) []Account {
	if a.accounts == nil {
		return nil
	}
	return a.accounts(user)
}

// config is the SSH server configuration for one connection.
func (a *auth) config(t *attempt) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{}
	if a.password != "" || a.open || a.accounts != nil {
		cfg.PasswordCallback = func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if accts := a.lookup(c.User()); len(accts) > 0 {
				t.named(accts[0].Tag)
				for _, acct := range accts {
					if acct.Password != "" && subtle.ConstantTimeCompare(pass, []byte(acct.Password)) == 1 {
						t.named(acct.Tag)
						return &ssh.Permissions{Extensions: map[string]string{extRoot: acct.Root, extTag: acct.Tag}}, nil
					}
				}
				return nil, fmt.Errorf("wlsftp: invalid password for user %q", c.User())
			}
			switch {
			case a.password != "":
				if string(pass) != a.password {
					return nil, fmt.Errorf("wlsftp: invalid password for user %q", c.User())
				}
				return nil, nil
			case a.open:
				return nil, nil
			}
			// Only accounts take a password on this server; its own login
			// is a key.
			return nil, fmt.Errorf("wlsftp: password authentication is not enabled for user %q", c.User())
		}
	}
	if len(a.allowed) > 0 || a.open {
		cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if accts := a.lookup(c.User()); len(accts) > 0 {
				t.named(accts[0].Tag)
				return nil, fmt.Errorf("wlsftp: user %q logs in with a password", c.User())
			}
			if a.open {
				return nil, nil
			}
			for _, allowed := range a.allowed {
				if bytes.Equal(key.Marshal(), allowed.Marshal()) {
					return nil, nil
				}
			}
			return nil, fmt.Errorf("wlsftp: unrecognized public key for user %q", c.User())
		}
	}
	return cfg
}

// ListenAndServe listens on addr and serves SSH/SFTP connections until ctx
// is cancelled or the listener fails to open; see Serve for the per-
// connection behavior.
func ListenAndServe(ctx context.Context, addr, root string, hostSigner ssh.Signer, password, authorizedKey string, logf Logf) error {
	return ListenAndServeObserved(ctx, addr, root, hostSigner, password, authorizedKey, logf, nil)
}

// Observer is told what a connected client did: a session opening, and
// then each request it made. It exists so the acquirer binary can show an
// operator that the platform's file pull actually happened — the single
// most common thing to be waiting on, and the one thing an SFTP server
// otherwise reveals only in its own stdout.
//
// Op is "session" when a client authenticates; otherwise it is the jail's
// own verb ("list", "download", "upload", "rename", …).
type Observer func(user, remote, op, path string, err error)

// session is obs as a SessionObserver.
func (obs Observer) session() SessionObserver {
	if obs == nil {
		return nil
	}
	return func(s Session, op, path string, err error) { obs(s.User, s.Remote, op, path, err) }
}

// ListenAndServeObserved is ListenAndServe with that hook.
func ListenAndServeObserved(ctx context.Context, addr, root string, hostSigner ssh.Signer, password, authorizedKey string, logf Logf, obs Observer) error {
	return Server{Root: root, HostSigner: hostSigner, Password: password, AuthorizedKey: authorizedKey,
		Logf: logf, Observe: obs.session()}.ListenAndServe(ctx, addr)
}

// ListenAndServe listens on addr and serves until ctx is cancelled; see
// Serve.
func (s Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("wlsftp: listen %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts SSH connections on ln, serving an SFTP subsystem jailed at
// root for each session channel, until ctx is cancelled or ln itself fails. A single connection's protocol error is
// logged and only that connection is closed, never the whole listener —
// matching this lab's graceful-degradation style elsewhere (log and
// continue, never panic on a downstream/peer being unreachable or
// misbehaving). Serve takes ownership of ln and closes it before
// returning.
func Serve(ctx context.Context, ln net.Listener, root string, hostSigner ssh.Signer, password, authorizedKey string, logf Logf) error {
	return ServeObserved(ctx, ln, root, hostSigner, password, authorizedKey, logf, nil)
}

// ServeObserved is Serve with an Observer attached to every session.
func ServeObserved(ctx context.Context, ln net.Listener, root string, hostSigner ssh.Signer, password, authorizedKey string, logf Logf, obs Observer) error {
	return Server{Root: root, HostSigner: hostSigner, Password: password, AuthorizedKey: authorizedKey,
		Logf: logf, Observe: obs.session()}.Serve(ctx, ln)
}

// Serve is the package's Serve for this configuration: a session that
// logged in as an Account is jailed at the account's root, every other at
// s.Root.
func (s Server) Serve(ctx context.Context, ln net.Listener) error {
	a, err := newAuth(s.Password, s.AuthorizedKey, s.Accounts, s.Logf)
	if err != nil {
		_ = ln.Close()
		return err
	}

	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		nConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wlsftp: accept: %w", err)
		}
		go s.handleConn(nConn, a)
	}
}

func (s Server) handleConn(nConn net.Conn, a *auth) {
	logf, obs := s.Logf, s.Observe
	if logf == nil {
		logf = func(string, ...any) {}
	}
	remote := hostOf(nConn.RemoteAddr())
	var t attempt
	sshConfig := a.config(&t)
	sshConfig.AddHostKey(s.HostSigner)
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, sshConfig)
	if err != nil {
		logf("wlsftp: ssh handshake from %s: %v", nConn.RemoteAddr(), err)
		// A refused handshake is worth seeing: a client with the wrong key
		// and a client that never connected look the same from the outside.
		// A refusal of an account's username is that account's to see.
		if obs != nil {
			obs(Session{Remote: remote, Tag: t.whose()}, "session", "", err)
		}
		return
	}
	sess := Session{User: sshConn.User(), Remote: remote}
	root := s.Root
	if p := sshConn.Permissions; p != nil && p.Extensions != nil {
		if r, ok := p.Extensions[extRoot]; ok {
			root, sess.Tag = r, p.Extensions[extTag]
		}
	}
	if obs != nil {
		obs(sess, "session", "", nil)
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			logf("wlsftp: accept channel from %s: %v", nConn.RemoteAddr(), err)
			continue
		}
		go serveSessionRequests(requests)
		go sshftp.ServeSFTPObserved(channel, root, func(f string, args ...any) { logf(f, args...) }, func(op, path string, err error) {
			if obs != nil {
				obs(sess, op, path, err)
			}
		})
	}
}

// serveSessionRequests answers the session channel's out-of-band requests,
// accepting only the "subsystem sftp" request an SFTP client sends.
func serveSessionRequests(in <-chan *ssh.Request) {
	for req := range in {
		ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
		if req.WantReply {
			_ = req.Reply(ok, nil)
		}
	}
}

// hostOf drops the ephemeral port from a client address: the port changes
// on every connection and is never the thing an operator is looking at.
func hostOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
