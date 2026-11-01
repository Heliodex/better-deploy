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
		// A hung app must not hold client connections forever: header and dial deadlines bound the wait before the response starts.
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: proxyResponseHeaderTimeout,
			IdleConnTimeout:       proxyIdleConnTimeout,
			MaxIdleConnsPerHost:   proxyMaxIdlePerHost,
			ExpectContinueTimeout: proxyExpectContinue,
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			host := r.In.Host
			proto := r.In.Header.Get("X-Forwarded-Proto")
			if proto == "" {
				proto = "http"
				if r.In.TLS != nil {
					proto = "https"
				}
			}
			r.SetURL(target)
			r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
			r.SetXForwarded()
			r.Out.Host = host
			r.Out.Header.Set("X-Forwarded-Host", host)
			r.Out.Header.Set("X-Forwarded-Proto", proto)
			// The Spin host headers give the component the standard net/http view of the original request.
			r.Out.Header.Set(spinFullURLHeader, proto+"://"+host+r.In.URL.RequestURI())
			r.Out.Header.Set(spinClientAddrHeader, r.In.RemoteAddr)
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

// formatAccessLog renders a supervisor access line as METHOD status duration path, so the panel can recognise and structure it.
func formatAccessLog(method string, status int, duration time.Duration, uri string) string {
	return fmt.Sprintf("%s %d %s %s", method, status, formatProxyDuration(duration), uri)
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
	host := requestHost(r)
	a := s.appForHost(host)
	if a == nil {
		http.Error(w, "no app for this host", http.StatusNotFound)
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
		if a.logs != nil {
			a.logs.logSupervisor(formatAccessLog(r.Method, http.StatusServiceUnavailable, time.Since(started), r.URL.RequestURI()))
		}
		return
	}
	defer a.requests.Done()
	wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	static, ready := a.servingState()

	// Static files come from the bundle and do not consume the app's concurrency slots, but a stopped app does not serve them.
	if static && a.serveStatic(wrapped, r) {
		if a.logs != nil {
			a.logs.logSupervisor(formatAccessLog(r.Method, wrapped.status, time.Since(started), r.URL.RequestURI()))
		}
		return
	}
	if !ready {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "app not ready", http.StatusServiceUnavailable)
		if a.logs != nil {
			a.logs.logSupervisor(formatAccessLog(r.Method, http.StatusServiceUnavailable, time.Since(started), r.URL.RequestURI()))
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
	if a.logs != nil {
		a.logs.logSupervisor(formatAccessLog(r.Method, wrapped.status, time.Since(started), r.URL.RequestURI()))
	}
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
	mux.HandleFunc("GET /usage", s.handleUsage)
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
		host := requestHost(r)
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
		host := requestHost(r)
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
