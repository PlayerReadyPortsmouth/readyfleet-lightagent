package sessionindicator

import (
	"image"
	"image/color"
	"testing"
)

func TestRenderBarFillsTheWholeBand(t *testing.T) {
	// Every pixel matters: a gap at the edge of the bar is a strip of
	// desktop the engine has excluded from the capture for nothing, and a
	// transparent pixel is a hole the wallpaper shows through.
	img := renderBar(800, ReserveHeight, Notice{Viewer: "Aurora"})
	if got := img.Bounds(); got != image.Rect(0, 0, 800, ReserveHeight) {
		t.Fatalf("bounds = %v, want 800x%d", got, ReserveHeight)
	}
	for _, p := range []image.Point{{0, 0}, {799, 0}, {0, ReserveHeight - 1}, {799, ReserveHeight - 1}, {400, 20}} {
		_, _, _, a := img.At(p.X, p.Y).RGBA()
		if a != 0xffff {
			t.Errorf("pixel %v has alpha %d, want fully opaque", p, a)
		}
	}
}

func TestRenderBarDrawsInk(t *testing.T) {
	// The only thing that makes this a warning rather than an orange
	// stripe is that the text actually got drawn, so assert some ink
	// landed instead of trusting the font stack.
	img := renderBar(900, ReserveHeight, Notice{})
	var ink int
	b := img.Bounds()
	for y := range b.Dy() {
		for x := range b.Dx() {
			if img.At(x, y) == color.RGBA(barInk) {
				ink++
			}
		}
	}
	if ink == 0 {
		t.Fatal("no text pixels were drawn on the bar")
	}
}

func TestRenderBarScalesWithTheBuffer(t *testing.T) {
	// A HiDPI output gets a buffer twice the logical size; the text must
	// grow with it, or the bar is a big empty band with tiny writing.
	small := inkRows(renderBar(1600, ReserveHeight, Notice{}))
	large := inkRows(renderBar(3200, 2*ReserveHeight, Notice{}))
	if large <= small {
		t.Errorf("ink spans %d rows at 1x and %d at 2x; the text did not scale", small, large)
	}
}

func TestRenderBarSurvivesAbsurdSizes(t *testing.T) {
	// The compositor is the one that decides the surface size, and a
	// degenerate configure must not panic inside a session.
	for _, tc := range []struct{ w, h int }{{0, 0}, {1, 1}, {-5, -5}, {20, ReserveHeight}, {4000, 6}} {
		img := renderBar(tc.w, tc.h, Notice{Viewer: "Somebody With A Very Long Name Indeed"})
		if img == nil || img.Bounds().Empty() {
			t.Errorf("renderBar(%d,%d) produced no image", tc.w, tc.h)
		}
	}
}

func TestToBGRAByteOrder(t *testing.T) {
	// wl_shm ARGB8888 and a Win32 32bpp DIB both want little-endian
	// 0xAARRGGBB, which is B,G,R,A in memory. Getting this backwards
	// renders a blue warning bar that nobody reads as a warning.
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff})
	got := toBGRA(img)
	want := []byte{0x33, 0x22, 0x11, 0xff}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toBGRA = % x, want % x", got, want)
		}
	}
}

func TestToBGRALength(t *testing.T) {
	got := toBGRA(renderBar(64, 8, Notice{}))
	if len(got) != 64*8*4 {
		t.Fatalf("len = %d, want %d", len(got), 64*8*4)
	}
}

// inkRows counts the distinct rows containing at least one text pixel.
func inkRows(img *image.RGBA) int {
	b := img.Bounds()
	rows := 0
	for y := range b.Dy() {
		for x := range b.Dx() {
			if img.At(x, y) == color.RGBA(barInk) {
				rows++
				break
			}
		}
	}
	return rows
}
