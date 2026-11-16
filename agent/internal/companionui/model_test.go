package companionui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture() Snapshot {
	now := time.Now()
	s := Snapshot{ObservedAt: now.Add(-time.Millisecond), ValidUntil: now.Add(time.Minute), Device: Device{ID: "device", Name: "Fictional PC"}, Hosting: Hosting{Known: true, Available: true, Active: true, SessionID: "hosting", Revision: "hosting-revision", ViewerNames: []string{"Example Viewer"}}, Viewing: Viewing{RegistrationID: "registration", SessionID: "session", ParticipantSID: "participant", HostName: "Example Host", Revision: "opaque-sha", Connected: true, Exclusive: true, PendingGuest: PendingGuest{Identity: "guest", OccurrenceID: "occurrence", Label: "Example guest", OccurrenceLabel: "Example activity at 14:30"}}, Authority: Authority{HostingAuthorized: true, ViewerAuthorized: true}}
	for k := SetHostingAvailable; k <= SignOut; k++ {
		s.Capabilities[k] = Capability{Allowed: true}
	}
	return s
}
func TestAdmissionBindsExactReview(t *testing.T) {
	s := fixture()
	r, err := s.Prepare(AdmitGuest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a := r.Action
	if a.GuestIdentity != "guest" || a.OccurrenceID != "occurrence" || a.RegistrationID != "registration" || a.ParticipantSID != "participant" || a.ExpectedRevision != "opaque-sha" {
		t.Fatalf("not exact: %+v", a)
	}
	if !strings.Contains(r.Description, "Example guest") {
		t.Fatal("review hides person")
	}
	for _, mutate := range []func(*Snapshot){func(s *Snapshot) { s.Viewing.PendingGuest.Identity = "other" }, func(s *Snapshot) { s.Viewing.PendingGuest.OccurrenceID = "next" }, func(s *Snapshot) { s.Viewing.Revision = "changed" }, func(s *Snapshot) { s.Viewing.ParticipantSID = "replacement" }, func(s *Snapshot) { s.Viewing.RegistrationID = "another-tab" }, func(s *Snapshot) { s.Viewing.SessionID = "new-session" }, func(s *Snapshot) { s.Authority.ViewerAuthorized = false }} {
		next := s.Clone()
		mutate(&next)
		if r.Matches(next, time.Now()) {
			t.Fatal("stale admission survived changed binding")
		}
	}
}
func TestFailsClosed(t *testing.T) {
	for _, mutate := range []func(*Snapshot){func(s *Snapshot) { s.ObservedAt = time.Now().Add(-16 * time.Second) }, func(s *Snapshot) { s.ValidUntil = time.Now().Add(-time.Second) }, func(s *Snapshot) { s.Authority.Locked = true }, func(s *Snapshot) { s.Authority.ViewerAuthorized = false }, func(s *Snapshot) { s.Viewing.Ambiguous = true }, func(s *Snapshot) { s.Viewing.ParticipantSID = "" }, func(s *Snapshot) { s.Viewing.RegistrationID = "" }, func(s *Snapshot) { s.Capabilities[TakeExclusive] = Capability{} }} {
		s := fixture()
		mutate(&s)
		if _, err := s.Prepare(TakeExclusive, time.Now()); err == nil {
			t.Fatal("unavailable action accepted")
		}
	}
}
func TestPowerReviewAndReleaseMeaning(t *testing.T) {
	s := fixture()
	r, err := s.Prepare(SetHostingAvailable, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Action.Enabled || !strings.Contains(r.Description, "disconnects the active viewer") {
		t.Fatal("active power change lacks explicit warning")
	}
	s.Hosting.Available = false
	if r.Matches(s, time.Now()) {
		t.Fatal("toggle changed its intended target")
	}
	r, err = s.Prepare(ReleaseExclusive, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Description, "input remains available") {
		t.Fatal("release implied input disabled")
	}
}
func TestUnknownHostingDisablesMutationsAndUpdates(t *testing.T) {
	s := fixture()
	s.Hosting.Known = false
	for _, k := range []Kind{SetHostingAvailable, DisconnectHosting, CheckUpdates} {
		if s.Allow(k, time.Now()) == nil {
			t.Fatal("unknown state permits mutation")
		}
	}
	s.Hosting.Known = true
	if s.Allow(CheckUpdates, time.Now()) == nil {
		t.Fatal("active host permits updates")
	}
}
func TestSnapshotCopyDoesNotAliasNames(t *testing.T) {
	s := fixture()
	copy := s.Clone()
	s.Hosting.ViewerNames[0] = "changed"
	if copy.Hosting.ViewerNames[0] != "Example Viewer" {
		t.Fatal("snapshot aliases backend names")
	}
}

type fakeBackend struct {
	s       Snapshot
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	err     error
}

func (f *fakeBackend) Snapshot(context.Context) (Snapshot, error) { return f.s.Clone(), nil }
func (f *fakeBackend) Action(ctx context.Context, _ Action) (ActionResult, error) {
	f.calls.Add(1)
	if f.entered != nil {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return ActionResult{}, ctx.Err()
		}
	}
	return ActionResult{Message: "pending"}, f.err
}
func TestExecutorRefreshAndDuplicateSuppression(t *testing.T) {
	f := &fakeBackend{s: fixture()}
	r, _ := f.s.Prepare(AdmitGuest, time.Now())
	e := NewExecutor(f)
	f.s.Viewing.PendingGuest.Identity = "other"
	if _, err := e.Execute(context.Background(), r); err == nil || f.calls.Load() != 0 {
		t.Fatal("stale review dispatched")
	}
	f.s.Viewing.PendingGuest.Identity = "guest"
	f.err = errors.New("unknown outcome")
	if _, err := e.Execute(context.Background(), r); err == nil {
		t.Fatal("error hidden")
	}
	if _, err := e.Execute(context.Background(), r); err == nil || f.calls.Load() != 1 {
		t.Fatal("unknown request blindly retried")
	}
}
func TestExecutorSingleFlight(t *testing.T) {
	f := &fakeBackend{s: fixture(), entered: make(chan struct{}), release: make(chan struct{})}
	r, _ := f.s.Prepare(TakeExclusive, time.Now())
	e := NewExecutor(f)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = e.Execute(context.Background(), r) }()
	<-f.entered
	other, _ := f.s.Prepare(ReleaseExclusive, time.Now())
	if _, err := e.Execute(context.Background(), other); err == nil {
		t.Fatal("concurrent mutation dispatched")
	}
	close(f.release)
	<-done
	if f.calls.Load() != 1 {
		t.Fatal("more than one backend action")
	}
}
func TestShortcutsAndClamp(t *testing.T) {
	b := DefaultShortcuts()
	if err := ValidateShortcuts(b); err != nil {
		t.Fatal(err)
	}
	b[1].Key = b[0].Key
	if ValidateShortcuts(b) == nil {
		t.Fatal("duplicate shortcut accepted")
	}
	for _, bad := range []string{"R", "Alt+R", "Ctrl+R", "Win+R", "Ctrl+Alt+Enter"} {
		if _, err := ParseShortcut(OpenPanel, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if s, err := ParseShortcut(ReviewControl, "Ctrl+Alt+Shift+Z"); err != nil || s.String() != "Ctrl+Alt+Shift+Z" {
		t.Fatal("custom shortcut failed")
	}
	for _, work := range []Rect{{0, 0, 1920, 1040}, {-1920, -100, 0, 900}, {0, 0, 320, 400}} {
		for _, dpi := range []int32{96, 144, 192} {
			r := Place(work, dpi)
			if r.Left < work.Left || r.Top < work.Top || r.Right > work.Right || r.Bottom > work.Bottom || r.Right <= r.Left || r.Bottom <= r.Top {
				t.Fatalf("outside work area: %+v %+v", work, r)
			}
		}
	}
}
func TestShortcutsNeverSelectAmbiguousOrLockedSession(t *testing.T) {
	s := fixture()
	s.Viewing.Ambiguous = true
	for _, request := range []OpenRequest{ReviewAdmission, ReviewControl, RestoreSharedInput} {
		if ShortcutEnabled(request, s, time.Now()) {
			t.Fatal("ambiguous session shortcut enabled")
		}
	}
	s.Authority.Locked = true
	if ShortcutEnabled(OpenPanel, s, time.Now()) {
		t.Fatal("locked shortcut enabled")
	}
	s = fixture()
	s.Authority.ViewerAuthorized = false
	s.Authority.HostingAuthorized = false
	for request := OpenPanel; request <= RestoreSharedInput; request++ {
		if ShortcutEnabled(request, s, time.Now()) {
			t.Fatal("signed-out shortcut enabled")
		}
	}
}
