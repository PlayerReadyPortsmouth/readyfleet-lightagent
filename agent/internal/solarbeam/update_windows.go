//go:build windows

package solarbeam

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	maxEngineUpdateBytes = 200 * 1024 * 1024
	engineUpdateTimeout  = 5 * time.Minute
)

var engineUpdateHTTPClient = &http.Client{Timeout: engineUpdateTimeout}

// applyEngineUpdate downloads args.URL — a zip package containing the
// engine's full install directory (sunshine.exe plus every DLL/asset it
// needs, not just the one exe) — verifies it against SHA256, extracts it
// over the engine's install directory, then verifies the resulting engine
// binary's Authenticode signer as defence in depth (the zip container
// itself carries no Authenticode signature; checking the extracted exe
// gives the same signer-fingerprint assurance the old single-exe download
// had).
//
// Deliberately has no "engine must already exist" precondition: extracting
// the whole package unconditionally handles a fresh install (the light
// agent's primary case — a personally-owned device never had SolarBeam
// preinstalled, so the directory doesn't exist yet) the exact same way it
// handles an in-place update (files already exist, get overwritten) — one
// code path for both halves of "auto-install and keep it updated", and it
// guarantees supporting DLLs never drift out of sync with the engine exe's
// own version the way a swap-just-the-exe approach could silently allow.
//
// Manager.Update already refused to reach this point while the engine is
// running, so nothing here is in use.
func applyEngineUpdate(ctx context.Context, paths Paths, args proto.SolarbeamEngineUpdateArgs) error {
	installDir := engineInstallDir(paths)
	if installDir == "" {
		return fmt.Errorf("engine path not configured")
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

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}
	if err := extractZip(downloadedPath, installDir); err != nil {
		return fmt.Errorf("extract engine package: %w", err)
	}

	if err := verify.VerifyAuthenticode(paths.Engine, args.SignerFingerprint); err != nil {
		return fmt.Errorf("authenticode: %w", err)
	}
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
