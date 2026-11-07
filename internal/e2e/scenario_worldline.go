package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

const merchantIBAN = "GB00SIM0000000000003"

type reportResp struct {
	MerchantID               string `json:"merchant_id"`
	SettlementState          string `json:"settlement_state"`
	ApprovedTransactionCount int    `json:"approved_transaction_count"`
	SettlementNetAmount      int64  `json:"settlement_net_amount"`
}

type cycleRunResp struct {
	Slot     string        `json:"slot"`
	FromDate string        `json:"from_date"`
	ToDate   string        `json:"to_date"`
	Files    []cutFileResp `json:"files"`
	LumpSums []struct {
		Currency    string `json:"currency"`
		AmountCents int64  `json:"amount_cents"`
		Status      string `json:"status"`
	} `json:"lump_sums"`
	Published []string `json:"published"`
	Errors    []string `json:"errors"`
}

type cutFileResp struct {
	Filename    string   `json:"filename"`
	MIDs        []string `json:"mids"`
	Currency    string   `json:"currency"`
	AmountCents int64    `json:"amount_cents"`
	Items       int      `json:"items"`
}

type pulledFilesResp struct {
	Files []struct {
		Name         string   `json:"name"`
		Slot         string   `json:"slot"`
		Processed    bool     `json:"processed"`
		Settlements  []string `json:"settlement_ids"`
		PayoutsByMID int      `json:"payouts_by_mid"`
		Error        string   `json:"error"`
	} `json:"files"`
}

// WorldlineSettlementToSFTP drives the real acquirer-to-platform path:
// Worldline acquires card transactions, cuts the morning settlement file
// from them, publishes it PGP-encrypted on its SFTP server and wires the
// matching lump sum to the safeguarding account; the platform then takes
// delivery of that file over the wire, decrypts it, and pays each
// submerchant out.
//
// The point of this scenario is that the platform has no other route to
// the file. Nothing is asserted from a shared directory, and the e2e suite
// independently dials the same SFTP server itself and re-parses the same
// bytes, so a passing run means the protocol really worked -- SSH auth,
// SFTP transfer, PGP decryption, CSV parse -- rather than that two
// services agreed about a file neither of them moved.
func WorldlineSettlementToSFTP() Scenario {
	return Scenario{
		Name: "worldline-settlement-to-sftp",
		Run: func(ctx context.Context, env *Env, state *State) error {
			// Worldline settles T+1, so transactions have to be dated
			// yesterday for today's morning cycle to pick them up.
			settledDay := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

			// Captured before the cycle, because taking delivery of the
			// file is what triggers the B4B/Banking Circle payout as a
			// side effect, and banking-circle-payout needs a true
			// pre-payout snapshot to assert an exact balance delta.
			bcAccountID := bankingcircle.AccountIDFor(merchantIBAN)
			before, err := bcReadBalance(ctx, env, bcAccountID)
			if err != nil {
				return fmt.Errorf("baseline banking-circle balance: %w", err)
			}
			state.Set("bc_merchant_balance_before", before)

			// 1. A card payment happened at the merchant's paypoint. The
			// acquirer is told, because the acquirer is who would know:
			// Worldline's lab hook, through the lab API.
			txns := []map[string]any{
				{"mid": merchantIBAN, "currency": "EUR", "amount_cents": 10000, "date": settledDay, "card_scheme_name": "Visa"},
				{"mid": merchantIBAN, "currency": "EUR", "amount_cents": 5000, "date": settledDay, "card_scheme_name": "Mastercard"},
			}
			var seeded map[string]any
			status, err := env.Lab.Post(ctx, Sim("worldline", "transactions"), txns, &seeded)
			if err != nil {
				return fmt.Errorf("seed worldline transactions: %w", err)
			}
			if status != 201 {
				return fmt.Errorf("seed worldline transactions: want 201, got %d", status)
			}

			// 2. The morning cycle fires: file cut, published encrypted,
			// lump sum wired.
			var run cycleRunResp
			status, err = env.Lab.Post(ctx, Sim("worldline", "settlement-cycle/run?slot=morning&date="+settledDay), nil, &run)
			if err != nil {
				return fmt.Errorf("run morning settlement cycle: %w", err)
			}
			if status != 200 {
				return fmt.Errorf("run morning settlement cycle: want 200, got %d (errors: %v)", status, run.Errors)
			}
			// One file per currency, covering every submerchant that
			// traded -- the merchant is a row-level key inside it, not
			// part of the filename.
			var cut *cutFileResp
			for i := range run.Files {
				if run.Files[i].Currency != "EUR" {
					continue
				}
				for _, mid := range run.Files[i].MIDs {
					if mid == merchantIBAN {
						cut = &run.Files[i]
					}
				}
			}
			if cut == nil {
				return fmt.Errorf("morning cycle cut no EUR file covering %s (files: %+v)", merchantIBAN, run.Files)
			}
			if cut.AmountCents < 15000 {
				return fmt.Errorf("cut file settles %d, want at least the 15000 just seeded", cut.AmountCents)
			}
			if len(run.Published) == 0 {
				return fmt.Errorf("morning cycle published nothing (errors: %v)", run.Errors)
			}
			if worldline.FilenameSlot(cut.Filename) != worldline.SlotMorning {
				return fmt.Errorf("cut filename %q does not carry the morning slot code", cut.Filename)
			}

			// The lump sum is the other half of settlement: the file says
			// what is owed, the money has to actually land.
			var credited bool
			for _, ls := range run.LumpSums {
				if ls.Currency == "EUR" && ls.Status == "credited" {
					credited = true
				}
			}
			if !credited {
				return fmt.Errorf("no EUR lump sum was credited to the safeguarding account: %+v", run.LumpSums)
			}

			// 3. The e2e suite dials the acquirer's SFTP server itself and
			// re-parses the file. This is the independent check: it proves
			// the bytes are really out there, really encrypted, and really
			// say what the cycle claimed.
			published := cut.Filename + ".pgp"
			var decrypted []byte
			if err := PollUntil(ctx, 15*time.Second, 500*time.Millisecond, func() (bool, error) {
				var derr error
				decrypted, derr = downloadWorldlineFile(env, published)
				return derr == nil, derr
			}); err != nil {
				return fmt.Errorf("download+decrypt %s over real sftp+pgp: %w", published, err)
			}
			parsed, err := worldline.ParseBamboraCSV(strings.NewReader(string(decrypted)))
			if err != nil {
				return fmt.Errorf("parse %s: %w", published, err)
			}
			if parsed.Meta.SettlementAmount != cut.AmountCents {
				return fmt.Errorf("file downloaded over sftp settles %d, but the cycle reported %d",
					parsed.Meta.SettlementAmount, cut.AmountCents)
			}
			// The per-outlet split is what the platform pays against, and
			// the parts must add up to the lump sum that landed.
			payouts := parsed.PayoutsByMID()
			if payouts[merchantIBAN] < 15000 {
				return fmt.Errorf("per-MID total for %s is %d, want at least the 15000 seeded", merchantIBAN, payouts[merchantIBAN])
			}
			var sum int64
			for _, v := range payouts {
				sum += v
			}
			if sum != parsed.Meta.SettlementAmount {
				return fmt.Errorf("per-MID totals sum to %d but the file settles %d -- the lump sum would not reconcile",
					sum, parsed.Meta.SettlementAmount)
			}

			// 4. The platform takes delivery over the same channel and
			// turns the file into settlements and payouts.
			var pulled pulledFilesResp
			if err := PollUntil(ctx, 20*time.Second, 500*time.Millisecond, func() (bool, error) {
				if _, err := PostJSON(ctx, env.Client, env.SettlementURL+"/worldline/pull", nil, nil, &pulled); err != nil {
					return false, err
				}
				for _, f := range pulled.Files {
					if f.Name == published && f.Processed {
						return true, nil
					}
				}
				return false, fmt.Errorf("settlement has not yet processed %s", published)
			}); err != nil {
				return err
			}

			var recordID string
			for _, f := range pulled.Files {
				if f.Name != published {
					continue
				}
				if f.Error != "" {
					return fmt.Errorf("settlement failed on %s: %s", published, f.Error)
				}
				if f.PayoutsByMID < 1 {
					return fmt.Errorf("settlement found no per-MID payouts in %s", published)
				}
				if len(f.Settlements) == 0 {
					return fmt.Errorf("settlement created no records from %s", published)
				}
				recordID = f.Settlements[0]
			}
			if recordID == "" {
				return fmt.Errorf("settlement never reported taking delivery of %s", published)
			}

			var rep reportResp
			if _, err := GetJSON(ctx, env.Client, env.SettlementURL+"/reports/settlement/"+recordID, &rep); err != nil {
				return fmt.Errorf("get report %s: %w", recordID, err)
			}
			if rep.SettlementState != "Settled" {
				return fmt.Errorf("record %s: want state Settled, got %q", recordID, rep.SettlementState)
			}
			if rep.MerchantID != merchantIBAN {
				return fmt.Errorf("record %s: merchant %q, want the MID from the file (%s)", recordID, rep.MerchantID, merchantIBAN)
			}
			if rep.SettlementNetAmount <= 0 {
				return fmt.Errorf("record %s: settlement_net_amount = %d, want > 0", recordID, rep.SettlementNetAmount)
			}

			state.Set("settlement_id", recordID)
			return nil
		},
	}
}

// WorldlineConfirmationFile drives the afternoon slot: the same settlement
// is confirmed by a second file, which the platform must retain without
// paying anybody a second time.
//
// This is the scenario that would catch the expensive mistake. A
// confirmation file looks almost identical to the morning file, and a
// consumer that processes both pays every merchant twice.
func WorldlineConfirmationFile() Scenario {
	return Scenario{
		Name: "worldline-confirmation-file",
		Run: func(ctx context.Context, env *Env, state *State) error {
			if _, ok := state.Get("settlement_id"); !ok {
				return fmt.Errorf("no settlement_id in state (worldline-settlement-to-sftp must run first)")
			}
			settledDay := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

			var before pulledFilesResp
			if _, err := GetJSON(ctx, env.Client, env.SettlementURL+"/worldline/files", &before); err != nil {
				return fmt.Errorf("list pulled files: %w", err)
			}
			processedBefore := 0
			for _, f := range before.Files {
				if f.Processed {
					processedBefore++
				}
			}

			var run cycleRunResp
			status, err := env.Lab.Post(ctx, Sim("worldline", "settlement-cycle/run?slot=afternoon&date="+settledDay), nil, &run)
			if err != nil {
				return fmt.Errorf("run afternoon settlement cycle: %w", err)
			}
			if status != 200 {
				return fmt.Errorf("run afternoon settlement cycle: want 200, got %d (errors: %v)", status, run.Errors)
			}
			if len(run.Published) == 0 {
				return fmt.Errorf("afternoon cycle published nothing (errors: %v)", run.Errors)
			}
			// The confirmation slot must not move money a second time.
			if len(run.LumpSums) != 0 {
				return fmt.Errorf("afternoon cycle wired %d lump sum(s); the confirmation file settles nothing new", len(run.LumpSums))
			}
			confirmation := run.Published[0]

			var after pulledFilesResp
			if err := PollUntil(ctx, 20*time.Second, 500*time.Millisecond, func() (bool, error) {
				if _, err := PostJSON(ctx, env.Client, env.SettlementURL+"/worldline/pull", nil, nil, &after); err != nil {
					return false, err
				}
				for _, f := range after.Files {
					if f.Name == confirmation {
						return true, nil
					}
				}
				return false, fmt.Errorf("settlement has not yet taken delivery of %s", confirmation)
			}); err != nil {
				return err
			}

			processedAfter := 0
			for _, f := range after.Files {
				if f.Processed {
					processedAfter++
				}
				if f.Name != confirmation {
					continue
				}
				if f.Slot != worldline.SlotAfternoon {
					return fmt.Errorf("%s was read as slot %q, want the afternoon confirmation slot", confirmation, f.Slot)
				}
				if f.Processed {
					return fmt.Errorf("%s was processed; a confirmation file must be retained, not paid out again", confirmation)
				}
				if len(f.Settlements) != 0 {
					return fmt.Errorf("%s created %d settlement record(s); it must create none", confirmation, len(f.Settlements))
				}
			}
			if processedAfter != processedBefore {
				return fmt.Errorf("processed-file count went from %d to %d over an afternoon confirmation", processedBefore, processedAfter)
			}
			return nil
		},
	}
}
