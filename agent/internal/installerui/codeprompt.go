package installerui

import "strings"

// pairingCodeAlphabet must match server/internal/api/pairing_codes.go's
// pairingCodeAlphabet exactly. Duplicated rather than shared across the
// agent/server module boundary (these are separate Go modules in this
// repo's go.work workspace with no existing shared "codes" package) — if
// the alphabet ever changes, both copies must change together; there is
// no compiler enforcement of that today.
const pairingCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
const pairingCodeLength = 9

// NormalizePairingCode trims and uppercases raw user input and validates
// it against the exact pairing-code shape (9 characters, every character
// in pairingCodeAlphabet). Returns ("", false) for anything that doesn't
// match — callers show a "that doesn't look like a code" message rather
// than attempting redemption with malformed input.
func NormalizePairingCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != pairingCodeLength {
		return "", false
	}
	for _, c := range code {
		if !strings.ContainsRune(pairingCodeAlphabet, c) {
			return "", false
		}
	}
	return code, true
}

// promptMessage joins PromptForCode's title and instruction into the single
// body string CredUI's pszMessageText can carry, keeping the wizard's
// caption (w.Title) free for the window title exactly like every TaskDialog
// screen in this package. Either part may be empty.
func promptMessage(title, instruction string) string {
	parts := make([]string, 0, 2)
	for _, p := range []string{strings.TrimSpace(title), strings.TrimSpace(instruction)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}
