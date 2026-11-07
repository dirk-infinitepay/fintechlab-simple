package wlsftp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Publish writes one settlement file into the SFTP server's own
// directories, PGP-encrypted and signed, under filename+".pgp".
//
// This replaces the previous arrangement, where the platform wrote plain
// files into a bind mount shared with the SFTP server and a background
// poller mirrored them across. That made the platform the producer of a
// file it was meant to be receiving, and meant nothing ever exercised the
// download path. Here the acquirer writes into its own root and the
// platform's only route to the content is over the wire.
//
// The file lands in both real path conventions (download/ and
// to_WLNORDIC/): the same content is reachable under either, which is what
// a real Worldline SFT account exposes.
func Publish(root, filename string, plaintext []byte, keys *PGPKeys) (string, error) {
	if keys == nil {
		return "", fmt.Errorf("wlsftp: publish %s: no PGP keys configured", filename)
	}
	encrypted, err := keys.EncryptAndSign(plaintext)
	if err != nil {
		return "", fmt.Errorf("wlsftp: encrypt %s: %w", filename, err)
	}
	outName := PGPFilename(filename)
	for _, d := range []string{DirDownload, DirToWLNordic} {
		dir := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("wlsftp: create %s: %w", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, outName), encrypted, 0o644); err != nil {
			return "", fmt.Errorf("wlsftp: write %s: %w", filepath.Join(dir, outName), err)
		}
	}
	return outName, nil
}

// RemoveFiles deletes the regular files directly in root/dir whose name
// ours accepts, and returns how many it deleted. It is how a reset takes
// back what the acquirer wrote into a directory it shares with the
// platform: the test is the caller's proof of authorship, and anything it
// does not accept stays. It never deletes a directory, a symlink or
// anything below dir. A missing dir is nothing to delete; a file that
// disappears before it can be deleted (the platform took it) is not
// counted. Every other failure is returned, after the rest are tried.
func RemoveFiles(root, dir string, ours func(name string) bool) (int, error) {
	full := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("wlsftp: read %s: %w", full, err)
	}
	var n int
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || !ours(e.Name()) {
			continue
		}
		p := filepath.Join(full, e.Name())
		if err := os.Remove(p); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("wlsftp: remove %s: %w", p, err))
			}
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
