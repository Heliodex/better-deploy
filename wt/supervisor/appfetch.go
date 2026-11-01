// Authenticated app-to-app fetches through the supervisor.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	appFetchPathPrefix = "/apps/"
	appCallerHeader    = "X-WT-App-Caller"
	appTimestampHeader = "X-WT-App-Timestamp"
	appSignatureHeader = "X-WT-App-Signature"
	maxAppFetchBody    = 8 << 20
	appFetchURLEnv     = "WT_APP_FETCH_URL"
	appNameEnv         = "WT_APP_NAME"
	appTokenEnv        = "WT_APP_TOKEN"
	appURLEnvPrefix    = "WT_APP_URL_"
)

// appFetchServer serves the internal app-to-app endpoint on loopback. It is separate from storage and the admin listener so an app never needs the admin surface.
type appFetchServer struct {
	listener   net.Listener
	server     *http.Server
	baseURL    string
	supervisor *supervisor
}

func newAppFetchServer(listen string, s *supervisor) (*appFetchServer, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("app fetch listen: %w", err)
	}
	return newAppFetchServerOn(listener, s)
}

func newAppFetchServerOn(listener net.Listener, s *supervisor) (*appFetchServer, error) {
	fetch := &appFetchServer{
		listener:   listener,
		baseURL:    "http://" + listener.Addr().String(),
		supervisor: s,
	}
	fetch.server = &http.Server{
		Handler:           http.HandlerFunc(fetch.handle),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := fetch.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("app fetch: %v", err)
		}
	}()
	return fetch, nil
}

func (f *appFetchServer) url() string {
	if f == nil {
		return ""
	}
	return f.baseURL
}

func (f *appFetchServer) endpoint() (string, int) {
	if f == nil {
		return "", 0
	}
	address, ok := f.listener.Addr().(*net.TCPAddr)
	if !ok || address == nil {
		return "", 0
	}
	ip := address.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.ParseIP("127.0.0.1")
	}
	return ip.String(), address.Port
}

func (f *appFetchServer) close() {
	if f == nil || f.server == nil {
		return
	}
	_ = f.server.Close()
}

// bearerToken returns the bearer credential from a request.
func bearerToken(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

// parseAppFetchPath splits "/apps/<target>/<rest>" into the target app and the target path.
func parseAppFetchPath(path string) (target, rest string, ok bool) {
	rest, ok = strings.CutPrefix(path, appFetchPathPrefix)
	if !ok || rest == "" {
		return "", "", false
	}
	target, rest, found := strings.Cut(rest, "/")
	if !validName(target) {
		return "", "", false
	}
	if !found || rest == "" {
		return target, "/", true
	}
	return target, "/" + rest, true
}

// allowsAppCaller reports whether the target's allow_callers list names the caller.
func allowsAppCaller(allowed []string, caller string) bool {
	for _, name := range allowed {
		if name == caller {
			return true
		}
	}
	return false
}

// appCallSignature binds the caller identity to one method, path and timestamp. The target's storage token is the key, so only the supervisor and that target can produce or verify it.
func appCallSignature(key, caller, method, path, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = io.WriteString(mac, caller+"\n"+method+"\n"+path+"\n"+timestamp)
	return hex.EncodeToString(mac.Sum(nil))
}

// handle forwards one authenticated app-to-app request.
func (f *appFetchServer) handle(w http.ResponseWriter, r *http.Request) {
	f.supervisor.handleAppFetch(w, r)
}

// handleAppFetch authenticates the caller with its storage token. The target's allow_callers list decides whether the call proceeds.
func (s *supervisor) handleAppFetch(w http.ResponseWriter, r *http.Request) {
	if s.storage == nil {
		writeError(w, http.StatusServiceUnavailable, "app fetch unavailable")
		return
	}
	caller, ok := s.storage.appForToken(bearerToken(r))
	if !ok || s.lookup(caller) == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wt-app-fetch"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target, rest, ok := parseAppFetchPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a := s.lookup(target)
	if a == nil {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	if !allowsAppCaller(cfg.AllowCallers, caller) {
		writeError(w, http.StatusForbidden, "app does not allow calls from "+caller)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAppFetchBody)
	r.URL.Path = rest
	r.URL.RawPath = ""
	r.RequestURI = r.URL.RequestURI()
	r.Host = cfg.Host
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	r.Header.Del("X-Forwarded-For")
	r.Header.Del("X-Forwarded-Host")
	r.Header.Del("X-Forwarded-Proto")
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-wt-") {
			delete(r.Header, key)
		}
	}
	if r.Header.Get(requestIDHeader) == "" {
		r.Header.Set(requestIDHeader, newRequestID())
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	signature := appCallSignature(s.storage.tokenFor(target), caller, r.Method, r.URL.RequestURI(), timestamp)
	r.Header.Set(appCallerHeader, caller)
	r.Header.Set(appTimestampHeader, timestamp)
	r.Header.Set(appSignatureHeader, signature)
	s.serveApp(w, r, a)
}

// appURLEnvName maps an app name to the env key that carries its internal URL.
func appURLEnvName(name string) string {
	var key strings.Builder
	key.WriteString(appURLEnvPrefix)
	for _, char := range strings.ToUpper(name) {
		switch {
		case char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			key.WriteRune(char)
		default:
			key.WriteByte('_')
		}
	}
	return key.String()
}

// appFetchURL builds the base URL for one target app.
func (f *appFetchServer) appURL(name string) string {
	return f.url() + appFetchPathPrefix + name
}
