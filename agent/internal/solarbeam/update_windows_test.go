//go:build windows

package solarbeam

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// buildTestZip writes a zip archive at path containing files, keyed by
// archive-internal name (using "/" regardless of host OS, matching how a
// real zip writer/reader always represents entry names) to their content.
func buildTestZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for name, content := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create entry %q: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write entry %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

func TestExtractZip_WritesFilesIncludingNestedDirs(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "pkg.zip")
	buildTestZip(t, zipPath, map[string]string{
		"sunshine.exe":       "engine-bytes",
		"libs/foo.dll":       "dll-bytes",
		"libs/nested/bar.so": "nested-bytes",
	})

	destDir := filepath.Join(dir, "install")
	if err := extractZip(zipPath, destDir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	for name, want := range map[string]string{
		"sunshine.exe":       "engine-bytes",
		"libs/foo.dll":       "dll-bytes",
		"libs/nested/bar.so": "nested-bytes",
	} {
		got, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read extracted %q: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("extracted %q = %q, want %q", name, got, want)
		}
	}
}

func TestExtractZip_OverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "install")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(destDir, "sunshine.exe")
	if err := os.WriteFile(stalePath, []byte("stale-old-version"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(dir, "pkg.zip")
	buildTestZip(t, zipPath, map[string]string{"sunshine.exe": "fresh-new-version"})

	if err := extractZip(zipPath, destDir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	got, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh-new-version" {
		t.Fatalf("existing file not overwritten: got %q", got)
	}
}

func TestExtractZip_RefusesPathTraversal(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	buildTestZip(t, zipPath, map[string]string{
		"../../escaped.exe": "evil-bytes",
	})

	destDir := filepath.Join(dir, "install")
	err := extractZip(zipPath, destDir)
	if err == nil {
		t.Fatal("expected extractZip to refuse a path-traversal entry")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "escaped.exe")); statErr == nil {
		t.Fatal("path-traversal entry was written outside destDir")
	}
}

func TestEngineInstallDir_PrefersRootOverEngineParent(t *testing.T) {
	got := engineInstallDir(Paths{
		Engine: `C:\Users\mentor\AppData\Local\ReadyFleet Light\solarbeam\sunshine.exe`,
		Root:   `C:\Users\mentor\AppData\Local\ReadyFleet Light\solarbeam-root`,
	})
	want := `C:\Users\mentor\AppData\Local\ReadyFleet Light\solarbeam-root`
	if got != want {
		t.Fatalf("engineInstallDir = %q, want %q", got, want)
	}
}

func TestEngineInstallDir_FallsBackToEngineParent(t *testing.T) {
	got := engineInstallDir(Paths{
		Engine: `C:\Users\mentor\AppData\Local\ReadyFleet Light\solarbeam\sunshine.exe`,
	})
	want := `C:\Users\mentor\AppData\Local\ReadyFleet Light\solarbeam`
	if got != want {
		t.Fatalf("engineInstallDir = %q, want %q", got, want)
	}
}

func TestEngineInstallDir_EmptyWhenEngineUnset(t *testing.T) {
	if got := engineInstallDir(Paths{}); got != "" {
		t.Fatalf("engineInstallDir = %q, want empty", got)
	}
}

// TestApplyEngineUpdate_FreshInstall_ExtractsThenFailsClosedOnUnsignedExe
// exercises the real applyEngineUpdate end-to-end (download → sha256 verify
// → mkdir → extract → Authenticode verify) against a genuinely nonexistent
// engine path — the light agent's normal starting state — proving the
// "auto-install" case actually creates the directory and writes the files
// rather than erroring out on the old "engine must already exist"
// precondition. There is no signed test fixture in this repo (no other
// package tests VerifyAuthenticode against a real signature either), so
// this can't assert success; it asserts the call reaches and fails at the
// Authenticode step specifically — proving the extraction genuinely ran
// (the file is there to check) and that an unsigned engine is correctly
// rejected rather than silently accepted.
func TestApplyEngineUpdate_FreshInstall_ExtractsThenFailsClosedOnUnsignedExe(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]string{
		"sunshine.exe": "not-a-real-signed-binary",
		"libs/foo.dll": "dll-bytes",
	}
	zipPath := filepath.Join(dir, "src.zip")
	buildTestZip(t, zipPath, payload)
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	installDir := filepath.Join(dir, "does-not-exist-yet", "solarbeam")
	enginePath := filepath.Join(installDir, "sunshine.exe")
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: installDir must not already exist, stat err = %v", err)
	}

	err = applyEngineUpdate(context.Background(), Paths{Engine: enginePath}, proto.SolarbeamEngineUpdateArgs{
		ReleaseID:         "rel-1",
		Version:           "2.0.6",
		URL:               srv.URL,
		SHA256:            hex.EncodeToString(sum[:]),
		SignerFingerprint: strings.Repeat("a", 64),
		ProductID:         solarbeamEngineProductID,
	})
	if err == nil || !strings.Contains(err.Error(), "authenticode") {
		t.Fatalf("err = %v, want an authenticode failure (proves extraction ran and the unsigned binary was correctly rejected)", err)
	}

	got, readErr := os.ReadFile(enginePath)
	if readErr != nil {
		t.Fatalf("engine exe was not extracted to disk: %v", readErr)
	}
	if string(got) != payload["sunshine.exe"] {
		t.Fatalf("extracted engine content = %q, want %q", got, payload["sunshine.exe"])
	}
	if _, err := os.Stat(filepath.Join(installDir, "libs", "foo.dll")); err != nil {
		t.Fatalf("supporting dll was not extracted: %v", err)
	}
}
