package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
)

// The jump host an environment's databases are reached through
// (labdb.Tunnel). A platform's databases are often private: reachable from
// a bastion in its own network, not from the lab. For each seeder call the
// lab connects to the environment's jump host over SSH and forwards a port
// of its own to every database the call hands over, so the seeder is given
// the database's own URL -- its credentials and options untouched -- with
// the forward's address in place of the database's. The seeder runs in the
// lab's pod and reaches the lab API's 127.0.0.1, so it needs no SSH of its
// own; an environment without a tunnel hands its URLs over as they are.

// tunnelTimeout bounds reaching the jump host and its handshake, and each
// check that it reaches a database.
const tunnelTimeout = 15 * time.Second

// hostKeyPin is a key's fingerprint as ssh-keygen -lf prints it.
var hostKeyPin = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

func validTunnel(t labdb.Tunnel) error {
	switch {
	case strings.TrimSpace(t.Host) == "" || strings.ContainsAny(t.Host, " /@"):
		return fmt.Errorf("config.tunnel.host is the jump host's name or address, not %q", t.Host)
	case t.Port < 0 || t.Port > 65535:
		return fmt.Errorf("config.tunnel.port is 1 to 65535 (22 when left out), not %d", t.Port)
	case strings.TrimSpace(t.User) == "":
		return errors.New("config.tunnel.user is the login on the jump host")
	case t.PrivateKey == "" && t.Password == "":
		return errors.New("config.tunnel needs a private_key, a password, or both")
	case t.HostKey != "" && !hostKeyPin.MatchString(t.HostKey):
		return fmt.Errorf("config.tunnel.host_key is the key's SHA256:… fingerprint, as ssh-keygen -lf prints it, not %q", t.HostKey)
	}
	if t.PrivateKey != "" && t.PrivateKey != tokenShown {
		if _, err := tunnelSigner(t); err != nil {
			return fmt.Errorf("config.tunnel.private_key: %w", err)
		}
	}
	return nil
}

// tunnelSigner is t's private key, decrypted with its passphrase when it
// has one.
func tunnelSigner(t labdb.Tunnel) (ssh.Signer, error) {
	key := []byte(t.PrivateKey)
	if t.Passphrase != "" {
		s, err := ssh.ParsePrivateKeyWithPassphrase(key, []byte(t.Passphrase))
		if err != nil {
			return nil, fmt.Errorf("not a private key this passphrase opens: %w", err)
		}
		return s, nil
	}
	s, err := ssh.ParsePrivateKey(key)
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, errors.New("it is encrypted: give its passphrase")
	case err != nil:
		return nil, fmt.Errorf("not a PEM private key: %w", err)
	}
	return s, nil
}

// tunnelAddr is t's host and port, 22 when it names none.
func tunnelAddr(t labdb.Tunnel) string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// dialTunnel connects to t's jump host and answers the client and the
// fingerprint of the host key it met (which a pinned host_key must match).
func dialTunnel(ctx context.Context, t labdb.Tunnel) (*ssh.Client, string, error) {
	addr := tunnelAddr(t)
	var auth []ssh.AuthMethod
	if t.PrivateKey != "" {
		signer, err := tunnelSigner(t)
		if err != nil {
			return nil, "", fmt.Errorf("the jump host %s: the private key: %w", addr, err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		auth = append(auth, ssh.Password(t.Password))
	}
	var met string
	cfg := &ssh.ClientConfig{
		User:    t.User,
		Auth:    auth,
		Timeout: tunnelTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			met = ssh.FingerprintSHA256(key)
			if t.HostKey != "" && met != t.HostKey {
				return fmt.Errorf("its host key is %s, not the pinned %s", met, t.HostKey)
			}
			return nil
		},
	}
	d := net.Dialer{Timeout: tunnelTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("the jump host %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(tunnelTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, met, fmt.Errorf("the jump host %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), met, nil
}

// dbTarget is a database URL and the host:port it names (5432 when it
// names no port). Its errors never repeat the URL, which holds a password.
func dbTarget(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, "", errors.New("its URL names no host")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return u, net.JoinHostPort(u.Hostname(), port), nil
}

// dialThrough reaches target through the jump host, giving up after
// tunnelTimeout: a jump host that cannot route to a database can hold a
// channel open for minutes.
func dialThrough(client *ssh.Client, target string) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := client.Dial("tcp", target)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(tunnelTimeout):
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("no answer in %s", tunnelTimeout)
	}
}

// forward opens a forward per database through client: a listener on
// 127.0.0.1 whose every connection the jump host carries on to the
// database. It answers each database's URL with the forward's address in
// it, after checking the jump host reaches every one; release closes them.
func forward(client *ssh.Client, dbs map[string]string) (map[string]string, func(), error) {
	var listeners []net.Listener
	release := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	out := make(map[string]string, len(dbs))
	for _, name := range sortedKeys(dbs) {
		u, target, err := dbTarget(dbs[name])
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("the %s database: %w", name, err)
		}
		probe, err := dialThrough(client, target)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("the jump host does not reach the %s database at %s: %w", name, target, err)
		}
		probe.Close()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("a forward to the %s database: %w", name, err)
		}
		listeners = append(listeners, l)
		go serveForward(l, client, name, target)
		local := *u
		local.Host = l.Addr().String()
		out[name] = local.String()
	}
	return out, release, nil
}

// serveForward carries each connection l accepts on to target through the
// jump host, until l is closed.
func serveForward(l net.Listener, client *ssh.Client, name, target string) {
	for {
		local, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer local.Close()
			remote, err := client.Dial("tcp", target)
			if err != nil {
				log.Printf("lab-api: tunnel: the %s database at %s: %v", name, target, err)
				return
			}
			defer remote.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			<-done
		}()
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// reachDatabases hands dbs over as the seeder will reach them: through the
// environment's tunnel when it names one, for as long as the call takes
// (release ends it), or as they are.
func reachDatabases(ctx context.Context, t *labdb.Tunnel, dbs map[string]string) (map[string]string, func(), error) {
	if t == nil {
		return dbs, func() {}, nil
	}
	client, _, err := dialTunnel(ctx, *t)
	if err != nil {
		return nil, nil, err
	}
	out, release, err := forward(client, dbs)
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return out, func() { release(); client.Close() }, nil
}

// checkDatabases answers POST /v1/platform/check: whether the lab reaches
// each database the environment names -- through its jump host when it has
// one, which is checked first -- without the seeder and without logging in
// to a database. A database that answers is "reachable"; its credentials
// are the seeder's to try.
func (a *app) checkDatabases(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.envConfig(r.Context())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	if len(cfg.Databases) == 0 {
		httputilx.Error(w, 409, fmt.Sprintf("this environment's config names no database: PATCH /v1/environments/%s {\"config\": {\"databases\": {…}}}", scopeOf(r.Context())))
		return
	}
	out := map[string]any{"env": scopeOf(r.Context()), "tunnel": nil}
	reached := map[string]string{}
	dial := func(target string) (net.Conn, error) {
		d := net.Dialer{Timeout: tunnelTimeout}
		return d.DialContext(r.Context(), "tcp", target)
	}
	if t := cfg.Tunnel; t != nil {
		tunnel := map[string]any{"host": tunnelAddr(*t), "user": t.User}
		client, met, err := dialTunnel(r.Context(), *t)
		if met != "" {
			tunnel["host_key"] = met
		}
		out["tunnel"] = tunnel
		if err != nil {
			tunnel["status"] = err.Error()
			for name := range cfg.Databases {
				reached[name] = "not checked: the jump host is not reached"
			}
			out["databases"] = reached
			httputilx.WriteJSON(w, 200, out)
			return
		}
		defer client.Close()
		tunnel["status"] = "ok"
		dial = func(target string) (net.Conn, error) { return dialThrough(client, target) }
	}
	for _, name := range sortedKeys(cfg.Databases) {
		_, target, err := dbTarget(cfg.Databases[name])
		if err != nil {
			reached[name] = err.Error()
			continue
		}
		c, err := dial(target)
		if err != nil {
			reached[name] = fmt.Sprintf("%s does not answer: %v", target, err)
			continue
		}
		c.Close()
		reached[name] = "reachable"
	}
	out["databases"] = reached
	httputilx.WriteJSON(w, 200, out)
}
