//go:build !windows

package sacposture

// systemReader is nil off Windows, which Classify reports as unknown.
func systemReader() Reader { return nil }
