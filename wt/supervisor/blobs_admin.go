package main

import (
	"net/http"
	"strings"
	"time"
)

type blobGCRequest struct {
	OlderThan string   `json:"olderThan"`
	Keep      []string `json:"keep,omitempty"`
	DryRun    bool     `json:"dryRun,omitempty"`
}

type blobGCResponse struct {
	Deleted   []string `json:"deleted"`
	DryRun    bool     `json:"dryRun"`
	OlderThan string   `json:"olderThan"`
}

// handleAppBlobs lists one app's stored blobs, including ones whose metadata sidecar is missing.
func (s *supervisor) handleAppBlobs(w http.ResponseWriter, r *http.Request, name string) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, errBlobStoreEmpty.Error())
		return
	}

	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	blobs, err := s.blobs.list(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"blobs": blobs})
}

// handleAppBlob deletes one blob by its digest.
func (s *supervisor) handleAppBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, errBlobStoreEmpty.Error())
		return
	}

	if !requireMethod(w, r, http.MethodDelete) {
		return
	}

	if err := s.blobs.remove(name, digest); err != nil {
		writeBlobError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAppBlobGC removes old blobs not protected by the keep list, or reports what a dry run would remove.
func (s *supervisor) handleAppBlobGC(w http.ResponseWriter, r *http.Request, name string) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, errBlobStoreEmpty.Error())
		return
	}

	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var request blobGCRequest
	if !readJSONBody(w, r, maxConfigRequestBytes, &request) {
		return
	}

	olderThan := strings.TrimSpace(request.OlderThan)

	duration, err := time.ParseDuration(olderThan)
	if err != nil || duration <= 0 {
		writeError(w, http.StatusBadRequest, "olderThan must be a positive duration such as 720h")
		return
	}

	keep := map[string]struct{}{}

	for _, digest := range request.Keep {
		digest = strings.TrimSpace(digest)
		if digest == "" {
			continue
		}

		if !validBlobDigest(digest) {
			writeError(w, http.StatusBadRequest, "keep contains an invalid digest")
			return
		}

		keep[digest] = struct{}{}
	}

	cutoff := time.Now().Add(-duration)
	deleted := []string{}

	if request.DryRun {
		blobs, err := s.blobs.list(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		for _, info := range blobs {
			if _, kept := keep[info.Digest]; kept || !info.CreatedAt.Before(cutoff) {
				continue
			}

			deleted = append(deleted, info.Digest)
		}
	} else {
		deleted, err = s.blobs.gc(name, cutoff, keep)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, blobGCResponse{Deleted: deleted, DryRun: request.DryRun, OlderThan: olderThan})
}
