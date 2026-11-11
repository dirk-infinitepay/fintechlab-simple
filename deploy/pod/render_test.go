package pod

import (
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
