package hiddenbrowser

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

type notifyCall struct {
	title, body string
	severity    trayicon.NotifySeverity
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

func (f *fakeNotifier) Notify(title, body string, severity trayicon.NotifySeverity, _ func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifyCall{title, body, severity})
	return nil
}

func (f *fakeNotifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeNotifier) lastTitle() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1].title
}

// fakeWebView implements webview2.WebView with no real window/COM calls.
// Window() returns the fake's own pointer as its "window handle" — a
// genuine, vet-safe pointer (unlike converting an arbitrary uintptr to
// unsafe.Pointer, which go vet correctly flags as a possible misuse) —
// tests compare against uintptr(unsafe.Pointer(fake)) for identity. Run
// blocks on a channel the test controls directly (via Terminate), so
// tests can simulate "the mentor closed the window" deterministically.
type fakeWebView struct {
	closed    chan struct{}
	closeOnce sync.Once
	destroyed atomic.Bool
}

func newFakeWebView() *fakeWebView {
	return &fakeWebView{closed: make(chan struct{})}
}

func (f *fakeWebView) Run()                                     { <-f.closed }
func (f *fakeWebView) Terminate()                               { f.closeOnce.Do(func() { close(f.closed) }) }
func (f *fakeWebView) Dispatch(fn func())                       { fn() }
func (f *fakeWebView) Destroy()                                 { f.destroyed.Store(true) }
func (f *fakeWebView) Window() unsafe.Pointer                   { return unsafe.Pointer(f) }
func (f *fakeWebView) SetTitle(title string)                    {}
func (f *fakeWebView) SetSize(w int, h int, hint webview2.Hint) {}
func (f *fakeWebView) Navigate(url string)                      {}
func (f *fakeWebView) SetHtml(html string)                      {}
func (f *fakeWebView) Init(js string)                           {}
func (f *fakeWebView) Eval(js string)                           {}
func (f *fakeWebView) Bind(name string, fn interface{}) error   { return nil }

// newTestLauncher builds a Launcher with every seam faked to a
// deterministic "everything succeeds" happy path, so each test only
// needs to override the one or two seams it cares about.
func newTestLauncher(notify *fakeNotifier, fake *fakeWebView) *Launcher {
	return &Launcher{
		notify:       notify,
		newWebViewFn: func(profileDir, url string) (webview2.WebView, error) { return fake, nil },
		excludeFn:    func(uintptr) error { return nil },
		foregroundFn: func(uintptr) {},
		runFn:        func(wv webview2.WebView) { wv.Run() },
	}
}

func TestOpen_HappyPath(t *testing.T) {
	notify := &fakeNotifier{}
	fake := newFakeWebView()
	l := newTestLauncher(notify, fake)
	var foregrounded uintptr
	l.foregroundFn = func(hwnd uintptr) { foregrounded = hwnd }

	if err := l.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := uintptr(unsafe.Pointer(fake))
	if foregrounded != want {
		t.Fatalf("foregrounded = %#x, want %#x", foregrounded, want)
	}
	if l.wv == nil {
		t.Fatal("wv not tracked after successful Open")
	}
	if notify.callCount() != 0 {
		t.Fatalf("notify called %d times on the happy path, want 0", notify.callCount())
	}
	fake.Terminate()
}

func TestOpen_FocusesExistingWindow(t *testing.T) {
	notify := &fakeNotifier{}
	fake := newFakeWebView()
	l := &Launcher{notify: notify, wv: fake}
	l.newWebViewFn = func(profileDir, url string) (webview2.WebView, error) {
		t.Fatal("newWebViewFn must not be called when a window is already open")
		return nil, nil
	}
	var foregrounded uintptr
	l.foregroundFn = func(hwnd uintptr) { foregrounded = hwnd }

	if err := l.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := uintptr(unsafe.Pointer(fake))
	if foregrounded != want {
		t.Fatalf("foregrounded = %#x, want %#x (the existing window)", foregrounded, want)
	}
}

func TestOpen_CreationFails(t *testing.T) {
	notify := &fakeNotifier{}
	l := &Launcher{notify: notify}
	l.newWebViewFn = func(profileDir, url string) (webview2.WebView, error) { return nil, errCreateFailed }
	l.excludeFn = func(hwnd uintptr) error {
		t.Fatal("excludeFn must not be called when creation fails")
		return nil
	}

	if err := l.Open(); err == nil {
		t.Fatal("Open: want error, got nil")
	}
	if got := notify.callCount(); got != 1 {
		t.Fatalf("notify called %d times, want 1", got)
	}
	if got := notify.lastTitle(); got != "Couldn't open hidden ReadyApp" {
		t.Fatalf("notify title = %q, want %q", got, "Couldn't open hidden ReadyApp")
	}
}

func TestOpen_ExcludeFailsButWindowStillTracked(t *testing.T) {
	notify := &fakeNotifier{}
	fake := newFakeWebView()
	l := newTestLauncher(notify, fake)
	l.excludeFn = func(uintptr) error { return errExcludeFailed }
	var foregrounded uintptr
	l.foregroundFn = func(hwnd uintptr) { foregrounded = hwnd }

	if err := l.Open(); err == nil {
		t.Fatal("Open: want error, got nil")
	}
	if got := notify.callCount(); got != 1 {
		t.Fatalf("notify called %d times, want 1", got)
	}
	if got := notify.lastTitle(); got != "Opened, but couldn't hide it from your stream" {
		t.Fatalf("notify title = %q, want the distinct exclude-failure warning", got)
	}
	if l.wv == nil {
		t.Fatal("window should still be tracked even though exclude failed")
	}
	want := uintptr(unsafe.Pointer(fake))
	if foregrounded != want {
		t.Fatalf("foregrounded = %#x, want %#x", foregrounded, want)
	}
	fake.Terminate()
}

func TestOpen_SerializesAgainstDoubleClick(t *testing.T) {
	notify := &fakeNotifier{}
	l := &Launcher{notify: notify}
	var createCalls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := newFakeWebView()
	l.newWebViewFn = func(profileDir, url string) (webview2.WebView, error) {
		atomic.AddInt32(&createCalls, 1)
		close(entered)
		<-release
		return fake, nil
	}
	l.excludeFn = func(uintptr) error { return nil }
	l.foregroundFn = func(uintptr) {}
	l.runFn = func(wv webview2.WebView) { wv.Run() }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = l.Open() }() // blocks inside newWebViewFn, holding l.mu

	<-entered // goroutine 1 confirmed inside newWebViewFn before we start goroutine 2

	wg.Add(1)
	go func() { defer wg.Done(); _ = l.Open() }() // must block on l.mu until goroutine 1 finishes

	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&createCalls); got != 1 {
		t.Fatalf("newWebViewFn called %d times, want exactly 1 (second Open() should have focused the window goroutine 1 created)", got)
	}
	fake.Terminate()
}

// TestOpen_ReopensAfterClose covers the transition Terminate() ->
// background goroutine clears l.wv -> next Open() creates a genuinely
// fresh window, rather than errorring or incorrectly reusing stale
// state. l.wv is cleared by the same background goroutine that runs
// runFn (see Open's doc comment), asynchronously to Terminate()
// returning, so the test polls for that cleanup to actually land before
// reopening instead of racing ahead of it.
func TestOpen_ReopensAfterClose(t *testing.T) {
	notify := &fakeNotifier{}
	fake1 := newFakeWebView()
	fake2 := newFakeWebView()
	l := newTestLauncher(notify, fake1)
	var createCalls int32
	l.newWebViewFn = func(profileDir, url string) (webview2.WebView, error) {
		if atomic.AddInt32(&createCalls, 1) == 1 {
			return fake1, nil
		}
		return fake2, nil
	}

	if err := l.Open(); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if got := atomic.LoadInt32(&createCalls); got != 1 {
		t.Fatalf("newWebViewFn called %d times after first Open, want 1", got)
	}

	fake1.Terminate() // simulates the mentor closing the window

	deadline := time.Now().Add(time.Second)
	for {
		l.mu.Lock()
		cleared := l.wv == nil
		l.mu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for l.wv to clear after Terminate()")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !fake1.destroyed.Load() {
		t.Fatal("first webview should have been Destroyed once the message pump goroutine unwound")
	}

	if err := l.Open(); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if got := atomic.LoadInt32(&createCalls); got != 2 {
		t.Fatalf("newWebViewFn called %d times after second Open, want 2 (a genuinely fresh window, not a stale reuse)", got)
	}
	l.mu.Lock()
	reopened := l.wv
	l.mu.Unlock()
	if reopened != webview2.WebView(fake2) {
		t.Fatal("second Open should track the newly created window")
	}

	fake2.Terminate()
}
