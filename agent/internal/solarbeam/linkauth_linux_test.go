//go:build linux

package solarbeam

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// /tmp is deliberately untrusted by both runtimes. Use private home ancestry.
func linkRuntimeFixture(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".fleet-link-auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestLinuxLinkCredentialPermissionsRotationAndOwnershipCleanup(t *testing.T) {
	dir := linkRuntimeFixture(t)
	first := []byte(strings.Repeat("1", 64))
	second := []byte(strings.Repeat("2", 64))
	path, oldCleanup, err := writeLinuxLinkSecret(dir, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o600}, {filepath.Dir(path), 0o700}} {
		st, err := os.Stat(check.path)
		if err != nil || st.Mode().Perm() != check.mode {
			t.Fatalf("wrong protected permissions: %v", err)
		}
	}
	samePath, newCleanup, err := writeLinuxLinkSecret(dir, second)
	if err != nil || samePath != path {
		t.Fatal("warm reconnect path changed", err)
	}
	if err := oldCleanup(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(second) {
		t.Fatal("old cleanup deleted rotated credential", err)
	}
	if err := newCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stopped credential remains")
	}
}

func TestLinuxLinkCredentialRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"parent-mode", "symlink-directory", "symlink-file", "file-mode", "hardlink", "fifo", "runtime-symlink"} {
		t.Run(kind, func(t *testing.T) {
			runtime := linkRuntimeFixture(t)
			parent := filepath.Join(runtime, "solarbeam-link-auth")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "secret")
			switch kind {
			case "parent-mode":
				os.Chmod(parent, 0o755)
			case "symlink-directory":
				os.Remove(parent)
				os.Symlink(runtime, parent)
			case "symlink-file":
				os.Symlink(filepath.Join(runtime, "target"), path)
			case "file-mode":
				os.WriteFile(path, []byte("fixture"), 0o644)
			case "hardlink":
				os.WriteFile(path, []byte("fixture"), 0o600)
				os.Link(path, filepath.Join(parent, "second"))
			case "fifo":
				unix.Mkfifo(path, 0o600)
			case "runtime-symlink":
				alias := filepath.Join(runtime, "alias")
				os.Symlink(runtime, alias)
				runtime = alias
			}
			if _, _, err := writeLinuxLinkSecret(runtime, []byte(strings.Repeat("1", 64))); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
	if _, _, err := writeLinuxLinkSecret(t.TempDir(), []byte(strings.Repeat("1", 64))); err == nil {
		t.Fatal("shared /tmp ancestry accepted")
	}
}

func TestLinuxProvisionEnvironmentAndRecoveredCleanup(t *testing.T) {
	runtime := linkRuntimeFixture(t)
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	m := New(Paths{Mode: ModeInteractive})
	supportLinkAuthForTest(m)
	if err := m.provisionPlatformLinkAuth(); err != nil {
		t.Fatal(err)
	}
	env := m.linkAuthEnvironment(nil)
	if len(env) != 2 || !strings.HasPrefix(env[0], "SOLARBEAM_LINK_SECRET_FILE=") || env[1] != "SOLARBEAM_LINK_AUTH_MODE=warn" {
		t.Fatal("wrong child configuration")
	}
	path := strings.TrimPrefix(env[0], "SOLARBEAM_LINK_SECRET_FILE=")
	// An agent restart adopts the recorded session, then cleans it after stop.
	adopted := New(Paths{Mode: ModeInteractive})
	if err := adopted.cleanupStoppedLinkAuth(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("recovered session left credential")
	}
}

func TestLinuxLinkCapabilityProbeUsesOnlyBundledLibraries(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/untrusted")
	t.Setenv("LD_PRELOAD", "/untrusted/inject.so")
	cmd := linkCapabilityCommand(context.Background(), "/bundle/sunshine")
	if !slices.Contains(cmd.Env, "LD_LIBRARY_PATH=/bundle/lib") {
		t.Fatal("capability probe cannot load the installed engine's bundled libraries")
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "LD_PRELOAD=") || strings.Contains(entry, "/untrusted") {
			t.Fatal("probe inherited library overrides")
		}
	}
}
