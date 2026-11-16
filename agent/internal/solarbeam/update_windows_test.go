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

// serveZip builds a zip from files and serves it, returning the update args
// that point at it.
func serveZip(t *testing.T, files map[string]string) proto.SolarbeamEngineUpdateArgs {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "src.zip")
	buildTestZip(t, zipPath, files)
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	t.Cleanup(srv.Close)
	return proto.SolarbeamEngineUpdateArgs{
		ReleaseID:         "rel-1",
		Version:           "2.1.7",
		URL:               srv.URL,
		SHA256:            hex.EncodeToString(sum[:]),
		SignerFingerprint: strings.Repeat("a", 64),
		ProductID:         solarbeamEngineProductID,
	}
}

// acceptSignatures swaps the Authenticode seam for one that accepts every
// binary and records what it was asked to verify.
func acceptSignatures(t *testing.T) *[]string {
	t.Helper()
	var checked []string
	old := verifyEngineSignature
	verifyEngineSignature = func(path, _ string) error {
		checked = append(checked, path)
		return nil
	}
	t.Cleanup(func() { verifyEngineSignature = old })
	return &checked
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should not exist (stat err = %v)", path, err)
	}
}

func assertNoStagingLeft(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "-update-") || strings.HasSuffix(e.Name(), ".previous") {
			t.Fatalf("update left %s behind in %s", e.Name(), parent)
		}
	}
}

// An unsigned engine must never reach the launch path: a fresh install is not
// created at all. Before verify-before-activate the files were extracted into
// the install directory first, and the next session launched them.
func TestApplyEngineUpdate_FreshInstall_UnsignedEngineIsNeverInstalled(t *testing.T) {
	dir := t.TempDir()
	args := serveZip(t, map[string]string{
		"sunshine.exe": "not-a-real-signed-binary",
		"libs/foo.dll": "dll-bytes",
	})
	installDir := filepath.Join(dir, "does-not-exist-yet", "SolarBeam")
	enginePath := filepath.Join(installDir, "sunshine.exe")

	err := applyEngineUpdate(context.Background(), Paths{Engine: enginePath, Root: installDir}, args)
	if err == nil || !strings.Contains(err.Error(), "authenticode") {
		t.Fatalf("err = %v, want an authenticode failure", err)
	}
	assertAbsent(t, installDir)
	assertNoStagingLeft(t, filepath.Dir(installDir))
}

// Same for an update: the current install is left exactly as it was.
func TestApplyEngineUpdate_UnsignedUpdateLeavesCurrentInstallUntouched(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	writeFiles(t, installDir, map[string]string{
		"sunshine.exe":                "old-signed-engine",
		packageManifestName:           "{}",
		"config/vibeshine_state.json": "state",
	})
	args := serveZip(t, map[string]string{"sunshine.exe": "unsigned-new-engine"})

	err := applyEngineUpdate(context.Background(), Paths{Engine: filepath.Join(installDir, "sunshine.exe"), Root: installDir}, args)
	if err == nil || !strings.Contains(err.Error(), "authenticode") {
		t.Fatalf("err = %v, want an authenticode failure", err)
	}
	if got := readFile(t, filepath.Join(installDir, "sunshine.exe")); got != "old-signed-engine" {
		t.Fatalf("engine replaced by an unverified package: %q", got)
	}
	if got := readFile(t, filepath.Join(installDir, "config", "vibeshine_state.json")); got != "state" {
		t.Fatalf("config changed: %q", got)
	}
	assertNoStagingLeft(t, filepath.Dir(installDir))
}

// The staged binary is the one verified, never the live one.
func TestApplyEngineUpdate_VerifiesTheStagedEngine(t *testing.T) {
	checked := acceptSignatures(t)
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	enginePath := filepath.Join(installDir, "sunshine.exe")
	args := serveZip(t, map[string]string{"sunshine.exe": "new", packageManifestName: "{}"})

	if err := applyEngineUpdate(context.Background(), Paths{Engine: enginePath, Root: installDir}, args); err != nil {
		t.Fatalf("applyEngineUpdate: %v", err)
	}
	if len(*checked) != 1 || filepath.Base((*checked)[0]) != "sunshine.exe" || filepath.Dir((*checked)[0]) == installDir {
		t.Fatalf("verified %v, want exactly the staged sunshine.exe", *checked)
	}
	if got := readFile(t, enginePath); got != "new" {
		t.Fatalf("engine = %q after update", got)
	}
	assertNoStagingLeft(t, filepath.Dir(installDir))
}

// Moving from the archived 0.5.x fork to engine v2: files the new package
// does not ship (the service host, stale DLLs) are gone, and the old config
// is kept aside rather than handed to the new engine.
func TestApplyEngineUpdate_FromPreManifestInstall_ReplacesTreeAndSetsOldConfigAside(t *testing.T) {
	acceptSignatures(t)
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	writeFiles(t, installDir, map[string]string{
		"sunshine.exe":               "engine-0.5.1",
		"solarbeam-bridge.exe":       "bridge-0.5.1",
		"tools/sunshinesvc.exe":      "service-host",
		"libsodium-26.dll":           "stale",
		"config/sunshine_state.json": "old-state",
	})
	args := serveZip(t, map[string]string{
		"sunshine.exe":                      "engine-v2",
		"solarbeam-bridge.exe":              "bridge-v2",
		"tools/sunshine_wgc_capture.exe":    "wgc",
		"tools/sunshine_display_helper.exe": "display",
		packageManifestName:                 "{}",
	})

	if err := applyEngineUpdate(context.Background(), Paths{Engine: filepath.Join(installDir, "sunshine.exe"), Root: installDir}, args); err != nil {
		t.Fatalf("applyEngineUpdate: %v", err)
	}
	if got := readFile(t, filepath.Join(installDir, "sunshine.exe")); got != "engine-v2" {
		t.Fatalf("engine = %q", got)
	}
	assertAbsent(t, filepath.Join(installDir, "tools", "sunshinesvc.exe"))
	assertAbsent(t, filepath.Join(installDir, "libsodium-26.dll"))
	assertAbsent(t, filepath.Join(installDir, "config"))
	if got := readFile(t, filepath.Join(installDir, previousLayoutConfigName, "sunshine_state.json")); got != "old-state" {
		t.Fatalf("old config not kept aside: %q", got)
	}
	assertNoStagingLeft(t, filepath.Dir(installDir))
}

// Between engine v2 packages the engine's state carries across.
func TestApplyEngineUpdate_BetweenManifestPackages_CarriesConfig(t *testing.T) {
	acceptSignatures(t)
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	writeFiles(t, installDir, map[string]string{
		"sunshine.exe":                "engine-2.1.7",
		packageManifestName:           "{}",
		"config/vibeshine_state.json": "state",
		"config/logs/sunshine.log":    "log",
	})
	args := serveZip(t, map[string]string{"sunshine.exe": "engine-2.1.8", packageManifestName: "{}"})

	if err := applyEngineUpdate(context.Background(), Paths{Engine: filepath.Join(installDir, "sunshine.exe"), Root: installDir}, args); err != nil {
		t.Fatalf("applyEngineUpdate: %v", err)
	}
	if got := readFile(t, filepath.Join(installDir, "config", "vibeshine_state.json")); got != "state" {
		t.Fatalf("config not carried: %q", got)
	}
	if got := readFile(t, filepath.Join(installDir, "config", "logs", "sunshine.log")); got != "log" {
		t.Fatalf("config subtree not carried: %q", got)
	}
	assertAbsent(t, filepath.Join(installDir, previousLayoutConfigName))
}

// A package that ships config/ would overwrite the person's engine state on
// every update, so it is refused before anything moves.
func TestApplyEngineUpdate_RefusesPackageWithConfigDir(t *testing.T) {
	acceptSignatures(t)
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	writeFiles(t, installDir, map[string]string{"sunshine.exe": "old", packageManifestName: "{}"})
	args := serveZip(t, map[string]string{"sunshine.exe": "new", "config/sunshine.conf": "x"})

	err := applyEngineUpdate(context.Background(), Paths{Engine: filepath.Join(installDir, "sunshine.exe"), Root: installDir}, args)
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got := readFile(t, filepath.Join(installDir, "sunshine.exe")); got != "old" {
		t.Fatalf("engine = %q", got)
	}
}

// If the final swap fails, the previous install and its config are restored.
func TestApplyEngineUpdate_FailedActivationRestoresPreviousInstall(t *testing.T) {
	acceptSignatures(t)
	installDir := filepath.Join(t.TempDir(), "SolarBeam")
	writeFiles(t, installDir, map[string]string{
		"sunshine.exe":                "old",
		packageManifestName:           "{}",
		"config/vibeshine_state.json": "state",
	})
	args := serveZip(t, map[string]string{"sunshine.exe": "new", packageManifestName: "{}"})
	old := renameDir
	renameDir = func(from, to string) error {
		if strings.Contains(filepath.Base(from), "-update-") && to == installDir {
			return os.ErrPermission
		}
		return old(from, to)
	}
	t.Cleanup(func() { renameDir = old })

	err := applyEngineUpdate(context.Background(), Paths{Engine: filepath.Join(installDir, "sunshine.exe"), Root: installDir}, args)
	if err == nil || !strings.Contains(err.Error(), "activate engine update") {
		t.Fatalf("err = %v, want an activation failure", err)
	}
	if got := readFile(t, filepath.Join(installDir, "sunshine.exe")); got != "old" {
		t.Fatalf("engine = %q, want the previous install restored", got)
	}
	if got := readFile(t, filepath.Join(installDir, "config", "vibeshine_state.json")); got != "state" {
		t.Fatalf("config = %q, want it restored", got)
	}
	assertNoStagingLeft(t, filepath.Dir(installDir))
}

func TestApplyEngineUpdate_RefusesEngineOutsideInstallDir(t *testing.T) {
	err := applyEngineUpdate(context.Background(), Paths{
		Engine: `C:\elsewhere\sunshine.exe`, Root: `C:\Users\mentor\AppData\Local\SolarBeam`,
	}, proto.SolarbeamEngineUpdateArgs{})
	if err == nil || !strings.Contains(err.Error(), "not inside the install dir") {
		t.Fatalf("err = %v", err)
	}
}
