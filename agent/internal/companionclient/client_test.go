package companionclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const device = "11111111-1111-4111-8111-111111111111"
const pairID = "22222222-2222-4222-8222-222222222222"
const regID = "33333333-3333-4333-8333-333333333333"
const sessionID = "44444444-4444-4444-8444-444444444444"
const commandID = "55555555-5555-4555-8555-555555555555"
const idem = "66666666-6666-4666-8666-666666666666"

var dummySecret = strings.Repeat("a", 64)
var dummyToken = strings.Repeat("b", 64)
var revision = strings.Repeat("c", 64)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(data any) *http.Response {
	raw, _ := json.Marshal(map[string]any{"data": data})
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func testClient(t *testing.T, fn roundTrip) *Client {
	t.Helper()
	c, err := New(Config{DeviceID: device, DeviceName: "Test PC", HTTPClient: &http.Client{Transport: fn}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func stateData() map[string]any {
	return map[string]any{"deviceId": device, "staffName": "Example Staff", "expiresAt": time.Now().Add(15 * time.Minute), "sessions": []map[string]any{{"registrationId": regID, "revision": revision, "sessionId": sessionID, "participantSid": "SID-current", "hostName": "Example Host", "allowedActions": []string{"take", "release"}}}}
}
func TestPairingSecretsOnlyInScopedBodyAndHeader(t *testing.T) {
	step := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		step++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Fatal("general credentials attached")
		}
		if strings.Contains(r.URL.String(), dummySecret) || strings.Contains(r.URL.String(), dummyToken) {
			t.Fatal("credential in URL")
		}
		switch step {
		case 1:
			if r.URL.Path != apiPrefix+"/pairings" {
				t.Fatal(r.URL.Path)
			}
			return response(map[string]any{"pairingId": pairID, "pairingSecret": dummySecret, "userCode": "A1B2C3D4", "verificationPath": "/solarbeam/companion/connect?pairing=" + pairID, "expiresAt": time.Now().Add(5 * time.Minute)}), nil
		case 2:
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), dummySecret) || r.Header.Get("X-ReadyFleet-Companion") != "" {
				t.Fatal("wrong poll credentials")
			}
			return response(map[string]any{"status": "approved", "token": dummyToken, "expiresAt": time.Now().Add(15 * time.Minute)}), nil
		case 3:
			if r.Header.Get("X-ReadyFleet-Companion") != dummyToken {
				t.Fatal("missing scoped header")
			}
			return response(stateData()), nil
		}
		t.Fatal("unexpected request")
		return nil, nil
	})
	p, err := c.BeginPairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", c, c), dummySecret) || strings.Contains(p.VerificationURL, dummySecret) {
		t.Fatal("credential exposed")
	}
	s, err := c.State(context.Background())
	if err != nil || s.DeviceID != device {
		t.Fatal(err)
	}
	if c.Pairing() != nil {
		t.Fatal("consumed secret retained")
	}
}
func TestClientRejectsRedirectAndUntrustedOrigin(t *testing.T) {
	if _, err := New(Config{Origin: "http://readyapp.player-ready.co.uk", DeviceID: device}); err == nil {
		t.Fatal("insecure origin accepted")
	}
	calls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		resp := response(nil)
		resp.StatusCode = 302
		resp.Header.Set("Location", "https://other.example/steal")
		return resp, nil
	})
	if _, err := c.BeginPairing(context.Background()); err == nil || calls != 1 {
		t.Fatal("followed redirect")
	}
}
func TestIdentityMismatchAndRevocation(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		data := stateData()
		data["deviceId"] = sessionID
		return response(data), nil
	})
	c.token = dummyToken
	c.expires = time.Now().Add(15 * time.Minute)
	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("wrong device accepted")
	}
	c.http.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":"companion_revoked"}`))}, nil
	})
	if _, err := c.State(context.Background()); err == nil || c.token != "" {
		t.Fatal("revoked local credential retained")
	}
}
func TestRenewalAndSignOut(t *testing.T) {
	paths := []string{}
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/renew") {
			return response(map[string]any{"expiresAt": time.Now().Add(15 * time.Minute)}), nil
		}
		if r.Method == http.MethodDelete {
			return response(map[string]bool{"revoked": true}), nil
		}
		return response(stateData()), nil
	})
	c.token = dummyToken
	c.expires = time.Now().Add(time.Minute)
	if _, err := c.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(paths[0], "/renew") {
		t.Fatal("lease not renewed")
	}
	if err := c.SignOut(context.Background()); err != nil || c.token != "" || c.pair != nil {
		t.Fatal("signout retains local authority")
	}
}
func TestExactActionPayloadAndQueuedReceipt(t *testing.T) {
	var captured Action
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		return response(Command{CommandID: commandID, Status: "queued"}), nil
	})
	c.token = dummyToken
	c.expires = time.Now().Add(time.Minute)
	req := Action{Action: "admit", RegistrationID: regID, ExpectedRevision: revision, IdempotencyKey: idem, Admission: &Admission{ActivityID: "exact-activity", CYPContactID: "exact-person"}}
	out, err := c.Action(context.Background(), req)
	if err != nil || out.Status != "queued" || captured.Admission.CYPContactID != "exact-person" || captured.ExpectedRevision != revision {
		t.Fatal("action lost exact binding", err)
	}
	req.ExpectedRevision = ""
	if _, err = c.Action(context.Background(), req); err == nil {
		t.Fatal("unbound action accepted")
	}
}
func TestConcurrentPollOnlyConsumesOnce(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			mu.Lock()
			polls++
			mu.Unlock()
			return response(map[string]any{"status": "approved", "token": dummyToken, "expiresAt": time.Now().Add(15 * time.Minute)}), nil
		}
		return response(stateData()), nil
	})
	c.pair = &pendingPair{id: pairID, secret: dummySecret, public: Pairing{ExpiresAt: time.Now().Add(time.Minute)}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.State(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if polls != 1 {
		t.Fatal("approval consumed more than once")
	}
}

func TestBrowserLeaseUnavailableRetainsCredentialAndRecovers(t *testing.T) {
	unavailable := true
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-ReadyFleet-Companion") != dummyToken {
			t.Fatal("binding changed")
		}
		if unavailable {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"browser_unavailable"}`))}, nil
		}
		return response(stateData()), nil
	})
	c.token = dummyToken
	c.expires = time.Now().Add(15 * time.Minute)
	if _, err := c.State(context.Background()); !errors.Is(err, ErrBrowserUnavailable) || !c.HasBinding() {
		t.Fatal("transient lease discarded binding", err)
	}
	unavailable = false
	if _, err := c.State(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestConsumedPairingClearsAndFreshSignInCanStart(t *testing.T) {
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if strings.HasSuffix(r.URL.Path, "/poll") {
			return &http.Response{StatusCode: 410, Body: io.NopCloser(strings.NewReader(`{"error":"pairing_expired"}`))}, nil
		}
		return response(map[string]any{"pairingId": pairID, "pairingSecret": dummySecret, "userCode": "A1B2C3D4", "verificationPath": "/solarbeam/companion/connect?pairing=" + pairID, "expiresAt": time.Now().Add(5 * time.Minute)}), nil
	})
	_, _ = c.BeginPairing(context.Background())
	if _, err := c.State(context.Background()); !errors.Is(err, ErrSignedOut) || c.Pairing() != nil {
		t.Fatal("consumed pairing kept", err)
	}
	if _, err := c.BeginPairing(context.Background()); err != nil || requests != 3 {
		t.Fatal("new explicit sign-in blocked", err, requests)
	}
}
func TestUnknownPairingOutcomeNeverReplaysConsumedCredential(t *testing.T) {
	polls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls++
			return nil, errors.New("response lost")
		}
		return response(map[string]any{"pairingId": pairID, "pairingSecret": dummySecret, "userCode": "A1B2C3D4", "verificationPath": "/solarbeam/companion/connect?pairing=" + pairID, "expiresAt": time.Now().Add(5 * time.Minute)}), nil
	})
	_, _ = c.BeginPairing(context.Background())
	for i := 0; i < 3; i++ {
		if _, err := c.State(context.Background()); !errors.Is(err, ErrPairingUncertain) {
			t.Fatal(err)
		}
	}
	if polls != 1 {
		t.Fatal("ambiguous one-use credential replayed")
	}
	_ = c.SignOut(context.Background())
	if c.Pairing() != nil {
		t.Fatal("explicit restart retains old secret")
	}
	if _, err := c.BeginPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Revoke is called when the Windows session locks. Local authority must be gone
// before it returns even if the broker never answers, and the delayed remote
// unlink must not erase a credential from a later, fresh sign-in.
func TestRevokeClearsLocallyBeforeRemoteUnlink(t *testing.T) {
	release := make(chan struct{})
	unlinked := make(chan string, 1)
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			<-release
			unlinked <- r.Header.Get("X-ReadyFleet-Companion")
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"companion_revoked"}`))}, nil
		}
		t.Errorf("unexpected broker request %s %s after revocation", r.Method, r.URL.Path)
		return response(stateData()), nil
	})
	c.token = dummyToken
	c.expires = time.Now().Add(15 * time.Minute)
	c.pair = &pendingPair{id: pairID, secret: dummySecret, public: Pairing{ExpiresAt: time.Now().Add(time.Minute)}}
	if !c.Revoke() {
		t.Fatal("held credential not reported")
	}
	if c.HasBinding() || c.Pairing() != nil {
		t.Fatal("local authority survived revocation")
	}
	if _, err := c.State(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("state after revoke = %v, want signed out", err)
	}
	fresh := strings.Repeat("d", 64)
	c.mu.Lock()
	c.token = fresh
	c.expires = time.Now().Add(15 * time.Minute)
	c.mu.Unlock()
	close(release)
	select {
	case got := <-unlinked:
		if got != dummyToken {
			t.Fatal("remote unlink did not carry the revoked credential")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote unlink never attempted")
	}
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != fresh {
		t.Fatal("background unlink erased a later credential")
	}
	if (&Client{}).Revoke() {
		t.Fatal("empty client reported a credential")
	}
}
