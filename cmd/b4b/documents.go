package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Document uploads.
//
// What is simulated: the two-step shape. A client uploads a file, gets an
// id, and references that id from the extended profile -- and an extended
// profile that names an id nobody uploaded is refused. That is the whole
// mechanism a client has to get right, and it is exercised end to end here.
//
// What is not: verification, and storage. The bytes are read to measure and
// digest them and then discarded. This lab has no business holding identity
// documents, invented or otherwise, and a mock that accepted a document and
// then *judged* it would be pretending to a competence it does not have.
// Everything uploaded is accepted.

// maxUploadBytes caps what one upload may stream. It is not a vendor limit,
// it is this process refusing to be a memory sink.
const maxUploadBytes = 32 << 20

type uploadMetaReq struct {
	DocumentType string `json:"document_type"`
	FileName     string `json:"file_name"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
}

// uploadDocument accepts a file three ways, because a client may send any
// of them and none of them is worth a 400: a multipart form (the usual), a
// JSON body of pure metadata (a client that uploads elsewhere and only
// needs a handle), or a raw body of bytes with the filename in a header or
// query string.
func (a *app) uploadDocument(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var p b4b.DocumentParams

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			httputilx.Error(w, 400, "parse multipart upload: "+err.Error())
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			httputilx.Error(w, 400, `multipart upload needs a "file" part: `+err.Error())
			return
		}
		defer file.Close()
		size, digest, err := measure(file)
		if err != nil {
			httputilx.Error(w, 400, "read upload: "+err.Error())
			return
		}
		p = b4b.DocumentParams{
			DocumentType: r.FormValue("document_type"),
			FileName:     header.Filename,
			ContentType:  header.Header.Get("Content-Type"),
			Size:         size,
			SHA256:       digest,
		}
	case strings.HasPrefix(ct, "application/json"):
		var meta uploadMetaReq
		if _, err := httputilx.ReadJSONExtras(r, &meta); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		p = b4b.DocumentParams{
			DocumentType: meta.DocumentType,
			FileName:     meta.FileName,
			ContentType:  meta.ContentType,
			Size:         meta.Size,
		}
	default:
		size, digest, err := measure(r.Body)
		if err != nil {
			httputilx.Error(w, 400, "read upload: "+err.Error())
			return
		}
		p = b4b.DocumentParams{
			DocumentType: r.URL.Query().Get("document_type"),
			FileName:     firstNonEmpty(r.Header.Get("X-File-Name"), r.URL.Query().Get("file_name")),
			ContentType:  ct,
			Size:         size,
			SHA256:       digest,
		}
	}

	doc := a.dir.In(envOf(r)).AddDocument(p)
	log.Printf("b4b: upload %s accepted type=%q file=%q bytes=%d (content discarded)",
		doc.ID, doc.DocumentType, doc.FileName, doc.Size)
	httputilx.WriteJSON(w, 201, doc)
}

func (a *app) getDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := a.dir.In(envOf(r)).Document(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The receipt, never the file: there is no file. A client that expects
	// to download what it uploaded is relying on something this mock will
	// not do.
	httputilx.WriteJSON(w, 200, doc)
}

// measure streams r, returning its length and SHA-256 without keeping it.
func measure(r io.Reader) (int64, string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, maxUploadBytes))
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
