package pod

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func source(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("fintechlab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func render(t *testing.T, o Options) podFile {
	t.Helper()
	out, err := Render(source(t), o)
	if err != nil {
		t.Fatalf("Render(%+v): %v", o, err)
	}
	var p podFile
	if err := yaml.Unmarshal(out, &p); err != nil {
		t.Fatalf("rendered manifest does not parse: %v\n%s", err, out)
	}
	return p
}

func envOf(c podContainer) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func containers(p podFile) map[string]podContainer {
	m := map[string]podContainer{}
	for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		m[c.Name] = c
	}
	return m
}

// A deployed platform cannot follow a lab clock, so the lab it talks to
// must not have one to move: no container, no name for it, no vendor
// following it, and a console that knows.
func TestTheCloudProfileRunsNoClock(t *testing.T) {
	p := render(t, Options{Profile: ProfileCloud})
	cs := containers(p)
	if _, ok := cs["clock"]; ok {
		t.Error("the cloud profile still runs the clock container")
	}
	for _, h := range p.Spec.HostAliases {
		for _, n := range h.Hostnames {
			if n == "clock" {
				t.Error("clock is still a host alias")
			}
		}
	}
	for name, c := range cs {
		if _, ok := envOf(c)["LAB_CLOCK_URL"]; ok {
			t.Errorf("%s still follows a lab clock", name)
		}
		for _, port := range c.Ports {
			if port.HostPort == 8096 {
				t.Errorf("%s publishes the clock's port", name)
			}
		}
	}
	// The lab API is what reads the catalogue; the console is only a page
	// that forwards to it, and a setting on it would change nothing.
	if got := envOf(cs["lab-api"])["CONSOLE_URL_CLOCK"]; got != "off" {
		t.Errorf("lab-api CONSOLE_URL_CLOCK = %q, want off", got)
	}
	if _, ok := envOf(cs["console"])["CONSOLE_URL_CLOCK"]; ok {
		t.Error("CONSOLE_URL_CLOCK set on the console, which does not read it")
	}
}

// Every rendering is the local lab with only the named differences: the
// same containers (less the clock in the cloud), the same images, ports
// and volumes, and every variable the same unless an option changed it.
func TestARenderingChangesOnlyWhatItWasAskedTo(t *testing.T) {
	var local podFile
	if err := yaml.Unmarshal(source(t), &local); err != nil {
		t.Fatal(err)
	}
	o := Options{
		Profile:      ProfileCloud,
		PublicHost:   "lab.example.test",
		WebhookAllow: []string{"hooks.example.test"},
		Set:          []string{"aci.ACI_WEBHOOK_TARGET_URL=https://hooks.example.test/aci"},
	}
	cloud := containers(render(t, o))
	changed := map[string]bool{
		"init.CERT_EXTRA_SANS": true, "lab-api.CONSOLE_URL_CLOCK": true,
		"lab-api.CONSOLE_CONNECT_HOST": true, "lab-api.CONSOLE_BROWSE_HOST": true,
		"aci.ACI_WEBHOOK_TARGET_URL": true,
	}
	for name, lc := range containers(local) {
		cc, ok := cloud[name]
		if name == "clock" {
			continue
		}
		if !ok {
			t.Errorf("%s is missing from the rendering", name)
			continue
		}
		if cc.Image != lc.Image || len(cc.Ports) != len(lc.Ports) || len(cc.VolumeMounts) != len(lc.VolumeMounts) {
			t.Errorf("%s: image, ports or mounts changed", name)
		}
		le, ce := envOf(lc), envOf(cc)
		for k, v := range le {
			key := name + "." + k
			switch {
			case k == "LAB_CLOCK_URL", changed[key]:
			case k == "WEBHOOK_ALLOWLIST":
				if ce[k] != v+",hooks.example.test" {
					t.Errorf("%s = %q, want the local list plus the platform's host", key, ce[k])
				}
			case ce[k] != v:
				t.Errorf("%s = %q, local %q: changed though no option asked", key, ce[k], v)
			}
		}
		for k := range ce {
			if _, ok := le[k]; !ok && !changed[name+"."+k] {
				t.Errorf("%s.%s added though no option asked", name, k)
			}
		}
	}
	if len(cloud) != len(containers(local))-1 {
		t.Errorf("%d containers, want the local lab's less the clock", len(cloud))
	}
}

// The platform reaches the lab at one address. It goes on the certificates
// it dials, and the console hands it out -- through a tunnel or not.
func TestThePublicHostIsOnTheCertificatesAndInTheEnvDownloads(t *testing.T) {
	for _, tc := range []struct {
		host  string
		extra []string
		want  string
	}{
		{"203.0.113.7", nil, "IP:203.0.113.7"},
		{"lab.example.test", []string{"IP:203.0.113.7"}, "DNS:lab.example.test,IP:203.0.113.7"},
		{"lab.example.test", []string{"DNS:lab.example.test"}, "DNS:lab.example.test"},
	} {
		cs := containers(render(t, Options{PublicHost: tc.host, ExtraSANs: tc.extra}))
		if got := envOf(cs["init"])["CERT_EXTRA_SANS"]; got != tc.want {
			t.Errorf("%s: CERT_EXTRA_SANS = %q, want %q", tc.host, got, tc.want)
		}
		api := envOf(cs["lab-api"])
		if api["CONSOLE_CONNECT_HOST"] != tc.host || api["CONSOLE_BROWSE_HOST"] != tc.host {
			t.Errorf("%s: lab-api hosts %q / %q", tc.host, api["CONSOLE_CONNECT_HOST"], api["CONSOLE_BROWSE_HOST"])
		}
		if _, ok := envOf(cs["console"])["CONSOLE_CONNECT_HOST"]; ok {
			t.Errorf("%s: CONSOLE_CONNECT_HOST set on the console, which does not read it", tc.host)
		}
		// The local profile keeps its clock: a remote lab that is not in
		// the cloud profile is still a lab you can move.
		if _, ok := cs["clock"]; !ok {
			t.Errorf("the local profile lost its clock")
		}
	}
}

// The REGENERATE_KEYS flag is a ConfigMap reference, not a value; a
// rendering must leave it as one or every play without the file fails.
func TestTheInitContainerKeepsItsPlayTimeFlag(t *testing.T) {
	cs := containers(render(t, Options{Profile: ProfileCloud, PublicHost: "203.0.113.7"}))
	for _, e := range cs["init"].Env {
		if e.Name == "REGENERATE_KEYS" {
			if e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil || !e.ValueFrom.ConfigMapKeyRef.Optional {
				t.Fatal("REGENERATE_KEYS lost its optional ConfigMap reference")
			}
			return
		}
	}
	t.Fatal("REGENERATE_KEYS is gone from the init container")
}

// Every image moves, including the one with a digit in its name -- the
// Makefile's sed once matched [a-z-]* and left b4b on localhost.
func TestImagesMoveToThePrefixAndVersionGiven(t *testing.T) {
	for name, c := range containers(render(t, Options{ImagePrefix: "registry.example.test/lab/", Version: "dev-1"})) {
		want := "registry.example.test/lab/" + map[string]string{"init": "ca"}[name] + ":dev-1"
		if name != "init" {
			want = "registry.example.test/lab/" + name + ":dev-1"
		}
		if c.Image != want {
			t.Errorf("%s: image %q, want %q", name, c.Image, want)
		}
	}
}

func TestOptionsThatCannotBeRightAreRefused(t *testing.T) {
	for _, o := range []Options{
		{Profile: "staging"},
		{PublicHost: "lab.example.test:8090"},
		{PublicHost: "a b"},
		{ExtraSANs: []string{"lab.example.test"}},
		{Set: []string{"nosuch.KEY=1"}},
		{Set: []string{"console=1"}},
		{Set: []string{"console.KEY"}},
	} {
		if _, err := Render(source(t), o); err == nil {
			t.Errorf("%+v rendered without complaint", o)
		}
	}
}

// The settings follow the variables, not a container's name. A manifest in
// which nothing reads them any more is refused rather than rendered into a
// lab that ignores them -- the way they were once written onto the console
// after the lab API took its place.
func TestSettingsNobodyReadsAreRefused(t *testing.T) {
	src := strings.NewReplacer("CONSOLE_URL_CLOCK", "SOMETHING_ELSE", "CONSOLE_BROWSE_HOST", "SOMETHING_MORE").Replace(string(source(t)))
	if _, err := Render([]byte(src), Options{Profile: ProfileCloud}); err == nil {
		t.Error("cloud profile rendered with no reader of CONSOLE_URL_CLOCK")
	}
	if _, err := Render([]byte(src), Options{PublicHost: "203.0.113.7"}); err == nil {
		t.Error("public host rendered with no reader of CONSOLE_BROWSE_HOST")
	}
}

func TestTheRenderingSaysItIsGenerated(t *testing.T) {
	out, err := Render(source(t), Options{Profile: ProfileCloud})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# Rendered from deploy/pod/fintechlab.yaml") {
		t.Errorf("no generated header:\n%s", out[:200])
	}
	if strings.Contains(string(out), "The whole lab as one podman pod.") {
		t.Error("the source manifest's own header was carried into the rendering")
	}
}

func TestASeederJoinsThePodOnTheLabAPIsVolume(t *testing.T) {
	src, err := os.ReadFile("fintechlab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(src, Options{SeederImage: "localhost/lightyear/lab-seeder:dev"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"name: seeder", "image: localhost/lightyear/lab-seeder:dev", "- seeder", "LAB_SEEDER_URL", "http://seeder:8098"} {
		if !strings.Contains(s, want) {
			t.Errorf("the rendered pod has no %q", want)
		}
	}
	plain, _ := Render(src, Options{})
	if strings.Contains(string(plain), "seeder") {
		t.Fatal("a pod rendered without a seeder image has one")
	}
}

func TestAnHTTPSHostPutsTheConsoleBehindAFront(t *testing.T) {
	src, err := os.ReadFile("fintechlab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(src, Options{Profile: ProfileCloud, PublicHost: "lab.example.test", HTTPSHost: "lab.example.test", FrontImage: "docker.io/library/caddy:2.10.2"})
	if err != nil {
		t.Fatal(err)
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name    string   `yaml:"name"`
				Image   string   `yaml:"image"`
				Command []string `yaml:"command"`
				Ports   []struct {
					ContainerPort int `yaml:"containerPort"`
					HostPort      int `yaml:"hostPort"`
				} `yaml:"ports"`
				Env []struct {
					Name  string `yaml:"name"`
					Value string `yaml:"value"`
				} `yaml:"env"`
				VolumeMounts []struct {
					Name      string `yaml:"name"`
					MountPath string `yaml:"mountPath"`
					ReadOnly  bool   `yaml:"readOnly"`
				} `yaml:"volumeMounts"`
			} `yaml:"containers"`
			Volumes []struct {
				Name string `yaml:"name"`
				PVC  struct {
					ClaimName string `yaml:"claimName"`
				} `yaml:"persistentVolumeClaim"`
			} `yaml:"volumes"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &pod); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range pod.Spec.Containers {
		if c.Name != "front" {
			continue
		}
		found = true
		if c.Image != "docker.io/library/caddy:2.10.2" {
			t.Errorf("the front runs %q", c.Image)
		}
		var ports []int
		for _, p := range c.Ports {
			if p.ContainerPort != p.HostPort {
				t.Errorf("the front's %d is published on %d", p.ContainerPort, p.HostPort)
			}
			ports = append(ports, p.HostPort)
		}
		if fmt.Sprint(ports) != "[80 443]" {
			t.Errorf("the front listens on %v, want [80 443]", ports)
		}
		if len(c.Env) != 1 || c.Env[0].Name != "CADDYFILE" {
			t.Fatalf("the front's environment is %+v, want its Caddyfile alone", c.Env)
		}
		cf := c.Env[0].Value
		for _, want := range []string{
			"lab.example.test {\n\treverse_proxy 127.0.0.1:8090\n}", "admin off", "disable_tlsalpn_challenge",
			// every vendor port's container, by its own name, and the lab API by its
			"b4b.lab.example.test {\n\treverse_proxy 127.0.0.1:8086\n}",
			"verification.lab.example.test {\n\treverse_proxy 127.0.0.1:8089\n}",
			"lab-api.lab.example.test {\n\treverse_proxy 127.0.0.1:8097\n}",
			// Banking Circle: the caller's client certificate checked at the edge,
			// the lab's own presented to the sim, which speaks TLS
			"banking-circle.lab.example.test {\n\ttls {\n\t\tclient_auth {\n\t\t\tmode require_and_verify\n\t\t\ttrust_pool file /keys/certs/ca.pem",
			"reverse_proxy https://127.0.0.1:8085",
			"tls_server_name banking-circle",
			"tls_client_auth /keys/certs/client.pem /keys/certs/client-key.pem",
		} {
			if !strings.Contains(cf, want) {
				t.Errorf("the Caddyfile has no %q:\n%s", want, cf)
			}
		}
		if len(c.Command) != 3 || !strings.Contains(c.Command[2], `"$CADDYFILE"`) || !strings.Contains(c.Command[2], "exec caddy run") {
			t.Errorf("the front does not run its Caddyfile: %q", c.Command)
		}
		for _, never := range []string{"console.lab.example.test", "clock.lab", "receiver.lab", "\napi.lab", ":8095", ":8443"} {
			if strings.Contains(cf, never) {
				t.Errorf("the Caddyfile serves %q, which is not a vendor's own surface:\n%s", never, cf)
			}
		}
		if len(c.VolumeMounts) != 2 || c.VolumeMounts[0].Name != "front-data" || c.VolumeMounts[0].MountPath != "/data" ||
			c.VolumeMounts[1].Name != "keys" || c.VolumeMounts[1].MountPath != "/keys" || !c.VolumeMounts[1].ReadOnly {
			t.Errorf("the front mounts %+v, want front-data at /data and the keys read-only at /keys", c.VolumeMounts)
		}
	}
	if !found {
		t.Fatal("no front container in the rendered pod")
	}
	var claim string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "front-data" {
			claim = v.PVC.ClaimName
		}
	}
	if claim != "fintechlab-front-data" {
		t.Errorf("the front-data volume is claim %q, want fintechlab-front-data", claim)
	}

	plain, _ := Render(src, Options{Profile: ProfileCloud, FrontImage: "docker.io/library/caddy:2.10.2"})
	if strings.Contains(string(plain), "front") {
		t.Fatal("a pod rendered without an HTTPS host has a front")
	}
	for _, bad := range []Options{
		{HTTPSHost: "lab.example.test"},
		{HTTPSHost: "203.0.113.7", FrontImage: "caddy:2"},
		{HTTPSHost: "localhost", FrontImage: "caddy:2"},
		{HTTPSHost: "lab.example.test", FrontImage: "Caddy Latest"},
	} {
		if _, err := Render(src, bad); err == nil {
			t.Errorf("Render(%+v) = nil error, want a refusal", bad)
		}
	}
}

func TestTheFrontTellsTheLabAPIItsNamesAndFollowsBCMTLS(t *testing.T) {
	src, err := os.ReadFile("fintechlab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	names := func(out []byte) string {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "CONSOLE_CONNECT_NAMES") {
				return line
			}
		}
		return ""
	}
	o := Options{Profile: ProfileCloud, PublicHost: "lab.example.test", HTTPSHost: "lab.example.test", FrontImage: "caddy:2"}
	out, err := Render(src, o)
	if err != nil {
		t.Fatal(err)
	}
	var told string
	for _, c := range strings.Split(string(out), "- name: ") {
		if strings.HasPrefix(c, "CONSOLE_CONNECT_NAMES") {
			told = c
		}
	}
	if !strings.Contains(told, `"aci,b4b,bank,banking-circle,lab-api,notifier,payment-api,settlement,verification,verify,worldline"`) {
		t.Errorf("the lab API is told %q, want every name the front serves", told)
	}

	// Under another public host the names would point nowhere: not told.
	o.PublicHost = "203.0.113.7"
	out, _ = Render(src, o)
	if names(out) != "" {
		t.Errorf("a front under %s told the lab API its names for %s", o.HTTPSHost, o.PublicHost)
	}

	// The front enforces what Banking Circle would: BC_MTLS off asks no
	// certificate of the caller, optional checks one when given.
	for mode, want := range map[string]string{"off": "", "optional": "mode verify_if_given", "require": "mode require_and_verify"} {
		o := Options{Profile: ProfileCloud, HTTPSHost: "lab.example.test", FrontImage: "caddy:2", Set: []string{"banking-circle.BC_MTLS=" + mode}}
		out, err := Render(src, o)
		if err != nil {
			t.Fatal(mode, err)
		}
		hasAuth := strings.Contains(string(out), "client_auth {")
		if want == "" && hasAuth || want != "" && !strings.Contains(string(out), want) {
			t.Errorf("BC_MTLS=%s: the front's client_auth is wrong:\n%s", mode, out)
		}
	}
	if _, err := Render(src, Options{HTTPSHost: "lab.example.test", FrontImage: "caddy:2", Set: []string{"banking-circle.BC_MTLS=sometimes"}}); err == nil {
		t.Error("a BC_MTLS the sim would refuse was rendered")
	}
}
