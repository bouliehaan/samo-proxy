package artwork

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"

	// Registered for their decoders only; GIF is decoded so it can be
	// recognised and declined rather than mangled.
	_ "image/gif"
)

// Resizing here rather than at the origin.
//
// samo-server has a perfectly good thumbnail ladder, and samo-proxy asks for a
// rung on every artwork request — but only some of its image routes honour it.
// serveMetadataImage short-circuits `cover_*` ids to getExtractedCover, which
// serves the file straight off disk and never consults the thumbnailer. Those
// extracted embedded covers are the common case for music, so in practice the
// biggest images on the wire were the ones the width parameter could not reach:
// 463 KB and 541 KB against on-disk 768px variants of 17-27 KB.
//
// Doing it here keeps samo-server untouched and makes samo-proxy self-
// sufficient: whatever the origin declines to shrink, the edge shrinks anyway.
// The scaler below is deliberately the same box filter samo-server uses, so a
// cover resized by either side looks identical.

var (
	// ErrNotSmaller means the source is already at or below the target, so
	// there is nothing to gain. Artwork is never upscaled.
	ErrNotSmaller = errors.New("source is already at or below the target size")
	// ErrUnsupported covers formats with no encoder worth using here.
	ErrUnsupported = errors.New("unsupported image format")
)

// maxSourcePixels caps what will be decoded into memory. A source is held as
// RGBA while it is resized, so this bounds one request at roughly 256 MB.
const maxSourcePixels = 64 << 20

// jpegQuality matches samo-server's, so neither side is visibly sharper.
const jpegQuality = 85

// Resize scales encoded image bytes down to fit within width, returning the
// re-encoded bytes and their content type.
//
// PNG stays PNG and everything else becomes JPEG, mirroring samo-server's
// thumbnailer exactly — a cover that arrives resized by the origin and one
// resized here should be indistinguishable.
func Resize(source []byte, width int) ([]byte, string, error) {
	if width <= 0 {
		return nil, "", ErrNotSmaller
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(source))
	if err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}
	switch format {
	case "jpeg", "png":
	default:
		// GIF loses animation through this path and WebP has no stdlib
		// encoder. Declining is the honest answer; the caller forwards the
		// original untouched.
		return nil, "", ErrUnsupported
	}
	if config.Width <= 0 || config.Height <= 0 {
		return nil, "", ErrUnsupported
	}
	if config.Width*config.Height > maxSourcePixels {
		return nil, "", ErrUnsupported
	}
	// Never upscale, and never pay a decode to produce something no smaller.
	if config.Width <= width && config.Height <= width {
		return nil, "", ErrNotSmaller
	}

	decoded, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	targetW, targetH := fitWithin(config.Width, config.Height, width)
	resized := downscale(decoded, targetW, targetH)

	var out bytes.Buffer
	if format == "png" {
		if err := png.Encode(&out, resized); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		return out.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&out, resized, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg: %w", err)
	}
	return out.Bytes(), "image/jpeg", nil
}

// IsResizable reports whether a content type is worth handing to Resize.
func IsResizable(contentType string) bool {
	normalized := strings.ToLower(contentType)
	if i := strings.IndexByte(normalized, ';'); i >= 0 {
		normalized = strings.TrimSpace(normalized[:i])
	}
	return normalized == "image/jpeg" || normalized == "image/jpg" || normalized == "image/png"
}

// fitWithin scales the longest edge to width, preserving aspect ratio.
func fitWithin(srcW, srcH, width int) (int, int) {
	if srcW >= srcH {
		height := srcH * width / srcW
		if height < 1 {
			height = 1
		}
		return width, height
	}
	targetWidth := srcW * width / srcH
	if targetWidth < 1 {
		targetWidth = 1
	}
	return targetWidth, width
}

// downscale area-averages src into a dstW x dstH image.
//
// A box filter rather than nearest-neighbour: sampling single pixels out of a
// 3000px cover produces visible aliasing on exactly the fine detail — text on
// an album sleeve — that makes artwork look cheap. Integer band edges keep
// every source pixel counted exactly once.
func downscale(src image.Image, dstW, dstH int) *image.RGBA {
	bounds := src.Bounds()
	flat, ok := src.(*image.RGBA)
	if !ok || flat.Bounds() != bounds {
		flat = image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		draw.Draw(flat, flat.Bounds(), src, bounds.Min, draw.Src)
	}

	srcW := flat.Bounds().Dx()
	srcH := flat.Bounds().Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	for dy := 0; dy < dstH; dy++ {
		sy0 := dy * srcH / dstH
		sy1 := (dy + 1) * srcH / dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := dx * srcW / dstW
			sx1 := (dx + 1) * srcW / dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}

			var sumR, sumG, sumB, sumA, count uint64
			for sy := sy0; sy < sy1; sy++ {
				row := sy * flat.Stride
				for sx := sx0; sx < sx1; sx++ {
					i := row + sx*4
					sumR += uint64(flat.Pix[i])
					sumG += uint64(flat.Pix[i+1])
					sumB += uint64(flat.Pix[i+2])
					sumA += uint64(flat.Pix[i+3])
					count++
				}
			}
			if count == 0 {
				continue
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o] = uint8(sumR / count)
			dst.Pix[o+1] = uint8(sumG / count)
			dst.Pix[o+2] = uint8(sumB / count)
			dst.Pix[o+3] = uint8(sumA / count)
		}
	}
	return dst
}
