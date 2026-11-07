// Package sftpgateway simulates the Worldline SFTP file exchange channel: a
// file-based stand-in for the real SFT (Secure File Transfer) directories
// Worldline provisions per merchant during technical onboarding. Onboarding
// documents and settlement correction files flow in; daily WX files and
// financial reports flow out; retrieved outbound files are archived by
// year/month so the outbound directory doesn't accumulate forever. See
// docs/ARCHITECTURE-worldline-sftp-channel.md.
package sftpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

// Inbound categories accepted by DropInbound and ListInbound.
const (
	CategoryOnboarding  = "onboarding"
	CategoryCorrections = "corrections"
)

// Outbound report categories served under a merchant's outbound directory.
const (
	CategoryDaily     = "daily"
	CategoryFinancial = "financial"
)

// MaxInboundFileSize caps a single inbound upload. This is a lab, not a
// production file-transfer service: the limit exists to reject an obviously
// wrong payload, not to model a real vendor quota.
const MaxInboundFileSize = 10 << 20 // 10MiB

// inboundExtensions lists the document types Worldline's onboarding channel
// actually accepts per category (docs/ARCHITECTURE-worldline-sftp-channel.md
// §4 "Supported onboarding document types" and §1 directory tree).
var inboundExtensions = map[string]map[string]bool{
	CategoryOnboarding:  {".pdf": true, ".csv": true, ".xml": true},
	CategoryCorrections: {".csv": true, ".xml": true},
}

// Channel represents the SFTP file exchange channel.
type Channel struct {
	BaseDir string // /sftp
}

// Config is the channel configuration persisted at
// {BaseDir}/config/merchant-onboarding.json.
type Config struct {
	MerchantID          string   `json:"merchant_id"`
	SFTPDirectory       string   `json:"sftp_directory"`
	ReportTypes         []string `json:"report_types"`         // ["WX", "Financial"]
	ReportFormats       []string `json:"report_formats"`       // ["CSV", "XML", "ASCII"]
	CutOffTime          string   `json:"cut_off_time"`         // "00:00 CET"
	RemittanceFrequency string   `json:"remittance_frequency"` // "daily" | "weekly"
	SFTPDirectoryCount  int      `json:"sftp_directory_count"` // max 3
}

func (c *Channel) configPath() string {
	return filepath.Join(c.BaseDir, "config", "merchant-onboarding.json")
}

// DefaultConfig is the configuration the channel is seeded with, and what
// a reset of everything puts back: the same shape as the
// sftp/config/merchant-onboarding.json tracked at the repo root.
func DefaultConfig(baseDir, merchantID string) *Config {
	return &Config{
		MerchantID:          merchantID,
		SFTPDirectory:       filepath.Join(baseDir, "outbound", merchantID),
		ReportTypes:         []string{"WX", "Financial"},
		ReportFormats:       []string{"CSV", "XML", "ASCII"},
		CutOffTime:          "00:00 CET",
		RemittanceFrequency: "daily",
		SFTPDirectoryCount:  3,
	}
}

// validMerchantID rejects empty, ".", "..", and any merchant id containing
// a path separator. merchantID is later joined as a single path component
// (filepath.Join(..., category, merchantID)); a caller-controlled value
// like "../../../etc" — reachable via a %2f-encoded URL path segment,
// which bypasses net/http's built-in ".." redirect entirely — must never
// be trusted to stay within one directory level.
func validMerchantID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("sftpgateway: invalid merchant id %q", id)
	}
	return nil
}

// DropInbound places a file in the onboarding or corrections inbound
// directory. category must be "onboarding" or "corrections"; filename is
// validated against the category's supported extensions and content is
// capped at MaxInboundFileSize.
func (c *Channel) DropInbound(category, merchantID string, filename string, content []byte) (path string, err error) {
	allowed, ok := inboundExtensions[category]
	if !ok {
		return "", fmt.Errorf("sftpgateway: invalid inbound category %q", category)
	}
	if err := validMerchantID(merchantID); err != nil {
		return "", err
	}
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		return "", fmt.Errorf("sftpgateway: invalid filename %q", filename)
	}
	if ext := strings.ToLower(filepath.Ext(filename)); !allowed[ext] {
		return "", fmt.Errorf("sftpgateway: unsupported extension %q for category %q", ext, category)
	}
	if len(content) > MaxInboundFileSize {
		return "", fmt.Errorf("sftpgateway: file too large (%d bytes, max %d)", len(content), MaxInboundFileSize)
	}
	dir := filepath.Join(c.BaseDir, "inbound", category, merchantID)
	if err := os.MkdirAll(dir, 0o777); err != nil { // world-writable: see internal/sftp/staging.go's Stage() for why
		return "", err
	}
	path = filepath.Join(dir, filename)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ListInbound lists inbound files for a category and merchant.
func (c *Channel) ListInbound(category, merchantID string) ([]os.FileInfo, error) {
	if _, ok := inboundExtensions[category]; !ok {
		return nil, fmt.Errorf("sftpgateway: invalid inbound category %q", category)
	}
	if err := validMerchantID(merchantID); err != nil {
		return nil, err
	}
	return listDir(filepath.Join(c.BaseDir, "inbound", category, merchantID))
}

// ListOutbound lists settlement files available for a merchant, across both
// the daily (WX) and financial report directories.
func (c *Channel) ListOutbound(merchantID string) ([]os.FileInfo, error) {
	if err := validMerchantID(merchantID); err != nil {
		return nil, err
	}
	var all []os.FileInfo
	for _, category := range []string{CategoryDaily, CategoryFinancial} {
		infos, err := c.ListOutboundCategory(merchantID, category)
		if err != nil {
			return nil, err
		}
		all = append(all, infos...)
	}
	return all, nil
}

// ListOutboundCategory lists outbound files for a merchant scoped to one
// report category ("daily" or "financial"), backing the
// /outbound/{merchant_id}/daily and /outbound/{merchant_id}/financial
// endpoints.
func (c *Channel) ListOutboundCategory(merchantID, category string) ([]os.FileInfo, error) {
	if err := validMerchantID(merchantID); err != nil {
		return nil, err
	}
	if category != CategoryDaily && category != CategoryFinancial {
		return nil, fmt.Errorf("sftpgateway: invalid outbound category %q", category)
	}
	return listDir(filepath.Join(c.BaseDir, "outbound", merchantID, category))
}

// ReadOutbound reads a specific outbound file for a merchant. relativePath
// is relative to the merchant's outbound directory and must begin with
// "daily/" or "financial/", e.g.
// "daily/GB00SIM0000000000003_2026-09-03_EUR_WX.csv".
func (c *Channel) ReadOutbound(merchantID, relativePath string) ([]byte, error) {
	path, err := c.resolveOutbound(merchantID, relativePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// ArchiveOutbound moves a retrieved outbound file into
// {BaseDir}/archive/{year}/{month} (year/month from the current time),
// matching the Worldline file lifecycle: retrieval archives the file so a
// merchant doesn't re-download the same settlement file twice.
func (c *Channel) ArchiveOutbound(merchantID, relativePath string) (string, error) {
	src, err := c.resolveOutbound(merchantID, relativePath)
	if err != nil {
		return "", err
	}
	now := labclock.Now()
	dir := filepath.Join(c.BaseDir, "archive", fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", now.Month()))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.Base(src))
	if err := moveFile(src, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// moveFile relocates src to dest. outbound and archive are typically
// separate bind-mounted volumes (distinct devices inside the container),
// so a plain os.Rename fails with "invalid cross-device link"; fall back to
// copy-then-remove whenever that happens.
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, data, info.Mode()); err != nil {
		return err
	}
	return os.Remove(src)
}

// ListArchive lists files archived for a given year and month
// ({BaseDir}/archive/{year}/{month}).
func (c *Channel) ListArchive(year, month string) ([]os.FileInfo, error) {
	return listDir(filepath.Join(c.BaseDir, "archive", year, month))
}

// GetConfig returns the channel configuration.
func (c *Channel) GetConfig() (*Config, error) {
	data, err := os.ReadFile(c.configPath())
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// UpdateConfig updates the channel configuration, creating the config
// directory if it does not already exist.
func (c *Channel) UpdateConfig(cfg *Config) error {
	if err := os.MkdirAll(filepath.Join(c.BaseDir, "config"), 0o777); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.configPath(), data, 0o644)
}

// archivedReport is the name of a report the download route archives: the
// WX report the settlement stand-in writes into outbound/{merchant_id}/daily/,
// {merchant_id}_{YYYY-MM-DD}_{CCY}_WX.{csv|xml|json}.
var archivedReport = regexp.MustCompile(`^[^/\\]+_\d{4}-\d{2}-\d{2}_[A-Za-z]{3}_WX\.(csv|xml|json)$`)

// archiveYear and archiveMonth are the folders ArchiveOutbound makes.
var archiveYear, archiveMonth = regexp.MustCompile(`^\d{4}$`), regexp.MustCompile(`^\d{2}$`)

// ClearArchive deletes what the download route archived: the regular files
// in archive/{YYYY}/{MM}/ named like a WX report. The archive is a bind
// mount the host can write to, so anything else there — a file of another
// name, a folder of another shape — is left, and so are the folders
// themselves. It returns how many files it deleted; every failure is
// returned, after the rest are tried.
func (c *Channel) ClearArchive() (int, error) {
	root := filepath.Join(c.BaseDir, "archive")
	years, err := readDirs(root, archiveYear)
	if err != nil {
		return 0, err
	}
	var n int
	var errs []error
	for _, year := range years {
		months, err := readDirs(year, archiveMonth)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, month := range months {
			entries, err := os.ReadDir(month)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, e := range entries {
				if !e.Type().IsRegular() || !archivedReport.MatchString(e.Name()) {
					continue
				}
				if err := os.Remove(filepath.Join(month, e.Name())); err != nil {
					if !os.IsNotExist(err) {
						errs = append(errs, err)
					}
					continue
				}
				n++
			}
		}
	}
	return n, errors.Join(errs...)
}

// readDirs lists the real directories (not symlinks) in dir whose name
// matches, as paths. A missing dir has none.
func readDirs(dir string, name *regexp.Regexp) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && name.MatchString(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// resolveOutbound joins and validates relativePath against merchantID's
// outbound directory: it must resolve to exactly the daily or financial
// subdirectory and must not escape it (no "..").
func (c *Channel) resolveOutbound(merchantID, relativePath string) (string, error) {
	if err := validMerchantID(merchantID); err != nil {
		return "", err
	}
	clean := strings.TrimPrefix(filepath.Clean(string(filepath.Separator)+relativePath), string(filepath.Separator))
	category := clean
	if i := strings.IndexRune(clean, filepath.Separator); i >= 0 {
		category = clean[:i]
	}
	if category != CategoryDaily && category != CategoryFinancial {
		return "", fmt.Errorf("sftpgateway: invalid outbound category %q", category)
	}
	root := filepath.Join(c.BaseDir, "outbound", merchantID)
	path := filepath.Join(root, clean)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("sftpgateway: invalid path %q", relativePath)
	}
	return path, nil
}

// listDir lists regular files in dir, returning an empty (not nil) slice
// and no error if the directory does not exist yet — a merchant or report
// category with nothing delivered yet is a normal, unremarkable state.
func listDir(dir string) ([]os.FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []os.FileInfo{}, nil
		}
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}
