package sessionindicator

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeBar is a bar with no compositor and no window behind it.
type fakeBar struct {
	done     chan struct{}
	once     sync.Once
	closedCh chan struct{}
}

func newFakeBar() *fakeBar {
	return &fakeBar{done: make(chan struct{}), closedCh: make(chan struct{}, 1)}
}

func (f *fakeBar) Done() <-chan struct{} { return f.done }
func (f *fakeBar) Close() {
	f.once.Do(func() {
		close(f.done)
		f.closedCh <- struct{}{}
	})
}

// die simulates the compositor tearing the bar down underneath us.
func (f *fakeBar) die() { f.once.Do(func() { close(f.done) }) }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// awake is the screen-state probe every pre-existing test wants: an
// unlocked machine with its displays on, definitely, so nothing skips.
func awake() ScreenState {
	return ScreenState{
		Locked:            No,
		LockedSource:      "test",
		DisplaysOff:       No,
		DisplaysOffSource: "test",
	}
}

// stateFunc adapts a fixed reading to the probe seam.
func stateFunc(s ScreenState) func() ScreenState {
	return func() ScreenState { return s }
}

func TestNoticeText(t *testing.T) {
	tests := []struct {
		name   string
		notice Notice
		want   string
	}{
		{"no viewer named", Notice{}, "Your screen is being viewed remotely"},
		{"viewer named", Notice{Viewer: "Aurora Nicholas"}, "Your screen is being viewed remotely - Aurora Nicholas"},
		{"whitespace-only viewer is no viewer", Notice{Viewer: "   "}, "Your screen is being viewed remotely"},
		{
			"control characters are scrubbed, not rendered",
			Notice{Viewer: "Aurora\n\tNicholas"},
			"Your screen is being viewed remotely - Aurora Nicholas",
		},
		{
			"an over-long name cannot push the warning off the bar",
			Notice{Viewer: "AAAAAAAAAABBBBBBBBBBCCCCCCCCCCDDDDDDDDDDEEEEEEEEEE"},
			"Your screen is being viewed remotely - AAAAAAAAAABBBBBBBBBBCCCCCCCCCCDDDDDDDDDD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.notice.Text(); got != tt.want {
				t.Errorf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSupervisorShowStartsOneBar(t *testing.T) {
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, awake, quietLogger())

	if err := s.Show(Notice{Viewer: "Aurora"}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if calls != 1 {
		t.Fatalf("started %d bars, want 1", calls)
	}
}

func TestSupervisorShowIsIdempotentForTheSameNotice(t *testing.T) {
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, awake, quietLogger())

	for range 5 {
		if err := s.Show(Notice{Viewer: "Aurora"}); err != nil {
			t.Fatalf("Show: %v", err)
		}
	}
	// The driver polls every few seconds; rebuilding the surface on every
	// tick would flicker the bar for the whole session.
	if calls != 1 {
		t.Fatalf("started %d bars for the same notice, want 1", calls)
	}
}

func TestSupervisorShowRebuildsWhenTheNoticeChanges(t *testing.T) {
	var mu sync.Mutex
	var started []Notice
	s := newSupervisor(func(n Notice) (bar, error) {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, n)
		return newFakeBar(), nil
	}, awake, quietLogger())

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if err := s.Show(Notice{Viewer: "Aurora"}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 || started[1].Viewer != "Aurora" {
		t.Fatalf("started = %+v, want two bars ending with the named viewer", started)
	}
}

func TestSupervisorShowReportsTheFirstFailureSynchronously(t *testing.T) {
	sentinel := errors.New("no zwlr_layer_shell_v1")
	s := newSupervisor(func(Notice) (bar, error) { return nil, sentinel }, awake, quietLogger())

	err := s.Show(Notice{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Show err = %v, want it to wrap %v", err, sentinel)
	}
	// A failed Show must not leave the supervisor armed: the caller has
	// been told the bar is not up, and applies its policy on that basis.
	if s.wanted {
		t.Error("a failed Show left the supervisor wanting a bar")
	}
}

func TestSupervisorReassertsABarThatDiesWhileWanted(t *testing.T) {
	first, second := newFakeBar(), newFakeBar()
	bars := []bar{first, second}
	starts := make(chan struct{}, 4)
	var mu sync.Mutex
	i := 0

	s := newSupervisor(func(Notice) (bar, error) {
		mu.Lock()
		defer mu.Unlock()
		b := bars[min(i, len(bars)-1)]
		i++
		starts <- struct{}{}
		return b, nil
	}, awake, quietLogger())
	s.sleep = func(time.Duration) {} // no real backoff in tests

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	<-starts // the initial bar

	// The compositor takes the bar away mid-session. It must come back:
	// this is the "cannot be dismissed by the remote party" guarantee.
	first.die()

	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("bar was never re-asserted after the compositor closed it")
	}
	_ = s.Close()
}

func TestSupervisorDoesNotReassertAfterHide(t *testing.T) {
	starts := make(chan struct{}, 8)
	s := newSupervisor(func(Notice) (bar, error) {
		starts <- struct{}{}
		return newFakeBar(), nil
	}, awake, quietLogger())
	s.sleep = func(time.Duration) {}

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	<-starts
	if err := s.Hide(); err != nil { // Hide closes the bar, which fires Done
		t.Fatalf("Hide: %v", err)
	}

	select {
	case <-starts:
		t.Fatal("a deliberately hidden bar was re-asserted")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSupervisorShowAfterCloseFails(t *testing.T) {
	s := newSupervisor(func(Notice) (bar, error) { return newFakeBar(), nil }, awake, quietLogger())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Show(Notice{}); err == nil {
		t.Fatal("Show on a closed indicator returned nil error")
	}
}

func TestSupervisorHideIsSafeWhenNothingIsShown(t *testing.T) {
	s := newSupervisor(func(Notice) (bar, error) { return newFakeBar(), nil }, awake, quietLogger())
	if err := s.Hide(); err != nil {
		t.Fatalf("Hide with nothing shown: %v", err)
	}
}

func TestSupervisorSkipsWhenTheMachineIsLocked(t *testing.T) {
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, stateFunc(ScreenState{Locked: Yes, LockedSource: "test", DisplaysOff: No}), quietLogger())

	err := s.Show(Notice{})
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("Show err = %v, want a skip", err)
	}
	// Not attempted, not merely torn down afterwards.
	if calls != 0 {
		t.Errorf("started %d bars against a locked screen, want 0", calls)
	}
	// A skip leaves the supervisor exactly as it found it, so the next
	// poll can decide again from scratch.
	if s.wanted {
		t.Error("a skipped Show left the supervisor wanting a bar")
	}
}

func TestSupervisorSkipsWhenTheDisplaysAreOff(t *testing.T) {
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, stateFunc(ScreenState{Locked: No, DisplaysOff: Yes, DisplaysOffSource: "test"}), quietLogger())

	err := s.Show(Notice{})
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("Show err = %v, want a skip", err)
	}
	if calls != 0 {
		t.Errorf("started %d bars against sleeping displays, want 0", calls)
	}
}

func TestSupervisorShowsWhenSomebodyIsThere(t *testing.T) {
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, awake, quietLogger())

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show on an awake, unlocked machine: %v", err)
	}
	if calls != 1 {
		t.Fatalf("started %d bars, want 1", calls)
	}
}

func TestSupervisorFailureIsNotASkip(t *testing.T) {
	// The fourth case: the machine is awake and the bar genuinely could
	// not be put up. That must keep the pre-existing behaviour — a real
	// error, NOT matching ErrSkipped, so require_session_indicator still
	// trips on it.
	sentinel := errors.New("no zwlr_layer_shell_v1")
	s := newSupervisor(func(Notice) (bar, error) { return nil, sentinel }, awake, quietLogger())

	err := s.Show(Notice{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Show err = %v, want it to wrap %v", err, sentinel)
	}
	if errors.Is(err, ErrSkipped) {
		t.Fatal("an inability to show the bar was reported as a deliberate skip")
	}
}

func TestSupervisorLeavesAnAlreadyShownBarUpWhenTheMachineLocks(t *testing.T) {
	// Taking the bar down on lock would leave the person unlocking with
	// an unannounced session for up to one poll interval, and there is
	// nothing to gain: the bar already stands inside the reserved band.
	locked := ScreenState{Locked: No, DisplaysOff: No}
	b := newFakeBar()
	s := newSupervisor(func(Notice) (bar, error) { return b, nil },
		func() ScreenState { return locked }, quietLogger())

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	locked = ScreenState{Locked: Yes, LockedSource: "test", DisplaysOff: No}

	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("the poll tick after the machine locked returned %v, want nil (bar left alone)", err)
	}
	select {
	case <-b.closedCh:
		t.Fatal("locking the machine tore down a bar that was already up")
	default:
	}
}

func TestSupervisorRaisesTheBarOnceTheMachineIsUnlocked(t *testing.T) {
	// The other half: a skip is re-decided every poll, so the bar appears
	// within one tick of the person coming back.
	state := ScreenState{Locked: Yes, LockedSource: "test"}
	var calls int
	s := newSupervisor(func(Notice) (bar, error) {
		calls++
		return newFakeBar(), nil
	}, func() ScreenState { return state }, quietLogger())

	if err := s.Show(Notice{}); !errors.Is(err, ErrSkipped) {
		t.Fatalf("Show while locked = %v, want a skip", err)
	}
	state = ScreenState{Locked: No, DisplaysOff: No}
	if err := s.Show(Notice{}); err != nil {
		t.Fatalf("Show after unlocking: %v", err)
	}
	if calls != 1 {
		t.Fatalf("started %d bars, want exactly the one after unlocking", calls)
	}
}
