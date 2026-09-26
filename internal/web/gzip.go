package web

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// Pages are compressed for browsers that accept it. A list page repeats the
// same markup for every task, so it shrinks by about ten times, and it is
// sent again whenever anyone changes anything, so on a phone that adds up.
//
// Left alone: the live-update stream, which must reach the browser the moment
// it is written rather than when a compressor has enough to fill a block;
// fonts and images, which are compressed already; and requests for part of a
// file, whose byte ranges would refer to the uncompressed file.

var gzips = sync.Pool{New: func() any {
	gz, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return gz
}}

// wantsGzip reports whether r's response may be compressed.
func wantsGzip(r *http.Request) bool {
	if r.URL.Path == "/events" || r.Header.Get("Range") != "" {
		return false
	}
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") {
			return true
		}
	}
	return false
}

// compressible reports whether a response of this type is worth compressing.
func compressible(contentType string) bool {
	t, _, _ := strings.Cut(contentType, ";")
	t = strings.TrimSpace(strings.ToLower(t))
	switch {
	case t == "text/event-stream":
		return false
	case strings.HasPrefix(t, "text/"):
		return true
	}
	switch t {
	case "application/json", "application/javascript", "application/manifest+json", "image/svg+xml":
		return true
	}
	return false
}

// gzipResponse compresses what a handler writes, once its headers show that
// it is worth it. The decision waits for the headers because a handler may
// set its Content-Type at any point before it starts writing.
type gzipResponse struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (g *gzipResponse) WriteHeader(code int) {
	if !g.decided {
		g.decide(code)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponse) decide(code int) {
	g.decided = true
	h := g.Header()
	if code < 200 || code == http.StatusNoContent || code == http.StatusNotModified ||
		h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) {
		return
	}
	h.Del("Content-Length") // it would be the uncompressed length
	h.Set("Content-Encoding", "gzip")
	g.gz = gzips.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
}

func (g *gzipResponse) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b)) // as net/http would
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush sends what has been written so far, compressed or not.
func (g *gzipResponse) Flush() {
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection underneath.
func (g *gzipResponse) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// finish ends the compressed stream, if there is one.
func (g *gzipResponse) finish() {
	if g.gz != nil {
		g.gz.Close()
		gzips.Put(g.gz)
		g.gz = nil
	}
}
