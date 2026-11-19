package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
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

// uploadPath is where an upload slot's presigned URL points: this
// listener, outside /oversight/v1 and its bearer token.
const uploadPath = "/direct-uploads/"

// uploadSlot is the answer to POST /oversight/v1/companies/{id}/uploads,
// as a client that uploads through a slot reads it: the id an extended
// profile names as a document, and the URL and headers to PUT the bytes
// with.
type uploadSlot struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// createUploadSlot opens a slot for one of the company's documents. The
// body is the file's metadata -- filename, byte_size, checksum (base64
// MD5), content_type -- and nothing is uploaded until the PUT. The URL
// names this listener as the caller reached it, path prefix included, so a
// client behind any base URL gets one it can reach.
func (a *app) createUploadSlot(w http.ResponseWriter, r *http.Request) {
	var p b4b.UploadSlotParams
	if _, err := httputilx.ReadJSONExtras(r, &p); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	doc, err := a.dir.In(envOf(r)).AddUploadSlot(r.PathValue("id"), p, b4b.RandomHex(16), maxUploadBytes)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("b4b: upload slot %s opened for company %s file=%q bytes=%d", doc.ID, doc.CompanyID, doc.FileName, doc.Size)
	httputilx.WriteJSON(w, 201, uploadSlot{
		ID:  doc.ID,
		URL: requestBase(r) + uploadPath + doc.UploadToken,
		Headers: map[string]string{
			"Content-Type": doc.ContentType,
			"Content-MD5":  doc.Checksum,
		},
	})
}

// receiveUpload is the presigned PUT. It answers 200 with an empty body,
// which is the only answer a slot client may take as success; 403 for a URL no
// slot has, as storage refuses a signature it cannot match; 400 when the bytes are not the size or MD5 the slot declared.
// The bytes are measured and discarded, as on POST /uploads.
func (a *app) receiveUpload(w http.ResponseWriter, r *http.Request) {
	h := md5.New()
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(h, digest), io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		httputilx.Error(w, 400, "read upload: "+err.Error())
		return
	}
	doc, err := a.dir.FillUpload(r.PathValue("token"), size,
		base64.StdEncoding.EncodeToString(h.Sum(nil)), hex.EncodeToString(digest.Sum(nil)))
	if err != nil {
		status := 400
		if errors.Is(err, b4b.ErrNotFound) {
			// What storage answers a presigned URL it cannot honour, and
			// what a slot client reads as a slot gone stale.
			status = 403
		}
		httputilx.Error(w, status, err.Error())
		return
	}
	log.Printf("b4b: upload %s received bytes=%d (content discarded)", doc.ID, doc.Size)
	w.WriteHeader(http.StatusOK)
}

// requestBase is the base URL the caller reached this listener at:
// scheme, host and path prefix, preferring what a proxy in front says. A
// proxy that serves b4b under a path (https://lab.example/b4b) and strips
// it on the way in names it in X-Forwarded-Prefix; without one there is no
// prefix, since this listener's routes sit at the root. Behind a chain of
// proxies the host and proto take their first entry, the client's, and the
// prefix joins every proxy's part.
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || httputilx.Forwarded(r, "X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := httputilx.Forwarded(r, "X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + httputilx.ForwardedPrefix(r)
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
