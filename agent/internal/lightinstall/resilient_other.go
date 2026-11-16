//go:build !windows && !linux && !darwin

package lightinstall

func platformPreflight(string) error            { return errUnsupportedPlatform }
func stopAgent(string) error                    { return errUnsupportedPlatform }
func acquireInstallLock(string) (func(), error) { return nil, errUnsupportedPlatform }
func snapshotStartup() (func() error, error)    { return nil, errUnsupportedPlatform }
func reportOS() string                          { return "unsupported" }
