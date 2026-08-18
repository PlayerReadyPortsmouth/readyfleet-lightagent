//go:build windows

// Package installerui drives both ReadyFleet installers' user-facing
// wizard: a welcome screen explaining what's about to happen and why,
// a live progress screen, and a result screen that always shows the
// real outcome — never a bare exit code from a console window that
// flashes and vanishes. That silent-failure mode is what turned one real
// bug into a two-hour blind investigation this session; this package is
// the direct fix for that, independent of what the bug itself was.
//
// Built on TaskDialogIndirect (comctl32.dll) via direct syscalls — the
// same native dialog API behind Windows' own installers and UAC-style
// prompts, not a third-party GUI toolkit. Requires the process to embed
// a ComCtl32 v6 manifest (see agent/cmd/light-installer/installer.manifest
// and agent/cmd/managed-installer/installer.manifest) — without one,
// Windows silently falls back to the ancient v5 common controls DLL,
// which does not implement TaskDialogIndirect at all.
package installerui

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modcomctl32            = windows.NewLazySystemDLL("comctl32.dll")
	procTaskDialogIndirect = modcomctl32.NewProc("TaskDialogIndirect")
)

// Native TASKDIALOGCONFIG flags/constants (commctrl.h). Deliberately not
// declared as Go constants with descriptive names beyond what's used —
// this file only needs the small subset the wizard actually exercises.
const (
	tdfAllowDialogCancellation = 0x0008
	tdfShowProgressBar         = 0x0200
	tdfExpandedByDefault       = 0x0080
	tdfSizeToContent           = 0x01000000

	tdnCreated = 0

	tdmSetProgressBarPos   = 0x0400 + 106
	tdmSetProgressBarRange = 0x0400 + 105
	tdmUpdateElementText   = 0x0400 + 114
	tdmClickButton         = 0x0400 + 102
	tdmEnableButton        = 0x0400 + 111

	tdeContent = 0

	// Sentinel "icon" values, passed directly in the icon union slot (no
	// TDF_USE_HICON_MAIN needed). These are MAKEINTRESOURCEW(-N): the C
	// macro casts through WORD (unsigned 16-bit) before widening, i.e.
	// zero-extends (WORD)(-N), NOT a sign-extended 64-bit -N.
	tdInformationIcon = uintptr(0xFFFD) // MAKEINTRESOURCEW(-3)
	tdErrorIcon       = uintptr(0xFFFE) // MAKEINTRESOURCEW(-2)

	idOK     = 1
	idCancel = 2
)

// taskDialogConfig mirrors TASKDIALOGCONFIG's fields, in order, as an
// ergonomic Go struct for callers in this package to fill in. It is NOT
// passed to TaskDialogIndirect directly — marshal() packs it into the raw
// byte layout the API actually expects first. See marshal's doc comment
// for why: TASKDIALOGCONFIG (and TASKDIALOG_BUTTON) are declared
// byte-packed in commctrl.h, with no natural-alignment padding between
// members, unlike most Win32 structs.
type taskDialogConfig struct {
	hwndParent              uintptr
	hInstance               uintptr
	dwFlags                 uint32
	dwCommonButtons         uint32
	pszWindowTitle          *uint16
	mainIcon                uintptr
	pszMainInstruction      *uint16
	pszContent              *uint16
	cButtons                uint32
	pButtons                uintptr
	nDefaultButton          int32
	cRadioButtons           uint32
	pRadioButtons           uintptr
	nDefaultRadioButton     int32
	pszVerificationText     *uint16
	pszExpandedInformation  *uint16
	pszExpandedControlText  *uint16
	pszCollapsedControlText *uint16
	footerIcon              uintptr
	pszFooter               *uint16
	pfCallback              uintptr
	lpCallbackData          uintptr
	cxWidth                 uint32
}

// taskDialogConfigSize is sizeof(TASKDIALOGCONFIG) on amd64 — 160 bytes,
// byte-packed (not the 176 a naturally-aligned Go struct of the same
// fields would produce). Confirmed empirically on this machine: a
// naturally-aligned 176-byte layout made every TaskDialogIndirect call
// fail instantly with E_INVALIDARG (0x80070057), reproduced identically
// through an independent .NET P/Invoke against the same real v6
// comctl32.dll loaded from WinSxS (ruling out anything Go-specific); a
// 160-byte packed layout ([StructLayout(Pack=1)] on the .NET side)
// succeeded and rendered a real dialog.
const taskDialogConfigSize = 160

// marshal packs cfg into TASKDIALOGCONFIG's real (byte-packed) layout.
// Go has no struct-pack pragma, so this writes each field at its exact
// packed byte offset by hand rather than relying on Go's own struct
// layout, which always naturally-aligns 8-byte pointer fields and would
// silently reintroduce the padding that isn't actually there in the C
// struct.
func (c *taskDialogConfig) marshal() []byte {
	buf := make([]byte, taskDialogConfigSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], taskDialogConfigSize)
	le.PutUint64(buf[4:], uint64(c.hwndParent))
	le.PutUint64(buf[12:], uint64(c.hInstance))
	le.PutUint32(buf[20:], c.dwFlags)
	le.PutUint32(buf[24:], c.dwCommonButtons)
	le.PutUint64(buf[28:], uint64(uintptr(unsafe.Pointer(c.pszWindowTitle))))
	le.PutUint64(buf[36:], uint64(c.mainIcon))
	le.PutUint64(buf[44:], uint64(uintptr(unsafe.Pointer(c.pszMainInstruction))))
	le.PutUint64(buf[52:], uint64(uintptr(unsafe.Pointer(c.pszContent))))
	le.PutUint32(buf[60:], c.cButtons)
	le.PutUint64(buf[64:], uint64(c.pButtons))
	le.PutUint32(buf[72:], uint32(c.nDefaultButton))
	le.PutUint32(buf[76:], c.cRadioButtons)
	le.PutUint64(buf[80:], uint64(c.pRadioButtons))
	le.PutUint32(buf[88:], uint32(c.nDefaultRadioButton))
	le.PutUint64(buf[92:], uint64(uintptr(unsafe.Pointer(c.pszVerificationText))))
	le.PutUint64(buf[100:], uint64(uintptr(unsafe.Pointer(c.pszExpandedInformation))))
	le.PutUint64(buf[108:], uint64(uintptr(unsafe.Pointer(c.pszExpandedControlText))))
	le.PutUint64(buf[116:], uint64(uintptr(unsafe.Pointer(c.pszCollapsedControlText))))
	le.PutUint64(buf[124:], uint64(c.footerIcon))
	le.PutUint64(buf[132:], uint64(uintptr(unsafe.Pointer(c.pszFooter))))
	le.PutUint64(buf[140:], uint64(c.pfCallback))
	le.PutUint64(buf[148:], uint64(c.lpCallbackData))
	le.PutUint32(buf[156:], c.cxWidth)
	return buf
}

// taskDialogButton mirrors TASKDIALOG_BUTTON, also byte-packed (12 bytes:
// a 4-byte int ID directly followed by an 8-byte string pointer, no
// padding) — same reasoning as taskDialogConfig above.
type taskDialogButton struct {
	nButtonID     int32
	pszButtonText *uint16
}

const taskDialogButtonSize = 12

// packTaskDialogButtons packs buttons into a contiguous TASKDIALOG_BUTTON
// array in TaskDialogIndirect's expected byte-packed layout. The caller
// must keep the returned slice reachable until after the TaskDialogIndirect
// call that receives its address returns.
func packTaskDialogButtons(buttons []taskDialogButton) []byte {
	buf := make([]byte, len(buttons)*taskDialogButtonSize)
	le := binary.LittleEndian
	for i, b := range buttons {
		off := i * taskDialogButtonSize
		le.PutUint32(buf[off:], uint32(b.nButtonID))
		le.PutUint64(buf[off+4:], uint64(uintptr(unsafe.Pointer(b.pszButtonText))))
	}
	return buf
}

// dialogHandle is populated from the TDN_CREATED callback so other
// goroutines can SendMessage to a running dialog to update its progress
// bar/text live. Guarded by a mutex since it's written from the dialog's
// own locked OS thread and read from the driving goroutine.
type dialogHandle struct {
	mu   sync.Mutex
	hwnd uintptr
	// ready is closed exactly once, when hwnd first becomes valid.
	ready chan struct{}
}

func newDialogHandle() *dialogHandle {
	return &dialogHandle{ready: make(chan struct{})}
}

func (d *dialogHandle) set(hwnd uintptr) {
	d.mu.Lock()
	d.hwnd = hwnd
	d.mu.Unlock()
	close(d.ready)
}

func (d *dialogHandle) get() uintptr {
	<-d.ready
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hwnd
}

var callbackHandles sync.Map // uintptr(lpCallbackData token) -> *dialogHandle

var nextCallbackToken uintptr
var callbackTokenMu sync.Mutex

func registerCallback(h *dialogHandle) uintptr {
	callbackTokenMu.Lock()
	nextCallbackToken++
	token := nextCallbackToken
	callbackTokenMu.Unlock()
	callbackHandles.Store(token, h)
	return token
}

func unregisterCallback(token uintptr) {
	callbackHandles.Delete(token)
}

// taskDialogCallback is the single process-wide TaskDialog callback proc.
// lpCallbackData carries a token identifying which in-flight dialog this
// notification belongs to (registerCallback/callbackHandles), since a
// process could in principle drive more than one dialog — not something
// this package does today, but the token indirection costs nothing and
// avoids a hidden single-dialog-at-a-time assumption baking in silently.
func taskDialogCallback(hwnd uintptr, msg uint32, _ uintptr, _ uintptr, lpRefData uintptr) uintptr {
	if msg == tdnCreated {
		if v, ok := callbackHandles.Load(lpRefData); ok {
			v.(*dialogHandle).set(hwnd)
		}
	}
	return 0
}

var taskDialogCallbackPtr = windows.NewCallback(taskDialogCallback)

func sendMessage(hwnd uintptr, msg uint32, wparam, lparam uintptr) {
	// user32.SendMessageW — used instead of PostMessage because several
	// TDM_* updates (progress bar range/state) must apply before a
	// following TDM_SET_PROGRESS_BAR_POS is meaningful; SendMessage blocks
	// until the dialog's message loop has processed it, PostMessage
	// would not guarantee ordering against our own next call.
	procSendMessageW.Call(hwnd, uintptr(msg), wparam, lparam)
}

var (
	moduser32        = windows.NewLazySystemDLL("user32.dll")
	procSendMessageW = moduser32.NewProc("SendMessageW")
)

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

// runTaskDialog is the shared low-level driver: marshals cfg, calls
// TaskDialogIndirect on a locked OS thread (required — TaskDialog's
// window and message loop are affinitized to the calling thread, and
// Go's goroutine scheduler will happily move an unlocked goroutine to a
// different OS thread mid-call), and returns the clicked button ID.
// onCreated, if non-nil, is called once with the dialog's HWND as soon as
// TDN_CREATED fires, from a background goroutine — giving the caller a
// window to drive the dialog live (progress updates) while
// TaskDialogIndirect itself blocks pumping its own message loop.
func runTaskDialog(cfg *taskDialogConfig, onCreated func(hwnd uintptr)) (buttonID int32, err error) {
	handle := newDialogHandle()
	token := registerCallback(handle)
	defer unregisterCallback(token)
	cfg.pfCallback = taskDialogCallbackPtr
	cfg.lpCallbackData = token

	if onCreated != nil {
		go func() {
			hwnd := handle.get()
			onCreated(hwnd)
		}()
	}

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		buf := cfg.marshal()
		var pnButton int32
		ret, _, callErr := procTaskDialogIndirect.Call(
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&pnButton)),
			0,
			0,
		)
		if ret != 0 {
			done <- fmt.Errorf("taskdialogindirect: %w", callErr)
			return
		}
		buttonID = pnButton
		done <- nil
	}()
	err = <-done
	return buttonID, err
}
