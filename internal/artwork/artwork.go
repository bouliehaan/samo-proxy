// Package artwork sizes covers on their way out.
//
// samo-server has had a thumbnail ladder since long before this proxy existed
// (internal/images/thumbnail.go: 64/128/256/384/512/768/1024, JPEG q85), and
// the desktop client uses it — samo-controller.ts pipes a rendered slot width
// through withSamoImageWidth on every request. The Android client never does.
// getSamoMetadataImageUrl builds /media/images/{id}/image bare, so every grid
// tile on the phone pulls the full-resolution embedded cover, which for a
// well-tagged library is routinely a 3000x3000 JPEG.
//
// Injecting a default width here fixes that for every client at once without
// touching either of them, which is what makes it a proxy concern rather than a
// client bug to chase. It is safe because samo-server treats the parameter as
// advisory: thumbnailFor falls through to the original bytes for anything it
// cannot resize, so a bad width degrades to today's behaviour rather than to an
// error.
package artwork

import "net/url"

// Ladder mirrors images.Widths in samo-server. Requests are snapped here as
// well as there so the value that lands in a cache key is the one the origin
// will actually serve, rather than whatever arbitrary number was asked for.
var Ladder = []int{64, 128, 256, 384, 512, 768, 1024}

// Snap rounds a width up to the next rung, reporting false for anything above
// the top rung — where samo-server serves the original and a width parameter
// would be noise.
func Snap(width int) (int, bool) {
	if width <= 0 {
		return 0, false
	}
	for _, rung := range Ladder {
		if width <= rung {
			return rung, true
		}
	}
	return 0, false
}

// ApplyDefault adds `width=` to an artwork query that does not already carry
// one, and reports whether it changed anything.
//
// A client that asked for a specific width is left alone unconditionally. The
// desktop app sizes its requests per slot and knows better than any default
// this proxy could pick.
func ApplyDefault(query url.Values, defaultWidth int) bool {
	if defaultWidth <= 0 {
		return false
	}
	if query.Get("width") != "" {
		return false
	}
	// Subsonic clients express the same intent with `size`. Honour it rather
	// than overriding it — and translate it, since samo-server only reads
	// `width`.
	if size := query.Get("size"); size != "" {
		return false
	}
	snapped, ok := Snap(defaultWidth)
	if !ok {
		// A configured default above the ladder means "send the original",
		// which is what omitting the parameter already does.
		return false
	}
	query.Set("width", itoa(snapped))
	return true
}

// itoa avoids pulling strconv in for one call and keeps the hot path free of
// the fmt package.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
