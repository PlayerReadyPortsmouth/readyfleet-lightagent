//go:build linux

package trayicon

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
)

// StatusNotifier pixmaps are network-order ARGB, not host-endian RGBA.
type iconPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

func loadTrayIcon(path string) []iconPixmap {
	if path != "" {
		if file, err := os.Open(path); err == nil {
			data, readErr := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
			_ = file.Close()
			if readErr == nil && len(data) <= 4*1024*1024 {
				if img := decodeTrayImage(data); img != nil {
					return []iconPixmap{imagePixmap(img)}
				}
			}
		}
	}
	// A self-contained mark remains visible on minimal desktops without icon
	// themes. Installed ICO files with PNG frames retain the actual brand image;
	// older bitmap-only ICO files use this blue/white ReadyFleet fallback.
	data := make([]byte, 32*32*4)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if (x-16)*(x-16)+(y-16)*(y-16) > 225 {
				continue
			}
			i := (y*32 + x) * 4
			data[i] = 255
			data[i+1] = 65
			data[i+2] = 79
			data[i+3] = 235
			if (x >= 10 && x <= 13 && y >= 8 && y <= 24) || (x >= 13 && x <= 23 && ((y >= 8 && y <= 11) || (y >= 15 && y <= 18))) {
				data[i+1] = 255
				data[i+2] = 255
				data[i+3] = 255
			}
		}
	}
	return []iconPixmap{{32, 32, data}}
}
func decodeTrayImage(data []byte) image.Image {
	if img := decodeSmallImage(data); img != nil {
		return img
	}
	if len(data) < 6 || binary.LittleEndian.Uint16(data) != 0 || binary.LittleEndian.Uint16(data[2:]) != 1 {
		return nil
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if count > 256 || len(data) < 6+16*count {
		return nil
	}
	var best image.Image
	for n := 0; n < count; n++ {
		entry := data[6+n*16 : 6+(n+1)*16]
		length := uint64(binary.LittleEndian.Uint32(entry[8:]))
		offset := uint64(binary.LittleEndian.Uint32(entry[12:]))
		if length == 0 || offset+length > uint64(len(data)) {
			continue
		}
		img := decodeSmallImage(data[offset : offset+length])
		if img == nil {
			continue
		}
		if best == nil || img.Bounds().Dx() > best.Bounds().Dx() {
			best = img
		}
	}
	return best
}
func decodeSmallImage(data []byte) image.Image {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 256 || config.Height > 256 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return img
}
func imagePixmap(img image.Image) iconPixmap {
	bounds := img.Bounds()
	out := iconPixmap{int32(bounds.Dx()), int32(bounds.Dy()), make([]byte, bounds.Dx()*bounds.Dy()*4)}
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			i := (y*bounds.Dx() + x) * 4
			// RGBA returns alpha-premultiplied channels; SNI pixmaps use ARGB32.
			out.Data[i] = byte(a >> 8)
			out.Data[i+1] = byte(r >> 8)
			out.Data[i+2] = byte(g >> 8)
			out.Data[i+3] = byte(b >> 8)
		}
	}
	return out
}
func withBadge(icons []iconPixmap) []iconPixmap {
	out := make([]iconPixmap, 0, len(icons))
	for _, icon := range icons {
		copyIcon := iconPixmap{icon.Width, icon.Height, append([]byte(nil), icon.Data...)}
		width, height := int(icon.Width), int(icon.Height)
		radius := width / 5
		if radius < 2 {
			radius = 2
		}
		cx, cy := width-radius-1, height-radius-1
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				d := (x-cx)*(x-cx) + (y-cy)*(y-cy)
				if d > (radius+1)*(radius+1) {
					continue
				}
				i := (y*width + x) * 4
				if i+3 >= len(copyIcon.Data) {
					continue
				}
				copyIcon.Data[i] = 255
				copyIcon.Data[i+1] = 255
				copyIcon.Data[i+2] = 255
				copyIcon.Data[i+3] = 255
				if d <= radius*radius {
					copyIcon.Data[i+1] = 230
					copyIcon.Data[i+2] = 35
					copyIcon.Data[i+3] = 53
				}
			}
		}
		out = append(out, copyIcon)
	}
	return out
}
