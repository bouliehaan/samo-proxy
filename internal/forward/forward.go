// Package forward is samo-proxy's request path: header hygiene, artwork
// sizing, and the transcoding audio pipeline, in front of a plain reverse
// proxy for everything else.
package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bouliehaan/samo-proxy/internal/artwork"
	"github.com/bouliehaan/samo-proxy/internal/cache"
	"github.com/bouliehaan/samo-proxy/internal/classify"
	"github.com/bouliehaan/samo-proxy/internal/config"
	"github.com/bouliehaan/samo-proxy/internal/transcode"
)

// Handler is the whole proxy.
type Handler struct {
	cfg    *config.Config
	log    *slog.Logger
	proxy  *httputil.ReverseProxy
	origin *http.Client
	// store is a guardedCache rather than a *cache.Cache so that no cached body
	// can be read without a request to authorize it. See cachegate.go.
	store   *guardedCache
	encoder transcode.Encoder
	profile transcode.Profile
}

// New wires the reverse proxy and the transcoding pipeline.
func New(cfg *config.Config, store *cache.Cache, logger *slog.Logger) *Handler {
	// Keep-alives to the origin are on and generous. This is a LAN hop, so
	// reuse is both safe and free — the silently-half-open-socket problem that
	// forced the Android client onto a no-reuse pool is a property of that
	// device's Wi-Fi, not of this link.
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: cfg.OriginDialTimeout}).DialContext,
		MaxIdleConns:        cfg.OriginIdleConns,
		MaxIdleConnsPerHost: cfg.OriginIdleConns,
		IdleConnTimeout:     90 * time.Second,
		// The origin is samo-server on the LAN; it never gzips, and asking
		// would only add a decode step in front of the transcoder.
		DisableCompression: true,
	}

	handler := &Handler{
		cfg:   cfg,
		log:   logger,
		store: newGuardedCache(store),
		origin: &http.Client{
			Transport: transport,
			// A redirect is a response the client needs to see, not something
			// to resolve here: samo-server 307s artwork to remote URLs, and
			// following that would proxy bytes it deliberately declined to.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		encoder: transcode.Encoder{
			FFmpegPath: cfg.FFmpegPath,
			Profile:    transcode.Profile{Codec: cfg.TranscodeCodec, BitrateKbps: cfg.TranscodeBitrate},
		},
		profile: transcode.Profile{Codec: cfg.TranscodeCodec, BitrateKbps: cfg.TranscodeBitrate},
	}

	handler.proxy = &httputil.ReverseProxy{
		Rewrite:   handler.rewrite,
		Transport: transport,
		// -1 flushes every write immediately, which is what an endless radio
		// stream and the SSE dashboard channel both need. samo-server already
		// sends a 25s SSE heartbeat to survive Cloudflare's 100s idle reap;
		// buffering here would defeat that by holding the heartbeat back.
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("origin request failed", "path", r.URL.Path, "error", err)
			http.Error(w, "origin unavailable", http.StatusBadGateway)
		},
		// Every plain request the origin accepts teaches the cache gate that
		// this caller's credential is good. This is where the gate gets warm in
		// practice: a client has always made authenticated API calls before it
		// asks for the artwork on the page those calls described.
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode < 400 && response.Request != nil {
				handler.store.Admit(response.Request)
			}
			return nil
		},
	}
	return handler
}

// rewrite maps an inbound request onto the origin.
func (h *Handler) rewrite(r *httputil.ProxyRequest) {
	r.Out.URL.Scheme = h.cfg.Origin.Scheme
	r.Out.URL.Host = h.cfg.Origin.Host
	r.Out.URL.Path = h.cfg.Origin.Path + r.In.URL.Path
	r.Out.URL.RawQuery = r.In.URL.RawQuery

	// Preserve the public hostname. samo-server's publicURL() builds absolute
	// URLs from r.Host, and its station directory renders links from the
	// request's own scheme and host — rewriting it to the LAN address would
	// hand out URLs that only work inside the house.
	r.Out.Host = r.In.Host

	h.sanitizeForwarded(r.In, r.Out)
}

// sanitizeForwarded decides what the origin is allowed to believe about who
// the client is.
//
// This is the security-critical function in samo-proxy. samo-server's login
// limiter reads CF-Connecting-IP first and X-Forwarded-For second, and keys its
// brute-force lockout on the result. Cloudflare's edge overwrites
// CF-Connecting-IP on every request, so the value cloudflared hands us is
// trustworthy — but only because it came from cloudflared. Anything else that
// can reach this port gets to choose its own rate-limit bucket, and forwarding
// that unmodified would hand an attacker an unlimited supply of them.
//
// So: believe the header only from a trusted source, and otherwise replace it
// with the address we actually saw. Either way the origin receives exactly one
// authoritative value, set rather than appended.
//
// The reverse proxy and the hand-built requests in fetchOrigin/revalidate both
// arrive here. They used to run two byte-identical copies of this logic, one of
// which carried a comment saying the two must never diverge — which is a rule
// somebody has to keep rather than a thing that stays true. One implementation,
// two callers.
func (h *Handler) sanitizeForwarded(in, out *http.Request) {
	clientIP := remoteIP(in.RemoteAddr)
	if h.cfg.TrustsAddr(in.RemoteAddr) {
		if cf := strings.TrimSpace(in.Header.Get("CF-Connecting-IP")); cf != "" {
			clientIP = cf
		} else if xff := firstForwarded(in.Header.Get("X-Forwarded-For")); xff != "" {
			clientIP = xff
		}
	}

	out.Header.Set("CF-Connecting-IP", clientIP)
	out.Header.Set("X-Forwarded-For", clientIP)
	// samo-server does not read these, but leaving a client-supplied value in
	// place for some future reader to trust would be leaving a trap behind.
	out.Header.Del("X-Real-IP")
	out.Header.Del("Forwarded")
	out.Header.Del("True-Client-IP")

	// cloudflared terminates TLS at Cloudflare's edge and forwards plain HTTP.
	// The origin cannot work the real scheme out for itself, and it needs to:
	// publicURL() builds absolute URLs from it, and security_headers.go gates
	// HSTS on it.
	out.Header.Set("X-Forwarded-Proto", h.cfg.ForwardedProto)
}

// Version identifies this proxy in responses. It is not a build stamp; it
// exists so "is traffic actually going through samo-proxy?" has an answer.
const Version = "samo-proxy/1"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind := classify.Of(r.URL.Path)

	// Mark every response, not just the audio ones.
	//
	// During a connector cutover two tunnels serve the same hostname and
	// Cloudflare load-balances between them, so a 200 proves only that
	// something answered — not which path it took. Without this header the
	// only way to tell them apart is to switch one off, which is exactly the
	// step you want evidence for before taking. Set before dispatch because
	// ReverseProxy adds to these headers rather than replacing them.
	w.Header().Set("X-Samo-Proxy", Version)
	w.Header().Set("X-Samo-Proxy-Route", kind.String())

	if kind == classify.Image && h.cfg.ImageDefaultWidth > 0 {
		h.applyImageWidth(r)
		if r.Method == http.MethodGet {
			h.serveImage(w, r)
			return
		}
	}

	if kind == classify.Audio && h.cfg.TranscodeEnabled && r.Method == http.MethodGet {
		h.serveAudio(w, r)
		return
	}

	h.proxy.ServeHTTP(w, r)
}

// applyImageWidth rewrites the request URL in place so both the reverse proxy
// and any logging see the width that will actually be served.
func (h *Handler) applyImageWidth(r *http.Request) {
	query := r.URL.Query()
	if artwork.ApplyDefault(query, h.cfg.ImageDefaultWidth) {
		r.URL.RawQuery = query.Encode()
	}
}

// serveImage forwards artwork, shrinking anything the origin declined to.
//
// The width parameter injected above is enough for the routes that reach
// samo-server's thumbnailer, and for those this function does nothing but pass
// bytes along. It exists for the routes that do not: `cover_*` ids are
// short-circuited to getExtractedCover, which serves the file straight off disk
// and never consults the thumbnailer, so extracted embedded art — the common
// case for music — arrived at full size no matter what was asked for.
//
// No revalidation here, unlike the audio path. samo-server serves artwork as
// `public, max-age=31536000, immutable` keyed by content id, which is a promise
// that these bytes do not change under this URL.
func (h *Handler) serveImage(w http.ResponseWriter, r *http.Request) {
	width, ok := Snap(h.cfg.ImageDefaultWidth)
	if !ok {
		h.proxy.ServeHTTP(w, r)
		return
	}
	// A caller that asked for its own width gets exactly that from the origin;
	// second-guessing it here would undo the desktop client's per-slot sizing.
	if requested := r.URL.Query().Get("width"); requested != itoa(width) {
		h.proxy.ServeHTTP(w, r)
		return
	}

	key := cache.Key("image", r.URL.Path, classify.CacheKeyQuery(r.URL.Query()), itoa(width))

	if file, meta, ok := h.openAuthorized(r, key); ok {
		defer file.Close()
		if meta.ContentType != "" {
			w.Header().Set("Content-Type", meta.ContentType)
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Samo-Proxy-Image", "hit")
		http.ServeContent(w, r, "artwork", meta.CreatedAt, file)
		return
	}

	response, err := h.fetchOrigin(r)
	if err != nil {
		h.log.Error("artwork fetch failed", "path", r.URL.Path, "error", err)
		http.Error(w, "origin unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	// A redirect, a 401 or a 404 is the origin's answer to give. samo-server
	// 307s artwork it has only as a remote URL, and resolving that here would
	// proxy bytes it deliberately declined to.
	if response.StatusCode != http.StatusOK || !artwork.IsResizable(response.Header.Get("Content-Type")) {
		copyHeader(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}

	// Bounded so a pathological source cannot exhaust memory here.
	source, err := io.ReadAll(io.LimitReader(response.Body, maxArtworkBytes))
	if err != nil {
		h.log.Warn("artwork read failed", "path", r.URL.Path, "error", err)
		copyHeader(w.Header(), response.Header)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(source)
		return
	}

	resized, contentType, err := artwork.Resize(source, width)
	if err != nil || len(resized) >= len(source) {
		// Already small enough, an undecodable format, or a re-encode that
		// bought nothing. Every one of these means the original is the right
		// answer — artwork must degrade to today's behaviour, never to an error.
		copyHeader(w.Header(), response.Header)
		w.Header().Set("X-Samo-Proxy-Image", "original")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(source)
		return
	}

	if sink, started := h.store.Begin(key, cache.Meta{ContentType: contentType}); started {
		_, writeErr := sink.Write(resized)
		sink.Close(writeErr)
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", itoa(len(resized)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Samo-Proxy-Image", "resized")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resized)
}

// maxArtworkBytes bounds one artwork read. Real covers are a few megabytes at
// worst; this is generous enough never to reject a legitimate one.
const maxArtworkBytes = 64 << 20

// serveAudio is the transcoding path.
//
// The shape here is driven by one decision: a Range request against a cold
// cache is not worth transcoding for. It means a seek into something we have
// never encoded, and the honest answer is the original bytes, immediately, from
// the origin. Sequential play — which is how a track is normally opened — is
// what populates the cache, and every request after that gets full range
// support from a complete file on disk.
func (h *Handler) serveAudio(w http.ResponseWriter, r *http.Request) {
	key := cache.Key(
		r.URL.Path,
		classify.CacheKeyQuery(r.URL.Query()),
		h.profile.String(),
	)

	// Peek reads the entry's metadata, not its body, so it needs no
	// authorization — and it must not require one. Revalidation is what
	// authorizes this path: it puts the caller's own credential in front of the
	// origin, so a rotated stream token is accepted here exactly as the origin
	// accepts it, and the cache still hits. Reading the body first and checking
	// afterwards would turn every 30-minute token rotation into a re-fetch of
	// the whole lossless source.
	if meta, ok := h.store.Peek(key); ok {
		fresh, err := h.revalidate(r, meta)
		if err == nil && fresh {
			// revalidate admitted this caller, so the body opens.
			file, _, opened := h.store.Open(r, key)
			if opened {
				defer file.Close()
				h.serveCached(w, r, file, meta)
				return
			}
		}
		if err == nil && !fresh {
			h.log.Info("cache entry stale, dropping", "path", r.URL.Path)
			h.store.Drop(key)
		}
		// A revalidation error is not a reason to fail the request: fall
		// through and let the origin answer it directly.
		if err != nil {
			h.log.Warn("revalidation failed", "path", r.URL.Path, "error", err)
			h.proxy.ServeHTTP(w, r)
			return
		}
	}

	// A genuine seek into something we have never encoded goes straight to the
	// origin. "bytes=0-" does NOT count: it means the whole file from the
	// start, which is exactly the request a fresh play makes, and treating it
	// as a seek meant any client that always sends a Range header — which is
	// most of them — could never trigger a transcode at all.
	if isSeekRange(r.Header.Get("Range")) {
		h.proxy.ServeHTTP(w, r)
		return
	}

	// Someone else is already encoding this. Follow their output rather than
	// starting a second ffmpeg over the same file.
	if reader, meta, ok := h.store.Follow(r, key); ok {
		defer reader.Close()
		h.streamEncoded(w, reader, meta.ContentType)
		return
	}

	h.encodeAndStream(w, r, key)
}

// serveCached answers from a complete cache entry, with full range support.
func (h *Handler) serveCached(w http.ResponseWriter, r *http.Request, file *os.File, meta cache.Meta) {
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Samo-Proxy-Transcode", h.profile.String())
	w.Header().Set("X-Samo-Proxy-Cache", "hit")
	// ServeContent handles Range, If-Range and HEAD. The name is only used to
	// sniff a content type we have already set, and the modtime is what makes
	// If-Modified-Since work for the client.
	http.ServeContent(w, r, "audio."+h.profile.Extension(), meta.CreatedAt, file)
}

// encodeAndStream fetches the source, decides whether it is worth encoding, and
// either streams the encode or forwards the original.
func (h *Handler) encodeAndStream(w http.ResponseWriter, r *http.Request, key string) {
	originResponse, err := h.fetchOrigin(r)
	if err != nil {
		h.log.Error("origin fetch failed", "path", r.URL.Path, "error", err)
		http.Error(w, "origin unavailable", http.StatusBadGateway)
		return
	}

	// Anything that is not a plain 200 — a redirect, a 401, an error — is the
	// origin's answer to give, not ours to reinterpret.
	if originResponse.StatusCode != http.StatusOK {
		defer originResponse.Body.Close()
		copyHeader(w.Header(), originResponse.Header)
		w.WriteHeader(originResponse.StatusCode)
		_, _ = io.Copy(w, originResponse.Body)
		return
	}

	contentType := originResponse.Header.Get("Content-Type")
	if transcode.Decide(contentType, r.URL.Path, h.cfg.TranscodeLossyToo) == transcode.Passthrough {
		defer originResponse.Body.Close()
		copyHeader(w.Header(), originResponse.Header)
		w.Header().Set("X-Samo-Proxy-Transcode", "passthrough")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, originResponse.Body)
		return
	}

	meta := cache.Meta{
		ContentType:        h.profile.ContentType(),
		OriginETag:         originResponse.Header.Get("ETag"),
		OriginLastModified: originResponse.Header.Get("Last-Modified"),
		OriginLength:       originResponse.ContentLength,
	}

	sink, started := h.store.Begin(key, meta)
	if !started {
		// Lost a race with another request between Follow and Begin. Follow
		// that one instead of encoding the same file twice.
		originResponse.Body.Close()
		if reader, followMeta, ok := h.store.Follow(r, key); ok {
			defer reader.Close()
			h.streamEncoded(w, reader, followMeta.ContentType)
			return
		}
		h.proxy.ServeHTTP(w, r)
		return
	}

	reader, err := sink.Reader()
	if err != nil {
		originResponse.Body.Close()
		sink.Close(err)
		h.proxy.ServeHTTP(w, r)
		return
	}
	defer reader.Close()

	seekable := transcode.NeedsSeekableInput(contentType, r.URL.Path)

	// The encode runs on its own context, not the request's. A listener who
	// skips three seconds in still leaves a complete cache entry behind —
	// otherwise skipping around an album caches nothing and every later play
	// pays the encode again.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), h.cfg.FFmpegTimeout)
		defer cancel()
		defer originResponse.Body.Close()

		err := h.encoder.Run(ctx, originResponse.Body, sink, seekable)
		if err != nil {
			h.log.Error("transcode failed", "path", r.URL.Path, "error", err)
		}
		sink.Close(err)
	}()

	w.Header().Set("X-Samo-Proxy-Cache", "miss")
	h.streamEncoded(w, reader, meta.ContentType)
}

// streamEncoded writes an in-progress encode to the client.
//
// There is no Content-Length: the encoded size is not known until ffmpeg
// finishes. Accept-Ranges is explicitly "none" so a client does not try to seek
// into a stream that cannot serve one — the next request for the same track
// will find a complete entry and get proper ranges.
func (h *Handler) streamEncoded(w http.ResponseWriter, reader io.Reader, contentType string) {
	if contentType == "" {
		contentType = h.profile.ContentType()
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Samo-Proxy-Transcode", h.profile.String())
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 64<<10)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				// The client went away. The encode keeps running so the cache
				// entry still completes.
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				h.log.Warn("encoded stream ended early", "error", err)
			}
			return
		}
	}
}

// fetchOrigin issues the request to samo-server, minus the client's Range.
func (h *Handler) fetchOrigin(r *http.Request) (*http.Response, error) {
	target := *h.cfg.Origin
	target.Path = h.cfg.Origin.Path + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	copyHeader(request.Header, r.Header)
	// We need the whole file to encode it.
	request.Header.Del("Range")
	request.Header.Del("If-Range")
	request.Header.Del("Accept-Encoding")
	request.Host = r.Host
	h.applyForwardedTo(r, request)

	response, err := h.origin.Do(request)
	if err == nil && response.StatusCode < 400 {
		// The origin served this caller, so the cache gate may serve them too.
		h.store.Admit(r)
	}
	return response, err
}

// probeOrigin asks the origin about a URL with the cheapest request that
// carries a real answer: a one-byte ranged GET. It is the same trick
// samo-server's own podcast stream service uses, because some servers refuse
// HEAD outright.
//
// One request answers both questions a cache hit needs settled: may THIS caller
// have this URL, and do the origin's validators still match what we stored.
// Keeping it in one function is deliberate — the authorization half is easy to
// leave out of a second copy, and leaving it out is precisely how the artwork
// path became readable without credentials.
//
// A success admits the caller to the cache gate. The caller owns the response
// body.
func (h *Handler) probeOrigin(r *http.Request) (*http.Response, error) {
	target := *h.cfg.Origin
	target.Path = h.cfg.Origin.Path + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	copyHeader(request.Header, r.Header)
	request.Header.Set("Range", "bytes=0-0")
	request.Header.Del("If-Range")
	request.Header.Del("If-None-Match")
	request.Header.Del("If-Modified-Since")
	request.Header.Del("Accept-Encoding")
	request.Host = r.Host
	h.applyForwardedTo(r, request)

	response, err := h.origin.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	// The body is tiny and fully drained by every caller, so releasing the
	// context here rather than leaking the cancel func is safe.
	cancel()

	// An auth failure or a vanished file is the origin's answer, not staleness
	// and not permission.
	if response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusOK {
		drainAndClose(response)
		return nil, fmt.Errorf("origin returned %d", response.StatusCode)
	}
	// The origin served this caller, so the gate may serve them cached bytes
	// for this URL — and the covers on the page they are playing from.
	h.store.Admit(r)
	return response, nil
}

// authorizeFromOrigin settles permission alone, for a cached response that has
// no freshness question to ask.
//
// Artwork is immutable by content id — samo-server serves it
// `public, max-age=31536000, immutable` — so there is nothing to revalidate.
// But "nothing to revalidate" is not "nobody to authorize", and treating the
// two as the same thing is what let a cached cover be read without a token.
// One one-byte request, only when the gate does not already know the caller.
func (h *Handler) authorizeFromOrigin(r *http.Request) error {
	response, err := h.probeOrigin(r)
	if err != nil {
		return err
	}
	drainAndClose(response)
	return nil
}

// drainAndClose releases a probe response, reading the little it carries so the
// connection can be reused rather than dropped.
func drainAndClose(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<10))
	response.Body.Close()
}

// openAuthorized is the one way a cached body reaches a client.
//
// It answers the authorization question before any bytes move, from the gate
// when it already knows the caller and from the origin when it does not — so a
// freshly rotated stream token costs one one-byte request rather than a re-fetch
// of the whole object, and an anonymous caller gets nothing either way.
func (h *Handler) openAuthorized(r *http.Request, key string) (*os.File, cache.Meta, bool) {
	meta, present := h.store.Peek(key)
	if !present {
		return nil, cache.Meta{}, false
	}
	if !h.store.Allows(r) {
		if err := h.authorizeFromOrigin(r); err != nil {
			return nil, cache.Meta{}, false
		}
	}
	file, _, opened := h.store.Open(r, key)
	if !opened {
		return nil, cache.Meta{}, false
	}
	return file, meta, true
}

// revalidate asks the origin whether a cached entry still matches the source.
//
// samo-server serves audio as `private, max-age=3600` precisely because a
// file's contents can change under a stable URL — a re-tag, a replaced
// download — so an entry that is never revalidated would serve stale audio
// indefinitely.
func (h *Handler) revalidate(r *http.Request, meta cache.Meta) (bool, error) {
	response, err := h.probeOrigin(r)
	if err != nil {
		return false, fmt.Errorf("revalidate: %w", err)
	}
	defer drainAndClose(response)

	// Prefer a strong validator when the origin offers one.
	if meta.OriginETag != "" {
		return response.Header.Get("ETag") == meta.OriginETag, nil
	}
	if meta.OriginLastModified != "" {
		if response.Header.Get("Last-Modified") == meta.OriginLastModified {
			return true, nil
		}
		return false, nil
	}
	// Fall back to total size from Content-Range. Weaker than a validator, but
	// it still catches a replaced file, and an origin that offers neither
	// validator leaves nothing better to compare.
	if total, ok := totalFromContentRange(response.Header.Get("Content-Range")); ok && meta.OriginLength > 0 {
		return total == meta.OriginLength, nil
	}
	// Nothing to compare: treat the entry as usable rather than re-encoding on
	// every play. The cache is bounded and eviction will retire it eventually.
	return true, nil
}

// applyForwardedTo applies the reverse proxy's header hygiene to a request we
// build ourselves, so the transcoding path cannot become a way around it.
//
// It delegates rather than duplicating: this used to be a second copy of
// sanitizeForwarded's body, kept in step by hand.
func (h *Handler) applyForwardedTo(in, out *http.Request) {
	h.sanitizeForwarded(in, out)
}

// Health reports whether the origin is answering, for samo-proxy's own probe.
func (h *Handler) Health(ctx context.Context) error {
	target := *h.cfg.Origin
	target.Path = h.cfg.Origin.Path + "/health"

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	response, err := h.origin.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode >= 500 {
		return fmt.Errorf("origin health returned %d", response.StatusCode)
	}
	return nil
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		// Hop-by-hop headers describe one connection and must not be relayed.
		switch http.CanonicalHeaderKey(key) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		dst[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
}

// Snap and itoa are thin aliases so the artwork ladder is named once.
func Snap(width int) (int, bool) { return artwork.Snap(width) }

func itoa(value int) string { return strconv.Itoa(value) }

func remoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// isSeekRange reports whether a Range header asks for anything other than the
// whole file from byte zero.
func isSeekRange(header string) bool {
	spec := strings.ToLower(strings.TrimSpace(header))
	if spec == "" {
		return false
	}
	// An unrecognised unit is not something to reason about; let the origin
	// answer it.
	if !strings.HasPrefix(spec, "bytes=") {
		return true
	}
	spec = strings.TrimSpace(strings.TrimPrefix(spec, "bytes="))
	// Multiple ranges are unambiguously not a plain sequential read.
	if strings.Contains(spec, ",") {
		return true
	}
	return spec != "0-"
}

func firstForwarded(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(value, ",", 2)[0])
}

func totalFromContentRange(header string) (int64, bool) {
	// "bytes 0-0/12345"
	slash := strings.LastIndexByte(header, '/')
	if slash < 0 || slash == len(header)-1 {
		return 0, false
	}
	size := strings.TrimSpace(header[slash+1:])
	if size == "*" {
		return 0, false
	}
	parsed, err := parseInt64(size)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func parseInt64(value string) (int64, error) {
	var out int64
	if value == "" {
		return 0, errors.New("empty")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("not a number: %q", value)
		}
		out = out*10 + int64(char-'0')
	}
	return out, nil
}
