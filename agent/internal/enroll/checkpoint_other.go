//go:build !windows

package enroll

import "os"

func restrictCheckpoint(path string) error { return os.Chmod(path, 0o600) }
