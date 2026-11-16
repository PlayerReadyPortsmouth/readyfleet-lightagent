//go:build windows

package solarbeam

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLinkCredentialDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name string
		sddl string
		want bool
	}{
		{"created descriptor", windowsLinkSecretSDDL, true},
		{"system owner", "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)", true},
		{"unprotected", "O:BAD:(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"public read", "O:BAD:P(A;;FA;;;SY)(A;;GR;;;WD)", false},
		{"ordinary owner", "O:BUD:P(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"empty ACL", "O:BAD:P", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if got := windowsLinkProtectedDescriptor(sd); got != tc.want {
				t.Fatalf("protected descriptor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowsLinkCredentialRejectsNonlocalPaths(t *testing.T) {
	for _, path := range []string{`relative\secret`, `\\server\share\secret`, `C:\secret:stream`, `\\?\C:\secret`} {
		if handles, err := pinWindowsLinkDirs(path, false); err == nil {
			closeWindowsLinkDirs(handles)
			t.Fatal("accepted a nonlocal or alternate-stream credential path")
		}
	}
}

// Run on the native Windows build VM as SYSTEM or an elevated administrator.
// Cross-compilation cannot establish NTFS sharing, ownership, or DACL behaviour.
func requireWindowsLinkCredentialPrivileges(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("native protected-file checks require SYSTEM or an elevated administrator")
	}
	redirectBridgeEnv(t)
}

func TestWindowsLinkCredentialRotationAndOwnedCleanup(t *testing.T) {
	requireWindowsLinkCredentialPrivileges(t)
	first := []byte(strings.Repeat("1", 64))
	second := []byte(strings.Repeat("2", 64))
	path, cleanupFirst, err := writeWindowsLinkSecret(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupFirst() })
	path2, cleanupSecond, err := writeWindowsLinkSecret(second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupSecond() })
	if path2 != path {
		t.Fatal("rotation changed the warm engine's stable path")
	}
	if err := cleanupFirst(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(second) {
		t.Fatal("old launch cleanup removed the current credential")
	}
	h, err := openWindowsLinkFile(path, windows.GENERIC_READ)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateWindowsLinkFile(h)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatal("rotated file no longer satisfies the reader's ACL/ownership contract")
	}
	if err := cleanupSecond(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("current credential survived cleanup")
	}
}

func TestWindowsLinkCredentialRefusesPublicParent(t *testing.T) {
	requireWindowsLinkCredentialPrivileges(t)
	if err := os.Mkdir(filepath.Join(bridgeEnvDir, "link-auth"), 0700); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := writeWindowsLinkSecret([]byte(strings.Repeat("3", 64)))
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("accepted inherited/public parent DACL")
	}
	if _, err := os.Stat(windowsLinkSecretPath()); !os.IsNotExist(err) {
		t.Fatal("secret was written into an unsafe directory")
	}
}

func TestWindowsLinkCredentialRefusesHardLink(t *testing.T) {
	requireWindowsLinkCredentialPrivileges(t)
	path, cleanup, err := writeWindowsLinkSecret([]byte(strings.Repeat("4", 64)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	alias := filepath.Join(filepath.Dir(path), "alias")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	if _, _, err := writeWindowsLinkSecret([]byte(strings.Repeat("5", 64))); err == nil {
		t.Fatal("replaced a hard-linked credential")
	}
	if err := cleanup(); err == nil {
		t.Fatal("deleted a hard-linked credential")
	}
}

func TestWindowsLinkCredentialRefusesReparseParent(t *testing.T) {
	requireWindowsLinkCredentialPrivileges(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(bridgeEnvDir, "link-auth")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeWindowsLinkSecret([]byte(strings.Repeat("6", 64))); err == nil {
		t.Fatal("accepted reparse parent")
	}
}
