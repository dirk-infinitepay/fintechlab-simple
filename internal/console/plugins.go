package console

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Plugins: a platform under test puts its own card in this console.
//
// The lab used to carry one particular platform — buddy's local runner — as
// a catalogue entry, six proxy routes and a dozen special cases in the UI.
// Every feature of that runner was a release of this lab, and a second
// platform would have needed all of it again. The lab simulates vendors; the
// system under test is somebody else's, so its card is too.
//
// A plugin is a service that registers itself: it POSTs a descriptor to
// /api/plugins and renews it on a timer. The descriptor is a catalogue entry
// (the same fields every vendor card is drawn from) plus the few things only
// a platform has — buttons, the settlement files it can run, and whether it
// follows the lab clock. The console draws it with the components it
// already has and forwards each button to the path the plugin declared, and
// nothing else. docs/plugins.md is the contract.

// ClockPolicy is whether a platform moves with the lab clock.
type ClockPolicy string

const (
	// ClockFollows: the platform reads the lab clock's offset and applies
	// it, as buddy's runner does through its clock shim.
	ClockFollows ClockPolicy = "follows"
	// ClockWall: the platform cannot be moved — a deployed stack — so the
	// lab must stay on the real time too, and the console refuses to move it.
	ClockWall ClockPolicy = "wall"
)

// PluginAction is one button: a label and the one call it makes.
type PluginAction struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	// Primary makes it the card's one filled button.
	Primary bool `json:"primary,omitempty"`
	// Confirm, when set, is asked before the call: what the action does
	// that cannot be taken back.
	Confirm string `json:"confirm,omitempty"`
	// Note is the toast when the platform's answer carries none.
	Note string `json:"note,omitempty"`
	// Fields are inputs asked for before the call, in a form in a modal;
	// the call's JSON body is {"<field id>": value}. The console draws
	// them from this list and nothing else (design-system.md, Action form).
	Fields []ActionField `json:"fields,omitempty"`
}

// ActionField is one input of an action's form.
type ActionField struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Type is "number", "text" or "select".
	Type string `json:"type"`
	// Default is a number for a number field, text otherwise.
	Default any      `json:"default,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	// Placeholder is shown in an empty text field.
	Placeholder string `json:"placeholder,omitempty"`
	// Options are a select's choices.
	Options []ActionOption `json:"options,omitempty"`
	// Hint is a sentence under the form: what the field changes.
	Hint string `json:"hint,omitempty"`
}

// ActionOption is one choice of a select field.
type ActionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// PluginSettlement is a platform that settles Worldline files: the three
// endpoints the lab's own views read, in the shapes docs/plugins.md fixes.
type PluginSettlement struct {
	// FilesPath lists the files the platform can run (the files table).
	FilesPath string `json:"files_path"`
	// RunPath starts runs: POST {"files": [...]}.
	RunPath string `json:"run_path"`
	// StatusPath says what is running and how the last run went, for the
	// Dashboard diagram.
	StatusPath string `json:"status_path,omitempty"`
	// UploadPath takes a file from anywhere: POST ?name=<file name> with the
	// raw bytes, DELETE ?name= to remove one it took. Optional.
	UploadPath string `json:"upload_path,omitempty"`
	// PreviewPath shows the head of a file: GET ?name=&lines=. Optional.
	PreviewPath string `json:"preview_path,omitempty"`
	// DeletePath deletes a listed file the platform marked deletable:
	// DELETE ?name=. Optional; without it, only uploads can be removed
	// (DELETE upload_path).
	DeletePath string `json:"delete_path,omitempty"`
	// RevealPath opens the folder a file is in, in the file manager of the
	// machine the platform runs on: POST {"name": "<file>"}. Optional —
	// only the platform can do it, since only it is on that machine.
	RevealPath string `json:"reveal_path,omitempty"`
	// RunsLog names the activity log that counts runs.
	RunsLog string `json:"runs_log,omitempty"`
	// Stages maps the diagram's two self-arrows ("ingest", "reports") to
	// the platform's own stage names.
	Stages map[string][]string `json:"stages,omitempty"`
}

// Plugin is what a platform registers.
type Plugin struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Summary      string            `json:"summary"`
	BaseURL      string            `json:"base_url"`
	BrowseURL    string            `json:"browse_url,omitempty"`
	HealthPath   string            `json:"health_path"`
	ActivityPath string            `json:"activity_path,omitempty"`
	Ports        []string          `json:"ports,omitempty"`
	Transport    string            `json:"transport,omitempty"`
	Auth         string            `json:"auth,omitempty"`
	SwapFor      string            `json:"swap_for,omitempty"`
	Docs         string            `json:"docs,omitempty"`
	Endpoints    []Endpoint        `json:"endpoints,omitempty"`
	Clock        ClockPolicy       `json:"clock"`
	StartHint    string            `json:"start_hint,omitempty"`
	Actions      []PluginAction    `json:"actions,omitempty"`
	Settlement   *PluginSettlement `json:"settlement,omitempty"`
	EmptyHints   map[string]string `json:"empty_hints,omitempty"`
	// StandIns is how the platform wants the lab's stand-ins: connected or
	// not, shown or not. Applied when it first registers and whenever it
	// changes, not on every renewal (standins.go).
	StandIns map[string]StandInPref `json:"stand_ins,omitempty"`
	// Env is the environment that registered the plugin
	// (docs/lab-api/environments.md §9): set by the lab API from the
	// registering token, never taken from the descriptor. Empty is the
	// default's.
	Env string `json:"env,omitempty"`
}

// Registration states, as the card says them.
const (
	PluginLive    = "live"    // renewed within its TTL
	PluginLapsed  = "lapsed"  // stopped renewing without saying goodbye
	PluginStopped = "stopped" // unregistered itself
)

// PluginInfo is the plugin half of a Service, as the page reads it.
type PluginInfo struct {
	Clock        ClockPolicy       `json:"clock"`
	StartHint    string            `json:"start_hint,omitempty"`
	Actions      []PluginAction    `json:"actions,omitempty"`
	Settlement   *PluginSettlement `json:"settlement,omitempty"`
	EmptyHints   map[string]string `json:"empty_hints,omitempty"`
	State        string            `json:"state"`
	Env          string            `json:"env,omitempty"`
	RegisteredAt time.Time         `json:"registered_at"`
	LastSeen     time.Time         `json:"last_seen"`
	TTLSeconds   int               `json:"ttl_seconds"`
}

type registration struct {
	p            Plugin
	registeredAt time.Time
	lastSeen     time.Time
	stopped      bool
}

// PluginTTL is how long a registration stands without being renewed.
// Plugins renew at a third of it, so one lost renewal is not a lapse.
const PluginTTL = 30 * time.Second

// ErrBadPlugin is a descriptor the console refuses.
var ErrBadPlugin = errors.New("bad plugin descriptor")

var (
	pluginID  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	actionID  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	logName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)
	fieldID   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	maxAction = 8
	maxFields = 6
	maxOption = 20
)

// validFields checks one action's form: the console draws it, so a field
// it cannot draw is refused here rather than rendered as nothing.
func validFields(action string, fields []ActionField) error {
	if len(fields) > maxFields {
		return badPlugin("action %s: at most %d fields", action, maxFields)
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if !fieldID.MatchString(f.ID) || seen[f.ID] {
			return badPlugin("action %s: field id %q must be unique, lower-case letters, digits and underscores", action, f.ID)
		}
		seen[f.ID] = true
		if strings.TrimSpace(f.Label) == "" {
			return badPlugin("action %s field %s needs a label", action, f.ID)
		}
		switch f.Type {
		case "number":
			if f.Default != nil {
				if _, ok := f.Default.(float64); !ok {
					return badPlugin("action %s field %s: default must be a number", action, f.ID)
				}
			}
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				return badPlugin("action %s field %s: min is above max", action, f.ID)
			}
		case "text":
			if f.Default != nil {
				if _, ok := f.Default.(string); !ok {
					return badPlugin("action %s field %s: default must be text", action, f.ID)
				}
			}
		case "select":
			if len(f.Options) == 0 || len(f.Options) > maxOption {
				return badPlugin("action %s field %s: a select needs 1 to %d options", action, f.ID, maxOption)
			}
			found := f.Default == nil
			for _, o := range f.Options {
				if strings.TrimSpace(o.Label) == "" {
					return badPlugin("action %s field %s: every option needs a value and a label", action, f.ID)
				}
				if f.Default == o.Value {
					found = true
				}
			}
			if !found {
				return badPlugin("action %s field %s: default %v is not one of its options", action, f.ID, f.Default)
			}
		default:
			return badPlugin("action %s field %s: type %q — number, text or select", action, f.ID, f.Type)
		}
	}
	return nil
}

func badPlugin(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadPlugin, fmt.Sprintf(format, args...))
}

// pluginPath is a path the console will append to the plugin's base URL:
// absolute, and unable to climb out of it or name another host.
func pluginPath(field, p string, required bool) error {
	if p == "" {
		if required {
			return badPlugin("%s is required", field)
		}
		return nil
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, "..") || strings.ContainsAny(p, " \t\r\n\\") {
		return badPlugin("%s %q must be an absolute path on the plugin's own base_url", field, p)
	}
	return nil
}

func (p *Plugin) validate(static func(string) bool) error {
	if !pluginID.MatchString(p.ID) {
		return badPlugin("id %q must be lower-case letters, digits and dashes", p.ID)
	}
	if static(p.ID) {
		return badPlugin("id %q is one of the lab's own services", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return badPlugin("name is required")
	}
	for field, raw := range map[string]string{"base_url": p.BaseURL, "browse_url": p.BrowseURL} {
		if raw == "" && field == "browse_url" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return badPlugin("%s %q must be an http(s) URL", field, raw)
		}
	}
	p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	if err := pluginPath("health_path", p.HealthPath, true); err != nil {
		return err
	}
	if err := pluginPath("activity_path", p.ActivityPath, false); err != nil {
		return err
	}
	switch p.Clock {
	case ClockFollows, ClockWall:
	default:
		return badPlugin(`clock must be "follows" or "wall", got %q`, p.Clock)
	}
	if len(p.Actions) > maxAction {
		return badPlugin("at most %d actions", maxAction)
	}
	seen := map[string]bool{}
	for i := range p.Actions {
		a := &p.Actions[i]
		if !actionID.MatchString(a.ID) || seen[a.ID] {
			return badPlugin("action id %q must be unique, lower-case letters, digits and dashes", a.ID)
		}
		seen[a.ID] = true
		if strings.TrimSpace(a.Label) == "" {
			return badPlugin("action %s needs a label", a.ID)
		}
		if a.Method == "" {
			a.Method = "POST"
		}
		switch a.Method {
		case "POST", "PUT", "DELETE":
		default:
			return badPlugin("action %s: method %q — actions change something, so POST, PUT or DELETE", a.ID, a.Method)
		}
		if err := pluginPath("action "+a.ID+" path", a.Path, true); err != nil {
			return err
		}
		if err := validFields(a.ID, a.Fields); err != nil {
			return err
		}
	}
	if s := p.Settlement; s != nil {
		if err := pluginPath("settlement.files_path", s.FilesPath, true); err != nil {
			return err
		}
		if err := pluginPath("settlement.run_path", s.RunPath, true); err != nil {
			return err
		}
		if err := pluginPath("settlement.status_path", s.StatusPath, false); err != nil {
			return err
		}
		if err := pluginPath("settlement.upload_path", s.UploadPath, false); err != nil {
			return err
		}
		if err := pluginPath("settlement.preview_path", s.PreviewPath, false); err != nil {
			return err
		}
		if err := pluginPath("settlement.delete_path", s.DeletePath, false); err != nil {
			return err
		}
		if err := pluginPath("settlement.reveal_path", s.RevealPath, false); err != nil {
			return err
		}
		if s.RunsLog != "" && !logName.MatchString(s.RunsLog) {
			return badPlugin("settlement.runs_log %q is not a log name", s.RunsLog)
		}
	}
	for name := range p.EmptyHints {
		if !logName.MatchString(name) {
			return badPlugin("empty_hints key %q is not a log name", name)
		}
	}
	return nil
}

func (c *Catalogue) clockNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Catalogue) static(id string) bool {
	for _, s := range c.Services {
		if s.ID == id {
			return true
		}
	}
	return false
}

// Register adds or renews a plugin. Renewing with a changed descriptor
// replaces it: the platform is the authority on its own card.
func (c *Catalogue) Register(p Plugin) (PluginInfo, error) {
	if err := p.validate(c.static); err != nil {
		return PluginInfo{}, err
	}
	if err := c.validStandIns(p.StandIns); err != nil {
		return PluginInfo{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clockNow().UTC()
	for _, r := range c.plugins {
		if r.p.ID == p.ID {
			if tenant.Normalize(r.p.Env) != tenant.Normalize(p.Env) {
				return PluginInfo{}, badPlugin("%s is registered by environment %s: a plugin id is one platform's across the lab", p.ID, tenant.Normalize(r.p.Env))
			}
			if r.stopped {
				r.registeredAt = now
			}
			r.p, r.lastSeen, r.stopped = p, now, false
			return c.infoLocked(r), nil
		}
	}
	r := &registration{p: p, registeredAt: now, lastSeen: now}
	c.plugins = append(c.plugins, r)
	return c.infoLocked(r), nil
}

// Unregister marks a plugin stopped. The card stays, grey, with the
// plugin's own words on how to start it again: a platform that was running
// a minute ago is one the operator is likely to want back.
func (c *Catalogue) Unregister(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.plugins {
		if r.p.ID == id {
			r.stopped = true
			return true
		}
	}
	return false
}

func (c *Catalogue) infoLocked(r *registration) PluginInfo {
	state := PluginLive
	switch {
	case r.stopped:
		state = PluginStopped
	case c.clockNow().Sub(r.lastSeen) > PluginTTL:
		state = PluginLapsed
	}
	return PluginInfo{
		Clock: r.p.Clock, StartHint: r.p.StartHint, Actions: r.p.Actions,
		Settlement: r.p.Settlement, EmptyHints: r.p.EmptyHints,
		State: state, Env: r.p.Env, RegisteredAt: r.registeredAt, LastSeen: r.lastSeen,
		TTLSeconds: int(PluginTTL / time.Second),
	}
}

func (c *Catalogue) serviceLocked(r *registration) Service {
	p := r.p
	info := c.infoLocked(r)
	return Service{
		ID: p.ID, Name: p.Name, Kind: KindPlatform, Summary: p.Summary,
		BaseURL: p.BaseURL, Browse: p.BrowseURL, HealthPath: p.HealthPath,
		Ports: p.Ports, Transport: p.Transport, Auth: p.Auth, SwapFor: p.SwapFor,
		Docs: p.Docs, Activity: p.ActivityPath, Endpoints: p.Endpoints,
		Plugin: &info,
	}
}

// Plugins is every registered plugin as a Service, in registration order.
func (c *Catalogue) Plugins() []Service { return c.PluginsIn(tenant.All) }

// PluginsIn is the plugins scope's environments registered.
func (c *Catalogue) PluginsIn(scope string) []Service {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Service, 0, len(c.plugins))
	for _, r := range c.plugins {
		if tenant.InScope(scope, r.p.Env) {
			out = append(out, c.serviceLocked(r))
		}
	}
	return out
}

// RemoveEnv forgets every plugin env registered: the environment is gone.
func (c *Catalogue) RemoveEnv(env string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.plugins[:0]
	n := 0
	for _, r := range c.plugins {
		if tenant.Normalize(r.p.Env) == tenant.Normalize(env) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	c.plugins = kept
	return n
}

// All is the lab's own services with the registered plugins first: the
// system under test heads its view, the stand-ins below it are reference.
func (c *Catalogue) All() []Service { return c.AllIn(tenant.All) }

// AllIn is All with only scope's plugins.
func (c *Catalogue) AllIn(scope string) []Service {
	out := c.PluginsIn(scope)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.Services {
		out = append(out, c.overlayLocked(s))
	}
	return out
}

// Plugin returns the registered descriptor for id.
func (c *Catalogue) Plugin(id string) (Plugin, PluginInfo, bool) { return c.PluginIn(tenant.All, id) }

// PluginIn returns id's descriptor when one of scope's environments
// registered it.
func (c *Catalogue) PluginIn(scope, id string) (Plugin, PluginInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, r := range c.plugins {
		if r.p.ID == id && tenant.InScope(scope, r.p.Env) {
			return r.p, c.infoLocked(r), true
		}
	}
	return Plugin{}, PluginInfo{}, false
}

// SettlementPlugin is the platform the Dashboard diagram draws: the
// first live plugin that settles, or failing that the first one that ever
// did, so a stopped runner still shows its last run.
func (c *Catalogue) SettlementPlugin() (Plugin, PluginInfo, bool) {
	return c.SettlementPluginIn(tenant.All)
}

// SettlementPluginIn is SettlementPlugin among scope's plugins.
func (c *Catalogue) SettlementPluginIn(scope string) (Plugin, PluginInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var fallback *registration
	for _, r := range c.plugins {
		if r.p.Settlement == nil || !tenant.InScope(scope, r.p.Env) {
			continue
		}
		if c.infoLocked(r).State == PluginLive {
			return r.p, c.infoLocked(r), true
		}
		if fallback == nil {
			fallback = r
		}
	}
	if fallback != nil {
		return fallback.p, c.infoLocked(fallback), true
	}
	return Plugin{}, PluginInfo{}, false
}

// WallClockPlugin names a live plugin that cannot follow the lab clock, if
// any: while one is registered, moving the clock would put the lab's
// vendors on a different day from the platform they serve.
func (c *Catalogue) WallClockPlugin() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, r := range c.plugins {
		if r.p.Clock == ClockWall && c.infoLocked(r).State == PluginLive {
			return r.p.Name, true
		}
	}
	return "", false
}
