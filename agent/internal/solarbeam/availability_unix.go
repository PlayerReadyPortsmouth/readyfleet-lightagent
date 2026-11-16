//go:build !windows

package solarbeam

import (
	"os"
	"path/filepath"
)

func replaceHostingState(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
