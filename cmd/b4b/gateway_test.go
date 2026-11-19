package main

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A platform gateway's B4B client, one run against the real vendor, walks
// a boarding chain; these tests send its request bodies exactly as it
// builds them, for a UK private limited company with one owner who is its
// director, no documents and one outlet:
//
//   - company:       the create, or a PATCH for a company already boarded
//   - addresses:     one per outlet (trading) and owner (ubo)
//   - person:        the owner as a natural person
//   - extended:      the regulatory profile
//   - beneficiary:   one per outlet
//   - upload slot:   a document's metadata, then its bytes
//
// and read back only what it reads. Placeholders in {braces} are the ids
// the chain learns as it goes.

const gatewayCallbackPath = "/api/v1/b4b/callback/sanctions"

// GB, so company_documents_waived; and person_documents_waived, because
// the one owner has no identity document on file. region is the county,
// sent as "" when there is none.
const gatewayCreateCompany = `{
  "external_ref": "IP000123",
  "legal_name": "Southwind Coffee Ltd",
  "company_type": "kyb",
  "acquirer": "Worldline",
  "primary_channel": "Online",
  "merchant_category_code": "5814",
  "company_documents_waived": true,
  "person_documents_waived": true,
  "address": {
    "line_1": "1 Simulation Way",
    "city": "London",
    "region": "",
    "postcode": "EC1A 1AA",
    "country": "GB",
    "type": "registered_office"
  }
}`

const gatewayOutletAddress = `{
  "id": "5d0f0c55-1b8a-5f1e-9a51-3e3c0b1f6a21",
  "type": "trading",
  "line_1": "2 High Street",
  "city": "Leeds",
  "country": "GB",
  "postcode": "LS1 1AA"
}`

const gatewayOwnerAddress = `{
  "id": "9b2e6f1a-7c3d-5e4f-8a9b-0c1d2e3f4a5b",
  "type": "ubo",
  "line_1": "3 Home Road",
  "city": "Bristol",
  "country": "GB",
  "line_2": "Flat 4",
  "region": "Avon",
  "postcode": "BS1 1AA"
}`

// The owner is a beneficial owner, so role shareholder; no document fields,
// because a UK owner with no document on file gets none.
const gatewayPerson = `{
  "external_ref": "42",
  "type": "natural_person",
  "role": "shareholder",
  "callback_url": "{callback_url}",
  "first_name": "Ada",
  "last_name": "Lovelace",
  "email": "ada@southwind.example",
  "positions_within_business": ["DIRECTOR"],
  "telephone_number": "+447700900123",
  "date_of_birth": "1985-12-10",
  "countries_of_citizenship": ["GB"],
  "address_id": "9b2e6f1a-7c3d-5e4f-8a9b-0c1d2e3f4a5b"
}`

// PRIVATE_LIMITED_COMPANY maps to "limited"; the director who is the
// beneficial owner is a key individual with director, ubo, and the
// authorised_representative role the client adds to every key individual.
const gatewayExtended = `{
  "company_type": "limited",
  "registered_company_no": "12345678",
  "registered_office_address_id": "{registered_office_address_id}",
  "business_address_id": "5d0f0c55-1b8a-5f1e-9a51-3e3c0b1f6a21",
  "tax_numbers": ["GB123456789"],
  "company_website": "https://southwind.example",
  "business_activities": "Coffee shop",
  "vat_number": "GB123456789",
  "compliance": {"risk_category": "low"},
  "ownership_structure": {
    "shareholders": [{"person_id": "{person_id}", "share_percentage": 100, "nominee": false}],
    "key_individuals": [{"person_id": "{person_id}", "share_percentage": 100, "roles": ["director", "ubo", "authorised_representative"]}]
  }
}`

// A GB business account: the account number, and the sort code as
// SC<digits>.
const gatewayBeneficiary = `{
  "company_id": "{company_id}",
  "external_ref": "beneficiary-outlet-7",
  "account_name": "Southwind Coffee Ltd",
  "beneficiary_type": "legal_entity",
  "account_number": "12345678",
  "financial_institution": "SC112233",
  "callback_url": "{callback_url}"
}`

const gatewayUpdateCompany = `{
  "external_ref": "IP000123",
  "legal_name": "Southwind Coffee Ltd",
  "company_type": "kyb",
  "acquirer": "Worldline",
  "primary_channel": "Online",
  "merchant_category_code": "5812",
  "company_documents_waived": true,
  "person_documents_waived": true
}`

// gatewayLab is b4b with a stand-in for the gateway's callback endpoint,
// and a token signed the way the gateway's jsonwebtoken signs one.
type gatewayLab struct {
	l        listeners
	token    string
	env      *peer // the environment's client-account endpoint
	gateway  *peer // the gateway, at the callback_url it sends
	callback string
}

func newGatewayLab(t *testing.T) *gatewayLab {
	t.Helper()
	env, gateway := newPeer(t), newPeer(t)
	a := testApp(t, env, newPeer(t), "", time.Millisecond)
	key := signingKey(t)
	lab := &gatewayLab{l: serve(t, a, key), env: env, gateway: gateway, callback: gateway.srv.URL + gatewayCallbackPath}
	lab.token = jsonwebtokenRS512(t, key, a.kid)
	return lab
}

// jsonwebtokenRS512 is what a client signing with the jsonwebtoken library
// sends: jwt.sign({aud}, key, {algorithm: RS512, keyid, expiresIn}), which
// adds typ to the header and iat and exp to the claims.
func jsonwebtokenRS512(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS512", "typ": "JWT", "kid": kid})
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{"aud": "b4b-payments", "iat": now, "exp": now + 3600})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha512.Sum512([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA512, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

// fill puts the chain's ids into a fixture.
func fill(fixture string, values map[string]string) string {
	for k, v := range values {
		fixture = strings.ReplaceAll(fixture, "{"+k+"}", v)
	}
	return fixture
}

// call is one gateway call, which must answer want.
func (lab *gatewayLab) call(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	st, b := send(t, lab.l.vendor, method, path, body, lab.token)
	if st != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, st, b, want)
	}
	return object(t, b)
}

// gatewayTakesSanctionsCallback mirrors a client's callback schema: id a
// non-empty string, sanctions_status one of pass, fail or review, and an
// optional string external_ref. It strips keys it does not declare, so an
// extra key is not a refusal.
func gatewayTakesSanctionsCallback(body string) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("not a JSON object: %s", body)
	}
	if id, ok := raw["id"].(string); !ok || id == "" {
		return nil, fmt.Errorf("id is not a non-empty string: %s", body)
	}
	switch raw["sanctions_status"] {
	case "pass", "fail", "review":
	default:
		return nil, fmt.Errorf("sanctions_status is not pass, fail or review: %s", body)
	}
	if ref, present := raw["external_ref"]; present {
		if _, ok := ref.(string); !ok {
			return nil, fmt.Errorf("external_ref is present and not a string: %s", body)
		}
	}
	return raw, nil
}

// The gateway's whole success path for a UK limited company: company,
// addresses, person, extended profile, beneficiary -- each body exactly as
// the gateway sends it, each answer carrying what the gateway reads, and
// both screening callbacks at the callback_url it sent, in a shape its
// callback schema takes.
func TestTheGatewaysUKBoardingChainRunsEndToEnd(t *testing.T) {
	lab := newGatewayLab(t)

	// The company create reads id; it logs address.type.
	company := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)
	companyID, _ := company["id"].(string)
	if companyID == "" {
		t.Fatalf("company answer has no id: %v", company)
	}
	office, _ := company["address"].(map[string]any)
	if office["type"] != "registered_office" || office["line_1"] != "1 Simulation Way" || office["postcode"] != "EC1A 1AA" {
		t.Fatalf("company address = %v, want the registered office in the gateway's field names", office)
	}
	if office["address_line_1"] != "1 Simulation Way" || office["postal_code"] != "EC1A 1AA" {
		t.Errorf("the answer drops the lab's earlier address names, which its other clients read: %v", office)
	}

	// The address create reads id, and uses it: the owner's becomes the person's
	// address_id, the outlet's the profile's business_address_id.
	for _, fixture := range []string{gatewayOutletAddress, gatewayOwnerAddress} {
		var sent map[string]any
		_ = json.Unmarshal([]byte(fixture), &sent)
		got := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/addresses", fixture, 201)
		if got["id"] != sent["id"] {
			t.Fatalf("address answer id = %v, want the id the gateway sent, %v", got["id"], sent["id"])
		}
	}

	// The owner's create reads id, external_ref and the inline sanctions_status.
	person := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/people",
		fill(gatewayPerson, map[string]string{"callback_url": lab.callback}), 201)
	personID, _ := person["id"].(string)
	if personID == "" || person["external_ref"] != "42" || person["sanctions_status"] != "pass" {
		t.Fatalf("person answer = %v, want an id, external_ref 42 and an inline sanctions_status of pass", person)
	}
	if person["entity_type"] != "natural_person" || person["address_id"] != "9b2e6f1a-7c3d-5e4f-8a9b-0c1d2e3f4a5b" {
		t.Errorf("person answer = %v, want the gateway's type and address_id kept", person)
	}

	// The extended step reads the registered office's id off a GET.
	read := lab.call(t, "GET", "/oversight/v1/companies/"+companyID, "", 200)
	officeID, _ := read["address"].(map[string]any)["id"].(string)
	if officeID == "" {
		t.Fatalf("GET company carries no address.id for registered_office_address_id: %v", read)
	}

	extended := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended",
		fill(gatewayExtended, map[string]string{"registered_office_address_id": officeID, "person_id": personID}), 201)
	if extended["id"] == "" || extended["company_type"] != "limited" || extended["registered_company_no"] != "12345678" ||
		extended["vat_number"] != "GB123456789" || extended["company_website"] != "https://southwind.example" {
		t.Fatalf("extended answer = %v, want the fields the gateway's response type names", extended)
	}

	// The beneficiary create reads id and external_ref.
	ben := lab.call(t, "POST", "/oversight/v1/beneficiaries",
		fill(gatewayBeneficiary, map[string]string{"company_id": companyID, "callback_url": lab.callback}), 201)
	benID, _ := ben["id"].(string)
	if benID == "" || ben["external_ref"] != "beneficiary-outlet-7" || ben["beneficiary_type"] != "legal_entity" {
		t.Fatalf("beneficiary answer = %v, want an id, external_ref beneficiary-outlet-7 and beneficiary_type kept", ben)
	}

	// Both screening callbacks go to the gateway's callback_url, none to
	// the environment's endpoint, and the gateway's schema takes each.
	waitFor(t, "the person and beneficiary callbacks", func() bool { return lab.gateway.count() == 2 })
	seen := map[string]map[string]any{}
	for _, body := range lab.gateway.received() {
		raw, err := gatewayTakesSanctionsCallback(body)
		if err != nil {
			t.Fatalf("the gateway would refuse this callback: %v", err)
		}
		seen[raw["id"].(string)] = raw
	}
	if seen[personID]["sanctions_status"] != "pass" {
		t.Errorf("no pass callback for person %s: %v", personID, seen)
	}
	if seen[benID]["sanctions_status"] != "pass" || seen[benID]["external_ref"] != "beneficiary-outlet-7" {
		t.Errorf("no pass callback for beneficiary %s: %v", benID, seen)
	}
	if n := lab.env.count(); n != 0 {
		t.Errorf("%d callback(s) went to the environment's endpoint; the records named their own callback_url", n)
	}
}

// A person created without a callback_url is still called back, at the
// environment's endpoint, as before.
func TestAPersonWithoutACallbackURLIsCalledBackAtTheEnvironments(t *testing.T) {
	lab := newGatewayLab(t)
	companyID := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)["id"].(string)
	person := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/people",
		`{"type": "legal_entity", "role": "shareholder", "name": "Holdco Ltd"}`, 201)
	if person["legal_name"] != "Holdco Ltd" || person["entity_type"] != "legal_entity" {
		t.Fatalf("legal entity answer = %v, want the gateway's name and type kept as legal_name and entity_type", person)
	}
	waitFor(t, "the person callback", func() bool { return lab.env.count() == 1 })
	if lab.gateway.count() != 0 {
		t.Errorf("a person with no callback_url was called back somewhere other than the environment's endpoint")
	}
}

// The rules the chain's references have to satisfy: a person's address_id
// and the profile's address and person ids must be this company's, and a
// callback_url must be one the environment may be called at.
func TestTheGatewaysReferencesMustNameThisCompanysRecords(t *testing.T) {
	lab := newGatewayLab(t)
	companyID := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)["id"].(string)

	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/people",
		fill(gatewayPerson, map[string]string{"callback_url": lab.callback}), 400) // no such address yet
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/addresses", gatewayOwnerAddress, 201)
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/addresses", gatewayOwnerAddress, 201) // a retry: the stored address
	moved := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/addresses",
		strings.Replace(gatewayOwnerAddress, "3 Home Road", "4 Home Road", 1), 201) // a re-run after an edit: the update
	if moved["id"] != "9b2e6f1a-7c3d-5e4f-8a9b-0c1d2e3f4a5b" || moved["line_1"] != "4 Home Road" {
		t.Fatalf("changed address under a known id = %v, want it updated in place", moved)
	}
	listed := lab.call(t, "GET", "/oversight/v1/companies/"+companyID+"/addresses", "", 200)["addresses"].([]any)
	if len(listed) != 2 || listed[1].(map[string]any)["line_1"] != "4 Home Road" {
		t.Fatalf("addresses = %v, want the office and the owner's updated address", listed)
	}
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/people",
		fill(gatewayPerson, map[string]string{"callback_url": "http://elsewhere.example" + gatewayCallbackPath}), 400)
	personID := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/people",
		fill(gatewayPerson, map[string]string{"callback_url": lab.callback}), 201)["id"].(string)

	officeID := lab.call(t, "GET", "/oversight/v1/companies/"+companyID, "", 200)["address"].(map[string]any)["id"].(string)
	// The outlet's address was never posted, so business_address_id names
	// nothing of this company's.
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended",
		fill(gatewayExtended, map[string]string{"registered_office_address_id": officeID, "person_id": personID}), 400)
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/addresses", gatewayOutletAddress, 201)
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended",
		fill(gatewayExtended, map[string]string{"registered_office_address_id": officeID, "person_id": "per_nobody"}), 400)
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended",
		fill(gatewayExtended, map[string]string{"registered_office_address_id": officeID, "person_id": personID}), 201)
}

// The gateway's branch for a company it already boarded: a PATCH of the
// KYB fields, answered with the company.
func TestTheGatewaysExistingCompanyBranchPatchesTheCompany(t *testing.T) {
	lab := newGatewayLab(t)
	companyID := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)["id"].(string)
	officeID := lab.call(t, "GET", "/oversight/v1/companies/"+companyID, "", 200)["address"].(map[string]any)["id"]

	// The PATCH reads id, external_ref and legal_name.
	got := lab.call(t, "PATCH", "/oversight/v1/companies/"+companyID, gatewayUpdateCompany, 200)
	if got["id"] != companyID || got["external_ref"] != "IP000123" || got["legal_name"] != "Southwind Coffee Ltd" {
		t.Fatalf("PATCH answer = %v, want the company", got)
	}
	read := lab.call(t, "GET", "/oversight/v1/companies/"+companyID, "", 200)
	if read["merchant_category_code"] != "5812" || read["address"].(map[string]any)["id"] != officeID {
		t.Fatalf("after PATCH = %v, want the new MCC and the registered office untouched", read)
	}

	lab.call(t, "PATCH", "/oversight/v1/companies/cmp_nobody", gatewayUpdateCompany, 404)
	other := strings.Replace(gatewayCreateCompany, `"IP000123"`, `"IP000999"`, 1)
	otherID := lab.call(t, "POST", "/oversight/v1/companies", other, 201)["id"].(string)
	lab.call(t, "PATCH", "/oversight/v1/companies/"+otherID, gatewayUpdateCompany, 422) // IP000123 is taken
	lab.call(t, "PATCH", "/oversight/v1/companies/"+companyID, `{"merchant_category_code": "58"}`, 400)

	// A partial address change keeps the fields it did not send.
	patched := lab.call(t, "PATCH", "/oversight/v1/companies/"+companyID, `{"address": {"postcode": "EC1A 2BB"}}`, 200)
	address := patched["address"].(map[string]any)
	if address["postcode"] != "EC1A 2BB" || address["line_1"] != "1 Simulation Way" || address["id"] != officeID {
		t.Fatalf("patched address = %v, want the new postcode on the same registered office", address)
	}

	// A field sent as "" clears it, under either spelling, and the
	// registered office's record in GET .../addresses with it.
	for _, name := range []string{"line_2", "address_line_2"} {
		set := lab.call(t, "PATCH", "/oversight/v1/companies/"+companyID, `{"address": {"`+name+`": "Unit 2"}}`, 200)
		if line2 := set["address"].(map[string]any)["line_2"]; line2 != "Unit 2" {
			t.Fatalf("%s set: line_2 = %v, want Unit 2", name, line2)
		}
		cleared := lab.call(t, "PATCH", "/oversight/v1/companies/"+companyID, `{"address": {"`+name+`": ""}}`, 200)
		clearedAddress := cleared["address"].(map[string]any)
		if clearedAddress["line_2"] != nil || clearedAddress["address_line_2"] != nil || clearedAddress["postcode"] != "EC1A 2BB" {
			t.Fatalf("%s sent empty: address = %v, want line_2 cleared in both spellings and the rest kept", name, clearedAddress)
		}
		listed := lab.call(t, "GET", "/oversight/v1/companies/"+companyID+"/addresses", "", 200)["addresses"].([]any)
		if office := listed[0].(map[string]any); office["id"] != officeID || office["line_2"] != nil {
			t.Fatalf("%s sent empty: addresses = %v, want the registered office's line_2 cleared", name, listed)
		}
	}
}

// The gateway's document upload: a slot for the company, the bytes PUT to
// its URL with its headers, then the slot's id named in the extended
// profile as {type, document}.
func TestTheGatewaysDocumentUploadIsASlotAndAPut(t *testing.T) {
	lab := newGatewayLab(t)
	// Not waived, so the profile needs a document.
	unwaived := strings.Replace(gatewayCreateCompany, `"company_documents_waived": true`, `"company_documents_waived": false`, 1)
	companyID := lab.call(t, "POST", "/oversight/v1/companies", unwaived, 201)["id"].(string)

	file := []byte("%PDF-1.4 a registry extract")
	sum := md5.Sum(file)
	checksum := base64.StdEncoding.EncodeToString(sum[:])
	metadata := fmt.Sprintf(`{"filename": "extract.pdf", "byte_size": %d, "checksum": %q, "content_type": "application/pdf"}`, len(file), checksum)

	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/uploads",
		`{"filename": "extract.pdf", "byte_size": 27, "checksum": "not-md5", "content_type": "application/pdf"}`, 400)
	lab.call(t, "POST", "/oversight/v1/companies/cmp_nobody/uploads", metadata, 404)

	// The slot create reads id, url and headers.
	slot := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/uploads", metadata, 201)
	slotID, _ := slot["id"].(string)
	url, _ := slot["url"].(string)
	headers, _ := slot["headers"].(map[string]any)
	if slotID == "" || !strings.HasPrefix(url, lab.l.vendor.URL+"/") || headers["Content-MD5"] != checksum {
		t.Fatalf("slot = %v, want an id, a url on this listener and the checksum as Content-MD5", slot)
	}

	profile := `{"company_type": "limited", "compliance": {"risk_category": "low"}, "documents": [{"type": "business_registry_extract", "document": "` + slotID + `"}]}`
	lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended", profile, 400) // no bytes yet

	put := func(target string, body []byte) (int, string) {
		req, err := http.NewRequest("PUT", target, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v.(string))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, _ := put(url, []byte("not the declared bytes!!!!!")); st != 400 {
		t.Fatalf("PUT of other bytes = %d, want 400", st)
	}
	if st, _ := put(lab.l.vendor.URL+uploadPath+"0000", file); st != 403 {
		t.Fatalf("PUT to a URL no slot has = %d, want 403", st)
	}
	// The client takes only a 200 with an empty body as success.
	if st, body := put(url, file); st != 200 || body != "" {
		t.Fatalf("PUT of the declared bytes = %d %q, want 200 and an empty body", st, body)
	}

	extended := lab.call(t, "POST", "/oversight/v1/companies/"+companyID+"/extended", profile, 201)
	docs, _ := extended["documents"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["document"] != slotID {
		t.Fatalf("extended documents = %v, want the {type, document} entry echoed", extended["documents"])
	}
	receipt := lab.call(t, "GET", "/oversight/v1/uploads/"+slotID, "", 200)
	if _, leaked := receipt["upload_token"]; leaked {
		t.Errorf("the receipt carries the slot's secret: %v", receipt)
	}
}

// A client that reaches b4b under a path prefix, through a proxy that
// strips it and names it in X-Forwarded-Prefix, gets a slot URL under the
// same prefix. Behind a chain of proxies the host and proto take their first
// entry, the one the client used, and the prefix joins every proxy's part.
func TestAnUploadSlotURLKeepsTheCallersPathPrefix(t *testing.T) {
	cases := map[string]struct{ host, proto, prefix, base string }{
		"one proxy":   {host: "lab.example", proto: "https", prefix: "/b4b/", base: "https://lab.example/b4b"},
		"two proxies": {host: "lab.example, b4b", proto: "https,http", prefix: "/lab, /b4b", base: "https://lab.example/lab/b4b"},
	}
	for name, forwarded := range cases {
		t.Run(name, func(t *testing.T) {
			lab := newGatewayLab(t)
			companyID := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)["id"].(string)
			req, err := http.NewRequest("POST", lab.l.vendor.URL+"/oversight/v1/companies/"+companyID+"/uploads",
				strings.NewReader(`{"filename": "x.pdf", "byte_size": 3, "checksum": "rL0Y20zC+Fzt72VPzMSk2A==", "content_type": "application/pdf"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+lab.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-Host", forwarded.host)
			req.Header.Set("X-Forwarded-Proto", forwarded.proto)
			req.Header.Set("X-Forwarded-Prefix", forwarded.prefix)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 201 {
				t.Fatalf("slot create = %d %s", resp.StatusCode, b)
			}
			url, _ := object(t, b)["url"].(string)
			if !strings.HasPrefix(url, forwarded.base+uploadPath) {
				t.Fatalf("slot url = %q, want it under %s", url, forwarded.base)
			}
		})
	}
}
