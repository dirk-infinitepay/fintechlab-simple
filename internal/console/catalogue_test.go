package console

import (
	"strings"
	"testing"
)

func TestCatalogueCoversTheVendorsAndLabelsTheSides(t *testing.T) {
	c := DefaultCatalogue()
	// The vendor/scaffolding line is the one docs/catalogue.md draws, and
	// the console is where most people will meet it.
	for _, id := range []string{"worldline", "b4b", "banking-circle", "aci", "bank"} {
		s, ok := c.Get(id)
		if !ok {
			t.Fatalf("%s missing from the catalogue", id)
		}
		if s.Kind != KindVendor {
			t.Errorf("%s kind = %q, want vendor", id, s.Kind)
		}
		if s.SwapFor == "" {
			t.Errorf("%s does not say how to point it at the real thing", id)
		}
	}
	for _, id := range []string{"settlement", "receiver", "payment-api", "notifier"} {
		s, _ := c.Get(id)
		if s.Kind != KindPlatform {
			t.Errorf("%s kind = %q, want platform", id, s.Kind)
		}
		// A stand-in says what it plays, or nobody can decide whether
		// they need it.
		if s.StandIn == nil || s.StandIn.Plays == "" {
			t.Errorf("%s is not a stand-in that says what it plays", id)
		}
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("Get invented a service")
	}
}

func TestBaseURLIsOverridableAndBrowseURLStaysOnTheHost(t *testing.T) {
	t.Setenv("CONSOLE_URL_BANKING_CIRCLE", "https://banking-circle:8085")
	t.Setenv("CONSOLE_BROWSE_HOST", "127.0.0.1")
	s, _ := DefaultCatalogue().Get("banking-circle")
	if s.BaseURL != "https://banking-circle:8085" {
		t.Fatalf("base_url = %q, override ignored", s.BaseURL)
	}
	// Inside compose the console reaches peers by service name, which
	// means nothing in the operator's browser -- the link has to be the
	// published port instead.
	if s.Browse != "https://127.0.0.1:8085" {
		t.Fatalf("browse_url = %q, want the published host port", s.Browse)
	}
}

// A deployment that does not run a service says so with "off", and the
// service is gone -- not listed as down, which would read as broken.
func TestAServiceTheDeploymentDoesNotRunIsLeftOut(t *testing.T) {
	t.Setenv("CONSOLE_URL_CLOCK", ServiceOff)
	c := DefaultCatalogue()
	if _, ok := c.Get("clock"); ok {
		t.Fatal("CONSOLE_URL_CLOCK=off and the clock is still in the catalogue")
	}
	for _, id := range []string{"worldline", "banking-circle", "b4b", "bank"} {
		if _, ok := c.Get(id); !ok {
			t.Errorf("leaving the clock out also lost %s", id)
		}
	}
}

func TestBrowseURLIsEmptyForAServiceWithNoHTTPPort(t *testing.T) {
	if got := browseURL(Service{Ports: []string{"2222/ssh"}}); got != "" {
		t.Fatalf("browse_url = %q for an SSH-only service, want empty", got)
	}
}

func TestEnvKeyShape(t *testing.T) {
	if got := envKeyFor("banking-circle"); got != "CONSOLE_URL_BANKING_CIRCLE" {
		t.Fatalf("envKeyFor = %q", got)
	}
	for _, s := range DefaultCatalogue().Services {
		if strings.Contains(envKeyFor(s.ID), "-") {
			t.Errorf("%s yields an env key with a dash in it", s.ID)
		}
	}
}

// TestTheLabKnowsNoPlatformByName. The system under test registers itself
// (plugins.go); a catalogue entry for one particular platform is what this
// lab used to carry and must not again.
func TestTheLabKnowsNoPlatformByName(t *testing.T) {
	for _, s := range DefaultCatalogue().Services {
		if strings.Contains(s.ID, "runner") || s.Plugin != nil {
			t.Errorf("%s is a platform in the lab's own catalogue", s.ID)
		}
	}
	if len(DefaultCatalogue().Plugins()) != 0 {
		t.Error("a fresh catalogue has plugins")
	}
}

// TestEveryServiceHasAViewToLiveIn. The console renders one view per kind;
// a service whose kind matches none of them is in the catalogue, probed,
// counted in no badge and rendered on no page — invisible in exactly the
// way a catalogue exists to prevent.
func TestEveryServiceHasAViewToLiveIn(t *testing.T) {
	views := map[Kind]bool{KindVendor: true, KindPlatform: true, KindVerification: true}
	for _, s := range DefaultCatalogue().Services {
		if !views[s.Kind] {
			t.Errorf("%s has kind %q, which no view renders", s.ID, s.Kind)
		}
	}
}

// TestBothVerificationServicesAreListed: the four-vendor mock (Creditsafe,
// iban.com, KYC6, LexisNexis) was running on 8089 and missing from the
// catalogue entirely, so the console showed one verification card for two
// services and nobody could tell.
func TestBothVerificationServicesAreListed(t *testing.T) {
	cat := DefaultCatalogue()
	for _, id := range []string{"verify", "verification"} {
		s, ok := cat.Get(id)
		if !ok {
			t.Fatalf("%s is not in the catalogue", id)
		}
		if s.Kind != KindVerification {
			t.Errorf("%s has kind %q, want %q", id, s.Kind, KindVerification)
		}
	}
}

// TestStandInsStartHidden: with a platform plugged in they are noise, so
// their cards start hidden unless CONSOLE_STAND_INS_SHOWN says otherwise.
func TestStandInsStartHidden(t *testing.T) {
	if !DefaultCatalogue().Hidden("receiver") {
		t.Error("receiver starts shown by default")
	}
	t.Setenv("CONSOLE_STAND_INS_SHOWN", "true")
	if DefaultCatalogue().Hidden("receiver") {
		t.Error("CONSOLE_STAND_INS_SHOWN=true did not show it")
	}
}

func TestTheControlAddressIsPlainHTTPOnThePortPlus1000(t *testing.T) {
	for base, want := range map[string]string{
		"https://banking-circle:8085": "http://banking-circle:9085",
		"http://127.0.0.1:8084":       "http://127.0.0.1:9084",
		"https://receiver:8443":       "http://receiver:9443",
		"http://no-port":              "",
	} {
		if got := ControlURL(base); got != want {
			t.Errorf("ControlURL(%q) = %q, want %q", base, got, want)
		}
	}
	t.Setenv("CONSOLE_URL_BANK", "http://bank:8081")
	t.Setenv("CONSOLE_CONTROL_URL_WORLDLINE", "http://elsewhere:7000")
	for _, s := range DefaultCatalogue().Services {
		switch s.ID {
		case "bank":
			if s.Control != "http://bank:9081" {
				t.Errorf("bank control = %q", s.Control)
			}
		case "worldline":
			if s.Control != "http://elsewhere:7000" {
				t.Errorf("worldline control = %q: the override should win", s.Control)
			}
		}
	}
}
