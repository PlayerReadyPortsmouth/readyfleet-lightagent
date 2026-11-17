//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRestoreLightUpdateReturnsCandidateToStaging(t *testing.T) {
	dir := t.TempDir()
	exe, previous, staged := filepath.Join(dir, "lightagent.exe"), filepath.Join(dir, "lightagent.exe.old"), filepath.Join(dir, "download.exe")
	if err := os.WriteFile(exe, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("enrolled agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreLightUpdate(exe, previous, staged); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{exe: "enrolled agent", staged: "replacement"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s=%q err=%v want=%q", path, got, err, want)
		}
	}
}

func TestRestoreLightUpdateKeepsReplacementWhenPreviousMissing(t *testing.T) {
	dir := t.TempDir()
	exe, previous, staged := filepath.Join(dir, "lightagent.exe"), filepath.Join(dir, "missing.old"), filepath.Join(dir, "download.exe")
	if err := os.WriteFile(exe, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreLightUpdate(exe, previous, staged); err == nil {
		t.Fatal("missing backup must fail")
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement lost: %q %v", got, err)
	}
}

func TestLightUpdateLeaseExcludesOverlappingStartup(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "lightagent.exe")
	first, err := acquireLightUpdateLease(exe)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireLightUpdateLease(exe)
	if err == nil {
		windows.CloseHandle(second)
		windows.CloseHandle(first)
		t.Fatal("overlapping startup obtained the lease")
	}
	windows.CloseHandle(first)
	next, err := acquireLightUpdateLease(exe)
	if err != nil {
		t.Fatal("closed helper lease must permit the next update:", err)
	}
	windows.CloseHandle(next)
}
