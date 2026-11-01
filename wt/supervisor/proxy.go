package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

var (
	proxyDialTimeout           = 5 * time.Second
	proxyResponseHeaderTimeout = 30 * time.Second
)

const (
	proxyIdleConnTimeout = 90 * time.Second
	proxyMaxIdlePerHost  = 16
	proxyExpectContinue  = time.Second

	requestIDHeader      = "X-Request-Id"
	spinFullURLHeader    = "spin-full-url"
	spinClientAddrHeader = "spin-client-addr"
	spinBasePathHeader   = "spin-base-path"
	spinPathInfoHeader   = "spin-path-info"
)

// newRequestID returns a short random identifier that ties a proxy, app and storage request together.
func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

func (s *supervisor) newProxy(a *app) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", a.port)}
	proxy := &httputil.ReverseProxy{
		// JSON responses are compressed by the supervisor, so the transport asks the app for identity and every response is a normal Go body.
		ModifyResponse: func(res *http.Response) error {
			accept, _ := res.Request.Context().Value(acceptEncodingKey{}).(string)
			return compressJSONResponse(res, accept)
		},
		// A hung app must not hold client connections forever: header and dial deadlines bound the wait before the response starts.
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: proxyResponseHeaderTimeout,
			IdleConnTimeout:       proxyIdleConnTimeout,
			MaxIdleConnsPerHost:   proxyMaxIdlePerHost,
			ExpectContinueTimeout: proxyExpectContinue,
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			info := s.forwardedInfo(r.In)
			r.SetURL(target)
			// Replace the whole inbound chain, so a client cannot prepend a spoofed address. When a trusted proxy supplied X-Forwarded-For, the first entry is the resolved client and the last is the immediate proxy.
			chain := info.ClientIP
			if remote := remoteIP(r.In.RemoteAddr); info.Trusted && remote != "" && remote != info.ClientIP {
				chain += ", " + remote
			}
			r.Out.Header.Set("X-Forwarded-For", chain)
			r.Out.Header.Set("X-Real-IP", info.ClientIP)
			r.Out.Header.Set("X-Forwarded-Host", info.Authority)
			r.Out.Header.Set("X-Forwarded-Proto", info.Proto)
			r.Out.Header.Del("Forwarded")
			r.Out.Host = info.Authority
			r.Out = r.Out.WithContext(context.WithValue(r.Out.Context(), acceptEncodingKey{}, r.In.Header.Get("Accept-Encoding")))
			r.Out.Header.Set("Accept-Encoding", "identity")
			// The Spin host headers give the component the standard net/http view of the original request.
			r.Out.Header.Set(spinFullURLHeader, info.Proto+"://"+info.Authority+r.In.URL.RequestURI())
			r.Out.Header.Set(spinClientAddrHeader, info.ClientIP)
			r.Out.Header.Set(spinPathInfoHeader, r.In.URL.RequestURI())
			r.Out.Header.Set(spinBasePathHeader, "")
			if requestID := r.In.Header.Get(requestIDHeader); requestID != "" {
				r.Out.Header.Set(requestIDHeader, requestID)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("app %s: proxy: %v", a.name, err)
			http.Error(w, "app unavailable", http.StatusBadGateway)
		},
	}
	return proxy
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// formatProxyDuration renders a request duration compactly for the access log.
func formatProxyDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// formatAccessLog renders a supervisor access line as METHOD status duration path, with a request id when one is available.
func formatAccessLog(method string, status int, duration time.Duration, uri, requestID string) string {
	line := fmt.Sprintf("%s %d %s %s", method, status, formatProxyDuration(duration), uri)
	if requestID != "" {
		line += " request=" + requestID
	}
	return line
}

// requestHost is the lower-case request host without a port, as used for app routing.
func requestHost(r *http.Request) string {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// appForHost finds an exact host first, then a single-label wildcard such as *.apps.example.com.
func (s *supervisor) appForHost(host string) *app {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a := s.hosts[host]; a != nil {
		return a
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return nil
	}
	return s.hosts["*."+strings.Join(labels[1:], ".")]
}

func (s *supervisor) handleProxy(w http.ResponseWriter, r *http.Request) {
	host := s.effectiveHost(r)
	a := s.appForHost(host)
	if a == nil {
		http.Error(w, "no app for this host", http.StatusNotFound)
		return
	}
	a.mu.Lock()
	identityMode := a.cfg.Identity == identityModePlatform
	a.mu.Unlock()
	if identityMode {
		if action, ok := identityAppAction(r.URL.Path); ok {
			s.handleIdentityApp(w, r, a, action)
			return
		}
	}
	s.identityProxyHeaders(r, a.name, identityMode)
	// Blob traffic belongs to the supervisor, not the wasm component, so large bodies never take an app slot.
	if r.URL.Path == blobPrefix || strings.HasPrefix(r.URL.Path, blobPrefix+"/") {
		s.handleBlobs(w, r, a)
		return
	}
	s.serveApp(w, r, a)
}

// serveApp proxies one request to a resolved app. The public host proxy and the internal app fetch share it, so both get concurrency limits, readiness, static files and access logs.
func (s *supervisor) serveApp(w http.ResponseWriter, r *http.Request, a *app) {
	requestID := r.Header.Get(requestIDHeader)
	if requestID == "" {
		requestID = newRequestID()
		r.Header.Set(requestIDHeader, requestID)
	}
	w.Header().Set(requestIDHeader, requestID)
	started := time.Now()
	if !a.beginRequest() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "app is restarting", http.StatusServiceUnavailable)
		if a.logs != nil && s.shouldLogAccess(a, http.StatusServiceUnavailable) {
			a.logs.logSupervisor(formatAccessLog(r.Method, http.StatusServiceUnavailable, time.Since(started), r.URL.RequestURI(), requestID))
		}
		return
	}
	defer a.requests.Done()
	wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	if s.metrics != nil {
		s.metrics.beginRequest(a.name)
		defer func() {
			s.metrics.endRequest(a.name, r.Method, wrapped.status, time.Since(started))
		}()
	}
	static, ready := a.servingState()

	// Static files come from the bundle and do not consume the app's concurrency slots, but a stopped app does not serve them.
	if static && a.serveStatic(wrapped, r) {
		if a.logs != nil && s.shouldLogAccess(a, wrapped.status) {
			a.logs.logSupervisor(formatAccessLog(r.Method, wrapped.status, time.Since(started), r.URL.RequestURI(), requestID))
		}
		return
	}
	if !ready {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "app not ready", http.StatusServiceUnavailable)
		if a.logs != nil && s.shouldLogAccess(a, http.StatusServiceUnavailable) {
			a.logs.logSupervisor(formatAccessLog(r.Method, http.StatusServiceUnavailable, time.Since(started), r.URL.RequestURI(), requestID))
		}
		return
	}

	a.mu.Lock()
	slots := a.slots
	timeout := a.cfg.requestTimeout
	proxy := a.proxy
	a.mu.Unlock()
	if proxy == nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "app is not ready", http.StatusServiceUnavailable)
		return
	}
	if slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "app is busy", http.StatusTooManyRequests)
			return
		}
	}
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	proxy.ServeHTTP(wrapped, r)
	if a.logs != nil && s.shouldLogAccess(a, wrapped.status) {
		a.logs.logSupervisor(formatAccessLog(r.Method, wrapped.status, time.Since(started), r.URL.RequestURI(), requestID))
	}
}

// shouldLogAccess samples successful access lines while always recording server errors. WT_ACCESS_LOG=off disables the sampled lines but keeps 5xx visible.
func (s *supervisor) shouldLogAccess(a *app, status int) bool {
	if status >= http.StatusInternalServerError {
		return true
	}
	if !s.cfg.accessLog && s.cfg.accessLogEvery != 0 {
		return false
	}
	every := s.cfg.accessLogEvery
	if every <= 1 {
		return true
	}
	return a.accessLines.Add(1)%uint64(every) == 0
}

// isAppHost reports whether host belongs to an app, which owns every path on its host.
func (s *supervisor) isAppHost(host string) bool {
	return s.appForHost(host) != nil
}

// adminMux holds the supervisor's own routes and never proxies app hosts.
func (s *supervisor) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/apps/", s.handleApp)
	mux.HandleFunc("/auth/tokens", s.handleAuthTokens)
	mux.HandleFunc("/auth/tokens/", func(w http.ResponseWriter, r *http.Request) {
		s.handleAuthToken(w, r, strings.TrimPrefix(r.URL.Path, "/auth/tokens/"))
	})
	mux.HandleFunc("/identity/users", s.handleIdentityUsers)
	mux.HandleFunc("/identity/users/", func(w http.ResponseWriter, r *http.Request) {
		s.handleIdentityUser(w, r, strings.TrimPrefix(r.URL.Path, "/identity/users/"))
	})
	mux.HandleFunc("GET /usage", s.handleUsage)
	mux.HandleFunc("GET /audit", s.handleAudit)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	// The panel shell is public so the browser can load the login form; every API route stays authenticated.
	mux.Handle("/admin/", http.StripPrefix("/admin", adminStatic()))
	return mux
}

// adminHandler serves admin/API routes on the separate admin listener.
func (s *supervisor) adminHandler() http.Handler {
	return s.adminMux()
}

// publicHandler serves app hosts and the health check only. Admin routes live on the separate listener.
func (s *supervisor) publicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := s.effectiveHost(r)
		if !s.reservedHost(host) && s.isAppHost(host) {
			s.handleProxy(w, r)
			return
		}
		if r.URL.Path == "/healthz" {
			w.Header().Set("content-type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		http.NotFound(w, r)
	})
}

func (s *supervisor) handler() http.Handler {
	mux := s.adminMux()
	mux.HandleFunc("/", s.handleProxy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := s.effectiveHost(r)
		if s.reservedHost(host) {
			mux.ServeHTTP(w, r)
			return
		}
		if s.isAppHost(host) {
			s.handleProxy(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// statusWriter captures the status code a proxied response is written with. Unwrap hands the underlying writer's optional interfaces (Flusher, Hijacker) through.
type statusWriter struct {
	http.ResponseWriter
	status int
}
