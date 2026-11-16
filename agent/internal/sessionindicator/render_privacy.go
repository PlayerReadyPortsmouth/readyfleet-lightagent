package sessionindicator

import (
	"image"
	"image/color"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/inconsolata"
	"golang.org/x/image/math/fixed"
)

var (
	privGround = color.RGBA{R: 0x0B, G: 0x12, B: 0x20, A: 0xFF}
	privTitle  = color.RGBA{R: 0xF5, G: 0xA3, B: 0x00, A: 0xFF}
	privInk    = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
)

// renderPrivacy draws the notice into a w×h opaque image: the first line in
// hazard amber, the rest white, centred, as large as the width allows. The
// bitmap font is carried in the binary so it renders identically on any PC.
func renderPrivacy(w, h int, n PrivacyNotice) *image.RGBA {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{privGround}, image.Point{}, draw.Src)
	lines := n.Lines()
	face := inconsolata.Bold8x16
	longest := 1
	for _, l := range lines {
		longest = max(longest, len(l))
	}
	scale := 1
	for s := 8; s >= 1; s-- {
		if face.Advance*s*longest <= w*9/10 && face.Height*s*2*len(lines) <= h {
			scale = s
			break
		}
	}
	if face.Advance*longest > w*9/10 && scale == 1 {
		face = basicfont.Face7x13 // very narrow head: smaller face, text clipped at the edge
	}
	lineH := face.Height * scale * 2
	y0 := (h - lineH*len(lines)) / 2
	for i, l := range lines {
		ink := privInk
		if i == 0 {
			ink = privTitle
		}
		drawCentred(img, face, scale, l, y0+i*lineH, ink)
	}
	return img
}

func drawCentred(dst *image.RGBA, face *basicfont.Face, scale int, text string, y int, ink color.RGBA) {
	tw := face.Advance * len(text)
	if tw == 0 {
		return
	}
	scratch := image.NewRGBA(image.Rect(0, 0, tw, face.Height))
	draw.Draw(scratch, scratch.Bounds(), &image.Uniform{privGround}, image.Point{}, draw.Src)
	d := font.Drawer{Dst: scratch, Src: &image.Uniform{ink}, Face: face, Dot: fixed.P(0, face.Ascent)}
	d.DrawString(text)
	b := dst.Bounds()
	ox := (b.Dx() - tw*scale) / 2
	for yy := 0; yy < face.Height*scale; yy++ {
		for xx := 0; xx < tw*scale; xx++ {
			x, py := ox+xx, y+yy
			if x < 0 || x >= b.Dx() || py < 0 || py >= b.Dy() {
				continue
			}
			dst.Set(x, py, scratch.At(xx/scale, yy/scale))
		}
	}
}
