package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type b4bCompanyResp struct {
	ID                    string `json:"id"`
	ExternalRef           string `json:"external_ref"`
	LegalName             string `json:"legal_name"`
	Status                string `json:"status"`
	SanctionsStatus       string `json:"sanctions_status"`
	PersonDocumentsWaived bool   `json:"person_documents_waived"`
}

type b4bPersonResp struct {
	ID                 string `json:"id"`
	EntityType         string `json:"entity_type"`
	SanctionsStatus    string `json:"sanctions_status"`
	PepSanctionsStatus string `json:"pep_sanctions_status"`
}

type b4bExtendedResp struct {
	ID                  string   `json:"id"`
	CompanyID           string   `json:"company_id"`
	RegisteredCompanyNo string   `json:"registered_company_no"`
	Documents           []string `json:"documents"`
}

type b4bDocumentResp struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

type b4bVibanResp struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
	IBAN     string `json:"iban"`
}

// B4BCompanyBoarding walks the chain a platform has to complete before it
// can pay anybody -- company, address, upload, people, extended profile,
// vIBAN, beneficiary -- and asserts the four refusals that decide whether a
// client's boarding code is correct:
//
//   - a repeated external_ref is 422, not a second company;
//   - an identity document is required on a natural person unless the
//     company was boarded with the waiver;
//   - the extended profile can be created once and only once;
//   - a profile naming an upload that never happened is refused.
//
// Every one of those is a real 422 a client meets in production and cannot
// meet against a mock that only answers the happy path.
func B4BCompanyBoarding() Scenario {
	return Scenario{
		Name: "b4b-company-boarding",
		Run: func(ctx context.Context, env *Env, state *State) error {
			token, ok := state.Get("b4b_bearer")
			if !ok {
				var err error
				token, err = b4bBearer(env)
				if err != nil {
					return err
				}
				state.Set("b4b_bearer", token)
			}
			hdr := map[string]string{"Authorization": "Bearer " + token}
			base := env.B4BURL + "/oversight/v1"

			// B4B persists, so a re-run must not collide with the last one.
			ref := fmt.Sprintf("harness-%d", time.Now().UnixNano())
			address := map[string]any{
				"address_line_1": "1 Simulation Way",
				"city":           "London",
				"postal_code":    "EC1A 1AA",
				"country":        "GB",
			}
			companyBody := func(externalRef string, personWaiver bool) map[string]any {
				return map[string]any{
					"external_ref":             externalRef,
					"legal_name":               "Harness Boarding Ltd",
					"trading_name":             "Harness Boarding",
					"company_type":             "kyb",
					"acquirer":                 "Worldline",
					"primary_channel":          "Infinite Payments",
					"merchant_category_code":   "5732",
					"company_documents_waived": false,
					"person_documents_waived":  personWaiver,
					"address":                  address,
				}
			}

			var company b4bCompanyResp
			status, err := PostJSON(ctx, env.Client, base+"/companies", hdr, companyBody(ref, false), &company)
			if err != nil {
				return fmt.Errorf("create company: %w", err)
			}
			if status != 201 || company.ID == "" {
				return fmt.Errorf("create company: want 201 + an id, got %d (%+v)", status, company)
			}
			if company.Status != "active" || company.SanctionsStatus != "pass" {
				return fmt.Errorf("a freshly boarded company is status=%q sanctions_status=%q, want active/pass",
					company.Status, company.SanctionsStatus)
			}

			// A repeated external_ref is how a reconciler learns it already
			// sent this. A second company would make that unknowable.
			dup, err := PostJSON(ctx, env.Client, base+"/companies", hdr, companyBody(ref, false), nil)
			if err != nil {
				return fmt.Errorf("duplicate company create: %w", err)
			}
			if dup != 422 {
				return fmt.Errorf("re-creating a company under an existing external_ref = %d, want 422", dup)
			}

			addrStatus, err := PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/addresses", hdr,
				merge(address, map[string]any{"type": "trading"}), nil)
			if err != nil {
				return fmt.Errorf("add address: %w", err)
			}
			if addrStatus != 201 {
				return fmt.Errorf("add address = %d, want 201", addrStatus)
			}

			// No waiver on this company, so a person with no identity
			// document is refused -- the non-GB boarding failure, exactly.
			person := map[string]any{
				"first_name":    "Ada",
				"last_name":     "Lovelace",
				"date_of_birth": "1815-12-10",
				"roles":         []string{"director", "ubo"},
			}
			undocumented, err := PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/people", hdr, person, nil)
			if err != nil {
				return fmt.Errorf("person with no identity document: %w", err)
			}
			if undocumented != 422 {
				return fmt.Errorf("a natural person with no document_type/number/country on an unwaived company = %d, want 422", undocumented)
			}

			var natural b4bPersonResp
			documented := merge(person, map[string]any{
				"document_type": "passport", "document_number": "P1234567", "document_country": "GB",
			})
			status, err = PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/people", hdr, documented, &natural)
			if err != nil {
				return fmt.Errorf("create person: %w", err)
			}
			if status != 201 || natural.ID == "" {
				return fmt.Errorf("create person: want 201 + an id, got %d (%+v)", status, natural)
			}
			if natural.SanctionsStatus != "pass" || natural.PepSanctionsStatus != "pass" {
				return fmt.Errorf("a new person screens %q/%q, want pass/pass -- and the two are reported separately",
					natural.SanctionsStatus, natural.PepSanctionsStatus)
			}

			// A shareholding company goes through the same endpoint.
			var entity b4bPersonResp
			status, err = PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/people", hdr, map[string]any{
				"entity_type": "legal_entity", "legal_name": "Harness Holdings Ltd", "registered_company_no": "87654321",
			}, &entity)
			if err != nil {
				return fmt.Errorf("create legal entity: %w", err)
			}
			if status != 201 || entity.EntityType != "legal_entity" {
				return fmt.Errorf("create legal entity: got %d %+v", status, entity)
			}

			// The extended profile needs a document, and the document has
			// to have been uploaded: a profile naming an id nobody sent is
			// a dangling reference, not a boarding.
			ghost, err := PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/extended", hdr, map[string]any{
				"registered_company_no": "12345678", "documents": []string{"doc_never_uploaded"},
			}, nil)
			if err != nil {
				return fmt.Errorf("extended profile naming an unknown document: %w", err)
			}
			if ghost != 400 {
				return fmt.Errorf("an extended profile naming an unuploaded document = %d, want 400", ghost)
			}

			var doc b4bDocumentResp
			status, err = PostJSON(ctx, env.Client, base+"/uploads", hdr, map[string]any{
				"document_type": "business_registry_extract",
				"file_name":     "companies-house-extract.pdf",
				"content_type":  "application/pdf",
				"size":          20481,
			}, &doc)
			if err != nil {
				return fmt.Errorf("upload document: %w", err)
			}
			if status != 201 || doc.ID == "" {
				return fmt.Errorf("upload document: want 201 + an id, got %d (%+v)", status, doc)
			}

			extendedBody := map[string]any{
				"registered_company_no": "12345678",
				"nace_codes":            []string{"6201"},
				"documents":             []string{doc.ID},
				"incorporation_date":    "2019-04-01",
				"legal_form":            "private_limited_company",
			}
			var extended b4bExtendedResp
			status, err = PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/extended", hdr, extendedBody, &extended)
			if err != nil {
				return fmt.Errorf("create extended profile: %w", err)
			}
			if status != 201 || extended.ID == "" {
				return fmt.Errorf("create extended profile: want 201 + an id, got %d (%+v)", status, extended)
			}

			// Create-once. A boarding chain re-run from the top lands here,
			// and a client that treats this 422 as fatal never finishes.
			second, err := PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/extended", hdr, extendedBody, nil)
			if err != nil {
				return fmt.Errorf("second extended profile: %w", err)
			}
			if second != 422 {
				return fmt.Errorf("re-creating an extended profile = %d, want 422 -- it can only be created once", second)
			}

			// Which is why the read exists: this is what a client should do
			// before posting the second time.
			var reread b4bExtendedResp
			if _, err := GetJSONWithHeaders(ctx, env.Client, base+"/companies/"+company.ID+"/extended", hdr, &reread); err != nil {
				return fmt.Errorf("read extended profile back: %w", err)
			}
			if reread.ID != extended.ID {
				return fmt.Errorf("read-back extended profile id = %q, want %q", reread.ID, extended.ID)
			}

			var viban b4bVibanResp
			status, err = PostJSON(ctx, env.Client, base+"/companies/"+company.ID+"/vibans", hdr,
				map[string]any{"currency": "EUR"}, &viban)
			if err != nil {
				return fmt.Errorf("register viban: %w", err)
			}
			if status != 201 || !strings.HasPrefix(viban.IBAN, "GB00SIM") {
				return fmt.Errorf("register viban: got %d %+v, want 201 and an obviously fake IBAN", status, viban)
			}

			// And the payout half joins on: a beneficiary registered under
			// the company that was just boarded.
			var ben b4bBeneficiaryResp
			status, err = PostJSON(ctx, env.Client, base+"/beneficiaries", hdr, map[string]any{
				"company_id":            company.ID,
				"external_ref":          ref + "-payee",
				"account_name":          "Harness Boarding Ltd",
				"account_number":        "GB00SIM0000000000077",
				"financial_institution": "SC112233",
				"country":               "GB",
			}, &ben)
			if err != nil {
				return fmt.Errorf("register beneficiary under the boarded company: %w", err)
			}
			if status != 201 || ben.ID == "" {
				return fmt.Errorf("register beneficiary: want 201 + an id, got %d (%+v)", status, ben)
			}

			// A payee under a company nobody boarded is a mistake worth
			// catching here rather than at the first payout.
			orphan, err := PostJSON(ctx, env.Client, base+"/beneficiaries", hdr, map[string]any{
				"company_id":            "cmp_never_boarded",
				"account_name":          "Nobody Ltd",
				"account_number":        "GB00SIM0000000000078",
				"financial_institution": "SC112233",
			}, nil)
			if err != nil {
				return fmt.Errorf("beneficiary under an unknown company: %w", err)
			}
			if orphan != 404 {
				return fmt.Errorf("registering a beneficiary under an unboarded company = %d, want 404", orphan)
			}
			return nil
		},
	}
}

func merge(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
