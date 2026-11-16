package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func retryOptions(t *testing.T, srv *httptest.Server) Options {
	t.Helper()
	return Options{EnrollmentURL: srv.URL + "/enroll/v1/redeem", BearerSecret: "rft_test-only", MaterialDir: t.TempDir(), Retry: true}
}
func writeEnrollment(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(Response{MachineID: testPollID, CertPEM: fakeCertPEM, CAPEM: fakeCAPEM})
}

func TestRetryRateLimitKeepsSameCSR(t *testing.T) {
	var calls int
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls < 3 {
			w.WriteHeader(429)
			return
		}
		writeEnrollment(w)
	}))
	defer srv.Close()
	opts := retryOptions(t, srv)
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("calls=%d, CSR changed=%v", calls, bodies)
	}
	if _, err := os.Stat(filepath.Join(opts.MaterialDir, checkpointName)); !os.IsNotExist(err) {
		t.Fatalf("successful enrollment retained checkpoint: %v", err)
	}
}

func TestRetryPreWriteDialFailures(t *testing.T) {
	var calls, dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeEnrollment(w) }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	opts.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) < 3 {
			return nil, errors.New("temporary dial failure")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}}
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 3 || calls.Load() != 1 {
		t.Fatalf("dials=%d posts=%d", dials.Load(), calls.Load())
	}
}

func TestRetryDoesNotReplayAmbiguousPOST(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	opts := retryOptions(t, srv)
	for i := 0; i < 2; i++ {
		if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrAmbiguousRedemption) {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("ambiguous request replayed %d times", calls)
	}
}

func TestRetryTerminalStatusesAndFlagOff(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{{401, true}, {400, true}, {500, true}, {503, true}, {429, false}} {
		t.Run(fmt.Sprintf("%d-retry-%t", tc.status, tc.retry), func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.status) }))
			defer srv.Close()
			opts := retryOptions(t, srv)
			opts.Retry = tc.retry
			if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrServer) {
				t.Fatalf("err=%v", err)
			}
			if calls != 1 {
				t.Fatalf("terminal response retried %d times", calls)
			}
		})
	}
}

func TestRetryCancellationInterruptsBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(429) }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	// Key generation precedes the request. Cancel when the first rate-limit
	// response arrives, ensuring this exercises the backoff rather than keygen.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			cancel()
		}
		return response, err
	})}
	if _, err := Enroll(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("retried after cancellation: %d", calls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryResumesSavedPendingWithoutNewPOST(t *testing.T) {
	var posts, polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(pendingRedeem{Status: "pending", PendingID: testPollID, PollToken: "rfp_secret", PollIntervalMs: 1})
			return
		}
		polls++
		if r.Header.Get("Authorization") != "Bearer rfp_secret" {
			t.Error("wrong poll credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "approved", "machine_id": testPollID, "cert_pem": fakeCertPEM, "ca_pem": fakeCAPEM})
	}))
	defer srv.Close()
	opts := retryOptions(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	opts.OnPending = func(Pending) { cancel() }
	if _, err := Enroll(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("first wait: %v", err)
	}
	recovery := filepath.Join(opts.MaterialDir, checkpointName)
	data, err := os.ReadFile(recovery)
	if err != nil {
		t.Fatal(err)
	}
	var state enrollmentCheckpoint
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(recovery)
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("checkpoint exposes credentials: %o", info.Mode().Perm())
	}
	opts.OnPending = func(Pending) {}
	result, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(result.KeyPath)
	if string(key) != state.KeyPEM || posts != 1 || polls != 1 {
		t.Fatalf("resume changed key or redeemed again: posts=%d polls=%d", posts, polls)
	}
}

func TestRetryCheckpointBoundToSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not send mismatched checkpoint") }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	if err := saveCheckpoint(opts, enrollmentCheckpoint{Binding: "another secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnrollmentNeverFollowsRedirectWithCredentials(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	opts.HTTPClient = srv.Client()
	if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrServer) {
		t.Fatalf("err=%v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestPreflightDateAndConnectivity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statuses   []int
		skew       time.Duration
		clockError bool
	}{{"healthy", []int{405}, 0, false}, {"clock", []int{401}, -10 * time.Minute, true}, {"transient", []int{503, 503, 405}, 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead || r.Header.Get("Authorization") != "" {
					t.Error("preflight must be credential-free HEAD")
				}
				status := tc.statuses[calls]
				calls++
				w.Header().Set("Date", time.Now().Add(tc.skew).UTC().Format(http.TimeFormat))
				w.WriteHeader(status)
			}))
			defer srv.Close()
			err := preflight(context.Background(), srv.URL, srv.Client())
			if errors.Is(err, ErrClockSkew) != tc.clockError {
				t.Fatalf("err=%v", err)
			}
			if !tc.clockError && err != nil {
				t.Fatal(err)
			}
			if calls != len(tc.statuses) {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
	if err := CheckConnectivity(context.Background(), "http://readyfleet.invalid"); err == nil {
		t.Fatal("accepted insecure preflight")
	}
	transport := newEnrollmentClient(false).Transport.(*http.Transport)
	if transport.Proxy == nil || transport.TLSClientConfig.RootCAs != pinnedCAPool {
		t.Fatal("lost proxy configuration or pinned CA")
	}
}

func TestPendingPollRetriesRateLimitAndServerErrors(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(pendingRedeem{Status: "pending", PendingID: testPollID, PollToken: "rfp_test", PollIntervalMs: 1})
			return
		}
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(503)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "approved", "machine_id": testPollID, "cert_pem": fakeCertPEM, "ca_pem": fakeCAPEM})
		}
	}))
	defer srv.Close()
	opts := retryOptions(t, srv)
	opts.OnPending = func(Pending) {}
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls=%d", polls.Load())
	}
}

func TestPreflightRejectsProxyAuthAndStopsOnCancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(407) }))
	defer srv.Close()
	if err := preflight(context.Background(), srv.URL, srv.Client()); err == nil {
		t.Fatal("treated proxy auth failure as healthy")
	}
	if calls.Load() != 3 {
		t.Fatalf("preflight attempts=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preflight(ctx, srv.URL, srv.Client()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestRetryResumesCredentialWriteWithoutRedeemingAgain(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); writeEnrollment(w) }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	obstruction := filepath.Join(opts.MaterialDir, "client.crt")
	if err := os.Mkdir(obstruction, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("expected obstructed credential write to fail")
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("credential repair re-spent single-use token: posts=%d", posts.Load())
	}
}

func TestRetryRateLimitExhaustionIsBounded(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(429) }))
	defer srv.Close()
	opts := retryOptions(t, srv)
	if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrServer) {
		t.Fatalf("err=%v", err)
	}
	if posts.Load() != 3 {
		t.Fatalf("unbounded/absent retries: posts=%d", posts.Load())
	}
}
