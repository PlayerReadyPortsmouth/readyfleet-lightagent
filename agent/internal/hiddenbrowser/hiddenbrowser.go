// Package hiddenbrowser lets the light agent open a window showing the
// ReadyApp PWA that is excluded from SolarBeam's own screen capture
// (SetWindowDisplayAffinity, WDA_EXCLUDEFROMCAPTURE) while remaining
// fully visible and usable to the mentor locally. See
// docs/superpowers/specs/2026-08-10-byod-hidden-browser-design.md.
//
// The window hosts a WebView2 control that lightagent creates and owns
// directly (via github.com/jchv/go-webview2) — the only way
// SetWindowDisplayAffinity can succeed, since it requires the calling
// process to own the target window (Microsoft's own docs: the hWnd
// "must belong to the current process"). A separately-spawned browser's
// window can never satisfy this — confirmed live during this feature's
// first implementation attempt (ERROR_ACCESS_DENIED spawning real
// Chrome), which is why this package hosts the browser engine itself
// instead.
package hiddenbrowser

import (
	"errors"
	"os"
	"runtime"
	"sync"

	webview2 "github.com/jchv/go-webview2"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

var (
	errCreateFailed  = errors.New("hiddenbrowser: failed to create webview")
	errExcludeFailed = errors.New("hiddenbrowser: failed to exclude window from capture")
)

// defaultReadyAppURL is the single fixed page this window shows — this
// is a single-purpose window, not a general browser (no address bar or
// navigation chrome exists to type another URL into). Overridable via
// READYFLEET_READYAPP_URL for local dev, mirroring cmd/lightagent/main.go's
// solarbeamPath env-var override convention. Namespaced with a
// READYFLEET_ prefix (not the bare READYAPP_URL a first pass used) so it
// can't collide with anything else already set on a mentor's personal PC.
const defaultReadyAppURL = "https://readyapp.player-ready.co.uk"

func readyAppURL() string {
	if v := os.Getenv("READYFLEET_READYAPP_URL"); v != "" {
		return v
	}
	return defaultReadyAppURL
}

// notifier is the subset of *trayicon.Tray Open needs — narrowed so
// tests can fake it without a real tray/window, same pattern
// cmd/lightagent/handlers.go's own notifier interface uses.
type notifier interface {
	Notify(title, body string, severity trayicon.NotifySeverity, onClick func()) error
}

// Launcher opens (or refocuses) one window showing the ReadyApp PWA,
// excluded from SolarBeam's own screen capture. See the package doc
// comment and docs/superpowers/specs/2026-08-10-byod-hidden-browser-design.md.
type Launcher struct {
	notify     notifier
	profileDir string

	mu sync.Mutex
	wv webview2.WebView // non-nil while the window is open; cleared when it closes

	newWebViewFn func(profileDir, url string) (webview2.WebView, error)
	excludeFn    func(hwnd uintptr) error
	foregroundFn func(hwnd uintptr)
	runFn        func(wv webview2.WebView)
}

// NewLauncher builds a Launcher wired to the real WebView2/Win32
// implementations (hiddenbrowser_windows.go). profileDir is where the
// window's dedicated, persistent WebView2 profile lives — callers pass
// this rather than Launcher computing it, since it depends on
// lightAppDataDir() in package main. notify is used to surface every
// failure as a tray balloon; see Open's doc comment for which message
// means what.
func NewLauncher(notify notifier, profileDir string) *Launcher {
	l := &Launcher{notify: notify, profileDir: profileDir}
	l.newWebViewFn = newRealWebView
	l.excludeFn = excludeFromCapture
	l.foregroundFn = foregroundWindow
	l.runFn = func(wv webview2.WebView) { wv.Run() }
	return l
}

// Open shows the mentor's hidden ReadyApp window: reusing/refocusing one
// that's already open (a repeat click brings the same window forward
// instead of piling up new ones), or creating one if none exists yet.
// Every failure surfaces as a tray balloon via notify rather than
// failing silently, with two distinct messages depending on what went
// wrong.
//
// WebView2 uses COM's single-threaded-apartment model: the OS thread
// that creates the control must be the same one that pumps its message
// loop for its entire life (the same constraint trayicon.Tray.Run()
// handles). Open spawns one such dedicated, runtime.LockOSThread()-locked
// goroutine per new window and blocks on a channel to get creation's
// result back — this does block for the whole WebView2
// environment/controller creation, which can take real time, not just
// briefly. What the channel split actually buys is that Open returns
// once creation finishes, without waiting for the window to be *open*
// for its whole life: the goroutine keeps running the message pump on
// that same locked thread until the mentor closes the window, long
// after Open itself has returned to the tray click handler.
func (l *Launcher) Open() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.wv != nil {
		l.foregroundFn(uintptr(l.wv.Window()))
		return nil
	}

	type createResult struct {
		wv  webview2.WebView
		err error
	}
	resultCh := make(chan createResult, 1)

	go func() {
		// Deliberately never unlocked: go-webview2 calls CoInitializeEx on
		// this thread during environment creation and never
		// CoUninitializes it, so explicitly unlocking would hand a
		// COM-initialized thread back to Go's general OS-thread pool for
		// reuse by unrelated goroutines. Leaving it locked means this
		// goroutine exiting (when the mentor closes the window) makes the
		// Go runtime terminate the OS thread outright instead — the
		// correct cleanup for an apartment-threaded resource (see
		// runtime.LockOSThread's docs).
		runtime.LockOSThread()

		wv, err := l.newWebViewFn(l.profileDir, readyAppURL())
		resultCh <- createResult{wv, err}
		if err != nil {
			return
		}
		l.runFn(wv) // blocks until the mentor closes the window
		l.mu.Lock()
		l.wv = nil
		l.mu.Unlock()
		wv.Destroy()
	}()

	res := <-resultCh
	if res.err != nil || res.wv == nil {
		// res.wv == nil alongside a nil err shouldn't happen given
		// today's newWebViewFn contract (it always pairs a non-nil error
		// with a nil WebView — see newRealWebView's doc comment), but
		// guarding it directly avoids a nil-panic on .Window() below if a
		// future seam implementation ever breaks that invariant.
		l.notifyFailure("Couldn't open hidden ReadyApp", "Try again in a moment.")
		if res.err != nil {
			return res.err
		}
		return errCreateFailed
	}

	hwnd := uintptr(res.wv.Window())
	if err := l.excludeFn(hwnd); err != nil {
		// The window DID open — just isn't hidden. Still track and
		// foreground it: a repeat click should focus it, not spawn a
		// pointless second window on top of a failure a retry is
		// unlikely to fix anyway.
		l.wv = res.wv
		l.foregroundFn(hwnd)
		l.notifyFailure("Opened, but couldn't hide it from your stream", "Close it and try again, or check your Windows version.")
		return err
	}

	l.wv = res.wv
	l.foregroundFn(hwnd)
	return nil
}

func (l *Launcher) notifyFailure(title, body string) {
	_ = l.notify.Notify(title, body, trayicon.NotifyWarning, nil)
}
