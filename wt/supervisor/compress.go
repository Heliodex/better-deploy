// On-the-fly gzip for proxied JSON, NDJSON and text responses.

package main

import (
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

type acceptEncodingKey struct{}

// compressibleMediaType reports whether a dynamic response of this type is worth gzipping. SSE stays plain, because browsers and proxies expect each event to arrive as soon as it is written.
func compressibleMediaType(mediaType string) bool {
	switch {
	case mediaType == "text/event-stream":
		return false
	case strings.HasPrefix(mediaType, "text/"), strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	}

	switch mediaType {
	case "application/json", "application/x-ndjson", "application/jsonl", "application/javascript", "application/xml", "image/svg+xml":
		return true
	}

	return false
}

// copyCompressed copies src into gz. A streamed body is flushed after every read, so each chunk the app writes reaches the client instead of waiting for gzip to fill a block.
func copyCompressed(gz *gzip.Writer, src io.Reader, stream bool) error {
	if !stream {
		_, err := io.Copy(gz, src)
		return err
	}

	buf := make([]byte, 32<<10)

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := gz.Write(buf[:n]); err != nil {
				return err
			}

			if err := gz.Flush(); err != nil {
				return err
			}
		}

		if errors.Is(readErr, io.EOF) {
			return nil
		}

		if readErr != nil {
			return readErr
		}
	}
}

// compressResponse gzips a buffered or streamed JSON, NDJSON or text response when the client accepts it. SSE, already encoded bodies, no-transform responses and tiny known bodies stay untouched.
func compressResponse(res *http.Response, acceptEncoding string) error {
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

	if !compressibleMediaType(mediaType) {
		return nil
	}

	if res.ContentLength >= 0 && res.ContentLength <= 256 {
		return nil
	}

	original := res.Body
	stream := res.ContentLength < 0
	reader, writer := io.Pipe()
	gz := gzip.NewWriter(writer)
	res.Body = &compressedBody{PipeReader: reader, original: original}
	res.Header.Set("Content-Encoding", "gzip")
	res.Header.Del("Content-Length")
	res.ContentLength = -1
	addVary(res.Header, "Accept-Encoding")

	go func() {
		copyErr := copyCompressed(gz, original, stream)
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
