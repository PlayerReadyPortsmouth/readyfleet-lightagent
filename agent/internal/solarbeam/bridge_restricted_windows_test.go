//go:build windows

package solarbeam

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The restricted bridge must not open the SYSTEM/Administrators-only file; it
// gets the same key in its own environment while the engine keeps the file.
// Native VM only (elevated): the file write needs real NTFS DACLs.
func TestRestrictedManagedBridgeGetsKeyInEnvironmentNotFile(t *testing.T) {
	requireWindowsLinkCredentialPrivileges(t)
	m := New(Paths{Mode: ModeManaged, LinkAuthMode: "enforce", RestrictedBridge: true})
	supportLinkAuthForTest(m)
	if err := m.provisionPlatformLinkAuth(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.cleanupLinkAuth() })
	var secret string
	for _, entry := range m.linkAuthEnvironment(nil) {
		if strings.HasPrefix(entry, "SOLARBEAM_LINK_SECRET_FILE=") {
			t.Fatal("restricted bridge was pointed at the protected file")
		}
		if v, ok := strings.CutPrefix(entry, "SOLARBEAM_LINK_SECRET="); ok {
			secret = v
		}
	}
	if len(secret) != 64 {
		t.Fatal("restricted bridge credential absent")
	}
	if got, err := os.ReadFile(windowsLinkSecretPath()); err != nil || string(got) != secret {
		t.Fatal("engine file and bridge environment hold different keys")
	}
}

// Run on the VM as SYSTEM (psexec -s): LogonUser for LocalService needs SeTcb.
func TestRestrictedBridgeTokenIsLowLocalServiceWithoutPrivileges(t *testing.T) {
	self, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !self.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Skip("restricted token construction requires LocalSystem")
	}
	token, logonSID, err := restrictedBridgeToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	if user, err := token.GetTokenUser(); err != nil || !user.User.Sid.IsWellKnown(windows.WinLocalServiceSid) {
		t.Fatal("bridge token is not LocalService")
	}
	if logonSID == nil {
		t.Fatal("no per-launch logon SID")
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&tokenInfo(t, token, windows.TokenIntegrityLevel)[0]))
	if !label.Label.Sid.IsWellKnown(windows.WinLowLabelSid) {
		t.Fatal("bridge token is not Low integrity")
	}
	var changeNotify windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeChangeNotifyPrivilege"), &changeNotify); err != nil {
		t.Fatal(err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&tokenInfo(t, token, windows.TokenPrivileges)[0]))
	for _, p := range privileges.AllPrivileges() {
		if p.Luid != changeNotify {
			t.Fatal("bridge token kept a privilege beyond SeChangeNotify")
		}
	}
}

func tokenInfo(t *testing.T, token windows.Token, class uint32) []byte {
	t.Helper()
	var n uint32
	_ = windows.GetTokenInformation(token, class, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, class, &buf[0], n, &n); err != nil {
		t.Fatal(err)
	}
	return buf
}
