package artwork

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/url"
	"testing"
)

func TestSnapRoundsUpToTheLadder(t *testing.T) {
	cases := []struct {
		in    int
		want  int
		valid bool
	}{
		{1, 64, true},
		{64, 64, true},
		{65, 128, true},
		{500, 512, true},
		{768, 768, true},
		{1024, 1024, true},
		// Above the top rung samo-server serves the original, so a width
		// parameter would be noise.
		{2048, 0, false},
		{0, 0, false},
		{-10, 0, false},
	}
	for _, tc := range cases {
		got, ok := Snap(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("Snap(%d) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

func TestApplyDefaultAddsAWidthWhenAbsent(t *testing.T) {
	query := url.Values{"stream_token": {"abc"}}
	if !ApplyDefault(query, 768) {
		t.Fatal("ApplyDefault reported no change")
	}
	if got := query.Get("width"); got != "768" {
		t.Errorf("width = %q, want 768", got)
	}
	if got := query.Get("stream_token"); got != "abc" {
		t.Errorf("stream_token = %q, want it untouched", got)
	}
}

// The desktop client sizes every request to its rendered slot and knows better
// than any default this proxy could pick.
func TestApplyDefaultRespectsAnExplicitWidth(t *testing.T) {
	query := url.Values{"width": {"128"}}
	if ApplyDefault(query, 768) {
		t.Fatal("ApplyDefault overrode an explicit width")
	}
	if got := query.Get("width"); got != "128" {
		t.Errorf("width = %q, want the caller's own 128", got)
	}
}

// Subsonic clients express the same intent as `size`. Honour it rather than
// adding a second, conflicting instruction.
func TestApplyDefaultRespectsSubsonicSize(t *testing.T) {
	query := url.Values{"size": {"300"}}
	if ApplyDefault(query, 768) {
		t.Fatal("ApplyDefault added a width alongside a Subsonic size")
	}
	if query.Get("width") != "" {
		t.Error("width was injected despite an explicit size")
	}
}

func TestApplyDefaultSnapsToTheLadder(t *testing.T) {
	query := url.Values{}
	if !ApplyDefault(query, 500) {
		t.Fatal("ApplyDefault reported no change")
	}
	if got := query.Get("width"); got != "512" {
		t.Errorf("width = %q, want it snapped up to 512", got)
	}
}

// Zero, and anything above the top rung, both mean "send the original" — which
// is what omitting the parameter already does.
func TestApplyDefaultDisabledCases(t *testing.T) {
	for _, width := range []int{0, -1, 4096} {
		query := url.Values{}
		if ApplyDefault(query, width) {
			t.Errorf("ApplyDefault(%d) injected a width", width)
		}
		if query.Get("width") != "" {
			t.Errorf("ApplyDefault(%d) set width=%q", width, query.Get("width"))
		}
	}
}

// --- Resize ---------------------------------------------------------------
//
// These cover the gap that made proxy-side resizing necessary at all: the
// origin serves `cover_*` artwork straight off disk, ignoring `?width=`, so
// whatever arrives oversized has to be shrunk here or not at all.

func encodeTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Gradient rather than flat colour: a solid image compresses to almost
	// nothing and would "prove" that resizing never saves bytes.
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

func TestResizeShrinksAnOversizedCover(t *testing.T) {
	source := encodeTestJPEG(t, 2000, 2000)

	resized, contentType, err := Resize(source, 768)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if contentType != "image/jpeg" {
		t.Errorf("contentType = %q, want image/jpeg", contentType)
	}
	if len(resized) >= len(source) {
		t.Errorf("resize did not shrink: %d -> %d bytes", len(source), len(resized))
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(resized))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if config.Width != 768 || config.Height != 768 {
		t.Errorf("result is %dx%d, want 768x768", config.Width, config.Height)
	}
}

// Artwork is never upscaled, and a decode that could not shrink anything is
// wasted work.
func TestResizeDeclinesSourcesAlreadySmallEnough(t *testing.T) {
	source := encodeTestJPEG(t, 512, 512)
	if _, _, err := Resize(source, 768); !errors.Is(err, ErrNotSmaller) {
		t.Fatalf("Resize err = %v, want ErrNotSmaller", err)
	}
}

func TestResizePreservesAspectRatio(t *testing.T) {
	source := encodeTestJPEG(t, 2000, 1000)
	resized, _, err := Resize(source, 768)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(resized))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if config.Width != 768 || config.Height != 384 {
		t.Errorf("result is %dx%d, want 768x384", config.Width, config.Height)
	}
}

// PNG stays PNG so transparency survives; anything else becomes JPEG. This
// mirrors samo-server's thumbnailer so both sides produce the same thing.
func TestResizeKeepsPNGAsPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 1600; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, uint8(x % 256)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}

	_, contentType, err := Resize(buf.Bytes(), 768)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if contentType != "image/png" {
		t.Errorf("contentType = %q, want image/png", contentType)
	}
}

// A format with no encoder worth using must be declined, not mangled — the
// caller forwards the original untouched.
func TestResizeDeclinesUndecodableInput(t *testing.T) {
	if _, _, err := Resize([]byte("not an image at all"), 768); err == nil {
		t.Fatal("Resize accepted garbage")
	}
}

func TestIsResizable(t *testing.T) {
	for _, contentType := range []string{"image/jpeg", "image/png", "image/jpeg; charset=binary", "IMAGE/PNG"} {
		if !IsResizable(contentType) {
			t.Errorf("IsResizable(%q) = false, want true", contentType)
		}
	}
	for _, contentType := range []string{"image/gif", "image/webp", "image/svg+xml", "audio/flac", ""} {
		if IsResizable(contentType) {
			t.Errorf("IsResizable(%q) = true, want false", contentType)
		}
	}
}
