package installerui

import "testing"

func TestNormalizePairingCode(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"valid uppercase", "XK7Q9MBRT", "XK7Q9MBRT", true},
		{"valid lowercase gets uppercased", "xk7q9mbrt", "XK7Q9MBRT", true},
		{"surrounding whitespace trimmed", "  XK7Q9MBRT  ", "XK7Q9MBRT", true},
		{"too short", "XK7Q9MBR", "", false},
		{"too long", "XK7Q9MBRTX", "", false},
		{"contains excluded I", "XKIQ9MBRT", "", false},
		{"contains excluded O", "XKOQ9MBRT", "", false},
		{"contains excluded digit 1", "XK1Q9MBRT", "", false},
		{"contains excluded digit 0", "XK0Q9MBRT", "", false},
		{"empty", "", "", false},
		{"contains a space in the middle", "XK7Q 9MBR", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizePairingCode(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("NormalizePairingCode(%q): ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("NormalizePairingCode(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPromptMessage covers the caption/body split PromptForCode relies on:
// CredUI has one message field, so the title and instruction arguments are
// joined into it and w.Title stays free to be the dialog's caption — the
// same window title every other screen in this package uses.
func TestPromptMessage(t *testing.T) {
	cases := []struct {
		name        string
		title       string
		instruction string
		want        string
	}{
		{"both parts", "Enter the venue pairing code", "Type it into the User name box.", "Enter the venue pairing code\n\nType it into the User name box."},
		{"instruction only", "", "Type it into the User name box.", "Type it into the User name box."},
		{"title only", "Enter the venue pairing code", "", "Enter the venue pairing code"},
		{"neither", "  ", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptMessage(tc.title, tc.instruction); got != tc.want {
				t.Fatalf("promptMessage(%q, %q) = %q, want %q", tc.title, tc.instruction, got, tc.want)
			}
		})
	}
}
