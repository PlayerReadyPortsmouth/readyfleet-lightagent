package winpaths

import (
	"errors"
	"testing"
)

func TestSolarBeamDir(t *testing.T) {
	cases := []struct {
		name, programFiles, location, wantDir, wantRefused string
	}{
		{"C:, no registry key", `C:\Program Files`, "", `C:\Program Files\SolarBeam`, ""},
		{"D:, no registry key (TAUNTON-PC05)", `D:\Program Files`, "", `D:\Program Files\SolarBeam`, ""},
		{"registry location", `D:\Program Files`, `d:\program files\SolarBeam Engine\`, `d:\program files\SolarBeam Engine`, ""},
		{"user-writable location refused", `D:\Program Files`, `D:\Users\cyp\SolarBeam`, `D:\Program Files\SolarBeam`, `D:\Users\cyp\SolarBeam`},
		{"other drive's Program Files refused", `D:\Program Files`, `C:\Program Files\SolarBeam`, `D:\Program Files\SolarBeam`, `C:\Program Files\SolarBeam`},
		{"dot-dot escape refused", `D:\Program Files`, `D:\Program Files\..\Temp`, `D:\Program Files\SolarBeam`, `D:\Program Files\..\Temp`},
		{"lookalike prefix refused", `D:\Program Files`, `D:\Program FilesEvil\SolarBeam`, `D:\Program Files\SolarBeam`, `D:\Program FilesEvil\SolarBeam`},
		{"UNC refused", `D:\Program Files`, `\\nas\share\SolarBeam`, `D:\Program Files\SolarBeam`, `\\nas\share\SolarBeam`},
		{"Program Files itself refused", `D:\Program Files`, `D:\Program Files\`, `D:\Program Files\SolarBeam`, `D:\Program Files\`},
	}
	for _, c := range cases {
		dir, refused := solarBeamDir(c.programFiles, c.location, func(string) error { return nil })
		// filepath.Join uses the host separator; compare in Windows form.
		if clean(dir) != c.wantDir || refused != c.wantRefused {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, dir, refused, c.wantDir, c.wantRefused)
		}
	}
}

// A location that passes the text check is still refused when the on-disk
// check (junction, user-writable folder) fails.
func TestSolarBeamDirOnDiskCheckRefuses(t *testing.T) {
	dir, refused := solarBeamDir(`D:\Program Files`, `D:\Program Files\Launcher\SolarBeam`,
		func(string) error { return errors.New("the path goes through a link") })
	if clean(dir) != `D:\Program Files\SolarBeam` || refused != `D:\Program Files\Launcher\SolarBeam: the path goes through a link` {
		t.Fatalf("got (%q, %q)", dir, refused)
	}
}
