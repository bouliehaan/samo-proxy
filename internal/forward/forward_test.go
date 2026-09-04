package forward

import (
	"bytes"
	"compress/gzip"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bouliehaan/samo-proxy/internal/cache"
	"github.com/bouliehaan/samo-proxy/internal/compress"
	"github.com/bouliehaan/samo-proxy/internal/config"
)

func testConfig(t *testing.T, originURL string) *config.Config {
	t.Helper()
	parsed, err := url.Parse(originURL)
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatalf("parse loopback cidr: %v", err)
	}
	return &config.Config{
		Addr:              "127.0.0.1:0",
		Origin:            parsed,
		ForwardedProto:    "https",
		TrustedCIDRs:      []*net.IPNet{loopback},
		CompressMinBytes:  1024,
		ImageDefaultWidth: 768,
		TranscodeEnabled:  true,
		TranscodeCodec:    "opus",
		TranscodeBitrate:  128,
		FFmpegPath:        "ffmpeg",
		FFmpegTimeout:     2 * time.Minute,
		CacheDir:          t.TempDir(),
		CacheMaxBytes:     1 << 30,
		OriginDialTimeout: 5 * time.Second,
		OriginIdleConns:   8,
	}
}

func newHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	store, err := cache.New(cfg.CacheDir, cfg.CacheMaxBytes)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	return New(cfg, store, slog.New(slog.DiscardHandler))
}

// The security-critical behaviour. samo-server's login limiter keys its
// brute-force lockout on CF-Connecting-IP, so a client that can set that header
// itself gets an unlimited supply of rate-limit buckets.
func TestUntrustedClientCannotSpoofForwardedHeaders(t *testing.T) {
	var seen http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	cfg := testConfig(t, origin.URL)
	// Trust nothing, so the request below arrives from an untrusted source.
	cfg.TrustedCIDRs = nil
	handler := newHandler(t, cfg)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/albums", nil)
	request.RemoteAddr = "203.0.113.9:5555"
	request.Header.Set("CF-Connecting-IP", "1.2.3.4")
	request.Header.Set("X-Forwarded-For", "5.6.7.8")
	request.Header.Set("X-Real-IP", "9.9.9.9")
	request.Header.Set("True-Client-IP", "8.8.8.8")

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if got := seen.Get("CF-Connecting-IP"); got != "203.0.113.9" {
		t.Errorf("CF-Connecting-IP = %q, want the real remote address", got)
	}
	if got := seen.Get("X-Forwarded-For"); got != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want the real remote address", got)
	}
	for _, header := range []string{"X-Real-IP", "True-Client-IP", "Forwarded"} {
		if value := seen.Get(header); value != "" {
			t.Errorf("%s survived sanitisation as %q", header, value)
		}
	}
}

// The counterpart: cloudflared is trusted, and Cloudflare's edge has already
// overwritten CF-Connecting-IP with the real client, so it must be believed.
func TestTrustedProxyForwardedHeadersAreBelieved(t *testing.T) {
	var seen http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/albums", nil)
	request.RemoteAddr = "127.0.0.1:44321"
	request.Header.Set("CF-Connecting-IP", "198.51.100.20")

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if got := seen.Get("CF-Connecting-IP"); got != "198.51.100.20" {
		t.Errorf("CF-Connecting-IP = %q, want the edge-supplied client", got)
	}
	if got := seen.Get("X-Forwarded-For"); got != "198.51.100.20" {
		t.Errorf("X-Forwarded-For = %q, want the edge-supplied client", got)
	}
}

// publicURL() in samo-server builds absolute URLs from r.Host, so rewriting the
// Host to the LAN origin would hand out links that only work inside the house.
func TestPublicHostAndSchemeArePreserved(t *testing.T) {
	var seenHost, seenProto string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		seenProto = r.Header.Get("X-Forwarded-Proto")
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/albums", nil)
	request.Host = "samo.example.com"
	request.RemoteAddr = "127.0.0.1:1234"

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if seenHost != "samo.example.com" {
		t.Errorf("Host = %q, want the public hostname", seenHost)
	}
	if seenProto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", seenProto)
	}
}

// The Android client never sends a width, so the proxy supplies one. This is
// the whole reason grid tiles stop pulling 3000x3000 covers.
func TestArtworkRequestGainsAWidth(t *testing.T) {
	var seenQuery string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/jpeg")
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/media/images/img_1/image?stream_token=abc", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if !strings.Contains(seenQuery, "width=768") {
		t.Errorf("origin query = %q, want an injected width", seenQuery)
	}
	if !strings.Contains(seenQuery, "stream_token=abc") {
		t.Errorf("origin query = %q, lost the caller's token", seenQuery)
	}
}

// A client that sized its own request knows better than our default.
func TestExplicitWidthIsNotOverridden(t *testing.T) {
	var seenQuery string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/media/images/img_1/image?width=128", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if !strings.Contains(seenQuery, "width=128") {
		t.Errorf("origin query = %q, want the caller's own width", seenQuery)
	}
	if strings.Contains(seenQuery, "768") {
		t.Errorf("origin query = %q, default overrode an explicit width", seenQuery)
	}
}

// A live radio channel is an endless response. It must reach the reverse proxy,
// never the transcoder, and never the disk cache.
func TestLiveStreamIsNeverTranscoded(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("live-bytes"))
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/channels/ch_1/stream", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Samo-Proxy-Transcode"); got != "" {
		t.Errorf("live stream was routed through the transcoder (%q)", got)
	}
	if body := recorder.Body.String(); body != "live-bytes" {
		t.Errorf("body = %q, want the origin's bytes untouched", body)
	}
}

// A lossy source is passed through: re-encoding it is an audible loss for a
// much smaller saving than lossless gets.
func TestLossySourceIsPassedThrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("mp3-bytes"))
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/tracks/tr_1/stream", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Samo-Proxy-Transcode"); got != "passthrough" {
		t.Errorf("X-Samo-Proxy-Transcode = %q, want passthrough", got)
	}
	if body := recorder.Body.String(); body != "mp3-bytes" {
		t.Errorf("body = %q, want the original bytes", body)
	}
}

// A seek into a track we have never encoded is answered from the origin
// immediately rather than by transcoding the whole file first.
func TestRangeRequestOnColdCacheGoesToOrigin(t *testing.T) {
	var sawRange string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "audio/flac")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("tail"))
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/music/tracks/tr_1/stream", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Range", "bytes=5000-")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if sawRange != "bytes=5000-" {
		t.Errorf("origin saw Range %q, want the client's own range", sawRange)
	}
	if got := recorder.Header().Get("X-Samo-Proxy-Transcode"); got != "" {
		t.Errorf("cold ranged request was transcoded (%q)", got)
	}
}

// End-to-end: a real FLAC through a real ffmpeg, cached, then served again from
// the cache with range support.
func TestLosslessIsEncodedThenServedFromCache(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	flac := synthFLAC(t)

	var originHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Header().Set("Last-Modified", "Mon, 01 Sep 2026 00:00:00 GMT")
		if r.Header.Get("Range") == "bytes=0-0" {
			// Revalidation probe.
			w.Header().Set("Content-Range", "bytes 0-0/"+strconv.Itoa(len(flac)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(flac[:1])
			return
		}
		originHits++
		w.WriteHeader(http.StatusOK)
		w.Write(flac)
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))
	const path = "/api/v1/music/tracks/tr_1/stream?stream_token=first-token"

	first := httptest.NewRequest(http.MethodGet, path, nil)
	first.RemoteAddr = "127.0.0.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)

	if got := firstRecorder.Header().Get("X-Samo-Proxy-Transcode"); got != "opus@128k" {
		t.Fatalf("X-Samo-Proxy-Transcode = %q, want opus@128k", got)
	}
	encoded := firstRecorder.Body.Bytes()
	if len(encoded) == 0 {
		t.Fatal("encoded body was empty")
	}
	if len(encoded) >= len(flac) {
		t.Errorf("encode did not shrink the source: %d -> %d bytes", len(flac), len(encoded))
	}
	if !strings.HasPrefix(string(encoded[:4]), "OggS") {
		t.Errorf("encoded body is not Ogg: %q", encoded[:4])
	}

	// Let the background encode finish and commit the cache entry.
	waitForCacheCommit(t, handler, path)

	// Second request, with a DIFFERENT stream token — the cache must still hit,
	// because the token is stripped from the key.
	second := httptest.NewRequest(http.MethodGet,
		"/api/v1/music/tracks/tr_1/stream?stream_token=second-token", nil)
	second.RemoteAddr = "127.0.0.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)

	if got := secondRecorder.Header().Get("X-Samo-Proxy-Cache"); got != "hit" {
		t.Fatalf("X-Samo-Proxy-Cache = %q, want hit after a token rotation", got)
	}
	if got := secondRecorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes on a cached entry", got)
	}
	if originHits != 1 {
		t.Errorf("origin was asked for the full file %d times, want 1", originHits)
	}
}

// waitForCacheCommit polls until the background encode has published its entry.
func waitForCacheCommit(t *testing.T, handler *Handler, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		probe := httptest.NewRequest(http.MethodGet, path, nil)
		probe.RemoteAddr = "127.0.0.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, probe)
		if recorder.Header().Get("X-Samo-Proxy-Cache") == "hit" {
			return
		}
		io.Copy(io.Discard, recorder.Body)
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("cache entry never committed")
}

// synthFLAC renders a few seconds of noise as FLAC so the test has a genuinely
// lossless source without checking a binary fixture into the repo.
//
// Noise, not a tone: FLAC squeezes a pure sine down to almost nothing, so a
// tone fixture would "prove" that encoding makes files bigger. Noise is
// near-incompressible losslessly, which is the property real music shares and
// the whole reason this pipeline exists.
func synthFLAC(t *testing.T) []byte {
	t.Helper()
	path := t.TempDir() + "/tone.flac"
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=duration=3:color=pink:sample_rate=44100:amplitude=0.5",
		"-ac", "2", "-sample_fmt", "s16", "-y", path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesise a FLAC fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// Every response carries the marker, not just the audio ones. During a
// connector cutover two tunnels serve the same hostname and a 200 proves only
// that something answered — this is what says which path it took.
func TestEveryResponseIsMarked(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))

	for _, tc := range []struct{ path, wantRoute string }{
		{"/health", "passthrough"},
		{"/api/v1/music/albums", "json"},
		{"/api/v1/media/images/i/image", "image"},
		{"/api/v1/channels/c/stream", "live"},
		{"/api/v1/events", "events"},
	} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if got := recorder.Header().Get("X-Samo-Proxy"); got != Version {
			t.Errorf("%s: X-Samo-Proxy = %q, want %q", tc.path, got, Version)
		}
		if got := recorder.Header().Get("X-Samo-Proxy-Route"); got != tc.wantRoute {
			t.Errorf("%s: X-Samo-Proxy-Route = %q, want %q", tc.path, got, tc.wantRoute)
		}
	}
}

// "bytes=0-" is how many clients ask for a whole file, and treating it as a
// seek meant those clients could never trigger a transcode at all.
func TestWholeFileRangeIsNotASeek(t *testing.T) {
	notSeeks := []string{"", "bytes=0-", "BYTES=0-", " bytes=0- "}
	for _, header := range notSeeks {
		if isSeekRange(header) {
			t.Errorf("isSeekRange(%q) = true, want false", header)
		}
	}
	seeks := []string{"bytes=100-", "bytes=0-99", "bytes=0-,200-300", "items=0-"}
	for _, header := range seeks {
		if !isSeekRange(header) {
			t.Errorf("isSeekRange(%q) = false, want true", header)
		}
	}
}

// The gap this closes: samo-server serves `cover_*` artwork straight off disk
// and ignores ?width= entirely, so the injected width reaches a handler that
// does nothing with it. Whatever arrives oversized gets shrunk here instead.
func TestOversizedCoverIsResizedByTheProxy(t *testing.T) {
	original := bigTestJPEG(t, 2000, 2000)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignores ?width=, exactly like getExtractedCover.
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.WriteHeader(http.StatusOK)
		w.Write(original)
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))
	const path = "/api/v1/media/covers/cover_1/image?stream_token=first"

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Samo-Proxy-Image"); got != "resized" {
		t.Fatalf("X-Samo-Proxy-Image = %q, want resized", got)
	}
	if recorder.Body.Len() >= len(original) {
		t.Errorf("proxy did not shrink the cover: %d -> %d", len(original), recorder.Body.Len())
	}

	// A rotated stream token must still hit the cache, same rule as audio.
	second := httptest.NewRequest(http.MethodGet,
		"/api/v1/media/covers/cover_1/image?stream_token=rotated", nil)
	second.RemoteAddr = "127.0.0.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)

	if got := secondRecorder.Header().Get("X-Samo-Proxy-Image"); got != "hit" {
		t.Errorf("X-Samo-Proxy-Image = %q, want hit after a token rotation", got)
	}
}

// An origin that already sized the image leaves nothing to do, and the proxy
// must not re-encode it for no gain.
func TestAlreadySmallCoverIsForwardedUntouched(t *testing.T) {
	small := bigTestJPEG(t, 400, 400)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(small)
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/media/covers/cover_2/image", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Samo-Proxy-Image"); got != "original" {
		t.Errorf("X-Samo-Proxy-Image = %q, want original", got)
	}
	if recorder.Body.Len() != len(small) {
		t.Errorf("body = %d bytes, want the original %d", recorder.Body.Len(), len(small))
	}
}

// samo-server 307s artwork it holds only as a remote URL. Resolving that here
// would proxy bytes it deliberately declined to.
func TestArtworkRedirectIsPassedThrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/cover.jpg", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/media/covers/cover_3/image", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want 307", recorder.Code)
	}
	if got := recorder.Header().Get("Location"); got != "https://example.invalid/cover.jpg" {
		t.Errorf("Location = %q, want the origin's redirect target", got)
	}
}

func bigTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x ^ y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// --- cache authorization -------------------------------------------------
//
// The cache key excludes credentials on purpose, so a cache hit is keyed on the
// URL alone. These tests are the reason that is safe: they assert that a caller
// the origin has not accepted is never handed cached bytes, for every media
// type the proxy caches. A new cached type that skips the gate fails here.

// A cover cached for a signed-in listener must not be served to a stranger who
// guessed the URL.
func TestCachedArtworkIsNotServedToAnAnonymousCaller(t *testing.T) {
	var originHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream_token") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		originHits++
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bigTestJPEG(t, 1400, 1400))
	}))
	defer origin.Close()

	handler := newHandler(t, testConfig(t, origin.URL))
	const path = "/api/v1/media/images/img_1/image"

	// A signed-in caller populates the cache.
	authed := httptest.NewRequest(http.MethodGet, path+"?stream_token=good", nil)
	authed.RemoteAddr = "127.0.0.1:1234"
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authed)
	if first.Code != http.StatusOK {
		t.Fatalf("authorized artwork = %d, want 200", first.Code)
	}

	// The same caller again should be a cache hit, proving the entry exists.
	repeat := httptest.NewRequest(http.MethodGet, path+"?stream_token=good", nil)
	repeat.RemoteAddr = "127.0.0.1:1234"
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, repeat)
	if got := second.Header().Get("X-Samo-Proxy-Image"); got != "hit" {
		t.Fatalf("second authorized request = %q, want a cache hit to prove the entry is there", got)
	}

	// Now a stranger asks for the identical URL with no credential at all.
	anonymous := httptest.NewRequest(http.MethodGet, path, nil)
	anonymous.RemoteAddr = "127.0.0.1:9999"
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, anonymous)

	if third.Code == http.StatusOK {
		t.Fatalf("anonymous caller got %d and %d bytes of cached artwork; the origin's 401 is the only correct answer",
			third.Code, third.Body.Len())
	}
	if got := third.Header().Get("X-Samo-Proxy-Image"); got == "hit" {
		t.Fatalf("anonymous caller was served from cache (%q)", got)
	}
}

// Follow attaches a second listener to an encode already in flight. It looks
// like an optimisation rather than a read, which is exactly why it needs the
// same gate: without it an anonymous caller is handed live audio for as long as
// somebody else's transcode is running.
func TestFollowingAnInFlightEncodeRequiresAuthorization(t *testing.T) {
	store, err := cache.New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	guarded := newGuardedCache(store)

	const key = "some-track"
	sink, started := guarded.Begin(key, cache.Meta{ContentType: "audio/ogg"})
	if !started {
		t.Fatal("Begin did not start an entry")
	}
	defer sink.Close(nil)
	if _, err := sink.Write([]byte("audio-bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}

	anonymous := httptest.NewRequest(http.MethodGet, "/api/v1/music/tracks/t1/stream", nil)
	if _, _, ok := guarded.Follow(anonymous, key); ok {
		t.Fatal("an anonymous caller was allowed to follow an in-flight encode")
	}

	authed := httptest.NewRequest(http.MethodGet, "/api/v1/music/tracks/t1/stream?stream_token=good", nil)
	guarded.Admit(authed)
	reader, _, ok := guarded.Follow(authed, key)
	if !ok {
		t.Fatal("an admitted caller could not follow the encode")
	}
	reader.Close()
}

// A credential the origin never accepted must not become usable just because
// somebody presented it.
func TestGateOnlyTrustsWhatTheOriginAccepted(t *testing.T) {
	gate := newCacheGate()
	request := httptest.NewRequest(http.MethodGet, "/x?stream_token=abc", nil)

	if gate.Allows(request) {
		t.Fatal("gate trusted a credential before the origin ever saw it")
	}
	gate.Admit(request)
	if !gate.Allows(request) {
		t.Fatal("gate did not trust a credential the origin accepted")
	}

	other := httptest.NewRequest(http.MethodGet, "/x?stream_token=different", nil)
	if gate.Allows(other) {
		t.Fatal("admitting one credential admitted another")
	}
}

// An anonymous request that succeeds means the route is public. That says
// nothing about who may read cached bytes, so it must teach the gate nothing —
// otherwise one public endpoint unlocks the whole cache for everyone.
func TestASuccessfulAnonymousRequestAdmitsNobody(t *testing.T) {
	gate := newCacheGate()
	public := httptest.NewRequest(http.MethodGet, "/health", nil)

	gate.Admit(public)
	if gate.Allows(public) {
		t.Fatal("an anonymous success admitted the empty credential")
	}
}

// A revoked token keeps working on cached bytes only until the entry ages out.
func TestAdmissionExpires(t *testing.T) {
	gate := newCacheGate()
	now := time.Now()
	gate.now = func() time.Time { return now }

	request := httptest.NewRequest(http.MethodGet, "/x?stream_token=abc", nil)
	gate.Admit(request)
	if !gate.Allows(request) {
		t.Fatal("freshly admitted credential was refused")
	}

	now = now.Add(credentialTTL + time.Second)
	if gate.Allows(request) {
		t.Fatal("credential outlived its TTL")
	}
}

// The gate is written to on every success, so it needs a ceiling. This is the
// lesson from samo-server's stream token store, which grows without one.
func TestGateIsBounded(t *testing.T) {
	gate := newCacheGate()
	gate.limit = 4

	for i := 0; i < 50; i++ {
		request := httptest.NewRequest(http.MethodGet, "/x?stream_token=t"+strconv.Itoa(i), nil)
		gate.Admit(request)
	}
	if len(gate.seen) > gate.limit {
		t.Fatalf("gate holds %d credentials, limit is %d", len(gate.seen), gate.limit)
	}
}

// Compression has to survive the reverse proxy, and for a long time it did not.
//
// The proxy runs with FlushInterval -1, so httputil flushes after every write.
// That reached compress.Middleware before the body had grown past its
// threshold and resolved the response as "do not compress" — silently, on
// every proxied route, which is every JSON route samo-server serves. The
// package's own tests all passed, because they call the middleware directly
// and nothing in them flushes.
//
// So the test lives here rather than in internal/compress: the bug was in the
// seam between the two, and only an assembled chain can see it.
func TestProxiedJSONIsCompressed(t *testing.T) {
	body := strings.Repeat(`{"id":"internet-radio_0001","title":"Station"},`, 100)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer origin.Close()

	cfg := testConfig(t, origin.URL)
	chain := compress.Middleware(cfg.CompressMinBytes, newHandler(t, cfg))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/internet-radio/stations", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip: %d bytes of JSON crossed the uplink raw", got, recorder.Body.Len())
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
}

// The other half of the same decision: a live stream must still reach the
// client the instant it is written, compressor or not.
func TestProxiedEventStreamIsNotCompressed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": connected\n\n")
		w.(http.Flusher).Flush()
	}))
	defer origin.Close()

	cfg := testConfig(t, origin.URL)
	chain := compress.Middleware(cfg.CompressMinBytes, newHandler(t, cfg))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("event stream was compressed (Content-Encoding %q)", got)
	}
	if body := recorder.Body.String(); body != ": connected\n\n" {
		t.Errorf("body = %q, want the raw SSE comment", body)
	}
}
