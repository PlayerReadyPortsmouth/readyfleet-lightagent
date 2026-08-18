// Package verify holds the download/verify/signature-check plumbing shared
// by every agent manager that fetches and applies a signed artifact:
// self-update (exec.UpdateManager), MSI install (exec.InstallManager), the
// file browser's upload path (exec.FileTransferManager), and the SolarBeam
// engine's own update path (solarbeam.Manager). It was previously
// duplicated-by-reference inside package exec; extracted here so
// agent/internal/solarbeam can reach it too without exec importing
// solarbeam or vice versa — solarbeam.Manager is the core, exec's managers
// become adapters over it, so the dependency has to run this direction.
//
// The security-critical boundaries below must stay behaviourally identical
// to how they worked inside exec:
//
//   - the size cap reads MaxBytes+1 through io.LimitReader and rejects
//     n > MaxBytes, so an over-cap body fails closed rather than being
//     silently truncated into something that looks valid;
//   - the sha256 compare is byte-exact hex equality against the
//     controller-supplied digest.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// SingleFlight guards a manager that must run at most one operation at a
// time (install, self-update, engine update). TryBegin reports whether the
// caller acquired the slot; a caller that gets true must pair it with End.
type SingleFlight struct {
	mu       sync.Mutex
	inFlight bool
}

func (s *SingleFlight) TryBegin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return false
	}
	s.inFlight = true
	return true
}

func (s *SingleFlight) End() {
	s.mu.Lock()
	s.inFlight = false
	s.mu.Unlock()
}

// DownloadParams describes a single verified download.
type DownloadParams struct {
	// URL is the stable installer/binary URL.
	URL string
	// SHA256 is the expected lowercase-hex digest of the downloaded bytes.
	SHA256 string
	// DownloadDir stages the temp file; empty means os.TempDir().
	DownloadDir string
	// TempPattern is the os.CreateTemp pattern (e.g. "fleet-agent-update-*.exe").
	TempPattern string
	// MaxBytes is the hard size cap; a body larger than this fails closed.
	MaxBytes int64
	Client   *http.Client
}

// DownloadVerified fetches p.URL to a temp file in p.DownloadDir, enforcing
// the p.MaxBytes size cap, then verifies the file's sha256 against
// p.SHA256. It returns the staged path on success. On any failure it
// removes the temp file and returns the error, so the caller only ever
// holds a fully verified file (or nothing). See the package doc for the
// size-cap / sha invariants.
func DownloadVerified(ctx context.Context, p DownloadParams) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: status %d", resp.StatusCode)
	}

	dir := p.DownloadDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, p.TempPattern)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	path := f.Name()
	limited := io.LimitReader(resp.Body, p.MaxBytes+1)
	n, err := io.Copy(f, limited)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("download: %w", err)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("download: %w", closeErr)
	}
	if n > p.MaxBytes {
		_ = os.Remove(path)
		return "", fmt.Errorf("download exceeded %d bytes", p.MaxBytes)
	}

	if err := verifySHA256(path, p.SHA256); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// verifySHA256 returns nil only if the file at path hashes to wantHex.
func verifySHA256(path, wantHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantHex {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, wantHex)
	}
	return nil
}
