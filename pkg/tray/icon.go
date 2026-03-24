package tray

// icon generates a 32×32 PNG icon at init time: purple background (#7c3aed),
// white ⚡ bolt drawn with filled rectangles.
// Having it in Go avoids an external binary asset dependency.

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

var iconPNG []byte

func init() {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	purple := color.RGBA{0x7c, 0x3a, 0xed, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}

	// Fill background
	draw.Draw(img, img.Bounds(), &image.Uniform{purple}, image.Point{}, draw.Src)

	// Draw a simple lightning bolt ⚡ using filled rectangles.
	// Upper-right diagonal bar (top-left to centre)
	for y := 4; y <= 16; y++ {
		x := 20 - (y-4)/2
		for dx := 0; dx < 4; dx++ {
			img.Set(x+dx, y, white)
		}
	}
	// Lower-left diagonal bar (centre to bottom-right)
	for y := 15; y <= 27; y++ {
		x := 8 + (y-15)/2
		for dx := 0; dx < 4; dx++ {
			img.Set(x+dx, y, white)
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	iconPNG = buf.Bytes()
}
