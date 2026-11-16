//go:build windows && byodv2e2e

package solarbeam

// End-to-end check of a Windows BYOD engine v2 package through the light
// agent's real ModeInteractive code path: Update (download, SHA-256, extract,
// Authenticode), Start (bridge, then engine), Status and Stop.
//
// It is NOT part of the normal suite. It needs a real Windows desktop session,
// a standard (non-administrator) user, the package zip, a local LiveKit
// server and the byod-local-room stand-in (engine repo
// tools/solarbeam/byod-windows-v2). Nothing here contacts ReadyApp, the C2 or
// any production service: every URL comes from the environment and the test
// refuses a non-loopback one.
//
//	GOOS=windows go test -c -tags byodv2e2e -o byodv2-e2e.test.exe ./internal/solarbeam
//	byodv2-e2e.test.exe -test.run TestBYODEngineV2EndToEnd -test.v -test.timeout 20m
//
// Environment:
//
//	BYODV2_ZIP            package zip to install
//	BYODV2_READYAPP       loopback URL of byod-local-room's bundle route
//	BYODV2_ROOM_REPORT    byod-local-room's report file
//	BYODV2_OUT            directory for this test's JSON result
//	BYODV2_SIGNED=1       the zip's sunshine.exe carries the pinned ReadyFleet
//	                      signature. Otherwise Update must fail at Authenticode
//	                      without touching the install, and the test then
//	                      replaces the signature step (recorded in the result)
//	                      to exercise activation and sessions
//	BYODV2_SIGNER         signer fingerprint to pin (default: a placeholder)
//	BYODV2_DIAGNOSTIC_ALLOW_ADMIN=1  allow an administrator run, recorded as
//	                      diagnostic (never evidence of an unprivileged run)
//	SOLARBEAM_JWKS_URL    must be loopback: inherited by the engine so it never
//	                      reaches for the compiled-in production JWKS

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

type byodV2Session struct {
	SessionID            string   `json:"sessionId"`
	StartError           string   `json:"startError,omitempty"`
	StartMillis          int64    `json:"startMillis"`
	RunningAfterStart    bool     `json:"runningAfterStart"`
	FirstVideoMillis     int64    `json:"firstVideoMillis"`
	VideoPackets         int      `json:"videoPackets"`
	VideoFrames          int      `json:"videoFrames"`
	VideoMimeType        string   `json:"videoMimeType"`
	AudioPackets         int      `json:"audioPackets"`
	ProcessesDuring      []string `json:"processesDuring"`
	StopError            string   `json:"stopError,omitempty"`
	RunningAfterStop     bool     `json:"runningAfterStop"`
	ProcessesAfterStop   []string `json:"processesAfterStop"`
	BridgePortOpenAfter  bool     `json:"bridgePortOpenAfterStop"`
	BridgeLogEngineConns int      `json:"bridgeLogEngineConnected"`
}

type byodV2Result struct {
	User                         string          `json:"user"`
	Elevated                     bool            `json:"elevated"`
	AdministratorsSID            bool            `json:"administratorsMember"`
	SessionID                    uint32          `json:"windowsSessionId"`
	DiagnosticAdmin              bool            `json:"diagnosticAdminRun"`
	InstallRoot                  string          `json:"installRoot"`
	ZipSHA256                    string          `json:"zipSha256"`
	UpdateError                  string          `json:"updateError,omitempty"`
	InstalledFiles               int             `json:"installedFiles"`
	EngineVersion                string          `json:"engineVersion"`
	ServiceHostPresent           bool            `json:"serviceHostPresent"`
	UnsignedLeftInstallUntouched bool            `json:"unsignedPackageLeftInstallUntouched"`
	SignatureBypassedFor         []string        `json:"signatureStepBypassedFor,omitempty"`
	ConfigCarriedAcrossUpdate    bool            `json:"configCarriedAcrossReupdate"`
	Sessions                     []byodV2Session `json:"sessions"`
}

func TestBYODEngineV2EndToEnd(t *testing.T) {
	zipPath := os.Getenv("BYODV2_ZIP")
	if zipPath == "" {
		t.Skip("BYODV2_ZIP not set")
	}
	readyapp := mustLoopbackURL(t, "BYODV2_READYAPP")
	mustLoopbackURL(t, "SOLARBEAM_JWKS_URL")
	roomReport := os.Getenv("BYODV2_ROOM_REPORT")
	outDir := os.Getenv("BYODV2_OUT")
	if roomReport == "" || outDir == "" {
		t.Fatal("BYODV2_ROOM_REPORT and BYODV2_OUT are required")
	}
	res := &byodV2Result{}
	defer func() {
		b, _ := json.MarshalIndent(res, "", "  ")
		_ = os.WriteFile(filepath.Join(outDir, "byodv2-e2e-result.json"), b, 0o644)
	}()

	// The whole point is an unprivileged run: refuse to produce evidence as an
	// administrator.
	token := windows.GetCurrentProcessToken()
	res.Elevated = token.IsElevated()
	adminSID, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	res.AdministratorsSID, _ = token.IsMember(adminSID)
	if u, err := token.GetTokenUser(); err == nil {
		if account, domain, _, err := u.User.Sid.LookupAccount(""); err == nil {
			res.User = domain + `\` + account
		}
	}
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &res.SessionID)
	if res.Elevated || res.AdministratorsSID {
		// A diagnostic run as the desktop's administrator owner separates
		// "needs privilege" from "a second user cannot capture someone else's
		// desktop". It is recorded as such and is never unprivileged evidence.
		if os.Getenv("BYODV2_DIAGNOSTIC_ALLOW_ADMIN") != "1" {
			t.Fatalf("must run as a standard user (elevated=%v administrators=%v)", res.Elevated, res.AdministratorsSID)
		}
		res.DiagnosticAdmin = true
	}

	// The light agent's own path derivation (platform_paths_windows.go).
	root := filepath.Join(os.Getenv("LocalAppData"), "SolarBeam")
	res.InstallRoot = root
	paths := Paths{
		Engine: filepath.Join(root, "sunshine.exe"),
		Bridge: filepath.Join(root, "solarbeam-bridge.exe"),
		Root:   root,
		Mode:   ModeInteractive,
	}

	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipBytes)
	res.ZipSHA256 = hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	signer := os.Getenv("BYODV2_SIGNER")
	if signer == "" {
		signer = strings.Repeat("ab", 32)
	}
	m := New(paths)
	m.TrustedSigners = map[string]struct{}{signer: {}}
	update := func() error {
		return m.Update(context.Background(), proto.SolarbeamEngineUpdateArgs{
			ReleaseID: "byodv2-e2e", Version: "byodv2-e2e", URL: srv.URL + "/engine.zip",
			SHA256: res.ZipSHA256, SignerFingerprint: signer, ProductID: solarbeamEngineProductID,
		})
	}
	snapshot := func() string {
		var b strings.Builder
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				fmt.Fprintf(&b, "%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
		return b.String()
	}

	// 1. The real Authenticode check against the package as given.
	before := snapshot()
	err = update()
	if err != nil {
		res.UpdateError = err.Error()
	}
	signed := os.Getenv("BYODV2_SIGNED") == "1"
	if signed && err != nil {
		t.Fatalf("signed package update: %v", err)
	}
	if !signed {
		if err == nil || !strings.Contains(err.Error(), "authenticode") {
			t.Fatalf("unsigned package must fail at the Authenticode step, got %v", err)
		}
		// Verify-before-activate: the install is exactly as it was.
		res.UnsignedLeftInstallUntouched = snapshot() == before
		if !res.UnsignedLeftInstallUntouched {
			t.Fatal("a package that failed Authenticode changed the install directory")
		}
		// 2. Continue with the signature step replaced, recorded as such, so
		// the unsigned local build can exercise activation and sessions.
		old := verifyEngineSignature
		verifyEngineSignature = func(path, fp string) error {
			res.SignatureBypassedFor = append(res.SignatureBypassedFor, path)
			return nil
		}
		defer func() { verifyEngineSignature = old }()
		if err := update(); err != nil {
			t.Fatalf("update with the signature step bypassed: %v", err)
		}
	}
	for _, p := range []string{paths.Engine, paths.Bridge, filepath.Join(root, packageManifestName)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("package file missing after activation: %v", err)
		}
	}
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			res.InstalledFiles++
		}
		return nil
	})
	_, statErr := os.Stat(filepath.Join(root, "tools", engineServiceBinName))
	res.ServiceHostPresent = statErr == nil
	res.EngineVersion = m.Status(context.Background()).EngineVersion

	session := func(label string) {
		s := runBYODV2Session(t, m, paths, readyapp, roomReport, fmt.Sprintf("byodv2-%s-%d", label, time.Now().Unix()))
		res.Sessions = append(res.Sessions, s)
		if s.StartError != "" || s.VideoPackets == 0 || s.RunningAfterStop || len(s.ProcessesAfterStop) > 0 || s.BridgePortOpenAfter {
			t.Errorf("session %s failed: %+v", label, s)
		}
		time.Sleep(5 * time.Second)
	}
	session("s1")
	session("s2")

	// 3. Update again over the engine's own state: config must carry across.
	statePath := filepath.Join(root, engineConfigDirName, "vibeshine_state.json")
	stateBefore, _ := os.ReadFile(statePath)
	if err := update(); err != nil {
		t.Fatalf("re-update: %v", err)
	}
	stateAfter, err := os.ReadFile(statePath)
	res.ConfigCarriedAcrossUpdate = err == nil && len(stateBefore) > 0 && string(stateBefore) == string(stateAfter)
	if !res.ConfigCarriedAcrossUpdate {
		t.Errorf("engine state not carried across the re-update (before %d bytes, after err %v)", len(stateBefore), err)
	}
	session("s3-after-reupdate")
}

func runBYODV2Session(t *testing.T, m *Manager, paths Paths, readyapp, roomReport, sid string) byodV2Session {
	s := byodV2Session{SessionID: sid}
	logPath := filepath.Join(childLogDirInteractive(), "bridge.log")
	before := countInFile(logPath, "engine connected")
	baseline := roomVideo(roomReport, "host-"+sid)

	t0 := time.Now()
	err := m.Start(context.Background(), proto.SolarbeamStartArgs{
		SessionID: sid, MachineID: "byodv2-e2e-machine", ReadyappURL: readyapp,
		BundleToken: "byod-v2-local-bundle-token",
	})
	s.StartMillis = time.Since(t0).Milliseconds()
	if err != nil {
		s.StartError = err.Error()
		_ = m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: sid})
		return s
	}
	s.RunningAfterStart = m.Status(context.Background()).Running

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		v := roomVideo(roomReport, "host-"+sid)
		if v.packets > baseline.packets {
			s.FirstVideoMillis = time.Since(t0).Milliseconds()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if s.FirstVideoMillis > 0 {
		time.Sleep(10 * time.Second) // let it stream for a while
	}
	v := roomVideo(roomReport, "host-"+sid)
	s.VideoPackets, s.VideoFrames, s.VideoMimeType, s.AudioPackets = v.packets, v.frames, v.mime, v.audio
	s.ProcessesDuring = solarbeamProcesses()
	s.BridgeLogEngineConns = countInFile(logPath, "engine connected") - before

	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: sid}); err != nil {
		s.StopError = err.Error()
	}
	time.Sleep(3 * time.Second)
	s.RunningAfterStop = m.Status(context.Background()).Running
	s.ProcessesAfterStop = solarbeamProcesses()
	if c, err := net.DialTimeout("tcp", bridgeWSAddr, time.Second); err == nil {
		_ = c.Close()
		s.BridgePortOpenAfter = true
	}
	return s
}

type roomVideoStats struct {
	packets, frames, audio int
	mime                   string
}

func roomVideo(path, identity string) roomVideoStats {
	var rep struct {
		Tracks map[string]struct {
			Identity string `json:"identity"`
			Kind     string `json:"kind"`
			MimeType string `json:"mimeType"`
			Packets  int    `json:"packets"`
			Frames   int    `json:"frames"`
		} `json:"tracks"`
	}
	var out roomVideoStats
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &rep) != nil {
		return out
	}
	for _, tr := range rep.Tracks {
		if tr.Identity != identity {
			continue
		}
		if tr.Kind == "video" {
			out.packets += tr.Packets
			out.frames += tr.Frames
			out.mime = tr.MimeType
		} else {
			out.audio += tr.Packets
		}
	}
	return out
}

func countInFile(path, needle string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return strings.Count(string(b), needle)
}

// solarbeamProcesses lists every running process whose image name belongs to
// the engine package or the bridge, with its PID and parent PID.
func solarbeamProcesses() []string {
	var out []string
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return []string{"snapshot failed: " + err.Error()}
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if strings.HasPrefix(name, "sunshine") || strings.HasPrefix(name, "solarbeam") {
			out = append(out, fmt.Sprintf("%s pid=%d ppid=%d", name, pe.ProcessID, pe.ParentProcessID))
		}
	}
	return out
}

func mustLoopbackURL(t *testing.T, key string) string {
	t.Helper()
	raw := os.Getenv(key)
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		t.Fatalf("%s must be set to a loopback URL", key)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("%s=%q is not loopback; this test never talks to a real service", key, raw)
	}
	return raw
}
