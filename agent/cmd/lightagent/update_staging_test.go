package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The point of the whole change: whatever updateStagingDir returns must be on
// the same filesystem as the binary the update will be renamed over, and
// os.TempDir() is not — /tmp is a tmpfs on a normal Linux desktop while the
// agent lives under ~/.config, so the activation rename failed with EXDEV.
func TestUpdateStagingDirIsTheDirectoryOfTheRunningBinary(t *testing.T) {
	exePath, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable on this host")
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	want := filepath.Dir(exePath)

	got := updateStagingDir()
	if got == "" {
		// Only legitimate when that directory is genuinely not writable — the
		// documented fallback. Assert that rather than accepting the empty
		// string as a pass.
		probe, err := os.CreateTemp(want, ".update-probe-*")
		if err == nil {
			name := probe.Name()
			_ = probe.Close()
			_ = os.Remove(name)
			t.Fatalf("returned the os.TempDir() fallback even though %s is writable", want)
		}
		return
	}
	if got != want {
		t.Fatalf("updateStagingDir = %q, want %q (the running binary's directory)", got, want)
	}
}

// A staged file must be renameable onto a sibling of the binary. This is the
// operation applyLightUpdate performs, and the one that was failing.
func TestUpdateStagingDirSupportsRenameOntoTheTarget(t *testing.T) {
	dir := updateStagingDir()
	if dir == "" {
		t.Skip("staging directory not writable on this host")
	}
	staged, err := os.CreateTemp(dir, ".rename-src-*")
	if err != nil {
		t.Fatal(err)
	}
	src := staged.Name()
	_ = staged.Close()
	t.Cleanup(func() { _ = os.Remove(src) })

	dst := filepath.Join(dir, ".rename-dst-"+filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename within the staging directory failed, which is the EXDEV bug: %v", err)
	}
	_ = os.Remove(dst)
}
