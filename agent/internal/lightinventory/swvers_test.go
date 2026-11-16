package lightinventory

import "testing"

func TestParseSwVers(t *testing.T) {
	for in, want := range map[string]string{"15.1.1\n": "macos:15", "14.7": "macos:14", " 26\n": "macos:26"} {
		got, err := parseSwVers(in)
		if err != nil || got != want {
			t.Errorf("parseSwVers(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "\n", "abc", ".5"} {
		if got, err := parseSwVers(bad); err == nil {
			t.Errorf("parseSwVers(%q) = %q, want error", bad, got)
		}
	}
}
