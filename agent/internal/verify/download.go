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
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// The artifact-transfer budget, shared by every fetch in this package's
// client list (self-update, MSI install, engine zip update, file browser).
// It is deliberately one set of numbers: all four move the same class of
// object — a signed artifact up to 200MB (the MaxBytes cap each caller
// passes) — over the same venue links, so a value that is right for one is
// right for all of them.
//
// Why the total is 30 minutes and not the 5 it used to be. Measured on
// venue PC DESKTOP-I01IOUG (19 Aug 2026): it pulls ~200 KB/s from our
// origin (dashboardapi.player-ready.co.uk) and ~697 KB/s via Cloudflare;
// the same origin serves the file in ~1s to other vantage points, so this
// is ISP<->OVH peering and cannot be fixed from the agent. At ~200 KB/s the
// 57MB engine MSI needs ~289s — inside a 5 minute budget on paper, but only
// just, and two of three real attempts failed (one HTTP/2 stream error, one
// "context deadline exceeded while reading body"); the third took 3m34s. An
// install that works one time in three cannot go to the fleet.
//
// 30 minutes covers the full 200MB cap at ~114 KB/s, and a realistic ~120MB
// artifact at ~68 KB/s — comfortably below the 150 KB/s we should expect a
// bad venue link to sustain, so the budget stops being the thing that fails
// first. It is safe to be this generous ONLY because ArtifactStallTimeout
// bounds a dead connection independently; see below.
const ArtifactTransferTimeout = 30 * time.Minute

// ArtifactStallTimeout is the read-progress guard: a transfer that delivers
// no bytes at all for this long is dead, not slow, and is cancelled
// regardless of how much of ArtifactTransferTimeout remains. This is what
// keeps a generous total budget from meaning "a hung socket hangs for half
// an hour" — slow-but-progressing is tolerated, stalled is not.
//
// 60s is far longer than any pause a link doing even 10 KB/s would produce,
// so it cannot fire on a merely slow venue transfer.
const ArtifactStallTimeout = 60 * time.Second

// artifactResponseHeaderTimeout bounds "connected but the origin never
// answered". Go's HTTP/2 transport silently ignores ResponseHeaderTimeout,
// so on h2 this constant did nothing at all; because artifact transfers now
// force HTTP/1.1 (see NewTransferClient) it does real work again, alongside
// — not instead of — the read-progress stall guard.
const artifactResponseHeaderTimeout = 30 * time.Second

// ErrTransferStalled reports a download cancelled by the read-progress
// guard: the body stopped producing bytes for ArtifactStallTimeout.
var ErrTransferStalled = errors.New("download stalled: no bytes received")

// NewTransferClient returns the http.Client every artifact transfer in this
// repo should use. Callers must not substitute a bare &http.Client{Timeout:
// …}: the total budget alone cannot tell a slow transfer from a dead one,
// which is the whole point of the pairing documented above.
//
// ⚠ This client deliberately speaks HTTP/1.1 ONLY. Do not "modernise" it
// back to HTTP/2 — h2 was measured to be both slower and actively broken on
// the route that matters. On venue PC DESKTOP-I01IOUG (20 Aug 2026),
// fetching engine 2.0.6 from
// dashboardapi.player-ready.co.uk/releases/agent-download/windows-engine:
//
//   - over h2 (Go's default): ~200 KB/s, and it FAILED twice with
//     "stream error: stream ID 1; INTERNAL_ERROR; received from peer" —
//     once at 201.7s. Not a timeout: a successful install on the same path
//     took 214s, so there is no server-side cutoff being hit. It is h2
//     stream flakiness on this route.
//   - over HTTP/1.1: ~295 KB/s (5,910,210 bytes in 20s), no resets.
//
// So h2 costs ~1.5x throughput AND supplies the only hard failures we have
// seen. The third strike is that Go's h2 transport ignores
// ResponseHeaderTimeout; pinning h1 restores that timeout to actually doing
// something (see artifactResponseHeaderTimeout).
//
// Forcing h1 is a reliability and throughput win, NOT a replacement for the
// stall guard — ArtifactStallTimeout remains the primary protection, since
// an h1 connection can hang just as dead as an h2 one.
//
// Scope: artifact transfers only. The control-plane websocket and every
// other client in the agent are untouched — this is about large binary
// downloads on bad venue links, nothing else.
func NewTransferClient() *http.Client {
	// Clone (rather than a fresh &http.Transport{}) to keep
	// ProxyFromEnvironment and the tuned dial/idle defaults.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = artifactResponseHeaderTimeout
	// The documented Go idiom for "never negotiate h2": a non-nil, empty
	// TLSNextProto means there is no h2 handler to hand an ALPN-negotiated
	// connection to, and clearing ForceAttemptHTTP2 stops the transport
	// advertising h2 in ALPN in the first place. Both are needed — the
	// former alone still advertises it.
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{
		Timeout:   ArtifactTransferTimeout,
		Transport: tr,
	}
}

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
	// Orthogonal to the timeouts: it bounds how big an artifact may be, not
	// how long it may take.
	MaxBytes int64
	Client   *http.Client
	// StallTimeout overrides ArtifactStallTimeout for this transfer. Zero
	// means the package default; tests set a short one.
	StallTimeout time.Duration
}

// DownloadVerified fetches p.URL to a temp file in p.DownloadDir, enforcing
// the p.MaxBytes size cap, then verifies the file's sha256 against
// p.SHA256. It returns the staged path on success. On any failure it
// removes the temp file and returns the error, so the caller only ever
// holds a fully verified file (or nothing). See the package doc for the
// size-cap / sha invariants.
func DownloadVerified(ctx context.Context, p DownloadParams) (string, error) {
	stall := p.StallTimeout
	if stall <= 0 {
		stall = ArtifactStallTimeout
	}
	// Cancellable child context so the read-progress guard can abort the
	// in-flight body read. WithCancelCause so the resulting error can say
	// "stalled" rather than a bare context.Canceled.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

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
	guarded := newStallReader(resp.Body, stall, cancel)
	defer guarded.stop()
	limited := io.LimitReader(guarded, p.MaxBytes+1)
	n, err := io.Copy(f, limited)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		// A stall surfaces as a context cancellation on the read; report
		// the real reason so an operator can tell "the link died" from
		// "the whole budget ran out".
		if cause := context.Cause(ctx); errors.Is(cause, ErrTransferStalled) {
			return "", fmt.Errorf("download: %w after %s without progress", ErrTransferStalled, stall)
		}
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

// stallReader is the read-progress guard behind ArtifactStallTimeout. It
// passes bytes through untouched and arms a timer that cancels the
// download's context if no byte arrives within idle; every successful read
// pushes the deadline out again.
//
// A timer rather than a goroutine polling a timestamp: time.AfterFunc
// already owns the "fire unless reset" behaviour, so there is no watchdog
// loop, no shared state to race on, and nothing to shut down beyond
// stopping the timer.
type stallReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

func newStallReader(r io.Reader, idle time.Duration, cancel context.CancelCauseFunc) *stallReader {
	return &stallReader{
		r:    r,
		idle: idle,
		// Armed before the first read, so a connection that yields headers
		// and then never a single byte is caught too.
		timer: time.AfterFunc(idle, func() { cancel(ErrTransferStalled) }),
	}
}

func (s *stallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 {
		s.timer.Reset(s.idle)
	}
	return n, err
}

func (s *stallReader) stop() { s.timer.Stop() }

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
