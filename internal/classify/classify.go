// Package classify decides what kind of samo-server resource a request is for.
//
// Everything downstream keys off this: compression must never touch audio or an
// event stream, artwork width injection must only hit image routes, and the
// transcoder must never be handed a live radio channel, which has no end.
//
// The route table below mirrors samo-server's own registrations in
// internal/api/server.go and internal/subsonic/server.go. It is deliberately an
// allow-list of exact shapes rather than a set of loose prefixes: a route this
// package does not recognise is classified Passthrough and forwarded untouched,
// which is the only safe default for a proxy that sits in front of an API it
// does not own. A new samo-server route costs a missed optimisation here, never
// a broken response.
package classify

import (
	"net/url"
	"strings"
)

// Kind is what a request is asking for.
type Kind int

const (
	// Passthrough is anything unrecognised: forward it and change nothing.
	Passthrough Kind = iota
	// JSON is an API response worth compressing.
	JSON
	// Image is artwork, which the origin can resize on request.
	Image
	// Audio is a finite media file: transcodable and cacheable.
	Audio
	// LiveStream is an endless radio/channel stream. Never cache it, never
	// transcode it, never buffer it.
	LiveStream
	// Events is the SSE dashboard channel. Never compress or buffer it.
	Events
)

func (k Kind) String() string {
	switch k {
	case JSON:
		return "json"
	case Image:
		return "image"
	case Audio:
		return "audio"
	case LiveStream:
		return "live"
	case Events:
		return "events"
	default:
		return "passthrough"
	}
}

// Live streams. These are open-ended: samo-server holds the connection and
// writes for as long as the listener stays. Handing one to ffmpeg with a disk
// cache behind it would fill the disk until it burst.
var livePrefixes = []string{
	"/radio/",
	"/internet-radio/",
	"/channels/",
	"/api/v1/channels/",
}

// Finite audio, as {prefix, suffix} pairs around the opaque id segment.
var audioRoutes = [][2]string{
	{"/api/v1/music/tracks/", "/stream"},
	{"/api/v1/audiobooks/", "/stream"},
	{"/api/v1/podcasts/episodes/", "/stream"},
	{"/api/v1/media/files/", "/stream"},
}

// Artwork. Every one of these reaches samo-server's serveCatalogImage, which
// honours `?width=` as an advisory hint — including the Subsonic covers, which
// route through the same helper via subsonicStreamAdapter.ServeCover.
var imageRoutes = [][2]string{
	{"/api/v1/media/images/", "/image"},
	{"/api/v1/media/covers/", "/image"},
	{"/api/v1/music/albums/", "/cover"},
	{"/api/v1/music/artists/", "/cover"},
	{"/api/v1/music/playlists/", "/cover"},
	{"/api/v1/podcasts/shows/", "/cover"},
	{"/api/v1/audiobooks/", "/cover"},
	{"/api/v1/channels/", "/cover"},
}

// Subsonic actions, which are flat paths with an optional ".view" suffix rather
// than the native API's path segments.
var subsonicAudio = []string{"stream", "download"}
var subsonicImage = []string{"getCoverArt"}

// Of returns the kind of the given request path.
func Of(path string) Kind {
	path = normalize(path)

	if path == "/api/v1/events" {
		return Events
	}

	// Live must be tested before the image and audio tables, because
	// /api/v1/channels/{id}/cover and /api/v1/channels/{id}/stream share a
	// prefix and only one of them is an endless stream.
	if strings.HasSuffix(path, "/stream") {
		for _, prefix := range livePrefixes {
			if strings.HasPrefix(path, prefix) && hasIDSegment(path, prefix, "/stream") {
				return LiveStream
			}
		}
	}

	if action, ok := subsonicAction(path); ok {
		for _, name := range subsonicAudio {
			if action == name {
				return Audio
			}
		}
		for _, name := range subsonicImage {
			if action == name {
				return Image
			}
		}
		// Every other Subsonic action returns a JSON or XML document.
		return JSON
	}

	for _, route := range imageRoutes {
		if matches(path, route[0], route[1]) {
			return Image
		}
	}
	for _, route := range audioRoutes {
		if matches(path, route[0], route[1]) {
			return Audio
		}
	}

	if strings.HasPrefix(path, "/api/v1/") {
		return JSON
	}
	return Passthrough
}

// IsImageWidthAware reports whether injecting `?width=` into this request is
// meaningful. It is a thin wrapper over Of, kept separate so the intent reads
// clearly at the call site.
func IsImageWidthAware(path string) bool { return Of(path) == Image }

// CacheKeyQuery renders a query string suitable for use in a cache key.
//
// The stream token is stripped, and this is the single most important line in
// the package. samo-server mints stream tokens with a 30-minute TTL
// (internal/users/streamtokens.go) and the clients re-mint on their own
// schedule, so the same track arrives under a different URL several times an
// hour. A cache keyed on the raw query would miss every time and transcode the
// same file forever — the same trap that once made artwork flash in the Android
// client, where the image cache was keyed on the rotating token.
//
// Everything else is kept and sorted. Parameters like an audiobook's seek
// offset genuinely select different bytes and must stay in the key.
func CacheKeyQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	filtered := make(url.Values, len(query))
	for key, values := range query {
		switch strings.ToLower(key) {
		// Native stream token, and the Subsonic credential set — all of them
		// authenticate the caller and none of them change a single byte of the
		// response.
		case "stream_token", "streamtoken", "u", "p", "t", "s", "c", "v", "apikey":
			continue
		}
		filtered[key] = values
	}
	// url.Values.Encode sorts by key, so the same request always renders the
	// same string regardless of the order the client sent the parameters in.
	return filtered.Encode()
}

// normalize collapses a path to the form the route tables are written in.
func normalize(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		return "/"
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
		if path == "" {
			return "/"
		}
	}
	return path
}

// matches reports whether path is prefix + <one id segment> + suffix.
func matches(path, prefix, suffix string) bool {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return false
	}
	return hasIDSegment(path, prefix, suffix)
}

// hasIDSegment checks that exactly one non-empty, slash-free segment sits
// between prefix and suffix. Without the slash check, /api/v1/audiobooks/a/b/cover
// would match a route that only ever has one id.
func hasIDSegment(path, prefix, suffix string) bool {
	if len(path) < len(prefix)+len(suffix)+1 {
		return false
	}
	middle := path[len(prefix) : len(path)-len(suffix)]
	return middle != "" && !strings.Contains(middle, "/")
}

// subsonicAction extracts the action name from /rest/<action>[.view].
func subsonicAction(path string) (string, bool) {
	const prefix = "/rest/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	action := path[len(prefix):]
	if action == "" || strings.Contains(action, "/") {
		return "", false
	}
	return strings.TrimSuffix(action, ".view"), true
}
