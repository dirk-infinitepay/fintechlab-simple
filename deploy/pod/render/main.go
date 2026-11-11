// Render writes the lab's pod manifest as one deployment runs it: the
// checked-in deploy/pod/fintechlab.yaml with a profile and a few values
// applied (deploy/pod/render.go says which, and why there is no second
// manifest). The result goes to stdout.
//
//	go run ./deploy/pod/render -profile cloud -public-host 203.0.113.7 \
//	    -allow hooks.example.test -set aci.ACI_WEBHOOK_TARGET_URL=https://hooks.example.test/aci
//
// make cloud-manifest and make cloud-bundle call it; docs/deploy-cloud.md
// is the walkthrough.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/webduvet/fintechlab-simple/deploy/pod"
)

type list []string

func (l *list) String() string { return strings.Join(*l, ",") }
func (l *list) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// csv splits the comma-separated flags, so one shell variable can carry
// several entries.
func csv(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func main() {
	var o pod.Options
	var set list
	in := flag.String("in", "deploy/pod/fintechlab.yaml", "the manifest to render")
	flag.StringVar(&o.Profile, "profile", pod.ProfileLocal, `"local" (with the lab clock) or "cloud" (without it)`)
	flag.StringVar(&o.PublicHost, "public-host", "", "the name or IPv4 address a platform reaches the lab at")
	sans := flag.String("san", "", "more certificate names, comma-separated: DNS:<name>,IP:<address>")
	allow := flag.String("allow", "", "hosts or CIDRs added to every webhook allowlist, comma-separated")
	flag.Var(&set, "set", "container.KEY=value, repeatable")
	flag.StringVar(&o.ImagePrefix, "image-prefix", "", "registry/path the images are under (default localhost/fintechlab)")
	flag.StringVar(&o.Version, "version", "", "image tag (default v1)")
	flag.StringVar(&o.SeederImage, "seeder-image", "", "the platform's seeder image, added to the pod as its seeder (empty: none)")
	flag.Parse()

	o.ExtraSANs, o.WebhookAllow, o.Set = csv(*sans), csv(*allow), set

	src, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
	out, err := pod.Render(src, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(out)
}
