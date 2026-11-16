package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Enroll only checks that the cert/CA fields are non-empty and writes the
// bytes verbatim, so these stand in for real PEM without dragging a CA
// fixture into this package's tests.
const (
	fakeCertPEM = "-----BEGIN CERTIFICATE-----\nZmFrZS1jZXJ0\n-----END CERTIFICATE-----\n"
	fakeCAPEM   = "-----BEGIN CERTIFICATE-----\nZmFrZS1jYQ==\n-----END CERTIFICATE-----\n"
	testPollID  = "11111111-2222-3333-4444-555555555555"
)

// pendingServer stands in for the C2's /enroll/v1/redeem endpoints: the POST
// answers 202 + RedeemPendingResponse (the BYOD consent gate), and the GET
// answers whatever the caller's pollBodies script says, one entry per poll.
type pendingServer struct {
	mu          sync.Mutex
	pollBodies  []map[string]any
	pollCount   int
	pollPaths   []string
	pollBearers []string
	postCount   int
}

func (p *pendingServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			p.postCount++
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":           "pending",
				"pending_id":       testPollID,
				"poll_token":       "rfp_poll-secret",
				"poll_interval_ms": 1,
			})
		case http.MethodGet:
			p.pollPaths = append(p.pollPaths, r.URL.Path)
			p.pollBearers = append(p.pollBearers, r.Header.Get("Authorization"))
			idx := p.pollCount
			p.pollCount++
			if idx >= len(p.pollBodies) {
				idx = len(p.pollBodies) - 1
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(p.pollBodies[idx])
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *pendingServer) polls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pollCount
}

func approvedBody() map[string]any {
	return map[string]any{
		"status":           "approved",
		"machine_id":       testPollID,
		"venue_id":         "venue-1",
		"cert_pem":         fakeCertPEM,
		"ca_pem":           fakeCAPEM,
		"agent_listen_url": "wss://c2.example.test/agent",
		"owner_actor_id":   "mentor-oid",
	}
}

// TestEnroll_ApprovalCarriesTheOwnerIdentity: the pending-approval poll is
// the only path a BYOD device takes, and it is where the agent learns who
// owns the machine it is running on. Without this the consent gate has
// nothing to compare a requester against and prompts the owner for access
// to their own PC.
func TestEnroll_ApprovalCarriesTheOwnerIdentity(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{approvedBody()}}
	ts := srv.start(t)
	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	res, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.OwnerActorID != "mentor-oid" {
		t.Errorf("OwnerActorID = %q, want the owner the controller recorded", res.OwnerActorID)
	}
}

// TestEnroll_MissingOwnerIsEmptyNotGuessed: a controller that predates the
// field, or an enrollment with no actor attribution, must leave the agent
// with no owner rather than an inferred one. Empty matches nobody, so the
// agent prompts for every request — the fail-closed direction.
func TestEnroll_MissingOwnerIsEmptyNotGuessed(t *testing.T) {
	body := approvedBody()
	delete(body, "owner_actor_id")
	srv := &pendingServer{pollBodies: []map[string]any{body}}
	ts := srv.start(t)
	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	res, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.OwnerActorID != "" {
		t.Errorf("OwnerActorID = %q, want empty", res.OwnerActorID)
	}
}

func testOptions(t *testing.T, srv *httptest.Server) Options {
	t.Helper()
	return Options{
		EnrollmentURL: srv.URL + "/enroll/v1/redeem",
		BearerSecret:  "rft_" + strings.Repeat("a", 43),
		AgentVersion:  "test",
		MaterialDir:   filepath.Join(t.TempDir(), "material"),
		HTTPClient:    srv.Client(),
	}
}

// The headline behaviour: a BYOD redemption that comes back 202/pending is
// waited on — polled until the mentor approves it in ReadyApp — and then
// completes exactly as if the POST had returned the cert synchronously,
// signing material and all.
func TestEnroll_PendingPollsUntilApproved(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{
		{"status": "pending"},
		{"status": "pending"},
		approvedBody(),
	}}
	ts := srv.start(t)

	var pendingCalls []Pending
	opts := testOptions(t, ts)
	opts.OnPending = func(p Pending) { pendingCalls = append(pendingCalls, p) }

	res, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if got := srv.polls(); got != 3 {
		t.Fatalf("poll count = %d, want 3", got)
	}
	if len(pendingCalls) != 1 {
		t.Fatalf("OnPending called %d times, want exactly 1", len(pendingCalls))
	}
	if pendingCalls[0].PendingID != testPollID {
		t.Errorf("OnPending pending id = %q, want %q", pendingCalls[0].PendingID, testPollID)
	}
	// The fixture proposes 1ms; the clamp must floor it rather than let a
	// silly server value turn the wait into a hot loop.
	if pendingCalls[0].PollInterval != minPollInterval {
		t.Errorf("poll interval = %s, want it clamped to %s", pendingCalls[0].PollInterval, minPollInterval)
	}
	if res.MachineID != testPollID || res.VenueID != "venue-1" {
		t.Errorf("result identity = %q/%q", res.MachineID, res.VenueID)
	}
	if res.AgentListenURL != "wss://c2.example.test/agent" {
		t.Errorf("agent listen url = %q", res.AgentListenURL)
	}
	for _, p := range []string{res.CertPath, res.KeyPath, res.CAPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected material at %s: %v", p, err)
		}
	}
	cert, err := os.ReadFile(res.CertPath)
	if err != nil || string(cert) != fakeCertPEM {
		t.Errorf("cert on disk = %q (err %v), want the polled cert", cert, err)
	}
	// The poll must hit GET /enroll/v1/redeem/{pending_id} carrying the
	// one-time poll token as a bearer — that token is the only credential
	// an unenrolled installer has at this point.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i, path := range srv.pollPaths {
		if path != "/enroll/v1/redeem/"+testPollID {
			t.Errorf("poll %d path = %q", i, path)
		}
		if srv.pollBearers[i] != "Bearer rfp_poll-secret" {
			t.Errorf("poll %d authorization = %q", i, srv.pollBearers[i])
		}
	}
}

// A caller with no way to tell a human what it is waiting for must fail
// fast rather than block for the pending window: the direct lightagent
// re-enrolment path and the installer's own non-interactive uses have no
// wizard, and a silent ten-minute hang there is indistinguishable from a
// crash.
func TestEnroll_PendingWithoutOnPendingFailsFast(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{approvedBody()}}
	ts := srv.start(t)

	opts := testOptions(t, ts)
	opts.OnPending = nil

	_, err := Enroll(context.Background(), opts)
	if !errors.Is(err, ErrPendingApprovalRequired) {
		t.Fatalf("Enroll error = %v, want ErrPendingApprovalRequired", err)
	}
	if got := srv.polls(); got != 0 {
		t.Fatalf("poll count = %d, want 0 — it must not poll at all", got)
	}
}

func TestEnroll_PendingDeclined(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{{"status": "declined"}}}
	ts := srv.start(t)

	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	_, err := Enroll(context.Background(), opts)
	if !errors.Is(err, ErrEnrollmentDeclined) {
		t.Fatalf("Enroll error = %v, want ErrEnrollmentDeclined", err)
	}
	if _, statErr := os.Stat(filepath.Join(opts.MaterialDir, "client.key")); statErr == nil {
		t.Error("a declined enrollment must not leave a private key on disk")
	}
}

func TestEnroll_PendingExpired(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{{"status": "expired"}}}
	ts := srv.start(t)

	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrEnrollmentExpired) {
		t.Fatalf("Enroll error = %v, want ErrEnrollmentExpired", err)
	}
}

// Cancelling the context must unwind the poll loop promptly rather than
// leaving it running to the pending window's end.
func TestEnroll_PendingRespectsContextCancellation(t *testing.T) {
	srv := &pendingServer{pollBodies: []map[string]any{{"status": "pending"}}}
	ts := srv.start(t)

	ctx, cancel := context.WithCancel(context.Background())
	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) { cancel() }

	done := make(chan error, 1)
	go func() {
		_, err := Enroll(ctx, opts)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Enroll error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Enroll did not return after its context was cancelled")
	}
}

// When the poll server is unreachable (transport failures / 5xx) for the
// entire pending window, the 15-minute client backstop is what ends the
// wait — not a genuine server verdict. That must not surface as the
// generic "gave up waiting" text or, worse, fall all the way through to a
// caller's fallback "installation failed" message: it must be the distinct
// ErrEnrollmentUnreachable sentinel, carrying the last retryable error the
// loop actually saw, so the real cause (the network) survives.
func TestEnroll_PendingBackstopReportsLastRetryableError(t *testing.T) {
	// Shrink the backstop so the test doesn't take 15 real minutes; restore
	// it so other tests in this package keep the production value.
	origWait := maxPendingWait
	maxPendingWait = 350 * time.Millisecond
	t.Cleanup(func() { maxPendingWait = origWait })

	var gets int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "pending", "pending_id": testPollID,
				"poll_token": "rfp_poll-secret", "poll_interval_ms": 1,
			})
		case http.MethodGet:
			atomic.AddInt32(&gets, 1)
			// Every poll for the whole window fails server-side — the
			// mentor's Wi-Fi is down, say.
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer ts.Close()

	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	start := time.Now()
	_, err := Enroll(context.Background(), opts)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrEnrollmentUnreachable) {
		t.Fatalf("Enroll error = %v, want ErrEnrollmentUnreachable", err)
	}
	if errors.Is(err, ErrEnrollmentDeclined) || errors.Is(err, ErrEnrollmentExpired) {
		t.Errorf("Enroll error = %v, must not read as a genuine server verdict", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("Enroll error = %q, want the last retryable poll error (503) preserved in it", err.Error())
	}
	if elapsed >= 15*time.Minute {
		t.Fatalf("Enroll took %s — it did not respect the shrunk backstop", elapsed)
	}
	if got := atomic.LoadInt32(&gets); got == 0 {
		t.Fatal("expected at least one poll attempt before the backstop fired")
	}
}

// A hostile or buggy server must not be able to steer the poll request at
// an arbitrary URL by way of the pending_id it hands back.
func TestEnroll_RejectsUnsafePendingID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s — it must never poll an unsafe id", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "pending", "pending_id": "../../evil", "poll_token": "rfp_x", "poll_interval_ms": 1,
		})
	}))
	defer ts.Close()

	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) {}

	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("expected an error for an unsafe pending_id, got nil")
	}
}

// Regression guard for the path every non-BYOD agent still takes: a 200
// with a cert must complete synchronously, never entering the poll loop
// and never touching OnPending.
func TestEnroll_SynchronousRedemptionIsUnchanged(t *testing.T) {
	var gets int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"machine_id": "machine-1", "venue_id": "venue-1",
			"cert_pem": fakeCertPEM, "ca_pem": fakeCAPEM,
			"agent_listen_url": "wss://c2.example.test/agent",
		})
	}))
	defer ts.Close()

	opts := testOptions(t, ts)
	opts.OnPending = func(Pending) { t.Error("OnPending must not fire for a synchronous redemption") }

	res, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.MachineID != "machine-1" {
		t.Errorf("machine id = %q", res.MachineID)
	}
	if gets != 0 {
		t.Errorf("issued %d poll requests, want 0", gets)
	}
}
