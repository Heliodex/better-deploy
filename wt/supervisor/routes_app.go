package main

import (
	"net/http"
)

// appRoutes returns the cached table that dispatches /apps/<name>/<action> requests.
func (s *supervisor) appRoutes() *routeTable {
	return s.appRoutesCache.get(s.buildAppRoutes)
}

// actionWithPrefix returns the parent action when rest is empty and parent/rest otherwise.
func actionWithPrefix(prefix string) func(map[string]string) string {
	return func(params map[string]string) string {
		if params["rest"] == "" {
			return prefix
		}
		return prefix + "/" + params["rest"]
	}
}

// handleAppRootRoute adapts the app root handler to the route table.
func (s *supervisor) handleAppRootRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppRoot(w, r, params["name"])
}

// handleAppConfigRoute adapts the config handler to the route table.
func (s *supervisor) handleAppConfigRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppConfig(w, r, params["name"])
}

// handleMaintenanceRoute adapts the maintenance handler to the route table.
func (s *supervisor) handleMaintenanceRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppMaintenance(w, r, params["name"])
}

// handleSecretsRoute adapts the secrets handler to the route table.
func (s *supervisor) handleSecretsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppSecrets(w, r, params["name"], params["key"])
}

// handleJobsRoute adapts the job handler to the route table.
func (s *supervisor) handleJobsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	action := "jobs"
	if params["rest"] != "" {
		action += "/" + params["rest"]
	}
	s.handleAppJobs(w, r, params["name"], action)
}

// handleBackupsRoute adapts the backup handler to the route table.
func (s *supervisor) handleBackupsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	rest := ""
	if params["rest"] != "" {
		rest = "/" + params["rest"]
	}
	s.handleBackupRoute(w, r, params["name"], rest)
}

// handleDeploymentsRoute adapts the deployment collection handler to the route table.
func (s *supervisor) handleDeploymentsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	if r.Method == http.MethodPost {
		s.handleDeploymentCreate(w, r, params["name"])
		return
	}
	s.handleDeployments(w, r, params["name"])
}

// handleDeploymentItemRoute adapts the deployment item handler to the route table.
func (s *supervisor) handleDeploymentItemRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleDeploymentRoute(w, r, params["name"], params["rest"])
}

// handleDatabaseRoute adapts the database handler to the route table.
func (s *supervisor) handleDatabaseRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	action := "database"
	if params["rest"] != "" {
		action += "/" + params["rest"]
	}
	s.handleAppDatabase(w, r, params["name"], action)
}

// handleLogsRoute adapts the log tail and stream handlers to the route table.
func (s *supervisor) handleLogsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppLogs(w, r, params["name"], "logs")
}

// handleLogStreamRoute adapts the live log handler to the route table.
func (s *supervisor) handleLogStreamRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppLogs(w, r, params["name"], "logs/stream")
}

// handleLogExportRoute adapts the log export handler to the route table.
func (s *supervisor) handleLogExportRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	a, ok := s.requireAppMethod(w, r, params["name"], http.MethodGet)
	if !ok {
		return
	}
	s.handleLogExport(w, r, a)
}

// handleResourcesRoute adapts the resource history handler to the route table.
func (s *supervisor) handleResourcesRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	a, ok := s.requireAppMethod(w, r, params["name"], http.MethodGet)
	if !ok {
		return
	}
	s.handleResources(w, r, a)
}

// handleStopRoute adapts the stop handler to the route table.
func (s *supervisor) handleStopRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleStop(w, r, params["name"])
}

// handleStartRoute adapts the start handler to the route table.
func (s *supervisor) handleStartRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleStart(w, r, params["name"])
}

// handleRestartRoute adapts the restart handler to the route table.
func (s *supervisor) handleRestartRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	a, ok := s.requireAppMethod(w, r, params["name"], http.MethodPost)
	if !ok {
		return
	}
	s.handleRestart(w, r, a)
}

// handleValidateRoute runs a deploy in dry-run mode.
func (s *supervisor) handleValidateRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	r.URL.RawQuery = "dry_run=1"
	s.handleDeploymentCreate(w, r, params["name"])
}

// handleRollbackRoute adapts the rollback handler to the route table.
func (s *supervisor) handleRollbackRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleRollback(w, r, params["name"], params["id"])
}

// handleAppBlobsRoute adapts the blob list handler.
func (s *supervisor) handleAppBlobsRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppBlobs(w, r, params["name"])
}

// handleAppBlobRoute adapts the blob delete handler.
func (s *supervisor) handleAppBlobRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppBlob(w, r, params["name"], params["digest"])
}

// handleAppBlobGCRoute adapts the blob garbage collection handler.
func (s *supervisor) handleAppBlobGCRoute(w http.ResponseWriter, r *http.Request, params map[string]string) {
	s.handleAppBlobGC(w, r, params["name"])
}

// buildAppRoutes declares every app subresource once, with the method and scope label it needs.
func (s *supervisor) buildAppRoutes() *routeTable {
	rt := newRouteTable()
	rt.addApp(http.MethodGet, "/apps/{name}", constantAction(""), s.handleAppRootRoute)
	rt.addApp(http.MethodDelete, "/apps/{name}", constantAction(""), s.handleAppRootRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/config", constantAction("config"), s.handleAppConfigRoute)
	rt.addApp(http.MethodPatch, "/apps/{name}/config", constantAction("config"), s.handleAppConfigRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/maintenance", constantAction("maintenance"), s.handleMaintenanceRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/maintenance", constantAction("maintenance"), s.handleMaintenanceRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/secrets", constantAction("secrets"), s.handleSecretsRoute)
	rt.addApp(http.MethodPut, "/apps/{name}/secrets/{key}", constantAction("secrets"), s.handleSecretsRoute)
	rt.addApp(http.MethodDelete, "/apps/{name}/secrets/{key}", constantAction("secrets"), s.handleSecretsRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/deployments", constantAction("deployments"), s.handleDeploymentsRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/deployments", constantAction("deployments"), s.handleDeploymentsRoute)
	rt.addApp("", "/apps/{name}/deployments/{rest...}", actionWithPrefix("deployments"), s.handleDeploymentItemRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/blobs", constantAction("blobs"), s.handleAppBlobsRoute)
	rt.addApp(http.MethodDelete, "/apps/{name}/blobs/{digest}", constantAction("blobs"), s.handleAppBlobRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/blobs/gc", constantAction("blobs/gc"), s.handleAppBlobGCRoute)
	rt.addApp("", "/apps/{name}/jobs/{rest...}", actionWithPrefix("jobs"), s.handleJobsRoute)
	rt.addApp("", "/apps/{name}/backups/{rest...}", actionWithPrefix("backups"), s.handleBackupsRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/database", constantAction("database"), s.handleDatabaseRoute)
	rt.addApp("", "/apps/{name}/database/{rest...}", actionWithPrefix("database"), s.handleDatabaseRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/logs", constantAction("logs"), s.handleLogsRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/logs/stream", constantAction("logs/stream"), s.handleLogStreamRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/logs/export", constantAction("logs/export"), s.handleLogExportRoute)
	rt.addApp(http.MethodGet, "/apps/{name}/resources", constantAction("resources"), s.handleResourcesRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/stop", constantAction("stop"), s.handleStopRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/start", constantAction("start"), s.handleStartRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/restart", constantAction("restart"), s.handleRestartRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/validate", constantAction("validate"), s.handleValidateRoute)
	rt.addApp(http.MethodPost, "/apps/{name}/rollback/{id}", func(params map[string]string) string {
		return "rollback/" + params["id"]
	}, s.handleRollbackRoute)
	return rt
}
