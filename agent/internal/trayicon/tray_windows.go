//go:build windows

// Package trayicon drives a Windows system tray icon via direct
// Shell_NotifyIcon syscalls — the same class of hand-rolled Win32 work
// agent/internal/installerui already established this session
// (TaskDialogIndirect), reusing its lessons: verify a struct's real
// layout against an independent .NET P/Invoke before trusting it (done
// for NOTIFYICONDATAW — confirmed naturally aligned this time, unlike
// TASKDIALOGCONFIG, via Marshal.SizeOf/OffsetOf against the real struct),
// drive the window/message loop on its own runtime.LockOSThread()-locked
// goroutine, and marshal callback dispatch through a token-keyed registry
// rather than raw Go pointers stashed in GWLP_USERDATA.
package trayicon

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modshell32  = windows.NewLazySystemDLL("shell32.dll")
	moduser32   = windows.NewLazySystemDLL("user32.dll")
	modkernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procShellNotifyIconW    = modshell32.NewProc("Shell_NotifyIconW")
	procRegisterClassExW    = moduser32.NewProc("RegisterClassExW")
	procCreateWindowExW     = moduser32.NewProc("CreateWindowExW")
	procDestroyWindow       = moduser32.NewProc("DestroyWindow")
	procDefWindowProcW      = moduser32.NewProc("DefWindowProcW")
	procGetMessageW         = moduser32.NewProc("GetMessageW")
	procTranslateMessage    = moduser32.NewProc("TranslateMessage")
	procDispatchMessageW    = moduser32.NewProc("DispatchMessageW")
	procPostQuitMessage     = moduser32.NewProc("PostQuitMessage")
	procPostMessageW        = moduser32.NewProc("PostMessageW")
	procCreatePopupMenu     = moduser32.NewProc("CreatePopupMenu")
	procDestroyMenu         = moduser32.NewProc("DestroyMenu")
	procAppendMenuW         = moduser32.NewProc("AppendMenuW")
	procTrackPopupMenuEx    = moduser32.NewProc("TrackPopupMenuEx")
	procSetForegroundWindow = moduser32.NewProc("SetForegroundWindow")
	procGetCursorPos        = moduser32.NewProc("GetCursorPos")
	procLoadIconW           = moduser32.NewProc("LoadIconW")
	procLoadImageW          = moduser32.NewProc("LoadImageW")
	procDestroyIcon         = moduser32.NewProc("DestroyIcon")
	procGetModuleHandleW    = modkernel32.NewProc("GetModuleHandleW")
	procGetSystemMetrics    = moduser32.NewProc("GetSystemMetrics")
	procShellExecuteW       = modshell32.NewProc("ShellExecuteW")

	procSetProcessDpiAwarenessContext = moduser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = moduser32.NewProc("SetProcessDPIAware")
)

// dpiAwarePerMonitorV2 is (DPI_AWARENESS_CONTEXT)-4 — the full-width HANDLE
// sentinel for DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (winuser.h).
// Unlike MAKEINTRESOURCEW's WORD-sized sentinels, DPI_AWARENESS_CONTEXT is
// a full pointer-width value, so the plain 64-bit two's-complement of -4 is
// correct here (^uintptr(3), not a zero-extended 16-bit form).
const dpiAwarePerMonitorV2 = ^uintptr(3)

// init declares this process DPI-aware before any window or icon is
// touched. Without this, a Win32 process defaults to DPI-unaware: every
// GetSystemMetrics call (including the SM_CXSMICON/SM_CYSMICON lookup
// loadIconFromFile relies on) returns unscaled 96-DPI values regardless of
// the monitor's real scale factor, so on a scaled display the icon
// loadIconFromFile builds is smaller than the tray slot Explorer (itself
// per-monitor DPI aware) actually renders — Explorer centers the
// undersized bitmap in that slot rather than stretching it, which is
// exactly the "tray icon looks tiny" symptom. Falls back to the older
// system-DPI-aware API on pre-1703 Windows where the per-monitor-v2 entry
// point doesn't exist.
func init() {
	if err := procSetProcessDpiAwarenessContext.Find(); err == nil {
		if ok, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarePerMonitorV2); ok != 0 {
			return
		}
	}
	if err := procSetProcessDPIAware.Find(); err == nil {
		procSetProcessDPIAware.Call()
	}
}

// Native constants (winuser.h / shellapi.h). Only the subset this package
// actually exercises, matching installerui's own convention.
const (
	wmDestroy      = 0x0002
	wmCommand      = 0x0111
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1

	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205

	// NIN_BALLOON* — delivered through the same tray callback message as
	// mouse events, distinguished by lParam like wmLButtonUp/wmRButtonUp
	// above. CLICK fires when the user clicks the balloon itself (not the
	// tray icon); TIMEOUT/HIDE fire when it's dismissed without a click.
	ninBalloonHide      = 0x0403
	ninBalloonTimeout   = 0x0404
	ninBalloonUserClick = 0x0405

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	// NIIF_* balloon icon styles (dwInfoFlags) — Notify's Severity levels.
	niifNone    = 0x00000000
	niifInfo    = 0x00000001
	niifWarning = 0x00000002
	niifError   = 0x00000003

	csHRedraw = 0x0002
	csVRedraw = 0x0001

	tpmRightAlign  = 0x0008
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100
	tpmNonNotify   = 0x0080

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfChecked   = 0x00000008
	mfDisabled  = 0x00000002

	idiApplication = 32512 // MAKEINTRESOURCEW(32512), a stock icon ordinal

	imageIcon      = 1
	lrLoadFromFile = 0x00000010

	// SM_CXSMICON/SM_CYSMICON — the small-icon system metric the shell
	// notification area actually renders at (DPI-scaled: 16px @100%,
	// 20px @125%, 24px @150%, 32px @200%, ...). LR_DEFAULTSIZE instead
	// resolves to SM_CXICON/SM_CYICON, the *large* desktop-icon metric
	// (32px @100% and up) — loading at that size is why the tray icon
	// rendered too small: Explorer shrinks a large-metric icon down to
	// fit the notification slot rather than picking the .ico's frame
	// that actually matches the slot.
	smCxSmIcon = 49
	smCySmIcon = 50
)

// notifyIconData mirrors NOTIFYICONDATAW field-for-field. Confirmed
// naturally aligned (976 bytes on amd64) against an independent .NET
// Marshal.SizeOf/OffsetOf check before writing this — see the package
// doc comment.
type notifyIconData struct {
	cbSize            uint32
	hwnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uVersionOrTimeout uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      uintptr
}

// wndClassEx mirrors WNDCLASSEXW.
type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type point struct{ x, y int32 }

func utf16OrNil(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

// --- window class + message loop plumbing -----------------------------

var (
	wndProcPtr   = windows.NewCallback(wndProc)
	registerOnce sync.Once
	registerErr  error
)

// windows keyed by hwnd so wndProc (a single process-wide callback) can
// route each message to the right Tray instance.
var trayByHwnd sync.Map // uintptr(hwnd) -> *trayState

type trayState struct {
	hwnd      uintptr
	iconAdded bool
	mu        sync.Mutex // guards hIcon/ownsHIcon/items/notifyClickHandler together
	hIcon     uintptr
	ownsHIcon bool
	items     []MenuItem
	// notifyClickHandler, if set, is the current balloon notification's
	// click callback (see Tray.Notify) — cleared after firing, or after
	// NIN_BALLOONHIDE/NIN_BALLOONTIMEOUT, so a stale handler from an
	// already-dismissed notification can never fire late.
	notifyClickHandler func()
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	v, ok := trayByHwnd.Load(hwnd)
	if !ok {
		r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
		return r
	}
	ts := v.(*trayState)

	switch message {
	case wmTrayCallback:
		switch lParam {
		case wmLButtonUp, wmRButtonUp:
			ts.showMenu()
		case ninBalloonUserClick:
			ts.mu.Lock()
			handler := ts.notifyClickHandler
			ts.notifyClickHandler = nil
			ts.mu.Unlock()
			if handler != nil {
				go handler()
			}
		case ninBalloonHide, ninBalloonTimeout:
			ts.mu.Lock()
			ts.notifyClickHandler = nil
			ts.mu.Unlock()
		}
		return 0

	case wmCommand:
		id := int32(loword(wParam))
		ts.mu.Lock()
		items := ts.items
		ts.mu.Unlock()
		for _, it := range items {
			if it.ID == id && it.OnClick != nil {
				go it.OnClick()
				break
			}
		}
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func loword(v uintptr) uint16 { return uint16(v & 0xFFFF) }

func registerWindowClass(hInstance uintptr) error {
	registerOnce.Do(func() {
		className := utf16OrNil("ReadyFleetTrayIconClass")
		wc := wndClassEx{
			style:         csHRedraw | csVRedraw,
			lpfnWndProc:   wndProcPtr,
			hInstance:     hInstance,
			lpszClassName: className,
		}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			registerErr = fmt.Errorf("RegisterClassExW: %w", callErr)
		}
	})
	return registerErr
}

func getModuleHandle() uintptr {
	h, _, _ := procGetModuleHandleW.Call(0)
	return h
}

func createMessageWindow(hInstance uintptr) (uintptr, error) {
	const hwndMessage = ^uintptr(2) // (HWND)-3, the message-only-window parent sentinel
	className := utf16OrNil("ReadyFleetTrayIconClass")
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16OrNil("ReadyFleetTray"))),
		0, 0, 0, 0, 0,
		hwndMessage,
		0,
		hInstance,
		0,
	)
	if hwnd == 0 {
		return 0, fmt.Errorf("CreateWindowExW: %w", callErr)
	}
	return hwnd, nil
}

// --- icon loading -------------------------------------------------------

// loadStockIcon returns the stock IDI_APPLICATION icon — used when no
// real .ico asset is configured (or as a placeholder before real
// ReadyFleet tray art exists).
func loadStockIcon() uintptr {
	h, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
	return h
}

// loadIconFromFile loads a .ico file from disk sized to SM_CXSMICON /
// SM_CYSMICON — the system metric the shell notification area actually
// renders tray icons at (DPI-scaled). LoadImageW picks the closest
// matching frame out of the multi-resolution .ico and rescales only if
// needed, rather than always extracting the large-icon frame and letting
// Explorer shrink it into the tray slot.
func loadIconFromFile(path string) (uintptr, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	cx, _, _ := procGetSystemMetrics.Call(uintptr(smCxSmIcon))
	cy, _, _ := procGetSystemMetrics.Call(uintptr(smCySmIcon))
	h, _, callErr := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(p)),
		imageIcon,
		cx, cy,
		lrLoadFromFile,
	)
	if h == 0 {
		return 0, fmt.Errorf("LoadImageW: %w", callErr)
	}
	return h, nil
}

// OpenURL opens rawURL in the user's default browser via ShellExecuteW's
// "open" verb — no subprocess (avoids shelling out to rundll32/cmd, this
// session's established preference for anything security-adjacent). Only
// https URLs are accepted: ShellExecuteW's "open" verb dispatches to
// whatever protocol handler is registered for the URL's scheme, not just
// a browser, so accepting an arbitrary scheme here would let a
// notification's URL (server-supplied, over an otherwise-trusted mTLS
// channel, but still executing unreviewed on the mentor's own machine)
// invoke a local file handler, mailto, or any other registered custom
// scheme instead of just opening a web page.
func OpenURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("trayicon: OpenURL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("trayicon: OpenURL: only https URLs are allowed, got %q", u.Scheme)
	}
	op, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(rawURL)
	if err != nil {
		return err
	}
	// ShellExecuteW's return isn't a real HINSTANCE despite the C
	// signature; per its docs, a value > 32 means success.
	ret, _, callErr := procShellExecuteW.Call(
		0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(target)), 0, 0, 1, /* SW_SHOWNORMAL */
	)
	if ret <= 32 {
		return fmt.Errorf("trayicon: ShellExecuteW: %w (code %d)", callErr, ret)
	}
	return nil
}

// --- Shell_NotifyIcon wrappers -------------------------------------------

func shellNotifyIcon(dwMessage uint32, nid *notifyIconData) error {
	ret, _, callErr := procShellNotifyIconW.Call(uintptr(dwMessage), uintptr(unsafe.Pointer(nid)))
	if ret == 0 {
		return fmt.Errorf("Shell_NotifyIconW(%d): %w", dwMessage, callErr)
	}
	return nil
}

func (ts *trayState) buildNID(tooltip string) *notifyIconData {
	ts.mu.Lock()
	hIcon := ts.hIcon
	ts.mu.Unlock()
	nid := &notifyIconData{
		hwnd:             ts.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback,
		hIcon:            hIcon,
	}
	nid.cbSize = uint32(unsafe.Sizeof(*nid))
	tipUTF16, err := windows.UTF16FromString(tooltip)
	if err == nil {
		n := copy(nid.szTip[:], tipUTF16)
		if n == len(nid.szTip) {
			nid.szTip[len(nid.szTip)-1] = 0 // ensure NUL-termination if truncated
		}
	}
	return nid
}

// buildNotifyNID builds the NIM_MODIFY payload for a one-shot balloon
// notification. uFlags deliberately omits NIF_TIP: NIM_MODIFY only touches
// the fields named in uFlags, so this never clobbers whatever SetTooltip
// last set — the persistent tooltip and a transient notification are
// independent.
func (ts *trayState) buildNotifyNID(title, body string, iconFlag uint32) *notifyIconData {
	ts.mu.Lock()
	hIcon := ts.hIcon
	ts.mu.Unlock()
	nid := &notifyIconData{
		hwnd:             ts.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifInfo,
		uCallbackMessage: wmTrayCallback,
		hIcon:            hIcon,
		dwInfoFlags:      iconFlag,
	}
	nid.cbSize = uint32(unsafe.Sizeof(*nid))
	if titleUTF16, err := windows.UTF16FromString(title); err == nil {
		n := copy(nid.szInfoTitle[:], titleUTF16)
		if n == len(nid.szInfoTitle) {
			nid.szInfoTitle[len(nid.szInfoTitle)-1] = 0 // ensure NUL-termination if truncated
		}
	}
	if bodyUTF16, err := windows.UTF16FromString(body); err == nil {
		n := copy(nid.szInfo[:], bodyUTF16)
		if n == len(nid.szInfo) {
			nid.szInfo[len(nid.szInfo)-1] = 0
		}
	}
	return nid
}

// --- context menu ---------------------------------------------------------

func (ts *trayState) showMenu() {
	ts.mu.Lock()
	items := ts.items
	ts.mu.Unlock()

	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	for _, it := range items {
		flags := uintptr(mfString)
		if it.Separator {
			flags = mfSeparator
		}
		if it.Checked {
			flags |= mfChecked
		}
		if it.Disabled {
			flags |= mfDisabled
		}
		label := utf16OrNil(it.Label)
		procAppendMenuW.Call(hMenu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(label)))
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// SetForegroundWindow before TrackPopupMenu, and a follow-up WM_NULL
	// post after, are both documented Microsoft workarounds — without the
	// first the menu can fail to receive focus and never dismiss on an
	// outside click; without the second a spurious extra click is
	// sometimes needed to actually close it.
	procSetForegroundWindow.Call(ts.hwnd)
	procTrackPopupMenuEx.Call(
		hMenu,
		tpmRightAlign|tpmBottomAlign|tpmNonNotify,
		uintptr(pt.x), uintptr(pt.y),
		ts.hwnd,
		0,
	)
	procPostMessageW.Call(ts.hwnd, 0 /* WM_NULL */, 0, 0)
}

// --- public API -----------------------------------------------------------

// MenuItem is one entry in the tray's right-click context menu.
type MenuItem struct {
	ID        int32
	Label     string
	Checked   bool
	Disabled  bool
	Separator bool
	OnClick   func()
}

// Tray drives one system tray icon. IconPath, if set, is loaded as the
// icon (a real .ico file); empty falls back to a stock system icon so
// the wizard/tray plumbing is usable before real ReadyFleet art exists.
type Tray struct {
	Tooltip  string
	IconPath string

	mu    sync.Mutex
	state *trayState
}

// Run creates the tray icon and pumps its message loop until ctx is
// cancelled or a menu item's OnClick calls [Tray.Quit]. items is the
// initial context menu; SetMenuItems/SetTooltip update it live. Run must
// be called from its own goroutine — it blocks and locks the calling
// goroutine to its OS thread for the whole call, the same requirement
// installerui's TaskDialog driver has and for the same reason (the
// window/message loop is thread-affinitized).
func (t *Tray) Run(ctx context.Context, items []MenuItem) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInstance := getModuleHandle()
	if err := registerWindowClass(hInstance); err != nil {
		return err
	}
	hwnd, err := createMessageWindow(hInstance)
	if err != nil {
		return err
	}

	hIcon := loadStockIcon()
	ownsHIcon := false
	if t.IconPath != "" {
		if h, err := loadIconFromFile(t.IconPath); err == nil {
			hIcon = h
			ownsHIcon = true
		}
	}

	ts := &trayState{hwnd: hwnd, hIcon: hIcon, ownsHIcon: ownsHIcon, items: items}
	t.mu.Lock()
	t.state = ts
	t.mu.Unlock()
	trayByHwnd.Store(hwnd, ts)
	defer trayByHwnd.Delete(hwnd)

	nid := ts.buildNID(t.Tooltip)
	if err := shellNotifyIcon(nimAdd, nid); err != nil {
		procDestroyWindow.Call(hwnd)
		return err
	}
	ts.iconAdded = true
	defer func() {
		if ts.iconAdded {
			_ = shellNotifyIcon(nimDelete, nid)
		}
		// Read the CURRENT icon off ts, not the hIcon/ownsHIcon locals
		// above — SetIconPath may have swapped it (and already freed the
		// original) by the time Run unwinds; closing over the stale
		// locals here would double-free or leak depending on timing.
		ts.mu.Lock()
		finalIcon, finalOwned := ts.hIcon, ts.ownsHIcon
		ts.mu.Unlock()
		if finalOwned {
			procDestroyIcon.Call(finalIcon)
		}
		procDestroyWindow.Call(hwnd)
	}()

	// Quitting via ctx cancellation posts WM_DESTROY to unblock GetMessageW
	// from the outside (e.g. the agent's own shutdown), same as a menu
	// item's Quit action does via t.Quit below.
	go func() {
		<-ctx.Done()
		procPostMessageW.Call(hwnd, wmDestroy, 0, 0)
	}()

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		// GetMessageW returns 0 on WM_QUIT, -1 (as a very large uintptr)
		// on error; either way, stop pumping.
		if ret == 0 || ret == ^uintptr(0) {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

// SetTooltip updates the tray icon's hover tooltip live.
func (t *Tray) SetTooltip(s string) {
	t.mu.Lock()
	ts := t.state
	t.mu.Unlock()
	if ts == nil {
		return
	}
	t.Tooltip = s
	nid := ts.buildNID(s)
	_ = shellNotifyIcon(nimModify, nid)
}

// NotifySeverity selects a balloon notification's icon style.
type NotifySeverity uint32

const (
	NotifyInfo    NotifySeverity = niifInfo
	NotifyWarning NotifySeverity = niifWarning
	NotifyError   NotifySeverity = niifError
)

// notifyReadyTimeout bounds how long Notify waits for Run's Win32 setup
// (window creation + the initial NIM_ADD) to finish before giving up. Run
// is launched on its own goroutine (agent/cmd/lightagent/main.go) and a
// command asking to show a notification can arrive over the WS connection
// before that setup completes, especially right at agent startup — unlike
// SetTooltip/SetIconPath/SetMenuItems, which silently no-op if called too
// early and simply take effect next time they're called, a dropped Notify
// has no "next time": the caller only asked once. A few hundred
// milliseconds is the realistic worst case for RegisterClassExW +
// CreateWindowExW + Shell_NotifyIconW(NIM_ADD) on any real machine; this
// timeout is generous headroom above that, not a tuned deadline.
const notifyReadyTimeout = 3 * time.Second

// Notify shows a one-shot native Windows balloon/toast notification from
// the tray icon — e.g. "someone wants to connect to your device". Distinct
// from SetTooltip (a persistent hover string): this is a transient,
// attention-grabbing alert, and calling it never changes the tooltip (see
// buildNotifyNID). Safe to call repeatedly; Windows queues/replaces
// balloons on its own.
//
// onClick, if non-nil, runs (on its own goroutine, like a MenuItem's
// OnClick) if the user clicks the balloon itself before it's dismissed —
// e.g. opening a browser to a deep link. Only ever the MOST RECENT
// Notify's onClick can fire: a second Notify call replaces the first's
// handler even if its balloon is still showing, matching how Windows
// itself replaces the visible balloon rather than queuing both.
func (t *Tray) Notify(title, body string, severity NotifySeverity, onClick func()) error {
	ts := t.waitForState(notifyReadyTimeout)
	if ts == nil {
		return fmt.Errorf("trayicon: tray not running (timed out after %s waiting for Run to finish setup)", notifyReadyTimeout)
	}
	ts.mu.Lock()
	ts.notifyClickHandler = onClick
	ts.mu.Unlock()
	nid := ts.buildNotifyNID(title, body, uint32(severity))
	return shellNotifyIcon(nimModify, nid)
}

// waitForState polls for Run to have published t.state, up to timeout —
// see notifyReadyTimeout's doc comment for why Notify specifically needs
// this instead of the other setters' silent-no-op-if-not-ready behavior.
// Returns nil on timeout, same as a nil t.state would mean today.
func (t *Tray) waitForState(timeout time.Duration) *trayState {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		ts := t.state
		t.mu.Unlock()
		if ts != nil {
			return ts
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SetIconPath reloads the tray icon from a new .ico file and applies it
// live via NIM_MODIFY — used to switch between connected/offline art as
// connection state changes. The old icon handle is destroyed only if this
// Tray itself loaded it (never the very first icon if that one came from
// loadStockIcon, which doesn't need — and per LoadIconW's docs, must not
// be — destroyed).
func (t *Tray) SetIconPath(path string) error {
	t.mu.Lock()
	ts := t.state
	t.mu.Unlock()
	if ts == nil {
		return nil
	}
	h, err := loadIconFromFile(path)
	if err != nil {
		return err
	}
	ts.mu.Lock()
	old, oldOwned := ts.hIcon, ts.ownsHIcon
	ts.hIcon, ts.ownsHIcon = h, true
	ts.mu.Unlock()

	nid := ts.buildNID(t.Tooltip)
	if err := shellNotifyIcon(nimModify, nid); err != nil {
		return err
	}
	if oldOwned && old != 0 {
		procDestroyIcon.Call(old)
	}
	return nil
}

// SetMenuItems replaces the context menu shown on the next right-click —
// e.g. to flip a "Disconnect"/"Reconnect" toggle's label and Checked
// state after the user clicks it.
func (t *Tray) SetMenuItems(items []MenuItem) {
	t.mu.Lock()
	ts := t.state
	t.mu.Unlock()
	if ts == nil {
		return
	}
	ts.mu.Lock()
	ts.items = items
	ts.mu.Unlock()
}

// Quit tears down the tray icon and stops Run's message loop — the
// implementation a "Quit"/"Exit" MenuItem's OnClick should call.
func (t *Tray) Quit() {
	t.mu.Lock()
	ts := t.state
	t.mu.Unlock()
	if ts == nil {
		return
	}
	procPostMessageW.Call(ts.hwnd, wmDestroy, 0, 0)
}
