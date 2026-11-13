// Iban.com simulates the iban.com account and name check the platform calls
// for non-UK bank accounts. Every account is valid with a matching name
// until the bank-account outcome is flipped (docs/api/services/iban-com.md).
package main

import (
	"encoding/json"
	"github.com/webduvet/fintechlab-simple/internal/activity"
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const checkBankAccount = "bank-account"

type app struct{ sim *vendorsim.Sim }

func newApp(addr string) (*app, error) {
	a := &app{}
	sim, err := vendorsim.New(vendorsim.Config{
		Service: "iban-com", Simulates: "iban.com",
		Checks:    []vendorsim.Check{{Name: checkBankAccount, Outcomes: []string{"pass", "mismatch", "invalid"}}},
		Summarize: refusal,
	}, addr)
	if err != nil {
		return nil, err
	}
	a.sim = sim
	sim.Vendor("POST /iban/verify", sim.APIKey(a.verify))
	return a, nil
}

// refusal is the journal's line for a call iban.com refused: it answers a
// refusal with a 200 and an error field, which is a refusal all the same.
func refusal(c *activity.Call) (string, map[string]string) {
	c.Refused = c.RespField("error")
	return "", nil
}

func main() {
	addr := vendorsim.Listen(":8094")
	a, err := newApp(addr)
	if err != nil {
		log.Fatalf("iban-com: %v", err)
	}
	a.sim.Run(addr)
}

// verify answers the account and name check. The base URL is the endpoint
// for this vendor, so IBAN_COM_BASE_URL points straight at this path. The
// client hands the body to its own caller unchanged.
func (a *app) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IBAN string `json:"IBAN"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.IBAN == "" {
		httputilx.WriteJSON(w, 200, map[string]any{"error": "IBAN is required"})
		return
	}

	valid, nameMatch := true, "yes"
	switch a.sim.Answer(r, checkBankAccount) {
	case "mismatch":
		nameMatch = "no"
	case "invalid":
		valid, nameMatch = false, "no"
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"query":  map[string]any{"IBAN": body.IBAN, "name": body.Name, "success": true},
		"result": map[string]any{"valid": valid, "name_match": nameMatch, "bic": "SIMBGB2L"},
	})
}
