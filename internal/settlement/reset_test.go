package settlement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreClearEmptiesTheStoreAndItsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settlements.json")
	s := NewStore()
	s.Path = path
	for _, id := range []string{"set_a", "set_b"} {
		if err := s.Create(&SettlementRecord{ID: id, MerchantID: "M1", TransactionsDate: "2026-09-24"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Clear = %d, want 2", n)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("after Clear the store holds %d record(s)", len(got))
	}
	if got := s.Query("0000-00-00", "9999-99-99", "M1"); len(got) != 0 {
		t.Fatalf("after Clear a query still finds %d record(s)", len(got))
	}
	// A restart after a reset loads nothing, as on a first start.
	again := NewStore()
	if err := again.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := again.List(); len(got) != 0 {
		t.Fatalf("the state file still holds %d record(s)", len(got))
	}
}

func TestIsReportFilename(t *testing.T) {
	for name, want := range map[string]bool{
		"GB00SIM0000000000003_2026-09-24_EUR_WX.csv":         true,
		"GB00SIM0000000000003_2026-09-24_gbp_WX.json":        true,
		"GB00SIM0000000000003_2026-09-24_EUR_WX.xml":         true,
		"GB00SIM0000000000004_2026-09-24_EUR_WX.csv":         false, // another merchant's name
		"GB00SIM0000000000003_2026-09-24_EUR_WX.pdf":         false,
		"GB00SIM0000000000003_20260924_EUR_WX.csv":           false,
		"20260925083412_Worldline_Settlement_ER_EUR.csv.pgp": false,
	} {
		if got := IsReportFilename("GB00SIM0000000000003", name); got != want {
			t.Errorf("IsReportFilename(%q) = %v, want %v", name, got, want)
		}
	}
}

// The report folders are bind mounts shared with Worldline's SFT channel
// and the host. A reset deletes a file only when its name says the
// stand-in wrote it for the merchant whose folder it is in.
func TestRemoveReportsDeletesOnlyTheStandInsReports(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) string {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{
		write("out/M1/M1_2026-09-24_EUR_WX.csv"),
		write("out/M1/M1_2026-09-24_EUR_WX.xml"),
		write("out/acc_merchant/acc_merchant_2026-09-24_GBP_WX.json"),
		write("outbound/M1/daily/M1_2026-09-24_EUR_WX.csv"),
	}
	kept := []string{
		write("out/M1/M2_2026-09-24_EUR_WX.csv"),       // another merchant's name
		write("out/M1/README.md"),                      // not a report
		write("out/M1_2026-09-24_EUR_WX.csv"),          // not in a merchant folder
		write("out/M1/daily/M1_2026-09-24_EUR_WX.csv"), // below the folder it writes to
		write("outbound/M1/M1_2026-09-24_EUR_WX.csv"),  // outbound reports go in daily/
		write("outbound/M1/financial/M1_2026-09-24_EUR_WX.csv"),
		write("outbound/shared/daily/report.csv"),
	}

	out, err := RemoveReports(filepath.Join(root, "out"), "")
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := RemoveReports(filepath.Join(root, "outbound"), "daily")
	if err != nil {
		t.Fatal(err)
	}
	if out != 3 || outbound != 1 {
		t.Fatalf("removed %d from out and %d from outbound, want 3 and 1", out, outbound)
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", strings.TrimPrefix(p, root), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", strings.TrimPrefix(p, root), err)
		}
	}
	for _, d := range []string{"out/M1", "outbound/M1/daily"} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Errorf("folder %s was removed: %v", d, err)
		}
	}

	if n, err := RemoveReports(filepath.Join(root, "missing"), ""); n != 0 || err != nil {
		t.Fatalf("a missing root: %d, %v; want 0, nil", n, err)
	}
}

func TestRemoveFilesLeavesSymlinksAndFolders(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := RemoveFiles(dir, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1 (only the regular file)", n)
	}
	for _, p := range []string{filepath.Join(dir, "link"), filepath.Join(dir, "folder"), outside} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}
