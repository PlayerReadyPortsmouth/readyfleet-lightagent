//go:build windows

package companionui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var user = windows.NewLazySystemDLL("user32.dll")
var gdi = windows.NewLazySystemDLL("gdi32.dll")
var kernel = windows.NewLazySystemDLL("kernel32.dll")
var dwm = windows.NewLazySystemDLL("dwmapi.dll")
var wts = windows.NewLazySystemDLL("wtsapi32.dll")
var panels sync.Map
var classOnce sync.Once
var classErr error
var callback = windows.NewCallback(windowProc)

const refreshMessage = 0x8011
const requestMessage = 0x8012

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }
type message struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          point
	Private        uint32
}
type windowClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Name                         *uint16
	SmallIcon                          uintptr
}
type monitorInfo struct {
	Size          uint32
	Monitor, Work rect
	Flags         uint32
}

func textPtr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}

// Every Go-owned pointer is pinned through the syscall. Native handles and
// callback addresses remain uintptr; no wrapper converts unpinned stack data.
func native(d *windows.LazyDLL, name string, args ...any) uintptr {
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

type panel struct {
	hwnd, brush           uintptr
	dpi                   int32
	fonts                 []uintptr
	children              map[int]uintptr
	bounds                map[int]Rect // design rects of the controls shown on this page
	shown                 map[int]bool
	viewport              PanelViewport
	layoutPage, layoutTab int
	wheelRemainder        int32
	opts                  Options
	ctx                   context.Context
	requests              chan OpenRequest
	actions               chan Review
	refresh               chan struct{}
	mu                    sync.Mutex
	snapshot              Snapshot
	status                string
	busy                  bool
	tab                   int
	page                  int // 0 main,1 review,2 details,3 shortcuts
	review                Review
	registered            [4]bool
	shortcutError         string
	choices               []SessionChoice
	visible               bool
	staff                 StaffAuthority // nil when the backend holds no staff lease
}

func (p *panel) px(n int32) int32 { return n * p.dpi / 96 }
func (p *panel) set(id int, s string) {
	if h := p.children[id]; h != 0 {
		native(user, "SetWindowTextW", h, textPtr(s))
	}
}
func (p *panel) enable(id int, b bool) {
	v := 0
	if b {
		v = 1
	}
	native(user, "EnableWindow", p.children[id], v)
}

// Panel colours (COLORREF, 0x00BBGGRR), the compact flyout's palette.
const (
	panelBackground = 0xfffcfb // #fbfcff
	panelInk        = 0x483122 // #223148
	panelMuted      = 0x8a7466 // #66748a
	panelAccent     = 0xcb3275 // #7532cb
	panelAccentSoft = 0xfaeaf1 // #f1eafa
	panelPressed    = 0xe9d6ed // #edd6e9
	panelBorder     = 0xe9e0db // #dbe0e9
	panelDisabled   = 0xb8aaa0 // #a0aab8
	panelDisabledBg = 0xf7f4f3 // #f3f4f7
	panelWhite      = 0xffffff
)

type drawItem struct {
	CtlType, CtlID, ItemID, ItemAction, ItemState uint32
	Item, DC                                      uintptr
	Rect                                          rect
	Data                                          uintptr
}

// fonts are created once per layout at the panel's DPI, indexed by fontKind.
func (p *panel) makeFonts() {
	for _, spec := range []struct{ size, weight int32 }{{14, 400}, {12, 400}, {14, 600}, {19, 700}} {
		h := native(gdi, "CreateFontW", -p.px(spec.size), 0, 0, 0, spec.weight, 0, 0, 0, 1, 0, 0, 5, 0, textPtr("Segoe UI"))
		p.fonts = append(p.fonts, h)
	}
}
func (p *panel) child(c panelControl) error {
	style := uint32(0x40000000) | c.Style // WS_CHILD; render shows what the page needs
	switch c.Class {
	case "STATIC":
		style |= 0x80 // SS_NOPREFIX: device text is literal
	case "BUTTON":
		style |= 0xb // BS_OWNERDRAW: flat buttons, no classic bevel or dotted focus box
	}
	hwnd := native(user, "CreateWindowExW", 0, textPtr(c.Class), textPtr(c.Text), style, 0, 0, 1, 1, p.hwnd, c.ID, native(kernel, "GetModuleHandleW", 0), 0)
	if hwnd == 0 {
		return fmt.Errorf("create native control %d", c.ID)
	}
	p.children[c.ID] = hwnd
	native(user, "SendMessageW", hwnd, 0x30, p.fonts[c.Font], 1)
	return nil
}
func (p *panel) build() error {
	p.makeFonts()
	for _, c := range panelControls {
		if err := p.child(c); err != nil {
			return err
		}
	}
	return nil
}
func (p *panel) view() (Snapshot, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot.Clone(), p.status, p.busy
}
func (p *panel) render() {
	s, status, busy := p.view()
	now := time.Now()
	p.set(3, s.Device.Name+" · "+s.Device.Profile)
	if p.page == 1 && !p.review.Matches(s, now) {
		p.set(61, editText(p.review.Description+"\n\nStatus changed. Go back and review the current session."))
	}
	state := panelState{Page: p.page, Tab: p.tab, HostingActive: s.Hosting.Active}
	for id, h := range p.children {
		bounds, show := controlRect(id, state)
		if show {
			p.bounds[id] = bounds
		} else {
			delete(p.bounds, id)
		}
		if show != p.shown[id] {
			sw := 0
			if show {
				sw = 5
			}
			native(user, "ShowWindow", h, sw)
			p.shown[id] = show
		}
	}
	// Tab buttons are owner-drawn with the selected tab highlighted.
	native(user, "InvalidateRect", p.children[10], 0, 0)
	native(user, "InvalidateRect", p.children[11], 0, 0)
	p.enable(10, !busy)
	p.enable(11, !busy)
	p.enable(63, !busy)
	p.enable(37, !busy)
	if s.Notice != "" && !busy {
		status = s.Notice
	}
	p.set(22, status)
	if p.shortcutError != "" {
		p.set(23, p.shortcutError)
	} else {
		p.set(23, p.opts.Shortcuts[0].String()+" · panel")
	}
	if p.page == 0 {
		if p.tab == 1 {
			if !reflect.DeepEqual(p.choices, s.Sessions) {
				p.choices = append([]SessionChoice(nil), s.Sessions...)
				native(user, "SendMessageW", p.children[45], 0x184, 0, 0)
				for _, choice := range p.choices {
					native(user, "SendMessageW", p.children[45], 0x180, 0, textPtr(choice.Label))
				}
			}
			selected := -1
			for i, choice := range p.choices {
				if choice.RegistrationID == s.Viewing.RegistrationID {
					selected = i
				}
			}
			if !busy {
				native(user, "SendMessageW", p.children[45], 0x186, selected, 0)
			}
			p.enable(45, !busy && s.Allow(SelectSession, now) == nil)
		}
		if p.tab == 0 {
			connection := s.Device.Connection
			if connection == "" || !s.Fresh(now) {
				connection = "Fleet status unavailable"
			}
			p.set(20, connection+"\r\n"+HostingText(s, now))
			p.set(21, VersionText(s.Device))
			caption := "Actions"
			if s.Authority.StaffName == "" && !s.Authority.HostingAuthorized {
				caption = "Sign in to use staff actions"
			}
			p.set(25, caption)
			label := "Turn SolarBeam on"
			if s.Hosting.Available {
				label = "Turn SolarBeam off"
			}
			p.set(30, label)
		} else {
			label := "Choose one signed-in browser session"
			if s.Viewing.Connected && !s.Viewing.Ambiguous {
				label = s.Viewing.HostName + "\r\nConnected browser session"
			}
			if !s.Fresh(now) {
				label = "Browser session status unavailable"
			}
			p.set(20, label)
			p.set(21, "Release restores shared input; your input remains available.")
		}
	}
	actionButtons := map[int]Kind{30: SetHostingAvailable, 31: DisconnectHosting, 32: CheckUpdates, 33: OpenReadyApp, 40: AdmitGuest, 41: TakeExclusive, 42: ReleaseExclusive, 43: FocusViewer, 44: LeaveViewer}
	for id, k := range actionButtons {
		p.enable(id, !busy && s.Allow(k, now) == nil)
	}
	sign := SignIn
	p.set(34, "Sign in")
	if s.Authority.StaffName != "" {
		sign = SignOut
		p.set(34, "Sign out")
	}
	p.enable(34, !busy && s.Allow(sign, now) == nil)
	p.enable(62, !busy && p.review.Matches(s, now))
	p.enable(79, !busy && p.opts.SaveShortcuts != nil && s.Fresh(now) && !s.Authority.Locked && (s.Authority.HostingAuthorized || s.Authority.ViewerAuthorized))
	p.syncShortcuts(s.Fresh(now) && !s.Authority.Locked && (s.Authority.HostingAuthorized || s.Authority.ViewerAuthorized) && desktopAvailable())
	pageChanged := p.layoutPage != p.page || p.layoutTab != p.tab
	if pageChanged {
		p.viewport.Offset = 0
		p.layoutPage, p.layoutTab = p.page, p.tab
	}
	p.positionControls()
	if pageChanged && p.visible {
		focusID := 10
		switch p.page {
		case 1:
			focusID = 61
		case 2:
			focusID = 64
		case 3:
			focusID = 70
		}
		native(user, "SetFocus", p.children[focusID])
		p.revealFocus()
	}
	native(user, "InvalidateRect", p.hwnd, 0, 1)
}

type scrollInfo struct {
	Size, Mask              uint32
	Min, Max                int32
	Page                    uint32
	Position, TrackPosition int32
}

func (p *panel) positionControls() {
	var client rect
	native(user, "GetClientRect", p.hwnd, unsafe.Pointer(&client))
	p.viewport.Height = max(1, client.Bottom-client.Top)
	// Native non-client scrollbar consumes width; read the client area again
	// after changing its visibility, rather than assuming a DPI-specific width.
	visible := 0
	if p.viewport.MaxOffset() > 0 {
		visible = 1
	}
	native(user, "ShowScrollBar", p.hwnd, 1, visible)
	native(user, "GetClientRect", p.hwnd, unsafe.Pointer(&client))
	p.viewport.Width = max(1, client.Right-client.Left)
	p.viewport.Offset = p.viewport.Clamp(p.viewport.Offset)
	info := scrollInfo{Size: uint32(unsafe.Sizeof(scrollInfo{})), Mask: 0x7,
		Max: p.viewport.ContentHeight() - 1, Page: uint32(p.viewport.Height), Position: p.viewport.Offset}
	native(user, "SetScrollInfo", p.hwnd, 1, unsafe.Pointer(&info), 1)
	for id, h := range p.children {
		b, ok := p.bounds[id]
		if !ok {
			continue // hidden on this page
		}
		r := p.viewport.Bounds(b)
		native(user, "MoveWindow", h, r.Left, r.Top, max(1, r.Right-r.Left), max(1, r.Bottom-r.Top), 1)
	}
}
func (p *panel) scrollTo(offset int32) {
	p.viewport.Offset = p.viewport.Clamp(offset)
	p.positionControls()
}
func (p *panel) revealFocus() {
	focus := native(user, "GetFocus")
	for id, h := range p.children {
		if b, ok := p.bounds[id]; ok && h == focus && native(user, "IsWindowVisible", h) != 0 {
			p.scrollTo(p.viewport.Reveal(b))
			return
		}
	}
}

func desktopAvailable() bool {
	h := native(user, "OpenInputDesktop", 0, 0, 0x100|1)
	if h == 0 {
		return false
	}
	defer native(user, "CloseDesktop", h)
	var name [128]uint16
	var needed uint32
	if native(user, "GetUserObjectInformationW", h, 2, unsafe.Pointer(&name[0]), uint32(len(name)*2), unsafe.Pointer(&needed)) == 0 {
		return false
	}
	return strings.EqualFold(windows.UTF16ToString(name[:]), "Default")
}
func (p *panel) syncShortcuts(enabled bool) {
	p.shortcutError = ""
	for i, b := range p.opts.Shortcuts {
		snapshot, _, _ := p.view()
		want := enabled && b.Key != 0 && ShortcutEnabled(b.Request, snapshot, time.Now())
		if !want && p.registered[i] {
			native(user, "UnregisterHotKey", p.hwnd, 100+i)
			p.registered[i] = false
		}
		if want && !p.registered[i] {
			if native(user, "RegisterHotKey", p.hwnd, 100+i, b.Modifiers|0x4000, b.Key) != 0 {
				p.registered[i] = true
			} else {
				p.shortcutError = "Shortcut conflict · " + b.String()
			}
		}
	}
}
func (p *panel) hide() { native(user, "ShowWindow", p.hwnd, 0); p.visible = false; p.page = 0 }

// revokeStaff drops the backend's staff lease and refreshes so the panel shows
// "Staff sign-in expired" promptly. Hiding alone (focus loss, display change)
// deliberately does not revoke: approving in the browser takes focus away.
func (p *panel) revokeStaff(reason RevokeReason) {
	if p.staff == nil {
		return
	}
	p.staff.RevokeStaff(reason)
	select {
	case p.refresh <- struct{}{}:
	default:
	}
}

// close is an explicit dismissal (Close, WM_CLOSE, Escape on the main page).
func (p *panel) close() {
	p.hide()
	p.revokeStaff(RevokePanelClosed)
}

// isPanelInput reports queued local input aimed at the panel or its controls.
// Background refreshes never pass through here, so they cannot keep a staff
// lease alive.
func (p *panel) isPanelInput(msg *message) bool {
	switch msg.Message {
	case 0x100, 0x104, 0x201, 0x204, 0x207, 0x20a, 0xa1: // key/syskey down, L/R/M button down, wheel, non-client L down
	default:
		return false
	}
	return msg.Window == p.hwnd || native(user, "IsChild", p.hwnd, msg.Window) != 0
}
func (p *panel) open(request OpenRequest) {
	if !desktopAvailable() {
		return
	}
	p.hide()
	if err := p.layout(); err != nil {
		p.mu.Lock()
		p.status = err.Error()
		p.mu.Unlock()
		return
	}
	p.page = 0
	switch request {
	case ReviewAdmission:
		p.tab = 1
		p.prepare(AdmitGuest)
	case ReviewControl:
		p.tab = 1
		p.prepare(TakeExclusive)
	case RestoreSharedInput:
		p.tab = 1
		p.prepare(ReleaseExclusive)
	}
	p.render()
	native(user, "ShowWindow", p.hwnd, 5)
	native(user, "SetForegroundWindow", p.hwnd)
	p.visible = true
	if p.page == 1 {
		native(user, "SetFocus", p.children[61])
	} else {
		native(user, "SetFocus", p.children[10])
	}
	p.revealFocus()
}
func (p *panel) prepare(kind Kind) {
	s, _, busy := p.view()
	if busy {
		return
	}
	review, err := s.Prepare(kind, time.Now())
	if err != nil {
		p.mu.Lock()
		p.status = err.Error()
		p.mu.Unlock()
		return
	}
	p.review = review
	p.page = 1
	p.set(60, review.Title)
	p.set(61, editText(review.Description))
	p.set(62, review.ConfirmLabel)
}
func (p *panel) submit() {
	s, _, busy := p.view()
	if busy || !p.review.Matches(s, time.Now()) || !desktopAvailable() {
		return
	}
	p.mu.Lock()
	p.busy = true
	p.status = "Request in progress…"
	p.mu.Unlock()
	select {
	case p.actions <- p.review:
	default:
		p.mu.Lock()
		p.busy = false
		p.status = "A request is already running."
		p.mu.Unlock()
	}
}
func (p *panel) saveShortcuts() {
	snapshot, _, busy := p.view()
	if p.opts.SaveShortcuts == nil || busy || !snapshot.Fresh(time.Now()) || snapshot.Authority.Locked || !(snapshot.Authority.HostingAuthorized || snapshot.Authority.ViewerAuthorized) {
		return
	}
	var bindings [4]Shortcut
	for i := range bindings {
		var buf [80]uint16
		native(user, "GetWindowTextW", p.children[70+i], unsafe.Pointer(&buf[0]), 80)
		b, err := ParseShortcut(OpenRequest(i), windows.UTF16ToString(buf[:]))
		if err != nil {
			p.mu.Lock()
			p.status = err.Error()
			p.mu.Unlock()
			return
		}
		bindings[i] = b
	}
	if err := ValidateShortcuts(bindings); err != nil {
		p.mu.Lock()
		p.status = err.Error()
		p.mu.Unlock()
		return
	}
	old := p.opts.Shortcuts
	p.syncShortcuts(false)
	p.opts.Shortcuts = bindings
	p.syncShortcuts(true)
	var err error
	if p.shortcutError != "" {
		err = errors.New(p.shortcutError)
	} else {
		err = p.opts.SaveShortcuts(bindings)
	}
	if err != nil {
		p.syncShortcuts(false)
		p.opts.Shortcuts = old
		p.syncShortcuts(true)
		p.mu.Lock()
		p.status = err.Error()
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.status = "Shortcut preferences saved."
	p.mu.Unlock()
	p.page = 0
}
func windowProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	value, ok := panels.Load(hwnd)
	if !ok {
		return native(user, "DefWindowProcW", hwnd, msg, wp, lp)
	}
	p := value.(*panel)
	switch msg {
	case 0x115: // WM_VSCROLL (the panel's own scrollbar only).
		if lp == 0 {
			offset := p.viewport.Offset
			switch wp & 0xffff {
			case 0:
				offset -= p.px(28)
			case 1:
				offset += p.px(28)
			case 2:
				offset -= max(p.px(28), p.viewport.Height-p.px(28))
			case 3:
				offset += max(p.px(28), p.viewport.Height-p.px(28))
			case 4, 5:
				info := scrollInfo{Size: uint32(unsafe.Sizeof(scrollInfo{})), Mask: 0x10}
				native(user, "GetScrollInfo", hwnd, 1, unsafe.Pointer(&info))
				offset = info.TrackPosition
			case 6:
				offset = 0
			case 7:
				offset = p.viewport.MaxOffset()
			}
			p.scrollTo(offset)
		}
		return 0
	case 0x20a: // WM_MOUSEWHEEL, including forwarded button/static wheel messages.
		p.wheelRemainder += int32(int16(wp >> 16))
		steps := p.wheelRemainder / 120
		p.wheelRemainder %= 120
		p.scrollTo(p.viewport.Offset - steps*p.px(84))
		return 0
	case 0x6:
		if wp&0xffff == 0 {
			p.hide()
		}
		return 0
	case 0x10:
		p.close()
		return 0
	case 0x2b1: // WM_WTSSESSION_CHANGE: every change for this session ends staff authority.
		reason, hide := SessionChangeRevocation(wp)
		p.revokeStaff(reason)
		if hide {
			p.syncShortcuts(false)
			p.hide()
		}
		return 0
	case 0x8013:
		native(user, "PostQuitMessage", 0)
		return 0
	case 0x14:
		var r rect
		native(user, "GetClientRect", hwnd, unsafe.Pointer(&r))
		native(user, "FillRect", wp, unsafe.Pointer(&r), p.brush)
		return 1
	case 0x138: // WM_CTLCOLORSTATIC (labels and read-only edits)
		color := uintptr(panelInk)
		switch int(native(user, "GetDlgCtrlID", lp)) {
		case 3, 21, 23, 24, 25, 78:
			color = panelMuted
		}
		native(gdi, "SetTextColor", wp, color)
		native(gdi, "SetBkColor", wp, panelBackground)
		return p.brush
	case 0x2b: // WM_DRAWITEM (owner-drawn buttons)
		var item drawItem
		// LPARAM is native memory valid only for this message; copy it rather
		// than converting a uintptr into a Go pointer.
		if windows.ReadProcessMemory(windows.CurrentProcess(), lp, (*byte)(unsafe.Pointer(&item)), unsafe.Sizeof(item), nil) == nil {
			p.drawButton(&item)
		}
		return 1
	case refreshMessage:
		p.render()
		return 0
	case requestMessage:
		select {
		case r := <-p.requests:
			p.open(r)
		default:
		}
		return 0
	case 0x312:
		// ShortcutEnabled refuses while a SolarBeam viewer may be injecting
		// input. The hotkey is unregistered then too, but a WM_HOTKEY already
		// queued can outlive its registration.
		i := int(wp) - 100
		snapshot, _, _ := p.view()
		if i >= 0 && i < 4 && p.registered[i] && ShortcutEnabled(p.opts.Shortcuts[i].Request, snapshot, time.Now()) && desktopAvailable() {
			p.open(p.opts.Shortcuts[i].Request)
		}
		return 0
	case 0x111:
		switch int(wp & 0xffff) {
		case 2:
			p.close()
		case 10:
			p.tab = 0
			p.page = 0
		case 11:
			p.tab = 1
			p.page = 0
		case 30:
			p.prepare(SetHostingAvailable)
		case 31:
			p.prepare(DisconnectHosting)
		case 32:
			p.prepare(CheckUpdates)
		case 33:
			p.prepare(OpenReadyApp)
		case 34:
			s, _, _ := p.view()
			if s.Authority.StaffName != "" {
				p.prepare(SignOut)
			} else {
				p.prepare(SignIn)
			}
		case 35:
			s, _, _ := p.view()
			p.page = 2
			p.set(64, editText(s.Details))
		case 36:
			p.page = 3
			for i, b := range p.opts.Shortcuts {
				p.set(70+i, b.String())
			}
		case 37:
			select {
			case p.refresh <- struct{}{}:
			default:
			}
		case 40:
			p.prepare(AdmitGuest)
		case 41:
			p.prepare(TakeExclusive)
		case 42:
			p.prepare(ReleaseExclusive)
		case 43:
			p.prepare(FocusViewer)
		case 44:
			p.prepare(LeaveViewer)
		case 45:
			if wp>>16 == 1 {
				index := int(native(user, "SendMessageW", p.children[45], 0x188, 0, 0))
				snapshot, _, busy := p.view()
				if !busy && index >= 0 && index < len(p.choices) {
					if review, err := snapshot.PrepareSelection(p.choices[index].RegistrationID, time.Now()); err == nil {
						p.review = review
						p.submit()
					}
				}
			}
		case 62:
			p.submit()
		case 63:
			p.page = 0
		case 79:
			p.saveShortcuts()
		}
		p.render()
		return 0
	case 0x7e, 0x1a, 0x2e0:
		p.hide()
		return 0
	}
	return native(user, "DefWindowProcW", hwnd, msg, wp, lp)
}
func (p *panel) layout() error {
	var pt point
	native(user, "GetCursorPos", unsafe.Pointer(&pt))
	monitor := native(user, "MonitorFromPoint", uintptr(uint64(uint32(pt.X))|uint64(uint32(pt.Y))<<32), 2)
	mi := monitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if native(user, "GetMonitorInfoW", monitor, unsafe.Pointer(&mi)) == 0 {
		return errors.New("read monitor work area")
	}
	// Move onto the target monitor first so the DPI read below is that monitor's.
	native(user, "SetWindowPos", p.hwnd, ^uintptr(0), mi.Work.Left, mi.Work.Top, 420, PanelContentHeight+8, 0x10)
	dpi := int32(native(user, "GetDpiForWindow", p.hwnd))
	if dpi < 96 {
		dpi = 96
	}
	area := Place(Rect{mi.Work.Left, mi.Work.Top, mi.Work.Right, mi.Work.Bottom}, dpi)
	p.dpi = dpi
	p.viewport = PanelViewport{DPI: dpi}
	for _, h := range p.children {
		native(user, "DestroyWindow", h)
	}
	p.children = map[int]uintptr{}
	p.bounds = map[int]Rect{}
	p.shown = map[int]bool{}
	p.choices = nil
	for _, f := range p.fonts {
		native(gdi, "DeleteObject", f)
	}
	p.fonts = nil
	if err := p.build(); err != nil {
		return err
	}
	native(user, "SetWindowPos", p.hwnd, ^uintptr(0), area.Left, area.Top, area.Right-area.Left, area.Bottom-area.Top, 0x10)
	p.positionControls()
	return nil
}

func Run(parent context.Context, backend Backend, opts Options) error {
	if backend == nil {
		return errors.New("companion backend is required")
	}
	for i := range opts.Shortcuts {
		if opts.Shortcuts[i].Key == 0 {
			opts.Shortcuts[i].Request = OpenRequest(i)
		}
	}
	if err := ValidateShortcuts(opts.Shortcuts); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tokenUser, tokenErr := windows.GetCurrentProcessToken().GetTokenUser()
	if tokenErr != nil || tokenUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("companion cannot run as SYSTEM")
	}
	var session uint32
	if native(kernel, "ProcessIdToSessionId", native(kernel, "GetCurrentProcessId"), unsafe.Pointer(&session)) == 0 || session == 0 {
		return errors.New("companion must run in an interactive user session")
	}
	oldDPI := native(user, "SetThreadDpiAwarenessContext", ^uintptr(3))
	if oldDPI != 0 {
		defer native(user, "SetThreadDpiAwarenessContext", oldDPI)
	}
	instance := native(kernel, "GetModuleHandleW", 0)
	classOnce.Do(func() {
		wc := windowClass{Proc: callback, Instance: instance, Cursor: native(user, "LoadCursorW", 0, 32512), Name: textPtr("ReadyFleetCompanionControls")}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if native(user, "RegisterClassExW", unsafe.Pointer(&wc)) == 0 {
			classErr = errors.New("register companion panel class")
		}
	})
	if classErr != nil {
		return classErr
	}
	hwnd := native(user, "CreateWindowExW", 0x80|0x8, textPtr("ReadyFleetCompanionControls"), textPtr("ReadyFleet controls"), 0x80000000|0x800000|0x200000|0x2000000, 0, 0, 420, PanelContentHeight+8, 0, 0, instance, 0)
	if hwnd == 0 {
		return errors.New("create companion panel")
	}
	ctx, cancel := context.WithCancel(parent)
	p := &panel{hwnd: hwnd, dpi: 96, brush: native(gdi, "CreateSolidBrush", panelBackground), children: map[int]uintptr{}, shown: map[int]bool{}, opts: opts, ctx: ctx, requests: make(chan OpenRequest, 4), actions: make(chan Review, 1), refresh: make(chan struct{}, 1)}
	p.staff, _ = backend.(StaffAuthority)
	panels.Store(hwnd, p)
	// Staff authority is granted only while lock/user-switch notifications
	// are actually registered; the backend withholds it otherwise.
	monitored := native(wts, "WTSRegisterSessionNotification", hwnd, 0) != 0
	if monitored {
		defer native(wts, "WTSUnRegisterSessionNotification", hwnd)
	}
	if p.staff != nil {
		p.staff.StaffSessionMonitored(monitored)
		defer p.staff.StaffSessionMonitored(false)
	}
	defer func() {
		cancel()
		p.syncShortcuts(false)
		native(user, "DestroyWindow", hwnd)
		panels.Delete(hwnd)
		for _, f := range p.fonts {
			native(gdi, "DeleteObject", f)
		}
		native(gdi, "DeleteObject", p.brush)
	}()
	if native(user, "SetWindowDisplayAffinity", hwnd, 0x11) == 0 {
		return errors.New("capture exclusion unavailable; companion panel remains hidden")
	}
	var corner uint32 = 2
	native(dwm, "DwmSetWindowAttribute", hwnd, 33, unsafe.Pointer(&corner), 4)
	if err := p.layout(); err != nil {
		return err
	}
	var workers sync.WaitGroup
	workers.Add(2)
	defer func() { cancel(); workers.Wait() }()
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				native(user, "PostMessageW", hwnd, 0x8013, 0, 0)
				return
			case request, ok := <-opts.Requests:
				if !ok {
					opts.Requests = nil
					continue
				}
				select {
				case p.requests <- request:
					native(user, "PostMessageW", hwnd, requestMessage, 0, 0)
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		executor := NewExecutor(backend)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		read := func() {
			c, stop := context.WithTimeout(ctx, 10*time.Second)
			s, err := backend.Snapshot(c)
			stop()
			p.mu.Lock()
			if err != nil {
				p.snapshot.ObservedAt = time.Time{}
				p.status = "Status unavailable. Refresh to try again."
			} else {
				p.snapshot = s.Clone()
				// Bootstrap starts the user tray before opening its service pipe.
				// Clear that transient read failure when fresh status arrives,
				// while retaining actual action/review feedback.
				if p.status == "Status unavailable. Refresh to try again." {
					p.status = ""
				}
			}
			p.mu.Unlock()
			native(user, "PostMessageW", hwnd, refreshMessage, 0, 0)
		}
		read()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				read()
			case <-p.refresh:
				read()
			case review := <-p.actions:
				result, err := executor.Execute(ctx, review)
				read()
				p.mu.Lock()
				p.busy = false
				if err != nil {
					p.status = "Request failed: " + err.Error()
				} else if result.Message != "" {
					p.status = result.Message
				} else {
					p.status = "Request received. Waiting for confirmed status."
				}
				p.mu.Unlock()
				native(user, "PostMessageW", hwnd, refreshMessage, 0, 0)
			}
		}
	}()
	if opts.InitiallyOpen {
		p.open(OpenPanel)
	}
	var msg message
	for {
		r := native(user, "GetMessageW", unsafe.Pointer(&msg), 0, 0, 0)
		if r == 0 {
			break
		}
		if int32(r) == -1 {
			return errors.New("native companion message loop")
		}
		if p.staff != nil && p.isPanelInput(&msg) {
			p.staff.StaffInteraction()
		}
		if msg.Message == 0x100 && msg.WParam == 0x1b {
			if p.page != 0 {
				p.page = 0
				p.render()
			} else {
				p.close()
			}
			continue
		}
		if msg.Message == 0x100 && msg.WParam == 0x0d && p.clickFocusedButton() {
			continue
		}
		if native(user, "IsDialogMessageW", hwnd, unsafe.Pointer(&msg)) != 0 {
			p.revealFocus()
			continue
		}
		native(user, "TranslateMessage", unsafe.Pointer(&msg))
		native(user, "DispatchMessageW", unsafe.Pointer(&msg))
	}
	return nil
}

// drawButton paints a flat, rounded button: white with an accent label,
// accent-filled for the confirming action, tinted for the selected tab, grey
// when disabled. Keyboard focus is a 2px accent ring, and only once the
// keyboard is in use (ODS_NOFOCUSRECT); there is no dotted focus box.
func (p *panel) drawButton(d *drawItem) {
	const (
		odsSelected    = 0x1
		odsDisabled    = 0x4
		odsFocus       = 0x10
		odsNoFocusRect = 0x200
	)
	id := int(d.CtlID)
	dc := d.DC
	disabled := d.ItemState&odsDisabled != 0
	fill, line, ink := uintptr(panelWhite), uintptr(panelBorder), uintptr(panelAccent)
	switch {
	case (id == 10 && p.tab == 0) || (id == 11 && p.tab == 1):
		fill, line = panelAccentSoft, panelAccent
	case id == 10 || id == 11 || id == 2 || id == 37:
		ink = panelInk
	case id == 62 || id == 79:
		fill, line, ink = panelAccent, panelAccent, panelWhite
	}
	if disabled {
		fill, line, ink = panelDisabledBg, panelDisabledBg, panelDisabled
	}
	if d.ItemState&odsSelected != 0 && !disabled {
		fill = panelPressed
		if id == 62 || id == 79 {
			fill = panelInk
		}
	}
	width := max(1, p.px(1))
	if d.ItemState&odsFocus != 0 && d.ItemState&odsNoFocusRect == 0 {
		line, width = panelAccent, max(2, p.px(2))
		if id == 62 || id == 79 {
			line = panelInk
		}
	}
	native(user, "FillRect", dc, unsafe.Pointer(&d.Rect), p.brush)
	brush := native(gdi, "CreateSolidBrush", fill)
	pen := native(gdi, "CreatePen", 6, width, line) // PS_INSIDEFRAME
	oldBrush := native(gdi, "SelectObject", dc, brush)
	oldPen := native(gdi, "SelectObject", dc, pen)
	r := d.Rect
	native(gdi, "RoundRect", dc, r.Left, r.Top, r.Right, r.Bottom, p.px(10), p.px(10))
	native(gdi, "SelectObject", dc, oldBrush)
	native(gdi, "SelectObject", dc, oldPen)
	native(gdi, "DeleteObject", brush)
	native(gdi, "DeleteObject", pen)

	var buf [96]uint16
	n := native(user, "GetWindowTextW", d.Item, unsafe.Pointer(&buf[0]), len(buf))
	font := native(user, "SendMessageW", d.Item, 0x31, 0, 0) // WM_GETFONT
	oldFont := native(gdi, "SelectObject", dc, font)
	native(gdi, "SetBkMode", dc, 1)
	native(gdi, "SetTextColor", dc, ink)
	r.Left += p.px(8)
	r.Right -= p.px(8)
	// DT_CENTER | DT_VCENTER | DT_SINGLELINE | DT_NOPREFIX | DT_END_ELLIPSIS
	native(user, "DrawTextW", dc, unsafe.Pointer(&buf[0]), int(n), unsafe.Pointer(&r), 0x1|0x4|0x20|0x800|0x8000)
	native(gdi, "SelectObject", dc, oldFont)
}

// clickFocusedButton keeps Enter working on a focused button. The dialog
// manager only presses push buttons on Enter, and owner-drawn buttons are not
// push buttons to it, so Enter would otherwise only work as Space does.
func (p *panel) clickFocusedButton() bool {
	focus := native(user, "GetFocus")
	for _, c := range panelControls {
		if c.Class == "BUTTON" && p.children[c.ID] == focus && focus != 0 {
			native(user, "PostMessageW", focus, 0xf5, 0, 0) // BM_CLICK
			return true
		}
	}
	return false
}
