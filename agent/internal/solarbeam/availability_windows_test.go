//go:build windows

package solarbeam

import "testing"

func TestConfiguredWindowsPathBoundary(t *testing.T) {
	const configured = `C:\Program Files\SolarBeam\sunshine.exe`
	for _, tt := range []struct {
		actual string
		want   bool
	}{
		{configured, true},
		{`c:\PROGRAM FILES\SolarBeam\SUNSHINE.EXE`, true},
		{`C:\Program Files\SolarBeam\tools\..\sunshine.exe`, true},
		{`D:\Program Files\SolarBeam\sunshine.exe`, false},
		{`C:\Users\other\SolarBeam\sunshine.exe`, false},
		{`C:\Program Files\SolarBeam-other\sunshine.exe`, false},
		{`sunshine.exe`, false},
	} {
		if got := sameConfiguredPath(tt.actual, configured); got != tt.want {
			t.Errorf("%q matched=%v", tt.actual, got)
		}
	}
}
