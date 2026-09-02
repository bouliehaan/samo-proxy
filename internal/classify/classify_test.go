package classify

import (
	"net/url"
	"testing"
)

func TestOf(t *testing.T) {
	cases := []struct {
		path string
		want Kind
	}{
		// Finite audio.
		{"/api/v1/music/tracks/abc123/stream", Audio},
		{"/api/v1/audiobooks/bk_1/stream", Audio},
		{"/api/v1/podcasts/episodes/ep_9/stream", Audio},
		{"/api/v1/media/files/f_7/stream", Audio},
		{"/rest/stream", Audio},
		{"/rest/stream.view", Audio},
		{"/rest/download.view", Audio},

		// Live streams must never be classified as transcodable: they have no
		// end, and a disk cache behind one would fill until it burst.
		{"/radio/rad_1/stream", LiveStream},
		{"/internet-radio/st_1/stream", LiveStream},
		{"/channels/ch_1/stream", LiveStream},
		{"/api/v1/channels/ch_1/stream", LiveStream},

		// Artwork.
		{"/api/v1/media/images/img_1/image", Image},
		{"/api/v1/media/covers/cover_1/image", Image},
		{"/api/v1/music/albums/al_1/cover", Image},
		{"/api/v1/music/artists/ar_1/cover", Image},
		{"/api/v1/music/playlists/pl_1/cover", Image},
		{"/api/v1/podcasts/shows/sh_1/cover", Image},
		{"/api/v1/audiobooks/bk_1/cover", Image},
		{"/api/v1/channels/ch_1/cover", Image},
		{"/rest/getCoverArt", Image},

		{"/api/v1/events", Events},
		{"/api/v1/music/albums", JSON},
		{"/rest/getAlbumList2", JSON},
		{"/login", Passthrough},
		{"/", Passthrough},
	}

	for _, tc := range cases {
		if got := Of(tc.path); got != tc.want {
			t.Errorf("Of(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

// A channel has both a /stream and a /cover route under the same prefix. Get
// this wrong and either artwork is treated as an endless stream or an endless
// stream is handed to the transcoder.
func TestChannelStreamAndCoverAreDistinguished(t *testing.T) {
	if got := Of("/api/v1/channels/ch_1/stream"); got != LiveStream {
		t.Fatalf("channel stream = %s, want live", got)
	}
	if got := Of("/api/v1/channels/ch_1/cover"); got != Image {
		t.Fatalf("channel cover = %s, want image", got)
	}
}

func TestTrailingSlashAndQueryAreIgnored(t *testing.T) {
	if got := Of("/api/v1/music/tracks/abc/stream/"); got != Audio {
		t.Errorf("trailing slash changed classification: %s", got)
	}
	if got := Of("/api/v1/music/tracks/abc/stream?stream_token=xyz"); got != Audio {
		t.Errorf("query changed classification: %s", got)
	}
}

// A path with extra segments where the route expects one opaque id is not that
// route.
func TestMultiSegmentIDDoesNotMatch(t *testing.T) {
	if got := Of("/api/v1/audiobooks/a/b/cover"); got == Image {
		t.Errorf("multi-segment path matched a single-id route")
	}
}

// The single most important behaviour in the package. Stream tokens have a
// 30-minute TTL and the clients re-mint on their own schedule, so a cache keyed
// on the raw query would miss several times an hour and re-encode forever.
func TestCacheKeyQueryStripsRotatingCredentials(t *testing.T) {
	first := url.Values{
		"stream_token": {"tok-aaaa"},
		"fileAt":       {"120"},
	}
	second := url.Values{
		"stream_token": {"tok-bbbb"},
		"fileAt":       {"120"},
	}

	if CacheKeyQuery(first) != CacheKeyQuery(second) {
		t.Fatalf("rotating token changed the cache key: %q vs %q",
			CacheKeyQuery(first), CacheKeyQuery(second))
	}
	if got := CacheKeyQuery(first); got != "fileAt=120" {
		t.Fatalf("CacheKeyQuery = %q, want fileAt=120", got)
	}
}

// Parameters that genuinely select different bytes must survive.
func TestCacheKeyQueryKeepsByteSelectingParams(t *testing.T) {
	atStart := url.Values{"stream_token": {"t"}, "fileAt": {"0"}}
	midway := url.Values{"stream_token": {"t"}, "fileAt": {"600"}}

	if CacheKeyQuery(atStart) == CacheKeyQuery(midway) {
		t.Fatal("different seek offsets collapsed to the same cache key")
	}
}

func TestCacheKeyQueryStripsSubsonicCredentials(t *testing.T) {
	withCreds := url.Values{
		"id": {"tr_1"},
		"u":  {"jake"},
		"t":  {"deadbeef"},
		"s":  {"salt"},
		"c":  {"samo"},
		"v":  {"1.16.1"},
	}
	if got := CacheKeyQuery(withCreds); got != "id=tr_1" {
		t.Fatalf("CacheKeyQuery = %q, want id=tr_1", got)
	}
}

// Parameter order on the wire must not change the key.
func TestCacheKeyQueryIsOrderStable(t *testing.T) {
	first, _ := url.ParseQuery("b=2&a=1&stream_token=x")
	second, _ := url.ParseQuery("a=1&stream_token=y&b=2")
	if CacheKeyQuery(first) != CacheKeyQuery(second) {
		t.Fatal("parameter order changed the cache key")
	}
}
