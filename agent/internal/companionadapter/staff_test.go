package companionadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
)

// newMonitored stands in for a panel that has registered for Windows session
// notifications, which is the precondition for any staff authority.
func newMonitored(opts Options) *Adapter {
	a := New(opts)
	a.StaffSessionMonitored(true)
	return a
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// signedInStaff returns an adapter whose staff member has approved in the
// browser, selected the viewing session, and holds a ready Leave review.
func signedInStaff(t *testing.T, clock *fakeClock, idle time.Duration) (*Adapter, *companionclient.Client, ui.Review) {
	t.Helper()
	state := brokerState()
	state.Sessions[0].AllowedActions = append(state.Sessions[0].AllowedActions, "leave")
	state.HostAllowedActions = []string{"set_hosting", "disconnect_hosting"}
	client := paired(t, &state, nil)
	opts := Options{Local: local, Client: client, OpenApproval: func(string) error { return nil }, StaffIdleTimeout: idle}
	if clock != nil {
		opts.Now = clock.Now
	}
	a := newMonitored(opts)
	s, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := s.PrepareSelection(registration, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Action(context.Background(), selection.Action); err != nil {
		t.Fatal(err)
	}
	s, _ = a.Snapshot(context.Background())
	review, err := s.Prepare(ui.LeaveViewer, time.Now())
	if err != nil {
		t.Fatalf("staff leave unavailable before revocation: %v", err)
	}
	return a, client, review
}

func assertStaffRevoked(t *testing.T, a *Adapter, client *companionclient.Client, review ui.Review) {
	t.Helper()
	// The action is refused where it executes, even though the panel still
	// holds a review prepared while authority was current.
	if _, err := a.Action(context.Background(), review.Action); !errors.Is(err, ErrStaffSignInExpired) {
		t.Fatalf("action after revocation = %v, want ErrStaffSignInExpired", err)
	}
	if client.HasBinding() || client.Pairing() != nil {
		t.Fatal("native staff credential survived revocation")
	}
	s, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ui.Kind{ui.SetHostingAvailable, ui.DisconnectHosting, ui.LeaveViewer, ui.TakeExclusive, ui.AdmitGuest, ui.SelectSession} {
		if s.Capabilities[kind].Allowed {
			t.Fatalf("capability %d still allowed after revocation", kind)
		}
	}
	if s.Authority.HostingAuthorized || s.Authority.ViewerAuthorized || s.Viewing.Connected {
		t.Fatal("staff authority still shown after revocation")
	}
	if s.Authority.Label != "Staff sign-in expired" || !strings.Contains(s.Notice, "Staff sign-in expired") {
		t.Fatalf("expired state not shown: label %q notice %q", s.Authority.Label, s.Notice)
	}
	if !s.Capabilities[ui.SignIn].Allowed {
		t.Fatal("expired staff sign-in cannot be restarted")
	}
}

func TestSessionEventsAndPanelCloseRevokeStaffAuthority(t *testing.T) {
	for _, reason := range []ui.RevokeReason{ui.RevokeSessionLocked, ui.RevokeSessionDisconnected, ui.RevokeSessionChanged, ui.RevokePanelClosed} {
		t.Run(reason.String(), func(t *testing.T) {
			a, client, review := signedInStaff(t, nil, 0)
			a.RevokeStaff(reason)
			assertStaffRevoked(t, a, client, review)
			s, _ := a.Snapshot(context.Background())
			if !strings.Contains(s.Details, reason.String()) {
				t.Fatalf("details do not say why: %q", s.Details)
			}
		})
	}
}

// A lock that lands while mu is held by an in-flight action or snapshot must
// still refuse the next action: RevokeStaff does not wait for mu.
func TestRevocationDoesNotWaitForBusyAdapter(t *testing.T) {
	a, client, review := signedInStaff(t, nil, 0)
	a.mu.Lock()
	done := make(chan struct{})
	go func() { a.RevokeStaff(ui.RevokeSessionLocked); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		a.mu.Unlock()
		t.Fatal("RevokeStaff blocked behind the adapter lock")
	}
	a.mu.Unlock()
	assertStaffRevoked(t, a, client, review)
}

func TestIdleTimeoutRevokesAndInteractionExtends(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	a, client, review := signedInStaff(t, clock, 5*time.Minute)
	clock.Advance(4 * time.Minute)
	a.StaffInteraction()
	clock.Advance(4 * time.Minute)
	if s, _ := a.Snapshot(context.Background()); !s.Authority.ViewerAuthorized {
		t.Fatal("recent panel input did not keep staff authority")
	}
	// Background refreshes are not interaction.
	for i := 0; i < 3; i++ {
		clock.Advance(40 * time.Second)
		_, _ = a.Snapshot(context.Background())
	}
	clock.Advance(time.Second)
	assertStaffRevoked(t, a, client, review)
	s, _ := a.Snapshot(context.Background())
	if !strings.Contains(s.Details, ui.RevokeIdle.String()) {
		t.Fatalf("idle reason missing: %q", s.Details)
	}
}

func TestClockBeforeLastActivityFailsClosed(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	a, client, review := signedInStaff(t, clock, 0)
	clock.Advance(-time.Minute)
	assertStaffRevoked(t, a, client, review)
}

// Without lock/user-switch notifications the adapter cannot keep its promise,
// so it neither grants nor starts staff sign-in.
func TestUnmonitoredSessionWithholdsStaffAuthority(t *testing.T) {
	state := brokerState()
	client := paired(t, &state, nil)
	a := New(Options{Local: local, Client: client, OpenApproval: func(string) error { return nil }})
	s, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Authority.HostingAuthorized || s.Capabilities[ui.SelectSession].Allowed || s.Capabilities[ui.SignIn].Allowed || len(s.Sessions) != 0 {
		t.Fatal("staff authority granted without session monitoring")
	}
	if client.Pairing() != nil || client.HasBinding() {
		t.Fatal("pending approval kept without session monitoring")
	}
	if _, err := a.Action(context.Background(), ui.Action{Kind: ui.SignIn}); err == nil {
		t.Fatal("sign-in started without session monitoring")
	}
	if _, err := a.Action(context.Background(), ui.Action{Kind: ui.CheckUpdates, RequestID: "66666666-6666-4666-8666-666666666666", DeviceID: dev}); err == nil {
		t.Fatal("staff-routed action accepted without session monitoring")
	}

	// Monitoring lost after sign-in revokes like a lock.
	b, client2, review := signedInStaff(t, nil, 0)
	b.StaffSessionMonitored(false)
	if _, err := b.Action(context.Background(), review.Action); err == nil {
		t.Fatal("action accepted after monitoring was lost")
	}
	if client2.HasBinding() {
		t.Fatal("credential kept after monitoring was lost")
	}
}

// A lock before approval must cancel the pending pairing: otherwise the
// browser approval would grant authority to whoever unlocks next.
func TestRevocationCancelsPendingPairing(t *testing.T) {
	state := brokerState()
	client := paired(t, &state, nil) // BeginPairing done; poll not yet run
	a := newMonitored(Options{Local: local, Client: client, OpenApproval: func(string) error { return nil }})
	a.RevokeStaff(ui.RevokeSessionLocked)
	s, _ := a.Snapshot(context.Background())
	if client.Pairing() != nil || client.HasBinding() || s.Authority.StaffName != "" || s.Capabilities[ui.SelectSession].Allowed {
		t.Fatal("pending approval survived a lock")
	}
	if !strings.Contains(s.Notice, "Staff sign-in expired") {
		t.Fatalf("expired notice missing: %q", s.Notice)
	}
}

// Owner operations on a BYOD device are the owner's own authority, not a staff
// lease, and a lock (which the owner can undo) does not remove them.
func TestOwnerActionsAreNotStaffRouted(t *testing.T) {
	called := false
	a := New(Options{Local: func(ctx context.Context) (ui.Snapshot, error) {
		s, _ := local(ctx)
		s.Authority.HostingAuthorized = true
		return s, nil
	}, OwnerHosting: func(context.Context, ui.Action) error { called = true; return nil }})
	a.RevokeStaff(ui.RevokeSessionLocked)
	s, _ := a.Snapshot(context.Background())
	r, err := s.Prepare(ui.SetHostingAvailable, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Action(context.Background(), r.Action); err != nil || !called {
		t.Fatalf("owner action refused: %v", err)
	}
	if strings.Contains(s.Notice, "expired") {
		t.Fatal("expired shown with no staff sign-in to expire")
	}
}

func TestClampStaffIdleTimeout(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                DefaultStaffIdleTimeout,
		-time.Minute:     MinStaffIdleTimeout,
		time.Second:      MinStaffIdleTimeout,
		7 * time.Minute:  7 * time.Minute,
		time.Hour:        MaxStaffIdleTimeout,
		15 * time.Minute: 15 * time.Minute,
	} {
		if got := ClampStaffIdleTimeout(in); got != want {
			t.Fatalf("ClampStaffIdleTimeout(%v) = %v, want %v", in, got, want)
		}
	}
	if DefaultStaffIdleTimeout != 5*time.Minute {
		t.Fatal("default staff idle timeout changed")
	}
}
