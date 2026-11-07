package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestAFileGoesToThePlatformUntouched. The console passes an upload on
// to the path the plugin declared — name in the query, bytes as they came —
// and stores nothing itself.
func TestAFileGoesToThePlatformUntouched(t *testing.T) {
	var mu sync.Mutex
	var gotName, gotBody, gotLines, gotDelete string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /sim/files/upload", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotName, gotBody = r.URL.Query().Get("name"), string(b)
		mu.Unlock()
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"name":"mine.csv","bytes":11}`))
	})
	mux.HandleFunc("DELETE /sim/files/upload", func(w http.ResponseWriter, r *http.Request) {
		gotDelete = r.URL.Query().Get("name")
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /sim/files/preview", func(w http.ResponseWriter, r *http.Request) {
		gotLines = r.URL.Query().Get("lines")
		_, _ = w.Write([]byte(`{"name":"mine.csv","lines":["a,b"],"truncated":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := testApp(t, srv)
	p := testPlugin(srv.URL)
	p.Settlement.UploadPath = "/sim/files/upload"
	p.Settlement.PreviewPath = "/sim/files/preview"
	register(t, a, p)

	w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/files?name=my%20file.csv", "hello,world")
	if w.Code != 201 || gotName != "my file.csv" || gotBody != "hello,world" {
		t.Fatalf("upload = %d %s; platform got name %q body %q", w.Code, w.Body.String(), gotName, gotBody)
	}
	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/files", "x"); w.Code != 400 {
		t.Errorf("an upload without a name = %d, want 400", w.Code)
	}

	// A preview is bounded however many lines are asked for.
	if w := call(t, a, http.MethodGet, "/v1/plugins/test-platform/files/preview?name=mine.csv&lines=1000000", ""); w.Code != 200 || gotLines != "1000" {
		t.Errorf("preview = %d, platform asked for %q lines, want 1000", w.Code, gotLines)
	}
	if w := call(t, a, http.MethodDelete, "/v1/plugins/test-platform/files?name=mine.csv", ""); w.Code != 204 || gotDelete != "mine.csv" {
		t.Errorf("delete = %d, platform got %q", w.Code, gotDelete)
	}
}

// TestNoUploadPathIsNoUploadButton: a plugin that declares no upload_path
// answers 501, which the page reads as "not offered", not "failed".
func TestNoUploadPathIsNoUploadButton(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := testApp(t, srv)
	register(t, a, testPlugin(srv.URL))
	w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/files?name=a.csv", "x")
	if w.Code != 501 || !strings.Contains(w.Body.String(), "uploads") {
		t.Errorf("upload without upload_path = %d %s, want 501", w.Code, w.Body.String())
	}
}

// TestDeleteGoesToDeletePathWhenDeclared: a platform that can delete any
// file it lists declares delete_path, and a delete goes there rather than
// to upload_path — which only knows the files it was handed.
func TestDeleteGoesToDeletePathWhenDeclared(t *testing.T) {
	var mu sync.Mutex
	var hit string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, path := range []string{"/sim/files/upload", "/sim/files/delete"} {
		mux.HandleFunc("DELETE "+path, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hit = path + "?" + r.URL.RawQuery
			mu.Unlock()
			w.WriteHeader(204)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := testApp(t, srv)
	p := testPlugin(srv.URL)
	p.Settlement.UploadPath = "/sim/files/upload"
	p.Settlement.DeletePath = "/sim/files/delete"
	register(t, a, p)
	if w := call(t, a, http.MethodDelete, "/v1/plugins/test-platform/files?name=gen.csv", ""); w.Code != 204 || hit != "/sim/files/delete?name=gen.csv" {
		t.Fatalf("delete = %d, platform got %q", w.Code, hit)
	}
}

// TestRevealIsForwardedAndOptional: "open location" is the platform's to
// do, on its own machine. The console forwards the body to reveal_path and
// answers with the platform's words; with no reveal_path it is a 501, so
// the page offers no button rather than one that fails.
func TestRevealIsForwardedAndOptional(t *testing.T) {
	var mu sync.Mutex
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /sim/files/reveal", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"path":"/home/me/files","note":"Opened /home/me/files"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := testApp(t, srv)
	register(t, a, testPlugin(srv.URL))
	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/files/reveal", `{"name":"a.csv"}`); w.Code != 501 {
		t.Errorf("reveal without reveal_path = %d %s, want 501", w.Code, w.Body.String())
	}

	p := testPlugin(srv.URL)
	p.Settlement.RevealPath = "/sim/files/reveal"
	register(t, a, p)
	w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/files/reveal", `{"name":"a.csv"}`)
	if w.Code != 200 || got != `{"name":"a.csv"}` || !strings.Contains(w.Body.String(), "Opened /home/me/files") {
		t.Fatalf("reveal = %d %s; platform got %q", w.Code, w.Body.String(), got)
	}
}

// TestABatchOfMerchantsFromCreateToDelete: one call makes a population,
// one call seeds its card payments at the acquirer — each merchant at its
// own ticket — and one call takes it away again.
func TestABatchOfMerchantsFromCreateToDelete(t *testing.T) {
	var mu sync.Mutex
	amounts := map[string]bool{}
	posted := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sim/transactions", func(w http.ResponseWriter, r *http.Request) {
		var txns []struct {
			AmountCents int64 `json:"amount_cents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&txns)
		mu.Lock()
		posted++
		for _, t := range txns {
			amounts[strconv.FormatInt(t.AmountCents, 10)] = true
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"accepted":` + strconv.Itoa(len(txns)) + `}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := testApp(t, srv)

	w := call(t, a, http.MethodPost, "/v1/merchants/batches", `{"count":12,"max_outlets":2,"seed":7}`)
	if w.Code != 201 {
		t.Fatalf("create batch = %d %s", w.Code, w.Body.String())
	}
	var made struct {
		Batch struct {
			ID   string `json:"id"`
			Seed int64  `json:"seed"`
		} `json:"batch"`
		Merchants int `json:"merchants"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &made)
	if made.Merchants != 12 || made.Batch.Seed != 7 {
		t.Fatalf("made %+v", made)
	}
	if w := call(t, a, http.MethodGet, "/v1/merchants", ""); !strings.Contains(w.Body.String(), `"batches":[{"id":"`+made.Batch.ID) {
		t.Errorf("the batch is not listed: %s", w.Body.String())
	}

	w = call(t, a, http.MethodPost, "/v1/merchants/batches/"+made.Batch.ID+"/trading", `{"count":2}`)
	if w.Code != 201 || posted != 12 {
		t.Fatalf("trading = %d %s; worldline was called %d times", w.Code, w.Body.String(), posted)
	}
	if len(amounts) < 3 {
		t.Errorf("every merchant traded at the same ticket: %v", amounts)
	}

	if w := call(t, a, http.MethodDelete, "/v1/merchants/batches/"+made.Batch.ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"merchants":12`) {
		t.Fatalf("delete batch = %d %s", w.Code, w.Body.String())
	}
	if w := call(t, a, http.MethodPost, "/v1/merchants/batches", `{"count":0}`); w.Code != 400 {
		t.Errorf("an empty batch = %d, want 400", w.Code)
	}
}
