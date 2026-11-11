package e2e

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
)

// EnvironmentsKeepPlatformsApart proves the lab serves two platforms
// without either seeing the other (docs/lab-api/environments.md): it makes
// two throwaway environments with the admin token, takes each one's kit as
// a platform would, and checks every vendor over its own surface, with
// each environment's own login —
//
//   - Banking Circle: each logs in with its own credential; one funds its
//     safeguarding account, and the other cannot even read that account;
//   - B4B: both board a company under the same external_ref, and neither
//     lists the other's;
//   - Worldline: a settlement file cut for one is in its SFTP folder, and
//     neither the other's login nor the default's can see it;
//   - the lab API: one's token cannot read the other.
//
// Then it deletes both, and their tokens stop working. It runs in no
// environment of its own and leaves the default's records alone.
func EnvironmentsKeepPlatformsApart() Scenario {
	return Scenario{
		Name: "environments-keep-platforms-apart",
		Run: func(ctx context.Context, env *Env, state *State) error {
			stamp := strconv.FormatInt(time.Now().Unix(), 36)
			a, err := newEnvironment(ctx, env, "e2e-"+stamp+"-a")
			if err != nil {
				return err
			}
			defer a.drop(env)
			b, err := newEnvironment(ctx, env, "e2e-"+stamp+"-b")
			if err != nil {
				return err
			}
			defer b.drop(env)

			// The lab API: a token is its environment's alone.
			if status, err := a.lab.Get(ctx, "/v1/environments/"+b.id, nil); err != nil || status != 404 {
				return fmt.Errorf("%s's token reading %s: %d %v, want 404", a.id, b.id, status, err)
			}

			// Banking Circle.
			aBC, err := a.bankingCircleBearer(ctx, env)
			if err != nil {
				return err
			}
			bBC, err := b.bankingCircleBearer(ctx, env)
			if err != nil {
				return err
			}
			var fund struct {
				Results []struct {
					Currency, Account, Funded, Skipped, Error string
				} `json:"results"`
			}
			if status, err := a.lab.Post(ctx, "/v1/actions/fund-sga?amount=500.00", nil, &fund); err != nil || status != 200 {
				return fmt.Errorf("fund %s's safeguarding accounts: %d %v", a.id, status, err)
			}
			eur := a.kit["BC_SAFEGUARDING_ACCOUNT_ID_EUR"]
			funded := false
			for _, r := range fund.Results {
				if r.Currency == "EUR" && r.Account == eur && r.Funded == "500.00" {
					funded = true
				}
			}
			if !funded {
				return fmt.Errorf("fund-sga in %s did not fund its own EUR account %s: %+v", a.id, eur, fund.Results)
			}
			balances := env.BankingCircleURL + "/api/v1/accounts/" + eur + "/balances"
			if status, err := GetJSONWithHeaders(ctx, env.Client, balances, aBC, nil); err != nil || status != 200 {
				return fmt.Errorf("%s reading its own safeguarding account: %d %v", a.id, status, err)
			}
			if status, err := GetJSONWithHeaders(ctx, env.Client, balances, bBC, nil); err != nil || status != 404 {
				return fmt.Errorf("%s reading %s's safeguarding account: %d %v, want 404", b.id, a.id, status, err)
			}

			// B4B.
			ref := "e2e-env-" + stamp
			aCompany, err := a.boardCompany(ctx, env, ref)
			if err != nil {
				return err
			}
			bCompany, err := b.boardCompany(ctx, env, ref)
			if err != nil {
				return fmt.Errorf("the same external_ref in a second environment: %w", err)
			}
			if aCompany == bCompany {
				return fmt.Errorf("two environments got one company, %s", aCompany)
			}
			if listed, err := a.companyIDs(ctx, env); err != nil {
				return err
			} else if listed[bCompany] || !listed[aCompany] {
				return fmt.Errorf("%s's company list %v: want its own %s and not %s's %s", a.id, keys(listed), aCompany, b.id, bCompany)
			}

			// Worldline: a file cut for a, in a's folder only.
			day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
			tx := map[string]any{"mid": "E2EENV" + strings.ToUpper(stamp), "currency": "EUR", "amount_cents": 4200, "date": day, "card_scheme_name": "Visa"}
			if status, err := a.lab.Post(ctx, Sim("worldline", "transactions"), []any{tx}, nil); err != nil || status >= 300 {
				return fmt.Errorf("seed a card payment in %s: %d %v", a.id, status, err)
			}
			var run struct {
				Published []string `json:"published"`
			}
			if status, err := a.lab.Post(ctx, "/v1/actions/worldline-cycle?slot=morning&date="+day, nil, &run); err != nil || status >= 300 {
				return fmt.Errorf("cut %s's settlement file: %d %v", a.id, status, err)
			}
			if len(run.Published) == 0 {
				return fmt.Errorf("the cycle in %s published nothing", a.id)
			}
			file := lastPart(run.Published[0])
			if seen, err := a.sftpFiles(env); err != nil || !seen[file] {
				return fmt.Errorf("%s's own SFTP folder: %v, want %s (%v)", a.id, keys(seen), file, err)
			}
			if seen, err := b.sftpFiles(env); err != nil || seen[file] {
				return fmt.Errorf("%s's SFTP login sees %s's file %s (%v)", b.id, a.id, file, err)
			}
			if seen, err := sftpFilesAs(env, "harness", env.WorldlineSFTPPassword); err != nil || seen[file] {
				return fmt.Errorf("the default's SFTP login sees %s's file %s (%v)", a.id, file, err)
			}

			// Gone, and their tokens with them.
			a.drop(env)
			if status, _ := a.lab.Get(ctx, "/v1/environment", nil); status != 401 {
				return fmt.Errorf("%s's token after its environment was deleted: %d, want 401", a.id, status)
			}
			return nil
		},
	}
}

// environment is one throwaway environment, as its platform holds it: its
// lab token and its kit.
type environment struct {
	id      string
	lab     Lab
	kit     map[string]string
	b4bKey  []byte
	dropped bool
}

func newEnvironment(ctx context.Context, env *Env, id string) (*environment, error) {
	var made struct {
		Token string `json:"token"`
	}
	status, err := env.Lab.Post(ctx, "/v1/environments", map[string]any{"id": id, "name": id}, &made)
	if err != nil || status != 201 {
		return nil, fmt.Errorf("create environment %s (the admin's LAB_API_TOKEN): %d %v", id, status, err)
	}
	e := &environment{id: id, lab: Lab{URL: env.Lab.URL, Token: made.Token, Client: env.Lab.Client}}
	text, err := e.download(ctx, "/v1/connect/env")
	if err != nil {
		return e, err
	}
	e.kit = parseEnvFile(text)
	if e.b4bKey, err = e.download(ctx, "/v1/connect/files/b4b/b4b-jwt-private.pem"); err != nil {
		return e, err
	}
	for _, k := range []string{"BC_API_M2M_USERNAME", "BC_API_M2M_PASSWORD", "BC_SAFEGUARDING_ACCOUNT_ID_EUR", "B4B_JWT_KEY_ID", "WORLDLINE_SFTP_USERNAME", "WORLDLINE_SFTP_PASSWORD"} {
		if e.kit[k] == "" {
			return e, fmt.Errorf("%s's kit has no %s", id, k)
		}
	}
	return e, nil
}

// drop deletes the environment, once.
func (e *environment) drop(env *Env) {
	if e == nil || e.dropped {
		return
	}
	e.dropped = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, env.Lab.URL+"/v1/environments/"+e.id, nil)
	if err != nil {
		return
	}
	for k, v := range env.Lab.headers() {
		req.Header.Set(k, v)
	}
	if resp, err := env.Lab.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (e *environment) download(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.lab.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.lab.Token)
	resp, err := e.lab.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s with %s's token: HTTP %d %s", path, e.id, resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}

func (e *environment) bankingCircleBearer(ctx context.Context, env *Env) (map[string]string, error) {
	basic := base64.StdEncoding.EncodeToString([]byte(e.kit["BC_API_M2M_USERNAME"] + ":" + e.kit["BC_API_M2M_PASSWORD"]))
	var auth bcAuthorizeResp
	status, err := GetJSONWithHeaders(ctx, env.Client, env.BankingCircleURL+"/api/v1/authorizations/authorize",
		map[string]string{"Authorization": "Basic " + basic}, &auth)
	if err != nil || status != 200 || auth.AccessToken == "" {
		return nil, fmt.Errorf("%s's Banking Circle login: %d %v", e.id, status, err)
	}
	return map[string]string{"Authorization": "Bearer " + auth.AccessToken}, nil
}

func (e *environment) b4bHeaders() (map[string]string, error) {
	block, _ := pemDecode(e.b4bKey)
	if block == nil {
		return nil, fmt.Errorf("%s's B4B key holds no PEM block", e.id)
	}
	key, err := parseRSAPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s's B4B key: %w", e.id, err)
	}
	token, err := b4b.SignBearerToken(key, e.kit["B4B_JWT_KEY_ID"])
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}

func (e *environment) boardCompany(ctx context.Context, env *Env, ref string) (string, error) {
	hdr, err := e.b4bHeaders()
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"external_ref": ref, "legal_name": "Environment " + e.id + " Ltd", "trading_name": "Environment " + e.id,
		"company_type": "kyb", "acquirer": "Worldline", "primary_channel": "Infinite Payments",
		"merchant_category_code": "5732", "company_documents_waived": false, "person_documents_waived": true,
		"address": map[string]any{"address_line_1": "1 Simulation Way", "city": "London", "postal_code": "EC1A 1AA", "country": "GB"},
	}
	var c b4bCompanyResp
	status, err := PostJSON(ctx, env.Client, env.B4BURL+"/oversight/v1/companies", hdr, body, &c)
	if err != nil || status != 201 || c.ID == "" {
		return "", fmt.Errorf("%s boarding %s at B4B: %d %v", e.id, ref, status, err)
	}
	return c.ID, nil
}

func (e *environment) companyIDs(ctx context.Context, env *Env) (map[string]bool, error) {
	hdr, err := e.b4bHeaders()
	if err != nil {
		return nil, err
	}
	var list struct {
		Companies []b4bCompanyResp `json:"companies"`
	}
	if status, err := GetJSONWithHeaders(ctx, env.Client, env.B4BURL+"/oversight/v1/companies", hdr, &list); err != nil || status != 200 {
		return nil, fmt.Errorf("%s listing its companies: %d %v", e.id, status, err)
	}
	out := map[string]bool{}
	for _, c := range list.Companies {
		out[c.ID] = true
	}
	return out, nil
}

func (e *environment) sftpFiles(env *Env) (map[string]bool, error) {
	return sftpFilesAs(env, e.kit["WORLDLINE_SFTP_USERNAME"], e.kit["WORLDLINE_SFTP_PASSWORD"])
}

// sftpFilesAs lists Worldline's download folder as user sees it.
func sftpFilesAs(env *Env, user, password string) (map[string]bool, error) {
	c, err := wlsftp.Dial(wlsftp.ClientConfig{Host: env.WorldlineSFTPHost, Port: env.WorldlineSFTPPort, User: user, Password: password})
	if err != nil {
		return nil, fmt.Errorf("SFTP as %s: %w", user, err)
	}
	defer c.Close()
	files, err := c.List()
	if err != nil {
		return nil, fmt.Errorf("SFTP list as %s: %w", user, err)
	}
	out := map[string]bool{}
	for _, f := range files {
		out[f.Name] = true
	}
	return out, nil
}

// parseEnvFile reads a kit's .env: KEY=value lines, a quoted value
// possibly spanning lines (a PEM), comments and commented-out lines left
// out.
func parseEnvFile(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.HasPrefix(v, `"`) {
			v = strings.TrimPrefix(v, `"`)
			for !strings.HasSuffix(v, `"`) && sc.Scan() {
				v += "\n" + sc.Text()
			}
			v = strings.TrimSuffix(v, `"`)
		}
		out[k] = v
	}
	return out
}

func lastPart(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// EnvironmentCardPublishesAtWorldline proves the environment's platform
// card (docs/lab-api/plan.md, phase 6) on a real lab: in a throwaway
// environment it seeds a card payment, has Worldline render the morning
// file into the environment's files, publishes it with its lump sum, and
// finds it in the environment's own SFTP folder and the money on its own
// safeguarding account. Its platform's poll is not set, so Poll now is the
// lab's 409.
func EnvironmentCardPublishesAtWorldline() Scenario {
	return Scenario{
		Name: "environment-card-publishes-at-worldline",
		Run: func(ctx context.Context, env *Env, state *State) error {
			e, err := newEnvironment(ctx, env, "e2e-card-"+strconv.FormatInt(time.Now().Unix(), 36))
			if err != nil {
				return err
			}
			defer e.drop(env)

			day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
			tx := map[string]any{"mid": "E2ECARD1", "currency": "EUR", "amount_cents": 12345, "date": day, "card_scheme_name": "Visa"}
			if status, err := e.lab.Post(ctx, Sim("worldline", "transactions"), []any{tx}, nil); err != nil || status >= 300 {
				return fmt.Errorf("seed a card payment in %s: %d %v", e.id, status, err)
			}
			var gen struct {
				File struct {
					Name, Currency, Total, Problem string
				} `json:"file"`
			}
			if status, err := e.lab.Post(ctx, "/v1/settlement-files/generate", map[string]string{"currency": "EUR", "date": day}, &gen); err != nil || status != 201 {
				return fmt.Errorf("generate %s's EUR file: %d %v", e.id, status, err)
			}
			if gen.File.Problem != "" || gen.File.Currency != "EUR" {
				return fmt.Errorf("the generated file: %+v", gen.File)
			}
			var pub struct {
				Published int              `json:"published"`
				Results   []map[string]any `json:"results"`
			}
			if status, err := e.lab.Post(ctx, "/v1/settlement-files/publish", map[string]any{"files": []string{gen.File.Name}, "wire": true}, &pub); err != nil || status != 200 || pub.Published != 1 {
				return fmt.Errorf("publish %s: %d %v %+v", gen.File.Name, status, err, pub.Results)
			}
			published, _ := pub.Results[0]["published"].(string)
			published = lastPart(published)
			seen, err := e.sftpFiles(env)
			if err != nil || published == "" || !seen[published] {
				return fmt.Errorf("%s's SFTP folder: %v, want %q (%v)", e.id, keys(seen), published, err)
			}
			if other, err := sftpFilesAs(env, "harness", env.WorldlineSFTPPassword); err == nil && other[published] {
				return fmt.Errorf("the default's SFTP login sees %s's published file", e.id)
			}
			var accounts bcAccounts
			if status, err := e.lab.Get(ctx, Sim("banking-circle", "accounts"), &accounts); err != nil || status != 200 {
				return fmt.Errorf("%s's Banking Circle accounts: %d %v", e.id, status, err)
			}
			sga := e.kit["BC_SAFEGUARDING_ACCOUNT_ID_EUR"]
			funded := false
			for _, a := range accounts.Accounts {
				if a.ID == sga && a.Balance != "0.00" && a.Balance != "" {
					funded = true
				}
			}
			if !funded {
				return fmt.Errorf("the lump sum did not reach %s's safeguarding account %s: %+v", e.id, sga, accounts.Accounts)
			}
			if status, _ := e.lab.Post(ctx, "/v1/platform/poll", map[string]string{"fileType": "ER"}, nil); status != 409 {
				return fmt.Errorf("poll with no poll_url: %d, want 409", status)
			}
			return nil
		},
	}
}
