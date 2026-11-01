// Package httpmeta derives the standard net/http request view from the Spin and forwarded headers.
package httpmeta

import (
	"net/http"
	"net/url"
	"strings"
)

// IncomingTarget returns the scheme, authority, path and client address a WASI HTTP request should expose to a net/http handler.
func IncomingTarget(pathWithQuery string, headers http.Header) (scheme, host, path, remote string) {
	scheme = headers.Get("X-Forwarded-Proto")
	host = headers.Get("Host")
	if host == "" {
		host = headers.Get("X-Forwarded-Host")
	}
	path = pathWithQuery

	if full := headers.Get("spin-full-url"); full != "" {
		if parsed, err := url.Parse(full); err == nil {
			if parsed.Scheme != "" {
				scheme = parsed.Scheme
			}
			if parsed.Host != "" {
				host = parsed.Host
			}
			if parsed.Path != "" || parsed.RawQuery != "" {
				path = parsed.RequestURI()
			}
		}
	}
	if scheme == "" {
		scheme = "http"
	}
	remote = headers.Get("spin-client-addr")
	if remote == "" {
		if forwarded := headers.Get("X-Forwarded-For"); forwarded != "" {
			remote = strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}
	return scheme, host, path, remote
}
