package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Lab is the lab API (cmd/labapi, docs/api/lab-api.md): how the e2e suite
// steers and reads the lab — seeding transactions, running a cycle,
// flipping sanctions, funding, traps, reset, the lab's own records. It is
// the API the console page and agents use, with the same token.
//
// What the suite does as the platform — SFTP at Worldline, B4B's Oversight
// API, Banking Circle's Connect API — it does on the vendor surfaces with
// each vendor's login, as a platform would. It never calls a control port
// or /internal/*: a lab in the cloud exposes neither, and the lab API is
// the one way in to what is not a vendor's.
type Lab struct {
	URL    string
	Token  string
	Client *http.Client
}

func newLab(base, token, caFile string) Lab {
	// The system roots, for a lab behind a public certificate, and the
	// lab's own CA, for one behind the lab's.
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if pem, err := os.ReadFile(caFile); err == nil {
		roots.AppendCertsFromPEM(pem)
	}
	return Lab{
		URL:   strings.TrimRight(base, "/"),
		Token: token,
		Client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		}},
	}
}

// Sim is the lab API path to a service's /sim/{rest} lab hook, query
// included: GET /v1/services/{id}/sim/{rest} and the other methods.
func Sim(service, rest string) string {
	return "/v1/services/" + url.PathEscape(service) + "/sim/" + rest
}

func (l Lab) headers() map[string]string {
	if l.Token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + l.Token}
}

// Get, Post and Put call path on the lab API and decode its answer into
// out (nil to discard), returning the status as given.
func (l Lab) Get(ctx context.Context, path string, out any) (int, error) {
	return doJSON(ctx, l.Client, http.MethodGet, l.URL+path, l.headers(), nil, out)
}

func (l Lab) Post(ctx context.Context, path string, body, out any) (int, error) {
	return doJSON(ctx, l.Client, http.MethodPost, l.URL+path, l.headers(), body, out)
}

func (l Lab) Put(ctx context.Context, path string, body, out any) (int, error) {
	return doJSON(ctx, l.Client, http.MethodPut, l.URL+path, l.headers(), body, out)
}

// Check asks the lab API for its index, so a run against a lab API that is
// down, or that wants another token, says so once instead of failing every
// scenario its own way.
func (l Lab) Check(ctx context.Context) error {
	status, err := l.Get(ctx, "/v1", nil)
	switch {
	case err != nil:
		return fmt.Errorf("the lab API at %s does not answer (LAB_API_URL): %w", l.URL, err)
	case status == http.StatusUnauthorized && l.Token == "":
		return fmt.Errorf("the lab API at %s wants a token: set LAB_API_TOKEN", l.URL)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("the lab API at %s refused LAB_API_TOKEN", l.URL)
	case status != http.StatusOK:
		return fmt.Errorf("the lab API at %s answered GET /v1 with %d", l.URL, status)
	}
	return nil
}
