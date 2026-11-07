// Package pod holds the lab's podman pod manifest, fintechlab.yaml, and the
// one way to change it for a deployment: Render.
//
// The manifest in the repo is the local lab -- the one compose.yml runs and
// manifest_test.go holds it to. A deployment differs from it in a handful
// of values (where the platform reaches it, which names its certificates
// carry, who it may call back) and, in the cloud, in having no lab clock.
// Those differences are applied here, to the parsed manifest, rather than
// kept in a second copy of it: a second copy is the drift manifest_test.go
// exists to stop, and a sed over YAML is how the b4b image once stayed on
// localhost (its name has a digit in it).
package pod

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile names. local is the manifest as it is; cloud runs no lab clock.
const (
	ProfileLocal = "local"
	ProfileCloud = "cloud"
)

// Options shape one rendering of the manifest. The zero value renders the
// local lab unchanged.
type Options struct {
	// Profile is ProfileLocal or ProfileCloud. The cloud profile leaves the
	// lab clock out: no clock container, no LAB_CLOCK_URL on any vendor
	// (each then dates what it does by its own wall clock, which is what it
	// does with no clock to follow), and CONSOLE_URL_CLOCK=off so the lab
	// API says the lab is on the real time instead of showing a clock that
	// is down. A deployed platform cannot follow a lab clock, so a lab
	// it talks to must not move.
	Profile string
	// PublicHost is the name or address a platform reaches the lab at. It
	// becomes the lab API's CONSOLE_CONNECT_HOST (the .env downloads) and
	// CONSOLE_BROWSE_HOST (its links), and a SAN on the certificates a
	// client outside the pod dials.
	PublicHost string
	// ExtraSANs are further "DNS:name" or "IP:address" entries for those
	// certificates.
	ExtraSANs []string
	// WebhookAllow is appended to every WEBHOOK_ALLOWLIST: the hosts (or
	// CIDRs) of the platform's own webhook and callback endpoints.
	WebhookAllow []string
	// Set overrides one variable in one container, "container.KEY=value"
	// -- a callback target, a delivery delay. The container must exist.
	Set []string
	// ImagePrefix and Version re-point every image from the manifest's
	// localhost/fintechlab/<service>:v1. Empty keeps that part.
	ImagePrefix string
	Version     string
}

// Container names Render needs to find. Everything else it changes, it
// finds by the variable: the clock and public-host settings go to whichever
// containers read them (the lab API today; the console before it), so a
// service that moves takes its settings with it.
const (
	clockContainer = "clock"
	initContainer  = "init"
)

// The variables Render sets, and so the ones a container must already carry
// for Render to set them there.
const (
	clockURLVar    = "CONSOLE_URL_CLOCK"
	browseHostVar  = "CONSOLE_BROWSE_HOST"
	connectHostVar = "CONSOLE_CONNECT_HOST"
)

var imagePattern = regexp.MustCompile(`^localhost/fintechlab/([a-z0-9-]+):v1$`)

// Render applies o to the manifest src and returns the result.
func Render(src []byte, o Options) ([]byte, error) {
	switch o.Profile {
	case "", ProfileLocal, ProfileCloud:
	default:
		return nil, fmt.Errorf("profile %q: want %q or %q", o.Profile, ProfileLocal, ProfileCloud)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("manifest: want one YAML mapping")
	}
	root := doc.Content[0]
	spec := get(root, "spec")
	if spec == nil {
		return nil, fmt.Errorf("manifest: no spec")
	}
	m := manifest{
		root:       root,
		spec:       spec,
		containers: append(items(get(spec, "initContainers")), items(get(spec, "containers"))...),
	}
	if m.find(initContainer) == nil {
		return nil, fmt.Errorf("manifest: no %s container", initContainer)
	}

	if o.Profile == ProfileCloud {
		if err := m.dropClock(); err != nil {
			return nil, err
		}
	}
	if err := m.publicHost(o.PublicHost, o.ExtraSANs); err != nil {
		return nil, err
	}
	m.allow(o.WebhookAllow)
	for _, s := range o.Set {
		if err := m.set(s); err != nil {
			return nil, err
		}
	}
	m.images(o.ImagePrefix, o.Version)

	profile := o.Profile
	if profile == "" {
		profile = ProfileLocal
	}
	if labels := get(get(root, "metadata"), "labels"); labels != nil {
		put(labels, "profile", profile)
	}
	header := "Rendered from deploy/pod/fintechlab.yaml by deploy/pod/render (profile " + profile + ").\n" +
		"Do not edit: change the source manifest or the render options. docs/deploy-cloud.md."
	doc.HeadComment, root.HeadComment = header, ""
	if len(root.Content) > 0 {
		root.Content[0].HeadComment = ""
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type manifest struct {
	root, spec *yaml.Node
	containers []*yaml.Node // init containers first, then the rest
}

func (m *manifest) find(name string) *yaml.Node {
	for _, c := range m.containers {
		if scalar(get(c, "name")) == name {
			return c
		}
	}
	return nil
}

// readers is every container that carries key in its environment.
func (m *manifest) readers(key string) []*yaml.Node {
	var out []*yaml.Node
	for _, c := range m.containers {
		if _, ok := env(c, key); ok {
			out = append(out, c)
		}
	}
	return out
}

func (m *manifest) dropClock() error {
	clockReaders := m.readers(clockURLVar)
	if len(clockReaders) == 0 {
		return fmt.Errorf("manifest: no container reads %s, so nothing would be told the clock is gone", clockURLVar)
	}
	cs := get(m.spec, "containers")
	kept := cs.Content[:0]
	for _, c := range cs.Content {
		if scalar(get(c, "name")) != clockContainer {
			kept = append(kept, c)
		}
	}
	cs.Content = kept
	m.containers = append(items(get(m.spec, "initContainers")), cs.Content...)

	for _, alias := range items(get(m.spec, "hostAliases")) {
		names := get(alias, "hostnames")
		if names == nil {
			continue
		}
		kept := names.Content[:0]
		for _, n := range names.Content {
			if n.Value != clockContainer {
				kept = append(kept, n)
			}
		}
		names.Content = kept
	}
	for _, c := range m.containers {
		delEnv(c, "LAB_CLOCK_URL")
	}
	for _, c := range clockReaders {
		setEnv(c, clockURLVar, "off")
	}
	return nil
}

func (m *manifest) publicHost(host string, extra []string) error {
	var sans []string
	if host != "" {
		if strings.ContainsAny(host, " ,/:") {
			return fmt.Errorf("public host %q: want a bare name or IPv4 address", host)
		}
		if net.ParseIP(host) != nil {
			sans = append(sans, "IP:"+host)
		} else {
			sans = append(sans, "DNS:"+host)
		}
		readers := m.readers(browseHostVar)
		if len(readers) == 0 {
			return fmt.Errorf("manifest: no container reads %s, so nothing would hand out the public host", browseHostVar)
		}
		for _, c := range readers {
			setEnv(c, connectHostVar, host)
			setEnv(c, browseHostVar, host)
		}
	}
	for _, s := range extra {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "DNS:") && !strings.HasPrefix(s, "IP:") {
			return fmt.Errorf("SAN %q: want DNS:<name> or IP:<address>", s)
		}
		sans = append(sans, s)
	}
	if len(sans) > 0 {
		setEnv(m.find(initContainer), "CERT_EXTRA_SANS", strings.Join(dedupe(sans), ","))
	}
	return nil
}

func (m *manifest) allow(hosts []string) {
	var add []string
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			add = append(add, h)
		}
	}
	if len(add) == 0 {
		return
	}
	for _, c := range m.containers {
		if v, ok := env(c, "WEBHOOK_ALLOWLIST"); ok {
			setEnv(c, "WEBHOOK_ALLOWLIST", strings.Join(dedupe(append(strings.Split(v, ","), add...)), ","))
		}
	}
}

func (m *manifest) set(s string) error {
	target, value, ok := strings.Cut(s, "=")
	name, key, dot := strings.Cut(target, ".")
	if !ok || !dot || name == "" || key == "" {
		return fmt.Errorf("set %q: want container.KEY=value", s)
	}
	c := m.find(name)
	if c == nil {
		return fmt.Errorf("set %q: no container %q in the manifest", s, name)
	}
	setEnv(c, key, value)
	return nil
}

func (m *manifest) images(prefix, version string) {
	if prefix == "" && version == "" {
		return
	}
	for _, c := range m.containers {
		img := get(c, "image")
		if img == nil {
			continue
		}
		sub := imagePattern.FindStringSubmatch(img.Value)
		if sub == nil {
			continue
		}
		p, v := prefix, version
		if p == "" {
			p = "localhost/fintechlab"
		}
		if v == "" {
			v = "v1"
		}
		img.Value = strings.TrimSuffix(p, "/") + "/" + sub[1] + ":" + v
	}
}

// --- yaml.Node plumbing --------------------------------------------------

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func put(m *yaml.Node, key, value string) {
	if v := get(m, key); v != nil {
		*v = *str(value)
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, str(value))
}

func items(seq *yaml.Node) []*yaml.Node {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	return seq.Content
}

func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func env(c *yaml.Node, key string) (string, bool) {
	for _, e := range items(get(c, "env")) {
		if scalar(get(e, "name")) == key {
			return scalar(get(e, "value")), true
		}
	}
	return "", false
}

// setEnv sets key to a plain value, replacing a valueFrom if it had one.
func setEnv(c *yaml.Node, key, value string) {
	list := get(c, "env")
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode}
		c.Content = append(c.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, list)
	}
	for _, e := range list.Content {
		if scalar(get(e, "name")) != key {
			continue
		}
		e.Content = []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: key},
			{Kind: yaml.ScalarNode, Value: "value"}, str(value),
		}
		return
	}
	list.Content = append(list.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: key},
		{Kind: yaml.ScalarNode, Value: "value"}, str(value),
	}})
}

func delEnv(c *yaml.Node, key string) {
	list := get(c, "env")
	if list == nil {
		return
	}
	kept := list.Content[:0]
	for _, e := range list.Content {
		if scalar(get(e, "name")) != key {
			kept = append(kept, e)
		}
	}
	list.Content = kept
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
