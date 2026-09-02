// Package transcode re-encodes samo-server's audio on its way out of the house.
//
// This is the one thing in samo-proxy that has no upstream equivalent, and the
// reason the proxy is a Go service rather than a Caddy config. samo-server
// serves original bytes and nothing else — ServeMediaFileAt is documented as
// "streams original on-disk bytes without transcoding", and the Subsonic
// adapter's handleStream delegates to the same path, ignoring maxBitRate
// entirely. On a LAN that is exactly right. Across a home uplink it is not: a
// 24/96 FLAC is roughly 4.5 Mbps sustained, which is most of a Starlink upload
// on its own.
//
// The policy is deliberately conservative. Lossless sources are re-encoded
// because the saving is enormous and the transparency of Opus at 128k makes it
// a free trade for streaming. Lossy sources are passed through untouched by
// default: re-encoding lossy audio is an audible loss for a much smaller
// saving, and a 192k MP3 is already fine over the wire.
package transcode

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// Action is what to do with a given source.
type Action int

const (
	// Passthrough forwards the origin's bytes unchanged.
	Passthrough Action = iota
	// Encode runs the source through ffmpeg.
	Encode
)

// Profile is the output format. It is part of every cache key, so changing a
// setting invalidates previously cached audio rather than mixing formats under
// one key.
type Profile struct {
	Codec       string // opus, aac or mp3
	BitrateKbps int
}

// String renders the profile for use in a cache key.
func (p Profile) String() string { return fmt.Sprintf("%s@%dk", p.Codec, p.BitrateKbps) }

// ContentType is the MIME type the encoded output should be served as.
func (p Profile) ContentType() string {
	switch p.Codec {
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "mp3":
		return "audio/mpeg"
	default:
		return "application/octet-stream"
	}
}

// Extension is the container extension, used only for readable temp names.
func (p Profile) Extension() string {
	switch p.Codec {
	case "opus":
		return "opus"
	case "aac":
		return "aac"
	default:
		return "mp3"
	}
}

// losslessTypes are the content types worth re-encoding. Matching is on
// substrings because servers spell these several ways (audio/flac,
// audio/x-flac) and samo-server passes through whatever the scanner recorded.
var losslessTypes = []string{
	"flac", "wav", "wave", "aiff", "aif", "alac", "ape", "wavpack", "dsf", "dff", "x-shorten",
}

// losslessExtensions back up the content type. samo-server derives Content-Type
// from the scanned file and falls back to a path lookup, so it is usually
// right — but an item with an empty or generic type would otherwise be treated
// as lossy and skipped, which is the wrong default for a file called .flac.
var losslessExtensions = []string{
	".flac", ".wav", ".aiff", ".aif", ".alac", ".ape", ".wv", ".dsf", ".dff", ".shn",
}

// mp4Containers cannot be transcoded from a pipe: their moov atom may sit at
// the end of the file, and ffmpeg has to seek backwards to find it. These get
// buffered to a temp file first.
var mp4Containers = []string{"mp4", "m4a", "m4b", "aac"}

// Decide chooses what to do with a source. requestPath is used only as a
// fallback hint when the origin's content type is unhelpful.
func Decide(contentType, requestPath string, lossyToo bool) Action {
	normalized := strings.ToLower(strings.TrimSpace(contentType))
	// Strip any parameters: "audio/flac; charset=binary".
	if i := strings.IndexByte(normalized, ';'); i >= 0 {
		normalized = strings.TrimSpace(normalized[:i])
	}

	// Anything that is not audio has no business in the encoder. This also
	// catches the origin answering an error page or a redirect body under a
	// route we classified as audio.
	if normalized != "" && !strings.HasPrefix(normalized, "audio/") &&
		!strings.HasPrefix(normalized, "application/octet-stream") {
		return Passthrough
	}

	if isLossless(normalized, requestPath) {
		return Encode
	}
	if lossyToo && strings.HasPrefix(normalized, "audio/") {
		return Encode
	}
	return Passthrough
}

func isLossless(contentType, requestPath string) bool {
	for _, marker := range losslessTypes {
		if strings.Contains(contentType, marker) {
			return true
		}
	}
	ext := strings.ToLower(path.Ext(pathWithoutQuery(requestPath)))
	for _, candidate := range losslessExtensions {
		if ext == candidate {
			return true
		}
	}
	return false
}

// NeedsSeekableInput reports whether ffmpeg must be given a real file rather
// than a pipe for this source.
func NeedsSeekableInput(contentType, requestPath string) bool {
	normalized := strings.ToLower(contentType)
	for _, marker := range mp4Containers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	switch strings.ToLower(path.Ext(pathWithoutQuery(requestPath))) {
	case ".m4a", ".m4b", ".mp4", ".alac":
		return true
	}
	return false
}

func pathWithoutQuery(raw string) string {
	if parsed, err := url.Parse(raw); err == nil {
		return parsed.Path
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// Encoder runs ffmpeg.
type Encoder struct {
	FFmpegPath string
	Profile    Profile
}

// Args builds the ffmpeg command line.
//
// -vn is load-bearing and easy to overlook: an embedded cover in a FLAC is a
// video stream to ffmpeg, and a 3000x3000 JPEG copied into every transcode
// would put megabytes back onto the wire that the whole exercise exists to
// remove. The artwork routes serve covers properly, sized.
func (e Encoder) Args(input string) []string {
	bitrate := strconv.Itoa(e.Profile.BitrateKbps) + "k"
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-i", input,
		"-vn",
		"-map_metadata", "0",
	}
	switch e.Profile.Codec {
	case "opus":
		args = append(args,
			"-c:a", "libopus",
			"-b:a", bitrate,
			"-vbr", "on",
			"-application", "audio",
			"-f", "opus",
		)
	case "aac":
		args = append(args,
			"-c:a", "aac",
			"-b:a", bitrate,
			"-f", "adts",
		)
	default:
		args = append(args,
			"-c:a", "libmp3lame",
			"-b:a", bitrate,
			"-f", "mp3",
		)
	}
	return append(args, "pipe:1")
}

// Run encodes source into out. It blocks until ffmpeg exits.
//
// ctx governs the encode and is deliberately not the request context: a
// listener who skips three seconds in should still leave a complete cache
// entry behind, so the caller passes a background context with its own timeout.
func (e Encoder) Run(ctx context.Context, source io.Reader, out io.Writer, seekable bool) error {
	input := "pipe:0"
	var tempPath string
	if seekable {
		temp, err := os.CreateTemp("", "samo-proxy-src-*")
		if err != nil {
			return fmt.Errorf("create temp source: %w", err)
		}
		tempPath = temp.Name()
		defer os.Remove(tempPath)
		if _, err := io.Copy(temp, source); err != nil {
			temp.Close()
			return fmt.Errorf("buffer source: %w", err)
		}
		if err := temp.Close(); err != nil {
			return fmt.Errorf("close temp source: %w", err)
		}
		input = tempPath
		source = nil
	}

	cmd := exec.CommandContext(ctx, e.FFmpegPath, e.Args(input)...)
	if source != nil {
		cmd.Stdin = source
	}
	cmd.Stdout = out
	// ffmpeg writes diagnostics to stderr; at -loglevel error there is nothing
	// unless something went wrong, and then it is the only explanation we get.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("ffmpeg: %w: %s", err, message)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}
