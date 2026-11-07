package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Stand-ins, the console's half (internal/console/standins.go has why).
//
// Each stand-in owns some wiring: a timer that pulls, a vendor delivering
// to it. Disconnecting it turns that wiring off where it lives — a lab
// switch on the service, a subscription deactivated at the bank through
// the bank's own API — and reading it asks those same places, so the
// console never reports a state it only remembers.

// standInConn is one piece of a stand-in's wiring, as read just now.
type standInConn struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Connected is nil when this wiring does not reach the stand-in in the
	// running lab — ACI pointed at a real platform, say — and so is not
	// the stand-in's to switch.
	Connected *bool  `json:"connected"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

type standInView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Shown bool   `json:"shown"`
	SetBy string `json:"set_by,omitempty"`
	SetAt string `json:"set_at,omitempty"`
	// Plays and StartsOff are the catalogue's: what it stands in for, and
	// how to have it start disconnected (a restart).
	Plays     string `json:"plays,omitempty"`
	StartsOff string `json:"starts_off,omitempty"`
	// State sums the connections up: connected, disconnected, partial
	// (some of each), unwired (nothing reaches it) or unknown (a read
	// failed).
	State       string        `json:"state"`
	Connections []standInConn `json:"connections"`
	Errors      []string      `json:"errors,omitempty"`
}

// wiring is one switchable connection: how to read it and how to set it.
type wiring struct {
	id, label string
	read      func(ctx context.Context) standInConn
	set       func(ctx context.Context, on bool, reason string) error
}

func (a *app) wiringFor(id string) []wiring {
	switch id {
	case "settlement":
		return []wiring{a.switchWiring("settlement", "timer", "Worldline pull and cutoff payouts", false)}
	case "receiver":
		return []wiring{a.bcReceiverWiring(), a.switchWiring("aci", "aci", "ACI webhooks", true)}
	}
	return nil
}

// switchWiring is a lab switch on a service (internal/labswitch), at
// /sim/connected. With onlyReceiver, it counts only when the service
// delivers to the receiver: ACI pointed at a real platform is the
// platform's wiring, not the stand-in's.
func (a *app) switchWiring(svcID, connID, label string, onlyReceiver bool) wiring {
	base := func() string {
		s, _ := a.cat.Get(svcID)
		return s.BaseURL
	}
	return wiring{
		id: connID, label: label,
		read: func(ctx context.Context) standInConn {
			c := standInConn{ID: connID, Label: label}
			var st struct {
				Connected bool   `json:"connected"`
				What      string `json:"what"`
				Target    string `json:"target"`
			}
			if err := a.getJSON(ctx, a.client, base()+"/sim/connected", &st); err != nil {
				c.Error = err.Error()
				return c
			}
			if onlyReceiver && endpointHost(st.Target) != "receiver" {
				c.Detail = svcID + " delivers to " + st.Target + " — not the receiver, so not the stand-in's to switch"
				return c
			}
			c.Connected, c.Detail = &st.Connected, st.What
			if st.Target != "" {
				c.Detail += " (" + st.Target + ")"
			}
			return c
		},
		set: func(ctx context.Context, on bool, reason string) error {
			body, _ := json.Marshal(map[string]any{"connected": on, "reason": reason})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base()+"/sim/connected", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := a.client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return fmt.Errorf("%s /sim/connected: HTTP %d %s", svcID, resp.StatusCode, strings.TrimSpace(string(b)))
			}
			return nil
		},
	}
}

// bcReceiverWiring is every Banking Circle subscription whose endpoint is
// the receiver — the seeded one, normally. Switched with the bank's own
// activate and deactivate calls, so the subscription's status on the
// Banking Circle card says the same thing.
func (a *app) bcReceiverWiring() wiring {
	const label = "Banking Circle notifications"
	subs := func(ctx context.Context) ([]struct {
		ID         string `json:"id"`
		Endpoint   string `json:"endpoint"`
		IsActive   bool   `json:"isActive"`
		RowVersion string `json:"rowVersion"`
	}, error) {
		var listed struct {
			Result []struct {
				ID         string `json:"id"`
				Endpoint   string `json:"endpoint"`
				IsActive   bool   `json:"isActive"`
				RowVersion string `json:"rowVersion"`
			} `json:"result"`
		}
		if err := a.bc.AuthorizedGet(ctx, "/api/v1/notificationselfservice/subscription?PageSize=50", &listed); err != nil {
			return nil, err
		}
		out := listed.Result[:0]
		for _, s := range listed.Result {
			if endpointHost(s.Endpoint) == "receiver" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return wiring{
		id: "banking-circle", label: label,
		read: func(ctx context.Context) standInConn {
			c := standInConn{ID: "banking-circle", Label: label}
			list, err := subs(ctx)
			if err != nil {
				c.Error = err.Error()
				return c
			}
			if len(list) == 0 {
				c.Detail = "no subscription points at the receiver"
				return c
			}
			on := false
			var eps []string
			for _, s := range list {
				on = on || s.IsActive
				eps = append(eps, s.Endpoint)
			}
			c.Connected = &on
			c.Detail = fmt.Sprintf("%d subscription(s) to %s", len(list), strings.Join(eps, ", "))
			return c
		},
		set: func(ctx context.Context, on bool, _ string) error {
			list, err := subs(ctx)
			if err != nil {
				return err
			}
			verb := "deactivate"
			if on {
				verb = "activate"
			}
			var errs []error
			for _, s := range list {
				if s.IsActive == on {
					continue
				}
				path := "/api/v1/notificationselfservice/subscription/" + url.PathEscape(s.ID) + "/" + verb
				// The bank's optimistic concurrency, as a real client must.
				hdr := http.Header{"If-Match": []string{s.RowVersion}}
				if err := a.bc.AuthorizedDo(ctx, http.MethodPut, path, hdr, nil); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
}

func (a *app) standIn(ctx context.Context, id string) standInView {
	svc, _ := a.cat.Get(id)
	v := standInView{ID: id, Name: svc.Name, Shown: true, Connections: []standInConn{}}
	if svc.StandIn != nil {
		v.Shown, v.SetBy = svc.StandIn.Shown, svc.StandIn.SetBy
		v.Plays, v.StartsOff = svc.StandIn.Plays, svc.StandIn.StartsOff
		if !svc.StandIn.SetAt.IsZero() {
			v.SetAt = svc.StandIn.SetAt.Format(time.RFC3339)
		}
	}
	on, off, failed := 0, 0, 0
	for _, w := range a.wiringFor(id) {
		c := w.read(ctx)
		switch {
		case c.Error != "":
			failed++
		case c.Connected == nil:
		case *c.Connected:
			on++
		default:
			off++
		}
		v.Connections = append(v.Connections, c)
	}
	switch {
	case failed > 0:
		v.State = "unknown"
	case on > 0 && off > 0:
		v.State = "partial"
	case on > 0:
		v.State = "connected"
	case off > 0:
		v.State = "disconnected"
	default:
		v.State = "unwired"
	}
	return v
}

func (a *app) listStandIns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	out := []standInView{}
	for _, id := range a.cat.StandInIDs() {
		out = append(out, a.standIn(ctx, id))
	}
	httputilx.WriteJSON(w, 200, map[string]any{"stand_ins": out})
}

// applyStandIn does what one preference asks. Shown is the console's own
// and cannot fail; each wiring is switched where it lives, and one that
// fails does not stop the rest — the answer lists what did not take.
func (a *app) applyStandIn(ctx context.Context, id string, pref console.StandInPref, by string) []string {
	var errs []string
	if pref.Shown != nil {
		if err := a.cat.ShowStandIn(id, *pref.Shown, by); err != nil {
			return []string{err.Error()}
		}
	}
	if pref.Connected != nil {
		for _, w := range a.wiringFor(id) {
			c := w.read(ctx)
			if c.Connected == nil && c.Error == "" {
				continue // not wired to the stand-in here: not ours to switch
			}
			if err := w.set(ctx, *pref.Connected, "set by "+by); err != nil {
				errs = append(errs, w.label+": "+err.Error())
			}
		}
	}
	return errs
}

func (a *app) setStandIn(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	id := r.PathValue("id")
	if !a.cat.IsStandIn(id) {
		httputilx.Error(w, 404, fmt.Sprintf("%q is not a stand-in; the lab's are %q", id, a.cat.StandInIDs()))
		return
	}
	var pref console.StandInPref
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&pref); err != nil || (pref.Connected == nil && pref.Shown == nil) {
		httputilx.Error(w, 400, `want {"connected": true|false} and/or {"shown": true|false}`)
		return
	}
	errs := a.applyStandIn(ctx, id, pref, "lab-api")
	v := a.standIn(ctx, id)
	v.Errors = errs
	code := 200
	if len(errs) > 0 && len(errs) >= len(a.wiringFor(id)) {
		code = 502 // nothing took
	}
	httputilx.WriteJSON(w, code, v)
}

// applyPluginStandIns applies a plugin's stand_ins when they are new or
// changed. In the background: registration must not wait on the bank.
func (a *app) applyPluginStandIns(p console.Plugin) {
	if !a.cat.StandInChanged(p.ID, p.StandIns) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for id, pref := range p.StandIns {
			for _, e := range a.applyStandIn(ctx, id, pref, p.ID) {
				log.Printf("lab-api: stand-in %s for plugin %s: %s", id, p.ID, e)
			}
		}
		log.Printf("lab-api: applied %s's stand_ins", p.ID)
	}()
}
