//go:build windows

package lightinstall

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProtectRestoredKeyReplacesInheritedAndExplicitGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(path, []byte("test private key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectKeyACL(path); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + u.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.String() != expected.String() {
		t.Fatal("key DACL is not current-user-only")
	}
}
