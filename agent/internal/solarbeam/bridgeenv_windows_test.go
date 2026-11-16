//go:build windows

package solarbeam

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// assertSystemAdminsOnly fails unless path is owned by SYSTEM with a
// protected DACL granting only SYSTEM and Administrators.
func assertSystemAdminsOnly(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	owner, _, _ := sd.Owner()
	control, _, _ := sd.Control()
	dacl, _, _ := sd.DACL()
	if owner == nil || !owner.IsWellKnown(windows.WinLocalSystemSid) || control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s: want SYSTEM owner and protected DACL, got %s", path, sd)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			!(sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
			t.Fatalf("%s: unexpected ACE in %s", path, sd)
		}
	}
}

// A folder an older agent made by inheritance (Users may create files and
// folders in it), with a file a user planted, is repaired on the next write.
func TestWriteBridgeEnvRepairsLooseFolder(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	dir := filepath.Dir(envFile)
	planted := filepath.Join(dir, "planted")
	if err := os.Mkdir(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	loose, _ := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;BU)(A;OICI;FA;;;WD)(A;OICI;0x1301bf;;;AU)")
	dacl, _, _ := loose.DACL()
	for _, p := range []string{dir, planted} {
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(envFile, []byte("SOLARBEAM_JWKS_URL=http://evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeBridgeEnv("uuid-1", ""); err != nil {
		t.Fatalf("writeBridgeEnv: %v", err)
	}
	for _, p := range []string{dir, planted, envFile} {
		assertSystemAdminsOnly(t, p)
	}
	got, _ := os.ReadFile(envFile)
	if want := "SOLARBEAM_BRIDGE_WS=" + bridgeWSURL + "\nSOLARBEAM_MACHINE_ID=uuid-1\n"; string(got) != want {
		t.Fatalf("bridge.env = %q, want %q", got, want)
	}
}

// A junction where the folder should be is refused, and its target's
// permissions are left alone.
func TestWriteBridgeEnvRefusesJunction(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	target := t.TempDir()
	before, _ := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	junction := filepath.Join(filepath.Dir(envFile), "SolarBeam")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	bridgeEnvDir, bridgeEnvFile = junction, filepath.Join(junction, "bridge.env")

	if _, err := writeBridgeEnv("uuid-1", ""); err == nil {
		t.Fatal("writeBridgeEnv followed a junction")
	}
	after, _ := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if before.String() != after.String() {
		t.Fatalf("junction target permissions changed: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(target, "bridge.env")); !os.IsNotExist(err) {
		t.Fatalf("bridge.env written through the junction: %v", err)
	}
}
