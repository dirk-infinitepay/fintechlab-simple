package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withKeys(t *testing.T, a *app) {
	t.Helper()
	a.keysDir = t.TempDir()
	dir := filepath.Join(a.keysDir, "wlsftp-keys")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"worldline_public.asc":  "PUBLIC",
		"worldline_private.asc": "SECRET-PGP-BYTES",
		"host_key":              "SERVER KEY",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, a *app, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = host
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	return w
}

func TestTheEnvFileIsADownloadAddressedToTheHostTheConsoleWasOpenedOn(t *testing.T) {
	a := testApp(t, nil)
	withKeys(t, a)

	w := get(t, a, "lab.example.test:8090", "/api/connect/env/worldline")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="fintechlab-worldline.env"` {
		t.Errorf("Content-Disposition %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("key material with Cache-Control %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "\nWORLDLINE_SFTP_HOST=lab.example.test\n") {
		t.Errorf("host not taken from the request:\n%s", body)
	}
	if !strings.Contains(body, "INFINITE_PGP_PRIVATE_KEY=SECRET-PGP-BYTES\n") {
		t.Errorf("key not inlined from the keys directory:\n%s", body)
	}

	w = get(t, a, "lab.example.test:8090", "/api/connect/env?host=10.0.0.5")
	if !strings.Contains(w.Body.String(), "\nWORLDLINE_SFTP_HOST=10.0.0.5\n") {
		t.Errorf("?host= did not win over the request's host")
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, `"fintechlab.env"`) {
		t.Errorf("the every-vendor file is named %q", got)
	}
}

// A lab deployed somewhere knows the one address a platform reaches it at,
// and the operator may be looking through a tunnel on 127.0.0.1. The
// deployment's host wins over the request's; ?host= still wins over both.
func TestADeployedLabHandsOutItsOwnAddressNotTheTunnels(t *testing.T) {
	t.Setenv("CONSOLE_CONNECT_HOST", "lab.example.test")
	a := testApp(t, nil)
	withKeys(t, a)

	w := get(t, a, "127.0.0.1:8090", "/api/connect/env/worldline")
	if !strings.Contains(w.Body.String(), "\nWORLDLINE_SFTP_HOST=lab.example.test\n") {
		t.Errorf("CONSOLE_CONNECT_HOST not used through a tunnel:\n%s", w.Body)
	}
	w = get(t, a, "127.0.0.1:8090", "/api/connect/env/worldline?host=10.0.0.5")
	if !strings.Contains(w.Body.String(), "\nWORLDLINE_SFTP_HOST=10.0.0.5\n") {
		t.Errorf("?host= did not win over CONSOLE_CONNECT_HOST")
	}
}

func TestAHostThatCouldInjectIntoTheFileIsRefused(t *testing.T) {
	a := testApp(t, nil)
	withKeys(t, a)
	for _, h := range []string{"a b", "x\nEVIL=1", "a/b", `"q"`} {
		w := get(t, a, "127.0.0.1:8090", "/api/connect/env?host="+strings.NewReplacer(" ", "%20", "\n", "%0A", "/", "%2F", `"`, "%22").Replace(h))
		if w.Code != 400 {
			t.Errorf("host %q: status %d", h, w.Code)
		}
	}
}

func TestOnlyAKitsOwnFilesCanBeDownloaded(t *testing.T) {
	a := testApp(t, nil)
	withKeys(t, a)

	w := get(t, a, "127.0.0.1:8090", "/api/connect/files/worldline/worldline-pgp-private.asc")
	if w.Code != 200 || w.Body.String() != "SECRET-PGP-BYTES" {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	for _, path := range []string{
		"/api/connect/files/worldline/host_key",
		"/api/connect/files/worldline/..%2Fworldline%2Fhost_key",
		"/api/connect/files/bank/anything",
	} {
		if w := get(t, a, "127.0.0.1:8090", path); w.Code != 404 {
			t.Errorf("%s: status %d, want 404", path, w.Code)
		}
	}
	// In the kit, but not generated yet: a different answer from "no such
	// file", because it sends you to a different place.
	if w := get(t, a, "127.0.0.1:8090", "/api/connect/files/worldline/worldline-sftp-host-key.pub"); w.Code != 503 {
		t.Errorf("missing-but-expected file: status %d, want 503", w.Code)
	}
}

func TestTheListingNamesVariablesButCarriesNoKeyMaterial(t *testing.T) {
	a := testApp(t, nil)
	withKeys(t, a)
	w := get(t, a, "127.0.0.1:8090", "/api/connect")
	if strings.Contains(w.Body.String(), "SECRET-PGP-BYTES") {
		t.Fatalf("listing carries a key: %s", w.Body)
	}
	var out struct {
		Host string `json:"host"`
		Kits []struct {
			Service string `json:"service"`
			Vars    []struct {
				Key string `json:"key"`
			} `json:"vars"`
			Files []struct {
				Name    string `json:"name"`
				Present bool   `json:"present"`
			} `json:"files"`
		} `json:"kits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// testApp's catalogue has worldline and no other vendor with a kit.
	if out.Host != "127.0.0.1" || len(out.Kits) != 1 || out.Kits[0].Service != "worldline" {
		t.Fatalf("got %+v", out)
	}
	if len(out.Kits[0].Vars) == 0 || !out.Kits[0].Files[0].Present {
		t.Errorf("got %+v", out.Kits[0])
	}
}
