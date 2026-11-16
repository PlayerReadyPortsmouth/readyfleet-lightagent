//go:build windows

package sessionindicator

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ⚠ NOT YET RUN ON WINDOWS, like indicator_windows.go. The Phase 0 harness
// (cmd/privacyspike) measures the same idiom on a real PC first; nothing
// that depends on this file ships before that result is recorded.

var (
	procPostThreadMessageW        = user32.NewProc("PostThreadMessageW")
	procEnumDisplayMonitors       = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procSetThreadDpiAwarenessCtx  = user32.NewProc("SetThreadDpiAwarenessContext")
	procGetCurrentThreadIdPrivacy = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentThreadId")
)

const (
	privClassName   = "ReadyFleetPrivacyOverlay"
	wmDisplayChange = 0x007E
	wmPrivWake      = 0x8001 // WM_APP+1: requests are waiting
	wmPrivRebuild   = 0x8002 // WM_APP+2: display topology changed
)

type monitorInfoEx struct {
	Size    uint32
	Monitor [4]int32
	Work    [4]int32
	Flags   uint32
	Device  [32]uint16
}

var (
	privClassOnce sync.Once
	privClassErr  error
	privWndProc   uintptr
	// privTID is the overlay thread, so the window proc (which runs on that
	// thread) can ask it to rebuild after a display change.
	privTID       atomic.Uint32
	rebuildQueued atomic.Bool

	enumMu   sync.Mutex
	enumOut  []MonitorRect
	enumOnce sync.Once
	enumCB   uintptr
)

type privReq struct {
	notice PrivacyNotice
	hide   bool
	reply  chan privReply
}

type privReply struct {
	status PrivacyStatus
	err    error
}

type windowsPrivacy struct {
	mu      sync.Mutex
	closed  bool
	running bool
	done    chan struct{}
	tid     uint32
	reqs    chan privReq
	notice  PrivacyNotice
	// written by the overlay thread, read by Show's idempotence check
	monitors, covered atomic.Int32
	log               *slog.Logger
}

// NewPrivacy returns the Windows privacy overlay. No window or thread exists
// until the first Show, and both are gone again after Hide.
func NewPrivacy() (Privacy, error) { return &windowsPrivacy{log: slog.Default()}, nil }

func (p *windowsPrivacy) Show(n PrivacyNotice) (PrivacyStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PrivacyStatus{}, errors.New("privacy overlay: closed")
	}
	if p.running {
		select {
		case <-p.done:
			p.running = false // thread died on its own
		default:
		}
	}
	if p.running && p.notice == n && p.covered.Load() == p.monitors.Load() && p.covered.Load() > 0 {
		return p.status(), nil
	}
	if !p.running {
		p.reqs = make(chan privReq, 4)
		p.done = make(chan struct{})
		ready := make(chan uint32, 1)
		go p.threadMain(ready)
		p.tid = <-ready
		p.running = true
	}
	r, err := p.call(privReq{notice: n})
	if err != nil {
		return PrivacyStatus{}, err
	}
	p.notice = n
	return r.status, r.err
}

func (p *windowsPrivacy) status() PrivacyStatus {
	return PrivacyStatus{Monitors: int(p.monitors.Load()), Covered: int(p.covered.Load())}
}

// call hands a request to the overlay thread and waits for its answer.
func (p *windowsPrivacy) call(r privReq) (privReply, error) {
	r.reply = make(chan privReply, 1)
	p.reqs <- r
	procPostThreadMessageW.Call(uintptr(p.tid), wmPrivWake, 0, 0)
	select {
	case out := <-r.reply:
		return out, nil
	case <-p.done:
		select {
		case out := <-r.reply:
			return out, nil
		default:
			return privReply{}, errors.New("privacy overlay: window thread ended")
		}
	}
}

func (p *windowsPrivacy) Hide() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	return nil
}

func (p *windowsPrivacy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	p.closed = true
	return nil
}

func (p *windowsPrivacy) stopLocked() {
	if !p.running {
		return
	}
	_, _ = p.call(privReq{hide: true})
	<-p.done
	p.running = false
}

type privWindow struct {
	hwnd windows.Handle
	rect MonitorRect
}

func (p *windowsPrivacy) threadMain(ready chan<- uint32) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(p.done)
	defer p.covered.Store(0)

	// Thread-scoped per-monitor DPI awareness: monitor rects and window
	// coordinates are physical pixels on mixed-DPI heads, without changing
	// the awareness of the rest of the host process.
	if procSetThreadDpiAwarenessCtx.Find() == nil {
		procSetThreadDpiAwarenessCtx.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)
	}
	tid, _, _ := procGetCurrentThreadIdPrivacy.Call()
	privTID.Store(uint32(tid))
	ready <- uint32(tid)

	var wins []privWindow
	var notice PrivacyNotice
	var firstErr error
	destroyAll := func() {
		for _, w := range wins {
			procDestroyWindow.Call(uintptr(w.hwnd))
		}
		wins = nil
		p.covered.Store(0)
	}
	defer destroyAll()
	rebuild := func() {
		destroyAll()
		firstErr = nil
		plan := PlanOverlays(listMonitors(), IsVirtualDisplayName)
		p.monitors.Store(int32(len(plan)))
		for _, m := range plan {
			h, err := createPrivacyWindow(m, notice)
			if err != nil {
				p.log.Warn("privacy overlay: monitor not covered", "phase", "privacy_monitor_failed", "monitor", m.Name, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			wins = append(wins, privWindow{h, m})
		}
		p.covered.Store(int32(len(wins)))
	}

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return
		}
		if m.Hwnd != 0 {
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			continue
		}
		switch m.Message {
		case wmPrivRebuild:
			rebuildQueued.Store(false)
			if len(wins) > 0 {
				rebuild()
			}
		case wmPrivWake:
			for {
				select {
				case r := <-p.reqs:
					if r.hide {
						destroyAll()
						r.reply <- privReply{}
						return
					}
					changed := r.notice != notice
					notice = r.notice
					switch {
					case len(wins) == 0 || len(wins) < int(p.monitors.Load()):
						rebuild() // first show, or retry the heads that failed
					case changed:
						for _, w := range wins {
							_ = paintPrivacy(w.hwnd, w.rect, notice)
						}
					}
					st := PrivacyStatus{Monitors: int(p.monitors.Load()), Covered: len(wins)}
					switch {
					case st.Monitors == 0:
						r.reply <- privReply{err: ErrNoMonitors}
						return
					case st.Covered == 0:
						r.reply <- privReply{err: fmt.Errorf("privacy overlay: no monitor could be covered: %w", firstErr)}
						return
					}
					r.reply <- privReply{status: st}
					continue
				default:
				}
				break
			}
		}
	}
}

func listMonitors() []MonitorRect {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumOnce.Do(func() {
		// One callback for the life of the process: windows.NewCallback
		// slots are never freed.
		enumCB = windows.NewCallback(func(hMon, hdc, rc, lp uintptr) uintptr {
			mi := monitorInfoEx{Size: uint32(unsafe.Sizeof(monitorInfoEx{}))}
			if r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
				enumOut = append(enumOut, MonitorRect{
					Left: mi.Monitor[0], Top: mi.Monitor[1], Right: mi.Monitor[2], Bottom: mi.Monitor[3],
					Primary: mi.Flags&1 != 0, Name: windows.UTF16ToString(mi.Device[:]),
				})
			}
			return 1
		})
	})
	enumOut = nil
	procEnumDisplayMonitors.Call(0, 0, enumCB, 0)
	return enumOut
}

func registerPrivacyClass() error {
	privClassOnce.Do(func() {
		privWndProc = windows.NewCallback(func(hwnd windows.Handle, uMsg uint32, wParam, lParam uintptr) uintptr {
			if uMsg == wmDisplayChange && rebuildQueued.CompareAndSwap(false, true) {
				procPostThreadMessageW.Call(uintptr(privTID.Load()), wmPrivRebuild, 0, 0)
			}
			ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
			return ret
		})
		name, err := windows.UTF16PtrFromString(privClassName)
		if err != nil {
			privClassErr = err
			return
		}
		hinst, _, _ := procGetModuleHandleW.Call(0)
		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   privWndProc,
			hInstance:     windows.Handle(hinst),
			lpszClassName: name,
		}
		if atom, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			privClassErr = fmt.Errorf("RegisterClassExW: %w", e)
		}
	})
	return privClassErr
}

// createPrivacyWindow makes one layered, click-through, topmost window over
// the whole monitor, excludes it from capture BEFORE it is shown, paints it.
// Click-through matters: the remote staff member's injected clicks are
// hit-tested like any other and an opaque hit-testable window would eat them.
func createPrivacyWindow(m MonitorRect, n PrivacyNotice) (windows.Handle, error) {
	if err := registerPrivacyClass(); err != nil {
		return 0, err
	}
	name, _ := windows.UTF16PtrFromString(privClassName)
	title, _ := windows.UTF16PtrFromString("SolarBeam privacy notice")
	hinst, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, callErr := procCreateWindowExW.Call(
		uintptr(wsExLayered|wsExTransparent|wsExToolwindow|wsExNoactivate|wsExTopmost),
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)), uintptr(uint32(wsPopup)),
		uintptr(m.Left), uintptr(m.Top), uintptr(m.W()), uintptr(m.H()),
		0, 0, hinst, 0)
	if hwnd == 0 {
		return 0, fmt.Errorf("CreateWindowExW: %w", callErr)
	}
	if ret, _, e := procSetWindowDisplayAffinity.Call(hwnd, wdaExcludeFromCapture); ret == 0 {
		procDestroyWindow.Call(hwnd)
		return 0, fmt.Errorf("SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE): %w "+
			"(refusing to show a privacy screen the remote viewer could also see)", e)
	}
	h := windows.Handle(hwnd)
	if err := paintPrivacy(h, m, n); err != nil {
		procDestroyWindow.Call(hwnd)
		return 0, err
	}
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	procShowWindow.Call(hwnd, swShowNoActivate)
	return h, nil
}

func paintPrivacy(hwnd windows.Handle, m MonitorRect, n PrivacyNotice) error {
	w, h := m.W(), m.H()
	pix := toBGRA(renderPrivacy(w, h, n))
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
	bi := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(w), Height: int32(-h), Planes: 1, BitCount: 32, Compression: biRGB}
	var bits unsafe.Pointer
	bmp, _, e := procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection: %w", e)
	}
	defer procDeleteObject.Call(bmp)
	copy(unsafe.Slice((*byte)(bits), len(pix)), pix)
	old, _, _ := procSelectObject.Call(memDC, bmp)
	defer procSelectObject.Call(memDC, old)
	sz := size{CX: int32(w), CY: int32(h)}
	src := point{}
	blend := blendFunction{BlendOp: acSrcOver, SourceConstantAlpha: 255, AlphaFormat: acSrcAlpha}
	// pptDst nil: the window keeps the position it was created at.
	if ret, _, e := procUpdateLayeredWindow.Call(uintptr(hwnd), screenDC, 0, uintptr(unsafe.Pointer(&sz)),
		memDC, uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha); ret == 0 {
		return fmt.Errorf("UpdateLayeredWindow: %w", e)
	}
	return nil
}
