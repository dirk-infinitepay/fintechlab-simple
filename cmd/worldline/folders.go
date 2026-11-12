package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
)

// The environment's SFTP folders as its login sees them now: what Worldline
// has left there and what the platform has put there, so the card can show
// where a file is without anyone logging in. Read straight from the
// environment's jail; nothing is changed.

// sftpFolderList is the folders a jail has, in the order a platform meets
// them, and what each one holds.
var sftpFolderList = []struct{ path, holds string }{
	{wlsftp.DirDownload, "settlement files: what a platform's poll lists and downloads"},
	{wlsftp.DirToWLNordic, "a second copy of every settlement file, and the platform's enrolment batches"},
	{wlsftp.DirFromWLNordic, "enrolment receipts"},
	{wlsftp.DirDisputesDownload, "dispute files (never written)"},
}

type sftpEntry struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

type sftpFolder struct {
	Path  string      `json:"path"`
	Holds string      `json:"holds"`
	Files []sftpEntry `json:"files"`
	Error string      `json:"error,omitempty"`
}

// sftpFolders answers GET /sim/sftp: each folder of the environment's jail
// and its files, newest first. A folder that is not there yet is empty.
func (s *sim) sftpFolders(w http.ResponseWriter, r *http.Request) {
	e, ok := s.one(w, r)
	if !ok {
		return
	}
	folders := make([]sftpFolder, 0, len(sftpFolderList))
	for _, d := range sftpFolderList {
		f := sftpFolder{Path: d.path, Holds: d.holds, Files: []sftpEntry{}}
		entries, err := os.ReadDir(filepath.Join(e.acq.root, filepath.FromSlash(d.path)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			f.Error = err.Error()
		}
		for _, de := range entries {
			if !de.Type().IsRegular() {
				continue
			}
			info, err := de.Info()
			if err != nil {
				continue
			}
			f.Files = append(f.Files, sftpEntry{Name: de.Name(), Bytes: info.Size(), Modified: info.ModTime().UTC()})
		}
		sort.SliceStable(f.Files, func(i, j int) bool { return f.Files[i].Modified.After(f.Files[j].Modified) })
		folders = append(folders, f)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"env": tenant.Normalize(e.env), "folders": folders})
}
