// Scoped operator and deploy tokens for the admin API.

package main

import (
	"net/http"
	"strings"
	"time"
)

const (
	roleAdmin  = "admin"
	roleDeploy = "deploy"
	roleRead   = "read"

	authFileVersion  = 1
	maxAuthTokenName = 64
	maxAuthTokens    = 256
	minTokenExpiry   = time.Minute
	maxTokenExpiry   = 10 * 365 * 24 * time.Hour
)

// authIdentity is the caller a request authenticated as. A stored token narrows the apps and actions it may touch.
type authIdentity struct {
	Name string
	Role string
	Apps []string
}

func (i authIdentity) isAdmin() bool {
	return i.Role == roleAdmin
}

func (i authIdentity) canAccessApp(name string) bool {
	if i.isAdmin() {
		return true
	}

	for _, app := range i.Apps {
		if app == "*" || app == name {
			return true
		}
	}

	return false
}

// canPerformAppAction maps the app routes to the read, deploy and admin roles. Deploy tokens cannot change config, secrets, backups, databases or delete an app, and a coordinated rollback with a backup is admin-only because it replaces data.
func (i authIdentity) canPerformAppAction(r *http.Request, action string) bool {
	if i.isAdmin() {
		return true
	}

	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if read {
		return !sensitiveAppRead(action)
	}

	if i.Role != roleDeploy || r.Method != http.MethodPost {
		return false
	}

	switch {
	case action == "", action == "deployments", strings.HasPrefix(action, "deployments/"), strings.HasPrefix(action, "rollback/"):
		if r.URL.Query().Get("backup") != "" {
			return false
		}

		return true
	case action == "restart", action == "stop", action == "start", action == "maintenance":
		return true
	case action == "jobs", strings.HasPrefix(action, "jobs/"), action == "validate":
		return true
	default:
		return false
	}
}

// sensitiveAppRead keeps database contents, backups and secrets to admins even for a read token.
func sensitiveAppRead(action string) bool {
	return action == "secrets" || strings.HasPrefix(action, "secrets/") ||
		action == "database" || strings.HasPrefix(action, "database/") ||
		action == "backups" || strings.HasPrefix(action, "backups/")
}
