package worldline

// What a reset needs from this package: emptying the transaction store, and
// telling the files this acquirer wrote from files somebody else put next to
// them. The SFTP root is shared with the platform — it uploads enrolment
// batches into to_WLNORDIC/ and may write anywhere in the jail — so a reset
// deletes only names it can attribute to itself, never a directory's whole
// contents (docs/lab-api/sim-control.md §6).

import (
	"os"
	"regexp"
	"strings"
)

// Clear forgets every acquired transaction and restarts the id sequence, so
// a scenario run again after a reset gets the same ids. It returns how many
// transactions there were.
func (s *Store) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.txns)
	s.txns, s.seq = nil, 0
	return n
}

// IsCutFilename reports whether name is one Cut gives a generated file
// under identifier — BamboraFilename's
// {YYYYMMDDHHMMSS}_{identifier}_{ER|AR}_{CCY}.csv — and nothing else.
func IsCutFilename(identifier, name string) bool {
	if identifier == "" {
		return false
	}
	return regexp.MustCompile(`^\d{14}_` + regexp.QuoteMeta(identifier) + `_(ER|AR)_[A-Z]+\.csv$`).MatchString(name)
}

// CutNames returns a test for the names a cycle under cfg gives its files:
// the generated pattern always, and in fixture mode also each fixture's own
// name, which cutFromFixtures keeps verbatim. A fixture is only a name here:
// the file in the fixture directory is never touched.
func (cfg CycleConfig) CutNames() (func(name string) bool, error) {
	fixtures := map[string]bool{}
	if cfg.Source == FileSourceFixture && cfg.FixtureDir != "" {
		entries, err := os.ReadDir(cfg.FixtureDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && (strings.Contains(e.Name(), "_"+SlotMorning+"_") || strings.Contains(e.Name(), "_"+SlotAfternoon+"_")) {
				fixtures[e.Name()] = true
			}
		}
	}
	return func(name string) bool {
		return fixtures[name] || IsCutFilename(cfg.Identifier, name)
	}, nil
}

// receiptFilename is ReceiptFilename's shape, and the .partial the desk
// writes first and renames.
var receiptFilename = regexp.MustCompile(`^INFINITEENROLL_\d{2}\.\d{8}\.ack\.pgp(\.partial)?$`)

// IsReceiptFilename reports whether name is an enrolment receipt the
// boarding desk writes into from_WLNORDIC/, or the temporary file it writes
// one through.
func IsReceiptFilename(name string) bool {
	return receiptFilename.MatchString(name)
}
