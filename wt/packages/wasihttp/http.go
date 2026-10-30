// Package wasihttp contains the helper functions for writing WASI HTTP components in Go, as well as for sending outbound HTTP requests.
package wasihttp

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	wit "go.bytecodealliance.org/pkg/wit/types"
	handler "wt.packages/binding/exports/export_wasi_http_0_3_0_handler"
	_ "wt.packages/binding/exports/wit_exports"
	wasi "wt.packages/binding/imports/wasi_http_0_3_0_types"
)

const (
	// HeaderBasePath is the application base path.
	HeaderBasePath = "spin-base-path"
	// HeaderComponentRoot is the component route pattern matched, excluding any wildcard indicator.
	HeaderComponentRoot = "spin-component-route"
	// HeaderFullUrl is the full URL of the request, including full host and scheme information.
	HeaderFullUrl = "spin-full-url"
	// HeaderMatchedRoute is the part of the request path that was matched by the route, including the base and wildcard indicator if present.
	HeaderMatchedRoute = "spin-matched-route"
	// HeaderPathInfo is the request path relative to the component route, including any base.
	HeaderPathInfo = "spin-path-info"
	// HeaderRawComponentRoot is the component route pattern matched, as written in the component manifest. It excludes the base but includes the wildcard indicator if present.
	HeaderRawComponentRoot = "spin-raw-component-route"
	// HeaderClientAddr is the client address for the request.
	HeaderClientAddr = "spin-client-addr"
)

// defaultHandler is a placeholder for returning a useful error to stderr when the handler is not set.
var defaultHandler = func(http.ResponseWriter, *http.Request) {
	fmt.Fprintln(os.Stderr, "http handler undefined")
}

// handlerFn is the function called by the HTTP trigger.
var handlerFn = defaultHandler

// Handle sets the handler function for the http trigger. It must be set in an init() function.
func Handle(fn func(http.ResponseWriter, *http.Request)) {
	handlerFn = fn
}

// handleErr is wit.Err instantiated for the handler result, so the call sites don't need explicit type arguments.
var handleErr = wit.Err[*wasi.Response, wasi.ErrorCode]

var wasiHandle = func(request *wasi.Request) wit.Result[*wasi.Response, wasi.ErrorCode] {
	httpRes := newHttpResponseWriter()

	go func() {
		defer httpRes.close()

		// convert the incoming request to go's net/http type
		httpReq, err := newHttpRequest(request)
		if err != nil {
			httpRes.channel <- handleErr(
				wasi.MakeErrorCodeInternalError(wit.Some(fmt.Sprintf(
					"failed to convert WASI Request to http.Request: %v\n",
					err,
				))),
			)
			return
		}
		defer httpReq.Body.Close()

		// run the user's handler
		handlerFn(httpRes, httpReq)

		// If the user's handler never sent a response, send a default one here.
		if err := httpRes.send(); err != nil {
			httpRes.channel <- handleErr(
				wasi.MakeErrorCodeInternalError(wit.Some(fmt.Sprintf(
					"failed to produce a response: %v\n",
					err,
				))),
			)
		}
	}()

	return (<-httpRes.channel)
}

func init() {
	handler.Exports.Handle = wasiHandle
}

func toWasiHeaders(headers http.Header) (*wasi.Fields, error) {
	fields := wasi.MakeFields()

	for key, vals := range headers {
		fieldVals := [][]uint8{}
		for _, val := range vals {
			fieldVals = append(fieldVals, []uint8(val))
		}

		if result := fields.Set(key, fieldVals); result.IsErr() {
			fields.Drop()
			switch result.Err().Tag() {
			case wasi.HeaderErrorInvalidSyntax:
				return nil, fmt.Errorf(
					"failed to set header %s to [%s]: invalid syntax",
					key,
					strings.Join(vals, ","),
				)
			case wasi.HeaderErrorForbidden:
				return nil, fmt.Errorf("failed to set forbidden header key %s", key)
			case wasi.HeaderErrorImmutable:
				return nil, fmt.Errorf("failed to set header on immutable header fields")
			default:
				return nil, fmt.Errorf("error setting header %s", key)
			}
		}
	}

	return fields, nil
}

func trailersFuture() *wit.FutureReader[wit.Result[wit.Option[*wasi.Fields], wasi.ErrorCode]] {
	tx, rx := wasi.MakeFutureResultOptionFieldsErrorCode()
	go tx.Write(wit.Ok[wit.Option[*wasi.Fields], wasi.ErrorCode](wit.None[*wasi.Fields]()))
	return rx
}

func unitFuture() *wit.FutureReader[wit.Result[wit.Unit, wasi.ErrorCode]] {
	tx, rx := wasi.MakeFutureResultUnitErrorCode()
	go tx.Write(wit.Ok[wit.Unit, wasi.ErrorCode](wit.Unit{}))
	return rx
}

func fieldSizeString(p wasi.FieldSizePayload) string {
	var parts []string
	if p.FieldName.IsSome() {
		parts = append(parts, fmt.Sprintf("field-name=%s", p.FieldName.Some()))
	}
	if p.FieldSize.IsSome() {
		parts = append(parts, fmt.Sprintf("field-size=%d", p.FieldSize.Some()))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// simpleErrorString describes error codes whose payload carries no extra detail.
func simpleErrorString(tag uint8) (string, bool) {
	switch tag {
	case wasi.ErrorCodeDnsTimeout:
		return "DNS timeout", true
	case wasi.ErrorCodeDestinationNotFound:
		return "destination not found", true
	case wasi.ErrorCodeDestinationUnavailable:
		return "destination unavailable", true
	case wasi.ErrorCodeDestinationIpProhibited:
		return "destination IP prohibited", true
	case wasi.ErrorCodeDestinationIpUnroutable:
		return "destination IP unroutable", true
	case wasi.ErrorCodeConnectionRefused:
		return "connection refused", true
	case wasi.ErrorCodeConnectionTerminated:
		return "connection terminated", true
	case wasi.ErrorCodeConnectionTimeout:
		return "connection timeout", true
	case wasi.ErrorCodeConnectionReadTimeout:
		return "connection read timeout", true
	case wasi.ErrorCodeConnectionWriteTimeout:
		return "connection write timeout", true
	case wasi.ErrorCodeConnectionLimitReached:
		return "connection limit reached", true
	case wasi.ErrorCodeTlsProtocolError:
		return "TLS protocol error", true
	case wasi.ErrorCodeTlsCertificateError:
		return "TLS certificate error", true
	case wasi.ErrorCodeHttpRequestDenied:
		return "HTTP request denied", true
	case wasi.ErrorCodeHttpRequestLengthRequired:
		return "HTTP request length required", true
	case wasi.ErrorCodeHttpRequestMethodInvalid:
		return "HTTP request method invalid", true
	case wasi.ErrorCodeHttpRequestUriInvalid:
		return "HTTP request URI invalid", true
	case wasi.ErrorCodeHttpRequestUriTooLong:
		return "HTTP request URI too long", true
	case wasi.ErrorCodeHttpResponseIncomplete:
		return "HTTP response incomplete", true
	case wasi.ErrorCodeHttpResponseTimeout:
		return "HTTP response timeout", true
	case wasi.ErrorCodeHttpUpgradeFailed:
		return "HTTP upgrade failed", true
	case wasi.ErrorCodeHttpProtocolError:
		return "HTTP protocol error", true
	case wasi.ErrorCodeLoopDetected:
		return "loop detected", true
	case wasi.ErrorCodeConfigurationError:
		return "configuration error", true
	}
	return "", false
}

// optionErrorString appends an option's value to a description when it is present.
func optionErrorString[T any](prefix string, value wit.Option[T]) string {
	if value.IsSome() {
		return fmt.Sprintf("%s: %v", prefix, value.Some())
	}
	return prefix
}

// fieldSizeOptionString describes an optional field-size payload.
func fieldSizeOptionString(prefix string, value wit.Option[wasi.FieldSizePayload]) string {
	if value.IsSome() {
		return prefix + " " + fieldSizeString(value.Some())
	}
	return prefix
}

func dnsErrorString(p wasi.DnsErrorPayload) string {
	var parts []string
	if p.Rcode.IsSome() {
		parts = append(parts, fmt.Sprintf("rcode=%s", p.Rcode.Some()))
	}
	if p.InfoCode.IsSome() {
		parts = append(parts, fmt.Sprintf("info-code=%d", p.InfoCode.Some()))
	}
	if len(parts) == 0 {
		return "DNS error"
	}
	return "DNS error (" + strings.Join(parts, ", ") + ")"
}

func tlsAlertString(p wasi.TlsAlertReceivedPayload) string {
	var parts []string
	if p.AlertId.IsSome() {
		parts = append(parts, fmt.Sprintf("alert-id=%d", p.AlertId.Some()))
	}
	if p.AlertMessage.IsSome() {
		parts = append(parts, fmt.Sprintf("alert-message=%s", p.AlertMessage.Some()))
	}
	if len(parts) == 0 {
		return "TLS alert received"
	}
	return "TLS alert received (" + strings.Join(parts, ", ") + ")"
}

func errorString(code wasi.ErrorCode) string {
	if message, ok := simpleErrorString(code.Tag()); ok {
		return message
	}
	switch code.Tag() {
	case wasi.ErrorCodeDnsError:
		return dnsErrorString(code.DnsError())
	case wasi.ErrorCodeTlsAlertReceived:
		return tlsAlertString(code.TlsAlertReceived())
	case wasi.ErrorCodeHttpRequestBodySize:
		return optionErrorString("HTTP request body size", code.HttpRequestBodySize())
	case wasi.ErrorCodeHttpRequestHeaderSectionSize:
		return optionErrorString("HTTP request header section size", code.HttpRequestHeaderSectionSize())
	case wasi.ErrorCodeHttpRequestHeaderSize:
		return fieldSizeOptionString("HTTP request header size", code.HttpRequestHeaderSize())
	case wasi.ErrorCodeHttpRequestTrailerSectionSize:
		return optionErrorString("HTTP request trailer section size", code.HttpRequestTrailerSectionSize())
	case wasi.ErrorCodeHttpRequestTrailerSize:
		return "HTTP request trailer size " + fieldSizeString(code.HttpRequestTrailerSize())
	case wasi.ErrorCodeHttpResponseHeaderSectionSize:
		return optionErrorString("HTTP response header section size", code.HttpResponseHeaderSectionSize())
	case wasi.ErrorCodeHttpResponseHeaderSize:
		return "HTTP response header size " + fieldSizeString(code.HttpResponseHeaderSize())
	case wasi.ErrorCodeHttpResponseBodySize:
		return optionErrorString("HTTP response body size", code.HttpResponseBodySize())
	case wasi.ErrorCodeHttpResponseTrailerSectionSize:
		return optionErrorString("HTTP response trailer section size", code.HttpResponseTrailerSectionSize())
	case wasi.ErrorCodeHttpResponseTrailerSize:
		return "HTTP response trailer size " + fieldSizeString(code.HttpResponseTrailerSize())
	case wasi.ErrorCodeHttpResponseTransferCoding:
		return optionErrorString("HTTP response transfer coding", code.HttpResponseTransferCoding())
	case wasi.ErrorCodeHttpResponseContentCoding:
		return optionErrorString("HTTP response content coding", code.HttpResponseContentCoding())
	case wasi.ErrorCodeInternalError:
		return optionErrorString("internal error", code.InternalError())
	default:
		return fmt.Sprintf("unknown error code: %d", code.Tag())
	}
}
