//go:build !windows

package signedin

// ConsoleLocked is Windows-only; elsewhere it never answers.
func ConsoleLocked() (locked bool, ok bool) { return false, false }
