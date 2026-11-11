package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
)

// jumpHost is an SSH bastion: it lets "jump" in with one key or a password
// and carries direct-tcpip channels on to where they ask, as sshd does for
// ssh -L.
type jumpHost struct {
	addr        string
	fingerprint string
}

func newJumpHost(t *testing.T, authorized ssh.PublicKey, password string) jumpHost {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == "jump" && authorized != nil && bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("not this key")
		},
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == "jump" && password != "" && string(p) == password {
				return nil, nil
			}
			return nil, errors.New("not this password")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveJump(nc, cfg)
		}
	}()
	return jumpHost{addr: ln.Addr().String(), fingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey())}
}

func serveJump(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "direct-tcpip" {
			_ = nch.Reject(ssh.UnknownChannelType, "only forwarding here")
			continue
		}
		var p struct {
			Host     string
			Port     uint32
			OrigHost string
			OrigPort uint32
		}
		if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
			_ = nch.Reject(ssh.ConnectionFailed, "bad request")
			continue
		}
		target, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
		if err != nil {
			_ = nch.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			target.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() {
			defer ch.Close()
			defer target.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(ch, target); done <- struct{}{} }()
			go func() { _, _ = io.Copy(target, ch); done <- struct{}{} }()
			<-done
		}()
	}
}

// echoDB stands in for a database: it answers every line with itself.
func echoDB(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// closedAddr is an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// clientKey is a key "jump" may log in with, as PEM, optionally encrypted.
func clientKey(t *testing.T, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), sshPub
}

func tunnelTo(t *testing.T, j jumpHost) labdb.Tunnel {
	t.Helper()
	host, port, _ := net.SplitHostPort(j.addr)
	p, _ := strconv.Atoi(port)
	return labdb.Tunnel{Host: host, Port: p, User: "jump"}
}

// ping writes a line through addr and reads it back.
func ping(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if line != "ping\n" {
		return fmt.Errorf("read back %q", line)
	}
	return nil
}

func TestATunnelHandsTheSeederEachDatabaseThroughTheJumpHost(t *testing.T) {
	key, pub := clientKey(t, "")
	j := newJumpHost(t, pub, "")
	db := echoDB(t)
	tun := tunnelTo(t, j)
	tun.PrivateKey, tun.HostKey = key, j.fingerprint

	raw := "postgres://lab:p%40ss@" + db + "/platform?sslmode=disable"
	got, release, err := reachDatabases(context.Background(), &tun, map[string]string{"platform": raw})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got["platform"])
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Hostname() != "127.0.0.1" || u.Host == db || u.User.Username() != "lab" || pw != "p@ss" ||
		u.Path != "/platform" || u.RawQuery != "sslmode=disable" {
		t.Fatalf("the seeder is handed %s, want the database's own URL at a forward", got["platform"])
	}
	// Several connections, as a pool opens them, each carried on.
	for i := 0; i < 3; i++ {
		if err := ping(u.Host); err != nil {
			t.Fatalf("through the forward: %v", err)
		}
	}
	release()
	if err := ping(u.Host); err == nil {
		t.Fatal("the forward still answers after release")
	}

	// No tunnel: the URLs as they are.
	got, release, err = reachDatabases(context.Background(), nil, map[string]string{"platform": raw})
	if err != nil || got["platform"] != raw {
		t.Fatalf("without a tunnel: %v %v", got, err)
	}
	release()
}

func TestATunnelSaysWhatStopsIt(t *testing.T) {
	key, pub := clientKey(t, "")
	other, _ := clientKey(t, "")
	j := newJumpHost(t, pub, "secret-pw")
	db := echoDB(t)
	dbs := map[string]string{"settle": "postgres://lab:pw@" + db + "/settle"}

	for _, c := range []struct {
		name string
		edit func(*labdb.Tunnel)
		want string
	}{
		{"a key it does not take", func(tun *labdb.Tunnel) { tun.PrivateKey = other }, "unable to authenticate"},
		{"another host key than the pinned one", func(tun *labdb.Tunnel) {
			tun.PrivateKey, tun.HostKey = key, "SHA256:"+strings.Repeat("A", 43)
		}, "not the pinned"},
		{"no jump host there", func(tun *labdb.Tunnel) {
			tun.PrivateKey = key
			tun.Host, tun.Port = "127.0.0.1", portOf(t, closedAddr(t))
		}, "the jump host 127.0.0.1"},
	} {
		tun := tunnelTo(t, j)
		c.edit(&tun)
		if _, _, err := reachDatabases(context.Background(), &tun, dbs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", c.name, err, c.want)
		}
	}

	// A password is enough; a database the jump host cannot reach is named.
	tun := tunnelTo(t, j)
	tun.Password = "secret-pw"
	got, release, err := reachDatabases(context.Background(), &tun, dbs)
	if err != nil {
		t.Fatalf("with a password: %v", err)
	}
	release()
	if !strings.Contains(got["settle"], "127.0.0.1") {
		t.Fatalf("with a password: %v", got)
	}
	gone := closedAddr(t)
	_, _, err = reachDatabases(context.Background(), &tun, map[string]string{"settle": "postgres://lab:pw@" + gone + "/settle"})
	if err == nil || !strings.Contains(err.Error(), "does not reach the settle database at "+gone) || strings.Contains(err.Error(), "pw@") {
		t.Fatalf("an unreachable database: %v", err)
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return p
}

// An environment registers its jump host with its databases; the view never
// shows its secrets, a change that sends them back as "(set)" keeps them,
// and the seeder is handed forwards that are live for its call.
func TestAnEnvironmentsTunnelCarriesTheSeedersCall(t *testing.T) {
	a, _ := newFilesApp(t)
	key, pub := clientKey(t, "")
	j := newJumpHost(t, pub, "")
	db := echoDB(t)

	// A seeder that uses the database it is handed while it is asked.
	var handed string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"service": "lab-seeder", "databases": []string{"platform"}})
	})
	mux.HandleFunc("POST /seed", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Databases map[string]string `json:"databases"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		handed = body.Databases["platform"]
		u, err := url.Parse(handed)
		if err == nil {
			err = ping(u.Host)
		}
		if err != nil {
			httputilx.Error(w, 500, "the database: "+err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{"merchants": 3})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.seederURL = srv.URL
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)

	host, _, _ := net.SplitHostPort(j.addr)
	cfg, _ := json.Marshal(map[string]any{"config": map[string]any{
		"databases": map[string]string{"platform": "postgres://lab:db-secret@" + db + "/platform"},
		"tunnel":    map[string]any{"host": host, "port": portOf(t, j.addr), "user": "jump", "private_key": key, "host_key": j.fingerprint},
	}})
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(cfg)); w.Code != 200 {
		t.Fatalf("register the tunnel: %d %s", w.Code, w.Body)
	}
	view := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "").Body.String()
	if strings.Contains(view, "PRIVATE KEY") || strings.Contains(view, "db-secret") || !strings.Contains(view, `"private_key":"(set)"`) ||
		!strings.Contains(view, `"user":"jump"`) {
		t.Fatalf("the view: %s", view)
	}
	if w := as(t, a, token, "GET", "/v1/platform", ""); !strings.Contains(w.Body.String(), `"tunnel":{"host":"`+j.addr+`"`) {
		t.Fatalf("platform state: %s", w.Body)
	}

	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 3}`); w.Code != 200 {
		t.Fatalf("seed through the tunnel: %d %s", w.Code, w.Body)
	}
	if u, _ := url.Parse(handed); u == nil || u.Host == db || u.Hostname() != "127.0.0.1" {
		t.Fatalf("the seeder was handed %q, want a forward", handed)
	}

	// The view sent back as it shows the tunnel keeps its key; the seed still works.
	back, _ := json.Marshal(map[string]any{"config": map[string]any{
		"tunnel": map[string]any{"host": host, "port": portOf(t, j.addr), "user": "jump", "private_key": "(set)", "host_key": j.fingerprint},
	}})
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(back)); w.Code != 200 {
		t.Fatalf("send the view back: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 3}`); w.Code != 200 {
		t.Fatalf("seed after the view went back: %d %s", w.Code, w.Body)
	}
	// The check reaches the database through it.
	w := as(t, a, token, "POST", "/v1/platform/check", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"platform":"reachable"`) || !strings.Contains(w.Body.String(), `"status":"ok"`) ||
		!strings.Contains(w.Body.String(), j.fingerprint) {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}

	// A jump host that has gone stops the seed with its reason, and null removes the tunnel.
	gone, _ := json.Marshal(map[string]any{"config": map[string]any{
		"tunnel": map[string]any{"host": "127.0.0.1", "port": portOf(t, closedAddr(t)), "user": "jump"},
	}})
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(gone)); w.Code != 200 {
		t.Fatalf("move the tunnel: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 3}`); w.Code != 502 || !strings.Contains(w.Body.String(), "the jump host") {
		t.Fatalf("seed with the jump host gone: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "POST", "/v1/platform/check", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "not checked") {
		t.Fatalf("check with the jump host gone: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", `{"config": {"tunnel": null}}`); w.Code != 200 {
		t.Fatalf("remove the tunnel: %d %s", w.Code, w.Body)
	}
	if view := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "").Body.String(); strings.Contains(view, `"tunnel"`) {
		t.Fatalf("the tunnel is still there: %s", view)
	}
	// Without one the database is reached as it is.
	if w := as(t, a, token, "POST", "/v1/platform/check", ""); !strings.Contains(w.Body.String(), `"platform":"reachable"`) || !strings.Contains(w.Body.String(), `"tunnel":null`) {
		t.Fatalf("check without a tunnel: %s", w.Body)
	}
}

func TestATunnelIsCheckedWhenItIsRegistered(t *testing.T) {
	a, _ := newFilesApp(t)
	createEnvOnly(t, a, `{"id": "sbx"}`)
	key, _ := clientKey(t, "")
	locked, _ := clientKey(t, "open sesame")
	for _, c := range []struct {
		tunnel map[string]any
		want   string
	}{
		{map[string]any{"user": "jump", "password": "x"}, "config.tunnel.host"},
		{map[string]any{"host": "bastion", "password": "x"}, "config.tunnel.user"},
		{map[string]any{"host": "bastion", "user": "jump"}, "a private_key, a password"},
		{map[string]any{"host": "bastion", "user": "jump", "private_key": "not a key"}, "not a PEM private key"},
		{map[string]any{"host": "bastion", "user": "jump", "private_key": locked}, "give its passphrase"},
		{map[string]any{"host": "bastion", "user": "jump", "private_key": locked, "passphrase": "wrong"}, "passphrase opens"},
		{map[string]any{"host": "bastion", "user": "jump", "private_key": key, "host_key": "ab:cd"}, "SHA256"},
		{map[string]any{"host": "bastion", "user": "jump", "private_key": key, "port": 70000}, "config.tunnel.port"},
	} {
		body, _ := json.Marshal(map[string]any{"config": map[string]any{"tunnel": c.tunnel}})
		if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(body)); w.Code != 400 || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("tunnel %v: %d %s, want 400 saying %q", c.tunnel, w.Code, w.Body, c.want)
		}
	}
	body, _ := json.Marshal(map[string]any{"config": map[string]any{"tunnel": map[string]any{
		"host": "bastion", "user": "jump", "private_key": locked, "passphrase": "open sesame",
	}}})
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(body)); w.Code != 200 {
		t.Fatalf("an encrypted key with its passphrase: %d %s", w.Code, w.Body)
	}
	if view := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "").Body.String(); strings.Contains(view, "open sesame") || !strings.Contains(view, `"passphrase":"(set)"`) {
		t.Fatalf("the view: %s", view)
	}
}
