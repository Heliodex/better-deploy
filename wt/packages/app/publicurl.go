package app

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// PublicURL returns the browser-facing origin of the app. It prefers WT_PUBLIC_URL, then a same-origin Origin or Referer header from the browser, and falls back to the scheme and host the supervisor passed in the request.
func PublicURL(r *http.Request) string {
	if value := strings.TrimRight(strings.TrimSpace(os.Getenv("WT_PUBLIC_URL")), "/"); value != "" {
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			return value
		}
	}

	scheme := "http"
	if r.TLS != nil || r.URL.Scheme == "https" || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}

	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	if host == "" {
		return ""
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		if parsed, err := url.Parse(origin); err == nil && browserOrigin(parsed, host) {
			return parsed.Scheme + "://" + parsed.Host
		}
	}

	if referer := r.Referer(); referer != "" {
		if parsed, err := url.Parse(referer); err == nil && browserOrigin(parsed, host) {
			return parsed.Scheme + "://" + parsed.Host
		}
	}

	return scheme + "://" + host
}

// browserOrigin accepts an http(s) origin whose host is the request host, or a loopback origin for the local dev server.
func browserOrigin(parsed *url.URL, requestHost string) bool {
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}

	if strings.EqualFold(parsed.Host, requestHost) {
		return true
	}

	return loopbackHost(parsed.Host) && loopbackHost(requestHost)
}

// hostname strips a port from a Host or authority.
func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}

	return host
}

// loopbackHost reports whether a host is a local development name.
func loopbackHost(host string) bool {
	name := strings.Trim(strings.ToLower(hostname(host)), "[]")

	return name == "localhost" || strings.HasSuffix(name, ".localhost") || name == "127.0.0.1" || name == "::1"
}
