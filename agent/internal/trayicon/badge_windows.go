//go:build windows

package trayicon

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// addConnectionBadge creates a separate icon with a red dot and white rim.
// Base icon ownership stays with the caller. No icon files are rewritten.
func addConnectionBadge(base uintptr) (uintptr, error) {
	// GDI batching is per OS thread; DrawIconEx and its flush must stay on
	// the same thread even when a background status poll requests this icon.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gdi := windows.NewLazySystemDLL("gdi32.dll")
	width, _, _ := procGetSystemMetrics.Call(uintptr(smCxSmIcon))
	height, _, _ := procGetSystemMetrics.Call(uintptr(smCySmIcon))
	if width < 1 || height < 1 || width > 256 || height > 256 {
		return 0, fmt.Errorf("invalid tray icon dimensions")
	}
	dc, _, _ := gdi.NewProc("CreateCompatibleDC").Call(0)
	if dc == 0 {
		return 0, fmt.Errorf("create badge DC")
	}
	defer gdi.NewProc("DeleteDC").Call(dc)
	type bitmapHeader struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		Used, Important        uint32
	}
	header := bitmapHeader{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	var pixels *byte
	bitmap, _, _ := gdi.NewProc("CreateDIBSection").Call(dc, uintptr(unsafe.Pointer(&header)), 0, uintptr(unsafe.Pointer(&pixels)), 0, 0)
	if bitmap == 0 {
		return 0, fmt.Errorf("create badge bitmap")
	}
	defer gdi.NewProc("DeleteObject").Call(bitmap)
	if pixels == nil {
		return 0, fmt.Errorf("badge bitmap has no pixel buffer")
	}
	old, _, _ := gdi.NewProc("SelectObject").Call(dc, bitmap)
	if old == 0 || old == ^uintptr(0) {
		return 0, fmt.Errorf("select badge bitmap")
	}
	defer gdi.NewProc("SelectObject").Call(dc, old)
	data := unsafe.Slice(pixels, int(width*height*4))
	clear(data)
	ok, _, _ := moduser32.NewProc("DrawIconEx").Call(dc, 0, 0, base, width, height, 0, 0, 3)
	if ok == 0 {
		return 0, fmt.Errorf("draw base icon")
	}
	// GDI can batch DrawIconEx. Finish that work before CPU writes to the
	// DIB buffer, otherwise a delayed draw may overwrite the badge pixels.
	if flushed, _, _ := gdi.NewProc("GdiFlush").Call(); flushed == 0 {
		return 0, fmt.Errorf("flush base icon drawing")
	}
	// Native icon DIB pixels are premultiplied BGRA. Write opaque pixels so
	// GDI's zero-alpha brush behaviour cannot make the indicator disappear.
	radius := max(3, int(width)*5/16)
	cx, cy := int(width)-radius, int(height)-radius
	for y := max(0, cy-radius); y < int(height); y++ {
		for x := max(0, cx-radius); x < int(width); x++ {
			dx, dy := x-cx, y-cy
			d := dx*dx + dy*dy
			if d > radius*radius {
				continue
			}
			i := (y*int(width) + x) * 4
			data[i], data[i+1], data[i+2], data[i+3] = 255, 255, 255, 255
			if d <= (radius-1)*(radius-1) {
				data[i], data[i+1], data[i+2] = 68, 49, 220
			}
		}
	}
	maskBytes := make([]byte, ((int(width)+15)/16)*2*int(height))
	mask, _, _ := gdi.NewProc("CreateBitmap").Call(width, height, 1, 1, uintptr(unsafe.Pointer(&maskBytes[0])))
	if mask == 0 {
		return 0, fmt.Errorf("create badge mask")
	}
	defer gdi.NewProc("DeleteObject").Call(mask)
	type iconInfo struct {
		Icon        uint32
		X, Y        uint32
		Mask, Color uintptr
	}
	info := iconInfo{Icon: 1, Mask: mask, Color: bitmap}
	icon, _, _ := moduser32.NewProc("CreateIconIndirect").Call(uintptr(unsafe.Pointer(&info)))
	if icon == 0 {
		return 0, fmt.Errorf("create badged icon")
	}
	return icon, nil
}

// SetBadge changes only the visual indicator; it never changes connection state.
func (t *Tray) SetBadge(active bool) error {
	t.mu.Lock()
	ts := t.state
	t.mu.Unlock()
	if ts == nil {
		return nil
	}
	ts.mu.Lock()
	if ts.badge == active {
		ts.mu.Unlock()
		return nil
	}
	ts.badge = active
	path := ts.iconPath
	ts.mu.Unlock()
	if path == "" {
		return nil
	}
	return t.SetIconPath(path)
}
