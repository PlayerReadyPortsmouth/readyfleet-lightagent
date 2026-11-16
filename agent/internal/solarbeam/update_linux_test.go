//go:build linux

package solarbeam

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeTarZstd(t *testing.T, entries []tar.Header) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.tar.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for i := range entries {
		h := entries[i]
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("binary")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractLinuxEngineSelectsEngineSubtree(t *testing.T) {
	src := writeTarZstd(t, []tar.Header{
		{Name: "bundle/agent/readyfleet-light", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
		{Name: "bundle/engine/sunshine", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
		{Name: "bundle/engine/solarbeam-bridge", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
	})
	dest := t.TempDir()
	if err := extractLinuxEngine(src, dest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sunshine", "solarbeam-bridge"} {
		if info, err := os.Stat(filepath.Join(dest, name)); err != nil || info.Mode()&0o111 == 0 {
			t.Fatalf("%s not executable: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "readyfleet-light")); !os.IsNotExist(err) {
		t.Fatal("agent subtree was extracted as engine content")
	}
}

func TestExtractLinuxEngineRejectsLinks(t *testing.T) {
	src := writeTarZstd(t, []tar.Header{{Name: "bundle/engine/sunshine", Linkname: "/bin/sh", Typeflag: tar.TypeSymlink}})
	if err := extractLinuxEngine(src, t.TempDir()); err == nil {
		t.Fatal("symlink entry accepted")
	}
}
