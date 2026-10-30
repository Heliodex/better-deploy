package main

import (
	"context"
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
)

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

func (s *supervisor) handleProxy(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	s.mu.RLock()
	a := s.hosts[host]
	if a != nil {
		a.requests.Add(1)
	}
	s.mu.RUnlock()
	if a == nil {
		http.Error(w, "no app for this host", http.StatusNotFound)
		return
	}
	defer a.requests.Done()

	a.mu.Lock()
	slots := a.slots
	timeout := a.cfg.requestTimeout
	a.mu.Unlock()
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

	started := time.Now()
	wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	a.proxy.ServeHTTP(wrapped, r)
	if a.logs != nil {
		a.logs.logSupervisor(formatAccessLog(r.Method, wrapped.status, time.Since(started), r.URL.RequestURI()))
	}
}

// isAppHost reports whether host belongs to an app, which owns every path on its host.
func (s *supervisor) isAppHost(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hosts[host] != nil
}

func (s *supervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/apps/", s.handleApp)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.Handle("/admin/", http.StripPrefix("/admin", adminStatic()))
	mux.HandleFunc("/", s.handleProxy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isAppHost(requestHost(r)) {
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
