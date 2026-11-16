//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteAgentBinary_ReplacesTheDestinationInode is the reinstall regression.
//
// Writing straight through to the destination fails with ETXTBSY ("text file
// busy") when the destination is a binary some process is currently executing —
// and on a reinstall it always is, because the agent's own systemd user service
// is running it. The first install has no file there and succeeds, so the
// failure appears only the second time a machine is set up, reported as
// "ReadyFleet could not stage the agent binary" with nothing to suggest that
// stopping the service would fix it.
//
// Replacing the directory entry instead leaves the running inode alone; the old
// process exits at the restart the installer performs anyway.
//
// The inode is the assertion because it is the property that makes ETXTBSY
// impossible. A test that only compared the file's contents would pass just as
// happily on the implementation this replaced.
func TestWriteAgentBinary_ReplacesTheDestinationInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readyfleet-light")
	if err := os.WriteFile(path, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := inodeOf(t, path)

	if err := writeAgentBinary(path, []byte("new agent")); err != nil {
		t.Fatalf("writeAgentBinary: %v", err)
	}

	if after := inodeOf(t, path); after == before {
		t.Fatal("the destination was written through rather than replaced; a running agent would make this ETXTBSY")
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode numbers are not available here")
	}
	return uint64(st.Ino)
}
