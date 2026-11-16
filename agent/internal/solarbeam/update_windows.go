//go:build windows

package solarbeam

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const maxEngineUpdateBytes = 200 * 1024 * 1024

var engineUpdateHTTPClient = verify.NewTransferClient()

// packageManifestName is the identity file an engine v2 BYOD package carries
// at its root (solarbeam-engine-v2 packaging/windows/solarbeam/build-byod-zip.ps1).
// Its presence in the CURRENT install tells an update that the engine's
// config directory was written by the same engine family and can be carried
// forward; an install without it predates engine v2 (the archived 0.5.x
// fork) and its config is set aside instead.
const packageManifestName = "solarbeam-byod-package.json"

// engineConfigDirName is the engine's own state directory. The engine resolves
// it next to its executable (platf::appdata on Windows), so on a per-user
// install it lives INSIDE the install directory and must survive an update.
const engineConfigDirName = "config"

// previousLayoutConfigName is where a pre-manifest install's config directory
// is kept after an update, for diagnosis. Nothing reads it.
const previousLayoutConfigName = "config.previous-layout"

// verifyEngineSignature and renameDir are seams: there is no signed fixture in
// this repository, and a failed activation cannot be provoked portably.
//
// The zip-package path is the per-user (BYOD) engine install, on personal PCs
// that do not — and must not be asked to — trust ReadyFleet's private signing
// root, so the chain may end in an untrusted root; the SHA-256 pin on the
// package and the signer-fingerprint pin carry the authority (see
// verify.VerifyAuthenticodeAllowUntrustedRoot). Managed venue PCs install
// the engine from the MSI path (exec/install_apply_windows.go), which keeps
// the strict check.
var (
	verifyEngineSignature = verify.VerifyAuthenticodeAllowUntrustedRoot
	renameDir             = os.Rename
)

// applyEngineUpdate downloads args.URL — a zip package containing the
// engine's full install directory (sunshine.exe, solarbeam-bridge.exe and
// every DLL, helper and asset they need) — verifies it against SHA256,
// extracts it into a staging directory beside the install directory,
// verifies the STAGED engine binary's Authenticode signer, and only then
// swaps the staged tree into place.
//
// Verify-before-activate matters because the extracted directory is the
// launch path: the previous implementation extracted over the live install
// and checked the signature afterwards, so a package that failed the signer
// check still left a runnable engine where the next start_solarbeam would
// launch it (observed on the build VM, 14 Sep 2026, with an unsigned engine
// v2 package). SHA-256 pinning already covers transit; Authenticode is the
// defence against a bad artefact reaching the release catalogue, and it only
// defends if it gates activation. update_linux.go has always staged.
//
// Replacing the directory wholesale (rather than extracting over it) also
// removes files the new package no longer ships — for the move from the
// archived 0.5.x fork to engine v2 that is tools\sunshinesvc.exe and a set of
// stale DLLs. The engine's config directory is carried into the new tree; see
// packageManifestName for the one exception.
//
// Deliberately has no "engine must already exist" precondition: a fresh
// install (the light agent's primary case — a personally-owned device never
// had SolarBeam) and an in-place update take the same path.
//
// Manager.Update already refused to reach this point while the engine is
// running, so nothing here is in use. A file still held open elsewhere makes
// the rename fail, and the update then fails without changing the install.
func applyEngineUpdate(ctx context.Context, paths Paths, args proto.SolarbeamEngineUpdateArgs) error {
	installDir := engineInstallDir(paths)
	if installDir == "" {
		return fmt.Errorf("engine path not configured")
	}
	engineRel, err := filepath.Rel(installDir, paths.Engine)
	if err != nil || engineRel == "." || strings.HasPrefix(engineRel, "..") || filepath.IsAbs(engineRel) {
		return fmt.Errorf("engine binary %q is not inside the install dir %q", paths.Engine, installDir)
	}

	downloadedPath, err := verify.DownloadVerified(ctx, verify.DownloadParams{
		URL:         args.URL,
		SHA256:      args.SHA256,
		TempPattern: "solarbeam-engine-update-*.zip",
		MaxBytes:    maxEngineUpdateBytes,
		Client:      engineUpdateHTTPClient,
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(downloadedPath) }()

	parent := filepath.Dir(installDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create install parent dir: %w", err)
	}
	// Beside the install directory so both renames stay on one volume.
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(installDir)+"-update-")
	if err != nil {
		return fmt.Errorf("create engine staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := extractZip(downloadedPath, stage); err != nil {
		return fmt.Errorf("extract engine package: %w", err)
	}
	if _, err := os.Stat(filepath.Join(stage, engineConfigDirName)); err == nil {
		return fmt.Errorf("engine package must not contain a %s directory (it would replace the engine's state)", engineConfigDirName)
	}
	stagedEngine := filepath.Join(stage, engineRel)
	if _, err := os.Stat(stagedEngine); err != nil {
		return fmt.Errorf("engine package has no %s: %w", engineRel, err)
	}
	if err := verifyEngineSignature(stagedEngine, args.SignerFingerprint); err != nil {
		return fmt.Errorf("authenticode: %w", err)
	}
	return activateStagedEngine(stage, installDir)
}

// activateStagedEngine swaps a verified staged tree into installDir, carrying
// the engine's config directory across. On any failure the previous install
// is put back as it was.
func activateStagedEngine(stage, installDir string) error {
	backup := installDir + ".previous"
	_ = os.RemoveAll(backup) // a leftover from an interrupted earlier update
	hadInstall := true
	if err := renameDir(installDir, backup); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("move current engine aside (is a SolarBeam process still running?): %w", err)
		}
		hadInstall = false
	}

	restore := func() {
		if hadInstall {
			_ = renameDir(backup, installDir)
		}
	}

	var movedConfigFrom, movedConfigTo string
	if hadInstall {
		oldConfig := filepath.Join(backup, engineConfigDirName)
		if info, err := os.Stat(oldConfig); err == nil && info.IsDir() {
			dest := filepath.Join(stage, engineConfigDirName)
			if _, err := os.Stat(filepath.Join(backup, packageManifestName)); err != nil {
				// The current install predates the package manifest (the
				// archived 0.5.x fork): keep its state for diagnosis, but let
				// engine v2 start from a clean config.
				dest = filepath.Join(stage, previousLayoutConfigName)
			}
			if err := renameDir(oldConfig, dest); err != nil {
				restore()
				return fmt.Errorf("carry engine config into the update: %w", err)
			}
			movedConfigFrom, movedConfigTo = oldConfig, dest
		}
	}

	if err := renameDir(stage, installDir); err != nil {
		if movedConfigTo != "" {
			_ = renameDir(movedConfigTo, movedConfigFrom)
		}
		restore()
		return fmt.Errorf("activate engine update: %w", err)
	}
	_ = os.RemoveAll(backup)
	return nil
}

// engineInstallDir resolves where the package should be extracted:
// paths.Root if the caller set it, else the engine binary's own parent
// directory — every light agent config built so far only sets
// Engine/Bridge, not Root, so this is the path actually exercised today.
func engineInstallDir(paths Paths) string {
	if paths.Root != "" {
		return paths.Root
	}
	if paths.Engine == "" {
		return ""
	}
	return filepath.Dir(paths.Engine)
}

// extractZip unpacks src into destDir, refusing any entry whose resolved
// path would land outside destDir ("zip slip"). The archive is already
// integrity-verified (SHA256 + the post-extract Authenticode check on the
// engine exe), but that proves the bytes match what ReadyApp published, not
// that a compromised release pipeline couldn't ship a malicious path — this
// is an independent guard against writing outside the intended directory
// regardless of how the archive was produced.
func extractZip(src, destDir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	for _, f := range r.File {
		entryPath := filepath.Join(destAbs, f.Name)
		if entryPath != destAbs && !strings.HasPrefix(entryPath, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry %q escapes install dir", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(entryPath, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, entryPath); err != nil {
			return fmt.Errorf("extract %s: %w", f.Name, err)
		}
	}
	return nil
}

// extractZipFile writes one archive entry to a sibling temp file and
// renames it into place, so a crash or power loss mid-extract never leaves
// a partially-written binary sitting at destPath for the next Start() to
// try to launch.
func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	tmp := destPath + ".tmp"
	mode := f.Mode()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
