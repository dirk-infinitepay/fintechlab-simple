package simctl

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Control is a simulation's control surface on a listener of its own,
// apart from the vendor's (docs/lab-api/architecture.md §4): every /sim/*
// route, GET /health, and nothing a platform calls.
//
// It carries no vendor authentication. The vendor's login is for the
// platform calling the vendor's API; reversing a payout or reading a log is
// the lab steering its own simulation, and asking for the vendor's bearer
// token to do it would make the lab a client of the vendor it simulates.
// The control port is trusted by where it is: compose does not publish it,
// so only the lab's own network reaches it, and SIM_CONTROL_ALLOW narrows
// that to the addresses listed. Authentication belongs one layer up, between
// a person, an agent or the console and the lab API.
type Control struct {
	// Mux is where a service registers its own /sim/* hooks, beside the
	// contract routes Mount puts there.
	Mux *http.ServeMux

	service string
	addr    string
	allow   []*net.IPNet
}

// NewControl creates cfg's control surface, with the contract mounted and
// GET /health, to listen on SIM_CONTROL_LISTEN or else defaultAddr (by
// convention the vendor port plus 1000). SIM_CONTROL_ALLOW is a
// comma-separated list of addresses or CIDRs allowed to call it; unset,
// every caller that can reach the port may.
func NewControl(cfg Config, defaultAddr string) (*Control, error) {
	addr := strings.TrimSpace(os.Getenv("SIM_CONTROL_LISTEN"))
	if addr == "" {
		addr = defaultAddr
	}
	if addr == "" {
		// ControlAddr could not read the vendor address: listening on
		// ":http" by accident would be worse than saying so.
		return nil, fmt.Errorf("%s: no control address; set SIM_CONTROL_LISTEN", cfg.Service)
	}
	allow, err := ParseAllow(os.Getenv("SIM_CONTROL_ALLOW"))
	if err != nil {
		return nil, fmt.Errorf("SIM_CONTROL_ALLOW: %w", err)
	}
	cfg.ControlListener = addr
	c := &Control{Mux: http.NewServeMux(), service: cfg.Service, addr: addr, allow: allow}
	c.Mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": cfg.Service, "surface": "control"})
	})
	Mount(c.Mux, cfg)
	return c, nil
}

// ControlAddr is the conventional control address for a vendor listener:
// the same host and the port plus 1000 (":8085" → ":9085"), so the ports
// stay unique when every service shares one network namespace, as in the
// pod. An address it cannot read gives "".
func ControlAddr(vendorListen string) string {
	host, port, err := net.SplitHostPort(vendorListen)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n+1000 > 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1000))
}

// Addr is the address the control listener listens on.
func (c *Control) Addr() string { return c.addr }

// Handler is the control mux behind the allowlist. Each call is logged as
// a request line, like the vendor port's: a payout returned or an outcome
// flipped from the lab is something that happened to the simulation, and
// GET /sim/logs is where to look for it (the ring leaves out the health
// probes and the log reads themselves).
func (c *Control) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(c.allow) > 0 && !c.allowed(r.RemoteAddr) {
			httputilx.Error(w, 403, c.service+"'s control port does not take calls from "+r.RemoteAddr+" (SIM_CONTROL_ALLOW)")
			return
		}
		log.Printf("%s %s", r.Method, r.URL.Path)
		c.Mux.ServeHTTP(w, r)
	})
}

// Start serves the control listener in the background. A control port that
// cannot listen stops the process, as a vendor port that cannot would: a
// simulation the lab cannot steer is not one to run half of.
func (c *Control) Start() {
	log.Printf("%s: control listening on %s", c.service, c.addr)
	go func() {
		log.Fatal(http.ListenAndServe(c.addr, c.Handler()))
	}()
}

func (c *Control) allowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range c.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseAllow reads "10.89.0.0/16,127.0.0.1,::1" into networks; a bare
// address is a network of one.
func ParseAllow(spec string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("%q is not an address or a CIDR", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			part = fmt.Sprintf("%s/%d", part, bits)
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a CIDR", part)
		}
		out = append(out, n)
	}
	return out, nil
}
