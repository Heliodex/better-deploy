package main

import (
	"net/http"
	"slices"
	"strings"
	"sync"
)

// routeHandler serves one matched route. Params contains the captured path segments.
type routeHandler func(http.ResponseWriter, *http.Request, map[string]string)

// route is one method and path pattern in a routeTable.
// An empty method matches every method; the handler then owns the method check.
// A pattern segment "{name}" captures one segment, and a trailing "{name...}" captures the rest.
type route struct {
	method  string
	pattern []string
	handler routeHandler
	// action derives the audit and scope label from the captured parameters.
	action func(map[string]string) string
}

// routeTable dispatches requests by path segments and method.
type routeTable struct {
	routes []route
}

// routeCache builds one route table on first use.
type routeCache struct {
	once  sync.Once
	table *routeTable
}

func (c *routeCache) get(build func() *routeTable) *routeTable {
	c.once.Do(func() {
		c.table = build()
	})

	return c.table
}

func newRouteTable() *routeTable {
	return &routeTable{}
}

// add registers one method and pattern. Empty method means any method.
func (rt *routeTable) add(method, pattern string, handler routeHandler) {
	rt.routes = append(rt.routes, route{method: method, pattern: routeSegments(pattern), handler: handler})
}

// addApp registers an app route and records the action used for scope checks and audit.
func (rt *routeTable) addApp(method, pattern string, action func(map[string]string) string, handler routeHandler) {
	rt.routes = append(rt.routes, route{method: method, pattern: routeSegments(pattern), handler: handler, action: action})
}

// ServeHTTP writes 405 for a known path with the wrong method and 404 otherwise.
func (rt *routeTable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.serve(w, r, "")
}

// serve dispatches the request and exposes app to a table whose handlers need the authenticated app name.
func (rt *routeTable) serve(w http.ResponseWriter, r *http.Request, app string) {
	route, params, allowed := rt.match(r.Method, r.URL.Path)
	if route == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		http.NotFound(w, r)

		return
	}

	if app != "" {
		params["app"] = app
	}

	route.handler(w, r, params)
}

// matches reports whether any route accepts the path, regardless of method.
func (rt *routeTable) matches(path string) bool {
	segments := routeSegments(path)
	for i := range rt.routes {
		if _, ok := matchRoute(rt.routes[i].pattern, segments); ok {
			return true
		}
	}

	return false
}

// plainHandler adapts a handler that only needs the request and response.
func plainHandler(fn func(http.ResponseWriter, *http.Request)) routeHandler {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		fn(w, r)
	}
}

// match returns the first route that accepts the method, or the methods that would have matched.
func (rt *routeTable) match(method, path string) (*route, map[string]string, []string) {
	segments := routeSegments(path)

	var allowed []string

	for i := range rt.routes {
		route := &rt.routes[i]

		params, ok := matchRoute(route.pattern, segments)
		if !ok {
			continue
		}

		if route.method != "" && route.method != method {
			if !slices.Contains(allowed, route.method) {
				allowed = append(allowed, route.method)
			}

			continue
		}

		return route, params, nil
	}

	return nil, nil, allowed
}

// routeSegments splits a path or pattern, trimming one trailing slash so /apps/x/ matches /apps/x.
func routeSegments(path string) []string {
	path = strings.TrimPrefix(path, "/")

	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return nil
	}

	return strings.Split(path, "/")
}

// matchRoute captures parameters from segments. A trailing {name...} accepts zero or more segments.
func matchRoute(pattern, segments []string) (map[string]string, bool) {
	params := map[string]string{}

	for i, part := range pattern {
		if rest, ok := strings.CutPrefix(part, "{"); ok {
			name := strings.TrimSuffix(rest, "}")
			if strings.HasSuffix(name, "...") {
				if i > len(segments) {
					return nil, false
				}

				params[strings.TrimSuffix(name, "...")] = strings.Join(segments[i:], "/")

				return params, true
			}

			if i >= len(segments) {
				return nil, false
			}

			params[name] = segments[i]

			continue
		}

		if i >= len(segments) || segments[i] != part {
			return nil, false
		}
	}

	return params, len(segments) == len(pattern)
}

// constantAction returns the same audit action for every match.
func constantAction(action string) func(map[string]string) string {
	return func(map[string]string) string { return action }
}
