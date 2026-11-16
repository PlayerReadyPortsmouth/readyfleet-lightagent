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

// The bar's palette. Hazard amber with near-black text: it has to read at
// a glance as a warning that is emphatically NOT part of the desktop, and
// it has to stay legible against whatever wallpaper or window is behind
// it, which rules out anything translucent or theme-following. The dark
// rule along the bottom stops the bar bleeding into a light-coloured
// title bar directly underneath.
var (
	barGround = color.RGBA{R: 0xF5, G: 0xA3, B: 0x00, A: 0xFF}
	barInk    = color.RGBA{R: 0x14, G: 0x14, B: 0x14, A: 0xFF}
	barRule   = color.RGBA{R: 0x1A, G: 0x14, B: 0x00, A: 0xFF}
)

// renderBar draws the indicator into a w×h image, in BUFFER pixels — so
// on a scale-2 output the caller passes twice ReserveHeight and gets
// proportionally larger text, with no separate scale argument to keep in
// sync. Alpha is fully opaque throughout: the bar must never be see-through,
// and opaque pixels make the premultiplied conversion in toBGRA a no-op.
func renderBar(w, h int, n Notice) *image.RGBA {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{barGround}, image.Point{}, draw.Src)

	// Bottom rule, ~1 logical pixel thick.
	rule := max(h/ReserveHeight, 1)
	draw.Draw(img, image.Rect(0, h-rule, w, h), &image.Uniform{barRule}, image.Point{}, draw.Src)

	text := n.Text()
	pad := 2 * rule

	// Pick the largest integer glyph scale whose line fits the bar's
	// height AND width, then shrink the text (not the bar) if even 1x
	// overflows. Losing the end of a viewer's name is acceptable; losing
	// "Your screen is being viewed remotely" is not, so the fallback
	// truncates from the right with an ellipsis.
	face, scale := pickFace(w, h-rule, pad, text)
	glyphW := faceAdvance(face) * scale
	if glyphW > 0 {
		if maxChars := (w - 2*pad) / glyphW; maxChars > 0 && len(text) > maxChars {
			if maxChars > 3 {
				text = text[:maxChars-3] + "..."
			} else {
				text = text[:maxChars]
			}
		}
	}

	drawScaledText(img, face, scale, text, pad, h-rule)
	return img
}

// pickFace chooses the bitmap face and integer magnification for a bar of
// this size. Inconsolata Bold 8x16 is the preferred face — it is heavier
// and reads better as a warning — with basicfont's 7x13 as the fallback
// when the bar is too short for a magnified 16px line.
func pickFace(w, h, pad int, text string) (*basicfont.Face, int) {
	candidates := []*basicfont.Face{inconsolata.Bold8x16, basicfont.Face7x13}
	best, bestScale := basicfont.Face7x13, 1
	for _, f := range candidates {
		for scale := 4; scale >= 1; scale-- {
			lineH := f.Height * scale
			lineW := faceAdvance(f) * scale * len(text)
			if lineH <= h-2*pad && lineW <= w-2*pad {
				if lineH > best.Height*bestScale {
					best, bestScale = f, scale
				}
				break
			}
		}
	}
	return best, bestScale
}

func faceAdvance(f *basicfont.Face) int { return f.Advance }

// drawScaledText renders text with face at 1x into a scratch image, then
// blows it up by an integer factor with nearest-neighbour sampling.
//
// Nearest-neighbour on a bitmap font is deliberate: the glyphs stay crisp
// and blocky at any magnification with no antialiasing, no hinting and no
// font file to find on the machine — the bar has to render identically on
// a stripped venue PC and a mentor's laptop, so it carries its own font.
func drawScaledText(dst *image.RGBA, face *basicfont.Face, scale int, text string, padX, bandH int) {
	if text == "" || scale < 1 {
		return
	}
	advance := faceAdvance(face)
	srcW := advance * len(text)
	srcH := face.Height
	if srcW < 1 || srcH < 1 {
		return
	}
	scratch := image.NewRGBA(image.Rect(0, 0, srcW, srcH))
	draw.Draw(scratch, scratch.Bounds(), &image.Uniform{barGround}, image.Point{}, draw.Src)
	d := font.Drawer{
		Dst:  scratch,
		Src:  &image.Uniform{barInk},
		Face: face,
		Dot:  fixed.P(0, face.Ascent),
	}
	d.DrawString(text)

	// Centre the magnified line vertically in the band above the rule.
	offY := (bandH - srcH*scale) / 2
	if offY < 0 {
		offY = 0
	}
	b := dst.Bounds()
	for y := 0; y < srcH*scale; y++ {
		dy := offY + y
		if dy < 0 || dy >= b.Dy() {
			continue
		}
		sy := y / scale
		for x := 0; x < srcW*scale; x++ {
			dx := padX + x
			if dx < 0 || dx >= b.Dx() {
				continue
			}
			dst.Set(dx, dy, scratch.At(x/scale, sy))
		}
	}
}

// toBGRA flattens an *image.RGBA into the byte order both wl_shm's
// ARGB8888 and Win32's 32bpp top-down DIB want: little-endian 0xAARRGGBB,
// which in memory is B, G, R, A. Alpha is premultiplied in both formats;
// renderBar only ever produces opaque pixels, so nothing has to be
// multiplied here — a fact worth keeping true if the palette ever gains
// transparency.
func toBGRA(img *image.RGBA) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		srcRow := img.Pix[y*img.Stride : y*img.Stride+w*4]
		dstRow := out[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			r := srcRow[x*4+0]
			g := srcRow[x*4+1]
			bl := srcRow[x*4+2]
			a := srcRow[x*4+3]
			dstRow[x*4+0] = bl
			dstRow[x*4+1] = g
			dstRow[x*4+2] = r
			dstRow[x*4+3] = a
		}
	}
	return out
}
