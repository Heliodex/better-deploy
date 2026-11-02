package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wt.packages/app"
	"wt.packages/storage"
)

const (
	maxBody     = 8 << 10
	maxFileSize = 8 << 20
	maxName     = 100
)

type fileRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	CreatedAt   string `json:"createdAt"`
	DownloadURL string `json:"downloadUrl"`
}

type filesResponse struct {
	Files []fileRecord `json:"files"`
}

type uploadRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type uploadResponse struct {
	UploadURL string `json:"uploadUrl"`
	Max       int64  `json:"max"`
}

type createFileRequest struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// blobApp owns the store and routes for the blob box.
type blobApp struct {
	store  *storage.Client
	client *http.Client
	mux    *http.ServeMux
}

func (a *blobApp) migrate(ctx context.Context) error {
	return a.store.Migrate(ctx, 1, storage.Statement{SQL: `create table if not exists files (
		id text primary key,
		name text not null,
		digest text not null,
		size integer not null,
		content_type text not null,
		created_at text not null
	)`})
}

// validateFile checks the fields shared by the upload and metadata requests.
func validateFile(name, digest, contentType string, size int64) (string, string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxName {
		return "", "", "", fmt.Errorf("name must be between 1 and %d characters", maxName)
	}
	if size <= 0 || size > maxFileSize {
		return "", "", "", fmt.Errorf("size must be between 1 and %d bytes", maxFileSize)
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 100 {
		return "", "", "", fmt.Errorf("contentType is too long")
	}
	if digest != "" {
		if len(digest) != 64 {
			return "", "", "", fmt.Errorf("digest must be 64 hex characters")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return "", "", "", fmt.Errorf("digest must be 64 hex characters")
		}
	}
	return name, digest, contentType, nil
}

func (a *blobApp) record(item fileRecord) (fileRecord, error) {
	link, err := app.BlobDownloadURL(item.Digest, time.Now().Add(time.Hour))
	if err != nil {
		return fileRecord{}, err
	}
	item.DownloadURL = link
	return item, nil
}

func (a *blobApp) handleListFiles(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.Query(r.Context(), `select id, name, digest, size, content_type, created_at from files order by created_at desc`)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	files := []fileRecord{}
	for rows.Next() {
		var item fileRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.Digest, &item.Size, &item.ContentType, &item.CreatedAt); err != nil {
			app.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		item, err = a.record(item)
		if err != nil {
			app.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		files = append(files, item)
	}
	if err := rows.Err(); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, filesResponse{Files: files})
}

func (a *blobApp) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	var request uploadRequest
	if !app.ReadJSON(w, r, maxBody, "name", &request) {
		return
	}
	_, _, contentType, err := validateFile(request.Name, "", request.ContentType, request.Size)
	if err != nil {
		app.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	uploadURL, err := app.BlobUploadURL(request.Size, contentType, 10*time.Minute)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, uploadResponse{UploadURL: uploadURL, Max: request.Size})
}

func (a *blobApp) handleCreateFile(w http.ResponseWriter, r *http.Request) {
	var request createFileRequest
	if !app.ReadJSON(w, r, maxBody, "name", &request) {
		return
	}
	name, digest, contentType, err := validateFile(request.Name, request.Digest, request.ContentType, request.Size)
	if err != nil {
		app.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	item := fileRecord{
		ID:          app.NewID(),
		Name:        name,
		Digest:      digest,
		Size:        request.Size,
		ContentType: contentType,
		CreatedAt:   time.Now().UTC().Format(app.TimestampFormat),
	}
	if _, err := a.store.Exec(r.Context(), `insert into files (id, name, digest, size, content_type, created_at) values (?, ?, ?, ?, ?, ?)`,
		item.ID, item.Name, item.Digest, item.Size, item.ContentType, item.CreatedAt); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err = a.record(item)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusCreated, item)
}

func (a *blobApp) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var digest string
	err := a.store.QueryRow(r.Context(), `select digest from files where id = ?`, id).Scan(&digest)
	if errors.Is(err, storage.ErrNoRows) {
		app.WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := a.store.Exec(r.Context(), `delete from files where id = ?`, id); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The row is gone; a failed blob delete leaves an orphan for the admin GC, not a broken reference.
	if err := app.DeleteBlob(r.Context(), a.client, digest); err != nil {
		app.Errorf("blob delete %s: %v", digest, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// newBlobApp builds the app around one store and registers its routes.
func newBlobApp(store *storage.Client, client *http.Client) *blobApp {
	if client == nil {
		client = http.DefaultClient
	}
	a := &blobApp{store: store, client: client, mux: http.NewServeMux()}
	a.mux.HandleFunc("GET /api/files", a.handleListFiles)
	a.mux.HandleFunc("POST /api/uploads", a.handleCreateUpload)
	a.mux.HandleFunc("POST /api/files", a.handleCreateFile)
	a.mux.HandleFunc("DELETE /api/files/{id}", a.handleDeleteFile)
	return a
}

func (a *blobApp) handler() http.Handler {
	return a.mux
}

func main() {}
