package trap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Routes serves the traps on a sim's control port (traps.md §8):
//
//	GET    /sim/traps/hooks    the catalogue: each hook, its fields, its graph
//	GET    /sim/traps          the traps, newest first; ?state= filters
//	POST   /sim/traps          arm one: 201 with it, 400 with the reason
//	GET    /sim/traps/{id}     one trap
//	DELETE /sim/traps/{id}     disarm it and cancel its pending steps
//	DELETE /sim/traps          disarm every one
func (s *Store) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sim/traps/hooks", s.handleHooks)
	mux.HandleFunc("GET /sim/traps", s.handleList)
	mux.HandleFunc("POST /sim/traps", s.handleArm)
	mux.HandleFunc("GET /sim/traps/{id}", s.handleGet)
	mux.HandleFunc("DELETE /sim/traps/{id}", s.handleDisarm)
	mux.HandleFunc("DELETE /sim/traps", s.handleDisarmAll)
}

// hookView is a hook as the catalogue shows it: the documentation an agent
// reads before arming anything.
type hookView struct {
	Hook
	FirstAfter string `json:"first_step_default_after,omitempty"`
}

func (s *Store) handleHooks(w http.ResponseWriter, r *http.Request) {
	out := make([]hookView, 0, len(s.order))
	for _, name := range s.order {
		h := s.hooks[name]
		v := hookView{Hook: h}
		if h.Effect == EffectPath {
			v.FirstAfter = h.FirstAfter.String()
		}
		out = append(out, v)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"service": s.service, "hooks": out})
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	switch state {
	case "", StateArmed, StateSpent, StateExpired, StateDisarmed:
	default:
		httputilx.Error(w, 400, "state is armed, spent, expired or disarmed")
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"service": s.service, "traps": s.ListIn(tenant.FromRequest(r), state)})
}

func (s *Store) handleGet(w http.ResponseWriter, r *http.Request) {
	t, ok := s.GetIn(tenant.FromRequest(r), r.PathValue("id"))
	if !ok {
		httputilx.Error(w, 404, "no trap "+r.PathValue("id"))
		return
	}
	httputilx.WriteJSON(w, 200, t)
}

func (s *Store) handleDisarm(w http.ResponseWriter, r *http.Request) {
	t, cancelled, ok := s.DisarmIn(tenant.FromRequest(r), r.PathValue("id"))
	if !ok {
		httputilx.Error(w, 404, "no trap "+r.PathValue("id"))
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"trap": t, "cancelled": cancelled})
}

func (s *Store) handleDisarmAll(w http.ResponseWriter, r *http.Request) {
	n, cancelled := s.DisarmAllIn(tenant.FromRequest(r))
	httputilx.WriteJSON(w, 200, map[string]any{"service": s.service, "disarmed": n, "cancelled": cancelled})
}

// armBody is POST /sim/traps. Times is raw so that absent (1) and null
// (until disarmed) can be told apart.
type armBody struct {
	Label     string          `json:"label"`
	Hook      string          `json:"hook"`
	Match     map[string]any  `json:"match"`
	Times     json.RawMessage `json:"times"`
	ExpiresAt *time.Time      `json:"expires_at"`
	Effect    *struct {
		Path    []Step    `json:"path"`
		Respond *Response `json:"respond"`
		Delay   string    `json:"delay"`
		Close   bool      `json:"close"`
	} `json:"effect"`
}

func (s *Store) handleArm(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	spec, err := ParseSpec(raw)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// A trap is armed in the environment the call names, and takes only
	// that environment's events.
	if spec.Env = tenant.FromRequest(r); spec.Env == tenant.All {
		httputilx.Error(w, 400, "a trap is armed in one environment, not in all of them")
		return
	}
	t, err := s.Arm(spec)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	httputilx.WriteJSON(w, 201, t)
}

// ParseSpec reads a trap as POST /sim/traps takes it, strictly: an unknown
// field is refused, because a typo must not arm a different trap.
func ParseSpec(raw []byte) (Spec, error) {
	var b armBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Spec{}, fmt.Errorf("%w: want {\"hook\", \"match\", \"effect\": {\"path\": […]} or {\"respond\"|\"delay\"|\"close\": …}, \"times\", \"label\", \"expires_at\"}: %v", ErrInvalid, err)
	}
	spec := Spec{Label: b.Label, Hook: b.Hook, Match: b.Match, ExpiresAt: b.ExpiresAt}
	if spec.Hook == "" {
		return Spec{}, fmt.Errorf("%w: hook is required; GET /sim/traps/hooks lists them", ErrInvalid)
	}
	if b.Match == nil {
		return Spec{}, fmt.Errorf("%w: match is required; {} takes every event at the hook", ErrInvalid)
	}
	switch t := bytes.TrimSpace(b.Times); {
	case len(t) == 0:
		spec.Times = 1
	case string(t) == "null":
		spec.Forever = true
	default:
		var n int
		if err := json.Unmarshal(t, &n); err != nil || n < 1 {
			return Spec{}, fmt.Errorf("%w: times is a whole number from 1, or null for every event until disarmed", ErrInvalid)
		}
		spec.Times = n
	}
	if b.Effect == nil {
		return Spec{}, fmt.Errorf("%w: effect is required", ErrInvalid)
	}
	spec.Path = b.Effect.Path
	spec.Respond, spec.Delay, spec.Close = b.Effect.Respond, b.Effect.Delay, b.Effect.Close
	return spec, nil
}

// IsInvalid reports whether err is a trap refused as invalid (a 400).
func IsInvalid(err error) bool { return errors.Is(err, ErrInvalid) }
