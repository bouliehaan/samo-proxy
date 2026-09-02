package forward

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/bouliehaan/samo-proxy/internal/classify"
)

// LogRequests wraps a handler with one line per request.
//
// This exists because the first real bug report against samo-proxy — a track
// that played silently over the tunnel — could not be diagnosed at all. The
// error log was empty, which proved only that nothing had thrown; whether the
// audio request had even arrived was unanswerable. A proxy that sits between
// two systems and records nothing about what passes through it cannot be
// operated, however clean its error handling is.
//
// Deliberately one line per request rather than sampled or buffered: the
// traffic here is one household, and the cost of a log line is nothing next to
// the cost of not being able to answer "did the request arrive?".
func LogRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		// The Range header is load-bearing for the audio path — a ranged
		// request against a cold cache deliberately bypasses the transcoder —
		// so it is logged rather than left to be guessed at.
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			rangeHeader = "-"
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"route", classify.Of(r.URL.Path).String(),
			"status", recorder.status,
			"bytes", recorder.written,
			"ms", time.Since(started).Milliseconds(),
			"range", rangeHeader,
			"client", remoteIP(r.RemoteAddr),
		}
		// Only present on the audio path, and the whole reason for looking at
		// these logs when a track misbehaves.
		if decision := recorder.Header().Get("X-Samo-Proxy-Transcode"); decision != "" {
			attrs = append(attrs, "transcode", decision)
		}
		if cache := recorder.Header().Get("X-Samo-Proxy-Cache"); cache != "" {
			attrs = append(attrs, "cache", cache)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "" {
			attrs = append(attrs, "type", contentType)
		}
		if length := recorder.Header().Get("Content-Length"); length != "" {
			attrs = append(attrs, "len", length)
		}

		level := slog.LevelInfo
		if recorder.status >= 500 {
			level = slog.LevelError
		} else if recorder.status >= 400 {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "request", attrs...)
	})
}

// statusRecorder remembers what was actually sent, since a ResponseWriter will
// not tell you afterwards.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// Flush must be forwarded or an endless radio stream and the SSE channel both
// stall behind this wrapper.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
