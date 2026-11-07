package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// NewTLSClient builds an http.Client that trusts caFile in addition to the
// system roots, and -- when both certFile and keyFile are non-empty --
// presents a client certificate for peers that require mTLS (Banking
// Circle's real-shaped API; see docs/ARCHITECTURE-vendor-corrections.md
// section 3). A peer that doesn't require a client cert (receiver) simply
// never asks for one, so this one client works for every https peer in
// this lab. An unreadable/empty caFile or cert pair is not fatal here —
// the same graceful-degradation the notifier and banking-circle services
// use — but any scenario that then fails to verify a peer's TLS cert, or
// that a server rejects for missing mTLS, will report a clear connection
// error, not a silent pass.
func NewTLSClient(caFile, certFile, keyFile string) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	if certFile != "" && keyFile != "" {
		if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}, nil
}

// PostJSON marshals body (nil for no body), POSTs it with any extra headers,
// and decodes the response into out (nil to discard). Returns the HTTP
// status code even on a non-2xx response so callers can assert on it.
func PostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body, out any) (int, error) {
	return doJSON(ctx, client, http.MethodPost, url, headers, body, out)
}

// PutJSON is PostJSON with the PUT method.
func PutJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body, out any) (int, error) {
	return doJSON(ctx, client, http.MethodPut, url, headers, body, out)
}

// GetJSON GETs url and decodes the response into out.
func GetJSON(ctx context.Context, client *http.Client, url string, out any) (int, error) {
	return doJSON(ctx, client, http.MethodGet, url, nil, nil, out)
}

// GetJSONWithHeaders is GetJSON with extra request headers — for a peer
// that requires an Authorization header on GET (Banking Circle's bearer-
// gated API, once authorized via its Basic->Bearer exchange).
func GetJSONWithHeaders(ctx context.Context, client *http.Client, url string, headers map[string]string, out any) (int, error) {
	return doJSON(ctx, client, http.MethodGet, url, headers, nil, out)
}

// GetBytes GETs url and returns the raw response body — for an endpoint
// that serves a file, not JSON (e.g. a staged settlement report).
func GetBytes(ctx context.Context, client *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("e2e: read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("e2e: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, fmt.Errorf("e2e: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("e2e: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("e2e: read response: %w", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("e2e: %s %s: decode response %q: %w", method, url, truncate(raw, 200), err)
		}
	}
	return resp.StatusCode, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// PollUntil calls check every interval until it returns true, returns nil,
// or timeout elapses. check's error is only surfaced if the timeout is
// reached without a true result — a transient error mid-poll (the receiver
// hasn't stored the event yet) is expected, not a failure.
func PollUntil(ctx context.Context, timeout, interval time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := check()
		if ok {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("e2e: timed out after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("e2e: timed out after %s waiting for condition", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
