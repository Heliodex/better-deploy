package main

import (
	"net/http"
	"strings"
)

// adminRoutes returns the cached table for the supervisor's own API and panel.
func (s *supervisor) adminRoutes() *routeTable {
	return s.adminRoutesCache.get(func() *routeTable {
		rt := newRouteTable()
		rt.add("", "/healthz", plainHandler(s.handleHealthz))
		rt.add("", "/readyz", plainHandler(s.handleReadyz))
		rt.add("", "/apps", plainHandler(s.handleApps))
		rt.add("", "/apps/{rest...}", plainHandler(s.handleApp))
		rt.add("", "/auth/tokens", plainHandler(s.handleAuthTokens))
		rt.add("", "/auth/tokens/{id}", func(w http.ResponseWriter, r *http.Request, params map[string]string) {
			s.handleAuthToken(w, r, params["id"])
		})
		rt.add("", "/identity/users", plainHandler(s.handleIdentityUsers))
		rt.add("", "/identity/users/{rest...}", func(w http.ResponseWriter, r *http.Request, params map[string]string) {
			s.handleIdentityUser(w, r, params["rest"])
		})
		rt.add(http.MethodGet, "/usage", plainHandler(s.handleUsage))
		rt.add(http.MethodGet, "/audit", plainHandler(s.handleAudit))
		rt.add(http.MethodGet, "/metrics", plainHandler(s.handleMetrics))
		rt.add(http.MethodGet, "/events", plainHandler(s.handleEvents))
		rt.add(http.MethodPost, "/login", plainHandler(s.handleLogin))
		rt.add(http.MethodPost, "/logout", plainHandler(s.handleLogout))
		files := http.StripPrefix("/admin", adminStatic())
		rt.add("", "/admin", func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			if strings.HasSuffix(r.URL.Path, "/") {
				files.ServeHTTP(w, r)
				return
			}
			http.Redirect(w, r, "/admin/", http.StatusFound)
		})
		rt.add("", "/admin/{rest...}", plainHandler(files.ServeHTTP))
		return rt
	})
}

// publicRoutes returns the table for the public listener when admin routes live elsewhere.
func (s *supervisor) publicRoutes() *routeTable {
	return s.publicRoutesCache.get(func() *routeTable {
		rt := newRouteTable()
		rt.add("", "/healthz", plainHandler(s.handleHealthz))
		rt.add("", "/readyz", plainHandler(s.handleReadyz))
		return rt
	})
}

// serveAdminOrProxy serves an admin route and falls back to the app proxy for anything else.
func (s *supervisor) serveAdminOrProxy(w http.ResponseWriter, r *http.Request) {
	routes := s.adminRoutes()
	route, _, allowed := routes.match(r.Method, r.URL.Path)
	if route != nil {
		routes.ServeHTTP(w, r)
		return
	}
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.handleProxy(w, r)
}
