// Package compress gzips responses on their way back to Cloudflare.
//
// samo-server has no compression middleware at all — its whole chain is
// WithSecurityHeaders(WithCORS(server)) — so every JSON response leaves the
// house raw. Cloudflare does compress, but it compresses at its edge, which is
// on the far side of the uplink this proxy exists to protect. Compressing here
// is the only place it saves anything, and it is the single cheapest win
// available: the Android client's first catalog sync moves a lot of JSON, and
// JSON gzips five to ten times over.
//
// What is NOT compressed matters as much as what is. Audio and images are
// already compressed and gzipping them burns CPU to add bytes. Server-sent
// events must never be buffered, and a compressor is a buffer by nature.
package compress

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// compressibleTypes are the response content types worth gzipping.
var compressibleTypes = []string{
	"application/json",
	"application/xml",
	"application/javascript",
	"text/",
	"image/svg+xml",
	"+json",
	"+xml",
}

var writerPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// Middleware returns a handler that gzips eligible responses from next.
//
// minBytes suppresses compression for tiny bodies, where the gzip header costs
// more than the encoding saves.
func Middleware(minBytes int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		writer := &responseWriter{ResponseWriter: w, minBytes: minBytes}
		defer writer.Close()
		next.ServeHTTP(writer, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		// "gzip;q=0" is a refusal, not an offer.
		fields := strings.Split(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			for _, param := range fields[1:] {
				if strings.EqualFold(strings.TrimSpace(param), "q=0") {
					return false
				}
			}
			return true
		}
	}
	return false
}

// responseWriter decides on first write whether to compress, then commits to
// that decision for the rest of the response.
type responseWriter struct {
	http.ResponseWriter
	minBytes int

	decided bool
	gzip    *gzip.Writer
	buffer  []byte
	status  int
	// passthrough is set once we know this response will not be compressed, so
	// later writes skip the decision entirely.
	passthrough bool
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	// The header is not sent yet: the compression decision needs the body size,
	// and sending headers now would freeze Content-Length before we know
	// whether it still applies. flushHeader below writes it.
	if w.decided {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.Write(p)
	}
	if w.gzip != nil {
		return w.gzip.Write(p)
	}

	// Not yet decided: accumulate until there is enough to judge, or until the
	// content type alone rules compression out.
	if !w.eligible() {
		w.commitPlain()
		return w.ResponseWriter.Write(p)
	}
	w.buffer = append(w.buffer, p...)
	if len(w.buffer) >= w.minBytes {
		w.commitGzip()
	}
	return len(p), nil
}

// Flush is what makes an event stream work through this wrapper. Reaching it
// means the handler wants bytes on the wire now, which is incompatible with
// waiting to see if the body grows past minBytes — so the decision has to be
// taken here, on what is known rather than on the body's final size.
func (w *responseWriter) Flush() {
	if !w.decided {
		w.commitFlushed()
	}
	if w.gzip != nil {
		_ = w.gzip.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseWriter) eligible() bool {
	header := w.Header()
	// An origin that already encoded the body owns that decision.
	if header.Get("Content-Encoding") != "" {
		return false
	}
	contentType := strings.ToLower(header.Get("Content-Type"))
	// An event stream is explicitly excluded even though it is text/*: it must
	// reach the client the instant it is written.
	if strings.Contains(contentType, "text/event-stream") {
		return false
	}
	for _, candidate := range compressibleTypes {
		if strings.Contains(contentType, candidate) {
			return true
		}
	}
	return false
}

// commitFlushed resolves a response that was flushed before its body was big
// enough to judge.
//
// Resolving this as "do not compress" is what silently switched the whole
// package off in production. The reverse proxy runs with FlushInterval -1 —
// which an endless radio stream and the SSE channel both need — and that means
// httputil flushes after EVERY write, including the first one. So a JSON
// response whose first read off the origin came back under minBytes was
// committed to plain before the rest of it had even arrived, and samo-server
// sends nearly everything chunked. Measured against the real binary: a 4700
// byte catalog response left the proxy as 4700 bytes, where the same body
// through the same middleware without the proxy in front leaves as 99.
//
// The content type still decides first, and that is what keeps the streaming
// paths untouched: text/event-stream is excluded by eligible(), and so is
// every audio and image type, so a live channel and the dashboard's SSE feed
// resolve to plain here exactly as before. What is left is the compressible
// case, where a flush means "this is being streamed to me in pieces" — and a
// streamed JSON body is the one this proxy exists to shrink.
//
// A declared Content-Length below the threshold is the one case where the
// answer is still knowable, and it keeps the tiny-body rule exact rather than
// approximate.
func (w *responseWriter) commitFlushed() {
	if !w.eligible() {
		w.commitPlain()
		return
	}
	if declared, err := strconv.Atoi(w.Header().Get("Content-Length")); err == nil && declared < w.minBytes {
		w.commitPlain()
		return
	}
	w.commitGzip()
}

func (w *responseWriter) commitPlain() {
	if w.decided {
		return
	}
	w.decided = true
	w.passthrough = true
	w.ResponseWriter.WriteHeader(w.statusOrOK())
	if len(w.buffer) > 0 {
		_, _ = w.ResponseWriter.Write(w.buffer)
		w.buffer = nil
	}
}

func (w *responseWriter) commitGzip() {
	if w.decided {
		return
	}
	w.decided = true
	header := w.Header()
	header.Set("Content-Encoding", "gzip")
	// Without Vary, a shared cache could hand a gzipped body to a client that
	// never asked for one.
	addVary(header, "Accept-Encoding")
	// The length of the compressed body is not known yet, and the origin's
	// length describes the body we are about to replace.
	header.Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.statusOrOK())

	writer := writerPool.Get().(*gzip.Writer)
	writer.Reset(w.ResponseWriter)
	w.gzip = writer
	if len(w.buffer) > 0 {
		_, _ = w.gzip.Write(w.buffer)
		w.buffer = nil
	}
}

func (w *responseWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Close resolves an undecided response — a body smaller than minBytes never
// triggered a decision — and releases the gzip writer.
func (w *responseWriter) Close() {
	if !w.decided {
		w.commitPlain()
		return
	}
	if w.gzip != nil {
		_ = w.gzip.Close()
		writerPool.Put(w.gzip)
		w.gzip = nil
	}
}

func addVary(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		if strings.EqualFold(strings.TrimSpace(existing), value) {
			return
		}
	}
	header.Add("Vary", value)
}
