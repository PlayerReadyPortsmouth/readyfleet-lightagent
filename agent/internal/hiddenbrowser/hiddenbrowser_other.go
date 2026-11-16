//go:build !windows

package hiddenbrowser

import (
	"errors"

	webview2 "github.com/jchv/go-webview2"
)

// This package's real behavior is Windows-only (lightagent, like the
// tray it lives in, only ever runs on Windows). These stubs exist purely
// so the package compiles cross-platform, matching every other
// _windows.go/_other.go pair in this repo. webview2.WebView is declared
// in go-webview2's untagged common.go, so it's safe to reference here;
// webview2.NewWithOptions (windows-tagged) is never called from this
// file. These stubs return clear errors rather than silently succeeding;
// tests exercise Launcher via injected fakes and never hit these.
func newRealWebView(_, _, _ string) (webview2.WebView, error) {
	return nil, errors.New("hiddenbrowser: only supported on Windows")
}

func excludeFromCapture(_ uintptr) error {
	return errors.New("hiddenbrowser: only supported on Windows")
}

func foregroundWindow(_ uintptr) {}
