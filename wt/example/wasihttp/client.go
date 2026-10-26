package wasihttp

import (
	"fmt"
	"io"
	"net/http"

	client "wt.example/binding/imports/wasi_http_0_3_0_client"
	wasi "wt.example/binding/imports/wasi_http_0_3_0_types"
)

// Transport implements [http.RoundTripper] using wasi:http.
type Transport struct{}

// Send sends an HTTP request using wasi:http and returns the response.
func Send(req *http.Request) (*http.Response, error) {
	request, err := newOutgoingHttpRequest(req)
	if err != nil {
		return nil, err
	}
	defer request.Drop()

	result := client.Send(request)
	if result.IsErr() {
		return nil, fmt.Errorf("error sending request: %s", errorString(result.Err()))
	}

	response := result.Ok()
	status := response.GetStatusCode()

	headerResource := response.GetHeaders()
	headers := headerResource.CopyAll()
	headerResource.Drop()

	rx, trailers := wasi.ResponseConsumeBody(response, unitFuture())
	body := newReader(rx, trailers)

	resp := &http.Response{
		StatusCode: int(status),
		Body:       body,
		Header:     http.Header{},
	}

	toHttpHeader(headers, &resp.Header)

	return resp, nil
}

// RoundTrip executes a single HTTP transaction using wasi:http.
func (r *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return Send(req)
}

// NewTransport returns an [http.RoundTripper] backed by wasi:http.
func NewTransport() http.RoundTripper {
	return &Transport{}
}

// NewClient returns a new HTTP client compatible with wasi:http.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &Transport{},
	}
}

// Get issues a GET request to the specified URL using wasi:http.
func Get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	return Send(req)
}

// Post issues a POST request to the specified URL using wasi:http with the given content type and body.
func Post(url string, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return Send(req)
}
