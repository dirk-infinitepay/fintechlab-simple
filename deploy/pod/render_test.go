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
	if got := envOf(cs["console"])["CONSOLE_URL_CLOCK"]; got != "off" {
		t.Errorf("console CONSOLE_URL_CLOCK = %q, want off", got)
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
		"init.CERT_EXTRA_SANS": true, "console.CONSOLE_URL_CLOCK": true,
		"console.CONSOLE_CONNECT_HOST": true, "console.CONSOLE_BROWSE_HOST": true,
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
		console := envOf(cs["console"])
		if console["CONSOLE_CONNECT_HOST"] != tc.host || console["CONSOLE_BROWSE_HOST"] != tc.host {
			t.Errorf("%s: console hosts %q / %q", tc.host, console["CONSOLE_CONNECT_HOST"], console["CONSOLE_BROWSE_HOST"])
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
