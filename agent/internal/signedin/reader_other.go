//go:build !windows && !linux

package signedin

import "time"

// Read reports nobody on platforms the agent does not target.
func Read() (string, time.Time) { return "", time.Time{} }
