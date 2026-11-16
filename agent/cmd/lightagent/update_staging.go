package main

import (
	"os"
	"path/filepath"
)

// updateStagingDir returns the directory a downloaded agent update should be
// staged in: the directory holding the running binary.
//
// The destination matters more than the directory being "temporary". An update
// is activated by renaming the downloaded file over the running binary, and
// rename(2) fails with EXDEV across filesystems — which is exactly what
// os.TempDir() is on a normal Linux desktop, where /tmp is a tmpfs and the
// agent lives under ~/.config. Staging beside the target makes the rename
// intra-filesystem, and therefore atomic.
//
// Falls back to "" (which update.Manager reads as os.TempDir(), the previous
// behaviour) when the running binary cannot be located, rather than guessing:
// a wrong directory would fail the download instead of the rename, which is
// not an improvement.
func updateStagingDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	dir := filepath.Dir(exePath)
	// Confirm it is writable before committing to it. On a machine where the
	// agent sits somewhere read-only, staging there would turn a working
	// update into a failed download.
	probe, err := os.CreateTemp(dir, ".update-probe-*")
	if err != nil {
		return ""
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return dir
}
