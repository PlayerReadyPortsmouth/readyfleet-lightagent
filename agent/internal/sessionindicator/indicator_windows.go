//go:build windows

package sessionindicator

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ⚠ NOTHING IN THIS FILE HAS BEEN RUN. It was written and reviewed on a
// Linux host, which cannot execute or even meaningfully exercise Win32.
// It compiles (GOOS=windows go vet ./internal/sessionindicator/) and it
// reuses, request for request, the SetWindowDisplayAffinity idiom that
// internal/hiddenbrowser already proves in production — but "the bar
// appears and is absent from the captured video" has NOT been observed on
// Windows and must be before this ships to a venue PC or a mentor's
// laptop. See the PR description.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procUpdateLayeredWindow      = user32.NewProc("UpdateLayeredWindow")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procGetDpiForSystem          = user32.NewProc("GetDpiForSystem")
	procSetProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	procCreateCompatibleDC       = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection         = gdi32.NewProc("CreateDIBSection")
	procSelectObject             = gdi32.NewProc("SelectObject")
	procDeleteObject             = gdi32.NewProc("DeleteObject")
	procDeleteDC                 = gdi32.NewProc("DeleteDC")
	procGetModuleHandleW         = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procLoadCursorW              = user32.NewProc("LoadCursorW")
)

// wdaExcludeFromCapture is WDA_EXCLUDEFROMCAPTURE (winuser.h): the window
// stays fully visible on the local desktop but is removed from DXGI
// Desktop Duplication, Windows Graphics Capture and PrintScreen — which
// is exactly the guarantee this package needs, and needs no cooperation
// from the SolarBeam engine at all, unlike the geometric arrangement
// Linux is forced into.
//
// Two hard constraints, both load-bearing:
//
//   - The HWND must belong to the CALLING PROCESS. That is why this file
//     creates its own window class and window rather than decorating
//     something else's; internal/hiddenbrowser learned this the expensive
//     way, failing live with ERROR_ACCESS_DENIED against a spawned Chrome.
//   - It requires Windows 10 version 2004 (build 19041) or newer. On any
//     older build SetWindowDisplayAffinity returns 0 and this code
//     DESTROYS THE WINDOW and returns an error, rather than leaving a bar
//     up that the remote viewer can also see. A visible-to-the-viewer
//     indicator is worse than none: it tells the remote party whether the
//     local person has been alerted.
const wdaExcludeFromCapture = 0x00000011

const (
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExToolwindow  = 0x00000080
	wsExNoactivate  = 0x08000000
	wsExTopmost     = 0x00000008
	wsPopup         = 0x80000000

	wmDestroy = 0x0002
	wmClose   = 0x0010

	swShowNoActivate = 4

	swpNoMove     = 0x0002
	swpNoSize     = 0x0001
	swpNoActivate = 0x0010

	smCXScreen = 0
	smCYScreen = 1

	dibRGBColors = 0
	biRGB        = 0

	ulwAlpha   = 0x00000002
	acSrcOver  = 0x00
	acSrcAlpha = 0x01
	idcArrow   = 32512
)

var hwndTopmost = ^uintptr(0) // HWND_TOPMOST == (HWND)-1

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type point struct{ X, Y int32 }

type size struct{ CX, CY int32 }

type msg struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

const className = "ReadyFleetSessionIndicator"

var (
	classOnce sync.Once
	classErr  error
	// wndProcCallback is created once and never freed: windows.NewCallback
	// allocates from a small, permanently-reserved pool, so making a new
	// one per bar would exhaust it after a few hundred sessions.
	wndProcCallback uintptr
)

// New returns the Windows indicator. Nothing is created until the first
// Show, so an agent that never hosts a session never makes a window.
func New() (Indicator, error) {
	return newSupervisor(startWindowsBar, platformScreenState, slog.Default()), nil
}

// startWindowsBar creates the excluded bar on its own permanently
// OS-thread-locked goroutine.
//
// The thread lock is not optional and not a nicety: a window belongs to
// the thread that created it, only that thread may pump its messages, and
// Go will otherwise move a goroutine between OS threads freely. This is
// the same arrangement internal/hiddenbrowser uses to host its WebView2
// window, for the same reason.
func startWindowsBar(n Notice) (bar, error) {
	b := &windowsBar{done: make(chan struct{}), ready: make(chan error, 1)}
	go b.threadMain(n)
	if err := <-b.ready; err != nil {
		<-b.done
		return nil, err
	}
	return b, nil
}

type windowsBar struct {
	done  chan struct{}
	ready chan error

	// hwnd is written on the window thread before ready is signalled, and
	// read afterwards by Close on another goroutine — the channel send
	// establishes the happens-before, so no atomic is needed.
	hwnd      windows.Handle
	closeOnce sync.Once
}

func (b *windowsBar) Done() <-chan struct{} { return b.done }

// Close asks the window thread to destroy the window. PostMessage rather
// than DestroyWindow: only the owning thread may destroy its window, and
// Close is called from the supervisor's goroutine.
func (b *windowsBar) Close() {
	b.closeOnce.Do(func() {
		if b.hwnd != 0 {
			procPostMessageW.Call(uintptr(b.hwnd), wmClose, 0, 0)
		}
	})
}

func (b *windowsBar) threadMain(n Notice) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(b.done)

	hwnd, err := createBarWindow(n)
	b.hwnd = hwnd
	b.ready <- err
	if err != nil {
		return
	}

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func registerClass() error {
	classOnce.Do(func() {
		// Per-monitor DPI would be better, but the agent has no manifest
		// control over its host process in every deployment, so ask for
		// system DPI awareness and fall back to the 96 dpi assumption if
		// even that is refused. Best-effort: a bar an eighth too short is
		// a cosmetic problem, a missing bar is not.
		procSetProcessDPIAware.Call()

		wndProcCallback = windows.NewCallback(func(hwnd windows.Handle, uMsg uint32, wParam, lParam uintptr) uintptr {
			switch uMsg {
			case wmClose:
				procDestroyWindow.Call(uintptr(hwnd))
				return 0
			case wmDestroy:
				procPostQuitMessage.Call(0)
				return 0
			}
			ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
			return ret
		})

		name, err := windows.UTF16PtrFromString(className)
		if err != nil {
			classErr = fmt.Errorf("class name: %w", err)
			return
		}
		hinst, _, _ := procGetModuleHandleW.Call(0)
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   wndProcCallback,
			hInstance:     windows.Handle(hinst),
			hCursor:       windows.Handle(cursor),
			lpszClassName: name,
		}
		if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			classErr = fmt.Errorf("RegisterClassExW: %w", callErr)
		}
	})
	return classErr
}

// createBarWindow makes the layered, click-through, topmost bar across the
// top of the primary monitor, excludes it from capture, and paints it.
// Must run on the calling (locked) thread.
func createBarWindow(n Notice) (windows.Handle, error) {
	if err := registerClass(); err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(className)
	if err != nil {
		return 0, fmt.Errorf("class name: %w", err)
	}
	title, err := windows.UTF16PtrFromString("SolarBeam session indicator")
	if err != nil {
		return 0, fmt.Errorf("window title: %w", err)
	}
	hinst, _, _ := procGetModuleHandleW.Call(0)

	screenW := systemMetric(smCXScreen)
	if screenW <= 0 {
		return 0, fmt.Errorf("GetSystemMetrics(SM_CXSCREEN) returned %d", screenW)
	}
	barH := int32(float64(ReserveHeight) * systemDPIScale())
	if barH < 1 {
		barH = ReserveHeight
	}

	hwnd, _, callErr := procCreateWindowExW.Call(
		uintptr(wsExLayered|wsExTransparent|wsExToolwindow|wsExNoactivate|wsExTopmost),
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(title)),
		uintptr(uint32(wsPopup)),
		0, 0, uintptr(screenW), uintptr(barH),
		0, 0, hinst, 0,
	)
	if hwnd == 0 {
		return 0, fmt.Errorf("CreateWindowExW: %w", callErr)
	}
	h := windows.Handle(hwnd)

	// Exclusion BEFORE the window is ever shown. If this fails the window
	// is destroyed rather than displayed — see wdaExcludeFromCapture.
	if ret, _, affErr := procSetWindowDisplayAffinity.Call(hwnd, wdaExcludeFromCapture); ret == 0 {
		procDestroyWindow.Call(hwnd)
		return 0, fmt.Errorf("SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE): %w "+
			"(needs Windows 10 2004 / build 19041 or newer; refusing to show an indicator the remote viewer could also see)", affErr)
	}

	if err := paintBarWindow(h, int(screenW), int(barH), n); err != nil {
		procDestroyWindow.Call(hwnd)
		return 0, err
	}

	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	procShowWindow.Call(hwnd, swShowNoActivate)
	return h, nil
}

// paintBarWindow blits the rendered bar into the layered window.
// UpdateLayeredWindow rather than a WM_PAINT handler: a layered window's
// content is set once from a DIB and then owned by the compositor, so
// there is no repaint loop to keep alive and nothing to redraw when the
// bar is occluded.
func paintBarWindow(hwnd windows.Handle, w, h int, n Notice) error {
	pix := toBGRA(renderBar(w, h, n))

	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return fmt.Errorf("GetDC(NULL) failed")
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memDC)

	bi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      int32(-h), // negative: top-down rows, matching toBGRA's order
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	var bits unsafe.Pointer
	bitmap, _, callErr := procCreateDIBSection.Call(
		memDC, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if bitmap == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection: %w", callErr)
	}
	defer procDeleteObject.Call(bitmap)
	copy(unsafe.Slice((*byte)(bits), len(pix)), pix)

	old, _, _ := procSelectObject.Call(memDC, bitmap)
	defer procSelectObject.Call(memDC, old)

	dst := point{X: 0, Y: 0}
	sz := size{CX: int32(w), CY: int32(h)}
	src := point{X: 0, Y: 0}
	// Alpha is 255 across the whole bar (renderBar never emits
	// transparency), so ULW_ALPHA with a per-pixel alpha format renders it
	// fully opaque while still going through the layered-window path that
	// WS_EX_LAYERED requires.
	blend := blendFunction{BlendOp: acSrcOver, SourceConstantAlpha: 255, AlphaFormat: acSrcAlpha}
	ret, _, ulwErr := procUpdateLayeredWindow.Call(
		uintptr(hwnd), screenDC,
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&sz)),
		memDC, uintptr(unsafe.Pointer(&src)),
		0, uintptr(unsafe.Pointer(&blend)), ulwAlpha,
	)
	if ret == 0 {
		return fmt.Errorf("UpdateLayeredWindow: %w", ulwErr)
	}
	return nil
}

func systemMetric(index int32) int32 {
	ret, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(ret)
}

// systemDPIScale is the primary display's scale factor, defaulting to 1
// on anything predating GetDpiForSystem (Windows 10 1607).
func systemDPIScale() float64 {
	if err := procGetDpiForSystem.Find(); err != nil {
		return 1
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi == 0 {
		return 1
	}
	return float64(dpi) / 96.0
}
