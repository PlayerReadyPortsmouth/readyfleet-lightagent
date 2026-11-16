//go:build windows

package enroll

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRecoveryCheckpointCurrentUserOnlyDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), checkpointName)
	if err := writeCheckpoint(path, []byte("private recovery material")); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.String() != expected.String() {
		t.Fatalf("checkpoint must have exactly the current user's protected grant: %s", descriptor.String())
	}
}
