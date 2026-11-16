// Command light-installer installs the BYOD lightagent as a current-user,
// no-admin background task. Two ways it can be handed its lightagent.exe
// bytes and enrollment token:
//
//   - Self-contained (ReadyApp's mentor download): a per-mentor copy of
//     this exe with a trailer appended after the build's own unsigned
//     bytes — magic marker + length-prefixed lightagent.exe + length-
//     prefixed enrollment token JSON — then signed as a whole by
//     codesign-service. See selfContainedPayload's doc comment for why
//     the trailer has to be appended BEFORE signing, not after: appending
//     after leaves WinVerifyTrust reporting no signature at all, confirmed
//     empirically against a real signed binary this session. One file,
//     one download, a real Authenticode signature.
//   - Sibling files (legacy multi-file bundle, and local dev/testing):
//     lightagent.exe and enrollment.json staged beside this exe's own
//     directory, matching installer/LightProduct.wxs's per-user WiX
//     package layout. Used whenever no trailer marker is found.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/installerui"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/selfpayload"
)

// selfPayloadMagic marks the start of a self-contained trailer — see the
// package doc comment. The compiled light-installer.exe itself contains
// this exact string as a Go constant literal (in its own .rodata), so
// selfContainedPayload deliberately searches for the LAST occurrence, not
// the first — confirmed live: an early version of this used bytes.Index
// and found the binary's own embedded copy of the constant before ever
// reaching the real appended trailer, failing with a bounds error on the
// garbage that happened to follow it in the compiled code. The real
// trailer is always appended after everything else, so it's always the
// rightmost match.
const selfPayloadMagic = "RFLTPAY1"

// selfContainedPayload looks for a trailer ReadyApp's per-mentor download
// endpoint appends to an unsigned light-installer.exe build before handing
// the whole thing to codesign-service: MAGIC, then a uint32 LE length +
// that many bytes for lightagent.exe, then a uint32 LE length + that many
// bytes for the enrollment token JSON (lightinstall.Payload's on-disk
// shape, unparsed here — ParsePayload does the real validation). ok is
// false with a nil error when this is an ordinary (non-self-contained)
// build; callers should fall back to sibling-file loading in that case.
func selfContainedPayload(exePath string) (agentBytes, tokenJSON []byte, ok bool, err error) {
	data, err := os.ReadFile(exePath)
	if err != nil {
		return nil, nil, false, fmt.Errorf("read self: %w", err)
	}

	// A compiled installer ALWAYS contains selfPayloadMagic in its own .rodata,
	// as the Go constant literal above. So the presence of the magic proves
	// nothing; only a parse that consumes exactly to EOF does.
	//
	// The trailer is appended after everything else at build time, so a real one
	// ends at the last byte of the file. That is what distinguishes it from the
	// incidental literal, and it is what an earlier bytes.LastIndex-only
	// implementation had no way to check: on an ORDINARY build (no trailer at
	// all — the shape the Linux bundle ships, with the agent and enrollment.json
	// as sibling files) it found the binary's own copy of the constant, read the
	// unrelated .rodata bytes after it as a length prefix, and failed. Every
	// non-self-contained install died with "installer file is corrupt" before it
	// reached the sibling-file path it was supposed to fall back to.
	//
	// Scanning backwards rather than taking only the last match keeps a genuine
	// trailer findable even if the payload bytes themselves happened to contain
	// the magic.
	// On Windows the installer is Authenticode-signed AFTER the trailer is
	// appended (signing first and appending after leaves no valid signature),
	// so the signature block — not the trailer — is at EOF. "Last thing in the
	// file" therefore means last thing before the certificate table, allowing
	// the signer's alignment padding. Measuring against len(data) instead made
	// every signed Windows BYOD installer miss its own trailer and fail with
	// "ReadyFleet enrollment file is invalid" (reproduced on a Windows 11 VM,
	// 24 Sep 2026); unsigned Linux trailers are unaffected (end == len(data)).
	end := selfpayload.ContentEnd(data)
	for idx := bytes.LastIndex(data[:end], []byte(selfPayloadMagic)); idx >= 0; idx = bytes.LastIndex(data[:idx], []byte(selfPayloadMagic)) {
		pos := idx + len(selfPayloadMagic)
		agent, pos, agentErr := selfpayload.ReadLenPrefixed(data[:end], pos)
		if agentErr != nil {
			continue
		}
		token, pos, tokenErr := selfpayload.ReadLenPrefixed(data[:end], pos)
		if tokenErr != nil {
			continue
		}
		// The defining property: a trailer is the LAST thing in the file.
		// Anything that parses but leaves bytes over is not one.
		if pos != end && !selfpayload.PaddingOnly(data, pos, end) {
			continue
		}
		return agent, token, true, nil
	}

	// No trailer. Not an error — this is an ordinary build, and the caller
	// falls back to loading the agent and enrollment.json from beside the
	// installer. A truncated or corrupt trailer lands here too, and that is
	// deliberately safe rather than fatal: it installs nothing, and the
	// sibling-file path then either succeeds on its own terms or reports its
	// own, accurate failure. What must never happen is staging a half-read
	// agent binary, and returning early here is what guarantees it cannot.
	return nil, nil, false, nil
}

// writeAgentBinary stages the self-contained trailer's embedded
// lightagent.exe at path, creating its directory if needed. 0o755 (not a
// stricter mode) matches how a normal per-user download/extract would
// leave it — nothing here holds elevated or shared-machine state that a
// tighter ACL would protect (see lightinstall's own package doc comment).
// writeAgentBinary installs data at path by writing a sibling temporary file
// and renaming it over the destination.
//
// The rename is not tidiness, it is the only way this works twice. Writing
// straight to the path fails with ETXTBSY ("text file busy") on Linux whenever
// the destination is a binary some process is currently executing — which, on a
// reinstall or a repair, is precisely what the destination is: the agent's own
// systemd user service is running it. The first install has no file there and
// succeeds, so the failure only appears the second time a machine is set up,
// reported as "ReadyFleet could not stage the agent binary" with nothing to
// suggest that stopping the service would fix it.
//
// A rename replaces the directory entry and leaves the old inode alive for the
// process still executing it, which then exits normally at the restart the
// installer performs anyway. It is also atomic: an interrupted install leaves
// either the old agent or the new one, never a half-written binary that systemd
// would then try to start.
func writeAgentBinary(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}
	// Same directory as the destination, so the rename cannot cross a
	// filesystem boundary and fall back to a copy.
	tmp, err := os.CreateTemp(dir, ".agent-install-*")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename has succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write agent binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set agent binary mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write agent binary: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace agent binary: %w", err)
	}
	return nil
}

// installTargetPaths is the self-contained build's fixed install
// location — %LocalAppData%\ReadyFleet Light, matching lightagent.exe's
// own lightAppDataDir() default (cmd/lightagent/main.go) exactly, since a
// self-contained download can land anywhere (Downloads, Desktop, a temp
// browser folder) and must never install itself beside wherever it
// happened to be saved.
func installTargetPaths() lightinstall.Paths {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		return lightinstall.DarwinPaths(home)
	}
	base := os.Getenv("LocalAppData")
	agentName := installerAgentName()
	if base == "" && runtime.GOOS == "linux" {
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, ".config")
			}
		}
	}
	if base == "" {
		base = "lightagent-data"
	}
	dirName := "ReadyFleet Light"
	if runtime.GOOS == "linux" && os.Getenv("LocalAppData") == "" {
		dirName = "readyfleet-light"
	}
	dir := filepath.Join(base, dirName)
	return lightinstall.Paths{
		Root:       dir,
		AgentExe:   filepath.Join(dir, agentName),
		ConfigPath: filepath.Join(dir, "config.json"),
		KeyPath:    filepath.Join(dir, "client.key"),
		LogPath:    filepath.Join(dir, "logs", "lightagent.log"),
	}
}

// logInstallFailure writes the real, unwrapped error chain from a failed
// Install() to a dedicated diagnostic log beside the install target — the
// wizard's own dialog deliberately shows only a generic message (raw Go
// errors aren't mentor-facing), so this is the one place the actual cause
// ("lightinstall: enroll: agent/enroll: post: dial tcp: ...", etc.) survives
// anywhere. Best-effort: a failure to write this log is not itself
// reported anywhere, since the wizard has already committed to showing the
// generic failure message by the time this runs.
func logInstallFailure(installErr error) {
	logPath := filepath.Join(installTargetPaths().Root, "install-error.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%v\n", time.Now().Format(time.RFC3339), installErr)
}

const (
	exitSuccess        = 0
	exitInstallFailed  = 10
	exitCleanupFailed  = 11
	exitCancelled      = 12
	exitUsage          = 64
	exitInvalidPayload = 65
	exitNotConfigured  = 66
)

// installStepCount matches the number of m.progress(...) calls in
// lightinstall.Manager.Install, plus the one AwaitingApproval step that
// only occurs on the BYOD consent path — used only to size the wizard's
// progress bar fraction; RunWithProgress still only ever advances it on a
// real step actually happening. A non-BYOD install therefore tops out at
// 6/7 before the bar is snapped to 100% on completion, which is honest:
// it did one fewer thing.
const installStepCount = 7

const contactFooter = "Questions or concerns? Contact readyapp@player-ready.co.uk."

// serverURL is the C2's wss:// URL, baked in at build time via
// -ldflags "-X main.serverURL=...". Never taken from the payload — a
// compromised enrollment.json must not be able to redirect enrollment to
// an attacker-controlled host. An unset value refuses to run rather than
// falling back to something guessed.
var serverURL = ""

type commandDeps struct {
	load    func(string) (lightinstall.Payload, error)
	install func(context.Context, lightinstall.Payload) error
	remove  func(string) error
}

func main() {
	// Built before the first failure check, not just before ShowWelcome: these
	// early paths used to report on stderr only, so a GUI-subsystem build made
	// them exit having shown the mentor nothing at all. Every user-visible
	// failure now goes through the wizard.
	w := installerui.Wizard{Title: "ReadyFleet Light Agent Setup"}
	fail := func(detail string, code int) {
		fmt.Fprintln(os.Stderr, detail)
		w.ShowResult(false, "Setup couldn't finish", detail+"\n\n"+contactFooter)
		os.Exit(code)
	}

	exePath, err := os.Executable()
	if err != nil {
		fail("ReadyFleet light installer could not locate itself", exitInvalidPayload)
	}

	agentBytes, tokenJSON, selfContained, err := selfContainedPayload(exePath)
	if err != nil {
		fail("ReadyFleet installer file is corrupt", exitInvalidPayload)
	}
	installPaths := pathsBeside(exePath)
	if selfContained {
		installPaths = installTargetPaths()
	} else if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		// A downloaded Linux or macOS bundle is normally extracted in Downloads.
		// Copy the sibling, pinned agent into its stable location (XDG, or
		// ~/Library/Application Support) before systemd/launchd
		// records ExecStart; otherwise deleting the download would silently
		// break the next login.
		sourceAgent := installPaths.AgentExe
		installPaths = installTargetPaths()
		agentData, readErr := os.ReadFile(sourceAgent)
		if readErr != nil {
			fail("ReadyFleet could not read the bundled agent", exitInstallFailed)
		}
		if err := writeAgentBinary(installPaths.AgentExe, agentData); err != nil {
			fail("ReadyFleet could not stage the agent binary", exitInstallFailed)
		}
	}

	// progress is set just before RunWithProgress drives run(); deps.install
	// reads it fresh on every call, so the same Manager.Progress hook that
	// exit-code tests exercise with a nil progress also drives the wizard's
	// live step display here — no separate code path to fall out of sync.
	var progress func(string)
	deps := commandDeps{
		load: lightinstall.LoadPayload,
		install: func(ctx context.Context, payload lightinstall.Payload) error {
			mgr := lightinstall.New(lightinstall.Config{
				Paths:     installPaths,
				ServerURL: serverURL,
			})
			mgr.Progress = progress
			// Setting this at all is what opts the install into waiting on
			// the BYOD consent gate — see lightinstall.Manager.AwaitingApproval.
			// Safe to set unconditionally here because this is the wizard
			// path: there is always a screen to show the mentor.
			mgr.AwaitingApproval = func(label string) {
				if progress != nil {
					progress(label)
				}
			}
			return mgr.Install(ctx, payload)
		},
		remove: lightinstall.RemovePayload,
	}
	if selfContained {
		if err := writeAgentBinary(installPaths.AgentExe, agentBytes); err != nil {
			fail("ReadyFleet could not stage the agent binary", exitInstallFailed)
		}
		// The embedded token is already in memory — no sibling
		// enrollment.json exists to load from or clean up afterward.
		deps.load = func(string) (lightinstall.Payload, error) { return lightinstall.ParsePayload(tokenJSON) }
		deps.remove = func(string) error { return nil }
	}

	text := consentCopy(runtime.GOOS)
	proceed := w.ShowWelcome("Set up the ReadyFleet Light Agent", text.intro, text.bullets, contactFooter)
	if !proceed {
		w.ShowResult(false, "Setup cancelled", "You clicked Cancel — nothing was installed, and your enrollment link is still valid if you'd like to try again.")
		os.Exit(exitCancelled)
	}

	var exitCode int
	var stderrBuf strings.Builder
	_ = w.RunWithProgress(installStepCount, func(report func(string)) error {
		progress = report
		exitCode = run(context.Background(), os.Args[1:], &stderrBuf, exePath, deps)
		if exitCode != exitSuccess {
			return fmt.Errorf("exit %d", exitCode)
		}
		return nil
	})

	if exitCode == exitSuccess {
		w.ShowResult(true, "All set", text.done)
	} else {
		detail := strings.TrimSpace(stderrBuf.String())
		if detail == "" {
			detail = "Setup did not complete."
		}
		w.ShowResult(false, "Setup couldn't finish", detail+"\n\n"+contactFooter)
	}
	os.Exit(exitCode)
}

func run(ctx context.Context, args []string, stderr io.Writer, exePath string, deps commandDeps) int {
	if serverURL == "" {
		fmt.Fprintln(stderr, "ReadyFleet light installer is not configured")
		return exitNotConfigured
	}
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: ReadyFleetLightInstaller.exe [enrollment.json]")
		return exitUsage
	}
	payloadPath := ""
	if len(args) == 1 {
		payloadPath = args[0]
	} else {
		payloadPath = filepath.Join(filepath.Dir(exePath), "enrollment.json")
	}

	cleanup := func(exitCode int) int {
		if deps.remove != nil {
			if err := deps.remove(payloadPath); err != nil {
				fmt.Fprintln(stderr, "ReadyFleet could not overwrite and remove enrollment.json")
				return exitCleanupFailed
			}
		}
		return exitCode
	}
	if deps.load == nil || deps.install == nil {
		fmt.Fprintln(stderr, "ReadyFleet light installer is not configured")
		return cleanup(exitInstallFailed)
	}
	payload, err := deps.load(payloadPath)
	if err != nil {
		fmt.Fprintln(stderr, "ReadyFleet enrollment file is invalid")
		return cleanup(exitInvalidPayload)
	}
	if err := deps.install(ctx, payload); err != nil {
		logInstallFailure(err)
		fmt.Fprintln(stderr, installFailureMessage(err))
		return cleanup(exitInstallFailed)
	}
	if code := cleanup(exitSuccess); code != exitSuccess {
		return code
	}
	fmt.Fprintln(stderr, "ReadyFleet light installation completed")
	return exitSuccess
}

// installFailureMessage picks the sentence the mentor actually sees on the
// failure screen (main() feeds stderr's contents straight into
// Wizard.ShowResult). The three BYOD consent-gate verdicts each get their
// own wording because none of them is a fault in the usual sense:
//
//   - declined means somebody said no;
//   - expired means nobody said anything in time;
//   - unreachable means neither of those happened — the wait's own
//     client-side backstop gave up because ReadyFleet could never be
//     reached for long enough to learn a verdict at all (almost always the
//     mentor's own network, not a ReadyApp decision).
//
// All three are recoverable by a human action the generic "see the light
// agent log" would never lead them to — and there is nothing in that log to
// see, since the agent was never installed.
//
// Never includes err itself: err can carry the enrollment URL and other
// detail this screen has no reason to show, and the package's existing
// tests assert no secret ever reaches stderr.
func installFailureMessage(err error) string {
	switch {
	case errors.Is(err, lightinstall.ErrEnrollmentDeclined):
		return "This device was declined in ReadyApp, so nothing was installed. " +
			"If that wasn't what you meant, confirm it in ReadyApp and run this installer again with a fresh setup link."
	case errors.Is(err, lightinstall.ErrEnrollmentExpired):
		return "The confirmation request expired before this device was confirmed in ReadyApp, so nothing was installed. " +
			"Request a new setup link in ReadyApp and run this installer again."
	case errors.Is(err, lightinstall.ErrEnrollmentUnreachable):
		return "Couldn't reach ReadyFleet while waiting for your confirmation. " +
			"Check your connection and try running the installer again."
	default:
		return "ReadyFleet installation failed; see the light agent log"
	}
}

// pathsBeside lays out a Manager's Paths relative to the installer's own
// directory — lightagent.exe, config.json, and client.key all live
// alongside light-installer.exe, matching where installer/LightProduct.wxs
// stages the per-user package.
func pathsBeside(exePath string) lightinstall.Paths {
	dir := filepath.Dir(exePath)
	return lightinstall.Paths{
		Root:       dir,
		AgentExe:   filepath.Join(dir, installerAgentName()),
		ConfigPath: filepath.Join(dir, "config.json"),
		KeyPath:    filepath.Join(dir, "client.key"),
		LogPath:    filepath.Join(dir, "logs", "lightagent.log"),
	}
}

func installerAgentName() string {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return "readyfleet-light"
	}
	return "lightagent.exe"
}
