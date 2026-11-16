//go:build linux

// Hand-transcribed bindings for wlr-layer-shell-unstable-v1, version 4,
// from wlr-protocols' unstable/wlr-layer-shell-unstable-v1.xml.
//
// Why hand-written rather than generated: the pure-Go Wayland library this
// agent uses (github.com/rajveermalviya/go-wayland) ships bindings for the
// core protocol, xdg-shell and a set of unstable/staging protocols, but NOT
// for zwlr_layer_shell_v1, and its code generator lives in a separate module
// (github.com/rajveermalviya/go-wayland/cmd/go-wayland-scanner) that this
// build cannot fetch. Layer-shell is the only Wayland protocol that can put
// a surface on the overlay layer with an exclusive zone, which is exactly
// what a top bar the desktop must not be hidden behind requires, so there is
// no substitute to fall back on.
//
// Everything below follows the exact shape go-wayland's generator emits —
// see unstable/idle-inhibit-v1/idle_inhibit.go in that module for the
// reference — and marshals through the same client.Context/client.BaseProxy
// primitives, so it behaves identically to generated code.
//
// Opcode numbering is from the XML's declaration order, requests and events
// counted separately. Note the ordering trap: set_layer is declared AFTER
// the two events in the XML but is still request opcode 8, because event
// declarations do not advance the request counter.
//
// wlr-layer-shell-unstable-v1.xml is MIT-licensed:
//
//	Copyright © 2017 Drew DeVault
//
//	Permission to use, copy, modify, distribute, and sell this software and
//	its documentation for any purpose is hereby granted without fee, provided
//	that the above copyright notice appear in all copies and that both that
//	copyright notice and this permission notice appear in supporting
//	documentation, and that the name of the copyright holders not be used in
//	advertising or publicity pertaining to distribution of the software
//	without specific, written prior permission. The copyright holders make no
//	representations about the suitability of this software for any purpose. It
//	is provided "as is" without express or implied warranty.
package sessionindicator

import (
	"fmt"

	"github.com/rajveermalviya/go-wayland/wayland/client"
)

// ⚠ go-wayland v0.0.0-20230130181619 MARSHALS STRINGS INCORRECTLY, and
// every request in this package that carries one has to route around it.
//
// The Wayland wire format encodes a string as a uint32 byte count that
// INCLUDES the terminating NUL, then the bytes, then zero padding to the
// next 4-byte boundary. That library's client.PutString writes the PADDED
// length in the count field instead:
//
//	func PutString(dst []byte, v string, l int) {
//	    PutUint32(dst[:4], uint32(l))   // l is PaddedLen(len(v)+1)
//	    copy(dst[4:], []byte(v))
//	}
//
// Every string-bearing request it generates is therefore malformed. On
// KWin 6.7 (libwayland-server) this is not a subtle degradation — the
// FIRST such request kills the connection outright:
//
//	wl_display error code 1: "invalid arguments for wl_registry#2.bind"
//
// and every later write fails with EPIPE. Observed live on 2026-09-07:
// client.Registry.Bind fails for wl_compositor, wl_shm and wl_output
// alike, at every version, while the identical request with the count set
// to len+1 is accepted and the bound proxy works. That is why
// bindGlobal below exists rather than a call to registry.Bind, and why
// GetLayerSurface writes its namespace by hand.
//
// Scope of the workaround: only requests with a string argument are
// affected. Everything else this package sends (set_size, set_anchor,
// attach, commit, …) is integers and object ids, which the library
// marshals correctly, so the rest of it is used as-is.

// putWaylandString writes s into dst as a wire-format string: the count
// including the NUL terminator, the bytes, then NUL padding. dst must be
// at least 4+PaddedLen(len(s)+1) bytes; it returns how many it used.
func putWaylandString(dst []byte, s string) int {
	n := len(s) + 1
	padded := client.PaddedLen(n)
	client.PutUint32(dst[:4], uint32(n))
	copy(dst[4:4+padded], s)
	for i := len(s); i < padded; i++ {
		dst[4+i] = 0
	}
	return 4 + padded
}

// bindGlobal is wl_registry.bind (request 0) with correct string
// marshalling. Use it instead of client.Registry.Bind — see the note
// above.
func bindGlobal(registry *client.Registry, name uint32, iface string, version uint32, id client.Proxy) error {
	const opcode = 0
	padded := client.PaddedLen(len(iface) + 1)
	reqLen := 8 + 4 + (4 + padded) + 4 + 4
	buf := make([]byte, reqLen)
	l := 0
	client.PutUint32(buf[l:l+4], registry.ID())
	l += 4
	client.PutUint32(buf[l:l+4], uint32(reqLen<<16|opcode&0x0000ffff))
	l += 4
	client.PutUint32(buf[l:l+4], name)
	l += 4
	l += putWaylandString(buf[l:], iface)
	client.PutUint32(buf[l:l+4], version)
	l += 4
	client.PutUint32(buf[l:l+4], id.ID())
	if err := registry.Context().WriteMsg(buf, nil); err != nil {
		return fmt.Errorf("bind %s v%d: %w", iface, version, err)
	}
	return nil
}

// layerShellInterface is the registry name to bind, and layerShellVersion
// the version these bindings implement. KWin 6 advertises 5; binding 4
// keeps the wire contract to exactly what is transcribed here (v5 adds
// set_exclusive_edge, which this package does not use).
const (
	layerShellInterface = "zwlr_layer_shell_v1"
	layerShellVersion   = 4
)

// Layer values for get_layer_surface. Overlay is the topmost layer — above
// normal windows and above fullscreen ones — which is what an indicator
// that must not be coverable needs.
const (
	layerBackground uint32 = 0
	layerBottom     uint32 = 1
	layerTop        uint32 = 2
	layerOverlay    uint32 = 3
)

// Anchor bits for set_anchor.
const (
	anchorTop    uint32 = 1
	anchorBottom uint32 = 2
	anchorLeft   uint32 = 4
	anchorRight  uint32 = 8
)

// Keyboard-interactivity values for set_keyboard_interactivity. None means
// the surface never takes keyboard focus, which together with an empty
// wl_surface input region is what makes the bar unclickable and
// undismissable.
const (
	keyboardInteractivityNone uint32 = 0
)

// LayerShell is zwlr_layer_shell_v1: the factory that turns a plain
// wl_surface into a desktop-shell surface on one of the four layers.
type LayerShell struct {
	client.BaseProxy
}

// NewLayerShell registers a LayerShell proxy. Bind it via
// wl_registry.bind before use.
func NewLayerShell(ctx *client.Context) *LayerShell {
	l := &LayerShell{}
	ctx.Register(l)
	return l
}

// GetLayerSurface : create a layer surface from a wl_surface (request 0).
//
// output may be nil, which lets the compositor choose; this package always
// names an output explicitly so a two-monitor desktop gets a bar on each.
// namespace is a compositor-visible role string used for rules and
// debugging.
func (i *LayerShell) GetLayerSurface(surface *client.Surface, output *client.Output, layer uint32, namespace string) (*LayerSurface, error) {
	id := NewLayerSurface(i.Context())
	const opcode = 0
	nsLen := client.PaddedLen(len(namespace) + 1)
	_reqBufLen := 8 + 4 + 4 + 4 + 4 + (4 + nsLen)
	_reqBuf := make([]byte, _reqBufLen)
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], id.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], surface.ID())
	l += 4
	var outputID uint32 // 0 is the wire encoding of a null object
	if output != nil {
		outputID = output.ID()
	}
	client.PutUint32(_reqBuf[l:l+4], outputID)
	l += 4
	client.PutUint32(_reqBuf[l:l+4], layer)
	l += 4
	// Not client.PutString: see the wire-format note at the top of this
	// file. Getting this wrong drops the whole connection.
	l += putWaylandString(_reqBuf[l:], namespace)
	err := i.Context().WriteMsg(_reqBuf, nil)
	return id, err
}

// Destroy : destroy the layer_shell object (request 1, since version 3).
func (i *LayerShell) Destroy() error {
	defer i.Context().Unregister(i)
	const opcode = 1
	const _reqBufLen = 8
	var _reqBuf [_reqBufLen]byte
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	return i.Context().WriteMsg(_reqBuf[:], nil)
}

// LayerSurface is zwlr_layer_surface_v1: one shell surface on a layer,
// with an anchor, a size and an exclusive zone.
type LayerSurface struct {
	client.BaseProxy
	configureHandler LayerSurfaceConfigureHandlerFunc
	closedHandler    LayerSurfaceClosedHandlerFunc
}

// NewLayerSurface registers a LayerSurface proxy. Created by
// LayerShell.GetLayerSurface, not directly.
func NewLayerSurface(ctx *client.Context) *LayerSurface {
	s := &LayerSurface{}
	ctx.Register(s)
	return s
}

// SetSize : desired size (request 0). A zero dimension means "you decide",
// which is only legal when the surface is anchored to both edges on that
// axis — width 0 with LEFT|RIGHT is how a full-width bar is asked for
// without the client having to know the output's width.
func (i *LayerSurface) SetSize(width, height uint32) error {
	return i.twoUint32Request(0, width, height)
}

// SetAnchor : which edges the surface sticks to (request 1).
func (i *LayerSurface) SetAnchor(anchor uint32) error {
	return i.oneUint32Request(1, anchor)
}

// SetExclusiveZone : how much space to reserve from the anchored edge
// (request 2). A positive zone makes the compositor shrink the work area
// by that many logical pixels, so maximised windows and the panel stop
// below the bar instead of behind it. Signed on the wire: -1 means "ignore
// other surfaces' exclusive zones", 0 means "do not reserve".
func (i *LayerSurface) SetExclusiveZone(zone int32) error {
	return i.oneUint32Request(2, uint32(zone))
}

// SetMargin : outward margins from the anchored edges (request 3).
func (i *LayerSurface) SetMargin(top, right, bottom, left int32) error {
	const opcode = 3
	const _reqBufLen = 8 + 4 + 4 + 4 + 4
	var _reqBuf [_reqBufLen]byte
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(top))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(right))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(bottom))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(left))
	l += 4
	return i.Context().WriteMsg(_reqBuf[:], nil)
}

// SetKeyboardInteractivity : whether the surface can take keyboard focus
// (request 4).
func (i *LayerSurface) SetKeyboardInteractivity(v uint32) error {
	return i.oneUint32Request(4, v)
}

// AckConfigure : acknowledge a configure event (request 6). The surface is
// not mapped until a configure has been acked and a buffer committed.
func (i *LayerSurface) AckConfigure(serial uint32) error {
	return i.oneUint32Request(6, serial)
}

// Destroy : destroy the layer surface (request 7).
func (i *LayerSurface) Destroy() error {
	defer i.Context().Unregister(i)
	const opcode = 7
	const _reqBufLen = 8
	var _reqBuf [_reqBufLen]byte
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	return i.Context().WriteMsg(_reqBuf[:], nil)
}

// SetLayer : move the surface to another layer (request 8, since version
// 2). Declared after the events in the XML; still request opcode 8.
func (i *LayerSurface) SetLayer(layer uint32) error {
	return i.oneUint32Request(8, layer)
}

func (i *LayerSurface) oneUint32Request(opcode, a uint32) error {
	const _reqBufLen = 8 + 4
	var _reqBuf [_reqBufLen]byte
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], a)
	l += 4
	return i.Context().WriteMsg(_reqBuf[:], nil)
}

func (i *LayerSurface) twoUint32Request(opcode, a, b uint32) error {
	const _reqBufLen = 8 + 4 + 4
	var _reqBuf [_reqBufLen]byte
	l := 0
	client.PutUint32(_reqBuf[l:4], i.ID())
	l += 4
	client.PutUint32(_reqBuf[l:l+4], uint32(_reqBufLen<<16|opcode&0x0000ffff))
	l += 4
	client.PutUint32(_reqBuf[l:l+4], a)
	l += 4
	client.PutUint32(_reqBuf[l:l+4], b)
	l += 4
	return i.Context().WriteMsg(_reqBuf[:], nil)
}

// LayerSurfaceConfigureEvent : the compositor's answer to the requested
// geometry. Width and height are in surface-local (logical) coordinates
// and must be adopted as-is; serial must be acked before committing a
// buffer.
type LayerSurfaceConfigureEvent struct {
	Serial uint32
	Width  uint32
	Height uint32
}

type LayerSurfaceConfigureHandlerFunc func(LayerSurfaceConfigureEvent)

// SetConfigureHandler sets the handler for LayerSurfaceConfigureEvent.
func (i *LayerSurface) SetConfigureHandler(f LayerSurfaceConfigureHandlerFunc) {
	i.configureHandler = f
}

// LayerSurfaceClosedEvent : the compositor has taken the surface away (the
// output was unplugged, the surface was closed by a rule, the session
// ended). The client must destroy the surface; this package treats it as a
// reason to put the bar back, not as a dismissal.
type LayerSurfaceClosedEvent struct{}

type LayerSurfaceClosedHandlerFunc func(LayerSurfaceClosedEvent)

// SetClosedHandler sets the handler for LayerSurfaceClosedEvent.
func (i *LayerSurface) SetClosedHandler(f LayerSurfaceClosedHandlerFunc) {
	i.closedHandler = f
}

func (i *LayerSurface) Dispatch(opcode uint32, _ int, data []byte) {
	switch opcode {
	case 0:
		if i.configureHandler == nil {
			return
		}
		if len(data) < 12 {
			return
		}
		var e LayerSurfaceConfigureEvent
		e.Serial = client.Uint32(data[0:4])
		e.Width = client.Uint32(data[4:8])
		e.Height = client.Uint32(data[8:12])
		i.configureHandler(e)
	case 1:
		if i.closedHandler == nil {
			return
		}
		i.closedHandler(LayerSurfaceClosedEvent{})
	}
}
