package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// serveBytes returns an httptest server that writes body for any request.
func serveBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// dirEmpty reports whether dir has no entries — used to prove DownloadVerified
// removes its temp file on every failure path.
func dirEmpty(t *testing.T, dir string) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	return len(ents) == 0
}

// TestDownloadVerified_SizeCapBoundary exercises the security-critical size
// cap: LimitReader(max+1) then reject n>max. A body of exactly max (or under)
// must succeed; max+1 (or more) must fail closed, not be truncated into a
// valid-looking file. The sha is set correctly per case so only the size cap
// decides the outcome.
func TestDownloadVerified_SizeCapBoundary(t *testing.T) {
	const max = 1024
	cases := []struct {
		name    string
		bodyLen int
		wantErr bool
	}{
		{"well under cap", max / 2, false},
		{"one under cap", max - 1, false},
		{"exactly at cap", max, false},
		{"one over cap", max + 1, true},
		{"well over cap", max * 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte{'x'}, tc.bodyLen)
			srv := serveBytes(t, body)
			dir := t.TempDir()

			path, err := DownloadVerified(context.Background(), DownloadParams{
				URL:         srv.URL,
				SHA256:      sha256Hex(body),
				DownloadDir: dir,
				TempPattern: "cap-*.bin",
				MaxBytes:    max,
				Client:      srv.Client(),
			})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("bodyLen=%d: expected size-cap error, got path %q", tc.bodyLen, path)
				}
				if !dirEmpty(t, dir) {
					t.Fatalf("bodyLen=%d: temp file left behind after size-cap failure", tc.bodyLen)
				}
				return
			}
			if err != nil {
				t.Fatalf("bodyLen=%d: unexpected error: %v", tc.bodyLen, err)
			}
			got, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read staged file: %v", rerr)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("bodyLen=%d: staged bytes differ from served bytes", tc.bodyLen)
			}
			_ = os.Remove(path)
		})
	}
}

// TestDownloadVerified_SHAMismatch proves a tampered body (wrong digest) is
// rejected and the temp file removed, and that a matching digest is accepted.
func TestDownloadVerified_SHAMismatch(t *testing.T) {
	body := []byte("payload-bytes")
	cases := []struct {
		name    string
		sha     string
		wantErr bool
	}{
		{"correct sha", sha256Hex(body), false},
		{"wrong sha", "00000000000000000000000000000000000000000000000000000000000000ff", true},
		{"empty sha", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveBytes(t, body)
			dir := t.TempDir()

			path, err := DownloadVerified(context.Background(), DownloadParams{
				URL:         srv.URL,
				SHA256:      tc.sha,
				DownloadDir: dir,
				TempPattern: "sha-*.bin",
				MaxBytes:    1 << 20,
				Client:      srv.Client(),
			})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected sha mismatch error, got path %q", path)
				}
				if !dirEmpty(t, dir) {
					t.Fatal("temp file left behind after sha mismatch")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_ = os.Remove(path)
		})
	}
}

// TestDownloadVerified_HTTPError proves a non-200 response fails and stages
// nothing.
func TestDownloadVerified_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	dir := t.TempDir()

	if _, err := DownloadVerified(context.Background(), DownloadParams{
		URL:         srv.URL,
		SHA256:      sha256Hex([]byte("x")),
		DownloadDir: dir,
		TempPattern: "http-*.bin",
		MaxBytes:    1 << 20,
		Client:      srv.Client(),
	}); err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !dirEmpty(t, dir) {
		t.Fatal("temp file staged despite HTTP error")
	}
}

// --- artifact transfer budget (ArtifactTransferTimeout / ArtifactStallTimeout) ---
//
// These pin the split that makes a generous total budget safe: a transfer that
// is merely SLOW must survive, a transfer that is STALLED must be cut. Before
// this split both were bounded by one 5-minute total, which is what made the
// 57MB engine MSI fail two attempts in three on a ~200 KB/s venue link.

// TestDownloadVerified_StalledTransferFailsFast proves the read-progress guard
// cuts a dead connection on ArtifactStallTimeout rather than letting it sit for
// the whole (much larger) total budget.
func TestDownloadVerified_StalledTransferFailsFast(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Enough bytes to get the body flowing, then hang forever — the
		// shape of the venue failure ("context deadline exceeded ... while
		// reading body").
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write([]byte("partial."))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	dir := t.TempDir()
	start := time.Now()
	_, err := DownloadVerified(context.Background(), DownloadParams{
		URL:         srv.URL,
		SHA256:      sha256Hex([]byte("partial.")),
		DownloadDir: dir,
		TempPattern: "stall-*.bin",
		MaxBytes:    1 << 20,
		// A long total budget, deliberately: the point is that the STALL
		// guard fires, not the total.
		Client:       &http.Client{Timeout: time.Minute},
		StallTimeout: 150 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTransferStalled) {
		t.Fatalf("want ErrTransferStalled, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stall guard did not fire promptly: took %s", elapsed)
	}
	if !dirEmpty(t, dir) {
		t.Fatalf("stalled download left its temp file behind")
	}
}

// TestDownloadVerified_SlowButProgressingSucceeds is the other half: a transfer
// whose TOTAL duration exceeds the stall timeout still completes, so long as
// bytes keep arriving. This is the venue case we must not break.
func TestDownloadVerified_SlowButProgressingSucceeds(t *testing.T) {
	t.Parallel()

	// 48 bytes, an exact multiple of chunks so the loop below serves all of it.
	body := []byte("aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff")
	const chunks = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := len(body) / chunks
		for i := 0; i < chunks; i++ {
			// Each gap is comfortably under StallTimeout; the sum of them
			// is comfortably over it.
			time.Sleep(40 * time.Millisecond)
			_, _ = w.Write(body[i*size : (i+1)*size])
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path, err := DownloadVerified(context.Background(), DownloadParams{
		URL:          srv.URL,
		SHA256:       sha256Hex(body),
		DownloadDir:  dir,
		TempPattern:  "slow-*.bin",
		MaxBytes:     1 << 20,
		Client:       &http.Client{Timeout: time.Minute},
		StallTimeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("slow-but-progressing transfer should succeed, got %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read staged file: %v", readErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("staged bytes differ from served bytes")
	}
}

// TestArtifactTransferBudget pins the numbers themselves. The regression this
// guards is somebody trimming the total back towards the old 5 minutes without
// re-doing the arithmetic on a real venue link.
func TestArtifactTransferBudget(t *testing.T) {
	t.Parallel()

	// The worst artifact any caller will pass is the 200MB MaxBytes cap, and
	// the throughput floor we design for is 150 KB/s (measured venue rate was
	// ~200 KB/s; 150 leaves margin below it).
	const capBytes = 200 * 1024 * 1024
	const floorBytesPerSec = 150 * 1024
	need := time.Duration(capBytes/floorBytesPerSec) * time.Second
	if ArtifactTransferTimeout < need {
		t.Fatalf("ArtifactTransferTimeout %s cannot carry %d bytes at %d B/s (needs %s)",
			ArtifactTransferTimeout, capBytes, floorBytesPerSec, need)
	}

	// The stall guard is what licenses a total this large; it must stay well
	// inside it, or a hung socket occupies the whole budget again.
	if ArtifactStallTimeout >= ArtifactTransferTimeout/10 {
		t.Fatalf("ArtifactStallTimeout %s is not comfortably inside ArtifactTransferTimeout %s",
			ArtifactStallTimeout, ArtifactTransferTimeout)
	}

	c := NewTransferClient()
	if c.Timeout != ArtifactTransferTimeout {
		t.Fatalf("NewTransferClient Timeout = %s, want %s", c.Timeout, ArtifactTransferTimeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("NewTransferClient Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.ResponseHeaderTimeout >= ArtifactTransferTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want a small positive bound", tr.ResponseHeaderTimeout)
	}
	// Cloning DefaultTransport (rather than a bare &http.Transport{}) is what
	// keeps proxy support — venue machines behind a corporate proxy depend on
	// ProxyFromEnvironment surviving.
	if tr.Proxy == nil {
		t.Fatalf("transfer client lost proxy support (Transport.Proxy is nil)")
	}
	// h2 must be off on BOTH switches; see NewTransferClient's comment for the
	// measured reason. TestTransferClient_ForcesHTTP11 proves the effect, this
	// pins the mechanism.
	if tr.ForceAttemptHTTP2 {
		t.Fatalf("transfer client re-enabled ForceAttemptHTTP2")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto must be non-nil and empty to disable h2, got %v", tr.TLSNextProto)
	}

	// THE load-bearing assertion. The two checks above are the widely-cited
	// idiom and they are NOT sufficient: on Go 1.26 the transport still
	// advertised h2 in ALPN, the origin selected it, and every download died
	// with a "malformed HTTP response" error (an h2 SETTINGS frame
	// hitting the h1 parser). Agent 0.21.0 shipped that way and could not
	// download anything at all.
	//
	// An httptest-based check cannot catch this — the effect test below passed
	// against the broken build. Only pinned ALPN prevents the server ever being
	// offered h2, so assert it directly.
	if tr.TLSClientConfig == nil {
		t.Fatalf("TLSClientConfig is nil; ALPN must be pinned to http/1.1")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("ALPN NextProtos = %v, want exactly [http/1.1]; without this the "+
			"origin selects h2 and every artifact download fails", got)
	}
}

// TestTransferClient_ForcesHTTP11 proves the effect of the h2 opt-out: against
// a server that genuinely offers HTTP/2 over ALPN, the transfer client still
// negotiates HTTP/1.1.
//
// The control half matters as much as the assertion. Without it this test
// would pass just as happily against a server that never offered h2 at all,
// proving nothing — so it first checks that httptest's OWN client reaches the
// same server over HTTP/2.0.
func TestTransferClient_ForcesHTTP11(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Proto)
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// Control: httptest's client negotiates h2 here, so the server really does
	// offer it and the assertion below is not vacuous.
	ctrlResp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	_ = ctrlResp.Body.Close()
	if ctrlResp.Proto != "HTTP/2.0" {
		t.Fatalf("control client got %s, want HTTP/2.0 — test server is not offering h2, "+
			"so this test cannot prove the transfer client opted out", ctrlResp.Proto)
	}

	// Subject: the artifact transfer client, taught to trust this server's
	// cert but otherwise exactly as NewTransferClient built it.
	c := NewTransferClient()
	tr := c.Transport.(*http.Transport)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("transfer client request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("transfer client negotiated %s, want HTTP/1.1 — h2 is measurably "+
			"slower and stream-resets on the venue route; see NewTransferClient", resp.Proto)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "HTTP/2.0" || seen[1] != "HTTP/1.1" {
		t.Fatalf("server saw %v, want [HTTP/2.0 HTTP/1.1]", seen)
	}
}
