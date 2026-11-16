//go:build !windows && !linux && !darwin

package lightinventory

const osFamily = "unknown"

// osMajorVersion is unsupported off Windows (dev/fakeagent hosts). Returns
// a placeholder rather than erroring so Collect stays testable.
func osMajorVersion() (string, error) {
	return "0", nil
}
