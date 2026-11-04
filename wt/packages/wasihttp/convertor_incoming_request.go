package wasihttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strconv"

	wit "go.bytecodealliance.org/pkg/wit/types"
	wasi "github.com/heliodex/better-deploy/wt/packages/v2/binding/imports/wasi_http_0_3_0_types"
	"github.com/heliodex/better-deploy/wt/packages/v2/httpmeta"
)

// methodToString maps a standard wasi:http method to its HTTP name and rejects the "other" variant.
func methodToString(m wasi.Method) (string, error) {
	switch m.Tag() {
	case wasi.MethodConnect:
		return "CONNECT", nil
	case wasi.MethodDelete:
		return "DELETE", nil
	case wasi.MethodGet:
		return "GET", nil
	case wasi.MethodHead:
		return "HEAD", nil
	case wasi.MethodOptions:
		return "OPTIONS", nil
	case wasi.MethodPatch:
		return "PATCH", nil
	case wasi.MethodPost:
		return "POST", nil
	case wasi.MethodPut:
		return "PUT", nil
	case wasi.MethodTrace:
		return "TRACE", nil
	case wasi.MethodOther:
		return m.Other(), errors.New("unknown http method 'other'")
	default:
		return "", errors.New("failed to convert http method")
	}
}

// toHTTPHeader appends wasi header pairs to dest, preserving repeated fields.
func toHTTPHeader(src []wit.Tuple2[string, []uint8], dest *http.Header) {
	for _, pair := range src {
		key := pair.F0
		value := string(pair.F1)
		dest.Add(key, value)
	}
}

// newHTTPRequest converts a wasi.Request to an http.Request. The request uses the handler's context so a disconnect can cancel the app's work.
func newHTTPRequest(ctx context.Context, ir *wasi.Request) (*http.Request, error) {
	defer ir.Drop()

	method, err := methodToString(ir.GetMethod())
	if err != nil {
		return nil, err
	}

	pathWithQuery := ""
	if value := ir.GetPathWithQuery(); value.IsSome() {
		pathWithQuery = value.Some()
	}

	headerResource := ir.GetHeaders()
	rawHeaders := headerResource.CopyAll()
	headerResource.Drop()

	headers := make(http.Header, len(rawHeaders))
	toHTTPHeader(rawHeaders, &headers)

	rx, trailers := wasi.RequestConsumeBody(ir, unitFuture())
	body := newReader(rx, trailers)

	scheme, host, requestPath, remote := httpmeta.IncomingTarget(pathWithQuery, headers)

	rawURL := requestPath
	if rawURL == "" {
		rawURL = "/"
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		body.Close()
		return nil, err
	}

	req.Header = headers
	req.Header.Del("Host")
	req.URL.Scheme = scheme
	req.URL.Host = host
	req.Host = host
	req.RequestURI = req.URL.RequestURI()

	req.RemoteAddr = remote
	if value := req.Header.Get("Content-Length"); value != "" {
		if length, err := strconv.ParseInt(value, 10, 64); err == nil {
			req.ContentLength = length
		}
	}

	if scheme == "https" && req.TLS == nil {
		req.TLS = &tls.ConnectionState{}
	}

	return req, nil
}
