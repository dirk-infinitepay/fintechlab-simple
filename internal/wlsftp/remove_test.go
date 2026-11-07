package wlsftp

import (
	"os"
	"path/filepath"
	"testing"
)

// RemoveFiles is how a reset takes back what the acquirer published into a
// directory the platform writes to as well. Only a regular file the test
// accepts goes: not the platform's file beside it, not a directory or a
// symlink of an accepted name, not anything below.
func TestRemoveFilesDeletesOnlyAcceptedRegularFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, DirToWLNordic)
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "ours-1.pgp"))
	write(filepath.Join(dir, "ours-2.pgp"))
	write(filepath.Join(dir, "INFINITEENROLL_01.20260925.pgp")) // the platform's
	write(filepath.Join(dir, "ours-dir.pgp", "ours-3.pgp"))     // a folder of an accepted name, and below it
	target := filepath.Join(outside, "ours-4.pgp")
	write(target)
	if err := os.Symlink(target, filepath.Join(dir, "ours-4.pgp")); err != nil {
		t.Fatal(err)
	}

	ours := func(name string) bool { return len(name) > 5 && name[:5] == "ours-" }
	n, err := RemoveFiles(root, DirToWLNordic, ours)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	for _, gone := range []string{"ours-1.pgp", "ours-2.pgp"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", gone, err)
		}
	}
	for _, kept := range []string{
		filepath.Join(dir, "INFINITEENROLL_01.20260925.pgp"),
		filepath.Join(dir, "ours-dir.pgp", "ours-3.pgp"),
		filepath.Join(dir, "ours-4.pgp"),
		target,
	} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}

	if n, err := RemoveFiles(root, "no/such/dir", ours); n != 0 || err != nil {
		t.Fatalf("a missing directory: %d, %v; want 0, nil", n, err)
	}
}
