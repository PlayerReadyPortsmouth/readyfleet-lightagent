//go:build windows

package venue

import (
	"strings"
	"testing"
)

func TestPipeSecurityRejectsNonAccountSID(t *testing.T) {
	for _, sid := range []string{"", "S-1-1-0", "S-1-5-18", "S-1-5-32-544", "bad)(A;;GA;;;WD)"} {
		if _, err := pipeSecurity(sid); err == nil {
			t.Fatalf("accepted %q", sid)
		}
	}
	s, err := pipeSecurity("S-1-5-21-100-200-300-1001")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, ";;;WD") || !strings.Contains(s, "0x12019b") {
		t.Fatalf("unsafe pipe ACL: %s", s)
	}
}
