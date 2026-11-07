package settlement

// What a reset needs from this package: emptying the record store, and
// deleting the report files the stand-in wrote — only those. The report
// directories are bind mounts shared with Worldline's SFT channel and the
// host, so a file is deleted only when its name and where it sits prove the
// stand-in wrote it (docs/lab-api/sim-control.md §6).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Clear forgets every record and, when the store is persisted, rewrites
// its file empty — as on a first start, which loads nothing. It returns how
// many records there were.
func (s *Store) Clear() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.records)
	s.records = map[string]*SettlementRecord{}
	s.byMerchant = map[string][]string{}
	s.byDate = map[string][]string{}
	if s.Path == "" {
		return n, nil
	}
	if err := s.saveToLocked(s.Path); err != nil {
		return n, fmt.Errorf("settlement: save %s: %w", s.Path, err)
	}
	return n, nil
}

// IsReportFilename reports whether name is the report file the stand-in
// writes for merchantID: {merchant_id}_{YYYY-MM-DD}_{CCY}_WX.{csv|xml|json},
// with this merchant's id. A file in a merchant's folder named for another
// merchant was not written there by the stand-in.
func IsReportFilename(merchantID, name string) bool {
	if merchantID == "" {
		return false
	}
	return regexp.MustCompile(`^` + regexp.QuoteMeta(merchantID) + `_\d{4}-\d{2}-\d{2}_[A-Za-z]{3}_WX\.(csv|xml|json)$`).MatchString(name)
}

// RemoveReports deletes the report files under root/{merchant_id}/sub — sub
// is "" for SFTP_OUT_DIR's layout and "daily" for SFTP_OUTBOUND_DIR's —
// whose name IsReportFilename accepts for that merchant. It never deletes a
// folder, a symlink or a file of any other name. It returns how many files
// it deleted; every failure is returned, after the rest are tried.
func RemoveReports(root, sub string) (int, error) {
	if root == "" {
		return 0, nil
	}
	merchants, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int
	var errs []error
	for _, m := range merchants {
		if !m.IsDir() || strings.HasPrefix(m.Name(), ".") {
			continue
		}
		mid := m.Name()
		n, err := RemoveFiles(filepath.Join(root, mid, sub), func(name string) bool { return IsReportFilename(mid, name) })
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// RemoveFiles deletes the regular files directly in dir whose name ours
// accepts, and returns how many it deleted. ours is the caller's proof that
// it wrote the file; anything it does not accept stays. A missing dir is
// nothing to delete, and a file that is gone before it can be deleted is
// not counted. Every other failure is returned, after the rest are tried.
func RemoveFiles(dir string, ours func(name string) bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var n int
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || !ours(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
