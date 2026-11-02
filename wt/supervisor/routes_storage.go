package main

import "net/http"

// storageRoutes returns the cached table for the storage API.
func (s *storageServer) storageRoutes() *routeTable {
	return s.routesCache.get(s.buildStorageRoutes)
}

// storageApp returns the authenticated app name placed in the route parameters.
func storageApp(params map[string]string) string {
	return params["app"]
}

// withStorageApp adapts a handler that takes the authenticated app name.
func withStorageApp(fn func(http.ResponseWriter, *http.Request, string)) routeHandler {
	return func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		fn(w, r, storageApp(params))
	}
}

// withStorageID adapts a handler that takes the authenticated app name and one path parameter.
func withStorageID(fn func(http.ResponseWriter, *http.Request, string, string)) routeHandler {
	return func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		fn(w, r, storageApp(params), params["id"])
	}
}

// buildStorageRoutes declares the storage API once with its methods and path parameters.
func (s *storageServer) buildStorageRoutes() *routeTable {
	rt := newRouteTable()
	rt.add(http.MethodPost, "/v1/publish", withStorageApp(func(w http.ResponseWriter, r *http.Request, app string) {
		handleEventPublish(s.bus, w, r, app)
	}))
	rt.add(http.MethodDelete, "/v1/blobs/{id}", withStorageID(func(w http.ResponseWriter, r *http.Request, app, digest string) {
		s.storageBlobDelete(w, r, app, digest)
	}))
	rt.add(http.MethodGet, "/v1/jobs", withStorageApp(s.storageJobs))
	rt.add(http.MethodPost, "/v1/jobs", withStorageApp(s.storageJobs))
	rt.add(http.MethodGet, "/v1/jobs/{id}", withStorageID(func(w http.ResponseWriter, r *http.Request, app, id string) {
		s.storageJob(w, r, app, id, "")
	}))
	rt.add(http.MethodPost, "/v1/jobs/{id}/cancel", withStorageID(func(w http.ResponseWriter, r *http.Request, app, id string) {
		s.storageJob(w, r, app, id, "cancel")
	}))
	rt.add(http.MethodPost, "/v1/jobs/{id}/retry", withStorageID(func(w http.ResponseWriter, r *http.Request, app, id string) {
		s.storageJob(w, r, app, id, "retry")
	}))
	rt.add(http.MethodPost, "/v1/sessions", withStorageApp(s.createSession))
	rt.add(http.MethodPost, "/v1/sessions/{id}/query", withStorageID(s.sessionQuery))
	rt.add(http.MethodPost, "/v1/sessions/{id}/exec", withStorageID(s.sessionExec))
	rt.add(http.MethodPost, "/v1/sessions/{id}/commit", withStorageID(func(w http.ResponseWriter, r *http.Request, app, id string) {
		s.closeSessionRequest(w, r, app, id, true)
	}))
	rt.add(http.MethodPost, "/v1/sessions/{id}/rollback", withStorageID(func(w http.ResponseWriter, r *http.Request, app, id string) {
		s.closeSessionRequest(w, r, app, id, false)
	}))
	rt.add(http.MethodPost, "/v1/query", withStorageApp(s.query))
	rt.add(http.MethodPost, "/v1/exec", withStorageApp(s.exec))
	rt.add(http.MethodPost, "/v1/write", withStorageApp(s.writeReturning))
	rt.add(http.MethodPost, "/v1/batch", withStorageApp(s.batch))
	rt.add(http.MethodPost, "/v1/transaction", withStorageApp(s.transaction))
	rt.add(http.MethodGet, "/v1/schema", withStorageApp(s.readSchema))
	rt.add(http.MethodPost, "/v1/migrate", withStorageApp(s.runMigration))
	return rt
}
