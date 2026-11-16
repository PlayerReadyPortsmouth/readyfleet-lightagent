package gamepaddriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePC is a Windows PC as far as Manager can tell: a driver file that may
// or may not exist, a download server, a signature checker and a UAC prompt.
type fakePC struct {
	t              *testing.T
	installed      bool
	version        string
	payload        []byte
	downloadErrs   map[string]error // by URL
	downloadedFrom []string
	swapAfterDL    []byte // replaces the staged file's content before Hold
	signerErr      error
	elevateErr     error
	exitCode       uint32
	installOnRun   bool
	elevated       []string // "exe args"
	sessionActive  bool
	productCode    string
	heldDuringRun  bool
	held           bool
}

func (f *fakePC) deps() Deps {
	return Deps{
		Detect: func() (Status, error) {
			return Status{Installed: f.installed, Version: f.version}, nil
		},
		Download: func(_ context.Context, url, sha, dir string, max int64) (string, error) {
			f.downloadedFrom = append(f.downloadedFrom, url)
			if err := f.downloadErrs[url]; err != nil {
				return "", err
			}
			sum := sha256.Sum256(f.payload)
			if hex.EncodeToString(sum[:]) != sha {
				return "", errors.New("sha256 mismatch")
			}
			if int64(len(f.payload)) > max {
				return "", errors.New("too big")
			}
			p := filepath.Join(dir, "vigembus-setup-1.exe")
			if err := os.WriteFile(p, f.payload, 0o600); err != nil {
				f.t.Fatal(err)
			}
			if f.swapAfterDL != nil {
				_ = os.WriteFile(p, f.swapAfterDL, 0o600)
			}
			return p, nil
		},
		Hold: func(path string) (io.ReadCloser, error) {
			fh, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			f.held = true
			return closeHook{fh, func() { f.held = false }}, nil
		},
		VerifySigner: func(path, fp string) error {
			if fp != ViGEmBus.SignerSHA256 {
				f.t.Errorf("signer pin = %q, want the spec's", fp)
			}
			return f.signerErr
		},
		RunElevated: func(_ context.Context, exe, args string) (uint32, error) {
			f.elevated = append(f.elevated, filepath.Base(exe)+" "+args)
			f.heldDuringRun = f.held
			if f.elevateErr != nil {
				return 0, f.elevateErr
			}
			if f.installOnRun {
				f.installed = !f.installed
				f.version = "1.21.442.0"
			}
			return f.exitCode, nil
		},
		Msiexec:              func() (string, error) { return `C:\Windows\System32\msiexec.exe`, nil },
		InstalledProductCode: func() (string, error) { return f.productCode, nil },
		SessionActive:        func() bool { return f.sessionActive },
	}
}

type closeHook struct {
	io.ReadCloser
	after func()
}

func (c closeHook) Close() error { err := c.ReadCloser.Close(); c.after(); return err }

// payloadSpec is ViGEmBus with the hash of a small test payload, so the
// download path is exercised end to end without 6 MB of fixture.
func newManager(t *testing.T, pc *fakePC) *Manager {
	t.Helper()
	pc.t = t
	if pc.payload == nil {
		pc.payload = []byte("pretend this is the Nefarius bootstrapper")
	}
	spec := ViGEmBus
	sum := sha256.Sum256(pc.payload)
	spec.SHA256 = hex.EncodeToString(sum[:])
	spec.SizeBytes = int64(len(pc.payload))
	return &Manager{Spec: spec, Deps: pc.deps(), StateDir: t.TempDir()}
}

func TestInstall_HappyPath(t *testing.T) {
	pc := &fakePC{installOnRun: true, productCode: "{9C581C76-2D68-40F8-AA6F-94D3C5215C05}"}
	m := newManager(t, pc)
	var steps []string
	got, err := m.Install(context.Background(), func(s string) { steps = append(steps, s) })
	if err != nil || got != OutcomeInstalled {
		t.Fatalf("Install = %v, %v; want OutcomeInstalled", got, err)
	}
	if len(pc.elevated) != 1 || !strings.HasSuffix(pc.elevated[0], " /exenoui /qn /norestart") {
		t.Fatalf("elevated runs = %q; want exactly one silent installer run", pc.elevated)
	}
	if !pc.heldDuringRun {
		t.Fatal("the installer was run without the file locked against replacement")
	}
	if pc.downloadedFrom[0] != ReleaseStoreURL {
		t.Fatalf("first download from %q; want our release store first", pc.downloadedFrom[0])
	}
	entries, _ := os.ReadDir(m.StateDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".exe") {
			t.Fatalf("staged installer %s left behind", e.Name())
		}
	}
	st, _ := m.Status()
	if !st.Ready() || !st.InstalledByUs {
		t.Fatalf("status after install = %+v; want ready and installed by us", st)
	}
	if len(steps) == 0 {
		t.Fatal("no progress reported")
	}
}

func TestInstall_AlreadyInstalledDoesNothing(t *testing.T) {
	pc := &fakePC{installed: true, version: "1.21.442.0"}
	m := newManager(t, pc)
	got, err := m.Install(context.Background(), nil)
	if err != nil || got != OutcomeAlreadyInstalled {
		t.Fatalf("Install = %v, %v", got, err)
	}
	if len(pc.downloadedFrom) != 0 || len(pc.elevated) != 0 {
		t.Fatal("downloaded or prompted for a driver that is already there")
	}
	if st, _ := m.Status(); st.InstalledByUs {
		t.Fatal("a driver someone else installed must not be recorded as ours")
	}
}

func TestInstall_RefusesOverAnOldDriver(t *testing.T) {
	pc := &fakePC{installed: true, version: "1.16.116.0"}
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("Install over 1.16 = %v; want a refusal naming the old version", err)
	}
	if len(pc.elevated) != 0 {
		t.Fatal("ran the installer over an old driver (the in-place upgrade is broken)")
	}
}

func TestInstall_DeclinedUACChangesNothing(t *testing.T) {
	pc := &fakePC{elevateErr: ErrElevationDeclined}
	m := newManager(t, pc)
	got, err := m.Install(context.Background(), nil)
	if err != nil || got != OutcomeDeclined {
		t.Fatalf("Install = %v, %v; want OutcomeDeclined", got, err)
	}
	if st, _ := m.Status(); st.Installed || st.InstalledByUs {
		t.Fatalf("status after a declined prompt = %+v", st)
	}
}

func TestInstall_SignerMismatchNeverPrompts(t *testing.T) {
	pc := &fakePC{signerErr: errors.New("signer fingerprint mismatch")}
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); err == nil {
		t.Fatal("Install succeeded with the wrong signer")
	}
	if len(pc.elevated) != 0 {
		t.Fatal("asked for administrator rights for a file with the wrong signer")
	}
}

func TestInstall_FileSwappedAfterDownloadNeverPrompts(t *testing.T) {
	pc := &fakePC{swapAfterDL: []byte("something else entirely")}
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "changed after download") {
		t.Fatalf("Install = %v; want the re-hash to catch the swap", err)
	}
	if len(pc.elevated) != 0 {
		t.Fatal("elevated a file that changed after its hash was checked")
	}
}

func TestInstall_FallsBackToVendorURL(t *testing.T) {
	pc := &fakePC{installOnRun: true, downloadErrs: map[string]error{ReleaseStoreURL: errors.New("status 404")}}
	m := newManager(t, pc)
	if got, err := m.Install(context.Background(), nil); err != nil || got != OutcomeInstalled {
		t.Fatalf("Install = %v, %v", got, err)
	}
	if len(pc.downloadedFrom) != 2 || !strings.Contains(pc.downloadedFrom[1], "github.com/nefarius/ViGEmBus") {
		t.Fatalf("downloads = %q; want the release store then the vendor asset", pc.downloadedFrom)
	}
}

func TestInstall_AllDownloadsFail(t *testing.T) {
	pc := &fakePC{downloadErrs: map[string]error{}}
	for _, u := range ViGEmBus.URLs {
		pc.downloadErrs[u] = errors.New("offline")
	}
	m := newManager(t, pc)
	_, err := m.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "readyapp.player-ready.co.uk") || !strings.Contains(err.Error(), "github.com") {
		t.Fatalf("Install = %v; want both hosts named", err)
	}
}

func TestInstall_ExitCodes(t *testing.T) {
	cases := []struct {
		code    uint32
		want    Outcome
		wantErr string
	}{
		{3010, OutcomeRebootRequired, ""},
		{1641, OutcomeRebootRequired, ""},
		{1602, OutcomeDeclined, ""},
		{1618, 0, "another installation"},
		{1603, 0, "exit code 1603"},
	}
	for _, c := range cases {
		pc := &fakePC{exitCode: c.code, installOnRun: c.code == 3010 || c.code == 1641}
		m := newManager(t, pc)
		got, err := m.Install(context.Background(), nil)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("exit %d: err = %v; want %q", c.code, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("exit %d: = %v, %v; want %v", c.code, got, err, c.want)
		}
	}
}

func TestInstall_SuccessButNoDriverIsAnError(t *testing.T) {
	pc := &fakePC{} // exit 0, but the driver never appears
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); err == nil {
		t.Fatal("a zero exit with no driver file was reported as success")
	}
}

func TestInstall_RefusedDuringSession(t *testing.T) {
	pc := &fakePC{sessionActive: true}
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("Install during a session = %v", err)
	}
	if len(pc.downloadedFrom) != 0 {
		t.Fatal("started work during a session")
	}
}

func TestUninstall_UsesRecordedProductCodeThroughMsiexec(t *testing.T) {
	pc := &fakePC{installOnRun: true, productCode: "{11111111-2222-3333-4444-555555555555}"}
	m := newManager(t, pc)
	if _, err := m.Install(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	pc.elevated = nil
	got, err := m.Uninstall(context.Background(), nil)
	if err != nil || got != OutcomeRemoved {
		t.Fatalf("Uninstall = %v, %v", got, err)
	}
	want := "msiexec.exe /x {11111111-2222-3333-4444-555555555555} /qn /norestart"
	if len(pc.elevated) != 1 || !strings.HasSuffix(pc.elevated[0], want) {
		t.Fatalf("elevated = %q; want %q", pc.elevated, want)
	}
	if st, _ := m.Status(); st.Installed || st.InstalledByUs {
		t.Fatalf("status after uninstall = %+v", st)
	}
}

func TestUninstall_FallsBackToSpecProductCode(t *testing.T) {
	pc := &fakePC{installed: true, version: "1.21.442.0", installOnRun: true}
	m := newManager(t, pc)
	if _, err := m.Uninstall(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pc.elevated[0], ViGEmBus.ProductCode) {
		t.Fatalf("elevated = %q; want the pinned product code", pc.elevated)
	}
}

func TestUninstall_RejectsMalformedProductCode(t *testing.T) {
	pc := &fakePC{installed: true, productCode: "{x} & calc"}
	m := newManager(t, pc)
	m.Spec.ProductCode = "not-a-guid"
	if _, err := m.Uninstall(context.Background(), nil); err == nil {
		t.Fatal("a malformed product code reached an elevated command line")
	}
	if len(pc.elevated) != 0 {
		t.Fatal("prompted with a malformed product code")
	}
}

func TestUninstall_NothingInstalled(t *testing.T) {
	pc := &fakePC{}
	m := newManager(t, pc)
	if got, err := m.Uninstall(context.Background(), nil); err != nil || got != OutcomeNotInstalled {
		t.Fatalf("Uninstall = %v, %v", got, err)
	}
	if len(pc.elevated) != 0 {
		t.Fatal("prompted to remove nothing")
	}
}

func TestSingleFlight(t *testing.T) {
	pc := &fakePC{}
	m := newManager(t, pc)
	if err := m.begin(); err != nil {
		t.Fatal(err)
	}
	defer m.end()
	if _, err := m.Install(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent Install = %v; want ErrBusy", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"":           true,
		"1.21.442.0": true,
		"1.17.0.0":   true,
		"1.16.116.0": false,
		"2.0.0.0":    true,
		"0.99":       false,
		"garbage":    false,
		"1.x.442.0":  false,
		"1.17":       true,
	} {
		if got := versionAtLeast(v, 1, 17); got != want {
			t.Errorf("versionAtLeast(%q) = %v; want %v", v, got, want)
		}
	}
}

// The pins are the whole security story; a typo here would either brick the
// flow or, worse, make it trust something else. Keep them in lockstep with
// ops/solarbeam-gamepad-portsadmin-20260926/artefact/artefact.json.
func TestSpecPins(t *testing.T) {
	s := ViGEmBus
	if s.SHA256 != "89220a7865076b342892f98865f3499fb7c4cfd673159e89d352c360fd014c6a" {
		t.Error("installer sha256 changed")
	}
	if s.SignerSHA256 != "e84e8537968cdc234e17a232bd813e3ea4e5b7826a8062f5cb033047e1ce3da0" {
		t.Error("signer certificate sha256 changed")
	}
	if !productCodeRE.MatchString(s.ProductCode) {
		t.Error("spec product code is not a GUID")
	}
	for _, u := range s.URLs {
		if !strings.HasPrefix(u, "https://") || strings.Contains(u, "?") {
			t.Errorf("URL %q must be https and query-string free", u)
		}
	}
}
