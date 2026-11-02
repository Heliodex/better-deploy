package main

import "net/http"

// storageBlobDelete removes one of the authenticated app's blobs without exposing the admin API.
func (s *storageServer) storageBlobDelete(w http.ResponseWriter, r *http.Request, app, digest string) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, errBlobStoreEmpty.Error())
		return
	}
	if err := s.blobs.remove(app, digest); err != nil {
		writeBlobError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
