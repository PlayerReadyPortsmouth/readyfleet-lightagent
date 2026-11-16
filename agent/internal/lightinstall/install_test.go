package lightinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- LoadPayload --------------------------------------------------------

func writePayloadFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "enrollment.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write payload fixture: %v", err)
	}
	return path
}

func TestLoadPayload_Valid(t *testing.T) {
	token := "rft_" + repeat("a", 43)
	path := writePayloadFile(t, `{"enrollment_token":"`+token+`"}`)

	payload, err := LoadPayload(path)
	if err != nil {
		t.Fatalf("LoadPayload: %v", err)
	}
	if payload.EnrollmentToken != token {
		t.Errorf("got token %q, want %q", payload.EnrollmentToken, token)
	}
}

func TestLoadPayload_RejectsExtraField(t *testing.T) {
	token := "rft_" + repeat("a", 43)
	path := writePayloadFile(t, `{"enrollment_token":"`+token+`","requested_name_base":"x"}`)

	if _, err := LoadPayload(path); err == nil {
		t.Fatal("expected error for unexpected field, got nil")
	}
}

func TestLoadPayload_RejectsDuplicateField(t *testing.T) {
	token := "rft_" + repeat("a", 43)
	path := writePayloadFile(t, `{"enrollment_token":"`+token+`","enrollment_token":"`+token+`"}`)

	if _, err := LoadPayload(path); err == nil {
		t.Fatal("expected error for duplicate field, got nil")
	}
}

func TestLoadPayload_RejectsMalformedToken(t *testing.T) {
	path := writePayloadFile(t, `{"enrollment_token":"not-a-real-token"}`)

	if _, err := LoadPayload(path); err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestLoadPayload_RejectsOversizedBody(t *testing.T) {
	token := "rft_" + repeat("a", 43)
	padding := repeat("x", maxPayloadBytes+1)
	path := writePayloadFile(t, `{"enrollment_token":"`+token+`","padding":"`+padding+`"}`)

	if _, err := LoadPayload(path); err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
}

func TestLoadPayload_RejectsMissingFile(t *testing.T) {
	if _, err := LoadPayload(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestRemovePayload_MissingFileIsNotAnError(t *testing.T) {
	if err := RemovePayload(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Errorf("RemovePayload on missing file: %v", err)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// --- Manager.Install / Uninstall orchestration --------------------------

func newTestManager() (*Manager, *[]string) {
	var calls []string
	m := &Manager{
		cfg: Config{Paths: Paths{
			Root:       "root",
			AgentExe:   "agent.exe",
			ConfigPath: "config.json",
			KeyPath:    "client.key",
			LogPath:    "lightagent.log",
		}},
	}
	m.verifyAgentFn = func(exePath string) error {
		calls = append(calls, "verify:"+exePath)
		return nil
	}
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload, onPending func()) error {
		calls = append(calls, "enroll")
		return nil
	}
	m.protectKeyFn = func(keyPath string) error {
		calls = append(calls, "protectKey:"+keyPath)
		return nil
	}
	m.registerTaskFn = func(agentExePath string) error {
		calls = append(calls, "registerTask:"+agentExePath)
		return nil
	}
	m.prepareEngineFn = func() error {
		calls = append(calls, "prepareEngine")
		return nil
	}
	m.startAgentFn = func(agentExePath string) error {
		calls = append(calls, "startAgent:"+agentExePath)
		return nil
	}
	m.waitForConnectedFn = func(ctx context.Context, logPath string, timeout time.Duration) error {
		calls = append(calls, "waitForConnected:"+logPath)
		return nil
	}
	m.unlinkOnlineFn = func(ctx context.Context, agentExePath string) error {
		calls = append(calls, "unlinkOnline:"+agentExePath)
		return nil
	}
	m.removeTaskFn = func() error {
		calls = append(calls, "removeTask")
		return nil
	}
	m.removeAllFn = func(root string) error {
		calls = append(calls, "removeAll:"+root)
		return nil
	}
	m.writeTombstoneFn = func(root string) error {
		calls = append(calls, "writeTombstone:"+root)
		return nil
	}
	return m, &calls
}

func TestManager_Install_OrderAndSuccess(t *testing.T) {
	m, calls := newTestManager()

	if err := m.Install(context.Background(), Payload{EnrollmentToken: "rft_x"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{
		"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe",
		"prepareEngine", "startAgent:agent.exe", "waitForConnected:lightagent.log",
	}
	assertCallOrder(t, *calls, want)
}

func TestManager_Install_StartAgentFailure_StopsBeforeWaitForConnected(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("exec denied")
	m.startAgentFn = func(agentExePath string) error {
		*calls = append(*calls, "startAgent:"+agentExePath)
		return wantErr
	}

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	assertCallOrder(t, *calls, []string{
		"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe",
		"prepareEngine", "startAgent:agent.exe",
	})
}

func TestManager_Install_WaitForConnectedFailure(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("timed out")
	m.waitForConnectedFn = func(ctx context.Context, logPath string, timeout time.Duration) error {
		*calls = append(*calls, "waitForConnected:"+logPath)
		return wantErr
	}

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	assertCallOrder(t, *calls, []string{
		"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe",
		"prepareEngine", "startAgent:agent.exe", "waitForConnected:lightagent.log",
	})
}

func TestManager_Install_ReportsProgress(t *testing.T) {
	m, _ := newTestManager()
	var steps []string
	m.Progress = func(step string) { steps = append(steps, step) }

	if err := m.Install(context.Background(), Payload{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(steps) != 7 {
		t.Fatalf("Progress called %d times, want 7: %v", len(steps), steps)
	}
}

// Preparing the engine environment is a stream-quality fix, not a correctness
// one: the agent enrolls, connects and takes commands without it, and the
// engine still streams (just with a three-second permission wait). Failing the
// whole install over it would trade a working machine for no machine at all.
func TestManager_Install_PrepareEngineFailureDoesNotAbortTheInstall(t *testing.T) {
	m, calls := newTestManager()
	m.prepareEngineFn = func() error {
		*calls = append(*calls, "prepareEngine")
		return errors.New("read-only file system")
	}
	var steps []string
	m.Progress = func(step string) { steps = append(steps, step) }

	if err := m.Install(context.Background(), Payload{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertCallOrder(t, *calls, []string{
		"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe",
		"prepareEngine", "startAgent:agent.exe", "waitForConnected:lightagent.log",
	})
	// The mentor must be told, since silently skipping it would make a slow
	// engine launch look like a fault with no trace of the cause.
	var warned bool
	for _, step := range steps {
		if strings.Contains(step, "read-only file system") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no progress line reported the skipped engine setup: %v", steps)
	}
}

func TestManager_Install_VerifyFailure_StopsImmediately(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("bad signature")
	m.verifyAgentFn = func(exePath string) error { return wantErr }

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no seam calls after verify failure, got %v", *calls)
	}
}

func TestManager_Install_EnrollFailure_StopsBeforeProtectKey(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("redeem rejected")
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload, onPending func()) error {
		*calls = append(*calls, "enroll")
		return wantErr
	}

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	assertCallOrder(t, *calls, []string{"verify:agent.exe", "enroll"})
}

func TestManager_Install_ProtectKeyFailure_StopsBeforeRegisterTask(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("icacls denied")
	m.protectKeyFn = func(keyPath string) error {
		*calls = append(*calls, "protectKey:"+keyPath)
		return wantErr
	}

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	assertCallOrder(t, *calls, []string{"verify:agent.exe", "enroll", "protectKey:client.key"})
}

func TestManager_Install_RegisterTaskFailure(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("registry write denied")
	m.registerTaskFn = func(agentExePath string) error {
		*calls = append(*calls, "registerTask:"+agentExePath)
		return wantErr
	}

	err := m.Install(context.Background(), Payload{})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want wrapping %v", err, wantErr)
	}
	assertCallOrder(t, *calls, []string{"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe"})
}

func TestManager_Uninstall_OnlineSuccess_NoTombstone(t *testing.T) {
	m, calls := newTestManager()

	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	want := []string{"unlinkOnline:agent.exe", "removeTask", "removeAll:root"}
	assertCallOrder(t, *calls, want)
	for _, c := range *calls {
		if c == "writeTombstone:root" {
			t.Errorf("expected no tombstone write on a successful online unlink, got calls %v", *calls)
		}
	}
}

func TestManager_Uninstall_OfflineWritesTombstoneThenContinues(t *testing.T) {
	m, calls := newTestManager()
	m.unlinkOnlineFn = func(ctx context.Context, agentExePath string) error {
		return errors.New("offline")
	}

	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	want := []string{"writeTombstone:root", "removeTask", "removeAll:root"}
	assertCallOrder(t, *calls, want)
}

func TestManager_Uninstall_OfflineAndTombstoneFails_AbortsBeforeCleanup(t *testing.T) {
	m, calls := newTestManager()
	m.unlinkOnlineFn = func(ctx context.Context, agentExePath string) error {
		return errors.New("offline")
	}
	m.writeTombstoneFn = func(root string) error {
		return errors.New("disk full")
	}

	if err := m.Uninstall(context.Background()); err == nil {
		t.Fatal("expected error when both unlink and tombstone write fail")
	}
	for _, c := range *calls {
		if c == "removeTask" || c[:len("removeAll")] == "removeAll" {
			t.Errorf("expected cleanup to be skipped when tombstone write fails, got calls %v", *calls)
		}
	}
}

func TestManager_Uninstall_RemoveTaskFailure_StopsBeforeRemoveAll(t *testing.T) {
	m, calls := newTestManager()
	wantErr := errors.New("registry delete denied")
	m.removeTaskFn = func() error { return wantErr }

	err := m.Uninstall(context.Background())
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Uninstall error = %v, want wrapping %v", err, wantErr)
	}
	for _, c := range *calls {
		if len(c) >= len("removeAll") && c[:len("removeAll")] == "removeAll" {
			t.Errorf("expected removeAll to be skipped after removeTask failure, got calls %v", *calls)
		}
	}
}

func assertCallOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order = %v, want %v", got, want)
		}
	}
}

func TestNew_WiresRealSeams(t *testing.T) {
	m := New(Config{})
	if m.verifyAgentFn == nil || m.protectKeyFn == nil || m.registerTaskFn == nil ||
		m.prepareEngineFn == nil ||
		m.removeTaskFn == nil || m.enrollFn == nil || m.startAgentFn == nil ||
		m.waitForConnectedFn == nil || m.unlinkOnlineFn == nil ||
		m.removeAllFn == nil || m.writeTombstoneFn == nil {
		t.Error("New left one or more seams unwired")
	}
}

// The BYOD consent wait is opt-in, and the opt-in signal that reaches
// enroll is the callback itself: a Manager with an AwaitingApproval hook
// hands enrollLight a non-nil onPending, and invoking it surfaces a
// human-readable label to whatever UI the caller attached.
func TestManager_Install_AwaitingApprovalReachesEnroll(t *testing.T) {
	m, _ := newTestManager()
	var labels []string
	m.AwaitingApproval = func(label string) { labels = append(labels, label) }

	var onPending func()
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload, cb func()) error {
		onPending = cb
		return nil
	}
	if err := m.Install(context.Background(), Payload{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if onPending == nil {
		t.Fatal("enroll got a nil onPending despite AwaitingApproval being set")
	}
	onPending()
	if len(labels) != 1 {
		t.Fatalf("AwaitingApproval called %d times, want 1", len(labels))
	}
	if labels[0] == "" {
		t.Error("AwaitingApproval label is empty — the mentor would see a blank wait screen")
	}
	if !strings.Contains(labels[0], "ReadyApp") {
		t.Errorf("label %q should name where the mentor has to go to confirm", labels[0])
	}
}

// Without an AwaitingApproval hook there is no UI to wait in front of, so
// enroll must receive a nil onPending and fail fast on a deferred
// redemption rather than block. This is the property that keeps the
// sibling-files/CLI and test paths from silently hanging.
func TestManager_Install_WithoutAwaitingApprovalPassesNilOnPending(t *testing.T) {
	m, _ := newTestManager()

	gotNonNil := false
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload, cb func()) error {
		gotNonNil = cb != nil
		return nil
	}
	if err := m.Install(context.Background(), Payload{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if gotNonNil {
		t.Fatal("enroll got a non-nil onPending with no AwaitingApproval hook set")
	}
}
