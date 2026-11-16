package gamepaddriver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Status is what the PC has now, read from the driver file itself — the same
// source the engine's own /api/vigembus/status reads, so the two can never
// disagree.
type Status struct {
	// Installed reports that ViGEmBus.sys exists.
	Installed bool
	// Version is the driver file version ("1.21.442.0"), or "" when the file
	// is absent or its version could not be read.
	Version string
	// Compatible reports Version >= the spec's minimum. An installed but
	// unreadable version counts as compatible: the file is there, and
	// refusing a working driver over a resource-section read is worse than
	// letting the engine decide.
	Compatible bool
	// InstalledByUs reports that this flow installed it (a receipt exists).
	// Only then does the tray offer to remove it: a driver that DS4Windows
	// or a Steam tool put there is not ours to take away.
	InstalledByUs bool
}

// Ready reports whether a session can create a virtual pad.
func (s Status) Ready() bool { return s.Installed && s.Compatible }

// Outcome is how an Install or Uninstall ended when it did not fail.
type Outcome int

const (
	// OutcomeInstalled: the installer succeeded and the driver is present.
	OutcomeInstalled Outcome = iota + 1
	// OutcomeAlreadyInstalled: nothing to do; no download, no prompt.
	OutcomeAlreadyInstalled
	// OutcomeRebootRequired: the vendor installer returned 3010/1641.
	OutcomeRebootRequired
	// OutcomeDeclined: the owner said No to the Windows administrator
	// prompt (or cancelled the installer). Nothing changed.
	OutcomeDeclined
	// OutcomeRemoved: the driver was uninstalled.
	OutcomeRemoved
	// OutcomeNotInstalled: Uninstall found nothing to remove.
	OutcomeNotInstalled
)

var (
	// ErrSessionActive refuses a change while a SolarBeam session runs:
	// installing is harmless mid-session but removing would pull pads out
	// from under whoever is playing, and the rule is simpler as one rule.
	ErrSessionActive = errors.New("a SolarBeam session is running on this PC; try again when it has ended")
	// ErrBusy refuses a second concurrent install/uninstall.
	ErrBusy = errors.New("gamepad support is already being changed")
	// ErrElevationDeclined is what RunElevated returns when the owner
	// answered No to the UAC prompt (ERROR_CANCELLED).
	ErrElevationDeclined = errors.New("administrator permission was not given")
	// ErrUnsupported is returned off Windows.
	ErrUnsupported = errors.New("gamepad driver install is only supported on Windows")
)

// Deps are the side effects, injectable so the orchestration below is tested
// without a network, a driver or a UAC prompt (agent/CLAUDE.md: never spawn
// real processes or touch the registry in tests).
type Deps struct {
	// Detect reads the current driver state (without InstalledByUs, which
	// the Manager fills from its receipt).
	Detect func() (Status, error)
	// Download fetches url to dir, returning a path whose content already
	// matched sha256 (verify.DownloadVerified).
	Download func(ctx context.Context, url, sha256Hex, dir string, maxBytes int64) (string, error)
	// Hold opens path so that nothing can write, rename or delete it until
	// the returned file is closed, and returns it for re-hashing.
	Hold func(path string) (io.ReadCloser, error)
	// VerifySigner checks the Authenticode signature is Valid and its signer
	// certificate hashes to fingerprint.
	VerifySigner func(path, fingerprint string) error
	// RunElevated asks Windows to run exe elevated ("runas"), waits for it to
	// exit and returns its exit code. ErrElevationDeclined when the owner
	// says No.
	RunElevated func(ctx context.Context, exe, args string) (uint32, error)
	// Msiexec is the absolute path of msiexec.exe.
	Msiexec func() (string, error)
	// InstalledProductCode returns the MSI product code the Uninstall key
	// records for this driver, or "" when none is found.
	InstalledProductCode func() (string, error)
	// SessionActive reports a live SolarBeam session.
	SessionActive func() bool
}

// Manager runs at most one install/uninstall at a time.
type Manager struct {
	Spec Spec
	Deps Deps
	// StateDir holds the staged download (deleted afterwards) and the
	// receipt that records this flow installed the driver. Per user, e.g.
	// %LocalAppData%\ReadyFleet Light\gamepad-driver.
	StateDir string

	mu     sync.Mutex
	active bool
}

const receiptName = "installed-by-readyfleet.json"

type receipt struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	SHA256      string `json:"sha256"`
	ProductCode string `json:"product_code,omitempty"`
}

// Status reads the current state and whether this flow installed it.
func (m *Manager) Status() (Status, error) {
	st, err := m.Deps.Detect()
	if err != nil {
		return Status{}, err
	}
	st.Compatible = st.Installed && versionAtLeast(st.Version, m.Spec.MinMajor, m.Spec.MinMinor)
	if st.Installed {
		if _, err := os.Stat(filepath.Join(m.StateDir, receiptName)); err == nil {
			st.InstalledByUs = true
		}
	}
	return st, nil
}

func (m *Manager) begin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active {
		return ErrBusy
	}
	m.active = true
	return nil
}

func (m *Manager) end() {
	m.mu.Lock()
	m.active = false
	m.mu.Unlock()
}

// Install downloads, verifies and runs the pinned installer through one UAC
// prompt. report receives short human-readable step labels.
func (m *Manager) Install(ctx context.Context, report func(string)) (Outcome, error) {
	if report == nil {
		report = func(string) {}
	}
	if err := m.begin(); err != nil {
		return 0, err
	}
	defer m.end()
	if m.Deps.SessionActive != nil && m.Deps.SessionActive() {
		return 0, ErrSessionActive
	}

	before, err := m.Status()
	if err != nil {
		return 0, fmt.Errorf("check for an existing driver: %w", err)
	}
	if before.Ready() {
		return OutcomeAlreadyInstalled, nil
	}
	if before.Installed {
		// An old ViGEmBus (< 1.17) from some game launcher. The 1.22.0
		// in-place upgrade is known to be broken, so do not run over it.
		return 0, fmt.Errorf("an older %s (version %s) is already installed; remove it in Windows Settings > Apps first, then try again", m.Spec.Name, before.Version)
	}

	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return 0, fmt.Errorf("create staging folder: %w", err)
	}

	report(fmt.Sprintf("Downloading %s %s (%.1f MB)…", m.Spec.Name, m.Spec.Version, float64(m.Spec.SizeBytes)/1e6))
	path, err := m.download(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(path) }()

	report("Checking the file is the exact, signed release…")
	held, err := m.Deps.Hold(path)
	if err != nil {
		return 0, fmt.Errorf("lock the downloaded installer: %w", err)
	}
	// Closed only after the installer has exited: the lock must cover the
	// whole window between the checks and the elevated run.
	defer func() { _ = held.Close() }()
	if err := hashMatches(held, m.Spec.SHA256); err != nil {
		return 0, err
	}
	if err := m.Deps.VerifySigner(path, m.Spec.SignerSHA256); err != nil {
		return 0, fmt.Errorf("the installer's signature is not %s's: %w", m.Spec.Publisher, err)
	}

	report("Waiting for you to approve the Windows administrator prompt…")
	code, err := m.Deps.RunElevated(ctx, path, m.Spec.InstallArgs)
	if errors.Is(err, ErrElevationDeclined) {
		return OutcomeDeclined, nil
	}
	if err != nil {
		return 0, fmt.Errorf("start the installer: %w", err)
	}

	report("Checking the driver is installed…")
	outcome, err := m.interpretInstallExit(code)
	if err != nil {
		return 0, err
	}
	if outcome == OutcomeDeclined {
		return outcome, nil
	}
	after, err := m.Deps.Detect()
	if err != nil {
		return 0, fmt.Errorf("installer finished but the driver could not be checked: %w", err)
	}
	if !after.Installed && outcome != OutcomeRebootRequired {
		return 0, fmt.Errorf("the installer reported success but %s is not present", m.Spec.Name)
	}
	productCode, _ := m.Deps.InstalledProductCode()
	if err := m.writeReceipt(productCode); err != nil {
		// Not fatal: the driver works. Only the tray's Remove item is lost.
		report("Installed, but could not record it for later removal: " + err.Error())
	}
	return outcome, nil
}

func (m *Manager) download(ctx context.Context) (string, error) {
	maxBytes := m.Spec.SizeBytes + 1024*1024
	var errs []string
	for _, url := range m.Spec.URLs {
		path, err := m.Deps.Download(ctx, url, m.Spec.SHA256, m.StateDir, maxBytes)
		if err == nil {
			return path, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		errs = append(errs, fmt.Sprintf("%s: %v", hostOf(url), err))
	}
	return "", fmt.Errorf("could not download %s: %s", m.Spec.Name, strings.Join(errs, "; "))
}

// Windows Installer exit codes the vendor bootstrapper passes through.
const (
	exitSuccess            = 0
	exitUserCancelled      = 1602
	exitAnotherInstall     = 1618
	exitUnknownProduct     = 1605
	exitRebootInitiated    = 1641
	exitSuccessRebootNeeds = 3010
)

func (m *Manager) interpretInstallExit(code uint32) (Outcome, error) {
	switch code {
	case exitSuccess:
		return OutcomeInstalled, nil
	case exitSuccessRebootNeeds, exitRebootInitiated:
		return OutcomeRebootRequired, nil
	case exitUserCancelled:
		return OutcomeDeclined, nil
	case exitAnotherInstall:
		return 0, errors.New("another installation is already running on this PC (often Windows Update); wait for it to finish, then try again")
	default:
		return 0, fmt.Errorf("the %s installer failed with exit code %d", m.Spec.Name, code)
	}
}

// Uninstall removes a driver this flow installed, via msiexec /x with the
// product code, through one UAC prompt.
func (m *Manager) Uninstall(ctx context.Context, report func(string)) (Outcome, error) {
	if report == nil {
		report = func(string) {}
	}
	if err := m.begin(); err != nil {
		return 0, err
	}
	defer m.end()
	if m.Deps.SessionActive != nil && m.Deps.SessionActive() {
		return 0, ErrSessionActive
	}
	st, err := m.Status()
	if err != nil {
		return 0, fmt.Errorf("check the driver: %w", err)
	}
	if !st.Installed {
		_ = os.Remove(filepath.Join(m.StateDir, receiptName))
		return OutcomeNotInstalled, nil
	}

	code, _ := m.Deps.InstalledProductCode()
	if code == "" {
		code = m.receiptProductCode()
	}
	if code == "" {
		code = m.Spec.ProductCode
	}
	if !productCodeRE.MatchString(code) {
		return 0, fmt.Errorf("refusing unexpected product code %q", code)
	}
	msiexec, err := m.Deps.Msiexec()
	if err != nil {
		return 0, fmt.Errorf("locate Windows Installer: %w", err)
	}

	report("Waiting for you to approve the Windows administrator prompt…")
	exit, err := m.Deps.RunElevated(ctx, msiexec, "/x "+code+" /qn /norestart")
	if errors.Is(err, ErrElevationDeclined) {
		return OutcomeDeclined, nil
	}
	if err != nil {
		return 0, fmt.Errorf("start Windows Installer: %w", err)
	}
	report("Checking the driver has been removed…")
	outcome := OutcomeRemoved
	switch exit {
	case exitSuccess:
	case exitSuccessRebootNeeds, exitRebootInitiated:
		outcome = OutcomeRebootRequired
	case exitUserCancelled:
		return OutcomeDeclined, nil
	case exitUnknownProduct:
		return 0, fmt.Errorf("Windows has no record of %s under product code %s; remove it from Windows Settings > Apps instead", m.Spec.Name, code)
	default:
		return 0, fmt.Errorf("removing %s failed with exit code %d", m.Spec.Name, exit)
	}
	after, err := m.Deps.Detect()
	if err == nil && after.Installed && outcome != OutcomeRebootRequired {
		return 0, fmt.Errorf("Windows Installer finished but %s is still present; restart the PC and check again", m.Spec.Name)
	}
	_ = os.Remove(filepath.Join(m.StateDir, receiptName))
	return outcome, nil
}

func (m *Manager) writeReceipt(productCode string) error {
	if !productCodeRE.MatchString(productCode) {
		productCode = ""
	}
	b, err := json.MarshalIndent(receipt{Name: m.Spec.Name, Version: m.Spec.Version, SHA256: m.Spec.SHA256, ProductCode: productCode}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.StateDir, receiptName), b, 0o600)
}

func (m *Manager) receiptProductCode() string {
	b, err := os.ReadFile(filepath.Join(m.StateDir, receiptName))
	if err != nil {
		return ""
	}
	var r receipt
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.ProductCode
}

// productCodeRE is the only shape a product code may take before it is put
// on an elevated command line.
var productCodeRE = regexp.MustCompile(`^\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}$`)

func hashMatches(r io.Reader, want string) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("re-read the installer: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(want))) != 1 {
		return fmt.Errorf("the installer changed after download (sha256 %s)", got)
	}
	return nil
}

// versionAtLeast compares the first two components of a dotted version. An
// empty version (file present, resource unreadable) is treated as
// compatible — see Status.Compatible.
func versionAtLeast(v string, major, minor int) bool {
	if v == "" {
		return true
	}
	parts := strings.Split(v, ".")
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	mn := 0
	if len(parts) > 1 {
		if mn, err = strconv.Atoi(parts[1]); err != nil {
			return false
		}
	}
	return maj > major || (maj == major && mn >= minor)
}

func hostOf(url string) string {
	rest := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
