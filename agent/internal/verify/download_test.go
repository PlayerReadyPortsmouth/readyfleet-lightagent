package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
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
