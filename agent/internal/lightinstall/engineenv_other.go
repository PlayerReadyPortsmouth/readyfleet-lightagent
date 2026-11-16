//go:build !linux

package lightinstall

// EnsureEngineEnvironment is a no-op off Linux.
//
// Both things it does are answers to KDE Wayland problems that have no Windows
// equivalent. There is no desktop-entry permission handshake: the Windows
// engine captures via DXGI Desktop Duplication, which needs no per-binary grant
// from the compositor, so there is nothing to declare and no three-second sleep
// to avoid. And the Windows engine picks its own capture backend at runtime
// (ddx/wgc), correctly and without help, so there is no configuration file to
// pin — writing one would only be a second, staler source of truth.
//
// The installer and the agent both call this unconditionally, so the stub keeps
// those call sites platform-neutral.
func EnsureEngineEnvironment() error { return nil }

// EnsureCaptureReserveTop is a no-op off Linux, and must stay one.
//
// The reserved band exists solely because KWin's screencast protocol
// cannot exclude a window from a capture. Windows has no such problem: the
// session indicator's window carries WDA_EXCLUDEFROMCAPTURE, so the OS
// removes it from DXGI/WGC/PrintScreen with no cooperation from the engine
// at all. Writing capture_reserve_top on Windows would crop 40 logical
// pixels off the top of every stream for nothing.
func EnsureCaptureReserveTop(int) error { return nil }
