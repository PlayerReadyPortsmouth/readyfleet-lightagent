//go:build !windows

package sessionindicator

// NewPrivacy reports that privacy mode is Windows-only (managed venue PCs).
func NewPrivacy() (Privacy, error) { return nil, ErrUnsupported }
