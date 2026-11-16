package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An existing agent at the destination is replaced, contents and mode.
// (The first-install path — no file there at all — is already covered by
// TestWriteAgentBinary_CreatesDirAndWritesBytes in main_test.go.)
func TestWriteAgentBinary_ReplacesAnExistingAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readyfleet-light")
	if err := os.WriteFile(path, []byte("old agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentBinary(path, []byte("new agent")); err != nil {
		t.Fatalf("writeAgentBinary: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new agent" {
		t.Fatalf("contents = %q, want the new agent", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("mode = %v, want executable", info.Mode())
	}
}

// A finished install must not leave staging files beside the agent, where the
// next reader of that directory would have to guess what they are.
func TestWriteAgentBinary_LeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeAgentBinary(filepath.Join(dir, "readyfleet-light"), []byte("agent")); err != nil {
		t.Fatalf("writeAgentBinary: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "readyfleet-light" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("install directory contains %v, want only the agent", names)
	}
}
