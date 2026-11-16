//go:build linux

package lightinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectKeyACLUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ProtectKeyACL(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestSystemdEscapeHandlesSpaces(t *testing.T) {
	if got, want := systemdEscape("/home/test/Ready Fleet/agent"), `/home/test/Ready\x20Fleet/agent`; got != want {
		t.Fatalf("systemdEscape = %q, want %q", got, want)
	}
}
