package main

import "net/http"

// storageJobs serves an app's job queue through the authenticated storage loopback service, so a component does not need the admin API.
func (s *storageServer) storageJobs(w http.ResponseWriter, r *http.Request, app string) {
	if s.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	store := s.jobs.store(app)
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	handleJobsCollection(w, r, store)
}

// storageJob serves one job and its cancel or retry verb through the storage loopback service.
func (s *storageServer) storageJob(w http.ResponseWriter, r *http.Request, app, id, verb string) {
	if s.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	store := s.jobs.store(app)
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	handleJobItem(w, r, store, id, verb)
}
