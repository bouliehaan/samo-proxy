package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, minBytes int, acceptEncoding string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/albums", nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	recorder := httptest.NewRecorder()
	Middleware(minBytes, handler).ServeHTTP(recorder, request)
	return recorder
}

// The point of the package: samo-server sends JSON raw, and the catalog sync
// moves a lot of it across the uplink.
func TestJSONIsCompressed(t *testing.T) {
	body := strings.Repeat(`{"id":"album","title":"a record"},`, 200)
	recorder := serve(t, 1024, "gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})

	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if recorder.Body.Len() >= len(body) {
		t.Errorf("gzip did not shrink the body: %d -> %d", len(body), recorder.Body.Len())
	}

	reader, err := gzip.NewReader(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != body {
		t.Error("decompressed body did not round-trip")
	}
	if !containsFold(recorder.Header().Values("Vary"), "Accept-Encoding") {
		t.Error("Vary: Accept-Encoding was not set")
	}
}

// Audio and images are already compressed; gzipping them burns CPU to add
// bytes.
func TestAlreadyCompressedTypesAreLeftAlone(t *testing.T) {
	for _, contentType := range []string{"audio/ogg", "audio/flac", "image/jpeg", "video/mp4"} {
		recorder := serve(t, 1, "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Write(bytes.Repeat([]byte("x"), 4096))
		})
		if got := recorder.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s was compressed (Content-Encoding %q)", contentType, got)
		}
	}
}

// An event stream must reach the client the instant it is written, and a
// compressor is a buffer by nature. samo-server's 25s SSE heartbeat exists to
// survive Cloudflare's 100s idle reap — holding it back here would defeat that.
func TestEventStreamIsNeverCompressedOrBuffered(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()

	Middleware(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": connected\n\n")
		w.(http.Flusher).Flush()
	})).ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("event stream was compressed (Content-Encoding %q)", got)
	}
	if !recorder.Flushed {
		t.Error("event stream was not flushed through the wrapper")
	}
	if body := recorder.Body.String(); body != ": connected\n\n" {
		t.Errorf("body = %q, want the raw SSE comment", body)
	}
}

// A client that did not offer gzip must not receive it.
func TestGzipIsNotAppliedWithoutAcceptEncoding(t *testing.T) {
	recorder := serve(t, 1, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, strings.Repeat("a", 4096))
	})
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a client that did not accept gzip", got)
	}
}

// "gzip;q=0" is a refusal, not an offer.
func TestGzipQualityZeroIsARefusal(t *testing.T) {
	recorder := serve(t, 1, "gzip;q=0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, strings.Repeat("a", 4096))
	})
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q despite q=0", got)
	}
}

// Below the threshold the gzip header costs more than the encoding saves.
func TestTinyBodiesAreNotCompressed(t *testing.T) {
	recorder := serve(t, 1024, "gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a tiny body", got)
	}
	if body := recorder.Body.String(); body != `{"ok":true}` {
		t.Errorf("body = %q, want the original JSON", body)
	}
}

// An origin that already encoded the body owns that decision.
func TestPreEncodedBodyIsNotDoubleCompressed(t *testing.T) {
	recorder := serve(t, 1, "gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.Write(bytes.Repeat([]byte("x"), 4096))
	})
	if got := recorder.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want the origin's own br", got)
	}
}

// A compressed body's length is not the origin's length.
func TestContentLengthIsDroppedWhenCompressing(t *testing.T) {
	recorder := serve(t, 1, "gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.Write(bytes.Repeat([]byte("x"), 4096))
	})
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it removed", got)
	}
}

// A non-200 status must survive the wrapper.
func TestStatusCodeIsPreserved(t *testing.T) {
	recorder := serve(t, 1024, "gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"nope"}`)
	})
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
