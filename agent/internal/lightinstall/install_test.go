package lightinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload) error {
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
		"startAgent:agent.exe", "waitForConnected:lightagent.log",
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
		"verify:agent.exe", "enroll", "protectKey:client.key", "registerTask:agent.exe", "startAgent:agent.exe",
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
		"startAgent:agent.exe", "waitForConnected:lightagent.log",
	})
}

func TestManager_Install_ReportsProgress(t *testing.T) {
	m, _ := newTestManager()
	var steps []string
	m.Progress = func(step string) { steps = append(steps, step) }

	if err := m.Install(context.Background(), Payload{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(steps) != 6 {
		t.Fatalf("Progress called %d times, want 6: %v", len(steps), steps)
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
	m.enrollFn = func(ctx context.Context, cfg Config, payload Payload) error {
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
		m.removeTaskFn == nil || m.enrollFn == nil || m.startAgentFn == nil ||
		m.waitForConnectedFn == nil || m.unlinkOnlineFn == nil ||
		m.removeAllFn == nil || m.writeTombstoneFn == nil {
		t.Error("New left one or more seams unwired")
	}
}
