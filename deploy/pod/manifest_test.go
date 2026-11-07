package pod

// The manifest repeats compose.yml's environment by hand, and a copy that
// quietly drifts is how the pod ends up running a different lab from the
// one everybody tested. These tests hold the two together; render_test.go
// holds every rendering of the manifest to it in turn.

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Ports       []string          `yaml:"ports"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

type podFile struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		HostAliases []struct {
			IP        string   `yaml:"ip"`
			Hostnames []string `yaml:"hostnames"`
		} `yaml:"hostAliases"`
		InitContainers []podContainer `yaml:"initContainers"`
		Containers     []podContainer `yaml:"containers"`
		Volumes        []struct {
			Name string `yaml:"name"`
		} `yaml:"volumes"`
	} `yaml:"spec"`
}

type podContainer struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	Ports []struct {
		ContainerPort int `yaml:"containerPort"`
		HostPort      int `yaml:"hostPort"`
	} `yaml:"ports"`
	Env []struct {
		Name      string `yaml:"name"`
		Value     string `yaml:"value"`
		ValueFrom *struct {
			ConfigMapKeyRef *struct {
				Name     string `yaml:"name"`
				Key      string `yaml:"key"`
				Optional bool   `yaml:"optional"`
			} `yaml:"configMapKeyRef"`
		} `yaml:"valueFrom"`
	} `yaml:"env"`
	VolumeMounts []struct {
		Name string `yaml:"name"`
	} `yaml:"volumeMounts"`
}

func load(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// ca is compose's one-shot certificate job; in the pod it is the init
// container, with its own command, so it has no container to compare.
const initService = "ca"

func TestEveryComposeServiceIsAContainerWithTheSameEnvironment(t *testing.T) {
	var c composeFile
	var p podFile
	load(t, "../../compose.yml", &c)
	load(t, "fintechlab.yaml", &p)

	byName := map[string]podContainer{}
	for _, ct := range p.Spec.Containers {
		byName[ct.Name] = ct
	}
	for name, svc := range c.Services {
		if name == initService {
			continue
		}
		ct, ok := byName[name]
		if !ok {
			t.Errorf("compose service %q has no container in the pod", name)
			continue
		}
		if want := "localhost/fintechlab/" + name + ":v1"; ct.Image != want {
			// make pod-images tags exactly this, and make's sed rewrites
			// exactly this shape for another registry or version.
			t.Errorf("%s: image %q, want %q", name, ct.Image, want)
		}
		got := map[string]string{}
		for _, e := range ct.Env {
			got[e.Name] = e.Value
		}
		for k, v := range svc.Environment {
			if g, ok := got[k]; !ok {
				t.Errorf("%s: %s=%q is in compose.yml but not in the pod", name, k, v)
			} else if g != v {
				t.Errorf("%s: %s is %q in the pod, %q in compose.yml", name, k, g, v)
			}
		}
		for k := range got {
			if _, ok := svc.Environment[k]; !ok {
				t.Errorf("%s: %s is set in the pod but not in compose.yml", name, k)
			}
		}
		want := append([]string(nil), svc.Ports...)
		var have []string
		for _, p := range ct.Ports {
			have = append(have, strconv.Itoa(p.HostPort)+":"+strconv.Itoa(p.ContainerPort))
		}
		sort.Strings(want)
		sort.Strings(have)
		if strings.Join(want, " ") != strings.Join(have, " ") {
			t.Errorf("%s: ports %v in the pod, %v in compose.yml", name, have, want)
		}
	}
	for name := range byName {
		if _, ok := c.Services[name]; !ok {
			t.Errorf("pod container %q is not a compose service", name)
		}
	}
}

// The pod's containers share one network namespace. The compose names only
// resolve because hostAliases maps every one of them to loopback -- miss one
// and a URL that works under compose fails in the pod with a DNS error.
func TestEveryServiceNameResolvesInsideThePod(t *testing.T) {
	var c composeFile
	var p podFile
	load(t, "../../compose.yml", &c)
	load(t, "fintechlab.yaml", &p)

	aliased := map[string]bool{}
	for _, h := range p.Spec.HostAliases {
		if h.IP != "127.0.0.1" {
			t.Errorf("host alias on %s: every service is on the pod's loopback", h.IP)
		}
		for _, n := range h.Hostnames {
			aliased[n] = true
		}
	}
	for name := range c.Services {
		if name != initService && !aliased[name] {
			t.Errorf("service %q is not in hostAliases", name)
		}
	}
}

func TestEveryMountedVolumeIsDeclared(t *testing.T) {
	var p podFile
	load(t, "fintechlab.yaml", &p)
	declared := map[string]bool{}
	for _, v := range p.Spec.Volumes {
		declared[v.Name] = true
	}
	for _, ct := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		for _, m := range ct.VolumeMounts {
			if !declared[m.Name] {
				t.Errorf("%s mounts undeclared volume %q", ct.Name, m.Name)
			}
		}
	}
	if len(p.Spec.InitContainers) != 1 || p.Spec.InitContainers[0].Image != "localhost/fintechlab/ca:v1" {
		t.Errorf("want one init container from the ca image, got %+v", p.Spec.InitContainers)
	}
}

// REGENERATE_KEYS is a play-time flag: it must name the ConfigMap that
// regenerate-keys.yaml defines, or passing the file does nothing, and it
// must be optional, or every play without the file fails.
func TestRegenerateKeysIsAnOptionalFlagFromItsOwnFile(t *testing.T) {
	var p podFile
	load(t, "fintechlab.yaml", &p)
	var cm struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	load(t, "regenerate-keys.yaml", &cm)

	for _, e := range p.Spec.InitContainers[0].Env {
		if e.Name != "REGENERATE_KEYS" {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil {
			t.Fatal("REGENERATE_KEYS must come from a ConfigMap, not a fixed value")
		}
		ref := e.ValueFrom.ConfigMapKeyRef
		if !ref.Optional {
			t.Error("REGENERATE_KEYS must be optional: a play without the file keeps the keys")
		}
		if cm.Kind != "ConfigMap" || ref.Name != cm.Metadata.Name {
			t.Errorf("init reads ConfigMap %q, regenerate-keys.yaml defines %s %q", ref.Name, cm.Kind, cm.Metadata.Name)
		}
		if cm.Data[ref.Key] != "true" {
			t.Errorf("regenerate-keys.yaml sets %s=%q, want \"true\"", ref.Key, cm.Data[ref.Key])
		}
		return
	}
	t.Error("the init container has no REGENERATE_KEYS")
}
