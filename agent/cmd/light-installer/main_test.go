package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall"
)

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "readyfleet-light-installer-tests-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("LocalAppData", base)
	code := m.Run()
	_ = os.RemoveAll(base)
	os.Exit(code)
}

func withServerURL(t *testing.T, url string) {
	t.Helper()
	original := serverURL
	serverURL = url
	t.Cleanup(func() { serverURL = original })
}

func TestRunReturnsStableExitCodesAndAlwaysRemovesPayload(t *testing.T) {
	withServerURL(t, "wss://c2.example.test")

	tests := []struct {
		name       string
		loadErr    error
		installErr error
		removeErr  error
		want       int
	}{
		{name: "success", want: exitSuccess},
		{name: "invalid payload", loadErr: errors.New("invalid rft_secret"), want: exitInvalidPayload},
		{name: "install failure", installErr: errors.New("failed rft_secret"), want: exitInstallFailed},
		{name: "payload cleanup failure", removeErr: errors.New("cleanup failed"), want: exitCleanupFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var removed string
			deps := commandDeps{
				load: func(path string) (lightinstall.Payload, error) {
					return lightinstall.Payload{EnrollmentToken: "rft_secret"}, tt.loadErr
				},
				install: func(context.Context, lightinstall.Payload) error {
					return tt.installErr
				},
				remove: func(path string) error {
					removed = path
					return tt.removeErr
				},
			}
			stderr := new(strings.Builder)
			exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")

			got := run(context.Background(), []string{filepath.Join(filepath.Dir(exePath), "enrollment.json")}, stderr, exePath, deps)
			if got != tt.want {
				t.Fatalf("exit code = %d, want %d", got, tt.want)
			}
			if removed != filepath.Join(filepath.Dir(exePath), "enrollment.json") {
				t.Fatalf("removed path = %q", removed)
			}
			if strings.Contains(stderr.String(), "rft_secret") {
				t.Fatalf("stderr leaked token: %q", stderr)
			}
			if tt.removeErr != nil && strings.Contains(stderr.String(), "completed") {
				t.Fatalf("cleanup failure reported success first: %q", stderr)
			}
		})
	}
}

func TestRunRejectsMoreThanOnePayloadArgument(t *testing.T) {
	withServerURL(t, "wss://c2.example.test")

	exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")
	got := run(context.Background(), []string{"one.json", "two.json"}, io.Discard, exePath, commandDeps{})
	if got != exitUsage {
		t.Fatalf("exit code = %d, want %d", got, exitUsage)
	}
}

func TestRunRefusesToRunWithoutServerURL(t *testing.T) {
	withServerURL(t, "")

	exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")
	got := run(context.Background(), nil, io.Discard, exePath, commandDeps{})
	if got != exitNotConfigured {
		t.Fatalf("exit code = %d, want %d", got, exitNotConfigured)
	}
}

func TestRunDefaultPayloadPathIsBesideInstaller(t *testing.T) {
	withServerURL(t, "wss://c2.example.test")

	exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")
	var loadedPath string
	deps := commandDeps{
		load: func(path string) (lightinstall.Payload, error) {
			loadedPath = path
			return lightinstall.Payload{}, errors.New("stop after load")
		},
		install: func(context.Context, lightinstall.Payload) error { return nil },
		remove:  func(string) error { return nil },
	}

	run(context.Background(), nil, io.Discard, exePath, deps)

	want := filepath.Join(filepath.Dir(exePath), "enrollment.json")
	if loadedPath != want {
		t.Fatalf("loaded payload path = %q, want %q", loadedPath, want)
	}
}

// --- self-contained trailer payload ------------------------------------

func lenPrefixed(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func writeSelfContainedExe(t *testing.T, prefix []byte, agent, token []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "installer.exe")
	var buf []byte
	buf = append(buf, prefix...)
	buf = append(buf, []byte(selfPayloadMagic)...)
	buf = append(buf, lenPrefixed(agent)...)
	buf = append(buf, lenPrefixed(token)...)
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestSelfContainedPayload_ExtractsAgentAndToken(t *testing.T) {
	agent := []byte("pretend agent exe bytes")
	token := []byte(`{"enrollment_token":"rft_x"}`)
	path := writeSelfContainedExe(t, []byte("unsigned PE bytes here..."), agent, token)

	gotAgent, gotToken, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true when the marker is present")
	}
	if string(gotAgent) != string(agent) {
		t.Errorf("agent bytes = %q, want %q", gotAgent, agent)
	}
	if string(gotToken) != string(token) {
		t.Errorf("token bytes = %q, want %q", gotToken, token)
	}
}

// Regression test: a real compiled light-installer.exe contains the
// selfPayloadMagic string as its own Go constant literal, BEFORE any real
// appended trailer — caught live when an early bytes.Index-based
// implementation found that spurious earlier match and failed reading
// garbage bytes after it. selfContainedPayload must find the LAST match
// (the real, appended-after-everything trailer), not the first.
func TestSelfContainedPayload_IgnoresEarlierIncidentalMagicOccurrence(t *testing.T) {
	agent := []byte("real agent bytes")
	token := []byte(`{"enrollment_token":"rft_x"}`)
	// A spurious earlier occurrence of the magic string, as if it were
	// compiled into the binary's own .rodata, followed by bytes that would
	// be nonsense if misread as a trailer.
	prefix := append([]byte("unsigned PE bytes... "), []byte(selfPayloadMagic)...)
	prefix = append(prefix, []byte("...more unrelated compiled-in bytes...")...)
	path := writeSelfContainedExe(t, prefix, agent, token)

	gotAgent, gotToken, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if string(gotAgent) != string(agent) {
		t.Errorf("agent bytes = %q, want %q", gotAgent, agent)
	}
	if string(gotToken) != string(token) {
		t.Errorf("token bytes = %q, want %q", gotToken, token)
	}
}

func TestSelfContainedPayload_NoMarkerFallsBackCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.exe")
	if err := os.WriteFile(path, []byte("just an ordinary unsigned build, no trailer"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	agent, token, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if ok || agent != nil || token != nil {
		t.Fatalf("expected ok=false, nil, nil for a build with no trailer; got ok=%v agent=%v token=%v", ok, agent, token)
	}
}

// A truncated or out-of-range trailer must not read past the buffer, must not
// stage a half-read agent, and must NOT be fatal.
//
// These two fixtures used to assert an error. They are byte-indistinguishable
// from an ordinary build — every compiled installer carries selfPayloadMagic in
// its own .rodata as the Go constant, followed by unrelated bytes — so
// "magic + bytes that don't parse => error" made every non-self-contained
// install die with "installer file is corrupt" before it could fall back to the
// sibling agent + enrollment.json. That is the shape the Linux bundle ships, so
// Linux BYOD enrolment could never have worked. Falling back is the safe
// outcome: nothing is installed from the unparseable bytes, and the sibling
// path reports its own accurate result.
func TestSelfContainedPayload_TruncatedTrailerFallsBackWithoutReadingPastTheBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.exe")
	buf := append([]byte("pe bytes"), []byte(selfPayloadMagic)...)
	buf = append(buf, lenPrefixed([]byte("short"))[:3]...) // truncated length prefix itself
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	agent, token, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if ok || agent != nil || token != nil {
		t.Fatalf("expected a clean fallback; got ok=%v agent=%v token=%v", ok, agent, token)
	}
}

func TestSelfContainedPayload_OversizedLengthPrefixFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.exe")
	buf := append([]byte("pe bytes"), []byte(selfPayloadMagic)...)
	lenBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuf, 0xFFFFFFF0) // wildly larger than the file
	buf = append(buf, lenBuf...)
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, _, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if ok {
		t.Fatal("an out-of-range length prefix must not be accepted as a trailer")
	}
}

// THE regression test for the Linux bundle. An ordinary build carries the magic
// as its own compiled-in constant with megabytes of unrelated binary after it,
// and no trailer at all. It must fall back cleanly so the installer can load the
// sibling agent and enrollment.json — not report itself corrupt.
func TestSelfContainedPayload_OrdinaryBuildWithCompiledInMagicFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.exe")
	// The magic in .rodata, followed by other string constants, exactly as the
	// real binary lays it out ("RFLTPAY1" then "GoStrings", "signal: ", ...).
	buf := append([]byte("ELF header and code..."), []byte(selfPayloadMagic)...)
	buf = append(buf, []byte("GoStringssignal: more compiled-in constants")...)
	buf = append(buf, make([]byte, 4096)...) // the rest of the binary
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	agent, token, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("an ordinary build must not look corrupt: %v", err)
	}
	if ok || agent != nil || token != nil {
		t.Fatalf("expected ok=false for a build with no trailer; got ok=%v agent=%v token=%v", ok, agent, token)
	}
}

// A trailer that parses but leaves bytes over is not a trailer: the real one is
// appended last and therefore ends at EOF. Without the exact-EOF check, the
// incidental .rodata occurrence could parse by coincidence and stage garbage as
// the agent binary.
func TestSelfContainedPayload_TrailerMustEndAtEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.exe")
	var buf []byte
	buf = append(buf, []byte("pe bytes")...)
	buf = append(buf, []byte(selfPayloadMagic)...)
	buf = append(buf, lenPrefixed([]byte("agent"))...)
	buf = append(buf, lenPrefixed([]byte(`{"enrollment_token":"rft_x"}`))...)
	buf = append(buf, []byte("trailing bytes that should not be there")...)
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, _, ok, err := selfContainedPayload(path)
	if err != nil {
		t.Fatalf("selfContainedPayload: %v", err)
	}
	if ok {
		t.Fatal("a payload that does not end at EOF must not be accepted as a trailer")
	}
}

func TestInstallTargetPaths_UsesLocalAppData(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin ignores LocalAppData; see TestInstallTargetPaths_UsesApplicationSupportOnDarwin")
	}
	t.Setenv("LocalAppData", filepath.Join("C:", "Users", "mentor", "AppData", "Local"))

	got := installTargetPaths()
	wantRoot := filepath.Join("C:", "Users", "mentor", "AppData", "Local", "ReadyFleet Light")
	if got.Root != wantRoot {
		t.Errorf("Root = %q, want %q", got.Root, wantRoot)
	}
	if got.AgentExe != filepath.Join(wantRoot, installerAgentName()) {
		t.Errorf("AgentExe = %q", got.AgentExe)
	}
}

func TestInstallTargetPaths_UsesApplicationSupportOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only layout")
	}
	// A stray Windows variable must not steer a Mac into an AppData path.
	t.Setenv("LocalAppData", filepath.Join("C:", "Users", "mentor", "AppData", "Local"))
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got := installTargetPaths()
	wantRoot := filepath.Join(home, "Library", "Application Support", "ReadyFleet")
	if got.Root != wantRoot {
		t.Fatalf("Root = %q, want %q", got.Root, wantRoot)
	}
	if got.AgentExe != filepath.Join(wantRoot, "readyfleet-light") {
		t.Fatalf("AgentExe = %q", got.AgentExe)
	}
}

func TestInstallTargetPaths_UsesXDGOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only layout")
	}
	t.Setenv("LocalAppData", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	got := installTargetPaths()
	if filepath.Base(got.Root) != "readyfleet-light" {
		t.Fatalf("Root = %q", got.Root)
	}
	if filepath.Base(got.AgentExe) != "readyfleet-light" {
		t.Fatalf("AgentExe = %q", got.AgentExe)
	}
	if got.ConfigPath != filepath.Join(got.Root, "config.json") {
		t.Fatalf("ConfigPath = %q", got.ConfigPath)
	}
}

func TestWriteAgentBinary_CreatesDirAndWritesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "lightagent.exe")
	data := []byte("agent bytes")

	if err := writeAgentBinary(path, data); err != nil {
		t.Fatalf("writeAgentBinary: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("written bytes = %q, want %q", got, data)
	}
}

func TestPathsBeside(t *testing.T) {
	exePath := filepath.Join("C:", "install-dir", "ReadyFleetLightInstaller.exe")
	got := pathsBeside(exePath)

	want := lightinstall.Paths{
		Root:       filepath.Join("C:", "install-dir"),
		AgentExe:   filepath.Join("C:", "install-dir", installerAgentName()),
		ConfigPath: filepath.Join("C:", "install-dir", "config.json"),
		KeyPath:    filepath.Join("C:", "install-dir", "client.key"),
		LogPath:    filepath.Join("C:", "install-dir", "logs", "lightagent.log"),
	}
	if got != want {
		t.Fatalf("pathsBeside = %+v, want %+v", got, want)
	}
}

// --- BYOD consent gate outcomes ----------------------------------------

// A mentor who declined the device in ReadyApp, or who left the
// confirmation too long, must be told that specifically. The generic
// "installation failed; see the light agent log" is actively misleading
// here: nothing failed, and there is nothing in the log to look at.
func TestRunExplainsConsentGateOutcomes(t *testing.T) {
	withServerURL(t, "wss://c2.example.test")

	tests := []struct {
		name        string
		installErr  error
		wantPhrases []string
	}{
		{
			name: "declined",
			// Wrapped, exactly as lightinstall.Manager.Install returns it.
			installErr:  fmt.Errorf("lightinstall: %w", lightinstall.ErrEnrollmentDeclined),
			wantPhrases: []string{"declined", "ReadyApp"},
		},
		{
			name:        "expired",
			installErr:  fmt.Errorf("lightinstall: %w", lightinstall.ErrEnrollmentExpired),
			wantPhrases: []string{"expired", "ReadyApp"},
		},
		{
			// The backstop-timeout case: the poll loop never reached the
			// server for long enough to learn a real verdict — declined and
			// expired are both server answers, this is the server never
			// getting the chance to answer. It must not fall through to the
			// generic "see the light agent log" text, and it must name the
			// connectivity cause rather than reading like a ReadyApp
			// decision (that's what "nobody confirmed in time" — expired —
			// already covers).
			name:        "unreachable",
			installErr:  fmt.Errorf("lightinstall: %w", lightinstall.ErrEnrollmentUnreachable),
			wantPhrases: []string{"reach", "connection"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := commandDeps{
				load:    func(string) (lightinstall.Payload, error) { return lightinstall.Payload{}, nil },
				install: func(context.Context, lightinstall.Payload) error { return tt.installErr },
				remove:  func(string) error { return nil },
			}
			stderr := new(strings.Builder)
			exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")

			got := run(context.Background(), nil, stderr, exePath, deps)
			if got != exitInstallFailed {
				t.Fatalf("exit code = %d, want %d", got, exitInstallFailed)
			}
			detail := stderr.String()
			for _, phrase := range tt.wantPhrases {
				if !strings.Contains(detail, phrase) {
					t.Errorf("message %q should say why (%q)", detail, phrase)
				}
			}
			if strings.Contains(detail, "see the light agent log") {
				t.Errorf("message %q fell through to the generic failure text", detail)
			}
		})
	}
}

// Anything that is not a consent-gate verdict keeps the existing generic
// message — this is a new branch, not a replacement.
func TestRunKeepsGenericMessageForOtherFailures(t *testing.T) {
	withServerURL(t, "wss://c2.example.test")

	deps := commandDeps{
		load:    func(string) (lightinstall.Payload, error) { return lightinstall.Payload{}, nil },
		install: func(context.Context, lightinstall.Payload) error { return errors.New("bad signature") },
		remove:  func(string) error { return nil },
	}
	stderr := new(strings.Builder)
	exePath := filepath.Join(t.TempDir(), "ReadyFleetLightInstaller.exe")

	if got := run(context.Background(), nil, stderr, exePath, deps); got != exitInstallFailed {
		t.Fatalf("exit code = %d, want %d", got, exitInstallFailed)
	}
	if !strings.Contains(stderr.String(), "see the light agent log") {
		t.Errorf("message = %q, want the generic install-failure text", stderr.String())
	}
}

// signedFixture mimics ReadyApp's Windows download: template + trailer, then
// Authenticode-signed, which pads to 8 bytes and appends the certificate table
// AFTER the trailer. selfContainedPayload must still find the trailer; before
// 24 Sep 2026 it required the trailer to end at EOF and every signed Windows
// BYOD installer failed with "ReadyFleet enrollment file is invalid".
func signedFixture(t *testing.T, padding []byte, agent, token []byte) string {
	t.Helper()
	h := make([]byte, 0x200)
	h[0], h[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(h[0x3C:], 0x80)
	copy(h[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(h[0x80+24:], 0x20b)
	buf := append(h, []byte("compiled installer ... "+selfPayloadMagic+" ... more rodata")...)
	buf = append(buf, []byte(selfPayloadMagic)...)
	buf = append(buf, lenPrefixed(agent)...)
	buf = append(buf, lenPrefixed(token)...)
	buf = append(buf, padding...)
	certOff := len(buf)
	cert := []byte("PKCS#7 signed data ... " + selfPayloadMagic + " not a trailer")
	buf = append(buf, cert...)
	entry := 0x80 + 24 + 112 + 4*8
	binary.LittleEndian.PutUint32(buf[entry:], uint32(certOff))
	binary.LittleEndian.PutUint32(buf[entry+4:], uint32(len(cert)))
	path := filepath.Join(t.TempDir(), "signed-installer.exe")
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestSelfContainedPayload_FindsTrailerBeforeAuthenticodeSignature(t *testing.T) {
	agent := []byte("signed agent bytes")
	token := []byte(`{"enrollment_token":"rft_x"}`)
	for _, pad := range [][]byte{nil, {0, 0, 0, 0, 0}} {
		gotAgent, gotToken, ok, err := selfContainedPayload(signedFixture(t, pad, agent, token))
		if err != nil || !ok {
			t.Fatalf("padding %d: ok=%v err=%v, want the trailer found", len(pad), ok, err)
		}
		if string(gotAgent) != string(agent) || string(gotToken) != string(token) {
			t.Fatalf("padding %d: got agent %q token %q", len(pad), gotAgent, gotToken)
		}
	}
}

func TestSelfContainedPayload_RejectsNonZeroBytesBeforeSignature(t *testing.T) {
	_, _, ok, err := selfContainedPayload(signedFixture(t, []byte{0, 1, 0}, []byte("a"), []byte(`{"enrollment_token":"rft_x"}`)))
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want no trailer when data other than padding precedes the signature", ok, err)
	}
}
