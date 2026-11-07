package sftpgateway

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The archive is a bind mount the host can write to. A reset deletes only
// what the download route archived: WX reports in archive/{YYYY}/{MM}/.
func TestClearArchiveDeletesOnlyArchivedReports(t *testing.T) {
	base := t.TempDir()
	c := &Channel{BaseDir: base}
	write := func(rel string) string {
		t.Helper()
		p := filepath.Join(base, "archive", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{
		write("2026/09/GB00SIM0000000000003_2026-09-24_EUR_WX.csv"),
		write("2026/10/acc_merchant_2026-10-01_GBP_WX.json"),
	}
	kept := []string{
		write("2026/09/financial-report.pdf"),                       // put there by hand, then downloaded
		write("2026/GB00SIM0000000000003_2026-09-24_EUR_WX.csv"),    // not a month folder
		write("misc/09/GB00SIM0000000000003_2026-09-24_EUR_WX.csv"), // not a year folder
	}

	n, err := c.ClearArchive()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(gone) {
		t.Fatalf("ClearArchive = %d, want %d", n, len(gone))
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", p, err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "archive", "2026", "09")); err != nil {
		t.Errorf("a month folder was removed: %v", err)
	}

	if n, err := (&Channel{BaseDir: t.TempDir()}).ClearArchive(); n != 0 || err != nil {
		t.Fatalf("no archive at all: %d, %v; want 0, nil", n, err)
	}
}

// Writing the seed back must be no change to the file tracked in the repo,
// byte for byte, or a reset of everything leaves a diff in the checkout.
func TestDefaultConfigIsTheSeededFile(t *testing.T) {
	seeded, err := os.ReadFile(filepath.Join("..", "..", "sftp", "config", "merchant-onboarding.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Channel{BaseDir: t.TempDir()}
	if err := c.UpdateConfig(DefaultConfig("/sftp", "GB00SIM0000000000003")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(c.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seeded) {
		t.Fatalf("the seed written back differs from sftp/config/merchant-onboarding.json:\n%s\nwant:\n%s", got, seeded)
	}
}
