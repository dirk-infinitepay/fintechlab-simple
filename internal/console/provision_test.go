package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func nowDate() string { return time.Now().UTC().Format("2006-01-02") }

func TestProvisionRegistersEveryOutletAndRecordsTheResult(t *testing.T) {
	var seen []map[string]string
	b4bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// B4B's control port: no JWT, the vendor's login is the platform's.
		if r.URL.Path != "/sim/beneficiaries" || r.Header.Get("Authorization") != "" {
			http.Error(w, "want POST /sim/beneficiaries with no credentials", 400)
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "ben_" + req["external_ref"], "sanctions_status": "pass",
		})
	}))
	defer b4bSrv.Close()

	reg, _ := NewRegistry("")
	m, _ := reg.AddMerchant(NewMerchantParams{LegalName: "Provisioned Ltd", Outlets: 2})
	p := &Provisioner{Client: b4bSrv.Client(), Registry: reg, B4BURL: b4bSrv.URL}

	res, err := p.ProvisionMerchant(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want one per outlet", len(res))
	}
	for _, r := range res {
		if r.Error != "" || r.BeneficiaryID == "" {
			t.Fatalf("outlet %s: %+v", r.OutletID, r)
		}
	}
	// The MID is the external reference, because that is the key a payout
	// later has to find the beneficiary by.
	for i, req := range seen {
		if req["external_ref"] != m.Outlets[i].MID {
			t.Fatalf("external_ref = %q, want the MID %q", req["external_ref"], m.Outlets[i].MID)
		}
		if req["account_number"] != m.Outlets[i].AccountNumber {
			t.Fatal("the registered account is not the one the outlet is paid into")
		}
	}
	got, _ := reg.Merchant(m.ID)
	if got.Outlets[0].BeneficiaryID == "" || got.Outlets[0].SanctionsStatus != "pass" {
		t.Fatalf("the registry did not record what was provisioned: %+v", got.Outlets[0])
	}
}

func TestProvisionReportsAPerOutletFailureWithoutAbandoningTheRest(t *testing.T) {
	n := 0
	b4bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, `{"error":"account_name is required"}`, 400)
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ben_ok", "sanctions_status": "pass"})
	}))
	defer b4bSrv.Close()

	reg, _ := NewRegistry("")
	m, _ := reg.AddMerchant(NewMerchantParams{LegalName: "Partly Ltd", Outlets: 3})
	p := &Provisioner{Client: b4bSrv.Client(), Registry: reg, B4BURL: b4bSrv.URL}
	res, err := p.ProvisionMerchant(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A partial run is a normal outcome; the caller has to see which
	// outlet failed rather than a single yes/no for the merchant.
	if res[0].Error == "" || !strings.Contains(res[0].Error, "account_name is required") {
		t.Fatalf("first outlet error = %q", res[0].Error)
	}
	if res[1].BeneficiaryID == "" || res[2].BeneficiaryID == "" {
		t.Fatal("one failure stopped the remaining outlets")
	}
}

// Provisioning needs no key of B4B's: only its control port, and when that
// does not answer, each outlet says so rather than the merchant failing.
func TestProvisionWithB4BUnreachableSaysSoPerOutlet(t *testing.T) {
	reg, _ := NewRegistry("")
	m, _ := reg.AddMerchant(NewMerchantParams{LegalName: "Offline Ltd", Outlets: 2})
	p := &Provisioner{Client: http.DefaultClient, Registry: reg, B4BURL: "http://127.0.0.1:1"}
	if !p.Ready() {
		t.Fatal("not ready with a control address and nothing else")
	}
	res, err := p.ProvisionMerchant(context.Background(), m.ID)
	if err != nil || len(res) != 2 || res[0].Error == "" || res[1].Error == "" {
		t.Fatalf("results = %+v, %v", res, err)
	}
}

func TestSeedTradingPostsToTheAcquirerAndDefaultsToYesterday(t *testing.T) {
	var batch []map[string]any
	wl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&batch)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(batch)})
	}))
	defer wl.Close()

	reg, _ := NewRegistry("")
	m, _ := reg.AddMerchant(NewMerchantParams{LegalName: "Trading Ltd", Outlets: 2, Country: "GB"})
	p := &Provisioner{Client: wl.Client(), Registry: reg, WorldlineURL: wl.URL}

	n, date, err := p.SeedTrading(context.Background(), m.ID, TradeParams{Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("accepted = %d, want count x outlets", n)
	}
	// Worldline settles T+1, so seeding "today" then running a cycle
	// produces an empty file -- the default has to be yesterday.
	if date == "" || date >= nowDate() {
		t.Fatalf("date = %q, want yesterday", date)
	}
	if batch[0]["mid"] != m.Outlets[0].MID {
		t.Fatalf("transactions were not attributed to the outlet's MID: %+v", batch[0])
	}
	if batch[0]["currency"] != "GBP" {
		t.Fatalf("currency = %v, want the merchant's own", batch[0]["currency"])
	}
}

func TestSeedTradingRefusesUnreasonableCounts(t *testing.T) {
	reg, _ := NewRegistry("")
	m, _ := reg.AddMerchant(NewMerchantParams{LegalName: "Bounded Ltd"})
	p := &Provisioner{Client: http.DefaultClient, Registry: reg, WorldlineURL: "http://127.0.0.1:1"}
	if _, _, err := p.SeedTrading(context.Background(), m.ID, TradeParams{Count: 5000}); err == nil {
		t.Fatal("5000 transactions per outlet was accepted")
	}
}
