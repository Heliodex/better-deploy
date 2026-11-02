package main

import (
	"os"
	"strings"
	"testing"
)

// routeInfo is one method and path pattern in any supervisor route table.
type routeInfo struct {
	method string
	path   string
}

// normalizeRoutePath replaces every captured segment so OpenAPI templates compare equal.
func normalizeRoutePath(path string) string {
	segments := routeSegments(path)
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") {
			segments[i] = "{}"
		}
	}

	return "/" + strings.Join(segments, "/")
}

// routeEntries lists every route whose method is known, so the OpenAPI coverage check can compare them.
func (s *supervisor) routeEntries() []routeInfo {
	tables := []*routeTable{
		s.buildAppRoutes(),
		newStorageServerRoutes(),
		s.identityAppRoutes(),
		s.adminRoutes(),
		s.publicRoutes(),
	}

	var entries []routeInfo

	for _, table := range tables {
		for _, route := range table.routes {
			if route.method == "" {
				continue
			}

			entries = append(entries, routeInfo{method: route.method, path: normalizeRoutePath("/" + strings.Join(route.pattern, "/"))})
		}
	}

	return entries
}

func newStorageServerRoutes() *routeTable {
	s := &storageServer{}
	return s.buildStorageRoutes()
}

// TestOpenAPIDocumentsRoutes fails when a method or path exists in the route tables but not in openapi.yaml.
func TestOpenAPIDocumentsRoutes(t *testing.T) {
	data, err := os.ReadFile("../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}

	documented := map[string]struct{}{}
	path := ""

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  /") || strings.HasPrefix(line, "  \"/") {
			raw := strings.Trim(strings.TrimSuffix(trimmed, ":"), "\"")
			path = normalizeRoutePath(raw)

			continue
		}

		switch trimmed {
		case "get:", "post:", "put:", "patch:", "delete:", "head:", "options:":
			if path != "" && strings.HasPrefix(line, "    ") {
				documented[strings.ToUpper(strings.TrimSuffix(trimmed, ":"))+" "+path] = struct{}{}
			}
		}
	}

	for _, entry := range (&supervisor{}).routeEntries() {
		key := strings.ToUpper(entry.method) + " " + entry.path
		if _, ok := documented[key]; !ok {
			t.Errorf("openapi.yaml is missing %s", key)
		}
	}
}
