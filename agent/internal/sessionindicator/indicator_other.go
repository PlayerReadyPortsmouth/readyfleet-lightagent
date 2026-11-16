//go:build !linux && !windows

package sessionindicator

// New reports that this platform has no indicator.
//
// Deliberately an error rather than a silent no-op Indicator: a caller
// that gets a working-looking Indicator here would believe the local
// person had been told a session was live when nothing was ever drawn,
// which is the exact failure the package exists to prevent. See the
// package doc's degradation section.
func New() (Indicator, error) {
	return nil, ErrUnsupported
}
