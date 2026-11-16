package companionui

type Rect struct{ Left, Top, Right, Bottom int32 }

// Place clamps the whole panel above the taskbar on the selected monitor,
// including monitors with negative coordinates and restricted work areas.
func Place(work Rect, dpi int32) Rect {
	dpi = max(96, dpi)
	width, height := max(1, work.Right-work.Left), max(1, work.Bottom-work.Top)
	margin := min(int32(12)*dpi/96, width/4, height/4)
	w, h := min(420*dpi/96, width-2*margin), min((PanelContentHeight+8)*dpi/96, height-2*margin)
	right, bottom := work.Right-margin, work.Bottom-margin
	return Rect{right - max(1, w), bottom - max(1, h), right, bottom}
}

// PanelViewport keeps controls at the user's actual text scale. Short monitors
// scroll the original design instead of shrinking fonts or losing bottom actions.
// Coordinates are client pixels; controls are specified in the 420-DIP-wide design (layout.go).
type PanelViewport struct {
	Width, Height, DPI, Offset int32
}

func (v PanelViewport) ContentHeight() int32     { return PanelContentHeight * max(96, v.DPI) / 96 }
func (v PanelViewport) MaxOffset() int32         { return max(0, v.ContentHeight()-v.Height) }
func (v PanelViewport) Clamp(offset int32) int32 { return min(max(0, offset), v.MaxOffset()) }
func (v PanelViewport) Bounds(design Rect) Rect {
	width := max(1, v.Width)
	scaleY := func(n int32) int32 { return n*max(96, v.DPI)/96 - v.Clamp(v.Offset) }
	return Rect{design.Left * width / 420, scaleY(design.Top), design.Right * width / 420, scaleY(design.Bottom)}
}

// Reveal scrolls a keyboard-focused control wholly into view whenever it fits.
// Oversized text areas align at their top and retain their own native scroll bar.
func (v PanelViewport) Reveal(design Rect) int32 {
	r := v.Bounds(design)
	offset := v.Clamp(v.Offset)
	if r.Top < 0 || r.Bottom-r.Top > v.Height {
		return v.Clamp(offset + r.Top)
	}
	if r.Bottom > v.Height {
		return v.Clamp(offset + r.Bottom - v.Height)
	}
	return offset
}
