package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNormalizeDevConfig(t *testing.T) {
	dev := &appDevConfig{}
	if err := normalizeDevConfig(dev); err != nil {
		t.Fatal(err)
	}

	if len(dev.Proxy) != 1 || dev.Proxy[0] != "/api" {
		t.Errorf("default proxy = %v, want [/api]", dev.Proxy)
	}

	if len(dev.SupervisorProxy) != len(defaultSupervisorProxy) {
		t.Errorf("default supervisor proxy = %v", dev.SupervisorProxy)
	}

	for _, prefix := range defaultSupervisorProxy {
		if !slices.Contains(dev.SupervisorProxy, prefix) {
			t.Errorf("default supervisor proxy is missing %s", prefix)
		}
	}

	for name, bad := range map[string]*appDevConfig{
		"absolute web":              {Web: "/tmp/web"},
		"parent web":                {Web: "../web"},
		"absolute watch":            {Watch: []string{"/tmp/file"}},
		"parent watch":              {Watch: []string{"../file"}},
		"relative proxy":            {Proxy: []string{"api"}},
		"parent proxy":              {Proxy: []string{"/../api"}},
		"relative supervisor proxy": {SupervisorProxy: []string{"v1/events"}},
		"parent supervisor proxy":   {SupervisorProxy: []string{"/../events"}},
		"large port":                {Port: 70000},
	} {
		if err := normalizeDevConfig(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	empty := &appDevConfig{SupervisorProxy: []string{}}
	if err := normalizeDevConfig(empty); err != nil {
		t.Fatal(err)
	}

	if len(empty.SupervisorProxy) != 0 {
		t.Errorf("explicit empty supervisor proxy = %v, want none", empty.SupervisorProxy)
	}
}

func TestViteDevConfigIncludesProxy(t *testing.T) {
	appDir := t.TempDir()

	webDir := filepath.Join(appDir, "web")
	if err := os.MkdirAll(filepath.Join(webDir, "src"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(webDir, "package.json"), []byte(`{"scripts":{"dev":"vite dev"}}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(webDir, "vite.config.ts"), []byte("export default {}\n"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	m := manifest{
		Name: testAppName,
		Dev:  &appDevConfig{Port: 5199, Proxy: []string{"/api", "/v1"}},
	}
	if err := normalizeDevConfig(m.Dev); err != nil {
		t.Fatal(err)
	}

	vite, ok, err := resolveViteDev(appDir, m, "demo.localhost")
	if err != nil || !ok {
		t.Fatalf("resolveViteDev = %v, %t, %v", vite, ok, err)
	}

	if vite.webDir != webDir || vite.port != 5199 || vite.command != "bun run dev" {
		t.Errorf("vite = %+v", vite)
	}

	if err := writeViteDevConfig(vite, 18080); err != nil {
		t.Fatal(err)
	}

	if !filepath.IsAbs(vite.configPath) {
		t.Errorf("config path is not absolute: %q", vite.configPath)
	}

	data, err := os.ReadFile(vite.configPath)
	if err != nil {
		t.Fatal(err)
	}

	config := string(data)
	for _, want := range []string{
		`import base from "./vite.config.ts"`,
		`target: "http://127.0.0.1:18080"`,
		`Host: "demo.localhost"`,
		"port: 5199",
		`"/api"`,
		`"/v1"`,
		`"/v1/events"`,
		`"/_wt/blobs"`,
		`"/_wt/identity"`,
		"strictPort: true",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config is missing %q:\n%s", want, config)
		}
	}
}

func TestViteDevConfigCanDisableSupervisorProxy(t *testing.T) {
	appDir := t.TempDir()

	webDir := filepath.Join(appDir, "web")
	if err := os.MkdirAll(webDir, privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(webDir, "package.json"), []byte(`{}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	m := manifest{Name: testAppName, Dev: &appDevConfig{Port: 5199, SupervisorProxy: []string{}}}
	if err := normalizeDevConfig(m.Dev); err != nil {
		t.Fatal(err)
	}

	vite, ok, err := resolveViteDev(appDir, m, "demo.localhost")
	if err != nil || !ok {
		t.Fatalf("resolveViteDev = %v, %t, %v", vite, ok, err)
	}

	if err := writeViteDevConfig(vite, 18080); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(vite.configPath)
	if err != nil {
		t.Fatal(err)
	}

	config := string(data)
	for _, unexpected := range defaultSupervisorProxy {
		if strings.Contains(config, strconv.Quote(unexpected)) {
			t.Errorf("config still contains %q:\n%s", unexpected, config)
		}
	}

	if !strings.Contains(config, `"/api"`) {
		t.Errorf("config is missing the app proxy:\n%s", config)
	}
}

func TestViteDevConfigWithoutBase(t *testing.T) {
	appDir := t.TempDir()

	webDir := filepath.Join(appDir, "web")
	if err := os.MkdirAll(webDir, privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(webDir, "package.json"), []byte(`{}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	vite, ok, err := resolveViteDev(appDir, manifest{Name: testAppName}, "demo.localhost")
	if err != nil || !ok {
		t.Fatalf("resolveViteDev = %v, %t, %v", vite, ok, err)
	}

	if err := writeViteDevConfig(vite, 18080); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(vite.configPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "const base: any = {}") {
		t.Errorf("config has no base fallback:\n%s", data)
	}
}

func TestComponentSourceState(t *testing.T) {
	root := t.TempDir()

	files := []string{
		"app.json",
		"main.go",
		"contract/contract.gen.go",
		"web/src/App.svelte",
		"web/src/lib/contract.ts",
		"static/style.css",
	}
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), privateDirMode); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("x"), privateFileMode); err != nil {
			t.Fatal(err)
		}
	}

	state, err := componentSourceState(root, []string{"web/src/lib/contract.ts"})
	if err != nil {
		t.Fatal(err)
	}

	has := func(name string) bool {
		_, ok := state[filepath.Join(root, name)]
		return ok
	}
	for _, name := range []string{"app.json", "main.go", "web/src/lib/contract.ts"} {
		if !has(name) {
			t.Errorf("%s was not watched", name)
		}
	}

	for _, name := range []string{"contract/contract.gen.go", "web/src/App.svelte", "static/style.css"} {
		if has(name) {
			t.Errorf("%s was watched", name)
		}
	}
}

func TestViteDevProxy(t *testing.T) {
	if os.Getenv("WT_VITE_TEST") != "1" {
		t.Skip("set WT_VITE_TEST=1 to run the Vite dev proxy test")
	}

	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is not installed")
	}

	webDir, err := filepath.Abs(filepath.Join("..", "example", "guestbook", "web"))
	if err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(filepath.Join(webDir, "node_modules")); err != nil || !info.IsDir() {
		t.Skip("guestbook web dependencies are not installed")
	}

	hostSeen := make(chan string, 1)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hostSeen <- r.Host:
		default:
		}

		switch r.URL.Path {
		case "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")

			_, _ = w.Write([]byte("data: hello\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case "/_wt/blobs/test":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("blob"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer target.Close()

	parsed, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	targetPort, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	m := manifest{Name: "guestbook", Dev: &appDevConfig{Proxy: []string{"/api"}}}
	if err := normalizeDevConfig(m.Dev); err != nil {
		t.Fatal(err)
	}

	vite, ok, err := resolveViteDev(filepath.Dir(webDir), m, "guestbook.localhost")
	if err != nil || !ok {
		t.Fatalf("resolveViteDev = %v, %t, %v", vite, ok, err)
	}

	if err := writeViteDevConfig(vite, targetPort); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(vite.configPath)

	cmd, err := startVite(vite)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		signalViteGroup(cmd, syscall.SIGTERM)

		done := make(chan struct{})

		go func() {
			_ = cmd.Wait()

			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			signalViteGroup(cmd, syscall.SIGKILL)
			<-done
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://127.0.0.1:" + strconv.Itoa(vite.port)
	deadline := time.Now().Add(20 * time.Second)

	for {
		response, err := client.Get(base + "/")
		if err == nil {
			response.Body.Close()
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("vite did not start: %v", err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	response, err := client.Get(base + "/api/check")
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxied API status = %d", response.StatusCode)
	}

	response, err = client.Get(base + "/v1/events?topic=checks")
	if err != nil {
		t.Fatal(err)
	}

	eventBody, err := io.ReadAll(response.Body)
	response.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK || !strings.Contains(string(eventBody), "data: hello") {
		t.Errorf("proxied event stream = %d %q", response.StatusCode, eventBody)
	}

	response, err = client.Get(base + "/_wt/blobs/test")
	if err != nil {
		t.Fatal(err)
	}

	blobBody, err := io.ReadAll(response.Body)
	response.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK || string(blobBody) != "blob" {
		t.Errorf("proxied blob = %d %q", response.StatusCode, blobBody)
	}

	select {
	case host := <-hostSeen:
		if host != "guestbook.localhost" {
			t.Errorf("proxied Host = %q, want guestbook.localhost", host)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy target did not receive the request")
	}
}

func TestCheckStaticSvelteKit(t *testing.T) {
	appDir := t.TempDir()

	routes := filepath.Join(appDir, "web", "src", "routes")
	if err := os.MkdirAll(filepath.Join(routes, "about"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(routes, "+page.svelte"), []byte("<h1>ok</h1>"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	m := manifest{Name: testAppName}
	if err := checkStaticSvelteKit(appDir, m); err != nil {
		t.Fatalf("prerenderable route rejected: %v", err)
	}

	for _, name := range []string{"+page.server.ts", "about/+layout.server.js", "+server.ts"} {
		path := filepath.Join(routes, name)
		if err := os.WriteFile(path, []byte("export {}"), privateFileMode); err != nil {
			t.Fatal(err)
		}

		if err := checkStaticSvelteKit(appDir, m); err == nil {
			t.Errorf("%s was accepted", name)
		}

		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	src := filepath.Join(appDir, "web", "src")

	extra := []string{"hooks.server.ts", "lib/server/db.ts"}
	for _, name := range extra {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), privateDirMode); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("export {}"), privateFileMode); err != nil {
			t.Fatal(err)
		}

		if err := checkStaticSvelteKit(appDir, m); err == nil {
			t.Errorf("%s was accepted", name)
		}

		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
