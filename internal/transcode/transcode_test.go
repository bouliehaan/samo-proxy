package transcode

import (
	"slices"
	"testing"
)

// Lossless sources are the whole point: a 24/96 FLAC is roughly 4.5 Mbps
// sustained, which is most of a home uplink on its own.
func TestLosslessSourcesAreEncoded(t *testing.T) {
	cases := []struct{ contentType, path string }{
		{"audio/flac", "/api/v1/music/tracks/t/stream"},
		{"audio/x-flac", "/api/v1/music/tracks/t/stream"},
		{"audio/wav", "/api/v1/music/tracks/t/stream"},
		{"audio/x-aiff", "/api/v1/music/tracks/t/stream"},
		// Content type missing or useless: fall back to the path.
		{"", "/api/v1/media/files/f/stream.flac"},
		{"application/octet-stream", "/api/v1/media/files/f/stream.wav"},
	}
	for _, tc := range cases {
		if got := Decide(tc.contentType, tc.path, false); got != Encode {
			t.Errorf("Decide(%q, %q) = passthrough, want encode", tc.contentType, tc.path)
		}
	}
}

// Re-encoding lossy audio is an audible loss for a much smaller saving than
// lossless gets, so it is off unless explicitly asked for.
func TestLossySourcesArePassedThroughByDefault(t *testing.T) {
	for _, contentType := range []string{"audio/mpeg", "audio/mp4", "audio/aac", "audio/ogg", "audio/opus"} {
		if got := Decide(contentType, "/api/v1/music/tracks/t/stream", false); got != Passthrough {
			t.Errorf("Decide(%q) = encode, want passthrough", contentType)
		}
		if got := Decide(contentType, "/api/v1/music/tracks/t/stream", true); got != Encode {
			t.Errorf("Decide(%q, lossyToo) = passthrough, want encode", contentType)
		}
	}
}

// The origin answering with an error page or a redirect body under an audio
// route must never reach ffmpeg.
func TestNonAudioIsNeverEncoded(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/json", "image/jpeg"} {
		if got := Decide(contentType, "/api/v1/music/tracks/t/stream", true); got != Passthrough {
			t.Errorf("Decide(%q) = encode, want passthrough", contentType)
		}
	}
}

func TestContentTypeParametersAreIgnored(t *testing.T) {
	if got := Decide("audio/flac; charset=binary", "/x", false); got != Encode {
		t.Error("a parameterised content type was not recognised")
	}
}

// An MP4-family container may keep its moov atom at the end of the file, which
// ffmpeg can only reach by seeking backwards — impossible on a pipe.
func TestMP4FamilyNeedsSeekableInput(t *testing.T) {
	for _, tc := range []struct{ contentType, path string }{
		{"audio/mp4", "/x"},
		{"audio/m4b", "/x"},
		{"", "/api/v1/audiobooks/b/stream.m4b"},
		{"", "/api/v1/media/files/f/stream.m4a"},
	} {
		if !NeedsSeekableInput(tc.contentType, tc.path) {
			t.Errorf("NeedsSeekableInput(%q, %q) = false, want true", tc.contentType, tc.path)
		}
	}
	for _, tc := range []struct{ contentType, path string }{
		{"audio/flac", "/x"},
		{"audio/mpeg", "/x"},
		{"", "/api/v1/media/files/f/stream.flac"},
	} {
		if NeedsSeekableInput(tc.contentType, tc.path) {
			t.Errorf("NeedsSeekableInput(%q, %q) = true, want false", tc.contentType, tc.path)
		}
	}
}

// -vn is load-bearing: an embedded 3000x3000 cover is a video stream to ffmpeg,
// and copying it into every transcode would put back the megabytes this whole
// pipeline exists to remove.
func TestArgsDropEmbeddedArtwork(t *testing.T) {
	args := Encoder{Profile: Profile{Codec: "opus", BitrateKbps: 128}}.Args("pipe:0")
	if !slices.Contains(args, "-vn") {
		t.Fatalf("ffmpeg args do not drop the video stream: %v", args)
	}
}

func TestArgsMatchTheProfile(t *testing.T) {
	cases := []struct {
		codec       string
		wantEncoder string
		wantFormat  string
	}{
		{"opus", "libopus", "opus"},
		{"aac", "aac", "adts"},
		{"mp3", "libmp3lame", "mp3"},
	}
	for _, tc := range cases {
		args := Encoder{Profile: Profile{Codec: tc.codec, BitrateKbps: 192}}.Args("pipe:0")
		if !slices.Contains(args, tc.wantEncoder) {
			t.Errorf("%s args missing encoder %s: %v", tc.codec, tc.wantEncoder, args)
		}
		if !slices.Contains(args, tc.wantFormat) {
			t.Errorf("%s args missing format %s: %v", tc.codec, tc.wantFormat, args)
		}
		if !slices.Contains(args, "192k") {
			t.Errorf("%s args missing the bitrate: %v", tc.codec, args)
		}
	}
}

// The profile is part of every cache key, so changing a setting must invalidate
// previously cached audio rather than mixing formats under one key.
func TestProfileStringDistinguishesSettings(t *testing.T) {
	a := Profile{Codec: "opus", BitrateKbps: 128}
	b := Profile{Codec: "opus", BitrateKbps: 192}
	c := Profile{Codec: "aac", BitrateKbps: 128}
	if a.String() == b.String() || a.String() == c.String() {
		t.Fatal("distinct profiles rendered to the same cache key fragment")
	}
}

func TestProfileContentTypes(t *testing.T) {
	cases := map[string]string{
		"opus": "audio/ogg",
		"aac":  "audio/aac",
		"mp3":  "audio/mpeg",
	}
	for codec, want := range cases {
		if got := (Profile{Codec: codec}).ContentType(); got != want {
			t.Errorf("%s ContentType = %q, want %q", codec, got, want)
		}
	}
}
