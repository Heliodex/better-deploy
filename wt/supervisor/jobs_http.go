package main

import (
	"net/http"
	"strings"
)

func writeJobActionError(w http.ResponseWriter, err error) {
	status := http.StatusConflict
	if strings.Contains(err.Error(), "not found") {
		status = http.StatusNotFound
	}

	writeError(w, status, err.Error())
}

// handleJobsCollection lists jobs or enqueues a new one.
func handleJobsCollection(w http.ResponseWriter, r *http.Request, store *jobStore) {
	switch r.Method {
	case http.MethodGet:
		status := r.URL.Query().Get("status")
		if status != "" && !validJobStatus(status) {
			writeError(w, http.StatusBadRequest, "unknown job status")
			return
		}

		writeJSON(w, http.StatusOK, jobsResponse{Jobs: store.list(status, 100)})
	case http.MethodPost:
		var req jobEnqueueRequest
		if !readJSONBody(w, r, jobMaxBodyBytes+maxConfigRequestBytes, &req) {
			return
		}

		job, created, err := store.enqueue(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		status := http.StatusOK
		if created {
			status = http.StatusCreated

			store.wakeIdle()
		}

		writeJSON(w, status, job)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleJobItem serves one job and its cancel or retry verbs.
func handleJobItem(w http.ResponseWriter, r *http.Request, store *jobStore, id, verb string) {
	switch {
	case verb == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		job, ok := store.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusOK, job)
	case verb == "cancel" && r.Method == http.MethodPost:
		job, err := store.cancel(id)
		if err != nil {
			writeJobActionError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, job)
	case verb == "retry" && r.Method == http.MethodPost:
		job, err := store.retry(id)
		if err != nil {
			writeJobActionError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, job)
	default:
		http.Error(w, "unknown job endpoint", http.StatusNotFound)
	}
}

// handleAppJobs serves the queue resource for one app.
func (s *supervisor) handleAppJobs(w http.ResponseWriter, r *http.Request, name, action string) {
	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}

	store := s.jobs.store(name)
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}

	if action == "jobs" {
		handleJobsCollection(w, r, store)
		return
	}

	rest := strings.TrimPrefix(action, "jobs/")

	id, verb, _ := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	handleJobItem(w, r, store, id, verb)
}
