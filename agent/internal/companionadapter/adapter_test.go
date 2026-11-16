package companionadapter

import (
	"context"
	"encoding/json"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const dev = "11111111-1111-4111-8111-111111111111"
const pair = "22222222-2222-4222-8222-222222222222"
const registration = "33333333-3333-4333-8333-333333333333"
const session = "44444444-4444-4444-8444-444444444444"
const command = "55555555-5555-4555-8555-555555555555"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func data(v any) *http.Response {
	raw, _ := json.Marshal(map[string]any{"data": v})
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func local(context.Context) (ui.Snapshot, error) {
	now := time.Now()
	return ui.Snapshot{ObservedAt: now, ValidUntil: now.Add(15 * time.Second), Device: ui.Device{ID: dev, Name: "Example PC"}, Hosting: ui.Hosting{Known: true, Available: true, Revision: "owned-revision"}}, nil
}
func paired(t *testing.T, state *companionclient.State, captured *companionclient.Action) *companionclient.Client {
	return pairedWithUnavailable(t, state, captured, nil)
}
func pairedWithUnavailable(t *testing.T, state *companionclient.State, captured *companionclient.Action, unavailable *bool) *companionclient.Client {
	t.Helper()
	client, err := companionclient.New(companionclient.Config{DeviceID: dev, DeviceName: "Example PC", HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pairings"):
			return data(map[string]any{"pairingId": pair, "pairingSecret": strings.Repeat("a", 64), "userCode": "AABB1122", "verificationPath": "/solarbeam/companion/connect?pairing=" + pair, "expiresAt": time.Now().Add(time.Minute)}), nil
		case strings.HasSuffix(r.URL.Path, "/poll"):
			return data(map[string]any{"status": "approved", "token": strings.Repeat("b", 64), "expiresAt": time.Now().Add(15 * time.Minute)}), nil
		case strings.HasSuffix(r.URL.Path, "/actions"):
			if captured != nil {
				_ = json.NewDecoder(r.Body).Decode(captured)
			}
			return data(companionclient.Command{CommandID: command, Status: "queued"}), nil
		default:
			if unavailable != nil && *unavailable {
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"browser_unavailable"}`))}, nil
			}
			return data(state), nil
		}
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.BeginPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	return client
}
func brokerState() companionclient.State {
	return companionclient.State{DeviceID: dev, StaffName: "Example Staff", ExpiresAt: time.Now().Add(15 * time.Minute), HostAllowedActions: []string{"set_hosting"}, Sessions: []companionclient.Session{{RegistrationID: registration, SessionID: session, ParticipantSID: "SID", Revision: strings.Repeat("c", 64), HostName: "Example Host", AllowedActions: []string{"take", "release", "admit"}, ExclusiveHeldBySelf: true, ActivityID: "activity", CYPContactID: "person", CYPName: "Example young person", ActivityLabel: "Example activity", ActivityStartsAt: time.Now()}}}
}
func TestExplicitSelectionAndExactAdmission(t *testing.T) {
	state := brokerState()
	var captured companionclient.Action
	a := newMonitored(Options{Local: local, Client: paired(t, &state, &captured)})
	snapshot, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Viewing.Connected || snapshot.Authority.ViewerAuthorized {
		t.Fatal("session selected without user choice")
	}
	selection, err := snapshot.PrepareSelection(registration, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Action(context.Background(), selection.Action); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = a.Snapshot(context.Background())
	if !snapshot.Viewing.Exclusive || snapshot.Authority.StaffName != "Example Staff" {
		t.Fatal("trusted state not mapped")
	}
	review, err := snapshot.Prepare(ui.AdmitGuest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Action(context.Background(), review.Action)
	if err != nil {
		t.Fatal(err)
	}
	if captured.RegistrationID != registration || captured.ExpectedRevision != state.Sessions[0].Revision || captured.Admission.CYPContactID != "person" || captured.Admission.ActivityID != "activity" {
		t.Fatal("exact admission binding lost")
	}
	if !strings.Contains(result.Message, "queued") {
		t.Fatal("queue acceptance claimed completion")
	}
}
func TestMultipleSessionsNeverGuessAndDisappearingSelectionClears(t *testing.T) {
	state := brokerState()
	other := state.Sessions[0]
	other.RegistrationID = "77777777-7777-4777-8777-777777777777"
	state.Sessions = append(state.Sessions, other)
	a := newMonitored(Options{Local: local, Client: paired(t, &state, nil)})
	s, _ := a.Snapshot(context.Background())
	if !s.Viewing.Ambiguous || s.Viewing.Connected || len(s.Sessions) != 2 {
		t.Fatal("ambiguous state guessed")
	}
	r, _ := s.PrepareSelection(registration, time.Now())
	_, _ = a.Action(context.Background(), r.Action)
	state.Sessions = state.Sessions[1:]
	s, _ = a.Snapshot(context.Background())
	if s.Viewing.Connected || a.selected != "" {
		t.Fatal("replaced registration retained authority")
	}
}
func TestBYODOwnerUsesLocalRevisionGuardWithoutBroker(t *testing.T) {
	called := false
	a := newMonitored(Options{Local: func(ctx context.Context) (ui.Snapshot, error) {
		s, _ := local(ctx)
		s.Authority.HostingAuthorized = true
		return s, nil
	}, OwnerHosting: func(_ context.Context, action ui.Action) error {
		called = true
		if action.ExpectedRevision != "owned-revision" || action.Enabled {
			t.Fatal("local reviewed target lost")
		}
		return nil
	}})
	s, _ := a.Snapshot(context.Background())
	r, err := s.Prepare(ui.SetHostingAvailable, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Action(context.Background(), r.Action)
	if err != nil || !called {
		t.Fatal("local owner action missing")
	}
	s, _ = a.Snapshot(context.Background())
	if !s.Hosting.Available {
		t.Fatal("optimistically changed hosting observation")
	}
}
func TestVenueIncomingNamesCannotAuthoriseHosting(t *testing.T) {
	a := newMonitored(Options{Local: func(ctx context.Context) (ui.Snapshot, error) {
		s, _ := local(ctx)
		s.Hosting.ViewerNames = []string{"Example Staff"}
		return s, nil
	}})
	s, _ := a.Snapshot(context.Background())
	if s.Authority.HostingAuthorized || s.Capabilities[ui.SetHostingAvailable].Allowed {
		t.Fatal("viewer name created staff authority")
	}
}
func TestMissingOccurrenceLabelDisablesAdmission(t *testing.T) {
	state := brokerState()
	state.Sessions[0].ActivityLabel = ""
	state.Sessions[0].ActivityStartsAt = time.Time{}
	a := newMonitored(Options{Local: local, Client: paired(t, &state, nil)})
	s, _ := a.Snapshot(context.Background())
	r, _ := s.PrepareSelection(registration, time.Now())
	_, _ = a.Action(context.Background(), r.Action)
	s, _ = a.Snapshot(context.Background())
	if s.Allow(ui.AdmitGuest, time.Now()) == nil {
		t.Fatal("admission without reviewable occurrence")
	}
}
func TestPendingHostMutationDisablesAnotherToggle(t *testing.T) {
	state := brokerState()
	state.HostMutationPending = true
	a := newMonitored(Options{Local: local, Client: paired(t, &state, nil)})
	s, _ := a.Snapshot(context.Background())
	if s.Capabilities[ui.SetHostingAvailable].Allowed || s.Capabilities[ui.DisconnectHosting].Allowed {
		t.Fatal("queued host mutation permits another action")
	}
	if !strings.Contains(s.Notice, "pending") {
		t.Fatal("pending state hidden")
	}
}

func TestBrowserUnavailablePausesAuthorityAndRecoversExactSelection(t *testing.T) {
	state := brokerState()
	unavailable := false
	a := newMonitored(Options{Local: local, Client: pairedWithUnavailable(t, &state, nil, &unavailable), OpenApproval: func(string) error { return nil }, OpenReadyApp: func() error { return nil }})
	s, _ := a.Snapshot(context.Background())
	r, _ := s.PrepareSelection(registration, time.Now())
	_, _ = a.Action(context.Background(), r.Action)
	s, _ = a.Snapshot(context.Background())
	if !s.Viewing.Connected {
		t.Fatal("selection missing")
	}
	unavailable = true
	s, _ = a.Snapshot(context.Background())
	if a.selected != registration || s.Viewing.Connected || s.Authority.ViewerAuthorized || s.Authority.HostingAuthorized || s.Capabilities[ui.SetHostingAvailable].Allowed || s.Capabilities[ui.TakeExclusive].Allowed || s.Capabilities[ui.SelectSession].Allowed {
		t.Fatal("unavailable browser retained mutation authority or lost selection")
	}
	if s.Capabilities[ui.SignIn].Allowed || !s.Capabilities[ui.SignOut].Allowed || !s.Capabilities[ui.OpenReadyApp].Allowed || !strings.Contains(s.Notice, "Browser unavailable") {
		t.Fatal("recovery incorrectly demands pairing")
	}
	unavailable = false
	s, _ = a.Snapshot(context.Background())
	if s.Viewing.RegistrationID != registration || !s.Authority.ViewerAuthorized {
		t.Fatal("fresh exact selection did not recover")
	}
	unavailable = true
	_, _ = a.Snapshot(context.Background())
	state.Sessions = nil
	unavailable = false
	s, _ = a.Snapshot(context.Background())
	if s.Viewing.Connected || a.selected != "" {
		t.Fatal("replacement reused stale selection")
	}
}
func TestPendingPairingAllowsExplicitCancel(t *testing.T) {
	// Hold the single-use poll at pending; the user can cancel/restart without
	// waiting for expiry or exposing the private pairing secret.
	c2, err := companionclient.New(companionclient.Config{DeviceID: dev, HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			return data(map[string]string{"status": "pending"}), nil
		}
		return data(map[string]any{"pairingId": pair, "pairingSecret": strings.Repeat("a", 64), "userCode": "AABB1122", "verificationPath": "/solarbeam/companion/connect?pairing=" + pair, "expiresAt": time.Now().Add(time.Minute)}), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c2.BeginPairing(context.Background())
	a := newMonitored(Options{Local: local, Client: c2, OpenApproval: func(string) error { return nil }})
	s, _ := a.Snapshot(context.Background())
	if !s.Capabilities[ui.SignOut].Allowed || s.Capabilities[ui.SignIn].Allowed {
		t.Fatal("pending pairing has no explicit restart")
	}
	_, _ = a.Action(context.Background(), ui.Action{Kind: ui.SignOut})
	if c2.Pairing() != nil {
		t.Fatal("cancel retains secret")
	}
}
