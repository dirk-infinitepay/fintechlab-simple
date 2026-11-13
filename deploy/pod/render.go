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
	"path"
	"regexp"
	"sort"
	"strconv"
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
	// SeederImage, when set, adds the platform's seeder to the pod
	// (docs/lab-api/plan.md, phase 6.1): a container from the platform's
	// own image, on the lab API's data volume at the same path, which the
	// lab API reaches at LAB_SEEDER_URL. Empty: a lab without one.
	SeederImage string
	// HTTPSHost, when set, serves the console at https://<HTTPSHost>, every
	// vendor port's service at https://<service>.<HTTPSHost> and the lab API
	// at https://lab-api.<HTTPSHost>: a front container (FrontImage, Caddy) on
	// :443 that gets a Let's Encrypt certificate for each name and renews
	// it, and on :80 answers the ACME HTTP challenge and redirects the rest
	// to https. Let's Encrypt has to reach :80 from the internet; :443 is
	// for whoever may reach the console. A TLS sim (Banking Circle) is
	// called over TLS with the lab's client certificate, and the front asks
	// the caller for one as the sim's BC_MTLS says. The certificates are
	// kept on the front-data volume, so a new deploy does not ask for them
	// again. When HTTPSHost is PublicHost, the lab API is told the names
	// (CONSOLE_CONNECT_NAMES) and hands them out in the .env downloads.
	// Empty: no front; every surface on its own port only.
	HTTPSHost string
	// FrontImage is the front's image, required with HTTPSHost and
	// ignored without it.
	FrontImage string
}

// imageRef is a container image reference: registry/path:tag or @digest.
var imageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]*$`)

// seederPort is where the seeder listens, inside the pod.
const seederPort = "8098"

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
	if o.SeederImage != "" {
		if err := m.seeder(o.SeederImage); err != nil {
			return nil, err
		}
	}
	if o.HTTPSHost != "" {
		if err := m.front(o.HTTPSHost, o.FrontImage, o.HTTPSHost == o.PublicHost); err != nil {
			return nil, err
		}
	}

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

// seeder adds the platform's seeder: its container, its name among the
// pod's host aliases, and its address on the lab API.
func (m *manifest) seeder(image string) error {
	api := m.find("lab-api")
	if api == nil {
		return fmt.Errorf("manifest: no lab-api container for the seeder to serve")
	}
	var mount string
	for _, vm := range items(get(api, "volumeMounts")) {
		if scalar(get(vm, "name")) == "console-data" {
			mount = scalar(get(vm, "mountPath"))
		}
	}
	if mount == "" {
		return fmt.Errorf("manifest: lab-api mounts no console-data volume for the seeder to share")
	}
	if !imageRef.MatchString(image) {
		return fmt.Errorf("seeder image %q: want a container image reference", image)
	}
	var c yaml.Node
	// LAB_SEEDER_STATE_DIR on the shared volume: the seeder remembers the
	// highest MID it handed out there, so a restart never hands one out
	// again.
	src := "name: seeder\nimage: " + image + "\nenv:\n  - name: PORT\n    value: \"" + seederPort + "\"\n" +
		"  - name: LAB_SEEDER_STATE_DIR\n    value: " + mount + "/seeder\n" +
		"volumeMounts:\n  - name: console-data\n    mountPath: " + mount + "\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		return err
	}
	containers := get(m.spec, "containers")
	if containers == nil {
		return fmt.Errorf("manifest: no containers")
	}
	containers.Content = append(containers.Content, c.Content[0])
	m.containers = append(m.containers, c.Content[0])
	for _, alias := range items(get(m.spec, "hostAliases")) {
		if names := get(alias, "hostnames"); names != nil && scalar(get(alias, "ip")) == "127.0.0.1" {
			names.Content = append(names.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "seeder"})
			break
		}
	}
	setEnv(api, "LAB_SEEDER_URL", "http://seeder:"+seederPort)
	return nil
}

// The front: what it is called in the pod, the volume its certificates
// live on, and what it serves.
const (
	frontContainer = "front"
	frontVolume    = "front-data"
	frontClaim     = "fintechlab-front-data"
	consolePort    = "8090"
	// labAPIName is the lab API's name under the host, its container's:
	// lab-api.<host>. Not api.<host>: Let's Encrypt refuses that one under
	// a name this deep, as a sign of runaway on-demand issuance.
	labAPIName = "lab-api"
	// The vendors' own ports: each container publishing one is served at
	// <container>.<host>, on 443. The console, 8090, is the host itself.
	vendorPortLow, vendorPortHigh = 8080, 8094
	// namesVar tells the lab API which names the front serves, so the
	// .env downloads hand those out (cmd/labapi/connect.go).
	namesVar = "CONSOLE_CONNECT_NAMES"
)

// dnsName is a host name a public CA issues for: two labels or more.
var dnsName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// frontSite is one name the front serves under the host, and where to.
type frontSite struct {
	name, port string
	// tls: the upstream speaks TLS, with a certificate from the lab's CA
	// (caFile) that names the container. clientAuth is what the front
	// asks of a caller before forwarding -- "require", "optional" or
	// "off" -- the upstream's own setting, enforced at the edge now that
	// the caller's certificate stops there. The front presents the lab's
	// client certificate (beside caFile, ca/generate.sh) to the upstream.
	tls        bool
	caFile     string
	clientAuth string
	keysMount  *yaml.Node // the upstream's mount that holds caFile
}

// frontSites is every name the front serves under the host: each vendor
// port's container, and the lab API as api.
func (m *manifest) frontSites() ([]frontSite, error) {
	var sites []frontSite
	for _, c := range items(get(m.spec, "containers")) {
		name := scalar(get(c, "name"))
		for _, p := range items(get(c, "ports")) {
			n, err := strconv.Atoi(scalar(get(p, "containerPort")))
			if err != nil || n < vendorPortLow || n > vendorPortHigh || strconv.Itoa(n) == consolePort {
				continue
			}
			site := frontSite{name: name, port: strconv.Itoa(n)}
			if _, ok := env(c, "TLS_CERT"); ok {
				ca, _ := env(c, "CA_FILE")
				if ca == "" {
					return nil, fmt.Errorf("manifest: %s serves TLS on %d but names no CA_FILE for the front to trust", name, n)
				}
				site.tls, site.caFile, site.clientAuth = true, ca, "off"
				if mode, ok := env(c, "BC_MTLS"); ok {
					if site.clientAuth, err = clientAuthOf(mode); err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
				}
				for _, vm := range items(get(c, "volumeMounts")) {
					if mp := scalar(get(vm, "mountPath")); mp != "" && strings.HasPrefix(ca, strings.TrimSuffix(mp, "/")+"/") {
						site.keysMount = vm
					}
				}
				if site.keysMount == nil {
					return nil, fmt.Errorf("manifest: %s's CA_FILE %s is on no volume it mounts", name, ca)
				}
			}
			sites = append(sites, site)
		}
	}
	api := m.find("lab-api")
	if api == nil {
		return nil, fmt.Errorf("manifest: no lab-api container for the front to serve as %s", labAPIName)
	}
	port := scalar(get(firstOf(get(api, "ports")), "containerPort"))
	if port == "" {
		return nil, fmt.Errorf("manifest: lab-api publishes no port for the front to serve")
	}
	sites = append(sites, frontSite{name: labAPIName, port: port})
	sort.Slice(sites, func(i, j int) bool { return sites[i].name < sites[j].name })
	return sites, nil
}

// clientAuthOf is BC_MTLS's value as the front's client_auth mode, read as
// cmd/bankingcircle reads it.
func clientAuthOf(mode string) (string, error) {
	switch mode {
	case "off", "false", "none":
		return "off", nil
	case "optional", "verify-if-given":
		return "optional", nil
	case "require", "required", "true", "":
		return "require", nil
	}
	return "", fmt.Errorf("BC_MTLS=%q, want off, optional or require", mode)
}

func firstOf(seq *yaml.Node) *yaml.Node {
	if list := items(seq); len(list) > 0 {
		return list[0]
	}
	return nil
}

// caddyfile is the front's whole configuration: the console at host, and
// every site at <name>.<host>.
func caddyfile(host string, sites []frontSite) string {
	var b strings.Builder
	// admin off: nothing else in the pod may reconfigure it. Only the HTTP
	// challenge: :443 is not open to Let's Encrypt, only to the lab's lists.
	b.WriteString("{\n\tadmin off\n\tcert_issuer acme {\n\t\tdisable_tlsalpn_challenge\n\t}\n}\n")
	fmt.Fprintf(&b, "\n%s {\n\treverse_proxy 127.0.0.1:%s\n}\n", host, consolePort)
	for _, s := range sites {
		if !s.tls {
			fmt.Fprintf(&b, "\n%s.%s {\n\treverse_proxy 127.0.0.1:%s\n}\n", s.name, host, s.port)
			continue
		}
		fmt.Fprintf(&b, "\n%s.%s {\n", s.name, host)
		if mode := map[string]string{"require": "require_and_verify", "optional": "verify_if_given"}[s.clientAuth]; mode != "" {
			fmt.Fprintf(&b, "\ttls {\n\t\tclient_auth {\n\t\t\tmode %s\n\t\t\ttrust_pool file %s\n\t\t}\n\t}\n", mode, s.caFile)
		}
		dir := path.Dir(s.caFile)
		fmt.Fprintf(&b, "\treverse_proxy https://127.0.0.1:%s {\n\t\ttransport http {\n\t\t\ttls_server_name %s\n\t\t\ttls_trust_pool file %s\n\t\t\ttls_client_auth %s %s\n\t\t}\n\t}\n}\n",
			s.port, s.name, s.caFile, path.Join(dir, "client.pem"), path.Join(dir, "client-key.pem"))
	}
	return b.String()
}

// front adds the HTTPS front: a Caddy container on :80 and :443 that serves
// the console at host and every vendor at <service>.<host>, each with a
// Let's Encrypt certificate. Its Caddyfile travels in its environment: the
// image is Caddy's own, unchanged. With names, the lab API is told which
// names the front serves, so the .env downloads hand them out -- only when
// host is the lab's public host, the one the downloads are addressed to.
func (m *manifest) front(host, image string, names bool) error {
	switch {
	case image == "":
		return fmt.Errorf("HTTPS host %q: give the front's image too", host)
	case !dnsName.MatchString(host):
		return fmt.Errorf("HTTPS host %q: want a DNS name a public CA can issue for (not an address)", host)
	case !imageRef.MatchString(image):
		return fmt.Errorf("front image %q: want a container image reference", image)
	case m.find("console") == nil:
		return fmt.Errorf("manifest: no console container for the front to serve")
	}
	sites, err := m.frontSites()
	if err != nil {
		return err
	}
	type envVar struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	}
	type port struct {
		ContainerPort int `yaml:"containerPort"`
		HostPort      int `yaml:"hostPort"`
	}
	type mount struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly,omitempty"`
	}
	mounts := []mount{{frontVolume, "/data", false}}
	seen := map[string]bool{}
	for _, s := range sites {
		if s.keysMount == nil {
			continue
		}
		vm := mount{scalar(get(s.keysMount, "name")), scalar(get(s.keysMount, "mountPath")), true}
		if !seen[vm.Name] {
			seen[vm.Name] = true
			mounts = append(mounts, vm)
		}
	}
	c := struct {
		Name         string   `yaml:"name"`
		Image        string   `yaml:"image"`
		Command      []string `yaml:"command"`
		Ports        []port   `yaml:"ports"`
		Env          []envVar `yaml:"env"`
		VolumeMounts []mount  `yaml:"volumeMounts"`
	}{
		Name:  frontContainer,
		Image: image,
		Command: []string{"sh", "-c",
			`printf '%s' "$CADDYFILE" > /etc/caddy/Caddyfile && exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile`},
		Ports:        []port{{80, 80}, {443, 443}},
		Env:          []envVar{{"CADDYFILE", caddyfile(host, sites)}},
		VolumeMounts: mounts,
	}
	node, err := toNode(c)
	if err != nil {
		return err
	}
	containers := get(m.spec, "containers")
	if containers == nil {
		return fmt.Errorf("manifest: no containers")
	}
	containers.Content = append(containers.Content, node)
	m.containers = append(m.containers, node)

	vol, err := toNode(map[string]any{"name": frontVolume, "persistentVolumeClaim": map[string]string{"claimName": frontClaim}})
	if err != nil {
		return err
	}
	volumes := get(m.spec, "volumes")
	if volumes == nil {
		return fmt.Errorf("manifest: no volumes")
	}
	volumes.Content = append(volumes.Content, vol)

	if names {
		list := make([]string, 0, len(sites))
		for _, s := range sites {
			list = append(list, s.name)
		}
		readers := m.readers(browseHostVar)
		if len(readers) == 0 {
			return fmt.Errorf("manifest: no container reads %s, so nothing would hand out the front's names", browseHostVar)
		}
		for _, c := range readers {
			setEnv(c, namesVar, strings.Join(list, ","))
		}
	}
	return nil
}

// toNode is v as a YAML node, to add to the manifest.
func toNode(v any) (*yaml.Node, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
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
