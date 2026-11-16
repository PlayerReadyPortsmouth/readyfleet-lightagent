//go:build windows

package staffmessage

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native staff-message banner. One top-level popup, topmost, a tool window
// (no taskbar or Alt+Tab entry) and WS_EX_NOACTIVATE, so it appears over the
// young person's windowed or borderless game WITHOUT taking keyboard focus
// from it. Real STATIC/BUTTON children, so the Dismiss button is reachable by
// mouse, touch and accessibility tools. Text is set with SS_NOPREFIX and
// never interpreted as markup.
//
// Visibility decision: the banner is NOT excluded from capture
// (WDA_EXCLUDEFROMCAPTURE is never applied). Unlike the session indicator or
// the PIN screen, a staff message is not private to the person at the PC —
// it is addressed to them by staff — and remote staff watching the screen
// through SolarBeam or a screen capture should be able to see that it
// arrived. The message text is staff-authored and already visible to anyone
// in front of the PC.
//
// Honest limits: an exclusive-fullscreen DirectX game draws over every
// desktop window, topmost or not, so the banner can be hidden behind one
// while the tray balloon and the ack still say it was shown. A locked
// workstation is detected (the input desktop is not this session's Default
// desktop) and acked as not shown.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	banners   sync.Map
	classOnce sync.Once
	classErr  error
	wndProc   = windows.NewCallback(bannerProc)
)

const (
	bannerClass = "ReadyFleetStaffMessage"
	msgWake     = 0x8031
	msgQuit     = 0x8032
	timerTick   = 1

	idAccent  = 10
	idHeader  = 11
	idTitle   = 12
	idBody    = 13
	idFooter  = 14
	idDismiss = 15

	designW = 540
	designH = 230
)

// BannerOptions wire the banner to the companion.
type BannerOptions struct {
	Queue *Queue
	// Wake is signalled by Poll whenever a message is queued.
	Wake <-chan struct{}
	// Balloon shows the same message as a tray balloon (best effort).
	Balloon func(title, body, severity string) error
}

type bRect struct{ Left, Top, Right, Bottom int32 }
type bPoint struct{ X, Y int32 }
type bMsg struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          bPoint
	Private        uint32
}
type bWndClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Name                         *uint16
	SmallIcon                          uintptr
}
type bMonitorInfo struct {
	Size          uint32
	Monitor, Work bRect
	Flags         uint32
}

func wtext(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}

// call pins Go pointers across the syscall (the companionui idiom).
func call(d *windows.LazyDLL, name string, args ...any) uintptr {
	var pin runtime.Pinner
	defer pin.Unpin()
	a := make([]uintptr, len(args))
	for i, v := range args {
		switch v := v.(type) {
		case uintptr:
			a[i] = v
		case int:
			a[i] = uintptr(v)
		case int32:
			a[i] = uintptr(v)
		case uint32:
			a[i] = uintptr(v)
		case *uint16:
			if v != nil {
				pin.Pin(v)
			}
			a[i] = uintptr(unsafe.Pointer(v))
		case unsafe.Pointer:
			if v != nil {
				pin.Pin(v)
			}
			a[i] = uintptr(v)
		default:
			panic("unsupported native argument")
		}
	}
	r, _, _ := d.NewProc(name).Call(a...)
	runtime.KeepAlive(args)
	return r
}

type banner struct {
	hwnd     uintptr
	opts     BannerOptions
	children map[int]uintptr
	fonts    []uintptr
	bg       uintptr
	accent   uintptr
	severity string
	visible  bool
	dpi      int32
}

// Severity accents (COLORREF is 0x00BBGGRR).
var accents = map[string]uintptr{
	SeverityInfo:    0xd98b3b, // #3b8bd9 blue
	SeverityWarning: 0x1fb4f5, // #f5b41f amber
	SeverityError:   0x3b3be0, // #e03b3b red
}

var headers = map[string]string{
	SeverityInfo:    "Message from staff",
	SeverityWarning: "Message from staff · Important",
	SeverityError:   "Message from staff · Urgent",
}

func (b *banner) px(n int32) int32 { return n * b.dpi / 96 }

// RunBanner owns the banner window on a locked OS thread until ctx ends.
func RunBanner(ctx context.Context, opts BannerOptions) error {
	if opts.Queue == nil {
		return errors.New("staff message banner needs a queue")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("staff message banner cannot run as SYSTEM")
	}
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil || session == 0 {
		return errors.New("staff message banner must run in an interactive user session")
	}
	if old := call(user32, "SetThreadDpiAwarenessContext", ^uintptr(3)); old != 0 {
		defer call(user32, "SetThreadDpiAwarenessContext", old)
	}
	instance := call(kernel32, "GetModuleHandleW", 0)
	classOnce.Do(func() {
		wc := bWndClass{Proc: wndProc, Instance: instance, Cursor: call(user32, "LoadCursorW", 0, 32512), Name: wtext(bannerClass)}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if call(user32, "RegisterClassExW", unsafe.Pointer(&wc)) == 0 {
			classErr = errors.New("register staff message window class")
		}
	})
	if classErr != nil {
		return classErr
	}
	// WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE; WS_POPUP | WS_BORDER | WS_CLIPCHILDREN.
	hwnd := call(user32, "CreateWindowExW", 0x8|0x80|0x08000000, wtext(bannerClass), wtext("Message from staff"),
		0x80000000|0x00800000|0x2000000, 0, 0, designW, designH, 0, 0, instance, 0)
	if hwnd == 0 {
		return errors.New("create staff message window")
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &banner{hwnd: hwnd, opts: opts, children: map[int]uintptr{}, dpi: 96,
		bg: call(gdi32, "CreateSolidBrush", 0x2b2118)}
	banners.Store(hwnd, b)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		call(user32, "KillTimer", hwnd, timerTick)
		call(user32, "DestroyWindow", hwnd)
		banners.Delete(hwnd)
		b.destroyChildren()
		call(gdi32, "DeleteObject", b.bg)
		if b.accent != 0 {
			call(gdi32, "DeleteObject", b.accent)
		}
	}()
	call(user32, "SetTimer", hwnd, timerTick, 500, 0)

	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				call(user32, "PostMessageW", hwnd, msgQuit, 0, 0)
				return
			case <-opts.Wake:
				call(user32, "PostMessageW", hwnd, msgWake, 0, 0)
			}
		}
	}()

	var m bMsg
	for {
		r := call(user32, "GetMessageW", unsafe.Pointer(&m), 0, 0, 0)
		if r == 0 {
			return nil
		}
		if int32(r) == -1 {
			return errors.New("staff message loop")
		}
		call(user32, "TranslateMessage", unsafe.Pointer(&m))
		call(user32, "DispatchMessageW", unsafe.Pointer(&m))
	}
}

func bannerProc(hwnd uintptr, message uint32, wp, lp uintptr) uintptr {
	v, ok := banners.Load(hwnd)
	if !ok {
		return call(user32, "DefWindowProcW", hwnd, message, wp, lp)
	}
	b := v.(*banner)
	switch message {
	case msgWake, 0x113: // WM_TIMER
		b.advance(time.Now())
		return 0
	case msgQuit:
		call(user32, "PostQuitMessage", 0)
		return 0
	case 0x10: // WM_CLOSE
		b.opts.Queue.Dismiss()
		b.advance(time.Now())
		return 0
	case 0x21: // WM_MOUSEACTIVATE: never take focus from the game.
		return 3 // MA_NOACTIVATE
	case 0x14: // WM_ERASEBKGND
		var r bRect
		call(user32, "GetClientRect", hwnd, unsafe.Pointer(&r))
		call(user32, "FillRect", wp, unsafe.Pointer(&r), b.bg)
		return 1
	case 0x138: // WM_CTLCOLORSTATIC
		call(gdi32, "SetBkMode", wp, 1)
		if lp == b.children[idAccent] && b.accent != 0 {
			return b.accent
		}
		color := uintptr(0xf5efe9)
		if lp == b.children[idHeader] || lp == b.children[idFooter] {
			color = 0xc8b8a8
		}
		call(gdi32, "SetTextColor", wp, color)
		return b.bg
	case 0x111: // WM_COMMAND
		if int(wp&0xffff) == idDismiss {
			b.opts.Queue.Dismiss()
			b.advance(time.Now())
		}
		return 0
	case 0x7e, 0x2e0: // WM_DISPLAYCHANGE, WM_DPICHANGED
		if b.visible {
			if it, waiting, _, ok := b.opts.Queue.Current(time.Now()); ok {
				b.layout()
				b.render(it.Message, waiting)
			}
		}
		return 0
	}
	return call(user32, "DefWindowProcW", hwnd, message, wp, lp)
}

// advance expires, promotes and shows messages, acking each one it takes
// off the queue. Runs on the window thread only.
func (b *banner) advance(now time.Time) {
	q := b.opts.Queue
	q.Expire(now)
	for {
		if _, waiting, _, ok := q.Current(now); ok {
			b.setFooter(waiting)
			// Stay above windows that opened after the banner.
			call(user32, "SetWindowPos", b.hwnd, ^uintptr(0), 0, 0, 0, 0, 0x1|0x2|0x10)
			return
		}
		it, ok := q.Promote(now)
		if !ok {
			b.hide()
			return
		}
		if !inputDesktopIsDefault() {
			q.Dismiss()
			go it.Ack(Ack{Reason: ReasonLocked})
			continue
		}
		_, waiting, _, _ := q.Current(now)
		shown := b.show(it.Message, waiting)
		tray := false
		if shown && b.opts.Balloon != nil {
			tray = b.opts.Balloon(it.Message.Title, it.Message.Body, it.Message.Severity) == nil
		}
		via := ViaFor(shown, tray)
		if via == "" {
			q.Dismiss()
			b.hide()
			go it.Ack(Ack{Reason: ReasonDisplayFailed})
			continue
		}
		go it.Ack(Ack{Displayed: true, Via: via})
		return
	}
}

// inputDesktopIsDefault is false when the secure desktop (lock screen, UAC,
// Ctrl+Alt+Del) has input: nothing on the user's desktop can be seen then.
func inputDesktopIsDefault() bool {
	h := call(user32, "OpenInputDesktop", 0, 0, 0x0001) // DESKTOP_READOBJECTS
	if h == 0 {
		return false
	}
	defer call(user32, "CloseDesktop", h)
	var name [64]uint16
	var needed uint32
	if call(user32, "GetUserObjectInformationW", h, 2, unsafe.Pointer(&name[0]), uint32(len(name)*2), unsafe.Pointer(&needed)) == 0 {
		return false
	}
	return strings.EqualFold(windows.UTF16ToString(name[:]), "Default")
}

func (b *banner) show(m Message, waiting int) bool {
	b.layout()
	b.render(m, waiting)
	call(user32, "SetWindowTextW", b.hwnd, wtext("Message from staff: "+m.Title))
	call(user32, "ShowWindow", b.hwnd, 4) // SW_SHOWNOACTIVATE
	// HWND_TOPMOST; SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE | SWP_SHOWWINDOW.
	call(user32, "SetWindowPos", b.hwnd, ^uintptr(0), 0, 0, 0, 0, 0x1|0x2|0x10|0x40)
	call(user32, "UpdateWindow", b.hwnd)
	b.visible = call(user32, "IsWindowVisible", b.hwnd) != 0
	return b.visible
}

func (b *banner) hide() {
	if b.visible {
		call(user32, "ShowWindow", b.hwnd, 0)
		b.visible = false
	}
}

func (b *banner) font(size, weight int32) uintptr {
	h := call(gdi32, "CreateFontW", -b.px(size), 0, 0, 0, weight, 0, 0, 0, 1, 0, 0, 5, 0, wtext("Segoe UI"))
	if h != 0 {
		b.fonts = append(b.fonts, h)
	}
	return h
}

func (b *banner) destroyChildren() {
	for _, c := range b.children {
		call(user32, "DestroyWindow", c)
	}
	for _, f := range b.fonts {
		call(gdi32, "DeleteObject", f)
	}
	b.children, b.fonts = map[int]uintptr{}, nil
}

// layout places the banner top-centre of the primary monitor's work area at
// that monitor's DPI and rebuilds the controls.
func (b *banner) layout() {
	monitor := call(user32, "MonitorFromPoint", uintptr(0), 1) // MONITOR_DEFAULTTOPRIMARY
	mi := bMonitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if call(user32, "GetMonitorInfoW", monitor, unsafe.Pointer(&mi)) == 0 {
		mi.Work = bRect{0, 0, 1280, 720}
	}
	// Move onto the monitor first so GetDpiForWindow reports its DPI.
	call(user32, "SetWindowPos", b.hwnd, ^uintptr(0), mi.Work.Left, mi.Work.Top, 0, 0, 0x1|0x10)
	b.dpi = max(96, int32(call(user32, "GetDpiForWindow", b.hwnd)))
	workW := mi.Work.Right - mi.Work.Left
	w := min(b.px(designW), workW-b.px(32))
	h := b.px(designH)
	x := mi.Work.Left + (workW-w)/2
	y := mi.Work.Top + b.px(24)
	call(user32, "SetWindowPos", b.hwnd, ^uintptr(0), x, y, w, h, 0x10)

	b.destroyChildren()
	header, title, body, button := b.font(13, 600), b.font(20, 700), b.font(16, 400), b.font(14, 600)
	const (
		ssNoPrefix      = 0x80
		ssEndEllipsis   = 0x4000
		ssEditControl   = 0x2000
		ssCenterImage   = 0x200
		wsTabStop       = 0x10000
		bsDefPushButton = 0x1
	)
	pad := b.px(24)
	strip := b.px(8)
	inner := w - strip - 2*pad
	b.child(idAccent, "STATIC", "", 0, 0, strip, h, header, 0)
	b.child(idHeader, "STATIC", "", strip+pad, b.px(14), inner, b.px(20), header, ssNoPrefix)
	b.child(idTitle, "STATIC", "", strip+pad, b.px(38), inner, b.px(30), title, ssNoPrefix|ssEndEllipsis)
	b.child(idBody, "STATIC", "", strip+pad, b.px(74), inner, h-b.px(74)-b.px(58), body, ssNoPrefix|ssEditControl)
	b.child(idFooter, "STATIC", "", strip+pad, h-b.px(46), inner-b.px(130), b.px(28), header, ssNoPrefix|ssCenterImage)
	b.child(idDismiss, "BUTTON", "Dismiss", w-pad-b.px(112), h-b.px(50), b.px(112), b.px(36), button, wsTabStop|bsDefPushButton)
}

func (b *banner) child(id int, class, label string, x, y, w, h int32, font uintptr, style uint32) {
	// WS_CHILD | WS_VISIBLE.
	hwnd := call(user32, "CreateWindowExW", 0, wtext(class), wtext(label), uint32(0x40000000|0x10000000)|style, x, y, w, h, b.hwnd, id, call(kernel32, "GetModuleHandleW", 0), 0)
	if hwnd == 0 {
		return
	}
	call(user32, "SendMessageW", hwnd, 0x30, font, 1) // WM_SETFONT
	b.children[id] = hwnd
}

func (b *banner) set(id int, s string) {
	if h := b.children[id]; h != 0 {
		call(user32, "SetWindowTextW", h, wtext(s))
	}
}

func (b *banner) render(m Message, waiting int) {
	if b.accent == 0 || b.severity != m.Severity {
		if b.accent != 0 {
			call(gdi32, "DeleteObject", b.accent)
		}
		color, ok := accents[m.Severity]
		if !ok {
			color = accents[SeverityInfo]
		}
		b.accent = call(gdi32, "CreateSolidBrush", color)
		b.severity = m.Severity
	}
	header, ok := headers[m.Severity]
	if !ok {
		header = headers[SeverityInfo]
	}
	b.set(idHeader, header)
	b.set(idTitle, m.Title)
	b.set(idBody, strings.ReplaceAll(m.Body, "\n", "\r\n"))
	b.setFooter(waiting)
	call(user32, "InvalidateRect", b.hwnd, 0, 1)
}

func (b *banner) setFooter(waiting int) {
	label := ""
	switch {
	case waiting == 1:
		label = "1 more message waiting"
	case waiting > 1:
		label = strconv.Itoa(waiting) + " more messages waiting"
	}
	if h := b.children[idFooter]; h != 0 {
		var buf [64]uint16
		n := call(user32, "GetWindowTextW", h, unsafe.Pointer(&buf[0]), uintptr(len(buf)))
		if windows.UTF16ToString(buf[:n]) != label {
			call(user32, "SetWindowTextW", h, wtext(label))
		}
	}
}
