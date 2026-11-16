//go:build linux

package solarbeam

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const maxLinuxEngineUpdateBytes = 256 * 1024 * 1024

var linuxEngineUpdateHTTPClient = verify.NewTransferClient()

func applyEngineUpdate(ctx context.Context, paths Paths, args proto.SolarbeamEngineUpdateArgs) error {
	installDir := engineInstallDirLinux(paths)
	if installDir == "" {
		return errors.New("engine path not configured")
	}
	publicKey, ok := paths.TrustedPublicKeys[args.SignerFingerprint]
	if !ok {
		return errors.New("no pinned Ed25519 public key for signer fingerprint")
	}
	downloaded, err := verify.DownloadVerified(ctx, verify.DownloadParams{
		URL: args.URL, SHA256: args.SHA256, TempPattern: "solarbeam-linux-update-*.tar.zst",
		MaxBytes: maxLinuxEngineUpdateBytes, Client: linuxEngineUpdateHTTPClient,
	})
	if err != nil {
		return err
	}
	defer os.Remove(downloaded)
	if args.Signature == "" {
		return errors.New("Linux engine release is missing its detached signature")
	}
	if err := verify.VerifyEd25519File(downloaded, args.Signature, args.SignerFingerprint, publicKey); err != nil {
		return fmt.Errorf("verify Linux engine signature: %w", err)
	}
	parent := filepath.Dir(installDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".solarbeam-update-")
	if err != nil {
		return fmt.Errorf("create engine staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := extractLinuxEngine(downloaded, stage); err != nil {
		return fmt.Errorf("extract engine package: %w", err)
	}
	for _, binary := range []string{"sunshine", "solarbeam-bridge"} {
		path := filepath.Join(stage, binary)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("engine package has no executable %s", binary)
		}
	}
	backup := installDir + ".previous"
	_ = os.RemoveAll(backup)
	if err := os.Rename(installDir, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup current engine: %w", err)
	}
	if err := os.Rename(stage, installDir); err != nil {
		_ = os.Rename(backup, installDir)
		return fmt.Errorf("activate engine update: %w", err)
	}
	_ = os.RemoveAll(backup)
	return nil
}

func engineInstallDirLinux(paths Paths) string {
	if paths.Root != "" {
		return paths.Root
	}
	if paths.Engine == "" {
		return ""
	}
	return filepath.Dir(paths.Engine)
}

func extractLinuxEngine(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		marker := "/engine/"
		idx := strings.Index(name, marker)
		if idx < 0 {
			continue
		}
		rel := strings.TrimPrefix(name[idx+len(marker):], "/")
		if rel == "" || rel == "." {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		targetAbs, _ := filepath.Abs(target)
		destAbs, _ := filepath.Abs(dest)
		if !strings.HasPrefix(targetAbs, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes engine directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o755
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type", hdr.Name)
		}
	}
}
