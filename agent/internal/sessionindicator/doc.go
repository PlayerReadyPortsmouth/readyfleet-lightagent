// Package sessionindicator puts an unmissable on-screen bar in front of
// the person sitting at a machine whenever a remote SolarBeam viewer is
// watching their screen — and keeps that bar OUT of the video the viewer
// receives.
//
// # Why this exists
//
// Before this package there was no local indication of a live session at
// all. The Windows light agent's tray icon has a hover tooltip reading
// "SolarBeam: running", but that (a) only reports that the engine process
// is alive, not that anyone is connected, (b) is invisible unless the
// person hovers it, and (c) does not exist off Windows at all
// (internal/trayicon/tray_other.go is a no-op stub). Worse, KWin's
// screencast capture runs happily with the monitors asleep and the
// session locked, and does not wake them: a session can run start to
// finish with nothing on the machine saying so.
//
// # The two halves of the contract
//
// An indicator that the remote viewer can ALSO see is worse than no
// indicator, because it tells the remote party whether the local person
// has been alerted. So "show it" and "hide it from the capture" are one
// requirement, not two, and every platform here must satisfy both or
// report failure.
//
// Windows and Linux solve the exclusion half completely differently.
//
// ## Windows — OS-level exclusion, no engine cooperation
//
// SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE) removes a window
// from DXGI Desktop Duplication, Windows Graphics Capture and PrintScreen
// while leaving it fully visible locally. The agent owns the window it
// creates, which is the documented precondition — a spawned third-party
// window cannot be excluded this way, as internal/hiddenbrowser found the
// hard way (ERROR_ACCESS_DENIED against a spawned Chrome). See
// internal/hiddenbrowser/hiddenbrowser_windows.go, whose idiom this
// package reuses verbatim, and
// docs/superpowers/specs/2026-08-10-byod-hidden-browser-design.md.
//
// WDA_EXCLUDEFROMCAPTURE needs Windows 10 2004 (build 19041) or newer. On
// anything older SetWindowDisplayAffinity fails, and this package treats
// that as a failure to SHOW: the window is destroyed rather than left up
// where the viewer would see it.
//
// ## Linux/Wayland — geometric exclusion, engine cooperation required
//
// KWin's screencast protocol (zkde_screencast_unstable_v1, version 6)
// has no per-window exclusion of any kind. Its complete request list is
// stream_output, stream_window, stream_virtual_output,
// stream_virtual_output_with_description and stream_region. There is no
// skip flag on a window anywhere in KWin. So exclusion on Linux can only
// be geometric, and it has to be agreed with the engine:
//
//   - The engine gains a config key `capture_reserve_top` (logical
//     pixels, default 0) in sunshine.conf. When it is greater than zero
//     the engine captures with stream_region over the streamed output's
//     rectangle MINUS a band of that many logical pixels along the TOP
//     edge, instead of stream_output over the whole output.
//   - The agent draws the indicator inside exactly that band.
//
// Those pixels are then never composited into the stream at all. This is
// a protocol-level exclusion — the region the compositor renders simply
// does not contain them — not a mask applied after the fact, so there is
// no frame in which the bar could leak.
//
// ReserveHeight is the single number both sides agree on. Changing it
// here without changing the engine's default breaks the contract in the
// dangerous direction (bar taller than the reserved band ⇒ the bar is
// partly IN the stream), so it is a constant, not a config field.
//
// The bar itself is a zwlr_layer_shell_v1 surface on the `overlay` layer,
// anchored TOP|LEFT|RIGHT, ReserveHeight tall, with
// set_exclusive_zone(ReserveHeight) so KWin shrinks the work area and no
// desktop content ends up hidden underneath it. It takes no keyboard
// focus and carries an empty wl_surface input region, so it cannot be
// clicked, dragged, focused or dismissed — there is deliberately no close
// button, because the remote party must not be able to talk the local
// person into hiding it.
//
// go-wayland does not ship zwlr_layer_shell_v1 bindings and its scanner
// lives in a module that cannot be fetched offline, so the two proxies
// are hand-transcribed in layershell_linux.go.
//
// # Which outputs get a bar
//
// All of them. Determining which output the engine is streaming would
// mean reading the engine's own output selection, which is not exposed
// anywhere the agent can see; a bar on every screen is strictly safer
// than a bar on the wrong one, and the engine reserves the same band on
// whichever output it streams.
//
// # Nobody is there — the skip rule
//
// Before any of the degradation policy below applies, there is a prior
// question: is there anyone in front of the screen at all? If the session
// is locked, or the displays are powered down, drawing the bar tells
// nobody anything. Screenstate.go reads both conditions (Linux:
// org.freedesktop.ScreenSaver with a systemd-logind LockedHint fallback,
// plus the DRM connectors' dpms attribute; Windows: WTSINFOEX
// SessionFlags), Decide turns a reading into show-or-skip, and Show
// returns a *Skipped rather than putting a bar up.
//
// Aurora's ruling, 7 Sep 2026:
//
//	If a machine is locked or displays are off, just skip the
//	indicator — for BYOD it requires approval anyway.
//
// The "anyway" is internal/consent: a BYOD connection from anyone but the
// device's enrolled owner already puts a consent prompt in front of the
// person at the machine, so they have been told by a route that does not
// depend on a bar being drawable, and the owner connecting to their own
// device is unattended by design.
//
// A SKIP IS NOT A FAILURE. That distinction is the whole reason *Skipped
// is a distinct type: require_session_indicator must fire on "the bar
// could not be shown" and must NOT fire on "there was no point showing
// the bar", or every locked machine becomes a machine that refuses
// support. Unknown readings mean SHOW — a probe that could not reach the
// compositor must never be able to silence the indicator.
//
// Skipping changes no state, so it is re-decided on every poll: a bar
// that is already up when the machine locks is left up, and a machine
// that is unlocked mid-session gets its bar within one poll interval.
//
// # Degradation — THE OPEN DECISION
//
// If the bar cannot be shown (no Wayland, an X11 session, no layer-shell
// global, SetWindowDisplayAffinity failing on an old Windows build), New
// or Show returns an error and the caller logs it loudly. What it must
// NOT do is decide on its own whether that kills the session.
//
// The trade-off, stated so it can be decided rather than defaulted into:
//
//   - Blocking (require_session_indicator = true) is the honest privacy
//     posture: nobody is ever watched without the machine saying so. Its
//     cost is that a compositor quirk, an X11 login, or a Windows build
//     older than 10 2004 turns "no indicator" into "no support session",
//     on exactly the machine that already has something wrong with it.
//   - Logging only (require_session_indicator = false, the shipped
//     default) keeps support working and leaves an auditable warn line,
//     but a session can then run with no local indication — which is the
//     status quo this package exists to end.
//
// The policy is one switch: config.Config.RequireSessionIndicator, absent
// key ⇒ false ⇒ log only. Nothing in this package reads it; the switch is
// applied where the session is started, so that this package stays a
// mechanism and the policy stays in one obvious place.
//
// # Testing
//
// The compositor and Win32 work sits behind the startBarFunc seam, and
// the lock/DPMS probes behind a second one the supervisor takes as a
// constructor argument. The supervisor (indicator.go), the decision
// (screenstate.go) and the renderer (render.go) are platform-neutral and
// tested without a compositor, per this repo's injectable-seam
// convention. The probe seam is required rather than defaulted on
// purpose: a test that forgot to supply it would otherwise read the
// developer's own desktop and pass or fail depending on whether that
// desktop happened to be locked.
package sessionindicator
