//go:build windows

package hiddenbrowser

import (
	"fmt"
	"os"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

var user32 = windows.NewLazySystemDLL("user32.dll")

var (
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
	procSendMessageW             = user32.NewProc("SendMessageW")
	procLoadImageW               = user32.NewProc("LoadImageW")
)

// WM_SETICON and its two icon slots (winuser.h). ICON_BIG is what Alt-Tab
// and the taskbar use; ICON_SMALL is the title bar and the taskbar's
// grouped/compact view. Set both, or one surface keeps whatever the WebView2
// default left behind.
const (
	wmSeticon = 0x0080
	iconSmall = 0
	iconBig   = 1
)

// IMAGE_ICON + LR_LOADFROMFILE (winuser.h): load straight off disk rather
// than out of a module's resource table, since the file staged by
// cmd/lightagent's writeTrayIcons is exactly the bytes to reuse, not a
// resource ID to look up.
const (
	imageIcon      = 1
	lrLoadfromfile = 0x00000010
	lrDefaultsize  = 0x00000040
)

// setWindowIconFromFile loads iconPath and applies it as this window's icon.
// Best-effort: a mentor's personal PC can have a locked-down or unusual
// filesystem in ways a shared venue PC never does, and a window with the
// wrong icon is a cosmetic problem, not a reason to fail the whole window.
func setWindowIconFromFile(hwnd uintptr, iconPath string) {
	if iconPath == "" {
		return
	}
	pathPtr, err := windows.UTF16PtrFromString(iconPath)
	if err != nil {
		return
	}
	icon, _, _ := procLoadImageW.Call(
		0, uintptr(unsafe.Pointer(pathPtr)), imageIcon, 0, 0,
		lrLoadfromfile|lrDefaultsize,
	)
	if icon == 0 {
		return
	}
	procSendMessageW.Call(hwnd, wmSeticon, iconBig, icon)
	procSendMessageW.Call(hwnd, wmSeticon, iconSmall, icon)
}

// wdaExcludeFromCapture is WDA_EXCLUDEFROMCAPTURE (winuser.h) — excludes
// a window from any screen/video capture (including SolarBeam/Sunshine's
// own DXGI Desktop Duplication) while leaving it fully visible and
// interactive on the local desktop. Requires the calling process to own
// the target window (Microsoft's own docs: the hWnd "must belong to the
// current process") — satisfied here because this package creates the
// window itself via webview2.NewWithOptions, unlike the first
// implementation attempt (spawning a separate Chrome process), which
// could never satisfy this and failed live with ERROR_ACCESS_DENIED.
const wdaExcludeFromCapture = 0x00000011

// newRealWebView creates a WebView2 window showing url, using a
// dedicated profile directory so the mentor's login persists across
// opens without touching their real Chrome profile (see the design
// doc's §4.2 for why that reuse isn't possible). Debug is off (no
// devtools chrome on a single-purpose window); AutoFocus keeps the
// WebView2 control focused when the window itself is focused.
//
// webview2.NewWithOptions returns a nil interface (not an error) on
// failure — wrapped here into this package's (webview2.WebView, error)
// seam contract so Launcher's orchestration logic has one consistent
// failure shape to test against, regardless of which seam failed.
//
// The vendored jchv/go-webview2 library calls log.Fatal (process-killing
// os.Exit(1), not a normal error return) from several call sites inside
// its async WebView2 environment/controller creation — there is no way
// to catch or convert those into an error from here. The two checks
// below run before webview2.NewWithOptions specifically to fail on our
// own terms (returning errCreateFailed, which Open() turns into a tray
// notification) for the two most plausible real-world triggers: no
// WebView2 runtime installed, and an unwritable profile directory. This
// doesn't close every log.Fatal path — see the design doc's §5 for the
// residual risk this leaves open.
func newRealWebView(profileDir, url, iconPath string) (webview2.WebView, error) {
	// GetInstalledVersion returns ("", nil) — not an error — when the
	// runtime is genuinely absent (its own doc comment: "If there is no
	// version installed, a blank string is returned"), so both the error
	// case and the blank-version case mean "not installed" here.
	if version, err := webviewloader.GetInstalledVersion(); err != nil {
		return nil, fmt.Errorf("%w: WebView2 runtime not installed: %v", errCreateFailed, err)
	} else if version == "" {
		return nil, fmt.Errorf("%w: WebView2 runtime not installed", errCreateFailed)
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: profile directory: %v", errCreateFailed, err)
	}

	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  profileDir,
		WindowOptions: webview2.WindowOptions{
			Title:  "ReadyApp",
			Width:  1200,
			Height: 800,
			Center: true,
		},
	})
	if wv == nil {
		return nil, fmt.Errorf("%w: webview2.NewWithOptions returned nil", errCreateFailed)
	}
	// Match the tray icon exactly, not whatever WebView2's own default
	// resolves to. Set after creation via WM_SETICON rather than
	// WindowOptions.IconId: that field looks up an embedded resource by a
	// numeric ID goversioninfo assigns internally and doesn't expose as a
	// stable constant, where this loads the same .ico bytes the tray already
	// staged to disk — one asset, two surfaces, no ID to keep in sync.
	setWindowIconFromFile(uintptr(wv.Window()), iconPath)
	wv.Navigate(url)
	return wv, nil
}

func excludeFromCapture(hwnd uintptr) error {
	ret, _, callErr := procSetWindowDisplayAffinity.Call(hwnd, uintptr(wdaExcludeFromCapture))
	if ret == 0 {
		return fmt.Errorf("%w: SetWindowDisplayAffinity: %v", errExcludeFailed, callErr)
	}
	return nil
}

func foregroundWindow(hwnd uintptr) {
	procSetForegroundWindow.Call(hwnd)
}
