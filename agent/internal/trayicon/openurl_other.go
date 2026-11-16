//go:build !windows && !linux && !darwin

package trayicon

// OpenURL has no browser to hand a URL to on a platform the agent does not
// ship for. A silent no-op is correct here and only here: nothing on such a
// platform ever calls it with a URL a person is waiting on.
func OpenURL(rawURL string) error { return nil }
