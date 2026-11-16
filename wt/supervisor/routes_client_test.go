package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

type routeClientEntry struct {
	name, pattern string
}

var (
	updateRouteClient = flag.Bool("update-routes", false, "regenerate web/src/lib/routes.gen.ts")

	// routeClientNames maps a route pattern to the generated TypeScript helper name.
	routeClientNames = map[string]string{
		"/admin": "admin",
		"/admin/{rest...}": "adminFile",
		"/apps": "apps",
		"/apps/{name}": "app",
		"/apps/{name}/backups/{rest...}": "appBackup",
		"/apps/{name}/blobs": "appBlobs",
		"/apps/{name}/blobs/gc": "appBlobGC",
		"/apps/{name}/blobs/{digest}": "appBlob",
		"/apps/{name}/config": "appConfig",
		"/apps/{name}/database": "appDatabase",
		"/apps/{name}/database/{rest...}": "appDatabasePath",
		"/apps/{name}/deployments": "appDeployments",
		"/apps/{name}/deployments/{rest...}": "appDeployment",
		"/apps/{name}/jobs/{rest...}": "appJob",
		"/apps/{name}/logs": "appLogs",
		"/apps/{name}/logs/export": "appLogsExport",
		"/apps/{name}/logs/stream": "appLogsStream",
		"/apps/{name}/maintenance": "appMaintenance",
		"/apps/{name}/resources": "appResources",
		"/apps/{name}/restart": "appRestart",
		"/apps/{name}/rollback/{id}": "appRollback",
		"/apps/{name}/secrets": "appSecrets",
		"/apps/{name}/secrets/{key}": "appSecret",
		"/apps/{name}/start": "appStart",
		"/apps/{name}/stop": "appStop",
		"/apps/{name}/validate": "appValidate",
		"/apps/{rest...}": "adminApp",
		"/audit": "audit",
		"/auth/tokens": "authTokens",
		"/auth/tokens/{id}": "authToken",
		"/capabilities": "capabilities",
		"/events": "events",
		"/healthz": "healthz",
		"/identity/users": "identityUsers",
		"/identity/users/{rest...}": "identityUser",
		"/login": "login",
		"/logout": "logout",
		"/metrics": "metrics",
		"/readyz": "readyz",
		"/usage": "usage",
	}
)

func renderRouteClient(entries []routeClientEntry) string {
	var b strings.Builder

	b.WriteString("// Code generated from the supervisor route table. DO NOT EDIT.\n\n")
	b.WriteString("const enc = encodeURIComponent\n\n")
	b.WriteString("export const routes = {\n")

	for _, entry := range entries {
		segments := routeSegments(entry.pattern)

		var (
			params []string
			template string
			templateSb94 strings.Builder
		)

		for i, segment := range segments {
			if i > 0 {
				templateSb94.WriteString("/")
			}

			if name, ok := strings.CutPrefix(segment, "{"); ok {
				name, _ = strings.CutSuffix(name, "}")

				if rest, isRest := strings.CutSuffix(name, "..."); isRest {
					params = append(params, rest+" = \"\"")

					templateSb94.WriteString("${" + rest + "}")
				} else {
					params = append(params, name+": string")

					templateSb94.WriteString("${enc(" + name + ")}")
				}

				continue
			}

			templateSb94.WriteString(segment)
		}

		template += templateSb94.String()

		if len(params) == 0 {
			fmt.Fprintf(&b, "\t%s: \"/%s\",\n", entry.name, template)
			continue
		}

		fmt.Fprintf(&b, "\t%s: (%s): string => `/%s`,\n", entry.name, strings.Join(params, ", "), template)
	}

	b.WriteString("}\n")

	return b.String()
}

// routeClientEntries collects the distinct app and admin patterns the panel uses.
func routeClientEntries() (entries []routeClientEntry) {
	s := &supervisor{}
	seen := map[string]struct{}{}

	for _, table := range []*routeTable{s.buildAppRoutes(), s.adminRoutes()} {
		for _, route := range table.routes {
			pattern := "/" + strings.Join(route.pattern, "/")

			if _, ok := seen[pattern]; ok {
				continue
			}

			seen[pattern] = struct{}{}

			name, ok := routeClientNames[pattern]
			if !ok {
				panic("missing route client name for " + pattern)
			}

			entries = append(entries, routeClientEntry{
				name: name,
				pattern: pattern,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})

	return
}

func TestGeneratedRouteClient(t *testing.T) {
	const path = "web/src/lib/routes.gen.ts"

	want := renderRouteClient(routeClientEntries())
	got, err := os.ReadFile(path)

	if *updateRouteClient {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}

		return
	}

	if err != nil {
		t.Fatalf("%s is missing; run go test -run TestGeneratedRouteClient -update-routes: %v", path, err)
	}

	if string(got) != want {
		t.Fatalf("%s is stale; run go test -run TestGeneratedRouteClient -update-routes", path)
	}
}
