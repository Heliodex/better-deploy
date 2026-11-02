// On-the-fly gzip for proxied JSON responses.

package main

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

type acceptEncodingKey struct{}

// compressJSONResponse gzips a buffered or streamed JSON response when the client accepts it. SSE, already encoded bodies, no-transform responses and tiny known bodies stay untouched.
func compressJSONResponse(res *http.Response, acceptEncoding string) error {
	if res == nil || res.Body == nil || res.Request == nil || res.Request.Method == http.MethodHead {
		return nil
	}
	if res.StatusCode < http.StatusOK || res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusNotModified {
		return nil
	}
	if res.Header.Get("Content-Encoding") != "" || !acceptsEncoding(acceptEncoding, "gzip") {
		return nil
	}
	if strings.Contains(strings.ToLower(res.Header.Get("Cache-Control")), "no-transform") {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return nil
	}
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return nil
	}
	if res.ContentLength >= 0 && res.ContentLength <= 256 {
		return nil
	}

	original := res.Body
	reader, writer := io.Pipe()
	gz := gzip.NewWriter(writer)
	res.Body = &compressedBody{PipeReader: reader, original: original}
	res.Header.Set("Content-Encoding", "gzip")
	res.Header.Del("Content-Length")
	res.ContentLength = -1
	addVary(res.Header, "Accept-Encoding")
	go func() {
		_, copyErr := io.Copy(gz, original)
		closeErr := gz.Close()
		_ = original.Close()
		_ = writer.CloseWithError(firstError(copyErr, closeErr))
	}()
	return nil
}

type compressedBody struct {
	*io.PipeReader
	original io.ReadCloser
	once     sync.Once
}

func (b *compressedBody) Close() error {
	b.once.Do(func() {
		_ = b.PipeReader.Close()
		_ = b.original.Close()
	})
	return nil
}

func firstError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func addVary(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		for token := range strings.SplitSeq(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(token), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}
